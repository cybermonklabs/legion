package daemon

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// catchUpDelivery is a root architect's catch-up between its queueing and its delivery: the
// notice tree of LEGION-1, its architect working, and the runner that delivers the row, which
// states the design gate the shipped deployment gates (the single step spec).
type catchUpDelivery struct {
	pool      *pgxpool.Pool
	records   record.Store
	publisher *hookPublisher
	runner    *outbox
	now       time.Time
}

func newCatchUpDelivery(t *testing.T) *catchUpDelivery {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	now := time.Now()
	publisher := &hookPublisher{}
	return &catchUpDelivery{pool: pool, records: records, publisher: publisher, now: now,
		runner: &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
			supervisor: sup, project: "legion", gatedSteps: config.DesignGateRootIssues.Steps(), now: func() time.Time { return now }}}
}

// write runs one write against the record between the queueing and the delivery.
func (d *catchUpDelivery) write(t *testing.T, what string, write func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error { return write(ctx, tx) }); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// putSpecGate registers LEGION-1's spec step on artifact at version, as register_gate records it.
func (d *catchUpDelivery) putSpecGate(t *testing.T, artifact string, version int) {
	t.Helper()
	d.write(t, "register the spec gate", func(ctx context.Context, tx pgx.Tx) error {
		return d.records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-1", Step: record.DesignStepSpec, ArtifactID: artifact, LatestVersion: version})
	})
}

// deliver leases the one queued catch-up of LEGION-1 and runs it as a lane does, returning the
// payload the row holds and every notice the run published.
func (d *catchUpDelivery) deliver(t *testing.T) (record.Notice, []record.Notice) {
	t.Helper()
	leased, queued := leaseCatchUp(t, d.pool, d.records, "LEGION-1", d.now)
	if err := d.runner.notice(context.Background(), leased, queued); err != nil {
		t.Fatalf("deliver the catch-up: %v", err)
	}
	var published []record.Notice
	for _, sent := range d.publisher.published() {
		published = append(published, sent.payload.(record.Notice))
	}
	return queued, published
}

// The replay of dispatch://INFRA-2819. A catch-up was composed when the architect's claim was ready
// and delivered from the outbox later; meanwhile the architect registered its design gate again, so
// the delivered payload reported the gate at version 7 while read_record, five seconds on,
// reported version 8, registered about fifteen minutes before. The delivery composes the tree
// again inside its own read (readNoticeTree, workflow.TreeCatchUp), so the catch-up reports the
// registration it was delivered after: the document and its version, whether the step had a
// registration when the row was queued or none.
func TestACatchUpQueuedBeforeAGateRegistrationReportsTheRegistrationItIsDeliveredAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(*testing.T, *catchUpDelivery)
		queued record.CatchUpGate
	}{
		{name: "registered at a later version", before: func(t *testing.T, d *catchUpDelivery) { d.putSpecGate(t, "artifact-spec", 7) },
			queued: record.CatchUpGate{Step: record.DesignStepSpec, Gated: true, Artifact: "artifact-spec", Version: 7}},
		{name: "registered for the first time", before: func(*testing.T, *catchUpDelivery) {},
			queued: record.CatchUpGate{Step: record.DesignStepSpec, Gated: true, Unregistered: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newCatchUpDelivery(t)
			tc.before(t, d)
			keptCatchUp(t, d.pool, d.records, "LEGION-1", 1, d.now)
			d.putSpecGate(t, "artifact-spec", 8)

			queued, published := d.deliver(t)
			if !reflect.DeepEqual(queued.CatchUp.Gates, []record.CatchUpGate{tc.queued}) {
				t.Fatalf("the queued catch-up states the gates %+v, want %+v: the row holds the record of its queueing", queued.CatchUp.Gates, tc.queued)
			}
			if len(published) != 1 {
				t.Fatalf("the delivery published %d notices, want the one catch-up", len(published))
			}
			want := record.CatchUpGate{Step: record.DesignStepSpec, Gated: true, Artifact: "artifact-spec", Version: 8}
			if got := published[0].CatchUp.Gates; !reflect.DeepEqual(got, []record.CatchUpGate{want}) {
				t.Fatalf("the delivered catch-up states the gates %+v, want %+v: the architect is told the registration the "+
					"record holds when the notice is delivered, not the one it held when the row was queued", got, want)
			}
		})
	}
}

// An issue that joins the tree while the catch-up waits in the outbox is in the catch-up the
// architect reads: the delivery lists the tree as it stands then, and its reason names the new
// issue when it waits in accepting for the architect's acceptance.
func TestACatchUpListsAnIssueItsTreeGainedBeforeItsDelivery(t *testing.T) {
	d := newCatchUpDelivery(t)
	keptCatchUp(t, d.pool, d.records, "LEGION-1", 1, d.now)
	root := "LEGION-1"
	putOutboxIssue(t, d.pool, d.records, record.Issue{Key: "LEGION-4", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: root, Parent: &root, Title: "Joined", Phase: phase.Accepting, Generation: 1, Status: "in_progress"})

	queued, published := d.deliver(t)
	if slices.ContainsFunc(queued.CatchUp.Issues, func(issue record.CatchUpIssue) bool { return issue.Key == "LEGION-4" }) || strings.Contains(queued.Reason, "LEGION-4") {
		t.Fatalf("the queued catch-up already names LEGION-4 (%+v, %q): the fixture adds it after the queueing", queued.CatchUp.Issues, queued.Reason)
	}
	if len(published) != 1 {
		t.Fatalf("the delivery published %d notices, want the one catch-up", len(published))
	}
	delivered := published[0]
	want := record.CatchUpIssue{Key: "LEGION-4", Parent: root, Title: "Joined", Phase: phase.Accepting, Status: "in_progress"}
	if !slices.Contains(delivered.CatchUp.Issues, want) {
		t.Fatalf("the delivered catch-up lists %+v, want it to list %+v: the tree is the one the record holds at the delivery", delivered.CatchUp.Issues, want)
	}
	if !strings.Contains(delivered.Reason, "waiting for your acceptance, which no worker gives: LEGION-4") {
		t.Fatalf("the delivered catch-up's reason is %q, want it to name LEGION-4 waiting for the architect's acceptance", delivered.Reason)
	}
}

// Whether a hand-over created the root at its generation (HandedOver) and whether its old root's
// gate came with it (Carried) are the record's at the delivery, as the gates and the issues are:
// they tell the architect whether its workers already run their phases and whether it registers a
// spec of its own, so a catch-up that stated them as they stood at its queueing would send it to
// decompose a tree that is already running, or to register a gate it carried. Each case moves one
// of them in the record (handed_over_roots, which workflow.TreeCatchUp reads) between the queueing
// and the delivery, at the root's own generation, so that nothing supersedes the row.
func TestACatchUpStatesTheHandOverItsRootRecordsWhenItIsDelivered(t *testing.T) {
	for _, tc := range []struct {
		name              string
		before, after     *record.HandedOverRoot
		queued, delivered record.CatchUp
	}{
		{name: "the root is handed over after the queueing",
			after:  &record.HandedOverRoot{Issue: "LEGION-1", Generation: 1},
			queued: record.CatchUp{}, delivered: record.CatchUp{HandedOver: true}},
		{name: "the handed-over root's gate is carried after the queueing",
			before: &record.HandedOverRoot{Issue: "LEGION-1", Generation: 1},
			after:  &record.HandedOverRoot{Issue: "LEGION-1", Generation: 1, GateCarried: true},
			queued: record.CatchUp{HandedOver: true}, delivered: record.CatchUp{HandedOver: true, Carried: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newCatchUpDelivery(t)
			handOver := func(root *record.HandedOverRoot) {
				if root == nil {
					return
				}
				d.write(t, "record the hand-over of LEGION-1", func(ctx context.Context, tx pgx.Tx) error {
					return d.records.PutHandedOverRoot(ctx, tx, *root)
				})
			}
			handOver(tc.before)
			keptCatchUp(t, d.pool, d.records, "LEGION-1", 1, d.now)
			handOver(tc.after)

			queued, published := d.deliver(t)
			if queued.CatchUp.HandedOver != tc.queued.HandedOver || queued.CatchUp.Carried != tc.queued.Carried {
				t.Fatalf("the queued catch-up states handed over %t, carried %t, want %t, %t: the row holds the record of its queueing",
					queued.CatchUp.HandedOver, queued.CatchUp.Carried, tc.queued.HandedOver, tc.queued.Carried)
			}
			if len(published) != 1 {
				t.Fatalf("the delivery published %d notices, want the one catch-up", len(published))
			}
			got := published[0].CatchUp
			if got.HandedOver != tc.delivered.HandedOver || got.Carried != tc.delivered.Carried {
				t.Fatalf("the delivered catch-up states handed over %t, carried %t, want %t, %t: the architect is told the hand-over "+
					"the record holds when the notice is delivered, not the one it held when the row was queued",
					got.HandedOver, got.Carried, tc.delivered.HandedOver, tc.delivered.Carried)
			}
		})
	}
}

// A catch-up whose root advanced a generation between its queueing and its delivery is superseded
// (catchUpSuperseded): the re-admission's start writes the fresh one. The fence reads the row's
// generation, as the row holds it, against the root's now, before the delivery composes the tree
// again, so the recomposition, which states the root's generation now, cannot hide the advance.
// Nothing is published.
func TestACatchUpWhoseRootAdvancedAGenerationBeforeItsDeliveryIsStillSuperseded(t *testing.T) {
	d := newCatchUpDelivery(t)
	keptCatchUp(t, d.pool, d.records, "LEGION-1", 1, d.now)
	d.write(t, "re-admit the root at generation 2", func(ctx context.Context, tx pgx.Tx) error {
		stored, err := d.records.Issue(ctx, tx, "LEGION-1")
		if err != nil {
			return err
		}
		stored.Generation = 2
		return d.records.PutIssue(ctx, tx, *stored)
	})

	queued, published := d.deliver(t)
	if queued.CatchUp.Generation != 1 {
		t.Fatalf("the queued catch-up is of generation %d, want 1", queued.CatchUp.Generation)
	}
	if len(published) != 0 {
		t.Fatalf("the delivery published %+v, want nothing: a newer catch-up supersedes the one queued at generation 1", published)
	}
}

// The decisions a catch-up's queueing named are the ones its delivery tells and settles. The
// queueing claimed them for its row (workflow.EnqueueNaming), and the publish forgets exactly what
// the row names (settleTold), so the delivery composes the tree again and leaves Decided and Kept
// as the row holds them. A decision kept after the queueing is no part of this telling: the
// catch-up does not name it, and it stays kept, unclaimed, for the architect's next start or
// catch-up to name.
func TestACatchUpPublishesAndSettlesTheKeptDecisionsItNamedWhenItWasQueued(t *testing.T) {
	d := newCatchUpDelivery(t)
	named, later := "dispatch://LEGION-1/ask/ask-named", "dispatch://LEGION-1/ask/ask-later"
	keepDecision(t, d.pool, d.records, "LEGION-1", named, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	keptCatchUp(t, d.pool, d.records, "LEGION-1", 1, d.now)
	keepDecision(t, d.pool, d.records, "LEGION-1", later, time.Date(2026, 10, 24, 9, 5, 0, 0, time.UTC))

	queued, published := d.deliver(t)
	if queued.CatchUp.Kept == nil || !strings.Contains(queued.CatchUp.Decided, named) || strings.Contains(queued.CatchUp.Decided, later) {
		t.Fatalf("the queued catch-up names %q (kept %+v), want it to name %s alone", queued.CatchUp.Decided, queued.CatchUp.Kept, named)
	}
	if len(published) != 1 {
		t.Fatalf("the delivery published %d notices, want the one catch-up", len(published))
	}
	delivered := published[0].CatchUp
	if delivered.Decided != queued.CatchUp.Decided || !reflect.DeepEqual(delivered.Kept, queued.CatchUp.Kept) {
		t.Fatalf("the delivered catch-up names %q (kept %+v), want what its row named, %q (kept %+v): the row's queueing claimed those decisions",
			delivered.Decided, delivered.Kept, queued.CatchUp.Decided, queued.CatchUp.Kept)
	}
	if delivered.Launch != queued.CatchUp.Launch || delivered.Generation != queued.CatchUp.Generation {
		t.Fatalf("the delivered catch-up is of launch %d at generation %d, want the row's, launch %d at generation %d",
			delivered.Launch, delivered.Generation, queued.CatchUp.Launch, queued.CatchUp.Generation)
	}
	left := keptDecisions(t, d.pool, d.records, "LEGION-1")
	if len(left) != 1 || left[0].Ref != later || left[0].ClaimedBy != 0 {
		t.Fatalf("kept after the publish = %s, want %s alone, unclaimed: the publish forgets what its row named and nothing else", keptRefs(left), later)
	}
}
