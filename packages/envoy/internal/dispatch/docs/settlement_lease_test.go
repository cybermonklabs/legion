package docs

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// leasePeriod is the resumption period these rows run at, a second in place of the server's
// minute (SettlementResumeInterval), and leaseHold how long each settlement of the document takes,
// three periods: every settlement is slower than one period, as the server's would be when each
// took longer than a minute. The server's settle delay is two seconds against its minute; these
// rows arm at 100 ms against their second.
const (
	leasePeriod = time.Second
	leaseHold   = 3 * leasePeriod
)

// slowSettlement is a document that owes a settlement whose row is older than settlementResumeAge,
// and whose every settlement is held between the read of the room's generation it stands at and
// the check that ends a settlement the room's generation moved under (applyStamp): the hook is
// afterSettleTreeRead, which runs in that window.
type slowSettlement struct {
	service    *Service
	artifactID string
	versions   int
	// reads counts the settlements that reached their tree read, and reconciles those that got
	// past applyStamp's supersede check to the reconciliation (afterSettleReconcile).
	reads, reconciles atomic.Int32
}

// newSlowSettlement seeds and settles a document, edits it with the settle delay at an hour, so
// the edit's own timer does not run, and moves its pending row back an hour, past
// settlementResumeAge. Each settlement of the document is then held for hold at its tree read,
// and the settle delay is set to 100 ms for the settlements a pass arms.
func newSlowSettlement(t *testing.T, hold time.Duration, configure func(*Deps)) *slowSettlement {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	deps := testServiceDeps(database)
	// The edit below arms its own settlement an hour away: only the resumption settles it.
	deps.Settle = time.Hour
	if configure != nil {
		configure(&deps)
	}
	service := New(deps)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	awaitRoomAppends(t, service, artifactID, "become durable", service.waitForDurableAppends)
	ctx := context.Background()
	// A pass arms a row once it is a minute old (settlementResumeAge); the row is moved back past
	// that rather than waited out. Nothing after this appends to the document, so nothing moves
	// it forward again.
	if _, err := database.Pool.Exec(ctx, `
		update doc_settlements_pending set marked_at = now() - interval '1 hour' where artifact_id = $1
	`, artifactID); err != nil {
		t.Fatalf("age the document's pending-settlement row: %v", err)
	}
	owes, err := settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the edit left no pending-settlement row, so nothing is owed to resume")
	}
	f := &slowSettlement{service: service, artifactID: artifactID, versions: latestVersionNumber(t, service, artifactID)}
	release := make(chan struct{})
	service.afterSettleTreeRead = func(room string, _ *pmdoc.Node) {
		if room != artifactID {
			return
		}
		f.reads.Add(1)
		select {
		case <-time.After(hold):
		case <-release:
		}
	}
	service.afterSettleReconcile = func(room string) {
		if room == artifactID {
			f.reconciles.Add(1)
		}
	}
	// Runs before the shutdown above: a settlement still held lets go at once.
	t.Cleanup(func() { close(release) })
	service.settle = 100 * time.Millisecond
	return f
}

// resumeEvery runs a resumption pass (resumeOwedSettlements at settlementResumeAge, the body of
// RunSettlementResumption) at once and then every period until the test ends, and answers how
// many passes have run.
func (f *slowSettlement) resumeEvery(t *testing.T, period time.Duration) func() int32 {
	t.Helper()
	var passes atomic.Int32
	resumption, stop := context.WithCancel(context.Background())
	resumed := make(chan struct{})
	go func() {
		defer close(resumed)
		tick := time.NewTicker(period)
		defer tick.Stop()
		for {
			if err := f.service.resumeOwedSettlements(resumption, settlementResumeAge); err != nil && resumption.Err() == nil {
				t.Errorf("run a resumption pass: %v", err)
			}
			passes.Add(1)
			select {
			case <-resumption.Done():
				return
			case <-tick.C:
			}
		}
	}()
	t.Cleanup(func() {
		stop()
		<-resumed
	})
	return passes.Load
}

// requireCommits fails unless the document's settlement commits - its pending-settlement row is
// deleted and a version written - within the given time, and unless one settlement read its tree:
// nothing armed the document again while that settlement ran. passes counts the resumption passes
// that ran, nil where the test does not count them.
func (f *slowSettlement) requireCommits(t *testing.T, within time.Duration, passes func() int32) {
	t.Helper()
	ran := func() string {
		if passes == nil {
			return "uncounted"
		}
		return fmt.Sprint(passes())
	}
	started := time.Now()
	for {
		owes, err := settlementPending(context.Background(), f.service.store.Pool, f.artifactID)
		if err != nil {
			t.Fatalf("read the document's pending settlement: %v", err)
		}
		if !owes {
			break
		}
		if time.Since(started) > within {
			t.Fatalf("the document still owes its settlement %v on (resumption period %v): %d settlements read its tree, %d of them reached the reconciliation, resumption passes ran: %s, latest version %d (%d before)",
				within, leasePeriod, f.reads.Load(), f.reconciles.Load(), ran(), latestVersionNumber(t, f.service, f.artifactID), f.versions)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if latest := latestVersionNumber(t, f.service, f.artifactID); latest <= f.versions {
		t.Fatalf("the pending-settlement row is gone but the latest version is %d, want past %d", latest, f.versions)
	}
	t.Logf("settled after %v: %d settlements read its tree, %d reached the reconciliation, resumption passes ran: %s",
		time.Since(started).Round(time.Millisecond), f.reads.Load(), f.reconciles.Load(), ran())
	if reads := f.reads.Load(); reads != 1 {
		t.Fatalf("%d settlements read the document's tree, want the one that committed: the document was armed again while it ran", reads)
	}
}

// expireSettlementLeases moves the lease each of rooms' pending-settlement rows carries an hour
// back, past its end, as waiting it out would (leaseSettlementPending): what the resumption meets
// once the lease of a settlement that could not give it back - its process died, or it never
// learned the lease's end - has run out.
func expireSettlementLeases(t *testing.T, database *store.Store, rooms ...string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update doc_settlements_pending set leased_until = leased_until - interval '1 hour'
		where artifact_id = any($1::uuid[])
	`, rooms); err != nil {
		t.Fatalf("run out the settlements' leases: %v", err)
	}
}

// settlementLeaseOf is the end of the lease room's pending-settlement row carries, nil for none.
func settlementLeaseOf(t *testing.T, service *Service, room string) *time.Time {
	t.Helper()
	var until *time.Time
	if err := service.store.Pool.QueryRow(context.Background(),
		`select leased_until from doc_settlements_pending where artifact_id = $1`, room).Scan(&until); err != nil {
		t.Fatalf("read the document's pending-settlement lease: %v", err)
	}
	return until
}

// passArms runs a resumption pass for every row however young (resumeOwedSettlements) and reports
// whether it armed room: the pass stamps tried_at on each row it arms, so room's stamp moves.
func passArms(t *testing.T, service *Service, room string) bool {
	t.Helper()
	ctx := context.Background()
	var before, after *time.Time
	if err := service.store.Pool.QueryRow(ctx, `select tried_at from doc_settlements_pending where artifact_id = $1`, room).Scan(&before); err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("run a resumption pass: %v", err)
	}
	if err := service.store.Pool.QueryRow(ctx, `select tried_at from doc_settlements_pending where artifact_id = $1`, room).Scan(&after); err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	return (before == nil) != (after == nil) || (before != nil && !before.Equal(*after))
}

// The control: the same slow settlement, armed once with no resumption running, commits. What
// the rows below meet is the resumption, not the hold.
func TestASlowSettlementArmedOnceCommits(t *testing.T) {
	f := newSlowSettlement(t, leaseHold, nil)
	f.service.scheduleSettle(f.artifactID)
	f.requireCommits(t, 6*leaseHold, func() int32 { return 0 })
}

// Every settlement of the document takes three resumption periods, and the resumption runs every
// period: resumeOwedSettlements at settlementResumeAge, the body of RunSettlementResumption, on a
// ticker of the test's own. The first pass arms the settlement; each pass after it finds the row
// under the running settlement's lease (leaseSettlementPending) and leaves it, so the settlement
// commits. Before the lease each pass picked the row again, since no transaction holds it while
// the settlement works, and armed the settlement again (scheduleSettle), which moved the room's
// generation under the settlement still working: every settlement ended at applyStamp writing
// nothing, and the document never committed (INFRA-592, round 66).
func TestASettlementThatOutlastsAResumptionPeriodCommits(t *testing.T) {
	f := newSlowSettlement(t, leaseHold, nil)
	passes := f.resumeEvery(t, leasePeriod)
	f.requireCommits(t, 6*leaseHold, passes)
}

// The row above with the resumption run as the server runs it, RunSettlementResumption under its
// lease (duty.RunLeased), its period shortened to a second through Deps.SettlementResume.
func TestASettlementThatOutlastsALeasedResumptionPeriodCommits(t *testing.T) {
	f := newSlowSettlement(t, leaseHold, func(deps *Deps) { deps.SettlementResume = leasePeriod })
	leases := excerptLeases(t, f.service.store)
	resumption, stop := context.WithCancel(context.Background())
	resumed := make(chan struct{})
	go func() {
		defer close(resumed)
		f.service.RunSettlementResumption(resumption, leases)
	}()
	t.Cleanup(func() {
		stop()
		<-resumed
	})
	f.requireCommits(t, 6*leaseHold, nil)
}

// A settlement held just under its lease is armed by no pass while it runs, and commits. The lease
// is four seconds here (settleLease) and the settlement is held three of them at its tree read;
// the second left is the settlement's own work around the hold, its read before and its commit
// after. The passes run every 200 ms, so a pass meets the row about every fifth of the hold.
func TestASettlementHeldJustUnderItsLeaseIsArmedByNoOtherPassAndCommits(t *testing.T) {
	const lease = 4 * time.Second
	f := newSlowSettlement(t, lease-time.Second, nil)
	f.service.settleLease = lease
	passes := f.resumeEvery(t, 200*time.Millisecond)
	f.requireCommits(t, 3*lease, passes)
	if ran := passes(); ran < 10 {
		t.Fatalf("%d resumption passes ran while the settlement was held, want one every 200 ms of its %v", ran, lease-time.Second)
	}
}

// The lease ends at its length: a settlement held past it is armed again by the next pass, whose
// arming moves the room's generation under it. That is what every settlement slower than a period
// met before the lease, so the lease (settlementLease) has to outlast the longest a settlement
// runs, and a document whose settlement's process died with the lease held is armed by the first
// pass after it. The lease is a second here and each settlement is held three, so a second
// settlement reads the tree while the first is still held.
func TestAPassArmsTheDocumentAgainOnceItsSettlementsLeaseHasRunOut(t *testing.T) {
	const lease = time.Second
	f := newSlowSettlement(t, 3*lease, nil)
	f.service.settleLease = lease
	passes := f.resumeEvery(t, 200*time.Millisecond)
	waitFor(t, 3*lease, "a pass to arm the document again once the first settlement's lease ran out, while it was still held", func() bool {
		return f.reads.Load() >= 2
	})
	if reconciled := f.reconciles.Load(); reconciled != 0 {
		t.Fatalf("%d settlements reached the reconciliation before the second read the tree, want none: the first was still held", reconciled)
	}
	t.Logf("%d settlements read the tree after %d resumption passes", f.reads.Load(), passes())
}

// A settlement that runs past its deadline - here another transaction holds the document's owner
// row past it - gives its lease back as it ends (releaseSettlementLease), so the next resumption
// pass arms the document at once, not once the lease has run out (settlementLease), and the
// settlement that pass arms commits.
func TestASettlementPastItsDeadlineIsRetriedAtTheNextPass(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	versions := latestVersionNumber(t, service, artifactID)

	ctx := context.Background()
	hold, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the owner row's holder: %v", err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `select key from issues where key = 'DOC-1' for no key update`); err != nil {
		t.Fatalf("hold the document's owner row: %v", err)
	}
	service.settleTimeout = 300 * time.Millisecond
	settleCurrentGeneration(t, service, artifactID)
	service.settleTimeout = settlementTimeout
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: the settlement held past its deadline committed", latest, versions)
	}
	if until := settlementLeaseOf(t, service, artifactID); until != nil {
		t.Fatalf("the settlement that ran past its deadline left its lease on the row, ending %v; want it given back", until)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("release the owner row: %v", err)
	}
	service.settle = 10 * time.Millisecond
	if !passArms(t, service, artifactID) {
		t.Fatal("the next resumption pass did not arm the document whose settlement ran past its deadline")
	}
	waitForDocumentVersion(t, service.store, artifactID, versions+1)
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")
}

// A settlement a newer settlement of its document superseded gives back only its own lease: the
// newer one has taken a lease of its own by the time the older ends, and the older's give-back,
// which clears the lease only where the row still holds the end it wrote
// (releaseSettlementLeaseStatement), leaves the newer lease standing, so no pass arms the
// document while the newer settlement runs. Both settlements are held at their tree read: the
// older until the newer has taken its lease, the newer until the older has ended.
func TestASupersededSettlementsGiveBackLeavesTheNewerLeaseStanding(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	awaitRoomAppends(t, service, artifactID, "become durable", service.waitForDurableAppends)
	versions := latestVersionNumber(t, service, artifactID)

	holds := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var lets [2]sync.Once
	let := func(index int) { lets[index].Do(func() { close(holds[index]) }) }
	t.Cleanup(func() { let(0); let(1) })
	var reads atomic.Int32
	service.afterSettleTreeRead = func(room string, _ *pmdoc.Node) {
		if room != artifactID {
			return
		}
		if read := reads.Add(1); read <= int32(len(holds)) {
			<-holds[read-1]
		}
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	ended := make(chan settleOutcome, 1)
	go func() { ended <- service.settleRoomWithin(context.Background(), artifactID, generation, false) }()
	waitFor(t, roomWait(t), "the older settlement to read the tree under its lease", func() bool { return reads.Load() >= 1 })
	older := settlementLeaseOf(t, service, artifactID)
	if older == nil {
		t.Fatal("the older settlement holds no lease on the row while it runs")
	}

	// The newer settlement: arming moves the room's generation, and its timer runs it at once.
	service.settle = 10 * time.Millisecond
	service.scheduleSettle(artifactID)
	waitFor(t, roomWait(t), "the newer settlement to read the tree under its own lease", func() bool { return reads.Load() >= 2 })
	newer := settlementLeaseOf(t, service, artifactID)
	if newer == nil || newer.Equal(*older) {
		t.Fatalf("the newer settlement's lease ends %v, want one of its own after the older's %v", newer, *older)
	}

	let(0)
	if outcome := <-ended; outcome.next != settleEnded || outcome.err != nil {
		t.Fatalf("the superseded settlement ended as %+v, want it ended writing nothing", outcome)
	}
	if standing := settlementLeaseOf(t, service, artifactID); standing == nil || !standing.Equal(*newer) {
		t.Fatalf("after the superseded settlement gave its lease back the row's lease ends %v, want the newer settlement's %v", standing, *newer)
	}
	if passArms(t, service, artifactID) {
		t.Fatal("a resumption pass armed the document while the newer settlement held its lease")
	}
	let(1)
	waitForDocumentVersion(t, service.store, artifactID, versions+1)
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")
}
