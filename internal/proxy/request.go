package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// attachmentTokens is what each inline base64 attachment holds against the
// token limits in place of its bytes. The real count bills after the reply.
const attachmentTokens = 1024

// bodyTooLargeError reports a request body over the configured cap.
type bodyTooLargeError struct{ limit int64 }

func (e *bodyTooLargeError) Error() string {
	return fmt.Sprintf("request body is larger than %d bytes", e.limit)
}

// Usage is the token accounting an OpenAI-compatible server reports.
type Usage struct {
	PromptTokens        int64                `json:"prompt_tokens"`
	CompletionTokens    int64                `json:"completion_tokens"`
	TotalTokens         int64                `json:"total_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details"`
}

type promptTokensDetails struct {
	CachedTokens *int64 `json:"cached_tokens"`
}

// timings is the llama.cpp block that rides beside usage and says how much of
// the prompt the server's cache already held. No other server sends it.
type timings struct {
	CacheN *int64 `json:"cache_n"`
}

// cachedTokens returns how much of the prompt the upstream served from its
// cache, if it said. The OpenAI field wins over the llama.cpp one, and a value
// above the whole prompt is clamped to it rather than trusted.
func cachedTokens(u Usage, t *timings) (int64, bool) {
	var v *int64
	switch {
	case u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil:
		v = u.PromptTokensDetails.CachedTokens
	case t != nil && t.CacheN != nil:
		v = t.CacheN
	}
	if v == nil || *v < 0 {
		return 0, false
	}
	if *v > u.PromptTokens {
		return u.PromptTokens, true
	}
	return *v, true
}

func (u Usage) Total() int64 {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
}

// requestInfo is what the proxy needs to know about an inbound completion request.
type requestInfo struct {
	Model     string
	Stream    bool
	MaxTokens int

	// True for one of the upstream's utility routes, which cost no tokens.
	Utility bool

	// True when the caller already asked for usage, so the reply must keep it.
	ClientWantsUsage bool
	// True when the proxy added stream_options and must remove the extra frame.
	Injected    bool
	injectAt    int
	injectComma bool

	Body []byte

	// Bytes of the body inside base64 data: URIs, and how many there were.
	attachmentBytes int
	attachments     int
}

// estimate is what the call is held against until the real cost is known: the
// reply cannot pass max_tokens, and the prompt is read off the body at a
// deliberately tight three bytes a token. Charging only max_tokens would leave
// every prompt out of the hold, which is most of the cost of a short reply.
//
// An inline image, document or video is base64 that a model never reads as
// text, so its bytes are left out and each one holds a flat attachmentTokens.
//
// A utility route holds nothing. It generates no tokens, so any hold would be
// room taken from a quota the call is never going to spend, and a long enough
// body could refuse the key outright for a request that bills zero.
func (i *requestInfo) estimate() int64 {
	if i.Utility {
		return 0
	}
	text := len(i.Body) - i.attachmentBytes
	return int64(text/3 + i.attachments*attachmentTokens + i.MaxTokens)
}

// inspectBody reads the request body and reports what the proxy needs from it.
// A body that is not JSON is passed through untouched.
func inspectBody(r *http.Request, defaultMaxTokens int, utility bool, maxBody int64) (*requestInfo, error) {
	if r.Body == nil {
		return &requestInfo{MaxTokens: defaultMaxTokens, Utility: utility}, nil
	}
	raw, err := readBody(r, maxBody)
	if err != nil {
		return nil, err
	}

	info := &requestInfo{Body: raw, MaxTokens: defaultMaxTokens, Utility: utility}
	setBody(r, info)

	p, ok := scanRequest(raw)
	if !ok {
		return info, nil
	}

	info.attachments, info.attachmentBytes = dataURIs(raw)
	info.Model = p.Model
	info.Stream = p.Stream
	info.ClientWantsUsage = p.HasStreamOptions && p.IncludeUsage
	switch {
	case p.MaxCompletionTokens > 0:
		info.MaxTokens = p.MaxCompletionTokens
	case p.MaxTokens > 0:
		info.MaxTokens = p.MaxTokens
	}
	return info, nil
}

// readBody sizes the buffer from Content-Length so a large prompt is not
// copied through a series of growing slices. A declared length over the cap
// is refused before any of the body is read.
func readBody(r *http.Request, maxBody int64) ([]byte, error) {
	defer r.Body.Close()

	if r.ContentLength > maxBody {
		return nil, &bodyTooLargeError{limit: maxBody}
	}
	var buf bytes.Buffer
	if n := r.ContentLength; n > 0 {
		buf.Grow(int(n) + 1)
	}
	if _, err := buf.ReadFrom(io.LimitReader(r.Body, maxBody+1)); err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if int64(buf.Len()) > maxBody {
		return nil, &bodyTooLargeError{limit: maxBody}
	}
	return buf.Bytes(), nil
}

var (
	dataURIPrefix = []byte("data:")
	base64Marker  = []byte(";base64,")
)

// dataURIs counts the base64 data: URIs among the body's strings and the bytes
// they hold. Only base64 counts, so prose that happens to open with "data:"
// stays in the estimate. It jumps quote to quote, as the scanner does.
func dataURIs(body []byte) (count, size int) {
	s := scanner{b: body}
	for {
		j := bytes.IndexByte(s.b[s.i:], '"')
		if j < 0 {
			return count, size
		}
		s.i += j
		str, ok := s.str()
		if !ok {
			return count, size
		}
		if !bytes.HasPrefix(str, dataURIPrefix) {
			continue
		}
		if bytes.Contains(str[:min(len(str), 256)], base64Marker) {
			count++
			size += len(str)
		}
	}
}

var (
	usageField = []byte(`"stream_options":{"include_usage":true}`)
	comma      = []byte(",")
)

// planInjection locates the closing brace the usage field goes before, and
// whether a separating comma is needed. It reports false for a non-object body.
func planInjection(body []byte) (at int, needComma bool, ok bool) {
	end := bytes.LastIndexByte(body, '}')
	if end < 0 {
		return 0, false, false
	}
	start := bytes.IndexByte(body, '{')
	if start < 0 || start > end {
		return 0, false, false
	}
	return end, len(bytes.TrimSpace(body[start+1:end])) > 0, true
}

// setBody points the request at the caller's bytes. When the usage field is
// being added it is stitched in with a reader rather than by rebuilding the
// body, so a large prompt is never copied a second time.
func setBody(r *http.Request, info *requestInfo) {
	if !info.Injected {
		r.Body = io.NopCloser(bytes.NewReader(info.Body))
		setLength(r, len(info.Body))
		return
	}

	n := len(info.Body) + len(usageField)
	parts := make([]io.Reader, 0, 4)
	parts = append(parts, bytes.NewReader(info.Body[:info.injectAt]))
	if info.injectComma {
		parts = append(parts, bytes.NewReader(comma))
		n++
	}
	parts = append(parts, bytes.NewReader(usageField), bytes.NewReader(info.Body[info.injectAt:]))

	r.Body = io.NopCloser(io.MultiReader(parts...))
	setLength(r, n)
}

func setLength(r *http.Request, n int) {
	r.ContentLength = int64(n)
	r.Header.Set("Content-Length", strconv.Itoa(n))
}
