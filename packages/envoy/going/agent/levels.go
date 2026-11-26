package agent

import (
	"fmt"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going/agentwire"
)

// Level is the budget the person chose for a run (R10): the most model calls it makes, the most
// time it works, the most output tokens each of its model calls writes, the reasoning tokens the
// pacer reserves for each call beside them, and the reasoning effort each call asks for. Time the
// run waits for its person counts against none of it.
type Level struct {
	Calls        int
	Time         time.Duration
	OutputTokens int
	// ReasoningTokens is what the pacer reserves for a call's reasoning (estimate): the output
	// bound, max_tokens, bounds the completion tokens alone, and a call's reasoning is not cut
	// at it.
	ReasoningTokens int
	Effort          string
}

// Levels is each level by the name going_runs.level holds: Fast, Standard and Deep (the spec's
// design values, which the eval may change). The output tokens of each were set on 2026-11-23 from
// the output and reasoning tokens the real model, grok-4.7, wrote in one call of each level in the
// preview, p50 / p99 / max: Fast 111 / 433 / 504, Standard 213 / 965 / 1163, Deep 326 / 2217 /
// 2217. Each bound is about twice its level's max; a call that reaches it ends the run with
// ReasonOutputTokens and offers Continue (cut). The reasoning tokens of each are the p99 of the
// reasoning tokens one call of the level wrote in that preview: the model's max_tokens bounds its
// completion tokens alone, so a call cut at 150 output tokens also wrote 129 reasoning tokens,
// and calls under a bound of 150 wrote up to 522.
var Levels = map[string]Level{
	"fast":     {Calls: 6, Time: 90 * time.Second, OutputTokens: 1000, ReasoningTokens: 353, Effort: "low"},
	"standard": {Calls: 24, Time: 5 * time.Minute, OutputTokens: 2500, ReasoningTokens: 865, Effort: "medium"},
	"deep":     {Calls: 80, Time: 20 * time.Minute, OutputTokens: 5000, ReasoningTokens: 1967, Effort: "high"},
}

// estimate is the tokens the pacer reserves for one model call of the level whose request sends
// prompt characters (going.PromptChars): a token for each charsPerToken of them, the level's
// output tokens and its reasoning tokens.
func (l Level) estimate(prompt int) int {
	return prompt/charsPerToken + l.OutputTokens + l.ReasoningTokens
}

// modelCallTime is the most one model call of a run may take, its tool calls included, within
// what is left of the run's working time; the watchdog gives up a step that runs past it and
// watchdogGrace.
const modelCallTime = 5 * time.Minute

// watchdogGrace is how long past its model call's time a step may run before the watchdog gives
// up its lease and counts its goroutine stuck.
const watchdogGrace = 10 * time.Second

// wordsCallTime bounds the words call of a reply in prose (drawnWords), as the run's end bounds
// its own call: the call is made once the step's watchdog stopped, so nothing else bounds a
// sign-in outage that serverCall waits out. A variable, so a test shortens it.
var wordsCallTime = 10 * time.Second

// The codes of the reasons a bound of its level stops a run with; each run so stopped offers
// Continue.
const (
	ReasonCallBudget   = "CALL_BUDGET"
	ReasonTimeBudget   = "TIME_BUDGET"
	ReasonOutputTokens = "OUTPUT_TOKEN_BUDGET"
)

// spent is the stop reason of a run of level that has spent used, and "" where it may make
// another model call: its calls or its working time are spent. A run stopped at its working time
// whose pacer held its model calls for more than half of it says so (busyWords): it spent most of
// its time waiting for the model's quota.
func (l Level) spent(name string, used agentwire.Budget) string {
	switch {
	case used.Calls >= l.Calls:
		return fmt.Sprintf("%s: this run made the %d model calls a %s run may; Continue starts a follow-up with a new budget", ReasonCallBudget, l.Calls, name)
	case time.Duration(used.WorkingMS)*time.Millisecond >= l.Time:
		busy := ""
		if 2*used.PaceMS > used.WorkingMS {
			busy = fmt.Sprintf(busyWords, (time.Duration(used.PaceMS) * time.Millisecond).Round(time.Second))
		}
		return fmt.Sprintf("%s: this run worked the %s a %s run may%s; Continue starts a follow-up with a new budget", ReasonTimeBudget, l.Time, name, busy)
	}
	return ""
}

// busyWords is what a time stop adds where the pacer held the run's model calls for more than
// half its working time, with how long it held them. The web reads it back
// (features/going/run-states.ts, endWords).
const busyWords = ", and the model was busy for %s of it"

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
