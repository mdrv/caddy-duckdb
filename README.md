# caddy-duckdb

A [Caddy](https://caddyserver.com/) HTTP handler module that integrates [DuckDB](https://duckdb.org/) directly into your Caddyfile. Write SQL queries inline, bind HTTP parameters, and get JSON/CSV responses — no external server needed.

## What it does

- **Write SQL in your Caddyfile** — `query` blocks with inline SQL or `sql_file` references
- **Bind HTTP request data** — params from JSON body, query string, headers, path segments, env vars, Caddy placeholders
- **Route HTTP endpoints to queries** — `route POST /api/location insert_location`
- **Built-in request logging** — every request auto-logged to `_requests` table (async batch writer)
- **Parquet day-based rollover** — keeps the active DB small for long-running workloads (years of GPS tracking, etc.)
- **Query result caching** — per-query TTL cache
- **Auth & rate limiting** — per-route `require_header` and `rate_limit`

## Use case: GPS tracking

Replaces a separate Bun/Node server for [Colota](https://github.com/mdrv/colota) GPS tracking:

```caddyfile
{
	order duckdb before basicauth
}

location.example.com {
	duckdb {
		db_path /var/data/colota/locations.db

		query insert_location {
			sql `INSERT INTO locations (lat, lon, acc, alt, vel, bear, batt, bs, tst, received)
			     VALUES ($lat, $lon, $acc, $alt, $vel, $bear, $batt, $bs, $tst, NOW())
			     RETURNING id, lat, lon, tst`
			param lat  { from body, key lat, type float }
			param lon  { from body, key lon, type float }
			param acc  { from body, key acc, type float }
			param tst  { from body, key tst, type string }
			output { format json }
		}

		query get_locations {
			sql `SELECT id, lat, lon, acc, batt, tst, received
			     FROM locations ORDER BY id DESC LIMIT $limit`
			param limit { from query, key limit, type int, default 100, cap 5000 }
			output { format json, envelope on }
		}

		route POST /api/location insert_location
		route GET /api/locations get_locations
	}

	file_server
}
```

## Build

Requires Go 1.22+, GCC/Clang, and the system `duckdb` package (`pacman -S duckdb` on Arch).

```bash
./build.sh
```

Or manually with `xcaddy`:

```bash
CGO_ENABLED=1 GOFLAGS="-tags=duckdb_use_lib" xcaddy build \
    --with github.com/mdrv/caddy-duckdb=. --output ./caddy-duckdb
```

Binary is ~75 MB (dynamically linked against `/usr/lib/libduckdb.so`).\
For a fully self-contained static binary, remove `-tags=duckdb_use_lib` (~139 MB, no runtime dependency).

Verify the module is included:

```bash
./caddy-duckdb list-modules | grep duckdb
# http.handlers.duckdb
```

## Caddyfile directives

```caddyfile
duckdb {
    # Required
    db_path <path>

    # Request logging
    batch_size     <int>        # default 500
    flush_interval <duration>   # default 200ms
    exclude_path   <path>       # skip logging for this path (repeatable)
    exclude_field  <field>      # omit from _requests (repeatable)

    # Queries
    query <name> {
        sql <backtick-enclosed SQL>
        sql_file <path>

        param <name> {
            from     [body|query|header|path|env|placeholder]
            key      <key>
            type     [string|int|float|bool|timestamp|duration]
            default  <value>
            min      <number>
            max      <number>
            cap      <number>
            pattern  <regex>
        }

        output {
            format   [json|csv|ndjson]
            envelope [on|off]
            status   <code>
            body     <text>
            omit     <column>       # repeatable
            alias    <col> <name>   # repeatable
        }

        cache    <duration>
        timeout  <duration>
    }

    # Routes
    route <METHOD> <path> <query_name> {
        require_header <name> <value>
        rate_limit <n> per <duration>
    }

    # Parquet partitioning
    partitioning {
        table      <name>           # repeatable
        interval   [daily|weekly|monthly]
        format     [parquet|csv]
        path       <directory>
        filename   <template>       # {table}_{date}.parquet
        keep_days  <int>
        compress   [zstd|snappy|gzip|none]
        max_age    <duration>
    }

    # Maintenance
    checkpoint   <duration>   # default 6h
    retention    <duration>   # default 90d

    # Internal query API
    api_path     <path>       # default /_duckdb
    api_token    <token>
    raw_sql      [on|off]
    max_rows     <int>        # default 10000
    cors_origin  <origin>
}
```

## Architecture

```
HTTP Request
  ├─ Matches a route? → resolve params → execute query → format output → respond
  └─ No match → pass to next handler → async log to _requests
```

- Route-matched queries run synchronously (client waits for result)
- Request logging is async (batch writer, never blocks response)
- Parquet partitioning runs in a background goroutine

## Files

```
cmd/main.go          # xcaddy entry point
module.go            # Caddy module registration, Caddyfile parser, provisioning
handler.go           # ServeHTTP: routing, auth, rate limiting, output formatting
query_registry.go    # Query storage, execution, param binding, type coercion
writer.go            # Async batch writer for _requests table
docs/01-v010.md      # Full design document
```

## Status

v0.1.0 — builds and provisions successfully. See [docs/02-issues.md](docs/02-issues.md) for open issues.

## License

MIT
