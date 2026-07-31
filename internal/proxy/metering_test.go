package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"quecto/internal/nullupstream"
	"quecto/internal/store"
)

type recorder struct {
	mu   sync.Mutex
	recs []Record
}

func (r *recorder) Report(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
}

func (r *recorder) last() (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.recs) == 0 {
		return Record{}, false
	}
	return r.recs[len(r.recs)-1], true
}

func (r *rig) withRecorder() *recorder {
	rec := &recorder{}
	r.proxy.SetReporter(rec)
	return rec
}

func (r *rig) setLimits(rpm, tpm *int) {
	r.t.Helper()
	if _, err := r.st.UpdateKey(1, store.KeyUpdate{RPMLimit: &rpm, TPMLimit: &tpm}); err != nil {
		r.t.Fatalf("set limits: %v", err)
	}
	r.registry.Reload()
}

func (r *rig) setQuota(total *int64) {
	r.t.Helper()
	if _, err := r.st.UpdateKey(1, store.KeyUpdate{TotalTokenLimit: &total}); err != nil {
		r.t.Fatalf("set quota: %v", err)
	}
	r.registry.Reload()
}

func intp(v int) *int { return &v }

func intp64(v int64) *int64 { return &v }

func TestInjectsUsageRequestAndStripsTheExtraFrame(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())
	rec := r.withRecorder()

	resp := r.post("/v1/chat/completions", `{"model":"m","stream":true}`, r.secret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	sent, _ := r.upstream.LastRequest()
	if !bytes.Contains(sent.Body, []byte(`"include_usage":true`)) {
		t.Fatalf("stream_options was not added to the upstream request: %s", sent.Body)
	}

	// The caller never asked for usage, so no usage frame may reach them.
	if bytes.Contains(body, []byte(`"usage"`)) {
		t.Fatalf("the usage frame was passed through to a caller that did not ask for it:\n%s", body)
	}
	if !bytes.Contains(body, []byte("[DONE]")) {
		t.Fatalf("stream is missing its terminator:\n%s", body)
	}
	if n := bytes.Count(body, []byte("data: ")); n != 10 {
		t.Fatalf("client saw %d frames, want 8 content frames plus stop plus DONE", n)
	}

	got, ok := rec.last()
	if !ok {
		t.Fatal("no record was reported")
	}
	if !got.HasUsage || got.Usage.PromptTokens != 17 || got.Usage.CompletionTokens != 8 {
		t.Fatalf("usage was not captured: %+v", got.Usage)
	}
	if !got.HasTTFT || got.TTFT <= 0 {
		t.Fatal("time to first token was not measured")
	}
	if !got.Stream {
		t.Fatal("record does not say the response was streamed")
	}
}

func TestCallerThatAsksForUsageKeepsIt(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())
	rec := r.withRecorder()

	resp := r.post("/v1/chat/completions",
		`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`, r.secret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !bytes.Contains(body, []byte(`"usage"`)) {
		t.Fatalf("the usage frame was stripped from a caller that asked for it:\n%s", body)
	}
	sent, _ := r.upstream.LastRequest()
	if n := bytes.Count(sent.Body, []byte("stream_options")); n != 1 {
		t.Fatalf("stream_options appears %d times upstream, want exactly 1", n)
	}
	if got, _ := rec.last(); !got.HasUsage {
		t.Fatal("usage was not recorded")
	}
}

func TestUsageOnTheFinalContentChunkIsNotDropped(t *testing.T) {
	scanner := newSSEScanner(io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n"+
			"data: [DONE]\n\n")), true, time.Now)

	out, err := io.ReadAll(scanner)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Contains(out, []byte(`"content":"hi"`)) {
		t.Fatalf("a content chunk was dropped because it carried usage:\n%s", out)
	}
	if u, ok := scanner.Usage(); !ok || u.PromptTokens != 3 {
		t.Fatalf("usage = %+v ok=%v", u, ok)
	}
}

func TestFramesSplitAcrossReadsAreReassembled(t *testing.T) {
	full := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":4,\"total_tokens\":15}}\n\n" +
		"data: [DONE]\n\n"

	// One byte at a time is the worst case for frame reassembly.
	scanner := newSSEScanner(io.NopCloser(&iotest1{s: full}), true, time.Now)
	out, err := io.ReadAll(scanner)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.Contains(out, []byte(`"usage"`)) {
		t.Fatalf("the usage frame survived reassembly:\n%s", out)
	}
	if !bytes.Contains(out, []byte(`"content":"a"`)) || !bytes.Contains(out, []byte("[DONE]")) {
		t.Fatalf("content was lost during reassembly:\n%s", out)
	}
	u, ok := scanner.Usage()
	if !ok || u.Total() != 15 {
		t.Fatalf("usage = %+v ok=%v", u, ok)
	}
}

// iotest1 returns a single byte per Read.
type iotest1 struct {
	s string
	i int
}

func (r *iotest1) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	p[0] = r.s[r.i]
	r.i++
	return 1, nil
}

func TestNonStreamingUsageIsCaptured(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())
	rec := r.withRecorder()

	resp := r.post("/v1/chat/completions", `{"model":"gpt-x","messages":[]}`, r.secret)
	io.ReadAll(resp.Body)
	resp.Body.Close()

	got, _ := rec.last()
	if !got.HasUsage || got.Usage.Total() != 25 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if got.Model != "gpt-x" {
		t.Fatalf("model = %q, want gpt-x", got.Model)
	}
	if got.Stream {
		t.Fatal("a non-streamed reply was recorded as streamed")
	}
}

func TestRequestLimitReturns429WithHeaders(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())
	r.setLimits(intp(2), nil)

	for n := 0; n < 2; n++ {
		resp := r.post("/v1/chat/completions", `{"model":"m"}`, r.secret)
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}

	resp := r.post("/v1/chat/completions", `{"model":"m"}`, r.secret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("no Retry-After header on a 429")
	} else if n, err := strconv.Atoi(got); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", got)
	}
	if got := resp.Header.Get("X-RateLimit-Limit-Requests"); got != "2" {
		t.Errorf("X-RateLimit-Limit-Requests = %q, want 2", got)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining-Requests"); got != "0" {
		t.Errorf("X-RateLimit-Remaining-Requests = %q, want 0", got)
	}
	if !bytes.Contains(body, []byte("requests_rate_limit_exceeded")) {
		t.Errorf("body = %s", body)
	}
	if !bytes.Contains(body, []byte(`"type":"rate_limit_error"`)) {
		t.Errorf("error type is not OpenAI shaped: %s", body)
	}
}

func TestSpentQuotaKillsTheKeyWithoutARetryHint(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.PromptTokens = 400
	opts.CompletionTokens = 400
	r := newRig(t, opts)
	rec := r.withRecorder()
	r.setQuota(intp64(600))

	// One reply costs 800, so the first call through spends the whole quota.
	resp := r.post("/v1/chat/completions", `{"model":"m","max_tokens":10}`, r.secret)
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request = %d, want 200", resp.StatusCode)
	}

	resp = r.post("/v1/chat/completions", `{"model":"m","max_tokens":10}`, r.secret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second request = %d, want 401 rather than a retryable 429", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("exhausted_api_key")) {
		t.Fatalf("body = %s", body)
	}
	// A retry hint would send the client into a backoff loop it never leaves.
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q on a key that will never be accepted again", got)
	}
	if len(r.upstream.Requests()) != 1 {
		t.Fatalf("upstream saw %d requests, the refused one should not have been forwarded", len(r.upstream.Requests()))
	}
	if last, _ := rec.last(); last.Status != http.StatusUnauthorized {
		t.Errorf("recorded status = %d, want the 401 that was served", last.Status)
	}
}

// The upstream runs several replies at once, and a quota checked against settled
// tokens alone would be measured by all of them against a tally none had moved
// yet. Each call in flight holds its estimate, so the overshoot is one reply's
// worth rather than one for every slot the model server has.
func TestConcurrentCallsCannotOverspendTheQuota(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.PromptTokens, opts.CompletionTokens = 400, 400
	// Slow enough that all four calls are inside the limiter before any settles.
	opts.TTFT, opts.ChunkDelay = 50*time.Millisecond, 10*time.Millisecond
	r := newRig(t, opts)
	r.setQuota(intp64(600))

	// A ceiling above the whole quota, which is the case that used to let every
	// concurrent call through: the first holds what is left and the rest wait.
	const body = `{"model":"m","stream":true,"max_tokens":2000}`
	var wg sync.WaitGroup
	codes := make([]int, 4)
	bodies := make([][]byte, 4)
	for n := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := r.post("/v1/chat/completions", body, r.secret)
			bodies[n], _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			codes[n] = resp.StatusCode
		}()
	}
	wg.Wait()

	// A call that lost the race is refused either way: held off while the first
	// reply runs, and terminally once it has settled past the limit.
	ok, refused := 0, 0
	for n, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests, http.StatusUnauthorized:
			refused++
		default:
			t.Fatalf("call %d = %d (%s), want 200, 429 or 401", n+1, code, bodies[n])
		}
	}
	if ok != 1 || refused != 3 {
		t.Fatalf("%d calls served and %d refused, want 1 and 3", ok, refused)
	}
	if got := len(r.upstream.Requests()); got != 1 {
		t.Fatalf("upstream saw %d requests, want only the one the quota could cover", got)
	}

	// One reply of 800 against a 600 quota, not four of them.
	key, err := r.st.GetKey(1)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if got := r.limiter.Peek(key).TotalSpent; got != 800 {
		t.Fatalf("spent = %d of a 600 quota, want the 800 a single reply costs", got)
	}
}

// A quota held by calls in flight is a wait, not the end of the key, so it must
// not answer with the terminal error the other key states use.
func TestQuotaHeldByCallsInFlightIsRetryable(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.PromptTokens, opts.CompletionTokens = 10, 10
	// The reply lasts long enough to be refused against, and costs 20 of 600, so
	// the quota is only ever held rather than spent.
	opts.TTFT, opts.ChunkDelay = 200*time.Millisecond, 20*time.Millisecond
	r := newRig(t, opts)
	r.setQuota(intp64(600))

	const body = `{"model":"m","stream":true,"max_tokens":2000}`
	first := make(chan struct{})
	go func() {
		defer close(first)
		resp := r.post("/v1/chat/completions", body, r.secret)
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}()
	r.waitForQuotaHold()

	resp := r.post("/v1/chat/completions", body, r.secret)
	refused, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second call = %d, want a retryable 429 rather than the terminal 401: %s",
			resp.StatusCode, refused)
	}
	if !bytes.Contains(refused, []byte("quota_pending")) {
		t.Fatalf("refusal body = %s", refused)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatal("a refusal that clears on its own carried no Retry-After")
	}

	<-first
	resp = r.post("/v1/chat/completions", body, r.secret)
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("call after the hold was released = %d, want 200", resp.StatusCode)
	}
}

// waitForQuotaHold blocks until a call in flight has taken the key's remaining
// quota, which is the point a second call has something to be refused against.
func (r *rig) waitForQuotaHold() {
	r.t.Helper()
	key, err := r.st.GetKey(1)
	if err != nil {
		r.t.Fatalf("get key: %v", err)
	}
	for n := 0; n < 2000; n++ {
		if d := r.limiter.Peek(key); d.TotalRemaining == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	r.t.Fatal("no call ever took the quota")
}

func TestQuotaSpentInAnEarlierRunIsRefusedAtTheDoor(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())
	// A key row the usage writer already took to its limit, as a restart finds it.
	r.setQuota(intp64(1000))
	if _, err := r.st.DB.Exec("UPDATE api_keys SET tokens_spent = 1000 WHERE id = 1"); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	r.registry.Reload()

	resp := r.post("/v1/chat/completions", `{"model":"m"}`, r.secret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("exhausted_api_key")) {
		t.Fatalf("body = %s", body)
	}
	if len(r.upstream.Requests()) != 0 {
		t.Fatal("a key with no quota left reached the upstream")
	}
}

func TestOverdrawnTokensLockTheKeyOut(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.PromptTokens = 5000
	opts.CompletionTokens = 5000
	r := newRig(t, opts)
	// The allowance is far smaller than one reply, so one call overdraws it.
	r.setLimits(nil, intp(600))

	resp := r.post("/v1/chat/completions", `{"model":"m","max_tokens":10}`, r.secret)
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request = %d, want 200", resp.StatusCode)
	}

	resp = r.post("/v1/chat/completions", `{"model":"m","max_tokens":10}`, r.secret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429 while the token balance is overdrawn", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("tokens_rate_limit_exceeded")) {
		t.Fatalf("body = %s", body)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining-Tokens"); got != "0" {
		t.Errorf("X-RateLimit-Remaining-Tokens = %q, want 0 rather than a negative value", got)
	}
	if len(r.upstream.Requests()) != 1 {
		t.Fatalf("upstream saw %d requests, the refused one should not have been forwarded", len(r.upstream.Requests()))
	}
}

func TestReservationIsRefundedSoSmallCallsKeepFlowing(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.PromptTokens = 1
	opts.CompletionTokens = 1
	r := newRig(t, opts)
	// Each call reserves 500 but settles at 2, so the refund must land.
	r.setLimits(nil, intp(1000))

	for n := 0; n < 20; n++ {
		resp := r.post("/v1/chat/completions", `{"model":"m","max_tokens":500}`, r.secret)
		io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d = %d, refunds are not being applied", n+1, resp.StatusCode)
		}
	}
}

func TestUpstreamThatRejectsStreamOptionsFallsBack(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.SupportsStreamOptions = false
	r := newRig(t, opts)

	resp := r.post("/v1/chat/completions", `{"model":"m","stream":true}`, r.secret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the retry to succeed: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("[DONE]")) {
		t.Fatalf("the caller did not get a usable stream:\n%s", body)
	}

	reqs := r.upstream.Requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream saw %d requests, want the original plus one retry", len(reqs))
	}
	if !bytes.Contains(reqs[0].Body, []byte("stream_options")) {
		t.Error("the first attempt should have carried stream_options")
	}
	if bytes.Contains(reqs[1].Body, []byte("stream_options")) {
		t.Errorf("the retry still carried stream_options: %s", reqs[1].Body)
	}

	known, supported := r.proxy.UsageReporting()
	if !known || supported {
		t.Fatalf("capability = known:%v supported:%v, want known and unsupported", known, supported)
	}

	// Later requests must skip the doomed injection entirely.
	r.upstream.Reset()
	resp = r.post("/v1/chat/completions", `{"model":"m","stream":true}`, r.secret)
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if n := len(r.upstream.Requests()); n != 1 {
		t.Fatalf("upstream saw %d requests after the capability was learned, want 1", n)
	}
}

func TestGenuineBadRequestIsNotRetried(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.ForceStatus = http.StatusBadRequest
	r := newRig(t, opts)

	resp := r.post("/v1/chat/completions", `{"model":"m","stream":true}`, r.secret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the upstream 400 to reach the caller", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("forced by the test fixture")) {
		t.Fatalf("the upstream error body was lost: %s", body)
	}
	if n := len(r.upstream.Requests()); n != 1 {
		t.Fatalf("upstream saw %d requests, a real 400 must not be retried", n)
	}
}

func TestUpstream429GetsARetryAfterHint(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.ForceStatus = http.StatusTooManyRequests
	r := newRig(t, opts)

	resp := r.post("/v1/chat/completions", `{"model":"m"}`, r.secret)
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream 429 to reach the caller", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
}

func TestUpstreamRetryAfterIsPreserved(t *testing.T) {
	opts := nullupstream.DefaultOptions()
	opts.ForceStatus = http.StatusTooManyRequests
	opts.ForceHeaders = map[string]string{"Retry-After": "17"}
	r := newRig(t, opts)

	resp := r.post("/v1/chat/completions", `{"model":"m"}`, r.secret)
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if got := resp.Header.Get("Retry-After"); got != "17" {
		t.Errorf("Retry-After = %q, want the upstream's own 17", got)
	}
}

// materialise applies the planned injection so the result can be asserted on.
func materialise(body []byte) ([]byte, bool) {
	at, needComma, ok := planInjection(body)
	if !ok {
		return body, false
	}
	info := &requestInfo{Body: body, Injected: true, injectAt: at, injectComma: needComma}
	req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	setBody(req, info)
	out, _ := io.ReadAll(req.Body)
	return out, true
}

func TestWithUsageRequested(t *testing.T) {
	cases := []struct {
		in     string
		ok     bool
		expect string
	}{
		{`{"model":"m","stream":true}`, true, `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`},
		{`{}`, true, `{"stream_options":{"include_usage":true}}`},
		{`  {"a":1}  `, true, `  {"a":1,"stream_options":{"include_usage":true}}  `},
		{`not json`, false, `not json`},
		{``, false, ``},
	}
	for _, c := range cases {
		got, ok := materialise([]byte(c.in))
		if ok != c.ok || string(got) != c.expect {
			t.Errorf("injection of %q = (%q, %v), want (%q, %v)", c.in, got, ok, c.expect, c.ok)
		}
		if ok {
			var v map[string]any
			if err := json.Unmarshal(got, &v); err != nil {
				t.Errorf("injection of %q produced invalid JSON: %v", c.in, err)
			}
		}
	}
}

func TestContentLengthMatchesTheInjectedBody(t *testing.T) {
	for _, in := range []string{`{"model":"m","stream":true}`, `{}`, `{"a":1,"b":[1,2,3]}`} {
		at, needComma, ok := planInjection([]byte(in))
		if !ok {
			t.Fatalf("could not plan injection for %q", in)
		}
		info := &requestInfo{Body: []byte(in), Injected: true, injectAt: at, injectComma: needComma}
		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		setBody(req, info)

		out, _ := io.ReadAll(req.Body)
		if int64(len(out)) != req.ContentLength {
			t.Errorf("%q: wrote %d bytes but declared Content-Length %d", in, len(out), req.ContentLength)
		}
		if req.Header.Get("Content-Length") != strconv.Itoa(len(out)) {
			t.Errorf("%q: Content-Length header is %q, want %d", in, req.Header.Get("Content-Length"), len(out))
		}
	}
}

func TestInspectBodyReadsLimitsAndModel(t *testing.T) {
	cases := []struct {
		body      string
		model     string
		stream    bool
		maxTokens int
		wantUsage bool
	}{
		{`{"model":"a","stream":true,"max_tokens":700}`, "a", true, 700, false},
		{`{"model":"b","max_completion_tokens":900}`, "b", false, 900, false},
		{`{"model":"c","stream":true,"stream_options":{"include_usage":true}}`, "c", true, 2048, true},
		{`{"model":"d"}`, "d", false, 2048, false},
		{`garbage`, "", false, 2048, false},
	}
	for _, c := range cases {
		req, _ := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(c.body))
		info, err := inspectBody(req, 2048)
		if err != nil {
			t.Fatalf("inspectBody(%q): %v", c.body, err)
		}
		if info.Model != c.model || info.Stream != c.stream ||
			info.MaxTokens != c.maxTokens || info.ClientWantsUsage != c.wantUsage {
			t.Errorf("inspectBody(%q) = %+v", c.body, info)
		}
		// The body must still be readable by the proxy afterwards.
		again, _ := io.ReadAll(req.Body)
		if string(again) != c.body {
			t.Errorf("body was consumed: got %q want %q", again, c.body)
		}
	}
}

func BenchmarkInspectAndInject(b *testing.B) {
	// A realistic prompt, since this is the one hot path that scales with body size.
	msg := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 1200)
	body := fmt.Sprintf(`{"model":"m","stream":true,"messages":[{"role":"user","content":%q}]}`, msg)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		info, err := inspectBody(req, 2048)
		if err != nil {
			b.Fatal(err)
		}
		at, needComma, ok := planInjection(info.Body)
		if !ok {
			b.Fatal("injection failed")
		}
		info.Injected, info.injectAt, info.injectComma = true, at, needComma
		setBody(req, info)
	}
}

func BenchmarkSSEScannerThroughput(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"content\":\"token%d \"}}]}\n\n", i)
	}
	sb.WriteString("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":500}}\n\n")
	sb.WriteString("data: [DONE]\n\n")
	stream := sb.String()

	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sc := newSSEScanner(io.NopCloser(strings.NewReader(stream)), true, time.Now)
		if _, err := io.Copy(io.Discard, sc); err != nil {
			b.Fatal(err)
		}
	}
}
