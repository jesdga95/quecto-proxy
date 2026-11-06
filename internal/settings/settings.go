// Package settings holds the configuration that can change while the server runs.
package settings

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"quecto/internal/config"
	"quecto/internal/store"
)

const (
	keyUpstreamURL      = "upstream_url"
	keyUpstreamAPIKey   = "upstream_api_key"
	keyDefaultMaxTokens = "default_max_tokens"
	keyLogRetention     = "log_retention_days"
	keyLogMaxRows       = "log_max_rows"
	keyMaxRequestBody   = "max_request_body_mib"
	keyHeaderTimeout    = "upstream_header_timeout_s"
	keySessionDays      = "session_lifetime_days"
)

type Values struct {
	UpstreamURL       string `json:"upstream_url"`
	UpstreamAPIKey    string `json:"upstream_api_key"`
	DefaultMaxTokens  int    `json:"default_max_tokens"`
	LogRetentionDays  int    `json:"log_retention_days"`
	LogMaxRows        int64  `json:"log_max_rows"`
	MaxRequestBodyMiB int64  `json:"max_request_body_mib"`
	HeaderTimeoutS    int64  `json:"upstream_header_timeout_s"`
	SessionDays       int    `json:"session_lifetime_days"`
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
		UpstreamURL:       cfg.UpstreamURL.String(),
		UpstreamAPIKey:    cfg.UpstreamAPIKey,
		DefaultMaxTokens:  cfg.DefaultMaxTokens,
		LogRetentionDays:  cfg.LogRetentionDays,
		LogMaxRows:        cfg.LogMaxRows,
		MaxRequestBodyMiB: cfg.MaxRequestBody >> 20,
		SessionDays:       cfg.SessionLifetimeDays,
		HeaderTimeoutS:    int64((cfg.UpstreamHeaderTimeout + time.Second - 1) / time.Second),
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

	if s, ok, err := st.Setting(keyMaxRequestBody); err != nil {
		return nil, err
	} else if ok {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			l.v.MaxRequestBodyMiB = n
		}
	}

	if s, ok, err := st.Setting(keyHeaderTimeout); err != nil {
		return nil, err
	} else if ok {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			l.v.HeaderTimeoutS = n
		}
	}

	if s, ok, err := st.Setting(keySessionDays); err != nil {
		return nil, err
	} else if ok {
		if n, err := strconv.Atoi(s); err == nil {
			l.v.SessionDays = n
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

// MaxRequestBody is the largest request body the proxy reads, in bytes.
func (l *Live) MaxRequestBody() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.v.MaxRequestBodyMiB << 20
}

// HeaderTimeout is how long the proxy waits for the model server to start replying.
func (l *Live) HeaderTimeout() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return time.Duration(l.v.HeaderTimeoutS) * time.Second
}

// SessionTTL is how long a newly issued dashboard login stays valid.
func (l *Live) SessionTTL() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return time.Duration(l.v.SessionDays) * 24 * time.Hour
}

type Patch struct {
	UpstreamURL       *string `json:"upstream_url"`
	UpstreamAPIKey    *string `json:"upstream_api_key"`
	DefaultMaxTokens  *int    `json:"default_max_tokens"`
	LogRetentionDays  *int    `json:"log_retention_days"`
	LogMaxRows        *int64  `json:"log_max_rows"`
	MaxRequestBodyMiB *int64  `json:"max_request_body_mib"`
	HeaderTimeoutS    *int64  `json:"upstream_header_timeout_s"`
	SessionDays       *int    `json:"session_lifetime_days"`
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

	if p.MaxRequestBodyMiB != nil {
		if *p.MaxRequestBodyMiB < 1 {
			return fmt.Errorf("The request size limit must be at least 1 MiB.")
		}
		next.MaxRequestBodyMiB = *p.MaxRequestBodyMiB
	}

	if p.HeaderTimeoutS != nil {
		if *p.HeaderTimeoutS < 1 {
			return fmt.Errorf("The upstream timeout must be at least 1 second.")
		}
		next.HeaderTimeoutS = *p.HeaderTimeoutS
	}

	if p.SessionDays != nil {
		if *p.SessionDays < 1 || *p.SessionDays > 365 {
			return fmt.Errorf("A login must last between 1 and 365 days.")
		}
		next.SessionDays = *p.SessionDays
	}

	writes := [][2]string{
		{keyUpstreamURL, next.UpstreamURL},
		{keyUpstreamAPIKey, next.UpstreamAPIKey},
		{keyDefaultMaxTokens, strconv.Itoa(next.DefaultMaxTokens)},
		{keyLogRetention, strconv.Itoa(next.LogRetentionDays)},
		{keyLogMaxRows, strconv.FormatInt(next.LogMaxRows, 10)},
		{keyMaxRequestBody, strconv.FormatInt(next.MaxRequestBodyMiB, 10)},
		{keyHeaderTimeout, strconv.FormatInt(next.HeaderTimeoutS, 10)},
		{keySessionDays, strconv.Itoa(next.SessionDays)},
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
