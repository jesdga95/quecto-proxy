// Package keys generates and verifies API keys, and caches them in memory.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
)

const (
	Prefix    = "qp_"
	bodyLen   = 40
	alphabet  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	maxUnbias = 248 // largest multiple of 62 below 256, keeps the mapping uniform
)

// Generate returns a new key and the values to persist for it.
func Generate() (raw string, hash []byte, display string, err error) {
	body := make([]byte, 0, bodyLen)
	buf := make([]byte, bodyLen)
	for len(body) < bodyLen {
		if _, err := rand.Read(buf); err != nil {
			return "", nil, "", fmt.Errorf("generate key: %w", err)
		}
		for _, b := range buf {
			if b >= maxUnbias {
				continue
			}
			body = append(body, alphabet[int(b)%len(alphabet)])
			if len(body) == bodyLen {
				break
			}
		}
	}
	raw = Prefix + string(body)
	sum := Hash(raw)
	return raw, sum[:], Display(raw), nil
}

func Hash(raw string) [32]byte {
	return sha256.Sum256([]byte(raw))
}

// Display masks a key for the dashboard, keeping enough on each end to identify it.
func Display(raw string) string {
	body := strings.TrimPrefix(raw, Prefix)
	if len(body) < 8 {
		return Prefix + "..."
	}
	return Prefix + body[:4] + "..." + body[len(body)-4:]
}

// FromAuthHeader pulls a bearer token out of an Authorization header value.
func FromAuthHeader(h string) (string, bool) {
	const bearer = "bearer "
	if len(h) <= len(bearer) || !strings.EqualFold(h[:len(bearer)], bearer) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(bearer):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// Equal compares two hashes without leaking timing information.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
