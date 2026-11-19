// Dispatch's own identity at the listener: the projected account token the client reads at each
// call, its failure, and the bearer a call presents (Client.authorize).

package envoy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ErrAccountToken is a call this client did not make because the account token it presents
// (WithAccountTokenFile) cannot be presented: its file cannot be read, holds nothing, or holds a
// token whose own exp has passed or is about to. Nothing reached the listener. It is
// ErrUnavailable too, since the listener could not be asked, and never ErrUnreachable: asking
// again repeats it until the file holds a live token.
var ErrAccountToken = errors.New("the listener account token cannot be presented")

// accountTokenError marks the failure it wraps as ErrAccountToken and ErrUnavailable.
type accountTokenError struct{ error }

func (accountTokenError) Is(target error) bool {
	return target == ErrAccountToken || target == ErrUnavailable
}

func (e accountTokenError) Unwrap() error { return e.error }

// accountTokenExpiryMargin is how long before its own exp a token read from the account token file
// is no longer presented: about a call's whole window (defaultTimeout) and a few seconds of clock
// difference, so a token is never sent to arrive expired. The kubelet rewrites a projected token
// once it is 80% of the way through its life, which for the hour the deployment projects leaves
// twelve minutes, so a working kubelet never brings a token this near.
const accountTokenExpiryMargin = 10 * time.Second

// accountTokenFromFile is the token the account token file holds now, trimmed, when its own exp
// leaves at least accountTokenExpiryMargin. A token whose payload names no exp, or is not a
// compact JWT, is presented as it is, for the listener to judge. Every failure is an
// accountTokenError naming the path and never the token.
func accountTokenFromFile(path string, now time.Time) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", accountTokenError{fmt.Errorf("%w: read %s: %v", ErrAccountToken, path, err)}
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", accountTokenError{fmt.Errorf("%w: %s holds no token", ErrAccountToken, path)}
	}
	if expiry, ok := tokenExpiry(token); ok && !now.Add(accountTokenExpiryMargin).Before(expiry) {
		return "", accountTokenError{fmt.Errorf("%w: the token in %s expires at %s and the file holds no newer one",
			ErrAccountToken, path, expiry.UTC().Format(time.RFC3339))}
	}
	return token, nil
}

// tokenExpiry reads the exp claim of a compact JWT's payload, unverified: it decides only whether
// to present the token, which the listener verifies.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Expiry *float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Expiry == nil {
		return time.Time{}, false
	}
	return time.Unix(int64(*claims.Expiry), 0), true
}

// presentsIdentity reports whether this client presents Dispatch's own identity, the one
// credential the listener takes a send's role mark from.
func (c *Client) presentsIdentity() bool {
	return c.accountTokenFile != ""
}

// authorize sets the bearer a call presents: the account token the file holds now, or the shared
// token, or none; the shared token alone when shared is set (do's fallback). A failure is
// ErrAccountToken, and the call is not made.
func (c *Client) authorize(request *http.Request, shared bool) error {
	if c.presentsIdentity() && !shared {
		token, err := accountTokenFromFile(c.accountTokenFile, time.Now())
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	if c.apiToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
	return nil
}
