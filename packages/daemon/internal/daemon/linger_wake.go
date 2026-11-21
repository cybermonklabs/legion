package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
)

// lingerWake carries the next moment the admission block can change with no write — the earliest
// linger deadline after the clock of the last committed admission pass among the records its block
// was judged from (record.NextLingerEnd), zero where none lies ahead — from the passes that report
// it (admit.Admission.UseLingerWake) to the loop that holds the daemon's one timer for it
// (wakeAtLingerDeadlines). A pass reports once its transaction has committed, with its number:
// passes run one at a time under the fact lock, but their commit hooks run on their own goroutines
// after it, so a report older than the one held is dropped rather than put back over it.
type lingerWake struct {
	mu       sync.Mutex
	pass     uint64
	deadline time.Time
	// moved wakes the loop to read the deadline again, one wake waiting at most.
	moved chan struct{}
}

func newLingerWake() *lingerWake {
	return &lingerWake{moved: make(chan struct{}, 1)}
}

// arm is a committed pass's report: deadline, the zero time where no linger the block reads runs
// out after the pass's clock.
func (l *lingerWake) arm(pass uint64, deadline time.Time) {
	l.mu.Lock()
	if pass < l.pass {
		l.mu.Unlock()
		return
	}
	l.pass, l.deadline = pass, deadline
	l.mu.Unlock()
	select {
	case l.moved <- struct{}{}:
	default:
	}
}

// next is the deadline the newest report named.
func (l *lingerWake) next() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.deadline
}

// fired takes deadline off once its timer has fired, unless a later report has named another one
// meanwhile: the pass the timer applies reports the deadline after it, and a pass that fails to
// apply leaves no deadline to fire on again at once, the next pass of any fact reporting one.
func (l *lingerWake) fired(deadline time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deadline.Equal(deadline) {
		l.deadline = time.Time{}
	}
}

// wakeAtLingerDeadlines holds the daemon's one timer for the clock's own change of the admission
// block, until ctx ends. The block's held list reads a root's linger against the pass's clock
// (record.HeldReasonOf's tree_closed word), so it changes at a linger's deadline with no write, and
// an admission pass runs only on a fact: the timer is armed for the deadline the newest pass
// reported (lingerWake), moved whenever a pass reports an earlier or a later one, and armed for none
// while none lies ahead, and at the deadline it applies an admission pass (applyLingerDeadline),
// which writes the block's change and queues its state event then, with no fact arriving and no
// tick. The pass it applies reports the next deadline in turn. A wake at a known time, and no poll:
// nothing runs between two deadlines.
func (w *workflowRuntime) wakeAtLingerDeadlines(ctx context.Context) {
	// A runtime openWorkflow did not build, a test's, registered no wake: no pass reports to it.
	if w.lingerWake == nil {
		return
	}
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var armed time.Time
	for {
		if next := w.lingerWake.next(); !next.Equal(armed) {
			armed = next
			timer.Stop()
			if !armed.IsZero() {
				timer.Reset(time.Until(armed))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-w.lingerWake.moved:
		case at := <-timer.C:
			w.lingerWake.fired(armed)
			armed = time.Time{}
			w.applyLingerDeadline(ctx, at)
		}
	}
}

// applyLingerDeadline applies one intake.LingerDeadline, whose admission pass writes the block the
// clock moved. Its event id is its own — the word, the project, this boot and the moment the timer
// fired — so a deadline the timer fires on twice, a pass that read a clock just short of it having
// reported it again, is applied twice and never taken for a duplicate. A failed apply is logged; the
// next pass of any fact reports the deadline again.
func (w *workflowRuntime) applyLingerDeadline(ctx context.Context, at time.Time) {
	eventID := fmt.Sprintf("linger-deadline:%s:%s:%d", w.dispatchProject, w.bootID, at.UnixNano())
	if _, err := intake.ApplyFact(ctx, w.pool, "linger-deadline", eventID, intake.LingerDeadline{}, w.handlers...); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("apply the admission pass at a linger deadline failed", "error", err)
		}
	}
}
