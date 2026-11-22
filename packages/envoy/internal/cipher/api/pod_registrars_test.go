// pod_registrars_test.go drives BROKER_POD_REGISTRARS and BROKER_POD_REGISTRATION_AUDIENCE at POST
// /v1/oauth/exchange/pod-sessions on the real broker api_test.go mounts: which logins the route
// admits for which pods, and that the registration audience is accepted there and nowhere else.
package api_test

import (
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// sessionAccount is the account a session pod runs as: one outside Legion that
// BROKER_SERVICE_LOGINS lists so the pod can enrol itself, and that no BROKER_POD_REGISTRARS entry
// names.
const sessionAccount = "system:serviceaccount:example-ns:example-session"

// notListed is the text of the route's POD_REGISTRAR_NOT_LISTED refusal of registrar registering a
// pod of pod.
func notListed(registrar, pod string) string {
	return registrar + " is not listed to register pods of " + pod + " (BROKER_POD_REGISTRARS)"
}

// refusedNotListed fails the test unless status and body are 403 POD_REGISTRAR_NOT_LISTED
// reading notListed(registrar, pod), what naming the registration it refuses.
func refusedNotListed(t *testing.T, what string, status int, body []byte, registrar, pod string) {
	t.Helper()
	want := notListed(registrar, pod)
	if refusal := decode[wireError](t, body); status != http.StatusForbidden || refusal.Code != "POD_REGISTRAR_NOT_LISTED" || refusal.Error != want {
		t.Fatalf("%s = %d %s, want 403 POD_REGISTRAR_NOT_LISTED reading %q", what, status, body, want)
	}
}

// audienceToken is a projected ServiceAccount token of the test server's issuer for account and
// audience, bound to podUID ("" for none), issued a few seconds ago and expiring lifetime from now
// (negative for one already expired): accountToken at an audience of the test's choosing.
func (ts *testServer) audienceToken(t *testing.T, account, podUID, audience string, lifetime time.Duration) string {
	t.Helper()
	claims := ts.podIssuer.Claims(account, audience)
	now := time.Now()
	claims["iat"] = now.Add(min(lifetime, 0) - 5*time.Second).Unix()
	claims["nbf"] = claims["iat"]
	claims["exp"] = now.Add(lifetime).Unix()
	if podUID != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	}
	return ts.podIssuer.Mint(t, ts.podKey, claims)
}

// TestASessionAccountLoginCannotRegister: a session pod logs in as a service, since
// BROKER_SERVICE_LOGINS lists its account so it can enrol itself, and so reaches the registration
// route with its own token. No BROKER_POD_REGISTRARS entry names its account, so whatever pod token
// it holds, a Legion pod's or its own, the registration is 403 POD_REGISTRAR_NOT_LISTED naming both
// accounts, and nothing is written: a login alone registers nothing. Its own pod is refused so
// too, before POD_CALLER_IS_THE_POD, since the list check comes first.
func TestASessionAccountLoginCannotRegister(t *testing.T) {
	ts := newTestServerWith(t, withExchangeAccounts(t, workerAccount, sessionAccount))
	ts.ServiceLogins[sessionAccount] = "example-session"
	login := ts.accountToken(t, sessionAccount, "pod-uid-session", time.Hour)
	for _, c := range []struct{ name, podToken, podAccount string }{
		{"a Legion pod", ts.accountToken(t, workerAccount, "pod-uid-worker", time.Hour), workerAccount},
		{"its own pod", login, sessionAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := ts.registerPodSession(t, login, c.podToken, "implementer-g1")
			refusedNotListed(t, sessionAccount+", a service login no entry names, registering "+c.name, status, body, sessionAccount, c.podAccount)
			if rows := ts.column(t, `select pod_uid from oauth_pod_sessions`); len(rows) != 0 {
				t.Fatalf("the refused registration wrote %v", rows)
			}
		})
	}
}

// TestARegistrarRegistersOnlyItsListedAccountsPods: an entry pairs a registrar with one pod
// account and admits that pairing alone. The daemon listed for the worker's account, presenting a
// pod token of another account, is 403 POD_REGISTRAR_NOT_LISTED naming the daemon and that
// account; the same login with a pod token of the worker's account is admitted, so the refusal is
// the pairing and not the login; and with the setting unset that admitted registration is 403
// POD_REGISTRAR_NOT_LISTED, since no list admits nothing.
func TestARegistrarRegistersOnlyItsListedAccountsPods(t *testing.T) {
	listing := func(registrars api.PodRegistrars) func(*api.Deps) {
		exchange := withExchangeAccounts(t, workerAccount, otherAccount)
		return func(d *api.Deps) {
			exchange(d)
			d.PodRegistrars = registrars
		}
	}
	ts := newTestServerWith(t, listing(api.PodRegistrars{{Registrar: daemonAccount, Pod: workerAccount}: {}}))
	registrar := ts.accountToken(t, daemonAccount, "", time.Hour)

	status, body := ts.registerPodSession(t, registrar, ts.accountToken(t, otherAccount, "pod-uid-other", time.Hour), "implementer-g1")
	refusedNotListed(t, daemonAccount+", listed for "+workerAccount+" alone, registering a pod of "+otherAccount, status, body, daemonAccount, otherAccount)
	status, body = ts.registerPodSession(t, registrar, ts.accountToken(t, workerAccount, "pod-uid-worker", time.Hour), "implementer-g1")
	if registration := decode[wirePodSession](t, body); status != http.StatusOK || registration.PodUID != "pod-uid-worker" {
		t.Fatalf("%s registering a pod of %s, the account it is listed for = %d %s, want 200 naming that pod: the refusal above is the pairing, not the login", daemonAccount, workerAccount, status, body)
	}
	if rows := ts.column(t, `select pod_uid from oauth_pod_sessions`); !slices.Equal(rows, []string{"pod-uid-worker"}) {
		t.Fatalf("oauth_pod_sessions holds %v, want the listed pairing's registration alone", rows)
	}

	unset := newTestServerWith(t, listing(nil))
	status, body = unset.registerPodSession(t, unset.accountToken(t, daemonAccount, "", time.Hour), unset.accountToken(t, workerAccount, "pod-uid-worker", time.Hour), "implementer-g1")
	refusedNotListed(t, "with BROKER_POD_REGISTRARS unset, "+daemonAccount+" registering a pod of "+workerAccount, status, body, daemonAccount, workerAccount)
	if rows := unset.column(t, `select pod_uid from oauth_pod_sessions`); len(rows) != 0 {
		t.Fatalf("with BROKER_POD_REGISTRARS unset, the refused registration wrote %v", rows)
	}
}

// exchanged is the token exchange's answer as a test reads it: its status, OAuth's error code and
// its description, and whether it issued an access token, which no failure message prints.
type exchanged struct {
	status            int
	code, description string
	issued            bool
}

// exchangePodToken trades subjectToken for session at POST /v1/oauth/token as a pod's RFC 8693
// exchange does, with a DPoP proof of a key of its own, and answers what the exchange answered.
func (ts *testServer) exchangePodToken(t *testing.T, subjectToken, session string) exchanged {
	t.Helper()
	return ts.exchangeProvedBy(t, newSigningKey(t), subjectToken, session)
}

// exchangeProvedBy is exchangePodToken with a DPoP proof of key, so the trade is the one a role
// holding key makes, which a registration bound to key's thumbprint admits.
func (ts *testServer) exchangeProvedBy(t *testing.T, key *ecdsa.PrivateKey, subjectToken, session string) exchanged {
	t.Helper()
	form := url.Values{
		"grant_type": {cipherclient.GrantTypeTokenExchange}, "client_id": {oauthwire.ExchangeClientID},
		"subject_token": {subjectToken}, oauth.FormSubjectTokenType: {oauthwire.SubjectTokenTypeKubernetes},
		oauthwire.FormSession: {session},
	}
	client := &http.Client{Transport: cipherclient.NewDPoPTransport(key, http.DefaultTransport)}
	answer := send(t, client, request(t, http.MethodPost, ts.URL+"/v1/oauth/token", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(answer.body, &body); err != nil {
		t.Fatalf("decode the exchange's answer, status %d: %v", answer.status, err)
	}
	return exchanged{status: answer.status, code: body.Error, description: body.Description, issued: body.AccessToken != ""}
}

// rolesPodToken is pod's token of ciphertest.ExchangeRolesAccount at the broker's own audience.
func (ts *testServer) rolesPodToken(t *testing.T, pod string) string {
	t.Helper()
	return ts.accountToken(t, ciphertest.ExchangeRolesAccount, pod, time.Hour)
}

// rolesTradeToken is rolesPodToken at the exchange's audience, the subject token pod trades.
func (ts *testServer) rolesTradeToken(t *testing.T, pod string) string {
	t.Helper()
	return ts.audienceToken(t, ciphertest.ExchangeRolesAccount, pod, ciphertest.ExchangeAudience, time.Hour)
}

// TestTheRegistrationAudienceIsAcceptedAtTheRouteAlone: BROKER_POD_REGISTRATION_AUDIENCE is an
// audience the registration route accepts a pod token at, beside the broker's own, and no other
// route does. A token of that audience bound to a pod of the rig exchange's free-session account
// is admitted at the route for a listed registrar. The very same token is then refused at POST
// /v1/oauth/token, whose exchange verifies its own audience, although the registration it just
// wrote is what admits that pod's trade under that name; and at POST /v1/enrollments, which
// verifies the broker's audience, where the same pod's token of the broker's audience enrols. An
// expired token of either audience is 401 POD_TOKEN_EXPIRED at the route, which a daemon retries,
// and a token of a third audience 401 POD_TOKEN_INVALID; and on a broker with the setting unset,
// the registration audience's token is 401 POD_TOKEN_INVALID at the route too.
func TestTheRegistrationAudienceIsAcceptedAtTheRouteAlone(t *testing.T) {
	const pod, session = "pod-uid-roles", "implementer-g1"
	ts := newTestServerWith(t, func(d *api.Deps) {
		// BROKER_SERVICES binds the roles account to the daemon's service, so the daemon's own
		// login may enrol its pod and the enrolment row below is refused for its audience alone.
		d.Enroll.ServicesByAccount[ciphertest.ExchangeRolesAccount] = "legion-daemon"
	})
	registrar := ts.accountToken(t, daemonAccount, "", time.Hour)
	token := ts.audienceToken(t, ciphertest.ExchangeRolesAccount, pod, registrationAudience, time.Hour)

	status, body := ts.registerPodSession(t, registrar, token, session)
	if registration := decode[wirePodSession](t, body); status != http.StatusOK || registration.PodUID != pod || registration.Session != session {
		t.Fatalf("registering pod %s of %s with a token of the registration audience = %d %s, want 200 naming that pod and %s", pod, ciphertest.ExchangeRolesAccount, status, body, session)
	}

	t.Run("the token exchange", func(t *testing.T) {
		answer := ts.exchangePodToken(t, token, session)
		if answer.status != http.StatusBadRequest || answer.code != "invalid_request" || !strings.Contains(answer.description, "was not minted for") {
			t.Fatalf("POST /v1/oauth/token trading pod %s's token of the registration audience for %s, which that pod is registered for = %+v, want 400 invalid_request refusing the token's audience: the exchange accepts its own audience alone", pod, session, answer)
		}
	})

	t.Run("the enrolment", func(t *testing.T) {
		key := newSigningKey(t)
		status, body := ts.tokenLogin(t, registrar, ts.daemonRequest(t, key))
		if status != http.StatusOK {
			t.Fatalf("the daemon's token login = %d %s, want 200", status, body)
		}
		credential := decode[wireTokenLogin](t, body).CredentialID
		enrol := func(podToken string) (int, []byte) {
			return ts.launcher(t, key, credential, http.MethodPost, "/v1/enrollments", map[string]any{
				"kind": "pod", "runtime_id": pod, "thumbprint": thumbprintOf(t, newSigningKey(t)), "pod_token": podToken,
			})
		}
		status, body = enrol(token)
		if code := decode[wireError](t, body).Code; status != http.StatusForbidden || code != "POD_IDENTITY_MISMATCH" {
			t.Fatalf("POST /v1/enrollments of pod %s with its token of the registration audience = %d %s, want 403 POD_IDENTITY_MISMATCH: enrolment accepts the broker's own audience alone", pod, status, body)
		}
		if status, body := enrol(ts.rolesPodToken(t, pod)); status != http.StatusCreated {
			t.Fatalf("POST /v1/enrollments of pod %s with its token of the broker's audience = %d %s, want 201: the refusal above is the audience, not the pod", pod, status, body)
		}
	})

	for _, c := range []struct {
		name, audience string
		lifetime       time.Duration
		code           string
	}{
		{"an expired token of the broker's audience", podAudience, -time.Minute, "POD_TOKEN_EXPIRED"},
		{"an expired token of the registration audience", registrationAudience, -time.Minute, "POD_TOKEN_EXPIRED"},
		{"a token of a third audience", "legion-broker-elsewhere", time.Hour, "POD_TOKEN_INVALID"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := ts.registerPodSession(t, registrar, ts.audienceToken(t, ciphertest.ExchangeRolesAccount, "pod-uid-refused", c.audience, c.lifetime), session)
			if code := decode[wireError](t, body).Code; status != http.StatusUnauthorized || code != c.code {
				t.Fatalf("registering %s = %d %s, want 401 %s", c.name, status, body, c.code)
			}
		})
	}
	if rows := ts.column(t, `select pod_uid || ' ' || session from oauth_pod_sessions`); !slices.Equal(rows, []string{pod + " " + session}) {
		t.Fatalf("oauth_pod_sessions holds %v, want the one registration the route admitted", rows)
	}

	t.Run("a broker with the setting unset", func(t *testing.T) {
		unset := newTestServerWith(t, func(d *api.Deps) { d.PodRegistrationTokens = nil })
		status, body := unset.registerPodSession(t, unset.accountToken(t, daemonAccount, "", time.Hour),
			unset.audienceToken(t, ciphertest.ExchangeRolesAccount, pod, registrationAudience, time.Hour), session)
		if code := decode[wireError](t, body).Code; status != http.StatusUnauthorized || code != "POD_TOKEN_INVALID" {
			t.Fatalf("registering a token of the registration audience with BROKER_POD_REGISTRATION_AUDIENCE unset = %d %s, want 401 POD_TOKEN_INVALID", status, body)
		}
	})
}
