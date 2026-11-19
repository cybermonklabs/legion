// Dispatch's own identity at the listener: the projected account token the client reads at each
// call, or its Google Cloud machine's identity token; their failures; the refusal of a send for a
// role where Dispatch presents neither; and the bearer a call presents (Client.authorize).

package envoy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/googleidentity"
)

// ErrAccountToken is a call this client did not make because the identity it presents cannot be
// presented now: the account token file (WithAccountTokenFile) cannot be read, holds nothing, or
// holds a token whose own exp has passed or is about to, or the metadata server that answers for
// this machine (WithGoogleIdentity) failed to mint a token. Nothing reached the listener. It is
// ErrUnavailable too, since the listener could not be asked, and never ErrUnreachable: asking
// again repeats it until the file holds a live token or the metadata server mints one.
var ErrAccountToken = errors.New("the listener account token cannot be presented")

// accountTokenError marks the failure it wraps as ErrAccountToken and ErrUnavailable.
type accountTokenError struct{ error }

func (accountTokenError) Is(target error) bool {
	return target == ErrAccountToken || target == ErrUnavailable
}

func (e accountTokenError) Unwrap() error { return e.error }

// ErrNoIdentity is a send for a role this client refused because Dispatch has no identity to
// present to the listener, which takes a role mark from Dispatch's own identity alone: neither
// WithAccountTokenFile nor WithGoogleIdentity is set, or WithGoogleIdentity is set on a machine no
// Google metadata server answers for. A send for a role is never made unmarked, since the session
// that takes the role from its holder would not receive it, so it is refused, loudly, and the
// delivery records this error, which names the setting that gives Dispatch an identity. It is not
// ErrUnavailable: no try repeats it until an operator sets that identity.
var ErrNoIdentity = errors.New("envoy: this Dispatch presents no identity to the Envoy listener, which takes a send's role mark from Dispatch's own identity alone; set ENVOY_ACCOUNT_TOKEN_FILE to Dispatch's service-account token for the listener's audience, or on Google Cloud ENVOY_ACCOUNT_GOOGLE_AUDIENCE to the listener's ENVOY_OIDC_GOOGLE_AUDIENCE (packages/envoy/deploy/README.md, \"Dispatch's identity at the Envoy listener\")")

// googleIdentity is Dispatch's Google Cloud identity (WithGoogleIdentity): the audience its
// identity token is minted for, and the token the metadata server last minted, kept until
// accountTokenExpiryMargin before its own exp, so a call does not ask the metadata server again
// for a token it holds.
type googleIdentity struct {
	audience string
	mu       sync.Mutex
	token    string
	expires  time.Time
}

// tokenAt is the identity token to present at now: the one held while it lives, else one the
// metadata server mints for the audience (googleidentity.Token). A mint that failed on a machine
// no Google metadata server answers for (googleidentity.Probe's ErrNoMetadataServer) is no Google
// identity at all, which fails closed: ErrNoIdentity, so a send for a role is refused and never
// made unmarked. Any other failure of the mint is ErrAccountToken, which a later call asks again;
// either way the call is not made on this identity.
func (g *googleIdentity) tokenAt(ctx context.Context, now time.Time) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && now.Add(accountTokenExpiryMargin).Before(g.expires) {
		return g.token, nil
	}
	token, err := googleidentity.Token(ctx, g.audience)
	if err != nil {
		if _, probeErr := googleidentity.Probe(ctx); errors.Is(probeErr, googleidentity.ErrNoMetadataServer) {
			return "", fmt.Errorf("%w: %w", ErrNoIdentity, probeErr)
		}
		return "", accountTokenError{fmt.Errorf("%w: mint a Google identity token for %s: %v", ErrAccountToken, g.audience, err)}
	}
	expires, ok := tokenExpiry(token)
	if !ok || !now.Add(accountTokenExpiryMargin).Before(expires) {
		return "", accountTokenError{fmt.Errorf("%w: the metadata server minted a Google identity token for %s that names no exp far enough ahead", ErrAccountToken, g.audience)}
	}
	g.token, g.expires = token, expires
	return token, nil
}

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
// credential the listener takes a send's role mark from: the account token file's, or the Google
// Cloud machine's.
func (c *Client) presentsIdentity() bool {
	return c.accountTokenFile != "" || c.google != nil
}

// authorize sets the bearer a call presents: Dispatch's identity token - the one the account token
// file holds now, or the one the metadata server minted for the Google audience - or the shared
// token, or none; the shared token alone when shared is set (do's fallback). A failure is
// ErrAccountToken, or ErrNoIdentity where no Google metadata server answers, and the call is not
// made.
func (c *Client) authorize(request *http.Request, shared bool) error {
	if c.presentsIdentity() && !shared {
		var token string
		var err error
		if c.google != nil {
			token, err = c.google.tokenAt(request.Context(), time.Now())
		} else {
			token, err = accountTokenFromFile(c.accountTokenFile, time.Now())
		}
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
