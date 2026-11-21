package oidc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	flowClientID     = "dispatch-client"
	flowClientSecret = "dispatch-client-secret"
	flowRedirect     = "https://dispatch.example/auth/callback"
	flowUsername     = "ExampleIdP_a.b+c@d.example"
	flowDomain       = "example.com"
)

// newCodeFlowFor starts a code-flow issuer for flowClientID and a code flow for it in group mode,
// the Cognito sign-in pool's.
func newCodeFlowFor(t *testing.T) (*oidctest.Issuer, *CodeFlow) {
	t.Helper()
	return newCodeFlowForDomain(t, "")
}

// newCodeFlowForDomain starts a code-flow issuer for flowClientID and a code flow for it for the
// hosted domain, empty for group mode. The flow discovers the issuer on its first call.
func newCodeFlowForDomain(t *testing.T, domain string) (*oidctest.Issuer, *CodeFlow) {
	t.Helper()
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), flowClientID, flowClientSecret)
	return issuer, NewCodeFlow(issuer.URL(), flowClientID, flowClientSecret, domain)
}

// authURL is flow's authorization URL for flowRedirect, failing the test when there is none.
func authURL(t *testing.T, flow *CodeFlow, state, nonce string) string {
	t.Helper()
	location, err := flow.AuthURL(context.Background(), flowRedirect, state, nonce)
	if err != nil {
		t.Fatalf("AuthURL: %v", err)
	}
	return location
}

// googleClaims is a Google ID token's claims for an account of the Workspace domain flowDomain,
// with emailVerified as its email_verified claim.
func googleClaims(emailVerified any) map[string]any {
	return map[string]any{
		"sub":            "110169484474386276334",
		"email":          "A.Person@example.com",
		"email_verified": emailVerified,
		"hd":             flowDomain,
	}
}

// exchange runs one sign-in through flow as the person claims names, and returns its session.
func exchange(t *testing.T, issuer *oidctest.Issuer, flow *CodeFlow, claims map[string]any) Session {
	t.Helper()
	issuer.SignInAs(claims)
	code, _ := issuer.Authorize(t, authURL(t, flow, "state-1", "nonce-1"))
	session, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	return session
}

// In domain mode the session carries the Google ID token's email, hd and sub, and email_verified
// as true only for the JSON true or the string "true": any other value or shape is false, never an
// error, so a token Google shapes differently is refused by the rule rather than failing the read.
func TestCodeFlowInDomainModeReadsTheWorkspaceClaims(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  bool
	}{
		{"true", true, true},
		{`"true"`, "true", true},
		{`"false"`, "false", false},
		{"false", false, false},
		{"a number", 1, false},
		{"null", nil, false},
		{"an object", map[string]any{"value": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			session := exchange(t, issuer, flow, googleClaims(tc.value))
			claims := session.Claims
			if claims.Email != "A.Person@example.com" || claims.HostedDomain != flowDomain || claims.Subject != "110169484474386276334" || claims.EmailVerified != tc.want {
				t.Errorf("claims = %#v, want the email, hd and sub read, and email_verified %t", claims, tc.want)
			}
		})
	}
	t.Run("an email and hd that are not strings", func(t *testing.T) {
		issuer, flow := newCodeFlowForDomain(t, flowDomain)
		claims := googleClaims(true)
		claims["email"], claims["hd"] = 7, []string{flowDomain}
		if session := exchange(t, issuer, flow, claims); session.Claims.Email != "" || session.Claims.HostedDomain != "" {
			t.Errorf("claims = %#v, want no email and no hd", session.Claims)
		}
	})
	t.Run("no email_verified, email or hd", func(t *testing.T) {
		issuer, flow := newCodeFlowForDomain(t, flowDomain)
		session := exchange(t, issuer, flow, map[string]any{"sub": "110169484474386276334"})
		if session.Claims.Email != "" || session.Claims.HostedDomain != "" || session.Claims.EmailVerified {
			t.Errorf("claims = %#v, want none of the Workspace claims", session.Claims)
		}
	})
}

// A refresh in domain mode reads the claims as Google holds them now, so the hourly confirmation
// applies the domain rule to the refreshed token.
func TestCodeFlowRefreshInDomainModeReadsTheCurrentWorkspaceClaims(t *testing.T) {
	issuer, flow := newCodeFlowForDomain(t, flowDomain)
	session := exchange(t, issuer, flow, googleClaims(true))
	issuer.UpdateGrants(func(claims map[string]any) {
		claims["hd"] = "other.example"
		claims["email_verified"] = "false"
	})
	refreshed, err := flow.Refresh(context.Background(), session.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.Claims.HostedDomain != "other.example" || refreshed.Claims.EmailVerified || refreshed.Claims.Email != "A.Person@example.com" {
		t.Errorf("refreshed claims = %#v, want hd other.example and email_verified false", refreshed.Claims)
	}
}

// Group mode reads none of the Workspace claims, whatever the token carries: a Cognito ID token's
// email is one the person can write.
func TestCodeFlowInGroupModeReadsNoWorkspaceClaims(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	claims := personClaims("dispatch-members")
	claims["email_verified"], claims["hd"] = true, flowDomain
	session := exchange(t, issuer, flow, claims)
	if session.Claims.Email != "" || session.Claims.EmailVerified || session.Claims.HostedDomain != "" {
		t.Errorf("claims = %#v, want no Workspace claims in group mode", session.Claims)
	}
	if session.Claims.Username != flowUsername || !reflect.DeepEqual(session.Claims.Groups, []string{"dispatch-members"}) {
		t.Errorf("claims = %#v, want the Cognito username and groups", session.Claims)
	}
}

// In domain mode the authorization request asks Google for offline access, so the exchange returns
// a refresh token, with consent each time, so a second sign-in returns one too, and names the
// domain as a hint; group mode sends none of the three.
func TestCodeFlowAuthURLAsksGoogleForOfflineAccessInTheDomain(t *testing.T) {
	_, domainFlow := newCodeFlowForDomain(t, flowDomain)
	_, groupFlow := newCodeFlowFor(t)
	query := func(flow *CodeFlow) url.Values {
		location, err := url.Parse(authURL(t, flow, "state-1", "nonce-1"))
		if err != nil {
			t.Fatal(err)
		}
		return location.Query()
	}
	domain := query(domainFlow)
	for name, want := range map[string]string{"access_type": "offline", "prompt": "consent", "hd": flowDomain, "client_id": flowClientID, "nonce": "nonce-1", "scope": "openid email profile"} {
		if got := domain.Get(name); got != want {
			t.Errorf("domain mode: %s = %q, want %q", name, got, want)
		}
	}
	group := query(groupFlow)
	for _, name := range []string{"access_type", "prompt", "hd"} {
		if group.Has(name) {
			t.Errorf("group mode: the authorization request carries %s=%q", name, group.Get(name))
		}
	}
}

// personClaims is a Cognito ID token's claims for a person signed in through the Google provider:
// its email claim is one the person could have written, unlike its username.
func personClaims(groups ...string) map[string]any {
	return map[string]any{
		"sub":              "f3c2a7e0-person",
		"cognito:username": flowUsername,
		"cognito:groups":   groups,
		"email":            "someone-else@d.example",
		"identities":       []map[string]any{{"providerName": "ExampleIdP", "providerType": "SAML", "primary": "true"}},
	}
}

func TestCodeFlowExchangesACodeForTheSignedInPerson(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.SignInAs(personClaims("dispatch-members", "other-group"))

	location := authURL(t, flow, "state-1", "nonce-1")
	code, state := issuer.Authorize(t, location)
	if state != "state-1" {
		t.Fatalf("authorization returned state %q, want state-1", state)
	}
	session, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if session.Claims.Username != flowUsername {
		t.Errorf("Username = %q, want %q", session.Claims.Username, flowUsername)
	}
	if want := []string{"dispatch-members", "other-group"}; !reflect.DeepEqual(session.Claims.Groups, want) {
		t.Errorf("Groups = %q, want %q", session.Claims.Groups, want)
	}
	if want := []string{"ExampleIdP"}; !reflect.DeepEqual(session.Claims.Providers, want) {
		t.Errorf("Providers = %q, want %q", session.Claims.Providers, want)
	}
	if session.Claims.Subject != "f3c2a7e0-person" || session.Claims.Issuer != issuer.URL() {
		t.Errorf("claims = %#v, want the person's subject from %s", session.Claims, issuer.URL())
	}
	if session.RefreshToken == "" {
		t.Fatal("Exchange returned no refresh token")
	}
}

func TestCodeFlowAuthURLAsksForTheCodeWithTheClientsScopes(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	location := authURL(t, flow, "state-1", "nonce-1")
	for _, want := range []string{
		issuer.URL() + "/authorize?",
		"client_id=" + flowClientID,
		"response_type=code",
		"scope=openid+email+profile",
		"state=state-1",
		"nonce=nonce-1",
		"redirect_uri=https%3A%2F%2Fdispatch.example%2Fauth%2Fcallback",
	} {
		if !strings.Contains(location, want) {
			t.Errorf("AuthURL %q does not carry %q", location, want)
		}
	}
}

// The nonce is what binds the ID token to the browser that started this sign-in: a code another
// sign-in minted, replayed into this callback, carries that sign-in's nonce.
func TestCodeFlowExchangeRefusesAnIDTokenForAnotherNonce(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.SignInAs(personClaims("dispatch-members"))
	code, _ := issuer.Authorize(t, authURL(t, flow, "state-1", "nonce-of-another-sign-in"))
	if _, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1"); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("Exchange with another sign-in's nonce: err = %v, want a nonce refusal", err)
	}
}

func TestCodeFlowExchangeRefusesACodeTheIssuerRefuses(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.SignInAs(personClaims("dispatch-members"))
	code, _ := issuer.Authorize(t, authURL(t, flow, "state-1", "nonce-1"))
	if _, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1"); err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	if _, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1"); err == nil {
		t.Fatal("a code exchanged twice was accepted the second time")
	}
}

// A code exchange's answer decides by its status alone (oauthrefusal.Outage). A 5xx, a 408 or a 429,
// whatever error its body names, and no answer at all are an issuer that could not answer, and
// match ErrIssuerUnavailable in both modes, so a caller answers the person as it answers an issuer
// it cannot reach. Every other answer is the token endpoint answering, and fails the exchange as
// that answer: a 4xx with or without an OAuth error, a 2xx that names an error or holds no token,
// and a 3xx.
func TestCodeFlowExchangeReadsOnlyAnOutageAsTheIssuerUnavailable(t *testing.T) {
	for _, c := range []struct {
		name        string
		fail        func(issuer *oidctest.Issuer)
		unavailable bool
	}{
		{"400 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusBadRequest, "invalid_grant") }, false},
		{"401 invalid_client", func(i *oidctest.Issuer) { i.FailToken(http.StatusUnauthorized, "invalid_client") }, false},
		{"403 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusForbidden, "invalid_grant") }, false},
		{"403 unauthorized_client", func(i *oidctest.Issuer) { i.FailToken(http.StatusForbidden, "unauthorized_client") }, false},
		{"400 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusBadRequest, "") }, false},
		{"401 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusUnauthorized, "") }, false},
		{"403 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusForbidden, "") }, false},
		{"404 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusNotFound, "") }, false},
		{"200 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusOK, "invalid_grant") }, false},
		{"200 holding no token", func(i *oidctest.Issuer) { i.FailToken(http.StatusOK, "") }, false},
		{"302 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusFound, "") }, false},
		{"408 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusRequestTimeout, "invalid_grant") }, true},
		{"503 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusServiceUnavailable, "") }, true},
		{"503 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusServiceUnavailable, "invalid_grant") }, true},
		{"500 invalid_client", func(i *oidctest.Issuer) { i.FailToken(http.StatusInternalServerError, "invalid_client") }, true},
		{"429 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusTooManyRequests, "invalid_grant") }, true},
		{"no answer", func(i *oidctest.Issuer) { i.DropToken() }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			c.fail(issuer)
			_, err := flow.Exchange(context.Background(), flowRedirect, "a-code", "a-nonce")
			if err == nil || errors.Is(err, ErrIssuerUnavailable) != c.unavailable {
				t.Fatalf("Exchange against a token endpoint answering %s: err = %v, want an error matching ErrIssuerUnavailable: %t", c.name, err, c.unavailable)
			}
			_, err = flow.OnlineExchange(context.Background(), flowRedirect, "a-code", onlineSignIn("a-nonce"))
			if err == nil || errors.Is(err, ErrIssuerUnavailable) != c.unavailable {
				t.Fatalf("OnlineExchange against a token endpoint answering %s: err = %v, want an error matching ErrIssuerUnavailable: %t", c.name, err, c.unavailable)
			}
		})
	}
}

// A code exchange whose own context ends before the token endpoint answers - the browser left the
// callback - fails with that context's error, not ErrIssuerUnavailable: the issuer was not shown
// unable to answer, so a caller writes no outage for it. The flow has discovered its issuer, so the
// context ends only the token request.
func TestCodeFlowExchangeTheCallerLeftIsNoIssuerOutage(t *testing.T) {
	issuer, flow := newCodeFlowForDomain(t, flowDomain)
	if err := flow.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := flow.Exchange(ctx, flowRedirect, "a-code", "a-nonce")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("Exchange whose context ended: err = %v, want context.Canceled and not ErrIssuerUnavailable", err)
	}
	_, err = flow.OnlineExchange(ctx, flowRedirect, "a-code", onlineSignIn("a-nonce"))
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("OnlineExchange whose context ended: err = %v, want context.Canceled and not ErrIssuerUnavailable", err)
	}
	if got := issuer.TokenRequests(); len(got) != 0 {
		t.Fatalf("the token endpoint received %q, want nothing: the context had ended", got)
	}
}

// leaveAfterAnswer is a transport that ends its caller's context once the token endpoint's answer,
// or the failure to get one, is in hand: a browser that leaves the callback just as the issuer
// answers. It reads the answer's body first, so the exchange still reads the answer whole.
type leaveAfterAnswer struct {
	base  http.RoundTripper
	leave context.CancelFunc
}

func (l leaveAfterAnswer) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := l.base.RoundTrip(request)
	if response != nil {
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			l.leave()
			return nil, readErr
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
	}
	l.leave()
	return response, err
}

// A code exchange reads the caller as gone only when its failure's cause is the caller's own
// context ending before any answer came. An issuer that answered an outage, or failed with no
// answer, just as the caller left is still an issuer that could not answer, and so is one that
// outlasted the token request's own timeout, whose error also matches context.DeadlineExceeded
// though the caller is still there.
func TestCodeFlowExchangeReadsTheCallerGoneOnlyFromItsOwnContext(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(issuer *oidctest.Issuer)
		// leave ends the caller's context once the answer is in hand; otherwise the flow's token
		// request times out first.
		leave bool
	}{
		{"a 503 answered as the caller leaves", func(i *oidctest.Issuer) { i.FailToken(http.StatusServiceUnavailable, "invalid_grant") }, true},
		{"no answer as the caller leaves", func(i *oidctest.Issuer) { i.DropToken() }, true},
		{"an answer past the token request's timeout", func(i *oidctest.Issuer) { i.DelayToken(5 * time.Second) }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			if err := flow.Discover(context.Background()); err != nil {
				t.Fatalf("Discover: %v", err)
			}
			c.fail(issuer)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := *flow.http
			if c.leave {
				client.Transport = leaveAfterAnswer{base: http.DefaultTransport, leave: cancel}
			} else {
				client.Timeout = 200 * time.Millisecond
			}
			flow.http = &client
			_, err := flow.Exchange(ctx, flowRedirect, "a-code", "a-nonce")
			if !errors.Is(err, ErrIssuerUnavailable) || errors.Is(err, context.Canceled) {
				t.Fatalf("Exchange against %s: err = %v, want ErrIssuerUnavailable and not context.Canceled", c.name, err)
			}
		})
	}
}

func TestCodeFlowRefreshReadsThePersonsCurrentClaims(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.SignInAs(personClaims("dispatch-members"))
	code, _ := issuer.Authorize(t, authURL(t, flow, "state-1", "nonce-1"))
	session, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	refreshed, err := flow.Refresh(context.Background(), session.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.Claims.Username != flowUsername || !reflect.DeepEqual(refreshed.Claims.Groups, []string{"dispatch-members"}) {
		t.Errorf("refreshed claims = %#v, want the same person in dispatch-members", refreshed.Claims)
	}
	// Cognito answers a refresh without a new refresh token; the one that worked stays the one to use.
	if refreshed.RefreshToken != session.RefreshToken {
		t.Errorf("refreshed refresh token = %q, want the original %q", refreshed.RefreshToken, session.RefreshToken)
	}

	issuer.UpdateGrants(func(claims map[string]any) { claims["cognito:groups"] = []string{"other-group"} })
	removed, err := flow.Refresh(context.Background(), session.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh after the group change: %v", err)
	}
	if !reflect.DeepEqual(removed.Claims.Groups, []string{"other-group"}) {
		t.Errorf("Groups after removal = %q, want [other-group]", removed.Claims.Groups)
	}

	issuer.RevokeGrants()
	if _, err := flow.Refresh(context.Background(), session.RefreshToken); err == nil {
		t.Fatal("Refresh with a revoked refresh token succeeded")
	}
}

// A refresh fails as ErrGrantRefused only when the token endpoint answers 400 or 401 with the OAuth
// error invalid_grant (RFC 6749 section 5.2), the issuer saying the sign-in is over. Any other
// failure says nothing about the sign-in and matches ErrIssuerUnavailable instead: a Google outage
// must not read as the person's grant being withdrawn.
func TestCodeFlowRefreshClassifiesItsFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fail    func(issuer *oidctest.Issuer)
		refused bool
	}{
		{"a revoked refresh token", func(issuer *oidctest.Issuer) { issuer.RevokeGrants() }, true},
		{"400 invalid_grant", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusBadRequest, "invalid_grant") }, true},
		{"401 invalid_grant", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusUnauthorized, "invalid_grant") }, true},
		// A 403 naming invalid_grant is a refusal, but not one that ends a grant (oauthrefusal.EndsGrant).
		{"403 invalid_grant", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusForbidden, "invalid_grant") }, false},
		{"408 invalid_grant", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusRequestTimeout, "invalid_grant") }, false},
		{"500", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusInternalServerError, "") }, false},
		{"503", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusServiceUnavailable, "") }, false},
		{"429", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusTooManyRequests, "") }, false},
		{"401 invalid_client", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusUnauthorized, "invalid_client") }, false},
		{"400 invalid_request", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusBadRequest, "invalid_request") }, false},
		{"no answer", func(issuer *oidctest.Issuer) { issuer.DropToken() }, false},
		// The refreshed ID token names another issuer, so it does not verify.
		{"an ID token that does not verify", func(issuer *oidctest.Issuer) { issuer.AdvertiseIssuer("https://another-issuer.example") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, flow := newCodeFlowFor(t)
			session := exchange(t, issuer, flow, personClaims("dispatch-members"))
			tc.fail(issuer)
			_, err := flow.Refresh(context.Background(), session.RefreshToken)
			if refused, unavailable := errors.Is(err, ErrGrantRefused), errors.Is(err, ErrIssuerUnavailable); refused != tc.refused || unavailable == tc.refused {
				t.Fatalf("Refresh: err = %v (ErrGrantRefused %t, ErrIssuerUnavailable %t), want ErrGrantRefused %t and ErrIssuerUnavailable %t", err, refused, unavailable, tc.refused, !tc.refused)
			}
			if strings.Contains(err.Error(), session.RefreshToken) {
				t.Errorf("Refresh's error carries the refresh token: %v", err)
			}
		})
	}
}

// The client authenticates at the token endpoint as the issuer's discovery document lists
// (token_endpoint_auth_methods_supported): by HTTP Basic where the list names
// client_secret_basic, RFC 8414's default, whatever else it names and in whatever order, and in
// the form where it names client_secret_post alone. Every grant is then one request: a code
// exchange the issuer refuses, one it grants, a refresh it refuses, and a refused refresh on a code
// flow made after the sign-in, as a process that restarted since the sign-in has. A document that
// names neither, or no list at all, leaves x/oauth2 to find the style itself: it asks by HTTP Basic
// and, when that fails, again in the form, until a grant has succeeded in one. So there a code
// flow's first grant, refused, is two requests (the control that shows TokenRequests sees a second
// one), and once the sign-in has succeeded every later grant of that flow is one, since each
// starts from the one config discovery built.
func TestCodeFlowAuthenticatesAtTheTokenEndpointAsTheDiscoveryDocumentLists(t *testing.T) {
	basic, form := []string{"client_secret_basic"}, []string{"client_secret_post"}
	guessed := []string{"client_secret_basic", "client_secret_post"}
	for _, tc := range []struct {
		name    string
		methods []string
		// first is the token requests of a refused grant that is its code flow's first: a code
		// exchange on a new flow, and a refresh on another flow made after the sign-in.
		first []string
		// signedIn is the token requests of the sign-in's code exchange, and of a refused refresh
		// on the flow that signed the person in.
		signedIn []string
	}{
		{"client_secret_basic alone", []string{"client_secret_basic"}, basic, basic},
		{"client_secret_post alone", []string{"client_secret_post"}, form, form},
		{"both, in Google's order", []string{"client_secret_post", "client_secret_basic"}, basic, basic},
		{"both, client_secret_basic first", []string{"client_secret_basic", "client_secret_post"}, basic, basic},
		{"neither", []string{"private_key_jwt"}, guessed, basic},
		{"no list", nil, guessed, basic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, flow := newCodeFlowFor(t)
			issuer.AdvertiseTokenAuthMethods(tc.methods...)
			seen := 0
			requests := func(grant string, want []string) {
				t.Helper()
				all := issuer.TokenRequests()
				if got := all[seen:]; !slices.Equal(got, want) {
					t.Errorf("%s: token requests = %q, want %q", grant, got, want)
				}
				seen = len(all)
			}
			if _, err := flow.Exchange(context.Background(), flowRedirect, "a-code-the-issuer-never-issued", "nonce-1"); err == nil {
				t.Fatal("Exchange of a code the issuer never issued succeeded")
			}
			requests("the refused code exchange on a new flow", tc.first)
			session := exchange(t, issuer, flow, personClaims("dispatch-members"))
			requests("the sign-in's code exchange", tc.signedIn)
			issuer.RevokeGrants()
			if _, err := flow.Refresh(context.Background(), session.RefreshToken); !errors.Is(err, ErrGrantRefused) {
				t.Fatalf("Refresh with a revoked refresh token: err = %v, want ErrGrantRefused", err)
			}
			requests("the refused refresh on the flow that signed in", tc.signedIn)
			restarted := NewCodeFlow(issuer.URL(), flowClientID, flowClientSecret, "")
			if _, err := restarted.Refresh(context.Background(), session.RefreshToken); !errors.Is(err, ErrGrantRefused) {
				t.Fatalf("Refresh with a revoked refresh token on a flow made after the sign-in: err = %v, want ErrGrantRefused", err)
			}
			requests("the refused refresh on a flow made after the sign-in", tc.first)
		})
	}
}

// Every grant starts from the one config discovery built, never from a copy of it: a copy taken
// while a refresh first uses the config races with that use (x/oauth2's config holds a
// sync/atomic.Value, which must not be copied after first use). Run with -race, sign-ins and
// refreshes made at once report no race.
func TestCodeFlowGrantsUseTheDiscoveredConfigWithoutARace(t *testing.T) {
	_, flow := newCodeFlowFor(t)
	if err := flow.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = flow.Refresh(context.Background(), "not-a-refresh-token")
		}()
		go func() {
			defer wg.Done()
			_, _ = flow.AuthURL(context.Background(), flowRedirect, "state-1", "nonce-1")
		}()
		go func() {
			defer wg.Done()
			_, _ = flow.Exchange(context.Background(), flowRedirect, "a-code-the-issuer-never-issued", "nonce-1")
		}()
	}
	wg.Wait()
}

// formRecorder is a transport that keeps the form of every request it carries, then sends the
// request on unchanged.
type formRecorder struct {
	mu    sync.Mutex
	forms []url.Values
}

func (r *formRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		read, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		body = read
		request.Body = io.NopCloser(bytes.NewReader(body))
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.forms = append(r.forms, form)
	r.mu.Unlock()
	return http.DefaultTransport.RoundTrip(request)
}

func (r *formRecorder) all() []url.Values {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.forms)
}

// A caller that names no redirect sends none: neither authorization URL and no token request of
// either exchange carries a redirect_uri. A caller that names one sends it in each token request
// of its exchange.
func TestCodeFlowSendsNoRedirectWhenTheCallerNamesNone(t *testing.T) {
	_, flow := newCodeFlowFor(t)
	recorder := &formRecorder{}
	flow.http.Transport = recorder
	ctx := context.Background()
	authorization, err := flow.AuthURL(ctx, "", "state-1", "nonce-1")
	if err != nil {
		t.Fatalf("AuthURL: %v", err)
	}
	online, err := flow.OnlineAuthURL(ctx, "", onlineSignIn("nonce-1"))
	if err != nil {
		t.Fatalf("OnlineAuthURL: %v", err)
	}
	for name, location := range map[string]string{"AuthURL": authorization, "OnlineAuthURL": online} {
		parsed, err := url.Parse(location)
		if err != nil {
			t.Fatalf("%s %q: %v", name, location, err)
		}
		if parsed.Query().Has("redirect_uri") {
			t.Errorf("%s with no redirect = %s, want no redirect_uri", name, location)
		}
	}
	seen := 0
	sent := func(exchange string) []url.Values {
		t.Helper()
		all := recorder.all()
		forms := all[seen:]
		seen = len(all)
		if len(forms) == 0 {
			t.Fatalf("%s made no token request the recorder saw", exchange)
		}
		return forms
	}
	if _, err := flow.Exchange(ctx, "", "a-code-the-issuer-never-issued", "nonce-1"); err == nil {
		t.Fatal("Exchange of a code the issuer never issued succeeded")
	}
	exchanged := sent("Exchange with no redirect")
	if _, err := flow.OnlineExchange(ctx, "", "a-code-the-issuer-never-issued", onlineSignIn("nonce-1")); err == nil {
		t.Fatal("OnlineExchange of a code the issuer never issued succeeded")
	}
	for i, form := range append(exchanged, sent("OnlineExchange with no redirect")...) {
		if form.Has("redirect_uri") {
			t.Errorf("token request %d with no redirect = %v, want no redirect_uri", i+1, form)
		}
	}
	if _, err := flow.Exchange(ctx, flowRedirect, "a-code-the-issuer-never-issued", "nonce-1"); err == nil {
		t.Fatal("Exchange of a code the issuer never issued succeeded")
	}
	for i, form := range sent("Exchange naming a redirect") {
		if got := form.Get("redirect_uri"); got != flowRedirect {
			t.Errorf("token request %d naming a redirect has redirect_uri %q, want %q", i+1, got, flowRedirect)
		}
	}
}

// A client whose secret holds characters the form reserves signs in, refreshes and revokes by
// HTTP Basic, each in one request: the client form-encodes its id and secret before it joins
// them (RFC 6749 section 2.3.1), and the issuer decodes them.
func TestCodeFlowSendsASecretTheFormReservesByHTTPBasic(t *testing.T) {
	const secret = "s+e/c=r%t &x"
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), flowClientID, secret)
	flow := NewCodeFlow(issuer.URL(), flowClientID, secret, "")
	session := exchange(t, issuer, flow, personClaims("dispatch-members"))
	if _, err := flow.Refresh(context.Background(), session.RefreshToken); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := issuer.TokenRequests(); !slices.Equal(got, []string{"client_secret_basic", "client_secret_basic"}) {
		t.Fatalf("the exchange's and the refresh's token requests = %q, want one each, by HTTP Basic", got)
	}
	if err := flow.Revoke(context.Background(), session.RefreshToken); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := issuer.RevocationRequests(); !slices.Equal(got, []string{session.RefreshToken}) {
		t.Fatalf("the revocation endpoint received %d tokens, want the one refresh token", len(got))
	}
}

// Revoke sends the refresh token to the revocation endpoint the issuer's discovery names, as the
// client, and the issuer ends it: the next refresh with it is refused.
func TestCodeFlowRevokeEndsTheRefreshToken(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.SignInAs(personClaims("dispatch-members"))
	code, _ := issuer.Authorize(t, authURL(t, flow, "state-1", "nonce-1"))
	session, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if err := flow.Revoke(context.Background(), session.RefreshToken); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := issuer.RevocationRequests(); !reflect.DeepEqual(got, []string{session.RefreshToken}) {
		t.Errorf("the revocation endpoint was sent %q, want the refresh token once", got)
	}
	if _, err := flow.Refresh(context.Background(), session.RefreshToken); err == nil {
		t.Fatal("Refresh with the revoked refresh token succeeded")
	}
}

// A revocation the issuer does not accept is an error naming its HTTP status and the OAuth error
// code its body names, if any, and never the token.
func TestCodeFlowRevokeReportsTheIssuersRefusalWithoutTheToken(t *testing.T) {
	for _, refusal := range []struct {
		status int
		code   string
		want   string
	}{{http.StatusBadRequest, "invalid_request", "HTTP 400 invalid_request"}, {http.StatusServiceUnavailable, "", "HTTP 503"}} {
		issuer, flow := newCodeFlowFor(t)
		issuer.FailRevocation(refusal.status, refusal.code)
		token := "plain-refresh-token-" + refusal.code
		err := flow.Revoke(context.Background(), token)
		if err == nil || !strings.Contains(err.Error(), refusal.want) || strings.Contains(err.Error(), token) {
			t.Errorf("Revoke against an issuer answering %d %q: err = %v, want one naming %s and not the token", refusal.status, refusal.code, err, refusal.want)
		}
	}
}

// A revocation endpoint that redirects is answered as a refusal, never followed: following a 307
// would re-send the token to the redirect's target.
func TestCodeFlowRevokeDoesNotFollowARedirect(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Add(1) }))
	t.Cleanup(target.Close)
	issuer, flow := newCodeFlowFor(t)
	issuer.RedirectRevocation(target.URL + "/elsewhere")
	if err := flow.Revoke(context.Background(), "plain-refresh-token"); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Errorf("Revoke against a redirecting endpoint: err = %v, want one naming HTTP 307", err)
	}
	if n := received.Load(); n != 0 {
		t.Errorf("the redirect's target received %d requests, want none", n)
	}
}

// An issuer whose discovery names no revocation endpoint cannot revoke: Revoke says so, and no
// endpoint is guessed.
func TestCodeFlowRevokeFailsAgainstAnIssuerWithNoRevocationEndpoint(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), flowClientID, flowClientSecret)
	issuer.OmitRevocationEndpoint()
	flow := NewCodeFlow(issuer.URL(), flowClientID, flowClientSecret, "")
	if err := flow.Revoke(context.Background(), "plain-refresh-token"); err == nil || !strings.Contains(err.Error(), "revocation_endpoint") {
		t.Fatalf("Revoke against an issuer with no revocation endpoint: err = %v, want one naming revocation_endpoint", err)
	}
	if got := issuer.RevocationRequests(); len(got) != 0 {
		t.Errorf("the issuer's /revoke was sent %q, want nothing", got)
	}
}

// An issuer whose discovery document names no authorization or token endpoint offers no sign-in:
// its discovery fails, naming token_endpoint, and so does every call that needs it.
func TestCodeFlowDiscoveryRefusesAnIssuerWithNoCodeFlowEndpoints(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.PublishKey(t, "signing-key")
	issuer.OmitCodeFlowEndpoints()
	flow := NewCodeFlow(issuer.URL(), flowClientID, flowClientSecret, "")
	err := flow.Discover(context.Background())
	if !errors.Is(err, ErrIssuerUnavailable) || !strings.Contains(err.Error(), "token_endpoint") {
		t.Fatalf("Discover against an issuer with no code-flow endpoints: err = %v, want ErrIssuerUnavailable naming token_endpoint", err)
	}
	if flow.Discovered() {
		t.Fatal("an issuer that offers no sign-in reads as discovered")
	}
}

// A discovery document whose token_endpoint_auth_methods_supported is not an array of strings is
// one the code flow cannot read, as one whose revocation_endpoint is not a string is: its
// discovery fails, naming the member, rather than guessing how the client authenticates. A null is
// not an array, and an array holding a null holds something that is not a string, though Go's
// encoding/json reads the first into a []string as no list and the second as the empty string. An
// array of strings, an empty one included, is read.
func TestCodeFlowDiscoveryRefusesATokenAuthMethodsListItCannotRead(t *testing.T) {
	for _, c := range []struct {
		name     string
		list     string // the member's JSON
		readable bool
	}{
		{"a string", `"client_secret_basic"`, false},
		{"null", `null`, false},
		{"an array holding null alone", `[null]`, false},
		{"client_secret_basic and null", `["client_secret_basic",null]`, false},
		{"client_secret_basic and a number", `["client_secret_basic",1]`, false},
		{"client_secret_basic alone", `["client_secret_basic"]`, true},
		{"an empty array", `[]`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/.well-known/openid-configuration" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"issuer":"` + server.URL + `","jwks_uri":"` + server.URL + `/keys",` +
					`"authorization_endpoint":"` + server.URL + `/authorize","token_endpoint":"` + server.URL + `/token",` +
					`"token_endpoint_auth_methods_supported":` + c.list + `}`))
			}))
			t.Cleanup(server.Close)
			flow := NewCodeFlow(server.URL, flowClientID, flowClientSecret, "")
			err := flow.Discover(context.Background())
			if c.readable {
				if err != nil || !flow.Discovered() {
					t.Fatalf("Discover against a document whose list is %s: err = %v, discovered = %t, want it read", c.list, err, flow.Discovered())
				}
				return
			}
			if !errors.Is(err, ErrIssuerUnavailable) || !strings.Contains(err.Error(), "token_endpoint_auth_methods_supported") {
				t.Fatalf("Discover against a document whose list is %s: err = %v, want ErrIssuerUnavailable naming token_endpoint_auth_methods_supported", c.list, err)
			}
			if flow.Discovered() {
				t.Fatal("a document the code flow cannot read reads as discovered")
			}
		})
	}
}

// onlineSignIn is an online-mode sign-in hinting the account of googleClaims, with a fresh PKCE
// verifier and nonce as its ID token's nonce.
func onlineSignIn(nonce string) OnlineSignIn {
	return OnlineSignIn{State: "state-1", Nonce: nonce, Verifier: oauth2.GenerateVerifier(), LoginHint: "a.person@example.com"}
}

// onlineAuthURL is flow's online-mode authorization URL for signIn, failing the test when there
// is none.
func onlineAuthURL(t *testing.T, flow *CodeFlow, signIn OnlineSignIn) string {
	t.Helper()
	location, err := flow.OnlineAuthURL(context.Background(), flowRedirect, signIn)
	if err != nil {
		t.Fatalf("OnlineAuthURL: %v", err)
	}
	return location
}

// onlineAuthURLQuery is the query of flow's online-mode authorization URL for signIn.
func onlineAuthURLQuery(t *testing.T, flow *CodeFlow, signIn OnlineSignIn) url.Values {
	t.Helper()
	location, err := url.Parse(onlineAuthURL(t, flow, signIn))
	if err != nil {
		t.Fatal(err)
	}
	return location.Query()
}

// An online-mode authorization request carries PKCE S256 for the sign-in's verifier, its nonce,
// state and login hint, and the domain as hd; it asks for no offline access and no consent, so
// Google shows no consent screen and issues no refresh token. prompt=select_account is sent only
// when the caller asks for an account switch, and login_hint only when the caller names one.
func TestCodeFlowOnlineAuthURLCarriesPKCEAndAsksForNoOfflineAccess(t *testing.T) {
	_, flow := newCodeFlowForDomain(t, flowDomain)
	signIn := onlineSignIn("nonce-1")
	query := onlineAuthURLQuery(t, flow, signIn)
	for name, want := range map[string]string{
		"code_challenge":        oauth2.S256ChallengeFromVerifier(signIn.Verifier),
		"code_challenge_method": "S256",
		"nonce":                 "nonce-1",
		"state":                 "state-1",
		"login_hint":            "a.person@example.com",
		"hd":                    flowDomain,
		"client_id":             flowClientID,
		"response_type":         "code",
		"redirect_uri":          flowRedirect,
		"scope":                 "openid email profile",
	} {
		if got := query.Get(name); got != want {
			t.Errorf("online mode: %s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"access_type", "prompt"} {
		if query.Has(name) {
			t.Errorf("online mode: the authorization request carries %s=%q", name, query.Get(name))
		}
	}

	switching := onlineSignIn("nonce-2")
	switching.LoginHint, switching.SelectAccount = "", true
	query = onlineAuthURLQuery(t, flow, switching)
	if got := query.Get("prompt"); got != "select_account" {
		t.Errorf("an account switch: prompt = %q, want select_account", got)
	}
	if query.Has("login_hint") || query.Has("access_type") {
		t.Errorf("an account switch with no hint: the request carries login_hint=%q access_type=%q", query.Get("login_hint"), query.Get("access_type"))
	}

	_, groupFlow := newCodeFlowFor(t)
	if query := onlineAuthURLQuery(t, groupFlow, onlineSignIn("nonce-3")); query.Has("hd") {
		t.Errorf("online mode with no hosted domain: the request carries hd=%q", query.Get("hd"))
	}
}

// An issuer that answers a code exchange with no refresh token, as Google does without offline
// access, signs the person in in online mode, which reads the Workspace claims and the nonce from
// the verified ID token; offline mode still refuses that answer. The flow reads the issuer's
// discovery document at its first sign-in, as a caller that builds it before the issuer is up
// does.
func TestCodeFlowOnlineExchangeSignsInWithNoRefreshToken(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), flowClientID, flowClientSecret)
	issuer.OmitRefreshTokens()
	flow := NewCodeFlow(issuer.URL(), flowClientID, flowClientSecret, flowDomain)

	issuer.SignInAs(googleClaims(true))
	signIn := onlineSignIn("nonce-1")
	code, state := issuer.Authorize(t, onlineAuthURL(t, flow, signIn))
	if state != signIn.State {
		t.Fatalf("authorization returned state %q, want %q", state, signIn.State)
	}
	claims, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn)
	if err != nil {
		t.Fatalf("OnlineExchange with no refresh token: %v", err)
	}
	if claims.Subject != "110169484474386276334" || claims.Email != "A.Person@example.com" || !claims.EmailVerified || claims.HostedDomain != flowDomain || claims.Nonce != "nonce-1" || claims.Issuer != issuer.URL() {
		t.Errorf("claims = %#v, want the Workspace account's verified claims with nonce-1", claims)
	}

	issuer.SignInAs(googleClaims(true))
	code, _ = issuer.Authorize(t, authURL(t, flow, "state-2", "nonce-2"))
	if _, err := flow.Exchange(context.Background(), flowRedirect, code, "nonce-2"); err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("offline Exchange with no refresh token: err = %v, want a refusal naming the missing refresh token", err)
	}
}

// The issuer exchanges a code whose authorization request carried a PKCE challenge only for the
// verifier of that challenge: a wrong verifier, or none (offline mode's Exchange sends none), is
// refused at the issuer with invalid_grant. OnlineExchange refuses a sign-in with no verifier
// before asking the issuer.
func TestCodeFlowOnlineExchangeRefusesAWrongOrMissingVerifier(t *testing.T) {
	issuer, flow := newCodeFlowForDomain(t, flowDomain)
	authorize := func(signIn OnlineSignIn) string {
		t.Helper()
		issuer.SignInAs(googleClaims(true))
		code, _ := issuer.Authorize(t, onlineAuthURL(t, flow, signIn))
		return code
	}

	signIn := onlineSignIn("nonce-1")
	code := authorize(signIn)
	wrong := signIn
	wrong.Verifier = oauth2.GenerateVerifier()
	if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, wrong); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("OnlineExchange with another verifier: err = %v, want the issuer's invalid_grant", err)
	}

	code = authorize(signIn)
	if _, err := flow.Exchange(context.Background(), flowRedirect, code, signIn.Nonce); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("an exchange with no verifier for a code with a challenge: err = %v, want the issuer's invalid_grant", err)
	}

	code = authorize(signIn)
	missing := signIn
	missing.Verifier = ""
	if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, missing); err == nil || !strings.Contains(err.Error(), "verifier") {
		t.Errorf("OnlineExchange with no verifier: err = %v, want a refusal naming the verifier", err)
	}
	// The refusal spent nothing at the issuer: the right verifier still signs the person in.
	if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn); err != nil {
		t.Errorf("OnlineExchange with the sign-in's own verifier: %v", err)
	}
}

// Online mode verifies the ID token as offline mode does: one minted for another sign-in's nonce,
// for another client, by another issuer, past its expiry, or under a key the issuer does not hold
// signs nobody in.
func TestCodeFlowOnlineExchangeRefusesAnIDTokenNotForThisSignIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(claims map[string]any)
		want  string
	}{
		{"another audience", func(claims map[string]any) { claims["aud"] = "another-client" }, "(audience)"},
		{"another issuer", func(claims map[string]any) { claims["iss"] = "https://issuer.internal.example" }, "(issuer)"},
		{"expired", func(claims map[string]any) {
			claims["iat"], claims["exp"] = time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
		}, "(expired)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			issuer.SpoilIDTokens(tc.spoil)
			issuer.SignInAs(googleClaims(true))
			signIn := onlineSignIn("nonce-1")
			code, _ := issuer.Authorize(t, onlineAuthURL(t, flow, signIn))
			if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("OnlineExchange of an ID token for %s: err = %v, want a refusal naming %s", tc.name, err, tc.want)
			}
		})
	}

	t.Run("another sign-in's nonce", func(t *testing.T) {
		issuer, flow := newCodeFlowForDomain(t, flowDomain)
		issuer.SignInAs(googleClaims(true))
		signIn := onlineSignIn("nonce-of-another-sign-in")
		code, _ := issuer.Authorize(t, onlineAuthURL(t, flow, signIn))
		signIn.Nonce = "nonce-1"
		if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn); err == nil || !strings.Contains(err.Error(), "nonce") {
			t.Errorf("OnlineExchange with another sign-in's nonce: err = %v, want a nonce refusal", err)
		}
	})

	t.Run("a signature by a key the issuer does not hold", func(t *testing.T) {
		issuer := oidctest.New(t)
		published := issuer.PublishKey(t, "signing-key")
		// A foreign key under the published key's id: the key set resolves, and only the
		// signature fails.
		issuer.EnableCodeFlow(oidctest.NewKey(t, published.KeyID()), flowClientID, flowClientSecret)
		flow := NewCodeFlow(issuer.URL(), flowClientID, flowClientSecret, flowDomain)
		issuer.SignInAs(googleClaims(true))
		signIn := onlineSignIn("nonce-1")
		code, _ := issuer.Authorize(t, onlineAuthURL(t, flow, signIn))
		if _, err := flow.OnlineExchange(context.Background(), flowRedirect, code, signIn); err == nil || !strings.Contains(err.Error(), "(signature)") {
			t.Errorf("OnlineExchange of a forged ID token: err = %v, want a refusal naming the signature", err)
		}
	})
}
