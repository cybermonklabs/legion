// packages/envoy/internal/cipherclient/device.go

package cipherclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/redact"
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
	// HTTP is the client every call goes through; nil is the client the context carries
	// (oauth2.HTTPClient), else http.DefaultClient.
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
// read as this issuer's document as fixed text, and a request that failed as its class. A
// redirect whose target fails is that failure.
func (d *DeviceFlow) discover(ctx context.Context) (*oauth2.Config, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config != nil {
		return d.config, nil
	}
	answered := &statusOf{}
	provider, err := gooidc.NewProvider(withStatusOf(d.context(ctx), answered), d.Issuer)
	readErr := answered.readErr()
	switch {
	case err != nil && readErr != nil:
		return nil, fmt.Errorf("read %s: %w", d.issuer(), redact.Masked(errors.Join(err, readErr), redact.Class(readErr).Error()))
	case err != nil && answered.status/100 > 2:
		return nil, fmt.Errorf("read %s: %w", d.issuer(), redact.Masked(err, redact.Answered(answered.status, nil)))
	case err != nil && answered.status != 0:
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

// context is ctx carrying the HTTP client x/oauth2 and go-oidc send their requests with.
func (d *DeviceFlow) context(ctx context.Context) context.Context {
	if d.HTTP == nil {
		return ctx
	}
	return context.WithValue(ctx, oauth2.HTTPClient, d.HTTP)
}

// withStatusOf is ctx carrying a copy of the client x/oauth2 and go-oidc send their requests
// with - the one ctx carries (oauth2.HTTPClient), else http.DefaultClient, as every other call
// of a DeviceFlow resolves it - timeout and all, with its transport wrapped so answered records
// the status of each answer it carries and how a read of its body failed. A library whose error
// quotes an answer's body is printed by that status (redact.Answered), or by that read failure's
// class, instead.
func withStatusOf(ctx context.Context, answered *statusOf) context.Context {
	wrapped := *oauth2.NewClient(ctx, nil)
	answered.base = wrapped.Transport
	if answered.base == nil {
		answered.base = http.DefaultTransport
	}
	wrapped.Transport = answered
	return context.WithValue(ctx, oauth2.HTTPClient, &wrapped)
}

// statusOf is a transport that records what became of the last request it carried, which is all a
// refusal may say of an answer whose library error quotes its body: the host it went to, the
// answer's status, or 0 where the request got no answer, so a redirect whose target fails is not
// read as the redirect; and its body, which records how a read of it failed (readErr), so an
// answer the client could not read whole is not read as a document it read and refused, and keeps
// what was read of it (answer) for redact.Answered, which prints a registered code alone. One call
// at a time uses it.
type statusOf struct {
	base   http.RoundTripper
	host   string
	status int
	body   *readOf
}

func (s *statusOf) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := s.base.RoundTrip(request)
	s.host, s.status, s.body = request.URL.Host, 0, nil
	if err == nil {
		s.status = response.StatusCode
		s.body = &readOf{ReadCloser: response.Body}
		response.Body = s.body
	}
	return response, err
}

// readErr is the first error other than io.EOF a read of the last answer's body ended in, and nil
// where that body read whole, or so far, or there was no answer.
func (s *statusOf) readErr() error {
	if s.body == nil {
		return nil
	}
	return s.body.err
}

// answer is what was read of the last answer's body, at most answerKept bytes, for
// redact.Answered alone: it is the server's text, and prints in no other way.
func (s *statusOf) answer() []byte {
	if s.body == nil {
		return nil
	}
	return s.body.kept
}

// answerKept is how much of an answer's body a statusOf keeps: an error body is short, and a
// longer one is no refusal redact.Answered reads.
const answerKept = 64 << 10

// readOf is an answer's body that keeps the first error other than io.EOF a read of it ends in,
// and the first answerKept bytes read of it.
type readOf struct {
	io.ReadCloser
	err  error
	kept []byte
}

func (r *readOf) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if room := answerKept - len(r.kept); room > 0 {
		r.kept = append(r.kept, p[:min(n, room)]...)
	}
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

// Start asks the issuer for a device code and a user code: what the person types in, and where.
func (d *DeviceFlow) Start(ctx context.Context) (*oauth2.DeviceAuthResponse, error) {
	config, err := d.discover(ctx)
	if err != nil {
		return nil, err
	}
	answer, err := config.DeviceAuth(d.context(ctx))
	if err != nil {
		return nil, fmt.Errorf("ask %s for a sign-in code: %w", d.issuer(), redact.Class(err))
	}
	if answer.DeviceCode == "" || answer.UserCode == "" {
		return nil, fmt.Errorf("%s answered no device code to poll or no code for the person to type", d.issuer())
	}
	return answer, nil
}

// Wait polls the issuer until the person has typed the code in, and answers the sign-in that
// leaves. x/oauth2 polls at the interval the issuer asked for and stops when the code expires.
func (d *DeviceFlow) Wait(ctx context.Context, authorization *oauth2.DeviceAuthResponse) (*SignIn, error) {
	config, err := d.discover(ctx)
	if err != nil {
		return nil, err
	}
	token, err := config.DeviceAccessToken(d.context(ctx), authorization)
	if err != nil {
		return nil, fmt.Errorf("wait for the sign-in at %s: %w", d.issuer(), redact.Class(err))
	}
	return d.signIn(token, true)
}

// ErrSignInEnded is a kept refresh token the issuer no longer honours: the person revoked the
// grant at Google, or it expired. It is a sentinel because it is as final as a revoke the
// broker records — no retry and no other caller cures it — so a client drops the token in hand
// and its renewal loop stops on it (finalRefusal), and every reader of a sign-in falls back to
// this machine's own authority on it rather than asking the issuer again at every enrolment
// (SignInAuthority.Current). Refresh returns it, with the command to sign in again
// (FailureSignInRefused), for a refresh the issuer refuses 400 or 401 invalid_grant
// (grantRefused), and no other text of that answer prints.
var ErrSignInEnded = errors.New("the issuer no longer honours this sign-in")

// Refresh mints a fresh ID token from a kept refresh token, with no step by the person. A
// refresh token the issuer no longer honours — the person revoked the grant, or it expired — is
// ErrSignInEnded, a sign-in that is over, which names the command that makes another. Every other
// failure, an issuer that is busy or down among them, is an error that says nothing about the
// sign-in, which a later refresh may cure.
func (d *DeviceFlow) Refresh(ctx context.Context, refreshToken string) (*SignIn, error) {
	config, err := d.discover(ctx)
	if err != nil {
		return nil, err
	}
	token, err := config.TokenSource(d.context(ctx), &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		if grantRefused(err) {
			return nil, WithHint(fmt.Errorf("%s: %w", d.issuer(), ErrSignInEnded), FailureSignInRefused)
		}
		return nil, fmt.Errorf("renew the sign-in at %s: %w", d.issuer(), redact.Class(err))
	}
	// A refresh answers no new refresh token, so the kept one stays the one to keep.
	return d.signIn(token, false)
}

// grantRefused reports whether err is the issuer's token endpoint answering 400 or 401 with the
// OAuth error invalid_grant (RFC 6749 section 5.2): the issuer saying the refresh token is over,
// the rule internal/oidc's CodeFlow.Refresh reads a refresh by. A 429 or a 5xx is the issuer
// being busy or down, and a 403 no refusal of the grant, whatever the body names, so none of them
// ends a sign-in.
func grantRefused(err error) bool {
	var refused *oauth2.RetrieveError
	if !errors.As(err, &refused) || refused.ErrorCode != "invalid_grant" || refused.Response == nil {
		return false
	}
	return refused.Response.StatusCode == http.StatusBadRequest || refused.Response.StatusCode == http.StatusUnauthorized
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
