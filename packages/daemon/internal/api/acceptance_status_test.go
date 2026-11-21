package api

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// acceptanceJudge is a stand-in for the workflow's judgement of sign_off and close_root
// (AcceptanceJudge): refusal, nil to take each act, and every issue it was asked of, each asked in
// a read-only transaction, which judged records.
type acceptanceJudge struct {
	refusal *intake.Refusal
	asked   []string
	judged  []bool
}

func (j *acceptanceJudge) judge(tx pgx.Tx, act, issue string) *intake.Refusal {
	j.asked = append(j.asked, act+" "+issue)
	j.judged = append(j.judged, tx.Conn() != nil)
	return j.refusal
}

func (j *acceptanceJudge) SignOffRefusal(_ context.Context, tx pgx.Tx, issue string) (*intake.Refusal, error) {
	return j.judge(tx, "sign_off", issue), nil
}

func (j *acceptanceJudge) CloseRootRefusal(_ context.Context, tx pgx.Tx, issue string) (*intake.Refusal, error) {
	return j.judge(tx, "close_root", issue), nil
}

// newAcceptanceHarness is newArchitectHarness with judge as the workflow's judgement of sign_off
// and close_root, and written as the sign-off's seam, each nil for none. Dispatch shows the tree
// root LEGION-208 in needs_review at sequence 7.
func newAcceptanceHarness(t *testing.T, judge AcceptanceJudge, written func(context.Context, string)) (*harness, *factRecorder, *statusRecorder) {
	t.Helper()
	h := newHarness(t)
	facts := &factRecorder{}
	statuses := &statusRecorder{issues: map[string]dispatch.Issue{"LEGION-208": {Key: "LEGION-208", Status: "needs_review", LastSeq: 7}}}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(), Handlers: []intake.Handler{facts}, Dispatch: statuses,
		Acceptance: judge, AfterSignOffStatusWrite: written,
	}).Handler
	seedTree(t, h, "LEGION-208", "LEGION-209")
	return h, facts, statuses
}

// sign_off and close_root write their status in Dispatch, asking it to refuse the write over an
// open child of another project, before they apply their fact: the judgement the workflow makes
// read-only comes first, and its refusal writes nothing; Dispatch's refusal is SIGNOFF_CHILD_OPEN
// naming each such child with its project and status, and applies nothing; and the sign-off's seam
// runs once the write has landed and before the fact.
func TestAnAcceptanceIsWrittenInDispatchBeforeItsFact(t *testing.T) {
	judge := &acceptanceJudge{}
	var seen []string
	var h *harness
	var facts *factRecorder
	var statuses *statusRecorder
	h, facts, statuses = newAcceptanceHarness(t, judge, func(_ context.Context, issue string) {
		seen = append(seen, issue+" "+statuses.issues[issue].Status+" with "+strings.Repeat("a fact ", len(facts.recorded())))
	})
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	signOff := func() (int, string) {
		t.Helper()
		answer := h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil)
		if answer.Code == http.StatusOK {
			return answer.Code, ""
		}
		return answer.Code, refused(t, answer, answer.Code, "SIGNOFF_CHILD_OPEN")
	}

	// The workflow refuses: nothing is written.
	judge.refusal = &intake.Refusal{Status: http.StatusConflict, Code: "SIGNOFF_OUTSIDE_ACCEPTING", Message: "LEGION-208 is in phase planning"}
	refused(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil),
		http.StatusConflict, "SIGNOFF_OUTSIDE_ACCEPTING")
	if len(statuses.writes) != 0 || len(facts.recorded()) != 0 || len(seen) != 0 {
		t.Fatalf("a sign-off the workflow refused wrote %+v, applied %#v and ran the seam %q, want none", statuses.writes, facts.recorded(), seen)
	}
	if want := []string{"sign_off LEGION-208"}; !reflect.DeepEqual(judge.asked, want) || !judge.judged[0] {
		t.Fatalf("the judgement was asked %q (in a transaction: %v), want %q", judge.asked, judge.judged, want)
	}
	judge.refusal = nil

	// Dispatch refuses over an open child of another project: nothing is applied.
	statuses.foreignOpen = map[string][]dispatch.ForeignChild{"LEGION-208": {{Key: "OTHR-5", Project: "OTHR", Status: "triage"}, {Key: "OTHR-7", Project: "OTHR", Status: "in_progress"}}}
	status, message := signOff()
	if status != http.StatusConflict || !strings.Contains(message, "OTHR-5 (OTHR, triage), OTHR-7 (OTHR, in_progress)") || !strings.Contains(message, "Sign off again") {
		t.Fatalf("the sign-off over open children of another project = %d %q, want 409 SIGNOFF_CHILD_OPEN naming both", status, message)
	}
	if len(statuses.writes) != 0 || len(facts.recorded()) != 0 || len(seen) != 0 {
		t.Fatalf("a sign-off Dispatch refused wrote %+v, applied %#v and ran the seam %q, want none", statuses.writes, facts.recorded(), seen)
	}
	statuses.foreignOpen = nil

	// Taken: retro is written, then the seam runs, then the fact applies.
	if status, message := signOff(); status != http.StatusOK {
		t.Fatalf("the sign-off = %d %q, want 200", status, message)
	}
	if want := []string{"LEGION-208=retro"}; !reflect.DeepEqual(statuses.acceptances, want) {
		t.Fatalf("acceptance writes = %q, want %q", statuses.acceptances, want)
	}
	if want := []string{"LEGION-208 retro with "}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("the seam saw %q, want Dispatch at retro and no fact applied yet: %q", seen, want)
	}
	if got := facts.recorded(); len(got) != 1 || got[0] != (intake.SignOff{Issue: "LEGION-208"}) {
		t.Fatalf("facts = %#v, want the sign-off", got)
	}

	// close_root is judged as its own act and refused by Dispatch the same way, naming its act.
	statuses.foreignOpen = map[string][]dispatch.ForeignChild{"LEGION-208": {{Key: "OTHR-6", Project: "OTHR", Status: "backlog"}}}
	answer := h.request(http.MethodPost, "/legion/v1/roots/close", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208", "reason": "moot"}, nil)
	if message := refused(t, answer, http.StatusConflict, "SIGNOFF_CHILD_OPEN"); !strings.Contains(message, "OTHR-6 (OTHR, backlog)") ||
		!strings.Contains(message, "is not closed") || !strings.Contains(message, "done write") {
		t.Fatalf("the close over an open child of another project = %q, want it naming OTHR-6 and the done write", message)
	}
	if judge.asked[len(judge.asked)-1] != "close_root LEGION-208" || len(facts.recorded()) != 1 {
		t.Fatalf("the close was judged as %q and applied %#v, want close_root judged and nothing applied", judge.asked, facts.recorded())
	}
}

// A fact that does not take after its status was written writes back the status Dispatch showed
// before the write, unless the record no longer holds the issue in the workflow: then something
// closed it since, and the done or the retro Dispatch shows stands.
func TestAnAcceptanceWhoseFactDoesNotTakeWritesTheStatusBack(t *testing.T) {
	h, facts, statuses := newAcceptanceHarness(t, nil, nil)
	facts.refusal = &intake.Refusal{Status: http.StatusConflict, Code: "SIGNOFF_RESTING_ROOT_IN_WORKFLOW", Message: "LEGION-300 rests on LEGION-208"}
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	refused(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil),
		http.StatusConflict, "SIGNOFF_RESTING_ROOT_IN_WORKFLOW")
	if want := []statusWrite{{"LEGION-208", "retro"}, {"LEGION-208", "needs_review"}}; !reflect.DeepEqual(statuses.writes, want) {
		t.Fatalf("Dispatch writes = %+v, want retro and then needs_review written back: %+v", statuses.writes, want)
	}

	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "update issues set phase = $2, status = 'done' where key = $1", "LEGION-208", string(phase.Done))
		return err
	}); err != nil {
		t.Fatalf("take LEGION-208 out of the workflow: %v", err)
	}
	statuses.writes = nil
	refused(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil),
		http.StatusConflict, "SIGNOFF_RESTING_ROOT_IN_WORKFLOW")
	if want := []statusWrite{{"LEGION-208", "retro"}}; !reflect.DeepEqual(statuses.writes, want) {
		t.Fatalf("Dispatch writes over an issue the record holds out of the workflow = %+v, want retro alone: %+v", statuses.writes, want)
	}
}
