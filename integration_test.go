//go:build duckdb_use_lib

package caddyduckdb_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"

	_ "github.com/mdrv/caddy-duckdb"
)

func caddyfile(dbPath, extra string) string {
	return `{
	admin localhost:2999
	http_port 8080
	https_port 8443
	order duckdb before basicauth
}
http://localhost:8080 {
	duckdb {
		db_path ` + dbPath + `
		batch_size 1
		flush_interval 50ms
		exclude_path /api/location

		schema ` + "`CREATE SEQUENCE IF NOT EXISTS locations_id_seq`" + `

		schema ` + "`" + `CREATE TABLE IF NOT EXISTS locations (
			id       BIGINT DEFAULT nextval('locations_id_seq') PRIMARY KEY,
			lat      DOUBLE NOT NULL,
			lon      DOUBLE NOT NULL,
			tst      BIGINT NOT NULL,
			received TIMESTAMPTZ DEFAULT NOW()
		)` + "`" + `

		query insert_location {
			sql ` + "`" + `INSERT INTO locations (lat, lon, tst)
			     VALUES ($lat, $lon, $tst)
			     RETURNING id, lat, lon, tst` + "`" + `
			param lat {
				from body
				key lat
				type float
			}
			param lon {
				from body
				key lon
				type float
			}
			param tst {
				from body
				key tst
				type int
			}
			output {
				format json
			}
		}

		query get_locations {
			sql ` + "`" + `SELECT id, lat, lon, tst FROM locations ORDER BY id DESC LIMIT $limit` + "`" + `
			param limit {
				from query
				key limit
				type int
				default 100
				cap 500
			}
			output {
				format json
				envelope on
			}
		}

		query get_last {
			sql ` + "`" + `SELECT id, lat, lon, tst FROM locations ORDER BY tst DESC LIMIT 1` + "`" + `
			output {
				format json
			}
		}

		route POST /api/location insert_location
		route GET  /api/locations get_locations
		route GET  /api/locations/last get_last

		api_path /_duckdb
		raw_sql  on
		` + extra + `
	}
	respond /ping 200 {
		body "pong"
	}
}`
}

func doRequest(t *testing.T, tc *caddytest.Tester, method, url, body string) (int, string) {
	t.Helper()
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := tc.Client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func assertStatus(t *testing.T, got, want int, body string) {
	t.Helper()
	if got != want {
		t.Errorf("expected status %d, got %d; body: %s", want, got, body)
	}
}

func assertContains(t *testing.T, body, sub string) {
	t.Helper()
	if !strings.Contains(body, sub) {
		t.Errorf("expected body to contain %q, got: %s", sub, body)
	}
}

func setup(t *testing.T) (*caddytest.Tester, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	tc := caddytest.NewTester(t)
	tc.InitServer(caddyfile(dbPath, ""), "caddyfile")
	return tc, dbPath
}

func TestInsertAndQuery(t *testing.T) {
	tc, _ := setup(t)

	status, body := doRequest(t, tc, "POST", "http://localhost:8080/api/location",
		`{"lat":-6.9222,"lon":107.607,"tst":1704067200}`)
	assertStatus(t, status, 200, body)
	assertContains(t, body, `"lat"`)

	status, body = doRequest(t, tc, "GET", "http://localhost:8080/api/locations", "")
	assertStatus(t, status, 200, body)
	assertContains(t, body, `"data"`)
}

func TestInsertReturnsRow(t *testing.T) {
	tc, _ := setup(t)

	status, raw := doRequest(t, tc, "POST", "http://localhost:8080/api/location",
		`{"lat":1.23,"lon":4.56,"tst":9999}`)
	assertStatus(t, status, 200, raw)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("response not JSON array: %v — body: %s", err, raw)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one row from RETURNING")
	}
	if rows[0]["lat"] == nil {
		t.Errorf("expected lat in response, got: %v", rows[0])
	}
}

func TestGetLocationsEnvelope(t *testing.T) {
	tc, _ := setup(t)

	status, raw := doRequest(t, tc, "GET", "http://localhost:8080/api/locations", "")
	assertStatus(t, status, 200, raw)

	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("response not JSON object: %v — body: %s", err, raw)
	}
	if env["data"] == nil {
		t.Error("envelope missing 'data' key")
	}
	if env["meta"] == nil {
		t.Error("envelope missing 'meta' key")
	}
}

func TestLimitCap(t *testing.T) {
	tc, _ := setup(t)

	status, raw := doRequest(t, tc, "GET", "http://localhost:8080/api/locations?limit=99999", "")
	assertStatus(t, status, 200, raw)

	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("response not JSON object: %v — body: %s", err, raw)
	}
	if env["meta"] == nil {
		t.Fatal("missing meta in envelope")
	}
}

func TestGetLast(t *testing.T) {
	tc, _ := setup(t)

	for _, tst := range []string{"1000", "2000", "3000"} {
		status, body := doRequest(t, tc, "POST", "http://localhost:8080/api/location",
			`{"lat":1.0,"lon":2.0,"tst":`+tst+`}`)
		assertStatus(t, status, 200, body)
	}

	status, raw := doRequest(t, tc, "GET", "http://localhost:8080/api/locations/last", "")
	assertStatus(t, status, 200, raw)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("response not JSON array: %v — body: %s", err, raw)
	}
	if len(rows) == 0 {
		t.Fatal("expected one row")
	}
}

func TestUnknownRoutePassthrough(t *testing.T) {
	tc, _ := setup(t)
	status, body := doRequest(t, tc, "GET", "http://localhost:8080/ping", "")
	assertStatus(t, status, 200, body)
	assertContains(t, body, "pong")
}

func TestInternalAPIListQueries(t *testing.T) {
	tc, _ := setup(t)

	status, raw := doRequest(t, tc, "GET", "http://localhost:8080/_duckdb/queries", "")
	assertStatus(t, status, 200, raw)

	var resp map[string]any
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	queries, ok := resp["queries"].([]any)
	if !ok || len(queries) == 0 {
		t.Errorf("expected non-empty queries list, got: %v", resp)
	}
}

func TestInternalAPIRawSQL(t *testing.T) {
	tc, _ := setup(t)

	status, raw := doRequest(t, tc, "POST", "http://localhost:8080/_duckdb/sql",
		`{"sql":"SELECT 42 AS answer"}`)
	assertStatus(t, status, 200, raw)

	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	data, _ := env["data"].([]any)
	if len(data) == 0 {
		t.Fatal("expected data in response")
	}
	row, _ := data[0].(map[string]any)
	if row["answer"] == nil {
		t.Errorf("expected 'answer' key in row, got: %v", row)
	}
}

func TestRequireHeader(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	extra := `route POST /api/secure insert_location {
			require_header X-Secret mysecret
		}`
	tc := caddytest.NewTester(t)
	tc.InitServer(caddyfile(dbPath, extra), "caddyfile")

	req, _ := http.NewRequest("POST", "http://localhost:8080/api/secure",
		bytes.NewBufferString(`{"lat":1.0,"lon":2.0,"tst":1000}`))
	req.Header.Set("Content-Type", "application/json")
	tc.AssertResponseCode(req, http.StatusUnauthorized)

	req2, _ := http.NewRequest("POST", "http://localhost:8080/api/secure",
		bytes.NewBufferString(`{"lat":1.0,"lon":2.0,"tst":1000}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Secret", "mysecret")
	tc.AssertResponseCode(req2, http.StatusOK)
}

func TestRateLimit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	extra := `route GET /api/limited get_last {
			rate_limit 2 per 10s
		}`
	tc := caddytest.NewTester(t)
	tc.InitServer(caddyfile(dbPath, extra), "caddyfile")

	for i := 0; i < 2; i++ {
		status, body := doRequest(t, tc, "GET", "http://localhost:8080/api/limited", "")
		assertStatus(t, status, 200, body)
	}
	status, body := doRequest(t, tc, "GET", "http://localhost:8080/api/limited", "")
	assertStatus(t, status, 429, body)
}

func TestMissingBodyParamsReturnError(t *testing.T) {
	tc, _ := setup(t)
	status, body := doRequest(t, tc, "POST", "http://localhost:8080/api/location", `{}`)
	assertStatus(t, status, 500, body)
}

func TestMissingDBPath(t *testing.T) {
	caddytest.AssertLoadError(t, `{
	admin localhost:2999
	http_port 8080
	order duckdb before basicauth
}
http://localhost:8080 {
	duckdb {
		query q {
			sql `+"`SELECT 1`"+`
		}
	}
}`, "caddyfile", "db_path is required")
}

func TestRawSQLDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	tc := caddytest.NewTester(t)
	tc.InitServer(`{
	admin localhost:2999
	http_port 8080
	order duckdb before basicauth
}
http://localhost:8080 {
	duckdb {
		db_path `+dbPath+`
	}
}`, "caddyfile")

	status, raw := doRequest(t, tc, "POST", "http://localhost:8080/_duckdb/sql",
		`{"sql":"SELECT 1"}`)
	assertStatus(t, status, 403, raw)
	assertContains(t, raw, "raw SQL disabled")
}

func TestSchemaBootstrap(t *testing.T) {
	tc, _ := setup(t)

	status, raw := doRequest(t, tc, "POST", "http://localhost:8080/_duckdb/sql",
		`{"sql":"SELECT count(*) AS c FROM locations"}`)
	assertStatus(t, status, 200, raw)
	assertContains(t, raw, `"c"`)
}

func TestDBFileCreated(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	tc := caddytest.NewTester(t)
	tc.InitServer(caddyfile(dbPath, ""), "caddyfile")

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Errorf("expected DB file to be created at %s", dbPath)
	}
}
