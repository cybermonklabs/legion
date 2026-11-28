package oidctest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// authorizeAt sends the authorization request a browser follows for clientID to redirect to
// redirectURI, and answers the status, the Location the issuer sent the browser to and the body,
// following no redirect.
func authorizeAt(t *testing.T, issuer *Issuer, clientID, redirectURI string) (status int, location, body string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(issuer.URL() + "/authorize?" + url.Values{
		"client_id": {clientID}, "response_type": {"code"}, "redirect_uri": {redirectURI}, "state": {"state-1"},
	}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, response.Header.Get("Location"), string(raw)
}

// wantRedirectedTo fails the test unless the authorization answered 302 to target with a code and
// the request's state, and nothing else in its query.
func wantRedirectedTo(t *testing.T, status int, location, body, target string) {
	t.Helper()
	sent, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse Location %q: %v", location, err)
	}
	query := sent.Query()
	sent.RawQuery = ""
	if status != http.StatusFound || sent.String() != target || query.Get("code") == "" || query.Get("state") != "state-1" {
		t.Fatalf("authorization: status %d to %q (%s), want 302 to %s with a code and state state-1", status, location, body, target)
	}
}

// wantRefusedMismatch fails the test unless the authorization was refused 400 as Google refuses a
// redirect URI its client did not register, sending the browser nowhere (RFC 6749 section
// 4.1.2.1).
func wantRefusedMismatch(t *testing.T, redirectURI string, status int, location, body string) {
	t.Helper()
	if status != http.StatusBadRequest || location != "" || !strings.Contains(body, "redirect_uri_mismatch") {
		t.Errorf("authorization to %s: status %d to %q (%s), want 400 redirect_uri_mismatch and no redirect", redirectURI, status, location, strings.TrimSpace(body))
	}
}

// TestAClientWithNoRedirectURIIsRedirectedNowhere arms the code flow for a client that registers
// no redirect URI: its authorization is refused, whatever address it names, and the browser is
// sent nowhere. A client whose callback is unregistered cannot complete a sign-in.
func TestAClientWithNoRedirectURIIsRedirectedNowhere(t *testing.T) {
	issuer := New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), "dispatch-client", "dispatch-secret")
	issuer.SignInByDefault(map[string]any{"sub": "alice@d.example", "email": "alice@d.example"})
	const foreign = "https://attacker.example/collect"
	status, location, body := authorizeAt(t, issuer, "dispatch-client", foreign)
	wantRefusedMismatch(t, foreign, status, location, body)
}

// TestAnAuthorizationRedirectsOnlyToARegisteredRedirectURI registers one callback: an
// authorization naming it is redirected there with its code and state, and one naming any other
// address, another path or another host or the same address with a query or fragment added, is
// refused 400 and sent nowhere.
func TestAnAuthorizationRedirectsOnlyToARegisteredRedirectURI(t *testing.T) {
	const callback = "https://dispatch.example/auth/callback"
	issuer := New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), "dispatch-client", "dispatch-secret", callback)
	issuer.SignInByDefault(map[string]any{"sub": "alice@d.example", "email": "alice@d.example"})

	status, location, body := authorizeAt(t, issuer, "dispatch-client", callback)
	wantRedirectedTo(t, status, location, body, callback)

	for _, foreign := range []string{
		"https://attacker.example/auth/callback",
		"https://dispatch.example/auth/other",
		"https://dispatch.example.attacker.example/auth/callback",
		"https://dispatch.example:8443/auth/callback",
		"http://dispatch.example/auth/callback",
		callback + "?next=https://attacker.example/",
		callback + "#fragment",
	} {
		status, location, body := authorizeAt(t, issuer, "dispatch-client", foreign)
		wantRefusedMismatch(t, foreign, status, location, body)
	}
}

// TestALoopbackRedirectURIMatchesAtAnyPort registers a loopback IP literal callback, as a client
// that binds a port the OS chooses must: RFC 8252 section 7.3 has the issuer take it at whatever
// port the request names, and nothing else about it may differ. The browser goes to the
// registered address at that port.
func TestALoopbackRedirectURIMatchesAtAnyPort(t *testing.T) {
	issuer := New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), "broker-client", "broker-secret",
		"http://127.0.0.1/signin/callback", "http://[::1]/signin/callback")
	issuer.SignInByDefault(map[string]any{"sub": "alice@d.example", "email": "alice@d.example"})

	for _, requested := range []string{
		"http://127.0.0.1/signin/callback",
		"http://127.0.0.1:43123/signin/callback",
		"http://[::1]:43123/signin/callback",
	} {
		status, location, body := authorizeAt(t, issuer, "broker-client", requested)
		wantRedirectedTo(t, status, location, body, requested)
	}
	for _, foreign := range []string{
		"http://127.0.0.1:43123/signin/other",
		"http://localhost:43123/signin/callback",
		"http://127.0.0.2:43123/signin/callback",
		"https://127.0.0.1:43123/signin/callback",
		"http://127.0.0.1:0/signin/callback",
		"http://127.0.0.1:65536/signin/callback",
		"http://127.0.0.1:443x/signin/callback",
		"http://user@127.0.0.1:43123/signin/callback",
		"http://127.0.0.1:43123/signin/callback?next=https://attacker.example/",
		"http://attacker.example:43123/signin/callback",
	} {
		status, location, body := authorizeAt(t, issuer, "broker-client", foreign)
		wantRefusedMismatch(t, foreign, status, location, body)
	}
}

// TestADeviceCodeExpires covers the one shape of the device grant no caller can reach by waiting:
// a device authorization whose life has run out. ExpireDeviceCodes ends the codes issued so far,
// so the person's approval is refused for a code that has expired, and the device's next poll is
// answered expired_token rather than kept pending for ever.
func TestADeviceCodeExpires(t *testing.T) {
	const clientID, clientSecret = "device-dev", "device-dev-client-secret"
	issuer := New(t)
	issuer.EnableDeviceFlow(issuer.PublishKey(t, "signing-key"), clientID, clientSecret)
	issuer.SignInByDefault(map[string]any{"sub": "alice@d.example", "email": "alice@d.example"})

	response, err := http.PostForm(issuer.URL()+"/device_authorization", url.Values{"client_id": {clientID}})
	if err != nil {
		t.Fatal(err)
	}
	var authorization struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	err = json.NewDecoder(response.Body).Decode(&authorization)
	response.Body.Close()
	if err != nil || authorization.DeviceCode == "" || authorization.UserCode == "" {
		t.Fatalf("the device authorization answered %+v (%v), want a device code and a user code", authorization, err)
	}

	issuer.ExpireDeviceCodes()

	if err := issuer.ApproveDeviceCode(authorization.UserCode); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("approving the expired user code: %v, want a refusal saying it has expired", err)
	}
	response, err = http.PostForm(issuer.URL()+"/token", url.Values{
		"grant_type": {deviceCodeGrantType}, "device_code": {authorization.DeviceCode},
		"client_id": {clientID}, "client_secret": {clientSecret},
	})
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Error string `json:"error"`
	}
	err = json.NewDecoder(response.Body).Decode(&answer)
	status := response.StatusCode
	response.Body.Close()
	if err != nil || status != http.StatusBadRequest || answer.Error != "expired_token" {
		t.Fatalf("the poll after the device code expired: status %d, %+v (%v), want 400 expired_token", status, answer, err)
	}
}
