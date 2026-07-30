package proxy

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Responses beyond this are streamed through without usage accounting. A
// non-streamed completion this large is far outside normal traffic.
const maxBufferedResponse = 2 << 20

// bodyReader is the part of a wrapped response body the proxy reads back from.
type bodyReader interface {
	io.ReadCloser
	Usage() (Usage, bool)
	Frames() int
	FirstFrameAt() (time.Time, bool)
}

// wholeBodyReader passes a non-streamed response through while keeping a
// bounded copy so the usage block can be read once the body ends.
type wholeBodyReader struct {
	src io.ReadCloser
	now func() time.Time

	mu      sync.Mutex
	buf     []byte
	over    bool
	usage   *Usage
	seen    bool
	firstAt time.Time
	parsed  bool
}

func newWholeBodyReader(src io.ReadCloser, now func() time.Time) *wholeBodyReader {
	return &wholeBodyReader{src: src, now: now, buf: make([]byte, 0, 8<<10)}
}

func (b *wholeBodyReader) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	if n > 0 {
		b.mu.Lock()
		if !b.seen {
			b.firstAt, b.seen = b.now(), true
		}
		if !b.over {
			if len(b.buf)+n > maxBufferedResponse {
				b.over, b.buf = true, nil
			} else {
				b.buf = append(b.buf, p[:n]...)
			}
		}
		b.mu.Unlock()
	}
	if err != nil {
		b.parse()
	}
	return n, err
}

func (b *wholeBodyReader) Close() error { return b.src.Close() }

func (b *wholeBodyReader) parse() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.parsed || b.over {
		return
	}
	b.parsed = true
	var payload struct {
		Usage *Usage `json:"usage"`
	}
	if json.Unmarshal(b.buf, &payload) == nil && payload.Usage != nil {
		b.usage = payload.Usage
	}
	b.buf = nil
}

func (b *wholeBodyReader) Usage() (Usage, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.usage == nil {
		return Usage{}, false
	}
	return *b.usage, true
}

func (b *wholeBodyReader) Frames() int { return 0 }

func (b *wholeBodyReader) FirstFrameAt() (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.firstAt, b.seen
}
