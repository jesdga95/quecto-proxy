// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultListen is "qp" in ASCII decimal, 113 and 112, which keeps Quecto
// clear of 8080 where llama.cpp and LocalAI usually sit.
const defaultListen = ":11312"

type Config struct {
	Listen string

	UpstreamURL           *url.URL
	UpstreamAPIKey        string
	UpstreamHeaderTimeout time.Duration

	DBPath string

	// Used to seed the admin password when none is stored yet.
	AdminPassword string
	SessionSecret string

	LogRetentionDays int
	LogMaxRows       int64

	DefaultMaxTokens  int
	TrustProxyHeaders bool
}

func Load() (*Config, error) {
	c := &Config{
		Listen:                envStr("QUECTO_LISTEN", defaultListen),
		UpstreamAPIKey:        envStr("QUECTO_UPSTREAM_API_KEY", ""),
		DBPath:                envStr("QUECTO_DB_PATH", "./quecto.db"),
		AdminPassword:         envStr("QUECTO_ADMIN_PASSWORD", ""),
		SessionSecret:         envStr("QUECTO_SESSION_SECRET", ""),
		UpstreamHeaderTimeout: envDur("QUECTO_UPSTREAM_HEADER_TIMEOUT", 120*time.Second),
		LogRetentionDays:      envInt("QUECTO_LOG_RETENTION_DAYS", 30),
		LogMaxRows:            int64(envInt("QUECTO_LOG_MAX_ROWS", 1_000_000)),
		DefaultMaxTokens:      envInt("QUECTO_DEFAULT_MAX_TOKENS", 2048),
		TrustProxyHeaders:     envBool("QUECTO_TRUST_PROXY_HEADERS", false),
	}

	raw := envStr("QUECTO_UPSTREAM_URL", "")
	if raw == "" {
		return nil, fmt.Errorf("QUECTO_UPSTREAM_URL is required, for example http://127.0.0.1:8000/v1")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("QUECTO_UPSTREAM_URL is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("QUECTO_UPSTREAM_URL must use http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("QUECTO_UPSTREAM_URL is missing a host")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	c.UpstreamURL = u

	if c.LogRetentionDays < 1 {
		return nil, fmt.Errorf("QUECTO_LOG_RETENTION_DAYS must be at least 1, got %d", c.LogRetentionDays)
	}
	if c.LogMaxRows < 1000 {
		return nil, fmt.Errorf("QUECTO_LOG_MAX_ROWS must be at least 1000, got %d", c.LogMaxRows)
	}
	if c.DefaultMaxTokens < 1 {
		return nil, fmt.Errorf("QUECTO_DEFAULT_MAX_TOKENS must be positive, got %d", c.DefaultMaxTokens)
	}
	return c, nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	// Tolerate thousands separators so values can be pasted from the dashboard.
	n, err := strconv.Atoi(strings.ReplaceAll(strings.ReplaceAll(v, ",", ""), "_", ""))
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDur(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
