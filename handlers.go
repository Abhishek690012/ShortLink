// handlers.go contains all HTTP handler logic for ShortLink.
// Module 2: URL Shortening (The Write Path).
// Module 3: Redirection & Tracking (The Read Path).
// Module 4: Stats, Polish & Documentation.
//
// Exported surface:
//   - Handler           — struct that groups all handlers and carries shared
//                         dependencies (DB pool, base URL).
//   - generateShortID   — URL-safe Base62 random string generator.
//   - ShortenURL        — POST /shorten       — creates a short link.
//   - RedirectURL       — GET  /r/{id}        — resolves & redirects a short link.
//   - GetStats          — GET  /stats/{id}    — returns click stats for a link.
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

// -----------------------------------------------------------------------
// Constants & helpers
// -----------------------------------------------------------------------

const (
	// base62Chars is the alphabet used for short ID generation (URL-safe).
	base62Chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// shortIDLen is the default length of generated short IDs.
	shortIDLen = 6

	// maxRetries is how many times we retry ID generation on collision.
	maxRetries = 5
)

// generateShortID returns a cryptographically-casual, URL-safe alphanumeric
// string of the given length drawn from the Base62 alphabet (a-z A-Z 0-9).
//
// math/rand/v2 (Go 1.22+) is seeded automatically per-process; it is fast
// and sufficient for a non-security-sensitive short ID. For a production
// service handling billions of URLs, swap this for crypto/rand.
func generateShortID(length int) string {
	sb := strings.Builder{}
	sb.Grow(length)
	for range length {
		sb.WriteByte(base62Chars[rand.IntN(len(base62Chars))])
	}
	return sb.String()
}

// -----------------------------------------------------------------------
// Handler
// -----------------------------------------------------------------------

// Handler groups all HTTP handlers and their shared dependencies so they
// can be registered onto a ServeMux cleanly.
type Handler struct {
	pool    *pgxpool.Pool
	baseURL string // e.g. "http://localhost:8080"
}

// NewHandler constructs a Handler with the given pool and base URL.
func NewHandler(pool *pgxpool.Pool, baseURL string) *Handler {
	return &Handler{pool: pool, baseURL: baseURL}
}

// -----------------------------------------------------------------------
// POST /shorten
// -----------------------------------------------------------------------

// shortenRequest is the expected JSON body for POST /shorten.
type shortenRequest struct {
	URL string `json:"url"`
}

// shortenResponse is the JSON body returned on success.
type shortenResponse struct {
	ID       string `json:"id"`
	ShortURL string `json:"short_url"`
}

// ShortenURL handles POST /shorten.
//
// Flow:
//  1. Decode and validate the JSON body.
//  2. Validate that the URL is absolute HTTP/HTTPS.
//  3. Generate a unique Base62 short ID (retries on collision).
//  4. Insert the record into the urls table.
//  5. Return 201 Created with the short URL.
func (h *Handler) ShortenURL(w http.ResponseWriter, r *http.Request) {
	// ── 1. Decode request body ───────────────────────────────────────────
	var req shortenRequest

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

	// ── 2. Validate URL ──────────────────────────────────────────────────
	if err := validateURL(req.URL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}

	// ── 3. Generate a unique short ID ────────────────────────────────────
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

	// ── 4. Persist to database ───────────────────────────────────────────
	const insertSQL = `INSERT INTO urls (id, long_url) VALUES ($1, $2)`

	if _, err := h.pool.Exec(ctx, insertSQL, id, req.URL); err != nil {
		log.Printf("ERROR inserting url (id=%s): %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "database error, please try again later",
		})
		return
	}

	log.Printf("✔  Shortened  %s  →  %s/r/%s", req.URL, h.baseURL, id)

	// ── 5. Respond 201 Created ───────────────────────────────────────────
	writeJSON(w, http.StatusCreated, shortenResponse{
		ID:       id,
		ShortURL: fmt.Sprintf("%s/r/%s", h.baseURL, id),
	})
}

// -----------------------------------------------------------------------
// GET /r/{id}
// -----------------------------------------------------------------------

// RedirectURL handles GET /r/{id}.
//
// Flow:
//  1. Extract the short ID from the URL path using the Go 1.22+ pattern
//     variable API (r.PathValue).
//  2. Look up the long_url in the database.
//     - If no row matches, return 404 with a JSON error body.
//     - If a DB error occurs (other than not-found), return 500.
//  3. Atomically increment the clicks counter for the matched row.
//  4. Issue a 302 Found redirect to the long_url.
func (h *Handler) RedirectURL(w http.ResponseWriter, r *http.Request) {
	// ── 1. Extract {id} path variable ────────────────────────────────────
	// r.PathValue is available in Go 1.22+ with the enhanced ServeMux.
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing short ID in path",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// ── 2. Look up the long URL ───────────────────────────────────────────
	var longURL string

	err := h.pool.QueryRow(ctx,
		`SELECT long_url FROM urls WHERE id = $1`, id,
	).Scan(&longURL)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The ID does not exist in the database.
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "short URL not found",
			})
			return
		}
		// Any other DB error is an internal failure.
		log.Printf("ERROR looking up id %q: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "database error, please try again later",
		})
		return
	}

	// ── 3. Atomically increment the click counter ─────────────────────────
	_, err = h.pool.Exec(ctx,
		`UPDATE urls SET clicks = clicks + 1 WHERE id = $1`, id,
	)
	if err != nil {
		// Log the error but do NOT abort the redirect — tracking is
		// best-effort; the user should still reach their destination.
		log.Printf("ERROR incrementing clicks for id %q: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "database error updating click count",
		})
		return
	}

	log.Printf("→  Redirecting  /r/%s  →  %s  (click recorded)", id, longURL)

	// ── 4. Issue 302 Found redirect ───────────────────────────────────────
	http.Redirect(w, r, longURL, http.StatusFound)
}

// -----------------------------------------------------------------------
// GET /stats/{id}
// -----------------------------------------------------------------------

// statsResponse is the JSON body returned by GET /stats/{id}.
type statsResponse struct {
	ID        string    `json:"id"`
	LongURL   string    `json:"long_url"`
	Clicks    int       `json:"clicks"`
	CreatedAt time.Time `json:"created_at"`
}

// GetStats handles GET /stats/{id}.
//
// Flow:
//  1. Extract the short ID from the URL path via r.PathValue.
//  2. Query the database for id, long_url, clicks, and created_at.
//     - If no row matches, return 404 with a JSON error body.
//     - On any other DB error, return 500.
//  3. Return 200 OK with a JSON statsResponse body.
func (h *Handler) GetStats(w http.ResponseWriter, r *http.Request) {
	// ── 1. Extract {id} path variable ────────────────────────────────────
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing short ID in path",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// ── 2. Query the database ─────────────────────────────────────────────
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

	// ── 3. Respond 200 OK ─────────────────────────────────────────────────
	writeJSON(w, http.StatusOK, resp)
}

// -----------------------------------------------------------------------
// Private helpers
// -----------------------------------------------------------------------

// validateURL returns an error if rawURL is not an absolute HTTP or HTTPS URL.
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

// uniqueShortID generates a Base62 short ID that does not already exist in
// the database. It retries up to maxRetries times before giving up.
func (h *Handler) uniqueShortID(ctx context.Context) (string, error) {
	for range maxRetries {
		id := generateShortID(shortIDLen)

		// Check for collision with a lightweight EXISTS query.
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

// writeJSON serialises v as JSON and writes it to w with the given status code.
// It always sets Content-Type: application/json.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		// At this point headers are already sent; just log.
		log.Printf("ERROR writing JSON response: %v", err)
	}
}

// notFound404 is a fallback handler registered on the mux so unknown routes
// return JSON instead of Go's plain-text 404 page.
func notFound404(w http.ResponseWriter, _ *http.Request) {
	// Avoid using writeJSON here — we set the header manually to not allocate
	// an extra encoder for a hot-path-unlikely case.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"error":"route not found"}`+"\n")
}

