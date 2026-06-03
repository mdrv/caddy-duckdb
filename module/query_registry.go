package caddyduckdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

type QueryRegistry struct {
	db      *sql.DB
	maxRows int
	logger  *zap.Logger
	queries map[string]*registeredQuery
}

type registeredQuery struct {
	def    *QueryDef
	cached *cachedResult
	mu     sync.RWMutex
}

type cachedResult struct {
	data      []map[string]any
	expiresAt time.Time
}

func (r *QueryRegistry) ListNames() []string {
	names := make([]string, 0, len(r.queries))
	for n := range r.queries {
		names = append(names, n)
	}
	return names
}

func NewQueryRegistry(db *sql.DB, maxRows int, logger *zap.Logger) *QueryRegistry {
	return &QueryRegistry{
		db:      db,
		maxRows: maxRows,
		logger:  logger,
		queries: make(map[string]*registeredQuery),
	}
}

func (r *QueryRegistry) Register(def *QueryDef) error {
	if def.Name == "" {
		return fmt.Errorf("query missing name")
	}
	if def.SQL == "" {
		return fmt.Errorf("query %q has no sql", def.Name)
	}
	if def.Output.Status == 0 {
		def.Output.Status = 200
	}
	if def.Output.Format == "" {
		def.Output.Format = "json"
	}
	r.queries[def.Name] = &registeredQuery{def: def}
	return nil
}

func (r *QueryRegistry) ValidateAll() error {
	for name, rq := range r.queries {
		sqlStr := paramRefRe.ReplaceAllString(rq.def.SQL, "NULL")
		rows, err := r.db.Query("EXPLAIN " + sqlStr)
		if err != nil {
			return fmt.Errorf("query %q: %w", name, err)
		}
		rows.Close()
	}
	return nil
}

func (r *QueryRegistry) Execute(ctx context.Context, name string, req *http.Request) ([]map[string]any, *OutputConfig, error) {
	rq, ok := r.queries[name]
	if !ok {
		return nil, nil, fmt.Errorf("query %q not found", name)
	}

	if cached, hit := rq.getCached(); hit {
		return cached, &rq.def.Output, nil
	}

	args, orderedKeys, err := resolveParams(rq.def.Params, req)
	if err != nil {
		return nil, nil, fmt.Errorf("param binding: %w", err)
	}

	timeout := 30 * time.Second
	if rq.def.Timeout > 0 {
		timeout = time.Duration(rq.def.Timeout)
	}
	qCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sqlStr := rq.def.SQL
	sqlStr, positionalArgs := convertNamedToPositional(sqlStr, args, orderedKeys)

	r.logger.Debug("executing query", zap.String("name", name), zap.String("sql", sqlStr))

	rows, err := r.db.QueryContext(qCtx, sqlStr, positionalArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("execute %q: %w", name, err)
	}
	defer rows.Close()

	result, err := scanRows(rows, r.maxRows)
	if err != nil {
		return nil, nil, err
	}

	if rq.def.CacheTTL > 0 {
		rq.setCached(result, time.Duration(rq.def.CacheTTL))
	}

	return result, &rq.def.Output, nil
}

func (rq *registeredQuery) getCached() ([]map[string]any, bool) {
	rq.mu.RLock()
	defer rq.mu.RUnlock()
	if rq.cached != nil && time.Now().Before(rq.cached.expiresAt) {
		return rq.cached.data, true
	}
	return nil, false
}

func (rq *registeredQuery) setCached(data []map[string]any, ttl time.Duration) {
	rq.mu.Lock()
	defer rq.mu.Unlock()
	rq.cached = &cachedResult{data: data, expiresAt: time.Now().Add(ttl)}
}

func resolveParams(bindings []ParamBinding, r *http.Request) (map[string]any, []string, error) {
	args := make(map[string]any, len(bindings))
	var orderedKeys []string

	var bodyMap map[string]any
	bodyParsed := false

	hasBody := false
	for _, b := range bindings {
		if b.Source == "body" {
			hasBody = true
			break
		}
	}

	if hasBody && r.Body != nil {
		bodyBytes, err := io.ReadAll(r.Body)
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			var body interface{}
			if json.Unmarshal(bodyBytes, &body) == nil {
				if m, ok := body.(map[string]any); ok {
					bodyMap = m
				}
			}
			bodyParsed = true
		}
	}

	for _, b := range bindings {
		orderedKeys = append(orderedKeys, b.Name)
		var raw string

		switch b.Source {
		case "query":
			raw = r.URL.Query().Get(b.Key)
		case "header":
			raw = r.Header.Get(b.Key)
		case "body":
			if bodyParsed && bodyMap != nil {
				raw = extractBodyField(bodyMap, b.Key)
			}
		case "path":
			raw = r.PathValue(b.Key)
		case "env":
			raw = os.Getenv(b.Key)
		case "placeholder":
			repl, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
			if ok {
				raw = repl.ReplaceAll("{"+b.Key+"}", "")
			}
		}

		if raw == "" {
			raw = b.Default
		}

		coerced, err := coerce(raw, b)
		if err != nil {
			return nil, nil, fmt.Errorf("param %q: %w", b.Name, err)
		}
		args[b.Name] = coerced
	}

	return args, orderedKeys, nil
}

func extractBodyField(m map[string]any, key string) string {
	parts := strings.Split(key, ".")
	var current any = m
	for _, p := range parts {
		cmap, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = cmap[p]
	}
	if current == nil {
		return ""
	}
	switch v := current.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case bool:
		return strconv.FormatBool(v)
	case json.Number:
		return v.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func coerce(raw string, b ParamBinding) (any, error) {
	if raw == "" || raw == "null" {
		return nil, nil
	}

	switch b.Type {
	case "int":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("not an integer: %q", raw)
		}
		if b.Min != nil && float64(n) < *b.Min {
			return nil, fmt.Errorf("value %d below minimum %g", n, *b.Min)
		}
		if b.Max != nil && float64(n) > *b.Max {
			return nil, fmt.Errorf("value %d above maximum %g", n, *b.Max)
		}
		if b.Cap != nil && float64(n) > *b.Cap {
			n = int64(*b.Cap)
		}
		return n, nil
	case "float":
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("not a float: %q", raw)
		}
		if b.Min != nil && f < *b.Min {
			return nil, fmt.Errorf("value %g below minimum %g", f, *b.Min)
		}
		if b.Max != nil && f > *b.Max {
			return nil, fmt.Errorf("value %g above maximum %g", f, *b.Max)
		}
		if b.Cap != nil && f > *b.Cap {
			f = *b.Cap
		}
		return f, nil
	case "bool":
		return strconv.ParseBool(raw)
	case "timestamp":
		return time.Parse(time.RFC3339, raw)
	case "duration":
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, err
		}
		return d.String(), nil
	default:
		if b.Pattern != "" {
			matched, err := regexp.MatchString(b.Pattern, raw)
			if err != nil {
				return nil, fmt.Errorf("invalid pattern: %w", err)
			}
			if !matched {
				return nil, fmt.Errorf("value %q doesn't match pattern %q", raw, b.Pattern)
			}
		}
		return raw, nil
	}
}

func convertNamedToPositional(sqlStr string, args map[string]any, orderedKeys []string) (string, []any) {
	var positionalArgs []any
	result := paramRefRe.ReplaceAllStringFunc(sqlStr, func(match string) string {
		name := match[1:]
		if val, ok := args[name]; ok {
			positionalArgs = append(positionalArgs, val)
			return "?"
		}
		positionalArgs = append(positionalArgs, nil)
		return "?"
	})
	return result, positionalArgs
}

func scanRows(rows *sql.Rows, maxRows int) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	result := make([]map[string]any, 0, 64)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}

	for rows.Next() {
		if len(result) >= maxRows {
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = vals[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
