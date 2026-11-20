// packages/envoy/internal/broker/approval/judge_page_test.go
// What the approval page says about a record of a judge key: read-only while the model judge
// holds it, an ordinary card once the judge has handed it to its person, and, once the judge has
// decided it, who the page names as its decider.
package approval_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// judgeModel is the model deciding in this file; the name the page prints for it is
// record.JudgeActor of it, which is also what requests.decided_by keeps and what
// `agent-secrets status` prints under decided_by.
const judgeModel = "claude-opus-4-6"

// judgeable gives the rig's broker airlock rules that let the judge decide up to 900 seconds of
// TEST_SECRET, ada's own human-tier secret, and wires a judge, a hook that does nothing, so a
// request for TEST_SECRET is held for the judge from the moment it is written.
func judgeable(r *rig) {
	seconds := uint32(900)
	r.machine.Airlock = airlock.Rules{Secrets: []airlock.SecretRule{
		{Name: "TEST_SECRET", Hosts: []string{"api.test.example"}, Judge: &seconds, Guidance: "grant for the billing job"},
	}}
	r.machine.Judge = func(requests.PendingRequest) {}
}

// TestThePageNamesTheModelJudgeAsTheDecider: the judge's approval carries no login at all — the
// person who would otherwise have decided is named nowhere in it — so the page reads the
// decision's actor and names the judge, and never the record's approver, who decided nothing.
// While the judge held the record, its approver was offered no decision of it.
func TestThePageNamesTheModelJudgeAsTheDecider(t *testing.T) {
	r := newRig(t)
	judgeable(r)
	enrollmentID, key := r.benSession(t, "ben-box")
	lifetime := 600
	p := r.request(t, enrollmentID, key, "the billing job needs the key", &lifetime, "", "TEST_SECRET")

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	if form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID); form != nil {
		t.Fatalf("the page offers %s a decision of a record the judge holds: %v", ada, form)
	}

	if _, err := r.machine.ApplyJudgeDecision(t.Context(), p.recordID, judgeModel, map[string]any{"verdict": "grant"}); err != nil {
		t.Fatalf("ApplyJudgeDecision: %v", err)
	}
	judge := record.JudgeActor(judgeModel)

	body := b.Page(t, p.recordID).Body
	block := pageText(t, recordBlock(t, body, p.recordID))
	if !strings.Contains(block, "State approved by the model judge "+judge+" at ") {
		t.Errorf("the judged record's block does not name %s as its decider:\n%s", judge, block)
	}
	if strings.Contains(block, "State approved by "+ada) {
		t.Errorf("the judged record's block names %s, who decided nothing, as its decider:\n%s", ada, block)
	}
	if form := decisionForm(t, body, p.recordID); form != nil {
		t.Errorf("the page offers %s a decision of a record the judge decided: %v", ada, form)
	}
	if grants, events := r.grants(t, p.recordID), r.terminalEvents(t, p.recordID); grants != 1 || events != 1 {
		t.Errorf("the record has %d grants and %d terminal events, want the judge's one of each", grants, events)
	}
}

// TestThePageNamesThePolicyThatTurnedAJudgesApprovalIntoADenial: a judge's approval of a request
// whose session the current policy has dropped from the secret's requesters tag is not refused
// and not granted. applyDecisionAttempt records it as the request's own denial under
// requests.PolicyDeniedActor, which overwrites the judge's actor on the event, and the judge's
// event carries no login, so the page names the policy, which made that answer, and not nobody.
func TestThePageNamesThePolicyThatTurnedAJudgesApprovalIntoADenial(t *testing.T) {
	r := newRig(t)
	judgeable(r)
	// TEST_SECRET lists ben, whose machine asks for it, so the request waits on ada as before.
	r.retag(t, policytest.ListedSecret("TEST_SECRET", ada, policy.TierHuman, ben, "test-v1"))
	enrollmentID, key := r.benSession(t, "ben-box")
	lifetime := 600
	p := r.request(t, enrollmentID, key, "the billing job needs the key", &lifetime, "", "TEST_SECRET")

	// The list drops ben while the request waits on the model: no login can approve the name
	// now, so the approval the judge then makes is recorded as the request's denial.
	r.retag(t, policytest.ListedSecret("TEST_SECRET", ada, policy.TierHuman, carol, "test-v1"))
	applied, err := r.machine.ApplyJudgeDecision(t.Context(), p.recordID, judgeModel, map[string]any{"verdict": "grant"})
	want := "TEST_SECRET: " + policy.ReasonRequesterNotListed
	if err != nil || applied.State != "denied" || applied.Detail != want {
		t.Fatalf("the judge's approval after the list dropped the session = %+v, %v; want it denied with the detail %q", applied, err, want)
	}

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	block := pageText(t, recordBlock(t, b.Page(t, p.recordID).Body, p.recordID))
	if !strings.Contains(block, "State denied by the current policy at ") {
		t.Errorf("the policy-denied record's block does not name the current policy as its decider:\n%s", block)
	}
	if strings.Contains(block, "State denied at ") {
		t.Errorf("the policy-denied record's block names nobody as its decider:\n%s", block)
	}
	if grants, events := r.grants(t, p.recordID), r.terminalEvents(t, p.recordID); grants != 0 || events != 1 {
		t.Errorf("the record has %d grants and %d terminal events, want no grant and the policy's one denial", grants, events)
	}
}

// TestThePageOffersNoDecisionWhileTheJudgeHolds: while the judge holds a record, its page —
// opened by ada herself, who decides any other request for TEST_SECRET — prints it read-only,
// saying the judge is deciding it, with no Approve and no Deny. A POST naming it, sent with the
// form of a card ada does decide, decides nothing, and the machine refuses ada's decision of it
// outright. Once the judge hands it over, the same page offers ada the ordinary card.
func TestThePageOffersNoDecisionWhileTheJudgeHolds(t *testing.T) {
	r := newRig(t)
	judgeable(r)
	lifetime := 600
	// The card ada decides: written by a broker that runs no judge, so it was hers from the
	// start.
	r.machine.Judge = nil
	otherEnrollment, otherKey := r.benSession(t, "ben-other-box")
	other := r.request(t, otherEnrollment, otherKey, "the billing job needs the key", &lifetime, "", "TEST_SECRET")
	r.machine.Judge = func(requests.PendingRequest) {}
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "the billing job needs the key", &lifetime, "", "TEST_SECRET")

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	body := b.Page(t, p.recordID).Body
	if form := decisionForm(t, body, p.recordID); form != nil {
		t.Fatalf("the page offers %s a decision of a record the judge holds: %v", ada, form)
	}
	block := pageText(t, recordBlock(t, body, p.recordID))
	if want := "The model judge is deciding this request"; !strings.Contains(block, want) {
		t.Errorf("the record's block does not say %q:\n%s", want, block)
	}
	if strings.Contains(block, "Approve gives this session") {
		t.Errorf("the record's block tells %s what Approve gives, for a request she cannot approve:\n%s", ada, block)
	}

	form := decisionForm(t, b.Page(t, other.recordID).Body, other.recordID)
	if form == nil {
		t.Fatalf("the page offers %s no decision of the card she does decide", ada)
	}
	forged := decision(form, "approve")
	forged.Set("record", p.recordID)
	b.FromPage(t, "/approve", forged)
	if grants, events := r.grants(t, p.recordID), r.terminalEvents(t, p.recordID); grants != 0 || events != 0 {
		t.Errorf("after the POST naming it the record has %d grants and %d terminal events; want neither", grants, events)
	}
	if _, err := r.machine.ApplyDecision(t.Context(), p.recordID, true, ada); !errors.Is(err, requests.ErrJudgeHolds) {
		t.Errorf("ApplyDecision by %s while the judge holds = %v; want requests.ErrJudgeHolds", ada, err)
	}

	if err := r.machine.LogJudged(t.Context(), p.recordID, judgeModel, map[string]any{"verdict": "deny", "error": "the model did not answer"}); err != nil {
		t.Fatalf("the hand-over: %v", err)
	}
	if form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID); form == nil {
		t.Errorf("after the hand-over the page offers %s no decision of the record; want the ordinary card", ada)
	}
}
