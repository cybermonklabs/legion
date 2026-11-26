package daemon

import (
	"context"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
)

// passLoops is every periodic loop of the daemon, as observedPass names it: each has a timer of
// its own and a trigger of its own (passTriggers) — on the daemon's clock for the loops that wait
// through a passTimer, and on the outbox's own timers for the merge-queue reads' retry
// (passMergeQueueRetry).
var passLoops = []passLoop{
	passIdle, passWakes, passOrphans, passUnfinished, passControllerKeeper, passControllerWatch,
	passControllerTick, passEntrySweep, passMergeState, passMergeQueueRetry, passHoldRelease, passReconnect,
}

// passTriggers is how a test runs the pass of a periodic loop it needs, rather than shortening
// that loop's interval: one channel of one slot for each loop of passLoops, which the loop waits
// on beside its timer (passTimer). overrides.passes is nil in production, where a pass runs on
// the clock alone, and a nil *passTriggers hands every loop a nil channel, which never yields.
type passTriggers struct {
	loops map[passLoop]chan struct{}
}

// newPassTriggers is a trigger for every loop.
func newPassTriggers() *passTriggers {
	loops := make(map[passLoop]chan struct{}, len(passLoops))
	for _, loop := range passLoops {
		loops[loop] = make(chan struct{}, 1)
	}
	return &passTriggers{loops: loops}
}

// at is the channel loop waits on beside its timer: nil where no test set any trigger.
func (p *passTriggers) at(loop passLoop) <-chan struct{} {
	if p == nil {
		return nil
	}
	return p.channel(loop)
}

// fire asks loop for a pass, and never blocks: a pass already asked for and not yet begun is the
// same pass.
func (p *passTriggers) fire(loop passLoop) { nudge(p.channel(loop)) }

// channel is loop's own trigger. A name passLoops does not hold is a mistake in this package, not
// a thing to wait for: a test that asked for a pass of it would wait for a pass nothing runs.
func (p *passTriggers) channel(loop passLoop) chan struct{} {
	trigger, named := p.loops[loop]
	if !named {
		panic("daemon: no pass trigger for the loop " + string(loop))
	}
	return trigger
}

// passTimer is what a periodic loop of the daemon waits on between its passes: its timer on the
// daemon's clock (plan.clock, the wall clock in production), armed again for each pass, and
// beside it the test's trigger for that loop, which production never has. Every loop waits
// through next, so a test runs the pass it needs now and none shortens a production interval.
type passTimer struct {
	timer clock.Timer
	every time.Duration
	wake  <-chan struct{}
}

// newPassTimer arms the timer at the first wait's deadline. After a timer or test trigger wakes
// the loop, next resets it to every. The controller tick has a shorter first wait. Arming
// the timer for every before resetting it to first exposed the wrong pending deadline.
// passes is nil in production.
func newPassTimer(clk clock.Clock, passes *passTriggers, loop passLoop, first, every time.Duration) *passTimer {
	return &passTimer{timer: clk.NewTimer(first), every: every, wake: passes.at(loop)}
}

// next waits for the loop's next pass — its interval passing, or a test's trigger — and reports
// false once ctx ends, which is the loop's own end. The timer is armed again for the whole
// interval from here, so a pass a test ran puts off the one the clock owed.
func (p *passTimer) next(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-p.timer.C():
	case <-p.wake:
	}
	p.timer.Reset(p.every)
	return true
}

// stop releases the timer, which a loop defers.
func (p *passTimer) stop() { p.timer.Stop() }
