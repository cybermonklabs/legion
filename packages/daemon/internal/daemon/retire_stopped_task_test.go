package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The admission pass's move of a parent into awaiting_children writes a `retire_task` row for the
// worker it stops (workflow's StopForChildren), and the outbox runs it through the claim's
// machine: the task the claim held for the stopped phase, not yet sent, is gone. After the return
// puts the issue back in that same phase at the same run — where the phase fence the daemon wires
// holds a task of that phase again — the worker that comes up is sent nothing of the stopped
// phase, and only the return's own start reaches it.
func TestTheMovesRetirementLeavesNoTaskOfTheStoppedPhase(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Parent", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds // exactly what daemon.go wires
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{
		Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, State: supervise.StateQueued,
	}, "")
	if err != nil {
		t.Fatalf("create the implementer claim: %v", err)
	}
	runner := &outbox{
		pool: pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	const stopped = "Continue Parent. Issue: LEGION-208. Phase: implementing. Reason: a review asked for changes."
	if err := runner.execute(ctx, mustOutboxRow(t, issue.Key, record.SuperviseRequest{
		Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing, Entry: 1, Task: stopped,
	}, time.Now())); err != nil {
		t.Fatalf("start the implementer on its phase: %v", err)
	}
	if p := machine.Claim().Pending; p == nil || p.Task != stopped {
		t.Fatalf("pending after the start = %+v, want the phase's task held for the worker coming up", p)
	}

	// The move: the parent waits on its children, and its row retires the stopped phase's task.
	waiting := issue
	waiting.Phase = phase.AwaitingChildren
	waiting.ChildrenWait = &record.ChildrenWait{Start: phase.Implementing, Task: stopped, Reason: "a child is open", Since: time.Now()}
	putOutboxIssue(t, pool, records, waiting)
	if err := runner.execute(ctx, mustOutboxRow(t, issue.Key, record.SuperviseRequest{
		Op: "retire_task", Tree: issue.Tree, Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing, Entry: 1,
	}, time.Now())); err != nil {
		t.Fatalf("run the retirement: %v", err)
	}
	if p := machine.Claim().Pending; p != nil {
		t.Fatalf("pending after the retirement = %+v, want none", p)
	}

	// The return: the issue is back in implementing at the same run, so the phase fence would hold
	// a task of that phase. The worker comes up and is sent nothing of the stopped phase.
	returned := issue
	putOutboxIssue(t, pool, records, returned)
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-impl", SessionFile: "/tmp/impl.jsonl"},
		supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-impl"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	// The worker that came up finds nothing to take: the claim holds no task of the stopped phase.
	machine.Wait()
	if pending := machine.Claim().Pending; pending != nil {
		t.Fatalf("once the worker came up the claim holds %+v, want nothing: the stopped phase's task was retired", pending)
	}

	// The return's own start, on an entry of its own, is what the worker takes.
	const back = "Continue Parent. Issue: LEGION-208. Phase: implementing. LEGION-208 waited on its children: merge forward."
	if err := runner.execute(ctx, mustOutboxRow(t, issue.Key, record.SuperviseRequest{
		Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing, Entry: 2, Task: back,
	}, time.Now())); err != nil {
		t.Fatalf("run the return's start: %v", err)
	}
	held := machine.Claim()
	if held.Pending == nil {
		t.Fatal("the return's start queued no task")
	}
	answer, err := machine.Take(ctx, supervise.RequestTake{Claim: token, Generation: held.Generation, Session: held.Session, Number: held.Pending.Number})
	if err != nil || answer.Outcome != supervise.TakeOutcomeAccepted {
		t.Fatalf("the worker's take of the return's task = %+v, %v; want it accepted", answer, err)
	}
	if !strings.Contains(answer.Text, "merge forward") || strings.Contains(answer.Text, "a review asked for changes") {
		t.Fatalf("the worker took %q, want the return's task alone", answer.Text)
	}
}
