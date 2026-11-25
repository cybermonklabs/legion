package oidctest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
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

const (
	testClientID     = "dispatch-dev"
	testClientSecret = "fixture"
	testRedirect     = "http://127.0.0.1:1/auth/callback"
)

// codeFlowIssuer starts an issuer serving the code flow for testClientID.
func codeFlowIssuer(t *testing.T) *Issuer {
	t.Helper()
	issuer := New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), testClientID, testClientSecret, testRedirect)
	return issuer
}

// tokenAnswer is what the token endpoint answers: a grant's tokens, or a refusal's OAuth error.
type tokenAnswer struct {
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
}

// postToken posts form to issuer's token endpoint as testClientID, and answers the status and the
// JSON answer, which is empty when the body is not JSON. It reports a failure rather than failing
// the test, so a goroutine other than the test's own can call it.
func postToken(ctx context.Context, issuer *Issuer, form url.Values) (int, tokenAnswer, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer.URL()+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, tokenAnswer{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(testClientID, testClientSecret)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, tokenAnswer{}, err
	}
	defer response.Body.Close()
	var answer tokenAnswer
	_ = json.NewDecoder(response.Body).Decode(&answer)
	return response.StatusCode, answer, nil
}

// refreshForm is the refresh grant of refreshToken.
func refreshForm(refreshToken string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}
}

// refresh refreshes refreshToken at issuer as testClientID.
func refresh(t *testing.T, issuer *Issuer, refreshToken string) (int, tokenAnswer) {
	t.Helper()
	status, answer, err := postToken(context.Background(), issuer, refreshForm(refreshToken))
	if err != nil {
		t.Fatalf("POST /token, a refresh: %v", err)
	}
	return status, answer
}

// signIn signs email in through issuer's code flow, as a browser and then the client do, and
// answers the refresh token the code exchange issues.
func signIn(t *testing.T, issuer *Issuer, email string) string {
	t.Helper()
	issuer.SignInAs(map[string]any{"sub": email, "email": email, "email_verified": true})
	code, _ := issuer.Authorize(t, issuer.URL()+"/authorize?"+url.Values{
		"client_id": {testClientID}, "response_type": {"code"}, "redirect_uri": {testRedirect},
		"state": {"state"}, "nonce": {"nonce"},
	}.Encode())
	status, answer, err := postToken(context.Background(), issuer, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
	})
	if err != nil || status != http.StatusOK || answer.RefreshToken == "" {
		t.Fatalf("%s's code exchange: status %d, error %q (%v), want 200 with a refresh token", email, status, answer.Error, err)
	}
	return answer.RefreshToken
}

// idTokenEmail is the email claim of the compact JWT raw. Nothing here checks its signature: the
// issuer under test signed it, and the test reads whom it names.
func idTokenEmail(t *testing.T, raw string) string {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("the ID token %q is not a compact JWT", raw)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims.Email
}

// wantRefreshed refreshes refreshToken and fails the test, saying when, unless the issuer serves
// it with an ID token naming email.
func wantRefreshed(t *testing.T, issuer *Issuer, refreshToken, email, when string) {
	t.Helper()
	status, answer := refresh(t, issuer, refreshToken)
	if status != http.StatusOK {
		t.Fatalf("%s's refresh %s: status %d, error %q, want 200", email, when, status, answer.Error)
	}
	if got := idTokenEmail(t, answer.IDToken); got != email {
		t.Fatalf("%s's refresh %s: the ID token names %q, want %s", email, when, got, email)
	}
}

// wantRefused refreshes refreshToken and fails the test, saying when, unless the issuer answers
// status with the OAuth error code, none when code is empty.
func wantRefused(t *testing.T, issuer *Issuer, refreshToken string, status int, code, when string) {
	t.Helper()
	if got, answer := refresh(t, issuer, refreshToken); got != status || answer.Error != code || answer.IDToken != "" {
		t.Fatalf("the refresh %s: status %d, error %q, ID token %t, want %d with error %q and no ID token", when, got, answer.Error, answer.IDToken != "", status, code)
	}
}

// TestRefuseRefreshRefusesOnePerson refuses ada's refresh, as Google does once it no longer
// confirms her account: her refresh is answered 400 invalid_grant, the answer Dispatch reads as her
// sign-in being over, and bob's is served. Her address matches whatever its case. The refusal is
// the person's, not one refresh token's: a sign-in of hers is still served, and the refresh token
// it issues is refused too. It revokes nothing: once it is turned off, both of her refresh tokens
// are served again.
func TestRefuseRefreshRefusesOnePerson(t *testing.T) {
	issuer := codeFlowIssuer(t)
	ada := signIn(t, issuer, "ada@example.com")
	bob := signIn(t, issuer, "bob@example.com")

	issuer.RefuseRefresh("Ada@Example.COM", true)
	wantRefused(t, issuer, ada, http.StatusBadRequest, "invalid_grant", "of ada while she is refused")
	wantRefreshed(t, issuer, bob, "bob@example.com", "while ada is refused")
	again := signIn(t, issuer, "ada@example.com")
	wantRefused(t, issuer, again, http.StatusBadRequest, "invalid_grant", "of ada's new sign-in while she is refused")

	issuer.RefuseRefresh("ada@example.com", false)
	wantRefreshed(t, issuer, ada, "ada@example.com", "once her refusal is off")
	wantRefreshed(t, issuer, again, "ada@example.com", "of her new sign-in once her refusal is off")
}

// TestFailRefreshForFailsOnePerson makes ada's refresh fail 503, as Google's does when it cannot
// answer now: hers is answered 503 with no OAuth error, which Dispatch reads as Google not
// reached rather than as her sign-in being over, and bob's is served. A status of 0 serves hers
// again. FailRefreshFor and RefuseRefresh set one answer per person, the later replacing the
// earlier.
func TestFailRefreshForFailsOnePerson(t *testing.T) {
	issuer := codeFlowIssuer(t)
	ada := signIn(t, issuer, "ada@example.com")
	bob := signIn(t, issuer, "bob@example.com")

	issuer.FailRefreshFor("ada@example.com", http.StatusServiceUnavailable)
	wantRefused(t, issuer, ada, http.StatusServiceUnavailable, "", "of ada while hers fails")
	wantRefreshed(t, issuer, bob, "bob@example.com", "while ada's fails")

	issuer.FailRefreshFor("ada@example.com", 0)
	wantRefreshed(t, issuer, ada, "ada@example.com", "once her failure is off")

	issuer.RefuseRefresh("ada@example.com", true)
	issuer.FailRefreshFor("ada@example.com", http.StatusServiceUnavailable)
	wantRefused(t, issuer, ada, http.StatusServiceUnavailable, "", "of ada failed after she was refused")
	issuer.FailRefreshFor("ada@example.com", 0)
	wantRefreshed(t, issuer, ada, "ada@example.com", "once the failure that replaced her refusal is off")
}

// heldWait is one wait an issuer started while holdWaits held them: the request it holds back,
// the length it was told to wait, and end, a send on which ends it as its time running out would.
type heldWait struct {
	request *http.Request
	delay   time.Duration
	end     chan<- time.Time
}

// holdWaits makes every wait an issuer's endpoints start, until the test ends, last until the test
// ends it rather than until its time is up: each wait is sent on the channel holdWaits answers as
// it starts, and it ends only on a send to its end or on its caller hanging up. A wait the test
// leaves held is ended at cleanup, before the issuer's server is closed, so a test that fails with
// a wait held fails rather than hangs. Every issuer's waits read the one waitTimer, so a test that
// calls this does not run in parallel.
func holdWaits(t *testing.T) <-chan heldWait {
	t.Helper()
	var (
		mu       sync.Mutex
		ends     []chan time.Time
		finished bool
	)
	started := make(chan heldWait, 8)
	previous := waitTimer
	waitTimer = func(r *http.Request, delay time.Duration) <-chan time.Time {
		end := make(chan time.Time)
		mu.Lock()
		over := finished
		if over {
			close(end)
		} else {
			ends = append(ends, end)
		}
		mu.Unlock()
		if !over {
			started <- heldWait{request: r, delay: delay, end: end}
		}
		return end
	}
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		finished = true
		for _, end := range ends {
			close(end)
		}
		waitTimer = previous
	})
	return started
}

// TestDelayTokenWaitsBeforeItAnswers makes the token endpoint wait d. Two refreshes are sent
// together, and both are held in a wait of d at once, so neither waits behind the other's wait;
// while they wait, neither is answered or counted. Ada is refused only while both wait, and once
// their waits end her refresh is refused and bob's is served: the wait comes before whatever the
// endpoint answers, and the answer is the one the issuer's state gives once the wait is over. With
// the wait set to zero a refresh starts no wait and is answered at once. holdWaits ends each wait
// when the test says, so what this reads is the order of events, never a clock.
func TestDelayTokenWaitsBeforeItAnswers(t *testing.T) {
	const d = time.Hour
	issuer := codeFlowIssuer(t)
	ada := signIn(t, issuer, "ada@example.com")
	bob := signIn(t, issuer, "bob@example.com")
	waits := holdWaits(t)
	issuer.DelayToken(d)
	before := issuer.Refreshes()

	type answered struct {
		who    string
		status int
		err    error
	}
	answers := make(chan answered, 3)
	send := func(who, refreshToken string) {
		go func() {
			status, _, err := postToken(context.Background(), issuer, refreshForm(refreshToken))
			answers <- answered{who, status, err}
		}()
	}
	send("ada", ada)
	send("bob", bob)
	var held []heldWait
	for len(held) < 2 {
		select {
		case hold := <-waits:
			if hold.delay != d {
				t.Errorf("a refresh is held in a wait of %v, want %v", hold.delay, d)
			}
			held = append(held, hold)
		case got := <-answers:
			t.Fatalf("%s's refresh was answered (status %d, %v) with %d of the two refreshes held in a wait, want both held before either is answered", got.who, got.status, got.err, len(held))
		}
	}
	if got := issuer.Refreshes(); got != before {
		t.Errorf("with both refreshes held in their wait the issuer counts %d refreshes, want %d", got, before)
	}

	issuer.RefuseRefresh("ada@example.com", true)
	for _, hold := range held {
		hold.end <- time.Time{}
	}
	want := map[string]int{"ada": http.StatusBadRequest, "bob": http.StatusOK}
	for range 2 {
		switch got := <-answers; {
		case got.err != nil:
			t.Errorf("%s's refresh: %v", got.who, got.err)
		case got.status != want[got.who]:
			t.Errorf("%s's refresh once the waits ended: status %d, want %d", got.who, got.status, want[got.who])
		}
	}

	issuer.DelayToken(0)
	send("bob", bob)
	select {
	case hold := <-waits:
		t.Errorf("with the wait set to zero bob's refresh is held in a wait of %v, want it answered at once", hold.delay)
	case got := <-answers:
		if got.err != nil || got.status != http.StatusOK {
			t.Errorf("bob's refresh with the wait set to zero: status %d, %v, want 200", got.status, got.err)
		}
	}
}

// TestDelayTokenEndsWhenTheCallerHangsUp hangs up on a refresh the token endpoint holds in its
// wait. The wait ends then and no longer listens for its end, so the issuer's server, which a
// test's cleanup closes, closes without the rest of the wait; and the issuer answers nothing, so
// the refresh counts as none. The wait is long and nothing but the hang-up ends it, so what this
// reads is the order of events, never a clock.
func TestDelayTokenEndsWhenTheCallerHangsUp(t *testing.T) {
	issuer := codeFlowIssuer(t)
	ada := signIn(t, issuer, "ada@example.com")
	waits := holdWaits(t)
	issuer.DelayToken(time.Hour)
	before := issuer.Refreshes()

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	gaveUp := make(chan error, 1)
	go func() {
		_, _, err := postToken(ctx, issuer, refreshForm(ada))
		gaveUp <- err
	}()
	var hold heldWait
	select {
	case hold = <-waits:
	case err := <-gaveUp:
		t.Fatalf("the refresh was answered (%v) before it was held in a wait", err)
	}
	hangUp()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Errorf("the refresh hung up on: %v, want the caller's cancellation", err)
	}
	// The issuer's server sees the hang-up once the connection closes, which follows the caller's
	// giving up rather than coming with it. Once it has, the wait no longer listens for its end:
	// the server closes, and a send to the end finds nobody to take it.
	<-hold.request.Context().Done()
	closed := make(chan struct{})
	go func() {
		issuer.server.Close()
		close(closed)
	}()
	select {
	case hold.end <- time.Time{}:
		t.Errorf("the wait still listened for its end after the caller hung up")
		<-closed
	case <-closed:
	}
	if got := issuer.Refreshes(); got != before {
		t.Errorf("after the caller hung up the issuer counts %d refreshes, want %d", got, before)
	}
}
