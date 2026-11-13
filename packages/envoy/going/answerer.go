package going

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/contracts"
	"golang.org/x/time/rate"
)

// charsPerToken is how many characters of a request are counted as one token. It is
// embed.EstimateTokens' own rule, cited rather than imported: that package links the AWS SDK and
// this one stays thin enough for the daemon to speak the same stream. Four characters a token is
// a rough estimate for English prose, which is what pacing a shared quota needs and all it needs;
// it is never exact, and dense text costs more.
const charsPerToken = 4

// answerOutputTokens is the most tokens one model call of a run may write. The answer itself is
// at most contracts.GoingAnswerWordMax words, about 200 tokens, and the block's spec and the
// actions' arguments a few hundred more; the rest is room for the reasoning a reasoning model
// counts against the same bound, so a model that thinks before it answers is not cut off at its
// first sentence. A reply past it ends the run with agentloop.FaultLength.
const answerOutputTokens = 4096

// answerRules is the system message every run sends: the rules an answer keeps, stated once
// because they never change between runs. It names the bounds and the tool from contracts, so the
// rules the model is given and the bounds the server enforces cannot drift apart.
var answerRules = fmt.Sprintf(`You answer one question from the items you are given, which is what Dispatch holds on it.
Answer in at most %d words. An answer past that is cut where it passes them.
Answer only from the items given to you. When they do not hold the answer, say that Dispatch holds no answer to the question; never answer from anything else you know.
Link each item you use as a markdown link whose target is that item's own href, exactly as it is given to you.
Never link anything else, and write no bare web address, host or email address in your words: a link to any other target reaches the reader as its own visible text alone, with the target dropped, and a word a reader's renderer would make a link of on its own is dropped from the answer whole, with the reader told the answer was changed.
Call %s to search Dispatch again when the items you have do not answer the question. It takes q, the words to search for.
Call the other tools to read the counts, the progress, the roster, the activity and the delivery figures Dispatch holds when the question needs them. Every number you write comes from an item or a read you were given.`,
	contracts.GoingAnswerWordMax, contracts.GoingSearchTool)

// searchToolSchema is the one tool's arguments: the words to search for, and nothing else. The
// server supplies the project, the limit and the offset, so there is no field for the model to
// widen a search with.
var searchToolSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "q": {"type": "string", "description": "The words to search Dispatch for."}
  },
  "required": ["q"],
  "additionalProperties": false
}`)

// finalSchema is the object an answer is asked to be (R2): the answer's own text, the block drawn
// beside it, and the arguments of the actions offered. It keeps to the subset of JSON Schema
// every service that serves structured outputs takes - one object, every property required,
// additionalProperties false on every object, no union - which schema_test.go holds it to.
// answer is the first property, so it is the first thing the model writes and the first thing
// that streams.
var finalSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "answer": {
      "type": "string",
      "description": "The answer to the question, in the words and with the links the rules ask for."
    },
    "block": {
      "type": "object",
      "description": "One block drawn beside the answer, or kind none for an answer that needs none.",
      "properties": {
        "kind": {
          "type": "string",
          "enum": ["chart", "progress", "table", "issue_list", "agent_status", "metric", "timeline", "none"],
          "description": "The kind of block, or none."
        },
        "title": {"type": "string", "description": "The block's title, or empty for none."},
        "spec": {"type": "string", "description": "The block's specification as JSON text, or empty for none."}
      },
      "required": ["kind", "title", "spec"],
      "additionalProperties": false
    },
    "arguments": {
      "type": "string",
      "description": "The arguments of each action you were offered, as the JSON text of an object keyed by the action's name, each a list of argument objects; {} when you were offered none."
    }
  },
  "required": ["answer", "block", "arguments"],
  "additionalProperties": false
}`)

// errQuotaPastDeadline is a call the quota cannot pace inside the time the run has left. It wraps
// context.DeadlineExceeded because that is what it is - the run reaching its deadline - which is
// the issue's one deadline ending and needs no second code of its own.
var errQuotaPastDeadline = fmt.Errorf("going: the quota cannot pace this call inside the time left: %w", context.DeadlineExceeded)

// What a run's answer call came to, as Outcome.StructuredOutput says it.
const (
	// StructuredOutputAccepted is an answer that came as the Final object.
	StructuredOutputAccepted = "accepted"
	// StructuredOutputRefused is an answer that came as prose: the service refused the
	// structured request with a 400, answered something that did not fit it, or had refused
	// one before, so this run did not ask.
	StructuredOutputRefused = "refused"
)

// Final is the answer as the structured reply wrote it: the answer's text, the block drawn beside
// it, and the arguments of the actions offered, as JSON text the caller parses against each
// action's own schema.
type Final struct {
	Answer    string     `json:"answer"`
	Block     FinalBlock `json:"block"`
	Arguments string     `json:"arguments"`
}

// FinalBlock is a structured reply's block: its kind, BlockNone for none, its title and its
// spec, the spec as the JSON text the model wrote.
type FinalBlock struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Spec  string `json:"spec"`
}

// Outcome is what a run's answer came to beyond the frames it streamed.
type Outcome struct {
	// StructuredOutput is StructuredOutputAccepted or StructuredOutputRefused, and empty for
	// a run that ended on a fault before either could be told.
	StructuredOutput string
	// Final is the structured reply, set exactly when StructuredOutput is accepted. The
	// stream carries its answer; its block and its arguments are the caller's to use.
	Final *Final
}

// Answerer answers one question from what Dispatch holds. It is built once at boot from the
// settings and used by every request, so its quota pacer is the process's one pacer and its
// account of the structured output is the process's one account.
type Answerer struct {
	model   agentloop.Model
	now     func() time.Time
	limiter *rate.Limiter

	// formatRefused is set by the first structured request the service answered 400, and
	// makes every later run ask for prose alone, so a service that serves no structured output
	// costs one call an answer and not two. formatAccepted is set by the first structured
	// reply accepted. A restart forgets both and tries again.
	formatRefused  atomic.Bool
	formatAccepted atomic.Bool
}

// AnswererConfig configures an Answerer.
type AnswererConfig struct {
	// Model is the model that answers, at its endpoint; required. Its endpoint's Token is
	// read for every HTTP attempt, after the quota paced it.
	Model agentloop.Model
	// TokensPerMinute is the deployment's quota, which paces this process's calls. Zero or less
	// is no pacing: the endpoint form names no quota.
	TokensPerMinute int
	// Now replaces time.Now; a test drives the pacer with it.
	Now func() time.Time
}

// NewAnswerer checks cfg's shape and returns an Answerer. It makes no call, so a model service
// that is down at boot never refuses boot.
func NewAnswerer(cfg AnswererConfig) (*Answerer, error) {
	switch {
	case cfg.Model.ID == "":
		return nil, errors.New("going: an answerer needs a model")
	case cfg.Model.Endpoint.BaseURL == "":
		return nil, errors.New("going: an answerer needs its model's endpoint")
	case cfg.Model.Endpoint.Token == nil:
		return nil, errors.New("going: an answerer needs its endpoint's credential")
	}
	answerer := &Answerer{model: cfg.Model, now: cfg.Now}
	if answerer.now == nil {
		answerer.now = time.Now
	}
	if cfg.TokensPerMinute > 0 {
		// One rate.Limiter, as embed.RateLimitedEmbedder paces the embedder, sized at the
		// tokens a minute with a burst of a minute's quota.
		answerer.limiter = rate.NewLimiter(rate.Limit(float64(cfg.TokensPerMinute)/60), cfg.TokensPerMinute)
	}
	return answerer, nil
}

// Blocks reports whether this process's model answers with blocks: blocks once a structured reply
// was accepted, not once the service refused a structured request with a 400, and known false
// while neither has happened. A refusal outweighs an acceptance, since after it no run asks.
func (a *Answerer) Blocks() (blocks, known bool) {
	switch {
	case a.formatRefused.Load():
		return false, true
	case a.formatAccepted.Load():
		return true, true
	}
	return false, false
}

// Request is one question and the reads the answerer makes through its caller, which is what
// lets this package stay outside internal/ while the reads live in the API package. Search and
// Bodies are required; Reads is the caller's to supply or leave empty.
type Request struct {
	// Words is the question.
	Words string
	// Search answers one search of the caller's project at the caller's limit and offset: the
	// server supplies those, so the model cannot widen the search past what the caller may read.
	Search func(ctx context.Context, q string) ([]Hit, error)
	// Bodies answers each hit's own stored text, keyed by Hit.Key, already cut to
	// contracts.GoingHitRuneMax runes per hit. A hit it answers nothing for has no body.
	Bodies func(ctx context.Context, hits []Hit) (map[string]string, error)
	// OnOutcome, when set, is told what the answer came to, once, just before the terminal
	// frame of every run that asked the model, and after every other frame of an answer that
	// ends through the filter, so a frame it emits follows the answer's text. An error from it
	// ends the run there, with no terminal frame, and is the error Run returns, as emit's is.
	OnOutcome func(Outcome) error
	// Offer is what the decision chose for this answer to write: the actions whose arguments
	// the structured reply writes, and the display. The zero value offers no action and asks
	// for no display, which is a run with no decision.
	Offer Offer
	// Reads are the read tools the caller lets this run call beside the search, by tool name
	// (ReadRoster and the rest), each made as the run's author. A name it omits is a tool the
	// model is not offered, and a block whose kind only that read fills is not offered either.
	Reads map[string]ReadFunc
}

// Offer is what the decision model chose before the answer call, for that call to write: the
// actions it offered, each with its own argument schema, whose arguments the reply's arguments
// text carries, and the display the answer is shown in.
type Offer struct {
	// Actions are the actions offered, best first: at most the decision's two best.
	Actions []OfferedAction
	// Display is DisplayChart where the answer is drawn as a chart unasked, which the server
	// settles from the decision's display choice and its measured threshold, and DisplayProse
	// or empty otherwise.
	Display string
}

// OfferedAction is one action the answer writes arguments for: its catalogue name and its
// arguments' schema as JSON, which is the catalogue's own.
type OfferedAction struct {
	Name   string
	Schema json.RawMessage
}

// Run answers Words, emitting the stream's events in order with exactly one terminal event last.
// The error it returns is emit's or OnOutcome's alone: everything else that fails is an event.
//
// A run asks for the Final object first, and its text frames are the answer property's growth
// across the object's partial snapshots. A service that refuses that request with a 400, or
// answers it with a reply that does not fit the schema, is asked the same again for prose, whose
// text frames are the prose itself; the run then has no block and no arguments, and reports the
// refusal.
func (a *Answerer) Run(ctx context.Context, request Request, emit func(Event) error) error {
	run := &answerRun{
		request:   request,
		emit:      emit,
		citations: make(map[string]Hit),
		runesLeft: contracts.GoingRequestRuneMax,
	}
	// The run's window is measured from here, the moment it began: both of its answer calls
	// share it, so a fallback to prose spends what the structured attempt left.
	deadline := time.Now().Add(contracts.GoingRunWindow)
	// The server's own seeding search is the first of contracts.GoingSearchMax: the model gets
	// what the question itself found before it is asked anything.
	items, err := run.search(ctx, request.Words)
	if err != nil {
		return run.readFault(ctx, err)
	}

	// One filter for the whole run, so every piece of the answer is cleared by the same state
	// machine and no half of a link the server would drop ever reaches a reader.
	filter := newAnswerFilter(run.citations, run.send)
	// The calls run on a context of this run's own, which a failed store read cancels: a search
	// the store could not serve leaves the run nothing further to give the model, so no call is
	// made after it.
	answering, stopCalls := context.WithCancel(ctx)
	defer stopCalls()
	run.stop = stopCalls
	run.input = run.ask(items)
	call := agentloop.Request{
		Model:  a.paced(run),
		System: answerRules,
		Tools:  append([]agentloop.Tool{run.searchTool()}, run.readTools()...),
	}

	outcome := Outcome{StructuredOutput: StructuredOutputRefused}
	var result agentloop.Result
	var runErr error
	calls := contracts.GoingModelCallMax
	prose := a.formatRefused.Load()
	if !prose {
		result, runErr = run.attempt(answering, call, deadline, calls, filter, true)
		prose = a.refused(run, result, runErr)
		switch {
		case prose:
		case runErr == nil && result.Fault == "":
			// agentloop accepted the value as an object of finalSchema, every one of which
			// reads as a Final.
			var final Final
			_ = json.Unmarshal(result.Value, &final)
			// The reader is given the answer whole: whatever of it the snapshots had not
			// fed the filter is fed now. The error is emit's, which run.send kept.
			if err := run.grow(filter, final.Answer); err != nil {
				return err
			}
			a.formatAccepted.Store(true)
			outcome = Outcome{StructuredOutput: StructuredOutputAccepted, Final: &final}
		case result.Fault == agentloop.FaultMalformed:
			// The reply did not fit, but its answer had already reached the reader, so the
			// same call asked again for prose would write the answer twice: the answer that
			// streamed stands, with no block and no arguments.
			result = agentloop.Result{}
		default:
			outcome.StructuredOutput = ""
		}
		calls -= len(result.Steps)
	}
	if prose {
		if calls < 1 {
			// The structured attempt spent every call the run had, so there is none left to
			// ask for prose with: the run ends as a run past its call bound does.
			result = agentloop.Result{Fault: agentloop.FaultCalls}
		} else {
			result, runErr = run.attempt(answering, call, deadline, calls, filter, false)
		}
	}
	// runErr is a callback's error or the context ending. The one callback the calls are given
	// here is the filter's, whose error is emit's own, which run.send kept: that is the one
	// error this run hands its caller, and it is handed back unwrapped.
	if run.emitErr != nil {
		return run.emitErr
	}
	// A failed store read ends the run, and it is read before the call's own fault: the fault
	// is this run having stopped the call at that read, and what the reader is owed is the
	// read's own code. The run ends here and nowhere else - no further model call was made,
	// and no tool result told the model the search merely found nothing - so a reader is never
	// written a whole answer this run then disowns with an error. Text the filter had already
	// cleared and emitted stays emitted: it was cleared, and an error frame is terminal, so no
	// end follows it.
	if run.readErr != nil {
		outcome = Outcome{}
		return run.end(outcome, func() error { return run.readFault(ctx, run.readErr) })
	}
	if code := a.faultCode(ctx, run, result, runErr); code != "" {
		return run.end(outcome, func() error { return run.send(Fault(code)) })
	}
	// The model answered, or it spent the call budget: either way what the filter cleared up to
	// here is a whole answer - the budget is the budget - so the run ends through the filter's
	// own Close and not with an error frame. The answer is settled before the caller is told
	// what it came to, so a frame the caller writes then follows every frame of the answer.
	if err := filter.Settle(); err != nil {
		return err
	}
	return run.end(outcome, filter.Close)
}

// refused reports whether a structured attempt's ending is the service refusing the structured
// request, which the run answers by asking again for prose: a 400, which also sets the process's
// flag so no later run asks, or a reply that does not fit the schema, which says less about the
// service than a 400 does and so is answered for this run alone. Neither is a refusal once the
// answer had begun to reach the reader: asked again, the answer would be written twice.
func (a *Answerer) refused(run *answerRun, result agentloop.Result, runErr error) bool {
	if runErr != nil || run.readErr != nil {
		return false
	}
	switch {
	case result.Fault == agentloop.FaultProviderError && result.Status == http.StatusBadRequest:
		// A 400 comes before any of the reply, so no answer has streamed. The flag is set
		// whether or not the prose call can be made, since it is the service that refused.
		a.formatRefused.Store(true)
		slog.Warn("going: the model service refused the structured answer, so this process answers in prose from now on",
			"detail", result.Detail)
		return !run.answered()
	case result.Fault == agentloop.FaultMalformed && !run.answered():
		// The fault alone: the detail of a malformed reply quotes the model's own words, which
		// may echo the question's.
		slog.Warn("going: the model's reply did not fit the structured answer, so this run answers in prose")
		return true
	}
	return false
}

// faultCode is the error code a run's ending carries, or empty for a run that answered or spent
// its call budget, which ends through the filter. Each ending the reader is told one code for is
// said here, once, for an operator: the run's time running out, the service's refusal and
// every other way a call failed. What is logged carries no credential and none of the question's
// words: the detail of a time or provider fault names the call and what the service answered,
// and the fault of a reply is logged without its detail, which quotes the model's own words.
func (a *Answerer) faultCode(ctx context.Context, run *answerRun, result agentloop.Result, runErr error) string {
	switch {
	case runErr != nil && isDeadline(ctx, runErr):
		slog.Warn("going: the run ran out of time with no answer", "error", runErr)
		return contracts.GoingErrorAnswerDidNotArrive
	case runErr != nil:
		slog.Warn("going: the answer call could not be made", "error", runErr)
		return contracts.GoingErrorAnswerFailed
	}
	switch result.Fault {
	case "", agentloop.FaultCalls:
		return ""
	case agentloop.FaultTime:
		slog.Warn("going: the run ran out of time with no answer", "detail", result.Detail)
		return contracts.GoingErrorAnswerDidNotArrive
	case agentloop.FaultProviderError:
		if run.paceFailed.Load() {
			// The quota could not pace a call inside what was left of the window
			// (errQuotaPastDeadline), which is the run reaching its deadline.
			slog.Warn("going: the run ran out of time with no answer", "detail", result.Detail)
			return contracts.GoingErrorAnswerDidNotArrive
		}
		// The reader is told ANSWER_FAILED and no more than that, so this is the one place
		// the refusal is said: an operator whose deployment answers 401 or 429 reads it here
		// and nowhere else.
		slog.Warn("going: the model service refused the run", "status", result.Status, "detail", result.Detail)
		return contracts.GoingErrorAnswerFailed
	default:
		// A reply cut at answerOutputTokens, or a refusal.
		slog.Warn("going: the model gave no answer the run could use", "fault", result.Fault)
		return contracts.GoingErrorAnswerFailed
	}
}

// paced is the answerer's model with the quota's pacing taken in its endpoint's Token, which
// agentloop calls before every HTTP attempt, the retry included, so the quota is charged for each
// attempt the service is sent. Each attempt is charged the estimate of what it sends: the system
// prompt, the question and its items, and the tool output so far.
func (a *Answerer) paced(run *answerRun) agentloop.Model {
	model := a.model
	if a.limiter == nil {
		return model
	}
	token := model.Endpoint.Token
	model.Endpoint.Token = func(ctx context.Context) (string, error) {
		if err := a.pace(ctx, run.estimate()); err != nil {
			run.paceFailed.Store(true)
			return "", err
		}
		return token(ctx)
	}
	return model
}

// pace waits out the quota's own pacing for tokens. A reservation that cannot be served inside
// the time the call has left is refused rather than waited on, because waiting it out would
// spend the whole window to make no call at all.
func (a *Answerer) pace(ctx context.Context, tokens int) error {
	reservation := a.limiter.ReserveN(a.now(), tokens)
	if !reservation.OK() {
		// The call costs more than a minute's whole quota, so no wait would ever serve it.
		return errQuotaPastDeadline
	}
	delay := reservation.DelayFrom(a.now())
	if delay <= 0 {
		return nil
	}
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline && a.now().Add(delay).After(deadline) {
		reservation.CancelAt(a.now())
		return errQuotaPastDeadline
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		reservation.CancelAt(a.now())
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// answerRun is one run's own state: what the model has been given, what is left of the run's
// budgets, and the failures that end the run.
type answerRun struct {
	request Request
	emit    func(Event) error

	// mu orders the run's two writers: the search tool, which adds the hits it finds to the
	// citations, and the callback that feeds the answer to the filter, which reads them at
	// each link and emits what it cleared. agentloop calls them from goroutines of its own.
	mu sync.Mutex
	// citations is every hit given to the model, with or without a body, keyed by href as the
	// filter reads it. The filter holds this same map, so a hit a later search found is citable.
	citations map[string]Hit
	// fed is the answer as the filter has been given it so far: the answer property's text
	// across the structured reply's snapshots, or the prose's.
	fed strings.Builder
	// searches counts the searches made, the seeding search included; runesLeft is what is left
	// of the run's body budget, and spent says it is gone, so the hits after the first body that
	// did not fit are named to the model without one.
	searches  int
	runesLeft int
	spent     bool
	// reads counts the read tools' calls, against readMax.
	reads int
	// input is the question with its items, as both calls are asked it; the structured call
	// is asked it with the offer besides (offerText).
	input string
	// prompt is how many characters the system prompt and the call's input come to, and
	// toolChars how many the tool output of the current call has added, which together are
	// what the next HTTP attempt is charged against the quota.
	prompt    int
	toolChars atomic.Int64
	// paceFailed says the quota refused to pace an attempt, which ends the call as a provider
	// error and the run as its deadline.
	paceFailed atomic.Bool

	// readErr is the first store read that failed, which ends the run: Run sends its code as
	// the run's one ending, and stop has already kept the run from making another call. The
	// first is kept because it is the one that ended the run - a read made after stop answers
	// context.Canceled, which would say the run ran out of time rather than that the store
	// failed.
	readErr error
	// stop ends the run's model calls. Run holds the cancel of the context the calls run on,
	// so a tool that ends the run stops it there rather than letting one more call be made.
	stop context.CancelFunc
	// emitErr is the stream writer's failure, the one failure Run hands back to its caller.
	emitErr error
}

// attempt makes one answer call: structured asks for the Final object, told the offer, and
// streams its answer property's growth, otherwise the call asks for prose and streams the prose,
// with no offer, since prose carries no arguments. It has what is left of the run's window and
// calls model calls.
func (r *answerRun) attempt(ctx context.Context, call agentloop.Request, deadline time.Time, calls int,
	filter *answerFilter, structured bool) (agentloop.Result, error) {
	left := time.Until(deadline)
	if left <= 0 {
		return agentloop.Result{Fault: agentloop.FaultTime, Detail: "the run's window was spent before the call"}, nil
	}
	input := r.input
	if structured {
		input += r.offerText()
	}
	r.prompt = utf8.RuneCountInString(answerRules) + utf8.RuneCountInString(input)
	call.Input = []agentloop.Message{{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: input}}}}
	r.toolChars.Store(0)
	call.Bounds = agentloop.Bounds{Time: left, Calls: calls, OutputTokens: answerOutputTokens}
	if structured {
		call.Final = finalSchema
		call.OnPartial = func(snapshot json.RawMessage) error {
			var partial struct {
				Answer *string `json:"answer"`
			}
			if json.Unmarshal(snapshot, &partial) != nil || partial.Answer == nil {
				return nil
			}
			return r.grow(filter, *partial.Answer)
		}
	} else {
		call.OnText = func(delta string) error { return r.write(filter, delta) }
	}
	return agentloop.Run(ctx, call)
}

// grow feeds the filter what answer adds to the answer fed so far. A snapshot is the whole of
// the object as far as the model has written it, so its answer is the last one's and more; one
// that is not is a snapshot the model went back on, which the reader has already been given
// what it can be given of.
func (r *answerRun) grow(filter *answerFilter, answer string) error {
	r.mu.Lock()
	fed := r.fed.String()
	r.mu.Unlock()
	added, extends := strings.CutPrefix(answer, fed)
	if !extends || added == "" {
		return nil
	}
	return r.write(filter, added)
}

// write feeds the filter one piece of the answer.
func (r *answerRun) write(filter *answerFilter, delta string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fed.WriteString(delta)
	return filter.Write(delta)
}

// answered reports whether any of the answer has reached the filter.
func (r *answerRun) answered() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fed.Len() > 0
}

// end tells the caller what the answer came to, then writes the run's terminal frame.
func (r *answerRun) end(outcome Outcome, terminal func() error) error {
	if r.request.OnOutcome != nil {
		if err := r.request.OnOutcome(outcome); err != nil {
			return err
		}
	}
	return terminal()
}

// estimate is what the next HTTP attempt is charged against the quota before it is made: the
// characters it sends, at charsPerToken. It counts the request alone, which is what a call can
// be charged for before its reply exists.
func (r *answerRun) estimate() int {
	return (r.prompt + int(r.toolChars.Load())) / charsPerToken
}

// send hands one event to the stream's writer and keeps its failure, which is the only error a
// run reports to its caller.
func (r *answerRun) send(event Event) error {
	if err := r.emit(event); err != nil {
		r.emitErr = err
		return err
	}
	return nil
}

// readFault ends the run on a store read that failed: the reader is told one code, and the
// read's own error is said here, where the run's time fault and the provider's refusal are
// said, so an operator whose store refused a read of an answer reads why. It is the one place
// that error is logged, and it carries no credential and none of the question's words.
func (r *answerRun) readFault(ctx context.Context, err error) error {
	slog.Warn("going: a store read the answer needed failed", "error", err)
	return r.send(Fault(readFaultCode(ctx, err)))
}

// search makes one search of the run and renders its hits for the model. Every hit found joins
// the citation set, with or without a body; the bodies are spent in rank order against the run's
// one rune budget, so a hit past it is still named to the model, with its kind, its id and its
// title and no body.
func (r *answerRun) search(ctx context.Context, q string) (string, error) {
	r.searches++
	hits, err := r.request.Search(ctx, q)
	if err != nil {
		return "", err
	}
	bodies, err := r.request.Bodies(ctx, hits)
	if err != nil {
		return "", err
	}
	var items strings.Builder
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, hit := range hits {
		r.citations[hit.Href] = hit
		body := bodies[hit.Key()]
		if body != "" {
			if runes := utf8.RuneCountInString(body); r.spent || runes > r.runesLeft {
				r.spent = true
				body = ""
			} else {
				r.runesLeft -= runes
			}
		}
		writeItem(&items, hit, body)
	}
	return items.String(), nil
}

// searchTool is the one tool the model may call. Its own Run closure holds the search budget,
// because a tool's budget is the tool's own logic: a search past contracts.GoingSearchMax is
// refused back to the model, in words it can act on, and ends no run. A search the store could
// not serve is the one thing that does end it: the alternative is telling the model to answer
// from what it has and then disowning the answer it writes, which is an answer a reader was
// given and then told to ignore. agentloop runs a turn's calls one at a time, so the budget
// needs no lock of its own.
func (r *answerRun) searchTool() agentloop.Tool {
	return agentloop.Tool{
		Name: contracts.GoingSearchTool,
		Description: fmt.Sprintf(
			"Search Dispatch again for the words q. One run searches at most %d times, and the search that found the items you were given was the first.",
			contracts.GoingSearchMax),
		Schema: searchToolSchema,
		Run: func(ctx context.Context, call agentloop.ToolCall) (string, bool) {
			output, isError := r.searchCall(ctx, call)
			r.toolChars.Add(int64(utf8.RuneCountInString(output)))
			return output, isError
		},
	}
}

// searchCall answers one call of the search tool.
func (r *answerRun) searchCall(ctx context.Context, call agentloop.ToolCall) (string, bool) {
	// The calls of one turn stop where the run does: a call that ended it (a search the store
	// could not serve) cancels the context, and a search made after it would run against a
	// store that can only answer context.Canceled. The run is over, so no result is owed.
	if r.readErr != nil || ctx.Err() != nil {
		return "This run has ended.", true
	}
	var arguments struct {
		Q string `json:"q"`
	}
	if err := json.Unmarshal(call.Input, &arguments); err != nil || strings.TrimSpace(arguments.Q) == "" {
		return "That call named no words to search for. Call it again with q, the words to search Dispatch for.", true
	}
	if r.searches >= contracts.GoingSearchMax {
		return fmt.Sprintf(
			"No search is left: one run searches Dispatch at most %d times, and the search that found the items you were given was the first. Answer from the items you have, or say that Dispatch holds no answer.",
			contracts.GoingSearchMax), false
	}
	items, err := r.search(ctx, arguments.Q)
	if err != nil {
		// The run ends on this read, so no further call is made and this result reaches no
		// model (Answerer.Run reads readErr before the call's own fault).
		r.readErr = err
		r.stop()
		return "That search could not be served. This run ends here.", true
	}
	if items == "" {
		return "That search found no item. Answer from the items you have, or say that Dispatch holds no answer.", false
	}
	return "That search found these items:\n" + items, false
}

// ask is the question as the model is asked it: the words, then every item the seeding search
// found.
func (r *answerRun) ask(items string) string {
	var out strings.Builder
	out.WriteString("question: ")
	out.WriteString(r.request.Words)
	out.WriteString("\n\nThe items Dispatch holds for it:\n")
	if items == "" {
		out.WriteString("\nNone: the search for this question found nothing.\n")
		return out.String()
	}
	out.WriteString(items)
	return out.String()
}

// offerText is what the structured call is told beside the question: the blocks it may draw -
// the kinds the words name, and a chart where the decision settled on one, each only where one
// of the run's reads fills it (BlockGuide) - and the actions the decision offered, each with its
// argument schema, whose arguments the reply's arguments text carries, keyed by the action's
// name, each a list of argument objects. A run with no offer is told only that it offers
// nothing, so its arguments are {}.
func (r *answerRun) offerText() string {
	offer := r.request.Offer
	chart := offer.Display == DisplayChart
	var out strings.Builder
	out.WriteString(BlockGuide(OfferedKinds(r.request.Words, chart, r.request.Reads), chart))
	if len(offer.Actions) == 0 {
		out.WriteString("\n\nYou are offered no action: write arguments as {}.\n")
		return out.String()
	}
	out.WriteString("\n\nThe words may ask Dispatch to perform one of these actions. In arguments, write the JSON text of an object keyed by each action's name, ")
	out.WriteString("each a list of argument objects, one for each item the action would change, holding only the arguments that action's schema names and only those the words give. ")
	out.WriteString("Leave an action out when the words ask nothing of it.\n")
	for _, action := range offer.Actions {
		out.WriteString("\n--- action\nname: ")
		out.WriteString(action.Name)
		out.WriteString("\nschema: ")
		out.Write(action.Schema)
		out.WriteString("\n")
	}
	return out.String()
}

// writeItem renders one hit as the model reads it: what it is, how it is linked, and its own text
// when the run's budget had room for it.
func writeItem(out *strings.Builder, hit Hit, body string) {
	out.WriteString("\n--- item\nkind: ")
	out.WriteString(hit.Kind)
	out.WriteString("\nid: ")
	out.WriteString(hit.ID)
	out.WriteString("\ntitle: ")
	out.WriteString(hit.Title)
	out.WriteString("\nhref: ")
	out.WriteString(hit.Href)
	out.WriteString("\n")
	if body == "" {
		return
	}
	out.WriteString("body:\n")
	out.WriteString(body)
	out.WriteString("\n")
}

// readFaultCode names what a failed store read ends the run with: the run ran out of time, or the
// store failed.
func readFaultCode(ctx context.Context, err error) string {
	if isDeadline(ctx, err) {
		return contracts.GoingErrorAnswerDidNotArrive
	}
	return contracts.GoingErrorAnswerFailed
}

// isDeadline reports whether err is this run running out of time: the context ended, or a call
// could not be made inside the time the run had left. Both are one ending, so the caller needs no
// second code for the quota.
func isDeadline(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}
