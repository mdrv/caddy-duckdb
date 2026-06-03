Is it possible to integrate Caddy with DuckDB and SurrealDB using modules (maybe through Unix sockets or WebSockets)? I want Caddy to be able to do CRUD operations.

---

## Caddy + DuckDB + SurrealDB Integration

Yes, this is entirely possible — but the approach differs per database due to their architectures. Here's a complete breakdown:

---

## Architecture Overview

```
Client
  │
  ▼
Caddy (reverse proxy / custom module)
  ├── Unix Socket ──► DuckDB sidecar service (Go, CGo)
  └── HTTP/WS ──────► SurrealDB (native HTTP + WS API)
```

---

## SurrealDB — Easiest Path

SurrealDB ships with a **native HTTP REST + WebSocket API**, so Caddy can integrate two ways:

### Option A: Caddy as a reverse proxy (no module needed)

```caddyfile
:8080 {
    # Proxy REST calls
    handle /db/* {
        reverse_proxy localhost:8000
    }

    # Proxy WebSocket (SurrealDB's live queries use WS)
    handle /rpc {
        reverse_proxy localhost:8000 {
            header_up Connection "Upgrade"
            header_up Upgrade "websocket"
        }
    }
}
```

### Option B: Custom Caddy module using the SurrealDB Go SDK

```go
package caddysurreal

import (
    "github.com/surrealdb/surrealdb.go"
    "github.com/caddyserver/caddy/v2"
    "github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

type SurrealHandler struct {
    db *surrealdb.DB
}

func (h SurrealHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
    switch r.Method {
    case http.MethodGet:
        data, err := h.db.Select("users")
        // ...
    case http.MethodPost:
        // h.db.Create(...)
    case http.MethodPut:
        // h.db.Update(...)
    case http.MethodDelete:
        // h.db.Delete(...)
    }
    return next.ServeHTTP(w, r)
}
```

---

## DuckDB — Requires a Sidecar or Embedded Module

DuckDB has **no built-in HTTP server** — it's embedded (like SQLite). You have two paths:

### Option A: Unix Socket sidecar (recommended)

Write a tiny Go service that wraps DuckDB and listens on a Unix socket, then Caddy proxies to it:

**`duckdb-server/main.go`**
```go
package main

import (
    "database/sql"
    "net"
    "net/http"
    _ "github.com/marcboeker/go-duckdb"
)

func main() {
    db, _ := sql.Open("duckdb", "/data/analytics.db")

    mux := http.NewServeMux()
    mux.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
        // Parse body, run query, return JSON
    })

    listener, _ := net.Listen("unix", "/run/duckdb.sock")
    http.Serve(listener, mux)
}
```

**Caddyfile:**
```caddyfile
:8080 {
    handle /analytics/* {
        reverse_proxy unix//run/duckdb.sock
    }
}
```

### Option B: Embed DuckDB directly in a Caddy module (CGo)

This compiles DuckDB into Caddy itself — powerful but requires CGo and a custom Caddy build via `xcaddy`:

```bash
xcaddy build \
    --with github.com/your-org/caddy-duckdb \
    --with github.com/your-org/caddy-surreal
```

```go
// Inside your caddy-duckdb module
import (
    "database/sql"
    _ "github.com/marcboeker/go-duckdb"  // CGo binding
)

func (h *DuckDBHandler) Provision(ctx caddy.Context) error {
    db, err := sql.Open("duckdb", h.DBPath)
    h.db = db
    return err
}
```

> ⚠️ CGo inside Caddy means you lose Caddy's pure-Go portability. Alpine/musl builds will break unless you handle it carefully.

---

## CRUD Routing Pattern (both DBs)

A clean module would map HTTP verbs to DB operations:

| HTTP Method | Route | Operation |
|---|---|---|
| `GET` | `/api/:table/:id?` | SELECT / SELECT by ID |
| `POST` | `/api/:table` | INSERT |
| `PUT` | `/api/:table/:id` | UPDATE |
| `DELETE` | `/api/:table/:id` | DELETE |

---

## Recommendation (working backwards from your goal)

| Goal | Best Path |
|---|---|
| CRUD on SurrealDB, fast setup | Caddy reverse proxy + SurrealDB HTTP API |
| Live queries / real-time | WebSocket proxy in Caddy → SurrealDB `/rpc` |
| DuckDB analytics queries | Unix socket sidecar (avoids CGo in Caddy) |
| Single binary, all in Caddy | `xcaddy` + custom modules (CGo, more effort) |

The **Unix socket sidecar for DuckDB + direct reverse proxy for SurrealDB** is the pragmatic production path. Writing a full embedded Caddy module makes sense if you want a single deployable binary or need tight middleware integration (auth, rate limiting, logging) applied *before* hitting the DB layer.

---

I'm fine with enabling CGO and embedding DuckDB (as long as it's lightweight).  Can you write comprehensive plan for Caddy & DuckDB integration? (my main purpose is for analytics and security/detecting abuses)

---

## Comprehensive Plan: Caddy + DuckDB Integration
### For Analytics & Abuse Detection

---

## 1. Goal Decomposition (Working Backwards)

```
END GOAL: Real-time analytics + abuse detection inside Caddy
    │
    ├── Abuse detection needs → fast pattern queries over recent requests
    │       └── requires → low-latency writes + window function queries
    │
    ├── Analytics needs → aggregations over large request history
    │       └── requires → columnar storage, batch inserts, OLAP queries
    │
    └── Both need → DuckDB embedded in Caddy via CGo, async write pipeline
```

DuckDB is *ideal* here — it's a columnar OLAP engine, meaning `GROUP BY`, `COUNT`, window functions, and time-series aggregations are extremely fast even over millions of rows. This is fundamentally different from using Postgres or SQLite for the same job.

---

## 2. Repository Structure

```
caddy-duckdb/
├── cmd/
│   └── main.go                  # xcaddy entry point
├── module/
│   ├── module.go                # Caddy module registration
│   ├── handler.go               # HTTP middleware handler
│   ├── provisioner.go           # Lifecycle: Provision/Cleanup
│   ├── config.go                # Caddyfile + JSON config parsing
│   └── query_api.go             # Internal /analytics REST API
├── db/
│   ├── client.go                # DuckDB connection pool
│   ├── schema.go                # DDL: table definitions
│   ├── writer.go                # Async batch write pipeline
│   └── queries.go               # Named analytics/abuse queries
├── detect/
│   ├── engine.go                # Abuse detection engine
│   ├── rules.go                 # Rule definitions
│   └── actions.go               # Responses: block, throttle, flag
├── Caddyfile.example
├── build.sh                     # xcaddy build with CGo flags
└── go.mod
```

---

## 3. Schema Design

This is the most critical architectural decision. DuckDB is **columnar** — you design schemas for aggregation, not normalization.

```sql
-- ─────────────────────────────────────────────
-- Core request log (append-only, high volume)
-- ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS requests (
    id            UHUGEINT DEFAULT nextval('req_seq'),
    ts            TIMESTAMPTZ NOT NULL,          -- request timestamp
    ip            VARCHAR     NOT NULL,          -- client IP (raw)
    ip_int        UBIGINT     NOT NULL,          -- IP as integer (fast range queries)
    method        VARCHAR(8)  NOT NULL,
    host          VARCHAR     NOT NULL,
    path          VARCHAR     NOT NULL,
    path_norm     VARCHAR     NOT NULL,          -- lowercased, stripped query params
    query         VARCHAR,
    status        USMALLINT   NOT NULL,
    latency_ms    UINTEGER    NOT NULL,          -- response time
    bytes_sent    UBIGINT     NOT NULL,
    referer       VARCHAR,
    user_agent    VARCHAR,
    ua_browser    VARCHAR,                       -- parsed from UA
    ua_os         VARCHAR,
    is_bot        BOOLEAN     DEFAULT false,
    country_code  CHAR(2),                       -- from IP geo (optional)
    tls_version   VARCHAR(8),
    http_version  VARCHAR(4),
    rule_matched  VARCHAR                        -- abuse rule that fired, if any
);

-- Partition hint: DuckDB handles this via row group sorting
-- Always insert in ts order — enables zone map pruning

-- ─────────────────────────────────────────────
-- IP reputation / block list (small, hot)
-- ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ip_reputation (
    ip            VARCHAR  PRIMARY KEY,
    score         INTEGER  DEFAULT 0,       -- higher = more suspicious
    flags         VARCHAR[],                -- ['brute_force', 'scanner', ...]
    first_seen    TIMESTAMPTZ,
    last_seen     TIMESTAMPTZ,
    req_count     UBIGINT  DEFAULT 0,
    blocked       BOOLEAN  DEFAULT false,
    blocked_until TIMESTAMPTZ,
    notes         VARCHAR
);

-- ─────────────────────────────────────────────
-- Abuse events (fired when rules trigger)
-- ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS abuse_events (
    id         UHUGEINT DEFAULT nextval('event_seq'),
    ts         TIMESTAMPTZ NOT NULL,
    ip         VARCHAR NOT NULL,
    rule       VARCHAR NOT NULL,            -- rule name that fired
    severity   UTINYINT NOT NULL,           -- 1=info 2=warn 3=block
    context    JSON,                        -- arbitrary rule context
    resolved   BOOLEAN DEFAULT false
);

-- ─────────────────────────────────────────────
-- Materialized analytics (refreshed periodically)
-- ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS hourly_stats AS
    SELECT
        time_bucket(INTERVAL '1 hour', ts) AS hour,
        host,
        COUNT(*)                            AS total_reqs,
        COUNT(DISTINCT ip)                  AS unique_ips,
        AVG(latency_ms)                     AS avg_latency,
        PERCENTILE_CONT(0.95)
            WITHIN GROUP (ORDER BY latency_ms) AS p95_latency,
        SUM(bytes_sent)                     AS bytes_total,
        COUNT(*) FILTER (WHERE status >= 400) AS errors,
        COUNT(*) FILTER (WHERE status >= 500) AS server_errors
    FROM requests
    GROUP BY ALL
    WITH NO DATA;
```

**Why `ip_int`?** CIDR block queries like "block all 192.168.1.0/24" become simple integer range checks — orders of magnitude faster than `LIKE` on string IP.

---

## 4. Module Registration & Config

```go
// module/module.go
package caddyduckdb

import "github.com/caddyserver/caddy/v2"

func init() {
    caddy.RegisterModule(Middleware{})
}

type Middleware struct {
    // Caddyfile / JSON config fields
    DBPath          string          `json:"db_path"`
    BatchSize       int             `json:"batch_size"`        // default 500
    FlushIntervalMs int             `json:"flush_interval_ms"` // default 200
    QueryAPIPath    string          `json:"query_api_path"`    // e.g. "/_analytics"
    QueryAPIToken   string          `json:"query_api_token"`
    AbuseDetection  AbuseConfig     `json:"abuse_detection"`
    GeoLite2Path    string          `json:"geolite2_path"`     // optional mmdb file

    // internal
    db     *db.Client
    writer *db.BatchWriter
    engine *detect.Engine
}

func (Middleware) CaddyModule() caddy.ModuleInfo {
    return caddy.ModuleInfo{
        ID:  "http.handlers.duckdb_analytics",
        New: func() caddy.Module { return new(Middleware) },
    }
}
```

```caddyfile
# Caddyfile.example
:443 {
    duckdb_analytics {
        db_path          /var/lib/caddy/analytics.db
        batch_size       500
        flush_interval   200ms
        query_api_path   /_internal/analytics
        query_api_token  {env.ANALYTICS_TOKEN}

        abuse_detection {
            rate_limit_window  60s
            rate_limit_max     300           # reqs/IP/window
            burst_threshold    50            # reqs in 5s
            path_scan_threshold 40           # distinct 404 paths/IP/min
            brute_force_paths  /login /admin /wp-login.php
            brute_force_max    10            # POST failures/IP/5min
            score_block_at     80            # auto-block score threshold
        }
    }

    reverse_proxy localhost:3000
}
```

---

## 5. Async Batch Write Pipeline

**This is the most important performance decision.** Synchronous DuckDB writes per-request would add ~1–5ms latency. Instead, buffer writes in a channel and flush in batches using `COPY` or `INSERT INTO ... VALUES (batch)`.

```go
// db/writer.go
package db

import (
    "context"
    "database/sql"
    "strings"
    "sync"
    "time"
)

type RequestRecord struct {
    TS         time.Time
    IP         string
    IPInt      uint64
    Method     string
    Host       string
    Path       string
    PathNorm   string
    Query      string
    Status     int
    LatencyMs  int64
    BytesSent  int64
    Referer    string
    UserAgent  string
    UABrowser  string
    UAOS       string
    IsBot      bool
    Country    string
    TLSVersion string
    HTTPVer    string
    RuleMatch  string
}

type BatchWriter struct {
    db          *sql.DB
    ch          chan RequestRecord
    batchSize   int
    flushTicker *time.Ticker
    mu          sync.Mutex
    buf         []RequestRecord
    wg          sync.WaitGroup
}

func NewBatchWriter(db *sql.DB, batchSize int, flushInterval time.Duration) *BatchWriter {
    w := &BatchWriter{
        db:          db,
        ch:          make(chan RequestRecord, 8192), // ring buffer headroom
        batchSize:   batchSize,
        flushTicker: time.NewTicker(flushInterval),
        buf:         make([]RequestRecord, 0, batchSize),
    }
    w.wg.Add(1)
    go w.run()
    return w
}

func (w *BatchWriter) Write(r RequestRecord) {
    select {
    case w.ch <- r:
    default:
        // Channel full — drop or sample. For abuse detection,
        // prefer dropping analytics over blocking the request path.
    }
}

func (w *BatchWriter) run() {
    defer w.wg.Done()
    for {
        select {
        case r := <-w.ch:
            w.buf = append(w.buf, r)
            if len(w.buf) >= w.batchSize {
                w.flush()
            }
        case <-w.flushTicker.C:
            if len(w.buf) > 0 {
                w.flush()
            }
        }
    }
}

func (w *BatchWriter) flush() {
    if len(w.buf) == 0 {
        return
    }

    // Build a single multi-row INSERT — much faster than N individual inserts
    sb := strings.Builder{}
    sb.WriteString(`INSERT INTO requests
        (ts, ip, ip_int, method, host, path, path_norm, query,
         status, latency_ms, bytes_sent, referer, user_agent,
         ua_browser, ua_os, is_bot, country_code, tls_version,
         http_version, rule_matched)
        VALUES `)

    args := make([]any, 0, len(w.buf)*20)
    placeholders := make([]string, 0, len(w.buf))
    i := 1
    for _, r := range w.buf {
        placeholders = append(placeholders, buildPlaceholder(i, 20))
        args = append(args,
            r.TS, r.IP, r.IPInt, r.Method, r.Host,
            r.Path, r.PathNorm, r.Query, r.Status, r.LatencyMs,
            r.BytesSent, r.Referer, r.UserAgent, r.UABrowser,
            r.UAOS, r.IsBot, r.Country, r.TLSVersion,
            r.HTTPVer, r.RuleMatch,
        )
        i += 20
    }
    sb.WriteString(strings.Join(placeholders, ","))

    _, err := w.db.ExecContext(context.Background(), sb.String(), args...)
    if err != nil {
        // log error, consider dead-letter queue
    }

    w.buf = w.buf[:0]
}
```

---

## 6. Handler Middleware

```go
// module/handler.go
func (m *Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
    start := time.Now()
    rec := caddyhttp.NewResponseRecorder(w, nil, nil)

    // ── Pre-request: abuse check (fast path, in-memory) ──────────
    ip := realIP(r)
    if m.engine.IsBlocked(ip) {
        http.Error(w, "Forbidden", http.StatusForbidden)
        return nil
    }

    err := next.ServeHTTP(rec, r)

    latency := time.Since(start).Milliseconds()

    // ── Build record (non-blocking) ───────────────────────────────
    record := RequestRecord{
        TS:        time.Now().UTC(),
        IP:        ip,
        IPInt:     ipToUint64(ip),
        Method:    r.Method,
        Host:      r.Host,
        Path:      r.URL.Path,
        PathNorm:  normalizePath(r.URL.Path),
        Query:     r.URL.RawQuery,
        Status:    rec.Status(),
        LatencyMs: latency,
        BytesSent: int64(rec.Size()),
        Referer:   r.Referer(),
        UserAgent: r.UserAgent(),
        // ... UA parsing, geo lookup ...
    }

    // ── Post-request: async write (never blocks response) ─────────
    m.writer.Write(record)

    // ── Post-request: abuse scoring (async goroutine) ─────────────
    go m.engine.Score(record)

    return err
}
```

---

## 7. Abuse Detection Engine

The engine runs **two tiers**:

- **Tier 1 (in-memory, nanoseconds):** Sliding window counters per IP using a `sync.Map` + atomic counters. Fires before the DB write.
- **Tier 2 (DuckDB queries, seconds):** Pattern queries run on a schedule (every 30s–5min). Catches sophisticated slow-burn attacks.

```go
// detect/engine.go

type Engine struct {
    db        *db.Client
    blocked   sync.Map               // ip → blockedUntil time.Time
    counters  sync.Map               // ip → *WindowCounter
    rules     []Rule
    cfg       AbuseConfig
}

// ── Tier 1: In-memory sliding window ──────────────────────────
func (e *Engine) Score(r db.RequestRecord) {
    ctr := e.getOrCreateCounter(r.IP)
    ctr.Add(r)

    score := 0

    // Rule: request rate
    if ctr.Rate(60*time.Second) > float64(e.cfg.RateLimitMax) {
        score += 30
        e.fireEvent(r.IP, "rate_limit_exceeded", 2, nil)
    }

    // Rule: burst (spike in 5s)
    if ctr.Rate(5*time.Second) > float64(e.cfg.BurstThreshold) {
        score += 40
        e.fireEvent(r.IP, "burst_detected", 3, nil)
    }

    // Rule: repeated auth failures
    if isBruteForce(r) {
        score += 25
        e.fireEvent(r.IP, "brute_force", 3, nil)
    }

    if score >= e.cfg.ScoreBlockAt {
        e.Block(r.IP, 15*time.Minute)
    }
}

// ── Tier 2: DuckDB pattern queries ────────────────────────────
func (e *Engine) RunDeepScan(ctx context.Context) {
    // Horizontal scan: many distinct paths, mostly 404
    e.runRule(ctx, `
        SELECT ip, COUNT(DISTINCT path_norm) AS paths, COUNT(*) AS reqs
        FROM requests
        WHERE ts > NOW() - INTERVAL '5 minutes'
          AND status = 404
        GROUP BY ip
        HAVING paths > $1
    `, e.cfg.PathScanThreshold, "path_scanner", 3)

    // Credential stuffing: POST to auth paths, high failure rate
    e.runRule(ctx, `
        SELECT ip,
               COUNT(*) FILTER (WHERE status IN (401, 403)) AS failures,
               COUNT(*) AS total
        FROM requests
        WHERE ts > NOW() - INTERVAL '5 minutes'
          AND path_norm = ANY($1)
          AND method = 'POST'
        GROUP BY ip
        HAVING failures > $2
          AND failures::FLOAT / NULLIF(total,0) > 0.7
    `, e.cfg.BruteForcePaths, e.cfg.BruteForceMax, "credential_stuffing", 3)

    // Data exfiltration: unusually high outbound bytes
    e.runRule(ctx, `
        SELECT ip, SUM(bytes_sent) AS total_bytes
        FROM requests
        WHERE ts > NOW() - INTERVAL '10 minutes'
        GROUP BY ip
        HAVING total_bytes > 100_000_000   -- 100MB threshold
    `, "data_exfil", 2)

    // Slow loris / low-rate DoS: very high latency on specific IP
    e.runRule(ctx, `
        SELECT ip, AVG(latency_ms) AS avg_lat, COUNT(*) AS cnt
        FROM requests
        WHERE ts > NOW() - INTERVAL '2 minutes'
        GROUP BY ip
        HAVING avg_lat > 5000 AND cnt > 10
    `, "slow_loris", 2)

    // UA anomaly: too many distinct UAs from one IP (rotation)
    e.runRule(ctx, `
        SELECT ip, COUNT(DISTINCT user_agent) AS ua_count
        FROM requests
        WHERE ts > NOW() - INTERVAL '5 minutes'
        GROUP BY ip
        HAVING ua_count > 8
    `, "ua_rotation", 1)
}
```

---

## 8. Query API (Internal Analytics Endpoint)

Protected by a bearer token, mounted at a configurable path:

```go
// module/query_api.go

// GET /_internal/analytics/summary?from=2024-01-01&to=2024-01-02&host=example.com
// GET /_internal/analytics/top_ips?limit=20&window=1h
// GET /_internal/analytics/abuse_events?severity=3
// GET /_internal/analytics/latency_percentiles?host=example.com&window=24h
// POST /_internal/analytics/query   (raw SQL — for trusted internal use only)

var analyticsQueries = map[string]string{

    "summary": `
        SELECT
            COUNT(*)                                   AS total_requests,
            COUNT(DISTINCT ip)                         AS unique_visitors,
            ROUND(AVG(latency_ms), 2)                  AS avg_latency_ms,
            PERCENTILE_CONT(0.99)
                WITHIN GROUP (ORDER BY latency_ms)     AS p99_latency_ms,
            SUM(bytes_sent)                            AS bytes_total,
            COUNT(*) FILTER (WHERE status >= 400)      AS client_errors,
            COUNT(*) FILTER (WHERE status >= 500)      AS server_errors,
            COUNT(*) FILTER (WHERE is_bot)             AS bot_requests
        FROM requests
        WHERE ts BETWEEN $1 AND $2
          AND ($3 = '' OR host = $3)
    `,

    "top_ips": `
        SELECT
            ip,
            country_code,
            COUNT(*)                                  AS req_count,
            COUNT(*) FILTER (WHERE status >= 400)     AS errors,
            MAX(ts)                                   AS last_seen,
            BOOL_OR(rule_matched IS NOT NULL)         AS has_violations
        FROM requests
        WHERE ts > NOW() - $1::INTERVAL
        GROUP BY ip, country_code
        ORDER BY req_count DESC
        LIMIT $2
    `,

    "traffic_over_time": `
        SELECT
            time_bucket(INTERVAL $1, ts)              AS bucket,
            COUNT(*)                                  AS requests,
            COUNT(DISTINCT ip)                        AS unique_ips,
            AVG(latency_ms)                           AS avg_latency
        FROM requests
        WHERE ts > NOW() - $2::INTERVAL
        GROUP BY bucket
        ORDER BY bucket
    `,

    "status_breakdown": `
        SELECT
            status // 100 * 100                       AS status_class,
            COUNT(*)                                  AS count,
            ROUND(COUNT(*) * 100.0 / SUM(COUNT(*))
                OVER (), 2)                           AS pct
        FROM requests
        WHERE ts > NOW() - $1::INTERVAL
        GROUP BY status_class
        ORDER BY status_class
    `,
}
```

---

## 9. Build Process

```bash
# build.sh
#!/usr/bin/env bash
set -euo pipefail

# Install xcaddy if not present
which xcaddy || go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

# CGo is required for go-duckdb
export CGO_ENABLED=1

# Optional: point to a pre-built libduckdb to avoid
# recompiling DuckDB from source (saves ~5min build time)
# export CGO_CFLAGS="-I/usr/local/include"
# export CGO_LDFLAGS="-L/usr/local/lib -lduckdb"

xcaddy build \
    --with github.com/your-org/caddy-duckdb=./  \
    --output ./caddy-analytics

echo "Build complete: ./caddy-analytics"
```

**Binary size note:** `go-duckdb` statically links libduckdb (~40MB). The final binary will be ~50–60MB — acceptable for a server binary, and still a single artifact with no runtime dependencies.

---

## 10. Maintenance & Data Lifecycle

```go
// Background goroutine in Provision()
func (m *Middleware) startMaintenance(ctx context.Context) {
    ticker := time.NewTicker(6 * time.Hour)
    go func() {
        for {
            select {
            case <-ticker.C:
                // 1. Checkpoint WAL
                m.db.Exec("CHECKPOINT")

                // 2. Purge old requests (keep 90 days)
                m.db.Exec(`
                    DELETE FROM requests
                    WHERE ts < NOW() - INTERVAL '90 days'
                `)

                // 3. Refresh materialized hourly stats
                m.db.Exec(`
                    INSERT INTO hourly_stats
                    SELECT ... FROM requests
                    WHERE ts > (SELECT MAX(hour) FROM hourly_stats)
                    GROUP BY ALL
                `)

                // 4. Expire old IP blocks
                m.db.Exec(`
                    UPDATE ip_reputation
                    SET blocked = false
                    WHERE blocked = true
                      AND blocked_until < NOW()
                `)
            case <-ctx.Done():
                return
            }
        }
    }()
}
```

---

## 11. Implementation Phases

| Phase | Deliverable | Complexity |
|---|---|---|
| **1** | Schema + batch writer + basic request logging | Low |
| **2** | Tier 1 in-memory abuse detection (rate limits, bursts) | Low |
| **3** | Query API + summary/top_ips endpoints | Medium |
| **4** | Tier 2 DuckDB pattern queries (deep scan) | Medium |
| **5** | UA parsing + GeoIP + bot detection | Medium |
| **6** | Materialized stats + maintenance lifecycle | Low |
| **7** | Grafana/dashboard integration (Parquet export or HTTP endpoint) | Optional |

Phase 1–3 gives you a production-viable system. Phase 4 is where the DuckDB-specific power really shows — running complex window-function abuse queries over millions of rows in milliseconds is something no row-store would handle gracefully at this scale.

---

USER: Great write-up, but I also need the module to expose flexible configuration and be able to execute DuckDB custom queries written in Caddyfile. Is that possible? Append the guide with more comprehensive covering.

---

## Appendix: Flexible Configuration & Caddyfile-Native Custom Queries

---

## 12. Config Design Philosophy (Working Backwards)

```
END GOAL: Write a query once in Caddyfile → it becomes a live HTTP endpoint
    │
    ├── Queries need → named registry, parameter binding, output shaping
    ├── Routes need  → match a query by name, bind request context to params
    ├── Config needs → env vars, validation, hot reload without restart
    └── All of this  → requires a layered config parser + JSON schema
```

The key insight is that Caddy already separates **config parsing** (`UnmarshalCaddyfile`) from **provisioning** (`Provision`). We exploit this to build a mini query-DSL that lives entirely inside the Caddyfile.

---

## 13. Full Configuration Schema

First, the complete Go config struct — this is the single source of truth that both JSON and Caddyfile parsing target:

```go
// module/config.go
package caddyduckdb

import (
    "time"
    "github.com/caddyserver/caddy/v2"
)

// ── Top-level module config ────────────────────────────────────────────

type Middleware struct {
    // Database
    DB DBConfig `json:"db"`

    // Write pipeline
    Writer WriterConfig `json:"writer"`

    // Query API (internal REST endpoint)
    QueryAPI QueryAPIConfig `json:"query_api,omitempty"`

    // Named custom queries defined inline in Caddyfile
    Queries []QueryDef `json:"queries,omitempty"`

    // Route-level query bindings (match path → run query)
    Routes []QueryRoute `json:"routes,omitempty"`

    // Abuse detection
    Abuse AbuseConfig `json:"abuse,omitempty"`

    // Logging
    Log LogConfig `json:"log,omitempty"`

    // internal
    db     *db.Client
    writer *db.BatchWriter
    engine *detect.Engine
    reg    *QueryRegistry
}

// ── Database config ────────────────────────────────────────────────────

type DBConfig struct {
    // Path to .db file. ":memory:" for ephemeral.
    Path string `json:"path"`

    // Max concurrent readers (DuckDB supports parallel reads)
    MaxReaders int `json:"max_readers,omitempty"` // default 4

    // Read-only mode (useful for a replica/analytics-only instance)
    ReadOnly bool `json:"read_only,omitempty"`

    // DuckDB PRAGMA / SET options applied at connect time
    Settings map[string]string `json:"settings,omitempty"`
    // e.g. {"threads": "4", "memory_limit": "1GB", "temp_directory": "/tmp/duckdb"}

    // WAL checkpoint interval
    CheckpointInterval caddy.Duration `json:"checkpoint_interval,omitempty"` // default 6h

    // Retention: delete requests older than this
    Retention caddy.Duration `json:"retention,omitempty"` // default 90d

    // Path to GeoLite2-Country.mmdb (optional)
    GeoIPPath string `json:"geoip_path,omitempty"`
}

// ── Writer config ──────────────────────────────────────────────────────

type WriterConfig struct {
    // How many records to accumulate before flushing
    BatchSize int `json:"batch_size,omitempty"` // default 500

    // Max time between flushes regardless of batch size
    FlushInterval caddy.Duration `json:"flush_interval,omitempty"` // default 200ms

    // Channel capacity (requests buffered before dropping)
    BufferSize int `json:"buffer_size,omitempty"` // default 8192

    // What to do when buffer is full: "drop" | "block" | "sample"
    OverflowPolicy string `json:"overflow_policy,omitempty"` // default "drop"

    // Log every Nth request (sampling for very high traffic)
    // 1 = log everything, 10 = log 1 in 10
    SampleRate int `json:"sample_rate,omitempty"` // default 1

    // Fields to exclude from logging (privacy / GDPR)
    ExcludeFields []string `json:"exclude_fields,omitempty"`
    // e.g. ["query", "referer", "user_agent"]

    // Paths to never log
    ExcludePaths []string `json:"exclude_paths,omitempty"`
    // e.g. ["/health", "/metrics", "/_internal/*"]
}

// ── Query API config ───────────────────────────────────────────────────

type QueryAPIConfig struct {
    // Mount path for the built-in analytics API
    Path string `json:"path,omitempty"` // default "/_duckdb"

    // Bearer token required in Authorization header
    Token string `json:"token,omitempty"`

    // Allow raw SQL POST (only for trusted internal networks)
    AllowRawSQL bool `json:"allow_raw_sql,omitempty"` // default false

    // CORS origin for browser-based dashboards
    CORSOrigin string `json:"cors_origin,omitempty"`

    // Max rows returned by any query
    MaxRows int `json:"max_rows,omitempty"` // default 10000

    // Query timeout
    Timeout caddy.Duration `json:"timeout,omitempty"` // default 30s
}

// ── Named query definition ─────────────────────────────────────────────

type QueryDef struct {
    // Unique name — referenced in routes and query API
    Name string `json:"name"`

    // The SQL. Use $param_name for named params (see ParamSources).
    SQL string `json:"sql"`

    // Where each $param comes from in the HTTP request
    Params []ParamBinding `json:"params,omitempty"`

    // How to shape the output
    Output OutputConfig `json:"output,omitempty"`

    // Cache this query's result for N duration
    CacheTTL caddy.Duration `json:"cache_ttl,omitempty"`

    // Optional: run this query on a schedule (cron-style)
    // Result is stored in memory and served without hitting DuckDB
    Schedule string `json:"schedule,omitempty"` // e.g. "*/5 * * * *"

    // Timeout override for this query
    Timeout caddy.Duration `json:"timeout,omitempty"`
}

// ── Parameter binding ──────────────────────────────────────────────────

type ParamBinding struct {
    // Name matches $name in SQL
    Name string `json:"name"`

    // Source: "query" | "path" | "header" | "body" | "placeholder" | "env"
    Source string `json:"source"`

    // Key within the source (e.g. "limit" for ?limit=50)
    Key string `json:"key"`

    // Type coercion: "string" | "int" | "float" | "bool" | "duration" | "timestamp"
    Type string `json:"type,omitempty"` // default "string"

    // Default value if the source key is absent
    Default string `json:"default,omitempty"`

    // Validation: min/max for numeric, regex for string
    Min     *float64 `json:"min,omitempty"`
    Max     *float64 `json:"max,omitempty"`
    Pattern string   `json:"pattern,omitempty"`

    // Cap numeric params to prevent abuse (e.g. limit=999999)
    Cap *float64 `json:"cap,omitempty"`
}

// ── Output shaping ─────────────────────────────────────────────────────

type OutputConfig struct {
    // "json" | "json_array" | "csv" | "ndjson" | "parquet"
    Format string `json:"format,omitempty"` // default "json"

    // Wrap result in {"data": [...], "meta": {...}}
    Envelope bool `json:"envelope,omitempty"` // default true

    // Rename columns in output
    ColumnAliases map[string]string `json:"column_aliases,omitempty"`

    // Omit these columns from output
    OmitColumns []string `json:"omit_columns,omitempty"`

    // HTTP response code when result is empty
    EmptyStatus int `json:"empty_status,omitempty"` // default 200

    // Write Content-Disposition: attachment for download
    Attachment bool `json:"attachment,omitempty"`

    // Filename for attachments (supports Go template: "export-{{.date}}.csv")
    Filename string `json:"filename,omitempty"`
}

// ── Route-level query binding ──────────────────────────────────────────

type QueryRoute struct {
    // HTTP method: "GET" | "POST" | "PUT" | "DELETE" | "*"
    Method string `json:"method"`

    // Path pattern (Caddy matcher syntax)
    Path string `json:"path"`

    // Query name to execute (must exist in Queries[])
    QueryName string `json:"query_name"`

    // Auth override for this route (separate from QueryAPI token)
    RequireHeader map[string]string `json:"require_header,omitempty"`

    // Rate limit this specific route (independent from abuse detection)
    RateLimit *RoutRateLimit `json:"rate_limit,omitempty"`
}

type RoutRateLimit struct {
    Requests int            `json:"requests"`
    Window   caddy.Duration `json:"window"`
}

// ── Abuse config ───────────────────────────────────────────────────────

type AbuseConfig struct {
    Enabled bool `json:"enabled,omitempty"`

    // Tier 1 — in-memory
    RateLimitWindow   caddy.Duration `json:"rate_limit_window,omitempty"`   // default 60s
    RateLimitMax      int            `json:"rate_limit_max,omitempty"`       // default 300
    BurstWindow       caddy.Duration `json:"burst_window,omitempty"`         // default 5s
    BurstThreshold    int            `json:"burst_threshold,omitempty"`      // default 50
    BruteForcePaths   []string       `json:"brute_force_paths,omitempty"`
    BruteForceMax     int            `json:"brute_force_max,omitempty"`      // default 10
    PathScanThreshold int            `json:"path_scan_threshold,omitempty"` // default 40

    // Tier 2 — DuckDB deep scan
    DeepScanInterval caddy.Duration `json:"deep_scan_interval,omitempty"` // default 30s

    // Custom abuse queries (same QueryDef format, but fire → abuse_events)
    CustomRules []AbuseRule `json:"custom_rules,omitempty"`

    // Score at which an IP is auto-blocked
    ScoreBlockAt int `json:"score_block_at,omitempty"` // default 80

    // How long auto-blocks last
    BlockDuration caddy.Duration `json:"block_duration,omitempty"` // default 15m

    // IPs/CIDRs that are never blocked
    Allowlist []string `json:"allowlist,omitempty"`

    // Action when blocked: "block" | "throttle" | "flag"
    BlockAction string `json:"block_action,omitempty"` // default "block"

    // HTTP status to return when blocked
    BlockStatus int `json:"block_status,omitempty"` // default 403
}

type AbuseRule struct {
    Name     string `json:"name"`
    SQL      string `json:"sql"`       // must return rows with at least an "ip" column
    Severity int    `json:"severity"`  // 1=info 2=warn 3=block
    Score    int    `json:"score"`     // score to add to ip_reputation
    Interval caddy.Duration `json:"interval"`
}

// ── Log config ─────────────────────────────────────────────────────────

type LogConfig struct {
    // "debug" | "info" | "warn" | "error"
    Level string `json:"level,omitempty"` // default "info"

    // Log slow queries (DuckDB tier 2 scans) above this threshold
    SlowQueryMs int `json:"slow_query_ms,omitempty"` // default 500

    // Emit a metric to Caddy's metrics endpoint
    Metrics bool `json:"metrics,omitempty"` // default true
}
```

---

## 14. Caddyfile DSL — Full Grammar

The goal is a Caddyfile block that reads naturally, with the full power of the config struct above accessible inline.

```
duckdb_analytics {
    # ── Database ────────────────────────────────────────────────
    db_path    <path>
    db_setting <key> <value>      # repeatable
    read_only  [true|false]
    retention  <duration>
    geoip      <path_to_mmdb>

    # ── Write pipeline ──────────────────────────────────────────
    batch_size      <int>
    flush_interval  <duration>
    buffer_size     <int>
    overflow        [drop|block|sample]
    sample_rate     <int>
    exclude_field   <field>       # repeatable
    exclude_path    <pattern>     # repeatable

    # ── Built-in query API ──────────────────────────────────────
    api_path    <path>
    api_token   <token>
    raw_sql     [on|off]
    max_rows    <int>
    cors_origin <origin>

    # ── Named queries (repeatable block) ────────────────────────
    query <name> {
        sql `<SQL statement>`

        param <name> {
            from     [query|path|header|body|env]
            key      <key>
            type     [string|int|float|bool|duration|timestamp]
            default  <value>
            min      <number>
            max      <number>
            cap      <number>
            pattern  <regex>
        }

        output {
            format   [json|json_array|csv|ndjson|parquet]
            envelope [on|off]
            alias    <col> <alias>    # repeatable
            omit     <col>            # repeatable
            empty_status <int>
            attachment [on|off]
            filename   <template>
        }

        cache   <duration>
        schedule <cron>
        timeout <duration>
    }

    # ── Route bindings (repeatable block) ───────────────────────
    route [GET|POST|PUT|DELETE|*] <path> <query_name> {
        require_header <name> <value>   # optional
        rate_limit <n> per <duration>   # optional
    }

    # ── Abuse detection ─────────────────────────────────────────
    abuse {
        rate_limit     <max> per <window>
        burst          <max> in <window>
        brute_force    <path> [<path>...] max <n>
        path_scan      max <n>
        deep_scan      every <duration>
        score_block_at <score>
        block_for      <duration>
        block_action   [block|throttle|flag]
        block_status   <int>
        allowlist      <ip_or_cidr>  # repeatable

        # Custom abuse rule (repeatable block)
        rule <name> {
            sql      `<SQL returning ip column>`
            severity [1|2|3]
            score    <int>
            every    <duration>
        }
    }
}
```

---

## 15. Caddyfile Parser Implementation

```go
// module/config.go  (continued)

func (m *Middleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
    // consume the directive name token
    d.Next()

    for d.NextBlock(0) {
        switch d.Val() {

        // ── Database ──────────────────────────────────────────────────
        case "db_path":
            if !d.NextArg() {
                return d.ArgErr()
            }
            m.DB.Path = d.Val()

        case "db_setting":
            key, val, err := twoArgs(d)
            if err != nil { return err }
            if m.DB.Settings == nil {
                m.DB.Settings = make(map[string]string)
            }
            m.DB.Settings[key] = val

        case "read_only":
            m.DB.ReadOnly = boolArg(d)

        case "retention":
            dur, err := durationArg(d)
            if err != nil { return err }
            m.DB.Retention = caddy.Duration(dur)

        case "geoip":
            if !d.NextArg() { return d.ArgErr() }
            m.DB.GeoIPPath = d.Val()

        // ── Writer ────────────────────────────────────────────────────
        case "batch_size":
            n, err := intArg(d)
            if err != nil { return err }
            m.Writer.BatchSize = n

        case "flush_interval":
            dur, err := durationArg(d)
            if err != nil { return err }
            m.Writer.FlushInterval = caddy.Duration(dur)

        case "buffer_size":
            n, err := intArg(d)
            if err != nil { return err }
            m.Writer.BufferSize = n

        case "overflow":
            if !d.NextArg() { return d.ArgErr() }
            policy := d.Val()
            if policy != "drop" && policy != "block" && policy != "sample" {
                return d.Errf("overflow must be drop, block, or sample; got %q", policy)
            }
            m.Writer.OverflowPolicy = policy

        case "sample_rate":
            n, err := intArg(d)
            if err != nil { return err }
            m.Writer.SampleRate = n

        case "exclude_field":
            if !d.NextArg() { return d.ArgErr() }
            m.Writer.ExcludeFields = append(m.Writer.ExcludeFields, d.Val())

        case "exclude_path":
            if !d.NextArg() { return d.ArgErr() }
            m.Writer.ExcludePaths = append(m.Writer.ExcludePaths, d.Val())

        // ── Query API ─────────────────────────────────────────────────
        case "api_path":
            if !d.NextArg() { return d.ArgErr() }
            m.QueryAPI.Path = d.Val()

        case "api_token":
            if !d.NextArg() { return d.ArgErr() }
            m.QueryAPI.Token = d.Val()

        case "raw_sql":
            m.QueryAPI.AllowRawSQL = boolArg(d)

        case "max_rows":
            n, err := intArg(d)
            if err != nil { return err }
            m.QueryAPI.MaxRows = n

        case "cors_origin":
            if !d.NextArg() { return d.ArgErr() }
            m.QueryAPI.CORSOrigin = d.Val()

        // ── Named query blocks ─────────────────────────────────────────
        case "query":
            qd, err := parseQueryDef(d)
            if err != nil { return err }
            m.Queries = append(m.Queries, qd)

        // ── Route binding blocks ───────────────────────────────────────
        case "route":
            r, err := parseQueryRoute(d)
            if err != nil { return err }
            m.Routes = append(m.Routes, r)

        // ── Abuse block ────────────────────────────────────────────────
        case "abuse":
            if err := parseAbuseConfig(d, &m.Abuse); err != nil {
                return err
            }

        default:
            return d.Errf("unknown directive: %s", d.Val())
        }
    }
    return nil
}

// ── Query block parser ─────────────────────────────────────────────────

func parseQueryDef(d *caddyfile.Dispenser) (QueryDef, error) {
    var qd QueryDef

    if !d.NextArg() {
        return qd, d.ArgErr() // query name
    }
    qd.Name = d.Val()

    for nesting := d.Nesting(); d.NextBlock(nesting); {
        switch d.Val() {
        case "sql":
            // Support both inline string and backtick-quoted multiline
            if !d.NextArg() {
                return qd, d.ArgErr()
            }
            qd.SQL = d.Val()

        case "param":
            p, err := parseParamBinding(d)
            if err != nil { return qd, err }
            qd.Params = append(qd.Params, p)

        case "output":
            o, err := parseOutputConfig(d)
            if err != nil { return qd, err }
            qd.Output = o

        case "cache":
            dur, err := durationArg(d)
            if err != nil { return qd, err }
            qd.CacheTTL = caddy.Duration(dur)

        case "schedule":
            if !d.NextArg() { return qd, d.ArgErr() }
            qd.Schedule = d.Val()

        case "timeout":
            dur, err := durationArg(d)
            if err != nil { return qd, err }
            qd.Timeout = caddy.Duration(dur)

        default:
            return qd, d.Errf("unknown query option: %s", d.Val())
        }
    }

    if qd.SQL == "" {
        return qd, d.Errf("query %q has no sql statement", qd.Name)
    }
    return qd, nil
}

// ── Param block parser ─────────────────────────────────────────────────

func parseParamBinding(d *caddyfile.Dispenser) (ParamBinding, error) {
    var p ParamBinding

    if !d.NextArg() {
        return p, d.ArgErr() // param name
    }
    p.Name = d.Val()

    for nesting := d.Nesting(); d.NextBlock(nesting); {
        switch d.Val() {
        case "from":
            if !d.NextArg() { return p, d.ArgErr() }
            src := d.Val()
            valid := map[string]bool{
                "query": true, "path": true, "header": true,
                "body": true, "env": true, "placeholder": true,
            }
            if !valid[src] {
                return p, d.Errf("invalid param source %q", src)
            }
            p.Source = src

        case "key":
            if !d.NextArg() { return p, d.ArgErr() }
            p.Key = d.Val()

        case "type":
            if !d.NextArg() { return p, d.ArgErr() }
            p.Type = d.Val()

        case "default":
            if !d.NextArg() { return p, d.ArgErr() }
            p.Default = d.Val()

        case "min":
            f, err := floatArg(d)
            if err != nil { return p, err }
            p.Min = &f

        case "max":
            f, err := floatArg(d)
            if err != nil { return p, err }
            p.Max = &f

        case "cap":
            f, err := floatArg(d)
            if err != nil { return p, err }
            p.Cap = &f

        case "pattern":
            if !d.NextArg() { return p, d.ArgErr() }
            p.Pattern = d.Val()
        }
    }
    return p, nil
}

// ── Route block parser ─────────────────────────────────────────────────

func parseQueryRoute(d *caddyfile.Dispenser) (QueryRoute, error) {
    var r QueryRoute

    // route METHOD /path query_name
    args := d.RemainingArgs()
    if len(args) < 3 {
        return r, d.Errf("route requires: METHOD path query_name")
    }
    r.Method, r.Path, r.QueryName = args[0], args[1], args[2]

    for nesting := d.Nesting(); d.NextBlock(nesting); {
        switch d.Val() {
        case "require_header":
            k, v, err := twoArgs(d)
            if err != nil { return r, err }
            if r.RequireHeader == nil {
                r.RequireHeader = make(map[string]string)
            }
            r.RequireHeader[k] = v

        case "rate_limit":
            // rate_limit 100 per 1m
            n, err := intArg(d)
            if err != nil { return r, err }
            if !d.NextArg() || d.Val() != "per" {
                return r, d.Errf("rate_limit syntax: rate_limit <n> per <duration>")
            }
            dur, err := durationArg(d)
            if err != nil { return r, err }
            r.RateLimit = &RoutRateLimit{Requests: n, Window: caddy.Duration(dur)}
        }
    }
    return r, nil
}
```

---

## 16. Provisioner — Wiring Config to Runtime

```go
// module/provisioner.go
func (m *Middleware) Provision(ctx caddy.Context) error {
    m.log = ctx.Logger(m)

    // ── 1. Apply defaults ────────────────────────────────────────
    m.applyDefaults()

    // ── 2. Expand env vars in all string fields ──────────────────
    if err := m.expandEnv(); err != nil {
        return fmt.Errorf("env expansion: %w", err)
    }

    // ── 3. Validate config ───────────────────────────────────────
    if err := m.validate(); err != nil {
        return fmt.Errorf("config validation: %w", err)
    }

    // ── 4. Open DuckDB connection ─────────────────────────────────
    client, err := db.Open(m.DB)
    if err != nil {
        return fmt.Errorf("open duckdb: %w", err)
    }
    m.db = client

    // ── 5. Apply DuckDB settings ──────────────────────────────────
    for k, v := range m.DB.Settings {
        if _, err := m.db.Exec(fmt.Sprintf("SET %s = %s", k, v)); err != nil {
            return fmt.Errorf("db setting %s: %w", k, err)
        }
    }

    // ── 6. Run schema migrations ──────────────────────────────────
    if err := db.Migrate(m.db); err != nil {
        return fmt.Errorf("schema migration: %w", err)
    }

    // ── 7. Build query registry ───────────────────────────────────
    m.reg = NewQueryRegistry(m.db, m.QueryAPI.MaxRows)
    for _, qd := range m.Queries {
        if err := m.reg.Register(qd); err != nil {
            return fmt.Errorf("register query %q: %w", qd.Name, err)
        }
    }

    // ── 8. Start batch writer ─────────────────────────────────────
    m.writer = db.NewBatchWriter(m.db, m.Writer)

    // ── 9. Start abuse detection engine ──────────────────────────
    if m.Abuse.Enabled {
        m.engine = detect.NewEngine(m.db, m.Abuse)
        m.engine.Start(ctx.Context)
    }

    // ── 10. Start maintenance loop ────────────────────────────────
    m.startMaintenance(ctx.Context)

    return nil
}

// ── Env var expansion ──────────────────────────────────────────────────
// Supports {env.VAR_NAME} in any string config field

func (m *Middleware) expandEnv() error {
    return structwalk(m, func(s *string) error {
        expanded := os.Expand(*s, func(key string) string {
            if after, ok := strings.CutPrefix(key, "env."); ok {
                return os.Getenv(after)
            }
            return ""
        })
        *s = expanded
        return nil
    })
}

// ── Config validation ──────────────────────────────────────────────────

func (m *Middleware) validate() error {
    var errs []error

    if m.DB.Path == "" {
        errs = append(errs, errors.New("db.path is required"))
    }

    // Validate all query SQL at provision time — fail fast, not at runtime
    for _, qd := range m.Queries {
        if err := validateSQL(qd.SQL); err != nil {
            errs = append(errs, fmt.Errorf("query %q: invalid sql: %w", qd.Name, err))
        }
        // Ensure all $params referenced in SQL have a binding
        for _, ref := range extractParamRefs(qd.SQL) {
            if !hasParamBinding(qd.Params, ref) {
                errs = append(errs,
                    fmt.Errorf("query %q: $%s used in SQL but no param block defined", qd.Name, ref))
            }
        }
    }

    // Validate routes reference known query names
    queryNames := make(map[string]bool)
    for _, qd := range m.Queries {
        queryNames[qd.Name] = true
    }
    for _, r := range m.Routes {
        if !queryNames[r.QueryName] {
            errs = append(errs,
                fmt.Errorf("route %s %s references unknown query %q", r.Method, r.Path, r.QueryName))
        }
    }

    return errors.Join(errs...)
}

// validateSQL does a EXPLAIN dry-run against DuckDB to catch syntax errors
func validateSQL(sql string) error {
    // Use an in-memory connection to avoid touching the real DB
    db, err := duckdb.Open(":memory:")
    if err != nil { return err }
    defer db.Close()
    // Replace $params with NULL for the dry run
    sanitized := paramRe.ReplaceAllString(sql, "NULL")
    _, err = db.Exec("EXPLAIN " + sanitized)
    return err
}
```

---

## 17. Query Registry & Execution Engine

```go
// module/query_registry.go
package caddyduckdb

import (
    "context"
    "database/sql"
    "encoding/json"
    "sync"
    "time"
)

type QueryRegistry struct {
    db      *sql.DB
    maxRows int
    mu      sync.RWMutex
    queries map[string]*RegisteredQuery
}

type RegisteredQuery struct {
    Def       QueryDef
    cache     *cachedResult
    cacheMu   sync.RWMutex
}

type cachedResult struct {
    data      []map[string]any
    expiresAt time.Time
}

func (rq *RegisteredQuery) cachedGet() ([]map[string]any, bool) {
    rq.cacheMu.RLock()
    defer rq.cacheMu.RUnlock()
    if rq.cache != nil && time.Now().Before(rq.cache.expiresAt) {
        return rq.cache.data, true
    }
    return nil, false
}

// ── Execute a named query from an HTTP request ─────────────────────────

func (reg *QueryRegistry) Execute(
    ctx context.Context,
    name string,
    r *http.Request,
) ([]map[string]any, *OutputConfig, error) {
    reg.mu.RLock()
    rq, ok := reg.queries[name]
    reg.mu.RUnlock()

    if !ok {
        return nil, nil, fmt.Errorf("query %q not found", name)
    }

    // Cache hit
    if cached, hit := rq.cachedGet(); hit {
        return cached, &rq.Def.Output, nil
    }

    // Resolve parameters from request context
    args, err := resolveParams(rq.Def.Params, r)
    if err != nil {
        return nil, nil, fmt.Errorf("param binding: %w", err)
    }

    // Execute with timeout
    timeout := 30 * time.Second
    if rq.Def.Timeout > 0 {
        timeout = time.Duration(rq.Def.Timeout)
    }
    qCtx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()

    rows, err := reg.db.QueryContext(qCtx, rq.Def.SQL, args...)
    if err != nil {
        return nil, nil, fmt.Errorf("execute query %q: %w", name, err)
    }
    defer rows.Close()

    result, err := scanRows(rows, reg.maxRows)
    if err != nil {
        return nil, nil, err
    }

    // Store in cache
    if rq.Def.CacheTTL > 0 {
        rq.cacheMu.Lock()
        rq.cache = &cachedResult{
            data:      result,
            expiresAt: time.Now().Add(time.Duration(rq.Def.CacheTTL)),
        }
        rq.cacheMu.Unlock()
    }

    return result, &rq.Def.Output, nil
}

// ── Param resolver ─────────────────────────────────────────────────────

func resolveParams(bindings []ParamBinding, r *http.Request) ([]any, error) {
    args := make([]any, len(bindings))

    for i, b := range bindings {
        raw := ""
        switch b.Source {
        case "query":
            raw = r.URL.Query().Get(b.Key)
        case "path":
            // Caddy injects path vars via context
            raw = caddyhttp.GetVar(r.Context(), b.Key).(string)
        case "header":
            raw = r.Header.Get(b.Key)
        case "body":
            raw = bodyField(r, b.Key)
        case "env":
            raw = os.Getenv(b.Key)
        case "placeholder":
            raw = caddyhttp.GetVar(r.Context(), b.Key).(string)
        }

        if raw == "" {
            raw = b.Default
        }

        coerced, err := coerce(raw, b)
        if err != nil {
            return nil, fmt.Errorf("param %q: %w", b.Name, err)
        }
        args[i] = coerced
    }
    return args, nil
}

// coerce validates and converts a raw string to the target type
func coerce(raw string, b ParamBinding) (any, error) {
    switch b.Type {
    case "int":
        n, err := strconv.ParseInt(raw, 10, 64)
        if err != nil { return nil, fmt.Errorf("not an integer: %q", raw) }
        if b.Min != nil && float64(n) < *b.Min {
            return nil, fmt.Errorf("value %d below minimum %g", n, *b.Min)
        }
        if b.Cap != nil && float64(n) > *b.Cap {
            n = int64(*b.Cap)
        }
        return n, nil
    case "float":
        f, err := strconv.ParseFloat(raw, 64)
        if err != nil { return nil, fmt.Errorf("not a float: %q", raw) }
        if b.Cap != nil && f > *b.Cap { f = *b.Cap }
        return f, nil
    case "bool":
        return strconv.ParseBool(raw)
    case "duration":
        d, err := time.ParseDuration(raw)
        if err != nil { return nil, err }
        return d.String(), nil // pass as string to DuckDB INTERVAL
    case "timestamp":
        t, err := time.Parse(time.RFC3339, raw)
        if err != nil { return nil, err }
        return t, nil
    default: // "string"
        if b.Pattern != "" {
            if ok, _ := regexp.MatchString(b.Pattern, raw); !ok {
                return nil, fmt.Errorf("value %q doesn't match pattern %q", raw, b.Pattern)
            }
        }
        return raw, nil
    }
}

// ── Row scanner ────────────────────────────────────────────────────────

func scanRows(rows *sql.Rows, maxRows int) ([]map[string]any, error) {
    cols, err := rows.Columns()
    if err != nil { return nil, err }

    result := make([]map[string]any, 0, 64)
    vals := make([]any, len(cols))
    ptrs := make([]any, len(cols))
    for i := range vals { ptrs[i] = &vals[i] }

    for rows.Next() {
        if len(result) >= maxRows { break }
        if err := rows.Scan(ptrs...); err != nil { return nil, err }
        row := make(map[string]any, len(cols))
        for i, col := range cols {
            row[col] = vals[i]
        }
        result = append(result, row)
    }
    return result, rows.Err()
}
```

---

## 18. Query API Handler — Serving Named & Raw Queries

```go
// module/query_api.go

func (m *Middleware) handleQueryAPI(w http.ResponseWriter, r *http.Request) {
    // ── Auth ──────────────────────────────────────────────────────
    if m.QueryAPI.Token != "" {
        tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
        if tok != m.QueryAPI.Token {
            http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
            return
        }
    }

    // CORS
    if m.QueryAPI.CORSOrigin != "" {
        w.Header().Set("Access-Control-Allow-Origin", m.QueryAPI.CORSOrigin)
        w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
        if r.Method == http.MethodOptions {
            w.WriteHeader(http.StatusNoContent)
            return
        }
    }

    path := strings.TrimPrefix(r.URL.Path, m.QueryAPI.Path)

    switch {
    // GET /_duckdb/queries → list registered query names
    case r.Method == http.MethodGet && path == "/queries":
        m.handleListQueries(w, r)

    // GET /_duckdb/query/{name} → execute named query
    case r.Method == http.MethodGet && strings.HasPrefix(path, "/query/"):
        name := strings.TrimPrefix(path, "/query/")
        m.handleNamedQuery(w, r, name)

    // GET /_duckdb/builtin/{name} → built-in analytics queries (see §8)
    case r.Method == http.MethodGet && strings.HasPrefix(path, "/builtin/"):
        name := strings.TrimPrefix(path, "/builtin/")
        m.handleBuiltinQuery(w, r, name)

    // POST /_duckdb/sql → raw SQL (only if allow_raw_sql = on)
    case r.Method == http.MethodPost && path == "/sql":
        if !m.QueryAPI.AllowRawSQL {
            http.Error(w, `{"error":"raw SQL disabled"}`, http.StatusForbidden)
            return
        }
        m.handleRawSQL(w, r)

    // GET /_duckdb/export/{name}.{format} → download as CSV/Parquet
    case r.Method == http.MethodGet && strings.HasPrefix(path, "/export/"):
        m.handleExport(w, r, path)

    default:
        http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
    }
}

func (m *Middleware) handleNamedQuery(w http.ResponseWriter, r *http.Request, name string) {
    data, out, err := m.reg.Execute(r.Context(), name, r)
    if err != nil {
        writeError(w, err, http.StatusBadRequest)
        return
    }
    writeOutput(w, r, data, out)
}

func (m *Middleware) handleRawSQL(w http.ResponseWriter, r *http.Request) {
    var body struct {
        SQL    string         `json:"sql"`
        Params []any          `json:"params,omitempty"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        writeError(w, err, http.StatusBadRequest)
        return
    }

    // Reject mutations unless explicitly in the request
    if isMutation(body.SQL) && r.URL.Query().Get("allow_write") != "1" {
        writeError(w, errors.New("mutations require ?allow_write=1"), http.StatusForbidden)
        return
    }

    timeout := time.Duration(m.QueryAPI.Timeout)
    ctx, cancel := context.WithTimeout(r.Context(), timeout)
    defer cancel()

    rows, err := m.db.QueryContext(ctx, body.SQL, body.Params...)
    if err != nil {
        writeError(w, err, http.StatusInternalServerError)
        return
    }
    defer rows.Close()

    data, err := scanRows(rows, m.QueryAPI.MaxRows)
    if err != nil {
        writeError(w, err, http.StatusInternalServerError)
        return
    }

    out := &OutputConfig{Format: "json", Envelope: true}
    writeOutput(w, r, data, out)
}

// ── Output writer (handles all formats) ───────────────────────────────

func writeOutput(w http.ResponseWriter, r *http.Request, data []map[string]any, out *OutputConfig) {
    // Apply column aliases and omissions
    data = transformColumns(data, out)

    switch out.Format {
    case "csv":
        w.Header().Set("Content-Type", "text/csv")
        if out.Attachment {
            fname := renderFilename(out.Filename)
            w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
        }
        writeCSV(w, data)

    case "ndjson":
        w.Header().Set("Content-Type", "application/x-ndjson")
        for _, row := range data {
            json.NewEncoder(w).Encode(row)
        }

    case "parquet":
        w.Header().Set("Content-Type", "application/octet-stream")
        fname := renderFilename(out.Filename)
        w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
        writeParquet(w, data)

    default: // "json"
        w.Header().Set("Content-Type", "application/json")
        if out.Envelope {
            json.NewEncoder(w).Encode(map[string]any{
                "data": data,
                "meta": map[string]any{
                    "count":     len(data),
                    "generated": time.Now().UTC(),
                },
            })
        } else {
            json.NewEncoder(w).Encode(data)
        }
    }
}
```

---

## 19. Complete Caddyfile Example

This is what a full real-world config looks like with all features in play:

```caddyfile
{
    order duckdb_analytics before reverse_proxy
}

analytics.example.com {
    duckdb_analytics {
        # ── Database ─────────────────────────────────────────────────
        db_path   /var/lib/caddy/analytics.db
        retention 90d
        geoip     /etc/caddy/GeoLite2-Country.mmdb

        db_setting threads       4
        db_setting memory_limit  512MB
        db_setting temp_directory /tmp/duckdb

        # ── Write pipeline ───────────────────────────────────────────
        batch_size     500
        flush_interval 200ms
        buffer_size    8192
        overflow       drop
        sample_rate    1

        exclude_path /_internal/*
        exclude_path /health
        exclude_path /favicon.ico
        exclude_field referer       # GDPR

        # ── Query API ─────────────────────────────────────────────────
        api_path    /_internal/analytics
        api_token   {env.ANALYTICS_TOKEN}
        raw_sql     off
        max_rows    5000
        cors_origin https://dashboard.example.com

        # ── Named queries ─────────────────────────────────────────────

        query top_pages {
            sql `
                SELECT
                    path_norm                              AS path,
                    COUNT(*)                               AS views,
                    COUNT(DISTINCT ip)                     AS unique_visitors,
                    ROUND(AVG(latency_ms), 1)              AS avg_ms
                FROM requests
                WHERE ts > NOW() - $window::INTERVAL
                  AND status = 200
                GROUP BY path_norm
                ORDER BY views DESC
                LIMIT $limit
            `
            param window {
                from    query
                key     window
                type    duration
                default 24h
            }
            param limit {
                from    query
                key     limit
                type    int
                default 20
                min     1
                cap     100
            }
            output {
                format   json
                envelope on
            }
            cache 60s
        }

        query requests_over_time {
            sql `
                SELECT
                    time_bucket($bucket::INTERVAL, ts)  AS t,
                    COUNT(*)                            AS requests,
                    COUNT(DISTINCT ip)                  AS visitors,
                    AVG(latency_ms)                     AS avg_latency,
                    SUM(bytes_sent)                     AS bytes
                FROM requests
                WHERE ts > NOW() - $window::INTERVAL
                  AND ($host = '' OR host = $host)
                GROUP BY t
                ORDER BY t
            `
            param bucket {
                from    query
                key     bucket
                type    duration
                default 1h
            }
            param window {
                from    query
                key     window
                type    duration
                default 7d
            }
            param host {
                from    query
                key     host
                type    string
                default ""
            }
            output {
                format   json
                envelope on
                alias    t       timestamp
                alias    avg_latency  latency_avg_ms
            }
            cache 5m
        }

        query suspicious_ips {
            sql `
                SELECT
                    r.ip,
                    ir.score,
                    ir.flags,
                    ir.blocked,
                    COUNT(r.id)                                    AS total_reqs,
                    COUNT(r.id) FILTER (WHERE r.status >= 400)    AS errors,
                    COUNT(DISTINCT r.path_norm)                    AS distinct_paths,
                    MAX(r.ts)                                      AS last_seen
                FROM requests r
                LEFT JOIN ip_reputation ir ON r.ip = ir.ip
                WHERE r.ts > NOW() - $window::INTERVAL
                GROUP BY r.ip, ir.score, ir.flags, ir.blocked
                HAVING ir.score > $min_score
                    OR errors > $min_errors
                ORDER BY ir.score DESC, errors DESC
                LIMIT $limit
            `
            param window {
                from    query
                key     window
                type    duration
                default 1h
            }
            param min_score {
                from    query
                key     min_score
                type    int
                default 20
            }
            param min_errors {
                from    query
                key     min_errors
                type    int
                default 50
            }
            param limit {
                from    query
                key     limit
                type    int
                default 50
                cap     500
            }
            output {
                format   json
                envelope on
            }
        }

        query ip_history {
            sql `
                SELECT ts, method, path, status, latency_ms,
                       user_agent, rule_matched
                FROM requests
                WHERE ip = $ip
                  AND ts > NOW() - $window::INTERVAL
                ORDER BY ts DESC
                LIMIT $limit
            `
            param ip {
                from    path
                key     ip
                type    string
                pattern `^\d{1,3}(\.\d{1,3}){3}$`
            }
            param window {
                from    query
                key     window
                type    duration
                default 24h
            }
            param limit {
                from    query
                key     limit
                type    int
                default 200
                cap     1000
            }
            output {
                format   json
                envelope on
            }
        }

        query export_requests {
            sql `
                SELECT ts, ip, country_code, method, path, status,
                       latency_ms, bytes_sent, ua_browser, ua_os
                FROM requests
                WHERE ts BETWEEN $from AND $to
                ORDER BY ts
            `
            param from {
                from    query
                key     from
                type    timestamp
                default "2000-01-01T00:00:00Z"
            }
            param to {
                from    query
                key     to
                type    timestamp
                default "2099-01-01T00:00:00Z"
            }
            output {
                format     csv
                attachment on
                filename   "requests-{{.date}}.csv"
                envelope   off
            }
            timeout 120s
        }

        # ── Route bindings ─────────────────────────────────────────────

        route GET /_internal/analytics/top_pages      top_pages
        route GET /_internal/analytics/traffic        requests_over_time
        route GET /_internal/analytics/suspicious     suspicious_ips
        route GET /_internal/analytics/ip/:ip/history ip_history {
            require_header Authorization "Bearer {env.ANALYTICS_TOKEN}"
        }
        route GET /_internal/analytics/export         export_requests {
            require_header X-Export-Key "{env.EXPORT_SECRET}"
            rate_limit 5 per 1m
        }

        # ── Abuse detection ────────────────────────────────────────────

        abuse {
            rate_limit     300 per 60s
            burst          50 in 5s
            brute_force    /login /admin /api/auth max 10
            path_scan      max 40
            deep_scan      every 30s
            score_block_at 80
            block_for      15m
            block_action   block
            block_status   429
            allowlist      127.0.0.1
            allowlist      10.0.0.0/8

            rule tor_exit_high_error {
                sql `
                    SELECT ip, COUNT(*) AS errors
                    FROM requests
                    WHERE ts > NOW() - INTERVAL '10 minutes'
                      AND status IN (401, 403, 404)
                    GROUP BY ip
                    HAVING errors > 100
                `
                severity 2
                score    35
                every    2m
            }

            rule ua_empty_post {
                sql `
                    SELECT DISTINCT ip
                    FROM requests
                    WHERE ts > NOW() - INTERVAL '5 minutes'
                      AND method = 'POST'
                      AND (user_agent = '' OR user_agent IS NULL)
                `
                severity 3
                score    50
                every    1m
            }
        }
    }

    reverse_proxy localhost:3000
}
```

---

## 20. Hot Reload Behaviour

Caddy's `caddy reload` replaces the module in-place. The `Cleanup` method handles graceful teardown:

```go
func (m *Middleware) Cleanup() error {
    // Flush remaining writes
    if m.writer != nil {
        m.writer.Flush()
        m.writer.Stop()
    }
    // Final checkpoint before closing
    if m.db != nil {
        m.db.Exec("CHECKPOINT")
        m.db.Close()
    }
    return nil
}
```

Because `Provision` re-validates all query SQL against DuckDB at startup, a bad query in the Caddyfile will cause `caddy reload` to **refuse the new config and keep the old one running** — a built-in safety net you get for free from Caddy's transactional config model.

---

## Updated Implementation Phases

| Phase | Deliverable |
|---|---|
| 1 | Schema + batch writer + basic request logging |
| 2 | Tier 1 in-memory abuse detection |
| 3 | `UnmarshalCaddyfile` parser for `db_*`, `writer_*`, `api_*` directives |
| 4 | `query {}` block parser + `QueryRegistry` + named query execution |
| 5 | `route` directive + route-level query binding + param resolver |
| 6 | Output formatter (JSON/CSV/NDJSON/Parquet) + attachment support |
| 7 | Tier 2 DuckDB deep-scan abuse rules + `rule {}` block parser |
| 8 | Env expansion + SQL dry-run validation at provision time |
| 9 | Scheduled queries (`schedule` directive) + query result cache |
| 10 | Maintenance loop, hot reload, `Cleanup()` |
