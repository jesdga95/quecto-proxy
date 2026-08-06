// Package nullupstream is a stand-in OpenAI-compatible server. It answers
// instantly so tests and benchmarks measure the proxy rather than a model.
package nullupstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Options struct {
	// Number of content chunks emitted for a streamed response.
	Chunks int
	// Wait before the first chunk, then between each chunk.
	TTFT       time.Duration
	ChunkDelay time.Duration

	PromptTokens     int
	CompletionTokens int

	// When false, a request carrying stream_options is rejected with 400,
	// which is how some strict servers behave.
	SupportsStreamOptions bool

	// Forces a status code for every request when non-zero,
	// with any of these headers attached.
	ForceStatus  int
	ForceHeaders map[string]string
	Model        string
}

func DefaultOptions() Options {
	return Options{
		Chunks:                8,
		PromptTokens:          17,
		CompletionTokens:      8,
		SupportsStreamOptions: true,
		Model:                 "null-model",
	}
}

type Recorded struct {
	Path          string
	Authorization string
	Body          []byte
}

type Upstream struct {
	mu       sync.Mutex
	opts     Options
	requests []Recorded
}

func New(o Options) *Upstream {
	if o.Model == "" {
		o.Model = "null-model"
	}
	if o.Chunks == 0 {
		o.Chunks = 1
	}
	return &Upstream{opts: o}
}

func (u *Upstream) SetOptions(o Options) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.opts = o
}

func (u *Upstream) options() Options {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.opts
}

func (u *Upstream) Requests() []Recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Recorded(nil), u.requests...)
}

func (u *Upstream) LastRequest() (Recorded, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		return Recorded{}, false
	}
	return u.requests[len(u.requests)-1], true
}

func (u *Upstream) Reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = nil
}

type chatRequest struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	MaxTokens     *int   `json:"max_tokens"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (u *Upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.requests = append(u.requests, Recorded{
		Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body,
	})
	u.mu.Unlock()

	opts := u.options()

	if opts.ForceStatus != 0 {
		for k, v := range opts.ForceHeaders {
			w.Header().Set(k, v)
		}
		writeError(w, opts.ForceStatus, "forced by the test fixture")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/models") {
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   []any{map[string]any{"id": opts.Model, "object": "model"}},
		})
		return
	}
	if reply, ok := utilityReply(r.URL.Path); ok {
		writeJSON(w, http.StatusOK, reply)
		return
	}

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.StreamOptions != nil && !opts.SupportsStreamOptions {
		writeError(w, http.StatusBadRequest, "unrecognized request argument: stream_options")
		return
	}

	if !req.Stream {
		u.serveWhole(w, req, opts)
		return
	}
	u.serveStream(w, req, opts)
}

// utilityReply answers the routes a model server keeps outside the OpenAI
// surface, in their own shapes. None of them carry a usage block, which is the
// part the proxy has to account for without.
func utilityReply(path string) (any, bool) {
	switch path {
	case "/health":
		return map[string]any{"status": "ok"}, true
	case "/props":
		return map[string]any{"default_generation_settings": map[string]any{"n_ctx": 4096}}, true
	case "/tokenize":
		return map[string]any{"tokens": []int{15339, 1917}}, true
	case "/detokenize":
		return map[string]any{"content": "hello world"}, true
	case "/apply-template":
		return map[string]any{"prompt": "<|user|>hello<|assistant|>"}, true
	}
	return nil, false
}

func (u *Upstream) serveWhole(w http.ResponseWriter, req chatRequest, opts Options) {
	if opts.TTFT > 0 {
		time.Sleep(opts.TTFT)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "chatcmpl-null", "object": "chat.completion", "created": time.Now().Unix(),
		"model": opts.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": strings.Repeat("tok ", opts.Chunks)},
			"finish_reason": "stop",
		}},
		"usage": usage(opts),
	})
}

func (u *Upstream) serveStream(w http.ResponseWriter, req chatRequest, opts Options) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	if opts.TTFT > 0 {
		time.Sleep(opts.TTFT)
	}
	for i := 0; i < opts.Chunks; i++ {
		if i > 0 && opts.ChunkDelay > 0 {
			time.Sleep(opts.ChunkDelay)
		}
		fmt.Fprintf(w, "data: %s\n\n", chunk(opts.Model, map[string]any{"content": fmt.Sprintf("tok%d ", i)}, nil))
		flush()
	}
	fmt.Fprintf(w, "data: %s\n\n", chunk(opts.Model, map[string]any{}, strPtr("stop")))
	flush()

	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		payload, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-null", "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": opts.Model, "choices": []any{}, "usage": usage(opts),
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flush()
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

func chunk(model string, delta map[string]any, finish *string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-null", "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": model,
		"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": finish,
		}},
	})
	return b
}

func usage(o Options) map[string]any {
	return map[string]any{
		"prompt_tokens":     o.PromptTokens,
		"completion_tokens": o.CompletionTokens,
		"total_tokens":      o.PromptTokens + o.CompletionTokens,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "invalid_request_error"},
	})
}

func strPtr(s string) *string { return &s }
