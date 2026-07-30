package limit

import (
	"net/http"
	"testing"
	"time"

	"quecto/internal/store"
)

var base = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// clocked returns a limiter whose clock the test drives by hand.
func clocked() (*Limiter, func(time.Duration)) {
	l := New()
	now := base
	l.now = func() time.Time { return now }
	return l, func(d time.Duration) { now = now.Add(d) }
}

func key(id int64, rpm, tpm *int) *store.APIKey {
	return &store.APIKey{ID: id, Name: "k", RPMLimit: rpm, TPMLimit: tpm}
}

func i(v int) *int { return &v }

func TestUnlimitedKeyAlwaysAdmits(t *testing.T) {
	l, _ := clocked()
	k := key(1, nil, nil)
	for n := 0; n < 1000; n++ {
		res, d := l.Admit(k, 999999)
		if !d.Allowed {
			t.Fatalf("refused request %d on an unlimited key", n)
		}
		res.Settle(999999)
	}
	d := l.Peek(k)
	if d.RPMLimit != nil || d.TPMLimit != nil {
		t.Fatal("an unlimited key reported a limit")
	}
}

func TestRPMSpendsBurstThenRefuses(t *testing.T) {
	l, _ := clocked()
	k := key(1, i(10), nil)

	for n := 0; n < 10; n++ {
		if _, d := l.Admit(k, 0); !d.Allowed {
			t.Fatalf("refused request %d, want the full burst of 10 allowed", n+1)
		}
	}
	_, d := l.Admit(k, 0)
	if d.Allowed {
		t.Fatal("11th request was allowed inside the same minute")
	}
	if d.Reason != "requests" {
		t.Fatalf("reason = %q, want requests", d.Reason)
	}
	if d.RetryAfter <= 0 {
		t.Fatal("no retry hint on a refused request")
	}
	if d.RPMRemaining != 0 {
		t.Fatalf("remaining = %d, want 0", d.RPMRemaining)
	}
}

func TestRPMRefills(t *testing.T) {
	l, advance := clocked()
	k := key(1, i(60), nil)

	for n := 0; n < 60; n++ {
		l.Admit(k, 0)
	}
	if _, d := l.Admit(k, 0); d.Allowed {
		t.Fatal("allowed past the burst")
	}

	// 60 rpm refills one request per second.
	advance(time.Second)
	if _, d := l.Admit(k, 0); !d.Allowed {
		t.Fatal("refused after a second of refill")
	}
	if _, d := l.Admit(k, 0); d.Allowed {
		t.Fatal("a second refill arrived early")
	}
}

func TestTPMReservationIsRefundedOnSettle(t *testing.T) {
	l, _ := clocked()
	k := key(1, nil, i(1000))

	res, d := l.Admit(k, 800)
	if !d.Allowed {
		t.Fatal("first request refused")
	}
	if got := l.Peek(k).TPMRemaining; got != 200 {
		t.Fatalf("remaining after reserving 800 = %d, want 200", got)
	}

	// The request actually used 100, so 700 comes back.
	res.Settle(100)
	if got := l.Peek(k).TPMRemaining; got != 900 {
		t.Fatalf("remaining after settling 100 = %d, want 900", got)
	}
}

func TestSettleIsIdempotent(t *testing.T) {
	l, _ := clocked()
	k := key(1, nil, i(1000))
	res, _ := l.Admit(k, 500)
	res.Settle(0)
	res.Settle(0)
	res.Settle(0)
	if got := l.Peek(k).TPMRemaining; got != 1000 {
		t.Fatalf("remaining = %d, want 1000, repeated settles double counted", got)
	}
}

func TestOversizedRequestOverdrawsAndLocksTheKeyOut(t *testing.T) {
	l, advance := clocked()
	k := key(1, nil, i(10_000))

	res, d := l.Admit(k, 100)
	if !d.Allowed {
		t.Fatal("first request refused")
	}
	// The response turned out to be far larger than reserved.
	res.Settle(40_000)

	peek := l.Peek(k)
	if !peek.TPMExhausted {
		t.Fatalf("balance %d is not reported as exhausted", peek.TPMRemaining)
	}

	_, d = l.Admit(k, 1)
	if d.Allowed {
		t.Fatal("an overdrawn key was admitted")
	}
	if d.Reason != "tokens" {
		t.Fatalf("reason = %q, want tokens", d.Reason)
	}

	// Balance is -30000 refilling at 10000/60 per second, so about 180s to reach 0.
	advance(179 * time.Second)
	if _, d := l.Admit(k, 1); d.Allowed {
		t.Fatal("admitted before the balance recovered")
	}
	advance(3 * time.Second)
	if _, d := l.Admit(k, 1); !d.Allowed {
		t.Fatal("still locked out after the balance should have recovered")
	}
}

func TestRefusalConsumesNothing(t *testing.T) {
	l, _ := clocked()
	k := key(1, i(100), i(1000))

	// Drive the token bucket negative, leaving plenty of request allowance.
	res, _ := l.Admit(k, 0)
	res.Settle(5000)

	before := l.Peek(k).RPMRemaining
	for n := 0; n < 5; n++ {
		if _, d := l.Admit(k, 1); d.Allowed {
			t.Fatal("admitted while the token balance was overdrawn")
		}
	}
	if after := l.Peek(k).RPMRemaining; after != before {
		t.Fatalf("request allowance fell from %d to %d on refused requests", before, after)
	}
}

func TestBucketNeverExceedsItsCeiling(t *testing.T) {
	l, advance := clocked()
	k := key(1, nil, i(1000))

	res, _ := l.Admit(k, 500)
	res.Settle(0)
	advance(10 * time.Minute)
	if got := l.Peek(k).TPMRemaining; got != 1000 {
		t.Fatalf("remaining = %d, want it capped at 1000", got)
	}
}

func TestEditingALimitRebuildsTheBucket(t *testing.T) {
	l, _ := clocked()
	k := key(1, i(5), nil)

	for n := 0; n < 5; n++ {
		l.Admit(k, 0)
	}
	if _, d := l.Admit(k, 0); d.Allowed {
		t.Fatal("allowed past the original burst")
	}

	raised := 50
	k.RPMLimit = &raised
	_, d := l.Admit(k, 0)
	if !d.Allowed {
		t.Fatal("raising the limit did not take effect")
	}
	if *d.RPMLimit != 50 {
		t.Fatalf("reported limit = %d, want 50", *d.RPMLimit)
	}

	k.RPMLimit = nil
	if _, d := l.Admit(k, 0); d.RPMLimit != nil {
		t.Fatal("clearing the limit did not take effect")
	}
}

func TestUsageThisMinuteOutlivesTheRefill(t *testing.T) {
	l, advance := clocked()
	k := key(1, i(60), i(120_000))

	for n := 0; n < 3; n++ {
		res, _ := l.Admit(k, 2048)
		res.Settle(1273)
		advance(time.Second)
	}

	// Both buckets have long since refilled, but the minute's usage stands.
	advance(10 * time.Second)
	d := l.Peek(k)
	if d.RPMUsed != 3 {
		t.Errorf("requests this minute = %d, want 3", d.RPMUsed)
	}
	if d.TPMUsed != 3819 {
		t.Errorf("tokens this minute = %d, want 3819", d.TPMUsed)
	}
	if d.RPMRemaining != 60 || d.TPMRemaining != 120_000 {
		t.Errorf("buckets should have refilled: rpm=%d tpm=%d", d.RPMRemaining, d.TPMRemaining)
	}

	// The window is a minute wide, so it eventually empties on its own.
	advance(60 * time.Second)
	if d := l.Peek(k); d.RPMUsed != 0 || d.TPMUsed != 0 {
		t.Errorf("usage = %d requests %d tokens after a quiet minute, want 0", d.RPMUsed, d.TPMUsed)
	}
}

func TestUsageIsTalliedForUnlimitedKeys(t *testing.T) {
	l, _ := clocked()
	k := key(1, nil, nil)

	res, _ := l.Admit(k, 0)
	res.Settle(500)

	d := l.Peek(k)
	if d.RPMUsed != 1 || d.TPMUsed != 500 {
		t.Fatalf("usage = %d requests %d tokens, want 1 and 500", d.RPMUsed, d.TPMUsed)
	}
}

func TestRefusedRequestsAreNotCountedAsUsage(t *testing.T) {
	l, _ := clocked()
	k := key(1, i(2), nil)

	for n := 0; n < 5; n++ {
		if res, d := l.Admit(k, 0); d.Allowed {
			res.Settle(0)
		}
	}
	if got := l.Peek(k).RPMUsed; got != 2 {
		t.Fatalf("requests this minute = %d, want only the 2 that were admitted", got)
	}
}

func TestSweepDropsIdleKeys(t *testing.T) {
	l, advance := clocked()
	a, b := key(1, i(10), nil), key(2, i(10), nil)
	l.Admit(a, 0)
	l.Admit(b, 0)

	advance(30 * time.Minute)
	l.Admit(b, 0)

	if n := l.Sweep(10 * time.Minute); n != 1 {
		t.Fatalf("swept %d keys, want 1", n)
	}
	if _, ok := l.byKey[2]; !ok {
		t.Fatal("the recently active key was swept")
	}
}

func TestBalancesRoundTripThroughRestore(t *testing.T) {
	l, _ := clocked()
	k := key(1, nil, i(1000))
	res, _ := l.Admit(k, 400)
	res.Settle(400)

	saved := l.Balances()
	if got := saved[1]; got != 600 {
		t.Fatalf("saved balance = %v, want 600", got)
	}

	fresh, _ := clocked()
	fresh.Restore(k, saved[1])
	if got := fresh.Peek(k).TPMRemaining; got != 600 {
		t.Fatalf("restored balance = %d, want 600", got)
	}
}

func TestHeadersUseOpenAINames(t *testing.T) {
	l, _ := clocked()
	k := key(1, i(60), i(1000))
	res, d := l.Admit(k, 100)
	res.Settle(100)

	h := http.Header{}
	d.ApplyHeaders(h)

	want := map[string]string{
		"X-Ratelimit-Limit-Requests":     "60",
		"X-Ratelimit-Remaining-Requests": "59",
		"X-Ratelimit-Limit-Tokens":       "1000",
		"X-Ratelimit-Remaining-Tokens":   "900",
	}
	for name, val := range want {
		if got := h.Get(name); got != val {
			t.Errorf("%s = %q, want %q", name, got, val)
		}
	}
	if h.Get("X-Ratelimit-Reset-Requests") == "" {
		t.Error("reset header for requests is missing")
	}
}

func TestHeadersOmitUnlimitedDimensions(t *testing.T) {
	l, _ := clocked()
	k := key(1, i(60), nil)
	_, d := l.Admit(k, 0)

	h := http.Header{}
	d.ApplyHeaders(h)
	if h.Get("X-Ratelimit-Limit-Tokens") != "" {
		t.Error("token headers were sent for a key with no token limit")
	}
	if h.Get("X-Ratelimit-Limit-Requests") == "" {
		t.Error("request headers are missing")
	}
}

func TestOverdrawnBalanceIsClampedToZeroInHeaders(t *testing.T) {
	l, _ := clocked()
	k := key(1, nil, i(1000))
	res, _ := l.Admit(k, 0)
	res.Settle(5000)

	d := l.Peek(k)
	if d.TPMRemaining >= 0 {
		t.Fatalf("expected a negative balance, got %d", d.TPMRemaining)
	}
	h := http.Header{}
	d.ApplyHeaders(h)
	if got := h.Get("X-Ratelimit-Remaining-Tokens"); got != "0" {
		t.Fatalf("remaining tokens header = %q, want 0 rather than a negative number", got)
	}
}

func TestRetryAfterSecondsRoundsUp(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{0, 1},
		{time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{10 * time.Second, 10},
	}
	for _, c := range cases {
		if got := (Decision{RetryAfter: c.in}).RetryAfterSeconds(); got != c.want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestConcurrentAdmitIsRaceFree(t *testing.T) {
	l := New()
	k := key(1, i(1000), i(100000))
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func() {
			for n := 0; n < 200; n++ {
				res, _ := l.Admit(k, 10)
				res.Settle(5)
				l.Peek(k)
			}
			done <- struct{}{}
		}()
	}
	for g := 0; g < 8; g++ {
		<-done
	}
}

func BenchmarkAdmitBothDimensions(b *testing.B) {
	l := New()
	k := key(1, i(1_000_000), i(1_000_000_000))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, _ := l.Admit(k, 100)
		res.Settle(50)
	}
}

func BenchmarkAdmitUnlimited(b *testing.B) {
	l := New()
	k := key(1, nil, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, _ := l.Admit(k, 100)
		res.Settle(50)
	}
}
