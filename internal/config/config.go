// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultListen is "qp" in ASCII decimal, 113 and 112, which keeps Quecto
	// clear of 8080 where llama.cpp and LocalAI usually sit.
	defaultListen = ":11312"

	// The dashboard sits on the next port up, reachable wherever the proxy is,
	// so that what publishes it is a tunnel or a reverse proxy naming the port
	// rather than anything Quecto decides on its own.
	defaultAdminListen = ":11313"
)

type Config struct {
	Listen string

	// Where the dashboard and its API listen, on a port of their own so that
	// exposing the proxy port exposes only the proxy. Naming the same address
	// as Listen puts them back on a single port.
	AdminListen string

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
		AdminListen:           envStr("QUECTO_ADMIN_LISTEN", defaultAdminListen),
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

	// An exact match is the way to ask for one listener. Anything else that
	// wants the same socket would fail the second bind with nothing to explain it.
	if c.AdminListen != c.Listen && portsCollide(c.AdminListen, c.Listen) {
		return nil, fmt.Errorf("QUECTO_ADMIN_LISTEN %q and QUECTO_LISTEN %q cannot both bind that port. "+
			"Give the dashboard a port of its own, or name the same address for both to serve them from one listener",
			c.AdminListen, c.Listen)
	}

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

// portsCollide reports whether two listen addresses would contend for one
// socket. A wildcard host already covers every address the other one names.
func portsCollide(a, b string) bool {
	ah, ap, err := net.SplitHostPort(a)
	if err != nil {
		return false
	}
	bh, bp, err := net.SplitHostPort(b)
	if err != nil {
		return false
	}
	if ap != bp {
		return false
	}
	ah, bh = anyHost(ah), anyHost(bh)
	return ah == bh || ah == "" || bh == ""
}

// anyHost folds every spelling of "every interface" onto one.
func anyHost(host string) string {
	switch host {
	case "0.0.0.0", "::", "[::]":
		return ""
	}
	return host
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
