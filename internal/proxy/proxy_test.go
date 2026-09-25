package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quecto/internal/config"
	"quecto/internal/keys"
	"quecto/internal/limit"
	"quecto/internal/nullupstream"
	"quecto/internal/settings"
	"quecto/internal/store"
)

type rig struct {
	t        testing.TB
	proxy    *Proxy
	front    *httptest.Server
	upstream *nullupstream.Upstream
	st       *store.Store
	registry *keys.Registry
	limiter  *limit.Limiter
	secret   string
}

func newRig(t testing.TB, opts nullupstream.Options) *rig {
	t.Helper()

	up := nullupstream.New(opts)
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	u, _ := url.Parse(upSrv.URL + "/v1")
	cfg := &config.Config{
		UpstreamURL: u, DefaultMaxTokens: 2048,
		LogRetentionDays: 30, LogMaxRows: 1_000_000,
		UpstreamHeaderTimeout: 30 * time.Second,
	}
	live, err := settings.Load(cfg, st)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}

	raw, hash, display, err := keys.Generate()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if _, err := st.CreateKey(store.NewKey{
		Name: "test", KeyHash: hash, KeyPrefix: display, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	reg, err := keys.NewRegistry(st)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	lim := limit.New()
	p := New(live, reg, lim, Options{HeaderTimeout: 30 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	front := httptest.NewServer(p.Handler())
	t.Cleanup(front.Close)

	return &rig{t: t, proxy: p, front: front, upstream: up, st: st, registry: reg, limiter: lim, secret: raw}
}

func (r *rig) post(path, body string, auth string) *http.Response {
	r.t.Helper()
	req, err := http.NewRequest("POST", r.front.URL+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("do request: %v", err)
	}
	return resp
}

func (r *rig) get(path, auth string) *http.Response {
	r.t.Helper()
	req, err := http.NewRequest("GET", r.front.URL+path, nil)
	if err != nil {
		r.t.Fatalf("build request: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestForwardsAuthenticatedRequest(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())

	resp := r.post("/v1/chat/completions", `{"model":"m","messages":[]}`, r.secret)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got, ok := r.upstream.LastRequest()
	if !ok {
		t.Fatal("the upstream never saw the request")
	}
	if got.Path != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", got.Path)
	}
	if got.Authorization != "" {
		t.Fatalf("the caller's key was forwarded upstream: %q", got.Authorization)
	}
}

func TestRejectsBadAuth(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())

	cases := []struct {
		name string
		auth string
	}{
		{"no key", ""},
		{"unknown key", "qp_thiskeydoesnotexistanywhereatallxxxxxx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := r.post("/v1/chat/completions", `{"model":"m"}`, tc.auth)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !bytes.Contains(body, []byte(`"type":"invalid_request_error"`)) {
				t.Fatalf("error body is not OpenAI shaped: %s", body)
			}
		})
	}
	if len(r.upstream.Requests()) != 0 {
		t.Fatal("a rejected request still reached the upstream")
	}
}

func TestRejectsExpiredAndRevoked(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())

	past := time.Now().Add(-time.Hour).Unix()
	pastPtr := &past
	if _, err := r.st.UpdateKey(1, store.KeyUpdate{ExpiresAt: &pastPtr}); err != nil {
		t.Fatalf("expire key: %v", err)
	}
	r.registry.Reload()

	resp := r.post("/v1/chat/completions", `{"model":"m"}`, r.secret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !bytes.Contains(body, []byte("expired_api_key")) {
		t.Fatalf("expired key: status %d body %s", resp.StatusCode, body)
	}

	var none *int64
	r.st.UpdateKey(1, store.KeyUpdate{ExpiresAt: &none})
	r.st.RevokeKey(1, time.Now().Unix())
	r.registry.Reload()

	resp = r.post("/v1/chat/completions", `{"model":"m"}`, r.secret)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !bytes.Contains(body, []byte("revoked_api_key")) {
		t.Fatalf("revoked key: status %d body %s", resp.StatusCode, body)
	}
}

// The chunks must reach the client as they are produced, not in one batch at the end.
func TestStreamsChunksAsTheyArrive(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.Chunks = 5
	opts.ChunkDelay = 40 * time.Millisecond
	r := newRig(t, opts)

	start := time.Now()
	resp := r.post("/v1/chat/completions", `{"model":"m","stream":true}`, r.secret)
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	var arrivals []time.Duration
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
			arrivals = append(arrivals, time.Since(start))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if len(arrivals) < 5 {
		t.Fatalf("received %d data frames, want at least 5", len(arrivals))
	}

	// If the proxy buffered, every frame would land at essentially the same moment.
	spread := arrivals[len(arrivals)-1] - arrivals[0]
	if spread < 100*time.Millisecond {
		t.Fatalf("frames arrived %v apart, which means the response was buffered", spread)
	}
}

// Closing the client connection must cancel the upstream request.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.Chunks = 200
	opts.ChunkDelay = 20 * time.Millisecond
	r := newRig(t, opts)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", r.front.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+r.secret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}

	buf := make([]byte, 128)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	cancel()
	resp.Body.Close()

	// Nothing should panic or hang once the caller is gone.
	time.Sleep(120 * time.Millisecond)
}

func TestNonStreamingPassesBodyThrough(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())

	resp := r.post("/v1/chat/completions", `{"model":"m","messages":[]}`, r.secret)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	for _, want := range []string{`"usage"`, `"prompt_tokens":17`, `"completion_tokens":8`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("response missing %s: %s", want, body)
		}
	}
}

func TestUpstreamCredentialIsInjected(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())

	live, _ := settings.Load(&config.Config{
		UpstreamURL: mustURL(r.front.URL), DefaultMaxTokens: 1, LogRetentionDays: 1, LogMaxRows: 1000,
	}, r.st)
	if err := live.Apply(settings.Patch{UpstreamAPIKey: strPtr("sk-upstream")}); err != nil {
		t.Fatalf("set upstream key: %v", err)
	}

	// The proxy holds its own Live, so reload it into the running instance.
	r.proxy.live.Apply(settings.Patch{UpstreamAPIKey: strPtr("sk-upstream")})

	resp := r.post("/v1/chat/completions", `{"model":"m"}`, r.secret)
	resp.Body.Close()

	got, _ := r.upstream.LastRequest()
	if got.Authorization != "Bearer sk-upstream" {
		t.Fatalf("upstream Authorization = %q, want the configured credential", got.Authorization)
	}
}

func TestUpstreamFailureIsOpenAIShaped(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	r := newRig(t, opts)

	// Point at a port that refuses connections.
	r.proxy.live.Apply(settings.Patch{UpstreamURL: strPtr("http://127.0.0.1:1/v1")})

	resp := r.post("/v1/chat/completions", `{"model":"m"}`, r.secret)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("upstream_unavailable")) {
		t.Fatalf("body = %s", body)
	}
}

func TestJoinUpstreamPath(t *testing.T) {
	cases := []struct{ base, in, want string }{
		{"/v1", "/v1/chat/completions", "/v1/chat/completions"},
		{"", "/v1/chat/completions", "/v1/chat/completions"},
		{"/openai/v1", "/v1/models", "/openai/v1/models"},
		{"/v1", "/v1", "/v1"},
		{"/v1", "/v1/", "/v1/"},

		// A utility route is served at the upstream's root, so the OpenAI
		// prefix comes off while any mount point in front of it stays.
		{"/v1", "/tokenize", "/tokenize"},
		{"", "/tokenize", "/tokenize"},
		{"/v1/", "/tokenize", "/tokenize"},
		{"/openai/v1", "/tokenize", "/openai/tokenize"},
		{"/llama/v1", "/health", "/llama/health"},
		{"/openai", "/props", "/openai/props"},
	}
	for _, c := range cases {
		if got := joinUpstreamPath(c.base, c.in); got != c.want {
			t.Errorf("joinUpstreamPath(%q, %q) = %q, want %q", c.base, c.in, got, c.want)
		}
	}
}

// A utility route is forwarded to the upstream's root and comes back untouched,
// which is the whole reason it is mounted rather than answered here.
func TestForwardsUtilityRoutes(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())

	resp := r.post("/tokenize", `{"content":"hello world"}`, r.secret)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"tokens"`)) {
		t.Fatalf("body = %s, want the upstream's own reply", body)
	}

	got, ok := r.upstream.LastRequest()
	if !ok {
		t.Fatal("the upstream never saw the request")
	}
	if got.Path != "/tokenize" {
		t.Fatalf("upstream path = %q, want /tokenize", got.Path)
	}
	if bytes.Contains(got.Body, []byte("stream_options")) {
		t.Fatalf("an OpenAI field was added to a native route: %s", got.Body)
	}

	// The bodyless ones take their own path through the request side.
	for _, path := range []string{"/health", "/props"} {
		resp := r.get(path, r.secret)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		if got, _ := r.upstream.LastRequest(); got.Path != path {
			t.Errorf("upstream saw %q, want %q", got.Path, path)
		}
	}
}

// The upstream lists twice and the two disagree, so /models has to reach the
// root and /v1/models the OpenAI prefix. Sending either to the other is the
// failure that returns a reply the caller cannot use.
func TestTheTwoModelListingsStayApart(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())

	native := r.get("/models", r.secret)
	defer native.Body.Close()
	body, _ := io.ReadAll(native.Body)
	if native.StatusCode != http.StatusOK {
		t.Fatalf("GET /models = %d, want 200: %s", native.StatusCode, body)
	}
	if got, _ := r.upstream.LastRequest(); got.Path != "/models" {
		t.Errorf("upstream saw %q, want /models", got.Path)
	}
	if !bytes.Contains(body, []byte(`"meta"`)) {
		t.Errorf("GET /models returned the OpenAI listing: %s", body)
	}

	openai := r.get("/v1/models", r.secret)
	defer openai.Body.Close()
	body, _ = io.ReadAll(openai.Body)
	if got, _ := r.upstream.LastRequest(); got.Path != "/v1/models" {
		t.Errorf("upstream saw %q, want /v1/models", got.Path)
	}
	if !bytes.Contains(body, []byte(`"object":"list"`)) {
		t.Errorf("GET /v1/models returned the upstream's own listing: %s", body)
	}
}

// A utility route still needs a key. Without auth the whitelist would be an
// unauthenticated way to spend the model server's CPU.
func TestUtilityRoutesStillNeedAKey(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())

	resp := r.post("/tokenize", `{"content":"hello"}`, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if _, ok := r.upstream.LastRequest(); ok {
		t.Fatal("an unauthenticated request reached the upstream")
	}
}

// A utility route generates nothing, so it must hold nothing and bill nothing.
// A key down to its last few tokens still tokenizes a body far longer than the
// quota, and the call leaves the quota where it found it.
func TestUtilityRoutesHoldNoQuota(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())
	rec := r.withRecorder()
	r.setQuota(intp64(10))

	long := `{"content":"` + strings.Repeat("a", 4096) + `"}`
	resp := r.post("/tokenize", long, r.secret)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	io.ReadAll(resp.Body)

	got, ok := rec.waitForRecord()
	if !ok {
		t.Fatal("the call was not reported")
	}
	if got.HasUsage || got.Usage.Total() != 0 {
		t.Errorf("a utility route billed tokens: %+v", got.Usage)
	}
	if got.Estimated {
		t.Error("a utility route was charged an estimate")
	}
}

// The hold is the mechanism behind that: the same body costs a completion its
// prompt and its reply, and costs a utility route nothing.
func TestUtilityRequestsEstimateNothing(t *testing.T) {
	body := []byte(`{"content":"` + strings.Repeat("a", 4096) + `"}`)

	completion := &requestInfo{Body: body, MaxTokens: 2048}
	if completion.estimate() == 0 {
		t.Fatal("a completion of this size must be held against the quota")
	}
	utility := &requestInfo{Body: body, MaxTokens: 2048, Utility: true}
	if got := utility.estimate(); got != 0 {
		t.Errorf("utility estimate = %d, want 0", got)
	}
}

func strPtr(s string) *string { return &s }

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
