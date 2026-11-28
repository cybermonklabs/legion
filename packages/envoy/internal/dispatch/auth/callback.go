package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/oidc"
)

// CallbackError describes the human-readable form of an authorization callback failure, the
// issuer's `?error=` and `?error_description=`. `access_denied` is the common one (the person
// cancelled).
type CallbackError struct {
	Code        string
	Description string
}

func (e *CallbackError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("sign-in callback %s: %s", e.Code, e.Description)
	}
	return "sign-in callback: " + e.Code
}

// ParseCallback extracts the code and state (or the error and its description) from an
// `/auth/callback?...` query string. It returns a CallbackError for an issuer-side error and a
// plain error for a malformed callback, one whose code is longer than oidc.MaxCallbackCode
// included.
func ParseCallback(req *http.Request) (code, state string, err error) {
	q := req.URL.Query()
	if errCode := q.Get("error"); errCode != "" {
		return "", "", &CallbackError{Code: errCode, Description: q.Get("error_description")}
	}
	code = strings.TrimSpace(q.Get("code"))
	state = strings.TrimSpace(q.Get("state"))
	if code == "" || state == "" {
		return "", "", errors.New("missing code or state in callback")
	}
	if len(code) > oidc.MaxCallbackCode {
		return "", "", fmt.Errorf("the callback's code is longer than %d bytes", oidc.MaxCallbackCode)
	}
	return code, state, nil
}
