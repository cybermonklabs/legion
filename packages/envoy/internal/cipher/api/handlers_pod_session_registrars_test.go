// handlers_pod_session_registrars_test.go drives the operator's move of a session name's binding,
// PUT /v1/pod-session-registrars/{session}, and the list of every binding, GET
// /v1/pod-session-registrars, on the real broker api_test.go mounts: only the operator entry of
// BROKER_NATS_SERVICES calls them, the move binds the name to the named daemon at once with no
// moment at which the name is bound to none, and the registration route and the token exchange
// read the moved binding from then on.
package api_test

import (
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/logtest"
)

// thirdDaemonAccount is a third Legion daemon's own account, beside daemonAccount and
// otherDaemonAccount, which a test lists for the rig exchange's free-session account with
// withThirdDaemon and lets log in as legion-daemon.
const thirdDaemonAccount = "system:serviceaccount:legion-system:legion-daemon-third"

// movedMessage is the move route's INFO line.
const movedMessage = "broker: the operator moved a session name's binding to another registrar"

// withThirdDaemon lists thirdDaemonAccount in BROKER_POD_REGISTRARS for the rig exchange's
// free-session account, beside the two daemons newTestServer lists. The test lets it log in too
// (testServer.ServiceLogins).
func withThirdDaemon(d *api.Deps) {
	d.PodRegistrars[api.PodRegistrar{Registrar: thirdDaemonAccount, Pod: ciphertest.ExchangeRolesAccount}] = struct{}{}
}

// wireBinding is one binding as the routes answer it.
type wireBinding struct {
	SessionName       string    `json:"session_name"`
	Registrar         string    `json:"registrar"`
	RegisteredAt      time.Time `json:"registered_at"`
	PreviousRegistrar string    `json:"previous_registrar"`
}

// wireOperatorRefusal is a refusal of a route only the operator calls: its code in error, as the
// NATS writer hold's routes answer it.
type wireOperatorRefusal struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// moveBinding asks PUT /v1/pod-session-registrars/{session} to bind session to registrar with
// token on the Authorization header ("" for none).
func (ts *testServer) moveBinding(t *testing.T, token, session, registrar string) (int, []byte) {
	t.Helper()
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return ts.req(t, http.MethodPut, "/v1/pod-session-registrars/"+url.PathEscape(session), headers, map[string]string{"registrar": registrar})
}

// listBindings asks GET /v1/pod-session-registrars with token on the Authorization header.
func (ts *testServer) listBindings(t *testing.T, token string) (int, []byte) {
	t.Helper()
	return ts.req(t, http.MethodGet, "/v1/pod-session-registrars", map[string]string{"Authorization": "Bearer " + token}, nil)
}

// operatorToken is the operator entry's platform token, live for ten minutes, as `kubectl create
// token --duration 10m` mints it for the NATS audience.
func (ts *testServer) operatorToken(t *testing.T) string {
	t.Helper()
	return ts.audienceToken(t, holdOperatorAccount, "", holdNATSAudience, 10*time.Minute)
}

// boundTo is the registrar pod_session_registrars binds session to, "" for none.
func (ts *testServer) boundTo(t *testing.T, session string) string {
	t.Helper()
	bound := ts.column(t, `select registrar from pod_session_registrars where session_name = $1`, session)
	if len(bound) == 0 {
		return ""
	}
	return bound[0]
}

// TestTheOperatorMovesABinding: daemon A (daemonAccount) registers its pod P_A for S, which binds
// S to A, and P_A trades under S. Each other entry of BROKER_NATS_SERVICES, presenting its own
// platform token for the NATS audience, and daemon B (otherDaemonAccount) and daemon C
// (thirdDaemonAccount), presenting their own broker logins, each ask to move S to B and to C: each
// is 403 not_operator, S stays bound to A and no move is logged, and the listener's token reading
// the list is 403 too. The operator's move of S to B answers 200 with the binding to B and its
// previous registrar A, binds S to B in the table at once, and logs one INFO line naming the
// operator, S, A and B. C's registration of its pod P_C for S, which lands after the move and
// before B has registered anything, the moment a removal of the binding would leave S to the first
// registrar to come, is refused 409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR naming B; A's
// registration of P_A for S again is refused the same. P_A's trade under S is refused the final
// invalid_grant session_name_not_this_registrars:, since its registration is A's and S is B's. B's
// registration of P_B for S is admitted and P_B trades under S. GET lists every binding: S to B
// and C's own name to C.
func TestTheOperatorMovesABinding(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServerWith(t, withThirdDaemon)
	ts.ServiceLogins[thirdDaemonAccount] = "legion-daemon"
	const session, ownSession = "implementer-g1", "reviewer-g1"
	const podA, podB, podC = "pod-uid-of-a", "pod-uid-of-b", "pod-uid-of-c"
	daemonA := ts.accountToken(t, daemonAccount, "", time.Hour)
	daemonB := ts.accountToken(t, otherDaemonAccount, "", time.Hour)
	daemonC := ts.accountToken(t, thirdDaemonAccount, "", time.Hour)

	ts.admits(t, "A's registration of "+podA+" for "+session, daemonA, ts.rolesPodToken(t, podA), podA, session)
	if answer := ts.exchangePodToken(t, ts.rolesTradeToken(t, podA), session); answer.status != http.StatusOK || !answer.issued {
		t.Fatalf("%s's trade under %s, which %s registered it for = %+v, want 200 with an access token", podA, session, daemonAccount, answer)
	}

	for _, caller := range []struct{ name, token string }{
		{"the listener entry's platform token", ts.audienceToken(t, holdListenerAccount, "", holdNATSAudience, 10*time.Minute)},
		{"the dispatch entry's platform token", ts.audienceToken(t, holdDispatchAccount, "", holdNATSAudience, 10*time.Minute)},
		{"the daemon entry's platform token", ts.audienceToken(t, holdDaemonAccount, "", holdNATSAudience, 10*time.Minute)},
		{"the measure entry's platform token", ts.audienceToken(t, holdMeasureAccount, "", holdNATSAudience, 10*time.Minute)},
		{"the probe entry's platform token", ts.audienceToken(t, holdProbeAccount, "", holdNATSAudience, 10*time.Minute)},
		{"daemon B's own broker login", daemonB},
		{"daemon C's own broker login", daemonC},
		{"no token", ""},
	} {
		for _, to := range []string{otherDaemonAccount, thirdDaemonAccount} {
			status, body := ts.moveBinding(t, caller.token, session, to)
			if refusal := decode[wireOperatorRefusal](t, body); status != http.StatusForbidden || refusal.Error != "not_operator" {
				t.Errorf("a move of %s to %s with %s = %d %s, want 403 not_operator", session, to, caller.name, status, strings.TrimSpace(string(body)))
			}
			if bound := ts.boundTo(t, session); bound != daemonAccount {
				t.Fatalf("after a move of %s to %s with %s, %s is bound to %q, want %s: a caller other than the operator moved it", session, to, caller.name, session, bound, daemonAccount)
			}
		}
	}
	if status, body := ts.listBindings(t, ts.audienceToken(t, holdListenerAccount, "", holdNATSAudience, 10*time.Minute)); status != http.StatusForbidden ||
		decode[wireOperatorRefusal](t, body).Error != "not_operator" {
		t.Errorf("the list of bindings with the listener entry's platform token = %d %s, want 403 not_operator", status, strings.TrimSpace(string(body)))
	}
	if logged := loggedAs(capture, movedMessage); len(logged) != 0 {
		t.Fatalf("the refused moves logged %v, want no move", logged)
	}

	operator := ts.operatorToken(t)
	before := time.Now().Add(-time.Second)
	status, body := ts.moveBinding(t, operator, session, otherDaemonAccount)
	moved := decode[wireBinding](t, body)
	if status != http.StatusOK || moved.SessionName != session || moved.Registrar != otherDaemonAccount || moved.PreviousRegistrar != daemonAccount || moved.RegisteredAt.Before(before) {
		t.Fatalf("the operator's move of %s to %s = %d %s, want 200 binding %s to %s since the move, its previous registrar %s", session, otherDaemonAccount, status, strings.TrimSpace(string(body)), session, otherDaemonAccount, daemonAccount)
	}
	// Each check from here reports and goes on, so a move that left the name bound to none also
	// shows which daemon the registrations below then bind it to.
	if bound := ts.boundTo(t, session); bound != otherDaemonAccount {
		t.Errorf("once the operator's move of %s to %s answered, %s is bound to %q, want %s", session, otherDaemonAccount, session, bound, otherDaemonAccount)
	}
	wantMoved := []map[string]any{{
		"level": slog.LevelInfo, "msg": movedMessage, "operator": holdOperatorAccount,
		"session": session, "previous_registrar": daemonAccount, "registrar": otherDaemonAccount,
	}}
	if logged := loggedAs(capture, movedMessage); !reflect.DeepEqual(logged, wantMoved) {
		t.Errorf("the operator's move of %s logged %v, want exactly %v", session, logged, wantMoved)
	}

	// C comes between the move and B's first registration of S: the moment a removal of the
	// binding would leave S to the first registrar to register it.
	for _, refused := range []struct{ what, registrar, account, pod string }{
		{"C's registration of " + podC, daemonC, thirdDaemonAccount, podC},
		{"A's registration of " + podA + " again", daemonA, daemonAccount, podA},
	} {
		status, body := ts.registerPodSession(t, refused.registrar, ts.rolesPodToken(t, refused.pod), session)
		if refusal := decode[wireError](t, body); status != http.StatusConflict || refusal.Code != "SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR" || !strings.Contains(refusal.Error, otherDaemonAccount) {
			t.Errorf("%s for %s once the operator moved it to %s = %d %s, want 409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR naming %s",
				refused.what, session, otherDaemonAccount, status, strings.TrimSpace(string(body)), otherDaemonAccount)
		}
	}
	if bound := ts.boundTo(t, session); bound != otherDaemonAccount {
		t.Fatalf("after C's and A's registrations of %s, %s is bound to %q, want %s, the registrar the operator moved it to", session, session, bound, otherDaemonAccount)
	}
	if answer := ts.exchangePodToken(t, ts.rolesTradeToken(t, podA), session); answer.status != http.StatusBadRequest || answer.code != "invalid_grant" || answer.issued ||
		!strings.HasPrefix(answer.description, oauthwire.SessionNameNotThisRegistrars+":") {
		t.Errorf("%s's trade under %s, which %s registered before the operator moved it to %s = %+v; want 400 invalid_grant, its description beginning %s:, and no token",
			podA, session, daemonAccount, otherDaemonAccount, answer, oauthwire.SessionNameNotThisRegistrars)
	}

	ts.admits(t, "B's registration of "+podB+" for "+session+", which the operator moved to B", daemonB, ts.rolesPodToken(t, podB), podB, session)
	if answer := ts.exchangePodToken(t, ts.rolesTradeToken(t, podB), session); answer.status != http.StatusOK || !answer.issued {
		t.Errorf("%s's trade under %s, which %s registered it for once the operator moved the name to it = %+v, want 200 with an access token", podB, session, otherDaemonAccount, answer)
	}

	ts.admits(t, "C's registration of "+podC+" for a name of its own, "+ownSession, daemonC, ts.rolesPodToken(t, podC), podC, ownSession)
	status, body = ts.listBindings(t, operator)
	listed := decode[struct {
		Bindings []wireBinding `json:"bindings"`
	}](t, body)
	var got []string
	for _, binding := range listed.Bindings {
		got = append(got, binding.SessionName+" to "+binding.Registrar)
		if binding.RegisteredAt.IsZero() || binding.PreviousRegistrar != "" {
			t.Errorf("the list's binding %+v, want its registered_at and no previous_registrar", binding)
		}
	}
	if want := []string{session + " to " + otherDaemonAccount, ownSession + " to " + thirdDaemonAccount}; status != http.StatusOK || !slices.Equal(got, want) {
		t.Errorf("the operator's list of bindings = %d %s, want 200 listing %v in that order", status, strings.TrimSpace(string(body)), want)
	}
}

// TestTheOperatorMovesANameOnlyToAListedRegistrar: the operator's move of a name to an account no
// BROKER_POD_REGISTRARS entry names as a registrar is 400 registrar_not_listed naming that account,
// and so is a move naming no registrar, or the account of the pods a registrar registers, which an
// entry names on its other side; a move of a name no registration could name is 400
// session_invalid. None moves the binding or logs a move. A move of a name no daemon has bound
// binds it to the named registrar, with no previous registrar, so that daemon's first registration
// of it is admitted and no other daemon's is.
func TestTheOperatorMovesANameOnlyToAListedRegistrar(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServer(t)
	const session, unbound, podA, podB = "implementer-g1", "planner-g1", "pod-uid-of-a", "pod-uid-of-b"
	daemonA := ts.accountToken(t, daemonAccount, "", time.Hour)
	daemonB := ts.accountToken(t, otherDaemonAccount, "", time.Hour)
	ts.admits(t, "A's registration of "+podA+" for "+session, daemonA, ts.rolesPodToken(t, podA), podA, session)
	operator := ts.operatorToken(t)

	for _, registrar := range []string{unlistedAccount, ciphertest.ExchangeRolesAccount, ""} {
		status, body := ts.moveBinding(t, operator, session, registrar)
		refusal := decode[wireOperatorRefusal](t, body)
		if status != http.StatusBadRequest || refusal.Error != "registrar_not_listed" || !strings.Contains(refusal.Message, "BROKER_POD_REGISTRARS") ||
			(registrar != "" && !strings.Contains(refusal.Message, registrar)) {
			t.Errorf("the operator's move of %s to %q = %d %s, want 400 registrar_not_listed naming it and BROKER_POD_REGISTRARS", session, registrar, status, strings.TrimSpace(string(body)))
		}
	}
	for _, invalid := range []string{"has space", "\x01control"} {
		status, body := ts.moveBinding(t, operator, invalid, otherDaemonAccount)
		if refusal := decode[wireOperatorRefusal](t, body); status != http.StatusBadRequest || refusal.Error != "session_invalid" {
			t.Errorf("the operator's move of %q, a name no registration could name, = %d %s, want 400 session_invalid", invalid, status, strings.TrimSpace(string(body)))
		}
	}
	if bound := ts.boundTo(t, session); bound != daemonAccount {
		t.Fatalf("after the refused moves, %s is bound to %q, want %s", session, bound, daemonAccount)
	}
	if bindings := ts.column(t, `select session_name from pod_session_registrars order by session_name`); !slices.Equal(bindings, []string{session}) {
		t.Fatalf("after the refused moves, pod_session_registrars binds %v, want %s alone", bindings, session)
	}
	if logged := loggedAs(capture, movedMessage); len(logged) != 0 {
		t.Fatalf("the refused moves logged %v, want no move", logged)
	}

	status, body := ts.moveBinding(t, operator, unbound, otherDaemonAccount)
	if moved := decode[wireBinding](t, body); status != http.StatusOK || moved.Registrar != otherDaemonAccount || moved.PreviousRegistrar != "" {
		t.Fatalf("the operator's move of %s, which no daemon has bound, to %s = %d %s, want 200 binding it to %s with no previous registrar", unbound, otherDaemonAccount, status, strings.TrimSpace(string(body)), otherDaemonAccount)
	}
	if status, body := ts.registerPodSession(t, daemonA, ts.rolesPodToken(t, podA), unbound); status != http.StatusConflict ||
		decode[wireError](t, body).Code != "SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR" {
		t.Errorf("A's first registration of %s once the operator bound it to %s = %d %s, want 409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR", unbound, otherDaemonAccount, status, strings.TrimSpace(string(body)))
	}
	ts.admits(t, "B's first registration of "+podB+" for "+unbound+", which the operator bound to B", daemonB, ts.rolesPodToken(t, podB), podB, unbound)
	wantMoved := []map[string]any{{
		"level": slog.LevelInfo, "msg": movedMessage, "operator": holdOperatorAccount,
		"session": unbound, "previous_registrar": "", "registrar": otherDaemonAccount,
	}}
	if logged := loggedAs(capture, movedMessage); !reflect.DeepEqual(logged, wantMoved) {
		t.Errorf("the operator's move of %s logged %v, want exactly %v", unbound, logged, wantMoved)
	}
}
