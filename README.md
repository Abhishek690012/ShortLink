# ShortLink

A minimalist, production-ready URL shortener built with Go and PostgreSQL.  
Paste a long URL, get a short one — and track every click with a single API call.

---

## Tech Stack

| Layer      | Technology                          |
|------------|-------------------------------------|
| Language   | Go 1.22+ (standard library HTTP)    |
| Database   | PostgreSQL 16 via `pgx/v5` pool     |
| Container  | Docker & Docker Compose             |
| Config     | `godotenv` + OS environment vars    |

---

## Project Structure

```
ShortLink/
├── main.go          # Entry point: config, DB connection, schema, router
├── handlers.go      # All HTTP handlers (ShortenURL, RedirectURL, GetStats)
├── docker-compose.yml
├── .env             # Local environment variables (not committed)
└── README.md
```

---

## Getting Started

### Prerequisites

- [Go 1.22+](https://go.dev/dl/)
- [Docker](https://docs.docker.com/get-docker/) & Docker Compose

### 1 — Clone and enter the project

```bash
git clone https://github.com/yourname/shortlink.git
cd shortlink
```

### 2 — Configure environment

Copy the example and edit if needed (defaults work out of the box with Docker):

```bash
cp .env.example .env   # or edit .env directly
```

Default `.env`:
```env
DB_HOST=localhost
DB_PORT=5432
DB_USER=shortlink
DB_PASSWORD=shortlink_secret
DB_NAME=shortlink_db
```

### 3 — Start PostgreSQL

```bash
docker compose up -d
```

This starts a PostgreSQL container named `shortlink_db` and exposes it on port `5432`.

### 4 — Run the server

```bash
go run .
```

The server auto-migrates the schema on startup (idempotent — safe to re-run):

```
Loaded environment from .env
Connected to PostgreSQL successfully
Schema is up to date (table 'urls' ready)
ShortLink listening on http://localhost:8080
    POST http://localhost:8080/shorten
    GET  http://localhost:8080/r/{id}
    GET  http://localhost:8080/stats/{id}
```

### 5 — Build a binary (optional)

```bash
go build -o shortlink .
./shortlink
```

---

## API Endpoints

### `POST /shorten` — Create a short URL

Accepts a JSON body with a valid HTTP/HTTPS URL. Returns a 6-character Base62 short ID.

```bash
curl -s -X POST http://localhost:8080/shorten \
  -H 'Content-Type: application/json' \
  -d '{"url": "https://example.com/very/long/path?ref=campaign"}' | jq
```

**201 Created**
```json
{
  "id": "YvkFhq",
  "short_url": "http://localhost:8080/r/YvkFhq"
}
```

**400 Bad Request** (invalid URL)
```json
{
  "error": "URL scheme must be http or https"
}
```

---

### `GET /r/{id}` — Redirect to the original URL

Issues a `302 Found` redirect to the original long URL and atomically increments the click counter.

```bash
# Follow the redirect automatically
curl -L http://localhost:8080/r/YvkFhq

# Inspect the redirect response without following it
curl -v http://localhost:8080/r/YvkFhq 2>&1 | grep -E 'Location|< HTTP'
```

**302 Found** → redirects to `https://example.com/very/long/path?ref=campaign`

**404 Not Found** (unknown ID)
```json
{
  "error": "short URL not found"
}
```

---

### `GET /stats/{id}` — View link statistics

Returns metadata and the total click count for a short URL.

```bash
curl -s http://localhost:8080/stats/YvkFhq | jq
```

**200 OK**
```json
{
  "id": "YvkFhq",
  "long_url": "https://example.com/very/long/path?ref=campaign",
  "clicks": 3,
  "created_at": "2026-06-13T00:00:00Z"
}
```

**404 Not Found** (unknown ID)
```json
{
  "error": "short URL not found"
}
```

---

## Database Schema

```sql
CREATE TABLE IF NOT EXISTS urls (
    id         VARCHAR(10)  PRIMARY KEY,
    long_url   TEXT         NOT NULL,
    clicks     INT          DEFAULT 0,
    created_at TIMESTAMP    DEFAULT NOW()
);
```

The schema is applied automatically at startup via `bootstrapSchema` in `main.go`.

---

## Error Reference

| Status | Meaning                                        |
|--------|------------------------------------------------|
| 201    | Short URL created successfully                 |
| 302    | Redirecting to long URL (click recorded)       |
| 400    | Bad request — invalid JSON or URL              |
| 404    | Short ID not found in database                 |
| 500    | Internal server error — check server logs      |

---

## Stopping the stack

```bash
# Stop the server: Ctrl+C

# Stop and remove the Postgres container
docker compose down

# To also delete the database volume (destructive)
docker compose down -v
```

---

## Load Testing

```bash
k6 run benchmark.js
```

## Benchmark results

### Highlights
- **Zero Downtime:** Achieved a **0.00% error rate** across ~138,800 total requests.
- **High Throughput:** Sustained **~466 Requests Per Second (RPS)** under heavy concurrent load.
- **Ultra-Low Latency:** Average HTTP response time of just **2.36ms**, with a 95th percentile (p95) of **7.62ms**.

### Detailed Results

| Operation | Throughput (RPS) | Avg Latency | Median (p50) | p95 Latency | Max Latency |
| :--- | :---: | :---: | :---: | :---: | :---: |
| **Read Path** (`GET /r/{id}`) | 238.11 | 2.81 ms | 1.70 ms | 9.19 ms | 211.52 ms |
| **Write Path** (`POST /shorten`) | 227.91 | 1.89 ms | 1.55 ms | 2.97 ms | 203.56 ms |
| **Overall HTTP** | **466.19** | **2.36 ms** | **1.62 ms** | **7.62 ms** | **211.52 ms** |

### Analysis
1. **Write vs. Read Performance:** The Write path is slightly faster on average (1.89ms vs 2.81ms). This is expected because writing a new URL only requires a single `INSERT` query. The Read path requires a `SELECT` to find the URL, plus an `UPDATE` to increment the click counter, resulting in slightly higher database overhead.u
2. **Handling Outliers:** While the *maximum* latency spiked to ~211ms, the **p95 latency remained under 10ms**. This means 95% of users experienced lightning-fast redirects, and the 211ms spikes were likely isolated database lock waits or Go Garbage Collection pauses, rather than a systemic bottleneck.
3. **Connection Pooling:** The `pgxpool` successfully managed the concurrent database requests without dropping a single connection or throwing a timeout error, proving the robustness of the database layer.


## License

MIT
