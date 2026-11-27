// packages/envoy/internal/cipherclient/device.go

package cipherclient

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oauthrefusal"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/redirects"
)

// SignInScopes are the only scopes a person's sign-in asks for: who they are and their address.
// `agent-secrets login` asks for nothing else, so the grant a person gives reaches no mail, no
// drive and no calendar of theirs.
var SignInScopes = []string{"openid", "email"}

// idTokenAlgorithms are the signature algorithms a person's ID token may be signed with, which
// go-jose requires a parser to name. Nothing here verifies a signature — the broker is the
// verifier, and this reads the address claim alone to say whose sign-in a record is — so the
// list is Google's own asymmetric set rather than a judgement of its own.
var idTokenAlgorithms = []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.ES512, jose.PS256, jose.PS384, jose.PS512}

// DeviceFlow is Google's device authorization grant (RFC 8628) as `agent-secrets login` runs it:
// the machine asks for a code, the person types it in on a device of their own, and the machine
// is answered the person's ID token and a refresh token it keeps. It is x/oauth2's own
// DeviceAuth and DeviceAccessToken against the endpoints the issuer's OpenID discovery document
// names, so the same code runs against Google and against a dev stack's issuer.
type DeviceFlow struct {
	// Issuer is the OpenID provider: https://accounts.google.com, or a dev stack's own.
	Issuer string
	// ClientID and ClientSecret are the device-flow client registered at that issuer. Google
	// registers a limited-input device client as confidential, so the secret is sent; it is no
	// secret of the person's, and the broker checks the audience of the token it mints rather
	// than trusting this client.
	ClientID, ClientSecret string
	// HTTP is the client every call goes through, as a copy that follows no redirect (clientOf);
	// nil is the client the context carries (oauth2.HTTPClient), else http.DefaultClient.
	HTTP *http.Client

	mu     sync.Mutex
	config *oauth2.Config
}

// SignIn is what a completed device grant leaves a machine: the person who signed in, the ID
// token of that moment, and the refresh token that mints the next one. `agent-secrets login`
// keeps the refresh token beside the box key, readable by that person's own user alone.
type SignIn struct {
	// Person is the `email` claim of the ID token, the address the broker records the sign-in
	// under.
	Person string
	// IDToken is the person's Google identity token, the subject of the broker's exchange.
	IDToken string
	// RefreshToken mints the next ID token with no step by the person.
	RefreshToken string
}

// discover reads the issuer's discovery document once per DeviceFlow and builds the x/oauth2
// client from it. An issuer that names no device authorization endpoint serves no device grant,
// which is a refusal of its own: a person cannot sign in there with no browser.
//
// go-oidc's own error quotes the issuer, the issuer the document names and the body of an answer
// that is not a success, so none of its text prints. An answer whose body the client could not
// read whole - a connection cut, a deadline passing in the middle of it - prints as that read
// failure's class (redact.Class), whatever its status; go-oidc quotes that failure without
// wrapping it, so it is joined to go-oidc's error and both stay in the chain. A document the
// issuer did not serve prints as the status of the last request (redact.Answered, through
// redact.Masked, so the error still unwraps to go-oidc's), a 2xx answer read whole that does not
// read as this issuer's document as fixed text, and a request that failed as its class. go-oidc
// hands back only the text of its error, so the call is read under a CallScope (redact.OAuth2), as
// every call of go-oidc and x/oauth2 is.
func (d *DeviceFlow) discover(ctx context.Context) (*oauth2.Config, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config != nil {
		return d.config, nil
	}
	answered := &oauthrefusal.Recorder{}
	s := redact.Scope[*gooidc.Provider](oauthrefusal.Record(ctx, d.client(ctx), answered))
	provider, err := redact.OAuth2(s.Read(gooidc.NewProvider(s.Context(), d.Issuer)))
	readErr := answered.ReadErr()
	switch {
	case err != nil && readErr != nil:
		return nil, fmt.Errorf("read %s: %w", d.issuer(), redact.Masked(errors.Join(err, readErr), redact.Class(readErr).Error()))
	case err != nil && answered.Status/100 > 2:
		return nil, fmt.Errorf("read %s: %w", d.issuer(), redact.Masked(err, redact.Answered(answered.Status, nil)))
	case err != nil && answered.Status != 0:
		return nil, fmt.Errorf("read %s: its discovery document names another issuer or is not one", d.issuer())
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", d.issuer(), redact.Class(err))
	}
	endpoint := provider.Endpoint()
	if endpoint.DeviceAuthURL == "" || endpoint.TokenURL == "" {
		return nil, fmt.Errorf("%s names no device_authorization_endpoint or token_endpoint, so it serves no device sign-in", d.issuer())
	}
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	d.config = &oauth2.Config{ClientID: d.ClientID, ClientSecret: d.ClientSecret, Endpoint: endpoint, Scopes: SignInScopes}
	return d.config, nil
}

// issuer is the issuer as a message names it: by its variable, and by its URL only as
// internal/redact's allow-list prints it, whole when it is a plain URL and not at all when it
// carries a user name, a password, a query or anything else a credential can sit in.
func (d *DeviceFlow) issuer() string {
	return "the sign-in issuer (" + SignInIssuerVar + " names " + redact.URL(d.Issuer) + ")"
}

// context is ctx carrying the HTTP client x/oauth2 and go-oidc send their requests with: a
// RefusalClient over d.client, as every client handed to those libraries is (redact.OAuth2).
func (d *DeviceFlow) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, redact.RefusalClient(d.client(ctx)))
}

// client is the client a DeviceFlow's calls go through (clientOf, with HTTP).
func (d *DeviceFlow) client(ctx context.Context) *http.Client {
	return clientOf(ctx, d.HTTP)
}

// clientOf is the client a call of a library over ctx goes through: a copy that follows no
// redirect (redirects.Refused) of explicit, else of the one ctx carries (oauth2.HTTPClient), else
// of http.DefaultClient, as x/oauth2 itself resolves a context's client.
func clientOf(ctx context.Context, explicit *http.Client) *http.Client {
	carried, _ := ctx.Value(oauth2.HTTPClient).(*http.Client)
	return redirects.Refused(cmp.Or(explicit, carried, http.DefaultClient))
}

// Start asks the issuer for a device code and a user code: what the person types in, and where.
func (d *DeviceFlow) Start(ctx context.Context) (*oauth2.DeviceAuthResponse, error) {
	config, err := d.discover(ctx)
	if err != nil {
		return nil, err
	}
	answer, err := redact.OAuth2(config.DeviceAuth(d.context(ctx)))
	if err != nil {
		return nil, fmt.Errorf("ask %s for a sign-in code: %w", d.issuer(), redact.Class(err))
	}
	if answer.DeviceCode == "" || answer.UserCode == "" {
		return nil, fmt.Errorf("%s answered no device code to poll or no code for the person to type", d.issuer())
	}
	return answer, nil
}

// Wait polls the issuer until the person has typed the code in, and answers the sign-in that
// leaves. x/oauth2 polls a copy of authorization held to the device grant's bounds
// (BoundDeviceAuth). A code that lapses no later than its first poll, the issuer's interval on, is
// never polled: x/oauth2's ticker does not fire before the code's end, its context's deadline, so
// Wait answers that deadline (context.DeadlineExceeded), which prints as "timed out", the class
// `agent-secrets login` prints for its own 15-minute limit too.
func (d *DeviceFlow) Wait(ctx context.Context, authorization *oauth2.DeviceAuthResponse) (*SignIn, error) {
	config, err := d.discover(ctx)
	if err != nil {
		return nil, err
	}
	bounded := *authorization
	BoundDeviceAuth(&bounded, time.Now())
	token, err := redact.OAuth2(config.DeviceAccessToken(d.context(ctx), &bounded))
	if err != nil {
		return nil, fmt.Errorf("wait for the sign-in at %s: %w", d.issuer(), redact.Class(err))
	}
	return d.signIn(token, true)
}

// The bounds a device grant's poll holds a server's device authorization to, whatever it names: the
// poll starts at defaultDeviceInterval seconds where the server names no interval, RFC 8628 section
// 3.2's 5 s, and at LeastDeviceInterval seconds at least where it names one, so no poll goes out
// with no wait; and a code is polled for mostDeviceLifetime at most, one that names no lifetime
// included, so no poll goes on past any use (BoundDeviceAuth). The interval is held to that
// lifetime too, MostDeviceInterval, so its seconds always hold in a time.Duration: RFC 8628 section
// 3.5 has a client wait at least the interval the server names, and an interval as long as the
// code's life leaves no poll before the code lapses (the helper's Broker.Login refuses such a code
// at once, and Wait times out on it). Both of this module's device grants keep these bounds: a
// person's sign-in (DeviceFlow.Wait) and the broker helper's machine login, whose refusal of a
// code no poll reaches words its cure from the least and the most interval, so those two are
// exported. defaultDeviceInterval, LeastDeviceInterval and mostDeviceLifetime are the TypeScript
// device login's DEFAULT_POLL_SECONDS, LEAST_POLL_SECONDS and MOST_LIFETIME_SECONDS
// (packages/cipher-client/src/device-login-reach.ts), which needs no most interval of its own.
// x/oauth2's own DeviceAccessToken takes any lifetime, and a positive interval a time.Duration
// holds, as sent.
// It reads any other interval as 5 s in v0.38.0, the version this repository's workspace builds;
// v0.37.0, which this module's go.mod names on its own, reads 0 as 5 s and panics its ticker on a
// negative interval, or on one whose seconds wrap negative as a time.Duration.
const (
	defaultDeviceInterval = 5
	LeastDeviceInterval   = 1
	mostDeviceLifetime    = time.Hour
	MostDeviceInterval    = int64(mostDeviceLifetime / time.Second)
)

// BoundDeviceAuth holds da's interval and lifetime to the device grant's bounds, its lifetime read
// from now, and reports whether the hour, not the server's own lifetime, ends the code: the server
// named none, or one past the hour. The end it writes is now plus what is left, one reading of
// now's monotonic clock where now carries one (time.Now does): x/oauth2 dates the end from
// time.Now().UTC(), which drops that reading, so a comparison with it reads the wall clock while
// a context's deadline and its timer run on the monotonic one, and a step of the wall clock would
// set the two apart. Time.Add drops the reading for an end before the year 1885 too; such a code
// has lapsed on either clock. The monotonic clock does not count a time the machine is suspended
// (CLOCK_MONOTONIC on Linux), so a machine that sleeps past the code's end polls on after it
// resumes, and the server answers expired_token, an expiry too.
//
// The edge values of a device authorization, as x/oauth2 reads them (oauth2.DeviceAuthResponse,
// whole seconds, 0 or null as none) and then the bounds: an interval of 0, none or null is 5 s,
// and a negative one the least; a lifetime of 0 or none is the hour, a negative one lapses the code
// before its first poll, and one past what a time.Duration holds (about 292 years) but inside an
// int64 wraps, as x/oauth2 counts its nanoseconds modulo 2^64: one of about 292 to 584 years to a
// time that has passed, and a longer one to a time that has passed, to one more than an hour on,
// which the hour bounds, or to one under the hour, which is kept as if the server had named it, as
// 18446744974 s wraps to about 900 s; a fraction of either, a count past an int64, or a value sent
// as a string fails the device authorization.
func BoundDeviceAuth(da *oauth2.DeviceAuthResponse, now time.Time) (hourEnds bool) {
	da.Interval = min(max(cmp.Or(da.Interval, defaultDeviceInterval), LeastDeviceInterval), MostDeviceInterval)
	hourEnds = da.Expiry.IsZero() || da.Expiry.Sub(now) > mostDeviceLifetime
	left := mostDeviceLifetime
	if !hourEnds {
		left = da.Expiry.Sub(now)
	}
	da.Expiry = now.Add(left)
	return hourEnds
}

// ErrSignInEnded is a kept refresh token the issuer no longer honours: the person revoked the
// grant at Google, or it expired. It is a sentinel because it is as final as a revoke the
// broker records — no retry and no other caller cures it — so a client drops the token in hand
// and its renewal loop stops on it (finalRefusal), and every reader of a sign-in falls back to
// this machine's own authority on it rather than asking the issuer again at every enrolment
// (SignInAuthority.Current). Refresh returns it, with the command to sign in again
// (FailureSignInRefused), for a refresh the issuer refuses 400 or 401 invalid_grant
// (oauthrefusal.EndsGrant), and no other text of that answer prints.
var ErrSignInEnded = errors.New("the issuer no longer honours this sign-in")

// Refresh mints a fresh ID token from a kept refresh token, with no step by the person. A
// refresh token the issuer no longer honours — the person revoked the grant, or it expired — is
// ErrSignInEnded, a sign-in that is over, which names the command that makes another; the rule is
// oauthrefusal.EndsGrant's, a 400 or 401 invalid_grant. Every other failure, an issuer that is
// busy or down or a 403 among them, is an error that says nothing about the sign-in, which a
// later refresh may cure.
func (d *DeviceFlow) Refresh(ctx context.Context, refreshToken string) (*SignIn, error) {
	config, err := d.discover(ctx)
	if err != nil {
		return nil, err
	}
	token, err := redact.OAuth2(config.TokenSource(d.context(ctx), &oauth2.Token{RefreshToken: refreshToken}).Token())
	if err != nil {
		if oauthrefusal.EndsGrant(err) {
			return nil, WithHint(fmt.Errorf("%s: %w", d.issuer(), ErrSignInEnded), FailureSignInRefused)
		}
		return nil, fmt.Errorf("renew the sign-in at %s: %w", d.issuer(), redact.Class(err))
	}
	// A refresh answers no new refresh token, so the kept one stays the one to keep.
	return d.signIn(token, false)
}

// signIn reads the ID token out of an answer and names the person it was minted for. needRefresh
// is the first grant's rule: a sign-in a machine cannot renew is no sign-in to keep.
func (d *DeviceFlow) signIn(token *oauth2.Token, needRefresh bool) (*SignIn, error) {
	idToken, _ := token.Extra("id_token").(string)
	if idToken == "" {
		return nil, fmt.Errorf("%s answered no id_token: the sign-in asked for the %q scope and must be answered one", d.issuer(), "openid")
	}
	if needRefresh && token.RefreshToken == "" {
		return nil, fmt.Errorf("%s answered no refresh token, so this machine could not renew the sign-in without the person again", d.issuer())
	}
	person, err := PersonOf(idToken)
	if err != nil {
		return nil, err
	}
	return &SignIn{Person: person, IDToken: idToken, RefreshToken: token.RefreshToken}, nil
}

// PersonOf is the `email` claim of an ID token: whose sign-in a record is. It verifies nothing —
// the broker checks the signature, the audience, the domain and email_verified, and refuses a
// token that fails any of them — so what it reads is a label on a local record and never a
// decision. The token is a credential, and go-jose's and encoding/json's errors quote the parts
// of it they could not read, so a token that does not parse is refused in fixed text.
func PersonOf(idToken string) (string, error) {
	parsed, err := jose.ParseSigned(idToken, idTokenAlgorithms)
	if err != nil {
		return "", errors.New("the identity token the sign-in answered is not a signed JWT this client reads")
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(parsed.UnsafePayloadWithoutVerification(), &claims); err != nil {
		return "", errors.New("the identity token the sign-in answered carries claims that are not a JSON object of strings")
	}
	if !strings.Contains(claims.Email, "@") {
		return "", errors.New("the identity token carries no email claim of an address's shape, so it names no person")
	}
	return strings.ToLower(claims.Email), nil
}

// PersonIdentity is a person's own Google identity token as the subject of the broker's
// exchange: their kept refresh token spent for a fresh ID token at every trade, so a sign-in
// that a person or Google has ended stops the machine at the next renewal rather than at the
// next hour.
type PersonIdentity struct {
	// Flow is the device-flow client the sign-in was made with, which mints the next ID token.
	Flow *DeviceFlow
	// RefreshToken is what `agent-secrets login` kept beside the box key.
	RefreshToken string
	// Person is whose sign-in it is, for a refusal to name.
	Person string
}

func (p *PersonIdentity) Token(ctx context.Context) (string, string, error) {
	signIn, err := p.Flow.Refresh(ctx, p.RefreshToken)
	if err != nil {
		return "", "", err
	}
	return signIn.IDToken, oauthwire.SubjectTokenTypePerson, nil
}

func (p *PersonIdentity) Describe() string {
	return p.Person + "'s sign-in on this machine"
}
