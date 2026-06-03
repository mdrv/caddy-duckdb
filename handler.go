package caddyduckdb

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func (m *Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	for i := range m.routes {
		mr := &m.routes[i]
		if mr.method != "*" && mr.method != r.Method {
			continue
		}
		if !matchPath(mr.path, r.URL.Path) {
			continue
		}

		for hdr, expected := range mr.headers {
			repl, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
			resolved := expected
			if ok {
				resolved = repl.ReplaceAll(expected, "")
			}
			if r.Header.Get(hdr) != resolved {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return nil
			}
		}

		if mr.rateLimit != nil {
			ip := extractIP(r)
			if !mr.checkRateLimit(ip) {
				http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
				return nil
			}
		}

		data, out, err := m.reg.Execute(r.Context(), mr.queryName, r)
		if err != nil {
			m.logger.Error("query execution failed",
				zap.String("query", mr.queryName),
				zap.Error(err),
			)
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return nil
		}

		writeOutput(w, data, out)
		return nil
	}

	if strings.HasPrefix(r.URL.Path, m.APIPath) {
		m.handleQueryAPI(w, r)
		return nil
	}

	start := time.Now()
	rec := caddyhttp.NewResponseRecorder(w, nil, nil)

	err := next.ServeHTTP(rec, r)
	if err != nil {
		return err
	}

	latency := time.Since(start).Milliseconds()

	if m.writer != nil && !m.isExcludedPath(r.URL.Path) {
		record := RequestRecord{
			TS:        time.Now().UTC(),
			IP:        extractIP(r),
			Method:    r.Method,
			Host:      r.Host,
			Path:      r.URL.Path,
			Query:     r.URL.RawQuery,
			Status:    rec.Status(),
			LatencyMs: latency,
			BytesSent: int64(rec.Size()),
			UserAgent: r.UserAgent(),
		}
		m.writer.Write(record)
	}

	return nil
}

func (m *Middleware) isExcludedPath(path string) bool {
	for _, p := range m.ExcludePaths {
		if strings.HasPrefix(p, "*") && strings.HasSuffix(path, p[1:]) {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(path, p[:len(p)-1]) {
			return true
		}
		if path == p {
			return true
		}
	}
	return false
}

func matchPath(pattern, path string) bool {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	pathParts := strings.Split(strings.Trim(path, "/"), "/")

	if len(patternParts) != len(pathParts) {
		return false
	}

	for i := range patternParts {
		if strings.HasPrefix(patternParts[i], ":") {
			continue
		}
		if patternParts[i] != pathParts[i] {
			return false
		}
	}
	return true
}

func extractIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		return strings.TrimSpace(parts[0])
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return real
	}
	idx := strings.LastIndex(r.RemoteAddr, ":")
	if idx == -1 {
		return r.RemoteAddr
	}
	return r.RemoteAddr[:idx]
}

func writeOutput(w http.ResponseWriter, data []map[string]any, out *OutputConfig) {
	if out == nil {
		out = &OutputConfig{Format: "json", Status: 200}
	}

	data = transformColumns(data, out)

	status := out.Status
	if status == 0 {
		status = 200
	}

	switch out.Format {
	case "csv":
		w.Header().Set("Content-Type", "text/csv")
		w.WriteHeader(status)
		writeCSV(w, data)
	case "ndjson":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(status)
		for _, row := range data {
			json.NewEncoder(w).Encode(row)
		}
	default:
		if out.Body != "" {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(status)
			w.Write([]byte(out.Body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
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

func transformColumns(data []map[string]any, out *OutputConfig) []map[string]any {
	if out == nil || (len(out.Aliases) == 0 && len(out.Omit) == 0) {
		return data
	}

	omitSet := make(map[string]bool, len(out.Omit))
	for _, o := range out.Omit {
		omitSet[o] = true
	}

	result := make([]map[string]any, 0, len(data))
	for _, row := range data {
		newRow := make(map[string]any, len(row))
		for k, v := range row {
			if omitSet[k] {
				continue
			}
			if alias, ok := out.Aliases[k]; ok {
				newRow[alias] = v
			} else {
				newRow[k] = v
			}
		}
		result = append(result, newRow)
	}
	return result
}

func writeCSV(w http.ResponseWriter, data []map[string]any) {
	if len(data) == 0 {
		return
	}

	cw := csv.NewWriter(w)
	defer cw.Flush()

	var headers []string
	for k := range data[0] {
		headers = append(headers, k)
	}
	cw.Write(headers)

	for _, row := range data {
		record := make([]string, len(headers))
		for i, h := range headers {
			record[i] = fmt.Sprintf("%v", row[h])
		}
		cw.Write(record)
	}
}

func (m *Middleware) handleQueryAPI(w http.ResponseWriter, r *http.Request) {
	if m.APIToken != "" {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok != m.APIToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
	}

	if m.CORSOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", m.CORSOrigin)
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	path := strings.TrimPrefix(r.URL.Path, m.APIPath)

	switch {
	case r.Method == http.MethodGet && path == "/queries":
		m.handleListQueries(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/query/"):
		name := strings.TrimPrefix(path, "/query/")
		m.handleNamedQuery(w, r, name)
	case r.Method == http.MethodPost && path == "/sql":
		if !m.RawSQL {
			http.Error(w, `{"error":"raw SQL disabled"}`, http.StatusForbidden)
			return
		}
		m.handleRawSQL(w, r)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

func (m *Middleware) handleListQueries(w http.ResponseWriter, r *http.Request) {
	names := m.reg.ListNames()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"queries": names})
}

func (m *Middleware) handleNamedQuery(w http.ResponseWriter, r *http.Request, name string) {
	data, out, err := m.reg.Execute(r.Context(), name, r)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	writeOutput(w, data, out)
}

func (m *Middleware) handleRawSQL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SQL    string `json:"sql"`
		Params []any  `json:"params,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	args := make([]any, len(body.Params))
	copy(args, body.Params)

	rows, err := m.db.QueryContext(ctx, body.SQL, args...)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	data, err := scanRows(rows, m.MaxRows)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	writeOutput(w, data, &OutputConfig{Format: "json", Envelope: true, Status: 200})
}

func (mr *matchedRoute) checkRateLimit(key string) bool {
	if mr.rateLimit == nil {
		return true
	}
	mr.counterMu.Lock()
	defer mr.counterMu.Unlock()

	now := time.Now()
	window := time.Duration(mr.rateLimit.Window)

	ctr, ok := mr.counter[key]
	if !ok || now.After(ctr.resetAt) {
		ctr = &rateCounter{count: 0, resetAt: now.Add(window)}
		mr.counter[key] = ctr
	}

	ctr.count++
	return ctr.count <= mr.rateLimit.Requests
}

var _ caddyhttp.MiddlewareHandler = (*Middleware)(nil)
