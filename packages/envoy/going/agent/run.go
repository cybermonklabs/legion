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
	// tool result of the run withheld from it (withhold).
	codeContentFilter = "MODEL_CONTENT_FILTER"
	// codeAgentFailed is a run the agent's own code failed on: a step it could not save, a
	// history it could not read, a callback of its loop that panicked.
	codeAgentFailed = "AGENT_FAILED"
)

// The frames a run's steps show its person's bar.
const (
	// frameTool is one tool call, as it ended: toolFrame.
	frameTool = "tool"
	// frameReply is one model reply: replyFrame.
	frameReply = "reply"
	// frameBlock is the block a draw_block call drew, as the server answered it
	// (going.BlockData), shown beside the run's answer.
	frameBlock = "block"
)

// toolFrame is the data of a tool frame: the step, the tool, its one-line note and whether it
// was refused.
type toolFrame struct {
	Step  int    `json:"step"`
	Tool  string `json:"tool"`
	Note  string `json:"note"`
	Error bool   `json:"error"`
}

// replyFrame is the data of a reply frame: the step, the reply's prose and the tools it called.
type replyFrame struct {
	Step  int      `json:"step"`
	Text  string   `json:"text"`
	Tools []string `json:"tools"`
}

// errStepTaken ends agentloop.Run once its one model call of a step finished and was saved: each
// step starts the loop again from the saved steps.
var errStepTaken = errors.New("the step's model call finished and was saved")

// errReplyFiltered ends agentloop.Run once its one model call of a step finished with a reply the
// chat model's content filter stopped, which is not saved: the step is made again (withhold).
var errReplyFiltered = errors.New("the chat model's content filter stopped the step's reply")

// finishContentFilter is agentloop.Step.Finish of a reply the content filter stopped.
const finishContentFilter = "content-filter"

// worker is one claimed run of the agent's: the run, its level, what it has spent, its history,
// and its own context, which a stop, a lease the run no longer holds, the watchdog and the
// agent's end each cancel.
type worker struct {
	agent *agent
	run   *agentwire.ClaimedRun
	name  string
	level Level

	// used is what the run has spent, its earlier turns' and this one's; turn is this turn's
	// alone, which end adds to the run's budget.
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
	w := &worker{agent: a, run: run, name: run.Level, history: run.History, release: release}
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

// work runs the run's turn: the rows of its plan that are approved and not yet run, then a step at
// a time from its saved steps, until it answers, proposes a plan, spends a bound of its level,
// fails, or stops being the agent's to work. A plan proposed in Auto-review is reviewed before the
// turn ends (review), and the rows the server approved with the plan run at once, which ends the
// turn (runApproved): the create of a new-issue run. A new-issue run whose create ran ends there.
// A run whose saved steps name a tool this build does not know fails at once (unknownTool).
func (w *worker) work() {
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

// settled reports whether the run's saved steps already end its turn: a last step that answered
// with no tool call, or that proposed a plan, which waits for its person.
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
		if json.Unmarshal(last.Body, &body) == nil && len(body.Calls) == 0 {
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
// tools it calls; the model call is saved (OnStep), each tool call as it ends (toolRun). The
// watchdog gives up the run when the step runs past its model call's time and watchdogGrace. A
// call the chat model's content filter refused, or whose reply it stopped, saves nothing, and
// the step is made again (withhold). The run's last model call, which is offered no tool, is told
// so (lastCallNote).
func (w *worker) step() stepOutcome {
	messages, err := conversation(w.run, lastWhole(w.history), w.withheld)
	if err != nil {
		return stepOutcome{state: agentwire.StateFailed, reason: fmt.Sprintf("%s: %v", codeAgentFailed, err)}
	}
	base := w.next
	var finish string
	var calls []string
	w.proposed.Store(false)
	system := w.system()
	prompt := len(system)
	for _, message := range messages {
		for _, block := range message.Blocks {
			prompt += len(block.Text) + len(block.Input)
			for _, part := range block.Content {
				prompt += len(part.Text)
			}
		}
	}
	request := w.level.request(w.agent.config.Model, w.used)
	if request.Bounds.Calls == 1 {
		messages = withLastCallNote(messages)
	}
	request.Model = w.agent.pacer.paced(request.Model, func() int { return prompt/charsPerToken + w.level.OutputTokens })
	request.System, request.Input, request.Tools = system, messages, w.tools(base)
	request.OnStep = func(step agentloop.Step) error {
		if step.Finish == finishContentFilter {
			return errReplyFiltered
		}
		finish = step.Finish
		calls = w.saveReply(base, step)
		return errStepTaken
	}
	if w.agent.config.Hooks {
		request.OnText = hookOnText
	}
	result, kept, err := w.runModel(request)
	switch {
	case !kept || w.lost.Load() || w.grantEnded.Load() != nil || w.ctx.Err() != nil:
		return stepOutcome{}
	case errors.Is(err, errStepTaken):
		w.filterRefused = false
		switch {
		case finish == "length":
			return stepOutcome{state: agentwire.StateStopped, reason: w.level.cut(w.name), offers: []string{agentwire.OfferContinue}}
		case w.proposed.Load():
			return stepOutcome{state: agentwire.StateWaiting}
		case len(calls) == 0:
			return stepOutcome{state: agentwire.StateDone}
		}
		return stepOutcome{}
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
// (saveReply). A call refused with no such result left to withhold ends the run failed with
// codeContentFilter and the provider's detail. A step that saves its model call makes the next
// refusal a first one again.
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
		return stepOutcome{state: agentwire.StateFailed, reason: fmt.Sprintf(
			"%s: the chat model's content filter refused a model call of this run with every tool result of the run withheld from it (%s)",
			codeContentFilter, note(detail))}
	}
	slog.Warn("agent: the chat model's content filter refused a model call again; its tool results are withheld and the call is made again",
		"run", w.run.ID, "withheld", added, "detail", note(detail))
	return stepOutcome{}
}

// lastCallNote is what the run's last model call is told, which agentloop offers no tool: a model
// that is not told so writes that it will read more, and ends the turn without an answer, or
// repeats that until its output tokens are spent.
const lastCallNote = "This is the last model call this run's level allows, and it is offered no tool: answer the person now, from what the results above hold, and say what you did not read. Write no promise to read more."

// withLastCallNote is messages with lastCallNote said as the person's last words after them, in
// a message of their own or added to the person's last message.
func withLastCallNote(messages []agentloop.Message) []agentloop.Message {
	told := agentloop.Block{Kind: agentloop.TextBlock, Text: lastCallNote}
	if last := len(messages) - 1; last >= 0 && messages[last].Role == agentloop.User {
		messages[last].Blocks = append(slices.Clone(messages[last].Blocks), told)
		return messages
	}
	return append(messages, agentloop.Message{Role: agentloop.User, Blocks: []agentloop.Block{told}})
}

// runModel makes request's one model call on the run's context, under the watchdog, adds what it
// spent to the run's budget, and adds the steps it saved to the run's history. It reports false
// where the watchdog gave the run up: the run is lost, and nothing more of it is written. A call
// the content filter refused, or whose reply it stopped, spends the run's time and tokens and no
// call of its level, since it answered nothing: it is made again as withhold says, which ends the
// run once a call is refused with every result withheld, and a refusal of a run's last call would
// otherwise end the run with no answer.
func (w *worker) runModel(request agentloop.Request) (agentloop.Result, bool, error) {
	stepCtx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	finished := w.agent.watch(w, request.Bounds.Time+watchdogGrace)
	started := time.Now()
	result, err := agentloop.Run(stepCtx, request)
	working := time.Since(started)
	if !finished() {
		// The watchdog gave the run up: its goroutine was counted stuck, and it writes nothing.
		w.lose()
		return result, false, err
	}
	w.turn.WorkingMS += working.Milliseconds()
	w.used.WorkingMS += working.Milliseconds()
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
// reason names the fault agentloop ended it with and its detail (faultDetail), so the reason says
// why a call that drew no answer failed.
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
	if result.Status == http.StatusTooManyRequests || strings.Contains(result.Detail, errQuotaPastDeadline.Error()) {
		return stepOutcome{retry: true}
	}
	detail := "the chat model answered no reply"
	if result.Status != 0 {
		detail = fmt.Sprintf("the chat model answered %d", result.Status)
	}
	if result.Fault != "" {
		detail += fmt.Sprintf(" (%s: %s)", result.Fault, faultDetail(result.Detail))
	}
	return stepOutcome{state: agentwire.StateFailed, reason: fmt.Sprintf("%s: %s", codeModelFailed, detail)}
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
// called. A call of a tool the run does not have is saved marked unknown (toolCall.Unknown): the
// model made the name up, and a later step reads it as such rather than as a tool of a later build.
func (w *worker) saveReply(base int, step agentloop.Step) []string {
	var body modelBody
	var tools []string
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
			body.Calls = append(body.Calls, toolCall{ID: block.ToolCallID, Tool: block.Name, Arguments: input, Unknown: !known[block.Name]})
			tools = append(tools, block.Name)
		}
	}
	if len(w.withheld) > 0 && (len(tools) == 0 || slices.Contains(tools, toolProposePlan)) {
		body.Text = strings.TrimSpace(body.Text + withheldTold(w.withheld))
	}
	body.Output = step.Usage.Output
	line := note(body.Text)
	if len(tools) > 0 {
		line = note(fmt.Sprintf("%s [calls %s]", body.Text, strings.Join(tools, ", ")))
	}
	frame, _ := json.Marshal(replyFrame{Step: base, Text: body.Text, Tools: append([]string{}, tools...)})
	w.save(agentwire.StepRequest{Step: base, Kind: agentwire.StepModel, Note: line, Frames: []agentwire.Frame{{Name: frameReply, Data: frame}}}, body)
	return tools
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

// toolRun is the agentloop Run of tool in the step whose model call is numbered base: it makes the
// call do makes as the step numbered after base by the tool call's index, and saves it as that
// step, with what the model is told of its result. A do that panics is saved as a refusal naming
// the panic, and the panic goes on to agentloop's recover, so the run's next step, which starts
// from the saved steps, tells the model the tool failed.
func (w *worker) toolRun(base int, tool string, do func(ctx context.Context, step int, input json.RawMessage) (string, bool)) func(context.Context, agentloop.ToolCall) (string, bool) {
	return func(ctx context.Context, call agentloop.ToolCall) (string, bool) {
		step := base + call.Index
		input := call.Input
		if !json.Valid(input) {
			input = json.RawMessage(`{}`)
		}
		saveStep := func(result string, refused bool) {
			if w.lost.Load() {
				return
			}
			line := note(fmt.Sprintf("%s %s: %s", tool, input, result))
			frame, _ := json.Marshal(toolFrame{Step: step, Tool: tool, Note: line, Error: refused})
			frames := []agentwire.Frame{{Name: frameTool, Data: frame}}
			if block, drew := w.drawn.LoadAndDelete(step); drew {
				frames = append(frames, agentwire.Frame{Name: frameBlock, Data: block.(json.RawMessage)})
			}
			w.save(agentwire.StepRequest{Step: step, Kind: agentwire.StepTool, Note: line, Frames: frames},
				toolBody{Call: toolCall{ID: call.ID, Tool: tool, Arguments: input}, Result: result, Error: refused})
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				saveStep(fmt.Sprintf("the tool %s failed: it panicked (%v)", tool, recovered), true)
				panic(recovered)
			}
		}()
		result, refused := do(ctx, step, call.Input)
		saveStep(result, refused)
		return result, refused
	}
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
