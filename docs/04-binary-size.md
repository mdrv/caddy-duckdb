# Binary Size Optimization

caddy-duckdb adds ~53 MB to the Caddy binary (100.8 MB → 153.8 MB). This is expected — DuckDB's C++ engine is statically linked with all default extensions.

## Why it's large

The `go-duckdb` driver statically links `libduckdb.a`, which bundles the full DuckDB engine plus extensions: parquet, json, icu, httpfs, fts, autocomplete, etc. Each extension adds 1-5 MB of compiled C++ code. caddy-duckdb only needs **core + parquet**.

## Memory impact

DuckDB is lazy — no data loads into memory at startup. The binary is large but runtime memory depends on queries:

- Idle: ~30 MB (engine initialized, connection pool created)
- Active queries: varies by query complexity and data scanned
- Connection pool: 4 idle connections (configurable via `db_setting max_threads`)

## Optimization options

### 1. UPX compression (quickest win)

UPX typically compresses Go binaries by 30-40%:

```bash
upx --best caddy-duckdb
# 153.8 MB → ~60-70 MB
```

Drawback: slightly slower startup (decompression), and some environments flag UPX binaries.

### 2. Dynamic linking (biggest win, trade-off)

Instead of statically linking libduckdb, link dynamically against a system-installed `libduckdb.so`:

```bash
# Build DuckDB from source with only needed extensions
cd duckdb
GEN=ninja make DISABLE_PARQUET=0 DISABLE_EXTENSIONS="json icu httpfs fts autocomplete" \
    BUILD_BENCHMARK=OFF EXTENSION_STATIC_BUILD=0

# Then build caddy-duckdb with dynamic linking
CGO_ENABLED=1 go build -tags duckdb_shared -o caddy-duckdb ./cmd/
```

This produces a ~5 MB binary but requires `libduckdb.so` on the target system. Trade-off: deployment complexity vs. binary size.

### 3. Build custom static DuckDB (moderate effort)

Build DuckDB from source with only the extensions caddy-duckdb uses:

```cmake
# extension_config.cmake
duckdb_extension_load(parquet)
# Remove: json, icu, httpfs, fts, autocomplete, etc.
```

Then compile and link against this minimal `libduckdb.a`. Saves ~20-30 MB.

Requires maintaining a custom DuckDB build.

### 4. Upgrade to official duckdb-go driver

The marcboeker fork (`github.com/marcboeker/go-duckdb/v2`) is now superseded by the official driver (`github.com/duckdb/duckdb-go/v2`). The official driver may have better build tag support for excluding extensions. Migration:

```bash
go get github.com/duckdb/duckdb-go/v2@latest
gofmt -w -r '"github.com/marcboeker/go-duckdb/v2" -> "github.com/duckdb/duckdb-go/v2"' .
go mod tidy
```

### 5. Already excluded: Arrow

The Arrow interface (`duckdb_arrow` build tag) is opt-in since go-duckdb v2. We don't use it, so it's already excluded — this saves ~5-10 MB compared to v1.

## Recommendation

For Colota's use case (family GPS tracking on a single server):

| Option                           | Binary size | Effort    | Deployment                                  |
| -------------------------------- | ----------- | --------- | ------------------------------------------- |
| Current (static, all extensions) | 154 MB      | None      | Simple — single binary                      |
| UPX                              | ~65 MB      | 1 command | Same — single binary, slightly slower start |
| Dynamic linking                  | ~5 MB       | Medium    | Needs `libduckdb.so` on target              |
| Custom static build              | ~100 MB     | High      | Single binary, maintenance burden           |

**Pragmatic choice:** The 154 MB binary is fine for a server. Memory impact is minimal (lazy loading). Only optimize if disk space or download time matters (e.g., embedded/ARM deployments).

If you want UPX, add it to `build.sh`:

```bash
# After xcaddy build
upx --best --lzma "$REPO_ROOT/caddy-duckdb"
```
