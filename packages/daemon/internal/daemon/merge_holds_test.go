package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// One event of the project's merge holds document, applied as the Dispatch consumer applies it,
// reaches each role that a hold can stop (#4065). With one issue in
// accepting and one in merging, it writes two deliveries, and the outbox publishes one notice to
// that tree's architect, on the architect's role topic, and one message to that issue's merger, on
// the merger's role topic. The merger's pod was released, as the idle release leaves a held merger
// that ended its turn: a Dispatch document event does not wake such a pod, so the row wakes it
// first, and publishes once the woken session holds the role. The same event delivered twice
// writes the deliveries once, and an event of another project writes none.
func TestAMergeHoldsChangeReachesTheArchitectInAcceptingAndTheMergerInMerging(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	for _, issue := range []record.Issue{
		{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
			Tree: "LEGION-208", Title: "Accepting", Phase: phase.Accepting, Generation: 1, Status: "needs_review", Rank: "U"},
		{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
			Tree: "LEGION-209", Title: "Merging", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "V"},
	} {
		putOutboxIssue(t, pool, records, issue)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	merger := releasedMergerOf(t, sup, "LEGION-209")
	publisher := &holderPublisher{}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	// The drain's clock is set once the facts are applied: each row is due at the engine's now when
	// its fact wrote it.
	var clock time.Time
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: outboxTreeStore(t, pool, "LEGION-208", "LEGION-209"),
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), notices: publisher, handlers: []intake.Handler{engine},
		now: func() time.Time { return clock }, log: quietLogger(),
	}
	apply := func(id, project string) {
		t.Helper()
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", id, intake.MergeHoldsChanged{Project: project, Event: id}, runner.handlers...); err != nil {
			t.Fatalf("apply the merge holds change %s of %s: %v", id, project, err)
		}
	}
	apply("dispatch-71", "LEGION")
	apply("dispatch-71", "LEGION")
	apply("dispatch-72", "OTHER")
	if rows := outboxRows(t, pool); rows != 2 {
		t.Fatalf("outbox rows = %d, want 2: the architect's notice and the merger's row, each written once", rows)
	}

	clock = time.Now()
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the merge holds change: %v", err)
	}
	architect := architectTopic(t, "LEGION-208")
	_, delivered := publisher.snapshot()
	if len(delivered) != 1 || delivered[0].topic != architect {
		t.Fatalf("delivered = %#v, want the architect's notice alone on %s, while the merger's row waits for its pod", delivered, architect)
	}
	if notice, ok := delivered[0].payload.(record.Notice); !ok || notice.Kind != "merge-holds-changed" || notice.Reason != record.MergeHoldsChangedReason("LEGION") {
		t.Fatalf("the architect was told %#v, want the merge-holds-changed notice that names the document", delivered[0].payload)
	}
	if held := merger.Claim(); held.Released() {
		t.Fatalf("the merger is %s by %s, want the row to have woken it", held.State, held.SuspendedBy)
	}
	if left := outboxRows(t, pool); left != 1 {
		t.Fatalf("outbox rows left = %d, want the merger's row waiting for the woken pod", left)
	}

	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the merger's row again: %v", err)
	}
	mergerTopic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-209", claim.RoleMerger))
	_, delivered = publisher.snapshot()
	want := record.MergeHoldsChanged{Project: "LEGION"}.Message("LEGION-209")
	if len(delivered) != 2 || delivered[1].topic != mergerTopic || delivered[1].message != want {
		t.Fatalf("delivered = %#v, want the merger told %q on %s", delivered, want, mergerTopic)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want both deliveries finished", left)
	}
	var wakes int
	if err := pool.QueryRow(ctx, `select count(*) from pod_resumes where issue = $1 and role = $2 and kind = $3`,
		"LEGION-209", string(claim.RoleMerger), pods.WakeDelivery).Scan(&wakes); err != nil {
		t.Fatalf("read the recorded wakes: %v", err)
	}
	if wakes != 1 {
		t.Fatalf("recorded delivery wakes of the merger = %d, want the one its row took", wakes)
	}
}

// A merge holds change row finishes undelivered once its issue has left merging: the merger of an
// issue that the architect sent back to implementing does not merge, so no hold is its concern.
// Nothing is published and no pod is woken.
func TestAMergeHoldsChangeOfAnIssueThatLeftMergingEndsUndelivered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-209", Title: "Sent back", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	merger := releasedMergerOf(t, sup, "LEGION-209")
	publisher := &holderPublisher{}
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: outboxTreeStore(t, pool, "LEGION-209"),
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), notices: publisher,
		now: time.Now, log: quietLogger(),
	}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-209", record.MergeHoldsChanged{Project: "LEGION"}, time.Now().Add(-time.Minute)))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the merge holds change: %v", err)
	}
	if attempts, delivered := publisher.snapshot(); attempts != 0 || len(delivered) != 0 {
		t.Fatalf("publish attempts = %d, delivered %#v; want none: the issue left merging", attempts, delivered)
	}
	if held := merger.Claim(); !held.Released() {
		t.Fatalf("the merger is %s by %s, want it left released", held.State, held.SuspendedBy)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the row finished undelivered", left)
	}
}

// releasedMergerOf creates the merger's claim on issue as an idle release left it: suspended with
// the source `released`, its session intact, and nothing but a wake to bring its pod back.
func releasedMergerOf(t *testing.T, sup *supervisor, issue string) *supervise.Machine {
	t.Helper()
	machine, _, err := sup.Create(context.Background(), supervise.Claim{Token: mustClaimToken(t, issue, claim.RoleMerger),
		Project: "legion", Tree: issue, Issue: issue, Role: claim.RoleMerger, Generation: 1, ServingGeneration: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
		Session: "ses-merger", SessionFile: t.TempDir() + "/session.jsonl"}, "")
	if err != nil {
		t.Fatalf("create the released merger's claim on %s: %v", issue, err)
	}
	if held := machine.Claim(); !held.Released() {
		t.Fatalf("the merger of %s is %s by %s, want it released", issue, held.State, held.SuspendedBy)
	}
	return machine
}
