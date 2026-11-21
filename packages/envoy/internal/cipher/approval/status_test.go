package approval_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// signInStatus is GET /signin/status's answer, as a caller with no credential reads it.
type signInStatus struct {
	ClientCheck            *string    `json:"client_check"`
	ClientCheckedAt        *time.Time `json:"client_checked_at"`
	LastSignInAt           *time.Time `json:"last_sign_in_at"`
	LastFailedSignInAt     *time.Time `json:"last_failed_sign_in_at"`
	LastFailedSignInReason *string    `json:"last_failed_sign_in_reason"`
	LastDecisionAt         *time.Time `json:"last_decision_at"`
}

// signInStatus fetches the route as a production check does - a plain client with no cookie, no
// token and no header of its own - and answers its body as read and as parsed, failing t on
// anything but a 200 of JSON.
func (r *rig) signInStatus(t *testing.T) (signInStatus, string) {
	t.Helper()
	res, err := r.srv.Client().Get(r.url + "/signin/status")
	if err != nil {
		t.Fatalf("GET /signin/status: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET /signin/status with no credential = %d %s:\n%s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	var status signInStatus
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("GET /signin/status answered %q: %v", body, err)
	}
	return status, string(body)
}

// TestSignInStatusReportsWhatHappenedOnThePage drives the public route through everything that
// writes it: before anything has happened every fact is null, a sign-in that completes sets its
// own time and nothing else, a decision sets the decision's time, and a sign-in that fails sets
// a coded reason. No answer carries the person's login or the record they decided.
func TestSignInStatusReportsWhatHappenedOnThePage(t *testing.T) {
	r := newRig(t)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")

	status, body := r.signInStatus(t)
	if status != (signInStatus{}) {
		t.Fatalf("GET /signin/status before anything happened = %s, want every fact null", body)
	}
	for _, want := range []string{`"client_check":null`, `"last_sign_in_at":null`, `"last_failed_sign_in_reason":null`} {
		if !strings.Contains(body, want) {
			t.Errorf("the status names no %s: %s", want, body)
		}
	}

	before := time.Now().Add(-time.Second)
	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	status, body = r.signInStatus(t)
	switch {
	case status.LastSignInAt == nil || status.LastSignInAt.Before(before):
		t.Fatalf("after ada signed in the status = %s, want the sign-in's own time", body)
	case status.LastFailedSignInAt != nil || status.LastFailedSignInReason != nil || status.LastDecisionAt != nil:
		t.Fatalf("ada's sign-in wrote a failure or a decision too: %s", body)
	}
	signedIn := *status.LastSignInAt

	b.Decide(t, p.recordID, true)
	status, body = r.signInStatus(t)
	switch {
	case status.LastDecisionAt == nil || status.LastDecisionAt.Before(signedIn):
		t.Fatalf("after ada's decision the status = %s, want the decision's own time", body)
	case !status.LastSignInAt.Equal(signedIn):
		t.Fatalf("the decision moved the last sign-in's time: %s", body)
	}

	// A failure now: a code the provider will not exchange. The facts already written stay.
	b2 := r.browser(t)
	callback := b2.Authorize(t, b2.StartSignIn(t, "/approve/"+p.recordID), person(ada))
	u, err := url.Parse(callback)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("code", "forged-code")
	u.RawQuery = q.Encode()
	if res := b2.Get(t, u.String()); res.Status != http.StatusForbidden {
		t.Fatalf("the callback with a forged code = %d, want 403:\n%s", res.Status, res.Body)
	}
	status, body = r.signInStatus(t)
	switch {
	case status.LastFailedSignInReason == nil || *status.LastFailedSignInReason != "invalid_grant":
		t.Fatalf("after the refused exchange the status = %s, want the reason invalid_grant", body)
	case status.LastFailedSignInAt == nil:
		t.Fatalf("the failed sign-in has no time: %s", body)
	case !status.LastSignInAt.Equal(signedIn) || status.LastDecisionAt == nil:
		t.Fatalf("the failure lost an earlier fact: %s", body)
	}

	// Nothing in the answer names a person, a record or a secret.
	for _, forbidden := range []string{ada, ben, p.recordID, p.requestID, "TEST_SECRET", "forged-code"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the public status carries %q: %s", forbidden, body)
		}
	}
}

// TestSignInStatusWritesAFailedSignInWithACodeFromTheClosedSet drives each failure the codes name through
// the real callback and pins the word written for it. A failure none of them names writes
// nothing at all rather than a word meaning "something else": the last named failure stands, at
// the time it was written. The token endpoint's every answer but an outage (a 5xx, a 408 or a 429,
// TestSignInCallbackAnswersATokenEndpointThatCannotAnswer503) is the endpoint answering, refused
// 403: its OAuth error is written when the closed set names it, and a 4xx naming no error, or a 200
// naming one, writes nothing.
func TestSignInStatusWritesAFailedSignInWithACodeFromTheClosedSet(t *testing.T) {
	for _, c := range []struct {
		name string
		// spoil changes the claims of every ID token the provider mints, before the sign-in.
		spoil func(claims map[string]any)
		// claims is what the person signs in as, person(ada) when nil.
		claims map[string]any
		// sign re-arms the provider's client with a key it does not publish.
		foreignKey bool
		// tokenStatus, when set, has the token endpoint answer the exchange with that status and
		// tokenCode as its OAuth error.
		tokenStatus int
		tokenCode   string
		want        string
	}{
		{name: "a wrong issuer", spoil: func(c map[string]any) { c["iss"] = "https://issuer.example.com" }, want: "bad_issuer"},
		{name: "a wrong audience", spoil: func(c map[string]any) { c["aud"] = "another-client" }, want: "bad_audience"},
		{name: "a wrong nonce", spoil: func(c map[string]any) { c["nonce"] = "another-nonce" }, want: "bad_nonce"},
		{name: "a signature by a key the provider does not serve", foreignKey: true, want: "bad_signature"},
		{name: "an account of another domain", claims: map[string]any{"sub": "sub-" + ada, "email": ada, "email_verified": true, "hd": "other.example"}, want: "not_in_domain"},
		{name: "an unverified email", claims: map[string]any{"sub": "sub-" + ada, "email": ada, "email_verified": false, "hd": domain}, want: "email_not_verified"},
		{name: "an ID token naming no email", claims: map[string]any{"sub": "sub-" + ada, "email_verified": true, "hd": domain}, want: ""},
		{name: "an expired ID token", spoil: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, want: ""},
		{name: "a 401 naming invalid_client", tokenStatus: http.StatusUnauthorized, tokenCode: "invalid_client", want: "invalid_client"},
		{name: "a 403 naming invalid_client", tokenStatus: http.StatusForbidden, tokenCode: "invalid_client", want: "invalid_client"},
		{name: "a 403 naming unauthorized_client", tokenStatus: http.StatusForbidden, tokenCode: "unauthorized_client", want: "unauthorized_client"},
		{name: "a 400 naming no error", tokenStatus: http.StatusBadRequest, want: ""},
		{name: "a 401 naming no error", tokenStatus: http.StatusUnauthorized, want: ""},
		{name: "a 404 naming no error", tokenStatus: http.StatusNotFound, want: ""},
		{name: "a 200 naming invalid_grant", tokenStatus: http.StatusOK, tokenCode: "invalid_grant", want: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			enrollmentID, key := r.benSession(t, "ben-box")
			p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
			// One named failure first, so a case that must write nothing is seen not to: the
			// reason afterwards is still this one.
			r.failSignIn(t, p.recordID, nil, false, func(u *url.URL) {
				q := u.Query()
				q.Set("code", "forged-code")
				u.RawQuery = q.Encode()
			})
			first, body := r.signInStatus(t)
			if first.LastFailedSignInReason == nil || *first.LastFailedSignInReason != "invalid_grant" || first.LastFailedSignInAt == nil {
				t.Fatalf("the first refused sign-in = %s, want invalid_grant", body)
			}

			if c.spoil != nil {
				r.issuer.SpoilIDTokens(c.spoil)
				t.Cleanup(func() { r.issuer.SpoilIDTokens(nil) })
			}
			if c.tokenStatus != 0 {
				r.issuer.FailToken(c.tokenStatus, c.tokenCode)
				t.Cleanup(func() { r.issuer.FailToken(0, "") })
			}
			claims := c.claims
			if claims == nil {
				claims = person(ada)
			}
			r.failSignIn(t, p.recordID, claims, c.foreignKey, nil)

			status, body := r.signInStatus(t)
			want := c.want
			if want == "" {
				want = "invalid_grant" // unchanged: the failure this case makes has no code
				if status.LastFailedSignInAt == nil || !status.LastFailedSignInAt.Equal(*first.LastFailedSignInAt) {
					t.Fatalf("after %s the failure time = %s, want the first failure's, unchanged", c.name, body)
				}
			}
			if status.LastFailedSignInReason == nil || *status.LastFailedSignInReason != want {
				t.Fatalf("after %s the reason = %s, want %q", c.name, body, want)
			}
			if r.sessions(t) != 0 {
				t.Fatalf("a refused sign-in made a session")
			}
		})
	}
}

// failSignIn takes one sign-in at the provider as claims and sends its callback back to the
// broker, which must refuse it (signInCallback).
func (r *rig) failSignIn(t *testing.T, recordID string, claims map[string]any, foreignKey bool, spoilCallback func(*url.URL)) {
	t.Helper()
	if res := r.signInCallback(t, recordID, claims, foreignKey, spoilCallback); res.Status != http.StatusForbidden {
		t.Fatalf("the refused callback = %d, want 403:\n%s", res.Status, res.Body)
	}
}

// signInCallback takes one sign-in at the provider as claims, person(ada) when nil, sends its
// callback back to the broker and answers what the broker answered. foreignKey re-arms the
// provider's client with a key of the same id that it does not publish, so its ID tokens do not
// verify; spoilCallback, when given, changes the callback URL before the browser follows it.
func (r *rig) signInCallback(t *testing.T, recordID string, claims map[string]any, foreignKey bool, spoilCallback func(*url.URL)) ciphertest.Response {
	t.Helper()
	if claims == nil {
		claims = person(ada)
	}
	b := r.browser(t)
	authURL := b.StartSignIn(t, "/approve/"+recordID)
	if foreignKey {
		// Under the published key's own id, so the key set resolves and only the signature
		// fails - oidctest.Key.KeyID's one shape.
		r.issuer.EnableCodeFlow(oidctest.NewKey(t, "signing-key"), clientID, ciphertest.SignInClientSecret)
	}
	callback := b.Authorize(t, authURL, claims)
	u, err := url.Parse(callback)
	if err != nil {
		t.Fatal(err)
	}
	if spoilCallback != nil {
		spoilCallback(u)
	}
	return b.Get(t, u.String())
}

// TestSignInCallbackAnswersATokenEndpointThatCannotAnswer503: a callback whose code exchange the
// provider's token endpoint cannot answer (a 5xx, a 408 or a 429, whatever error its body names,
// or no answer at all) is the provider unreachable. The person is answered 503 "Sign-in
// unavailable", no session is made, and the status writes issuer_unreachable. Every other answer is
// the token endpoint answering, refused 403 as before: a 400 or 403 naming invalid_grant writes
// invalid_grant, and a 4xx naming no OAuth error, a 200 naming one and a 3xx write nothing (want
// empty).
func TestSignInCallbackAnswersATokenEndpointThatCannotAnswer503(t *testing.T) {
	for _, c := range []struct {
		name   string
		fail   func(issuer *oidctest.Issuer)
		status int
		want   string
	}{
		{"503 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusServiceUnavailable, "") }, http.StatusServiceUnavailable, "issuer_unreachable"},
		{"503 invalid_client", func(i *oidctest.Issuer) { i.FailToken(http.StatusServiceUnavailable, "invalid_client") }, http.StatusServiceUnavailable, "issuer_unreachable"},
		{"503 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusServiceUnavailable, "invalid_grant") }, http.StatusServiceUnavailable, "issuer_unreachable"},
		{"500 redirect_uri_mismatch", func(i *oidctest.Issuer) { i.FailToken(http.StatusInternalServerError, "redirect_uri_mismatch") }, http.StatusServiceUnavailable, "issuer_unreachable"},
		{"429 unauthorized_client", func(i *oidctest.Issuer) { i.FailToken(http.StatusTooManyRequests, "unauthorized_client") }, http.StatusServiceUnavailable, "issuer_unreachable"},
		{"429 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusTooManyRequests, "invalid_grant") }, http.StatusServiceUnavailable, "issuer_unreachable"},
		{"500 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusInternalServerError, "invalid_grant") }, http.StatusServiceUnavailable, "issuer_unreachable"},
		{"408 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusRequestTimeout, "invalid_grant") }, http.StatusServiceUnavailable, "issuer_unreachable"},
		{"no answer", func(i *oidctest.Issuer) { i.DropToken() }, http.StatusServiceUnavailable, "issuer_unreachable"},
		{"400 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusBadRequest, "invalid_grant") }, http.StatusForbidden, "invalid_grant"},
		{"403 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusForbidden, "invalid_grant") }, http.StatusForbidden, "invalid_grant"},
		{"400 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusBadRequest, "") }, http.StatusForbidden, ""},
		{"401 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusUnauthorized, "") }, http.StatusForbidden, ""},
		{"403 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusForbidden, "") }, http.StatusForbidden, ""},
		{"404 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusNotFound, "") }, http.StatusForbidden, ""},
		{"200 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusOK, "invalid_grant") }, http.StatusForbidden, ""},
		{"302 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusFound, "") }, http.StatusForbidden, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			enrollmentID, key := r.benSession(t, "ben-box")
			p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
			c.fail(r.issuer)
			t.Cleanup(func() { r.issuer.FailToken(0, "") })
			res := r.signInCallback(t, p.recordID, nil, false, nil)
			unavailable := strings.Contains(res.Body, "Sign-in unavailable")
			if res.Status != c.status || unavailable != (c.status == http.StatusServiceUnavailable) {
				t.Fatalf("the callback whose token endpoint answers %s = %d, want %d, the page saying Sign-in unavailable: %t:\n%s",
					c.name, res.Status, c.status, c.status == http.StatusServiceUnavailable, res.Body)
			}
			status, body := r.signInStatus(t)
			if c.want == "" {
				if status.LastFailedSignInReason != nil || status.LastFailedSignInAt != nil {
					t.Fatalf("after a callback whose token endpoint answers %s the status = %s, want no failure written", c.name, body)
				}
			} else if status.LastFailedSignInReason == nil || *status.LastFailedSignInReason != c.want {
				t.Fatalf("after a callback whose token endpoint answers %s the reason = %s, want %q", c.name, body, c.want)
			}
			if r.sessions(t) != 0 {
				t.Fatalf("a callback whose token endpoint answers %s made a session", c.name)
			}
		})
	}
}

// TestSignInCallbackBoundsItsCodeBeforeTheExchange: the callback's code is chosen by whoever holds
// the browser, so the broker sends the provider's token endpoint a code of at most 2048 bytes. One
// of 2048 is exchanged (the provider refuses it, a code it never issued); one of 2049 is refused
// 400 SIGN_IN_CODE_INVALID with no token request and nothing written to the sign-in status.
func TestSignInCallbackBoundsItsCodeBeforeTheExchange(t *testing.T) {
	for _, c := range []struct {
		size      int
		exchanged bool
	}{
		{2048, true},
		{2049, false},
	} {
		t.Run(strconv.Itoa(c.size), func(t *testing.T) {
			r := newRig(t)
			enrollmentID, key := r.benSession(t, "ben-box")
			p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
			before := len(r.issuer.TokenRequests())
			res := r.signInCallback(t, p.recordID, nil, false, func(u *url.URL) {
				q := u.Query()
				q.Set("code", strings.Repeat("c", c.size))
				u.RawQuery = q.Encode()
			})
			sent := len(r.issuer.TokenRequests()) - before
			if c.exchanged {
				if sent != 1 || res.Status != http.StatusForbidden {
					t.Fatalf("a callback with a %d-byte code = %d after %d token requests, want 403 after one:\n%s", c.size, res.Status, sent, res.Body)
				}
				return
			}
			if sent != 0 || res.Status != http.StatusBadRequest || !strings.Contains(res.Body, "Sign-in refused") {
				t.Fatalf("a callback with a %d-byte code = %d after %d token requests, want 400 Sign-in refused after none:\n%s", c.size, res.Status, sent, res.Body)
			}
			if status, body := r.signInStatus(t); status.LastFailedSignInReason != nil || status.LastFailedSignInAt != nil {
				t.Fatalf("a callback refused for its code's size wrote the sign-in status %s, want nothing", body)
			}
			if r.sessions(t) != 0 {
				t.Fatalf("a callback with a %d-byte code made a session", c.size)
			}
		})
	}
}

// TestSignInStatusWritesIssuerUnreachableWhenTheProviderCannotBeReached: a callback is a sign-in
// holding a code, so a provider that cannot be read there is an exchange that could not be made.
func TestSignInStatusWritesIssuerUnreachableWhenTheProviderCannotBeReached(t *testing.T) {
	r := newRig(t)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")

	// The sign-in in progress is written by hand, not started on the page: the page reads the
	// provider's discovery document when it starts one, and a read that has succeeded is kept
	// for the life of the process, so a sign-in started here would leave the callback with a
	// provider it could still reach.
	r.issuer.FailDiscovery(http.StatusInternalServerError)
	pending := map[string]string{
		"state": "a-state", "nonce": "a-nonce", "verifier": strings.Repeat("v", 43),
		"next": "/approve/" + p.recordID,
	}
	res := r.getWith(t, "/signin/callback?"+url.Values{"code": {"a-code"}, "state": {pending["state"]}}.Encode(),
		&http.Cookie{Name: "__Host-broker-signin", Value: encodePending(t, pending)})
	if res.Status != http.StatusServiceUnavailable {
		t.Fatalf("the callback with the provider down = %d, want 503:\n%s", res.Status, res.Body)
	}
	status, body := r.signInStatus(t)
	if status.LastFailedSignInReason == nil || *status.LastFailedSignInReason != "issuer_unreachable" {
		t.Fatalf("the status after a callback the provider could not answer = %s, want issuer_unreachable", body)
	}
}

// TestSignInStatusWritesNoDecisionWhenThePostDecidedNothing pins the one fact the rollout reads
// this field for: the spec's step 3 contracts the old approval paths away "once GET /signin/status
// on the production broker shows a decision made on a page", and that decision is "an approval or
// a denial". A POST that decided neither must leave the time where it was, or the gate opens on a
// page nobody ever decided anything on.
//
// The resend here is the ordinary way to make one: a browser repeating the decision form it
// already sent (a second click, a back-and-resubmit, a retried request). It is the page's own
// form, from a live session, bound to a record the person may decide, and the broker decides
// nothing by it, because the record is already terminal.
func TestSignInStatusWritesNoDecisionWhenThePostDecidedNothing(t *testing.T) {
	r := newRig(t)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	form := ciphertest.DecisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
	if form == nil {
		t.Fatalf("the page for %s offers no decision", p.recordID)
	}
	if res := b.FromPage(t, "/approve", ciphertest.Decision(form, "approve")); res.Status != http.StatusSeeOther {
		t.Fatalf("ada's approval = %d, want 303:\n%s", res.Status, res.Body)
	}
	status, body := r.signInStatus(t)
	if status.LastDecisionAt == nil {
		t.Fatalf("after ada's approval the status = %s, want the decision's own time", body)
	}
	decided := *status.LastDecisionAt

	// Far enough apart that two writes cannot share a timestamp.
	time.Sleep(20 * time.Millisecond)
	if res := b.FromPage(t, "/approve", ciphertest.Decision(form, "approve")); res.Status != http.StatusSeeOther {
		t.Fatalf("the resend of the same form = %d, want 303:\n%s", res.Status, res.Body)
	}
	if n := r.grants(t, p.recordID); n != 1 {
		t.Fatalf("the resend left %d grants on %s, want the first approval's one", n, p.recordID)
	}
	if got := r.state(t, p.recordID); got != "approved" {
		t.Fatalf("the resend left %s in %q, want approved", p.recordID, got)
	}
	status, body = r.signInStatus(t)
	switch {
	case status.LastDecisionAt == nil:
		t.Fatalf("the resend cleared the last decision's time: %s", body)
	case !status.LastDecisionAt.Equal(decided):
		t.Fatalf("a POST that decided nothing moved the last decision's time from %s to %s: %s",
			decided.Format(time.RFC3339Nano), status.LastDecisionAt.Format(time.RFC3339Nano), body)
	}
}
