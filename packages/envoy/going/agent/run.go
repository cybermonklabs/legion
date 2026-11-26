package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agentwire"
)

// The byte limits of one model call of a run (section 5 of the background-runs design): the
// response stream at most 8 MiB and the reply it builds at most 512 KiB, past which the call
// fails and the run says why.
const (
	runStreamLimit = 8 << 20
	runReplyLimit  = 512 << 10
)

// The reasons a run ends failed with, beside the bounds of its level (levels.go) and the reasons
// the server names.
const (
	// ReasonNoModel is a run of an agent whose server names no chat model.
	ReasonNoModel = "NO_MODEL: this server names no chat model (DISPATCH_ANSWERER), so its agent cannot work the run"
	// codeModelFailed, codeModelRefused, codeModelTooLarge and codeModelCallTime are a model call
	// that failed, that the model refused, whose stream or reply passed its limit, and that took
	// past modelCallTime.
	codeModelFailed   = "MODEL_FAILED"
	codeModelRefused  = "MODEL_REFUSED"
	codeModelTooLarge = "MODEL_REPLY_TOO_LARGE"
	codeModelCallTime = "MODEL_CALL_TIME"
	// codeContentFilter is a model call the chat model's content filter refused with every
	// tool result of the run withheld from it (withhold), which stops the run.
	codeContentFilter = "MODEL_CONTENT_FILTER"
	// codeAgentFailed is a run the agent's own code failed on: a step it could not save, a
	// history it could not read, a callback of its loop that panicked.
	codeAgentFailed = "AGENT_FAILED"
)

// The frames a run's steps show its person's bar.
const (
	// frameCall is one tool call, as it starts: callFrame.
	frameCall = "call"
	// frameTool is one tool call, as it ended: toolFrame.
	frameTool = "tool"
	// frameReply is one model reply: replyFrame.
	frameReply = "reply"
	// frameBlock is the block a draw_block call drew, as the server answered it
	// (going.BlockData), shown beside the run's answer.
	frameBlock = "block"
)

// callFrame is the data of a call frame, which a tool call sends as it starts (callStarted): the
// step its tool frame will carry, the tool, the kind of step it is, and the present form of its
// tool's label filled from its arguments, which the bar shows while the call runs. The first call
// frame of a model call carries the prose that model call streamed so far (Text), "" where it
// streamed none, which the bar shows as the header line of its calls; each later call frame of
// that model call carries none.
type callFrame struct {
	Step  int     `json:"step"`
	Tool  string  `json:"tool"`
	Kind  string  `json:"kind"`
	Label string  `json:"label"`
	Text  *string `json:"text,omitempty"`
}

// toolFrame is the data of a tool frame: the step, the tool, its one-line note for the model and
// whether it failed in a way its person can act on - no access, the run stopped, Dispatch failing;
// and what the bar shows of it: its label, the past form of its tool's label filled from its
// arguments, with the count of a list it answered, " · none found" for a read Dispatch answered
// 404, which is no error, or one sentence that says why it failed (ended), or, for a slip, what
// the model tried (slipWords); the kind of step it is; the count of the results of a list or a
// search; the items it named and answered, by key or reference; and whether it is a slip: a call
// of the model's that failed by the model's own guess - a tool no one has, arguments its tool
// refuses, a change outside a plan - which the model mends itself, and which the bar shows as no
// failure. A slip is not an error. MS is how many milliseconds the call took, from its start to
// its end (toolRun), left out under one millisecond and on a frame of a plan's row or a review.
type toolFrame struct {
	Step  int      `json:"step"`
	Tool  string   `json:"tool"`
	Note  string   `json:"note"`
	Error bool     `json:"error"`
	Label string   `json:"label"`
	Kind  string   `json:"kind"`
	Count int      `json:"count,omitempty"`
	Items []string `json:"items"`
	Slip  bool     `json:"slip"`
	MS    int64    `json:"ms,omitempty"`
}

// replyFrame is the data of a reply frame: the step, the reply's prose, the tools it called, the
// present form of each call's label filled from its arguments, in the order of tools, which the
// bar shows while the call runs, and, on a reply that ends the run's turn, how many tool results
// the run withheld from the model (withhold), which the bar says under the answer, and whether its
// answer is a question the run needs the person to answer before it can go on (ask_person), false
// on every other reply. A reply in prose that the guard held back (saveNoPlan) carries no prose and
// NoPlan, and one held back as long (saveLong) no prose and HeldLong: the bar adds a reply of no
// prose to no answer (web features/going/run-frames.ts), so the person never reads it. It says
// where the model call's time went too (modelCall): the milliseconds from its request to its last
// token and to its first token of prose, left out for a reply of tool calls alone, how long the
// agent's pacer held it, the tokens of its prompt, those of the provider's cache included, of its
// reply, its reasoning among them, and of that reasoning, the tokens the pacer reserved for it
// before it was made, and the tokens the pacer charged it in the end; a reply held back as long
// and shown whole once its rewrite drew no reply, or one that linked what it does not (showHeld),
// says none, since no call wrote it again. A reply in prose that the guard checked before it
// could end the turn (checkProse), held back or not, also says what the check took: its
// milliseconds and its tokens; one it did not check because Jev read the turn's words as Ask says
// so; each says what Jev read them as; and one that would end the turn done says how many words
// the bar draws of it.
type replyFrame struct {
	Step           int      `json:"step"`
	Text           string   `json:"text"`
	Tools          []string `json:"tools"`
	Labels         []string `json:"labels"`
	Withheld       int      `json:"withheld,omitempty"`
	WaitsForPerson bool     `json:"waits_for_person"`
	NoPlan         bool     `json:"no_plan,omitempty"`
	// ModelMS is the milliseconds from the call's request to its last token, the pacer's hold of
	// it (PaceMS) included.
	ModelMS int64 `json:"model_ms"`
	// PaceMS is the milliseconds the agent's pacer held the call's HTTP attempts before each was
	// made, summed, and left out where it held none: model_ms - pace_ms is the provider's own time.
	PaceMS       int64 `json:"pace_ms,omitempty"`
	FirstTokenMS int64 `json:"first_token_ms,omitempty"`
	InputTokens  int   `json:"input_tokens"`
	// OutputTokens is the tokens the provider generated for the call's reply, its reasoning among
	// them however the provider reports it (agentloop.Usage), and ReasoningTokens the part of
	// those that was reasoning.
	OutputTokens    int `json:"output_tokens"`
	ReasoningTokens int `json:"reasoning_tokens"`
	// ReservedTokens is the tokens the pacer charged the call before it was made: the estimate of
	// its prompt, its tool definitions among it, its level's output tokens and its level's
	// reasoning tokens (Level.estimate), and for a rewrite the estimate of its short prompt
	// (rewritePrompt, the person's words, the held reply and longTold), no tool, since it sends
	// none, its level's output tokens and rewriteReasoningTokens (rewrite), summed over its HTTP
	// attempts, which a run whose deployment names no quota still counts. Set against
	// input_tokens + output_tokens, it says how far the estimate is from what the call spent.
	ReservedTokens int `json:"reserved_tokens"`
	// ChargedTokens is the tokens the pacer charged the call in the end: what it drew for the
	// call's HTTP attempts, at most a minute's share each, and the tokens the call spent past
	// them (going.Pacer.Owe), counted where no quota is named too.
	ChargedTokens int `json:"charged_tokens"`
	// CheckMS is the milliseconds the guard's check of the reply took (checkProse), from its
	// request to its end, which the run's working time counts and its model time does not, and
	// CheckTokens the tokens the check spent, its prompt's and its reply's (going.SpentTokens).
	// endProse sets both, 0 included, on a reply the check was made for, held back or not, and
	// whatever the check answered; every other reply leaves both out: one that called a tool,
	// asked the person or was cut, one in a turn that holds a plan, a new-issue run's, which is
	// held back with no check, one whose run had no working time left for the check, and one
	// whose check was skipped (CheckSkipped).
	CheckMS     *int64 `json:"check_ms,omitempty"`
	CheckTokens *int   `json:"check_tokens,omitempty"`
	// CheckSkipped is why endProse made no check of a reply in prose it would have checked:
	// checkSkippedJevAsk, the decision model Jev read the turn's words as Ask, so the reply adds
	// no check's time to its turn. It is left out on every other reply.
	CheckSkipped string `json:"check_skipped,omitempty"`
	// DrawnWords is how many words the bar draws of the reply's prose as the run's answer, as the
	// server counted them (holdLong): each link as its item's chip, an issue's with its key, its
	// status and its title. endProse sets it, 0 included, on a reply in prose it would end the
	// turn done with, held back as long or not, and showHeld on a reply held back as long that it
	// shows whole; every other reply leaves it out, as does one whose count failed. HeldLong marks
	// a reply held back as long (saveLong), which carries no prose.
	DrawnWords *int `json:"drawn_words,omitempty"`
	HeldLong   bool `json:"held_long,omitempty"`
	// JevLine is what Jev read the turn's words as when endProse decided on the reply (jevLine):
	// ask, do or file, none for no line, late for a line call still out. It is left out where the
	// guard asks Jev nothing (asksJev): a run started from Do or File, a turn the person's answer
	// to a plan started, and every reply endProse does not decide on.
	JevLine string `json:"jev_line,omitempty"`
}

// errStepTaken ends agentloop.Run once its one model call of a step finished and was saved: each
// step starts the loop again from the saved steps.
var errStepTaken = errors.New("the step's model call finished and was saved")

// errReplyFiltered ends agentloop.Run once its one model call of a step finished with a reply the
// chat model's content filter stopped, which is not saved: the step is made again (withhold).
var errReplyFiltered = errors.New("the chat model's content filter stopped the step's reply")

// finishContentFilter is agentloop.Step.Finish of a reply the content filter stopped, and
// finishLength that of a reply cut at its output tokens.
const (
	finishContentFilter = "content-filter"
	finishLength        = "length"
)

// worker is one claimed run of the agent's: the run, its level, what it has spent, its history,
// the time it took the run's turn, or the agent's own time now where a test sets one
// (Config.Now), which its model is told as the time now and from which each time the model is
// given is written relative to now (shownResult), and its own context, which a stop, a lease the
// run no longer holds, the watchdog and the agent's end each cancel, and which ends as the run's
// turn ends (work).
type worker struct {
	agent *agent
	run   *agentwire.ClaimedRun
	name  string
	level Level
	now   time.Time

	// used is what the run has spent, its earlier turns' and this one's; turn is this turn's
	// alone, which end adds to the run's budget. A tool call adds its time to both from the
	// goroutine it runs on (toolRun): one of agentloop's, which runs a step's calls one at a time
	// and ends them before its Run returns, or the run's own.
	used, turn agentwire.Budget
	// history is the run's steps as the claim answered them, then each step saved since, in the
	// order of their numbers; next is the number the next step takes.
	history []agentwire.SavedStep
	next    int

	ctx    context.Context
	cancel context.CancelFunc
	// release frees the run's slot of the agent's, once whichever calls it first: the run's
	// goroutine as it returns, or the watchdog as it gives the run up.
	release func()

	// mu guards stepped, the steps a model call in flight saved, which it adds to history at its
	// end.
	mu      sync.Mutex
	stepped []agentwire.SavedStep
	// inFlight is the model call in flight (modelCall), whose first call frame of its step
	// carries the prose it streamed so far (callStarted); nil between model calls.
	inFlight atomic.Pointer[modelCall]

	// grantEnded is the reason a call's grant ended with, which stops the run; lost is set once
	// the run is no longer held under its lease, after which the worker writes nothing more of it;
	// proposed is set by a step whose model proposed a plan, and plan is the plan the server kept
	// of it.
	grantEnded atomic.Pointer[string]
	lost       atomic.Bool
	proposed   atomic.Bool
	plan       atomic.Pointer[agentwire.PlanAnswer]
	// drawn is the block the server drew for a draw_block call (going.BlockData), by the step
	// of the call, until that step is saved with it as its block frame.
	drawn sync.Map
	// jev is Jev's reading of the turn's words (readWords), nil where the guard asks Jev nothing.
	// It is set as the turn starts and read where the guard decides; a test's model reads it too.
	jev atomic.Pointer[jevRead]
	// withheld is the tool steps of the run whose results the model is no longer given, by their
	// numbers, each with its tool: the content filter refused a model call that held them
	// (withhold); filterRefused is set once the filter refused a call that is made again as it
	// was, until a step saves its model call. Only the run's goroutine writes them, between its
	// model calls.
	withheld      map[int]string
	filterRefused bool
}

// newWorker is the worker of run, its context ctx's, its slot freed by release.
func newWorker(ctx context.Context, a *agent, run *agentwire.ClaimedRun, release func()) *worker {
	now := a.config.Now
	if now.IsZero() {
		now = time.Now()
	}
	w := &worker{agent: a, run: run, name: run.Level, history: run.History, release: release, now: now}
	w.ctx, w.cancel = context.WithCancel(ctx)
	w.used = run.BudgetUsed
	models := 0
	for _, step := range run.History {
		if step.Kind == agentwire.StepModel {
			models++
		}
		w.next = step.Step
	}
	// A turn cut short by the agent's death adds nothing to budget_used, so the run's own saved
	// model steps count its calls too.
	w.used.Calls = max(w.used.Calls, models)
	w.next++
	return w
}

// lose stops working on the run: it is no longer held under its lease, so nothing more of it is
// written.
func (w *worker) lose() {
	w.lost.Store(true)
	w.cancel()
}

// endGrant stops the run for reason, the grant it acts with having ended.
func (w *worker) endGrant(reason string) {
	w.grantEnded.CompareAndSwap(nil, &reason)
	w.cancel()
}

// work runs the run's turn: the rows of its plan that are approved and not yet run, then, for a
// run with no saved step, the read of its page's issue (readPage), then a step at a time from its
// saved steps, until it answers, proposes a plan, spends a bound of its level, fails, or stops
// being the agent's to work. A plan proposed in Auto-review is reviewed before the turn ends
// (review), and the rows the server approved with the plan run at once, which ends the turn
// (runApproved): the create of a new-issue run. A new-issue run whose create ran ends there.
// A run whose saved steps name a tool this build does not know fails at once (unknownTool). Before
// its first model call, a turn of the person's words asks Jev for their line on a goroutine of its
// own where the guard reads it (readWords), which work ends and waits for as it returns. The
// run's context ends as work returns, however the turn ended: a context left live stays among the
// children of the agent's, with all made from it, until the agent's process exits. Nothing of the
// run outlives work but the failed end agent.work makes after a panic, which end makes on the
// run's context without its cancel.
func (w *worker) work() {
	defer w.cancel()
	level, known := Levels[w.run.Level]
	switch {
	case w.agent.config.Model.ID == "":
		w.end(agentwire.StateFailed, ReasonNoModel, nil)
		return
	case !known:
		w.end(agentwire.StateFailed, fmt.Sprintf("%s: the run's level %q is none of fast, standard and deep", codeAgentFailed, w.run.Level), nil)
		return
	}
	if reason := w.unknownTool(); reason != "" {
		w.end(agentwire.StateFailed, reason, nil)
		return
	}
	w.level = level
	if len(w.run.Rows) > 0 {
		w.runRows(w.run.Rows)
	} else if outcome, resumed := w.resumePlan(); resumed {
		if w.halted() {
			return
		}
		if outcome.state != "" {
			w.end(outcome.state, outcome.reason, outcome.offers)
			return
		}
	} else if done, state, reason := w.settled(); done {
		w.end(state, reason, nil)
		return
	}
	if w.halted() {
		return
	}
	defer w.readWords()()
	if len(w.history) == 0 {
		w.readPage()
		if w.halted() {
			return
		}
	}
	if w.run.Kind == agentwire.RunNewIssue && w.filed() {
		w.end(agentwire.StateDone, "", nil)
		return
	}
	retry := time.Second
	for w.ctx.Err() == nil {
		if reason := w.level.spent(w.name, w.used); reason != "" {
			w.end(agentwire.StateStopped, reason, []string{agentwire.OfferContinue})
			return
		}
		if wholeBytes(lastWhole(w.history)) > historyLimit {
			w.end(agentwire.StateFailed, ReasonHistoryTooLarge, nil)
			return
		}
		outcome := w.step()
		if outcome.state == agentwire.StateWaiting && w.proposed.Load() && !w.halted() {
			if plan := w.plan.Load(); plan != nil && len(plan.Run) > 0 {
				outcome = w.runApproved(plan.Run)
			} else if w.run.Mode == modeAutoReview {
				outcome = w.review()
			}
		}
		switch {
		case w.halted():
			return
		case outcome.retry:
			timer := time.NewTimer(retry)
			select {
			case <-w.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			retry = min(2*retry, 30*time.Second)
			continue
		case outcome.state != "":
			w.end(outcome.state, outcome.reason, outcome.offers)
			return
		}
		retry = time.Second
	}
}

// halted reports whether the run is no longer the worker's to go on with: it is no longer held,
// its grant ended, which ends the run stopped with the grant's reason, or its context ended.
func (w *worker) halted() bool {
	switch {
	case w.lost.Load():
		return true
	case w.grantEnded.Load() != nil:
		w.end(agentwire.StateStopped, *w.grantEnded.Load(), nil)
		w.grantEnded.Store(nil)
		w.lost.Store(true)
		return true
	}
	return w.ctx.Err() != nil
}

// pageCallID is the call id the read of the page's issue is saved under (readPage), which no
// call of a model's takes: conversation gives its result in the person's message.
const pageCallID = "page"

// readPage reads the issue the run's page names, as the person, as every read is made, and saves
// it as the run's first step, with the frames of a read's step, before the run's first model
// call: conversation gives its result, marked as data, in the person's message, so no model call
// is spent reading the issue the run already knows. A page that names no issue reads nothing.
func (w *worker) readPage() {
	var page struct {
		Issue string `json:"issue"`
	}
	if json.Unmarshal(w.run.Context, &page) != nil || page.Issue == "" {
		return
	}
	input, _ := json.Marshal(map[string]string{"issue": page.Issue})
	w.toolRun(w.next-1, toolRead, w.read)(w.ctx, agentloop.ToolCall{Index: 1, ID: pageCallID, Input: input})
	w.merge()
}

// settled reports whether the run's saved steps already end its turn: a last step that answered
// with no tool call, or that proposed a plan, which waits for its person. A reply in prose that
// the guard held back (saveNoPlan), or that was held back as long (saveLong), ends no turn: a
// worker that starts from it makes the next step, whose model is told that nothing changed, or
// how many words the person would have read.
func (w *worker) settled() (bool, string, string) {
	if len(w.history) == 0 {
		return false, "", ""
	}
	last := w.history[len(w.history)-1]
	if len(last.Body) == 0 {
		return false, "", ""
	}
	switch last.Kind {
	case agentwire.StepModel:
		var body modelBody
		if json.Unmarshal(last.Body, &body) == nil && len(body.Calls) == 0 && !body.NoPlan && body.LongWords == 0 {
			return true, agentwire.StateDone, ""
		}
	case agentwire.StepTool:
		var body toolBody
		if json.Unmarshal(last.Body, &body) == nil && body.Call.Tool == toolProposePlan && !body.Error {
			return true, agentwire.StateWaiting, ""
		}
	}
	return false, "", ""
}

// stepOutcome is how a step ended: retry to make it again after a wait, or the state, reason and
// offers to end the run's turn with; neither for a step that saved its model call and goes on.
type stepOutcome struct {
	retry  bool
	state  string
	reason string
	offers []string
}

// step makes one step: one agentloop.Run of one model call, from the run's saved steps, with the
// tools it calls; each tool call shows its call frame as it starts (callStarted), with the prose
// the model call streamed so far (OnText) on the first, and is saved as it ends (toolRun); the
// model call is saved once its tools ran (OnStep), with the times runModel keeps of it
// (modelCall). The call asks for no structured reply: a reply that calls no tool is the answer in
// prose, and one that calls ask_person ends the turn with its question (saveReply). A reply in
// prose in a turn that holds no plan is saved once the step's call returned, as the guard decides
// (endProse): it ends the turn, or it is held back and the turn goes on. The watchdog gives up the
// run when the step runs past its model call's time and watchdogGrace. A call the chat model's
// content filter refused, or whose reply it stopped, saves nothing, and the step is made again
// (withhold). The run's last model call, which is offered no tool, is told so at the end of its
// system message (lastCallNote). The step after a reply held back as long is that reply's rewrite
// (heldLast, length.go): its call is given rewritePrompt, the person's words, the held reply and
// longTold alone, not the run's conversation, is offered no tool, at rewriteEffort, within
// rewriteCallTime, and the pacer reserves the tokens of that short prompt and
// rewriteReasoningTokens for it (rewrite). A reply of the rewrite that links what the held reply
// does not ends the turn done with the held reply shown whole, and any other reply of it ends the
// turn as endProse ends it (endRewrite); a rewrite that draws no reply - its bound ended it, or
// its call failed, was refused or was held back for the quota - ends the turn done with the held
// reply shown whole too (missedRewrite).
func (w *worker) step() stepOutcome {
	base := w.next
	var finish string
	var ends bool
	var prose *proseReply
	request := w.level.request(w.agent.config.Model, w.used)
	held, rewrite := w.heldLast()
	var estimate int
	if rewrite {
		estimate = w.rewrite(&request, held)
	} else {
		messages, err := conversation(w.run, lastWhole(w.history), w.withheld, w.now)
		if err != nil {
			return stepOutcome{state: agentwire.StateFailed, reason: fmt.Sprintf("%s: %v", codeAgentFailed, err)}
		}
		request.System, request.Input = w.system(), messages
		if request.Bounds.Calls == 1 {
			request.System += "\n\n" + lastCallNote
		}
		request.Tools = w.tools(base)
		estimate = w.level.estimate(going.PromptChars(request.System, request.Input, request.Tools))
	}
	w.proposed.Store(false)
	request.Model = w.agent.pacer.Paced(request.Model, func() int { return estimate }, w.charge)
	request.OnStep = func(step agentloop.Step) error {
		if w.agent.config.Hooks {
			hookOnStep(step)
		}
		if step.Finish == finishContentFilter {
			return errReplyFiltered
		}
		finish = step.Finish
		ends, prose = w.saveReply(base, step, w.inFlight.Load())
		return errStepTaken
	}
	request.OnText = func(delta string) error {
		w.inFlight.Load().add(delta)
		return nil
	}
	result, kept, err := w.runModel(request)
	switch {
	case !kept || w.lost.Load() || w.grantEnded.Load() != nil || w.ctx.Err() != nil:
		return stepOutcome{}
	case errors.Is(err, errStepTaken):
		w.filterRefused = false
		switch {
		case finish == finishLength:
			return stepOutcome{state: agentwire.StateStopped, reason: w.level.cut(w.name), offers: []string{agentwire.OfferContinue}}
		case w.proposed.Load():
			return stepOutcome{state: agentwire.StateWaiting}
		case prose != nil && rewrite:
			return w.endRewrite(held, prose)
		case prose != nil:
			return w.endProse(prose)
		case ends:
			return stepOutcome{state: agentwire.StateDone}
		}
		return stepOutcome{}
	case rewrite:
		return w.missedRewrite(base, held, result, err)
	case errors.Is(err, errReplyFiltered):
		return w.withhold("the provider's content filter stopped the reply")
	case err != nil:
		return stepOutcome{state: agentwire.StateFailed, reason: fmt.Sprintf("%s: %v", codeAgentFailed, err)}
	case result.Fault == agentloop.FaultContentFilter:
		return w.withhold(result.Detail)
	}
	return w.faulted(result)
}

// withhold is how a step goes on whose model call the chat model's content filter refused. The
// filter's verdict is on what a call holds and what the model writes back, which the model writes
// anew at each call, so a first refusal makes the same call again. A refusal of that call again
// withholds every tool step of the run whose result holds data that people, agents, GitHub or a
// web page wrote (storedDataNote), which the agent cannot vouch for, and that is not yet withheld,
// so the model is told of each that its result was withheld (conversation), and the step is made
// again; a reply that ends the turn then tells the person which results were withheld
// (saveReply). A step that saves its model call makes the next refusal a first one again.
// A call refused with no such result left to withhold stops the run (filterStopped).
func (w *worker) withhold(detail string) stepOutcome {
	if !w.filterRefused {
		w.filterRefused = true
		slog.Warn("agent: the chat model's content filter refused a model call; the call is made again as it was",
			"run", w.run.ID, "detail", note(detail))
		return stepOutcome{}
	}
	added := 0
	for _, step := range w.history {
		if _, done := w.withheld[step.Step]; done || step.Kind != agentwire.StepTool {
			continue
		}
		if tool, stored := storedResult(step); stored {
			if w.withheld == nil {
				w.withheld = map[int]string{}
			}
			w.withheld[step.Step] = tool
			added++
		}
	}
	if added == 0 {
		return w.filterStopped(detail)
	}
	slog.Warn("agent: the chat model's content filter refused a model call again; its tool results are withheld and the call is made again",
		"run", w.run.ID, "withheld", added, "detail", note(detail))
	return stepOutcome{}
}

// filterStopped is how a run ends whose model call the content filter refused with every result
// it could withhold withheld (withhold). The refusal is the model's provider's, not a failure of
// Dispatch or of the model: the run stops with codeContentFilter, its reason the provider's
// detail, with the label the filter named (agentloop's ContentFilterRefusal), and what the person
// can do. A run that withheld results it had found offers Continue: the follow-up reads this
// run's steps as their one-line notes and its page anew, so its calls hold other words than the
// refused one. A run that withheld nothing was refused for the words of the call itself - the
// person's words, the page, the agent's own rules - which a follow-up sends again, so it offers
// nothing, and its reason says to ask in other words.
func (w *worker) filterStopped(detail string) stepOutcome {
	slog.Warn("agent: the chat model's content filter refused a model call with every result it could withhold withheld; the run stops",
		"run", w.run.ID, "withheld", len(w.withheld), "detail", note(detail))
	if len(w.withheld) == 0 {
		return stepOutcome{state: agentwire.StateStopped, reason: fmt.Sprintf(
			"%s: the chat model's provider refused to read this run's model call, which held nothing the run found, only the person's words, the page and the agent's rules (%s); ask in other words",
			codeContentFilter, note(detail))}
	}
	return stepOutcome{state: agentwire.StateStopped, offers: []string{agentwire.OfferContinue}, reason: fmt.Sprintf(
		"%s: the chat model's provider refused to read part of what this run found, also with the %d results it could not vouch for withheld (%s); Continue starts a follow-up with a new budget, or ask in other words",
		codeContentFilter, len(w.withheld), note(detail))}
}

// lastCallNote is what the run's last model call is told, which agentloop offers no tool: a model
// that is not told so writes that it will read more, and ends the turn without an answer, or
// repeats that until its output tokens are spent. A change the words ask for can no longer be
// proposed in that call, so the answer says nothing changed and why, rather than that it is made.
// It ends the call's system message, as the agent's own rules do, and never stands in the
// person's turn: said there as the person's last words, Azure's jailbreak shield refused the call
// for it (the c13 rerun at 8465df75: every refused call of chart-08 and change-14 was a run's last
// call; replayed, a body with the note in the person's turn was refused 5 of 5, and with it at
// the end of the system message, word for word, 0 of 10).
const lastCallNote = "This is the last model call this run's level allows, and it is offered no tool: answer the person now, from what the results above hold, and say what you did not read. Write no promise to read more. Where the words ask for a change that no plan of this run proposed, say that nothing changed and what stopped it."

// runModel makes request's one model call on the run's context, under the watchdog, adds what it
// spent to the run's budget, and adds the steps it saved to the run's history. It reports false
// where the watchdog gave the run up: the run is lost, and nothing more of it is written. A call
// the content filter refused, or whose reply it stopped, spends the run's time and tokens and no
// call of its level, since it answered nothing: it is made again as withhold says, which ends the
// run once a call is refused with every result withheld, and a refusal of a run's last call would
// otherwise end the run with no answer. The call is the run's model call in flight (inFlight)
// until it returns, and adds its time from its request to its last token to the run's model
// time: the time agentloop.Run took, where no token marked its end (modelCall); and the time the
// pacer held its HTTP attempts to the run's pace time. The tokens the call spent past those the
// pacer drew for it are charged to the pacer as it returns (going.Pacer.Owe), the run given up
// or not.
func (w *worker) runModel(request agentloop.Request) (agentloop.Result, bool, error) {
	stepCtx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	finished := w.agent.watch(w, request.Bounds.Time+watchdogGrace)
	call := &modelCall{asked: time.Now()}
	w.inFlight.Store(call)
	defer w.inFlight.Store(nil)
	result, err := agentloop.Run(stepCtx, request)
	returned := time.Now()
	held, _, drawn := call.pacing()
	w.agent.pacer.Owe(going.SpentTokens(result.Usage) - drawn)
	if !finished() {
		// The watchdog gave the run up: its goroutine was counted stuck, and it writes nothing.
		w.lose()
		return result, false, err
	}
	working := returned.Sub(call.asked)
	model, _ := call.ended(returned)
	w.turn.WorkingMS += working.Milliseconds()
	w.used.WorkingMS += working.Milliseconds()
	w.turn.ModelMS += model.Milliseconds()
	w.used.ModelMS += model.Milliseconds()
	w.turn.PaceMS += held.Milliseconds()
	w.used.PaceMS += held.Milliseconds()
	w.turn.Tokens += result.Usage.Output
	w.used.Tokens += result.Usage.Output
	if refused := result.Fault == agentloop.FaultContentFilter || errors.Is(err, errReplyFiltered); !refused && (len(result.Steps) > 0 || result.Fault != "") {
		w.turn.Calls++
		w.used.Calls++
	}
	w.merge()
	return result, true, err
}

// merge adds the steps saved since the last merge to the run's history, and numbers the next step
// after the last of them.
func (w *worker) merge() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.history = append(w.history, w.stepped...)
	if len(w.stepped) > 0 {
		w.next = max(w.next, w.stepped[len(w.stepped)-1].Step+1)
	}
	w.stepped = nil
}

// faulted is how a step whose model call saved nothing ends the run's turn: at its level's time,
// or at the model call's own; a model call that failed, that the model refused or that passed its
// size limits fails the run; a model the quota or a 429 held back is tried again. A failed call's
// reason says why it drew no answer (modelFailure).
func (w *worker) faulted(result agentloop.Result) stepOutcome {
	switch result.Fault {
	case agentloop.FaultTime:
		if reason := w.level.spent(w.name, w.used); reason != "" {
			return stepOutcome{state: agentwire.StateStopped, reason: reason, offers: []string{agentwire.OfferContinue}}
		}
		return stepOutcome{state: agentwire.StateFailed, reason: fmt.Sprintf("%s: a model call of this run took past %s", codeModelCallTime, modelCallTime)}
	case agentloop.FaultTooLarge:
		return stepOutcome{state: agentwire.StateFailed, reason: fmt.Sprintf("%s: %s", codeModelTooLarge, result.Detail)}
	case agentloop.FaultRefusal:
		return stepOutcome{state: agentwire.StateFailed, reason: codeModelRefused + ": the chat model refused to answer"}
	case agentloop.FaultLength:
		return stepOutcome{state: agentwire.StateStopped, reason: w.level.cut(w.name), offers: []string{agentwire.OfferContinue}}
	}
	if result.Status == http.StatusTooManyRequests || strings.Contains(result.Detail, going.ErrQuotaPastDeadline.Error()) {
		return stepOutcome{retry: true}
	}
	return stepOutcome{state: agentwire.StateFailed, reason: fmt.Sprintf("%s: %s", codeModelFailed, modelFailure(result))}
}

// modelFailure says why a model call that saved nothing drew no answer: the status the chat model
// answered, where it answered one, and the fault agentloop ended the call with and its detail
// (faultDetail).
func modelFailure(result agentloop.Result) string {
	detail := "the chat model answered no reply"
	if result.Status != 0 {
		detail = fmt.Sprintf("the chat model answered %d", result.Status)
	}
	if result.Fault != "" {
		detail += fmt.Sprintf(" (%s: %s)", result.Fault, faultDetail(result.Detail))
	}
	return detail
}

// urlQuery is the query of an address in a fault's detail, which faultDetail leaves out.
var urlQuery = regexp.MustCompile(`(https?://[^\s"'?]*)\?[^\s"']*`)

// faultDetail is a fault's detail as a run's reason carries it: on one line, at most noteLimit
// bytes, and each address in it without its query. The transport sends the endpoint's token in a
// header alone, so the detail holds none; the query is left out since a caller's endpoint may
// carry its own parameters there.
func faultDetail(detail string) string {
	return note(urlQuery.ReplaceAllString(detail, "$1"))
}

// saveReply saves step, the model call of the step numbered base, and answers the tools it
// called, and reports whether the reply ends the run's turn: it calls no tool, or it asks the
// person something (ask_person). A reply in prose - no call, no ask - that the provider did not
// cut, in a turn that holds no plan (turnSoFar), is not saved here: saveReply answers it, and the
// guard saves it once the step's call returned (endProse), since a reply that says a change is
// made, where no plan was proposed, must not reach the person as the answer. A call of a tool the
// run does not have is saved marked unknown (toolCall.Unknown): the model made the name up, and a
// later step reads it as such rather than as a tool of a later build. An ask_person call is saved
// as no call, and its question as the reply's text, which the model reads in its later history as
// the answer of its turn; a question with no words leaves the reply's prose as its text. The frame
// of a reply that asks carries the question as the answer, names no call, so the bar draws no line
// of the ask, and waits for the person; the calls beside it ran, and their own frames drew their
// lines. A reply that proposed a plan waits for the plan, not for a question. Its frame says where
// call, the model call in flight, spent its time, the pacer's hold of it among that, the tokens of
// step's prompt, reply and reasoning, the tokens the pacer reserved for it, and the tokens the
// pacer charged it in the end: what it drew for the call, or what the call spent where that is
// more, as runModel charges it (going.Pacer.Owe).
func (w *worker) saveReply(base int, step agentloop.Step, call *modelCall) (bool, *proseReply) {
	took, first := call.ended(time.Now())
	held, reserved, drawn := call.pacing()
	var body modelBody
	var questions []string
	asked := false
	tools, labels := []string{}, []string{}
	known := w.knownTools()
	for _, block := range step.Blocks {
		switch block.Kind {
		case agentloop.TextBlock:
			body.Text += block.Text
		case agentloop.ToolCallBlock:
			input := block.Input
			if !json.Valid(input) {
				input = json.RawMessage(`{}`)
			}
			if block.Name == toolAskPerson {
				asked = true
				if question := askedQuestion(input); question != "" {
					questions = append(questions, question)
				}
				continue
			}
			body.Calls = append(body.Calls, toolCall{ID: block.ToolCallID, Tool: block.Name, Arguments: input, Unknown: !known[block.Name]})
			tools = append(tools, block.Name)
			labels = append(labels, presentLabel(stepLabel(block.Name, input)))
		}
	}
	if first == 0 && body.Text != "" {
		// agentloop gives the step to OnStep on a goroutine of its own, which can run before
		// its stream's reader passed the step's prose on (OnText): prose not passed on yet came
		// last.
		first = took
	}
	if len(questions) > 0 {
		body.Text = strings.Join(questions, "\n\n")
	}
	body.Output = step.Usage.Output
	waits := asked && !w.proposed.Load()
	reply := replyFrame{Step: base, Tools: tools, Labels: labels, WaitsForPerson: waits,
		ModelMS: took.Milliseconds(), PaceMS: held.Milliseconds(), FirstTokenMS: first.Milliseconds(),
		InputTokens: step.Usage.Input + step.Usage.CacheRead + step.Usage.CacheWrite, OutputTokens: step.Usage.Output,
		ReasoningTokens: step.Usage.Reasoning, ReservedTokens: reserved, ChargedTokens: max(drawn, going.SpentTokens(step.Usage))}
	if waits {
		reply.Tools, reply.Labels = []string{}, []string{}
	}
	if !asked && len(tools) == 0 && step.Finish != finishLength {
		if !w.turnSoFar().planned {
			return true, &proseReply{body: body, frame: reply}
		}
	}
	ends := asked || len(tools) == 0
	w.saveModel(body, reply, tools, ends || slices.Contains(tools, toolProposePlan))
	return ends, nil
}

// saveModel saves body, a model step, with reply, its frame, which carries body's text. A reply
// that ends the run's turn or proposes a plan (told), where the run withheld tool results from the
// model, saves the notice of them (withheldTold) after its text, which the model reads in its
// later history; its frame carries the text alone and how many results were withheld, which the
// bar says in its own words. The step's note is its text, with the tools it called.
func (w *worker) saveModel(body modelBody, reply replyFrame, tools []string, told bool) {
	reply.Text = body.Text
	if len(w.withheld) > 0 && told {
		body.Text = strings.TrimSpace(body.Text + withheldTold(w.withheld))
		reply.Withheld = len(w.withheld)
	}
	line := note(body.Text)
	if len(tools) > 0 {
		line = note(fmt.Sprintf("%s [calls %s]", body.Text, strings.Join(tools, ", ")))
	}
	frame, _ := json.Marshal(reply)
	w.save(agentwire.StepRequest{Step: reply.Step, Kind: agentwire.StepModel, Note: line, Frames: []agentwire.Frame{{Name: frameReply, Data: frame}}}, body)
}

// withheldTold is what a reply that ends the run's turn - an answer, or a plan proposed - adds for
// its person where the run withheld tool results from the model (withhold): which, by step and
// tool, and that the reply does not use them.
func withheldTold(withheld map[int]string) string {
	var named []string
	for _, step := range slices.Sorted(maps.Keys(withheld)) {
		named = append(named, fmt.Sprintf("step %d (%s)", step, withheld[step]))
	}
	if len(named) == 1 {
		return fmt.Sprintf("\n\nOne tool result of this run, %s, was withheld from the chat model, since its content filter refused a call that held it: this reply does not use it.", named[0])
	}
	return fmt.Sprintf("\n\n%d tool results of this run, %s, were withheld from the chat model, since its content filter refused a call that held them: this reply does not use them.",
		len(named), strings.Join(named, ", "))
}

// toolPanicked is what the model is told of a tool that panicked (toolRun), which a step's words
// read back (failedReason).
const toolPanicked = "it panicked"

// toolRun is the agentloop Run of tool in the step whose model call is numbered base: it makes the
// call do makes as the step numbered after base by the tool call's index, which it shows the bar
// first as a running call (callStarted), and saves it as that step, with what the model is told of
// its result and the words the bar shows of it (ended), or, for a call that failed by the model's
// own slip, what it tried (slipWords), as a slip and no error. A read Dispatch answered 404 found
// nothing, which is no error either: the model is told what Dispatch answered, and the bar shows
// the read with "none found" (endedStep.noneFound). A do that panics is saved as a
// refusal naming the panic, and the panic goes on to agentloop's recover, so the run's next step,
// which starts from the saved steps, tells the model the tool failed. The time do takes is the
// tool frame's and is added to the run's tool time. The first call of a step marks the last token
// of the model call in flight (modelCall): agentloop runs a step's tools once its stream ended.
func (w *worker) toolRun(base int, tool string, do func(ctx context.Context, step int, input json.RawMessage) (string, bool)) func(context.Context, agentloop.ToolCall) (string, bool) {
	return func(ctx context.Context, call agentloop.ToolCall) (string, bool) {
		if model := w.inFlight.Load(); model != nil {
			model.ended(time.Now())
		}
		step := base + call.Index
		input := call.Input
		if !json.Valid(input) {
			input = json.RawMessage(`{}`)
		}
		label, arguments := stepLabel(tool, input)
		kind := label.Kind
		if kind == kindWrite {
			// A change call_tool asks for outside an approved plan is refused, and shows as a
			// step of the plan's kind: the bar draws a write only as the tick of a plan's row.
			kind = kindPlan
		}
		w.callStarted(callFrame{Step: step, Tool: tool, Kind: kind, Label: presentLabel(label, arguments)})
		var started time.Time
		saveStep := func(result string, refused bool) {
			took := time.Since(started).Milliseconds()
			w.turn.ToolMS += took
			w.used.ToolMS += took
			if w.lost.Load() {
				return
			}
			line := note(fmt.Sprintf("%s %s: %s", tool, input, result))
			end := ended(label, arguments, result, refused)
			slip := false
			if refused {
				var words string
				if words, slip = slipWords(label, arguments, result); slip {
					end.label = words
				}
			}
			frame, _ := json.Marshal(toolFrame{Step: step, Tool: tool, Note: line, Error: refused && !slip && !end.noneFound, Label: end.label, Kind: kind, Count: end.count, Items: end.items, Slip: slip, MS: took})
			frames := []agentwire.Frame{{Name: frameTool, Data: frame}}
			if block, drew := w.drawn.LoadAndDelete(step); drew {
				frames = append(frames, agentwire.Frame{Name: frameBlock, Data: block.(json.RawMessage)})
			}
			w.save(agentwire.StepRequest{Step: step, Kind: agentwire.StepTool, Note: line, Frames: frames},
				toolBody{Call: toolCall{ID: call.ID, Tool: tool, Arguments: input}, Result: result, Error: refused})
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				saveStep(fmt.Sprintf("the tool %s failed: %s (%v)", tool, toolPanicked, recovered), true)
				panic(recovered)
			}
		}()
		started = time.Now()
		result, refused := do(ctx, step, call.Input)
		saveStep(result, refused)
		return result, refused
	}
}

// callStarted sends call's frame as the call starts, once, so the bar shows the call's line while
// it runs; the first call frame of a step carries the prose its model call streamed so far
// (modelCall.first). A send that fails is logged, and the run goes on: the call's tool frame still
// ends its step. A run no longer held sends nothing, and a send the server refuses as no longer
// held loses the run, as a step's save does.
func (w *worker) callStarted(call callFrame) {
	if w.lost.Load() {
		return
	}
	if model := w.inFlight.Load(); model != nil {
		call.Text = model.first()
	}
	frame, _ := json.Marshal(call)
	request := agentwire.FramesRequest{Run: w.run.ID, LeaseToken: w.run.LeaseToken, Frames: []agentwire.Frame{{Name: frameCall, Data: frame}}}
	if _, err := w.agent.client.Frames(w.ctx, request); err != nil {
		var refused *agentwire.Error
		if errors.As(err, &refused) && refused.Code == agentwire.CodeStaleLease {
			w.lose()
			return
		}
		if w.ctx.Err() == nil {
			slog.Warn("agent: send a call's frame", "run", w.run.ID, "step", call.Step, "error", err)
		}
	}
}

// charge adds the pacing of one HTTP attempt to the model call in flight (going.Pacer.Paced):
// the tokens the pacer charged it, those it drew from its share and how long it held it.
// agentloop reads the endpoint's token, and so paces the attempt, inside the Run runModel makes
// while the call is in flight.
func (w *worker) charge(charge going.Charge) {
	if call := w.inFlight.Load(); call != nil {
		call.charge(charge.Tokens, charge.Drawn, charge.Held)
	}
}

// modelCall is one model call of a run in flight (runModel): when it was asked, the times of its
// first and last tokens, how long the pacer held its HTTP attempts, the tokens it charged them and
// those it drew from its share for them, and the prose it streams (agentloop.Request.OnText),
// which the first call frame of its step carries - the prose so far as that call starts, since
// the stream and the step's tools run on goroutines of their own. Its first token is its first
// delta of prose, none for a reply of tool calls alone. agentloop runs a step's tools once its
// stream ended, and gives the step to OnStep once they ran, so its last token is the start of its
// first tool call (toolRun), or the step's OnStep for a reply that calls none (saveReply), or the
// return of agentloop.Run for a call that drew no reply. The pacer holds an attempt inside
// agentloop.Run, after the call was asked, so the call's time to its last token holds the pacer's
// hold.
type modelCall struct {
	asked time.Time

	mu                    sync.Mutex
	text                  strings.Builder
	told                  bool
	firstToken, lastToken time.Time
	held                  time.Duration
	reserved, drawn       int
}

// add adds delta, the next of the prose the model call streamed.
func (c *modelCall) add(delta string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.firstToken.IsZero() {
		c.firstToken = time.Now()
	}
	c.text.WriteString(delta)
}

// first is the prose streamed so far, "" for none, the first time a call frame of the step asks
// for it, and nil each later time.
func (c *modelCall) first() *string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.told {
		return nil
	}
	c.told = true
	text := c.text.String()
	return &text
}

// ended marks at as the call's last token, where nothing marked one before, and answers how long
// the call took from its request to its last token and to its first token, 0 for none.
func (c *modelCall) ended(at time.Time) (took, first time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastToken.IsZero() {
		c.lastToken = at
	}
	if !c.firstToken.IsZero() {
		first = c.firstToken.Sub(c.asked)
	}
	return c.lastToken.Sub(c.asked), first
}

// charge adds one HTTP attempt's pacing: the tokens the pacer charged it, those it drew from its
// share and how long it held it.
func (c *modelCall) charge(tokens, drawn int, held time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserved += tokens
	c.drawn += drawn
	c.held += held
}

// pacing is how long the pacer held the call's HTTP attempts, the tokens it charged them and
// those it drew from its share for them, each summed over the attempts so far.
func (c *modelCall) pacing() (held time.Duration, reserved, drawn int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held, c.reserved, c.drawn
}

// save saves one step of the run with body, and keeps it for the history of the next step. A
// step the server refuses as no longer held loses the run; any other failure fails the step's
// call to the server, which the run's next step makes again.
func (w *worker) save(request agentwire.StepRequest, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		slog.Error("agent: encode a step", "run", w.run.ID, "step", request.Step, "error", err)
		return
	}
	request.Run, request.LeaseToken, request.Body = w.run.ID, w.run.LeaseToken, encoded
	if _, err := w.agent.client.Step(w.ctx, request); err != nil {
		var refused *agentwire.Error
		if errors.As(err, &refused) && refused.Code == agentwire.CodeStaleLease {
			w.lose()
			return
		}
		if w.ctx.Err() == nil {
			slog.Warn("agent: save a step", "run", w.run.ID, "step", request.Step, "error", err)
		}
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stepped = append(w.stepped, agentwire.SavedStep{Step: request.Step, Kind: request.Kind, Body: encoded, Note: request.Note})
	// The model step is saved after the tool steps it asked for, so the history keeps the
	// numbers' order.
	for i := len(w.stepped) - 1; i > 0 && w.stepped[i].Step < w.stepped[i-1].Step; i-- {
		w.stepped[i], w.stepped[i-1] = w.stepped[i-1], w.stepped[i]
	}
}

// end ends the run's turn in state, with reason and offers, adding what the turn spent. A run
// that is no longer held, or that was stopped, is ended by nobody: its new owner, or the stop,
// ended it.
func (w *worker) end(state, reason string, offers []string) {
	if w.lost.Load() {
		return
	}
	// The end is the run's last word, made even as the agent stops, within its own bound.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), 10*time.Second)
	defer cancel()
	request := agentwire.EndRequest{Run: w.run.ID, LeaseToken: w.run.LeaseToken, State: state, Reason: reason, Used: w.turn, Offers: offers}
	if err := w.agent.client.End(ctx, request); err != nil {
		var refused *agentwire.Error
		if !errors.As(err, &refused) || refused.Code != agentwire.CodeStaleLease {
			slog.Warn("agent: end a run", "run", w.run.ID, "state", state, "error", err)
		}
	}
}
