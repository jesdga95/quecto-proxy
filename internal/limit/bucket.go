package limit

import "time"

// tokenBucket refills at limit/60 per second and is allowed to go negative.
// A request that costs more than the whole allowance leaves the key in debt
// until the bucket climbs back above zero.
type tokenBucket struct {
	balance float64
	updated time.Time
}

func newTokenBucket(max float64, now time.Time) *tokenBucket {
	return &tokenBucket{balance: max, updated: now}
}

func (b *tokenBucket) refill(now time.Time, max float64) {
	if !now.After(b.updated) {
		return
	}
	b.balance += now.Sub(b.updated).Seconds() * (max / 60.0)
	if b.balance > max {
		b.balance = max
	}
	b.updated = now
}

// until reports how long until the balance reaches want.
func (b *tokenBucket) until(want, max float64) time.Duration {
	if b.balance >= want {
		return 0
	}
	perSec := max / 60.0
	if perSec <= 0 {
		return 0
	}
	return time.Duration((want - b.balance) / perSec * float64(time.Second))
}

// Reservation holds the tokens set aside for one in-flight request.
type Reservation struct {
	limiter  *Limiter
	keyID    int64
	reserved int64
	settled  bool
}

// Settle swaps the reservation for the real token count. Calling it twice is a no-op.
func (r *Reservation) Settle(actual int64) {
	if r == nil || r.settled {
		return
	}
	r.settled = true
	if actual < 0 {
		actual = 0
	}
	if refund := r.reserved - actual; refund != 0 {
		r.limiter.credit(r.keyID, float64(refund))
	}
}

// Reserved reports how many tokens were held at admission.
func (r *Reservation) Reserved() int64 {
	if r == nil {
		return 0
	}
	return r.reserved
}

func (l *Limiter) credit(keyID int64, amount float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.byKey[keyID]
	if s == nil || s.tpm == nil || s.tpmLimit == nil {
		return
	}
	max := float64(*s.tpmLimit)
	s.tpm.balance += amount
	if s.tpm.balance > max {
		s.tpm.balance = max
	}
}
