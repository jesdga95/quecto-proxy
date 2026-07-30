// Package usage records one row per proxied request without ever blocking the
// proxy. Records go onto a buffered channel and a single goroutine writes them
// in batches, so the request path never touches the database.
package usage

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"quecto/internal/proxy"
	"quecto/internal/settings"
	"quecto/internal/store"
)

const (
	defaultBuffer = 4096
	batchSize     = 500
	flushEvery    = time.Second
	pruneEvery    = 10 * time.Minute
	// Hourly totals outlive raw rows by a wide margin so charts stay long range.
	rollupRetentionDays = 400
)

type Writer struct {
	st   *store.Store
	live *settings.Live
	log  *slog.Logger
	now  func() time.Time

	ch      chan proxy.Record
	dropped atomic.Int64
	written atomic.Int64
	stopped chan struct{}
}

func NewWriter(st *store.Store, live *settings.Live, log *slog.Logger) *Writer {
	return &Writer{
		st: st, live: live, log: log, now: time.Now,
		ch:      make(chan proxy.Record, defaultBuffer),
		stopped: make(chan struct{}),
	}
}

// Report never blocks. When the buffer is full the record is dropped and
// counted, which the dashboard surfaces rather than hiding.
func (w *Writer) Report(rec proxy.Record) {
	select {
	case w.ch <- rec:
	default:
		w.dropped.Add(1)
	}
}

func (w *Writer) Dropped() int64 { return w.dropped.Load() }
func (w *Writer) Written() int64 { return w.written.Load() }

// Run drains the buffer until the context ends, then flushes what is left.
func (w *Writer) Run(ctx context.Context) {
	defer close(w.stopped)

	batch := make([]store.LogRow, 0, batchSize)
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()

	w.prune()

	for {
		select {
		case rec := <-w.ch:
			batch = append(batch, w.toRow(rec))
			if len(batch) >= batchSize {
				batch = w.write(batch)
			}
		case <-flush.C:
			batch = w.write(batch)
		case <-prune.C:
			batch = w.write(batch)
			w.prune()
		case <-ctx.Done():
			for {
				select {
				case rec := <-w.ch:
					batch = append(batch, w.toRow(rec))
					continue
				default:
				}
				break
			}
			w.write(batch)
			return
		}
	}
}

// Wait blocks until Run has flushed and returned.
func (w *Writer) Wait() { <-w.stopped }

func (w *Writer) write(batch []store.LogRow) []store.LogRow {
	if len(batch) == 0 {
		return batch
	}
	if err := w.st.WriteBatch(batch); err != nil {
		w.log.Error("could not write usage records", "count", len(batch), "error", err)
	} else {
		w.written.Add(int64(len(batch)))
	}
	return batch[:0]
}

func (w *Writer) prune() {
	days, maxRows := w.live.Retention()
	now := w.now()

	n, err := w.st.Prune(now.AddDate(0, 0, -days).Unix(), maxRows)
	if err != nil {
		w.log.Error("could not prune usage records", "error", err)
		return
	}
	if n > 0 {
		w.log.Info("pruned usage records", "rows", n, "retention_days", days)
	}
	if _, err := w.st.PruneRollup(now.AddDate(0, 0, -rollupRetentionDays).Unix()); err != nil {
		w.log.Error("could not prune hourly totals", "error", err)
	}
}

func (w *Writer) toRow(rec proxy.Record) store.LogRow {
	row := store.LogRow{
		KeyID:      rec.KeyID,
		TS:         w.now().Unix(),
		Model:      rec.Model,
		Status:     rec.Status,
		Stream:     rec.Stream,
		DurationMS: rec.Duration.Milliseconds(),
	}
	if rec.HasUsage || rec.Estimated {
		p, c := rec.Usage.PromptTokens, rec.Usage.CompletionTokens
		row.PromptTokens, row.CompletionTokens = &p, &c
	}
	if rec.HasTTFT {
		ms := rec.TTFT.Milliseconds()
		row.TTFTMS = &ms
	}
	return row
}
