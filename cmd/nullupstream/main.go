// Command nullupstream serves canned OpenAI-compatible responses instantly.
// Comparing direct and proxied requests against it isolates what Quecto costs
// from what the model costs.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"quecto/internal/nullupstream"
)

func main() {
	addr := flag.String("listen", ":8000", "address to listen on")
	chunks := flag.Int("chunks", 8, "content frames per streamed reply")
	ttft := flag.Duration("ttft", 0, "delay before the first frame")
	delay := flag.Duration("chunk-delay", 0, "delay between frames")
	prompt := flag.Int("prompt-tokens", 17, "prompt tokens to report")
	completion := flag.Int("completion-tokens", 8, "completion tokens to report")
	supports := flag.Bool("stream-options", true, "accept stream_options in requests")
	flag.Parse()

	up := nullupstream.New(nullupstream.Options{
		Chunks:                *chunks,
		TTFT:                  *ttft,
		ChunkDelay:            *delay,
		PromptTokens:          *prompt,
		CompletionTokens:      *completion,
		SupportsStreamOptions: *supports,
		Model:                 "null-model",
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           up,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("null upstream listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
