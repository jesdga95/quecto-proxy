// Package limit enforces per-key request and token rates entirely in memory.
package limit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"

	"quecto/internal/store"
)

// Decision carries the outcome of an admission check plus everything needed
// to describe the current allowance to the caller.
type Decision struct {
	Allowed bool
	// Set when Allowed is false: "requests" or "tokens".
	Reason     string
	RetryAfter time.Duration

	RPMLimit     *int
	RPMRemaining int
	RPMReset     time.Duration
	// True once the burst is spent, which is when request limited calls are refused.
	RPMExhausted bool

	TPMLimit     *int
	TPMRemaining int64
	TPMReset     time.Duration
	// True once the balance is spent, which is when token limited calls are refused.
	TPMExhausted bool

	// The lifetime allowance, which never refills. TotalSpent is tallied for
	// every key, so it reads even where no quota is enforced.
	TotalLimit     *int64
	TotalSpent     int64
	TotalRemaining int64
	// True once the quota is gone, which makes the key permanently unusable.
	TotalExhausted bool

	// What the key spent over the last minute, which is what the rpm and tpm
	// labels mean to a reader. Set whether or not a limit is configured.
	RPMUsed int64
	TPMUsed int64
}

type keyState struct {
	rpmLimit *int
	rpm      *rate.Limiter

	tpmLimit *int
	tpm      *tokenBucket

	totalLimit *int64
	spent      int64
	// The lifetime tally is seeded from the key row once and then owned here, so
	// a registry reload part way through a batch cannot rewind it.
	seeded bool

	used     window
	lastSeen time.Time
}

type Limiter struct {
	mu    sync.Mutex
	byKey map[int64]*keyState
	now   func() time.Time
}

func New() *Limiter {
	return &Limiter{byKey: make(map[int64]*keyState), now: time.Now}
}

// state returns the per-key buckets, rebuilding any whose limit was edited.
// touch marks the key as active; reads from the dashboard leave it alone so
// they cannot keep idle buckets alive forever.
func (l *Limiter) state(k *store.APIKey, now time.Time, touch bool) *keyState {
	s := l.byKey[k.ID]
	if s == nil {
		s = &keyState{}
		l.byKey[k.ID] = s
	}
	if !samePtr(s.rpmLimit, k.RPMLimit) {
		s.rpmLimit = copyInt(k.RPMLimit)
		if k.RPMLimit == nil {
			s.rpm = nil
		} else {
			n := *k.RPMLimit
			s.rpm = rate.NewLimiter(rate.Limit(float64(n)/60.0), n)
		}
	}
	if !samePtr(s.tpmLimit, k.TPMLimit) {
		s.tpmLimit = copyInt(k.TPMLimit)
		if k.TPMLimit == nil {
			s.tpm = nil
		} else {
			s.tpm = newTokenBucket(float64(*k.TPMLimit), now)
		}
	}
	// The quota has no bucket to rebuild, so an edited limit is simply the new
	// ceiling the existing tally is measured against.
	if !s.seeded {
		s.spent, s.seeded = k.TokensSpent, true
	}
	s.totalLimit = copyInt64(k.TotalTokenLimit)
	if touch {
		s.lastSeen = now
	}
	return s
}

// Admit consumes one request from the RPM bucket and reserves tokens against
// the TPM bucket. A refused request consumes nothing.
func (l *Limiter) Admit(k *store.APIKey, reserve int64) (*Reservation, Decision) {
	now := l.now()
	if reserve < 0 {
		reserve = 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state(k, now, true)

	d := Decision{Allowed: true, RPMLimit: s.rpmLimit, TPMLimit: s.tpmLimit, TotalLimit: s.totalLimit}

	// The quota goes first because it is the one refusal that never clears on its
	// own, so there is no point reporting a retry hint from a refilling bucket.
	if s.totalLimit != nil && s.spent >= *s.totalLimit {
		d.Allowed = false
		d.Reason = "quota"
		l.fill(&d, s, now)
		return nil, d
	}

	// Check the token bucket before spending a request, so a refusal is free.
	if s.tpm != nil {
		s.tpm.refill(now, float64(*s.tpmLimit))
		if s.tpm.balance <= 0 {
			d.Allowed = false
			d.Reason = "tokens"
			d.RetryAfter = s.tpm.until(1, float64(*s.tpmLimit))
			l.fill(&d, s, now)
			return nil, d
		}
	}

	if s.rpm != nil && !s.rpm.AllowN(now, 1) {
		d.Allowed = false
		d.Reason = "requests"
		d.RetryAfter = untilOneToken(s.rpm, now)
		l.fill(&d, s, now)
		return nil, d
	}

	s.used.add(now, 1, 0)

	// Only the refilling bucket holds the reserve, because only it gets the
	// unused part back within seconds. Charging max_tokens against a quota that
	// never refills would spend the whole allowance the moment a long reply
	// started, and the key would read exhausted for as long as it ran.
	if s.tpm != nil {
		s.tpm.balance -= float64(reserve)
	}
	res := &Reservation{limiter: l, keyID: k.ID, reserved: reserve}

	l.fill(&d, s, now)
	return res, d
}

// Peek reports the current allowance without consuming anything.
func (l *Limiter) Peek(k *store.APIKey) Decision {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state(k, now, false)
	d := Decision{Allowed: true, RPMLimit: s.rpmLimit, TPMLimit: s.tpmLimit, TotalLimit: s.totalLimit}
	l.fill(&d, s, now)
	return d
}

func (l *Limiter) fill(d *Decision, s *keyState, now time.Time) {
	d.RPMUsed, d.TPMUsed = s.used.sum(now)
	d.TotalSpent = s.spent
	if s.totalLimit != nil {
		d.TotalRemaining = max(0, *s.totalLimit-s.spent)
		d.TotalExhausted = s.spent >= *s.totalLimit
	}
	if s.rpm != nil {
		avail := s.rpm.TokensAt(now)
		if avail < 0 {
			avail = 0
		}
		d.RPMRemaining = int(avail)
		// Admission needs a whole token, so any fraction is already a refusal.
		d.RPMExhausted = avail < 1
		d.RPMReset = untilOneToken(s.rpm, now)
	}
	if s.tpm != nil {
		max := float64(*s.tpmLimit)
		s.tpm.refill(now, max)
		d.TPMRemaining = int64(s.tpm.balance)
		// A balance of exactly zero is refused too, so it counts as exhausted.
		d.TPMExhausted = s.tpm.balance <= 0
		d.TPMReset = s.tpm.until(1, max)
	}
}

// Sweep drops buckets for keys that have been idle, keeping memory flat. State
// carrying a quota stays put: the per minute buckets refill on their own so
// losing them costs nothing, but a lifetime tally rebuilt from a key row the
// registry has not reloaded would hand back allowance that was already spent.
func (l *Limiter) Sweep(idleFor time.Duration) int {
	cutoff := l.now().Add(-idleFor)
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for id, s := range l.byKey {
		if s.totalLimit != nil {
			continue
		}
		if s.lastSeen.Before(cutoff) {
			delete(l.byKey, id)
			n++
		}
	}
	return n
}

// ResetSpend clears the lifetime tally for one key, which is what revives a key
// that spent its quota. The database column is cleared by the same admin write.
func (l *Limiter) ResetSpend(keyID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.byKey[keyID]; s != nil {
		s.spent = 0
	}
}

// Forget drops everything held for a key, and must be called when one is
// deleted. SQLite hands the next key the id the deleted one gave up, so state
// left behind here would be inherited by a key that never spent it.
func (l *Limiter) Forget(keyID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byKey, keyID)
}

// Balances returns the current token balance per key, for checkpointing.
func (l *Limiter) Balances() map[int64]float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[int64]float64, len(l.byKey))
	for id, s := range l.byKey {
		if s.tpm != nil {
			out[id] = s.tpm.balance
		}
	}
	return out
}

// Restore seeds token balances saved by a previous run.
func (l *Limiter) Restore(k *store.APIKey, balance float64) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state(k, now, true)
	if s.tpm != nil {
		s.tpm.balance = balance
		s.tpm.updated = now
	}
}

func untilOneToken(r *rate.Limiter, now time.Time) time.Duration {
	avail := r.TokensAt(now)
	if avail >= 1 {
		return 0
	}
	perSec := float64(r.Limit())
	if perSec <= 0 {
		return 0
	}
	return time.Duration((1 - avail) / perSec * float64(time.Second))
}

func samePtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func copyInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyInt64(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
