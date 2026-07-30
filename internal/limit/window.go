package limit

import "time"

// window is a rolling one minute tally of what a key actually spent, kept in
// per second slots. The buckets refill within a second or two, so they cannot
// answer "how much did this key use this minute" on their own.
//
// Each slot stores the second it holds, which means an idle stretch needs no
// separate reset pass: a stale slot simply falls outside the window.
type window struct {
	sec      [60]int64
	requests [60]int64
	tokens   [60]int64
}

func (w *window) add(now time.Time, requests, tokens int64) {
	s := now.Unix()
	i := int(((s % 60) + 60) % 60)
	if w.sec[i] != s {
		w.sec[i], w.requests[i], w.tokens[i] = s, 0, 0
	}
	w.requests[i] += requests
	w.tokens[i] += tokens
}

func (w *window) sum(now time.Time) (requests, tokens int64) {
	cutoff := now.Unix() - 59
	for i, sec := range w.sec {
		if sec >= cutoff {
			requests += w.requests[i]
			tokens += w.tokens[i]
		}
	}
	return requests, tokens
}
