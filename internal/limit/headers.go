package limit

import (
	"net/http"
	"strconv"
	"time"
)

// ApplyHeaders writes the rate limit headers using the names OpenAI clients
// already parse, so existing SDK backoff logic works unchanged.
func (d Decision) ApplyHeaders(h http.Header) {
	if d.RPMLimit != nil {
		h.Set("X-RateLimit-Limit-Requests", strconv.Itoa(*d.RPMLimit))
		h.Set("X-RateLimit-Remaining-Requests", strconv.Itoa(max(0, d.RPMRemaining)))
		h.Set("X-RateLimit-Reset-Requests", formatReset(d.RPMReset))
	}
	if d.TPMLimit != nil {
		h.Set("X-RateLimit-Limit-Tokens", strconv.Itoa(*d.TPMLimit))
		h.Set("X-RateLimit-Remaining-Tokens", strconv.FormatInt(max(0, d.TPMRemaining), 10))
		h.Set("X-RateLimit-Reset-Tokens", formatReset(d.TPMReset))
	}
}

// RetryAfterSeconds rounds up so a client that waits this long will be admitted.
func (d Decision) RetryAfterSeconds() int {
	if d.RetryAfter <= 0 {
		return 1
	}
	s := int((d.RetryAfter + time.Second - time.Nanosecond) / time.Second)
	if s < 1 {
		return 1
	}
	return s
}

func formatReset(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(time.Millisecond).String()
}
