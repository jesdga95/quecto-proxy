package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"quecto/internal/config"
	"quecto/internal/keys"
	"quecto/internal/limit"
	"quecto/internal/settings"
	"quecto/internal/store"
)

const testPassword = "correct-horse-battery"

type stubHealth struct{}

func (stubHealth) UsageReporting() (bool, bool)                    { return true, true }
func (stubHealth) UpstreamReachable(context.Context) (bool, error) { return true, nil }
func (stubHealth) LogsDropped() int64                              { return 0 }
func (stubHealth) OverheadP99() time.Duration                      { return 250 * time.Microsecond }

type harness struct {
	t   *testing.T
	mux *http.ServeMux
	st  *store.Store
	reg *keys.Registry
	srv *Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	u, _ := url.Parse("http://127.0.0.1:9/v1")
	cfg := &config.Config{
		Listen: ":0", UpstreamURL: u, DBPath: "test.db",
		AdminPassword: testPassword, DefaultMaxTokens: 2048,
		LogRetentionDays: 30, LogMaxRows: 1_000_000,
		UpstreamHeaderTimeout: 120 * time.Second,
	}
	live, err := settings.Load(cfg, st)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	reg, err := keys.NewRegistry(st)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	srv, err := New(cfg, st, reg, limit.New(), live, stubHealth{})
	if err != nil {
		t.Fatalf("new admin: %v", err)
	}
	mux := http.NewServeMux()
	srv.Routes(mux)
	return &harness{t: t, mux: mux, st: st, reg: reg, srv: srv}
}

func (h *harness) do(method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func (h *harness) login() *http.Cookie {
	h.t.Helper()
	rec := h.do("POST", "/admin/api/session", map[string]string{"password": testPassword}, nil)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("login = %d, body %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	h.t.Fatal("no session cookie issued")
	return nil
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response %s: %v", rec.Body, err)
	}
	return v
}

func TestAuthRequired(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/admin/api/me", "/admin/api/keys", "/admin/api/settings"} {
		if rec := h.do("GET", path, nil, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a cookie = %d, want 401", path, rec.Code)
		}
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	h := newHarness(t)
	rec := h.do("POST", "/admin/api/session", map[string]string{"password": "nope"}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with wrong password = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a cookie was issued for a failed login")
	}
}

func TestSessionCookieIsHardened(t *testing.T) {
	h := newHarness(t)
	c := h.login()
	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Error("session cookie is not SameSite=Strict")
	}
}

func TestCreateKeyReturnsSecretOnceOnly(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	rec := h.do("POST", "/admin/api/keys", map[string]any{
		"name": "laptop", "rpm_limit": 60, "tpm_limit": 120000,
	}, c)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body)
	}
	created := decodeBody[struct {
		Key    keyDTO `json:"key"`
		Secret string `json:"secret"`
	}](t, rec)

	if created.Secret == "" {
		t.Fatal("no secret returned on creation")
	}
	if created.Key.Status != "active" {
		t.Fatalf("status = %q, want active", created.Key.Status)
	}

	// The registry must resolve the secret without a restart.
	if _, ok := h.reg.Lookup(keys.Hash(created.Secret)); !ok {
		t.Fatal("registry does not know the key that was just created")
	}

	// Neither the list nor the detail view may repeat the secret.
	list := h.do("GET", "/admin/api/keys", nil, c)
	if bytes.Contains(list.Body.Bytes(), []byte(created.Secret)) {
		t.Fatal("the list endpoint leaked the raw key")
	}
	detail := h.do("GET", "/admin/api/keys/1", nil, c)
	if bytes.Contains(detail.Body.Bytes(), []byte(created.Secret)) {
		t.Fatal("the detail endpoint leaked the raw key")
	}
}

func TestCreateKeyValidation(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	cases := []struct {
		name string
		body map[string]any
	}{
		{"empty name", map[string]any{"name": "  "}},
		{"past expiry", map[string]any{"name": "x", "expires_at": time.Now().Add(-time.Hour).Unix()}},
		{"zero rpm", map[string]any{"name": "x", "rpm_limit": 0}},
		{"negative tpm", map[string]any{"name": "x", "tpm_limit": -5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := h.do("POST", "/admin/api/keys", tc.body, c); rec.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400, body %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestRevokeAndDelete(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	rec := h.do("POST", "/admin/api/keys", map[string]any{"name": "temp"}, c)
	created := decodeBody[struct {
		Key    keyDTO `json:"key"`
		Secret string `json:"secret"`
	}](t, rec)

	rec = h.do("POST", "/admin/api/keys/1/revoke", nil, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d, body %s", rec.Code, rec.Body)
	}
	if got := decodeBody[keyDTO](t, rec); got.Status != "revoked" {
		t.Fatalf("status after revoke = %q", got.Status)
	}

	// A revoked key stays in the registry, but reports itself unusable.
	k, ok := h.reg.Lookup(keys.Hash(created.Secret))
	if !ok {
		t.Fatal("revoked key vanished from the registry")
	}
	if usable, reason := k.Status(time.Now().Unix()); usable || reason != "revoked" {
		t.Fatalf("revoked key reports usable=%v reason=%q", usable, reason)
	}

	if rec := h.do("DELETE", "/admin/api/keys/1", nil, c); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body %s", rec.Code, rec.Body)
	}
	if _, ok := h.reg.Lookup(keys.Hash(created.Secret)); ok {
		t.Fatal("deleted key is still in the registry")
	}
	if rec := h.do("GET", "/admin/api/keys/1", nil, c); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
}

func TestExpiredKeyReportsExpired(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	future := time.Now().Add(time.Hour).Unix()
	h.do("POST", "/admin/api/keys", map[string]any{"name": "soon", "expires_at": future}, c)

	past := time.Now().Add(-time.Hour).Unix()
	if _, err := h.st.UpdateKey(1, store.KeyUpdate{ExpiresAt: ptr(&past)}); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}

	rec := h.do("GET", "/admin/api/keys/1", nil, c)
	if got := decodeBody[keyDTO](t, rec); got.Status != "expired" {
		t.Fatalf("status = %q, want expired", got.Status)
	}
}

func TestUpdateClearsLimitsAndExpiry(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	exp := time.Now().Add(24 * time.Hour).Unix()
	h.do("POST", "/admin/api/keys", map[string]any{
		"name": "k", "expires_at": exp, "rpm_limit": 10, "tpm_limit": 20,
	}, c)

	rec := h.do("PATCH", "/admin/api/keys/1", map[string]any{
		"clear_expiry": true, "clear_rpm_limit": true, "clear_tpm_limit": true,
	}, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, body %s", rec.Code, rec.Body)
	}
	got := decodeBody[keyDTO](t, rec)
	if got.ExpiresAt != nil || got.RPMLimit != nil || got.TPMLimit != nil {
		t.Fatalf("clears did not apply: %+v", got)
	}
}

func TestPasswordChangeInvalidatesOtherSessions(t *testing.T) {
	h := newHarness(t)
	old := h.login()

	rec := h.do("PATCH", "/admin/api/settings", map[string]any{
		"new_password": "another-long-password",
	}, old)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password = %d, body %s", rec.Code, rec.Body)
	}

	if got := h.do("GET", "/admin/api/me", nil, old); got.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie still works after a password change: %d", got.Code)
	}

	var fresh *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			fresh = c
		}
	}
	if fresh == nil {
		t.Fatal("no replacement cookie issued to the browser that changed the password")
	}
	if got := h.do("GET", "/admin/api/me", nil, fresh); got.Code != http.StatusOK {
		t.Fatalf("replacement cookie rejected: %d", got.Code)
	}
}

func TestSettingsHideUpstreamCredential(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	rec := h.do("PATCH", "/admin/api/settings", map[string]any{"upstream_api_key": "sk-secret-value"}, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, body %s", rec.Code, rec.Body)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("sk-secret-value")) {
		t.Fatal("the settings response echoed the upstream credential")
	}

	// Sending the mask back must not overwrite the stored credential.
	h.do("PATCH", "/admin/api/settings", map[string]any{"upstream_api_key": "********"}, c)
	live, _ := settings.Load(&config.Config{
		UpstreamURL: mustURL("http://127.0.0.1:9/v1"), DefaultMaxTokens: 1,
		LogRetentionDays: 1, LogMaxRows: 1000,
	}, h.st)
	if got := live.UpstreamAPIKey(); got != "sk-secret-value" {
		t.Fatalf("stored credential = %q, want it unchanged", got)
	}
}

func TestSettingsRejectBadUpstream(t *testing.T) {
	h := newHarness(t)
	c := h.login()
	for _, bad := range []string{"ftp://x/v1", "not a url", "http://"} {
		rec := h.do("PATCH", "/admin/api/settings", map[string]any{"upstream_url": bad}, c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("upstream_url=%q = %d, want 400", bad, rec.Code)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// Verifying a password allocates 64MB, so an unauthenticated flood must not be
// able to run an unbounded number of them at once.
func TestLoginVerificationIsBounded(t *testing.T) {
	h := newHarness(t)
	h.srv.verifySlots = make(chan struct{}, 1)
	h.srv.verifyWait = time.Millisecond

	const attempts = 8
	codes := make(chan int, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- h.do("POST", "/admin/api/session", map[string]string{"password": "wrong"}, nil).Code
		}()
	}
	wg.Wait()
	close(codes)

	var shed, rejected int
	for c := range codes {
		switch c {
		case http.StatusTooManyRequests:
			shed++
		case http.StatusUnauthorized:
			rejected++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if shed == 0 {
		t.Fatal("every concurrent attempt ran a full verification, so memory is unbounded")
	}
	if shed+rejected != attempts {
		t.Fatalf("accounted for %d of %d attempts", shed+rejected, attempts)
	}
}

// Shedding must never let a wrong password through.
func TestSheddingDoesNotWeakenAuth(t *testing.T) {
	h := newHarness(t)
	h.srv.verifySlots = make(chan struct{}, 1)
	h.srv.verifyWait = time.Millisecond

	for i := 0; i < 20; i++ {
		if code := h.do("POST", "/admin/api/session", map[string]string{"password": "wrong"}, nil).Code; code == http.StatusOK {
			t.Fatal("a wrong password was accepted while shedding load")
		}
	}
	// The correct password still works once there is no contention.
	if code := h.do("POST", "/admin/api/session", map[string]string{"password": testPassword}, nil).Code; code != http.StatusOK {
		t.Fatalf("correct password = %d, want 200", code)
	}
}
