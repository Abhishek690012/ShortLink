# Project Report: ShortLink URL Shortener Service

ShortLink is a minimalist, production-ready, high-performance URL shortener service built in Go and backed by PostgreSQL. The service supports URL shortening, redirection with click tracking, and performance statistics.

---

## 1. Architecture Overview

The system is designed with a lightweight, database-backed architecture using a layered approach within Go's standard library. It avoids heavy third-party framework overhead, relying on standard Go library components for routing, networking, and request handling.

```mermaid
graph TD
    Client[HTTP Client] -->|HTTP Requests| Server[Go HTTP Server :8080]
    Server -->|Router| Mux[http.ServeMux]
    Mux -->|POST /shorten| ShortenHandler[ShortenURL Handler]
    Mux -->|GET /r/{id}| RedirectHandler[RedirectURL Handler]
    Mux -->|GET /stats/{id}| StatsHandler[GetStats Handler]
    
    ShortenHandler -->|pgxpool| DB[(PostgreSQL Database)]
    RedirectHandler -->|pgxpool| DB
    StatsHandler -->|pgxpool| DB
```

### Technical Stack
* **Language:** Go 1.25.0 (leveraging the standard library `net/http` routing)
* **Database:** PostgreSQL 15 (managed using connection pooling via `github.com/jackc/pgx/v5`)
* **Infrastructure:** Docker and Docker Compose for PostgreSQL containers
* **Configuration:** Environment-driven configuration via OS variables and `.env` loader (`github.com/joho/godotenv`)
* **Load Testing:** JS-based load tests powered by `k6`

---

## 2. Codebase Walkthrough

### 2.1 Entry Point: `main.go`
`main.go` acts as the orchestrator for the service, handling initialization, database connection management, schema migration, and server routing.
* **Environment Configuration:** Reads values using the helper function `getEnv()`, falling back to standard local development defaults (`localhost:5432`, user `shortlink`, etc.) if no `.env` file is present.
* **Database Connectivity:** Uses `pgxpool.New(ctx, dsn)` to construct a concurrency-safe connection pool, followed by a `Ping()` check with a 5-second timeout constraint to ensure database readiness.
* **Database Schema Bootstrap:** Runs a raw SQL script `createSchema` on startup (`bootstrapSchema`) to instantiate the `urls` table if it does not exist, keeping migrations simple and idempotent.
* **Routing Setup:** Uses the modern pattern-matching `http.ServeMux` available in Go 1.22+ to define strict HTTP methods and path variables:
  * `POST /shorten` -> `h.ShortenURL`
  * `GET /r/{id}` -> `h.RedirectURL`
  * `GET /stats/{id}` -> `h.GetStats`
  * `/` -> `notFound404`

### 2.2 Core Logic: `handlers.go`
`handlers.go` contains the dependency injection structure and the application endpoints.
* **Handler Context:** The `Handler` struct encapsulates the database connection pool (`*pgxpool.Pool`) and the `baseURL` string (used to formulate absolute redirection URLs), facilitating testing and modularity.
* **Base62 ID Generation:** Uses a 62-character alphabet (`a-zA-Z0-9`) to encode pseudo-random IDs of length 6. It utilizes the new standard library `math/rand/v2` module, which offers improved performance and security over the legacy `math/rand`.
* **Conflict Resolution (`uniqueShortID`):** To handle ID generation collisions, the system performs up to 5 retries. It checks if the ID exists in the database using a fast existence check (`SELECT EXISTS(SELECT 1 FROM urls WHERE id = $1)`). If a collision persists after 5 attempts, it aborts and returns an error response.

---

## 3. Database Schema

The database design relies on a single relational table optimized for fast lookups.

```sql
CREATE TABLE IF NOT EXISTS urls (
    id         VARCHAR(10)  PRIMARY KEY,
    long_url   TEXT         NOT NULL,
    clicks     INT          DEFAULT 0,
    created_at TIMESTAMP    DEFAULT NOW()
);
```

### Indexing and Storage Detail
* **Primary Key:** The short URL `id` is a primary key, which automatically generates a unique B-tree index in PostgreSQL. Lookups on `id` are extremely fast ($\mathcal{O}(\log N)$ operations).
* **Click Counter:** The `clicks` column tracks redirection volume and is updated atomically.

---

## 4. API Specification

### 4.1 Shorten URL
* **Endpoint:** `POST /shorten`
* **Content-Type:** `application/json`
* **Request Body:**
  ```json
  {
    "url": "https://example.com/very/long/path?ref=campaign"
  }
  ```
* **Validation Rules:**
  1. The JSON body must not contain unknown fields.
  2. The `url` field is required and must be non-empty.
  3. The URL must be absolute and use either `http` or `https` schemes.
* **Success Response (201 Created):**
  ```json
  {
    "id": "YvkFhq",
    "short_url": "http://localhost:8080/r/YvkFhq"
  }
  ```

### 4.2 Redirect Short URL
* **Endpoint:** `GET /r/{id}`
* **Path Parameters:** `id` (6-character short link identifier)
* **Response Details:**
  * Performs an atomic database update: `UPDATE urls SET clicks = clicks + 1 WHERE id = $1`.
  * Issues a standard HTTP `302 Found` redirect header: `Location: <longURL>`.
  * If the ID is missing or not found, it responds with `404 Not Found`.

### 4.3 Link Statistics
* **Endpoint:** `GET /stats/{id}`
* **Path Parameters:** `id` (6-character short link identifier)
* **Success Response (200 OK):**
  ```json
  {
    "id": "YvkFhq",
    "long_url": "https://example.com/very/long/path?ref=campaign",
    "clicks": 3,
    "created_at": "2026-06-13T00:00:00Z"
  }
  ```

---

## 5. Performance Benchmarks (k6 Load Test)

The project includes a robust load testing suite built with `k6` (`benchmark.js`) simulating write-heavy stress, read-heavy stress, and mixed workloads.

### Test Scenarios
1. **`write_stress` (Ramping VUs):** Gradually scales from 1 to 50 concurrent virtual users over 1.5 minutes, hitting `POST /shorten` repeatedly.
2. **`read_stress` (Ramping Arrival Rate):** Simulates constant arrival rates starting at 50/sec up to 300/sec, evaluating `GET /r/{id}` using pre-seeded IDs.
3. **`mixed_workload` (80% Reads / 20% Writes):** Simulates a balanced user interaction patterns under concurrent access.

### Performance Profile Results
Below is the summary of benchmark results compiled during execution:

| Metric | Measured Value | Analysis / Significance |
| :--- | :---: | :--- |
| **Error Rate** | **0.00%** | Zero failures observed out of 138,795 requests, proving network stability. |
| **Throughput (RPS)** | **466.19 RPS** | High sustained processing capability under resource-capped environments. |
| **Average Latency** | **2.36 ms** | Outstanding general response latency under concurrency. |
| **p95 Latency** | **7.62 ms** | 95% of requests completed in under 8 ms, indicating low tail latency. |
| **Read Latency (Avg)** | **2.81 ms** | Slightly higher than write latency due to the dual-query read flow (SELECT followed by UPDATE). |
| **Write Latency (Avg)** | **1.89 ms** | Single-query overhead (INSERT only), leading to ultra-fast average response. |

---

## 6. Recommendations & Scaling Strategies

To transition this service into a large-scale, production-grade URL shortener handling millions of requests daily, the following improvements are recommended:

1. **Implement Read Caching (Redis):**
   * *Problem:* Currently, every redirect request makes a `SELECT` and an `UPDATE` database call. Database connections are the ultimate bottleneck.
   * *Solution:* Cache the mappings (`id -> long_url`) in Redis. Redirection lookups can be served directly from memory ($\approx 10\times$ faster).
2. **Asynchronous Click Tracking:**
   * *Problem:* The redirection path waits for the database update (`UPDATE urls SET clicks = clicks + 1`) before returning the redirect to the client, adding latency.
   * *Solution:* Return the redirect to the user immediately, and write the click event to a queue (e.g., RabbitMQ, Kafka, or a buffered Go channel) to be processed asynchronously by worker pools.
3. **Optimized ID Generation (Distributed Snowflake IDs):**
   * *Problem:* Random base62 ID generation checks for collision using `SELECT EXISTS` on every request. At scale, collision probability rises, leading to multiple database roundtrips.
   * *Solution:* Use a counter-based or deterministic hashing algorithm (like Hashids) or a distributed sequence generator (like Snowflake) to generate guaranteed unique IDs without checking the database.
4. **Database Connection Pool Tuning:**
   * Configure the max connections and idle connection limits in `pgxpool` explicitly depending on the container or host server capacity.
