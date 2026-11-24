package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// startBranchIssue is the issue the tests of a start's branch read.
const startBranchIssue = "LEGION-209"

// startRowOf is the start the queue builds of start on issue (queuedRow), decoded.
func startRowOf(t *testing.T, tx pgx.Tx, issue string, start record.SuperviseRequest) record.SuperviseRequest {
	t.Helper()
	row, err := queuedRow(t.Context(), tx, record.NewStore(), issue, start, time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("queue the start of %s on %s: %v", start.Role, issue, err)
	}
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("decode the start of %s on %s: %v", start.Role, issue, err)
	}
	queued, ok := payload.(record.SuperviseRequest)
	if !ok {
		t.Fatalf("the start of %s on %s queued as %T", start.Role, issue, payload)
	}
	return queued
}

// agentToken is the claim token of agent on issue.
func agentToken(t *testing.T, issue string, agent claim.Agent) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.AgentToken(project, issue, agent)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// seedStartBranchPlan writes on startBranchIssue, a child of LEGION-208 in implementing, the plan
// of tasks a, ab and b, one implementation step each: a's by the agent k1, ab's and b's by k2. Each
// takes its branch in the order a, b, ab, so a holds the issue's own branch and b
// legion/LEGION-209.b; a's and b's steps run, and ab's waits for k2. It answers each task's branch.
func seedStartBranchPlan(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	ctx := t.Context()
	openTree(t, pool)
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: startBranchIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: parent,
		Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	records := record.NewStore()
	given := time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC)
	agents := map[string]claim.Agent{"a": "k1", "ab": "k2", "b": "k2"}
	branches := map[string]string{}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		version := record.PlanVersion{Issue: startBranchIssue, Generation: 1, Version: 1, ArtifactID: "artifact-209", Source: record.PlanSourceOwner,
			GivenBy: claim.Token("legion-legion-legion-208-architect"), GivenAt: given}
		var tasks []record.PlanTask
		var steps []record.PlanStep
		for _, id := range []string{"a", "ab", "b"} {
			tasks = append(tasks, record.PlanTask{Issue: startBranchIssue, Generation: 1, Task: id, PlanVersion: 1, BlockText: "Do " + id + ".",
				State: record.TaskWaiting})
			steps = append(steps, record.PlanStep{Issue: startBranchIssue, Generation: 1, Task: id, Position: 1, Role: string(claim.RoleImplementer),
				Type: "implementation", AgentKey: string(agents[id]), Agent: agents[id]})
		}
		if err := records.PutPlan(ctx, tx, version, tasks, steps); err != nil {
			return err
		}
		for _, id := range []string{"a", "b", "ab"} {
			branch, err := records.AssignTaskBranch(ctx, tx, startBranchIssue, 1, id)
			if err != nil {
				return err
			}
			branches[id] = branch
			if err := records.MoveTask(ctx, tx, startBranchIssue, 1, id, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning); err != nil {
				return err
			}
		}
		// The store's move into running writes that each step waits for its join; a's and b's
		// joins ended, so their steps run.
		for _, id := range []string{"a", "b"} {
			if err := records.SetStepWait(ctx, tx, startBranchIssue, 1, id, record.StepWaitNone, nil); err != nil {
				return err
			}
		}
		return records.SetStepWait(ctx, tx, startBranchIssue, 1, "ab", record.StepWaitAgent, &given)
	})
	if branches["a"] != "legion/LEGION-209" || branches["b"] != "legion/LEGION-209.b" {
		t.Fatalf("the branches given are %v, want a on the issue's own and b on legion/LEGION-209.b", branches)
	}
	return branches
}

// The start of a step names the branch of the step's task, so the step's agent adopts that task's
// working copy: tasks a and b each run their implementation step, a's by the agent k1 and b's by
// k2, and the start of k2 names b's branch. Task ab, whose implementation step waits for k2 and
// comes before b in task id order, runs no step yet, so the start of k2 is still b's. The start of
// a, whose task holds the issue's own branch, names none, as every start of a task on the issue's
// own branch did before tasks had branches of their own.
func TestAStartOfATaskNamesItsBranch(t *testing.T) {
	pool := migratedPool(t)
	parent := "LEGION-208"
	branches := seedStartBranchPlan(t, pool)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		start := record.SuperviseRequest{Op: "start", Tree: parent, Role: claim.RoleImplementer, Task: "Implement it.", Generation: 1, Phase: phase.Implementing}
		start.Claim = agentToken(t, startBranchIssue, "k2")
		if got := startRowOf(t, tx, startBranchIssue, start).Branch; got != branches["b"] {
			t.Fatalf("the start of k2 names the branch %q, want b's, %q: k2 runs b's step, and ab's step waits for it", got, branches["b"])
		}
		start.Claim = agentToken(t, startBranchIssue, "k1")
		if got := startRowOf(t, tx, startBranchIssue, start).Branch; got != "" {
			t.Fatalf("the start of k1 names the branch %q, want none: a's task holds the issue's own branch", got)
		}
		return nil
	})
}

// A start of a phase worker whose step the plan does not run is refused, naming it, where the
// plan's tasks work on branches of their own: no branch would name the issue's own, which another
// task holds, so the agent would adopt that task's working copy. Of the plan seedStartBranchPlan
// writes, the start in implementing of the agent k3, which runs no step, is refused naming its
// claim and the phase. A start that serves no step's phase names no branch, as before: the
// architect's, which names no phase, its start at the owner's acceptance, which works no task's
// working copy, and a production check's look, which names no phase.
//
// The fault it catches is a step's start whose step is unknown queued as a start on the issue's own
// branch (its control: startBranch answering no branch for it, as it did).
func TestAStartOfAStepThePlanDoesNotRunIsRefused(t *testing.T) {
	pool := migratedPool(t)
	parent := "LEGION-208"
	seedStartBranchPlan(t, pool)
	k3 := agentToken(t, startBranchIssue, "k3")
	seedRecord(t, pool, func(tx pgx.Tx) error {
		start := record.SuperviseRequest{Op: "start", Tree: parent, Role: claim.RoleImplementer, Claim: k3, Task: "Implement it.", Generation: 1,
			Phase: phase.Implementing}
		row, err := queuedRow(t.Context(), tx, record.NewStore(), startBranchIssue, start, time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC))
		if err == nil || !strings.Contains(err.Error(), string(k3)) || !strings.Contains(err.Error(), string(phase.Implementing)) {
			t.Errorf("the start of k3, which runs no step, = %+v, %v, want refused naming %s and implementing", row, err, k3)
		}
		for name, other := range map[string]record.SuperviseRequest{
			"the architect's": {Op: "start", Tree: parent, Role: claim.RoleArchitect, Task: "Own it.", Generation: 1},
			"the architect's at the owner's acceptance": {Op: "start", Tree: parent, Role: claim.RoleArchitect, Task: "Accept it.", Generation: 1,
				Phase: phase.Accepting},
			"a production check's look": {Op: "start", Tree: parent, Role: claim.RoleImplementer, Claim: k3, Task: "Look.", Generation: 1, CheckLook: 1},
		} {
			if got := startRowOf(t, tx, startBranchIssue, other).Branch; got != "" {
				t.Errorf("%s start names the branch %q, want none", name, got)
			}
		}
		return nil
	})
}

// An issue with no plan rows runs on its own branch, and its start names no branch, as before.
func TestAStartOfAnIssueWithNoPlanRowsNamesNoBranch(t *testing.T) {
	pool := migratedPool(t)
	openTree(t, pool)
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: startBranchIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: parent,
		Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		start := record.SuperviseRequest{Op: "start", Tree: parent, Role: claim.RoleImplementer, Task: "Implement it.", Generation: 1, Phase: phase.Implementing}
		if got := startRowOf(t, tx, startBranchIssue, start).Branch; got != "" {
			t.Fatalf("the start of an issue with no plan rows names the branch %q, want none", got)
		}
		return nil
	})
}
