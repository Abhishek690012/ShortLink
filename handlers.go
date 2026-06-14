package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	base62Chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// shortIDLen is the default length of generated short IDs.
	shortIDLen = 6

	// maxRetries is how many times we retry ID generation on collision.
	maxRetries = 5
)

func generateShortID(length int) string {
	sb := strings.Builder{}
	sb.Grow(length)
	for range length {
		sb.WriteByte(base62Chars[rand.IntN(len(base62Chars))])
	}
	return sb.String()
}

// Handler groups all HTTP handlers and their shared dependencies so they
// can be registered onto a ServeMux cleanly.
type Handler struct {
	pool    *pgxpool.Pool
	baseURL string
}

// NewHandler constructs a Handler with the given pool and base URL.
func NewHandler(pool *pgxpool.Pool, baseURL string) *Handler {
	return &Handler{pool: pool, baseURL: baseURL}
}

type shortenRequest struct {
	URL string `json:"url"`
}

type shortenResponse struct {
	ID       string `json:"id"`
	ShortURL string `json:"short_url"`
}

// steps for handling requests:
//  1. Decode and validate the JSON body.
//  2. Validate that the URL is absolute HTTP/HTTPS.
//  3. Generate a unique Base62 short ID (retries on collision).
//  4. Insert the record into the urls table.
//  5. Return 201 Created with the short URL.
func (h *Handler) ShortenURL(w http.ResponseWriter, r *http.Request) {
	var req shortenRequest

	//strict type checking
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid JSON body: " + err.Error(),
		})
		return
	}

	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `"url" field is required and must not be empty`,
		})
		return
	}

	if err := validateURL(req.URL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}

	// Generate a unique short ID
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	id, err := h.uniqueShortID(ctx)
	if err != nil {
		log.Printf("ERROR generating short ID: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "could not generate a unique short ID, please try again",
		})
		return
	}

	// Persist to database
	const insertSQL = `INSERT INTO urls (id, long_url) VALUES ($1, $2)`

	if _, err := h.pool.Exec(ctx, insertSQL, id, req.URL); err != nil {
		log.Printf("ERROR inserting url (id=%s): %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "database error, please try again later",
		})
		return
	}

	log.Printf("✔  Shortened  %s  →  %s/r/%s", req.URL, h.baseURL, id)

	// response 201 Created
	writeJSON(w, http.StatusCreated, shortenResponse{
		ID:       id,
		ShortURL: fmt.Sprintf("%s/r/%s", h.baseURL, id),
	})
}

// RedirectURL handles GET /r/{id}.
func (h *Handler) RedirectURL(w http.ResponseWriter, r *http.Request) {
	//  Extract {id} path
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing short ID in path",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	//  Look up the long URL
	var longURL string

	err := h.pool.QueryRow(ctx,
		`SELECT long_url FROM urls WHERE id = $1`, id,
	).Scan(&longURL)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "ID doesn't exist in the database",
			})
			return
		}

		log.Printf("ERROR looking up id %q: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "database error, please try again later",
		})
		return
	}

	// atomic increment in the click counter
	_, err = h.pool.Exec(ctx,
		`UPDATE urls SET clicks = clicks + 1 WHERE id = $1`, id,
	)
	if err != nil {
		// Log the error but without aborting the redirect
		log.Printf("ERROR incrementing clicks for id %q: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "database error updating click count",
		})
	}

	log.Printf("→  Redirecting  /r/%s  →  %s  (click recorded)", id, longURL)

	http.Redirect(w, r, longURL, http.StatusFound)
}

type statsResponse struct {
	ID        string    `json:"id"`
	LongURL   string    `json:"long_url"`
	Clicks    int       `json:"clicks"`
	CreatedAt time.Time `json:"created_at"`
}

// GetStats handles GET /stats/{id}.
func (h *Handler) GetStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing short ID in path",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var resp statsResponse

	err := h.pool.QueryRow(ctx,
		`SELECT id, long_url, clicks, created_at FROM urls WHERE id = $1`, id,
	).Scan(&resp.ID, &resp.LongURL, &resp.Clicks, &resp.CreatedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "short URL not found",
			})
			return
		}
		log.Printf("ERROR fetching stats for id %q: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "database error, please try again later",
		})
		return
	}

	log.Printf("📊  Stats served  /stats/%s  (%d clicks)", id, resp.Clicks)

	writeJSON(w, http.StatusOK, resp)
}

// Private helpers

// returns an error if rawURL is not an absolute HTTP or HTTPS URL.
func validateURL(rawURL string) error {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return errors.New("URL scheme must be http or https")
	}

	if parsed.Host == "" {
		return errors.New("URL must include a host")
	}

	return nil
}

func (h *Handler) uniqueShortID(ctx context.Context) (string, error) {
	for range maxRetries {
		id := generateShortID(shortIDLen)

		var exists bool
		err := h.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM urls WHERE id = $1)`, id,
		).Scan(&exists)

		if err != nil {
			return "", fmt.Errorf("collision check failed: %w", err)
		}
		if !exists {
			return id, nil
		}

		log.Printf("⚠  Short ID collision on %q — retrying ...", id)
	}

	return "", fmt.Errorf("exceeded %d retries generating a unique ID", maxRetries)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		// At this point headers are already sent; just log.
		log.Printf("ERROR writing JSON response: %v", err)
	}
}

func notFound404(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"error":"route not found"}`+"\n")
}
