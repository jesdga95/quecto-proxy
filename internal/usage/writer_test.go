package usage

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"quecto/internal/config"
	"quecto/internal/proxy"
	"quecto/internal/settings"
	"quecto/internal/store"
)

func newTestWriter(t *testing.T, retentionDays int, maxRows int64) (*Writer, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	u, _ := url.Parse("http://127.0.0.1:9/v1")
	live, err := settings.Load(&config.Config{
		UpstreamURL: u, DefaultMaxTokens: 2048,
		LogRetentionDays: retentionDays, LogMaxRows: maxRows,
	}, st)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}

	if _, err := st.CreateKey(store.NewKey{
		Name: "k1", KeyHash: []byte("hash-one"), KeyPrefix: "qp_aaaa...bbbb", CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return NewWriter(st, live, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

func rec(status int, prompt, completion int64) proxy.Record {
	return proxy.Record{
		KeyID: 1, Model: "m", Status: status, Stream: true,
		Usage:    proxy.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion},
		HasUsage: true, Duration: 1200 * time.Millisecond,
		TTFT: 300 * time.Millisecond, HasTTFT: true,
	}
}

// runUntilIdle drives the writer and stops it once the records have landed.
func runUntilIdle(t *testing.T, w *Writer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	// Give Run a moment to pick records off the channel, then stop it. The
	// shutdown path drains whatever is left, so nothing is lost.
	time.Sleep(50 * time.Millisecond)
	cancel()
	w.Wait()
}

func TestRecordsAreWrittenAndAggregated(t *testing.T) {
	w, st := newTestWriter(t, 30, 1_000_000)

	w.Report(rec(200, 100, 50))
	w.Report(rec(200, 200, 25))
	w.Report(rec(429, 0, 0))
	runUntilIdle(t, w)

	logs, _, err := st.ListLogs(store.LogFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("wrote %d rows, want 3", len(logs))
	}
	if logs[0].KeyName != "k1" {
		t.Errorf("key name = %q, want k1", logs[0].KeyName)
	}
	if logs[0].TTFTMS == nil || *logs[0].TTFTMS != 300 {
		t.Errorf("ttft = %v, want 300", logs[0].TTFTMS)
	}

	now := time.Now().Unix()
	totals, err := st.Totals(now-7200, now+3600)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Requests != 3 {
		t.Errorf("requests = %d, want 3", totals.Requests)
	}
	if totals.Errors != 1 {
		t.Errorf("errors = %d, want 1", totals.Errors)
	}
	if totals.PromptTokens != 300 || totals.CompletionTokens != 75 {
		t.Errorf("tokens = %d/%d, want 300/75", totals.PromptTokens, totals.CompletionTokens)
	}

	k, err := st.GetKey(1)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if k.TotalRequests != 3 || k.TotalPromptTokens != 300 || k.TotalCompletionTokens != 75 {
		t.Errorf("key counters = %d/%d/%d", k.TotalRequests, k.TotalPromptTokens, k.TotalCompletionTokens)
	}
	// The quota ledger rides along here, which is what carries it over a restart.
	if k.TokensSpent != 375 {
		t.Errorf("tokens spent = %d, want 375", k.TokensSpent)
	}
	if k.LastUsedAt == nil {
		t.Error("last used was not recorded")
	}
}

func TestReportNeverBlocksAndCountsDrops(t *testing.T) {
	w, _ := newTestWriter(t, 30, 1_000_000)

	// Nothing is draining, so the buffer fills and the rest must be dropped
	// rather than blocking the caller.
	done := make(chan struct{})
	go func() {
		for i := 0; i < defaultBuffer*2; i++ {
			w.Report(rec(200, 1, 1))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Report blocked when the buffer was full")
	}
	if w.Dropped() == 0 {
		t.Fatal("records were discarded without being counted")
	}
}

func TestShutdownFlushesPendingRecords(t *testing.T) {
	w, st := newTestWriter(t, 30, 1_000_000)

	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	for i := 0; i < 50; i++ {
		w.Report(rec(200, 1, 1))
	}
	// Stop well inside the flush interval so the drain path is what saves them.
	time.Sleep(20 * time.Millisecond)
	cancel()
	w.Wait()

	logs, _, err := st.ListLogs(store.LogFilter{Limit: 200})
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 50 {
		t.Fatalf("wrote %d rows on shutdown, want 50", len(logs))
	}
}

func TestPruneByAge(t *testing.T) {
	w, st := newTestWriter(t, 30, 1_000_000)

	old := time.Now().AddDate(0, 0, -60).Unix()
	if err := st.WriteBatch([]store.LogRow{
		{KeyID: 1, TS: old, Model: "m", Status: 200, DurationMS: 10},
		{KeyID: 1, TS: time.Now().Unix(), Model: "m", Status: 200, DurationMS: 10},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w.prune()

	logs, _, err := st.ListLogs(store.LogFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("kept %d rows, want only the recent one", len(logs))
	}

	// Hourly totals must survive, which is what keeps long range charts working.
	totals, err := st.Totals(old-3600, time.Now().Unix()+3600)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Requests != 2 {
		t.Fatalf("rollup requests = %d, want 2 to outlive the pruned row", totals.Requests)
	}
}

func TestPruneByRowCeiling(t *testing.T) {
	w, st := newTestWriter(t, 30, 10)

	rows := make([]store.LogRow, 0, 25)
	now := time.Now().Unix()
	for i := 0; i < 25; i++ {
		rows = append(rows, store.LogRow{KeyID: 1, TS: now, Model: "m", Status: 200, DurationMS: 5})
	}
	if err := st.WriteBatch(rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w.prune()

	logs, _, err := st.ListLogs(store.LogFilter{Limit: 200})
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 10 {
		t.Fatalf("kept %d rows, want the ceiling of 10", len(logs))
	}
}

func TestLogFiltersAndPaging(t *testing.T) {
	_, st := newTestWriter(t, 30, 1_000_000)

	now := time.Now().Unix()
	rows := []store.LogRow{
		{KeyID: 1, TS: now, Model: "m", Status: 200, DurationMS: 5},
		{KeyID: 1, TS: now, Model: "m", Status: 429, DurationMS: 1},
		{KeyID: 1, TS: now, Model: "m", Status: 500, DurationMS: 9},
		{KeyID: 1, TS: now, Model: "m", Status: 200, DurationMS: 7},
	}
	if err := st.WriteBatch(rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, tc := range []struct {
		status string
		want   int
	}{
		{"", 4}, {"success", 2}, {"rate_limited", 1}, {"server_error", 1}, {"client_error", 1},
	} {
		got, _, err := st.ListLogs(store.LogFilter{Status: tc.status, Limit: 50})
		if err != nil {
			t.Fatalf("filter %q: %v", tc.status, err)
		}
		if len(got) != tc.want {
			t.Errorf("filter %q returned %d rows, want %d", tc.status, len(got), tc.want)
		}
	}

	page1, next, err := st.ListLogs(store.LogFilter{Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1) != 2 || next == nil {
		t.Fatalf("page 1 returned %d rows, cursor %v", len(page1), next)
	}
	page2, _, err := st.ListLogs(store.LogFilter{Limit: 2, Cursor: next})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page 2 returned %d rows, want 2", len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Fatal("the second page repeated the first")
	}
}

func TestPercentileReadsRawRows(t *testing.T) {
	_, st := newTestWriter(t, 30, 1_000_000)

	now := time.Now().Unix()
	rows := make([]store.LogRow, 0, 100)
	for i := 1; i <= 100; i++ {
		ms := int64(i)
		rows = append(rows, store.LogRow{KeyID: 1, TS: now, Model: "m", Status: 200, DurationMS: ms, TTFTMS: &ms})
	}
	if err := st.WriteBatch(rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p95, err := st.Percentile("ttft_ms", now-60, now+60, 0.95)
	if err != nil {
		t.Fatalf("percentile: %v", err)
	}
	if p95 == nil || *p95 < 94 || *p95 > 97 {
		t.Fatalf("p95 = %v, want around 95", p95)
	}

	if _, err := st.Percentile("model", now-60, now+60, 0.95); err == nil {
		t.Fatal("an arbitrary column name was accepted")
	}
}

func TestEstimatedUsageIsStillRecorded(t *testing.T) {
	w, st := newTestWriter(t, 30, 1_000_000)

	w.Report(proxy.Record{
		KeyID: 1, Model: "m", Status: 200, Stream: true,
		Usage:     proxy.Usage{CompletionTokens: 42, TotalTokens: 42},
		Estimated: true, Duration: time.Second,
	})
	runUntilIdle(t, w)

	logs, _, err := st.ListLogs(store.LogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(logs))
	}
	if logs[0].CompletionTokens == nil || *logs[0].CompletionTokens != 42 {
		t.Fatalf("estimated tokens were not stored: %v", logs[0].CompletionTokens)
	}
}
