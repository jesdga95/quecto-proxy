package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"quecto/internal/nullupstream"
	"quecto/internal/settings"
)

// unreadable fails the test if anything reads it.
type unreadable struct{ t *testing.T }

func (u unreadable) Read([]byte) (int, error) {
	u.t.Error("the body was read although its declared length was over the cap")
	return 0, io.EOF
}

func TestDeclaredLengthOverTheCapIsRefusedUnread(t *testing.T) {
	req, _ := http.NewRequest("POST", "/v1/chat/completions", unreadable{t})
	req.ContentLength = 2<<20 + 1
	_, err := readBody(req, 2<<20)
	var big *bodyTooLargeError
	if !errors.As(err, &big) || big.limit != 2<<20 {
		t.Fatalf("err = %v, want bodyTooLargeError naming 2 MiB", err)
	}
}

func TestBodyWithNoLengthIsCutAtTheCap(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())
	if err := r.proxy.live.Apply(settings.Patch{MaxRequestBodyMiB: ptr(int64(1))}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 1<<20+64<<10) + `"}]}`
	// MultiReader hides the length, so the client sends the body chunked.
	req, _ := http.NewRequest("POST", r.front.URL+"/v1/chat/completions", io.MultiReader(strings.NewReader(big)))
	req.Header.Set("Authorization", "Bearer "+r.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !bytes.Contains(body, []byte("larger than 1 MiB")) {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

// imageRequest carries one base64 image of about n bytes beside a short prompt.
func imageRequest(n int, escapeSlashes bool) []byte {
	b64 := strings.Repeat("AB/+", n/4)
	if escapeSlashes {
		b64 = strings.ReplaceAll(b64, "/", `\/`)
	}
	return []byte(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"What is in this picture?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]}]}`)
}

func TestDataURIsDoNotInflateTheEstimate(t *testing.T) {
	for _, escaped := range []bool{false, true} {
		req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(imageRequest(200<<20, escaped)))
		info, err := inspectBody(req, 2048, false, 512<<20)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		if info.attachments != 1 {
			t.Errorf("escaped=%v: attachments = %d, want 1", escaped, info.attachments)
		}
		if got, max := info.estimate(), int64(100+attachmentTokens+200); got > max {
			t.Errorf("escaped=%v: estimate = %d, want at most %d", escaped, got, max)
		}
	}
}

func TestProseStartingWithDataStaysInTheEstimate(t *testing.T) {
	body := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"data: ` + strings.Repeat("x", 3000) + `"}]}`
	req, _ := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	info, err := inspectBody(req, 2048, false, 32<<20)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.attachments != 0 || info.estimate() < 1000 {
		t.Errorf("attachments = %d, estimate = %d, want the prose counted", info.attachments, info.estimate())
	}
}

func TestLargeImageIsAdmittedUnderATokenLimit(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())
	r.setLimits(nil, intp(20_000))

	resp := r.post("/v1/chat/completions", string(imageRequest(8<<20, false)), r.secret)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, body = %s", resp.StatusCode, body)
	}
}

// slowUpload takes upload to write the request, then answer to reply.
type slowUpload struct{ upload, answer time.Duration }

func (s slowUpload) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-time.After(s.upload):
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	if tr := httptrace.ContextClientTrace(r.Context()); tr != nil && tr.WroteRequest != nil {
		tr.WroteRequest(httptrace.WroteRequestInfo{})
	}
	select {
	case <-time.After(s.answer):
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func TestUpstreamTimeoutStartsAfterTheUpload(t *testing.T) {
	r := newRig(t, nullupstream.DefaultOptions())
	if err := r.proxy.live.Apply(settings.Patch{HeaderTimeoutS: ptr(int64(1))}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	req, _ := http.NewRequest("POST", "http://upstream/v1/chat/completions", nil)

	// An upload longer than the timeout must not count against it.
	ht := &headerTimeout{base: slowUpload{upload: 1500 * time.Millisecond, answer: 100 * time.Millisecond}, live: r.proxy.live}
	resp, err := ht.RoundTrip(req)
	if err != nil {
		t.Fatalf("slow upload, quick answer: %v", err)
	}
	resp.Body.Close()

	// A slow answer once the body is sent still times out.
	ht.base = slowUpload{upload: 10 * time.Millisecond, answer: 3 * time.Second}
	if _, err := ht.RoundTrip(req); err == nil {
		t.Fatal("an answer slower than the timeout was accepted")
	}
}
