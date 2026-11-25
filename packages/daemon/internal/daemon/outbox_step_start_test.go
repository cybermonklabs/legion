package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// stepStartKey is the issue of the tests of a start or a task of one step of the phase step.
const stepStartKey = "LEGION-208"

// stepStartLinter is the role of step 3 of stepStartKey's plan `implementer checker linter`.
const stepStartLinter claim.Role = "linter"

// seedStepStartPlan records stepStartKey at step, generation 2, with the plan `implementer checker
// linter` of the type implementation, testing and testing, its one task running at step at, and
// the linter's phases row on entry 1, the entry its start carries: no later start raised it.
func seedStepStartPlan(t *testing.T, pool *store.Pool, records record.Store, at int) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: stepStartKey, Generation: 2, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: mustClaimToken(t, stepStartKey, claim.RoleArchitect), GivenAt: time.Now().UTC()}
		task := record.PlanTask{Issue: stepStartKey, Generation: 2, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := record.NameAgents(nil, []record.PlanStep{
			{Issue: stepStartKey, Generation: 2, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: stepStartKey, Generation: 2, Task: "a", Position: 2, Role: "checker", Type: "testing"},
			{Issue: stepStartKey, Generation: 2, Task: "a", Position: 3, Role: string(stepStartLinter), Type: "testing"},
		}, nil)
		if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
			return err
		}
		if err := records.MoveTask(ctx, tx, stepStartKey, 2, "a", record.TaskPlace{Kind: record.PlaceStep, Position: at}, record.TaskRunning); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: stepStartKey, Role: stepStartLinter, Entry: 1,
			EntryTask: "Lint it. Issue: LEGION-208. Phase: step.", EntryRaisedAt: time.Now()})
	}); err != nil {
		t.Fatalf("seed the plan of %s at step %d: %v", stepStartKey, at, err)
	}
}

// Every role the deployment defines runs as the phase step, so a start that names the phase alone
// does not say which step it is of. The linter's start of step 3 of `implementer checker linter`
// was queued, under the transition's lock, while the task stood at step 3. It waited on its row's
// backoff (an operator's suspension of the claim, or a pod built before the role); meanwhile a
// move back sent the work to the checker's step 2, and the task now stands there, still at the phase
// step, with the linter's row on the same entry, so the generation, the phase and the entry all
// pass. The outbox's fence holds the start to the role of the step the task stands at
// (record.Issue.HoldsQueued) and finishes it without acting: no linter claim is created and no task
// reaches it. The control is the same row with the task at the linter's own step 3, which starts
// the linter.
func TestAStartOfAnotherStepsRoleFinishesWithoutActing(t *testing.T) {
	t.Parallel()
	const lintTask = "Lint it. Issue: LEGION-208. Phase: step."
	for _, tc := range []struct {
		name   string
		at     int
		role   claim.Role
		starts bool
	}{
		{"the linter's start of step 3 with the task back at the checker's step 2", 2, "checker", false},
		{"the control: the linter's start with the task at its step 3", 3, stepStartLinter, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: stepStartKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: stepStartKey,
				Title: "Workflow", Phase: phase.Step, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
			putOutboxIssue(t, pool, records, issue)
			openTree(t, pool, "LEGION", issue.Tree)
			holdSlot(t, pool, issue.Key)
			seedStepStartPlan(t, pool, records, tc.at)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			read, err := runner.issue(ctx, stepStartKey)
			if err != nil {
				t.Fatalf("read %s as the outbox does: %v", stepStartKey, err)
			}
			if read.Phase != phase.Step || read.StepRole != tc.role {
				t.Fatalf("%s reads at %s with the step role %q, want step and %q", stepStartKey, read.Phase, read.StepRole, tc.role)
			}
			start := mustOutboxRow(t, stepStartKey, record.SuperviseRequest{Op: "start", Tree: stepStartKey, Role: stepStartLinter, Generation: 2, Phase: phase.Step,
				Task: lintTask, Entry: 1}, time.Now())
			start.ID = 93
			execErr := runner.execute(ctx, start)
			machine, found := sup.Machine(mustClaimToken(t, stepStartKey, stepStartLinter))
			var pending string
			if found {
				machine.Wait()
				if p := machine.Claim().Pending; p != nil {
					pending = p.Task
				}
			}
			if found != tc.starts || execErr != nil {
				t.Fatalf("the linter's start of step 3 with the task at step %d (%s running): execute = %v, linter claim created %t (pending task %q), want no error and created %t",
					tc.at, read.StepRole, execErr, found, pending, tc.starts)
			}
		})
	}
}

// A start acts on the claim it was queued for, which every phase worker's start names
// (record.SuperviseRequest.Claim, workflow's started), and never on the claim the role's row
// names by the time it runs. In `implementer:a reviewer:b implementer:b`, key b's start of step 3
// at entry 3 waited on its row's backoff:
//   - while the task went back to step 1, whose row, key a's, was raised above every row of its
//     role, to 4: the outbox finishes it without acting, and no claim is made or handed a task. A
//     step's start raises its entry above every row of its role, so no row of the role stands on
//     the entry key b's start carries;
//   - while the implementer's row stood on no step at entry 3, naming key b's claim: it starts key
//     b, the claim it was queued for, with its task. A start that named no claim acted on the
//     claim the row gave the role, its own token, which is key a's. The record leaves such a row
//     for no issue standing at step 3, since the clear of a tree keeps the steps of a member whose
//     generation it keeps (record.Store.ClearTreeGeneration); the case holds the outbox to the
//     claim a start names whatever row it reads.
//
// The controls: key b's start with the task at its step 3 starts key b; and a start the release
// before wrote, which names no claim, starts the claim of the row now: key b's at step 3, and on
// the row of no step the role's own token, key a's.
func TestAStartActsOnTheClaimItWasQueuedFor(t *testing.T) {
	t.Parallel()
	const key = stepStartKey
	keyA, keyB := mustClaimToken(t, key, claim.RoleImplementer), mustClaimToken(t, key, claim.RoleReviewer)
	earlier, later := time.Now().Add(-time.Minute), time.Now()
	stepOne := func(entry int64, at time.Time) record.PhaseRow {
		return record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: keyA, Entry: entry,
			EntryTask: "Implement it. Issue: LEGION-208. Phase: implementing.", EntryRaisedAt: at, StepTask: "a", StepPlace: 1}
	}
	stepThree := func(at time.Time) record.PhaseRow {
		return record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: keyB, Entry: 3,
			EntryTask: "Implement it again. Issue: LEGION-208. Phase: implementing.", EntryRaisedAt: at, StepTask: "a", StepPlace: 3}
	}
	cleared := record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: keyB, Entry: 3, EntryRaisedAt: earlier}
	for _, tc := range []struct {
		name   string
		at     int
		rows   []record.PhaseRow
		named  claim.Token
		starts claim.Token
	}{
		{"key b's start of step 3 with the task back at key a's step 1", 1, []record.PhaseRow{stepOne(4, later), stepThree(earlier)}, keyB, ""},
		{"key b's start of step 3 with the role's row on no step", 3, []record.PhaseRow{cleared}, keyB, keyB},
		{"the control: key b's start with the task at its step 3", 3, []record.PhaseRow{stepOne(1, earlier), stepThree(later)}, keyB, keyB},
		{"a start the release before wrote with the task at step 3", 3, []record.PhaseRow{stepOne(1, earlier), stepThree(later)}, "", keyB},
		{"a start the release before wrote with the role's row on no step", 3, []record.PhaseRow{cleared}, "", keyA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: key,
				Title: "Workflow", Phase: phase.Implementing, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
			putOutboxIssue(t, pool, records, issue)
			openTree(t, pool, "LEGION", issue.Tree)
			holdSlot(t, pool, issue.Key)
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				v := record.PlanVersion{Issue: key, Generation: 2, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
					GivenBy: mustClaimToken(t, key, claim.RoleArchitect), GivenAt: time.Now().UTC()}
				task := record.PlanTask{Issue: key, Generation: 2, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
				steps := []record.PlanStep{
					{Issue: key, Generation: 2, Task: "a", Position: 1, Role: "implementer", Type: "implementation", AgentKey: "a"},
					{Issue: key, Generation: 2, Task: "a", Position: 2, Role: "reviewer", Type: "review", AgentKey: "b"},
					{Issue: key, Generation: 2, Task: "a", Position: 3, Role: "implementer", Type: "implementation", AgentKey: "b"},
				}
				if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
					return err
				}
				if err := records.MoveTask(ctx, tx, key, 2, "a", record.TaskPlace{Kind: record.PlaceStep, Position: tc.at}, record.TaskRunning); err != nil {
					return err
				}
				for _, row := range tc.rows {
					if err := records.PutPhase(ctx, tx, row); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("seed the plan of %s at step %d: %v", key, tc.at, err)
			}
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			const task = "Implement it again. Issue: LEGION-208. Phase: implementing."
			start := mustOutboxRow(t, key, record.SuperviseRequest{Op: "start", Tree: key, Role: claim.RoleImplementer, Claim: tc.named, Generation: 2,
				Phase: phase.Implementing, Task: task, Entry: 3}, time.Now())
			start.ID = 94
			if err := runner.execute(ctx, start); err != nil {
				t.Fatalf("run key b's start of step 3: %v", err)
			}
			for _, token := range []claim.Token{keyA, keyB} {
				machine, found := sup.Machine(token)
				var pending string
				if found {
					machine.Wait()
					if p := machine.Claim().Pending; p != nil {
						pending = p.Task
					}
				}
				if want := token == tc.starts; found != want || (want && pending != task) {
					t.Fatalf("the start of step 3 naming %q made %s's claim %t with the task %q pending; want it made %t, with step 3's task", tc.named, token,
						found, pending, want)
				}
			}
		})
	}
}

// The outbox judges a start on its own read of the issue (record.Issue.HoldsQueued), and the
// workflow's move of the task does not wait on that read: the move can commit after it and before
// the start brings the claim back. Here the linter's start of step 3 passed the read with the task
// at step 3, and the task went back to the checker's step 2 while the start provisioned the
// workspace, before its spawn. The start judges its work again under the machine's lock, before it
// spawns (supervise.StartWork, the spawn's own judge): the linter's claim starts no process, holds
// no task, and the row finishes without an error. Without that judge the runtime started the
// linter's process, the task it was handed was dropped under the machine's lock once the agent was
// ready (PhaseHolds), and the process stood ready with nothing to do for as long as its issue ran,
// as every role whose step has ended does. The control is the same start with the task left at
// step 3, which starts the linter and hands it its task.
func TestALateStartWhoseStepTheTaskLeftSpawnsNoProcess(t *testing.T) {
	t.Parallel()
	const lintTask = "Lint it. Issue: LEGION-208. Phase: step."
	for _, tc := range []struct {
		name   string
		moves  bool
		starts bool
	}{
		{"the task goes back to the checker's step 2 before the spawn", true, false},
		{"the control: the task stays at the linter's step 3", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: stepStartKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: stepStartKey, Title: "Workflow", Phase: phase.Step, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			openTree(t, pool, "LEGION", stepStartKey)
			holdSlot(t, pool, stepStartKey)
			seedStepStartPlan(t, pool, records, 3)
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds // exactly what the daemon wires
			token := mustClaimToken(t, stepStartKey, stepStartLinter)
			conn := fake.NewConn()
			sup.deps.Conns.(*fake.Conns).Register(token, conn)
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
				// The workspace is provisioned after the outbox's read of the issue and before the
				// spawn: the move of the task lands here.
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					if tc.moves {
						if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
							return records.MoveTask(ctx, tx, stepStartKey, 2, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
						}); err != nil {
							t.Errorf("move the task back to step 2: %v", err)
						}
					}
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			start := mustOutboxRow(t, stepStartKey, record.SuperviseRequest{Op: "start", Tree: stepStartKey, Role: stepStartLinter, Generation: 2, Phase: phase.Step,
				Task: lintTask, Entry: 1}, time.Now())
			start.ID = 95
			execErr := runner.execute(ctx, start)
			machine, found := sup.Machine(token)
			if !found {
				t.Fatalf("the linter's start (execute = %v) created no claim, want the claim it creates before its spawn", execErr)
			}
			machine.Wait()
			spawned := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume"))
			// What the started process then does: its agent comes up, and the task it was handed is
			// sent or dropped under the machine's lock (PhaseHolds).
			if spawned > 0 {
				generation := machine.Claim().Generation
				for _, ev := range []supervise.Event{
					supervise.StreamHello{Claim: token, Generation: generation},
					supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-linter", SessionFile: "/tmp/linter.jsonl", CapabilityHash: []byte("linter")},
					supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-linter"},
				} {
					if err := machine.Handle(ctx, ev); err != nil {
						t.Fatalf("bring the linter's agent up: handle %T: %v", ev, err)
					}
				}
				machine.Wait()
			}
			after := machine.Claim()
			var pending string
			if after.Pending != nil {
				pending = after.Pending.Task
			}
			sent := len(conn.Prompts())
			if execErr != nil || (spawned > 0) != tc.starts || (sent > 0) != tc.starts {
				t.Fatalf("the linter's start, the task moved back %t: execute = %v; the runtime started %d processes, the claim stands %s holding %q, and %d prompts reached its agent; want no error and a process and its task %t",
					tc.moves, execErr, spawned, after.State, pending, sent, tc.starts)
			}
		})
	}
}

// The answer supervise asks for before it sends a queued task, at the phase step: a task queued
// for the step of one role is not the work of the step of another, though both run as step. The
// linter's task of step 3 is dropped while the task stands at the checker's step 2, and held at
// its own step 3; the checker's task is held at step 2. The rest of the rule is
// TestPhaseHoldsAnswersFromTheIssuesCurrentPhase's.
func TestPhaseHoldsAnswersFromTheRoleOfTheRunningStep(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		at   int
		role claim.Role
		want bool
	}{
		{"the linter's task of step 3 with the task at the checker's step 2", 2, stepStartLinter, false},
		{"the control: the checker's task with the task at its step 2", 2, "checker", true},
		{"the control: the linter's task with the task at its step 3", 3, stepStartLinter, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: stepStartKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: stepStartKey, Title: "Workflow", Phase: phase.Step, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			seedStepStartPlan(t, pool, records, tc.at)
			holds := (&workflowRuntime{pool: pool, records: records}).phaseHolds
			got, err := holds(context.Background(), supervise.Claim{Issue: stepStartKey, Role: tc.role},
				supervise.Delivery{Phase: phase.Step, Generation: 2})
			if err != nil {
				t.Fatalf("phaseHolds: %v", err)
			}
			if got != tc.want {
				t.Fatalf("phaseHolds of the %s's step task with the task at step %d = %v, want %v", tc.role, tc.at, got, tc.want)
			}
		})
	}
}

// The pod start gate puts the worker of the issue's open phase first among the starts no request
// raised (supervise.Deps.PhaseRole, workflowRuntime.phaseRole), and at step that worker is the role
// of the running step, read with the issue, which the phase alone does not name: the checker with
// the task of `implementer checker linter` at step 2, the linter at step 3. The control is the
// issue at implementing, whose implementer the phase names.
func TestPhaseRoleAnswersTheRoleOfTheRunningStep(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		phase phase.Phase
		at    int
		want  claim.Role
	}{
		{"the task at the checker's step 2", phase.Step, 2, "checker"},
		{"the task at the linter's step 3", phase.Step, 3, stepStartLinter},
		{"the control: implementing", phase.Implementing, 0, claim.RoleImplementer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: stepStartKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: stepStartKey, Title: "Workflow", Phase: tc.phase, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			if tc.at > 0 {
				seedStepStartPlan(t, pool, records, tc.at)
			}
			got, err := (&workflowRuntime{pool: pool, records: records}).phaseRole(context.Background(), stepStartKey)
			if err != nil {
				t.Fatalf("phaseRole: %v", err)
			}
			if got != tc.want {
				t.Fatalf("phaseRole of %s (%s) = %q, want %q, the worker of its open phase", stepStartKey, tc.name, got, tc.want)
			}
		})
	}
}
