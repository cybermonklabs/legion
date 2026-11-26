package admit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The project's one deadline of admission by signals (S5): from the inputs each fact committed,
// the moment the earliest series with a limit would next cross it with no new event
// (LiveReading.Deadline). It is computed again at each pass and at each report of model requests
// and turn edge, written in that fact's transaction under a fence that rises at each change
// (nextDeadline), and armed once the fact commits (UseLiveSignals' arm, DeadlineTimer). Its firing
// is intake.SignalDeadline, carrying the fence, which runs one pass of a deadline where it is
// still the committed deadline and due (deadlineFires), and that pass computes the deadline again.
// Nothing fires on a period, and a fact rolled back neither writes nor arms anything.

// SignalHeldStartLine is the line of each start a signal held at a pass (S4), once its fact
// commits: the issue, the signal and part (store.SignalPart), its value and its limit.
const SignalHeldStartLine = "admission: a signal held a start"

// UseLiveSignals registers live, the daemon's live signals whose reader is the pass's gate
// (UseSignals), for what admission keeps beside each pass: the record of changes of what the pass
// computed of each series (writeSignalChanges), whose newest entries the architect's rule for a
// report of model requests and a turn edge reads (signalsMoved), and the project's one deadline,
// which each pass computes again (commitDeadline) and hands to arm once the fact that changed it
// commits. A firing of the deadline (intake.SignalDeadline) runs a pass only where it is still the
// committed deadline and due. It is a setter rather than a parameter of New for the reason
// UseExperiments is.
func (a *Admission) UseLiveSignals(live *LiveSignals, arm func(record.SignalDeadline)) {
	a.live, a.armDeadline = live, arm
}

// nextDeadline is the deadline committed after a fact whose pass computed due (set false for none)
// over committed, and whether it changed: the same where it stands as computed, and otherwise the
// computed one under a fence one higher, which no firing of an earlier deadline names.
func nextDeadline(committed record.SignalDeadline, due time.Time, set bool) (record.SignalDeadline, bool) {
	next := record.SignalDeadline{Due: due, Set: set, Fence: committed.Fence + 1}
	if !set {
		next.Due = time.Time{}
	}
	if committed.Set == next.Set && committed.Due.Equal(next.Due) {
		return committed, false
	}
	return next, true
}

// deadlineFires says whether firing, a timer's intake.SignalDeadline, is the committed deadline's
// and due at now (its fence and moment are the committed ones, and now is at or after it), and
// whether it is the committed deadline's and early, which runs no pass and is left for its timer
// to fire again.
func deadlineFires(committed record.SignalDeadline, firing intake.SignalDeadline, now time.Time) (fires, early bool) {
	if !committed.Set || committed.Fence != firing.Fence || !committed.Due.Equal(firing.At) {
		return false, false
	}
	if now.Before(firing.At) {
		return false, true
	}
	return true, false
}

// deadlineRuns says whether fact, a firing of the deadline, runs its pass (deadlineFires) where
// the daemon keeps the deadline (UseLiveSignals), and whether it is early. Where admission keeps
// none, every firing runs its pass, as a replay's would.
func (a *Admission) deadlineRuns(ctx context.Context, tx pgx.Tx, fact intake.SignalDeadline) (runs, early bool, err error) {
	if a.live == nil {
		return true, false, nil
	}
	committed, err := a.store.SignalDeadline(ctx, tx, a.project)
	if err != nil {
		return false, false, err
	}
	runs, early = deadlineFires(committed, fact, a.now())
	if !runs {
		a.log.Info("admission: a firing of the signal deadline is not the committed one or not due, and runs no pass",
			"at", fact.At, "fence", fact.Fence, "committed", committed.Due, "committedFence", committed.Fence, "early", early)
	}
	return runs, early, nil
}

// commitDeadline writes the deadline reading computes (LiveReading.Deadline) where it differs from
// the committed one (nextDeadline), in the fact's transaction, and arms it once that commits.
func (a *Admission) commitDeadline(ctx context.Context, tx pgx.Tx, reading LiveReading) error {
	crossing, due := reading.Deadline()
	committed, err := a.store.SignalDeadline(ctx, tx, a.project)
	if err != nil {
		return err
	}
	next, changed := nextDeadline(committed, crossing.At, due)
	if !changed {
		return nil
	}
	if err := a.store.PutSignalDeadline(ctx, tx, a.project, next); err != nil {
		return err
	}
	arm := a.armDeadline
	store.OnCommit(ctx, func() {
		if arm != nil {
			arm(next)
		}
	})
	return nil
}

// signalFactMoved is the architect's rule for a fact that keeps an input of a signal with no mark
// of its own (a report of model requests, a turn edge): it reads the live signals at the
// admission's clock, sets the deadline again from them, and keeps the mark that runs the pass
// (record.NoteSignalsMoved) only where they moved against the last committed pass, the record's
// newest entries of each series (signalsMoved). A pass it runs computes the deadline again itself.
// Where admission keeps no live signals, it keeps nothing.
func (a *Admission) signalFactMoved(ctx context.Context, tx pgx.Tx) error {
	if a.live == nil {
		return nil
	}
	reading, err := a.live.Reading(ctx, tx, a.now())
	if err != nil {
		return fmt.Errorf("read the signals at a report: %w", err)
	}
	newest, err := store.NewestSeriesEntriesTx(ctx, tx, a.project)
	if err != nil {
		return err
	}
	if signalsMoved(reading, newest) {
		record.NoteSignalsMoved(ctx)
		return nil
	}
	return a.commitDeadline(ctx, tx, reading)
}

// afterSignalPass is what a pass that judged the line under gate keeps once it is done: the line
// of each start a signal held (S4), and, where admission keeps the live signals, the record of
// changes of what the pass computed from the reading its gate read (writeSignalChanges), and the
// deadline computed again from the inputs as the pass left them, its grants and releases among
// them.
func (a *Admission) afterSignalPass(ctx context.Context, tx pgx.Tx, gate passGate, moves *passMoves) error {
	a.logSignalHolds(ctx, gate, moves)
	if a.live == nil {
		return nil
	}
	read, ok := passReadingOf(ctx)
	if !ok {
		return fmt.Errorf("the %s pass keeps the live signals, and its gate read none of them: register the live signals' Read as its reader (UseSignals)", gate.kind)
	}
	if err := a.writeSignalChanges(ctx, tx, read, len(moves.given) > 0 || len(moves.taken) > 0); err != nil {
		return err
	}
	reading := LiveReading{}
	if gate.bySignals {
		var err error
		if reading, err = a.live.Reading(ctx, tx, a.now()); err != nil {
			return fmt.Errorf("read the signals after the %s pass: %w", gate.kind, err)
		}
	}
	return a.commitDeadline(ctx, tx, reading)
}

// logSignalHolds writes, once the fact commits, one line for each series that held each start the
// pass's last round passed over for a signal (record.PassedSignal): the issue, the signal and its
// part, its value and its limit (Snapshot.Holding).
func (a *Admission) logSignalHolds(ctx context.Context, gate passGate, moves *passMoves) {
	if !gate.bySignals {
		return
	}
	for _, passed := range moves.passed {
		if passed.Reason != record.PassedSignal {
			continue
		}
		for _, name := range gate.snapshot.Holding() {
			reading := gate.snapshot.Readings[name]
			signal, part := store.SignalPart(name)
			a.logOnCommit(ctx, SignalHeldStartLine, "issue", passed.Issue, "signal", signal, "part", partOrNone(part),
				"value", reading.Value.Value, "limit", *reading.Limit, "kind", gate.kind)
		}
	}
}

// DeadlineTimer is a project's one timer of its deadline of admission by signals: one replaceable
// one-shot timer, armed for the committed deadline (Arm) and firing fire at its moment, which the
// daemon's runtime builds at each boot from the committed deadline and stops at its shutdown
// (Stop). A deadline of a fence below the one armed is a stale callback and replaces nothing.
type DeadlineTimer struct {
	now   func() time.Time
	after func(time.Duration, func()) func() bool
	fire  func(record.SignalDeadline)

	mu      sync.Mutex
	armed   record.SignalDeadline
	stop    func() bool
	stopped bool
}

// NewDeadlineTimer is a timer that fires fire at each deadline it is armed for, on the wall clock.
func NewDeadlineTimer(fire func(record.SignalDeadline)) *DeadlineTimer {
	return newDeadlineTimer(time.Now, func(delay time.Duration, run func()) func() bool { return time.AfterFunc(delay, run).Stop }, fire)
}

// newDeadlineTimer is NewDeadlineTimer on a clock and a one-shot timer of the caller's.
func newDeadlineTimer(now func() time.Time, after func(time.Duration, func()) func() bool, fire func(record.SignalDeadline)) *DeadlineTimer {
	return &DeadlineTimer{now: now, after: after, fire: fire}
}

// Arm replaces the timer with one for deadline, or with none where deadline is unset. A deadline
// whose fence is below the one armed is stale and changes nothing, and nothing is armed after
// Stop. The same deadline armed again, as after an early firing, fires again.
func (t *DeadlineTimer) Arm(deadline record.SignalDeadline) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || deadline.Fence < t.armed.Fence {
		return
	}
	if t.stop != nil {
		t.stop()
		t.stop = nil
	}
	t.armed = deadline
	if !deadline.Set {
		return
	}
	t.stop = t.after(max(deadline.Due.Sub(t.now()), 0), func() {
		t.mu.Lock()
		current := !t.stopped && t.armed == deadline
		t.mu.Unlock()
		if current {
			t.fire(deadline)
		}
	})
}

// Stop cancels the timer, and every deadline armed after it.
func (t *DeadlineTimer) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	if t.stop != nil {
		t.stop()
		t.stop = nil
	}
}
