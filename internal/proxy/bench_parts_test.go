package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func benchBody() string {
	msg := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 1200)
	return fmt.Sprintf(`{"model":"m","stream":true,"messages":[{"role":"user","content":%q}]}`, msg)
}

func BenchmarkPartScanRequest(b *testing.B) {
	body := []byte(benchBody())
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := scanRequest(body); !ok {
			b.Fatal("scan failed")
		}
	}
}

func BenchmarkPartSetBodyInjected(b *testing.B) {
	body := []byte(benchBody())
	at, needComma, ok := planInjection(body)
	if !ok {
		b.Fatal("inject failed")
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		info := &requestInfo{Body: body, Injected: true, injectAt: at, injectComma: needComma}
		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		setBody(req, info)
	}
}

func BenchmarkPartReadBody(b *testing.B) {
	body := benchBody()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		if _, err := readBody(req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPartNewRequestOnly(b *testing.B) {
	body := benchBody()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	}
}
