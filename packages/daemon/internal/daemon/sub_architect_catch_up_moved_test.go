package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A child that left the workflow ends its unpublished catch-up whatever status a person writes on
// it afterwards, short of the todo that re-enters it. A person can move a parked child to a status
// only a running issue has (in_progress here) without a todo: the engine re-enters a child on todo
// alone (workflow's dispatchIssue), so the child stays done at the same generation, with its own
// architect suspended by the park, while admission records the status the person wrote (its
// recordObservation). Nothing supersedes the catch-up then and its claim has not ended, so a check
// that reads only the out-of-workflow statuses lets the row wait again on every tick, publishing to
// a role nobody holds and warning from the seventh attempt, for as long as the child stands so.
// The child's phase says it runs nothing (record.InWorkflow reads both), and its next launch writes
// a fresh catch-up, so the row finishes without a publish.
func TestAParkedChildsCatchUpEndsAfterAPersonMovesItToARunningStatus(t *testing.T) {
	t.Parallel()
	// The record state is reachable: a park, then a person's in_progress, leaves the child done at
	// its generation, under a status no out-of-workflow check names.
	rig := newChildArchitectRig(t)
	rig.written("release", childAt("todo", 2))
	rig.apply("child-parked", intake.DispatchIssue{Key: doorChild, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "backlog",
		Title: doorChild, Parent: doorRoot, Rank: "V"})
	rig.apply("child-in-progress", intake.DispatchIssue{Key: doorChild, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated", Status: "in_progress",
		Title: doorChild, Parent: doorRoot, Rank: "V"})
	moved := readIssue(t, rig.pool, doorChild)
	if moved.Phase != phase.Done || moved.Status != "in_progress" || moved.Generation != 1 || record.OutOfWorkflow(moved.Status) {
		t.Fatalf("the parked child after a person's in_progress = phase %s, status %s, generation %d; the scenario needs it done at generation 1 under a running status",
			moved.Phase, moved.Status, moved.Generation)
	}

	// Its catch-up, unpublished when the park came, with its architect suspended by the park.
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	root := "LEGION-1"
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root,
		Parent: &root, Title: "Child", Phase: moved.Phase, Generation: 1, Status: moved.Status})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	subArchitectOn(t, sup, supervise.StateSuspended, 1, supervise.SuspendedByDaemon)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", subArchitectCatchUp(1, 1), time.Now()))
	var logged bytes.Buffer
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-2"))
	clock := time.Now()
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
		supervisor: sup, project: "legion", now: func() time.Time { return clock }}
	for tick := range 8 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		clock = clock.Add(2 * time.Minute)
	}
	if attempts, delivered := publisher.snapshot(); attempts != 0 || len(delivered) != 0 || outboxRows(t, pool) != 0 || strings.Contains(logged.String(), `msg="outbox row waits"`) {
		t.Fatalf("%d publishes, delivered %v with %d rows left, logged:\n%s\nwant the catch-up of the done child finished with no publish and no wait",
			attempts, deliveredKinds(t, delivered), outboxRows(t, pool), logged.String())
	}
}
