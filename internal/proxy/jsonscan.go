package proxy

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// parsedRequest is the handful of top-level fields the proxy cares about.
type parsedRequest struct {
	Model               string
	Stream              bool
	MaxTokens           int
	MaxCompletionTokens int
	HasStreamOptions    bool
	IncludeUsage        bool
}

// scanRequest reads the fields above without descending into the message list.
// A full unmarshal would walk the entire prompt, which is the largest part of
// the body and none of it is needed here.
func scanRequest(body []byte) (parsedRequest, bool) {
	var p parsedRequest
	s := scanner{b: body}

	s.space()
	if !s.take('{') {
		return p, false
	}
	for {
		s.space()
		if s.done() {
			return p, false
		}
		switch s.b[s.i] {
		case '}':
			return p, true
		case ',':
			s.i++
			continue
		}

		key, ok := s.str()
		if !ok {
			return p, false
		}
		s.space()
		if !s.take(':') {
			return p, false
		}
		s.space()

		start := s.i
		if !s.value() {
			return p, false
		}
		raw := bytes.TrimSpace(s.b[start:s.i])

		switch string(key) {
		case "model":
			if v, err := strconv.Unquote(string(raw)); err == nil {
				p.Model = v
			}
		case "stream":
			p.Stream = bytes.Equal(raw, []byte("true"))
		case "max_tokens":
			if n, err := strconv.Atoi(string(raw)); err == nil {
				p.MaxTokens = n
			}
		case "max_completion_tokens":
			if n, err := strconv.Atoi(string(raw)); err == nil {
				p.MaxCompletionTokens = n
			}
		case "stream_options":
			p.HasStreamOptions = true
			var so struct {
				IncludeUsage bool `json:"include_usage"`
			}
			if json.Unmarshal(raw, &so) == nil {
				p.IncludeUsage = so.IncludeUsage
			}
		}
	}
}

type scanner struct {
	b []byte
	i int
}

func (s *scanner) done() bool { return s.i >= len(s.b) }

func (s *scanner) take(c byte) bool {
	if s.done() || s.b[s.i] != c {
		return false
	}
	s.i++
	return true
}

func (s *scanner) space() {
	for !s.done() {
		switch s.b[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

// str consumes a quoted string and returns its raw contents, escapes included.
// It jumps quote to quote rather than walking each byte, which matters because
// the prompt is usually the largest string in the body and is always skipped.
func (s *scanner) str() ([]byte, bool) {
	if s.done() || s.b[s.i] != '"' {
		return nil, false
	}
	s.i++
	start := s.i
	for s.i <= len(s.b) {
		j := bytes.IndexByte(s.b[s.i:], '"')
		if j < 0 {
			return nil, false
		}
		end := s.i + j
		s.i = end + 1

		// An odd run of backslashes means this quote is escaped, not the end.
		slashes := 0
		for k := end - 1; k >= start && s.b[k] == '\\'; k-- {
			slashes++
		}
		if slashes%2 == 0 {
			return s.b[start:end], true
		}
	}
	return nil, false
}

// value advances past one JSON value of any shape.
func (s *scanner) value() bool {
	s.space()
	if s.done() {
		return false
	}
	switch s.b[s.i] {
	case '"':
		_, ok := s.str()
		return ok
	case '{', '[':
		open := s.b[s.i]
		shut := byte('}')
		if open == '[' {
			shut = ']'
		}
		depth := 0
		for !s.done() {
			switch c := s.b[s.i]; {
			case c == '"':
				if _, ok := s.str(); !ok {
					return false
				}
			case c == open:
				depth++
				s.i++
			case c == shut:
				depth--
				s.i++
				if depth == 0 {
					return true
				}
			default:
				s.i++
			}
		}
		return false
	default:
		for !s.done() {
			switch s.b[s.i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return true
			default:
				s.i++
			}
		}
		return true
	}
}
