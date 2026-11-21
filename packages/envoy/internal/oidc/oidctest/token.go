package oidctest

import (
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
)

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
	flow := i.codeFlow
	if flow == nil {
		i.mu.Unlock()
		http.Error(w, "code flow not enabled", http.StatusNotFound)
		return
	}
	flow.tokenRequests = append(flow.tokenRequests, clientAuthentication(r))
	delay := flow.tokenDelay
	i.mu.Unlock()
	if !wait(r, delay) {
		return
	}
	i.mu.Lock()
	if failure := flow.token; failure.status != 0 || failure.drop {
		// The failure and the refresh count are read and kept under the lock; the answer is written
		// without it, as the delay above and handleRevoke's failure answer are.
		if r.PostForm.Get("grant_type") == "refresh_token" {
			flow.refreshes++
		}
		i.mu.Unlock()
		switch {
		case failure.drop:
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
		case failure.cut != cutNone:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "200")
			w.WriteHeader(failure.status)
			_, _ = w.Write([]byte(`{"access_t`))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			breakOff(w, r, failure.cut)
		case failure.code == "":
			http.Error(w, http.StatusText(failure.status), failure.status)
		default:
			tokenError(w, failure.status, failure.code)
		}
		return
	}
	defer i.mu.Unlock()
	clientID, clientSecret, basic, err := basicCredentials(r)
	if err != nil {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
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

// clientAuthentication is how a token request authenticates its client, named as
// token_endpoint_auth_methods_supported names it: client_secret_basic for HTTP Basic,
// client_secret_post for the client's secret in the form, and none for neither.
func clientAuthentication(r *http.Request) string {
	if _, _, basic := r.BasicAuth(); basic {
		return "client_secret_basic"
	}
	if r.PostForm.Has("client_secret") {
		return "client_secret_post"
	}
	return "none"
}

// basicCredentials is the client a request names by HTTP Basic, each part form-decoded, since RFC
// 6749 section 2.3.1 has a client form-encode its id and secret before it joins them (x/oauth2's
// AuthStyleInHeader does): basic is false for a request with no Basic credentials, and err is set
// for one whose id or secret does not decode.
func basicCredentials(r *http.Request) (clientID, clientSecret string, basic bool, err error) {
	rawID, rawSecret, basic := r.BasicAuth()
	if !basic {
		return "", "", false, nil
	}
	if clientID, err = url.QueryUnescape(rawID); err != nil {
		return "", "", true, err
	}
	if clientSecret, err = url.QueryUnescape(rawSecret); err != nil {
		return "", "", true, err
	}
	return clientID, clientSecret, true, nil
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
	clientID, clientSecret, basic, err := basicCredentials(r)
	client, served := flow.clients[clientID]
	if err != nil || !basic || !served || clientSecret != client.clientSecret {
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
	if !wait(r, delay) {
		return
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

// breakOff ends an answer whose body the token endpoint promised and did not send, as how says:
// cutClose aborts the response, so the server closes the connection; cutReset takes the
// connection and resets it (a TCP RST: SO_LINGER of zero, then close); cutStall sends nothing
// more until the client hangs up, so the client's own timeout ends its read. The caller holds no
// lock of the issuer's.
func breakOff(w http.ResponseWriter, r *http.Request, how cutAnswer) {
	switch how {
	case cutReset:
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
				_ = conn.Close()
				return
			}
		}
	case cutStall:
		<-r.Context().Done()
		return
	}
	panic(http.ErrAbortHandler)
}
