package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim that forgets its session starts a new one, and the Envoy messages sent to the old
// session that it never read must reach the new one. The registration of the new session names
// the old one as `replaces` (replacesContract), which the session hands its Envoy role claim as
// `previous_session_id`, so the Envoy listener moves the old session's unread messages to it.

// registration posts session's registration on the launch bootToken names and answers its body,
// which must be a 200. A contract other than 0 is the plugin's: stated in the plugin contract
// header and sent as the body's pluginContract. Contract 0 states no header and sends this
// daemon's own contract, as harness.register does.
func (h *harness) registration(bootToken, session string, contract int) []byte {
	h.t.Helper()
	var header http.Header
	sent := DaemonAPIVersion()
	if contract != 0 {
		header = http.Header{pluginContractHeader: {strconv.Itoa(contract)}}
		sent = contract
	}
	recorder := h.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: bootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent-" + session, PluginContract: sent,
	}, header)
	if recorder.Code != http.StatusOK {
		h.t.Fatalf("register %s = %d, want 200; body %s", session, recorder.Code, recorder.Body)
	}
	return recorder.Body.Bytes()
}

// registeredRole is registration for a role's claim, decoded as claim.RegisterResponse, with the
// answer's keys as a reader of the wire sees them.
func (h *harness) registeredRole(bootToken, session string, contract int) (claim.RegisterResponse, map[string]any) {
	h.t.Helper()
	body := h.registration(bootToken, session, contract)
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		h.t.Fatalf("decode the registration of %s: %v; body %s", session, err, body)
	}
	return registered, answerKeys(h.t, body)
}

// answerKeys is body decoded key by key, so a test tells a key the answer leaves out from one it
// writes empty.
func answerKeys(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var keys map[string]any
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("decode the answer %s: %v", body, err)
	}
	return keys
}

// wantReplaces fails unless the answer's keys carry `replaces` as want, or, where want is "",
// carry no `replaces` key at all.
func wantReplaces(t *testing.T, what string, keys map[string]any, want string) {
	t.Helper()
	got, carried := keys["replaces"]
	switch {
	case want == "" && carried:
		t.Errorf("%s answered replaces %v, want no replaces key", what, got)
	case want != "" && got != want:
		t.Errorf("%s answered replaces %v (carried: %v), want %q", what, got, carried, want)
	}
}

// abandon has the operator start the claim on a new session, as the fresh-session recovery
// does: suspended, its session forgotten, and resumed. It returns the boot token of the launch
// that follows, and fails unless the claim records session as the one it abandoned.
func (h *harness) abandon(token claim.Token, session string) string {
	h.t.Helper()
	h.suspend(token)
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.RequestForgetSession{Claim: token, Session: session}); err != nil {
		h.t.Fatalf("forget %s's session %s: %v", token, session, err)
	}
	if err := m.Handle(h.ctx, supervise.RequestResume{Claim: token}); err != nil {
		h.t.Fatalf("resume %s: %v", token, err)
	}
	if got := m.Claim().Abandoned.Session; got != session {
		h.t.Fatalf("%s abandoned %q, want %s", token, got, session)
	}
	return h.bootToken(token)
}

// A claim that never forgot a session replaces none, and its registration carries no `replaces`.
// Once it forgets ses_first, the registration of its new session names ses_first, and so does
// each later registration of the claim, a relaunch resuming the new session among them, since the
// claim keeps the record until its next forget. An operator's release then frees ses_first's
// names, and another claim registers ses_first, whose messages are now that claim's: neither that
// claim's registration nor the next one of the first claim names it.
func TestARegistrationNamesTheSessionItsClaimAbandonedUntilAReleaseFreesIt(t *testing.T) {
	h := newHarness(t)
	owner, boot := h.launch("LEGION-901", claim.RoleImplementer)

	registered, keys := h.registeredRole(boot, "ses_first", 0)
	wantReplaces(t, "the first registration of a claim that abandoned no session", keys, "")
	h.readyAt(owner, "ses_first", registered)

	boot = h.abandon(owner, "ses_first")
	registered, keys = h.registeredRole(boot, "ses_second", 0)
	wantReplaces(t, "the registration of the new session", keys, "ses_first")
	if registered.Replaces != "ses_first" {
		t.Errorf("the registration of the new session decodes replaces %q, want ses_first", registered.Replaces)
	}
	h.readyAt(owner, "ses_second", registered)

	boot = h.relaunch(owner)
	registered, keys = h.registeredRole(boot, "ses_second", 0)
	wantReplaces(t, "the registration of a relaunch resuming the new session", keys, "ses_first")
	h.readyAt(owner, "ses_second", registered)

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(owner)+"/release-names", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the release of %s's names = %d, want 200; body %s", owner, recorder.Code, recorder.Body)
	}
	var released ReleasedNames
	if err := json.Unmarshal(recorder.Body.Bytes(), &released); err != nil || released.Abandoned != "ses_first" {
		t.Fatalf("the release answered %s (%v), want ses_first released", recorder.Body, err)
	}

	other, otherBoot := h.launch("LEGION-902", claim.RoleTester)
	taken, keys := h.registeredRole(otherBoot, "ses_first", 0)
	if taken.ClaimToken != other {
		t.Fatalf("%s registering the released ses_first answered %+v, want it admitted", other, taken)
	}
	wantReplaces(t, "the registration of another claim that took the released session", keys, "")

	boot = h.relaunch(owner)
	_, keys = h.registeredRole(boot, "ses_second", 0)
	wantReplaces(t, "the registration of a relaunch after the release", keys, "")
}

// A claim that abandoned ses_first and whose next agent registers ses_first again replaces no
// session: the session it abandoned is the one registering, so the registration carries no
// `replaces`.
func TestARegistrationOfTheAbandonedSessionItselfReplacesNone(t *testing.T) {
	h := newHarness(t)
	owner, boot := h.launch("LEGION-903", claim.RoleImplementer)
	registered, _ := h.registeredRole(boot, "ses_first", 0)
	h.readyAt(owner, "ses_first", registered)

	boot = h.abandon(owner, "ses_first")
	_, keys := h.registeredRole(boot, "ses_first", 0)
	wantReplaces(t, "the registration of the abandoned session itself", keys, "")
}

// A plugin below replacesContract did not ask for the session a registration replaces, so a
// caller that states such a contract is served the registration without `replaces`, while one at
// replacesContract is served it. The harness relaunches no claim for its contract, so the older
// plugin's registration is answered rather than refused.
func TestAnOlderContractIsServedNoReplaces(t *testing.T) {
	h := newHarnessUnrelaunchable(t)
	for _, tc := range []struct {
		issue, session string
		contract       int
		want           string
	}{
		{issue: "LEGION-904", session: "ses_older", contract: entryNumber(replacesContract) - 1},
		{issue: "LEGION-905", session: "ses_current", contract: entryNumber(replacesContract), want: "ses_current"},
	} {
		owner, boot := h.launch(tc.issue, claim.RoleImplementer)
		registered, _ := h.registeredRole(boot, tc.session, 0)
		h.readyAt(owner, tc.session, registered)
		boot = h.abandon(owner, tc.session)
		replacing, keys := h.registeredRole(boot, tc.session+"_next", tc.contract)
		wantReplaces(t, "the registration of a caller stating contract "+strconv.Itoa(tc.contract), keys, tc.want)
		if replacing.ClaimToken != owner {
			t.Errorf("the registration stating contract %d answered %+v, want %s", tc.contract, replacing, owner)
		}
	}
}

// A registration that replaces no session is the same body at every contract, so writeAnswer
// reads no claims to place its caller; one that carries `replaces`, a role's or the controller's,
// is placed by its pod, as every answer a band moves is.
func TestOnlyARegistrationThatReplacesASessionIsPlacedByItsPod(t *testing.T) {
	for _, tc := range []struct {
		id     AnswerID
		body   any
		placed bool
	}{
		{AnswerRegister, claim.RegisterResponse{}, false},
		{AnswerRegister, claim.RegisterResponse{Replaces: "ses_first"}, true},
		{AnswerRegisterController, ControllerRegisterResponse{}, false},
		{AnswerRegisterController, ControllerRegisterResponse{Replaces: "ses_first"}, true},
	} {
		if got := placedByPod(tc.id, tc.body); got != tc.placed {
			t.Errorf("placedByPod(%s, %+v) = %v, want %v", tc.id, tc.body, got, tc.placed)
		}
	}
}

// The daemon's own controller (`controller: daemon`) holds a claim, which forgets a session as
// any claim does: its first registration replaces none, and the registration of the session
// that follows a forget names the one it forgot.
func TestALaunchedControllersRegistrationNamesTheSessionItsClaimAbandoned(t *testing.T) {
	h := newControllerHarness(t, true)
	token, boot := h.launchController()
	wantReplaces(t, "the launched controller's first registration", answerKeys(t, h.registration(boot, "ses_first", 0)), "")

	boot = h.abandon(token, "ses_first")
	body := h.registration(boot, "ses_second", 0)
	wantReplaces(t, "the launched controller's registration of its new session", answerKeys(t, body), "ses_first")
	var registered ControllerRegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil || registered.ClaimToken != token || registered.Replaces != "ses_first" {
		t.Errorf("the launched controller's registration answered %s (%v), want %s replacing ses_first", body, err, token)
	}
}

// The operator's controller (`controller: operator`) registers on no claim, so no forget records
// a session it left behind: a later `legion controller start`'s registration of a new session is
// answered no `replaces`, as the first one is.
func TestTheOperatorsControllerIsAnsweredNoReplaces(t *testing.T) {
	h := newControllerHarness(t, false)
	for _, session := range []string{"ses_operator_first", "ses_operator_second"} {
		recorder := h.register(h.mintedSecret(), session)
		if recorder.Code != http.StatusOK {
			t.Fatalf("register %s with the controller capability = %d, want 200; body %s", session, recorder.Code, recorder.Body)
		}
		wantReplaces(t, "the operator's controller's registration of "+session, answerKeys(t, recorder.Body.Bytes()), "")
	}
}
