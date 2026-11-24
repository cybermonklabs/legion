package oidctest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// codeFlowClient is one confidential client the code flow serves: its secret,
// the key the ID tokens issued to it are signed with, the redirect URIs it
// registered, the only addresses an authorization sends the browser back to,
// and whether the device authorization grant is armed for it
// (EnableDeviceFlow), which the clients EnableCodeFlow arms it for do not serve.
type codeFlowClient struct {
	key          *Key
	clientSecret string
	redirectURIs []*url.URL
	deviceGrant  bool
}

// codeFlow is the state of the authorization and token endpoints: the
// confidential clients they serve, keyed by client id, the sign-ins queued for
// the next authorization requests and the one used when none is queued, and
// the codes and refresh tokens issued. Everything that is not a client's own is
// the issuer's, shared by every client it serves: one queue of sign-ins, one
// token failure, one revocation state.
type codeFlow struct {
	clients         map[string]*codeFlowClient
	queued          []map[string]any
	signInByDefault map[string]any
	codes           map[string]codeGrant
	grants          map[string]*refreshGrant
	deviceCodes     map[string]*deviceGrant
	refreshes       int
	token           tokenFailure
	// tokenRequests is each token request's client authentication (TokenRequests).
	tokenRequests []string
	// tokenDelay is the token endpoint's wait before it answers (DelayToken).
	tokenDelay time.Duration
	revocation revocation
	// omitRefresh makes a code exchange answer with no refresh token
	// (OmitRefreshTokens).
	omitRefresh bool
	// spoil changes the claims of every ID token the token endpoint mints,
	// after the issuer has filled them in (SpoilIDTokens).
	spoil func(claims map[string]any)
}

// tokenFailure is how the token endpoint answers every grant instead of
// granting it, when FailToken, DropToken, FailTokenWithStatusLine, CutToken,
// ResetToken or StallToken set it: status with code as the body's OAuth error;
// with drop, closing the connection unanswered; with statusLine, that status
// line and a page that is no OAuth error answer; or, with cut, answering status
// and a body it breaks off part of the way through, by closing the connection
// (cutClose), resetting it (cutReset) or sending nothing more (cutStall).
type tokenFailure struct {
	status     int
	code       string
	drop       bool
	statusLine string
	cut        cutAnswer
}

// cutAnswer is how the token endpoint breaks off a body it promised 200 bytes
// of, after sending 10: none is a whole answer.
type cutAnswer int

const (
	cutNone cutAnswer = iota
	cutClose
	cutReset
	cutStall
)

// revocation is the state of the revocation endpoint: every token the client
// sent it, in order, whatever it answered; the status and OAuth error code it
// answers instead of revoking, when FailRevocation set one; where it redirects
// instead, when RedirectRevocation set it; and how long it waits before
// answering.
type revocation struct {
	requests []string
	status   int
	code     string
	redirect string
	delay    time.Duration
}

// codeGrant is one authorization code: the client it was issued to, the person
// it signs in, the nonce and redirect URI of the authorization request that
// issued it, and the PKCE S256 code_challenge that request carried, empty when
// it carried none.
type codeGrant struct {
	clientID    string
	claims      map[string]any
	nonce       string
	redirectURI string
	challenge   string
}

// refreshGrant is one refresh token: the client it was issued to, the claims
// each refresh's ID token carries, which UpdateGrants changes the way a pool
// changes a user, and whether RevokeGrants has ended it.
type refreshGrant struct {
	clientID string
	claims   map[string]any
	revoked  bool
}

// deviceGrant is one device authorization (RFC 8628): the client it was issued
// to, the user code a person types in to approve it, the claims of the sign-in
// that approved it, nil while nobody has, and the moment it stops being
// answered.
type deviceGrant struct {
	clientID string
	userCode string
	claims   map[string]any
	expires  time.Time
}

const (
	// deviceCodeGrantType is RFC 8628's grant type, which x/oauth2 spells in a
	// package its users cannot import.
	deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// deviceCodeLifetime is how long a device code is answered for, the span
	// the device authorization answer reports as expires_in. ExpireDeviceCodes
	// ends the codes issued so far without a test waiting it out.
	deviceCodeLifetime = 10 * time.Minute
	// devicePollInterval is the interval, in seconds, the device authorization
	// answer asks the device to poll at. Nothing enforces it — a poll that
	// arrives sooner is answered, never slow_down — so a test polls as fast as
	// it likes; it is one second rather than Google's five so that a client
	// that does honour it still finishes a test quickly.
	devicePollInterval = 1
	// userCodeAlphabet is what a user code is drawn from: capitals with no
	// vowel, so no code reads as a word, and none of the letters a person
	// confuses with a digit while typing one in.
	userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"
)

// googleTokenAuthMethods is the token_endpoint_auth_methods_supported of Google's discovery
// document, in its order, as https://accounts.google.com/.well-known/openid-configuration read
// on 2026-11-20: the issuer lists it until AdvertiseTokenAuthMethods says otherwise.
var googleTokenAuthMethods = []string{"client_secret_post", "client_secret_basic"}

// EnableCodeFlow arms the authorization and token endpoints for one
// confidential client, signing every ID token they issue to it with key (which
// the caller publishes). redirectURIs are the client's registered redirect
// URIs, as a client registers its own at Google: an authorization naming any
// other is refused 400 redirect_uri_mismatch and sends the browser nowhere
// (RFC 6749 section 4.1.2.1), so a client that registers none signs nobody
// in. A registered loopback IP literal URI with no port is taken at any port
// the request names (RFC 8252 section 7.3), for a server that binds a port the
// OS chooses after the issuer starts. EnableCodeFlow panics on a redirect URI
// ParseRedirectURI refuses, as regexp.MustCompile panics on a bad pattern: it
// is the caller's own mistake.
//
// Call it again with another client id to serve that client too: one issuer
// holds a set of clients keyed by client id, each with its own secret, signing
// key and redirect URIs, while the queued sign-ins, the token failure and the
// revocation state stay the issuer's own, shared by all of them. A client id
// already served is replaced, and the codes and refresh tokens already issued
// stand; a code or a refresh token is exchanged only by the client it was
// issued to.
func (i *Issuer) EnableCodeFlow(key *Key, clientID, clientSecret string, redirectURIs ...string) {
	registered := make([]*url.URL, len(redirectURIs))
	for at, raw := range redirectURIs {
		parsed, err := ParseRedirectURI(raw)
		if err != nil {
			panic("oidctest: EnableCodeFlow for " + clientID + ": " + err.Error())
		}
		registered[at] = parsed
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.enableClient(&codeFlowClient{key: key, clientSecret: clientSecret, redirectURIs: registered}, clientID)
}

// EnableDeviceFlow arms the device authorization endpoint and the device code
// grant for one confidential client, the way EnableCodeFlow arms the
// authorization and token endpoints, and on the same terms: the client's own
// signing key and secret, the issuer's own queued sign-ins. A client id
// already served is replaced, so a client serves the device grant or the code
// flow, not both, as a Google client registered for limited-input devices is
// not the one a browser signs in at: it registers no redirect URI. The device
// codes it issues are approved by ApproveDeviceCode.
func (i *Issuer) EnableDeviceFlow(key *Key, clientID, clientSecret string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.enableClient(&codeFlowClient{key: key, clientSecret: clientSecret, deviceGrant: true}, clientID)
}

// enableClient serves client as clientID. The caller holds i.mu.
func (i *Issuer) enableClient(client *codeFlowClient, clientID string) {
	if i.codeFlow == nil {
		i.codeFlow = &codeFlow{
			clients:     map[string]*codeFlowClient{},
			codes:       map[string]codeGrant{},
			grants:      map[string]*refreshGrant{},
			deviceCodes: map[string]*deviceGrant{},
		}
	}
	i.codeFlow.clients[clientID] = client
}

// ParseRedirectURI reads raw as a redirect URI a client may register: an
// absolute http or https URL naming a host, with no fragment (RFC 6749 section
// 3.1.2). cmd/dev-oidc-issuer refuses a --code-flow callback with its error.
func ParseRedirectURI(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	switch {
	case err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Opaque != "":
		return nil, fmt.Errorf("the redirect URI %q is not an absolute http or https URL", raw)
	case parsed.Fragment != "" || strings.Contains(raw, "#"):
		return nil, fmt.Errorf("the redirect URI %q carries a fragment", raw)
	}
	return parsed, nil
}

// redirectTarget is where an authorization naming requested sends the browser:
// the registered redirect URI that requested is, character for character, or,
// for a registered loopback IP literal URI with no port, that URI at the port
// requested names, when nothing else of requested differs from it. ok is false
// for any other address. The target is built from the registered URI, never
// from requested: only the port, read as a number, comes from the request.
func (c *codeFlowClient) redirectTarget(requested string) (target url.URL, ok bool) {
	asked, err := url.Parse(requested)
	if err != nil {
		return url.URL{}, false
	}
	for _, registered := range c.redirectURIs {
		if requested == registered.String() {
			return *registered, true
		}
		if registered.Port() != "" || !loopbackIP(registered.Hostname()) {
			continue
		}
		port, err := strconv.Atoi(asked.Port())
		if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != asked.Port() {
			continue
		}
		withoutPort := *asked
		withoutPort.Host = strings.TrimSuffix(asked.Host, ":"+asked.Port())
		if withoutPort.String() != registered.String() {
			continue
		}
		target = *registered
		target.Host = net.JoinHostPort(registered.Hostname(), strconv.Itoa(port))
		return target, true
	}
	return url.URL{}, false
}

// loopbackIP reports whether host is an IP literal for a loopback address, as
// RFC 8252 section 7.3 names the redirect URIs whose port an issuer must let a
// request choose. A name, localhost included, is not one (section 8.3).
func loopbackIP(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Zone() == "" && !addr.Is4In6() && addr.IsLoopback()
}

// OmitCodeFlowEndpoints makes the discovery document name no authorization or
// token endpoint, the shape of an issuer that offers no sign-in.
func (i *Issuer) OmitCodeFlowEndpoints() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.omitCodeFlow = true
}

// OmitRevocationEndpoint makes the discovery document name no revocation
// endpoint, the shape of an issuer that offers no token revocation.
func (i *Issuer) OmitRevocationEndpoint() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.omitRevocation = true
}

// AdvertiseTokenAuthMethods makes the discovery document list methods as its
// token_endpoint_auth_methods_supported (RFC 8414 section 2), how the token endpoint takes a
// client's credentials, in place of Google's list, which it names until then. With no methods
// the document names no such member, the shape of an issuer that does not say. Whatever it
// lists, the token endpoint takes the client's secret by HTTP Basic or in the form, as Google's
// and Cognito's do, and TokenRequests records which one each request used.
func (i *Issuer) AdvertiseTokenAuthMethods(methods ...string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.tokenAuthMethods = slices.Clone(methods)
}

// FailToken makes the token endpoint answer every grant, a code exchange or a
// refresh, with status instead of granting it, with code as the body's OAuth
// error when code is not empty: 400 invalid_grant is an issuer refusing the
// refresh token, a 5xx, a 408 or a 429 one that cannot answer now. A status of 0
// makes it grant again, and undoes DropToken too.
func (i *Issuer) FailToken(status int, code string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.token = tokenFailure{status: status, code: code}
}

// DropToken makes the token endpoint close every connection a grant arrives
// on without answering: a client sees a transport failure, as it does when
// the issuer's network path breaks mid-request. FailToken(0, "") undoes it.
func (i *Issuer) DropToken() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.token = tokenFailure{drop: true}
}

// FailTokenWithStatusLine makes the token endpoint answer every grant with
// statusLine, a status code and the reason phrase the issuer writes, and a page
// that is no OAuth error answer, written on the connection itself, since
// net/http writes only the standard reason phrase. x/oauth2's error for an
// answer with no error code prints its status line. FailToken(0, "") undoes it.
func (i *Issuer) FailTokenWithStatusLine(statusLine string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.token = tokenFailure{statusLine: statusLine}
}

// CutToken makes the token endpoint answer every grant with status and a body
// cut off: it promises 200 bytes, sends 10 and closes the connection, as an
// issuer does that fails part of the way through its answer. A client reads the
// status and then fails to read the body. FailToken(0, "") undoes it.
func (i *Issuer) CutToken(status int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.token = tokenFailure{status: status, cut: cutClose}
}

// ResetToken is CutToken with the connection reset (a TCP RST) after the 10
// bytes, where CutToken closes it. FailToken(0, "") undoes it.
func (i *Issuer) ResetToken(status int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.token = tokenFailure{status: status, cut: cutReset}
}

// StallToken is CutToken with nothing more sent after the 10 bytes until the
// client hangs up, so the client's own timeout ends its read of the body.
// FailToken(0, "") undoes it.
func (i *Issuer) StallToken(status int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.token = tokenFailure{status: status, cut: cutStall}
}

// DelayToken makes the token endpoint wait d before it answers each grant, or until the client
// hangs up: an issuer slow to answer. The answer, a grant or a refusal, is the one the issuer's
// state gives once the wait is over.
func (i *Issuer) DelayToken(d time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.tokenDelay = d
}

// TokenRequests is how every request the token endpoint has received authenticated its client,
// in order, whatever it answered, each named as token_endpoint_auth_methods_supported names it:
// client_secret_basic for HTTP Basic, client_secret_post for the client's secret in the form,
// and none for neither. A client that asks once by HTTP Basic and again in the form is two
// entries.
func (i *Issuer) TokenRequests() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.codeFlow.tokenRequests)
}

// FailRevocation makes the revocation endpoint answer status, with code as the
// body's OAuth error when code is not empty, instead of revoking; a status of 0
// makes it revoke again. Cognito answers 400 invalid_request to a client with
// revocation turned off.
func (i *Issuer) FailRevocation(status int, code string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.revocation.status, i.codeFlow.revocation.code = status, code
}

// DelayRevocation makes the revocation endpoint wait d before answering, or
// until the client hangs up: a pool that accepts the connection and does not
// answer.
func (i *Issuer) DelayRevocation(d time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.revocation.delay = d
}

// RedirectRevocation makes the revocation endpoint answer 307 to target
// instead of revoking: a client that follows it re-sends the token there.
func (i *Issuer) RedirectRevocation(target string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.revocation.redirect = target
}

// RevocationRequests is every token the client has sent the revocation
// endpoint, in order, whatever it answered.
func (i *Issuer) RevocationRequests() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.codeFlow.revocation.requests...)
}

// OmitRefreshTokens makes every code exchange from now on answer with no
// refresh token, as Google does for a sign-in that did not ask for offline
// access.
func (i *Issuer) OmitRefreshTokens() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.omitRefresh = true
}

// SpoilIDTokens makes change rewrite the claims of every ID token the token
// endpoint mints from now on, after the issuer has set iss, aud, iat, exp and
// the nonce, so a test can spoil one of them in a token the code flow
// otherwise issues as usual. A nil change mints them unspoiled again.
func (i *Issuer) SpoilIDTokens(change func(claims map[string]any)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.spoil = change
}

// SignInAs queues the claims the next authorization request signs in with: the
// person who completes the sign-in at the identity provider. The issuer adds
// iss, aud, iat, exp and the request's nonce when it mints the ID token.
func (i *Issuer) SignInAs(claims map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.queued = append(i.codeFlow.queued, maps.Clone(claims))
}

// SignInByDefault sets the claims an authorization request signs in with when
// SignInAs has queued none, so every sign-in completes as that person until a
// queued one takes a turn. Without it, such a request is refused.
func (i *Issuer) SignInByDefault(claims map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeFlow.signInByDefault = maps.Clone(claims)
}

// UpdateGrants changes the claims every refresh token issued so far answers
// with, as a pool does when a user's groups change.
func (i *Issuer) UpdateGrants(change func(claims map[string]any)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, grant := range i.codeFlow.grants {
		change(grant.claims)
	}
}

// RevokeGrants ends every refresh token issued so far: the next refresh with any
// of them answers invalid_grant, as a pool does for a disabled or deleted user.
func (i *Issuer) RevokeGrants() {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, grant := range i.codeFlow.grants {
		grant.revoked = true
	}
}

// Refreshes counts the refresh-token grants the token endpoint has answered,
// successful or not.
func (i *Issuer) Refreshes() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.codeFlow.refreshes
}

// ApproveDeviceCode approves the device authorization whose user code a person
// has typed in, signing it in as the next person SignInAs queued or, with none
// queued, the one SignInByDefault set, the way an authorization request signs
// one in. The user code is read as the person types it, whatever its case and
// surrounding space. A user code no device authorization is waiting on, one
// approved already and one whose device code has expired are each refused, as
// is an approval with nobody to sign in. It takes no TB because a command
// calls it: cmd/dev-oidc-issuer's control route that approves a user code.
func (i *Issuer) ApproveDeviceCode(userCode string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	typed := strings.ToUpper(strings.TrimSpace(userCode))
	if i.codeFlow == nil {
		return fmt.Errorf("no device authorization is waiting on the user code %q", typed)
	}
	flow := i.codeFlow
	for _, grant := range flow.deviceCodes {
		if grant.userCode != typed {
			continue
		}
		switch {
		case grant.claims != nil:
			return fmt.Errorf("the user code %q was approved already", typed)
		case !time.Now().Before(grant.expires):
			return fmt.Errorf("the user code %q has expired", typed)
		case len(flow.queued) > 0:
			grant.claims, flow.queued = flow.queued[0], flow.queued[1:]
		case flow.signInByDefault != nil:
			grant.claims = maps.Clone(flow.signInByDefault)
		default:
			return errors.New("no sign-in queued (SignInAs) and no default (SignInByDefault)")
		}
		return nil
	}
	return fmt.Errorf("no device authorization is waiting on the user code %q", typed)
}

// ExpireDeviceCodes ends every device authorization issued so far: the next
// poll with any of their device codes answers expired_token, as one does once
// the person took longer than deviceCodeLifetime to approve it.
func (i *Issuer) ExpireDeviceCodes() {
	i.mu.Lock()
	defer i.mu.Unlock()
	past := time.Now().Add(-time.Second)
	for _, grant := range i.codeFlow.deviceCodes {
		grant.expires = past
	}
}

// Authorize follows authURL the way a browser does through the identity
// provider's sign-in, and returns the code and state the issuer redirects back
// with. The person signed in is the one SignInAs queued, or, when none is
// queued, the one SignInByDefault set.
func (i *Issuer) Authorize(t TB, authURL string) (code, state string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("GET %s: %v", authURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("GET %s: status %d, want 302", authURL, response.StatusCode)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect %q: %v", response.Header.Get("Location"), err)
	}
	return location.Query().Get("code"), location.Query().Get("state")
}

// handleAuthorize issues a code for the next queued sign-in, or else the person
// SignInByDefault set, and redirects to the client's registered redirect URI
// the request names (codeFlowClient.redirectTarget) with it and the request's
// state. A redirect_uri the client did not register is refused 400
// redirect_uri_mismatch, as Google refuses one, before any code is issued or
// queued sign-in spent, and the browser is sent nowhere. It records the client
// the code is for, the request's redirect_uri, which the exchange must repeat,
// its nonce, for the ID token, and its PKCE code_challenge, which only the S256
// method may carry: a challenge by any other method, plain included, is a bad
// request, so a client that sends one never gets a code to exchange.
func (i *Issuer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	i.mu.Lock()
	defer i.mu.Unlock()
	flow := i.codeFlow
	if flow == nil {
		http.Error(w, "code flow not enabled", http.StatusNotFound)
		return
	}
	clientID := query.Get("client_id")
	client := flow.clients[clientID]
	if client == nil || query.Get("response_type") != "code" || query.Get("redirect_uri") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	target, registered := client.redirectTarget(query.Get("redirect_uri"))
	switch {
	case !registered:
		http.Error(w, "redirect_uri_mismatch: the redirect_uri is not one the client registered", http.StatusBadRequest)
		return
	case query.Has("code_challenge") && (query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256"):
		http.Error(w, "bad authorization request: code_challenge must be S256", http.StatusBadRequest)
		return
	case len(flow.queued) == 0 && flow.signInByDefault == nil:
		http.Error(w, "no sign-in queued (SignInAs) and no default (SignInByDefault)", http.StatusBadRequest)
		return
	}
	var claims map[string]any
	switch {
	case len(flow.queued) > 0:
		claims = flow.queued[0]
		flow.queued = flow.queued[1:]
	default:
		claims = maps.Clone(flow.signInByDefault)
	}
	code := randomValue()
	flow.codes[code] = codeGrant{clientID: clientID, claims: claims, nonce: query.Get("nonce"), redirectURI: query.Get("redirect_uri"), challenge: query.Get("code_challenge")}
	// The registered URI's own query is kept, and the code and state follow the "?" that ends
	// the URI's path, so nothing the request sent reaches the scheme, host or path.
	values := target.Query()
	values.Set("code", code)
	values.Set("state", query.Get("state"))
	target.RawQuery, target.ForceQuery = "", false
	http.Redirect(w, r, target.String()+"?"+values.Encode(), http.StatusFound)
}

// handleDeviceAuthorization answers a device authorization request (RFC 8628
// section 3.1) as Google's does: the client names itself and the scope it
// wants, and the answer is the device code it polls the token endpoint with,
// the user code a person types in, where they type it, and how long the codes
// live. The request carries no secret, as the device holds none it can keep;
// a client the issuer does not serve the device grant for is invalid_client,
// and so is one it serves only the code flow for.
func (i *Issuer) handleDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	flow := i.codeFlow
	if flow == nil {
		http.Error(w, "code flow not enabled", http.StatusNotFound)
		return
	}
	client, served := flow.clients[r.PostForm.Get("client_id")]
	if !served || !client.deviceGrant {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	deviceCode, userCode := randomValue(), randomUserCode()
	flow.deviceCodes[deviceCode] = &deviceGrant{
		clientID: r.PostForm.Get("client_id"),
		userCode: userCode,
		expires:  time.Now().Add(deviceCodeLifetime),
	}
	verification := i.server.URL + "/device"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device_code":               deviceCode,
		"user_code":                 userCode,
		"verification_uri":          verification,
		"verification_uri_complete": verification + "?user_code=" + url.QueryEscape(userCode),
		"expires_in":                int(deviceCodeLifetime.Seconds()),
		"interval":                  devicePollInterval,
	})
}

func tokenError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func randomValue() string {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		panic(errors.New("oidctest: crypto/rand failed: " + err.Error()))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// randomUserCode is the code a person types in to approve a device
// authorization: short enough to read off one screen and type into another,
// in the two groups of four Google's is written in.
func randomUserCode() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic(errors.New("oidctest: crypto/rand failed: " + err.Error()))
	}
	code := make([]byte, 0, len(buf)+1)
	for at, b := range buf {
		if at == len(buf)/2 {
			code = append(code, '-')
		}
		code = append(code, userCodeAlphabet[int(b)%len(userCodeAlphabet)])
	}
	return string(code)
}
