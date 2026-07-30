package config

import (
	"strings"
	"testing"
)

// load fills in the one variable that has no default, so a case only has to
// name what it is actually testing.
func load(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	t.Setenv("QUECTO_UPSTREAM_URL", "http://127.0.0.1:8080/v1")
	for k, v := range env {
		t.Setenv(k, v)
	}
	return Load()
}

// The dashboard reaches as far as the proxy does. What separates them is the
// port, so that a tunnel can name one without the other.
func TestDashboardDefaultsToItsOwnPort(t *testing.T) {
	c, err := load(t, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.AdminListen == c.Listen {
		t.Fatalf("AdminListen = %q, want a port of its own", c.AdminListen)
	}
	if c.AdminListen != ":11313" {
		t.Errorf("AdminListen = %q, want :11313", c.AdminListen)
	}
}

// Equality is what the rest of the program reads as "one listener", so it has
// to survive Load untouched.
func TestTheSameAddressTwiceSurvivesLoad(t *testing.T) {
	c, err := load(t, map[string]string{
		"QUECTO_LISTEN":       ":9000",
		"QUECTO_ADMIN_LISTEN": ":9000",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.AdminListen != c.Listen {
		t.Errorf("AdminListen = %q, Listen = %q, want them equal", c.AdminListen, c.Listen)
	}
}

// Two spellings of one socket would leave the second listener failing to bind
// with nothing to explain it, so say so up front instead.
func TestAContestedPortIsRefused(t *testing.T) {
	for _, admin := range []string{"0.0.0.0:11312", "127.0.0.1:11312", "[::]:11312"} {
		_, err := load(t, map[string]string{
			"QUECTO_LISTEN":       ":11312",
			"QUECTO_ADMIN_LISTEN": admin,
		})
		if err == nil {
			t.Errorf("QUECTO_ADMIN_LISTEN=%q was accepted alongside :11312", admin)
			continue
		}
		if !strings.Contains(err.Error(), "QUECTO_ADMIN_LISTEN") {
			t.Errorf("error for %q does not name the variable: %v", admin, err)
		}
	}
}

// Different ports on the same host are two listeners, which is the default.
func TestDistinctPortsAreAccepted(t *testing.T) {
	for _, admin := range []string{"127.0.0.1:11313", ":11313", "0.0.0.0:9999"} {
		if _, err := load(t, map[string]string{
			"QUECTO_LISTEN":       ":11312",
			"QUECTO_ADMIN_LISTEN": admin,
		}); err != nil {
			t.Errorf("QUECTO_ADMIN_LISTEN=%q was refused: %v", admin, err)
		}
	}
}

func TestPortsCollide(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{":11312", "0.0.0.0:11312", true},
		{":11312", "127.0.0.1:11312", true},
		{"[::]:11312", "10.0.0.1:11312", true},
		{"127.0.0.1:11312", "127.0.0.1:11312", true},
		{":11312", ":11313", false},
		{"127.0.0.1:11312", "10.0.0.1:11312", false},
		{"garbage", ":11312", false},
	}
	for _, c := range cases {
		if got := portsCollide(c.a, c.b); got != c.want {
			t.Errorf("portsCollide(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
