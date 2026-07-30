package admin

import (
	"net/http"
	"strings"

	"quecto/internal/settings"
)

type settingsResponse struct {
	settings.Values

	// Changing these needs a restart, so the dashboard shows them read only.
	Listen                string `json:"listen"`
	AdminListen           string `json:"admin_listen"`
	DBPath                string `json:"db_path"`
	UpstreamHeaderTimeout string `json:"upstream_header_timeout"`
	TrustProxyHeaders     bool   `json:"trust_proxy_headers"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	v := s.live.Values()
	// Never echo the upstream credential back to the browser.
	if v.UpstreamAPIKey != "" {
		v.UpstreamAPIKey = "********"
	}
	writeJSON(w, http.StatusOK, settingsResponse{
		Values:                v,
		Listen:                s.cfg.Listen,
		AdminListen:           s.cfg.AdminListen,
		DBPath:                s.cfg.DBPath,
		UpstreamHeaderTimeout: s.cfg.UpstreamHeaderTimeout.String(),
		TrustProxyHeaders:     s.cfg.TrustProxyHeaders,
	})
}

type updateSettingsRequest struct {
	settings.Patch
	NewPassword *string `json:"new_password"`
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body updateSettingsRequest
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The masked value means "unchanged", so drop it before applying.
	if body.UpstreamAPIKey != nil && *body.UpstreamAPIKey == "********" {
		body.UpstreamAPIKey = nil
	}
	if err := s.live.Apply(body.Patch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if body.NewPassword != nil {
		pw := strings.TrimSpace(*body.NewPassword)
		if len(pw) < 10 {
			writeError(w, http.StatusBadRequest, "Use a password of at least 10 characters.")
			return
		}
		hash, err := hashPassword(pw)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.st.SetSetting(settingPasswordHash, hash); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.rotatePassword(hash)
		// The old cookie no longer verifies, so hand this browser a fresh one.
		s.setSessionCookie(w, r, s.session().issue(s.now()), sessionTTL)
	}

	s.handleGetSettings(w, r)
}
