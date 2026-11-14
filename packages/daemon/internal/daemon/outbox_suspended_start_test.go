package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The one row a start is written as (workflow.StartOrOwe), run with the cap full on a claim that
// is neither running nor failed and still holds its phase's task: nothing happens until the
// issue's slot comes, and then that row does the whole start.
//
// A member's start is delivered while its Sandbox launches, and the task is still unconfirmed
// when the tree stops: a person parks it, and the claim is suspended holding that generation and
// phase's task, or the linger closes the tree and retires the claim, which keeps what it held
// (supervise's retire). The person reopens the root — inside the linger, or after the close — and
// re-admission starts the member again (admit's startMidPhaseMembers). Bringing such a claim
// back sends the task it kept as soon as its agent is ready (supervise's sendPending), so the
// one thing that may do it is the row the cap holds: the member works its phase exactly when the
// cap says it may, and there is no second row behind this one to prompt the same phase again.
//
// Which of the two tasks the worker then gets is the machine's own rule, and either way it is one
// task and it arrives with the slot: a daemon suspension ends the claim's phase, so the task
// queued for that phase goes with it (supervise's settle) and the start row's own is delivered,
// while a retirement keeps the task the claim was working and the retry sends that one, which is
// why the row delivers none of its own to a retired claim.
func TestTheStartRowBringsBackAClaimThatHoldsItsTaskWhenTheSlotComes(t *testing.T) {
	const kept, owedTask = "Continue. Reason: the start the stop interrupted.", "Continue. Reason: the tree came back."
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		// works is the one task the worker is left with once the slot has come.
		works string
	}{
		{"suspended by the linger", supervise.StateSuspended, owedTask},
		{"retired by the tree's close", supervise.StateRetired, kept},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
			putOutboxIssue(t, pool, records, issue)
			openTree(t, pool, "LEGION", issue.Tree)
			sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
			token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
			if err != nil {
				t.Fatalf("claim token: %v", err)
			}
			// The claim as the daemon holds it when the tree stopped and the re-admission comes:
			// stopped where it stood, keeping its session and the unconfirmed task of the phase it
			// was working.
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
				Role: claim.RoleImplementer, State: tc.state, Generation: 1, Session: "ses-implementer", SessionFile: "/tmp/implementer.jsonl",
				SuspendedBy: supervise.SuspendedByDaemon,
				Pending:     &supervise.Delivery{ID: "outbox:17", Task: kept, Phase: issue.Phase, Generation: issue.Generation},
			}, "")
			if err != nil {
				t.Fatalf("create the implementer claim: %v", err)
			}

			clock := time.Now()
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: func() time.Time { return clock },
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
				remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
			}

			// The start as the re-admission writes it, with no slot for the issue: one row, which
			// carries the phase's task and which the outbox holds unclaimable until the slot comes
			// (record.Store's outboxInOrder).
			owed := record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer,
				Generation: issue.Generation, Phase: issue.Phase, Task: owedTask, ResumeTask: true, NeedsSlot: true}
			enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, owed, clock))

			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the outbox while the issue holds no slot: %v", err)
			}
			machine.Wait()
			if got := machine.Claim(); got.State != tc.state || got.Pending == nil || got.Pending.ID != "outbox:17" {
				t.Fatalf("the claim while the issue holds no slot = %+v, want it still %s holding the task it kept: bringing it back sends that task as soon as its agent is ready",
					got, tc.state)
			}
			if started := runtime.Calls(); len(started) != 0 {
				t.Fatalf("the outbox made %d runtime calls while the issue holds no slot, want none: the start waits for the cap", len(started))
			}
			if left := outboxRows(t, pool); left != 1 {
				t.Fatalf("outbox rows while the issue holds no slot = %d, want the one start waiting for the slot", left)
			}

			// The slot comes: that row brings the claim back, and exactly one task reaches the
			// worker — never the kept one and a second prompt of its own.
			holdSlot(t, pool, issue.Key)
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the start row: %v", err)
			}
			machine.Wait()
			got := machine.Claim()
			if got.State == tc.state {
				t.Fatalf("the claim after the start row = %+v, want it brought back: the row that carries the task is the one that does it", got)
			}
			if len(resumesOf(runtime, token)) != 1 {
				t.Fatalf("the start resumed the kept session %d times, want exactly once", len(resumesOf(runtime, token)))
			}
			if got.Pending == nil || got.Pending.Task != tc.works {
				t.Fatalf("the pending delivery = %+v, want the one task %q, delivered once: the start must not prompt the same phase twice", got.Pending, tc.works)
			}
			if left := outboxRows(t, pool); left != 0 {
				t.Fatalf("outbox rows after the start = %d, want none left", left)
			}
		})
	}
}
