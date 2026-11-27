package testwait

import (
	"context"
	"sync"
	"time"
)

// StandInDeadline is a context that stands in, in a test, for one a deadline ends — the
// context.WithTimeout or context.WithTimeoutCause that production code runs a command or an
// attempt under — where the test decides the moment its deadline passes: a fact of the run under
// test, such as the end of the process group a probe attempt started, rather than a time a loaded
// machine can reach before that run has started. It ends as the real one does, so code that tells
// one end from another reads the same answer from both:
//
//   - pass ends it as its deadline passing does: its Err is context.DeadlineExceeded, and
//     context.Cause reads cause, of it and of every context derived from it (context.DeadlineExceeded
//     where cause is nil, as context.WithTimeout's);
//   - cancel ends it as the real one's CancelFunc does: Err and cause context.Canceled;
//   - where parent ends first, it ends as parent did: parent's Err, and parent's cause. It reads that
//     end at once, as a context the standard library derives from parent does: Done, Err, Value
//     (and so context.Cause) each look at parent first, so a stand-in made under a parent that has
//     ended, or read right after its parent ended, is ended already, and a command started under it
//     never starts.
//
// The first end is the one it keeps; a later pass or cancel changes nothing. It carries parent's
// values. Its Deadline is parent's until pass ends it, and from then the moment pass ended it: the
// moment its own deadline passes is a fact, not a time known when it is made, and once it has
// passed, a caller that reads the time against Deadline reads it past, as against a real one.
// This is the one place it differs from a real deadline on purpose, whose Deadline never changes.
//
// Every test clock that stands in for a production limit, budget or deadline uses it
// (docs/testing.md). A clock built on context.WithCancelCause keeps the cause but gives Err
// context.Canceled, which no real limit gives, so code that tells one end from another by Err
// passes a test it should fail.
func StandInDeadline(parent context.Context) (ctx context.Context, pass func(cause error), cancel context.CancelFunc) {
	// causes holds the cause: context.Cause finds a cause only in the context the standard
	// library's own cancel records it in, which it looks up through Value. It is cut from parent's
	// cancellation (WithoutCancel), so it ends only as this context ends, with this context's cause.
	causes, record := context.WithCancelCause(context.WithoutCancel(parent))
	d := &standInDeadline{parent: parent, causes: causes, record: record, done: make(chan struct{})}
	// A context derived from d, or a select already waiting on Done, learns parent's end when d
	// ends, which this does for those that do not ask d first.
	stop := func() bool { return false } // CONTROL ONLY: the parent's end not observed
	// pass and cancel look at parent first too: a parent that ended before them ended d then.
	pass = func(cause error) {
		if cause == nil {
			cause = context.DeadlineExceeded
		}
		d.endWithParent()
		d.end(context.DeadlineExceeded, cause, time.Now())
		stop()
	}
	cancel = func() {
		d.endWithParent()
		d.end(context.Canceled, context.Canceled, time.Time{})
		stop()
	}
	return d, pass, cancel
}

// standInDeadline is StandInDeadline's context. Its Done is a channel of its own, not causes',
// so a context derived from it does not take causes for its parent (the standard library's
// parentCancelCtx compares the two channels) and learns its end the way it learns any other
// context's: from Err and context.Cause once Done is closed.
type standInDeadline struct {
	parent   context.Context
	causes   context.Context
	record   context.CancelCauseFunc
	done     chan struct{}
	mu       sync.Mutex
	err      error
	passedAt time.Time
}

func (d *standInDeadline) Deadline() (time.Time, bool) {
	d.mu.Lock()
	passedAt := d.passedAt
	d.mu.Unlock()
	if !passedAt.IsZero() {
		return passedAt, true
	}
	return d.parent.Deadline()
}

func (d *standInDeadline) Done() <-chan struct{} {
	d.endWithParent()
	return d.done
}

func (d *standInDeadline) Value(key any) any {
	d.endWithParent()
	return d.causes.Value(key)
}

func (d *standInDeadline) Err() error {
	d.endWithParent()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// endWithParent ends d as parent ended, where parent has ended and d has not.
func (d *standInDeadline) endWithParent() {
	if err := d.parent.Err(); err != nil {
		d.end(err, context.Cause(d.parent), time.Time{})
	}
}

// end ends d with err and cause, and with passedAt as the moment its deadline passed (zero for an
// end that is no pass), unless d has ended already. The cause and the moment are recorded before
// Done is closed, so whoever Done wakes reads both.
func (d *standInDeadline) end(err, cause error, passedAt time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return
	}
	d.record(cause)
	d.err = err
	d.passedAt = passedAt
	close(d.done)
}
