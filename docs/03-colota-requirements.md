# Colota Integration Requirements

What the `/g/colota/` project needs to provide, based on caddy-duckdb's current capabilities.

## What caddy-duckdb handles

Everything in the Caddyfile — no separate server process needed:

| Concern                 | How                                                        |
| ----------------------- | ---------------------------------------------------------- |
| HTTPS / TLS             | Caddy auto (Let's Encrypt)                                 |
| Data ingestion (INSERT) | `route POST /pub insert_location`                          |
| Data querying (SELECT)  | `route GET /api/*`                                         |
| Schema bootstrap        | `schema` directive — DDL runs at provision time            |
| Auth (Bearer token)     | `require_header Authorization "Bearer {env.COLOTA_TOKEN}"` |
| Parquet archiving       | `partitioning` block with `timestamp_col tst`              |
| Static file serving     | Caddy `file_server`                                        |

## What /g/colota/ needs to provide

### 1. Caddyfile

A production Caddyfile is already written in `/g/colota/docs/01-v010.md` section 4. It uses:

- `schema` for DDL (CREATE SEQUENCE, CREATE TABLE, CREATE INDEX)
- `query` blocks for all 6 endpoints (insert, get_locations, last_location, all_last_locations, daily_summary)
- `partitioning` with `timestamp_col tst` (Colota uses unix epoch `tst`, not datetime `ts`)
- `route` blocks with `require_header` using `{env.COLOTA_TOKEN}`

**Required change:** The `query bootstrap` in the existing Caddyfile should be converted to a `schema` directive. The current `query bootstrap` won't work because caddy-duckdb's query engine uses `QueryContext` (expects result rows), and `ValidateAll()` will try to `EXPLAIN` the DDL. Use `schema` instead:

```caddyfile
schema `CREATE SEQUENCE IF NOT EXISTS loc_seq; CREATE TABLE IF NOT EXISTS locations (...)`
```

The `schema` directive runs DDL directly at provision time, before query validation.

### 2. Schema considerations

Colota's schema uses `received DOUBLE` (epoch seconds) and `tst BIGINT` (device unix timestamp). The partitioning `timestamp_col` must be set to `tst` (not the default `ts`). The archive cutoff uses `time.Now().AddDate(0, 0, -keepDays)` compared against this column — since `tst` is a unix epoch integer, this comparison will be incorrect.

**Issue:** Partitioning currently compares with `time.Time` values, but Colota stores unix epochs as BIGINT. The partitioning needs to compare against `epoch(now()) - keepDays * 86400` for integer timestamps. This is a caddy-duckdb limitation that needs a fix — either:

- Add a `timestamp_type [datetime|epoch]` config option to partitioning
- Or store `received`/`tst` as TIMESTAMPTZ in Colota's schema

### 3. Frontend (Svelte 5 SPA)

Already specified in `/g/colota/docs/01-v010.md` sections 5.1–5.5. Uses:

- MapLibre GL JS for maps
- vanilla-extract for styling
- API client hitting `/api/*` endpoints
- Static build output served by Caddy's `file_server`

### 4. Deployment artifacts

- `Caddyfile` — production config (already in docs)
- `build.sh` — build frontend with `bun run build`
- Systemd service or direct caddy-duckdb binary execution

## API contract

The caddy-duckdb module returns:

**POST /pub** — OwnTracks ingestion:

- Request: JSON body with `_type`, `tid`, `lat`, `lon`, `acc`, `alt`, `vel`, `cog`, `batt`, `bs`, `tst`
- Response: `200` with empty body (configured via `output { status 200, body "" }`)

**GET /api/locations** — Location history:

- Params: `tid?`, `since?`, `until?`, `limit?`
- Response: `{"data": [...], "meta": {"count": N, "generated": "..."}}`

**GET /api/locations/last** — Last position:

- Params: `tid?`
- Response: `[{"tst":..., "tid":"AA", "lat":..., "lon":..., ...}]` or `[]`

**GET /api/locations/all** — All devices' last positions:

- Response: `[{"tid":"AA", ...}, {"tid":"BB", ...}]`

**GET /api/locations/daily** — Daily summary:

- Params: `tid?`, `since?`, `limit?`
- Response: `{"data": [...], "meta": {"count": N, "generated": "..."}}`

All endpoints require `Authorization: Bearer <token>` header.

## Open caddy-duckdb issues affecting Colota

1. **Epoch timestamp partitioning** — `archiveTable` compares with `time.Time` but Colota uses BIGINT epoch. Needs fix in caddy-duckdb or schema change in Colota.
2. **`daily_summary` query uses `AGGREGATE_LIST`** — verify this DuckDB function is available in the version we link.
3. **No tests** — caddy-duckdb has zero test coverage. Colota integration tests would help both projects.
