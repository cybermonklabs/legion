package daemon

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// These tests hold the reply to one transaction's COMMIT, or cut its connection, on the way
// between the daemon and a real PostgreSQL (testdb.CommitFaults), and read what the daemon did
// with the work that transaction's commit runs: admission's slot line, the lane wake of a
// released notice, the clearing of a held root. A write whose commit landed keeps that work,
// whether its reply came after its caller's cancel or the store could say only later that it
// committed; a write whose commit never landed runs none of it, and its retry does.

// faultMatch is the SQL every transaction that serializes with the facts sends (intake's
// SerializeFacts): an admission reconcile's and a fact's.
const faultMatch = "legion.apply_fact"

// noticesReleasedMessage is what a release of the notices waiting for a ready architect logs
// once its transaction has committed (outbox.releaseWaiting).
const noticesReleasedMessage = "outbox notices released: their architect is ready"

// faultedPools is one cloned database reached two ways: direct, which seeds and observes, and
// faulted, through a proxy whose armed fault holds or cuts one transaction's commit. Both are
// the daemon's guarded pool (store.Pool), closed before the proxy and the clone at the test's end.
func faultedPools(t *testing.T) (direct, faulted *store.Pool, fault *testdb.CommitFault) {
	t.Helper()
	dsn := testpg.Clone(t, "legion_commit_outcome_test")
	proxied, fault := testdb.CommitFaults(t, dsn)
	return openGuardedPool(t, dsn), openGuardedPool(t, proxied), fault
}

// openGuardedPool is the daemon's guarded pool on dsn, closed when the test ends.
func openGuardedPool(t *testing.T, dsn string) *store.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open a pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.NewPool(pool)
}

// cancelWhileCommitting cancels a write that is still waiting for its commit's reply, and returns
// what the write returned. A write that returned before the cancel fails the test: its reply came
// first, so the cancel tested nothing.
func cancelWhileCommitting(t *testing.T, done <-chan error, cancel context.CancelFunc) error {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the write returned %v before the cancel: its commit's reply came first, so the cancel tested nothing", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("the write did not return after its cancel")
		return nil
	}
}

// reconcileRoot is the root the reconcile tests list: recorded at admitted with no slot, and
// listed level with its record, so the reconcile's admission pass gives it the one free slot.
const reconcileRoot = "CAPTURE-1"

// reconcileRig is the workflow runtime of the reconcile tests on the faulted pool, the listing it
// reconciles, and where its log lines go.
type reconcileRig struct {
	direct  *store.Pool
	fault   *testdb.CommitFault
	logged  *syncBuffer
	w       *workflowRuntime
	listing []dispatch.IssueSummary
}

func newReconcileRig(t *testing.T) *reconcileRig {
	t.Helper()
	direct, faulted, fault := faultedPools(t)
	logged, log := watchedLog()
	listing := []dispatch.IssueSummary{{Key: reconcileRoot, ResolvedRepo: "acme/widgets", Title: reconcileRoot, Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1}}
	client := &bootClaimDispatch{outboxDispatch: &outboxDispatch{}, shown: map[string]dispatch.Issue{}, listing: listing}
	w := catchUpRigOn(t, faulted, client, 1, log)
	w.consumers = bootClaimConsumers(t, direct)
	w.repositories = newRepositories("CAPTURE", ghrepo.Renames{})
	putOutboxIssue(t, direct, record.NewStore(), record.Issue{Key: reconcileRoot, Tree: reconcileRoot, Project: "CAPTURE", Title: reconcileRoot,
		Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", LastDispatchSeq: 1})
	return &reconcileRig{direct: direct, fault: fault, logged: logged, w: w, listing: listing}
}

// reconcile reconciles the rig's listing as a recovery does (recoverRepositories): outside the
// boot, judging the summaries its keep keeps, here every one.
func (r *reconcileRig) reconcile(ctx context.Context) error {
	return r.w.reconcileListing(ctx, r.listing, func(map[string]*record.Issue, dispatch.IssueSummary) bool { return true }, false)
}

// A reconcile whose commit's reply arrives after its caller's cancel committed: the root holds
// its slot, so the slot's line is in the log, and the reconcile answers nil. A retry finds the
// slot taken and takes no other.
func TestAReconcileWhoseCommitReplyComesAfterTheCancelLogsTheSlotItTook(t *testing.T) {
	rig := newReconcileRig(t)
	fired := rig.fault.HoldReply(faultMatch, 300*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rig.reconcile(ctx) }()
	testdb.Met(t, fired)
	testwait.Eventually(t, "the reconcile's slot to be visible while its commit's reply is held", func() bool {
		return slices.Contains(tickSlotted(t, rig.direct), reconcileRoot)
	})
	if err := cancelWhileCommitting(t, done, cancel); err != nil {
		t.Errorf("the reconcile whose commit landed = %v, want nil", err)
	}
	if lines := releaseLogLines(rig.logged, slotTakenMessage); len(lines) != 1 || !strings.Contains(lines[0], "issue="+reconcileRoot) {
		t.Errorf("slot taken lines after the reconcile = %q, want one naming %s: its commit landed", lines, reconcileRoot)
	}
	slotted := tickSlotted(t, rig.direct)

	if err := rig.reconcile(context.Background()); err != nil {
		t.Fatalf("the retried reconcile = %v, want nil", err)
	}
	if lines := releaseLogLines(rig.logged, slotTakenMessage); len(lines) != 1 {
		t.Errorf("slot taken lines after the retry = %q, want still the one: the retry took no slot", lines)
	}
	if after := tickSlotted(t, rig.direct); !slices.Equal(after, slotted) {
		t.Errorf("slots after the retry = %v, want %v unchanged", after, slotted)
	}
}

// A reconcile whose connection is cut before its COMMIT reaches the server committed nothing: no
// slot and no slot line. Its retry takes the slot and logs it once.
func TestAReconcileCutBeforeItsCommitTakesNoSlotUntilItsRetry(t *testing.T) {
	rig := newReconcileRig(t)
	fired := rig.fault.Cut(faultMatch, testdb.CutBeforeCommit)
	err := rig.reconcile(context.Background())
	testdb.Met(t, fired)
	if err == nil {
		t.Fatal("the reconcile whose commit never reached the server = nil, want an error")
	}
	if slotted := tickSlotted(t, rig.direct); len(slotted) != 0 {
		t.Errorf("slots after the cut reconcile = %v, want none", slotted)
	}
	if lines := releaseLogLines(rig.logged, slotTakenMessage); len(lines) != 0 {
		t.Errorf("slot taken lines after the cut reconcile = %q, want none: nothing committed", lines)
	}

	if err := rig.reconcile(context.Background()); err != nil {
		t.Fatalf("the retried reconcile = %v, want nil", err)
	}
	if slotted := tickSlotted(t, rig.direct); !slices.Equal(slotted, []string{reconcileRoot}) {
		t.Errorf("slots after the retry = %v, want [%s]", slotted, reconcileRoot)
	}
	if lines := releaseLogLines(rig.logged, slotTakenMessage); len(lines) != 1 || !strings.Contains(lines[0], "issue="+reconcileRoot) {
		t.Errorf("slot taken lines after the retry = %q, want one naming %s", lines, reconcileRoot)
	}
}

// laneWakes records the issue of each lane wake a transaction's commit hooks make.
type laneWakes struct {
	mu     sync.Mutex
	issues []string
}

func (l *laneWakes) wake(issue string, _ record.OutboxKind) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.issues = append(l.issues, issue)
}

// sorted is each woken issue, sorted.
func (l *laneWakes) sorted() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Sorted(slices.Values(l.issues))
}

// releasedLanes is the lane wakes the notice tests' one release makes: the root's two notices and
// another tree's root's one, while the sub-architect's notice keeps its time.
var releasedLanes = []string{"LEGION-1", "LEGION-1", "LEGION-9"}

// noticeRig is a runner on the faulted pool whose records wake lanes, over the notices of
// TestAReadyArchitectReleasesOnlyItsOwnNotices, seeded through the direct pool.
type noticeRig struct {
	direct *store.Pool
	fault  *testdb.CommitFault
	logged *syncBuffer
	wakes  *laneWakes
	runner *outbox
	now    time.Time
}

func newNoticeRig(t *testing.T) *noticeRig {
	t.Helper()
	direct, faulted, fault := faultedPools(t)
	records := record.NewStore()
	noticeTree(t, direct, records, false)
	putOutboxIssue(t, direct, records, record.Issue{Key: "LEGION-9", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-9", Title: "Another root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaimOn(t, sup, "LEGION-1", supervise.StateRegistered, "ses_arch")
	architectClaimOn(t, sup, "LEGION-2", supervise.StateWorking, "ses_sub")
	architectClaimOn(t, sup, "LEGION-9", supervise.StateReady, "ses_other")
	now := time.Now()
	later := now.Add(noticeReholdDelays[1])
	for _, c := range []struct {
		issue   string
		resends int
	}{
		{"LEGION-1", 1}, // a re-held copy of the root's
		{"LEGION-1", 0}, // the root's, held on backoff
		{"LEGION-3", 1}, // the sub-architect's: LEGION-3 is under LEGION-2, whose sub-architect runs
		{"LEGION-9", 1}, // another tree's root's
	} {
		enqueueOutbox(t, direct, records, mustOutboxRow(t, c.issue, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Resends: c.resends}, later))
	}
	logged, log := watchedLog()
	wakes := &laneWakes{}
	runner := &outbox{log: log, pool: faulted, dispatchProject: "LEGION", supervisor: sup, project: "legion", now: func() time.Time { return now },
		records: projectRecords{Store: record.NewStore(), project: "LEGION", wake: wakes.wake, log: log}}
	return &noticeRig{direct: direct, fault: fault, logged: logged, wakes: wakes, runner: runner, now: now}
}

// release releases the notices waiting for LEGION-1's and LEGION-9's architects.
func (r *noticeRig) release(t *testing.T, ctx context.Context) error {
	t.Helper()
	return r.runner.releaseWaiting(ctx, architectTopicToken(t, "LEGION-1"), architectTopicToken(t, "LEGION-9"))
}

// due is how many notices the direct pool reads as due at the rig's moment.
func (r *noticeRig) due(t *testing.T) int {
	t.Helper()
	var due int
	if err := r.direct.QueryRow(context.Background(), "select count(*) from outbox where next_at <= $1", r.now).Scan(&due); err != nil {
		t.Fatalf("count the due notices: %v", err)
	}
	return due
}

// A release whose commit's reply arrives after its caller's cancel committed: each released
// notice's lane is woken, the release's line is in the log, and the release answers nil. A retry
// finds nothing waiting and wakes nothing more.
func TestANoticeReleaseWhoseCommitReplyComesAfterTheCancelWakesItsLanes(t *testing.T) {
	rig := newNoticeRig(t)
	fired := rig.fault.HoldReply("update outbox set next_at", 300*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rig.release(t, ctx) }()
	testdb.Met(t, fired)
	testwait.Eventually(t, "the released notices to be due while the release's commit reply is held", func() bool {
		return rig.due(t) == len(releasedLanes)
	})
	if err := cancelWhileCommitting(t, done, cancel); err != nil {
		t.Errorf("the release whose commit landed = %v, want nil", err)
	}
	if woken := rig.wakes.sorted(); !slices.Equal(woken, releasedLanes) {
		t.Errorf("lanes woken after the release = %v, want %v: its commit landed", woken, releasedLanes)
	}
	if lines := releaseLogLines(rig.logged, noticesReleasedMessage); len(lines) != 1 {
		t.Errorf("release lines after the release = %q, want one: its commit landed", lines)
	}

	if err := rig.release(t, context.Background()); err != nil {
		t.Fatalf("the retried release = %v, want nil", err)
	}
	if woken := rig.wakes.sorted(); !slices.Equal(woken, releasedLanes) {
		t.Errorf("lanes woken after the retry = %v, want still %v: the retry released nothing", woken, releasedLanes)
	}
	if lines := releaseLogLines(rig.logged, noticesReleasedMessage); len(lines) != 1 {
		t.Errorf("release lines after the retry = %q, want still the one", lines)
	}
}

// A release whose connection is cut before its COMMIT reaches the server committed nothing: no
// notice is due, no lane is woken and no line is logged. Its retry releases them.
func TestANoticeReleaseCutBeforeItsCommitWakesNothingUntilItsRetry(t *testing.T) {
	rig := newNoticeRig(t)
	fired := rig.fault.Cut("update outbox set next_at", testdb.CutBeforeCommit)
	err := rig.release(t, context.Background())
	testdb.Met(t, fired)
	if err == nil {
		t.Fatal("the release whose commit never reached the server = nil, want an error")
	}
	if due := rig.due(t); due != 0 {
		t.Errorf("due notices after the cut release = %d, want none", due)
	}
	if woken := rig.wakes.sorted(); len(woken) != 0 {
		t.Errorf("lanes woken after the cut release = %v, want none: nothing committed", woken)
	}
	if lines := releaseLogLines(rig.logged, noticesReleasedMessage); len(lines) != 0 {
		t.Errorf("release lines after the cut release = %q, want none", lines)
	}

	if err := rig.release(t, context.Background()); err != nil {
		t.Fatalf("the retried release = %v, want nil", err)
	}
	if due := rig.due(t); due != len(releasedLanes) {
		t.Errorf("due notices after the retry = %d, want %d", due, len(releasedLanes))
	}
	if woken := rig.wakes.sorted(); !slices.Equal(woken, releasedLanes) {
		t.Errorf("lanes woken after the retry = %v, want %v", woken, releasedLanes)
	}
	if lines := releaseLogLines(rig.logged, noticesReleasedMessage); len(lines) != 1 {
		t.Errorf("release lines after the retry = %q, want one", lines)
	}
}

// A release of admission's held roots whose commit landed while the store could not be reached
// afterwards: the fact fails, and so does its retry while the store refuses every connection.
// Once the store answers again, the retry is a duplicate, since the release committed, and the
// hold clears, with the one slot line of the root the release admitted.
func TestAHeldReleaseWhoseCommitOutcomeIsUnknownReleasesOnceTheStoreAnswers(t *testing.T) {
	direct, faulted, fault := faultedPools(t)
	logged, log := watchedLog()
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	if err := pgx.BeginFunc(context.Background(), direct, func(tx pgx.Tx) error {
		for _, seed := range []struct{ key, rank string }{{"LEGION-HELD", "A"}, {"LEGION-NEXT", "B"}} {
			if err := record.NewStore().PutIssue(context.Background(), tx, record.Issue{
				Key: seed.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Project: "CAPTURE", Title: seed.key, Tree: seed.key, Phase: phase.Admitted,
				Generation: 1, Status: "todo", Rank: seed.rank, HandedOver: true, LastDispatchSeq: 1,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed roots: %v", err)
	}
	// LEGION-HELD's summary stands ahead of its record, so the reconcile holds it; the target is
	// past anything the stream's ack floor reaches, so only an idle reading releases it.
	reconcileAdmission(t, direct, admission, []dispatch.IssueSummary{
		{Key: "LEGION-HELD", ResolvedRepo: "sjawhar/legion", Title: "LEGION-HELD", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 2},
		{Key: "LEGION-NEXT", ResolvedRepo: "sjawhar/legion", Title: "LEGION-NEXT", Status: "todo", Rank: "B", HandedOver: true, LastSeq: 1},
	}, 1000, intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: false})
	if !admission.Held() {
		t.Fatal("Held() = false after the reconcile read a record behind Dispatch, want it held")
	}

	// The id the hold-release poll gives this reading (pollHoldReleaseWith), so each retry below
	// is the poll's own retry of the same reading.
	const id = "dispatch-position:CAPTURE:test-boot:1:1000:0:true"
	idle := intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: true}
	fired := fault.Cut(faultMatch, testdb.CutAfterCommitRefusing)
	_, err := intake.ApplyFact(context.Background(), faulted, "dispatch", id, idle, engine, admission)
	testdb.Met(t, fired)
	if err == nil {
		t.Fatal("the release whose commit's reply was lost = nil, want an error: the store could not say whether it committed")
	}
	if _, err := intake.ApplyFact(context.Background(), faulted, "dispatch", id, idle, engine, admission); err == nil {
		t.Fatal("the retry while the store refuses every connection = nil, want an error")
	}
	fault.Refuse(false)
	result, err := intake.ApplyFact(context.Background(), faulted, "dispatch", id, idle, engine, admission)
	if err != nil || !result.Duplicate {
		t.Fatalf("the retry once the store answers = %+v, %v; want a duplicate: the release committed", result, err)
	}

	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"the hold to clear once the store says the release committed", func() bool { return !admission.Held() })
	if lines := releaseLogLines(logged, slotTakenMessage); len(lines) != 1 || !strings.Contains(lines[0], "issue=LEGION-HELD") {
		t.Fatalf("slot taken lines = %q, want one naming LEGION-HELD: the release admits the root it held, once", lines)
	}
}
