// Package admin serves the dashboard API: sign in, key management, logs, settings.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"quecto/internal/config"
	"quecto/internal/keys"
	"quecto/internal/limit"
	"quecto/internal/settings"
	"quecto/internal/store"
)

// Health is the runtime state the dashboard reports on.
type Health interface {
	UsageReporting() (known, supported bool)
	UpstreamReachable(ctx context.Context) (bool, error)
	LogsDropped() int64
	OverheadP99() time.Duration
}

// Verifying a password costs 64MB and about 70ms by design. Bounding how many
// run at once stops an unauthenticated flood from exhausting memory.
const (
	defaultVerifySlots = 4
	defaultVerifyWait  = 2 * time.Second
)

type Server struct {
	cfg      *config.Config
	health   Health
	st       *store.Store
	registry *keys.Registry
	limiter  *limit.Limiter
	live     *settings.Live
	now      func() time.Time

	verifySlots chan struct{}
	verifyWait  time.Duration

	// Guards the signing key, which is rebuilt whenever the password changes.
	mu       sync.RWMutex
	secret   string
	pwHash   string
	sessions *sessions
}

func New(cfg *config.Config, st *store.Store, registry *keys.Registry, limiter *limit.Limiter, live *settings.Live, health Health) (*Server, error) {
	pwHash, ok, err := st.Setting(settingPasswordHash)
	if err != nil {
		return nil, err
	}
	if !ok {
		if cfg.AdminPassword == "" {
			return nil, errors.New("no admin password is set. Start once with QUECTO_ADMIN_PASSWORD to create one")
		}
		if pwHash, err = hashPassword(cfg.AdminPassword); err != nil {
			return nil, err
		}
		if err := st.SetSetting(settingPasswordHash, pwHash); err != nil {
			return nil, err
		}
	}

	secret := cfg.SessionSecret
	if secret == "" {
		stored, found, err := st.Setting(settingSessionKey)
		if err != nil {
			return nil, err
		}
		if !found {
			if stored, err = randomSecret(); err != nil {
				return nil, err
			}
			if err := st.SetSetting(settingSessionKey, stored); err != nil {
				return nil, err
			}
		}
		secret = stored
	}

	return &Server{
		cfg: cfg, st: st, registry: registry, limiter: limiter, live: live, health: health, now: time.Now,
		secret: secret, pwHash: pwHash, sessions: newSessions(secret, pwHash),
		verifySlots: make(chan struct{}, defaultVerifySlots),
		verifyWait:  defaultVerifyWait,
	}, nil
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/api/session", s.handleLogin)
	mux.HandleFunc("DELETE /admin/api/session", s.handleLogout)

	mux.Handle("GET /admin/api/me", s.auth(s.handleMe))
	mux.Handle("GET /admin/api/keys", s.auth(s.handleListKeys))
	mux.Handle("POST /admin/api/keys", s.auth(s.handleCreateKey))
	mux.Handle("GET /admin/api/keys/{id}", s.auth(s.handleGetKey))
	mux.Handle("PATCH /admin/api/keys/{id}", s.auth(s.handleUpdateKey))
	mux.Handle("POST /admin/api/keys/{id}/revoke", s.auth(s.handleRevokeKey))
	mux.Handle("DELETE /admin/api/keys/{id}", s.auth(s.handleDeleteKey))
	mux.Handle("GET /admin/api/logs", s.auth(s.handleListLogs))
	mux.Handle("GET /admin/api/stats", s.auth(s.handleStats))
	mux.Handle("GET /admin/api/health", s.auth(s.handleHealth))
	mux.Handle("GET /admin/api/settings", s.auth(s.handleGetSettings))
	mux.Handle("PATCH /admin/api/settings", s.auth(s.handleUpdateSettings))
}

func (s *Server) rotatePassword(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pwHash = hash
	s.sessions = newSessions(s.secret, hash)
}

func (s *Server) session() *sessions {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions
}

func (s *Server) passwordHash() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pwHash
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.session().valid(c.Value, s.now()) {
			writeError(w, http.StatusUnauthorized, "Sign in to continue.")
			return
		}
		next(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	release, ok := s.acquireVerify(r)
	if !ok {
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, "Too many sign in attempts at once. Try again in a moment.")
		return
	}
	valid := verifyPassword(s.passwordHash(), body.Password)
	release()

	if !valid {
		writeError(w, http.StatusUnauthorized, "That password is not correct.")
		return
	}
	s.setSessionCookie(w, r, s.session().issue(s.now()), sessionTTL)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// acquireVerify takes a verification slot, giving up rather than letting
// callers pile up waiting for memory that is already spoken for.
func (s *Server) acquireVerify(r *http.Request) (func(), bool) {
	timer := time.NewTimer(s.verifyWait)
	defer timer.Stop()

	select {
	case s.verifySlots <- struct{}{}:
		return func() { <-s.verifySlots }, true
	case <-timer.C:
		return nil, false
	case <-r.Context().Done():
		return nil, false
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.setSessionCookie(w, r, "", -time.Hour)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"key_count":     s.registry.Len(),
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r, s.cfg.TrustProxyHeaders),
		MaxAge:   int(maxAge.Seconds()),
	})
}

func isHTTPS(r *http.Request, trustProxy bool) bool {
	if r.TLS != nil {
		return true
	}
	return trustProxy && r.Header.Get("X-Forwarded-Proto") == "https"
}

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("the request body could not be read: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
