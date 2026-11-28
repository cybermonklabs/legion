package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The entries of the implementer's phases row these tests stand on: the entry its first step ran
// at, and the entry the fix of the task raised the row to.
const (
	firstEntry int64 = 1
	fixEntry   int64 = 2
)

// fixPushed is the commit the fix pushed over headSHA, the head its check failed at.
const fixPushed = "f1c5000000000000000000000000000000000001"

// firstImplementRecord is the handoff record the implementer's completion of the task's first step
// named, which its row keeps as the record it last named.
const firstImplementRecord = "handoff-LEGION-208-implement-v1-first"

// seedReviewStep stands LEGION-208 at the reviewer's step of its one task, which runs the
// implementer, the tester and the reviewer and takes a fix. Its pull request is at headSHA, and
// the implementer's completion of the first step stands on its row, on firstEntry. With fixRuns,
// fix 1 of the task runs: the check unit failed at headSHA, and the fix raised the implementer's
// row to fixEntry, the entry its task carries, as the start of a fix does; that start ends the
// handoff the row held and keeps the record the implementer last named.
func seedReviewStep(t *testing.T, h *harness, fixRuns bool) {
	t.Helper()
	ctx, records, key := context.Background(), record.NewStore(), "LEGION-208"
	seedIssueAt(t, h, key, phase.Reviewing)
	seedPullRequest(t, h, key, headSHA)
	v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-" + key, Source: record.PlanSourceOwner,
		GivenBy: "owner", GivenAt: time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)}
	tasks := []record.PlanTask{{Issue: key, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Task a.", State: record.TaskWaiting, FixesCI: true}}
	steps := []record.PlanStep{
		{Issue: key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
		{Issue: key, Generation: 1, Task: "a", Position: 2, Role: "tester", Type: "testing"},
		{Issue: key, Generation: 1, Task: "a", Position: 3, Role: "reviewer", Type: "review"}}
	row := record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: "implementer-claim", Entry: firstEntry,
		HandoffCommit: headSHA, HandoffRecord: firstImplementRecord, HandoffVersion: 1, LastHandoff: firstImplementRecord}
	if fixRuns {
		row.Entry, row.HandoffCommit, row.HandoffRecord, row.HandoffVersion = fixEntry, "", "", 0
	}
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		if err := records.MoveTask(ctx, tx, key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 3}, record.TaskRunning); err != nil {
			return err
		}
		if err := records.PutPhase(ctx, tx, row); err != nil {
			return err
		}
		if !fixRuns {
			return nil
		}
		fix := record.TaskFix{Issue: key, Generation: 1, Task: "a", Fix: 1, State: record.FixReading, Check: "unit", Head: headSHA, Place: 1}
		if err := records.PutFix(ctx, tx, fix); err != nil {
			return err
		}
		started := time.Date(2026, 11, 23, 7, 0, 0, 0, time.UTC)
		fix.State, fix.Entry, fix.StartedAt = record.FixRunning, fixEntry, &started
		return records.UpdateFix(ctx, tx, fix)
	}); err != nil {
		t.Fatalf("seed %s at its review step: %v", key, err)
	}
}

// implementingTask is the outbox id of the implementer's task of entry.
func implementingTask(entry int64) string {
	return fmt.Sprintf("outbox:%d", 90+entry)
}

// takeImplementingTask hands the implementer's claim its task of entry, of phase implementing as
// the start of the first step and the start of a fix each queue it, in a turn of its own.
func takeImplementingTask(t *testing.T, h *harness, token claim.Token, entry int64) {
	t.Helper()
	deliverPhaseTask(t, h, token, implementingTask(entry), "implement the task", phase.Implementing, entry, 1)
	machine, _ := h.supervisor.Machine(token)
	if err := machine.Handle(context.Background(), supervise.StreamTurnStart{Claim: token, DeliveryID: implementingTask(entry)}); err != nil {
		t.Fatalf("start the turn of %s: %v", token, err)
	}
}

// moveBack posts the implementer's move back of LEGION-208 to implementing.
func moveBack(t *testing.T, h *harness, implementer liveClaim) *httptest.ResponseRecorder {
	t.Helper()
	return h.request(http.MethodPost, "/legion/v1/phase/backward", PhaseBackwardRequest{
		GrantID: implementer.grant(t), To: phase.Implementing, Reason: "the check fails for a reason this step cannot reach",
	}, nil)
}

// lastBackwardMove is the newest backward move facts recorded.
func lastBackwardMove(t *testing.T, facts *factRecorder) intake.BackwardMove {
	t.Helper()
	recorded := facts.recorded()
	if len(recorded) == 0 {
		t.Fatal("no fact reached the intake")
	}
	fact, ok := recorded[len(recorded)-1].(intake.BackwardMove)
	if !ok {
		t.Fatalf("the fact recorded is %T, want a backward move", recorded[len(recorded)-1])
	}
	return fact
}

// A fix runs the task's first step again while the issue stands at the reviewer's step, so the
// implementer completes it in reviewing. Its claim serves the entry the fix raised, and the route
// takes the completion as the fix's: the fact names fix 1, and the key places it at phase
// implementing, as the fix's own completion, never one of reviewing. The implementer works its
// step through the fix, so the completion is held to the implement record that step ends with, as
// any completion of the phase its role works is: one naming none is refused, and nothing reaches
// the workflow. A claim that serves no run and holds the fix's task unread (a lost prompt, an
// adoption that failed) is taken from that task as a claim holding its phase's task is: the
// fix's task names phase implementing while the issue is in reviewing, and is the issue's work.
func TestTheFixCompletesWhileTheIssueIsInReviewing(t *testing.T) {
	fixWorks := func(t *testing.T) (*harness, *factRecorder, liveClaim) {
		h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
		seedReviewStep(t, h, true)
		implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
		takeImplementingTask(t, h, implementer.token, fixEntry)
		return h, facts, implementer
	}
	t.Run("the fact names the fix", func(t *testing.T) {
		h, facts, implementer := fixWorks(t)
		written := implementer.writeHandoff("implement", 2)
		completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "the check passes", Commit: fixPushed,
			HandoffID: written, HandoffVersion: 2}))
		fact := lastCompletion(t, facts)
		if fact.Fix != 1 || fact.Role != claim.RoleImplementer || fact.Generation != 1 || fact.Commit != fixPushed || fact.HandoffRecord != written {
			t.Fatalf("the completion's fact = %+v, want the implementer's completion of fix 1 at %s naming %s", fact, fixPushed, written)
		}
		want := "handoff:LEGION-208:1:1:1:implementer:implementing:a:0:fix1:" + written + ":"
		if got := completionKeys(t, h, "LEGION-208"); len(got) != 1 || got[0] != want {
			t.Fatalf("the completion keys = %q, want [%q]", got, want)
		}
	})
	t.Run("the fix's completion names its record", func(t *testing.T) {
		h, facts, implementer := fixWorks(t)
		refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "the check passes", Commit: fixPushed}),
			http.StatusBadRequest, "HANDOFF_RECORD_REQUIRED")
		if got := facts.recorded(); len(got) != 0 {
			t.Fatalf("facts after the fix's completion naming no record = %#v, want none applied", got)
		}
	})
	t.Run("the fix's task the claim holds unread", func(t *testing.T) {
		h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
		seedReviewStep(t, h, true)
		implementer := untaskedClaim(t, h, "LEGION-208", claim.RoleImplementer)
		holdingTask(t, h, implementer, supervise.RequestDeliver{
			Task: "fix the check", ID: implementingTask(fixEntry), Generation: 1, Phase: phase.Implementing, Entry: fixEntry})
		completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "the check passes", Commit: fixPushed,
			HandoffID: implementer.writeHandoff("implement", 2), HandoffVersion: 2}))
		if fact := lastCompletion(t, facts); fact.Fix != 1 || fact.Generation != 1 {
			t.Fatalf("the completion's fact = %+v, want the completion of fix 1 at generation 1", fact)
		}
	})
}

// The implementer's move back while its claim serves the fix's entry is the fix's report of a
// failure, so the fact names the fix; the workflow judges where the move may go.
func TestTheFixMovesBackWhileTheIssueIsInReviewing(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedReviewStep(t, h, true)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	takeImplementingTask(t, h, implementer.token, fixEntry)

	if recorder := moveBack(t, h, implementer); recorder.Code != http.StatusOK {
		t.Fatalf("the fix's move back = %d: %s", recorder.Code, recorder.Body)
	}
	if fact := lastBackwardMove(t, facts); fact.Fix != 1 || fact.Requester != claim.RoleImplementer || fact.To != phase.Implementing {
		t.Fatalf("the move's fact = %+v, want the implementer's move to implementing naming fix 1", fact)
	}
}

// While a fix runs, the implementer works its task's first step again, and the fix's task is
// owed to its claim. A completion of the entry before the fix's, one the claim still serves while
// the fix's task waits unread on it, is behind the step the implementer works, and the entry
// fence refuses it so: HANDOFF_STALE_ENTRY at phase implementing, naming the fix's task that
// waits. It is refused even at the head the pull request is at: the fix's entry asks for the
// step's work to be done again at that very head, so no verdict on it answers for the fix.
// Nothing reaches the workflow.
func TestAnImplementerCompletionWithAnOldEntryIsRefused(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedReviewStep(t, h, true)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	takeImplementingTask(t, h, implementer.token, firstEntry)
	deliverPhaseTask(t, h, implementer.token, implementingTask(fixEntry), "fix the check", phase.Implementing, fixEntry, 1)

	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: headSHA,
		HandoffID: implementer.writeHandoff("implement", 2), HandoffVersion: 2}), http.StatusConflict, "HANDOFF_STALE_ENTRY")
	for _, says := range []string{
		"LEGION-208 handed the implementer phase implementing again: this completion is of entry 1, which is 1 behind the entry 2 the phase is on",
		"the entry the phase is on is fix 1 of the task, for check unit failed at head " + headSHA,
		"your turn began before the daemon's task for entry 2 (" + implementingTask(fixEntry) + ") reached this session",
		staleEntryInstruction,
	} {
		if !strings.Contains(message, says) {
			t.Fatalf("the refusal = %q, want it to say %q", message, says)
		}
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts after the refusal = %#v, want none applied", got)
	}
}

// With no fix running, an implementer's completion and its move back while the issue stands at
// the reviewer's step are answered as before: the workflow refuses each, in these words, and
// neither fact names a fix.
func TestNoFixLeavesTheRouteAsToday(t *testing.T) {
	facts := &factRecorder{}
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard, facts)
	seedReviewStep(t, h, false)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	takeImplementingTask(t, h, implementer.token, firstEntry)

	refusedSaying(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented again", Commit: fixPushed,
		HandoffID: implementer.writeHandoff("implement", 2), HandoffVersion: 2}), http.StatusConflict, "HANDOFF_NOT_CURRENT_PHASE",
		"the implementer does not run phase reviewing of LEGION-208; this completion changed nothing")
	if fact := lastCompletion(t, facts); fact.Fix != 0 {
		t.Fatalf("the completion's fact = %+v, want it naming no fix", fact)
	}
	refusedSaying(t, moveBack(t, h, implementer), http.StatusConflict, "BACKWARD_REFUSED",
		"a backward move is the role running the issue's phase: reviewing of LEGION-208 is the reviewer's, so the implementer's move to implementing changed nothing")
	if fact := lastBackwardMove(t, facts); fact.Fix != 0 {
		t.Fatalf("the move's fact = %+v, want it naming no fix", fact)
	}
}
