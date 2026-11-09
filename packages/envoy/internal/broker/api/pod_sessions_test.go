package api_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
)

// registerPodSession posts a registration to POST /v1/oauth/exchange/pod-sessions with bearer as
// the calling service's own ServiceAccount token — none at all when it is empty.
func (ts *testServer) registerPodSession(t *testing.T, bearer, podToken, session string) (int, []byte) {
	t.Helper()
	headers := map[string]string{}
	if bearer != "" {
		headers["Authorization"] = "Bearer " + bearer
	}
	return ts.req(t, http.MethodPost, "/v1/oauth/exchange/pod-sessions", headers,
		map[string]any{"pod_token": podToken, "session": session})
}

// TestThePodSessionRouteTakesOnlyARegistrarsOwnLogin: the route is authenticated by
// BROKER_SERVICE_LOGINS and by nothing else, since a deployment's daemon registering its own pods
// is not one of the callers that relay a person's decisions. A request with no token at all, a
// token of an account that list does not name — including the very account BROKER_API_ACCOUNTS
// lists for the routes Dispatch's server calls — and an expired one are each refused, and each
// refusal names the list rather than the token.
func TestThePodSessionRouteTakesOnlyARegistrarsOwnLogin(t *testing.T) {
	ts := newTestServer(t)
	podToken := ts.accountToken(t, brokertest.ExchangeAccount, "pod-uid-registrar", time.Hour)
	for _, c := range []struct {
		name   string
		bearer string
		status int
		code   string
		proves string
	}{
		{
			name: "no token at all", bearer: "",
			status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_INVALID",
			proves: "the route takes the registrar's own ServiceAccount token, and a request with no Authorization header presents none",
		},
		{
			name: "an account BROKER_SERVICE_LOGINS does not name", bearer: ts.accountToken(t, unlistedAccount, "", time.Hour),
			status: http.StatusForbidden, code: "ACCOUNT_NOT_LISTED",
			proves: "a sound token of this cluster for the right audience, of an account that logs in as no service, registers nothing",
		},
		{
			name: "the listed caller of Dispatch's own routes", bearer: ts.uiToken,
			status: http.StatusForbidden, code: "ACCOUNT_NOT_LISTED",
			proves: "BROKER_API_ACCOUNTS is another authority: being the caller that relays a person's decisions is not being a registrar",
		},
		{
			name: "an expired login token", bearer: ts.accountToken(t, daemonAccount, "", -time.Minute),
			status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_EXPIRED",
			proves: "a registrar's token is held to its own expiry, as its token login is",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := ts.registerPodSession(t, c.bearer, podToken, "implementer-g1")
			if code := decode[wireError](t, body).Code; status != c.status || code != c.code {
				t.Fatalf("that registration = %d %s, want %d %s: %s", status, body, c.status, c.code, c.proves)
			}
			if rows := ts.column(t, `select pod_uid from oauth_pod_sessions`); len(rows) != 0 {
				t.Fatalf("the refused registration wrote %v", rows)
			}
		})
	}
}

// TestThePodSessionRouteRefusesWhatNeedsNoRegistration: what a registrar may register is the pod
// of an account BROKER_EXCHANGE_ACCOUNTS lists with no fixed session, and nothing else. A pod
// token that does not verify or has expired is refused as such; a token bound to no pod proves
// only that the account exists; an account no exchange entry lists exchanges nothing to register
// for; and an account whose entry fixes a session writes under the configuration's name, so
// registering one of its pods would say something no exchange reads — the one refusal that names
// the fixed name, so an operator reads why there is nothing to do.
func TestThePodSessionRouteRefusesWhatNeedsNoRegistration(t *testing.T) {
	ts := newTestServer(t)
	registrar := ts.accountToken(t, daemonAccount, "", time.Hour)
	for _, c := range []struct {
		name     string
		podToken string
		session  string
		status   int
		code     string
		says     string
		proves   string
	}{
		{
			name: "a pod token nobody signed", podToken: "not.a.token", session: "implementer-g1",
			status: http.StatusUnauthorized, code: "POD_TOKEN_INVALID",
			proves: "the body's token is verified against the cluster issuer, exactly as an enrolling pod's token is",
		},
		{
			name:     "a pod token past its expiry",
			podToken: ts.accountToken(t, brokertest.ExchangeAccount, "pod-uid-expired", -time.Minute), session: "implementer-g1",
			status: http.StatusUnauthorized, code: "POD_TOKEN_EXPIRED",
			proves: "an expired pod token names no running pod and is refused as expired rather than as unverifiable",
		},
		{
			name:     "a token bound to no pod",
			podToken: ts.accountToken(t, brokertest.ExchangeAccount, "", time.Hour), session: "implementer-g1",
			status: http.StatusBadRequest, code: "POD_NOT_BOUND",
			says:   brokertest.ExchangeAccount,
			proves: "a token carrying no kubernetes.io pod uid — the kind `kubectl create token` mints — names no pod to register, and the refusal names the account",
		},
		{
			name:     "a subject no exchange entry lists",
			podToken: ts.accountToken(t, unlistedAccount, "pod-uid-unlisted", time.Hour), session: "implementer-g1",
			status: http.StatusForbidden, code: "POD_ACCOUNT_NOT_LISTED",
			says:   unlistedAccount,
			proves: "a pod of an account BROKER_EXCHANGE_ACCOUNTS does not list exchanges nothing, so there is no name to register it for, and the refusal names the account",
		},
		{
			name:     "an entry that fixes a session",
			podToken: ts.accountToken(t, brokertest.ExchangeAccount, "pod-uid-fixed", time.Hour), session: brokertest.ExchangeSession,
			status: http.StatusConflict, code: "SESSION_FIXED",
			says:   brokertest.ExchangeSession,
			proves: "every pod of an account whose entry fixes a session writes under that name with no registration, so the refusal names the name the configuration fixes",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := ts.registerPodSession(t, registrar, c.podToken, c.session)
			if code := decode[wireError](t, body).Code; status != c.status || code != c.code {
				t.Fatalf("that registration = %d %s, want %d %s: %s", status, body, c.status, c.code, c.proves)
			}
			if c.says != "" && !strings.Contains(string(body), c.says) {
				t.Fatalf("the refusal reads %s, which does not name %q: %s", body, c.says, c.proves)
			}
			if rows := ts.column(t, `select pod_uid from oauth_pod_sessions`); len(rows) != 0 {
				t.Fatalf("the refused registration wrote %v", rows)
			}
		})
	}
}

// withExchangeAccounts wires the broker's OAuth provider on the test server's own database with a
// token exchange that lists accounts, none of them fixing a session — as withOAuth wires one for
// the machine-login routes. The rig's own entry fixes a session (brokertest.ExchangeSession), and
// a registration is written only for an entry that fixes none, so a test that registers anything
// lists the accounts it registers here.
func withExchangeAccounts(t *testing.T, accounts ...string) func(*api.Deps) {
	t.Helper()
	entries := make([]exchange.AccountEntry, 0, len(accounts))
	for _, account := range accounts {
		entries = append(entries, exchange.AccountEntry{Account: account, Project: brokertest.ExchangeProject, Person: brokertest.ExchangePerson})
	}
	policy, err := exchange.NewSet(entries, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	return func(d *api.Deps) {
		store := oauthstore.New(d.MachineLogin.Store.Pool)
		store.Reused = enroll.RevokeReusedCredential
		secret, err := store.Secret(context.Background())
		if err != nil {
			t.Fatalf("Secret: %v", err)
		}
		provider, err := oauth.New(oauth.Config{
			Secret: secret, DeviceLifetime: 15 * time.Minute, AccessLifetime: 15 * time.Minute, RefreshLifetime: time.Hour,
			VerificationURL: brokertest.MachineLoginURL, ProofSkew: time.Minute,
			Exchange: oauth.ExchangeConfig{Policy: policy},
		}, store)
		if err != nil {
			t.Fatalf("oauth.New: %v", err)
		}
		d.OAuth = provider
	}
}

// wirePodSession is POST /v1/oauth/exchange/pod-sessions's answer as a registrar reads it.
type wirePodSession struct {
	PodUID  string `json:"pod_uid"`
	Session string `json:"session"`
}

// TestARegistrarRegistersAnyPodButTheOneItRunsIn: the route writes a registration for any pod but
// the caller's own. BROKER_SERVICE_LOGINS may name an account BROKER_SERVICES binds to a service,
// and then every pod of that account logs the service in and is a caller here. The registration
// is the one thing that tells two pods of one account apart at the exchange, so a pod that could
// write its own row would choose the session name it writes under, including a name a sibling
// pod holds. A registrar registering a pod it is not — a deployment's daemon registering the pods
// it starts, and a daemon of one account registering a sibling pod of that account across a
// rollout — is unchanged, as is a registrar whose own token names no pod at all.
func TestARegistrarRegistersAnyPodButTheOneItRunsIn(t *testing.T) {
	ts := newTestServerWith(t, withExchangeAccounts(t, daemonAccount, workerAccount))
	// The daemon's own token, which it presents as its login and which names the pod it runs in:
	// an account BROKER_SERVICE_LOGINS and BROKER_EXCHANGE_ACCOUNTS both list.
	own := ts.accountToken(t, daemonAccount, "pod-uid-daemon", time.Hour)

	t.Run("the pod it runs in", func(t *testing.T) {
		status, body := ts.registerPodSession(t, own, own, "implementer-g1")
		refusal := decode[wireError](t, body)
		if status != http.StatusForbidden || refusal.Code != "POD_CALLER_IS_THE_POD" {
			t.Fatalf("a caller registering the pod it runs in = %d %s, want 403 POD_CALLER_IS_THE_POD", status, body)
		}
		if !strings.Contains(refusal.Error, "pod-uid-daemon") || !strings.Contains(refusal.Error, daemonAccount) {
			t.Fatalf("the refusal reads %q, which does not name pod pod-uid-daemon of %s", refusal.Error, daemonAccount)
		}
		if rows := ts.column(t, `select pod_uid from oauth_pod_sessions`); len(rows) != 0 {
			t.Fatalf("the refused registration wrote %v", rows)
		}
	})

	t.Run("a sibling pod of its own account", func(t *testing.T) {
		sibling := ts.accountToken(t, daemonAccount, "pod-uid-daemon-two", time.Hour)
		status, body := ts.registerPodSession(t, own, sibling, "implementer-g1")
		if registration := decode[wirePodSession](t, body); status != http.StatusOK || registration.PodUID != "pod-uid-daemon-two" {
			t.Fatalf("registering a sibling pod of %s = %d %s, want 200 naming that pod: one generation of a rollout registers the next", daemonAccount, status, body)
		}
	})

	t.Run("a pod of another account", func(t *testing.T) {
		status, body := ts.registerPodSession(t, own, ts.accountToken(t, workerAccount, "pod-uid-worker", time.Hour), "implementer-g2")
		if registration := decode[wirePodSession](t, body); status != http.StatusOK || registration.PodUID != "pod-uid-worker" || registration.Session != "implementer-g2" {
			t.Fatalf("registering a pod of %s = %d %s, want 200 naming that pod and that session", workerAccount, status, body)
		}
		if rows := ts.column(t, `select pod_uid || ' ' || session from oauth_pod_sessions order by pod_uid`); !slices.Equal(rows, []string{"pod-uid-daemon-two implementer-g1", "pod-uid-worker implementer-g2"}) {
			t.Fatalf("oauth_pod_sessions holds %v, want the two registrations the registrar wrote", rows)
		}
	})
}
