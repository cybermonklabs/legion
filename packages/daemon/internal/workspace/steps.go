package workspace

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Steps is the step clock of one provision: it splits the provision's run into named steps, one
// open at a time, and logs each step's duration as the step ends, so the log says where the time
// went and, while the provision still runs, which step it is in (the step after the last one
// logged). workspace-init's provision keeps one (cmd/legion's workspaceInit), from its start to its
// end, and hands it to Provision (Request.Steps), which hands it to the issue pod's fill (remote,
// CacheStream); each place that starts a step begins it by a name fixed where it opens, so a name
// says what runs in that step on every path that reaches it. The tmux runtime keeps none: a nil
// *Steps does nothing, and its provision's log gains no line.
//
// Each boundary reads the clock once, and that one instant ends the open step and starts the next,
// so every instant from NewSteps to End or Fail belongs to exactly one step, and the durations the
// steps log add up to the total End logs: each is printed in seconds with one decimal, so the
// printed ones add up to the printed total within 0.05 s for each of them and the total.
//
// One provision's steps run one after another, on its one goroutine; a Steps is not for use from
// two at once.
type Steps struct {
	clock Clock
	log   func(string)
	// name is the open step's, and opened when it opened; started is when the first step opened.
	name    string
	opened  time.Time
	started time.Time
	// ended is set by End and by Fail, after which nothing more is logged.
	ended bool
}

// firstStep is the step NewSteps opens: the checks of a provision's inputs before it touches the
// volume (cmd/legion's provisionInputs).
const firstStep = "inputs"

// stepName is the shape of a step's name: lower-case words joined by hyphens.
var stepName = regexp.MustCompile(`^[a-z]+(?:-[a-z]+)*$`)

// NewSteps is a step clock on clock, whose lines go to log, with its first step, inputs, open at
// clock's now.
func NewSteps(clock Clock, log func(string)) *Steps {
	now := clock.Now()
	return &Steps{clock: clock, log: log, name: firstStep, opened: now, started: now}
}

// Begin ends the open step, logging `step <its name> took <seconds>s`, and opens the step name at
// the same instant. A name that is not lower-case words joined by hyphens is a mistake in the code
// that names it, and panics, on a nil *Steps too, so the first test that runs that code names it.
func (s *Steps) Begin(name string) {
	if !stepName.MatchString(name) {
		panic(fmt.Sprintf("workspace: the step name %q is not lower-case words joined by hyphens", name))
	}
	if s == nil || s.ended {
		return
	}
	now := s.clock.Now()
	s.log("step " + s.name + " took " + seconds(now.Sub(s.opened)))
	s.name, s.opened = name, now
}

// Fail ends the steps at the open one, which failed, logging `step <its name> failed after
// <seconds>s`. The caller's error, which it writes after, stays the log's last line.
func (s *Steps) Fail() {
	if s == nil || s.ended {
		return
	}
	s.ended = true
	s.log("step " + s.name + " failed after " + seconds(s.clock.Now().Sub(s.opened)))
}

// End ends the open step, logging `step <its name> took <seconds>s`, and then the run of every
// step, `steps took <seconds>s`.
func (s *Steps) End() {
	if s == nil || s.ended {
		return
	}
	s.ended = true
	now := s.clock.Now()
	s.log("step " + s.name + " took " + seconds(now.Sub(s.opened)))
	s.log("steps took " + seconds(now.Sub(s.started)))
}

// seconds is d in seconds with one decimal and an s: 12.3s.
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
}
