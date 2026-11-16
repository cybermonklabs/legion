// packages/envoy/internal/broker/requests/judge_hook_test.go
// The two seams the broker's model judge reaches the request machine through, against the real
// database: Machine.Judge, the hook Create hands a request it has just written pending, and
// Machine.JudgePolicy, the policy question a run asks again before it decides.
package requests

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// judged records every request a fixture's hook was handed, in order. The hook runs inside
// Create, on the caller's own goroutine, so a plain slice is the whole of what it needs.
type judged struct{ handed []PendingRequest }

// watchJudge sets m.Judge to a hook that records what it is handed, and answers the record.
func watchJudge(m *Machine) *judged {
	j := &judged{}
	m.Judge = func(p PendingRequest) { j.handed = append(j.handed, p) }
	return j
}

// judgedSession is the Envoy session the enrollment of these tests records, as agentbox's
// --session-id writes one, so the hook's SessionID is read rather than empty either way.
const judgedSession = "legion-1353"

// TestCreateHandsAPendingRequestToTheJudgeOnce: the one request Create wrote pending reaches the
// hook exactly once, and every field of it is the one the request it just wrote carries — the
// record and request ids it returned, the enrollment that asked and the session that enrollment
// recorded, the names in the request's own order, the lifetime the requester signed, that
// requester's signed reason, and the approver the record names.
func TestCreateHandsAPendingRequestToTheJudgeOnce(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	nameTheSession(t, m, enr, judgedSession, fixtureOperator)
	watched := watchJudge(m)

	const reason = "Rotating the Deel key for INFRA-1353"
	req, err := m.Create(t.Context(), enr, signJudgeable(t, m, key, reason, 900, "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) = %+v, %v; want a pending request", req, err)
	}
	if len(watched.handed) != 1 {
		t.Fatalf("the judge was handed %d requests; want exactly the one Create wrote pending", len(watched.handed))
	}
	want := PendingRequest{
		RecordID: *req.RecordID, RequestID: req.ID, EnrollmentID: enr, SessionID: judgedSession,
		Secrets: []string{"DEEL_API_KEY"}, LifetimeSeconds: new(900), Reason: reason, Approver: approver,
	}
	got := watched.handed[0]
	if got.RecordID != want.RecordID || got.RequestID != want.RequestID || got.EnrollmentID != want.EnrollmentID ||
		got.SessionID != want.SessionID || !slices.Equal(got.Secrets, want.Secrets) || got.Reason != want.Reason ||
		got.Approver != want.Approver || got.LifetimeSeconds == nil || *got.LifetimeSeconds != *want.LifetimeSeconds {
		t.Fatalf("the judge was handed %+v (lifetime %v); want %+v (lifetime %d)",
			got, got.LifetimeSeconds, want, *want.LifetimeSeconds)
	}
	// The request row the hook names is the one the database holds, and the record is its own:
	// a hook handed ids that name nothing would judge a request that is not there.
	var state, recordID string
	if err := m.Store.Pool.QueryRow(t.Context(), `select state, record_id::text from requests where id=$1`, got.RequestID).
		Scan(&state, &recordID); err != nil {
		t.Fatalf("read the request the judge was handed: %v", err)
	}
	if state != "pending" || recordID != got.RecordID {
		t.Fatalf("the request the judge was handed is %s with record %s; want pending with record %s", state, recordID, got.RecordID)
	}
}

// TestTheJudgeIsHandedAnAnyoneApproverRecord: a shared human-tier secret's record waits on
// anyone, and that sentinel is what the hook carries, since it is who the judge decides in the
// place of.
func TestTheJudgeIsHandedAnAnyoneApproverRecord(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	watched := watchJudge(m)
	if req, err := m.Create(t.Context(), enr, signJudgeable(t, m, key, "Reading the shared key for INFRA-1353", 900, "SHARED_KEY"), ""); err != nil || req.State != "pending" {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want a pending request", req, err)
	}
	if len(watched.handed) != 1 || watched.handed[0].Approver != record.AnyoneApprover {
		t.Fatalf("the judge was handed %+v; want one request whose approver is %s", watched.handed, record.AnyoneApprover)
	}
}

// TestTheJudgeIsNotHandedACoalescedRequest: a second ask for the same names and the same lifetime
// joins the pending request already there rather than opening a second card, and the judge is
// handed nothing for it — so one record is judged once however many times its session asks.
func TestTheJudgeIsNotHandedACoalescedRequest(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	watched := watchJudge(m)
	const reason = "Rotating the Deel key for INFRA-1353"
	first, err := m.Create(t.Context(), enr, signJudgeable(t, m, key, reason, 900, "DEEL_API_KEY"), "")
	if err != nil || first.State != "pending" {
		t.Fatalf("the first Create = %+v, %v; want a pending request", first, err)
	}
	again, err := m.Create(t.Context(), enr, signJudgeable(t, m, key, reason, 900, "DEEL_API_KEY"), "")
	if err != nil || !again.Coalesced || again.ID != first.ID {
		t.Fatalf("the second Create = %+v, %v; want it coalesced onto request %s", again, err, first.ID)
	}
	if len(watched.handed) != 1 || watched.handed[0].RequestID != first.ID {
		t.Fatalf("the judge was handed %+v; want the first request alone", watched.handed)
	}
}

// TestTheJudgeIsNotHandedARequestThePolicyDecided: a request the policy grants at once and one it
// denies at once are both answered without a person, so there is nothing for the judge to decide
// and it is handed neither.
func TestTheJudgeIsNotHandedARequestThePolicyDecided(t *testing.T) {
	for name, c := range map[string]struct{ secret, state string }{
		"granted at once": {"AUTO_TOKEN", "granted"},
		"denied at once":  {"SERVICE_KEY", "denied"},
	} {
		t.Run(name, func(t *testing.T) {
			m, enr, key, _ := newFixture(t)
			watched := watchJudge(m)
			req, err := m.Create(t.Context(), enr, signRequest(t, m, key, "need it for INFRA-1353", c.secret), "")
			if err != nil || req.State != c.state {
				t.Fatalf("Create(%s) = %+v, %v; want %s", c.secret, req, err, c.state)
			}
			if len(watched.handed) != 0 {
				t.Fatalf("the judge was handed %+v for a %s request; want nothing", watched.handed, c.state)
			}
		})
	}
}

// TestANilJudgeIsNoJudge: the hook is unset in every test and in every deployment that
// configures no judge, and such a broker writes the pending request and its record exactly as
// before rather than faulting on a nil call.
func TestANilJudgeIsNoJudge(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	if m.Judge != nil {
		t.Fatal("newFixture built a machine with a judge; a machine nobody wires has none")
	}
	req, err := m.Create(t.Context(), enr, signJudgeable(t, m, key, "Rotating the Deel key for INFRA-1353", 900, "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil || req.Approver == nil || *req.Approver != approver {
		t.Fatalf("Create with no judge = %+v, %v; want the pending request %s waits for", req, err, approver)
	}
}

// TestJudgePolicyAnswersTheLivePolicy: the decision the judge's third check reads is the one the
// policy makes for the enrollment that asked, right now — an approval for a human-tier secret
// that is still the operator's, and the deny the policy gives a service's secret to a session
// that is not that service's.
func TestJudgePolicyAnswersTheLivePolicy(t *testing.T) {
	m, enr, _, approver := newFixture(t)
	decision, withheld, admin, err := m.JudgePolicy(t.Context(), enr, "DEEL_API_KEY")
	if err != nil || decision.Outcome != policy.Approval || decision.Approver != approver || withheld || admin {
		t.Fatalf("JudgePolicy(DEEL_API_KEY) = %+v, withheld %v, admin %v, %v; want an approval by %s, neither withheld nor an administrator's", decision, withheld, admin, err, approver)
	}
	// The requester is read from the database and not from the request: this session proves no
	// service, so the service's own secret is denied it.
	if decision, withheld, admin, err := m.JudgePolicy(t.Context(), enr, "SERVICE_KEY"); err != nil || decision.Outcome != policy.Deny || withheld || admin {
		t.Fatalf("JudgePolicy(SERVICE_KEY) for a person's session = %+v, withheld %v, admin %v, %v; want a deny", decision, withheld, admin, err)
	}
}

// TestJudgePolicyReportsAnAdminSecret: the admin tag reaches the judge beside the decision, and
// it is the only way it could reach it — the policy answers an administrator's secret with the
// ordinary approval its owner makes, exactly as it answers the human-tier secret beside it, so
// nothing in the outcome tells the two apart. The judge decides none of the first sort, whatever
// seconds a rules file gives it for the name, and this is the reading its admin check makes.
func TestJudgePolicyReportsAnAdminSecret(t *testing.T) {
	m, enr, _, approver := newFixture(t)
	retag(t, m, adminSecret())
	decision, withheld, admin, err := m.JudgePolicy(t.Context(), enr, adminSecretName)
	if err != nil || decision.Outcome != policy.Approval || decision.Approver != approver || withheld || !admin {
		t.Fatalf("JudgePolicy(%s) = %+v, withheld %v, admin %v, %v; want an approval by %s that says the secret is an administrator's", adminSecretName, decision, withheld, admin, err, approver)
	}
	// The flag is the secret's own: the human-tier secret the fixture has always served, whose
	// decision reads the same, is not an administrator's.
	if decision, _, admin, err := m.JudgePolicy(t.Context(), enr, "DEEL_API_KEY"); err != nil || decision.Outcome != policy.Approval || admin {
		t.Fatalf("JudgePolicy(DEEL_API_KEY) beside an admin secret = %+v, admin %v, %v; want the same approval and admin false", decision, admin, err)
	}
	// A name the policy does not serve is no administrator's secret either, and says so rather
	// than reading the zero value of a secret that is not there as anything else.
	if _, _, admin, err := m.JudgePolicy(t.Context(), enr, "NO_SUCH_KEY"); !errors.Is(err, policy.ErrUnknownSecret) || admin {
		t.Fatalf("JudgePolicy(NO_SUCH_KEY) = admin %v, %v; want policy.ErrUnknownSecret and admin false", admin, err)
	}
}

// TestJudgePolicyReportsAWithheldName: the operator's revoke of an automatic grant withholds that
// name from the session, and JudgePolicy says so beside the decision — the policy answers the
// withheld name with an approval, which is exactly why the judge needs the flag as well as the
// outcome: a name its owner took back is not one the judge hands straight back.
func TestJudgePolicyReportsAWithheldName(t *testing.T) {
	m, enr, key, operator := newFixture(t)
	auto, err := m.Create(t.Context(), enr, signRequest(t, m, key, "need it for INFRA-1353", "AUTO_TOKEN"), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want an automatic grant", auto, err)
	}
	if decision, withheld, _, err := m.JudgePolicy(t.Context(), enr, "AUTO_TOKEN"); err != nil || decision.Outcome != policy.Automatic || withheld {
		t.Fatalf("JudgePolicy(AUTO_TOKEN) before the withhold = %+v, withheld %v, %v; want automatic and not withheld", decision, withheld, err)
	}
	if err := m.RevokeByApprover(t.Context(), *auto.GrantID, operator, record.HumanActor(operator)); err != nil {
		t.Fatalf("RevokeByApprover: %v", err)
	}
	decision, withheld, _, err := m.JudgePolicy(t.Context(), enr, "AUTO_TOKEN")
	if err != nil || !withheld || decision.Outcome != policy.Approval {
		t.Fatalf("JudgePolicy(AUTO_TOKEN) after the operator took it back = %+v, withheld %v, %v; want withheld, and an approval", decision, withheld, err)
	}
	// Another name of the same session is untouched: the flag is per name, not per session.
	if _, withheld, _, err := m.JudgePolicy(t.Context(), enr, "DEEL_API_KEY"); err != nil || withheld {
		t.Fatalf("JudgePolicy(DEEL_API_KEY) after AUTO_TOKEN was withheld = withheld %v, %v; want it not withheld", withheld, err)
	}
}

// TestJudgePolicyRefusesAnEnrollmentThatIsNotLive: a revoked enrollment, and one that was never
// there, are each an error rather than a decision. A judge that read silence as approval would
// grant a credential to a box that has gone.
func TestJudgePolicyRefusesAnEnrollmentThatIsNotLive(t *testing.T) {
	m, enr, _, _ := newFixture(t)
	if _, err := m.Store.Pool.Exec(context.Background(), `update enrollments set revoked_at=now() where id=$1`, enr); err != nil {
		t.Fatalf("revoke the enrollment: %v", err)
	}
	decision, withheld, admin, err := m.JudgePolicy(t.Context(), enr, "DEEL_API_KEY")
	if !errors.Is(err, pgx.ErrNoRows) || decision.Outcome != "" || withheld || admin {
		t.Fatalf("JudgePolicy on a revoked enrollment = %+v, withheld %v, admin %v, %v; want an error carrying pgx.ErrNoRows and no decision", decision, withheld, admin, err)
	}
	if _, _, _, err := m.JudgePolicy(t.Context(), "00000000-0000-0000-0000-000000000000", "DEEL_API_KEY"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("JudgePolicy on an enrollment that was never there = %v; want an error carrying pgx.ErrNoRows", err)
	}
}
