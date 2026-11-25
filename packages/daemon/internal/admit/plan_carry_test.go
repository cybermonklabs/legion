package admit

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// planRecordsOf is every plan record the outbox holds for issue, in the order queued.
func planRecordsOf(t *testing.T, pool *pgxpool.Pool, issue string) []record.PlanRecord {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where kind = $1 and issue = $2 order by id`,
		string(record.OutboxKindPlanRecord), issue)
	if err != nil {
		t.Fatalf("read the plan records of %s: %v", issue, err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read the plan records of %s: %v", issue, err)
	}
	var plans []record.PlanRecord
	for _, payload := range payloads {
		var plan record.PlanRecord
		if err := json.Unmarshal(payload, &plan); err != nil {
			t.Fatalf("decode a plan record of %s: %v", issue, err)
		}
		plans = append(plans, plan)
	}
	return plans
}

// A hand-over carries a root into a tree of its own at a new generation in the phase it ran in
// elsewhere (readmit's carry), so that run never leaves admitted, where the run's plan is written
// otherwise (the move TriggerGateOpened makes). The carry writes the run's plan itself, the plan
// the carried run walks, under that run's own id, so the project's read shows this run's plan and
// not the run before's: the default plan where the run walked the project's default steps, and
// its owner's plan, with the document and the version Legion took, where its owner gave one. The
// root here is carried at accepting, as the hand-over runs in production: the old root's linger
// ends, its fence names the child, and the daemon's labelled todo re-admits it carrying its run.
func TestACarriedRootsNewRunWritesItsPlan(t *testing.T) {
	const oldRoot, root = "LEGION-700", "LEGION-301"
	for _, tc := range []struct {
		name string
		// owners says whether the run before walked its owner's plan, `implementer reviewer
		// acceptance:owner`, standing at its acceptance, rather than the default steps.
		owners bool
		want   record.PlanRecord
	}{
		{name: "the default steps", want: record.PlanRecord{ID: workflow.PlanID(root, 2), Generation: 2, Source: "default",
			Tasks: []record.PlanRecordTask{{ID: "default", Steps: []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}}}}},
		{name: "the owner's plan", owners: true, want: record.PlanRecord{ID: workflow.PlanID(root, 2), Generation: 2, Source: "owner",
			PlanDocument: &record.PlanDocument{ArtifactID: "plan-artifact", Version: 2},
			Tasks:        []record.PlanRecordTask{{ID: "a", Steps: []string{"implementer", "reviewer", "acceptance:owner"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 4)
			until := fixedNow.Add(time.Hour)
			parent := oldRoot
			putIssue(t, pool, record.Issue{Key: oldRoot, Tree: oldRoot, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Project: testProject, Title: "the root whose linger ends", Phase: phase.Done,
				Generation: 1, Status: "done", LingerUntil: &until})
			putIssue(t, pool, record.Issue{Key: root, Tree: oldRoot, Parent: &parent, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Project: testProject, Title: "a carried root", Phase: phase.Accepting,
				Generation: 1, Status: "needs_review", Rank: "A", LastDispatchSeq: 1})
			inTx(t, pool, func(tx pgx.Tx) {
				if err := record.NewStore().PutHandover(context.Background(), tx, record.Handover{Issue: root, Tree: oldRoot, Generation: 1}); err != nil {
					t.Fatalf("fence the hand-over of %s: %v", root, err)
				}
				if tc.owners {
					seedOwnersPlanAtAcceptance(t, tx, root, architectClaim(t, oldRoot))
				}
			})

			apply(t, pool, admission, "handover", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: "todo", Title: root, Parent: oldRoot, Rank: "A", HandedOver: true, DaemonActor: true}, admission.engine)

			if stands := issue(t, pool, root); stands.Tree != root || stands.Generation != 2 || stands.Phase != phase.Accepting {
				t.Fatalf("the carried root after the hand-over = %#v, want a tree of its own at generation 2 in accepting: the case's premise", stands)
			}
			if plans := planRecordsOf(t, pool, root); len(plans) != 1 || !reflect.DeepEqual(plans[0], tc.want) {
				t.Fatalf("the carry queued the plans %+v, want the one plan of its new run %+v", plans, tc.want)
			}
		})
	}
}

// seedOwnersPlanAtAcceptance writes the plan key's owner, givenBy, gave for its run at generation
// 1: version 2 of plan-artifact, one task a whose steps are `implementer reviewer
// acceptance:owner`, running at its acceptance, step 3, where an issue in accepting stands.
func seedOwnersPlanAtAcceptance(t *testing.T, tx pgx.Tx, key string, givenBy claim.Token) {
	t.Helper()
	ctx := context.Background()
	v := record.PlanVersion{Issue: key, Generation: 1, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: givenBy, GivenAt: fixedNow}
	task := record.PlanTask{Issue: key, Generation: 1, Task: "a", PlanVersion: 2, BlockText: "Build the widget.", State: record.TaskWaiting}
	roles := plan.BuiltinRoles()
	var steps []record.PlanStep
	for i, step := range []plan.Step{{Role: "implementer"}, {Role: "reviewer"}, {Role: "acceptance", Key: "owner"}} {
		steps = append(steps, record.PlanStep{Issue: key, Generation: 1, Task: "a", Position: i + 1, Role: step.Role, AgentKey: step.Key,
			Type: roles[step.Role].Type})
	}
	steps = record.NameAgents(nil, nil, steps, nil)
	store := record.NewStore()
	if err := store.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
		t.Fatalf("put the owner's plan of %s: %v", key, err)
	}
	if err := store.MoveTask(ctx, tx, key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 3}, record.TaskRunning); err != nil {
		t.Fatalf("stand task a of %s at its acceptance: %v", key, err)
	}
}
