package api_test

import (
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/logtest"
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

// withExchangeAccounts is withProvider with a token exchange that lists accounts, none of them
// fixing a session, as withOAuth is withProvider with the machine-login routes' device login.
// The rig's own entry fixes a session (brokertest.ExchangeSession), and a registration is
// written only for an entry that fixes none, so a test that registers anything lists the
// accounts it registers here.
func withExchangeAccounts(t *testing.T, accounts ...string) func(*api.Deps) {
	t.Helper()
	entries := make([]exchange.AccountEntry, 0, len(accounts))
	for _, account := range accounts {
		entries = append(entries, exchange.AccountEntry{Account: account, Project: brokertest.ExchangeProject, Person: brokertest.ExchangePerson})
	}
	policy, err := exchange.NewSet(entries, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	return withProvider(t, func(config *oauth.Config) {
		config.Exchange = oauth.ExchangeConfig{Policy: policy}
	})
}

// wirePodSession is POST /v1/oauth/exchange/pod-sessions's answer as a registrar reads it.
type wirePodSession struct {
	PodUID     string `json:"pod_uid"`
	Session    string `json:"session"`
	Thumbprint string `json:"thumbprint"`
	Generation *int64 `json:"generation"`
}

// TestTheRegistrationRouteAnswersTheStoredGeneration: the route answers the registration as
// stored, its generation included. A write of generation 2 with kB is answered generation 2; a
// later write of generation 1 with kA, an old incarnation's committed late, changes nothing and
// is answered the stored row, kB at generation 2; a write that sends no generation is answered no
// generation field, as before generations existed (R9;
// TestARegistrationNamingNoKeyAndNoGenerationIsAnsweredAsBefore holds the whole answer); and a
// negative generation is 400 GENERATION_INVALID and writes nothing.
func TestTheRegistrationRouteAnswersTheStoredGeneration(t *testing.T) {
	ts := newTestServerWith(t, withExchangeAccounts(t, daemonAccount, workerAccount))
	registrar := ts.accountToken(t, daemonAccount, "", time.Hour)
	podToken := ts.accountToken(t, workerAccount, "pod-uid-issue", time.Hour)
	const kA, kB = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	register := func(fields map[string]any) (int, []byte) {
		fields["pod_token"] = podToken
		return ts.req(t, http.MethodPost, "/v1/oauth/exchange/pod-sessions",
			map[string]string{"Authorization": "Bearer " + registrar}, fields)
	}
	answered := func(what string, status int, body []byte, thumbprint string, generation int64) {
		t.Helper()
		got := decode[wirePodSession](t, body)
		if status != http.StatusOK || got.Thumbprint != thumbprint || got.Generation == nil || *got.Generation != generation {
			t.Fatalf("%s = %d %s, want 200 answering the stored row: thumbprint %s at generation %d", what, status, body, thumbprint, generation)
		}
	}

	status, body := register(map[string]any{"session": "implementer-g1", "thumbprint": kB, "generation": 2})
	answered("a write of generation 2 with kB", status, body, kB, 2)
	status, body = register(map[string]any{"session": "implementer-g1", "thumbprint": kA, "generation": 1})
	answered("a late write of generation 1 with kA after generation 2", status, body, kB, 2)
	status, body = register(map[string]any{"session": "watched-session"})
	if got := decode[wirePodSession](t, body); status != http.StatusOK || got.Thumbprint != "" || got.Generation != nil {
		t.Fatalf("a write that sends no generation = %d %s, want 200 answering no thumbprint and no generation", status, body)
	}

	status, body = register(map[string]any{"session": "refused-session", "thumbprint": kA, "generation": -1})
	if code := decode[wireError](t, body).Code; status != http.StatusBadRequest || code != "GENERATION_INVALID" {
		t.Fatalf("a write of generation -1 = %d %s, want 400 GENERATION_INVALID", status, body)
	}
	if rows := ts.column(t, `select session from oauth_pod_sessions where session = 'refused-session'`); len(rows) != 0 {
		t.Fatalf("the refused write of generation -1 wrote %v", rows)
	}
}

// TestTheRegistrationRouteHoldsTheThumbprintsShape: a registration's thumbprint is an RFC 7638
// SHA-256 thumbprint, 43 base64url characters, or the route refuses it 400 THUMBPRINT_INVALID
// and writes nothing; one of that shape is stored and answered back.
func TestTheRegistrationRouteHoldsTheThumbprintsShape(t *testing.T) {
	ts := newTestServerWith(t, withExchangeAccounts(t, daemonAccount, workerAccount))
	registrar := ts.accountToken(t, daemonAccount, "", time.Hour)
	podToken := ts.accountToken(t, workerAccount, "pod-uid-issue", time.Hour)
	register := func(thumbprint string) (int, []byte) {
		return ts.req(t, http.MethodPost, "/v1/oauth/exchange/pod-sessions",
			map[string]string{"Authorization": "Bearer " + registrar},
			map[string]any{"pod_token": podToken, "session": "implementer-g1", "thumbprint": thumbprint})
	}
	for _, malformed := range []string{
		strings.Repeat("A", 42),
		strings.Repeat("A", 44),
		strings.Repeat("A", 42) + "=",
		strings.Repeat("A", 42) + "+",
		strings.Repeat("A", 42) + "/",
		" " + strings.Repeat("A", 42),
	} {
		status, body := register(malformed)
		if code := decode[wireError](t, body).Code; status != http.StatusBadRequest || code != "THUMBPRINT_INVALID" {
			t.Fatalf("a registration naming the thumbprint %q = %d %s, want 400 THUMBPRINT_INVALID", malformed, status, body)
		}
	}
	if rows := ts.column(t, `select pod_uid from oauth_pod_sessions`); len(rows) != 0 {
		t.Fatalf("the refused registrations wrote %v", rows)
	}
	const good = "0123456789abcdefghijklmnopqrstuvwxyzABCDE_-"
	status, body := register(good)
	if registration := decode[wirePodSession](t, body); status != http.StatusOK || registration.Thumbprint != good {
		t.Fatalf("a registration naming the thumbprint %s = %d %s, want 200 answering it back", good, status, body)
	}
}

// The route's two log messages: a write it applied, and a write the generation rule refused.
const (
	registeredMessage = "broker: registered a pod to write under a session name"
	notAppliedMessage = "broker: a pod's session registration was not written: the registration holds a later generation"
)

// registrationRig is a test server whose exchange lists the daemon's account and the worker's,
// the daemon's own login as the registrar, and the token of the worker's pod pod, with the
// default logger captured for the test.
type registrationRig struct {
	ts                  *testServer
	capture             *logtest.Recorder
	registrar, podToken string
	pod                 string
	// seen is how many of capture's records an earlier call of logged answered or passed over.
	seen int
}

func newRegistrationRig(t *testing.T, pod string) *registrationRig {
	t.Helper()
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServerWith(t, withExchangeAccounts(t, daemonAccount, workerAccount))
	return &registrationRig{
		ts: ts, capture: capture, pod: pod,
		registrar: ts.accountToken(t, daemonAccount, "", time.Hour),
		podToken:  ts.accountToken(t, workerAccount, pod, time.Hour),
	}
}

// register writes the pod's registration with fields beside its token, and fails the test unless
// the route answers 200.
func (r *registrationRig) register(t *testing.T, fields map[string]any) {
	t.Helper()
	fields["pod_token"] = r.podToken
	status, body := r.ts.req(t, http.MethodPost, "/v1/oauth/exchange/pod-sessions",
		map[string]string{"Authorization": "Bearer " + r.registrar}, fields)
	if status != http.StatusOK {
		t.Fatalf("a registration of %s with %v = %d %s, want 200", r.pod, fields, status, body)
	}
}

// logged is every record the broker has logged naming the rig's pod since the last call, each as
// its level, its message and its attributes by key, in the order they were written.
func (r *registrationRig) logged(t *testing.T) []map[string]any {
	t.Helper()
	records := r.capture.Records()
	var found []map[string]any
	for _, record := range records[r.seen:] {
		fields := map[string]any{"level": record.Level, "msg": record.Message}
		record.Attrs(func(a slog.Attr) bool { fields[a.Key] = a.Value.Any(); return true })
		if fields["pod"] == r.pod {
			found = append(found, fields)
		}
	}
	r.seen = len(records)
	return found
}

// loggedOnly fails the test unless the broker logged exactly want naming the rig's pod since the
// last call: the one record of what the write did, which what names.
func (r *registrationRig) loggedOnly(t *testing.T, what string, want map[string]any) {
	t.Helper()
	want["service"], want["account"], want["pod"] = "legion-daemon", workerAccount, r.pod
	if got := r.logged(t); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("%s logged %v, want exactly %v", what, got, want)
	}
}

// TestAFreshRegistrationLogsWhatWasSentAndStored: a registration's first write is logged at Info
// as registered, naming the thumbprint and generation the registrar sent beside those stored, and
// no key replaced, since there was none to replace.
func TestAFreshRegistrationLogsWhatWasSentAndStored(t *testing.T) {
	const kA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	rig := newRegistrationRig(t, "pod-uid-fresh")
	rig.register(t, map[string]any{"session": "implementer-g1", "thumbprint": kA, "generation": 1})
	rig.loggedOnly(t, "the first write of implementer-g1 with kA at generation 1", map[string]any{
		"level": slog.LevelInfo, "msg": registeredMessage, "session": "implementer-g1",
		"sent_thumbprint": kA, "sent_generation": int64(1), "thumbprint": kA, "generation": int64(1),
	})
}

// TestARegistrationReplacingAKeyLogsTheKeyItReplaced: a write that binds the registration to
// another key is logged as registered with the thumbprint it replaced beside what was sent and
// stored. A renewal naming the key the row holds, and a write naming no key, which keeps it,
// replace nothing and name nothing replaced.
func TestARegistrationReplacingAKeyLogsTheKeyItReplaced(t *testing.T) {
	const kA, kB = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	rig := newRegistrationRig(t, "pod-uid-relaunched")
	rig.register(t, map[string]any{"session": "implementer-g1", "thumbprint": kA, "generation": 1})
	rig.logged(t)

	rig.register(t, map[string]any{"session": "implementer-g1", "thumbprint": kA, "generation": 1})
	rig.loggedOnly(t, "a renewal of implementer-g1 with the key it holds, kA at generation 1", map[string]any{
		"level": slog.LevelInfo, "msg": registeredMessage, "session": "implementer-g1",
		"sent_thumbprint": kA, "sent_generation": int64(1), "thumbprint": kA, "generation": int64(1),
	})
	rig.register(t, map[string]any{"session": "implementer-g1", "thumbprint": kB, "generation": 2})
	rig.loggedOnly(t, "a write of implementer-g1 replacing kA with kB at generation 2", map[string]any{
		"level": slog.LevelInfo, "msg": registeredMessage, "session": "implementer-g1",
		"sent_thumbprint": kB, "sent_generation": int64(2), "thumbprint": kB, "generation": int64(2),
		"replaced_thumbprint": kA,
	})
	rig.register(t, map[string]any{"session": "implementer-g1", "generation": 2})
	rig.loggedOnly(t, "a write of implementer-g1 naming no key at generation 2", map[string]any{
		"level": slog.LevelInfo, "msg": registeredMessage, "session": "implementer-g1",
		"sent_thumbprint": "", "sent_generation": int64(2), "thumbprint": kB, "generation": int64(2),
	})
}

// TestARegistrationTheGenerationRuleRefusesLogsALineOfItsOwn: a write of a lower generation
// than the row's changes nothing, so it is not logged as registered: it logs a WARN line of its
// own naming the thumbprint and generation it sent beside the row's, which it left as it was.
func TestARegistrationTheGenerationRuleRefusesLogsALineOfItsOwn(t *testing.T) {
	const kA, kB = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	rig := newRegistrationRig(t, "pod-uid-late")
	rig.register(t, map[string]any{"session": "implementer-g1", "thumbprint": kB, "generation": 2})
	rig.logged(t)

	rig.register(t, map[string]any{"session": "implementer-g1", "thumbprint": kA, "generation": 1})
	rig.loggedOnly(t, "a late write of implementer-g1 with kA at generation 1 after kB at generation 2", map[string]any{
		"level": slog.LevelWarn, "msg": notAppliedMessage, "session": "implementer-g1",
		"sent_thumbprint": kA, "sent_generation": int64(1), "thumbprint": kB, "generation": int64(2),
	})
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
