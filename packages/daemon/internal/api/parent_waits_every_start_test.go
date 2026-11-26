package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// inEngineFact runs fn in one transaction scoped as a fact's is (intake.ApplyFact): commit hooks,
// the engine's open fact and its settle before the commit. It is how the admission pass calls the
// engine's moves of a parent into awaiting_children and back (workflow's StopForChildren and
// ReturnFromChildren), which the API's harness has no admission pass to call.
func inEngineFact(t *testing.T, h *harness, engine *workflow.Engine, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	if err := store.Transact(engine.OpenFact(context.Background()), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return engine.SettleFact(ctx, tx)
	}); err != nil {
		t.Fatalf("the engine's move: %v", err)
	}
}

// The completion route's side of a parent's wait on its children (dispatch://INFRA-3320 R10 and
// R12): staleTesterCompletionAfterTheReturn, which the check
// dispatch://INFRA-3320/artifact/verification-md#b-k12 runs here, beside internal/admit's and
// internal/daemon's test of the same name, and #b-k13 runs in TestParentWaitsHoldsNoSlotAndComesBack.
func TestParentWaitsHoldsEveryPhaseStart(t *testing.T) {
	t.Run(staleCompletionAfterTheReturn, staleTesterCompletionAfterTheReturn)
}

// staleCompletionAfterTheReturn is the name of the case staleTesterCompletionAfterTheReturn runs.
const staleCompletionAfterTheReturn = "the stopped tester's completion from before the wait is refused after the return at the same head, and the return's task's completion is taken"

// staleTesterCompletionAfterTheReturn is a parent whose tester is in the middle of testing when an
// admission pass finds a child open under it and stops the tester into awaiting_children, holding
// testing; the tester's claim still serves the stopped entry, since the retirement of its task
// leaves that (supervise's RetireStoppedTask). Once no child is open the return takes the held
// testing back as testing, on an entry of its own raised as a move back out of awaiting_children
// (record.PhaseRow.EntryMovedBack). The engine reads a tester's completion as one of the phase the
// issue stands in, and the completion carries no entry, so what refuses the stopped round's
// completion is the route's entry fence (intake.EntryFence, intake.BehindEntryClause): it is behind an entry a
// move back raised, and is refused HANDOFF_STALE_ENTRY at the very head the record holds, before
// any fact is applied, moving nothing and recording nothing. The completion of the return's own
// task is taken.
func staleTesterCompletionAfterTheReturn(t *testing.T) {
	const parent, child = "LEGION-208", "LEGION-301"
	h, engine, facts := newEntryHarness(t)
	ctx := context.Background()
	h.recordIssue(record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: parent,
		Project: testProject, Title: parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: parent, Repo: "acme/widgets",
			Number: 42, Branch: "legion/" + parent, HeadSHA: headSHA, CheckedHead: headSHA, Failing: []string{}})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}

	// The implementer's round ends at the head, and the tester takes the testing task.
	implementer := newLiveClaim(t, h, parent, claim.RoleImplementer)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: headSHA,
		HandoffID: implementer.writeHandoff("implement", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Testing)
	tester := newLiveClaim(t, h, parent, claim.RoleTester)
	stopped := takeStart(t, h, tester.token, parent, claim.RoleTester)

	// A child is released under the parent: the admission pass stops the tester in the middle of
	// testing and the parent waits, holding testing.
	inEngineFact(t, h, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := record.NewStore().Issue(ctx, tx, parent)
		if err != nil {
			return err
		}
		moved, err := engine.StopForChildren(ctx, tx, *issue, []record.OpenChild{{Key: child, Status: "todo"}}, nil)
		if err == nil && !moved {
			t.Errorf("the pass left %s where it stood, want it moved to awaiting_children", parent)
		}
		return err
	})
	assertIssuePhase(t, h, phase.AwaitingChildren)

	// The child is finished: the next pass takes the parent back.
	inEngineFact(t, h, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := record.NewStore().Issue(ctx, tx, parent)
		if err != nil {
			return err
		}
		return engine.ReturnFromChildren(ctx, tx, *issue)
	})

	// Before the return's task reaches the tester, the stopped round's turn reports its completion
	// at the head the record holds. Refused, with nothing moved and nothing recorded.
	before := recordedCompletions(t, h, claim.RoleTester)
	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		Commit: headSHA, HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusConflict, intake.StaleEntryCode)
	for _, want := range []string{
		"the move that handed the phase out again moved the issue back",
		"nothing was recorded",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal = %q, want it to say %q", message, want)
		}
	}
	if got := recordedCompletions(t, h, claim.RoleTester); got != before {
		t.Fatalf("%d tester completions are recorded after the refusal, want the %d there were", got, before)
	}
	for _, fact := range facts.recorded() {
		if completed, ok := fact.(intake.HandoffComplete); ok && completed.Role == claim.RoleTester {
			t.Fatalf("the workflow saw the tester's completion %#v, want the route to have refused it", completed)
		}
	}
	assertIssuePhase(t, h, phase.Testing)
	if entry := rowEntry(t, h, parent, claim.RoleTester); entry != stopped+1 {
		t.Fatalf("the tester's row is on entry %d, want the return's %d above the stopped %d", entry, stopped+1, stopped)
	}

	// The return's task reaches the tester, and the completion of its own round is taken.
	if returned := takeStart(t, h, tester.token, parent, claim.RoleTester); returned != stopped+1 {
		t.Fatalf("the tester took entry %d, want the return's %d", returned, stopped+1)
	}
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass on the head merged with its base",
		Verdict: "pass", Commit: headSHA, HandoffID: tester.writeHandoff("test", 2), HandoffVersion: 2}))
	assertIssuePhase(t, h, phase.Reviewing)
}
