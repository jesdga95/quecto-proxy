package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/", "index.html", true},
		{"", "index.html", true},
		{"/keys", "keys.html", true},
		{"/settings", "settings.html", true},
		{"/assets/app.js", "assets/app.js", true},
		// Clean resolves the traversal away, leaving a name inside the build
		// that simply does not exist.
		{"/../go.mod", "go.mod", true},
		{"/../../etc/passwd", "etc/passwd.html", true},
	}
	for _, c := range cases {
		got, ok := resolve(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("resolve(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// requireBuilt skips tests that need the compiled dashboard, so a clean
// checkout can still run the suite without Node.
func requireBuilt(t *testing.T) http.Handler {
	t.Helper()
	return requireBuiltAs(t, true)
}

// requireBuiltAs builds the handler for a browser that either is or is not
// signed in.
func requireBuiltAs(t *testing.T, signedIn bool) http.Handler {
	t.Helper()
	h, err := Handler(func(*http.Request) bool { return signedIn })
	if err != nil {
		t.Fatalf("build handler: %v", err)
	}
	if !Built() {
		t.Skip("dashboard is not built, run `make dashboard`")
	}
	return h
}

func TestUnbuiltDashboardExplainsItself(t *testing.T) {
	h := notBuiltHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "make dashboard") {
		t.Error("the page does not say how to fix it")
	}
}

func TestServesPages(t *testing.T) {
	h := requireBuilt(t)

	for _, path := range []string{"/admin/", "/admin/keys", "/admin/logs", "/admin/settings"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q", path, ct)
		}
		if !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
			t.Errorf("GET %s did not return a page", path)
		}
	}
}

// This mount serves a browser, so its own miss is a page. The API beside it
// answers in JSON, and neither has to know about the other.
func TestUnknownPageIsNotFound(t *testing.T) {
	h := requireBuilt(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown page = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "404") || !strings.Contains(body, "Not found") {
		t.Errorf("the page does not say what happened: %q", body)
	}
	if !strings.Contains(body, `href="`+Prefix+`/"`) {
		t.Error("the page offers no way back to the dashboard")
	}
}

// A traversal attempt must not escape the embedded build.
func TestTraversalIsContained(t *testing.T) {
	h := requireBuilt(t)
	for _, path := range []string{
		"/admin/../go.mod",
		"/admin/%2e%2e/go.mod",
		"/admin/assets/../../go.mod",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "module quecto") {
			t.Fatalf("GET %s escaped the embedded build", path)
		}
	}
}

func TestAssetsAreCachedAndPagesAreNot(t *testing.T) {
	h := requireBuilt(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("page Cache-Control = %q, want no-cache", got)
	}

	asset := findAsset(t, rec.Body.String())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", asset, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", asset, rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("asset Cache-Control = %q, want it immutable", got)
	}
}

// Every page must carry inlined icon markup rather than reaching for a network font.
func TestIconsAreInlined(t *testing.T) {
	h := requireBuilt(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/keys", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `viewBox="0 0 256 256"`) {
		t.Error("no inlined icon found on the keys page")
	}
	// The xmlns on an SVG is a namespace name, not a fetch, so only look at
	// attributes the browser would actually load.
	for _, ref := range []string{`src="http`, `href="http`, `src="//`, `href="//`, "unpkg", "jsdelivr", "googleapis"} {
		if strings.Contains(body, ref) {
			t.Errorf("page loads from an external host: %q", ref)
		}
	}
}

// A signed out browser must be sent to the login page without ever receiving
// the dashboard markup, which is what made the old page flash before it
// bounced itself.
func TestSignedOutIsRedirectedBeforeAnyMarkup(t *testing.T) {
	h := requireBuiltAs(t, false)

	for _, path := range []string{"/admin/", "/admin/keys", "/admin/logs", "/admin/settings", "/admin/index.html"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))

		if rec.Code != http.StatusFound {
			t.Errorf("GET %s = %d, want 302", path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Location"); got != LoginPath {
			t.Errorf("GET %s sent the browser to %q, want %q", path, got, LoginPath)
		}
		if strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
			t.Errorf("GET %s leaked a page to a signed out browser", path)
		}
	}
}

func TestSignedOutCanReachLoginAndItsAssets(t *testing.T) {
	h := requireBuiltAs(t, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/login = %d, want 200", rec.Code)
	}

	// Gating assets too would leave the login page unstyled and inert.
	asset := findAsset(t, rec.Body.String())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", asset, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET %s = %d, want 200", asset, rec.Code)
	}
}

// Nothing on the login page is useful once a session exists.
func TestSignedInSkipsLogin(t *testing.T) {
	h := requireBuiltAs(t, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/login", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("GET /admin/login = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != Prefix+"/" {
		t.Errorf("Location = %q, want %q", got, Prefix+"/")
	}
}

// Answering 404 for a signed out visitor would map which pages exist.
func TestSignedOutLearnsNothingFromAnUnknownPage(t *testing.T) {
	h := requireBuiltAs(t, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/nope", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("unknown page while signed out = %d, want 302", rec.Code)
	}
}

// A cache holding either answer would serve it to the wrong browser.
func TestCookieDependentRepliesAreNotCached(t *testing.T) {
	rec := httptest.NewRecorder()
	requireBuiltAs(t, false).ServeHTTP(rec, httptest.NewRequest("GET", "/admin/", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("redirect Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Vary"); got != "Cookie" {
		t.Errorf("redirect Vary = %q, want Cookie", got)
	}

	rec = httptest.NewRecorder()
	requireBuiltAs(t, true).ServeHTTP(rec, httptest.NewRequest("GET", "/admin/", nil))
	if got := rec.Header().Get("Vary"); got != "Cookie" {
		t.Errorf("page Vary = %q, want Cookie", got)
	}
}

func findAsset(t *testing.T, html string) string {
	t.Helper()
	i := strings.Index(html, "/admin/assets/")
	if i < 0 {
		t.Fatal("no asset link in the page")
	}
	rest := html[i:]
	end := strings.IndexAny(rest, `"'`)
	if end < 0 {
		t.Fatal("malformed asset link")
	}
	return rest[:end]
}
