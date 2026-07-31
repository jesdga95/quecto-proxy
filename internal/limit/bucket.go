package limit

import "time"

// tokenBucket refills at limit/60 per second and is allowed to go negative.
// A request costing more than the whole allowance overdraws it, and the key
// stays locked out until the balance climbs back above zero.
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

// Reservation holds the tokens set aside for one in-flight request: the reserve
// taken from the per minute bucket, and the part of it the quota is holding,
// which is smaller whenever the reserve ran past what the quota had left.
type Reservation struct {
	limiter  *Limiter
	keyID    int64
	reserved int64
	held     int64
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
	r.limiter.settle(r.keyID, r.reserved, r.held, actual)
}

// settle swaps a reservation for the real token count: the unused part goes back
// to the per minute bucket, the quota releases its hold, and the lifetime tally
// takes the real cost, which is the only thing it ever counts. Both tallies are
// kept for every key, including unlimited ones, so the dashboard can show a rate
// even where nothing is being enforced.
func (l *Limiter) settle(keyID int64, reserved, held, actual int64) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.byKey[keyID]
	if s == nil {
		return
	}
	s.used.add(now, 0, actual)
	s.spent += actual
	// Clamped because a key deleted mid reply hands its id to the next one, whose
	// state never took this hold and must not be handed allowance to release.
	s.held = max(0, s.held-held)

	if s.tpm == nil || s.tpmLimit == nil {
		return
	}
	max := float64(*s.tpmLimit)
	s.tpm.balance += float64(reserved - actual)
	if s.tpm.balance > max {
		s.tpm.balance = max
	}
}
