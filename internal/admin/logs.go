package admin

import (
	"net/http"
	"strconv"
	"time"

	"quecto/internal/store"
)

func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.LogFilter{
		Status: q.Get("status"),
		Limit:  atoiOr(q.Get("limit"), 50),
	}
	f.KeyID = optInt64(q.Get("key_id"))
	f.From = optInt64(q.Get("from"))
	f.To = optInt64(q.Get("to"))
	f.Cursor = optInt64(q.Get("cursor"))

	entries, next, err := s.st.ListLogs(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logs":        entries,
		"next_cursor": next,
	})
}

type statsResponse struct {
	From             int64            `json:"from"`
	To               int64            `json:"to"`
	Totals           store.Totals     `json:"totals"`
	Series           []store.Bucket   `json:"series"`
	TopKeys          []store.KeyTotal `json:"top_keys"`
	TTFTP95MS        *int64           `json:"ttft_p95_ms"`
	DurationP95MS    *int64           `json:"duration_p95_ms"`
	ProxyOverheadP99 float64          `json:"proxy_overhead_p99_ms"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	to := s.now()
	from := to.Add(-rangeOf(r.URL.Query().Get("range")))

	totals, err := s.st.Totals(from.Unix(), to.Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	series, err := s.st.Series(from.Unix(), to.Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	top, err := s.st.TopKeys(from.Unix(), to.Unix(), 5)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ttft, err := s.st.Percentile("ttft_ms", from.Unix(), to.Unix(), 0.95)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dur, err := s.st.Percentile("duration_ms", from.Unix(), to.Unix(), 0.95)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, statsResponse{
		From: from.Unix(), To: to.Unix(),
		Totals: totals, Series: series, TopKeys: top,
		TTFTP95MS: ttft, DurationP95MS: dur,
		ProxyOverheadP99: s.overhead(),
	})
}

type healthResponse struct {
	Upstream        string  `json:"upstream"`
	UpstreamOK      bool    `json:"upstream_ok"`
	UpstreamError   string  `json:"upstream_error,omitempty"`
	CheckedAt       int64   `json:"checked_at"`
	UsageKnown      bool    `json:"usage_reporting_known"`
	UsageSupported  bool    `json:"usage_reporting_supported"`
	LogsDropped     int64   `json:"logs_dropped"`
	LogRows         int64   `json:"log_rows"`
	DatabaseBytes   int64   `json:"database_bytes"`
	KeyCount        int     `json:"key_count"`
	ProxyOverheadMS float64 `json:"proxy_overhead_p99_ms"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	rows, bytes, err := s.st.LogStats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	known, supported := s.health.UsageReporting()
	ok, reachErr := s.health.UpstreamReachable(r.Context())

	resp := healthResponse{
		Upstream:        s.live.Values().UpstreamURL,
		UpstreamOK:      ok,
		CheckedAt:       s.now().Unix(),
		UsageKnown:      known,
		UsageSupported:  supported,
		LogsDropped:     s.health.LogsDropped(),
		LogRows:         rows,
		DatabaseBytes:   bytes,
		KeyCount:        s.registry.Len(),
		ProxyOverheadMS: s.overhead(),
	}
	if reachErr != nil {
		resp.UpstreamError = reachErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) overhead() float64 {
	if s.health == nil {
		return 0
	}
	return s.health.OverheadP99().Seconds() * 1000
}

func rangeOf(v string) time.Duration {
	switch v {
	case "1h":
		return time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	case "90d":
		return 90 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

func optInt64(v string) *int64 {
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func atoiOr(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
