package agent

import (
	"fmt"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going/agentwire"
)

// Level is the budget the person chose for a run (R10): the most model calls it makes, the most
// time it works, the most output tokens each of its model calls writes, and the reasoning effort
// each call asks for. Time the run waits for its person counts against none of it.
type Level struct {
	Calls        int
	Time         time.Duration
	OutputTokens int
	Effort       string
}

// Levels is each level by the name going_runs.level holds: Fast, Standard and Deep (the spec's
// design values, which the eval may change).
var Levels = map[string]Level{
	"fast":     {Calls: 6, Time: 90 * time.Second, OutputTokens: 4000, Effort: "low"},
	"standard": {Calls: 24, Time: 5 * time.Minute, OutputTokens: 8000, Effort: "medium"},
	"deep":     {Calls: 80, Time: 20 * time.Minute, OutputTokens: 16000, Effort: "high"},
}

// modelCallTime is the most one model call of a run may take, its tool calls included, within
// what is left of the run's working time; the watchdog gives up a step that runs past it and
// watchdogGrace.
const modelCallTime = 5 * time.Minute

// watchdogGrace is how long past its model call's time a step may run before the watchdog gives
// up its lease and counts its goroutine stuck.
const watchdogGrace = 10 * time.Second

// The codes of the reasons a bound of its level stops a run with; each run so stopped offers
// Continue.
const (
	ReasonCallBudget   = "CALL_BUDGET"
	ReasonTimeBudget   = "TIME_BUDGET"
	ReasonOutputTokens = "OUTPUT_TOKEN_BUDGET"
)

// spent is the stop reason of a run of level that has spent used, and "" where it may make
// another model call: its calls or its working time are spent.
func (l Level) spent(name string, used agentwire.Budget) string {
	switch {
	case used.Calls >= l.Calls:
		return fmt.Sprintf("%s: this run made the %d model calls a %s run may; Continue starts a follow-up with a new budget", ReasonCallBudget, l.Calls, name)
	case time.Duration(used.WorkingMS)*time.Millisecond >= l.Time:
		return fmt.Sprintf("%s: this run worked the %s a %s run may; Continue starts a follow-up with a new budget", ReasonTimeBudget, l.Time, name)
	}
	return ""
}

// cut is the stop reason of a run of level whose model call wrote its level's output tokens and
// was cut there.
func (l Level) cut(name string) string {
	return fmt.Sprintf("%s: a model call of this run wrote the %d tokens a call of a %s run may; Continue starts a follow-up with a new budget",
		ReasonOutputTokens, l.OutputTokens, name)
}

// request is the agentloop request of the next step of a run of level that has spent used, where
// spent answers "", on model: the model at the level's effort; what is left of the run's working
// time, at most modelCallTime; the output tokens of one call; the size limits of one call; and
// room for one model call, the one call agentloop.Run makes of a step. A run with one call left
// is given Calls 1, so that call is offered no tool and must answer; any other is given 2, so its
// tools are offered and run, and the step ends after the first (OnStep). The caller adds the
// prompt, the messages, the tools and the callbacks.
func (l Level) request(model agentloop.Model, used agentwire.Budget) agentloop.Request {
	calls := 2
	if used.Calls >= l.Calls-1 {
		calls = 1
	}
	left := l.Time - time.Duration(used.WorkingMS)*time.Millisecond
	model.Effort = l.Effort
	return agentloop.Request{
		Model:       model,
		Bounds:      agentloop.Bounds{Time: min(left, modelCallTime), Calls: calls, OutputTokens: l.OutputTokens},
		StreamLimit: runStreamLimit,
		ReplyLimit:  runReplyLimit,
		NoCache:     true,
	}
}
