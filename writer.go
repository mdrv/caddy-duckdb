package caddyduckdb

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

type RequestRecord struct {
	TS        time.Time
	IP        string
	Method    string
	Host      string
	Path      string
	Query     string
	Status    int
	LatencyMs int64
	BytesSent int64
	UserAgent string
}

type BatchWriter struct {
	db        *sql.DB
	ch        chan RequestRecord
	batchSize int
	flushTick *time.Ticker
	mu        sync.Mutex
	buf       []RequestRecord
	wg        sync.WaitGroup
	stopCh    chan struct{}
	logger    *zap.Logger
}

func NewBatchWriter(db *sql.DB, batchSize int, flushInterval time.Duration, logger *zap.Logger) *BatchWriter {
	w := &BatchWriter{
		db:        db,
		ch:        make(chan RequestRecord, 8192),
		batchSize: batchSize,
		flushTick: time.NewTicker(flushInterval),
		buf:       make([]RequestRecord, 0, batchSize),
		stopCh:    make(chan struct{}),
		logger:    logger,
	}
	w.wg.Add(1)
	go w.run()
	return w
}

func (w *BatchWriter) Write(r RequestRecord) {
	select {
	case w.ch <- r:
	default:
	}
}

func (w *BatchWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flush()
}

func (w *BatchWriter) Stop() {
	close(w.stopCh)
	w.flushTick.Stop()
	w.wg.Wait()
}

func (w *BatchWriter) run() {
	defer w.wg.Done()
	for {
		select {
		case r := <-w.ch:
			w.mu.Lock()
			w.buf = append(w.buf, r)
			if len(w.buf) >= w.batchSize {
				w.flush()
			}
			w.mu.Unlock()
		case <-w.flushTick.C:
			w.mu.Lock()
			if len(w.buf) > 0 {
				w.flush()
			}
			w.mu.Unlock()
		case <-w.stopCh:
			w.mu.Lock()
			w.flush()
			w.mu.Unlock()
			return
		}
	}
}

func (w *BatchWriter) flush() {
	if len(w.buf) == 0 {
		return
	}

	var sb strings.Builder
	sb.WriteString(`INSERT INTO _requests (ts, ip, method, host, path, query, status, latency_ms, bytes_sent, user_agent) VALUES `)

	args := make([]any, 0, len(w.buf)*10)
	for i, r := range w.buf {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?,?,?,?,?,?,?,?,?)")
		args = append(args,
			r.TS, r.IP, r.Method, r.Host, r.Path,
			r.Query, r.Status, r.LatencyMs, r.BytesSent, r.UserAgent,
		)
	}

	_, err := w.db.ExecContext(context.Background(), sb.String(), args...)
	if err != nil {
		w.logger.Error("batch write failed", zap.Error(err), zap.Int("batch_size", len(w.buf)))
	}

	w.buf = w.buf[:0]
}
