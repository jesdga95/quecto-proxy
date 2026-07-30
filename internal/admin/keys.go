package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"quecto/internal/keys"
	"quecto/internal/store"
)

type keyDTO struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	KeyPrefix  string `json:"key_prefix"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  *int64 `json:"expires_at"`
	RevokedAt  *int64 `json:"revoked_at"`
	RPMLimit   *int   `json:"rpm_limit"`
	TPMLimit   *int   `json:"tpm_limit"`
	LastUsedAt *int64 `json:"last_used_at"`
	Status     string `json:"status"`

	TotalRequests         int64 `json:"total_requests"`
	TotalPromptTokens     int64 `json:"total_prompt_tokens"`
	TotalCompletionTokens int64 `json:"total_completion_tokens"`

	Live liveLimits `json:"live"`
}

// liveLimits is what the key spent over the last minute alongside what is left
// in its buckets. Lifetime totals cannot express a per minute allowance, and
// the buckets alone cannot either: they refill within seconds, so a dashboard
// reading only remaining would report an idle key moments after a burst.
type liveLimits struct {
	RPMUsed      int64 `json:"rpm_used"`
	RPMRemaining int   `json:"rpm_remaining"`
	RPMExhausted bool  `json:"rpm_exhausted"`
	TPMUsed      int64 `json:"tpm_used"`
	TPMRemaining int64 `json:"tpm_remaining"`
	TPMExhausted bool  `json:"tpm_exhausted"`
}

func (s *Server) toDTO(k *store.APIKey, now int64) keyDTO {
	status := "active"
	if ok, reason := k.Status(now); !ok {
		status = reason
	}
	d := s.limiter.Peek(k)
	return keyDTO{
		ID: k.ID, Name: k.Name, KeyPrefix: k.KeyPrefix,
		CreatedAt: k.CreatedAt, ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt,
		RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit, LastUsedAt: k.LastUsedAt,
		Status:                status,
		TotalRequests:         k.TotalRequests,
		TotalPromptTokens:     k.TotalPromptTokens,
		TotalCompletionTokens: k.TotalCompletionTokens,
		Live: liveLimits{
			RPMUsed:      d.RPMUsed,
			RPMRemaining: d.RPMRemaining,
			RPMExhausted: d.RPMExhausted,
			TPMUsed:      d.TPMUsed,
			TPMRemaining: d.TPMRemaining,
			TPMExhausted: d.TPMExhausted,
		},
	}
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := s.now().Unix()
	out := make([]keyDTO, 0, len(rows))
	for _, k := range rows {
		out = append(out, s.toDTO(k, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) handleGetKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.lookupPathKey(w, r)
	if err != nil {
		return
	}
	writeJSON(w, http.StatusOK, s.toDTO(k, s.now().Unix()))
}

type createKeyRequest struct {
	Name      string `json:"name"`
	ExpiresAt *int64 `json:"expires_at"`
	RPMLimit  *int   `json:"rpm_limit"`
	TPMLimit  *int   `json:"tpm_limit"`
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body createKeyRequest
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "Give the key a name so you can recognise it later.")
		return
	}
	now := s.now().Unix()
	if body.ExpiresAt != nil && *body.ExpiresAt <= now {
		writeError(w, http.StatusBadRequest, "The expiry date is in the past.")
		return
	}
	if msg, ok := validLimits(body.RPMLimit, body.TPMLimit); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	raw, hash, display, err := keys.Generate()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	k, err := s.st.CreateKey(store.NewKey{
		Name: body.Name, KeyHash: hash, KeyPrefix: display, CreatedAt: now,
		ExpiresAt: body.ExpiresAt, RPMLimit: body.RPMLimit, TPMLimit: body.TPMLimit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.registry.Reload(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"key": s.toDTO(k, now),
		// The only time the raw key leaves the server.
		"secret": raw,
	})
}

type updateKeyRequest struct {
	Name      *string `json:"name"`
	ExpiresAt *int64  `json:"expires_at"`
	RPMLimit  *int    `json:"rpm_limit"`
	TPMLimit  *int    `json:"tpm_limit"`

	ClearExpiry   bool `json:"clear_expiry"`
	ClearRPMLimit bool `json:"clear_rpm_limit"`
	ClearTPMLimit bool `json:"clear_tpm_limit"`
}

func (s *Server) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.lookupPathKey(w, r)
	if err != nil {
		return
	}
	var body updateKeyRequest
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now().Unix()

	u := store.KeyUpdate{}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "The name cannot be empty.")
			return
		}
		u.Name = &name
	}
	if body.ClearExpiry {
		var none *int64
		u.ExpiresAt = &none
	} else if body.ExpiresAt != nil {
		if *body.ExpiresAt <= now {
			writeError(w, http.StatusBadRequest, "The expiry date is in the past.")
			return
		}
		u.ExpiresAt = &body.ExpiresAt
	}
	if body.ClearRPMLimit {
		var none *int
		u.RPMLimit = &none
	} else if body.RPMLimit != nil {
		u.RPMLimit = &body.RPMLimit
	}
	if body.ClearTPMLimit {
		var none *int
		u.TPMLimit = &none
	} else if body.TPMLimit != nil {
		u.TPMLimit = &body.TPMLimit
	}
	if msg, ok := validLimits(body.RPMLimit, body.TPMLimit); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	updated, err := s.st.UpdateKey(k.ID, u)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.registry.Reload(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.toDTO(updated, now))
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.lookupPathKey(w, r)
	if err != nil {
		return
	}
	if err := s.st.RevokeKey(k.ID, s.now().Unix()); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.registry.Reload(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	fresh, err := s.st.GetKey(k.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toDTO(fresh, s.now().Unix()))
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.lookupPathKey(w, r)
	if err != nil {
		return
	}
	if err := s.st.DeleteKey(k.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.registry.Reload(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) lookupPathKey(w http.ResponseWriter, r *http.Request) (*store.APIKey, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "That key id is not valid.")
		return nil, err
	}
	k, err := s.st.GetKey(id)
	if err != nil {
		writeStoreError(w, err)
		return nil, err
	}
	return k, nil
}

func validLimits(rpm, tpm *int) (string, bool) {
	if rpm != nil && *rpm < 1 {
		return "Requests per minute must be at least 1, or left empty for no limit.", false
	}
	if tpm != nil && *tpm < 1 {
		return "Tokens per minute must be at least 1, or left empty for no limit.", false
	}
	return "", true
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrKeyNotFound) {
		writeError(w, http.StatusNotFound, "That key no longer exists.")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
