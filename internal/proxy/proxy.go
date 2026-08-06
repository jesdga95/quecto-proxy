// Package proxy forwards OpenAI-compatible traffic to the upstream model server.
package proxy

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"quecto/internal/keys"
	"quecto/internal/limit"
	"quecto/internal/settings"
	"quecto/internal/store"
)

type ctxKey int

const ctxKeyState ctxKey = iota

// Record is what the proxy observed while serving one request.
type Record struct {
	KeyID     int64
	Model     string
	Status    int
	Stream    bool
	Usage     Usage
	HasUsage  bool
	Duration  time.Duration
	TTFT      time.Duration
	HasTTFT   bool
	Estimated bool
}

// Reporter receives one Record per completed request. It must not block.
type Reporter interface {
	Report(Record)
}

// Observer receives the time spent authenticating and rate limiting, which is
// everything the proxy adds before the request leaves for the upstream.
type Observer interface {
	ObserveGate(time.Duration)
}

type Options struct {
	HeaderTimeout    time.Duration
	TrustForwardedIP bool
}

type Proxy struct {
	live     *settings.Live
	registry *keys.Registry
	limiter  *limit.Limiter
	reporter Reporter
	observer Observer
	opts     Options
	cap      capability
	rp       *httputil.ReverseProxy
	log      *slog.Logger
	now      func() time.Time
}

// state travels with a request so the response side can settle and report it.
type state struct {
	key       *store.APIKey
	info      *requestInfo
	res       *limit.Reservation
	start     time.Time
	body      bodyReader
	status    int
	streaming bool
}

func New(live *settings.Live, registry *keys.Registry, limiter *limit.Limiter, opts Options, log *slog.Logger) *Proxy {
	p := &Proxy{
		live: live, registry: registry, limiter: limiter,
		opts: opts, log: log, now: time.Now,
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: opts.HeaderTimeout,
		ForceAttemptHTTP2:     true,
		// Responses are read as they stream, so never let the transport gzip them.
		DisableCompression: true,
	}

	p.rp = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		Transport:      transport,
		ModifyResponse: p.modifyResponse,
		FlushInterval:  -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			p.log.Warn("upstream request failed", "path", r.URL.Path, "error", err)
			errUpstreamUnreachable().write(w)
		},
	}
	return p
}

func (p *Proxy) SetReporter(r Reporter) { p.reporter = r }

func (p *Proxy) SetObserver(o Observer) { p.observer = o }

// UsageReporting tells the dashboard whether the upstream returns token counts.
func (p *Proxy) UsageReporting() (known, supported bool) { return p.cap.Supported() }

func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(p.serve)
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	start := p.now()

	key, apiErr := p.authenticate(r)
	if apiErr != nil {
		apiErr.write(w)
		return
	}

	info, err := inspectBody(r, p.live.DefaultMaxTokens(), isUtilityPath(r.URL.Path))
	if err != nil {
		errBadRequestBody(err).write(w)
		return
	}

	res, decision := p.limiter.Admit(key, info.estimate())
	decision.ApplyHeaders(w.Header())
	if !decision.Allowed {
		// A spent quota answers like the other terminal key states. Telling a
		// client to retry would send it into a backoff loop it never gets out of.
		// A quota merely held by calls in flight is the opposite: retryable, and
		// answered as a rate limit like every other refusal that clears itself.
		apiErr := errRateLimited(decision)
		if decision.Reason == "quota" {
			apiErr = errExhaustedKey()
		} else {
			w.Header().Set("Retry-After", strconv.Itoa(decision.RetryAfterSeconds()))
		}
		apiErr.write(w)
		p.report(Record{
			KeyID: key.ID, Model: info.Model, Status: apiErr.Status,
			Stream: info.Stream, Duration: p.now().Sub(start),
		})
		return
	}

	// Ask for token counts when the caller did not, then remove the extra frame.
	// stream_options is an OpenAI field, so a utility route never gets it.
	if info.Stream && !info.Utility && !info.ClientWantsUsage && p.cap.mayInject() {
		if at, needComma, ok := planInjection(info.Body); ok {
			info.Injected, info.injectAt, info.injectComma = true, at, needComma
			setBody(r, info)
		}
	}

	st := &state{key: key, info: info, res: res, start: start}
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyState, st))

	if p.observer != nil {
		p.observer.ObserveGate(p.now().Sub(start))
	}

	known, _ := p.cap.Supported()
	rw := &retryWriter{ResponseWriter: w, watch: info.Injected && !known}
	p.rp.ServeHTTP(rw, r)

	if rw.rejectedInjection() {
		p.cap.reject()
		p.log.Warn("upstream rejected stream_options, token accounting is unavailable for streamed replies")
		p.retryWithoutInjection(w, r, st)
		return
	}
	rw.release()

	p.settleAndReport(st)
}

// retryWithoutInjection replays the request using the caller's original body.
func (p *Proxy) retryWithoutInjection(w http.ResponseWriter, r *http.Request, st *state) {
	st.info.Injected = false
	st.body = nil
	setBody(r, st.info)
	p.rp.ServeHTTP(w, r)
	p.settleAndReport(st)
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	// Give client backoff logic a hint when the upstream throttles without one.
	if resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") == "" {
		resp.Header.Set("Retry-After", "1")
	}

	st, ok := resp.Request.Context().Value(ctxKeyState).(*state)
	if !ok {
		return nil
	}
	st.status = resp.StatusCode

	if resp.StatusCode == http.StatusOK && st.info.Injected {
		p.cap.confirm()
	}

	if isEventStream(resp) {
		st.streaming = true
		sc := newSSEScanner(resp.Body, st.info.Injected, p.now)
		resp.Body = sc
		st.body = sc
		return nil
	}
	wb := newWholeBodyReader(resp.Body, p.now)
	resp.Body = wb
	st.body = wb
	return nil
}

// settleAndReport releases the token reservation and hands the record on.
func (p *Proxy) settleAndReport(st *state) {
	rec := Record{
		KeyID: st.key.ID, Model: st.info.Model, Status: st.status,
		Stream: st.streaming, Duration: p.now().Sub(st.start),
	}
	if rec.Status == 0 {
		rec.Status = http.StatusBadGateway
	}

	var charge int64
	switch {
	case st.body == nil:
		charge = 0
	default:
		if u, ok := st.body.Usage(); ok {
			rec.Usage, rec.HasUsage = u, true
			charge = u.Total()
		} else if frames := st.body.Frames(); frames > 0 {
			// The stream ended without a usage frame, most likely because the
			// caller went away. Charge for the frames that were produced.
			charge = int64(frames)
			rec.Usage = Usage{CompletionTokens: charge, TotalTokens: charge}
			rec.Estimated = true
		}
		if at, ok := st.body.FirstFrameAt(); ok {
			rec.TTFT, rec.HasTTFT = at.Sub(st.start), true
		}
	}

	st.res.Settle(charge)
	p.report(rec)
}

func (p *Proxy) report(rec Record) {
	if p.reporter != nil {
		p.reporter.Report(rec)
	}
}

func (p *Proxy) authenticate(r *http.Request) (*store.APIKey, *apiError) {
	raw, ok := keys.FromAuthHeader(r.Header.Get("Authorization"))
	if !ok {
		return nil, errMissingKey()
	}
	key, found := p.registry.Lookup(keys.Hash(raw))
	if !found {
		return nil, errInvalidKey()
	}
	if usable, reason := key.Status(p.now().Unix()); !usable {
		switch reason {
		case "expired":
			return nil, errExpiredKey()
		case "revoked":
			return nil, errRevokedKey()
		case "exhausted":
			return nil, errExhaustedKey()
		default:
			return nil, errInvalidKey()
		}
	}
	return key, nil
}

func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	up := p.live.Upstream()

	pr.Out.URL.Scheme = up.Scheme
	pr.Out.URL.Host = up.Host
	pr.Out.Host = up.Host
	pr.Out.URL.Path = joinUpstreamPath(up.Path, pr.In.URL.Path)
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery

	// Replace the caller's key with the upstream credential, if one is set.
	pr.Out.Header.Del("Authorization")
	if k := p.live.UpstreamAPIKey(); k != "" {
		pr.Out.Header.Set("Authorization", "Bearer "+k)
	}
	pr.Out.Header.Del("Accept-Encoding")

	if p.opts.TrustForwardedIP {
		pr.SetXForwarded()
	}
}

func isEventStream(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}

// joinUpstreamPath maps an incoming path onto the configured base path. A base
// without a path is treated as /v1 so both common upstream forms work.
//
// The upstream's utility routes are served at its root, not under its OpenAI
// prefix, so they are joined to the base's parent instead. Without that a base
// of /v1 would turn /tokenize into /v1/tokenize, which is nowhere.
func joinUpstreamPath(base, incoming string) string {
	if base == "" {
		base = "/v1"
	}
	if !strings.HasPrefix(incoming, "/v1") {
		return upstreamRoot(base) + incoming
	}
	suffix := strings.TrimPrefix(incoming, "/v1")
	if suffix == "" {
		return base
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return base + suffix
}

// upstreamRoot is the base with its OpenAI prefix removed, which is where the
// upstream serves its own routes. A base carrying a mount point keeps it, so a
// swapper behind /llama/v1 is still reached at /llama/tokenize.
func upstreamRoot(base string) string {
	return strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/v1")
}
