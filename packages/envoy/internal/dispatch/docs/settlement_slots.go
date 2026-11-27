package docs

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// settlementWorkers is how many document settlements run at once, every way one starts: a timer, a
// room's last browser leaving (settleLastPeer), a retry, the resumption and the shutdown drain all
// end in settleRoomWithin, which takes one of these slots before it loads a cold room. Each running
// settlement holds one connection of the store's settlements pool (store.Pool.Settlements) for its
// transaction, and that pool has as many connections as there are slots
// (TestSettlementSlotsMatchTheSettlementsPool), so a settlement holding a slot never waits for a
// connection behind another settlement. Without the bound N owed documents - every one a backlog
// of resumptions arms at once - ran N settlements, each loading its room and wanting a connection,
// which is DR-05's collapse.
//
// A settlement does not wait for the requests to ease before it takes a slot, as a compaction's
// fold does (requestYield). That wait was built, measured on the load test and refused
// (INFRA-592). The execution trace of the span where a burst's recovery keeps failing (30 to 55 s
// after the drop) puts the settlement timers second only to the collector's mark workers among
// the work that is not a request handler: 5.4 to 5.6 s of the 75 processor seconds, over 50 to 53
// settlements (about 105 ms of processor each), against 15 s for the handlers. A settlement that
// waited out the request pressure before it took its slot, for at most 10 s, made a step's
// settlement work grow by about 35%: the settlements pool's acquires in the two minutes after the
// drop went from 791 and 818 to 1,042 to 1,108 over four runs, and the request p99 of the windows
// that fail did not improve. A settlement that waits is likelier to see its room's generation
// move, which ends it and arms another (openLiveWrite), so to defer settlements is to multiply
// them. The slots are what bounds them.
const settlementWorkers = 4

// settlementTimeout bounds every settlement (settleRoomWithin), from its wait for a slot to its
// write phase, whichever way it started: a timer, a room's last browser leaving, a retry, the
// resumption, the settlement an issue's close runs first (settleOwedNow) and the shutdown drain. A
// caller's own deadline that comes first ends it first: the drain's budget (ShutdownDrainBudget), a
// keyed request's run. A settlement still running at the bound rolls back, keeps the document's
// pending-settlement row and counts no failure: the resumption settles it once the load that held
// it up has passed (RunSettlementResumption). Three parts it cannot stop: a cold room's load, since
// ygo loads a room with no context of its own (servicePersistenceAdapter.LoadDoc), whose reads run
// under bounds of their own (coldReadTimeout); what a pass has left to do once it has written into
// the room (settlementFinishBound); and a pass's write phase, which runs under a bound that grows
// with the document (settlementWriteBound).
const settlementTimeout = 60 * time.Second

// errSettlementTimeout is the cause a settlement's context ends with at settlementTimeout, which
// tells its retry the deadline from a shutdown's budget.
var errSettlementTimeout = errors.New("document settlement ran past its deadline")

// settlementFinishBound bounds what a settlement pass has left to do once it has written into its
// room: the broadcast of that update, its append and the reads, rows and events its commit needs,
// up to its write phase, which runs under a bound of its own that is never shorter than what this
// one has left (writePhase). That work runs apart from the settlement's deadline
// (settlementTimeout), under this bound of its own, as a commit runs apart from its caller's
// deadline (store.CommitContext). The room and its browsers hold the update by then, so a deadline
// falling there would fail the room, close every editor's socket and reload the document over work
// the pass had all but committed. Only work the database leaves unanswered for this long - an
// update in the room that will never reach the store - fails the room now.
const settlementFinishBound = 30 * time.Second

// errSettlementFinishTimeout is the cause the context of a pass that has written into its room ends
// with at settlementFinishBound.
var errSettlementFinishTimeout = errors.New("document settlement ran past the bound of the pass that wrote into its room")

// finishInRoom is the context a settlement pass finishes what it started on, once it has written
// into its room: ctx's values - its transaction, its ledger, its room cursor - without the
// settlement's deadline, bounded at settlementFinishBound.
func finishInRoom(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(context.WithoutCancel(ctx), settlementFinishBound, errSettlementFinishTimeout)
}

// settlementWriteFloor, settlementWriteBlockCost and settlementWriteHeadroom make the bound on a
// settlement pass's write phase (settlementWriteBound): the batch of every statement the pass
// queued (Ledger.runQueue), and then its commit (commitSettlement), whose COMMIT runs under the
// bound of its own every commit has (store.CommitContext). A pass decides everything before it
// writes anything, so that phase is where all of its writes go, a version's block index - its graph
// nodes and tuples (graphstore.QueueNodes, graphstore.QueueCaused), its document_blocks rows and
// its block texts (refs.BlockIndexing.Queue) - among them, and its length grows with the version's
// blocks. The phases before it keep the settlement's deadline (settlementTimeout), and a pass that
// has written into its room the finish bound (settlementFinishBound); the write phase runs apart
// from both, under this bound, or under what that deadline or finish bound has left where that is
// longer (writePhase). So a write phase ends no sooner than it did when the deadline bounded it, a
// version of many blocks runs as long as its rows take, and a write the database leaves unanswered
// is still cut and the settlement left to the resumption (settlePastDeadline).
//
// Measured on 2026-11-23 (INFRA-592) with TestASettlementStampsMoreBlocksThanYgosDefaultQueue, a
// version of 150,001 blocks, on this branch merged with main 6e4e55be, a plain build on a 2-CPU
// pod against its test Postgres 18: the write phase ran 43.8 s, 23 kinds of statement in 10
// batches (store.WriteBatch.Send), each a statement over all of its rows but for the graph nodes'
// two and the graph tuples' append, 55 statements of up to 8,192 rows (graphstore.appendRows):
// none is a statement or a round trip per row. The append took 28.5 s (600,004 tuples, the base
// attributes among them, and the move of 150,001 nodes onto their event), the document_blocks
// rows 6.7 s (2.3 s of it the foreign key each row's node key checks in graph_nodes, 0.9 s the one
// into artifacts), the graph nodes 5.0 s (two statements), the block texts 3.4 s and every other
// statement 0.1 s together. Postgres's plans (explain analyze of each kind) put that time in the
// server's work on each row - the heap insert, the six indexes of graph_tuples and the two unique
// ones it arbitrates on, the node moves, the foreign-key triggers - and the process spent 0.49 s
// of processor in the write phases of the test's two passes, 42 ms of it building the statements'
// arguments. That is 292 µs a block, which settlementWriteBlockCost rounds up; main's graph store
// (#3178) added most of it, the nodes, tuples and node keys, which took the test from 8.45 s on CI
// before it to 60.44 s after. The bound counts each block of the tree the pass read
// (settlementPass.offsets), for each of which the version's block index writes those rows; a
// block's links add a tuple and a block_refs row each, which the blocks of that test held none of,
// so settlementWriteHeadroom, three times the measured cost, is also what covers a document whose
// blocks hold links. The floor is the bound of a write phase that writes no block index: the
// longest measured is the stamp of those 150,001 blocks (commitStamp), three statements, one an
// 8.5 MB update, in 0.30 s and 0.52 s over two runs, of which the floor is three times, rounded
// up.
const (
	settlementWriteFloor     = 2 * time.Second
	settlementWriteBlockCost = 300 * time.Microsecond
	settlementWriteHeadroom  = 3
)

// settlementWriteBound is the bound on the write phase of a settlement pass whose tree holds
// blocks blocks: settlementWriteFloor, and settlementWriteBlockCost a block times
// settlementWriteHeadroom. A version of 150,001 blocks has 2 minutes and 17 s.
func settlementWriteBound(blocks int) time.Duration {
	return settlementWriteFloor + time.Duration(blocks)*settlementWriteBlockCost*settlementWriteHeadroom
}

// errSettlementWriteTimeout is the cause a settlement pass's write phase ends with at its bound
// (writePhase), which its stop short reads as the settlement past its deadline (stopShort).
var errSettlementWriteTimeout = errors.New("document settlement's write phase ran past its bound")

// writePhase is the context a settlement pass runs its write phase on (Ledger.runQueue,
// commitSettlement), made from ctx, the context the pass reached it under: ctx's values, without
// ctx's deadline, bounded at settlementWriteBound for blocks blocks - or at that deadline where
// that is later, so no write phase has less time than the deadline left it - with
// errSettlementWriteTimeout as the cause. A caller other than nil ends it too: the context the
// settlement's own caller handed it (settlement.caller), whose end - the shutdown drain's budget, a
// keyed request's run - ends a pass that has not written into its room. A pass that has (commitStamp,
// commitRepairs) passes nil, since its finish already runs apart from that caller (finishInRoom).
func writePhase(ctx, caller context.Context, blocks int) (context.Context, context.CancelFunc) {
	bound := settlementWriteBound(blocks)
	if deadline, ok := ctx.Deadline(); ok {
		bound = max(bound, time.Until(deadline))
	}
	bounded, cancelBound := context.WithTimeoutCause(context.WithoutCancel(ctx), bound, errSettlementWriteTimeout)
	if caller == nil {
		return bounded, cancelBound
	}
	write, end := context.WithCancelCause(bounded)
	stop := context.AfterFunc(caller, func() { end(context.Cause(caller)) })
	return write, func() {
		stop()
		end(context.Canceled)
		cancelBound()
	}
}

// writeFailed is err, a failure of a pass's write phase run on write (writePhase), naming
// errSettlementWriteTimeout where the phase's bound is what ended it: the statement cut short
// answers the context's error and not its cause.
func writeFailed(write context.Context, err error) error {
	if err == nil || !errors.Is(context.Cause(write), errSettlementWriteTimeout) || errors.Is(err, errSettlementWriteTimeout) {
		return err
	}
	return fmt.Errorf("%w: %w", err, errSettlementWriteTimeout)
}

// settlementLease is how long a settlement that has begun its work on a document holds the
// document's pending-settlement row against the resumption (leaseSettlementPending,
// doc_settlements_pending.leased_until) at most, but for a document of many blocks: the longest it
// can still be under way from that point before it commits or gives up. A settlement that ends
// gives the lease back - its commit deletes the row, and any other end clears the lease
// (releaseSettlementLease) - so the length matters only while a settlement runs and once its
// process has died with the lease still held. It is the sum of the bounds on what follows the
// lease, each counted to its end as if each ran to its bound after the one before it had:
//   - settlementTimeout: the settlement's deadline, which covers its wait for a slot, before the
//     lease, and its passes' work after it (settleRoomWithin);
//   - two cold reads (store.ColdReadBound, coldReadTimeout): a cold room's load, which the
//     settlement's warm-up runs and no deadline of the settlement's stops, reads the room's stored
//     history under one bound (servicePersistenceAdapter.LoadDoc) and then the document's issue and
//     pending-settlement row under another (onLoadDocument), each the bound withLoadReadsBound
//     makes, a cold read's;
//   - settlementFinishBound: what a pass that has written into its room has left to do, which runs
//     past the deadline;
//   - settlementWriteFloor: the write phase, which runs past the deadline or the finish bound by at
//     most settlementWriteBound (writePhase). The part of that bound a document's blocks add is no
//     part of the sum: a pass whose write bound the lease does not cover extends the lease by it
//     before it opens its transaction (leaseThroughWrite).
//
// What none of these bounds is the load's decode of what it read (bootstrapLoadedState), work of
// the processor alone over a history the room's item cap bounds (maxUpdateItems), and the sum
// counts the load and the finish both to their ends, where a load that ran past the deadline
// leaves the passes after it nothing they could write into the room. A lease longer than the
// settlement only keeps a document whose settlement's process died, or whose give-back failed,
// from the resumption for longer. One shorter than a running settlement lets a pass arm the
// document again, which moves the room's generation under the settlement and ends it writing
// nothing (applyStamp): a document whose every settlement outlasted that lease would never commit.
const settlementLease = settlementTimeout + 2*store.ColdReadBound + settlementFinishBound + settlementWriteFloor

// settlementSlots is the bound settlementWorkers sets: a slot per running settlement, and counts of
// the settlements running and of those waiting for a slot, which the metrics endpoint reports.
type settlementSlots struct {
	slots   chan struct{}
	running atomic.Int64
	waiting atomic.Int64
}

func newSettlementSlots(size int) *settlementSlots {
	return &settlementSlots{slots: make(chan struct{}, size)}
}

// acquire waits for a slot until ctx ends, and returns its release. A settlement waiting here holds
// no connection and no lock, so the wait is the only place the excess queues.
func (s *settlementSlots) acquire(ctx context.Context) (func(), error) {
	s.waiting.Add(1)
	select {
	case s.slots <- struct{}{}:
		s.waiting.Add(-1)
		s.running.Add(1)
		return func() {
			s.running.Add(-1)
			<-s.slots
		}, nil
	case <-ctx.Done():
		s.waiting.Add(-1)
		return nil, context.Cause(ctx)
	}
}

// SettlementLoad is how many document settlements hold a slot and how many wait for one.
type SettlementLoad struct {
	Running int64
	Waiting int64
}

// SettlementLoad reports the settlements running and waiting now (settlementWorkers), the
// settlements_running and settlements_waiting gauges of the metrics endpoint.
func (s *Service) SettlementLoad() SettlementLoad {
	return SettlementLoad{Running: s.settlements.running.Load(), Waiting: s.settlements.waiting.Load()}
}
