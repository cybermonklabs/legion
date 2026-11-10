// Package exp20 is experiment 20 of the Temporal evaluation: experiment 19's handout,
// held open for minutes rather than seconds.
//
// Experiment 19 measured what Temporal does with an activity whose body returned
// activity.ErrResultPending -- the worker that took it killed, the activity completed
// from a process holding only its task token, a superseded attempt's token refused,
// and a bound nobody answered sat through. Every one of those readings came from a
// run of seconds, and Legion's unit of work is a prompt into a live session that can
// take hours. A claim about hours must not rest on a run of seconds.
//
// So this experiment does the one thing experiment 19 did not. It hands one activity
// out, kills the worker that took it, and then leaves it alone for five minutes of
// real time: no worker polls its task queue, nothing heartbeats it, nothing completes
// it. The server is asked every thirty seconds what it holds, and every one of those
// readings is printed, so a reader sees the series rather than only its ends. The
// activity carries no heartbeat timeout at all and a start-to-close timeout of ten
// minutes, so the five minutes sit inside the one bound the server was told to
// enforce, and anything that happened to the handout came from somewhere else.
//
// The question is therefore a single one: does anything other than the start-to-close
// timeout reap or re-dispatch a handed-out activity whose worker is dead?
//
// What the handout is made of is experiment 19's and is imported, not repeated: the
// workflow, the activity, the worker process that is killed, the completer process
// that holds nothing but the task token, and the readings taken of the server. This
// package holds only what it measures differently.
package exp20

import (
	"fmt"
	"time"
)

// The experiment's timings. Minutes rather than seconds is the whole point of this
// experiment, so the hold is real time that nothing shortens.
const (
	dialTimeout = 30 * time.Second

	// handoutWindow is the StartToCloseTimeout of the handout: the one bound the
	// server is told to enforce on it. Ten minutes leaves the five-minute hold and
	// the completion after it well inside, so nothing the run sees is this bound.
	handoutWindow = 10 * time.Minute
	// holdWindow is how long the handed-out activity is left with no worker alive
	// and nothing touching it. It is five minutes of real time: the reading this
	// experiment exists for.
	holdWindow = 5 * time.Minute
	// pollInterval is how often the server is asked what it holds during the hold.
	// Thirty seconds is often enough to show a change and rare enough that the
	// series fits on a page.
	pollInterval = 30 * time.Second
	// pollsPerHold is how many readings that makes, which the verdict checks were
	// all taken: a hold with readings missing from it measures less than it says.
	pollsPerHold = int(holdWindow / pollInterval)

	// workflowTimeout must outlast the handout's own bound, or the workflow would
	// expire before the thing being measured does.
	workflowTimeout = 20 * time.Minute
	workerStartWait = 40 * time.Second
	bodyWait        = 30 * time.Second
	completerWait   = 30 * time.Second
	workflowWait    = 60 * time.Second
)

// poll is one reading of the handed-out activity, taken while nobody holds it: what
// the server says about the activity, what its history has recorded, and how many
// times the activity's body has run.
type poll struct {
	n       int
	elapsed time.Duration
	pending bool
	state   string
	attempt int32
	starts  int
	lines   int
}

// line is the reading as the transcript prints it: one line per poll, so the series
// can be read down the page.
func (p poll) line() string {
	return fmt.Sprintf("elapsed=%s pending=%t state=%s attempt=%d activity-task-started-events=%d side-effect-file-lines=%d",
		p.elapsed.Round(time.Second), p.pending, p.state, p.attempt, p.starts, p.lines)
}

// same reports whether two readings say the same thing about the activity. The series
// exists to show that nothing changed across it, and "nothing changed" is computed
// rather than eyed.
func (p poll) same(o poll) bool {
	return p.pending == o.pending &&
		p.state == o.state &&
		p.attempt == o.attempt &&
		p.starts == o.starts &&
		p.lines == o.lines
}
