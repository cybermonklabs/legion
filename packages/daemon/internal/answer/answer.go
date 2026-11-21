// Package answer is the daemon's model loop: it answers one question to an agent that is not
// running, as that agent, by resuming the agent's own conversation on the model the conversation
// last ran on, with the loop's tools (the slice in tools.go, holding gh), and returns the agent's
// reply in its structured form, the turns to append to the conversation, and what the answer
// cost.
//
// The model calls themselves are agentloop's (github.com/sjawhar/envoy/agentloop), the one model
// loop the daemon and Dispatch share and the only import of Grafana's SDK: the providers, the
// retry, the step bound with no tool on the last allowed call and the message that call carries,
// the thinking and reasoning options, the cache marks, the check of the reply against its schema,
// the one ask again for a reply the form's own rules refuse and the one call again for a reply
// the provider cut at output_tokens are all its. This package is its caller: it builds the
// request (the conversation as agentloop's messages, the system prompt, the reply form as the
// request's Final schema, the form's own rules as its Check, the gh tool described for the
// agent's repository, the last call's message, the bounds and the endpoint `answers.providers`
// names, whose key is read again for every call), decides the faults that need no model call
// (no_provider, no_price, unknown_format), refuses an answer whose calls the provider metered
// nothing for (unmetered), maps agentloop's faults onto the daemon's own reasons, ends a run the
// project's pause comes during before its next model call or gh call (ErrPaused), and reads the
// run back as the agent's reply, the turns to append (an answer's, or those of a run that gave
// none, for the caller to end with its own reply), a cut call agentloop made again left out, and
// what the answer cost.
package answer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/sjawhar/envoy/agentloop"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Kind is what the agent's reply is.
type Kind string

const (
	// Answered is an answer to the question.
	Answered Kind = "answered"
	// NotAnswered is a reply that the agent could not answer from its conversation and what its
	// tools read, with what it would need. It is also the kind of every reply with a Fault, which
	// carries no text of the agent's.
	NotAnswered Kind = "not_answered"
	// NeedsWork is a reply that the message asks for work the agent's tools cannot do, such as work
	// needing its working copy or a run of its phase; nothing starts.
	NeedsWork Kind = "needs_work"
	// None is a reply that the message asks nothing of the agent. Only a decision message and an
	// agent's Envoy message may get it (Request.MayAskNothing).
	None Kind = "none"
)

// Fault is why a question got no answer from the model: a bound it passed, or what was wrong.
// The first six are agentloop's own faults under the daemon's names (faultOf); the last three the
// daemon decides itself, before any model call.
type Fault string

const (
	// AnswerSeconds is the answer_seconds bound: the answer ran past its time.
	AnswerSeconds Fault = "answer_seconds"
	// ModelCalls is the model_calls bound: the last allowed model call still called a tool.
	ModelCalls Fault = "model_calls"
	// Length is a reply cut off at the output_tokens bound that agentloop did not make its call
	// again for: a second cut, or one on the last model call model_calls allows.
	Length Fault = "length"
	// Refusal is a last reply the provider refused or filtered.
	Refusal Fault = "refusal"
	// Malformed is a last reply that is not exactly one JSON object of the reply form, or one the
	// form takes but the daemon's own rules refuse (a kind the message does not offer, a reply
	// naming no need), or turns no appender could write.
	Malformed Fault = "malformed"
	// ProviderError is a model call that failed: an error status after agentloop's one retry, a
	// stream that broke, or a key that could not be read; and a request agentloop refused to run
	// at all, which is the daemon's own bug and is logged with its detail.
	ProviderError Fault = "provider_error"
	// NoProvider is a conversation whose model's provider `answers.providers` does not name, or
	// whose provider API the loop does not call; no model call is made.
	NoProvider Fault = "no_provider"
	// NoPrice is a conversation whose model `answers.prices` does not price; no model call is
	// made.
	NoPrice Fault = "no_price"
	// UnknownFormat is a conversation holding history the loop cannot send as its harness would
	// (a provider-side compaction, a native item of a kind it does not know); no model call is
	// made.
	UnknownFormat Fault = "unknown_format"
	// Unmetered is a run no model call of which the provider reported usage for: agentloop saw no
	// counts at all (agentloop.Usage.Metered false), or every call came back with zero input,
	// output and cache tokens, which is what a gateway that strips usage looks like through a
	// provider whose counts are never absent, only zero (the Anthropic adapter fills them in).
	// Either way the answer cannot be priced and would cost the project's daily bound nothing
	// however many calls it made. An answer nothing meters is no answer: the reply is dropped,
	// the run's turns are appended to the agent's conversation as any fault's are, and the
	// question goes to the controller, as an unpriced model's does (NoPrice).
	Unmetered Fault = "unmetered"
)

// faultOf is agentloop's fault under the daemon's own name, which is what the controller reads as
// an agent-question's reason and what the daemon's reply to the asker names.
func faultOf(fault agentloop.Fault) Fault {
	switch fault {
	case agentloop.FaultTime:
		return AnswerSeconds
	case agentloop.FaultCalls:
		return ModelCalls
	case agentloop.FaultLength:
		return Length
	case agentloop.FaultRefusal:
		return Refusal
	case agentloop.FaultMalformed:
		return Malformed
	default:
		return ProviderError
	}
}

// GH runs one gh call the model makes: its seq among the answer's calls, from 1, and its
// arguments, a JSON array of strings, gh's arguments without `gh`. The daemon's runs ghtool.Run
// with the answer's ghtool.Answer and bounds, which refuses a call past the gh_calls bound.
type GH interface {
	Run(ctx context.Context, seq int, args string) ghtool.Result
}

// Agent is the stopped agent that answers: its role, its issue, its session and the phase the
// daemon has it in; and GHUsage, how it calls the gh tool (its issue's repository and, where the
// daemon's record holds one, the issue's pull request), which the gh tool's description and the
// system prompt name in the words the tool's own refusals use (ghtool.Agent.Usage, from the same
// record).
type Agent struct {
	Role    claim.Role
	Issue   string
	Session string
	Phase   string
	GHUsage ghtool.Usage
}

// Question is what the agent is asked: its id (which the log line names and the append records),
// its text, who asked and where.
type Question struct {
	ID    string
	Text  string
	Asker string
	Where string
}

// Request is one question to answer.
type Request struct {
	// Conversation is the agent's conversation as its harness rebuilds it on resume; the answer
	// runs on its model, at its thinking level.
	Conversation conversation.Conversation
	Agent        Agent
	// ChangeNote is what has changed on the agent's issue since it stopped (its Dispatch status,
	// its phase, its pull request's state and head), as the daemon writes it.
	ChangeNote string
	Question   Question
	// MayAskNothing is a decision message or an agent's Envoy message, which may carry a question
	// or not: only then may the agent reply None.
	MayAskNothing bool
	// Earlier are the gh calls an earlier run of this question already made, in seq order, each
	// with its result; a call with no Result is of unknown outcome. New calls are numbered after
	// them.
	Earlier []store.AnswerToolCall
	// Providers, Prices and Bounds are `answers`' (config.Answers).
	Providers map[config.AnswerProvider]config.AnswerEndpoint
	Prices    map[string]config.AnswerPrice
	Bounds    config.AnswerBounds
	// GH runs the gh tool's calls.
	GH GH
	// Paused says whether the project is paused; nil is never paused. It is read before the
	// first model call, before each gh call and as each model call ends, once the gh calls it
	// asked for have returned, and a run that finds the project paused ends with ErrPaused
	// before its next model call or gh call.
	Paused func() bool
	// Log takes the answer's `answers: answered` line and the model loop's warnings
	// (agentloop.Request.Log); slog.Default() when nil.
	Log *slog.Logger
}

// paused is whether req's project is paused (Request.Paused).
func (req Request) paused() bool { return req.Paused != nil && req.Paused() }

// Reply is the agent's reply to a question and what it cost.
type Reply struct {
	// Kind, Text and Needs are the agent's reply in its form: Needs is what the agent would need,
	// for NotAnswered and NeedsWork. A reply with a Fault has Kind NotAnswered and no text.
	Kind  Kind
	Text  string
	Needs string
	// Fault is why the model gave no answer, "" when it replied in the form; Detail says more
	// (the provider's error, the text that was not in the form), for the daemon's log.
	Fault  Fault
	Detail string
	// Append is the question and the model's turns (thinking with its signatures or OpenAI
	// reasoning items, each tool call with its result, a reply the form refused with the
	// sentence it was asked again with, the reply), for the conversation's appender. With a
	// Fault it holds every turn a model call returned before the run ended and no answer, which
	// the caller ends with its own reply in the agent's name (conversation.Append.NoAnswer), so
	// the agent's conversation holds every gh call the run made, a write among them; it holds
	// no turns where no call returned anything (a fault decided before any model call, or a
	// first call that failed). It holds nothing else the loop told the model: not the system
	// prompt, and not the message the last model call carries (lastCall), which is the loop's
	// own instruction to that call and no part of what the agent did, so the agent's
	// conversation never holds it. Nor does it hold a reply the provider cut at output_tokens
	// whose call agentloop made again (agentloop.Step.Dropped): no later call was sent it.
	Append conversation.Append
	// Calls is how many model calls the answer made, a cut call made again included; Usage
	// their tokens and costs summed, cache writes and reads included, reasoning counted as
	// output; CostUSD the sum of its costs.
	Calls   int
	Usage   conversation.Usage
	CostUSD float64
}

// The provider APIs the loop calls, as a conversation names them; each is the protocol agentloop
// speaks to that API.
const (
	anthropicMessages = string(agentloop.AnthropicMessages)
	openaiResponses   = string(agentloop.OpenAIResponses)
)

// ErrPaused is the error of an answer its request's pause check stopped (Request.Paused): the
// project was paused before the first model call, as a model call ended, or before or during a gh
// call, and the run ended there, with no later model call or gh call. It is no Fault: the question
// has no answer yet, and is answered once the project resumes, given the gh calls this run made
// (Request.Earlier).
var ErrPaused = errors.New("answer: the project is paused")

// Answer answers req's question. Every outcome the question has is a Reply: an answer, the
// agent's reply that it cannot answer, or no answer with its Fault. The error is for a request
// the loop cannot run at all (no question, no gh tool, no repository, no bounds), for ctx
// ending, when the daemon itself stops, and ErrPaused for a run the project's pause stopped; the
// Reply then still carries what the calls made so far cost.
func Answer(ctx context.Context, req Request) (Reply, error) {
	if err := req.validate(); err != nil {
		return Reply{}, err
	}
	log := req.Log
	if log == nil {
		log = slog.Default()
	}
	reply, err := answer(ctx, req)
	if reply.Fault != "" {
		reply.Kind = NotAnswered
	}
	attrs := []any{
		"question", req.Question.ID, "issue", req.Agent.Issue, "role", req.Agent.Role, "session", req.Agent.Session,
		"model", req.Conversation.Model, "kind", reply.Kind, "fault", reply.Fault, "calls", reply.Calls,
		"inputTokens", reply.Usage.InputTokens, "outputTokens", reply.Usage.OutputTokens,
		"cacheWriteTokens", reply.Usage.CacheWriteTokens, "cacheReadTokens", reply.Usage.CacheReadTokens,
		"reasoningTokens", reply.Usage.ReasoningTokens, "costUSD", reply.CostUSD,
	}
	if reply.Detail != "" {
		attrs = append(attrs, "detail", reply.Detail)
	}
	if errors.Is(err, ErrPaused) {
		attrs = append(attrs, "paused", true)
	}
	log.Info("answers: answered", attrs...)
	return reply, err
}

func (req Request) validate() error {
	switch {
	case req.Question.ID == "" || strings.TrimSpace(req.Question.Text) == "":
		return errors.New("answer: the request names no question")
	case req.GH == nil:
		return errors.New("answer: the request has no gh tool")
	case req.Agent.GHUsage.Repository.IsZero():
		return errors.New("answer: the request names no repository for the gh tool to call on")
	case req.Bounds.Answer <= 0 || req.Bounds.ModelCalls <= 0 || req.Bounds.OutputTokens <= 0:
		return errors.New("answer: the request has no answer_seconds, model_calls or output_tokens bound")
	}
	return nil
}

// route is the provider a conversation's model is called through.
type route struct {
	provider config.AnswerProvider
	// kind is the protocol agentloop speaks to that API.
	kind agentloop.Kind
	// model is the model's id at its provider ("claude-opus-5-5").
	model    string
	endpoint config.AnswerEndpoint
	price    config.AnswerPrice
}

// routeOf is the provider, endpoint and price of the conversation's model: `anthropic/…` on
// Anthropic Messages, `openai/…` on OpenAI Responses, each only where `answers.providers` names
// the provider and `answers.prices` prices the model.
func routeOf(req Request) (route, Fault, string) {
	conv := req.Conversation
	name, model, ok := strings.Cut(conv.Model, "/")
	if !ok || model == "" {
		return route{}, NoProvider, fmt.Sprintf("the conversation's model %q names no provider", conv.Model)
	}
	r := route{provider: config.AnswerProvider(name), model: model}
	switch {
	case r.provider == config.AnswerAnthropic && conv.API == anthropicMessages:
		r.kind = agentloop.AnthropicMessages
	case r.provider == config.AnswerOpenAI && conv.API == openaiResponses:
		r.kind = agentloop.OpenAIResponses
	default:
		return route{}, NoProvider, fmt.Sprintf("the loop calls no provider %s on API %q", name, conv.API)
	}
	endpoint, ok := req.Providers[r.provider]
	if !ok {
		return route{}, NoProvider, fmt.Sprintf("answers.providers names no %s", name)
	}
	price, ok := req.Prices[conv.Model]
	if !ok {
		return route{}, NoPrice, fmt.Sprintf("answers.prices prices no %s", conv.Model)
	}
	r.endpoint, r.price = endpoint, price
	return r, "", ""
}

// agentModel is the route as agentloop's model: its protocol and base URL from
// `answers.providers`, its token the provider's key read again for every HTTP attempt
// (config.AnswerCredential.Read, so a rotated key or a minted token is the one each call
// carries), its effort the level the conversation last ran at, and its price `answers.prices`'.
func (r route) agentModel(level string) agentloop.Model {
	return agentloop.Model{
		Endpoint: agentloop.Endpoint{Kind: r.kind, BaseURL: r.endpoint.BaseURL, Token: r.endpoint.Credential.Read},
		ID:       r.model,
		Effort:   level,
		Price: agentloop.Price{
			Input: r.price.Input, Output: r.price.Output,
			CacheWrite: r.price.CacheWrite, CacheRead: r.price.CacheRead,
		},
	}
}

func answer(ctx context.Context, req Request) (Reply, error) {
	if req.paused() {
		return Reply{}, ErrPaused
	}
	r, fault, detail := routeOf(req)
	if fault != "" {
		return Reply{Fault: fault, Detail: detail}, nil
	}
	history, err := toAgentloop(req.Conversation.Messages, r)
	if err != nil {
		return Reply{Fault: UnknownFormat, Detail: err.Error()}, nil
	}
	question := userMessage(req)
	// The gh tool adds each call's output to what the agent has in front of it (inFront).
	front := &inFront{history: history, question: question}
	result, runErr := agentloop.Run(ctx, agentloop.Request{
		Model:   r.agentModel(req.Conversation.ThinkingLevel),
		System:  systemPrompt(req),
		History: history,
		Input:   []agentloop.Message{{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: question}}}},
		Tools:   toolSet(req, front.add),
		Bounds: agentloop.Bounds{
			Time: req.Bounds.Answer, Calls: req.Bounds.ModelCalls, OutputTokens: req.Bounds.OutputTokens,
		},
		FirstToolIndex: firstToolIndex(req.Earlier),
		Final:          finalSchema(req.MayAskNothing),
		// A reply the form's own rules refuse (inFront.read) is asked for once again, with
		// the rule's sentence (parseReply's, or errPlaceholder) as the message after it,
		// within the same bounds: the rules the strict schema cannot state are the ones a
		// model gets wrong, and one more call costs less than a question passed on as
		// malformed. A second refusal is finish's to make.
		Check: func(value json.RawMessage) error {
			_, err := front.read(value, req.MayAskNothing)
			return err
		},
		AskAgain: 1,
		// A reply the provider cuts at output_tokens is a model running on inside the form,
		// not one with more to say: the one such reply of 415 calls measured on 2026-11-19
		// was a whole answer and then newline escapes to the bound, where the longest reply
		// it did not cut used 567 tokens. agentloop makes that call once again without the
		// cut reply, within the same bounds, and marks the cut step Dropped, which exchange
		// appends nothing of. A second cut is the length fault.
		CallAgainWhenCut: 1,
		// The last call, which offers no tool, is told so (lastCall). agentloop puts the
		// message on that call alone and in no step, so exchange appends nothing of it.
		LastCall: lastCall,
		// A model call that ends while the project is paused is the run's last. agentloop
		// finishes a step once the gh calls it asked for have returned, their results among its
		// blocks, so this also ends a run the pause came during or before a gh call of, the
		// step's calls still to run refused (toolSet); and it keeps a reply asked for again, or
		// a cut call made again, from going on.
		OnStep: func(agentloop.Step) error {
			if req.paused() {
				return ErrPaused
			}
			return nil
		},
		Log: req.Log,
	})
	reply := Reply{Calls: len(result.Steps), Usage: usageOf(result.Usage, r.price), CostUSD: result.Usage.CostUSD}
	if runErr != nil && ctx.Err() != nil {
		return reply, fmt.Errorf("answer: %w", ctx.Err())
	}
	if errors.Is(runErr, ErrPaused) {
		return reply, ErrPaused
	}
	reply.Append = exchange(req, r, result, front)
	switch {
	case runErr != nil:
		reply.Fault, reply.Detail = ProviderError, runErr.Error()
	case result.Fault != "":
		reply.Fault, reply.Detail = faultOf(result.Fault), result.Detail
	case unmetered(result):
		// The run answered and nothing counted it, so its cost is unknown and the day's spend
		// would not move: the answer is dropped rather than given away (Unmetered).
		reply.Fault, reply.Detail = Unmetered, fmt.Sprintf("the provider reported no usage for the answer's %d model call(s)", len(result.Steps))
	default:
		finish(&reply, req, result, front)
	}
	return reply, nil
}

// inFront is what the agent has in front of it as it replies, whose words a reply may be without
// being a placeholder (placeholder): the conversation's history and the question the run is
// given, and the output of every gh call the run makes, which add takes as the gh tool runs it
// (toolSet). The SDK runs a tool call on a goroutine of its own and agentloop checks a reply on
// Run's, so the outputs are held under a lock.
type inFront struct {
	history  []agentloop.Message
	question string
	mu       sync.Mutex
	outputs  []string
}

// add takes one gh call's output, as the model reads it.
func (f *inFront) add(output string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs = append(f.outputs, output)
}

// read reads a reply by the form's own rules: parseReply's, and a text of any kind but none that
// is no placeholder of what the agent has in front of it.
func (f *inFront) read(value json.RawMessage, mayAskNothing bool) (form, error) {
	reply, err := parseReply(value, mayAskNothing)
	if err == nil && reply.kind != None && placeholder(reply.text, f.holds) {
		return form{}, errPlaceholder
	}
	return reply, err
}

// holds says whether word is a word of what the agent has in front of it: of each history
// block's text (prose, thinking, a reasoning summary or a tool result's output), a tool call's
// input and a tool result's parts; of the question; and of the gh outputs. A word is a run of
// letters and digits as long as it goes (notWord), compared in any case. A text holding a
// backslash is read once more with each JSON escape as a space (jsonEscape), which can only add
// words.
//
// The guard's own exchange is left out: a user message whose one block is a placeholder
// sentence (placeholderSentences), and the reply before it that the sentence refused. An earlier
// answer appends both to the conversation (exchange), and the refused reply holds its
// placeholder, so without the skip the same placeholder would pass on every later question.
func (f *inFront) holds(word string) bool {
	inText := func(text string) bool {
		for w := range strings.FieldsFuncSeq(text, notWord) {
			if strings.EqualFold(w, word) {
				return true
			}
		}
		return false
	}
	inString := func(text string) bool {
		return inText(text) || strings.Contains(text, `\`) && inText(jsonEscape.ReplaceAllLiteralString(text, " "))
	}
	wordBytes := []byte(word)
	inBytes := func(text []byte) bool {
		for w := range bytes.FieldsFuncSeq(text, notWord) {
			if bytes.EqualFold(w, wordBytes) {
				return true
			}
		}
		return false
	}
	inInput := func(input json.RawMessage) bool {
		return inBytes(input) || bytes.IndexByte(input, '\\') >= 0 && inBytes(jsonEscape.ReplaceAllLiteral(input, []byte(" ")))
	}
	for i, message := range f.history {
		if askedAgain(message) || message.Role == agentloop.Assistant && i+1 < len(f.history) && askedAgain(f.history[i+1]) {
			continue
		}
		for _, block := range message.Blocks {
			if inString(block.Text) || inInput(block.Input) {
				return true
			}
			for _, part := range block.Content {
				if inString(part.Text) {
					return true
				}
			}
		}
	}
	if inString(f.question) {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.outputs, inString)
}

// notWord says whether r ends a word: a word is a run of letters and digits.
func notWord(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }

// jsonEscape is one escape of a JSON string. Raw JSON (a tool call's input, gh's --json or gh
// api output) glues the letters after an escape to the escape's own: `\nBramble` and
// `\u003eBramble` hold no word Bramble until the escape is read as a space.
var jsonEscape = regexp.MustCompile(`\\(u[0-9a-fA-F]{4}|.)`)

// askedAgain says whether message is a placeholder sentence (placeholderSentences) as the loop
// asks again with it and an appender writes it: a user message of that one text.
func askedAgain(message agentloop.Message) bool {
	return message.Role == agentloop.User && len(message.Blocks) == 1 &&
		message.Blocks[0].Kind == agentloop.TextBlock && slices.Contains(placeholderSentences, message.Blocks[0].Text)
}

// unmetered says whether nothing counted the run's model calls (Unmetered). Two things say so,
// and a run has to meet only one: agentloop reports no counts at all (Usage.Metered false, a
// provider whose adapter leaves an absent usage object absent), or every call of the run reports
// zero input, output and cache tokens (a provider whose adapter answers an absent usage object
// with zeros, Anthropic's among them). A real call always reads its prompt, so a call counting
// nothing at all counted nothing: the gateway, or the provider, sent no usage.
func unmetered(result agentloop.Result) bool {
	if !result.Usage.Metered {
		return true
	}
	for _, step := range result.Steps {
		u := step.Usage
		if u.Input+u.Output+u.CacheWrite+u.CacheRead+u.Reasoning > 0 {
			return false
		}
	}
	return true
}

// firstToolIndex is how many gh calls an earlier run of the question already made, which
// agentloop numbers this run's calls on from, so a re-run's first call takes the seq after the
// recorded ones.
func firstToolIndex(earlier []store.AnswerToolCall) int {
	first := 0
	for _, call := range earlier {
		first = max(first, call.Seq)
	}
	return first
}

// exchange is the run as the turns to append under front's question: every call of the run in
// order but a cut one whose call agentloop made again, which no later call was sent and the
// conversation never holds, and one that returned nothing at all, which no harness writes as a
// turn. A run whose calls returned nothing has no turns.
func exchange(req Request, r route, result agentloop.Result, front *inFront) conversation.Append {
	a := conversation.Append{
		QuestionID: req.Question.ID, Question: front.question,
		Provider: string(r.provider), API: string(r.kind), Model: r.model,
	}
	for _, step := range result.Steps {
		if step.Dropped {
			continue
		}
		if turn := turnOf(step, r.price); len(turn.Content) > 0 || len(turn.Native) > 0 {
			a.Turns = append(a.Turns, turn)
		}
	}
	return a
}

// finish reads the run's last reply into reply by the form's own rules (front's read): the
// agent's reply in the form, or no answer when the rules refuse it, which they do once agentloop
// has asked again where its bounds let it, or when the reply's exchange is none an appender could
// write as an answer. A reply with no answer keeps its exchange, which the caller ends with its
// own reply.
func finish(reply *Reply, req Request, result agentloop.Result, front *inFront) {
	form, err := front.read(result.Value, req.MayAskNothing)
	if err != nil {
		reply.Fault, reply.Detail = Malformed, fmt.Sprintf("%v: %.500q", err, result.Text)
		return
	}
	if err := reply.Append.Validate(); err != nil {
		reply.Fault, reply.Detail = Malformed, err.Error()
		return
	}
	reply.Kind, reply.Text, reply.Needs = form.kind, form.text, form.needs
}

// turnOf is one model call as a turn of the append: its thinking (with its signature), redacted
// thinking, text and tool calls in order, each call's result, the provider's own output items for
// a turn that has them (an OpenAI Responses turn, which the appender writes from them), the
// sentence the call's reply was asked again with when the form refused it, and what the call
// used and cost.
func turnOf(step agentloop.Step, price config.AnswerPrice) conversation.Turn {
	turn := conversation.Turn{Native: step.Native, AskedAgain: step.AskedAgain, Usage: usageOf(step.Usage, price)}
	for _, block := range step.Blocks {
		switch block.Kind {
		case agentloop.ThinkingBlock:
			if block.Redacted != "" {
				turn.Content = append(turn.Content, conversation.Block{Type: conversation.RedactedThinking, Data: block.Redacted})
				continue
			}
			turn.Content = append(turn.Content, conversation.Block{Type: conversation.Thinking, Text: block.Text, Signature: block.Signature})
		case agentloop.ReasoningBlock:
			// An OpenAI reasoning item's id and encrypted content are in the turn's native
			// items, which the appender writes the turn from; the block keeps its summary.
			turn.Content = append(turn.Content, conversation.Block{Type: conversation.Thinking, Text: block.Text})
		case agentloop.TextBlock:
			if block.Text != "" {
				turn.Content = append(turn.Content, conversation.Block{Type: conversation.Text, Text: block.Text})
			}
		case agentloop.ToolCallBlock:
			turn.Content = append(turn.Content, conversation.Block{
				Type: conversation.ToolCall, ToolCallID: block.ToolCallID, ToolName: block.Name, Arguments: block.Input,
			})
		case agentloop.ToolResultBlock:
			turn.Results = append(turn.Results, conversation.Block{
				Type: conversation.ToolResult, ToolCallID: block.ToolCallID, ToolName: block.Name,
				Content: []conversation.Block{{Type: conversation.Text, Text: resultText(block)}}, IsError: block.IsError,
			})
		}
	}
	return turn
}

// resultText is a tool result's output as one text, which is how the loop's tools answer.
func resultText(block agentloop.Block) string {
	if len(block.Content) == 0 {
		return block.Text
	}
	texts := make([]string, 0, len(block.Content))
	for _, part := range block.Content {
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n")
}

// usageOf is agentloop's usage as the conversation's: its token counts, and its cost split by
// what each count cost at the model's price (USD per million tokens), which is the breakdown the
// appender writes into the agent's own session beside the turn.
func usageOf(usage agentloop.Usage, price config.AnswerPrice) conversation.Usage {
	const perToken = 1e-6
	return conversation.Usage{
		InputTokens: int64(usage.Input), OutputTokens: int64(usage.Output),
		CacheReadTokens: int64(usage.CacheRead), CacheWriteTokens: int64(usage.CacheWrite),
		ReasoningTokens: int64(usage.Reasoning),
		InputCost:       float64(usage.Input) * price.Input * perToken,
		OutputCost:      float64(usage.Output) * price.Output * perToken,
		CacheReadCost:   float64(usage.CacheRead) * price.CacheRead * perToken,
		CacheWriteCost:  float64(usage.CacheWrite) * price.CacheWrite * perToken,
	}
}
