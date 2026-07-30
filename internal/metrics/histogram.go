// Package metrics keeps a small in-process latency histogram. It exists so the
// dashboard can show what the proxy itself costs without pulling in a metrics
// library or a scrape endpoint.
package metrics

import (
	"math"
	"sync/atomic"
	"time"
)

const (
	buckets = 72
	// Bounds start at one microsecond and grow by 40 percent per bucket,
	// which spans microseconds to minutes in a small fixed array.
	firstBound = float64(time.Microsecond)
	growth     = 1.4
)

var bounds = func() [buckets]float64 {
	var b [buckets]float64
	v := firstBound
	for i := range b {
		b[i] = v
		v *= growth
	}
	return b
}()

type Histogram struct {
	counts [buckets]atomic.Int64
	total  atomic.Int64
}

func NewHistogram() *Histogram { return &Histogram{} }

func (h *Histogram) Observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	h.counts[index(float64(d))].Add(1)
	h.total.Add(1)
}

// ObserveGate lets the histogram satisfy the proxy observer interface directly.
func (h *Histogram) ObserveGate(d time.Duration) { h.Observe(d) }

func (h *Histogram) Count() int64 { return h.total.Load() }

// Quantile returns the upper bound of the bucket holding the requested
// quantile, which is an over-estimate by at most one bucket width.
func (h *Histogram) Quantile(q float64) time.Duration {
	total := h.total.Load()
	if total == 0 {
		return 0
	}
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	target := int64(math.Ceil(float64(total) * q))
	if target < 1 {
		target = 1
	}

	var seen int64
	for i := 0; i < buckets; i++ {
		seen += h.counts[i].Load()
		if seen >= target {
			return time.Duration(bounds[i])
		}
	}
	return time.Duration(bounds[buckets-1])
}

func (h *Histogram) Reset() {
	for i := range h.counts {
		h.counts[i].Store(0)
	}
	h.total.Store(0)
}

func index(v float64) int {
	if v <= bounds[0] {
		return 0
	}
	i := int(math.Log(v/firstBound) / math.Log(growth))
	if i < 0 {
		return 0
	}
	if i >= buckets {
		return buckets - 1
	}
	// Log rounding can land one bucket low, so step up when it does.
	if i+1 < buckets && v > bounds[i] {
		i++
	}
	return i
}
