package oidctest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

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
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), testClientID, testClientSecret)
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

// TestDelayTokenWaitsBeforeItAnswers makes the token endpoint wait d. Two refreshes sent together,
// one of them refused, each answer no sooner than d after they were sent and well before 2d, so
// the wait comes before whatever the endpoint answers, and neither request waited behind the
// other's wait. A wait of zero answers at once again.
func TestDelayTokenWaitsBeforeItAnswers(t *testing.T) {
	const d = time.Second
	issuer := codeFlowIssuer(t)
	ada := signIn(t, issuer, "ada@example.com")
	bob := signIn(t, issuer, "bob@example.com")
	issuer.RefuseRefresh("ada@example.com", true)
	issuer.DelayToken(d)

	type answered struct {
		who    string
		status int
		took   time.Duration
		err    error
	}
	answers := make(chan answered, 2)
	start := make(chan struct{})
	for _, person := range []struct{ who, token string }{{"ada", ada}, {"bob", bob}} {
		go func() {
			<-start
			sent := time.Now()
			status, _, err := postToken(context.Background(), issuer, refreshForm(person.token))
			answers <- answered{person.who, status, time.Since(sent), err}
		}()
	}
	close(start)
	want := map[string]int{"ada": http.StatusBadRequest, "bob": http.StatusOK}
	for range 2 {
		got := <-answers
		switch {
		case got.err != nil:
			t.Errorf("%s's refresh: %v", got.who, got.err)
		case got.status != want[got.who]:
			t.Errorf("%s's refresh: status %d, want %d", got.who, got.status, want[got.who])
		case got.took < d || got.took >= 2*d:
			t.Errorf("%s's refresh answered after %v, want at least %v and under %v", got.who, got.took, d, 2*d)
		}
	}

	issuer.DelayToken(0)
	sent := time.Now()
	wantRefreshed(t, issuer, bob, "bob@example.com", "with no wait")
	if took := time.Since(sent); took >= d {
		t.Errorf("bob's refresh with no wait answered after %v, want under %v", took, d)
	}
}

// TestDelayTokenEndsWhenTheCallerHangsUp hangs up on a refresh during the token endpoint's wait:
// the issuer stops waiting and answers nothing, so even past the end of the wait the refresh
// counts as none, and the issuer's server, which a test's cleanup closes, is not held for the
// rest of the wait.
func TestDelayTokenEndsWhenTheCallerHangsUp(t *testing.T) {
	const d = time.Second
	issuer := codeFlowIssuer(t)
	ada := signIn(t, issuer, "ada@example.com")
	before := issuer.Refreshes()
	issuer.DelayToken(d)

	ctx, cancel := context.WithTimeout(context.Background(), d/10)
	defer cancel()
	if status, _, err := postToken(ctx, issuer, refreshForm(ada)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a refresh given up after %v: status %d, %v, want the caller's deadline", d/10, status, err)
	}
	// A wait that went on would answer, and count, the refresh d after it arrived.
	time.Sleep(d + d/2)
	if got := issuer.Refreshes(); got != before {
		t.Fatalf("%v after the caller hung up the issuer counts %d refreshes, want %d", d+d/2, got, before)
	}
}
