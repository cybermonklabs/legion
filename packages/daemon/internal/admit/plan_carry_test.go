package admit

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
// otherwise (the move TriggerGateOpened makes). The carry writes the run's plan itself: the
// default plan at the new generation, under that run's own id, so the project's read shows this
// run's plan and not the run before's. The root here is carried at accepting, as the hand-over
// runs in production: the old root's linger ends, its fence names the child, and the daemon's
// labelled todo re-admits it carrying its run.
func TestACarriedRootsNewRunWritesItsPlan(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	const oldRoot, root = "LEGION-700", "LEGION-301"
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
	})

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: root, Parent: oldRoot, Rank: "A", HandedOver: true, DaemonActor: true}, admission.engine)

	if stands := issue(t, pool, root); stands.Tree != root || stands.Generation != 2 || stands.Phase != phase.Accepting {
		t.Fatalf("the carried root after the hand-over = %#v, want a tree of its own at generation 2 in accepting: the case's premise", stands)
	}
	want := record.PlanRecord{ID: workflow.PlanID(root, 2), Generation: 2, Source: "default",
		Tasks: []record.PlanRecordTask{{ID: "default", Steps: workflow.DefaultSteps()}}}
	if plans := planRecordsOf(t, pool, root); len(plans) != 1 || !reflect.DeepEqual(plans[0], want) {
		t.Fatalf("the carry queued the plans %+v, want the one plan of its new run %+v", plans, want)
	}
}
