package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// wholeStartRow is the one supervise start row a transition left, with the fields the executor
// acts on read out of its payload.
type wholeStartRow struct {
	row    record.OutboxRow
	role   claim.Role
	task   string
	stands phase.Phase
}

// theOneStartRow is the single supervise start row issue holds, and the assertion that it is
// single. A start of a phase worker is one row that carries the worker's task, so the row this
// returns is the whole of the start the transition wrote.
func theOneStartRow(t *testing.T, pool *pgxpool.Pool, issue string) wholeStartRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, payload, coalesce(payload->>'role', ''),
		coalesce(payload->>'task', ''), coalesce(payload->>'phase', '')
		from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
	if err != nil {
		t.Fatalf("read the start rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var found []wholeStartRow
	for rows.Next() {
		var start wholeStartRow
		var payload []byte
		if err := rows.Scan(&start.row.ID, &payload, &start.role, &start.task, &start.stands); err != nil {
			t.Fatalf("read the start rows of %s: %v", issue, err)
		}
		start.row.Issue, start.row.Kind, start.row.Payload = issue, record.OutboxKindSupervise, json.RawMessage(payload)
		start.row.NextAt = time.Now()
		found = append(found, start)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the start rows of %s: %v", issue, err)
	}
	if len(found) != 1 {
		spelled := make([]string, len(found))
		for i, start := range found {
			spelled[i] = fmt.Sprintf("row %d: %s in %s, task %q", start.row.ID, start.role, start.stands, start.task)
		}
		t.Fatalf("the start rows of %s = %v, want exactly one: a phase worker's start is one row, and it carries the task", issue, spelled)
	}
	return found[0]
}

// oneStartRig is the fixture these cases share: an issue whose planner has just completed, the
// implementer start that completion wrote, and the outbox over a supervisor of its own.
type oneStartRig struct {
	pool        *pgxpool.Pool
	issue       record.Issue
	runner      *outbox
	sup         *supervisor
	rt          *fake.Runtime
	token       claim.Token
	provisioned *int
}

// newOneStartRig records the issue in planning with its planner's phases row, applies that
// planner's completion, and builds the outbox runner that will run what the completion wrote.
// The completion is a transition that gives the implementer its first task of the run, so every
// start row the issue holds afterwards belongs to that one start.
func newOneStartRig(t *testing.T) oneStartRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: issue.Key, Role: claim.RolePlanner, Claim: "plan-claim"})
	}); err != nil {
		t.Fatalf("record the planner's phases row: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	applyFact(t, pool, "api", "plan-complete", intake.HandoffComplete{Generation: 1, Issue: issue.Key,
		Role: claim.RolePlanner, Claim: "plan-claim", HandoffRecord: "plan", HandoffVersion: 1}, engine)

	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	provisioned := 0
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			provisioned++
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	return oneStartRig{pool: pool, issue: issue, runner: runner, sup: sup, rt: rt, token: token, provisioned: &provisioned}
}

// One outbox row does the whole start of a phase worker: it brings the worker's process back and
// gives it the task of the phase it is starting, in one row and one run of the executor.
//
// The daemon used to write two rows for a start (workflow.StartOrOwe): a task-less relaunch row
// claimable at once, which started the process, and a second row that carried the task and
// waited for the issue's admission slot. A worker could therefore run a whole turn with nothing
// to do (INFRA-1573). The assertion that shows that behaviour is theOneStartRow's, which every
// case here begins with: before this change the planner's completion left two start rows for the
// implementer and this test stopped there.
//
// Each case drives the executor on the row the transition wrote, as the package's other start
// cases do, and reads what the claim holds afterwards.
func TestOneStartRowDoesTheWholeStart(t *testing.T) {
	ctx := context.Background()

	t.Run("a role with no claim is created, provisioned, spawned and given the task", func(t *testing.T) {
		rig := newOneStartRig(t)
		start := theOneStartRow(t, rig.pool, rig.issue.Key)
		if start.role != claim.RoleImplementer || start.task == "" || start.stands != phase.Implementing {
			t.Fatalf("the start row = %+v, want the implementer's, carrying a task and the %s phase it is for", start, phase.Implementing)
		}

		if err := rig.runner.execute(ctx, start.row); err != nil {
			t.Fatalf("run the start row: %v", err)
		}
		machine, found := rig.sup.Machine(rig.token)
		if !found {
			t.Fatal("the start row created no claim for the implementer")
		}
		machine.Wait()
		got := machine.Claim()
		if got.State != supervise.StateLaunching || *rig.provisioned != 1 || len(rig.rt.CallsOf("Spawn")) != 1 {
			t.Fatalf("the claim after the start row = %+v, with %d workspace provisions and %d spawns, want it launching, provisioned once and spawned once",
				got, *rig.provisioned, len(rig.rt.CallsOf("Spawn")))
		}
		if want := fmt.Sprintf("%s%d", outboxDeliveryPrefix, start.row.ID); got.Pending == nil || got.Pending.ID != want || got.Pending.Task != start.task {
			t.Fatalf("the pending delivery = %+v, want the task %q of this row's own delivery %s: the row that starts the worker is the row that tells it its work",
				got.Pending, start.task, want)
		}
	})

	t.Run("a failed claim that holds the phase's task is retried and prompted once", func(t *testing.T) {
		const kept = "Continue. Reason: the launch that failed was working this."
		rig := newOneStartRig(t)
		machine, _, err := rig.sup.Create(ctx, supervise.Claim{Token: rig.token, Project: "legion", Tree: rig.issue.Tree,
			Issue: rig.issue.Key, Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
		if err != nil {
			t.Fatalf("create the implementer claim: %v", err)
		}
		if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: rig.token, Task: kept, ID: "outbox:40",
			Phase: phase.Implementing, Generation: 1}); err != nil {
			t.Fatalf("give the implementer the task its launch was working: %v", err)
		}
		rig.rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("pane launch failed")}, fake.SpawnResult{Err: errors.New("pane launch failed")})
		_ = machine.Handle(ctx, supervise.RequestSpawn{Claim: rig.token})
		if got := machine.Claim(); got.State != supervise.StateFailed || got.Pending == nil || got.Pending.ID != "outbox:40" {
			t.Fatalf("the claim before the start row = %+v, want it failed still holding the task it was working: the case's own premise", got)
		}
		start := theOneStartRow(t, rig.pool, rig.issue.Key)

		if err := rig.runner.execute(ctx, start.row); err != nil {
			t.Fatalf("run the start row on a failed claim: %v", err)
		}
		machine.Wait()
		got := machine.Claim()
		if got.State != supervise.StateLaunching || got.Budgets != (supervise.Budgets{}) {
			t.Fatalf("the claim after the start row = %+v, want it launching on fresh budgets: the one row that carries the task is the one that retries the claim",
				got)
		}
		if got.Pending == nil || got.Pending.ID != "outbox:40" || got.Pending.Task != kept {
			t.Fatalf("the pending delivery = %+v, want the one task %q the claim kept: the retry sends it, so the row's own task is no second prompt for the same phase",
				got.Pending, kept)
		}
	})

	t.Run("a claim whose session runs is given the task with no spawn", func(t *testing.T) {
		rig := newOneStartRig(t)
		machine, _, err := rig.sup.Create(ctx, supervise.Claim{Token: rig.token, Project: "legion", Tree: rig.issue.Tree,
			Issue: rig.issue.Key, Role: claim.RoleImplementer, State: supervise.StateReady, Generation: 1,
			Session: "ses-implementer", SessionFile: "/tmp/implementer.jsonl"}, "")
		if err != nil {
			t.Fatalf("create the implementer claim: %v", err)
		}
		start := theOneStartRow(t, rig.pool, rig.issue.Key)

		if err := rig.runner.execute(ctx, start.row); err != nil {
			t.Fatalf("run the start row on a running claim: %v", err)
		}
		machine.Wait()
		got := machine.Claim()
		if spawns, resumes := len(rig.rt.CallsOf("Spawn")), len(rig.rt.CallsOf("Resume")); spawns != 0 || resumes != 0 {
			t.Fatalf("the start row made %d spawns and %d resumes, want none: the session is already up, so the row has only its task to give",
				spawns, resumes)
		}
		if want := fmt.Sprintf("%s%d", outboxDeliveryPrefix, start.row.ID); got.Pending == nil || got.Pending.ID != want || got.Pending.Task != start.task {
			t.Fatalf("the pending delivery = %+v, want the task %q of this row's own delivery %s", got.Pending, start.task, want)
		}
		if *rig.provisioned != 0 {
			t.Fatalf("the start row provisioned the workspace %d times, want none: the claim it found needs no workspace built again", *rig.provisioned)
		}
	})
}
