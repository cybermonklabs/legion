package admit

//ci:reads packages/daemon/internal/intake/testdata/dispatch/issue-updated.json TestCapturedDispatchTodoEventAdmitsAndProjectsActiveSlot reads the captured event through ../intake/testdata

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

var _ intake.Handler = (*Admission)(nil)

// promotedStatus is the status write a root's promotion queues: in_progress over the todo it
// waited at, read without its fence as effects reads every row.
var promotedStatus = record.OrdinaryStatusWrite("in_progress", "todo", "")

func TestApplyFactAdmitsRootAndQueuesWhenFull(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	apply(t, pool, admission, "root-todo", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "Admission", Rank: "B", HandedOver: handed}, engineStub{})
	root := issue(t, pool, "LEGION-208")
	if root.Phase != phase.Admitted || root.Status != "in_progress" || root.Tree != root.Key {
		t.Fatalf("admitted root = %#v, want admitted root with truthful in_progress status and its own tree", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-208", Index: 0, AdmittedAt: fixedNow}})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-208", payload: promotedStatus},
		{kind: record.OutboxKindIssueBranch, issue: "LEGION-208", payload: record.IssueBranch{Generation: 1}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-208", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 1,
			Task: architectStart(t, pool, admission, "LEGION-208", ""), Entry: 1, Decided: true}},
	})

	apply(t, pool, admission, "queued-todo", intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "Queued", Rank: "A", HandedOver: handed}, engineStub{})
	queued := issue(t, pool, "LEGION-209")
	if queued.Status != "todo" || queued.Tree != queued.Key {
		t.Fatalf("queued root = %#v, want slotless root with its observed todo status", queued)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-208", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, []string{"LEGION-209"})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-208", payload: promotedStatus},
		{kind: record.OutboxKindIssueBranch, issue: "LEGION-208", payload: record.IssueBranch{Generation: 1}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-208", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 1,
			Task: architectStart(t, pool, admission, "LEGION-208", ""), Entry: 1, Decided: true}},
	})
}

func TestApplyFactPromotesWaitingRootsInRankOrder(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE-1", "A")
	seedSlotted(t, pool, "LEGION-ACTIVE-2", "B")

	for _, event := range []intake.DispatchIssue{
		{Key: "LEGION-C", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank C", Rank: "C", HandedOver: handed},
		{Key: "LEGION-A", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank A", Rank: "A", HandedOver: handed},
		{Key: "LEGION-B", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank B", Rank: "B", HandedOver: handed},
	} {
		apply(t, pool, admission, "arrive-"+event.Key, event, engineStub{})
	}

	apply(t, pool, admission, "leave-first", intake.DispatchIssue{Key: "LEGION-ACTIVE-1", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "done", Title: "active", Rank: "A"}, engineStub{})
	assertSlots(t, pool, []record.Slot{
		{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow},
		{Issue: "LEGION-ACTIVE-2", Index: 1, AdmittedAt: fixedNow},
	})

	apply(t, pool, admission, "leave-second", intake.DispatchIssue{Key: "LEGION-ACTIVE-2", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "done", Title: "active", Rank: "B"}, engineStub{})
	assertSlots(t, pool, []record.Slot{
		{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow},
		{Issue: "LEGION-B", Index: 1, AdmittedAt: fixedNow},
	})
	assertWaiting(t, pool, []string{"LEGION-C"})
}

// A workflow root whose own lifecycle the operator holds (an operator spawn under its key, R22 of
// dispatch://INFRA-3320) cannot be admitted while that lifecycle is open. That is the one root's
// wait: the fact goes on and admits the next root in rank order, and intake keeps flowing.
func TestAnOperatorTreeHoldsOnlyItsOwnWorkflowRootsAdmission(t *testing.T) {
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(&logged, nil)))
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := record.NewStore().OpenIssueLifecycle(context.Background(), tx, "legion", "LEGION-OPS", treelifecycle.AuthorityOperator); err != nil {
			t.Fatal(err)
		}
	})
	apply(t, pool, admission, "arrive-ops", intake.DispatchIssue{Key: "LEGION-OPS", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "held by the operator", Rank: "A", HandedOver: handed}, engineStub{})
	apply(t, pool, admission, "arrive-b", intake.DispatchIssue{Key: "LEGION-B", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank B", Rank: "B", HandedOver: handed}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-B", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, []string{"LEGION-OPS"})
	if !strings.Contains(logged.String(), "admission waits for the issue's other authority") || !strings.Contains(logged.String(), "issue=LEGION-OPS") {
		t.Fatalf("log %q, want the held root's wait named", logged.String())
	}
}

func TestApplyFactLeavesEngineRecordedChildWithoutSlotOrEffects(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	parent := "LEGION-ROOT"
	child := record.Issue{Key: "LEGION-CHILD", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Title: "child", Parent: &parent, Tree: "LEGION-ROOT", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "B", LastDispatchSeq: 1}

	apply(t, pool, admission, "child-todo", intake.DispatchIssue{Key: child.Key, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: child.Title, Parent: parent, Rank: child.Rank}, engineStub{store: record.NewStore(), recordChild: &child})
	if got := issue(t, pool, child.Key); !reflect.DeepEqual(withoutMovement(got), child) {
		t.Fatalf("child record = %#v, want engine-owned child %#v", got, child)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, nil)
	assertEffects(t, pool, nil)
}

func TestApplyFactDropsWaitingIssueWhenHumanMovesItOutOfTodo(t *testing.T) {
	for _, status := range []string{"triage", "icebox", "backlog", "done"} {
		t.Run(status, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			seedWaiting(t, pool, "LEGION-WAITING", "A")

			apply(t, pool, admission, "leave-"+status, intake.DispatchIssue{Key: "LEGION-WAITING", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: status, Title: "waiting", Rank: "A"}, engineStub{})
			if got := issue(t, pool, "LEGION-WAITING"); got.Status != status {
				t.Fatalf("status = %q, want %q", got.Status, status)
			}
			assertSlots(t, pool, nil)
			assertWaiting(t, pool, nil)
		})
	}
}

func TestApplyFactReleasesSlotWhenEngineCompletesPhaseAndPromotesHead(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")

	apply(t, pool, admission, "phase-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "in_progress", Title: "active", Rank: "A"}, engineStub{store: record.NewStore(), done: "LEGION-ACTIVE"})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
	if got := issue(t, pool, "LEGION-NEXT"); got.Status != "in_progress" {
		t.Fatalf("promoted status = %q, want in_progress", got.Status)
	}
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-NEXT", payload: promotedStatus},
		{kind: record.OutboxKindIssueBranch, issue: "LEGION-NEXT", payload: record.IssueBranch{Generation: 1}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-NEXT", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-NEXT", Role: claim.RoleArchitect, Generation: 1,
			Task: architectStart(t, pool, admission, "LEGION-NEXT", ""), Entry: 1, Decided: true}},
	})
}

func TestApplyFactReleasesSlotWhenDispatchLeavesActiveSetAndPromotesHead(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")

	apply(t, pool, admission, "status-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "done", Title: "active", Rank: "A"}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
	if got := issue(t, pool, "LEGION-ACTIVE"); got.Status != "done" {
		t.Fatalf("released issue status = %q, want done", got.Status)
	}
}

// superviseOps counts the supervise rows of op queued for issue.
func superviseOps(t *testing.T, pool *pgxpool.Pool, issue, op string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = $2`, issue, op).Scan(&count); err != nil {
		t.Fatalf("count %s rows of %s: %v", op, issue, err)
	}
	return count
}

// Every newer Dispatch observation is recorded, not only a status change: re-ranking a waiting
// root, renaming it, or re-parenting it arrives as an issue.updated at the same status, and the
// waiting line has to follow Dispatch rank order at once, not after the next boot's read. It runs
// through the real engine, which sees every fact before admission. The new parent stands on the
// backlog, out of Legion's work, so the root stays a root on a chain the copy holds (R15 of
// dispatch://INFRA-3320).
func TestApplyFactRecordsRankTitleAndParentChangesAtTheSameStatus(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Clock: func() time.Time { return fixedNow }}, nil)
	seedSlotted(t, pool, "LEGION-1", "A")
	seedWaiting(t, pool, "LEGION-2", "B")
	seedWaiting(t, pool, "LEGION-3", "C")
	seedMirrorChild(t, pool, "LEGION-9", "backlog", "")

	apply(t, pool, admission, "rerank", intake.DispatchIssue{Key: "LEGION-3", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: "renamed", Parent: "LEGION-9", Rank: "AB", HandedOver: handed}, engine)
	got := issue(t, pool, "LEGION-3")
	if got.Rank != "AB" || got.Title != "renamed" || got.Parent == nil || *got.Parent != "LEGION-9" || got.LastDispatchSeq != 2 {
		t.Fatalf("observed record = %#v, want rank AB, title renamed, parent LEGION-9, seq 2", got)
	}
	assertWaiting(t, pool, []string{"LEGION-3", "LEGION-2"})
}

// A title is a person's own text, so it can hold a NUL, which the store keeps as U+FFFD. The boot
// listing shows the title as Dispatch holds it, at the sequence the record has already applied, and
// the record keeps what the store made of it: the two are the same title, so the listing writes
// nothing, as it writes nothing of any title the record already holds. Compared as listed, the
// title would differ from its own stored copy, and the record would be written again at every boot.
func TestTheBootListingOfATitleHoldingNULWritesNothingTheRecordAlreadyHolds(t *testing.T) {
	pool := migratedPool(t)
	facts := store.NewPool(pool)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Clock: func() time.Time { return fixedNow }}, nil)
	seedSlotted(t, pool, "LEGION-1", "A")
	seedWaiting(t, pool, "LEGION-3", "C")
	const title = "re\x00named"
	ctx := context.Background()

	if _, err := intake.ApplyFact(ctx, facts, "dispatch", "rename", intake.DispatchIssue{Key: "LEGION-3", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: title, Rank: "C", HandedOver: handed}, engine, admission); err != nil {
		t.Fatalf("apply the rename: %v", err)
	}
	if got := issue(t, pool, "LEGION-3"); got.Title != "re\uFFFDnamed" || got.LastDispatchSeq != 2 {
		t.Fatalf("the renamed record is titled %q at seq %d, want %q at 2", got.Title, got.LastDispatchSeq, "re\uFFFDnamed")
	}
	// xmin is the transaction that wrote the row's current version: every write of the row,
	// whatever it changes, gives it another.
	written := func() string {
		t.Helper()
		var xmin string
		if err := pool.QueryRow(ctx, `select xmin::text from issues where key = $1`, "LEGION-3").Scan(&xmin); err != nil {
			t.Fatalf("read the record's xmin: %v", err)
		}
		return xmin
	}
	before := written()

	listed := []intake.DispatchIssue{intake.ListedIssue(dispatch.IssueSummary{Key: "LEGION-3", Title: title, Status: "todo", Rank: "C", HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets"})}
	if err := store.Transact(ctx, facts, func(ctx context.Context, tx pgx.Tx) error {
		_, err := admission.Reconcile(ctx, tx, listed, 2, intake.DispatchConsumerPosition{AckFloorStream: 2, Idle: true})
		return err
	}); err != nil {
		t.Fatalf("reconcile the boot listing: %v", err)
	}
	if after := written(); after != before {
		t.Fatalf("the boot listing of the title the record holds wrote the record again (xmin %s, then %s), want no write", before, after)
	}
	if got := issue(t, pool, "LEGION-3"); got.Title != "re\uFFFDnamed" || got.LastDispatchSeq != 2 {
		t.Fatalf("the record after the boot listing is titled %q at seq %d, want %q at 2", got.Title, got.LastDispatchSeq, "re\uFFFDnamed")
	}
}

// The boot read opens a new run of a child the daemon closed that a person set back to todo while
// the daemon was down, as the live event does (decision 8 of dispatch://INFRA-4730): a new
// generation at admitted, under its parent on Dispatch's chain. Its tree is written once and stays,
// it is no root, and its first phase waits for the gate above it, so it takes no slot. The listing
// is newer than the record, and the Dispatch consumer has caught up, so the summary applies now
// through the engine, as its event would.
func TestReconcileNeverReadmitsAChildAsARoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-1", "A")
	parent := "LEGION-1"
	putIssue(t, pool, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: testProject, Title: "child", Parent: &parent, Phase: phase.Done, Generation: 1, Status: "done", Rank: "B", LastDispatchSeq: 1})

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-1", ResolvedRepo: "acme/widgets", Title: "LEGION-1", Status: "in_progress", Rank: "A"},
		{Key: "LEGION-2", ResolvedRepo: "acme/widgets", Title: "child", Status: "todo", Parent: &parent, Rank: "B", LastSeq: 2},
	}, 2, 2, true)
	if got := issue(t, pool, "LEGION-2"); got.Tree != "LEGION-1" || got.Generation != 2 || got.Phase != phase.Admitted || got.HoldsGate {
		t.Fatalf("reconciled child = %#v, want a new run at generation 2, admitted, under LEGION-1 and holding no gate of its own", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow}})
}

// The boot read opens a new run of a root the daemon closed that a person set back to todo while
// the daemon was down, exactly as the live event does: a new generation at admitted, promoted.
func TestTheBootReadOpensANewRunOfAClosedRootSetBackToTodo(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	putIssue(t, pool, record.Issue{Key: "LEGION-CLOSED", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Title: "closed", Tree: "LEGION-CLOSED", Phase: phase.Done, Generation: 3, Status: "done", Rank: "A", HoldsGate: true, LastDispatchSeq: 1})

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-CLOSED", ResolvedRepo: "acme/widgets", Title: "closed", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 2}}, 2, 2, true)
	again := issue(t, pool, "LEGION-CLOSED")
	if again.Generation != 4 || again.Phase != phase.Admitted || again.Status != "in_progress" {
		t.Fatalf("reconciled root = %#v, want a new run at generation 4, admitted and promoted", again)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-CLOSED", Index: 0, AdmittedAt: fixedNow}})
}

func TestReconcileFillsRaisedCapInRankOrderAndIsIdempotent(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedWaiting(t, pool, "LEGION-B", "B")
	seedWaiting(t, pool, "LEGION-A", "A")
	seedWaiting(t, pool, "LEGION-C", "C")
	admission.cap = 2
	summaries := []dispatch.IssueSummary{
		{Key: "LEGION-C", ResolvedRepo: "acme/widgets", Title: "C", Status: "todo", Rank: "C", HandedOver: handed},
		{Key: "LEGION-B", ResolvedRepo: "acme/widgets", Title: "B", Status: "todo", Rank: "B", HandedOver: handed},
		{Key: "LEGION-A", ResolvedRepo: "acme/widgets", Title: "A", Status: "todo", Rank: "A", HandedOver: handed},
	}

	reconcile(t, pool, admission, summaries)
	assertSlots(t, pool, []record.Slot{
		{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow},
		{Issue: "LEGION-B", Index: 1, AdmittedAt: fixedNow},
	})
	assertWaiting(t, pool, []string{"LEGION-C"})
	firstEffects := effects(t, pool)
	if len(firstEffects) != 6 {
		t.Fatalf("effects after raised-cap reconcile = %#v, want two status, two issue_branch and two supervise rows", firstEffects)
	}

	reconcile(t, pool, admission, summaries)
	if got := effects(t, pool); !reflect.DeepEqual(got, firstEffects) {
		t.Fatalf("second reconcile effects = %#v, want unchanged %#v", got, firstEffects)
	}
}

// A status change applies through Reconcile only when the summary's own sequence is genuinely
// newer than the record's, and the consumer has caught up to it — otherwise it is either deferred
// (behind) or, level with what is already recorded, left alone (see hold_test.go's
// TestReconcileLeavesAnIssueTheStreamHoldsNewerEventsFor and applySummary).
func TestReconcileReleasesSlotWhoseDispatchStatusLeftActiveSet(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Title: "active", Status: "done", Rank: "A", LastSeq: 1},
	}, 1, 1, true)
	assertSlots(t, pool, nil)
	if got := issue(t, pool, "LEGION-ACTIVE"); got.Status != "done" {
		t.Fatalf("reconciled status = %q, want done", got.Status)
	}
}

func TestCapturedDispatchTodoEventAdmitsAndProjectsActiveSlot(t *testing.T) {
	pool := migratedPool(t)
	// The admission runs for CAPTURE, the project the captured event names, so the issue is
	// recorded under it and holds one of CAPTURE's own slots: the state's admission.active lists
	// the project's own slot holders alone (blockers.JudgeAdmission).
	admission := newAdmissionForProject(t, "CAPTURE", 1, config.DesignGate("").Steps(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	js := testJetStream(t)
	consumers, err := intake.OpenConsumers(context.Background(), js, pool, intake.ConsumerSpec{Project: "CAPTURE", Repositories: []ghrepo.Repository{ghrepo.MustParse("acme/widgets")}, AckWait: time.Second, NakDelay: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// The daemon's own handler order: the copy first, so admission reads the captured issue's
	// chain from the row this very event writes (R16 of dispatch://INFRA-3320).
	go func() {
		done <- consumers.Run(ctx, blockers.NewMirror(record.NewStore(), "CAPTURE"), engineStub{}, admission)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	captured, err := os.ReadFile("../intake/testdata/dispatch/issue-updated.json")
	if err != nil {
		t.Fatalf("read captured Dispatch issue.updated event: %v", err)
	}
	// The captured issue carries no labels; this one is handed to Legion.
	labeled := strings.Replace(string(captured), `\"labels\":[]`, `\"labels\":[\"legion\"]`, 1)
	if labeled == string(captured) {
		t.Fatal("the captured event has no empty labels to hand the issue over with")
	}
	if _, err := js.Publish(context.Background(), "notifications.dispatch.issue.CAPTURE-3.issue.updated", []byte(labeled)); err != nil {
		t.Fatalf("publish captured Dispatch event: %v", err)
	}
	testwait.Eventually(t, "captured event admission", func() bool {
		tx, err := pool.Begin(context.Background())
		if err != nil {
			return false
		}
		defer tx.Rollback(context.Background())
		state, err := projection.Project(context.Background(), projection.Inputs{
			Tx: tx, Store: record.NewStore(), Project: "CAPTURE",
			Now: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			return false
		}
		return reflect.DeepEqual(state.Admission.Active, []string{"CAPTURE-3"}) && state.Issues["CAPTURE-3"].Slot != nil
	})
}
