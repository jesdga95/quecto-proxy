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

// quotaKey carries a lifetime allowance and whatever the key row says it has
// already spent, which is what a fresh process seeds itself from.
func quotaKey(id int64, total, spent int64) *store.APIKey {
	return &store.APIKey{ID: id, Name: "k", TotalTokenLimit: &total, TokensSpent: spent}
}

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

func TestRPMExhaustedFollowsAdmission(t *testing.T) {
	l, advance := clocked()
	k := key(1, i(2), nil)

	if l.Peek(k).RPMExhausted {
		t.Fatal("a fresh key reports its request allowance as spent")
	}
	for n := 0; n < 2; n++ {
		if _, d := l.Admit(k, 0); !d.Allowed {
			t.Fatalf("refused request %d of the burst", n+1)
		}
	}
	if !l.Peek(k).RPMExhausted {
		t.Fatal("the burst is spent but the key is not reported as exhausted")
	}
	if _, d := l.Admit(k, 0); d.Allowed {
		t.Fatal("admitted past the burst")
	}

	// Refilling at 2/60 per second, so a whole token is back after 30s.
	advance(30 * time.Second)
	if l.Peek(k).RPMExhausted {
		t.Fatal("still reported as exhausted once a token refilled")
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

func TestQuotaKillsTheKeyOnceSpent(t *testing.T) {
	l, advance := clocked()
	k := quotaKey(1, 1000, 0)

	res, d := l.Admit(k, 0)
	if !d.Allowed {
		t.Fatal("first request refused")
	}
	res.Settle(1000)

	peek := l.Peek(k)
	if !peek.TotalExhausted {
		t.Fatalf("spent %d of 1000 is not reported as exhausted", peek.TotalSpent)
	}
	if peek.TotalRemaining != 0 {
		t.Fatalf("remaining = %d, want 0", peek.TotalRemaining)
	}

	_, d = l.Admit(k, 1)
	if d.Allowed {
		t.Fatal("admitted a key that had spent its quota")
	}
	if d.Reason != "quota" {
		t.Fatalf("reason = %q, want quota", d.Reason)
	}
	// Nothing refills, so a retry hint would be a lie no matter how long we wait.
	if d.RetryAfter != 0 {
		t.Fatalf("retry hint = %v on a quota that never refills", d.RetryAfter)
	}
	advance(24 * time.Hour)
	if _, d := l.Admit(k, 1); d.Allowed {
		t.Fatal("a day of waiting revived a spent quota")
	}
}

func TestQuotaOutranksTheRefillingRails(t *testing.T) {
	l, _ := clocked()
	k := quotaKey(1, 100, 100)
	k.RPMLimit, k.TPMLimit = i(1), i(1)

	// Every allowance is gone, so the reason has to be the one that is terminal.
	res, _ := l.Admit(k, 0)
	if res != nil {
		res.Settle(10_000)
	}
	_, d := l.Admit(k, 1)
	if d.Reason != "quota" {
		t.Fatalf("reason = %q, want quota to outrank the per minute rails", d.Reason)
	}
}

func TestQuotaSeedsFromTheKeyRow(t *testing.T) {
	l, _ := clocked()

	// A restart picks up a key the usage writer had already taken to its limit.
	k := quotaKey(1, 5000, 5000)
	if _, d := l.Admit(k, 1); d.Allowed {
		t.Fatal("a fresh limiter handed back a quota the key row says is spent")
	}

	// And one with room left carries on from where it was.
	k2 := quotaKey(2, 5000, 4000)
	res, d := l.Admit(k2, 0)
	if !d.Allowed {
		t.Fatal("refused a key with 1000 tokens left")
	}
	if d.TotalRemaining != 1000 {
		t.Fatalf("remaining = %d, want 1000 carried over from the row", d.TotalRemaining)
	}
	res.Settle(1000)
	if _, d := l.Admit(k2, 1); d.Allowed {
		t.Fatal("admitted once the carried over remainder was spent")
	}
}

func TestQuotaTallyIsNotRewoundByAStaleRow(t *testing.T) {
	l, _ := clocked()
	k := quotaKey(1, 1000, 0)

	res, _ := l.Admit(k, 0)
	res.Settle(900)

	// The registry reloads rows while a batch is still in the writer's buffer,
	// so the column can lag well behind what the limiter has already counted.
	k.TokensSpent = 0
	if got := l.Peek(k).TotalSpent; got != 900 {
		t.Fatalf("spent = %d, want 900, a stale row rewound the tally", got)
	}
	if got := l.Peek(k).TotalRemaining; got != 100 {
		t.Fatalf("remaining = %d, want only the 100 that was left", got)
	}
}

// The last call through is admitted on whatever is left and allowed to overshoot,
// because the reserve is a ceiling rather than a price. Refusing on the reserve
// would strand the tail of every quota behind whatever max_tokens happened to be.
func TestTheLastCallOverdrawsTheQuotaAndEndsIt(t *testing.T) {
	l, _ := clocked()
	k := quotaKey(1, 1000, 900)

	res, d := l.Admit(k, 5000)
	if !d.Allowed {
		t.Fatal("refused while 100 tokens of quota were still unspent")
	}
	res.Settle(4000)

	peek := l.Peek(k)
	if peek.TotalSpent != 4900 {
		t.Fatalf("spent = %d, want the real 4900 even though it overshot", peek.TotalSpent)
	}
	if !peek.TotalExhausted || peek.TotalRemaining != 0 {
		t.Fatalf("an overdrawn quota reads exhausted=%v remaining=%d", peek.TotalExhausted, peek.TotalRemaining)
	}
	if _, d := l.Admit(k, 1); d.Allowed || d.Reason != "quota" {
		t.Fatalf("after overdrawing: allowed=%v reason=%q", d.Allowed, d.Reason)
	}
}

// The quota counts settled tokens only. max_tokens is a ceiling a caller can set
// far above what a reply costs, and holding it against an allowance that never
// refills would spend the lot the moment a long reply started: the key would read
// exhausted for as long as it ran and refuse everything else terminally.
func TestQuotaCountsWhatWasSpentNotWhatWasReserved(t *testing.T) {
	l, _ := clocked()
	k := quotaKey(1, 3000, 0)

	res, d := l.Admit(k, 30_000)
	if !d.Allowed {
		t.Fatal("first request refused")
	}
	if d.TotalSpent != 0 {
		t.Fatalf("spent while the reply is still running = %d, want 0", d.TotalSpent)
	}
	if peek := l.Peek(k); peek.TotalExhausted || peek.TotalRemaining != 3000 {
		t.Fatalf("a key mid reply reads exhausted=%v remaining=%d", peek.TotalExhausted, peek.TotalRemaining)
	}
	// A second call arriving while the first is still streaming is admitted, since
	// nothing has been spent yet.
	if _, d := l.Admit(k, 30_000); !d.Allowed {
		t.Fatal("a concurrent call was refused against a quota nothing had touched")
	}

	res.Settle(2844)
	peek := l.Peek(k)
	if peek.TotalSpent != 2844 {
		t.Fatalf("spent = %d, want the real cost of 2844", peek.TotalSpent)
	}
	if peek.TotalRemaining != 156 {
		t.Fatalf("remaining = %d, want 156", peek.TotalRemaining)
	}
}

// The per minute bucket keeps its reserve, because it gets the unused part back
// within seconds and it is what stops calls in flight oversubscribing a rate.
func TestTPMStillHoldsTheReserveWhileTheQuotaDoesNot(t *testing.T) {
	l, _ := clocked()
	k := quotaKey(1, 100_000, 0)
	k.TPMLimit = i(1000)

	res, d := l.Admit(k, 800)
	if !d.Allowed {
		t.Fatal("first request refused")
	}
	if d.TPMRemaining != 200 {
		t.Fatalf("tpm remaining while 800 is in flight = %d, want 200", d.TPMRemaining)
	}
	if d.TotalSpent != 0 {
		t.Fatalf("quota spent while 800 is in flight = %d, want 0", d.TotalSpent)
	}

	res.Settle(100)
	peek := l.Peek(k)
	if peek.TPMRemaining != 900 {
		t.Fatalf("tpm remaining after settling 100 = %d, want 900", peek.TPMRemaining)
	}
	if peek.TotalSpent != 100 {
		t.Fatalf("quota spent after settling 100 = %d, want 100", peek.TotalSpent)
	}
}

func TestQuotaIsTalliedWithoutALimit(t *testing.T) {
	l, _ := clocked()
	k := key(1, nil, nil)

	res, _ := l.Admit(k, 2048)
	res.Settle(700)

	d := l.Peek(k)
	if d.TotalLimit != nil {
		t.Fatal("a key with no quota reported one")
	}
	if d.TotalSpent != 700 {
		t.Fatalf("spent = %d, want the real cost of 700 tallied anyway", d.TotalSpent)
	}
	if d.TotalExhausted {
		t.Fatal("a key with no quota was reported as exhausted")
	}
}

func TestRaisingTheQuotaRevivesTheKey(t *testing.T) {
	l, _ := clocked()
	k := quotaKey(1, 1000, 1000)
	if _, d := l.Admit(k, 1); d.Allowed {
		t.Fatal("admitted on a spent quota")
	}

	raised := int64(2000)
	k.TotalTokenLimit = &raised
	_, d := l.Admit(k, 0)
	if !d.Allowed {
		t.Fatal("raising the quota did not take effect")
	}
	if d.TotalRemaining != 1000 {
		t.Fatalf("remaining after raising to 2000 = %d, want 1000", d.TotalRemaining)
	}

	k.TotalTokenLimit = nil
	if _, d := l.Admit(k, 0); d.TotalLimit != nil {
		t.Fatal("clearing the quota did not take effect")
	}
}

func TestResetSpendRevivesTheKey(t *testing.T) {
	l, _ := clocked()
	k := quotaKey(1, 1000, 1000)
	if _, d := l.Admit(k, 1); d.Allowed {
		t.Fatal("admitted on a spent quota")
	}

	l.ResetSpend(k.ID)
	_, d := l.Admit(k, 0)
	if !d.Allowed {
		t.Fatal("the key is still refused after its tally was reset")
	}
	if d.TotalRemaining != 1000 {
		t.Fatalf("remaining after the reset = %d, want the whole quota back", d.TotalRemaining)
	}
}

func TestSweepKeepsKeysCarryingAQuota(t *testing.T) {
	l, advance := clocked()
	k := quotaKey(1, 1000, 0)
	res, _ := l.Admit(k, 0)
	res.Settle(900)

	advance(30 * time.Minute)
	if n := l.Sweep(10 * time.Minute); n != 0 {
		t.Fatalf("swept %d keys, want the quota tally left alone", n)
	}
	if got := l.Peek(k).TotalSpent; got != 900 {
		t.Fatalf("spent = %d after a sweep, want 900", got)
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
