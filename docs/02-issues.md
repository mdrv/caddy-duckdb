# Open Issues

Known issues and unfinished work. Updated after critical fixes.

## Fixed (this session)

| #  | Issue                                                   | Fix                                                                       |
| -- | ------------------------------------------------------- | ------------------------------------------------------------------------- |
| 1  | User-defined tables not auto-created                    | Added `schema` directive (repeatable) — DDL runs at provision time        |
| 2  | No provision-time SQL validation                        | Added `ValidateAll()` — runs `EXPLAIN` with NULL params for all queries   |
| 4  | Body consumed on first param read                       | Buffer body with `io.ReadAll`, restore via `io.NopCloser`                 |
| 5  | Partitioning timestamp column hardcoded to `ts`         | Added `timestamp_col` config (default `ts`)                               |
| 6  | Partitioning query rewrite fragile                      | Added SELECT-only guard before rewrite                                    |
| 7  | SQL injection in partitioning table names               | Added `identRe` validation in `validate()`                                |
| —  | `require_header` doesn't resolve Caddy placeholders     | Now resolves `{env.VAR}` etc. via `caddy.Replacer`                        |
| —  | Unbound `$params` cause "mixing named/positional" error | `convertNamedToPositional` replaces ALL `$name` with `?` (unbound → NULL) |
| 13 | No `build.sh`                                           | Created with TMPDIR handling                                              |
| 17 | `schema` directive not in grammar                       | Added to parser                                                           |

## Remaining

### Critical

#### 3. No tests

Zero test coverage. Need:

- Unit tests for `resolveParams`, `coerce`, `convertNamedToPositional`, `scanRows`
- Unit tests for Caddyfile parsing (`UnmarshalCaddyfile`)
- Integration test: start Caddy with test Caddyfile, hit endpoints with `net/http`, verify responses
- Integration test for batch writer (flush behavior, drop on full)

---

### Medium Priority

#### 8. Rate limiter has no cleanup

`matchedRoute.counter` grows unboundedly. Fix: periodically sweep expired entries.

#### 9. No graceful shutdown for partitioner

In-progress export could leave partial Parquet file. Fix: use separate context per export.

#### 10. Batch writer silently drops on full channel

Add counter for dropped records and periodic logging.

#### 11. DuckDB single-writer limitation

Document this. DuckDB supports concurrent readers but only one writer process per `.db` file. Within a single Caddy process this is fine (connection pool serializes writes). Multi-instance setups need read-only replicas or shared Parquet.

#### 12. `exclude_path` matching is simplistic

Exact, prefix (`path*`), suffix (`*path`) only. Off-by-one in suffix matching.

---

### Low Priority

#### 14. No `Caddyfile.example`

Should have a complete example covering all directives.

#### 15. CSV header order non-deterministic

Map iteration is random. Use `rows.Columns()` order.

#### 16. JSON output key order non-deterministic

Map keys in random order. Consider sorted keys.

#### 18. Provision-time validation skips DDL queries

`ValidateAll()` runs `EXPLAIN` which works for SELECT/INSERT/UPDATE/DELETE but may fail on multi-statement schema DDL. Schema validation is implicitly tested by running the DDL during provision — if it fails, provision fails.
