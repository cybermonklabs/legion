package daemon

import (
	"fmt"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// passer runs the pass of a periodic loop a test needs, in place of shortening that loop's
// interval: it holds the daemon's triggers (passTriggers) and hears, through an observer of its
// own, the end of every pass the daemon runs (observedPass). pass both asks for a pass and waits
// for the report that it ended, which is when what the pass did can be read.
//
// A test hooks one before the daemon starts — triggering for a daemon a test boots through
// overrides, triggeringRuntime or triggeringPlan for a runtime or a supervision a test builds by
// hand — and every pass it asks for afterwards runs while the loop's own interval, the production
// one, stands. A passer counts the reports it has heard rather than reading the loops' own pass
// numbers, so a test that stops a daemon and boots another on the same overrides goes on waiting
// for the passes of the new daemon's loops, which number from one again.
type passer struct {
	triggers *passTriggers
	mu       sync.Mutex
	// heard is how many passes of each loop this passer has been told of.
	heard   map[passLoop]uint64
	reports testwait.Stream[passReport]
}

// passReport is one pass this passer heard of, and which of that loop's reports it was.
type passReport struct {
	pass  observedPass
	heard uint64
}

// triggering hooks a trigger for every periodic loop of the daemon into ov, with an observer of
// its own to hear the passes they run. An observer ov already had goes on hearing every report.
func triggering(ov *overrides) *passer {
	p := newPasser()
	ov.passes = p.triggers
	ov.observe = p.hook(ov.observe)
	return p
}

// triggeringRuntime is triggering for a workflow runtime a test built by hand, whose loops it
// runs itself (tickEntrySweep, pollHoldReleaseWith, reconnectWatchdogWith, climbMergeState).
func triggeringRuntime(w *workflowRuntime) *passer {
	p := newPasser()
	w.passes = p.triggers
	w.observe = p.hook(w.observe)
	return p
}

// triggeringPlan is triggering for a supervision a test built by hand, whose periodic loops it
// runs itself (reconcileOrphans, relaunchUnfinished, watchController, the controller keeper).
func triggeringPlan(plan *plan) *passer {
	p := newPasser()
	plan.passes = p.triggers
	plan.observe = p.hook(plan.observe)
	return p
}

func newPasser() *passer {
	return &passer{triggers: newPassTriggers(), heard: map[passLoop]uint64{}}
}

// hook is the observer the daemon reports to: it records each pass as this passer's next of that
// loop, and hands every report on to prev, the observer the test already had, if any.
func (p *passer) hook(prev func(observation)) func(observation) {
	return func(e observation) {
		if pass, ok := e.(observedPass); ok {
			p.mu.Lock()
			p.heard[pass.loop]++
			report := passReport{pass: pass, heard: p.heard[pass.loop]}
			p.mu.Unlock()
			p.reports.Add(report)
		}
		if prev != nil {
			prev(e)
		}
	}
}

// pass runs one pass of loop: it asks the loop for one and waits for the report that that pass
// ended. A pass reported before this call is passed over, so what it returns is a pass that ran
// after the ask.
func (p *passer) pass(t *testing.T, loop passLoop) observedPass {
	t.Helper()
	after := p.sofar(loop)
	p.triggers.fire(loop)
	return p.await(t, loop, after, fmt.Sprintf("the end of the pass of the daemon's %s loop this test ran", loop))
}

// passes runs n passes of loop, one after another.
func (p *passer) passes(t *testing.T, loop passLoop, n int) {
	t.Helper()
	for range n {
		p.pass(t, loop)
	}
}

// ask asks loop for a pass and returns at once, for a test whose subject is what a pass does
// while it runs: a pass that never ends reports nothing, so no wait for its end could return. A
// pass asked for before its loop has started runs as soon as it does.
func (p *passer) ask(loop passLoop) { p.triggers.fire(loop) }

// next waits for the end of loop's next pass without asking for one: the pass a loop runs of its
// own accord, such as a merge-state ladder's first read.
func (p *passer) next(t *testing.T, loop passLoop) observedPass {
	t.Helper()
	return p.await(t, loop, p.sofar(loop), fmt.Sprintf("the end of the daemon's next %s pass", loop))
}

// until runs passes of loop, one at a time, until holds reports true, and returns the last of
// them: the wait for a fact whose pass a test cannot count, such as a release that waits on what
// JetStream reports of a consumer. what names the fact, for the wait's failure, and the pass
// returned says how many passes of that loop the daemon had run by then.
func (p *passer) until(t *testing.T, loop passLoop, what string, holds func() bool) observedPass {
	t.Helper()
	var last observedPass
	testwait.Eventually(t, what, func() bool {
		last = p.pass(t, loop)
		return holds()
	})
	return last
}

// sofar is how many passes of loop this passer has heard of.
func (p *passer) sofar(loop passLoop) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.heard[loop]
}

// await waits for the report after this passer's after-th of loop.
func (p *passer) await(t *testing.T, loop passLoop, after uint64, what string) observedPass {
	t.Helper()
	return testwait.Next(t, what, &p.reports, func(r passReport) bool {
		return r.pass.loop == loop && r.heard > after
	}).pass
}
