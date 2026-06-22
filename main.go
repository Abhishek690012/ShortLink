// Package main is the entry point for the ShortLink URL shortener.
// Module 1: Project Scaffold & Database Layer.
// Module 2: URL Shortening (The Write Path).
// Module 3: Redirection & Tracking (The Read Path).
// Module 4: Stats, Polish & Documentation.
//
// Responsibilities:
//   - Load environment variables from .env (with sensible defaults for local dev).
//   - Connect to PostgreSQL using a pgxpool connection pool.
//   - Bootstrap the database schema (idempotent — safe to run multiple times).
//   - Register HTTP routes and serve on :8080.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

// Schema
const createSchema = `
CREATE TABLE IF NOT EXISTS urls (
    id         VARCHAR(10)  PRIMARY KEY,
    long_url   TEXT         NOT NULL,
    clicks     INT          DEFAULT 0,
    created_at TIMESTAMP    DEFAULT NOW()
);`

// Config
type config struct {
	host          string //database connection parameters
	port          string
	user          string
	password      string
	dbName        string
	redisHost     string
	redisPort     string
	redisPassword string
	redisDB       int
	cacheTTL      time.Duration
	clickWorkers  int
	clickQueueSize int
}

// loadConfig reads values from the environment (populated by godotenv) and
// falls back to sensible local-dev defaults so the binary works even without
// a .env file.
func loadConfig() config {
	dbVal, err := strconv.Atoi(getEnv("REDIS_DB", "0"))
	if err != nil {
		dbVal = 0
	}

	ttlVal, err := time.ParseDuration(getEnv("CACHE_TTL", "300s"))
	if err != nil {
		ttlVal = 5 * time.Minute
	}

	workersVal, err := strconv.Atoi(getEnv("CLICK_WORKERS", "10"))
	if err != nil {
		workersVal = 10
	}

	queueSizeVal, err := strconv.Atoi(getEnv("CLICK_QUEUE_SIZE", "50000"))
	if err != nil {
		queueSizeVal = 50000
	}

	return config{
		host:          getEnv("DB_HOST", "localhost"),
		port:          getEnv("DB_PORT", "5432"),
		user:          getEnv("DB_USER", "shortlink"),
		password:      getEnv("DB_PASSWORD", "shortlink_secret"),
		dbName:        getEnv("DB_NAME", "shortlink_db"),
		redisHost:     getEnv("REDIS_HOST", "localhost"),
		redisPort:     getEnv("REDIS_PORT", "6379"),
		redisPassword: getEnv("REDIS_PASSWORD", ""),
		redisDB:       dbVal,
		cacheTTL:      ttlVal,
		clickWorkers:  workersVal,
		clickQueueSize: queueSizeVal,
	}
}

// dsn returns a postgresql:// Data Source Name string built from the config.
func (c config) dsn() string {
	return fmt.Sprintf(
		"postgresql://%s:%s@%s:%s/%s",
		c.user, c.password, c.host, c.port, c.dbName,
	)
}

// getEnv returns the value of the environment variable named by key, or
// fallback if the variable is unset or empty.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Database

// connectDB establishes a pgxpool connection pool, pings the server to
// confirm connectivity, and returns the pool.
func connectDB(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database ping failed: %w", err)
	}

	return pool, nil
}

// bootstrapSchema executes the DDL to create required tables if they do not
// already exist. It is safe to call on every startup.
func bootstrapSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, createSchema)
	if err != nil {
		return fmt.Errorf("schema bootstrap failed: %w", err)
	}
	return nil
}

const (
	serverAddr = ":8080"
	baseURL    = "http://localhost:8080"
)

func main() {

	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found — using environment variables / defaults")
	} else {
		log.Println("Loaded environment from .env")
	}

	cfg := loadConfig()
	log.Printf("  Connecting to database at %s:%s/%s ...", cfg.host, cfg.port, cfg.dbName)

	// Connect to PostgreSQL
	ctx := context.Background()

	pool, err := connectDB(ctx, cfg.dsn())
	if err != nil {
		log.Fatalf("Connection error: %v", err)
	}
	defer pool.Close() //atomic operation

	log.Println("Connected to PostgreSQL successfully")

	// Bootstrap the schema
	log.Println(" Applying schema migrations ..")

	if err := bootstrapSchema(ctx, pool); err != nil {
		log.Fatalf("Schema error: %v", err)
	}

	log.Println("Schema is up to date (table 'urls' ready)")

	// Connect to Redis
	log.Printf("  Connecting to Redis cache at %s:%s ...", cfg.redisHost, cfg.redisPort)
	rdb := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", cfg.redisHost, cfg.redisPort),
		Password: cfg.redisPassword,
		DB:       cfg.redisDB,
	})

	redisPingCtx, redisCancel := context.WithTimeout(ctx, 3*time.Second)
	if err := rdb.Ping(redisPingCtx).Err(); err != nil {
		log.Printf("WARNING: Redis cache ping failed: %v. Running in degraded mode without caching.", err)
	} else {
		log.Println("Connected to Redis successfully")
	}
	redisCancel()
	defer rdb.Close()

	// Initialize Click Queue Channel and Worker Pool
	clickChan := make(chan string, cfg.clickQueueSize)
	var wg sync.WaitGroup

	log.Printf("Starting %d background click workers...", cfg.clickWorkers)
	for i := 1; i <= cfg.clickWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for id := range clickChan {
				updateCtx, updateCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := pool.Exec(updateCtx, `UPDATE urls SET clicks = clicks + 1 WHERE id = $1`, id)
				updateCancel()
				if err != nil {
					log.Printf("ERROR (click worker %d) for id %q: %v", workerID, id, err)
				}
			}
		}(i)
	}

	// Register HTTP route
	h := NewHandler(pool, rdb, cfg.cacheTTL, clickChan, baseURL)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /shorten", h.ShortenURL) // Module 2 — write path
	mux.HandleFunc("GET /r/{id}", h.RedirectURL)  // Module 3 — read path
	mux.HandleFunc("GET /stats/{id}", h.GetStats) // Module 4 — stats
	mux.HandleFunc("/", notFound404)              // catch-all 404

	// Start HTTP server
	srv := &http.Server{
		Addr:         serverAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Run HTTP server asynchronously in a goroutine
	go func() {
		log.Printf(" ShortLink listening on %s", baseURL)
		log.Printf("POST %s/shorten", baseURL)
		log.Printf(" GET  %s/r/{id}", baseURL)
		log.Printf("GET  %s/stats/{id}", baseURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Graceful shutdown handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down HTTP server...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server Shutdown error: %v", err)
	}

	log.Println("Closing click queue channel...")
	close(clickChan)

	log.Println("Waiting for click workers to finish...")
	wg.Wait()
	log.Println("Graceful shutdown complete.")
}
