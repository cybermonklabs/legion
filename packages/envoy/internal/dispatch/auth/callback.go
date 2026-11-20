package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/redact"
)

// CallbackError describes the human-readable form of an authorization callback failure, the
// issuer's `?error=` and `?error_description=`. `access_denied` is the common one (the person
// cancelled). Both are the issuer's words, held as redact.RemoteText, which prints only the rule's
// reading of them, under every verb, in JSON and in a log field: a URL in them prints with no user
// information and no query or fragment value, and a field the rule cannot read prints as
// redact.WordsNotReadable. A caller that tells callbacks apart compares the code with HasCode.
type CallbackError struct {
	code        redact.RemoteText
	description redact.RemoteText
}

func (e *CallbackError) Error() string {
	if !e.description.IsEmpty() {
		return fmt.Sprintf("sign-in callback %s: %s", e.code, e.description)
	}
	return "sign-in callback: " + e.code.String()
}

// Format prints Error under the verb, flags, width and precision it is given, as fmt prints that
// string, so no verb prints this error's fields.
func (e *CallbackError) Format(f fmt.State, verb rune) {
	fmt.Fprintf(f, fmt.FormatString(f, verb), e.Error())
}

// LogValue is Error, so a structured log field names the error only as a message may.
func (e *CallbackError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// HasCode reports whether the issuer's `?error=` is code, compared as it came. It answers no text.
func (e *CallbackError) HasCode(code string) bool { return e.code.Equal(code) }

// ParseCallback extracts the code and state (or the error and its description) from an
// `/auth/callback?...` query string. It returns a CallbackError for an issuer-side error and a
// plain error for a malformed callback.
func ParseCallback(req *http.Request) (code, state string, err error) {
	q := req.URL.Query()
	if errCode := q.Get("error"); errCode != "" {
		return "", "", &CallbackError{code: redact.NewRemoteText(errCode), description: redact.NewRemoteText(q.Get("error_description"))}
	}
	code = strings.TrimSpace(q.Get("code"))
	state = strings.TrimSpace(q.Get("state"))
	if code == "" || state == "" {
		return "", "", errors.New("missing code or state in callback")
	}
	return code, state, nil
}
