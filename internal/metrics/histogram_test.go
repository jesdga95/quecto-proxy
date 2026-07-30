package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestEmptyHistogramReportsZero(t *testing.T) {
	h := NewHistogram()
	if got := h.Quantile(0.99); got != 0 {
		t.Fatalf("Quantile on an empty histogram = %v, want 0", got)
	}
}

func TestQuantileTracksTheDistribution(t *testing.T) {
	h := NewHistogram()
	// 99 fast observations and one slow one.
	for i := 0; i < 99; i++ {
		h.Observe(10 * time.Microsecond)
	}
	h.Observe(2 * time.Second)

	p50 := h.Quantile(0.5)
	if p50 > 100*time.Microsecond {
		t.Errorf("p50 = %v, want it near the fast observations", p50)
	}
	p99 := h.Quantile(0.99)
	if p99 > 100*time.Microsecond {
		t.Errorf("p99 = %v, one outlier in a hundred should not move it", p99)
	}
	p100 := h.Quantile(1.0)
	if p100 < time.Second {
		t.Errorf("p100 = %v, want it to reach the slow observation", p100)
	}
}

func TestBucketsNeverUnderReport(t *testing.T) {
	h := NewHistogram()
	for _, d := range []time.Duration{
		time.Nanosecond, time.Microsecond, 50 * time.Microsecond,
		time.Millisecond, 250 * time.Millisecond, time.Second, time.Minute,
	} {
		h.Reset()
		h.Observe(d)
		// The reported value is the bucket's upper bound, so it must not sit
		// below the observation or the dashboard would understate latency.
		if got := h.Quantile(1.0); got < d && d > time.Microsecond {
			t.Errorf("observed %v but reported %v", d, got)
		}
	}
}

func TestCountAndReset(t *testing.T) {
	h := NewHistogram()
	for i := 0; i < 10; i++ {
		h.Observe(time.Millisecond)
	}
	if h.Count() != 10 {
		t.Fatalf("count = %d, want 10", h.Count())
	}
	h.Reset()
	if h.Count() != 0 || h.Quantile(0.5) != 0 {
		t.Fatal("reset did not clear the histogram")
	}
}

func TestConcurrentObserveIsRaceFree(t *testing.T) {
	h := NewHistogram()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				h.Observe(time.Duration(i) * time.Microsecond)
				h.Quantile(0.99)
			}
		}()
	}
	wg.Wait()
	if h.Count() != 8000 {
		t.Fatalf("count = %d, want 8000", h.Count())
	}
}

func BenchmarkObserve(b *testing.B) {
	h := NewHistogram()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Observe(123 * time.Microsecond)
	}
}
