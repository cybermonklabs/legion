package admit

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// putThreeTasks writes key's plan at generation 1 of three tasks, each with the steps `implementer
// reviewer tester`: a running at the implementer's step 1, b running at the reviewer's step 2, and
// c held at the tester's step 3, so the store reads the implementer's and the reviewer's steps as
// running and the tester's as held.
func putThreeTasks(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		project, err := claim.ProjectToken(testProject)
		if err != nil {
			t.Fatal(err)
		}
		architect, err := claim.NewToken(project, key, claim.RoleArchitect)
		if err != nil {
			t.Fatal(err)
		}
		v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: architect, GivenAt: fixedNow}
		var tasks []record.PlanTask
		var steps []record.PlanStep
		for i, task := range []string{"a", "b", "c"} {
			tasks = append(tasks, record.PlanTask{Issue: key, Generation: 1, Task: task, PlanVersion: 1, BlockText: "Do " + task + ".",
				State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: i + 1}})
			for position, role := range []string{"implementer", "reviewer", "tester"} {
				steps = append(steps, record.PlanStep{Issue: key, Generation: 1, Task: task, Position: position + 1, Role: role,
					Type: plan.BuiltinRoles()[role].Type})
			}
		}
		if err := records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			t.Fatalf("seed the plan of %s: %v", key, err)
		}
		if err := records.HoldTask(ctx, tx, key, 1, "c", ""); err != nil {
			t.Fatalf("hold task c of %s: %v", key, err)
		}
	})
}

// A promotion brings back the worker of each running step of a member (promotedOwners, the owners
// startMidPhaseMembers starts on their work): a member with task a at the implementer's step and
// task b at the reviewer's, read as the store reads it, has the implementer and the reviewer as
// owners while it stands in implementing, and task c held at the tester's step adds none. A member
// with no plan rows standing in testing has its tester, as before; one held from a step has none,
// since a held step takes no work; and one at accepting has none, since that step is the
// architect's own.
//
// The promotion is held at its selection here; its start of each owner, on the copy of the member
// read for that owner's task (workflow's PromotedView), is
// TestACarriedOrphanOfSeveralTasksStartsEachTasksStep's.
//
// The fault it catches is a promotion that starts one running step of several (its control: the
// owners read as the single role of the member's phase, which brings back the implementer alone).
func TestAPromotionStartsEachRunningStepOfAMember(t *testing.T) {
	pool := migratedPool(t)
	ctx, records := context.Background(), record.NewStore()
	member := func(key string, p phase.Phase) record.Issue {
		return record.Issue{Key: key, Tree: "LEGION-700", Project: testProject, Title: key, Phase: p, Generation: 1,
			Status: "in_progress", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	}
	held := member("LEGION-703", phase.Held)
	held.Hold = &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated}
	seeded := []record.Issue{member("LEGION-701", phase.Implementing), member("LEGION-702", phase.Testing), held, member("LEGION-704", phase.Accepting)}
	inTx(t, pool, func(tx pgx.Tx) {
		for _, issue := range seeded {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				t.Fatalf("seed %s: %v", issue.Key, err)
			}
		}
	})
	putThreeTasks(t, pool, "LEGION-701")

	for key, want := range map[string][]claim.Role{
		"LEGION-701": {claim.RoleImplementer, claim.RoleReviewer},
		"LEGION-702": {claim.RoleTester},
		"LEGION-703": nil,
		"LEGION-704": nil,
	} {
		if got := promotedOwners(issue(t, pool, key)); !slices.Equal(got, want) {
			t.Errorf("the promotion's owners of %s = %v, want %v", key, got, want)
		}
	}
}

// The entry repair (owedEntryRoles, through phaseWorkersOf) sends a worker behind the entry its
// phases row names that entry's work on the copy of the issue its step is given its work on
// (workflow.StepCopyOn). Of LEGION-751's tasks a at the implementer's step, b at the reviewer's and
// c held at the tester's, the reviewer's row names an entry and stores no task of it, as a
// re-admission's reset or the upgrade to the entry leaves it, so the repair sends the reviewer the
// phase's resume task (workflow.Engine.SendOwedEntry, ResumeTask): the resume of b's step 2, in
// reviewing, at that entry, naming the claim of the step's agent. The selection read the member at
// the step's phase alone, which names no task, so the resume's walk of a plan of several tasks
// failed, and with it the whole fact.
//
// The fault it catches is a repair that composes one step's start on a copy of the issue naming no
// task (its control: workflow.StepCopyOn answering the issue at the step's phase, under which the
// pass fails with the walk that names no task).
func TestTheEntryRepairResumesAStepOnItsTasksCopy(t *testing.T) {
	pool := migratedPool(t)
	const root, key = "LEGION-750", "LEGION-751"
	seedTreeAndMember(t, pool, root, key, phase.Implementing, 1, false)
	takeSlot(t, pool, key)
	putThreeTasks(t, pool, key)
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, key, claim.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: key, Role: claim.RoleReviewer,
			Claim: token, Entry: seededEntry, EntryRaisedAt: fixedNow}); err != nil {
			t.Fatalf("record the reviewer's phases row of %s with no task: %v", key, err)
		}
	})
	agent, err := claim.AgentToken(project, key, claim.Agent(claim.RoleReviewer))
	if err != nil {
		t.Fatal(err)
	}

	tick(t, pool, quiet(t, 1), "converge-pass-1")

	starts := roleStarts(t, pool, key, claim.RoleReviewer)
	if len(starts) != 1 || starts[0].Phase != phase.Reviewing || starts[0].Entry != seededEntry ||
		starts[0].Claim != agent || !strings.Contains(starts[0].Task, " Task: b. Step 2 of 3: reviewer.") ||
		!strings.Contains(starts[0].Task, "Resume the existing phase work.") {
		t.Fatalf("the repair's starts of the reviewer of %s = %+v, want the one resume of b's step 2 in reviewing at entry %d, naming %s", key, starts, seededEntry, agent)
	}
	if implementer := roleStarts(t, pool, key, claim.RoleImplementer); len(implementer) != 0 {
		t.Fatalf("the repair's starts of the implementer of %s = %+v, want none: its role has no phases row, so it is owed no entry", key, implementer)
	}
}
