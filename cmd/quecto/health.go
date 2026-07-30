package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"quecto/internal/metrics"
	"quecto/internal/proxy"
	"quecto/internal/settings"
	"quecto/internal/usage"
)

// health answers the dashboard's status questions by combining the proxy, the
// usage writer, and the latency histogram.
type health struct {
	px     *proxy.Proxy
	writer *usage.Writer
	live   *settings.Live
	gate   *metrics.Histogram
	client *http.Client

	mu        sync.Mutex
	checkedAt time.Time
	lastOK    bool
	lastErr   error
}

const upstreamCheckTTL = 15 * time.Second

func newHealth(px *proxy.Proxy, writer *usage.Writer, live *settings.Live, gate *metrics.Histogram) *health {
	return &health{
		px: px, writer: writer, live: live, gate: gate,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func (h *health) UsageReporting() (bool, bool) { return h.px.UsageReporting() }
func (h *health) LogsDropped() int64           { return h.writer.Dropped() }
func (h *health) OverheadP99() time.Duration   { return h.gate.Quantile(0.99) }

// UpstreamReachable asks the model server for its model list, caching the
// answer briefly so opening the dashboard does not hammer it.
func (h *health) UpstreamReachable(ctx context.Context) (bool, error) {
	h.mu.Lock()
	if time.Since(h.checkedAt) < upstreamCheckTTL {
		ok, err := h.lastOK, h.lastErr
		h.mu.Unlock()
		return ok, err
	}
	h.mu.Unlock()

	ok, err := h.probe(ctx)

	h.mu.Lock()
	h.checkedAt, h.lastOK, h.lastErr = time.Now(), ok, err
	h.mu.Unlock()
	return ok, err
}

func (h *health) probe(ctx context.Context) (bool, error) {
	base := h.live.Upstream()
	target := *base
	target.Path = base.Path + "/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false, err
	}
	if k := h.live.UpstreamAPIKey(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return false, errors.New("the model server did not respond")
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return false, errors.New("the model server returned an error")
	}
	return true, nil
}
