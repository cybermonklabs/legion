package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// movingRecords is the record store of a start a move back overtakes: once the start's fences
// have read the issue's phases rows, its next read of an issue (the tree root's, the outbox's
// root) first commits move, as a fact that lands between the fences and the claim's re-role would.
type movingRecords struct {
	record.Store
	phasesRead, moved bool
	move              func()
}

func (s *movingRecords) Phases(ctx context.Context, tx pgx.Tx, issue string) ([]record.PhaseRow, error) {
	s.phasesRead = true
	return s.Store.Phases(ctx, tx, issue)
}

func (s *movingRecords) Issue(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	if s.phasesRead && !s.moved && s.move != nil {
		s.moved = true
		s.move()
	}
	return s.Store.Issue(ctx, tx, key)
}

// Key a's agent works the implementer's step and then the tester's in one claim, which holds the
// implementer's role and runs nothing, and the start of the tester's entry would give it the
// tester's role and resume it with the tester's task. A move back to implementing that commits
// after the start's fences read the record and before the re-role — the tester's fail, which
// raises the implementer's entry on the same agent — leaves the step the start was for: the
// claim keeps the implementer's role, nothing resumes it as the tester, and the row finishes
// without acting, since the move back's own start gives the claim its role. The re-role judges
// the step again under the machine's lock (supervise.Machine.Rerole), on the start's own fence, the
// step's role included (record.Issue.HoldsQueued): every step of a role the deployment defines
// stands at the one phase step, so a move from the checker's step to the linter's raises no entry
// on the checker's row and leaves the phase as it was, and only the step's role says that the
// checker's start is no longer the work. The control is the same start with no move: the claim
// takes the tester's role and is resumed on its session.
func TestAStartsReroleForAStepTheIssueLeftIsNotMade(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// step is whether the start is of the checker's step at the phase step, which a move to
		// the linter's step leaves, rather than of the tester's entry, which a move back leaves.
		step, move bool
	}{
		{name: "control: nothing moves the issue while the start runs"},
		{name: "a move back commits between the start's fences and its re-role", move: true},
		{name: "a move to the next step of the phase step commits between the start's fences and its re-role", step: true, move: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
			role, at := claim.RoleTester, phase.Testing
			if tc.step {
				role, at = claim.Role("checker"), phase.Step
				issue.Phase, issue.Status = phase.Step, "in_progress"
			}
			putOutboxIssue(t, pool, records, issue)
			if tc.step {
				seedKeyedStepPlan(t, pool, records, issue.Key)
			}
			keyed := mustClaimToken(t, issue.Key, claim.RoleImplementer)
			putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: keyed, StepTask: "t", StepPlace: 1, Entry: 1})
			putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: role, Claim: keyed, StepTask: "t", StepPlace: 2, Entry: 2})
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: keyed, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
				Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
			if err != nil {
				t.Fatalf("create the claim of key a: %v", err)
			}
			if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: keyed, Exempt: true}); err != nil {
				t.Fatalf("spawn key a's agent: %v", err)
			}
			launch := machine.Claim().Generation
			if err := machine.Handle(ctx, supervise.RequestRegister{Claim: keyed, Generation: launch, Session: "ses-a", SessionFile: "/tmp/a.jsonl"}); err != nil {
				t.Fatalf("register key a's agent: %v", err)
			}
			if err := machine.Handle(ctx, supervise.RequestReady{Claim: keyed, Generation: launch, Session: "ses-a"}); err != nil {
				t.Fatalf("ready key a's agent: %v", err)
			}
			if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: keyed, By: supervise.SuspendedByDaemon, Reason: "its implementer's step ended"}); err != nil {
				t.Fatalf("suspend key a's agent: %v", err)
			}
			if got := machine.Claim().State; got != supervise.StateSuspended {
				t.Fatalf("key a's claim is %s, want suspended: the case's premise", got)
			}
			moving := &movingRecords{Store: records}
			switch {
			case tc.move && tc.step:
				moving.move = func() {
					if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
						return records.MoveTask(ctx, tx, issue.Key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 3}, record.TaskRunning)
					}); err != nil {
						t.Fatalf("move the task to the linter's step: %v", err)
					}
				}
			case tc.move:
				moving.move = func() {
					back := issue
					back.Phase, back.Status = phase.Implementing, "in_progress"
					putOutboxIssue(t, pool, records, back)
					putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: keyed, StepTask: "t", StepPlace: 1, Entry: 3})
				}
			}
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: moving, supervisor: sup, notices: &outboxPublisher{},
				trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			resumes := len(rt.CallsOf("Resume"))

			start := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Phase: at,
				Entry: 2, Task: "Test it.", Generation: 1}, time.Now())
			start.ID = 71
			if err := runner.execute(ctx, start); err != nil {
				t.Fatalf("the %s's start: %v", role, err)
			}
			if tc.move && !moving.moved {
				t.Fatal("the move never landed inside the start: the case saw no window")
			}
			calls := rt.CallsOf("Resume")[resumes:]
			if !tc.move {
				if got := machine.Claim().Role; got != claim.RoleTester || len(calls) != 1 || calls[0].Spec.Role != claim.RoleTester {
					t.Fatalf("with no move the claim holds %s and was relaunched as %+v, want the tester, resumed once", got, calls)
				}
				return
			}
			if got := machine.Claim().Role; got != claim.RoleImplementer {
				t.Errorf("after a move overtook the %s's start the claim holds the role %s, want the implementer it held", role, got)
			}
			if len(calls) != 0 {
				t.Errorf("after a move overtook the %s's start the claim was relaunched as %+v, want no relaunch for a step the issue left", role, calls)
			}
			if got := machine.Claim().State; got != supervise.StateSuspended {
				t.Errorf("after a move overtook the %s's start the claim is %s, want it suspended, running nothing", role, got)
			}
		})
	}
}

// seedKeyedStepPlan records key's plan of generation 1, `implementer:a checker:a linter`, all at the
// phase step but the implementer's, its one task running at the checker's step 2: key a's agent,
// named implementer, works the implementer's step and then the checker's.
func seedKeyedStepPlan(t *testing.T, pool *store.Pool, records record.Store, key string) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: mustClaimToken(t, key, claim.RoleArchitect), GivenAt: time.Now().UTC()}
		task := record.PlanTask{Issue: key, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := record.NameAgents(nil, []record.PlanStep{
			{Issue: key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation", AgentKey: "a"},
			{Issue: key, Generation: 1, Task: "a", Position: 2, Role: "checker", Type: "testing", AgentKey: "a"},
			{Issue: key, Generation: 1, Task: "a", Position: 3, Role: "linter", Type: "testing"},
		}, nil)
		if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
			return err
		}
		return records.MoveTask(ctx, tx, key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
	}); err != nil {
		t.Fatalf("seed the plan of %s at the checker's step: %v", key, err)
	}
}

// The start of the tester's entry finds key a's claim in a turn as the implementer, so the
// suspension it asks is judged on the step then and held for the turn's end
// (prepareClaimForStep). A move back to implementing that commits before the turn ends raises the
// implementer's entry on the same claim, and the held suspension lands at the turn's end on the
// judgment it passed when it was asked. What that costs is one suspension and one resume of the
// claim on its own session, and nothing else: the tester's start finishes without acting on its
// next attempt (the issue left testing), the claim keeps the implementer's role, and the move
// back's own start resumes it as the implementer on the session it recorded and hands it the
// implementer's task.
func TestAHeldSuspensionAMoveBackOvertakesCostsOneSuspendAndResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
	putOutboxIssue(t, pool, records, issue)
	keyed := mustClaimToken(t, issue.Key, claim.RoleImplementer)
	putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: keyed, StepTask: "t", StepPlace: 1, Entry: 1})
	putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: keyed, StepTask: "t", StepPlace: 2, Entry: 2})
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: keyed, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the claim of key a: %v", err)
	}
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
		trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: keyed, Exempt: true}); err != nil {
		t.Fatalf("spawn key a's agent: %v", err)
	}
	launch := machine.Claim().Generation
	if err := machine.Handle(ctx, supervise.RequestRegister{Claim: keyed, Generation: launch, Session: "ses-a", SessionFile: "/tmp/a.jsonl"}); err != nil {
		t.Fatalf("register key a's agent: %v", err)
	}
	if err := machine.Handle(ctx, supervise.RequestReady{Claim: keyed, Generation: launch, Session: "ses-a"}); err != nil {
		t.Fatalf("ready key a's agent: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: keyed}); err != nil {
		t.Fatalf("key a's agent's turn: %v", err)
	}
	suspends, resumes := len(rt.CallsOf("Suspend")), len(rt.CallsOf("Resume"))

	tester := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester, Phase: phase.Testing,
		Entry: 2, Task: "Test it.", Generation: 1}, time.Now())
	tester.ID = 71
	if err := runner.execute(ctx, tester); !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the tester's start during the implementer's turn = %v, want a wait", err)
	}
	if view := machine.View(); !view.SuspensionHeld || view.State != supervise.StateWorking {
		t.Fatalf("key a's claim is %s with a suspension held = %t, want working with the suspension held: the case's premise", view.State, view.SuspensionHeld)
	}
	// The tester's fail: the issue moves back to implementing, and the implementer's entry rises
	// on the same claim.
	back := issue
	back.Phase, back.Status = phase.Implementing, "in_progress"
	putOutboxIssue(t, pool, records, back)
	putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: keyed, StepTask: "t", StepPlace: 1, Entry: 3})

	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: keyed}); err != nil {
		t.Fatalf("end key a's agent's turn: %v", err)
	}
	if err := runner.execute(ctx, tester); err != nil {
		t.Fatalf("the tester's start after the move back: %v", err)
	}
	implementer := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Phase: phase.Implementing,
		Entry: 3, Task: "Implement it again.", Generation: 1}, time.Now())
	implementer.ID = 72
	if err := runner.execute(ctx, implementer); err != nil {
		t.Fatalf("the move back's start of the implementer: %v", err)
	}

	if got := len(rt.CallsOf("Suspend")) - suspends; got != 1 {
		t.Fatalf("the claim's process was stopped %d times, want the one held suspension", got)
	}
	calls := rt.CallsOf("Resume")[resumes:]
	if len(calls) != 1 || calls[0].Spec.Role != claim.RoleImplementer || calls[0].Spec.ResumeSessionFile != "/tmp/a.jsonl" {
		t.Fatalf("the claim was relaunched as %+v, want once, as the implementer, on its own session /tmp/a.jsonl", calls)
	}
	if got := machine.Claim(); got.Role != claim.RoleImplementer || got.Pending == nil || got.Pending.ID != "outbox:72" {
		t.Fatalf("the claim holds the role %s and the task %+v, want the implementer holding the move back's task outbox:72", got.Role, got.Pending)
	}
}
