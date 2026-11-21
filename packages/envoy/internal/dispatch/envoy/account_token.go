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

// ErrNoListenerIdentity is a send for a role this client refused because Dispatch has no identity
// to present to the listener, which takes a role mark from Dispatch's own identity alone: neither
// WithAccountTokenFile nor WithGoogleIdentity is set. A send for a role is never made unmarked,
// since the session that takes the role from its holder would not receive it, so it is refused,
// loudly, and the delivery records this error, which names the setting that gives Dispatch an
// identity. It is not ErrUnavailable: no try repeats it until an operator sets that identity. A
// machine with WithGoogleIdentity set and no Google metadata server is refused at Dispatch's start
// instead (CheckGoogleIdentity), so a metadata server that does not answer at a call is a failure
// of that call (ErrAccountToken), never this.
var ErrNoListenerIdentity = errors.New("envoy: this Dispatch presents no identity to the Envoy listener, which takes a send's role mark from Dispatch's own identity alone; set ENVOY_ACCOUNT_TOKEN_FILE to Dispatch's service-account token for the listener's audience, or on Google Cloud ENVOY_ACCOUNT_GOOGLE_AUDIENCE to the listener's ENVOY_OIDC_GOOGLE_AUDIENCE (packages/envoy/deploy/README.md, \"Dispatch's identity at the Envoy listener\")")

// CheckGoogleIdentity is Dispatch's start-time check of ENVOY_ACCOUNT_GOOGLE_AUDIENCE: the machine
// must have a Google metadata server to mint its identity token (googleidentity.Probe). A machine
// with none is no Google machine, and Dispatch refuses to start rather than refuse every role
// route's send later; the error names the setting. A metadata server that answers with a fault is
// a start like any other: each call's mint is judged on its own (googleIdentity.tokenAt).
func CheckGoogleIdentity(ctx context.Context) error {
	if _, err := googleidentity.Probe(ctx); errors.Is(err, googleidentity.ErrNoMetadataServer) {
		return fmt.Errorf("ENVOY_ACCOUNT_GOOGLE_AUDIENCE gives Dispatch its Google Cloud machine's identity at the Envoy listener, and this machine has no Google metadata server to mint it; set ENVOY_ACCOUNT_TOKEN_FILE off Google Cloud (packages/envoy/deploy/README.md, \"Dispatch's identity at the Envoy listener\"): %w", err)
	}
	return nil
}

// listenerIdentity is Dispatch's own identity at the listener, the one credential the listener
// takes a send's role mark from: the projected token a file holds (tokenFile, WithAccountTokenFile)
// or the Google Cloud machine's identity token (googleIdentity, WithGoogleIdentity). token is the
// identity token to present at now; forget drops a token the listener refused (401), so the next
// call presents a fresh one.
type listenerIdentity interface {
	token(ctx context.Context, now time.Time) (string, error)
	forget()
}

// tokenFile is Dispatch's projected service-account token, read from its file at each call
// (accountTokenFromFile). It caches nothing, so forget does nothing: the next read is the file's.
type tokenFile string

func (f tokenFile) token(_ context.Context, now time.Time) (string, error) {
	return accountTokenFromFile(string(f), now)
}

func (tokenFile) forget() {}

// googleIdentity is Dispatch's Google Cloud identity (WithGoogleIdentity): the audience its
// identity token is minted for, and the token the metadata server last minted, kept until
// accountTokenExpiryMargin before its own exp, so a call does not ask the metadata server again
// for a token it holds. mint is the mint in flight, which every caller that finds no live token
// waits on, each on its own context, so a slow metadata server holds no caller past its own
// deadline and is asked once for all of them.
type googleIdentity struct {
	audience string
	mu       sync.Mutex
	held     string
	expires  time.Time
	mint     *googleMint
}

// googleMint is one mint of a Google identity token: done closes once token and err are set.
type googleMint struct {
	done  chan struct{}
	token string
	err   error
}

// token is the identity token to present at now: the one held while it lives, else one the
// metadata server mints for the audience (googleidentity.Token), shared with every caller that
// asks while it is in flight. A mint that failed, whatever failed - a metadata server that did not
// answer for a moment, one that answered with a fault - is ErrAccountToken: the call is not made
// on this identity, never made unmarked, and a later call, or the delivery's next try, asks again.
// A caller whose context ends while it waits gets its context's error as ErrAccountToken, and the
// mint goes on for the others. Whether the machine has a metadata server at all was decided at
// Dispatch's start (CheckGoogleIdentity).
func (g *googleIdentity) token(ctx context.Context, now time.Time) (string, error) {
	g.mu.Lock()
	if g.held != "" && now.Add(accountTokenExpiryMargin).Before(g.expires) {
		held := g.held
		g.mu.Unlock()
		return held, nil
	}
	mint := g.mint
	if mint == nil {
		mint = &googleMint{done: make(chan struct{})}
		g.mint = mint
		go g.run(mint)
	}
	g.mu.Unlock()
	select {
	case <-mint.done:
		return mint.token, mint.err
	case <-ctx.Done():
		return "", accountTokenError{fmt.Errorf("%w: wait for a Google identity token for %s: %v", ErrAccountToken, g.audience, ctx.Err())}
	}
}

// googleMintTimeout bounds one mint, which no one caller's context bounds since it is shared.
const googleMintTimeout = 10 * time.Second

// run mints a token for mint, keeps it when it lives long enough, and ends the mint.
func (g *googleIdentity) run(mint *googleMint) {
	ctx, cancel := context.WithTimeout(context.Background(), googleMintTimeout)
	defer cancel()
	token, err := googleidentity.Token(ctx, g.audience)
	var expires time.Time
	if err != nil {
		err = accountTokenError{fmt.Errorf("%w: mint a Google identity token for %s: %v", ErrAccountToken, g.audience, err)}
	} else if exp, ok := tokenExpiry(token); !ok || !time.Now().Add(accountTokenExpiryMargin).Before(exp) {
		token, err = "", accountTokenError{fmt.Errorf("%w: the metadata server minted a Google identity token for %s that names no exp far enough ahead", ErrAccountToken, g.audience)}
	} else {
		expires = exp
	}
	g.mu.Lock()
	if err == nil {
		g.held, g.expires = token, expires
	}
	g.mint = nil
	g.mu.Unlock()
	mint.token, mint.err = token, err
	close(mint.done)
}

// forget drops the held token, which the listener refused, so the next call mints a fresh one.
func (g *googleIdentity) forget() {
	g.mu.Lock()
	g.held, g.expires = "", time.Time{}
	g.mu.Unlock()
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
// credential the listener takes a send's role mark from (listenerIdentity).
func (c *Client) presentsIdentity() bool {
	return c.identity != nil
}

// authorize sets the bearer a call presents: Dispatch's identity token (listenerIdentity), or none
// for a client with no identity. A failure is ErrAccountToken, and the call is not made.
func (c *Client) authorize(request *http.Request) error {
	if c.identity == nil {
		return nil
	}
	token, err := c.identity.token(request.Context(), time.Now())
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return nil
}
