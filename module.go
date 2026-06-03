package caddyduckdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	httpcaddyfile "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/duckdb/duckdb-go/v2"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Middleware{})
	httpcaddyfile.RegisterHandlerDirective("duckdb", parseCaddyfileHandler)
}

func parseCaddyfileHandler(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(Middleware)
	err := m.UnmarshalCaddyfile(h.Dispenser)
	if err != nil {
		return nil, err
	}
	return m, nil
}

type Middleware struct {
	DBPath        string         `json:"db_path"`
	DBSettings    []SettingKV    `json:"db_settings,omitempty"`
	ReadOnly      bool           `json:"read_only,omitempty"`
	Checkpoint    caddy.Duration `json:"checkpoint_interval,omitempty"`
	Retention     caddy.Duration `json:"retention,omitempty"`
	BatchSize     int            `json:"batch_size,omitempty"`
	FlushInterval caddy.Duration `json:"flush_interval,omitempty"`
	BufferSize    int            `json:"buffer_size,omitempty"`
	Overflow      string         `json:"overflow,omitempty"`
	ExcludePaths  []string       `json:"exclude_paths,omitempty"`
	ExcludeFields []string       `json:"exclude_fields,omitempty"`
	Schema        []string       `json:"schema,omitempty"`
	Queries       []QueryDef     `json:"queries,omitempty"`
	Routes        []QueryRoute   `json:"routes,omitempty"`
	Partitioning  *PartConfig    `json:"partitioning,omitempty"`
	APIPath       string         `json:"api_path,omitempty"`
	APIToken      string         `json:"api_token,omitempty"`
	RawSQL        bool           `json:"raw_sql,omitempty"`
	MaxRows       int            `json:"max_rows,omitempty"`
	CORSOrigin    string         `json:"cors_origin,omitempty"`

	db     *sql.DB
	writer *BatchWriter
	reg    *QueryRegistry
	logger *zap.Logger
	routes []matchedRoute
	mu     sync.RWMutex
}

type SettingKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (Middleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.duckdb",
		New: func() caddy.Module { return new(Middleware) },
	}
}

func (m *Middleware) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger(m)
	m.applyDefaults()

	var err error
	m.db, err = sql.Open("duckdb", m.buildDSN())
	if err != nil {
		return fmt.Errorf("open duckdb: %w", err)
	}

	m.db.SetMaxOpenConns(4)

	for _, s := range m.DBSettings {
		if _, err := m.db.Exec(fmt.Sprintf("SET %s = '%s'", s.Key, s.Value)); err != nil {
			return fmt.Errorf("set %s=%s: %w", s.Key, s.Value, err)
		}
	}

	if !m.ReadOnly {
		if err := m.bootstrapSchema(); err != nil {
			return fmt.Errorf("schema bootstrap: %w", err)
		}
		for i, ddl := range m.Schema {
			if _, err := m.db.Exec(ddl); err != nil {
				return fmt.Errorf("schema[%d]: %w", i, err)
			}
		}
	}

	m.reg = NewQueryRegistry(m.db, m.MaxRows, m.logger)
	for i := range m.Queries {
		if err := m.reg.Register(&m.Queries[i]); err != nil {
			return fmt.Errorf("register query %q: %w", m.Queries[i].Name, err)
		}
	}

	if err := m.reg.ValidateAll(); err != nil {
		return fmt.Errorf("query validation: %w", err)
	}

	m.routes = make([]matchedRoute, 0, len(m.Routes))
	for i := range m.Routes {
		mr, err := m.buildRoute(&m.Routes[i])
		if err != nil {
			return fmt.Errorf("route %s %s: %w", m.Routes[i].Method, m.Routes[i].Path, err)
		}
		m.routes = append(m.routes, mr)
	}

	if !m.ReadOnly {
		m.writer = NewBatchWriter(m.db, m.BatchSize, time.Duration(m.FlushInterval), m.logger)
	}

	if m.Partitioning != nil {
		go m.startPartitioner(ctx)
	}

	go m.startMaintenance(ctx)

	m.logger.Info("duckdb module provisioned",
		zap.String("db_path", m.DBPath),
		zap.Int("queries", len(m.Queries)),
		zap.Int("routes", len(m.Routes)),
	)

	return nil
}

func (m *Middleware) Cleanup() error {
	if m.writer != nil {
		m.writer.Flush()
		m.writer.Stop()
	}
	if m.db != nil {
		m.db.Exec("CHECKPOINT")
		return m.db.Close()
	}
	return nil
}

func (m *Middleware) applyDefaults() {
	if m.BatchSize == 0 {
		m.BatchSize = 500
	}
	if m.FlushInterval == 0 {
		m.FlushInterval = caddy.Duration(200 * time.Millisecond)
	}
	if m.BufferSize == 0 {
		m.BufferSize = 8192
	}
	if m.Overflow == "" {
		m.Overflow = "drop"
	}
	if m.MaxRows == 0 {
		m.MaxRows = 10000
	}
	if m.APIPath == "" {
		m.APIPath = "/_duckdb"
	}
	if m.Checkpoint == 0 {
		m.Checkpoint = caddy.Duration(6 * time.Hour)
	}
	if m.Retention == 0 {
		m.Retention = caddy.Duration(90 * 24 * time.Hour)
	}
}

func (m *Middleware) buildDSN() string {
	if m.ReadOnly {
		return m.DBPath + "?access_mode=read_only"
	}
	return m.DBPath
}

func (m *Middleware) bootstrapSchema() error {
	_, err := m.db.Exec(`
		CREATE TABLE IF NOT EXISTS _requests (
			ts           TIMESTAMPTZ NOT NULL,
			ip           VARCHAR     NOT NULL,
			method       VARCHAR(8)  NOT NULL,
			host         VARCHAR     NOT NULL,
			path         VARCHAR     NOT NULL,
			query        VARCHAR,
			status       SMALLINT    NOT NULL,
			latency_ms   INTEGER     NOT NULL,
			bytes_sent   BIGINT      NOT NULL,
			user_agent   VARCHAR
		)
	`)
	return err
}

func (m *Middleware) startMaintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(m.Checkpoint))
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := m.db.Exec("CHECKPOINT"); err != nil {
				m.logger.Error("checkpoint failed", zap.Error(err))
			}
			retention := time.Duration(m.Retention)
			if retention > 0 {
				if _, err := m.db.Exec(
					"DELETE FROM _requests WHERE ts < NOW() - INTERVAL '1 millisecond' * $1",
					retention.Milliseconds(),
				); err != nil {
					m.logger.Error("retention cleanup failed", zap.Error(err))
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

var paramRefRe = regexp.MustCompile(`\$(\w+)`)
var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func extractParamRefs(sql string) []string {
	matches := paramRefRe.FindAllStringSubmatch(sql, -1)
	seen := make(map[string]bool)
	var result []string
	for _, m := range matches {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	return result
}

func (m *Middleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	for d.NextBlock(0) {
		switch d.Val() {
		case "db_path":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.DBPath = d.Val()
		case "db_setting":
			var kv SettingKV
			if !d.NextArg() {
				return d.ArgErr()
			}
			kv.Key = d.Val()
			if !d.NextArg() {
				return d.ArgErr()
			}
			kv.Value = d.Val()
			m.DBSettings = append(m.DBSettings, kv)
		case "read_only":
			m.ReadOnly = true
		case "retention":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid retention duration: %v", err)
			}
			m.Retention = caddy.Duration(dur)
		case "checkpoint":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid checkpoint duration: %v", err)
			}
			m.Checkpoint = caddy.Duration(dur)
		case "batch_size":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n, err := parseInt(d.Val())
			if err != nil {
				return d.Errf("invalid batch_size: %v", err)
			}
			m.BatchSize = n
		case "flush_interval":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid flush_interval: %v", err)
			}
			m.FlushInterval = caddy.Duration(dur)
		case "buffer_size":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n, err := parseInt(d.Val())
			if err != nil {
				return d.Errf("invalid buffer_size: %v", err)
			}
			m.BufferSize = n
		case "overflow":
			if !d.NextArg() {
				return d.ArgErr()
			}
			v := d.Val()
			if v != "drop" && v != "block" {
				return d.Errf("overflow must be drop or block, got %q", v)
			}
			m.Overflow = v
		case "exclude_path":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.ExcludePaths = append(m.ExcludePaths, d.Val())
		case "exclude_field":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.ExcludeFields = append(m.ExcludeFields, d.Val())
		case "schema":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.Schema = append(m.Schema, d.Val())
		case "query":
			qd, err := parseQueryDef(d)
			if err != nil {
				return err
			}
			m.Queries = append(m.Queries, qd)
		case "route":
			r, err := parseQueryRoute(d)
			if err != nil {
				return err
			}
			m.Routes = append(m.Routes, r)
		case "partitioning":
			pc, err := parsePartitionConfig(d)
			if err != nil {
				return err
			}
			m.Partitioning = pc
		case "api_path":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.APIPath = d.Val()
		case "api_token":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.APIToken = d.Val()
		case "raw_sql":
			if d.NextArg() {
				m.RawSQL = d.Val() == "on" || d.Val() == "true"
			} else {
				m.RawSQL = true
			}
		case "max_rows":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n, err := parseInt(d.Val())
			if err != nil {
				return d.Errf("invalid max_rows: %v", err)
			}
			m.MaxRows = n
		case "cors_origin":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.CORSOrigin = d.Val()
		default:
			return d.Errf("unknown directive: %s", d.Val())
		}
	}

	if m.DBPath == "" {
		return d.Err("db_path is required")
	}

	return m.validate()
}

func (m *Middleware) validate() error {
	queryNames := make(map[string]bool)
	for _, q := range m.Queries {
		if q.Name == "" {
			return fmt.Errorf("query missing name")
		}
		if q.SQL == "" {
			return fmt.Errorf("query %q has no sql", q.Name)
		}
		queryNames[q.Name] = true
	}
	for _, r := range m.Routes {
		if !queryNames[r.QueryName] {
			return fmt.Errorf("route references unknown query %q", r.QueryName)
		}
	}
	if m.Partitioning != nil {
		for _, tbl := range m.Partitioning.Tables {
			if !identRe.MatchString(tbl) {
				return fmt.Errorf("partitioning table %q is not a valid identifier", tbl)
			}
		}
	}
	return nil
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func parseQueryDef(d *caddyfile.Dispenser) (QueryDef, error) {
	var qd QueryDef
	if !d.NextArg() {
		return qd, d.ArgErr()
	}
	qd.Name = d.Val()

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "sql":
			if !d.NextArg() {
				return qd, d.ArgErr()
			}
			qd.SQL = d.Val()
		case "sql_file":
			if !d.NextArg() {
				return qd, d.ArgErr()
			}
			data, err := os.ReadFile(d.Val())
			if err != nil {
				return qd, d.Errf("read sql_file: %v", err)
			}
			qd.SQL = string(data)
		case "param":
			p, err := parseParamInline(d)
			if err != nil {
				return qd, err
			}
			qd.Params = append(qd.Params, p)
		case "output":
			o, err := parseOutputConfig(d)
			if err != nil {
				return qd, err
			}
			qd.Output = o
		case "cache":
			if !d.NextArg() {
				return qd, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return qd, d.ArgErr()
			}
			qd.CacheTTL = caddy.Duration(dur)
		case "timeout":
			if !d.NextArg() {
				return qd, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return qd, d.ArgErr()
			}
			qd.Timeout = caddy.Duration(dur)
		default:
			return qd, d.Errf("unknown query option: %s", d.Val())
		}
	}
	return qd, nil
}

func parseParamInline(d *caddyfile.Dispenser) (ParamBinding, error) {
	var p ParamBinding
	if !d.NextArg() {
		return p, d.ArgErr()
	}
	p.Name = d.Val()

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "from":
			if !d.NextArg() {
				return p, d.ArgErr()
			}
			p.Source = d.Val()
		case "key":
			if !d.NextArg() {
				return p, d.ArgErr()
			}
			p.Key = d.Val()
		case "type":
			if !d.NextArg() {
				return p, d.ArgErr()
			}
			p.Type = d.Val()
		case "default":
			if !d.NextArg() {
				return p, d.ArgErr()
			}
			p.Default = d.Val()
		case "min":
			if !d.NextArg() {
				return p, d.ArgErr()
			}
			f, err := parseFloat(d.Val())
			if err != nil {
				return p, err
			}
			p.Min = &f
		case "max":
			if !d.NextArg() {
				return p, d.ArgErr()
			}
			f, err := parseFloat(d.Val())
			if err != nil {
				return p, err
			}
			p.Max = &f
		case "cap":
			if !d.NextArg() {
				return p, d.ArgErr()
			}
			f, err := parseFloat(d.Val())
			if err != nil {
				return p, err
			}
			p.Cap = &f
		case "pattern":
			if !d.NextArg() {
				return p, d.ArgErr()
			}
			p.Pattern = d.Val()
		default:
			return p, d.Errf("unknown param option: %s", d.Val())
		}
	}
	return p, nil
}

func parseOutputConfig(d *caddyfile.Dispenser) (OutputConfig, error) {
	var o OutputConfig
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "format":
			if !d.NextArg() {
				return o, d.ArgErr()
			}
			o.Format = d.Val()
		case "envelope":
			if !d.NextArg() {
				o.Envelope = true
			} else {
				o.Envelope = d.Val() == "on"
			}
		case "alias":
			var col, alias string
			if !d.NextArg() {
				return o, d.ArgErr()
			}
			col = d.Val()
			if !d.NextArg() {
				return o, d.ArgErr()
			}
			alias = d.Val()
			if o.Aliases == nil {
				o.Aliases = make(map[string]string)
			}
			o.Aliases[col] = alias
		case "omit":
			if !d.NextArg() {
				return o, d.ArgErr()
			}
			o.Omit = append(o.Omit, d.Val())
		case "status":
			if !d.NextArg() {
				return o, d.ArgErr()
			}
			n, err := parseInt(d.Val())
			if err != nil {
				return o, err
			}
			o.Status = n
		case "body":
			if !d.NextArg() {
				return o, d.ArgErr()
			}
			o.Body = d.Val()
		default:
			return o, d.Errf("unknown output option: %s", d.Val())
		}
	}
	return o, nil
}

func parseQueryRoute(d *caddyfile.Dispenser) (QueryRoute, error) {
	var r QueryRoute
	args := d.RemainingArgs()
	if len(args) < 3 {
		return r, d.Errf("route requires: METHOD path query_name")
	}
	r.Method = args[0]
	r.Path = args[1]
	r.QueryName = args[2]

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "require_header":
			var name, value string
			if !d.NextArg() {
				return r, d.ArgErr()
			}
			name = d.Val()
			if !d.NextArg() {
				return r, d.ArgErr()
			}
			value = d.Val()
			if r.RequireHeaders == nil {
				r.RequireHeaders = make(map[string]string)
			}
			r.RequireHeaders[name] = value
		case "rate_limit":
			if !d.NextArg() {
				return r, d.ArgErr()
			}
			n, err := parseInt(d.Val())
			if err != nil {
				return r, err
			}
			if !d.NextArg() || d.Val() != "per" {
				return r, d.Errf("rate_limit syntax: rate_limit <n> per <duration>")
			}
			if !d.NextArg() {
				return r, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return r, d.Errf("invalid rate_limit duration: %v", err)
			}
			r.RateLimit = &RateLimit{Requests: n, Window: caddy.Duration(dur)}
		default:
			return r, d.Errf("unknown route option: %s", d.Val())
		}
	}
	return r, nil
}

func parsePartitionConfig(d *caddyfile.Dispenser) (*PartConfig, error) {
	pc := &PartConfig{
		Format:       "parquet",
		Interval:     "daily",
		Filename:     "{table}_{date}.parquet",
		KeepDays:     1,
		Compress:     "zstd",
		TimestampCol: "ts",
	}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "table":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			pc.Tables = append(pc.Tables, d.Val())
		case "timestamp_col":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			pc.TimestampCol = d.Val()
		case "timestamp_type":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			v := strings.ToLower(d.Val())
			if v != "timestamptz" && v != "epoch" && v != "auto" {
				return nil, d.Errf("timestamp_type must be timestamptz, epoch, or auto, got %q", v)
			}
			pc.TimestampType = v
		case "interval":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			pc.Interval = d.Val()
		case "format":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			pc.Format = d.Val()
		case "path":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			pc.Path = d.Val()
		case "filename":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			pc.Filename = d.Val()
		case "keep_days":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			n, err := parseInt(d.Val())
			if err != nil {
				return nil, err
			}
			pc.KeepDays = n
		case "compress":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			pc.Compress = d.Val()
		case "max_age":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return nil, d.Errf("invalid max_age: %v", err)
			}
			pc.MaxAge = caddy.Duration(dur)
		default:
			return nil, d.Errf("unknown partitioning option: %s", d.Val())
		}
	}
	return pc, nil
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}

var _ caddy.Provisioner = (*Middleware)(nil)
var _ caddy.CleanerUpper = (*Middleware)(nil)
var _ caddyfile.Unmarshaler = (*Middleware)(nil)

type QueryDef struct {
	Name     string         `json:"name"`
	SQL      string         `json:"sql"`
	Params   []ParamBinding `json:"params,omitempty"`
	Output   OutputConfig   `json:"output,omitempty"`
	CacheTTL caddy.Duration `json:"cache_ttl,omitempty"`
	Timeout  caddy.Duration `json:"timeout,omitempty"`
}

type ParamBinding struct {
	Name    string   `json:"name"`
	Source  string   `json:"source"`
	Key     string   `json:"key"`
	Type    string   `json:"type,omitempty"`
	Default string   `json:"default,omitempty"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Cap     *float64 `json:"cap,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
}

type OutputConfig struct {
	Format   string            `json:"format,omitempty"`
	Envelope bool              `json:"envelope,omitempty"`
	Aliases  map[string]string `json:"aliases,omitempty"`
	Omit     []string          `json:"omit,omitempty"`
	Status   int               `json:"status,omitempty"`
	Body     string            `json:"body,omitempty"`
}

type QueryRoute struct {
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	QueryName      string            `json:"query_name"`
	RequireHeaders map[string]string `json:"require_headers,omitempty"`
	RateLimit      *RateLimit        `json:"rate_limit,omitempty"`
}

type RateLimit struct {
	Requests int            `json:"requests"`
	Window   caddy.Duration `json:"window"`
}

type PartConfig struct {
	Tables        []string       `json:"tables"`
	TimestampCol  string         `json:"timestamp_col,omitempty"`
	TimestampType string         `json:"timestamp_type,omitempty"`
	Interval      string         `json:"interval,omitempty"`
	Format        string         `json:"format,omitempty"`
	Path          string         `json:"path,omitempty"`
	Filename      string         `json:"filename,omitempty"`
	KeepDays      int            `json:"keep_days,omitempty"`
	Compress      string         `json:"compress,omitempty"`
	MaxAge        caddy.Duration `json:"max_age,omitempty"`
}

type matchedRoute struct {
	method    string
	path      string
	pathParts []string
	queryName string
	headers   map[string]string
	rateLimit *RateLimit
	counter   map[string]*rateCounter
	counterMu sync.Mutex
}

type rateCounter struct {
	count   int
	resetAt time.Time
}

func (m *Middleware) buildRoute(r *QueryRoute) (matchedRoute, error) {
	mr := matchedRoute{
		method:    strings.ToUpper(r.Method),
		path:      r.Path,
		pathParts: strings.Split(r.Path, "/"),
		queryName: r.QueryName,
		headers:   r.RequireHeaders,
		rateLimit: r.RateLimit,
	}
	if mr.rateLimit != nil {
		mr.counter = make(map[string]*rateCounter)
	}
	return mr, nil
}

func (m *Middleware) startPartitioner(ctx context.Context) {
	if m.Partitioning == nil {
		return
	}
	pc := m.Partitioning
	if err := os.MkdirAll(pc.Path, 0755); err != nil {
		m.logger.Error("partition dir create failed", zap.Error(err))
		return
	}

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			for _, tbl := range pc.Tables {
				m.archiveTable(ctx, tbl, pc)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (m *Middleware) archiveTable(ctx context.Context, table string, pc *PartConfig) {
	cutoff := time.Now().AddDate(0, 0, -pc.KeepDays)
	dateStr := cutoff.Format("2006-01-02")

	fname := strings.ReplaceAll(pc.Filename, "{table}", table)
	fname = strings.ReplaceAll(fname, "{date}", dateStr)
	fname = strings.ReplaceAll(fname, "{year}", cutoff.Format("2006"))
	fname = strings.ReplaceAll(fname, "{month}", cutoff.Format("01"))
	archivePath := filepath.Join(pc.Path, fname)

	format := strings.ToUpper(pc.Format)
	if format == "" {
		format = "PARQUET"
	}

	tsCol := pc.TimestampCol
	if tsCol == "" {
		tsCol = "ts"
	}

	isEpoch := m.isEpochColumn(ctx, table, tsCol, pc.TimestampType)

	var exportSQL string
	var delSQL string
	var arg any

	if isEpoch {
		exportSQL = fmt.Sprintf(
			"COPY (SELECT * FROM %s WHERE %s < $1) TO '%s' (FORMAT %s, COMPRESSION '%s')",
			table, tsCol, archivePath, format, pc.Compress,
		)
		delSQL = fmt.Sprintf("DELETE FROM %s WHERE %s < $1", table, tsCol)
		arg = cutoff.Unix()
	} else {
		exportSQL = fmt.Sprintf(
			"COPY (SELECT * FROM %s WHERE %s < $1) TO '%s' (FORMAT %s, COMPRESSION '%s')",
			table, tsCol, archivePath, format, pc.Compress,
		)
		delSQL = fmt.Sprintf("DELETE FROM %s WHERE %s < $1", table, tsCol)
		arg = cutoff
	}

	res, err := m.db.ExecContext(ctx, exportSQL, arg)
	if err != nil {
		m.logger.Error("partition export failed", zap.String("table", table), zap.Error(err))
		return
	}

	affected, _ := res.RowsAffected()
	if affected == 0 {
		os.Remove(archivePath)
		return
	}

	_, err = m.db.ExecContext(ctx, delSQL, arg)
	if err != nil {
		m.logger.Error("partition delete failed", zap.String("table", table), zap.Error(err))
		return
	}

	m.db.ExecContext(ctx, "CHECKPOINT")

	m.logger.Info("partition archived",
		zap.String("table", table),
		zap.String("file", archivePath),
		zap.Int64("rows", affected),
		zap.Bool("epoch_ts", isEpoch),
	)

	if pc.MaxAge > 0 {
		m.purgeOldFiles(pc)
	}
}

func (m *Middleware) isEpochColumn(ctx context.Context, table, col, hint string) bool {
	switch hint {
	case "epoch":
		return true
	case "timestamptz":
		return false
	}
	var dataType string
	err := m.db.QueryRowContext(ctx,
		"SELECT data_type FROM information_schema.columns WHERE table_name = $1 AND column_name = $2",
		table, col,
	).Scan(&dataType)
	if err != nil {
		m.logger.Warn("could not detect timestamp column type, assuming timestamptz",
			zap.String("table", table), zap.String("column", col), zap.Error(err))
		return false
	}
	dt := strings.ToUpper(dataType)
	return dt == "BIGINT" || dt == "INTEGER" || dt == "INT" || dt == "INT64"
}

func (m *Middleware) purgeOldFiles(pc *PartConfig) {
	maxAge := time.Duration(pc.MaxAge)
	cutoff := time.Now().Add(-maxAge)

	entries, err := os.ReadDir(pc.Path)
	if err != nil {
		return
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(pc.Path, e.Name())
			os.Remove(path)
			m.logger.Info("purged old partition", zap.String("file", path))
		}
	}
}

func (m *Middleware) rewriteForPartitioning(sqlStr string, table string) string {
	if m.Partitioning == nil {
		return sqlStr
	}
	for _, tbl := range m.Partitioning.Tables {
		if tbl != table {
			continue
		}
		pattern := filepath.Join(m.Partitioning.Path, tbl+"_*.parquet")
		oldFROM := "FROM " + tbl
		if !strings.Contains(strings.ToUpper(sqlStr), "SELECT") {
			return sqlStr
		}
		newFROM := fmt.Sprintf(
			"FROM (SELECT * FROM %s UNION ALL SELECT * FROM read_parquet('%s')) AS %s",
			tbl, pattern, tbl,
		)
		return strings.Replace(sqlStr, oldFROM, newFROM, 1)
	}
	return sqlStr
}
