// Package settings holds the configuration that can change while the server runs.
package settings

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"quecto/internal/config"
	"quecto/internal/store"
)

const (
	keyUpstreamURL      = "upstream_url"
	keyUpstreamAPIKey   = "upstream_api_key"
	keyDefaultMaxTokens = "default_max_tokens"
	keyLogRetention     = "log_retention_days"
	keyLogMaxRows       = "log_max_rows"
)

type Values struct {
	UpstreamURL      string `json:"upstream_url"`
	UpstreamAPIKey   string `json:"upstream_api_key"`
	DefaultMaxTokens int    `json:"default_max_tokens"`
	LogRetentionDays int    `json:"log_retention_days"`
	LogMaxRows       int64  `json:"log_max_rows"`
}

// Live serves the current values to the proxy without a database read.
type Live struct {
	st *store.Store

	mu     sync.RWMutex
	v      Values
	parsed *url.URL
}

// Load seeds from the database, falling back to the environment for unset values.
func Load(cfg *config.Config, st *store.Store) (*Live, error) {
	l := &Live{st: st, v: Values{
		UpstreamURL:      cfg.UpstreamURL.String(),
		UpstreamAPIKey:   cfg.UpstreamAPIKey,
		DefaultMaxTokens: cfg.DefaultMaxTokens,
		LogRetentionDays: cfg.LogRetentionDays,
		LogMaxRows:       cfg.LogMaxRows,
	}}

	if s, ok, err := st.Setting(keyUpstreamURL); err != nil {
		return nil, err
	} else if ok {
		l.v.UpstreamURL = s
	}
	if s, ok, err := st.Setting(keyUpstreamAPIKey); err != nil {
		return nil, err
	} else if ok {
		l.v.UpstreamAPIKey = s
	}
	if s, ok, err := st.Setting(keyDefaultMaxTokens); err != nil {
		return nil, err
	} else if ok {
		if n, err := strconv.Atoi(s); err == nil {
			l.v.DefaultMaxTokens = n
		}
	}
	if s, ok, err := st.Setting(keyLogRetention); err != nil {
		return nil, err
	} else if ok {
		if n, err := strconv.Atoi(s); err == nil {
			l.v.LogRetentionDays = n
		}
	}
	if s, ok, err := st.Setting(keyLogMaxRows); err != nil {
		return nil, err
	} else if ok {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			l.v.LogMaxRows = n
		}
	}

	u, err := parseUpstream(l.v.UpstreamURL)
	if err != nil {
		return nil, err
	}
	l.parsed = u
	return l, nil
}

func (l *Live) Values() Values {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.v
}

func (l *Live) Upstream() *url.URL {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.parsed
}

func (l *Live) UpstreamAPIKey() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.v.UpstreamAPIKey
}

func (l *Live) DefaultMaxTokens() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.v.DefaultMaxTokens
}

func (l *Live) Retention() (days int, maxRows int64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.v.LogRetentionDays, l.v.LogMaxRows
}

type Patch struct {
	UpstreamURL      *string `json:"upstream_url"`
	UpstreamAPIKey   *string `json:"upstream_api_key"`
	DefaultMaxTokens *int    `json:"default_max_tokens"`
	LogRetentionDays *int    `json:"log_retention_days"`
	LogMaxRows       *int64  `json:"log_max_rows"`
}

// Apply validates, persists, and swaps in the new values. The error is safe to show a user.
func (l *Live) Apply(p Patch) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	next := l.v
	parsed := l.parsed

	if p.UpstreamURL != nil {
		u, err := parseUpstream(strings.TrimSpace(*p.UpstreamURL))
		if err != nil {
			return err
		}
		next.UpstreamURL, parsed = u.String(), u
	}
	if p.UpstreamAPIKey != nil {
		next.UpstreamAPIKey = strings.TrimSpace(*p.UpstreamAPIKey)
	}
	if p.DefaultMaxTokens != nil {
		if *p.DefaultMaxTokens < 1 {
			return fmt.Errorf("Default max tokens must be at least 1.")
		}
		next.DefaultMaxTokens = *p.DefaultMaxTokens
	}
	if p.LogRetentionDays != nil {
		if *p.LogRetentionDays < 1 {
			return fmt.Errorf("Logs must be kept for at least 1 day.")
		}
		next.LogRetentionDays = *p.LogRetentionDays
	}
	if p.LogMaxRows != nil {
		if *p.LogMaxRows < 1000 {
			return fmt.Errorf("The row ceiling must be at least 1000.")
		}
		next.LogMaxRows = *p.LogMaxRows
	}

	writes := [][2]string{
		{keyUpstreamURL, next.UpstreamURL},
		{keyUpstreamAPIKey, next.UpstreamAPIKey},
		{keyDefaultMaxTokens, strconv.Itoa(next.DefaultMaxTokens)},
		{keyLogRetention, strconv.Itoa(next.LogRetentionDays)},
		{keyLogMaxRows, strconv.FormatInt(next.LogMaxRows, 10)},
	}
	for _, kv := range writes {
		if err := l.st.SetSetting(kv[0], kv[1]); err != nil {
			return err
		}
	}

	l.v, l.parsed = next, parsed
	return nil
}

func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("That upstream URL could not be read.")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("The upstream URL must start with http or https.")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("The upstream URL needs a host, for example http://127.0.0.1:8000/v1")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}
