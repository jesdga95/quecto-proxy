package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quecto/internal/config"
	"quecto/internal/web"
)

// The mounts stand in for the real handlers. What is under test here is which
// port each one is reachable on, not what any of them serves.
func marker(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(name))
	})
}

func build(t *testing.T, adminListen string) (public, admin *http.ServeMux) {
	t.Helper()
	cfg := &config.Config{Listen: ":11312", AdminListen: adminListen}
	return routes(cfg, marker("admin-api"), marker("dashboard"), marker("proxy"))
}

func status(h http.Handler, method, path string) int {
	return probe(h, method, path).Code
}

func probe(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// A reply nothing can parse is the one an API client cannot report, so every
// miss on an API port has to carry its message as JSON.
func TestAMissOnTheProxyPortIsStillTheApiShape(t *testing.T) {
	proxyMux, _ := build(t, "127.0.0.1:11313")

	for _, c := range []struct{ method, path string }{
		{"GET", "/nope"},
		{"GET", "/"},
		{"POST", "/chat/completions"}, // the /v1 prefix left off
		{"GET", "/admin/"},            // moved to its own port
		{"POST", "/admin/api/session"},
	} {
		rec := probe(proxyMux, c.method, c.path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", c.method, c.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s Content-Type = %q, want application/json", c.method, c.path, ct)
		}

		var body struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s body is not JSON: %v", c.method, c.path, err)
			continue
		}
		if body.Error.Code != "unknown_url" {
			t.Errorf("%s %s code = %q, want unknown_url", c.method, c.path, body.Error.Code)
		}
		if !strings.Contains(body.Error.Message, c.path) {
			t.Errorf("%s %s message does not name the path: %q", c.method, c.path, body.Error.Message)
		}
	}
}

// A port answers in the shape of what it is for. The dashboard's port is
// opened in a browser, so a stray URL there is a page, not the API's JSON.
func TestEachPortMissesInItsOwnShape(t *testing.T) {
	proxyMux, adminMux := build(t, "127.0.0.1:11313")

	rec := probe(adminMux, "GET", "/123")
	if rec.Code != http.StatusNotFound {
		t.Errorf("dashboard port GET /123 = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("dashboard port GET /123 Content-Type = %q, want html", ct)
	}

	rec = probe(proxyMux, "GET", "/123")
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("proxy port GET /123 Content-Type = %q, want application/json", ct)
	}
}

// The API sits under the dashboard's own prefix, so the more specific mount
// has to win or every API call would be answered with markup.
func TestTheApiAndTheDashboardDoNotOverlap(t *testing.T) {
	_, adminMux := build(t, "127.0.0.1:11313")

	for _, c := range []struct{ path, want string }{
		{"/admin/api/keys", "admin-api"},
		{"/admin/api/nope", "admin-api"},
		{"/admin/api/", "admin-api"},
		{"/admin/", "dashboard"},
		{"/admin/keys", "dashboard"},
		{"/admin/assets/app.js", "dashboard"},
	} {
		if got := probe(adminMux, "GET", c.path).Body.String(); got != c.want {
			t.Errorf("GET %s reached %q, want %q", c.path, got, c.want)
		}
	}
}

// The whole point of a separate address: exposing the proxy port must not
// expose the dashboard or the login endpoint along with it.
func TestSplitKeepsAdminOffTheProxyPort(t *testing.T) {
	public, admin := build(t, "127.0.0.1:11313")

	if public == admin {
		t.Fatal("a separate address must mean a separate mux")
	}

	for _, path := range []string{"/admin/", "/admin/keys", "/admin/api/keys", "/"} {
		if got := status(public, "GET", path); got != http.StatusNotFound {
			t.Errorf("proxy port GET %s = %d, want 404", path, got)
		}
	}
	if got := status(public, "POST", "/admin/api/session"); got != http.StatusNotFound {
		t.Errorf("proxy port serves the login endpoint: %d, want 404", got)
	}

	// The proxy itself is untouched by the split.
	if got := status(public, "POST", "/v1/chat/completions"); got != http.StatusOK {
		t.Errorf("proxy port GET /v1/... = %d, want 200", got)
	}
	if got := status(public, "GET", "/healthz"); got != http.StatusOK {
		t.Errorf("proxy port GET /healthz = %d, want 200", got)
	}

	for _, path := range []string{"/admin/", "/admin/api/keys"} {
		if got := status(admin, "GET", path); got != http.StatusOK {
			t.Errorf("dashboard port GET %s = %d, want 200", path, got)
		}
	}
	if got := status(admin, "GET", "/"); got != http.StatusFound {
		t.Errorf("dashboard port GET / = %d, want a redirect to the dashboard", got)
	}
}

// Naming the proxy's own address is how a deployment opts back into the single
// port it had before.
func TestSharingIsOptedIntoByName(t *testing.T) {
	public, admin := build(t, ":11312")

	if public != admin {
		t.Fatal("one address must mean one mux")
	}
	for _, path := range []string{"/admin/", "/admin/api/keys", "/healthz"} {
		if got := status(public, "GET", path); got != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, got)
		}
	}
	if got := status(public, "GET", "/"); got != http.StatusFound {
		t.Errorf("GET / = %d, want a redirect to the dashboard", got)
	}
}

// An untouched install must land on two ports, since that is the whole defence
// against a tunnel picking up the admin login.
func TestTheDefaultIsTwoPorts(t *testing.T) {
	t.Setenv("QUECTO_UPSTREAM_URL", "http://127.0.0.1:8080/v1")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.AdminListen == cfg.Listen {
		t.Fatalf("the dashboard shares %q by default", cfg.Listen)
	}

	public, admin := build(t, cfg.AdminListen)
	if public == admin {
		t.Fatal("the default must build two muxes")
	}
	if got := status(public, "GET", web.Prefix+"/"); got != http.StatusNotFound {
		t.Errorf("proxy port GET %s/ = %d, want 404", web.Prefix, got)
	}
}
