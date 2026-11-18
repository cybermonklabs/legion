package daemon

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// observer is a test's observer of the daemon (overrides.observe, or outbox.observe for an outbox
// a test builds by hand): it appends every report to its stream and returns at once, so a slow test
// never holds the daemon, and the test waits on the stream for the report it needs (testwait.Next).
// The stream is read in order, a wait passing over the reports before the one it returns, so a test
// hooks its observer before the work it waits for begins, waits for one report before a later one,
// and waits for reports that may come in either order in one wait (settled takes several rows).
type observer struct {
	events testwait.Stream[observation]
}

func newObserver() *observer { return &observer{} }

// observe is the daemon's observer: it appends e and returns.
func (o *observer) observe(e observation) { o.events.Add(e) }

// observing hooks a new observer into ov and returns it. An observer ov already had goes on hearing
// every report, after the new one.
func observing(ov *overrides) *observer {
	obs := newObserver()
	if prev := ov.observe; prev != nil {
		ov.observe = func(e observation) {
			obs.observe(e)
			prev(e)
		}
	} else {
		ov.observe = obs.observe
	}
	return obs
}

// runReturned is the rig's own mark in an observer's stream, never a report of the daemon's: run
// returned err while the test waited for the daemon to be ready (readyOrReturned).
type runReturned struct{ err error }

func (runReturned) isObservation() {}

// readyOrReturned waits for whichever comes first, the daemon's report that it is ready to serve
// (observedReady), for which it returns false, or run's return, for which it returns true and
// run's result. done is the channel of one slot run's result is sent on: a result this wait takes
// is put back, for whoever stops the daemon.
func (o *observer) readyOrReturned(t *testing.T, done chan error) (returned bool, err error) {
	t.Helper()
	waiting := make(chan struct{})
	defer close(waiting)
	go func() {
		select {
		case err := <-done:
			done <- err
			o.events.Add(runReturned{err})
		case <-waiting:
		}
	}()
	switch e := testwait.Next(t, "the daemon's report that it is ready to serve, or its run's return", &o.events, func(e observation) bool {
		switch e.(type) {
		case observedReady, runReturned:
			return true
		}
		return false
	}).(type) {
	case runReturned:
		return true, e.err
	default:
		return false, nil
	}
}

// ready waits for the daemon's report that it is ready to serve (observedReady), and fails at once
// when run returns first; done is as readyOrReturned takes it.
func (o *observer) ready(t *testing.T, done chan error) {
	t.Helper()
	if returned, err := o.readyOrReturned(t, done); returned {
		t.Fatalf("the daemon's run returned before it was ready to serve: %v", err)
	}
}

// next waits for the next report match accepts (testwait.Next), and fails at once when the stream
// says run returned (runReturned) before it.
func (o *observer) next(t *testing.T, what string, match func(observation) bool) observation {
	t.Helper()
	return testwait.Next(t, what, &o.events, func(e observation) bool {
		if returned, ok := e.(runReturned); ok {
			t.Fatalf("the daemon's run returned before %s: %v", what, returned.err)
		}
		return match(e)
	})
}

// pass waits for the end of the next pass of loop (observedPass) and returns it.
func (o *observer) pass(t *testing.T, loop passLoop) observedPass {
	t.Helper()
	return o.next(t, fmt.Sprintf("the end of a pass of the daemon's %s loop", loop), func(e observation) bool {
		p, ok := e.(observedPass)
		return ok && p.loop == loop
	}).(observedPass)
}

// admissionPasses waits for the next n admission passes (observedAdmissionPass) of the facts match
// accepts, and returns those facts in order; a nil fact is a reconcile of Dispatch's listing.
// facts says which facts match accepts, for the wait's message.
func (o *observer) admissionPasses(t *testing.T, facts string, n int, match func(intake.Fact) bool) []intake.Fact {
	t.Helper()
	passed := make([]intake.Fact, 0, n)
	for len(passed) < n {
		what := fmt.Sprintf("the daemon's report of admission pass %d of %d of %s", len(passed)+1, n, facts)
		pass := o.next(t, what, func(e observation) bool {
			p, ok := e.(observedAdmissionPass)
			return ok && match(p.fact)
		}).(observedAdmissionPass)
		passed = append(passed, pass.fact)
	}
	return passed
}

// rowOf picks the settlements (observedOutboxSettled) a wait is for: of the row id names (0 is any
// row), of issue ("" is any issue), of kind ("" is any kind), and with outcome ("" is any outcome).
type rowOf struct {
	id      int64
	issue   string
	kind    record.OutboxKind
	outcome settlement
}

func (r rowOf) matches(s observedOutboxSettled) bool {
	return (r.id == 0 || s.row == r.id) && (r.issue == "" || s.issue == r.issue) &&
		(r.kind == "" || s.kind == r.kind) && (r.outcome == "" || s.outcome == r.outcome)
}

// String names the row and what it is to come to, for a wait's message: "LEGION-208's
// dispatch_message row settled as finished", "row 12 (a supervise row of LEGION-208) settled".
func (r rowOf) String() string {
	var row string
	switch {
	case r.issue != "" && r.kind != "":
		row = fmt.Sprintf("%s's %s row", r.issue, r.kind)
	case r.issue != "":
		row = "a row of " + r.issue
	case r.kind != "":
		row = fmt.Sprintf("a %s row", r.kind)
	}
	switch {
	case r.id != 0 && row != "":
		row = fmt.Sprintf("row %d (%s)", r.id, row)
	case r.id != 0:
		row = fmt.Sprintf("row %d", r.id)
	case row == "":
		row = "a row"
	}
	if r.outcome == "" {
		return row + " settled"
	}
	return fmt.Sprintf("%s settled as %s", row, r.outcome)
}

// settled waits for one settlement (observedOutboxSettled) of each of rows, in whatever order they
// come, and returns them in the order of rows. A settlement that two of rows match is taken for
// the first of them still waiting.
func (o *observer) settled(t *testing.T, rows ...rowOf) []observedOutboxSettled {
	t.Helper()
	got := make([]observedOutboxSettled, len(rows))
	waiting := make([]int, len(rows))
	for i := range rows {
		waiting[i] = i
	}
	for len(waiting) > 0 {
		clauses := make([]string, len(waiting))
		for at, i := range waiting {
			clauses[at] = rows[i].String()
		}
		var matched int
		s := o.next(t, "the outbox's report that "+strings.Join(clauses, ", and that "), func(e observation) bool {
			s, ok := e.(observedOutboxSettled)
			if !ok {
				return false
			}
			for at, i := range waiting {
				if rows[i].matches(s) {
					matched = at
					return true
				}
			}
			return false
		}).(observedOutboxSettled)
		got[waiting[matched]] = s
		waiting = slices.Delete(waiting, matched, matched+1)
	}
	return got
}

// stranded waits for the boot's report of the claims nothing will wake to finish
// (observedStranded).
func (o *observer) stranded(t *testing.T) {
	t.Helper()
	o.next(t, "the boot's report of the claims nothing will wake", func(e observation) bool {
		_, ok := e.(observedStranded)
		return ok
	})
}
