package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// The bound on a settlement pass's write phase is its floor for a write of no blocks and grows by
// the measured cost of a block, times the headroom, with every block the pass's tree holds: a small
// write keeps the floor, and the version of 150,001 blocks whose write phase was measured at 43.8 s
// (settlementWriteBlockCost) has at least three times that.
func TestASettlementsWriteBoundIsItsFloorPlusAMeasuredCostForEachBlock(t *testing.T) {
	t.Parallel()
	if got := settlementWriteBound(0); got != settlementWriteFloor {
		t.Fatalf("the write bound of a pass of no blocks is %v, want the floor %v", got, settlementWriteFloor)
	}
	for _, blocks := range []int{1, 10, 100} {
		if got := settlementWriteBound(blocks); got < settlementWriteFloor || got > settlementWriteFloor+100*time.Millisecond {
			t.Fatalf("the write bound of a pass of %d blocks is %v, want the floor %v and at most 100 ms more", blocks, got, settlementWriteFloor)
		}
	}
	block := settlementWriteBound(1) - settlementWriteBound(0)
	if want := settlementWriteBlockCost * settlementWriteHeadroom; block != want {
		t.Fatalf("one block adds %v to the write bound, want its measured cost times the headroom, %v", block, want)
	}
	for _, blocks := range []int{1_000, 10_000, 150_001} {
		if grown := settlementWriteBound(2*blocks) - settlementWriteBound(blocks); grown != time.Duration(blocks)*block {
			t.Fatalf("doubling %d blocks adds %v to the write bound, want %v: %d blocks' worth", blocks, grown, time.Duration(blocks)*block, blocks)
		}
	}
	const measured = 43800 * time.Millisecond
	if got := settlementWriteBound(150_001); got < settlementWriteHeadroom*measured {
		t.Fatalf("the write bound of the 150,001 blocks whose write phase ran %v is %v, want at least %d times that", measured, got, settlementWriteHeadroom)
	}
}

// A write phase runs apart from the deadline the pass reached it under (writePhase): it keeps a
// deadline further than its own bound, has its own bound where the deadline is nearly spent and
// goes on once that deadline has passed, still ends with the caller a pass that wrote nothing into
// its room hands it, and names its own bound as the cause when that bound ends it (writeFailed).
func TestAWritePhaseRunsApartFromTheDeadlineItStartsUnder(t *testing.T) {
	t.Parallel()
	const blocks = 1_000
	settlement, cancel := context.WithTimeoutCause(context.Background(), 50*time.Millisecond, errSettlementTimeout)
	defer cancel()
	started := time.Now()
	write, end := writePhase(settlement, nil, blocks)
	defer end()
	deadline, ok := write.Deadline()
	if want := started.Add(settlementWriteBound(blocks)); !ok || deadline.Before(want) || deadline.After(want.Add(time.Second)) {
		t.Fatalf("the write phase under a deadline 50 ms away ends at %v (%v), want its own bound %v from its start", deadline.Sub(started), ok, settlementWriteBound(blocks))
	}
	<-settlement.Done()
	if err := write.Err(); err != nil {
		t.Fatalf("the write phase ended with the settlement's deadline (%v), want it to run on to its own bound", err)
	}

	far, cancelFar := context.WithTimeout(context.Background(), time.Hour)
	defer cancelFar()
	kept, endKept := writePhase(far, nil, blocks)
	defer endKept()
	farDeadline, _ := far.Deadline()
	if keptDeadline, ok := kept.Deadline(); !ok || keptDeadline.Before(farDeadline.Add(-time.Second)) {
		t.Fatalf("the write phase under a deadline an hour away ends at %v (%v), want no sooner than that deadline, %v", keptDeadline, ok, farDeadline)
	}

	drained := errors.New("test: the shutdown drain's budget ended")
	caller, endCaller := context.WithCancelCause(context.Background())
	settling, cancelSettling := context.WithTimeoutCause(caller, time.Hour, errSettlementTimeout)
	defer cancelSettling()
	following, endFollowing := writePhase(settling, caller, blocks)
	defer endFollowing()
	endCaller(drained)
	select {
	case <-following.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the write phase went on after its caller ended")
	}
	if cause := context.Cause(following); !errors.Is(cause, drained) {
		t.Fatalf("the write phase ended with %v, want its caller's %v", cause, drained)
	}
	if err := writeFailed(following, context.Canceled); errors.Is(err, errSettlementWriteTimeout) {
		t.Fatalf("a write phase its caller ended failed as %v, want no write bound named", err)
	}

	bounded, endBounded := writePhase(context.Background(), nil, 0)
	defer endBounded()
	select {
	case <-bounded.Done():
	case <-time.After(settlementWriteFloor + 5*time.Second):
		t.Fatalf("the write phase of no blocks under no deadline was still running past its floor %v", settlementWriteFloor)
	}
	if cause := context.Cause(bounded); !errors.Is(cause, errSettlementWriteTimeout) {
		t.Fatalf("the write phase ended at its bound with %v, want %v", cause, errSettlementWriteTimeout)
	}
	if err := writeFailed(bounded, context.DeadlineExceeded); !errors.Is(err, errSettlementWriteTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a statement its write phase's bound cut failed as %v, want both the statement's error and %v", err, errSettlementWriteTimeout)
	}
}

// A settlement's write phase - the batch of every statement its pass queued, the version's block
// index among them - runs apart from the settlement's deadline, under a bound of its own that grows
// with the document's blocks (writePhase). Here the database answers nothing once that batch is
// sent (storetest.NewFrozenSocket on the settlements pool, armed on the edit's own text, which the
// batch alone carries): the write phase outlives the settlement's deadline, is cut at its own
// bound, and the settlement rolls back, keeps the document's pending-settlement row, gives its lease
// back and counts no failure, so the resumption settles the document.
func TestASettlementWhoseWritePhaseStallsIsCutAtItsWriteBoundAndStaysOwed(t *testing.T) {
	t.Parallel()
	socket := storetest.NewFrozenSocket(store.SettlementsPool)
	database := storetest.Reopened(t, storetest.Open(t), func(config *pgxpool.Config) {
		config.ConnConfig.DialFunc = socket.Dial
	})
	service, artifactID := newTestServiceWithDocument(t, database)
	service.settle = time.Hour
	ctx := context.Background()
	// Blocks enough that the write bound is well past the settlement's deadline below.
	const paragraphs = 5_000
	var seed strings.Builder
	for index := range paragraphs {
		fmt.Fprintf(&seed, "Paragraph %d.\n\n", index)
	}
	seedServiceText(t, service, artifactID, seed.String())
	settleCurrentGeneration(t, service, artifactID)
	const marker = "the write phase stalls"
	editLiveTree(t, service, artifactID, replaceRun("Paragraph 0.", "Paragraph 0, "+marker+"."))
	awaitRoomAppends(t, service, artifactID, "become durable", service.waitForDurableAppends)
	versions := latestVersionNumber(t, service, artifactID)
	stamped, err := service.Blocks(ctx, artifactID)
	if err != nil {
		t.Fatalf("read the document's blocks: %v", err)
	}
	bound := settlementWriteBound(len(stamped))

	// The pass's work before its write phase - its read and render of the tree, its version's
	// block index - runs under the settlement's deadline. Run alone on a 2-CPU pod it took 0.1 s to
	// 0.4 s, and up to 0.8 s under the race detector; beside the package's other tests under the
	// race detector a deadline of 2 s failed this row as a settlement that runs out its deadline
	// before it queues its version's writes fails it, with nothing to arm the socket
	// (envoy-go-race-docs, 2026-11-25). The deadline is what the write bound of these blocks
	// (6.5 s) leaves after a margin that still tells the two apart.
	const deadline = 5 * time.Second
	service.settleTimeout = deadline
	testcleanup.Add(t, func() { service.settleTimeout = settlementTimeout })
	if bound < deadline+time.Second {
		t.Fatalf("the write bound of %d blocks is %v, too near the %v deadline to tell the two apart", len(stamped), bound, deadline)
	}
	// The write phase begins once the version's writes are queued: from here the batch carrying the
	// edit's text is the next thing the pass sends.
	var queued atomic.Int64
	service.afterSettleVersionWrite = func(room string) error {
		if room == artifactID {
			queued.Store(time.Now().UnixNano())
			socket.StallAfter(marker)
		}
		return nil
	}
	started := time.Now()
	outcome := service.settleRoomWithin(ctx, artifactID, roomGeneration(t, service, artifactID), false)
	returned := time.Now()
	service.afterSettleVersionWrite = nil

	// A settlement that never queued its version's writes never armed the socket: its outcome says
	// what ended it first.
	if queued.Load() == 0 {
		t.Fatalf("the settlement ended %d (%v) %v after it began, before it queued its version's writes: want them queued inside its %v deadline", outcome.next, outcome.err, returned.Sub(started), deadline)
	}
	if !socket.Fired() {
		t.Fatal("the settlements socket never stalled, so the write phase ran as any other")
	}
	wrote := time.Unix(0, queued.Load())
	if wrote.Sub(started) >= deadline {
		t.Fatalf("the version's writes were queued %v into the settlement, want inside its %v deadline", wrote.Sub(started), deadline)
	}
	// The write phase ended at its own bound and not at the deadline: it ran no shorter than that
	// bound, and the cause it ended with is that bound's (errSettlementWriteTimeout, below).
	if took := returned.Sub(wrote); took < bound {
		t.Fatalf("the stalled write phase ended %v after it began, want at its own bound %v: not at the %v deadline", took, bound, deadline)
	}
	if outcome.next != settlePastDeadline || !errors.Is(outcome.err, errSettlementWriteTimeout) {
		t.Fatalf("the settlement ended as %+v, want it past its deadline, cut at its write phase's bound", outcome)
	}
	owes, err := settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone, want it kept for the resumption")
	}
	if until := settlementLeaseOf(t, service, artifactID); until != nil {
		t.Fatalf("the settlement cut at its write bound left its lease on the row, ending %v; want it given back", until)
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the document's room is gone, want it resident")
	}
	state := service.room(artifactID)
	state.mu.Lock()
	failures, failed := state.settleFailures, state.failed
	state.mu.Unlock()
	if failures != 0 || failed != nil {
		t.Fatalf("settlement failures = %d, room failure = %v; want none for a write the database left unanswered", failures, failed)
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: no settlement whose write phase was cut commits", latest, versions)
	}

	service.settleTimeout = settlementTimeout
	service.settle = 10 * time.Millisecond
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the owed settlement: %v", err)
	}
	waitForDocumentVersion(t, service.store, artifactID, versions+1)
	var markdown string
	if err := service.store.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read the latest document version: %v", err)
	}
	if !strings.Contains(markdown, marker) {
		t.Fatalf("the version the resumption wrote holds no %q", marker)
	}
}

// A pass whose write phase may outrun its settlement's lease extends the lease before it opens its
// transaction (leaseThroughWrite), by a compare-and-set of the lease's end
// (extendSettlementLease), and the settlement's give-back then clears the extended lease
// (releaseSettlementLease). A pass of a document of few blocks under a lease long enough writes
// nothing; an extension of a lease that is no longer the settlement's, or that would end it
// sooner, writes nothing either.
func TestAPassWhoseWriteBoundOutrunsItsLeaseExtendsItAndStillGivesItBack(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	ctx := context.Background()
	if _, err := service.store.Pool.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id) values ($1) on conflict (artifact_id) do nothing
	`, artifactID); err != nil {
		t.Fatalf("mark the document owed: %v", err)
	}
	settlements, err := service.store.Pool.Settlements()
	if err != nil {
		t.Fatalf("open the settlements pool: %v", err)
	}
	lease, err := leaseSettlementPending(ctx, settlements, artifactID, settlementLease)
	if err != nil || lease == nil {
		t.Fatalf("lease the document's pending settlement: %v, %v", lease, err)
	}
	settling, cancel := context.WithTimeoutCause(ctx, settlementTimeout, errSettlementTimeout)
	defer cancel()
	pass := func(blocks int) *settlementPass {
		return &settlementPass{
			settlement: &settlement{service: service, room: artifactID, settlements: settlements, lease: lease},
			offsets:    make([]pmdoc.BlockOffset, blocks),
		}
	}

	few := pass(100)
	if err := few.leaseThroughWrite(settling); err != nil {
		t.Fatalf("lease a pass of 100 blocks through its write phase: %v", err)
	}
	if !few.lease.Equal(*lease) {
		t.Fatalf("a pass of 100 blocks moved its lease from %v to %v, want it left as its settlement took it", *lease, *few.lease)
	}

	const blocks = 300_000
	many := pass(blocks)
	before := time.Now()
	if err := many.leaseThroughWrite(settling); err != nil {
		t.Fatalf("lease a pass of %d blocks through its write phase: %v", blocks, err)
	}
	extended := many.lease
	if want := before.Add(settlementFinishBound + settlementWriteBound(blocks)); extended.Equal(*lease) || extended.Before(want) {
		t.Fatalf("a pass of %d blocks leased the row until %v, want past its write phase's end, %v", blocks, *extended, want)
	}
	if standing := settlementLeaseOf(t, service, artifactID); standing == nil || !standing.Equal(*extended) {
		t.Fatalf("the row's lease ends %v, want the extended %v", standing, *extended)
	}
	if stale, err := extendSettlementLease(ctx, settlements, artifactID, *lease, 2*settlementLease); err != nil || stale != nil {
		t.Fatalf("an extension of the lease as the settlement first took it answered %v, %v, want nothing written: the row's lease ends %v now", stale, err, *extended)
	}
	if sooner, err := extendSettlementLease(ctx, settlements, artifactID, *extended, time.Second); err != nil || sooner != nil {
		t.Fatalf("an extension ending the lease sooner answered %v, %v, want nothing written", sooner, err)
	}
	if standing := settlementLeaseOf(t, service, artifactID); standing == nil || !standing.Equal(*extended) {
		t.Fatalf("the row's lease ends %v, want the extended %v still", standing, *extended)
	}
	releaseSettlementLease(ctx, settlements, artifactID, *extended)
	if standing := settlementLeaseOf(t, service, artifactID); standing != nil {
		t.Fatalf("after the give-back of the extended lease the row's lease ends %v, want none", standing)
	}
}
