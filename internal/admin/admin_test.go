package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
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
	mux http.Handler
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
	return &harness{t: t, mux: srv.Handler(), st: st, reg: reg, srv: srv}
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
	rec := h.do("POST", "/quecto/api/session", map[string]string{"password": testPassword}, nil)
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
	for _, path := range []string{"/quecto/api/me", "/quecto/api/keys", "/quecto/api/settings"} {
		if rec := h.do("GET", path, nil, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a cookie = %d, want 401", path, rec.Code)
		}
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	h := newHarness(t)
	rec := h.do("POST", "/quecto/api/session", map[string]string{"password": "nope"}, nil)
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

	rec := h.do("POST", "/quecto/api/keys", map[string]any{
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
	list := h.do("GET", "/quecto/api/keys", nil, c)
	if bytes.Contains(list.Body.Bytes(), []byte(created.Secret)) {
		t.Fatal("the list endpoint leaked the raw key")
	}
	detail := h.do("GET", "/quecto/api/keys/1", nil, c)
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
			if rec := h.do("POST", "/quecto/api/keys", tc.body, c); rec.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400, body %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestRevokeAndDelete(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	rec := h.do("POST", "/quecto/api/keys", map[string]any{"name": "temp"}, c)
	created := decodeBody[struct {
		Key    keyDTO `json:"key"`
		Secret string `json:"secret"`
	}](t, rec)

	rec = h.do("POST", "/quecto/api/keys/1/revoke", nil, c)
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

	if rec := h.do("DELETE", "/quecto/api/keys/1", nil, c); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body %s", rec.Code, rec.Body)
	}
	if _, ok := h.reg.Lookup(keys.Hash(created.Secret)); ok {
		t.Fatal("deleted key is still in the registry")
	}
	if rec := h.do("GET", "/quecto/api/keys/1", nil, c); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
}

func TestExpiredKeyReportsExpired(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	future := time.Now().Add(time.Hour).Unix()
	h.do("POST", "/quecto/api/keys", map[string]any{"name": "soon", "expires_at": future}, c)

	past := time.Now().Add(-time.Hour).Unix()
	if _, err := h.st.UpdateKey(1, store.KeyUpdate{ExpiresAt: ptr(&past)}); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}

	rec := h.do("GET", "/quecto/api/keys/1", nil, c)
	if got := decodeBody[keyDTO](t, rec); got.Status != "expired" {
		t.Fatalf("status = %q, want expired", got.Status)
	}
}

func TestSpentQuotaReportsExhaustedAndResetRevivesIt(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	rec := h.do("POST", "/quecto/api/keys", map[string]any{
		"name": "capped", "total_token_limit": 1000,
	}, c)
	if got := decodeBody[struct {
		Key keyDTO `json:"key"`
	}](t, rec).Key; got.TotalTokenLimit == nil || *got.TotalTokenLimit != 1000 {
		t.Fatalf("created key limit = %v, want 1000", got.TotalTokenLimit)
	}

	if _, err := h.st.DB.Exec("UPDATE api_keys SET tokens_spent = 1000 WHERE id = 1"); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	got := decodeBody[keyDTO](t, h.do("GET", "/quecto/api/keys/1", nil, c))
	if got.Status != "exhausted" {
		t.Fatalf("status = %q, want exhausted", got.Status)
	}

	rec = h.do("PATCH", "/quecto/api/keys/1", map[string]any{"reset_tokens_spent": true}, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d, body %s", rec.Code, rec.Body)
	}
	got = decodeBody[keyDTO](t, rec)
	if got.Status != "active" {
		t.Fatalf("status after the reset = %q, want active", got.Status)
	}
	if got.Live.TotalRemaining != 1000 {
		t.Fatalf("remaining after the reset = %d, want the whole quota back", got.Live.TotalRemaining)
	}
}

// SQLite hands the next key the id a deleted one gave up, so a quota left in the
// limiter would be inherited by a key that never spent a token of it.
func TestDeletedKeyLeavesNoQuotaBehindForItsSuccessor(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	h.do("POST", "/quecto/api/keys", map[string]any{"name": "first", "total_token_limit": 1000}, c)
	if _, err := h.st.DB.Exec("UPDATE api_keys SET tokens_spent = 1000 WHERE id = 1"); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	if got := decodeBody[keyDTO](t, h.do("GET", "/quecto/api/keys/1", nil, c)); got.Status != "exhausted" {
		t.Fatalf("status = %q, want exhausted before the delete", got.Status)
	}

	if rec := h.do("DELETE", "/quecto/api/keys/1", nil, c); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body %s", rec.Code, rec.Body)
	}

	rec := h.do("POST", "/quecto/api/keys", map[string]any{"name": "second", "total_token_limit": 1000}, c)
	got := decodeBody[struct {
		Key keyDTO `json:"key"`
	}](t, rec).Key
	if got.ID != 1 {
		t.Skipf("the new key took id %d, so this build does not recycle ids", got.ID)
	}
	if got.Status != "active" {
		t.Fatalf("a fresh key on a recycled id reads %q, want active", got.Status)
	}
	if got.Live.TotalSpent != 0 || got.Live.TotalRemaining != 1000 {
		t.Fatalf("fresh key inherited spend=%d remaining=%d", got.Live.TotalSpent, got.Live.TotalRemaining)
	}
}

func TestDeletedKeyUsageStaysInTheTotalsButNotOnTheNextKey(t *testing.T) {
	h := newHarness(t)
	c := h.login()
	ts := time.Now().Unix()

	create := func(name string) int64 {
		t.Helper()
		rec := h.do("POST", "/quecto/api/keys", map[string]any{"name": name}, c)
		return decodeBody[struct {
			Key keyDTO `json:"key"`
		}](t, rec).Key.ID
	}
	spend := func(keyID, prompt, completion int64, cached ...int64) {
		t.Helper()
		row := store.LogRow{
			KeyID: keyID, TS: ts, Model: "m", Status: 200,
			PromptTokens: &prompt, CompletionTokens: &completion, DurationMS: 5,
		}
		if len(cached) > 0 {
			c := cached[0]
			row.CachedTokens = &c
		}
		if err := h.st.WriteBatch([]store.LogRow{row}); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	stats := func() statsResponse {
		t.Helper()
		return decodeBody[statsResponse](t, h.do("GET", "/quecto/api/stats?range=24h", nil, c))
	}
	topKey := func(s statsResponse, id int64) *store.KeyTotal {
		t.Helper()
		for i := range s.TopKeys {
			if s.TopKeys[i].KeyID == id {
				return &s.TopKeys[i]
			}
		}
		return nil
	}

	first := create("shared-name")
	spend(first, 700, 300, 250)
	if rec := h.do("DELETE", "/quecto/api/keys/"+strconv.FormatInt(first, 10), nil, c); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body %s", rec.Code, rec.Body)
	}

	second := create("shared-name")
	spend(second, 40, 60)

	got := stats()
	if want := (store.Totals{Requests: 2, PromptTokens: 740, CompletionTokens: 360,
		CachedTokens: 250, CacheKnownPromptTokens: 700}); got.Totals != want {
		t.Fatalf("totals = %+v, want %+v with the deleted key still counted, cached counts included", got.Totals, want)
	}
	if dead := topKey(got, store.DeletedKeyID); dead == nil || dead.Requests != 1 || dead.Tokens != 1000 {
		t.Fatalf("deleted key row = %+v, want its 1 request and 1000 tokens kept", dead)
	}
	if live := topKey(got, second); live == nil || live.Requests != 1 || live.Tokens != 100 {
		t.Fatalf("successor row = %+v, want only its own 1 request and 100 tokens", live)
	}

	// A second delete folds into the same sentinel rows rather than colliding.
	if rec := h.do("DELETE", "/quecto/api/keys/"+strconv.FormatInt(second, 10), nil, c); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body %s", rec.Code, rec.Body)
	}
	got = stats()
	if len(got.TopKeys) != 1 {
		t.Fatalf("top keys = %+v, want the two dead keys merged into one row", got.TopKeys)
	}
	if dead := topKey(got, store.DeletedKeyID); dead == nil || dead.Requests != 2 || dead.Tokens != 1100 {
		t.Fatalf("merged row = %+v, want 2 requests and 1100 tokens", dead)
	}
}

func TestStatsShortWindowReadsTheRawLog(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	rec := h.do("POST", "/quecto/api/keys", map[string]any{"name": "k"}, c)
	keyID := decodeBody[struct {
		Key keyDTO `json:"key"`
	}](t, rec).Key.ID

	now := time.Now().Unix()
	prompt, completion := int64(10), int64(20)
	if err := h.st.WriteBatch([]store.LogRow{
		// Old enough to sit outside the one-hour window, inside the rollup's.
		{KeyID: keyID, TS: now - 2*3600, Model: "m", Status: 200,
			PromptTokens: &prompt, CompletionTokens: &completion, DurationMS: 5},
		{KeyID: keyID, TS: now - 10*60, Model: "m", Status: 200,
			PromptTokens: &prompt, CompletionTokens: &completion, DurationMS: 5},
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	s1h := decodeBody[statsResponse](t, h.do("GET", "/quecto/api/stats?range=1h", nil, c))
	if s1h.SeriesStep != 300 {
		t.Fatalf("one-hour series_step = %d, want 300", s1h.SeriesStep)
	}
	if want := (store.Totals{Requests: 1, PromptTokens: 10, CompletionTokens: 20}); s1h.Totals != want {
		t.Fatalf("one-hour totals = %+v, want %+v without the older request", s1h.Totals, want)
	}
	if len(s1h.Series) != 1 || s1h.Series[0].Requests != 1 {
		t.Fatalf("one-hour series = %+v, want a single five-minute bucket with the recent request", s1h.Series)
	}
	if len(s1h.TopKeys) != 1 || s1h.TopKeys[0].Requests != 1 {
		t.Fatalf("one-hour top keys = %+v, want the key with only its recent request", s1h.TopKeys)
	}

	s24h := decodeBody[statsResponse](t, h.do("GET", "/quecto/api/stats?range=24h", nil, c))
	if s24h.SeriesStep != 3600 {
		t.Fatalf("24h series_step = %d, want 3600", s24h.SeriesStep)
	}
	// The rollup covers the whole current hour, so both requests count.
	if want := (store.Totals{Requests: 2, PromptTokens: 20, CompletionTokens: 40}); s24h.Totals != want {
		t.Fatalf("24h totals = %+v, want %+v from the hourly rollup", s24h.Totals, want)
	}
}

func TestQuotaLimitMustBePositive(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	rec := h.do("POST", "/quecto/api/keys", map[string]any{"name": "k", "total_token_limit": 0}, c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with a zero quota = %d, want 400", rec.Code)
	}
}

func TestUpdateClearsLimitsAndExpiry(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	exp := time.Now().Add(24 * time.Hour).Unix()
	h.do("POST", "/quecto/api/keys", map[string]any{
		"name": "k", "expires_at": exp, "rpm_limit": 10, "tpm_limit": 20,
		"total_token_limit": 5000,
	}, c)

	rec := h.do("PATCH", "/quecto/api/keys/1", map[string]any{
		"clear_expiry": true, "clear_rpm_limit": true, "clear_tpm_limit": true,
		"clear_total_token_limit": true,
	}, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, body %s", rec.Code, rec.Body)
	}
	got := decodeBody[keyDTO](t, rec)
	if got.ExpiresAt != nil || got.RPMLimit != nil || got.TPMLimit != nil || got.TotalTokenLimit != nil {
		t.Fatalf("clears did not apply: %+v", got)
	}
}

func TestPasswordChangeInvalidatesOtherSessions(t *testing.T) {
	h := newHarness(t)
	old := h.login()

	rec := h.do("PATCH", "/quecto/api/settings", map[string]any{
		"new_password": "another-long-password",
	}, old)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password = %d, body %s", rec.Code, rec.Body)
	}

	if got := h.do("GET", "/quecto/api/me", nil, old); got.Code != http.StatusUnauthorized {
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
	if got := h.do("GET", "/quecto/api/me", nil, fresh); got.Code != http.StatusOK {
		t.Fatalf("replacement cookie rejected: %d", got.Code)
	}
}

func TestSettingsHideUpstreamCredential(t *testing.T) {
	h := newHarness(t)
	c := h.login()

	rec := h.do("PATCH", "/quecto/api/settings", map[string]any{"upstream_api_key": "sk-secret-value"}, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, body %s", rec.Code, rec.Body)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("sk-secret-value")) {
		t.Fatal("the settings response echoed the upstream credential")
	}

	// Sending the mask back must not overwrite the stored credential.
	h.do("PATCH", "/quecto/api/settings", map[string]any{"upstream_api_key": "********"}, c)
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
		rec := h.do("PATCH", "/quecto/api/settings", map[string]any{"upstream_url": bad}, c)
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
			codes <- h.do("POST", "/quecto/api/session", map[string]string{"password": "wrong"}, nil).Code
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
		if code := h.do("POST", "/quecto/api/session", map[string]string{"password": "wrong"}, nil).Code; code == http.StatusOK {
			t.Fatal("a wrong password was accepted while shedding load")
		}
	}
	// The correct password still works once there is no contention.
	if code := h.do("POST", "/quecto/api/session", map[string]string{"password": testPassword}, nil).Code; code != http.StatusOK {
		t.Fatalf("correct password = %d, want 200", code)
	}
}

// The handler owns every reply under its mount, including the ones for paths
// it does not serve. A caller that parses JSON must never be handed anything
// else, whatever it asks for.
func TestAMissIsAnsweredInJson(t *testing.T) {
	h := newHarness(t)

	for _, c := range []struct{ method, path string }{
		{"GET", "/quecto/api/nope"},
		{"GET", "/quecto/api/"},
		{"PUT", "/quecto/api/keys"},   // a real path by a method it does not serve
		{"GET", "/quecto/api/keys/1"}, // signed out, so this one is a 401
	} {
		rec := h.do(c.method, c.path, nil, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s = 200, want a refusal", c.method, c.path)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s Content-Type = %q, want application/json", c.method, c.path, ct)
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s body is not JSON: %v", c.method, c.path, err)
			continue
		}
		if body.Error == "" {
			t.Errorf("%s %s carries no message", c.method, c.path)
		}
	}
}
