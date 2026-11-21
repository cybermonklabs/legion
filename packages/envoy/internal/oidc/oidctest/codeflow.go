package oidctest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// codeFlowClient is one confidential client the code flow serves: its secret,
// the key the ID tokens issued to it are signed with, and whether the device
// authorization grant is armed for it (EnableDeviceFlow), which the clients
// EnableCodeFlow arms it for do not serve.
type codeFlowClient struct {
	key          *Key
	clientSecret string
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
	revocation      revocation
	// omitRefresh makes a code exchange answer with no refresh token
	// (OmitRefreshTokens).
	omitRefresh bool
	// spoil changes the claims of every ID token the token endpoint mints,
	// after the issuer has filled them in (SpoilIDTokens).
	spoil func(claims map[string]any)
}

// tokenFailure is how the token endpoint answers every grant instead of
// granting it, when FailToken, DropToken or FailTokenWithStatusLine set it:
// status with code as the body's OAuth error, or, with drop, closing the
// connection unanswered, or, with statusLine, that status line and a page that
// is no OAuth error answer.
type tokenFailure struct {
	status     int
	code       string
	drop       bool
	statusLine string
}

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

// EnableCodeFlow arms the authorization and token endpoints for one
// confidential client, signing every ID token they issue to it with key (which
// the caller publishes). Call it again with another client id to serve that
// client too: one issuer holds a set of clients keyed by client id, each with
// its own secret and signing key, while the queued sign-ins, the token failure
// and the revocation state stay the issuer's own, shared by all of them. A
// client id already served is replaced, and the codes and refresh tokens
// already issued stand; a code or a refresh token is exchanged only by the
// client it was issued to.
func (i *Issuer) EnableCodeFlow(key *Key, clientID, clientSecret string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.enableClient(key, clientID, clientSecret, false)
}

// EnableDeviceFlow arms the device authorization endpoint and the device code
// grant for one confidential client, the way EnableCodeFlow arms the
// authorization and token endpoints, and on the same terms: the client's own
// signing key and secret, the issuer's own queued sign-ins. A client id
// already served is replaced, so a client serves the device grant or the code
// flow, not both, as a Google client registered for limited-input devices is
// not the one a browser signs in at. The device codes it issues are approved
// by ApproveDeviceCode.
func (i *Issuer) EnableDeviceFlow(key *Key, clientID, clientSecret string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.enableClient(key, clientID, clientSecret, true)
}

// enableClient serves clientID, arming the device grant for it when device is
// set. The caller holds i.mu.
func (i *Issuer) enableClient(key *Key, clientID, clientSecret string, device bool) {
	if i.codeFlow == nil {
		i.codeFlow = &codeFlow{
			clients:     map[string]*codeFlowClient{},
			codes:       map[string]codeGrant{},
			grants:      map[string]*refreshGrant{},
			deviceCodes: map[string]*deviceGrant{},
		}
	}
	i.codeFlow.clients[clientID] = &codeFlowClient{key: key, clientSecret: clientSecret, deviceGrant: device}
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

// FailToken makes the token endpoint answer every grant, a code exchange or a
// refresh, with status instead of granting it, with code as the body's OAuth
// error when code is not empty: 400 invalid_grant is an issuer refusing the
// refresh token, a 5xx or a 429 one that cannot answer now. A status of 0
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
// SignInByDefault set, and redirects to the request's redirect_uri with it and
// the request's state. It records the client the code is for, the request's
// nonce, for the ID token, and its PKCE code_challenge, which only the S256
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
	switch {
	case flow.clients[clientID] == nil || query.Get("response_type") != "code" || query.Get("redirect_uri") == "":
		http.Error(w, "bad authorization request", http.StatusBadRequest)
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
	target, err := url.Parse(query.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	values := target.Query()
	values.Set("code", code)
	values.Set("state", query.Get("state"))
	target.RawQuery = values.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
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

// handleToken answers the three grants a confidential client makes, as
// Cognito's token endpoint does for the first two and Google's does for the
// third: a code exchange returns an ID token, an access token and a refresh
// token (none once OmitRefreshTokens is set); a refresh returns a fresh ID
// token and access token and no new refresh token; and a device code grant
// (RFC 8628 section 3.4) returns what a code exchange does, once a person has
// approved its user code, answering authorization_pending until they have and
// expired_token once the code's life has run out. Each grant is the client's
// own, and authenticates as the client, device code included, as Google's
// device grant does: a code, a refresh token or a device code another client
// was issued is invalid_grant. A code whose authorization request carried a
// PKCE challenge is exchanged only with the code_verifier whose S256 is that
// challenge (RFC 7636 section 4.6); a missing or wrong verifier is
// invalid_grant, and spends the code like any refusal.
func (i *Issuer) handleToken(w http.ResponseWriter, r *http.Request) {
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
	if failure := flow.token; failure.status != 0 || failure.drop || failure.statusLine != "" {
		if r.PostForm.Get("grant_type") == "refresh_token" {
			flow.refreshes++
		}
		switch {
		case failure.drop:
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
		case failure.statusLine != "":
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, buf, err := hijacker.Hijack(); err == nil {
					const page = "<html>gateway</html>"
					_, _ = fmt.Fprintf(buf, "HTTP/1.1 %s\r\nContent-Type: text/html\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", failure.statusLine, len(page), page)
					_ = buf.Flush()
					_ = conn.Close()
				}
			}
		case failure.code == "":
			http.Error(w, http.StatusText(failure.status), failure.status)
		default:
			tokenError(w, failure.status, failure.code)
		}
		return
	}
	clientID, clientSecret, basic := r.BasicAuth()
	if !basic {
		clientID, clientSecret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	client, served := flow.clients[clientID]
	if !served || clientSecret != client.clientSecret {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	var (
		claims  map[string]any
		refresh string
	)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		grant, ok := flow.codes[r.PostForm.Get("code")]
		delete(flow.codes, r.PostForm.Get("code"))
		if !ok || grant.clientID != clientID || grant.redirectURI != r.PostForm.Get("redirect_uri") {
			tokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		// A missing verifier is refused outright rather than hashed, so it can
		// never match a recorded challenge.
		verifier := r.PostForm.Get("code_verifier")
		if grant.challenge != "" && (verifier == "" || oauth2.S256ChallengeFromVerifier(verifier) != grant.challenge) {
			tokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		claims = maps.Clone(grant.claims)
		if grant.nonce != "" {
			claims["nonce"] = grant.nonce
		}
		if !flow.omitRefresh {
			refresh = randomValue()
			flow.grants[refresh] = &refreshGrant{clientID: clientID, claims: maps.Clone(grant.claims)}
		}
	case "refresh_token":
		flow.refreshes++
		grant, ok := flow.grants[r.PostForm.Get("refresh_token")]
		if !ok || grant.revoked || grant.clientID != clientID {
			tokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		claims = maps.Clone(grant.claims)
	case deviceCodeGrantType:
		code := r.PostForm.Get("device_code")
		grant, waiting := flow.deviceCodes[code]
		switch {
		case !waiting || grant.clientID != clientID:
			tokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		case !time.Now().Before(grant.expires):
			delete(flow.deviceCodes, code)
			tokenError(w, http.StatusBadRequest, "expired_token")
			return
		case grant.claims == nil:
			tokenError(w, http.StatusBadRequest, "authorization_pending")
			return
		}
		// The poll that redeems a device code spends it, as the exchange
		// spends an authorization code; a poll that is answered
		// authorization_pending spends nothing.
		delete(flow.deviceCodes, code)
		claims = maps.Clone(grant.claims)
		if !flow.omitRefresh {
			refresh = randomValue()
			flow.grants[refresh] = &refreshGrant{clientID: clientID, claims: maps.Clone(grant.claims)}
		}
	default:
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	now := time.Now()
	claims["iss"] = i.issuerClaim
	claims["aud"] = clientID
	claims["iat"] = now.Add(-time.Minute).Unix()
	claims["exp"] = now.Add(time.Hour).Unix()
	if flow.spoil != nil {
		flow.spoil(claims)
	}
	idToken, err := sign(client.key, claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body := map[string]any{
		"access_token": randomValue(),
		"id_token":     idToken,
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if refresh != "" {
		body["refresh_token"] = refresh
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// handleRevoke answers a revocation as Cognito's /oauth2/revoke does for a
// client with a secret: the client authenticates with HTTP Basic, the body
// carries the token, and a token the pool does not know, or has revoked
// already, is answered 200 like one it revokes now. A revoked refresh token's
// next refresh answers invalid_grant.
func (i *Issuer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	i.mu.Lock()
	flow := i.codeFlow
	if flow == nil {
		i.mu.Unlock()
		http.Error(w, "code flow not enabled", http.StatusNotFound)
		return
	}
	clientID, clientSecret, basic := r.BasicAuth()
	client, served := flow.clients[clientID]
	if !basic || !served || clientSecret != client.clientSecret {
		i.mu.Unlock()
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	token := r.PostForm.Get("token")
	if token == "" {
		i.mu.Unlock()
		tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	flow.revocation.requests = append(flow.revocation.requests, token)
	status, code, redirect, delay := flow.revocation.status, flow.revocation.code, flow.revocation.redirect, flow.revocation.delay
	i.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	if status != 0 {
		if code == "" {
			http.Error(w, http.StatusText(status), status)
		} else {
			tokenError(w, status, code)
		}
		return
	}
	i.mu.Lock()
	if grant, ok := flow.grants[token]; ok && grant.clientID == clientID {
		grant.revoked = true
	}
	i.mu.Unlock()
	w.WriteHeader(http.StatusOK)
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
