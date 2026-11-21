package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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

// newWorkflowAcceptanceHarness is newAcceptanceHarness with the workflow itself as the judgement
// and as the handler of each fact, and LEGION-208 recorded in accepting, so a sign-off is taken or
// refused as the daemon takes it. written is the sign-off's seam, given the harness and the
// engine. Dispatch shows LEGION-208 in needs_review at sequence 7.
func newWorkflowAcceptanceHarness(t *testing.T, written func(ctx context.Context, h *harness, engine *workflow.Engine)) (*harness, *statusRecorder) {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testDispatchProject}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	statuses := &statusRecorder{issues: map[string]dispatch.Issue{"LEGION-208": {Key: "LEGION-208", Status: "needs_review", LastSeq: 7}}}
	seedTree(t, h, "LEGION-208")
	if _, err := h.store.Pool().Exec(t.Context(), "update issues set phase = $1, status = 'needs_review' where key = 'LEGION-208'", string(phase.Accepting)); err != nil {
		t.Fatalf("put LEGION-208 in accepting: %v", err)
	}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Grants: credential.New(nil), Pool: h.store.Pool(), Record: records, Handlers: []intake.Handler{engine}, Dispatch: statuses,
		Acceptance: engine, AfterSignOffStatusWrite: func(ctx context.Context, _ string) { written(ctx, h, engine) },
	}).Handler
	return h, statuses
}

// recordedPhase is the phase the daemon records issue in.
func recordedPhase(t *testing.T, h *harness, issue string) phase.Phase {
	t.Helper()
	var at string
	if err := h.store.Pool().QueryRow(t.Context(), "select phase from issues where key = $1", issue).Scan(&at); err != nil {
		t.Fatalf("read the phase of %s: %v", issue, err)
	}
	return phase.Phase(at)
}

// Two sign-offs of one issue can both pass the judgement before either records. Here the other
// one's fact takes the issue to retro while this one waits between its Dispatch write and its own
// fact, which the workflow then refuses, the issue being no longer in accepting. Dispatch keeps the
// retro the daemon records, not the needs_review this sign-off read before its write.
func TestASignOffThatLosesToAnotherLeavesTheAcceptanceInDispatch(t *testing.T) {
	h, statuses := newWorkflowAcceptanceHarness(t, func(ctx context.Context, h *harness, engine *workflow.Engine) {
		result, err := intake.ApplyFact(ctx, h.store.Pool(), "api", "the-other-sign-off", intake.SignOff{Issue: "LEGION-208"}, engine)
		if err != nil || result.Refusal != nil {
			t.Fatalf("the other sign-off = %+v, %v; want it taken", result.Refusal, err)
		}
	})
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	refused(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil),
		http.StatusConflict, "SIGNOFF_OUTSIDE_ACCEPTING")
	if at := recordedPhase(t, h, "LEGION-208"); at != phase.Retro {
		t.Fatalf("LEGION-208 is recorded in %s, want retro, the other sign-off's", at)
	}
	if got := statuses.issues["LEGION-208"].Status; got != "retro" {
		t.Fatalf("Dispatch shows LEGION-208 %s while the daemon records it accepted, in retro: the losing sign-off wrote back the status it read before its write", got)
	}
}

// The control of the race above, on the same workflow: the sign-off's fact is refused for an issue
// beneath it that entered the workflow after the judgement, the record still holds the issue in
// accepting, and Dispatch gets back the needs_review it showed before the write.
func TestASignOffRefusedInPlaceWritesBackTheStatusItReplaced(t *testing.T) {
	h, statuses := newWorkflowAcceptanceHarness(t, func(ctx context.Context, h *harness, _ *workflow.Engine) {
		parent := "LEGION-208"
		if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
			return record.NewStore().PutIssue(ctx, tx, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: "LEGION-208", Project: testProject, Title: "LEGION-209", Parent: &parent, Phase: phase.Planning, Status: "in_progress"})
		}); err != nil {
			t.Fatalf("record LEGION-209 beneath LEGION-208: %v", err)
		}
	})
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	refused(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil),
		http.StatusConflict, "SIGNOFF_CHILD_IN_WORKFLOW")
	if at := recordedPhase(t, h, "LEGION-208"); at != phase.Accepting {
		t.Fatalf("LEGION-208 is recorded in %s, want accepting still", at)
	}
	if want := []statusWrite{{"LEGION-208", "retro"}, {"LEGION-208", "needs_review"}}; !reflect.DeepEqual(statuses.writes, want) {
		t.Fatalf("Dispatch writes = %+v, want retro and then needs_review written back: %+v", statuses.writes, want)
	}
}

// The admission pass can move the issue from accepting to awaiting_children between the judgement
// and the fact, which the workflow then refuses. The record holds no acceptance, and a phase that
// waits writes no status of its own (workflow.PhaseStatus), so only the write back takes Dispatch
// off the retro that every other project's daemon reads as an acceptance.
func TestASignOffRefusedAfterAnotherMoveWritesBackTheStatusItReplaced(t *testing.T) {
	var h *harness
	h, facts, statuses := newAcceptanceHarness(t, nil, func(ctx context.Context, issue string) {
		if _, err := h.store.Pool().Exec(ctx, `update issues set phase = $2, children_wait_start = 'testing', children_wait_task = 'Run the tests.',
			children_wait_reason = 'OTHR-5 is open', children_wait_since = now() where key = $1`, issue, string(phase.AwaitingChildren)); err != nil {
			t.Fatalf("move %s to awaiting_children: %v", issue, err)
		}
	})
	facts.refusal = &intake.Refusal{Status: http.StatusConflict, Code: "WAITING_ON_CHILDREN", Message: "LEGION-208 waits on its children"}
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	refused(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil),
		http.StatusConflict, "WAITING_ON_CHILDREN")
	if want := []statusWrite{{"LEGION-208", "retro"}, {"LEGION-208", "needs_review"}}; !reflect.DeepEqual(statuses.writes, want) {
		t.Fatalf("Dispatch writes = %+v, want retro and then needs_review written back: %+v", statuses.writes, want)
	}
}

// A person closes the issue in Dispatch after the sign-off's retro write and before the event of
// that close reaches the daemon, and the sign-off's fact is then refused. Dispatch no longer shows
// the sign-off's own write, so nothing is written back over the person's done.
func TestARefusedSignOffLeavesAPersonsLaterCloseInDispatch(t *testing.T) {
	var statuses *statusRecorder
	h, facts, shown := newAcceptanceHarness(t, nil, func(ctx context.Context, issue string) {
		if _, err := statuses.SetStatus(ctx, issue, "done"); err != nil {
			t.Fatalf("the person's close: %v", err)
		}
	})
	statuses = shown
	facts.refusal = &intake.Refusal{Status: http.StatusConflict, Code: "SIGNOFF_CHILD_IN_WORKFLOW", Message: "LEGION-209 is still in the workflow"}
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	refused(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil),
		http.StatusConflict, "SIGNOFF_CHILD_IN_WORKFLOW")
	if got := statuses.issues["LEGION-208"].Status; got != "done" {
		t.Fatalf("Dispatch shows LEGION-208 %s, want the person's done: the write back landed over it", got)
	}
}

// An event that leaves the status as the sign-off wrote it, a message on the issue, moves the
// sequence between the write and the write back. The write back is refused at the write's sequence
// (ISSUE_CHANGED), reads the issue again, finds the sign-off's retro still there, and writes back
// at the sequence it read.
func TestARefusedSignOffWritesBackOverAnEventThatKeptItsStatus(t *testing.T) {
	var statuses *statusRecorder
	h, facts, shown := newAcceptanceHarness(t, nil, func(_ context.Context, issue string) {
		moved := statuses.issues[issue]
		moved.LastSeq++
		statuses.issues[issue] = moved
	})
	statuses = shown
	facts.refusal = &intake.Refusal{Status: http.StatusConflict, Code: "SIGNOFF_CHILD_IN_WORKFLOW", Message: "LEGION-209 is still in the workflow"}
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	refused(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil),
		http.StatusConflict, "SIGNOFF_CHILD_IN_WORKFLOW")
	if got := statuses.issues["LEGION-208"]; got.Status != "needs_review" || got.LastSeq != 10 {
		t.Fatalf("Dispatch shows LEGION-208 %s at sequence %d, want needs_review written back at 10, past the message at 9", got.Status, got.LastSeq)
	}
}

// A sign-off whose caller goes away after its retro write still writes the status back once its
// fact does not take: the write back repairs what the daemon itself wrote in Dispatch, and the
// caller leaving does not make that write any less the daemon's.
func TestARefusedSignOffWritesBackAfterItsCallerWentAway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _, statuses := newAcceptanceHarness(t, nil, func(context.Context, string) { cancel() })
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	body, err := json.Marshal(map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"})
	if err != nil {
		t.Fatal(err)
	}
	answer := httptest.NewRecorder()
	h.handler.ServeHTTP(answer, httptest.NewRequest(http.MethodPost, "/legion/v1/signoff", bytes.NewReader(body)).WithContext(ctx))
	refused(t, answer, http.StatusInternalServerError, "FACT_APPLY_FAILED")
	if got := statuses.issues["LEGION-208"].Status; got != "needs_review" {
		t.Fatalf("Dispatch shows LEGION-208 %s once its caller went away, want needs_review written back", got)
	}
}

// Two acceptances of one issue run one after the other: the second reads Dispatch, is judged and
// writes only once the first has settled, so it never reads the first one's write as the status
// to write back.
func TestASecondAcceptanceOfAnIssueWaitsForTheFirstToSettle(t *testing.T) {
	var calls atomic.Int32
	overlapped := make(chan struct{}, 1)
	second := make(chan *httptest.ResponseRecorder, 1)
	var h *harness
	var grants [2]string
	h, _, statuses := newAcceptanceHarness(t, &acceptanceJudge{}, func(context.Context, string) {
		if calls.Add(1) > 1 {
			overlapped <- struct{}{}
			return
		}
		go func() {
			second <- h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": grants[1], "issue": "LEGION-208"}, nil)
		}()
		select {
		case <-overlapped:
			t.Error("the second sign-off wrote retro while the first stood between its own write and its fact")
		case <-time.After(5 * time.Second):
		}
	})
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	grants = [2]string{architect.grant(t), architect.grant(t)}
	if first := h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": grants[0], "issue": "LEGION-208"}, nil); first.Code != http.StatusOK {
		t.Fatalf("the first sign-off = %d %s, want 200", first.Code, first.Body)
	}
	select {
	case answer := <-second:
		if answer.Code != http.StatusOK {
			t.Fatalf("the second sign-off = %d %s, want 200", answer.Code, answer.Body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the second sign-off did not end within 30 seconds of the first")
	}
	if want := []string{"LEGION-208=retro", "LEGION-208=retro"}; !reflect.DeepEqual(statuses.acceptances, want) {
		t.Fatalf("acceptance writes = %q, want %q", statuses.acceptances, want)
	}
}

// movedWhileJudged is a judgement of sign_off and close_root before which move runs: what someone
// does in Dispatch after the route read the issue and before the route's own write.
type movedWhileJudged struct {
	AcceptanceJudge
	move func(ctx context.Context, issue string)
}

func (j movedWhileJudged) SignOffRefusal(ctx context.Context, tx pgx.Tx, issue string) (*intake.Refusal, error) {
	j.move(ctx, issue)
	return j.AcceptanceJudge.SignOffRefusal(ctx, tx, issue)
}

func (j movedWhileJudged) CloseRootRefusal(ctx context.Context, tx pgx.Tx, issue string) (*intake.Refusal, error) {
	j.move(ctx, issue)
	return j.AcceptanceJudge.CloseRootRefusal(ctx, tx, issue)
}

// A person moves the issue in Dispatch after the route read it and before the route's own write:
// closes it under a sign-off, parks it under a close_root. The route writes at the sequence of its
// read, so Dispatch refuses the write, nothing is written over the person's move and nothing is
// recorded, and the route answers 409 ISSUE_CHANGED naming the status the person set. The control
// of each act moves nothing, and the act is taken.
func TestAnAcceptanceWritesNothingOverAPersonsMoveSinceItsRead(t *testing.T) {
	for _, tc := range []struct {
		name, route, writes, moved string
		body                       map[string]any
	}{
		{name: "sign_off under a person's close", route: "/legion/v1/signoff", writes: "retro", moved: "done"},
		{name: "sign_off with no move", route: "/legion/v1/signoff", writes: "retro"},
		{name: "close_root under a person's park", route: "/legion/v1/roots/close", writes: "done", moved: "backlog",
			body: map[string]any{"reason": "The human decided no change is needed."}},
		{name: "close_root with no move", route: "/legion/v1/roots/close", writes: "done",
			body: map[string]any{"reason": "The human decided no change is needed."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var statuses *statusRecorder
			judge := movedWhileJudged{AcceptanceJudge: &acceptanceJudge{}, move: func(ctx context.Context, issue string) {
				if tc.moved == "" {
					return
				}
				if _, err := statuses.SetStatus(ctx, issue, tc.moved); err != nil {
					t.Errorf("the person's %s: %v", tc.moved, err)
				}
			}}
			h, facts, shown := newAcceptanceHarness(t, judge, nil)
			statuses = shown
			architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
			body := map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}
			for key, value := range tc.body {
				body[key] = value
			}
			answer := h.request(http.MethodPost, tc.route, body, nil)
			if tc.moved == "" {
				if answer.Code != http.StatusOK || statuses.issues["LEGION-208"].Status != tc.writes || len(facts.recorded()) != 1 {
					t.Fatalf("%s = %d %s, Dispatch %s, facts %#v; want 200, %s and the act's fact", tc.route, answer.Code, answer.Body,
						statuses.issues["LEGION-208"].Status, facts.recorded(), tc.writes)
				}
				return
			}
			if got := statuses.issues["LEGION-208"].Status; got != tc.moved {
				t.Fatalf("Dispatch shows LEGION-208 %s after %s answered %d %s, want the person's %s: the act's %s was written over it",
					got, tc.route, answer.Code, answer.Body, tc.moved, tc.writes)
			}
			if message := refused(t, answer, http.StatusConflict, "ISSUE_CHANGED"); !strings.Contains(message, tc.moved) {
				t.Fatalf("the refusal = %q, want it naming the person's %s", message, tc.moved)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts = %#v, want none applied", got)
			}
		})
	}
}

// An event that leaves the status as the route read it, a message, moves the sequence between the
// read and the write. Dispatch refuses the write at the read's sequence; the route reads the issue
// again, finds the status it read there, and writes at that read's sequence.
func TestAnAcceptanceWritesAtANewReadOverAnEventThatKeptTheStatus(t *testing.T) {
	var statuses *statusRecorder
	judge := movedWhileJudged{AcceptanceJudge: &acceptanceJudge{}, move: func(_ context.Context, issue string) {
		moved := statuses.issues[issue]
		moved.LastSeq++
		statuses.issues[issue] = moved
	}}
	h, facts, shown := newAcceptanceHarness(t, judge, nil)
	statuses = shown
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	answer := h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil)
	if answer.Code != http.StatusOK || len(facts.recorded()) != 1 {
		t.Fatalf("the sign-off = %d %s with facts %#v, want 200 and its fact", answer.Code, answer.Body, facts.recorded())
	}
	if got := statuses.issues["LEGION-208"]; got.Status != "retro" || got.LastSeq != 9 {
		t.Fatalf("Dispatch shows LEGION-208 %s at sequence %d, want retro written at 9, past the message at 8", got.Status, got.LastSeq)
	}
}
