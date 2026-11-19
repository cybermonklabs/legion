// session_binding_test.go drives the binding of a session name to the daemon that first registered
// it, at POST /v1/oauth/exchange/pod-sessions and at the token exchange on the real broker
// api_test.go mounts: a second daemon listed for the same pod account is refused a name the first
// registered, for good, whatever the first's registrations of it, and a pod it registered cannot
// trade under that name.
package api_test

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/logtest"
)

// heldMessage is the route's WARN line for a registration refused because another registrar holds
// the session name.
const heldMessage = "broker: a pod's session registration was refused: another registrar holds the session name"

// admits fails the test unless the registration of the pod podToken names for session, written by
// the registrar whose login token registrar is, is answered 200 naming that pod and that session;
// what names the registration.
func (ts *testServer) admits(t *testing.T, what, registrar, podToken, pod, session string) {
	t.Helper()
	status, body := ts.registerPodSession(t, registrar, podToken, session)
	if got := decode[wirePodSession](t, body); status != http.StatusOK || got.PodUID != pod || got.Session != session {
		t.Fatalf("%s = %d %s, want 200 naming pod %s and session %s", what, status, body, pod, session)
	}
}

// loggedAs is every record capture holds whose message is message, each as its level, its message
// and its attributes, in the order they were logged.
func loggedAs(capture *logtest.Recorder, message string) []map[string]any {
	var logged []map[string]any
	for _, record := range capture.Records() {
		if record.Message != message {
			continue
		}
		fields := map[string]any{"level": record.Level, "msg": record.Message}
		record.Attrs(func(a slog.Attr) bool { fields[a.Key] = a.Value.Any(); return true })
		logged = append(logged, fields)
	}
	return logged
}

// TestASessionNameStaysWithItsFirstRegistrar: a session name belongs, for good, to the daemon that
// first registered it. Daemon A (daemonAccount) registers its pod P_A for S, which binds S to A.
// Daemon B (otherDaemonAccount), listed for the same pod account, is then refused S at each step
// below, 409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR naming S and A, with a WARN line naming B, A,
// the pod and S, and records nothing: every row of oauth_pod_sessions stays as it was, and
// pod_session_registrars still binds S to A alone. It is refused registering its own pod P_B for
// S and A's own pod P_A for S while A's registration is live. A registering a second pod P_A2 for
// S, as a relaunch does, is admitted. Once every row A wrote of S has lapsed, B's registration of
// P_B for S is still refused; and once A has registered P_A for S again and released S at the
// release route, it is still refused. B is listed, so it registers P_B under a name of its own:
// what refuses it S is A's binding of the name, not B's login, its entry in the list or any row of
// A's. Every failure names S.
func TestASessionNameStaysWithItsFirstRegistrar(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServer(t)
	const session, ownSession = "implementer-g1", "reviewer-g1"
	const podA, podA2, podB = "pod-uid-of-a", "pod-uid-of-a-2", "pod-uid-of-b"
	daemonA, daemonB := ts.accountToken(t, daemonAccount, "", time.Hour), ts.accountToken(t, otherDaemonAccount, "", time.Hour)
	// rows is every row of oauth_pod_sessions whole, so a refused registration that changed any
	// column of any row shows.
	rows := func() []string {
		return ts.column(t, `select r::text from oauth_pod_sessions r order by pod_uid, session`)
	}
	bindings := func() []string {
		return ts.column(t, `select session_name || ' to ' || registrar from pod_session_registrars order by session_name`)
	}
	var want []map[string]any
	refused := func(when, pod string) {
		t.Helper()
		before := rows()
		status, body := ts.registerPodSession(t, daemonB, ts.rolesPodToken(t, pod), session)
		if refusal := decode[wireError](t, body); status != http.StatusConflict || refusal.Code != "SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR" ||
			!strings.Contains(refusal.Error, session) || !strings.Contains(refusal.Error, daemonAccount) {
			t.Fatalf("%s, listed for %s, registering %s for %s %s = %d %s; want 409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR naming %s and %s, the registrar %s is bound to",
				otherDaemonAccount, brokertest.ExchangeRolesAccount, pod, session, when, status, strings.TrimSpace(string(body)), session, daemonAccount, session)
		}
		if after := rows(); !slices.Equal(after, before) {
			t.Fatalf("oauth_pod_sessions holds %v after %s was refused %s %s, want %v unchanged", after, otherDaemonAccount, session, when, before)
		}
		if got, wantBindings := bindings(), []string{session + " to " + daemonAccount}; !slices.Equal(got, wantBindings) {
			t.Fatalf("pod_session_registrars holds %v after %s was refused %s %s, want %v alone", got, otherDaemonAccount, session, when, wantBindings)
		}
		want = append(want, map[string]any{
			"level": slog.LevelWarn, "msg": heldMessage, "service": "legion-daemon", "registrar": otherDaemonAccount, "holder": daemonAccount,
			"account": brokertest.ExchangeRolesAccount, "pod": pod, "session": session,
		})
	}

	ts.admits(t, "A's registration of "+podA+" for "+session, daemonA, ts.rolesPodToken(t, podA), podA, session)
	refused("while A's registration of "+podA+" for it is live", podB)
	refused("for A's own pod, while A's registration of it for "+session+" is live", podA)
	ts.admits(t, "A's registration of a second pod, "+podA2+", for "+session, daemonA, ts.rolesPodToken(t, podA2), podA2, session)

	if _, err := ts.Store.Pool.Exec(context.Background(), `update oauth_pod_sessions set expires_at = now() - interval '1 minute'
		where session = $1 and registrar = $2`, session, daemonAccount); err != nil {
		t.Fatalf("lapse %s's registrations of %s: %v", daemonAccount, session, err)
	}
	if live := ts.column(t, `select pod_uid from oauth_pod_sessions where session = $1 and expires_at > now()`, session); len(live) != 0 {
		t.Fatalf("%s's registrations of %s for %v are still live after they were lapsed, want none", daemonAccount, session, live)
	}
	refused("once every registration "+daemonAccount+" wrote of it has lapsed", podB)

	ts.admits(t, "A's registration of "+podA+" for "+session+" again, once its registrations lapsed", daemonA, ts.rolesPodToken(t, podA), podA, session)
	ts.releases(t, daemonAccount+"'s release of "+session, daemonA, []string{session}, 1, 1)
	refused("once "+daemonAccount+" has released it", podB)

	if logged := loggedAs(capture, heldMessage); !reflect.DeepEqual(logged, want) {
		t.Fatalf("the refused registrations of %s logged %v, want exactly %v", session, logged, want)
	}
	ts.admits(t, "B's registration of "+podB+" for a name of its own, "+ownSession, daemonB, ts.rolesPodToken(t, podB), podB, ownSession)
}

// notThisRegistrarsMessage is the exchange's WARN line for a pod's trade under a session name
// bound to a daemon other than the one whose registration of the pod the trade found.
const notThisRegistrarsMessage = "broker: the token exchange refused a pod's trade under a session name another daemon holds"

// TestAPodAnotherDaemonRegisteredCannotTradeUnderAHeldName: daemon A registers its pod P_A for S
// and P_A trades under S at POST /v1/oauth/token. Daemon B, listed for the same pod account,
// registers its pod P_B for a name of its own, S_B, and is refused S, 409
// SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR. With no registration of P_B for S, P_B's trade under S
// is refused invalid_grant with the description of a pod no registration names
// (pod_session_not_registered:), the one both clients trade again on. A live registration of P_B
// for S naming B, which no build's route writes and the test writes by hand, does not admit it
// either: P_B's trade under S is then refused invalid_grant with a description beginning
// session_name_not_this_registrars: that names S and not A, with no token, and a WARN line naming
// the pod, S, B and A. P_B's trade under S_B is answered. Both pods run as one account, which is
// one principal at the exchange, so the claim on the name, which binds a name to a principal,
// cannot be what refuses P_B: A's binding of S is, at the route and at the exchange alike.
func TestAPodAnotherDaemonRegisteredCannotTradeUnderAHeldName(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServer(t)
	const session, ownSession, podA, podB = "implementer-g1", "reviewer-g1", "pod-uid-of-a", "pod-uid-of-b"
	daemonA, daemonB := ts.accountToken(t, daemonAccount, "", time.Hour), ts.accountToken(t, otherDaemonAccount, "", time.Hour)

	ts.admits(t, "A's registration of "+podA+" for "+session, daemonA, ts.rolesPodToken(t, podA), podA, session)
	if answer := ts.exchangePodToken(t, ts.rolesTradeToken(t, podA), session); answer.status != http.StatusOK || !answer.issued {
		t.Fatalf("%s's trade under %s, which %s registered it for = %+v, want 200 with an access token", podA, session, daemonAccount, answer)
	}
	ts.admits(t, "B's registration of "+podB+" for its own name, "+ownSession, daemonB, ts.rolesPodToken(t, podB), podB, ownSession)
	// Each check below reports and goes on, so a refusal that is missing also shows what the
	// checks after it find.
	status, body := ts.registerPodSession(t, daemonB, ts.rolesPodToken(t, podB), session)
	if code := decode[wireError](t, body).Code; status != http.StatusConflict || code != "SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR" {
		t.Errorf("%s registering %s for %s, which %s holds = %d %s, want 409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR", otherDaemonAccount, podB, session, daemonAccount, status, strings.TrimSpace(string(body)))
	}

	answer := ts.exchangePodToken(t, ts.rolesTradeToken(t, podB), session)
	if answer.status != http.StatusBadRequest || answer.code != "invalid_grant" || answer.issued || !strings.HasPrefix(answer.description, oauthwire.PodSessionNotRegistered+":") {
		t.Errorf("%s's trade under %s, which %s holds and did not register it for = %+v; want 400 invalid_grant, its description beginning %s:, and no token",
			podB, session, daemonAccount, answer, oauthwire.PodSessionNotRegistered)
	}

	if _, err := ts.Store.Pool.Exec(context.Background(), `insert into oauth_pod_sessions (pod_uid, session, expires_at, registrar)
		values ($1, $2, now() + interval '1 day', $3)`, podB, session, otherDaemonAccount); err != nil {
		t.Fatalf("write a live registration of %s for %s naming %s by hand: %v", podB, session, otherDaemonAccount, err)
	}
	answer = ts.exchangePodToken(t, ts.rolesTradeToken(t, podB), session)
	if answer.status != http.StatusBadRequest || answer.code != "invalid_grant" || answer.issued ||
		!strings.HasPrefix(answer.description, oauthwire.SessionNameNotThisRegistrars+":") || !strings.Contains(answer.description, session) {
		t.Errorf("%s's trade under %s, with a live registration of it for %s naming %s, which %s is not bound to = %+v; want 400 invalid_grant, its description beginning %s: and naming %s, and no token",
			podB, session, session, otherDaemonAccount, session, answer, oauthwire.SessionNameNotThisRegistrars, session)
	}
	if strings.Contains(answer.description, daemonAccount) {
		t.Errorf("%s's refused trade under %s reads %q, which names %s, the daemon %s is bound to: no exchange refusal names another daemon", podB, session, answer.description, daemonAccount, session)
	}
	wantWarned := []map[string]any{{
		"level": slog.LevelWarn, "msg": notThisRegistrarsMessage, "principal": exchange.AccountPrincipal(brokertest.ExchangeRolesAccount),
		"pod": podB, "session": session, "registrar": otherDaemonAccount, "holder": daemonAccount,
	}}
	if logged := loggedAs(capture, notThisRegistrarsMessage); !reflect.DeepEqual(logged, wantWarned) {
		t.Errorf("%s's refused trade under %s logged %v, want exactly %v", podB, session, logged, wantWarned)
	}
	if answer := ts.exchangePodToken(t, ts.rolesTradeToken(t, podB), ownSession); answer.status != http.StatusOK || !answer.issued {
		t.Errorf("%s's trade under %s, which %s registered it for = %+v, want 200 with an access token: the refusal above is the name, not the pod", podB, ownSession, otherDaemonAccount, answer)
	}
}
