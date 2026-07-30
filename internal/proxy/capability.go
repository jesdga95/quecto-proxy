package proxy

import (
	"bytes"
	"net/http"
	"sync/atomic"
)

// Whether the upstream accepts stream_options. Some strict servers reject
// unknown request fields, so the answer is learned from the first refusal.
const (
	usageUnknown int32 = iota
	usageSupported
	usageUnsupported
)

type capability struct{ state atomic.Int32 }

func (c *capability) mayInject() bool { return c.state.Load() != usageUnsupported }
func (c *capability) confirm()        { c.state.CompareAndSwap(usageUnknown, usageSupported) }
func (c *capability) reject()         { c.state.Store(usageUnsupported) }

func (c *capability) Supported() (known, ok bool) {
	switch c.state.Load() {
	case usageSupported:
		return true, true
	case usageUnsupported:
		return true, false
	default:
		return false, false
	}
}

// retryWriter holds back a 400 that might be the upstream rejecting the
// injected field, so the request can be sent again without it. Every other
// response is passed straight through and keeps streaming.
//
// While watching, headers are collected separately. A held response must leave
// nothing behind, or its Content-Type and Content-Length would corrupt the retry.
type retryWriter struct {
	http.ResponseWriter
	watch bool

	shadow    http.Header
	held      bool
	committed bool
	status    int
	body      bytes.Buffer
}

func (w *retryWriter) Header() http.Header {
	if !w.watch || w.committed {
		return w.ResponseWriter.Header()
	}
	if w.shadow == nil {
		w.shadow = make(http.Header)
	}
	return w.shadow
}

func (w *retryWriter) commit() {
	if !w.committed {
		for k, vs := range w.shadow {
			w.ResponseWriter.Header()[k] = append([]string(nil), vs...)
		}
		w.committed = true
	}
}

func (w *retryWriter) WriteHeader(status int) {
	if w.committed || w.held {
		return
	}
	if w.watch && status == http.StatusBadRequest {
		w.held, w.status = true, status
		return
	}
	w.commit()
	w.ResponseWriter.WriteHeader(status)
}

func (w *retryWriter) Write(p []byte) (int, error) {
	if w.held {
		return w.body.Write(p)
	}
	w.commit()
	return w.ResponseWriter.Write(p)
}

func (w *retryWriter) Flush() {
	if w.held {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// rejectedInjection reports whether the held response looks like the upstream
// complaining about stream_options rather than about the caller's own request.
func (w *retryWriter) rejectedInjection() bool {
	return w.held && bytes.Contains(w.body.Bytes(), []byte("stream_options"))
}

// release forwards a held response that turned out to be a genuine error.
func (w *retryWriter) release() {
	if !w.held {
		return
	}
	w.held = false
	w.committed = true
	w.ResponseWriter.WriteHeader(w.status)
	w.ResponseWriter.Write(w.body.Bytes())
}
