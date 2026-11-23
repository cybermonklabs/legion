// The known-session store's repair: the one background retry that opens the bucket or moves the
// watcher to the connection the store holds now (repairUntilStopped), its attempts (retryRepair,
// attemptRepair), the reconnect hook's own attempt (repairOnce), and the arming and retiring of
// the retry (armRepair, retireRepair).

package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// repairResult is what one attempt to repair the store left behind (attemptRepair).
type repairResult int

const (
	// repairNothing is an attempt that found the store current already: its bucket is open and
	// its watcher is on the connection the store holds now, because the move a connection event
	// asked for was made by whoever got there first. It is what ends the background retry.
	repairNothing repairResult = iota
	// repairOpened is an attempt that opened a bucket that had never opened, and installed its
	// watcher.
	repairOpened
	// repairMoved is an attempt that moved the watcher, and the handle the store writes through,
	// to the connection the store holds now.
	repairMoved
	// repairAsked is an attempt that found nothing to repair while an arm came during it (rearm):
	// the retry attempts again.
	repairAsked
)

// repairAttemptWindow bounds one attempt of the background retry (retryRepair): the attempt holds
// repairMu, and its requests end with their ctx, so a reconnect's handshake, which holds nats.go's
// connection lock for up to its connect timeout, keeps the lock from the hooks and the arms for at
// most this long. An attempt that runs out is an attempt that failed, and the retry waits and
// tries again.
const repairAttemptWindow = 30 * time.Second

// repairUntilStopped is the store's one background repair: it attempts the store again, waiting
// retryFirst and then twice the wait before it up to retryMax, until an attempt finds nothing
// left to repair or the store is stopped. An attempt that repaired something starts the waits
// over, so a move that fails after a long denial is not left waiting out the cap, and so does a
// connection event that found no bucket open (wake), which is attempted at once.
//
// It repairs both of the states the store can be left in - a bucket that never opened, and a
// watcher a failed move left on a connection the listener has left - because they are one
// repair, make the store current on the connection it holds now, and because one loop is what
// makes two of them impossible: it is armed only under repairStateMu and only while none runs
// (armRepair), and only this goroutine retires it. A second connection event while it runs is
// another move for the attempt it is already making or for the next one, not a retry of its own,
// and a stop ends it wherever it is.
//
// The store repairs a failed move itself, in this loop and nowhere else: it is kept out of the
// listener's self-health rebuild, so this loop is the whole repair.
//
// It goes on past the attempt that repaired the store rather than ending there, so a move asked
// for while that attempt ran - whose connection the attempt never saw - is repaired too. The
// attempt after it finds nothing to repair and ends the retry.
//
// A bucket that opens is not the end of it: the watcher it installs fills the records in the
// background, and until it has, the store answers for none of the sessions the bucket knows, so
// the open is reported only once the copy has become servable (awaitOpened). That wait runs on a
// goroutine of its own, never in this loop, which makes its next attempt after its first wait, as
// after any repair. The first read can last as long as the repair of a failed move: a disconnect
// during it completes nothing and wakes no waiter (kvwatch.Watcher.Disconnected), and nats.go
// keeps the watcher's subscription open, so no end of the watcher ends the wait either. A loop
// blocked in that wait would stay armed while it repaired nothing, so the move of the reconnect
// after that disconnect, when it failed, would arm no retry (armRepair), and the store would stay
// off the bucket until the next connection event. The next attempt instead moves the watcher such
// a move left behind, or finds nothing and retires the retry, and a move that fails after that
// arms a fresh one; a watch that ends before its first read completes arms one too (awaitOpened).
// The bucket opens once, so at most one such wait runs per store, and it ends at the stop.
//
// The only lines it writes are the repairs (awaitOpened, attemptRepair), and none for an attempt
// that failed: the first failure is logged where it happened (OpenKnownSessions, and the
// listener's reconnect hook), and a line per attempt would be one every five minutes for the life
// of a listener whose grant never arrives. The connection still logs each attempt the server
// refuses, as one `envoy nats async error` at ERROR for the permissions violation
// (internal/bus/connect.go); this loop writes nothing beside it.
func (s *KnownSessions) repairUntilStopped() {
	for wait := s.retryFirst; ; {
		timer := time.NewTimer(wait)
		select {
		case <-s.stopped:
			timer.Stop()
			s.retireRepair()
			return
		case <-s.wake:
			timer.Stop()
			wait = s.retryFirst
		case <-timer.C:
		}
		result, err := s.retryRepair()
		if err != nil {
			wait = min(2*wait, s.retryMax)
			continue
		}
		wait = s.retryFirst
		switch result {
		case repairNothing:
			return
		case repairOpened:
			go s.awaitOpened()
		case repairMoved:
		}
	}
}

// retryRepair is the background retry's attempt (attemptRepair), which retires the retry when it
// found nothing left to repair. The retirement is made in the same hold as that reading, so no
// second retry is armed between this goroutine's last attempt and its end, and no attempt of
// another caller's lands between them either. The attempt runs until the store stops
// (untilStopped), so a stop ends an open that waits on the cluster (bus.EnsureKeyValue).
func (s *KnownSessions) retryRepair() (repairResult, error) {
	s.repairMu.Lock()
	defer s.repairMu.Unlock()
	ctx, cancel := s.untilStopped()
	defer cancel()
	ctx, cancelAttempt := s.attemptContext(ctx)
	defer cancelAttempt()
	result, err := s.attemptRepair(ctx)
	if err == nil && result == repairNothing {
		s.repairStateMu.Lock()
		if s.rearm {
			s.rearm, result = false, repairAsked
		} else {
			s.repairing = false
		}
		s.repairStateMu.Unlock()
	}
	return result, err
}

// attemptContext bounds one attempt of the retry at repairAttemptWindow, or as a test's own bound
// does (attemptBound).
func (s *KnownSessions) attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.attemptBound != nil {
		return s.attemptBound(ctx)
	}
	return context.WithTimeout(ctx, repairAttemptWindow)
}

// repairOnce is one attempt a caller of its own makes, the reconnect hook's move (Rewatch), which
// waits on nothing past ctx, the hook's run. It leaves the retry as it found it: a caller that
// fails arms one (armRepair), and one that succeeds leaves the running retry to find at its next
// attempt that there is nothing left to repair, which is where a retry is retired. Its wait for
// repairMu ends with ctx too (repairMu's LockContext), since the retry holds that lock through an
// attempt of its own.
func (s *KnownSessions) repairOnce(ctx context.Context) (repairResult, error) {
	if err := s.repairMu.LockContext(ctx); err != nil {
		return repairNothing, err
	}
	defer s.repairMu.Unlock()
	return s.attemptRepair(ctx)
}

// attemptRepair makes one attempt to make the store current on the connection it holds now: it
// opens the bucket while that has never opened, and otherwise moves the watcher and the handle
// the store writes through to that connection, while a move is owed (covered, asked) or the
// watcher has ended without one (kvwatch.Watcher.Err), which the listener's self-health rebuild
// repairs for every other cache and nothing repairs for this one. Callers hold repairMu, so the
// connection it reads is the one the store holds for the whole attempt and no other attempt is
// moving the watcher meanwhile: the watcher cannot be put back onto a connection the store has
// already left.
//
// A move that succeeds after one that failed says so in one line, whichever caller makes it: the
// background retry, or the next connection event's own move (Rewatch). The line is the store's
// report that it is back, so it is written for every such repair and only for one: a move that
// follows a connection event with nothing failed before it is the ordinary path, and says nothing.
func (s *KnownSessions) attemptRepair(ctx context.Context) (repairResult, error) {
	s.openMu.RLock()
	conn, watcher, asked, covered := s.conn, s.watcher, s.asked, s.covered
	s.openMu.RUnlock()
	switch {
	case watcher == nil:
		kv, err := openKnownBucket(ctx, conn, s.replicas, s.openWait)
		if err != nil {
			return repairNothing, err
		}
		s.startWatch(kv)
		s.cover(asked)
		return repairOpened, nil
	case covered < asked || watcher.Err() != nil:
		if err := watcher.Rewatch(ctx, conn); err != nil {
			s.moveFailed = true
			return repairNothing, err
		}
		s.cover(asked)
		if s.moveFailed {
			s.moveFailed = false
			s.logger().Info("the known-session cache is back on this listener's NATS connection; it answers its records and a publish's recipient count from the bucket again once its read of it completes",
				slog.String("bucket", bus.KnownSessionBucket))
		}
		return repairMoved, nil
	default:
		return repairNothing, nil
	}
}

// cover records that the attempt that just succeeded carried out every move asked for up to
// asked, the count it read before it began. A move asked for while it ran is not among them: it
// names the connection that attempt never read, so the attempt after it makes that one.
func (s *KnownSessions) cover(asked uint64) {
	s.openMu.Lock()
	if asked > s.covered {
		s.covered = asked
	}
	s.openMu.Unlock()
}

// armRepair starts the background repair unless one runs already or the store has stopped. A
// store that stopped repairs nothing: its watcher is retired and startWatch installs none.
func (s *KnownSessions) armRepair() {
	s.repairStateMu.Lock()
	defer s.repairStateMu.Unlock()
	if s.repairing {
		s.rearm = true
		return
	}
	select {
	case <-s.stopped:
		return
	default:
	}
	s.repairing = true
	go s.repairUntilStopped()
}

// armRepairWithin is armRepair for a reconnect hook's run. A run that has ended arms nothing: its
// connection is gone, the run is superseded and leaves the hooks due, and the run the next
// connection event brings asks again (Rewatch). It waits for no attempt (repairStateMu).
func (s *KnownSessions) armRepairWithin(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.armRepair()
}

// retireRepair records that no background repair runs, for the goroutine that ended at the stop.
// A repair armed beside it is refused by the same stop (armRepair).
func (s *KnownSessions) retireRepair() {
	s.repairStateMu.Lock()
	s.repairing, s.rearm = false, false
	s.repairStateMu.Unlock()
}
