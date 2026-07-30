package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quecto/internal/nullupstream"
)

// The benchmarks below run the same request against the model server directly
// and then through Quecto. The difference is what the proxy costs, with the
// model's own latency removed by the null upstream.

func benchRequest(b *testing.B, url, auth, body string) {
	b.Helper()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req, err := http.NewRequest("POST", url, strings.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status %d", resp.StatusCode)
		}
	}
}

func benchPair(b *testing.B, body string, chunks int) {
	opts := nullupstream.DefaultOptions()
	opts.Chunks = chunks

	b.Run("direct", func(b *testing.B) {
		up := httptest.NewServer(nullupstream.New(opts))
		defer up.Close()
		benchRequest(b, up.URL+"/v1/chat/completions", "", body)
	})

	b.Run("proxied", func(b *testing.B) {
		r := newRig(b, opts)
		benchRequest(b, r.front.URL+"/v1/chat/completions", r.secret, body)
	})
}

func BenchmarkEndToEndNonStreaming(b *testing.B) {
	benchPair(b, `{"model":"m","messages":[{"role":"user","content":"hello"}]}`, 8)
}

func BenchmarkEndToEndStreaming(b *testing.B) {
	benchPair(b, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hello"}]}`, 8)
}

func BenchmarkEndToEndStreamingLong(b *testing.B) {
	benchPair(b, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hello"}]}`, 200)
}

func BenchmarkEndToEndLargePrompt(b *testing.B) {
	prompt := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 1200)
	benchPair(b, `{"model":"m","stream":true,"messages":[{"role":"user","content":"`+prompt+`"}]}`, 8)
}
