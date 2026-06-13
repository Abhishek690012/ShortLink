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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
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
	host     string //dabase connection parameters
	port     string
	user     string
	password string
	dbName   string
}

// loadConfig reads values from the environment (populated by godotenv) and
// falls back to sensible local-dev defaults so the binary works even without
// a .env file.
func loadConfig() config {
	return config{
		host:     getEnv("DB_HOST", "localhost"),
		port:     getEnv("DB_PORT", "5432"),
		user:     getEnv("DB_USER", "shortlink"),
		password: getEnv("DB_PASSWORD", "shortlink_secret"),
		dbName:   getEnv("DB_NAME", "shortlink_db"),
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

	// Register HTTP route
	h := NewHandler(pool, baseURL)

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

	log.Printf(" ShortLink listening on %s", baseURL)
	log.Printf("POST %s/shorten", baseURL)
	log.Printf(" GET  %s/r/{id}", baseURL)
	log.Printf("GET  %s/stats/{id}", baseURL)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
