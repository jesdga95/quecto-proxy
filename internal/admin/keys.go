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
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	KeyPrefix       string `json:"key_prefix"`
	CreatedAt       int64  `json:"created_at"`
	ExpiresAt       *int64 `json:"expires_at"`
	RevokedAt       *int64 `json:"revoked_at"`
	RPMLimit        *int   `json:"rpm_limit"`
	TPMLimit        *int   `json:"tpm_limit"`
	TotalTokenLimit *int64 `json:"total_token_limit"`
	LastUsedAt      *int64 `json:"last_used_at"`
	Status          string `json:"status"`

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
	// The lifetime tally, which the limiter owns while the process runs.
	TotalSpent     int64 `json:"total_spent"`
	TotalRemaining int64 `json:"total_remaining"`
	TotalExhausted bool  `json:"total_exhausted"`
}

func (s *Server) toDTO(k *store.APIKey, now int64) keyDTO {
	status := "active"
	if ok, reason := k.Status(now); !ok {
		status = reason
	}
	d := s.limiter.Peek(k)
	// The limiter sees a quota run out before the usage writer has folded the
	// spend into the row, so it has the last word on whether the key is finished.
	if status == "active" && d.TotalExhausted {
		status = "exhausted"
	}
	return keyDTO{
		ID: k.ID, Name: k.Name, KeyPrefix: k.KeyPrefix,
		CreatedAt: k.CreatedAt, ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt,
		RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit, TotalTokenLimit: k.TotalTokenLimit,
		LastUsedAt:            k.LastUsedAt,
		Status:                status,
		TotalRequests:         k.TotalRequests,
		TotalPromptTokens:     k.TotalPromptTokens,
		TotalCompletionTokens: k.TotalCompletionTokens,
		Live: liveLimits{
			RPMUsed:        d.RPMUsed,
			RPMRemaining:   d.RPMRemaining,
			RPMExhausted:   d.RPMExhausted,
			TPMUsed:        d.TPMUsed,
			TPMRemaining:   d.TPMRemaining,
			TPMExhausted:   d.TPMExhausted,
			TotalSpent:     d.TotalSpent,
			TotalRemaining: d.TotalRemaining,
			TotalExhausted: d.TotalExhausted,
		},
	}
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListKeys()
	if err != nil {
		writeServerError(w, err)
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
	Name            string `json:"name"`
	ExpiresAt       *int64 `json:"expires_at"`
	RPMLimit        *int   `json:"rpm_limit"`
	TPMLimit        *int   `json:"tpm_limit"`
	TotalTokenLimit *int64 `json:"total_token_limit"`
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
	if msg, ok := validLimits(body.RPMLimit, body.TPMLimit, body.TotalTokenLimit); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	raw, hash, display, err := keys.Generate()
	if err != nil {
		writeServerError(w, err)
		return
	}
	k, err := s.st.CreateKey(store.NewKey{
		Name: body.Name, KeyHash: hash, KeyPrefix: display, CreatedAt: now,
		ExpiresAt: body.ExpiresAt, RPMLimit: body.RPMLimit, TPMLimit: body.TPMLimit,
		TotalTokenLimit: body.TotalTokenLimit,
	})
	if err != nil {
		writeServerError(w, err)
		return
	}
	if err := s.registry.Reload(); err != nil {
		writeServerError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"key": s.toDTO(k, now),
		// Only this response and a rotation ever carry the raw key.
		"secret": raw,
	})
}

type updateKeyRequest struct {
	Name            *string `json:"name"`
	ExpiresAt       *int64  `json:"expires_at"`
	RPMLimit        *int    `json:"rpm_limit"`
	TPMLimit        *int    `json:"tpm_limit"`
	TotalTokenLimit *int64  `json:"total_token_limit"`

	ClearExpiry          bool `json:"clear_expiry"`
	ClearRPMLimit        bool `json:"clear_rpm_limit"`
	ClearTPMLimit        bool `json:"clear_tpm_limit"`
	ClearTotalTokenLimit bool `json:"clear_total_token_limit"`
	// Puts a key that spent its quota back to work without raising the limit.
	ResetTokensSpent bool `json:"reset_tokens_spent"`
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
	if body.ClearTotalTokenLimit {
		var none *int64
		u.TotalTokenLimit = &none
	} else if body.TotalTokenLimit != nil {
		u.TotalTokenLimit = &body.TotalTokenLimit
	}
	u.ResetTokensSpent = body.ResetTokensSpent
	if msg, ok := validLimits(body.RPMLimit, body.TPMLimit, body.TotalTokenLimit); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	updated, err := s.st.UpdateKey(k.ID, u)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The limiter owns the live tally, so clearing the column alone would leave
	// the running process still refusing the key.
	if body.ResetTokensSpent {
		s.limiter.ResetSpend(k.ID)
	}
	if err := s.registry.Reload(); err != nil {
		writeServerError(w, err)
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
		writeServerError(w, err)
		return
	}
	fresh, err := s.st.GetKey(k.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toDTO(fresh, s.now().Unix()))
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.lookupPathKey(w, r)
	if err != nil {
		return
	}
	raw, hash, display, err := keys.Generate()
	if err != nil {
		writeServerError(w, err)
		return
	}
	rotated, err := s.st.RotateKey(k.ID, hash, display)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.registry.Reload(); err != nil {
		writeServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key":    s.toDTO(rotated, s.now().Unix()),
		"secret": raw,
	})
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
	// SQLite gives the next key the id this one just freed, so the limiter has to
	// let go of the tally with it or the new key inherits a spent quota.
	s.limiter.Forget(k.ID)
	if err := s.registry.Reload(); err != nil {
		writeServerError(w, err)
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

func validLimits(rpm, tpm *int, total *int64) (string, bool) {
	if rpm != nil && *rpm < 1 {
		return "Requests per minute must be at least 1, or left empty for no limit.", false
	}
	if tpm != nil && *tpm < 1 {
		return "Tokens per minute must be at least 1, or left empty for no limit.", false
	}
	if total != nil && *total < 1 {
		return "The total token limit must be at least 1, or left empty for no limit.", false
	}
	return "", true
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrKeyNotFound) {
		writeError(w, http.StatusNotFound, "That key no longer exists.")
		return
	}
	if errors.Is(err, store.ErrKeyRevoked) {
		writeError(w, http.StatusConflict, "A revoked key cannot be rotated. Create a new one instead.")
		return
	}
	writeServerError(w, err)
}
