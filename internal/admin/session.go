package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	settingPasswordHash = "admin_password_hash"
	settingSessionKey   = "session_secret"
	sessionCookie       = "quecto_session"
	sessionTTL          = 14 * 24 * time.Hour
)

var errBadPassword = errors.New("incorrect password")

type argonParams struct {
	time    uint32
	memory  uint32
	threads uint8
	keyLen  uint32
}

var defaultArgon = argonParams{time: 3, memory: 64 * 1024, threads: 2, keyLen: 32}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	p := defaultArgon
	sum := argon2.IDKey([]byte(pw), salt, p.time, p.memory, p.threads, p.keyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum),
	), nil
}

func verifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	p.keyLen = uint32(len(want))
	got := argon2.IDKey([]byte(pw), salt, p.time, p.memory, p.threads, p.keyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// sessions issues signed cookies. The signing key folds in the password hash,
// so changing the password invalidates every existing session.
type sessions struct {
	key []byte
}

func newSessions(secret, passwordHash string) *sessions {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(passwordHash))
	return &sessions{key: mac.Sum(nil)}
}

func (s *sessions) issue(now time.Time) string {
	exp := strconv.FormatInt(now.Add(sessionTTL).Unix(), 10)
	return exp + "." + s.sign(exp)
}

func (s *sessions) valid(token string, now time.Time) bool {
	exp, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	if !hmac.Equal([]byte(sig), []byte(s.sign(exp))) {
		return false
	}
	ts, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return false
	}
	return now.Unix() < ts
}

func (s *sessions) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}
