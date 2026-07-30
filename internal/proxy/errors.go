package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"

	"quecto/internal/limit"
)

// apiError renders in the shape OpenAI clients already know how to read.
type apiError struct {
	Status  int
	Message string
	Type    string
	Code    string
}

func (e *apiError) write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": e.Message,
			"type":    e.Type,
			"param":   nil,
			"code":    e.Code,
		},
	})
}

// NotFoundHandler answers paths this port does not serve. Without it the
// router writes plain text, which is the one reply on an OpenAI compatible
// surface a client cannot read a message out of.
//
// It says nothing about what is here. This port faces whoever can reach it,
// and a client that belongs on it already knows the URL it wants.
func NotFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(&apiError{
			Status:  http.StatusNotFound,
			Message: fmt.Sprintf("Invalid URL (%s %s)", r.Method, r.URL.Path),
			Type:    "invalid_request_error",
			Code:    "unknown_url",
		}).write(w)
	})
}

func errMissingKey() *apiError {
	return &apiError{
		Status:  http.StatusUnauthorized,
		Message: "No API key was provided. Send it as a bearer token in the Authorization header.",
		Type:    "invalid_request_error",
		Code:    "missing_api_key",
	}
}

func errInvalidKey() *apiError {
	return &apiError{
		Status:  http.StatusUnauthorized,
		Message: "That API key is not valid.",
		Type:    "invalid_request_error",
		Code:    "invalid_api_key",
	}
}

func errExpiredKey() *apiError {
	return &apiError{
		Status:  http.StatusUnauthorized,
		Message: "That API key has expired.",
		Type:    "invalid_request_error",
		Code:    "expired_api_key",
	}
}

func errRevokedKey() *apiError {
	return &apiError{
		Status:  http.StatusUnauthorized,
		Message: "That API key was revoked.",
		Type:    "invalid_request_error",
		Code:    "revoked_api_key",
	}
}

func errUpstreamUnreachable() *apiError {
	return &apiError{
		Status:  http.StatusBadGateway,
		Message: "The model server could not be reached.",
		Type:    "api_error",
		Code:    "upstream_unavailable",
	}
}

func errBadRequestBody(err error) *apiError {
	return &apiError{
		Status:  http.StatusBadRequest,
		Message: "The request body could not be read.",
		Type:    "invalid_request_error",
		Code:    "invalid_request_body",
	}
}

func errRateLimited(d limit.Decision) *apiError {
	msg := "Rate limit reached for this API key. Try again shortly."
	code := "rate_limit_exceeded"
	switch d.Reason {
	case "requests":
		msg = "Rate limit reached: too many requests per minute for this API key."
		code = "requests_rate_limit_exceeded"
	case "tokens":
		msg = "Rate limit reached: this API key has used its token allowance. It becomes usable again as the allowance refills."
		code = "tokens_rate_limit_exceeded"
	}
	return &apiError{Status: http.StatusTooManyRequests, Message: msg, Type: "rate_limit_error", Code: code}
}
