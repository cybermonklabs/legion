package kvwatch

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrRefilling is what Servable answers while the copy cannot be served: no read has completed
// yet, or the copy has not followed the bucket for longer than the stale bound.
var ErrRefilling = errors.New("kvwatch: the cache has no complete read of its bucket recent enough to serve")

// defaultStaleBound is how long a copy that stopped following its bucket is still served. It is
// under half the 5-minute record lifetime of the session bucket, so a copy past it would mostly
// list sessions that already ended; within it the reconnect hook and the listener's 30 s
// self-health rebuild normally complete a read.
const defaultStaleBound = 2 * time.Minute

// boundFor is the stale bound of a stream whose MaxAge is maxAge: staleBound, or half the MaxAge
// when that is shorter, so a copy is never served once most of its records would have expired.
func (w *Watcher) boundFor(maxAge time.Duration) time.Duration {
	if maxAge > 0 {
		return min(w.staleBound, maxAge/2)
	}
	return w.staleBound
}

// Servable reports whether the copy can be served: nil once a read has completed and the copy
// either follows its bucket or stopped following it no longer ago than the stale bound (2 minutes,
// or half the stream's MaxAge when that is shorter), and ErrRefilling otherwise. It reads the
// watcher's state only, never the cache, so a caller may ask it under the cache's mutex.
func (w *Watcher) Servable() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.servable()
}

// servable is Servable's reading, under mu. WaitComplete takes it together with the terminal error
// and the stop, in one reading, so a copy whose bucket Check found recreated is never answered
// servable while the view behind it refuses every read.
func (w *Watcher) servable() error {
	if w.completedOnce && (w.following || w.now().Sub(w.staleSince) <= w.bound) {
		return nil
	}
	return ErrRefilling
}

// WaitComplete blocks until the copy can be served, the state Servable reports, and then returns
// nil: a view that answers no read until then (store.Registry.RolesHeldBy) gates its caller on
// this rather than on WaitReady, which a read whose scan timed out releases too. Being servable is
// not one-way -- a copy that stopped following its bucket is refused again once its stale episode
// outlasts the bound -- so this reads the state again after every wake-up and returns nil only on
// a reading that says servable; a read whose scan timed out is read again by the watcher itself
// until one does. It returns early, rather than waiting ctx out, when no read will complete: the
// current watcher's terminal error (Err), which only a Rewatch clears, and a stop, after which the
// watcher arms nothing. The error is read before servability, as RolesHeldBy reads it, so a
// watcher Check found on a recreated bucket never answers this wait nil while that view still
// refuses every read. Callers bound ctx, whose error it returns when ctx ends first: a bucket can
// be unreachable.
func (w *Watcher) WaitComplete(ctx context.Context) error {
	for {
		w.mu.RLock()
		err, stopped, servable, changed := w.err, w.stopped, w.servable(), w.completeChanged
		w.mu.RUnlock()
		switch {
		case err != nil:
			return err
		case servable == nil:
			return nil
		case stopped:
			return fmt.Errorf("%s watcher stopped before a read of its bucket completed", w.name)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// wakeComplete wakes every WaitComplete waiter, which then reads the state the caller changed.
// The caller holds mu for writing, so a waiter that took the channel before this reading of the
// state is woken, and one that takes it after reads the new state with the channel that replaces
// the closed one.
func (w *Watcher) wakeComplete() {
	close(w.completeChanged)
	w.completeChanged = make(chan struct{})
}

// Following reports whether the copy follows its bucket: a read has completed since the copy last
// stopped following (a watch installing a new read, the current watcher's own end, a deleted or
// recreated bucket a read of its stream found, a recreate Check found, or a disconnect).
func (w *Watcher) Following() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.following
}

// Progress is what one WaitCaughtUp barrier found: whether the copy caught up to the bucket
// stream's LastSeq as read at the barrier's start, the highest stream sequence this epoch's copy
// had applied, and that LastSeq.
type Progress struct {
	CaughtUp bool
	Applied  uint64
	LastSeq  uint64
}

// WaitCaughtUp reads the bucket's stream once (bus.KeyValue.ReadStreamState, bounded by
// bus.RequestTimeout and ctx), then waits at most bound for the copy to have applied a record at
// or past the stream's LastSeq, reporting whether it caught up together with the copy's applied
// sequence and LastSeq.
//
// What it proves: on CaughtUp, the copy is on the same stream incarnation the read answered for
// and either that stream held no messages (Msgs == 0, so no manifest can name a chunk) or the
// copy has applied every live record the bucket held up through LastSeq -- every Entry.Revision
// handed to Cache.Apply in this epoch, and every live key up through a completed read's own
// Read.LastSeq (which covers a tail message since purged or expired whose revision no scan
// delivers). Any record written to the bucket before the stream read is therefore in the copy.
//
// What it does not prove: it does not prove the copy still follows its bucket (the caller checks
// Following before and after), nor that no record landed after the stream read, nor that the copy
// has no lag once WaitCaughtUp returns. It reports behind (CaughtUp false, nil error) when the
// stream read names a stream other than the one the watcher reads (as confirmStream compares),
// when a new epoch installs during the wait, or when bound passes before applied reaches LastSeq.
// It returns the stream read's error when that read fails, and ctx.Err() when ctx ends the wait.
func (w *Watcher) WaitCaughtUp(ctx context.Context, bound time.Duration) (Progress, error) {
	state, err := w.KV().ReadStreamState(ctx)
	if err != nil {
		return Progress{}, err
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	expired := bound <= 0
	w.mu.Lock()
	epoch := w.epoch
	for {
		progress := Progress{Applied: w.applied, LastSeq: state.LastSeq}
		switch {
		case w.epoch != epoch || w.stream.IsZero() || !state.Created.Equal(w.stream):
			w.mu.Unlock()
			return progress, nil
		case state.Msgs == 0 || progress.Applied >= state.LastSeq:
			w.mu.Unlock()
			progress.CaughtUp = true
			return progress, nil
		case expired:
			w.mu.Unlock()
			return progress, nil
		}
		if w.appliedChanged == nil {
			w.appliedChanged = make(chan struct{})
		}
		changed := w.appliedChanged
		w.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			expired = true
		case <-ctx.Done():
			return progress, ctx.Err()
		}
		w.mu.Lock()
	}
}

// wakeApplied wakes every WaitCaughtUp waiter, under mu. appliedChanged is created only when a
// waiter blocks, so steady-state Apply calls allocate nothing.
func (w *Watcher) wakeApplied() {
	if w.appliedChanged != nil {
		close(w.appliedChanged)
		w.appliedChanged = nil
	}
}

// advanceApplied raises applied to seq for a record of the current epoch and wakes every
// WaitCaughtUp waiter, under mu.
func (w *Watcher) advanceApplied(epoch, seq uint64) {
	if epoch == w.epoch && seq > w.applied {
		w.applied = seq
		w.wakeApplied()
	}
}

// Disconnected says the connection the watcher reads through was lost. nats.go reconnects forever
// and keeps the watcher's subscription open through the outage, so the watcher does not end and
// nothing else would say its copy stopped following the bucket: the listener calls this from the
// bus's disconnect hook (bus.Client.AddDisconnectHook). The copy stops following, its stale episode
// begins unless one is under way, a read under way completes nothing, and the scheduled read is
// disarmed. The reconnect hook's Rewatch then reads the bucket again, and that read's completion
// follows it once more and arms the next scheduled read.
func (w *Watcher) Disconnected() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.disconnects++
	w.stopFollowing()
	// The reconnect hook's Rewatch reads the bucket once the connection is back, and a scheduled
	// read fired during the outage would only fail, and be armed again, until it is.
	w.clearScheduledArm()
}

// NeedsRead reports whether the copy does not follow its bucket and no read is under way to make
// it: the current generation's scan has ended or never began, and no re-read (of a timed-out scan,
// or of a recreated bucket a read of its stream found) is armed. Such a copy is repaired by nothing
// but a Rewatch, for instance when the reconnect hook's Rewatch failed while the old watcher
// survived, so the listener's self-health check reads it as a watcher to rebuild. A stopped watcher
// needs nothing.
func (w *Watcher) NeedsRead() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return !w.stopped && !w.following && !w.reading && w.rereadTimer == nil
}

// stopFollowing records that the copy no longer follows its bucket, under mu: the read under way,
// if any, can no longer restore following, and the stale episode begins unless one already has, so
// one episode runs from the first stop to the next completed read across any number of failed
// reads.
func (w *Watcher) stopFollowing() {
	w.following = false
	w.reading = false
	w.markStale()
}

// markStale begins the stale episode unless one is under way, under mu.
func (w *Watcher) markStale() {
	if w.staleSince.IsZero() {
		w.staleSince = w.now()
	}
}
