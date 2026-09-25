package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"time"
)

var (
	frameSep     = []byte("\n\n")
	dataPrefix   = []byte("data:")
	usageMarker  = []byte(`"usage"`)
	doneMarker   = []byte("[DONE]")
	maxFrameSize = 1 << 20
)

// sseScanner passes an event stream through untouched while reading the token
// usage out of it. When the proxy added stream_options, it also removes the
// usage-only frame the caller never asked for.
type sseScanner struct {
	src        io.ReadCloser
	stripUsage bool
	now        func() time.Time

	scratch []byte
	carry   []byte
	pending []byte

	done bool
	err  error

	mu        sync.Mutex
	usage     *Usage
	cached    *int64
	frames    int
	firstSeen time.Time
}

func newSSEScanner(src io.ReadCloser, stripUsage bool, now func() time.Time) *sseScanner {
	return &sseScanner{
		src:        src,
		stripUsage: stripUsage,
		now:        now,
		scratch:    make([]byte, 16<<10),
	}
}

func (s *sseScanner) Read(p []byte) (int, error) {
	for len(s.pending) == 0 && !s.done {
		n, err := s.src.Read(s.scratch)
		if n > 0 {
			s.carry = append(s.carry, s.scratch[:n]...)
			s.split(false)
		}
		if err != nil {
			s.done = true
			s.split(true)
			if err != io.EOF {
				s.err = err
			}
		}
	}
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		if len(s.pending) == 0 {
			s.pending = s.pending[:0]
		}
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	return 0, io.EOF
}

func (s *sseScanner) Close() error { return s.src.Close() }

// split moves complete frames from carry into pending, dropping any the caller
// should not see. Incomplete frames stay in carry until the rest arrives.
func (s *sseScanner) split(final bool) {
	for {
		idx := bytes.Index(s.carry, frameSep)
		if idx < 0 {
			break
		}
		frame := s.carry[:idx+len(frameSep)]
		s.carry = s.carry[idx+len(frameSep):]
		if s.keep(frame) {
			s.pending = append(s.pending, frame...)
		}
	}
	// A frame larger than the cap is malformed, so stop holding it back.
	if len(s.carry) > maxFrameSize {
		s.pending = append(s.pending, s.carry...)
		s.carry = s.carry[:0]
	}
	if final && len(s.carry) > 0 {
		if s.keep(s.carry) {
			s.pending = append(s.pending, s.carry...)
		}
		s.carry = s.carry[:0]
	}
}

func (s *sseScanner) keep(frame []byte) bool {
	s.mu.Lock()
	if s.frames == 0 {
		s.firstSeen = s.now()
	}
	s.frames++
	s.mu.Unlock()

	if !bytes.Contains(frame, usageMarker) || bytes.Contains(frame, doneMarker) {
		return true
	}
	payload, ok := framePayload(frame)
	if !ok {
		return true
	}

	var chunk struct {
		Usage   *Usage            `json:"usage"`
		Timings *timings          `json:"timings"`
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil || chunk.Usage == nil {
		return true
	}

	s.mu.Lock()
	s.usage = chunk.Usage
	// A later usage frame replaces the earlier one whole, cached value included.
	s.cached = nil
	if c, ok := cachedTokens(*chunk.Usage, chunk.Timings); ok {
		s.cached = &c
	}
	s.mu.Unlock()

	// Only a usage-only frame is safe to drop. Servers that attach usage to the
	// last content chunk must keep it, or the caller loses that chunk.
	return !(s.stripUsage && len(chunk.Choices) == 0)
}

// framePayload returns the JSON after the data: field of an SSE frame.
func framePayload(frame []byte) ([]byte, bool) {
	for len(frame) > 0 {
		line := frame
		if i := bytes.IndexByte(frame, '\n'); i >= 0 {
			line, frame = frame[:i], frame[i+1:]
		} else {
			frame = nil
		}
		if !bytes.HasPrefix(line, dataPrefix) {
			continue
		}
		payload := bytes.TrimSpace(line[len(dataPrefix):])
		if len(payload) > 0 && payload[0] == '{' {
			return payload, true
		}
	}
	return nil, false
}

func (s *sseScanner) Usage() (Usage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usage == nil {
		return Usage{}, false
	}
	return *s.usage, true
}

func (s *sseScanner) CachedTokens() (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached == nil {
		return 0, false
	}
	return *s.cached, true
}

func (s *sseScanner) Frames() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames
}

func (s *sseScanner) FirstFrameAt() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstSeen, s.frames > 0
}
