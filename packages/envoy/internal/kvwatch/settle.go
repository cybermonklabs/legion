package kvwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"
)

// settling is one read's wait past its barrier, which consume keeps while the read waits. The
// barrier is the stream sequence its write was acknowledged at, 0 until the acknowledgement
// arrives on acks; passed is the highest stream sequence the consumer has delivered. The read stops
// waiting once passed has reached the barrier, and then reads from the stream's leader every key
// it has no evidence of, among the copy's rows and the keys the stream lists, before it removes
// any (verifyUnmarked).
//
// What a delivery at or past the barrier proves is the consumer's place: it walks the stream in
// sequence order, handing over every message it passes that the store still holds
// (nats-server v2.15.1 consumer.go:6400-6406, 5050-5074), so every key the read has delivered
// nothing for by then either has no record or has one written after the consumer passed its old
// one, which the leader's read finds: a row the copy holds, and a key only the stream lists, one
// a window write of another writer's stored and the bucket replaced ahead of the consumer. The
// pending count a delivery carries (its Delta) is read for nothing: it is the server's cached
// count, not a reading of the store, and it is wrong both ways. A stored message is acknowledged
// before its consumer signal is queued (stream.go:7505-7526), the signal is counted later on
// another goroutine (stream.go:8416-8449, consumer.go:7084-7102), the removal of an undelivered
// message the bucket replaced is counted at once (consumer.go:7020-7026), and a count below zero is
// reported as zero (consumer.go:5709-5713), so a delivery can carry 0 while a replacement is stored
// and undelivered; and a removal the cursor had already skipped is not counted at all
// (consumer.go:5688-5689, 7020-7022), so a delivery can carry 1 with nothing left to deliver, and a
// read that waited for a 0 would wait out its timeout.
type settling struct {
	since   time.Time
	acks    chan barrierAck
	barrier uint64
	passed  uint64
}

// barrierAck is the answer to a read's barrier write: the sequence it was committed at, or err.
type barrierAck struct {
	sequence uint64
	err      error
}

// settleAcks is the channel settle's barrier answer arrives on, nil while no read is waiting, so
// consume selects on it only then.
func settleAcks(settle *settling) <-chan barrierAck {
	if settle == nil {
		return nil
	}
	return settle.acks
}

// delivered records entry, a record the read's consumer delivered, and reports whether the read
// may stop waiting now: the consumer has delivered a record at or past the barrier.
func (s *settling) delivered(entry jetstream.KeyValueEntry) bool {
	s.passed = max(s.passed, entry.Revision())
	return s.barrier != 0 && s.passed >= s.barrier
}

// caughtUp reports whether the consumer has already delivered a record at or past barrier: the
// barrier's acknowledgement can arrive after its own delivery.
func (s *settling) caughtUp(barrier uint64) bool {
	return s.passed >= barrier
}

// errSettleTimedOut is what a read that did not read its consumer past its barrier within the idle
// timeout fails with (consume).
var errSettleTimedOut = errors.New("the read did not settle past its barrier")

// beginSettle starts generation's wait past a barrier, once its scan has ended: it writes the
// barrier from a goroutine of its own, whose answer consume reads on the returned wait's acks, so
// the consumer's deliveries go on meanwhile. A read that may no longer complete - replaced,
// stopped, ended, or disconnected since its watch began - waits for nothing: it is ended here
// (completeRead, which completes nothing for it) and nil is returned.
func (w *Watcher) beginSettle(generation uint64, read Read) *settling {
	w.mu.Lock()
	if !w.completes(generation) {
		w.mu.Unlock()
		w.finishSettle(generation, read, time.Time{}, 0)
		return nil
	}
	kv := w.kv
	settle := &settling{since: w.now(), acks: make(chan barrierAck, 1)}
	w.settleSince = settle.since
	w.mu.Unlock()
	go func() {
		sequence, err := kv.WriteBarrier(context.Background())
		settle.acks <- barrierAck{sequence: sequence, err: err}
	}()
	return settle
}

// armSettle arms generation's bound on its wait past the barrier, on the scan's idle wait, which
// the scan's end disarmed: the idle timeout from now. consume calls it once the barrier's
// acknowledgement has arrived and the stream check that follows it (confirmStream, bounded by
// bus.RequestTimeout) has returned, so the bound starts when that check returns, not at the
// acknowledgement. The bound's deadline is kept (settleDeadline), so a signal on that channel
// ends the wait only once it has gone (settleExpired).
func (w *Watcher) armSettle(generation uint64, idle chan<- struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if generation == w.generation && !w.stopped {
		w.settleDeadline = w.now().Add(w.scanIdle)
		w.armIdle(w.scanIdle, idle)
	}
}

// settleExpired reports whether generation's wait past its barrier has gone the idle timeout, when
// a signal came on its channel: a signal for a generation no longer current, after Stop, or once
// the bound was disarmed ends nothing. The scan's idle wait signals the same channel, and a
// stopped wait's callback may already be running (time.Timer.Stop does not wait for it), so a
// signal before the bound's deadline - the scan's retired wait - ends nothing either: the bound is
// armed again for what remains of it, as idleExpired does for a scan.
func (w *Watcher) settleExpired(generation uint64, idle chan<- struct{}) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if generation != w.generation || w.stopped || w.idleTimer == nil {
		return false
	}
	if remaining := w.settleDeadline.Sub(w.now()); remaining > 0 {
		w.armIdle(remaining, idle)
		return false
	}
	w.clearIdleArm()
	return true
}

// finishSettle ends generation's wait past its barrier, begun at since and acknowledged at stream
// sequence barrier. It first reads from the stream's leader every key the read has no evidence of
// (verifyUnmarked); a read of them that fails ends the wait as settleFailed does, so the read
// removes nothing and is read again. Otherwise it disarms the wait's bound, completes the read
// (completeRead) and releases readiness.
func (w *Watcher) finishSettle(generation uint64, read Read, since time.Time, barrier uint64) {
	if err := w.verifyUnmarked(generation, read, barrier); err != nil {
		w.settleFailed(generation, read, since, err)
		return
	}
	w.endSettle(generation)
	w.completeRead(generation, read)
	if w.current(generation) {
		w.signalReady()
	}
}

// unmarkedReads is how many keys verifyUnmarked reads from the stream's leader at once.
const unmarkedReads = 16

// verifyUnmarked reads from the stream's leader each key generation's read has no evidence of
// (Cache.Unmarked): each row of the copy, which CompleteRead would remove, and each key the
// bucket's stream holds a message on (bus.KeyValue.StoredKeys, asked through barrier) that the
// copy does not hold. It applies each record the leader holds to the cache as a delivered record
// (Cache.Apply): the key takes the leader's value and revision through the copy's fence and is
// marked, so CompleteRead keeps it. A key the leader answers it holds no record of - none, or a
// delete or purge marker as its newest message (nats.ErrKeyNotFound) - is left for CompleteRead to
// remove, or is never added. Any other answer, a refusal, a timeout, a list of the keys that is
// incomplete or from a copy of the stream behind the barrier, or an error from the stream, keeps
// every row and is returned, for the caller to fail the read; nothing is applied then.
//
// The leader is the authority because it alone answers the stream message get
// (bus.KeyValue.HeldFromLeader; nats-server v2.15.1 jetstream_api.go:3956-3958), and it stores a
// write before it acknowledges it (stream.go:7505-7526), so a write the copy took through a
// write-through is in the leader's store. A direct get is answered by any copy of the stream 90%
// caught up (jetstream_cluster.go:4810-4822) and may not hold it yet.
//
// The list is what finds a key the copy never held. Another writer's window write - stored after
// the read's consumer was made, before the read began - that the bucket replaced ahead of the
// consumer, after the barrier, is delivered only once the consumer passes the barrier, after a
// delivery at the barrier has ended the wait. The list is taken only from the stream's leader,
// and only once it has stored through the barrier (bus.ErrKeyListBehind), and the leader holds a
// message on every key that held a record at the barrier, that record or a later one, so the list
// names it, and the leader's read applies it before the read completes. A read that reaches its
// barrier makes one key listing: one STREAM.INFO per page of 100,000 subjects, normally one page,
// and one more for each step back past a subject removed between two pages
// (bus.KeyValue.StoredKeys), of one entry per key the bucket holds a message on. That is beside
// the barrier write and the stream check (confirmStream, a STREAM.INFO) consume makes when the
// barrier's acknowledgement arrives before the consumer has passed the barrier, which is the
// usual order. The scan delivers every key's newest message, delete markers included, so the
// leader reads only what the scan and the wait did not deliver.
//
// At most unmarkedReads reads are in flight, each bounded by bus.RequestTimeout, as the list is,
// and the first failure ends the rest. A read that may no longer complete, or a generation
// replaced or stopped meanwhile, reads and applies nothing.
func (w *Watcher) verifyUnmarked(generation uint64, read Read, barrier uint64) error {
	w.mu.RLock()
	completes := w.completes(generation)
	kv := w.kv
	w.mu.RUnlock()
	if !completes {
		return nil
	}
	stored, err := kv.StoredKeys(context.Background(), barrier)
	if err != nil {
		return fmt.Errorf("list the keys of KV bucket %s before the read completes: %w", w.bucket, err)
	}
	w.applyMu.Lock()
	w.mu.RLock()
	completes = w.completes(generation)
	w.mu.RUnlock()
	var keys []string
	if completes {
		keys = w.cache.Unmarked(stored)
	}
	w.applyMu.Unlock()
	if len(keys) == 0 {
		return nil
	}
	held := make([]nats.KeyValueEntry, len(keys))
	reads, ctx := errgroup.WithContext(context.Background())
	reads.SetLimit(unmarkedReads)
	for i, key := range keys {
		reads.Go(func() error {
			if !w.current(generation) {
				return nil
			}
			entry, err := kv.HeldFromLeader(ctx, key)
			switch {
			case err == nil:
				held[i] = entry
			case errors.Is(err, nats.ErrKeyNotFound):
			default:
				return err
			}
			return nil
		})
	}
	if err := reads.Wait(); err != nil {
		return fmt.Errorf("read from the stream's leader a key of KV bucket %s the read did not deliver: %w", w.bucket, err)
	}
	w.applyMu.Lock()
	defer w.applyMu.Unlock()
	if !w.current(generation) {
		return nil
	}
	for _, entry := range held {
		if entry != nil {
			w.cache.Apply(LegacyEntry(entry), read)
		}
	}
	return nil
}

// settleFailed ends generation's wait past its barrier, begun at since, without completing the
// read. The barrier was not written (TestReadErrReportsAReadFailureThatOutlastsTheStaleBound); the
// consumer did not read past it in time, whatever the stream check after the timeout answered
// except a replaced bucket (TestAReadsWaitPastItsBarrierIsBoundedAndReported,
// TestReadErrCountsATimedOutWaitWhoseStreamCheckFails); or, once the consumer was past it, the key
// list or a leader read of what the read did not deliver failed (finishSettle, verifyUnmarked;
// TestReadErrCountsAReadWhoseKeyListFailsAfterTheBarrier,
// TestReadErrCountsAReadWhoseLeaderReadFailsAfterTheBarrier). A read that may still complete
// removes nothing, is read again after the run's next wait and adds to the run ReadErr reports
// (readAgainUnverified), logged at WARN; one that may not (completeRead) ends as any such read
// does, and adds nothing (TestReadErrCountsNoFailureOfAReadTheConnectionDroppedUnder). Readiness
// is released either way.
func (w *Watcher) settleFailed(generation uint64, read Read, since time.Time, err error) {
	w.endSettle(generation)
	w.mu.RLock()
	completes := w.completes(generation)
	w.mu.RUnlock()
	if completes {
		w.logger().Warn(w.name+" read could not read its bucket past a barrier; it removes nothing and is read again",
			slog.String("bucket", w.bucket), slog.String("error", err.Error()))
		w.readAgainUnverified(generation, since, err)
	} else {
		w.completeRead(generation, read)
	}
	if w.current(generation) {
		w.signalReady()
	}
}

// settleReplaced ends generation's wait past its barrier, whose bucket the stream check found
// deleted, or deleted and created again (confirmStream, which stopped the copy following and arms
// the read of the new bucket): the read completes nothing, as completeRead's own check of a
// replaced stream leaves it, and readiness is released.
func (w *Watcher) settleReplaced(generation uint64) {
	w.endSettle(generation)
	if w.current(generation) {
		w.signalReady()
	}
}

// endSettle disarms generation's bound on its wait past the barrier and clears the wait's start.
func (w *Watcher) endSettle(generation uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if generation == w.generation {
		w.clearIdleArm()
		w.clearSettleLocked()
	}
}

// clearSettleLocked clears the current read's wait past its barrier: its start, which ReadErr
// measures from while no failed read has begun a run, and its bound's deadline. A wait ends when
// its read does (endSettle), and also when its generation ends without it: a new watch replaces it
// (watch), its watcher ends on its own (closed), or the watcher stops (Stop). Callers hold mu.
func (w *Watcher) clearSettleLocked() {
	w.settleSince = time.Time{}
	w.settleDeadline = time.Time{}
}
