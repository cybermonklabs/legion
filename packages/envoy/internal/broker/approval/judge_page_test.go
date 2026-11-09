// packages/envoy/internal/broker/approval/judge_page_test.go
// What the approval page says about a record the model judge decided: who the page names as its
// decider, and what the person who clicks Approve after it reads.
package approval_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// judgeModel is the model deciding in this file; the name the page prints for it is
// record.JudgeActor of it, which is also what requests.decided_by keeps and what
// `agent-secrets status` prints under decided_by.
const judgeModel = "claude-opus-4-6"

// judgeable gives the rig's broker airlock rules that let the judge decide up to 900 seconds of
// TEST_SECRET, ada's own human-tier secret, which is what record.MayJudge reads before any model
// is asked.
func judgeable(r *rig) {
	seconds := uint32(900)
	r.machine.Airlock = airlock.Rules{Secrets: []airlock.SecretRule{
		{Name: "TEST_SECRET", Hosts: []string{"api.test.example"}, Judge: &seconds},
	}}
}

// TestThePageNamesTheModelJudgeAsTheDecider: the judge's approval carries no login at all — the
// person who would otherwise have decided is named nowhere in it — so a page that read the login
// alone said "approved" with nobody beside it, and the person who clicked Approve after the
// judge read "granted (approved by at <time>)", which reads as their own approval. The page
// reads the decision's actor instead:
//
//   - the record's State line names the judge, and never the record's approver, who decided
//     nothing;
//   - the approver's own late Approve, from a page they opened while the request was pending,
//     writes nothing and answers a line that opens "already decided:" and names the judge.
//
// Ada is TEST_SECRET's owner and the record's approver throughout, so every line that names the
// judge is a line that could have named her instead.
func TestThePageNamesTheModelJudgeAsTheDecider(t *testing.T) {
	r := newRig(t)
	judgeable(r)
	enrollmentID, key := r.benSession(t, "ben-box")
	lifetime := 600
	p := r.request(t, enrollmentID, key, "INFRA-1353 needs the billing key", &lifetime, "", "TEST_SECRET")

	// Ada opens the request while it is still pending and keeps its form, as a person does who
	// reads a request before deciding it.
	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
	if form == nil {
		t.Fatalf("the pending request offers %s no decision form", ada)
	}

	if _, err := r.machine.ApplyJudgeDecision(t.Context(), p.recordID, judgeModel, map[string]any{"verdict": "approve"}); err != nil {
		t.Fatalf("ApplyJudgeDecision: %v", err)
	}
	judge := record.JudgeActor(judgeModel)

	block := pageText(t, recordBlock(t, b.Page(t, p.recordID).Body, p.recordID))
	if !strings.Contains(block, "State approved by the model judge "+judge+" at ") {
		t.Errorf("the judged record's block does not name %s as its decider:\n%s", judge, block)
	}
	if strings.Contains(block, "State approved by "+ada) {
		t.Errorf("the judged record's block names %s, who decided nothing, as its decider:\n%s", ada, block)
	}

	res := b.FromPage(t, "/approve", decision(form, "approve"))
	if res.Status != http.StatusSeeOther {
		t.Fatalf("%s's Approve after the judge = %d, want 303 to the record with its outcome:\n%s", ada, res.Status, res.Body)
	}
	after := pageText(t, b.Get(t, res.Header.Get("Location")).Body)
	if want := "TEST_SECRET: already decided: granted (approved by " + judge + " at "; !strings.Contains(after, want) {
		t.Errorf("the page after %s's late Approve does not say %q:\n%s", ada, want, after)
	}
	if strings.Contains(after, "granted (approved by "+ada) {
		t.Errorf("the page after %s's late Approve reads as her own approval:\n%s", ada, after)
	}
	if grants, events := r.grants(t, p.recordID), r.terminalEvents(t, p.recordID); grants != 1 || events != 1 {
		t.Errorf("after the late Approve the record has %d grants and %d terminal events, want the judge's one of each", grants, events)
	}
}

// TestThePageNamesThePolicyThatTurnedAJudgesApprovalIntoADenial: a judge's approval of a request
// whose session the current policy has dropped from the secret's requesters tag is not refused
// and not granted. applyDecisionAttempt records it as the request's own denial under
// requests.PolicyDeniedActor, which overwrites the judge's actor on the event, and the judge's
// event carries no login — so the page had nothing left to read and said "denied by  at <time>",
// with no name, and ada's late Approve read as a denial nobody made.
//
// The policy is what such an event names, since the policy and not the judge made that answer:
//
//   - the record's State line reads "denied by the current policy";
//   - ada's own late Approve, from a page she opened while the request was pending, writes
//     nothing and answers a line that opens "already decided:" and names the policy.
//
// Ada is TEST_SECRET's owner and the record's approver throughout. Her own approval the policy
// denies keeps her login on the event, and is the case this one must not change.
func TestThePageNamesThePolicyThatTurnedAJudgesApprovalIntoADenial(t *testing.T) {
	r := newRig(t)
	judgeable(r)
	// TEST_SECRET lists ben, whose machine asks for it, so the request waits on ada as before.
	r.retag(t, policytest.ListedSecret("TEST_SECRET", ada, policy.TierHuman, ben, "test-v1"))
	enrollmentID, key := r.benSession(t, "ben-box")
	lifetime := 600
	p := r.request(t, enrollmentID, key, "INFRA-1353 needs the billing key", &lifetime, "", "TEST_SECRET")

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
	if form == nil {
		t.Fatalf("the pending request offers %s no decision form", ada)
	}

	// The list drops ben while the request waits on the model: no login can approve the name
	// now, so the approval the judge then makes is recorded as the request's denial.
	r.retag(t, policytest.ListedSecret("TEST_SECRET", ada, policy.TierHuman, carol, "test-v1"))
	applied, err := r.machine.ApplyJudgeDecision(t.Context(), p.recordID, judgeModel, map[string]any{"verdict": "approve"})
	want := "TEST_SECRET: " + policy.ReasonRequesterNotListed
	if err != nil || applied.State != "denied" || applied.Detail != want {
		t.Fatalf("the judge's approval after the list dropped the session = %+v, %v; want it denied with the detail %q", applied, err, want)
	}

	block := pageText(t, recordBlock(t, b.Page(t, p.recordID).Body, p.recordID))
	if !strings.Contains(block, "State denied by the current policy at ") {
		t.Errorf("the policy-denied record's block does not name the current policy as its decider:\n%s", block)
	}
	if strings.Contains(block, "State denied at ") {
		t.Errorf("the policy-denied record's block names nobody as its decider:\n%s", block)
	}

	res := b.FromPage(t, "/approve", decision(form, "approve"))
	if res.Status != http.StatusSeeOther {
		t.Fatalf("%s's Approve after the policy's denial = %d, want 303 to the record with its outcome:\n%s", ada, res.Status, res.Body)
	}
	after := pageText(t, b.Get(t, res.Header.Get("Location")).Body)
	if want := "TEST_SECRET: already decided: denied by the current policy at "; !strings.Contains(after, want) {
		t.Errorf("the page after %s's late Approve does not say %q:\n%s", ada, want, after)
	}
	if grants, events := r.grants(t, p.recordID), r.terminalEvents(t, p.recordID); grants != 0 || events != 1 {
		t.Errorf("after the late Approve the record has %d grants and %d terminal events, want no grant and the policy's one denial", grants, events)
	}
}
