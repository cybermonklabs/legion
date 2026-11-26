package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"

	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	aisdk "github.com/grafana/ai-sdk"
	"github.com/grafana/ai-sdk/output"
	"github.com/grafana/ai-sdk/provider"
	"github.com/grafana/ai-sdk/providers/anthropic"
	"github.com/grafana/ai-sdk/providers/azure"
	"github.com/grafana/ai-sdk/providers/azure/foundry"
	"github.com/grafana/ai-sdk/providers/openai"
	openaicompatible "github.com/grafana/ai-sdk/providers/openai-compatible"
	"github.com/grafana/ai-sdk/schema"
	openaisdk "github.com/openai/openai-go/v3"
	openaiopt "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

// placeholderKey is the key each vendor client is built with; the run's transport puts the
// token it reads for the attempt in its place on every request.
const placeholderKey = "read-per-call"

// Run makes one run of the loop. Every outcome of the request is a Result: the model's reply,
// or a Fault saying why the run ended short of one, each with every step that finished and what
// those steps spent. The error is for a request the loop cannot run at all, for the caller's
// context ending, and for a callback that returned an error; the Result then still carries what
// the calls made so far produced and cost. A reply Request.Check refuses is asked for again
// inside the same run and the same bounds, at most Request.AskAgain times (doc.go, "Asking
// again"); a call whose reply the provider cut for length is made again, without that reply, at
// most Request.CallAgainWhenCut times (doc.go, "A reply cut for length"); and a reply out of form
// on a call that carried no schema is dropped, and the run's next call is its last (doc.go, "The
// structured reply").
func Run(ctx context.Context, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	kind := req.Model.Endpoint.Kind
	history, err := toProvider(req.History, kind)
	if err != nil {
		return Result{}, fmt.Errorf("agentloop: the request's history: %w", err)
	}
	input, err := toProvider(req.Input, kind)
	if err != nil {
		return Result{}, fmt.Errorf("agentloop: the request's input: %w", err)
	}
	messages := append(history, input...)

	transport := newModelTransport(req.Model.Endpoint, req.Final, req.Log)
	model, options, err := languageModel(req, transport.client())
	if err != nil {
		return Result{}, err
	}
	if !req.NoCache {
		switch {
		case anthropicKind(kind):
			markAnthropicHistory(messages, len(history))
		case kind == OpenAIResponses:
			markOpenAIHistory(messages, len(history))
		}
	}

	r := &run{
		req: req, kind: kind, history: len(history), bound: req.Bounds.Calls,
		indexes: make(map[string]int, len(req.Tools)), next: req.FirstToolIndex + 1,
	}
	opts := []aisdk.StreamOption{
		aisdk.WithMaxOutputTokens(req.Bounds.OutputTokens),
		aisdk.WithProviderOptions(options),
		aisdk.WithMaxRetries(0),
		aisdk.WithPrepareStep(r.prepareStep),
		aisdk.OnStepFinish(r.stepFinished),
	}
	if readsRawChunks(kind) {
		opts = append(opts, aisdk.WithIncludeRawChunks())
	}
	if req.System != "" {
		opts = append(opts, aisdk.WithSystem(req.System))
	}
	if req.Final != nil {
		final, err := finalOutput(req.Final)
		if err != nil {
			return Result{}, err
		}
		opts = append(opts, aisdk.WithOutput(final))
	}
	set, err := r.toolSet()
	if err != nil {
		return Result{}, err
	}
	if len(set) > 0 {
		opts = append(opts, aisdk.WithTools(set))
	}

	runCtx, cancel := context.WithTimeout(ctx, req.Bounds.Time)
	defer cancel()
	r.cancel = cancel
	for {
		stream := r.ask(runCtx, model, opts, messages)
		result := r.result()
		// Every path below returns the steps the run finished, a fault's among them, so each
		// is given its call's own output items before any path is taken.
		r.native(&result)
		switch failure := r.failure(); {
		case failure != nil:
			return result, failure
		case ctx.Err() != nil:
			return result, fmt.Errorf("agentloop: %w", ctx.Err())
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			result.Fault, result.Detail = FaultTime, fmt.Sprintf("the run went past %s", req.Bounds.Time)
		case stream.Err() != nil:
			result.Fault, result.Detail = FaultProviderError, stream.Err().Error()
		case len(result.Steps) == r.before:
			result.Fault, result.Detail = FaultProviderError, "the model made no reply"
		default:
			r.finish(&result, stream)
		}
		again, outOfForm := r.callAgain(runCtx, result), r.outOfForm(runCtx, result)
		if again || outOfForm {
			// The run goes on from the messages the dropped call was sent: the ones this ask
			// was given and every message its calls before the dropped one produced. The
			// SDK's response messages end with the dropped call's own, which the stream's
			// last response holds, so none of its reply goes back to the model.
			produced := stream.ResponseMessages()
			messages = slices.Concat(messages, produced[:len(produced)-len(stream.Response().Messages)])
			r.dropped()
			if outOfForm {
				// The next call is the run's last, so it offers no tool and carries the
				// schema and Request.LastCall.
				r.bound = r.before + 1
			} else {
				r.cut++
			}
			continue
		}
		refusal, again := r.refusal(runCtx, result)
		if !again {
			return result, nil
		}
		// The run goes on from its own messages: the ones this ask was given, every
		// message its calls produced as the SDK carries a step's turn to the next call,
		// the refused reply among them, and the check's sentence answering that reply.
		messages = slices.Concat(messages, stream.ResponseMessages(), []provider.Message{provider.UserText(refusal)})
		r.askedAgain(refusal)
	}
}

// ask makes the model calls of one ask, the run's first, one after Request.Check refused a
// reply, or one after a dropped call: a stream of the messages the run holds so far, stopped at
// the model calls the run has left, read to its end.
func (r *run) ask(ctx context.Context, model provider.LanguageModel, opts []aisdk.StreamOption, messages []provider.Message) *aisdk.StreamTextResult {
	stream := aisdk.StreamText(ctx, model, slices.Concat([]aisdk.StreamOption{
		aisdk.WithModelMessages(messages...),
		aisdk.WithStopWhen(aisdk.StepCountIs(r.bound - r.before)),
	}, opts)...)
	r.drain(stream)
	return stream
}

// refusal is the sentence the run asks the model again with, and whether it asks again at all:
// it does where the last reply answered with a value, Request.Check refuses that value, and the
// run has an ask, a model call and time left; the call after a dropped reply out of form is the
// run's last, so it leaves none. A run that does not ask again returns its last reply as it
// stands.
func (r *run) refusal(ctx context.Context, result Result) (string, bool) {
	if r.req.Check == nil || result.Fault != "" || result.Value == nil || r.asked >= r.req.AskAgain ||
		len(result.Steps) >= r.bound || ctx.Err() != nil {
		return "", false
	}
	if err := r.req.Check(result.Value); err != nil {
		return err.Error(), true
	}
	return "", false
}

// callAgain is whether the run makes its last model call again: it does where the provider cut
// that call's reply for length, the request makes a cut call again and has a call again left,
// and the run has a model call and time left. A run that does not call again ends on the cut
// reply's FaultLength.
func (r *run) callAgain(ctx context.Context, result Result) bool {
	return result.Fault == FaultLength && r.cut < r.req.CallAgainWhenCut &&
		len(result.Steps) < r.bound && ctx.Err() == nil
}

// outOfForm is whether the run drops its last step, a reply that is no Final object on a call
// that carried no schema, and makes its next call its last (doc.go, "The structured reply"): the
// reply ended the run on FaultMalformed, on the Messages API, the run offers tools, and the call
// was not its last, so it offered them and the transport sent it with no schema (withoutFormat);
// and the run has time left. A call that is not the run's last leaves the run a model call; the
// last, whose tool choice prepareStep sets to none, carries the schema, so a run drops such a
// reply once at most, and a reply out of form on a call that carried the schema ends the run on
// its FaultMalformed.
func (r *run) outOfForm(ctx context.Context, result Result) bool {
	return result.Fault == FaultMalformed && anthropicKind(r.kind) && len(r.req.Tools) > 0 &&
		len(result.Steps) < r.bound && ctx.Err() == nil
}

// askedAgain records the sentence the run sends after its last step's reply on that step, and
// starts the next ask's count of calls after every call the run has made.
func (r *run) askedAgain(refusal string) {
	r.mu.Lock()
	r.steps[len(r.steps)-1].AskedAgain = refusal
	r.before = len(r.steps)
	r.mu.Unlock()
	r.asked++
}

// dropped marks the run's last step as a call none of whose reply goes back to the model, a cut
// call the run makes again or a reply out of form, and starts the next ask's count of calls
// after every call the run has made, the dropped one included.
func (r *run) dropped() {
	r.mu.Lock()
	r.steps[len(r.steps)-1].Dropped = true
	r.before = len(r.steps)
	r.mu.Unlock()
}

// validate is whether the request is one the loop can run at all: it has its bounds, an
// endpoint that reads a token, a model, a Final object for any Check to read, and tools that are
// each named, run something and are named once.
func (req Request) validate() error {
	if req.Bounds.Time <= 0 || req.Bounds.Calls <= 0 || req.Bounds.OutputTokens <= 0 {
		return errors.New("agentloop: the request has no time, call or output token bound")
	}
	if err := req.Model.validate(); err != nil {
		return err
	}
	if req.Check != nil && req.Final == nil {
		return errors.New("agentloop: the request checks a final object it does not ask for")
	}
	named := make(map[string]bool, len(req.Tools))
	for _, t := range req.Tools {
		switch {
		case t.Name == "":
			return errors.New("agentloop: a tool has no name")
		case t.Run == nil:
			return fmt.Errorf("agentloop: tool %s runs nothing", t.Name)
		case named[t.Name]:
			return fmt.Errorf("agentloop: tool %s is registered twice", t.Name)
		}
		named[t.Name] = true
	}
	return nil
}

// languageModel is the model the run calls and the provider options that call carries, for the
// protocol the run's endpoint speaks: the endpoint's base URL, the run's own HTTP client, which
// puts the token on every request, and the vendor client's own retries off, so the only retry is
// the transport's. A protocol Run does not serve is a request it cannot make.
func languageModel(req Request, client *http.Client) (provider.LanguageModel, provider.ProviderOption, error) {
	endpoint := req.Model.Endpoint
	switch endpoint.Kind {
	case AnthropicMessages:
		model := anthropic.New(placeholderKey, req.Model.ID, anthropic.WithRequestOptions(
			anthropicopt.WithHTTPClient(client), anthropicopt.WithMaxRetries(0), anthropicopt.WithBaseURL(endpoint.BaseURL)))
		return model, anthropicOptions(req.Model.Effort), nil
	case OpenAIResponses:
		// openai-go's own client reads a base URL, a key, an organization, a project and
		// whatever headers OPENAI_CUSTOM_HEADERS names from the environment, and puts
		// them on every call it makes. Its Responses service reads none of them: it is
		// built from these options alone, so nothing the run sends comes from the
		// environment and no header has to be taken off again.
		service := responses.NewResponseService(
			openaiopt.WithHTTPClient(client), openaiopt.WithMaxRetries(0),
			openaiopt.WithBaseURL(endpoint.BaseURL), openaiopt.WithAPIKey(placeholderKey))
		model := openai.NewResponsesWithClient(openaisdk.Client{Responses: service}, req.Model.ID)
		return model, openaiOptions(req.Model.Effort), nil
	case OpenAIChat:
		// The run's own transport puts the token on every request and adds the
		// endpoint's query parameters, so the client is given neither headers nor query
		// parameters of its own: on this protocol the transport is the only thing that
		// authorizes a call. (On azure-anthropic the provider's own credential
		// middleware does it instead, and the transport reads no token.)
		// WithIncludeUsage asks the stream for its token counts, and
		// WithStructuredOutputs lets a Final reply's schema ride as
		// response_format.json_schema rather than being downgraded to a bare json_object.
		model := openaicompatible.New(req.Model.ID,
			openaicompatible.WithBaseURL(endpoint.BaseURL), openaicompatible.WithAPIKey(placeholderKey),
			openaicompatible.WithHTTPClient(client), openaicompatible.WithIncludeUsage(true),
			openaicompatible.WithStructuredOutputs(true))
		return model, chatOptions(req.Model.Effort), nil
	case AzureAnthropic:
		// The deployment name is given only where it differs from the model id, and the
		// foundry config owns the base URL, which NewAnthropic applies itself. The run's
		// client takes the place of the one that config builds, and refuses redirects as
		// that one does.
		deployment := endpoint.Deployment
		if deployment == "" {
			deployment = req.Model.ID
		}
		config := foundry.Config{BaseURL: endpoint.BaseURL, Credential: func(ctx context.Context) (foundry.Credential, error) {
			token, err := endpoint.Token(ctx)
			if err != nil {
				return foundry.Credential{}, err
			}
			return foundry.Credential{AuthToken: token}, nil
		}}
		model, err := azure.NewAnthropic(config, req.Model.ID, deployment, anthropic.WithRequestOptions(
			anthropicopt.WithHTTPClient(client), anthropicopt.WithMaxRetries(0)))
		if err != nil {
			return nil, nil, fmt.Errorf("agentloop: the %s endpoint: %w", endpoint.Kind, err)
		}
		return model, anthropicOptions(req.Model.Effort), nil
	default:
		return nil, nil, fmt.Errorf("agentloop: the loop makes no run on the %s protocol", endpoint.Kind)
	}
}

// finalOutput is Request.Final as the SDK's output: the schema is held to the strict
// structured-output subset both providers enforce (strict.go), and the last reply is validated
// against it and read back as the object itself. A schema outside that subset, or one the SDK
// refuses, is a request the loop cannot run: no model call is made. The check runs here, before
// the run's first call, because the SDK sends a schema on as it is written and a provider that
// cannot compile a constraint drops it silently.
func finalOutput(final json.RawMessage) (aisdk.Output, error) {
	if err := strictFinal(final); err != nil {
		return nil, fmt.Errorf("agentloop: the request's final schema: %w", err)
	}
	s, err := schema.SchemaFromJSON(final)
	if err != nil {
		return nil, fmt.Errorf("agentloop: the request's final schema: %w", err)
	}
	out, err := output.Object[json.RawMessage](s)
	if err != nil {
		return nil, fmt.Errorf("agentloop: the request's final schema: %w", err)
	}
	return out, nil
}

// run is one Run's state across its model calls.
type run struct {
	req  Request
	kind Kind
	// calls are the raw provider chunks of each model call the run started, in order, and
	// hold as much of them as the protocol reads back. Only drain writes them, part by
	// part as the stream carries them, and only finish reads them, once drain has
	// returned: the two run one after the other in the calling goroutine, so the mutex
	// below does not cover them. A step's chunks go on the stream before that step's
	// OnStepFinish is called, but drain need not have read them by then, which is why
	// stepFinished reads none of this.
	calls []rawCall
	// cancel ends the run's own context, which is how a callback's error stops the run.
	cancel context.CancelFunc

	// history is how many messages the request's history converted to before the first
	// call; afterHistory finds where they end in a step's messages, and the marks a step
	// moves are the ones after them.
	history int
	// before is how many model calls the run made before its current ask, which prepareStep
	// adds to a step's own number, counted from the ask's first, to find the run's last
	// call; asked is how many times the run has asked again, and cut how many times it has
	// made a cut call again. bound is how many model calls the run may make: Bounds.Calls,
	// until a dropped reply out of form makes the next call the last (outOfForm). All four
	// change only between two asks, before the stream that reads them starts.
	before, asked, cut, bound int

	mu sync.Mutex
	// steps are the model calls that finished, in order; text and reason are the last of
	// them's reply and finish reason; failed is the first error a callback returned.
	steps  []Step
	text   string
	reason provider.FinishReason
	failed error

	// tool holds one tool call at a time, so a step's calls run one after another.
	tool sync.Mutex

	// numbering guards the tool call indexes: indexes is the index each call's id took
	// when the model asked for it, and next is the index the next call takes.
	numbering sync.Mutex
	indexes   map[string]int
	next      int
}

// readsRawChunks is whether a protocol's reply is read back out of the provider's own chunks,
// which the run then asks the stream for: openai-responses for each call's output items,
// openai-chat for a refusal. No other protocol says anything there that the SDK's own parts do
// not carry.
func readsRawChunks(kind Kind) bool {
	return kind == OpenAIResponses || kind == OpenAIChat
}

// rawCall is what one model call's raw provider chunks held of what its protocol reads back:
// the output items an openai-responses stream reported done, each byte for byte as the event
// carried it, and the refusal an openai-chat stream carried, pieced together as the model wrote
// it.
type rawCall struct {
	items   []json.RawMessage
	refusal []byte
}

// drain reads both of the run's streams to their end, which the SDK needs for the run to finish
// at all: the Final object's snapshots in a goroutine, the stream's own parts here. Each is read
// to the end whether or not the caller takes it, and after a callback has failed. The parts a
// protocol reads its reply back out of are kept as they go by, because the stream is the only
// place they are said.
func (r *run) drain(stream *aisdk.StreamTextResult) {
	partials := make(chan struct{})
	go func() {
		defer close(partials)
		for snapshot := range stream.PartialOutputStream() {
			if r.req.OnPartial == nil || r.failure() != nil {
				continue
			}
			if err := r.req.OnPartial(snapshot); err != nil {
				r.fail(err)
			}
		}
	}()
	for part := range stream.FullStream() {
		switch part := part.(type) {
		case aisdk.StreamStartStep:
			r.calls = append(r.calls, rawCall{})
		case aisdk.StreamRaw:
			r.readRaw(part.RawValue)
		case aisdk.StreamTextDelta:
			if r.req.OnText == nil || r.failure() != nil {
				continue
			}
			if err := r.req.OnText(part.Text); err != nil {
				r.fail(err)
			}
		}
	}
	<-partials
}

// readRaw reads one raw provider chunk of the model call the stream is in into that call's own
// chunks: an openai-responses event that reported an output item done carries that item, and an
// openai-chat chunk carries a piece of a refusal, in the choice's streamed delta or on the whole
// message a gateway answers a stream request with. A chunk the provider could not read back as
// JSON arrives empty, and holds nothing either way.
func (r *run) readRaw(raw json.RawMessage) {
	if len(r.calls) == 0 || len(raw) == 0 {
		return
	}
	call := &r.calls[len(r.calls)-1]
	switch r.kind {
	case OpenAIResponses:
		var event struct {
			Type string          `json:"type"`
			Item json.RawMessage `json:"item"`
		}
		if json.Unmarshal(raw, &event) == nil && event.Type == "response.output_item.done" {
			call.items = append(call.items, event.Item)
		}
	case OpenAIChat:
		var chunk struct {
			Choices []struct {
				Delta struct {
					Refusal string `json:"refusal"`
				} `json:"delta"`
				Message struct {
					Refusal string `json:"refusal"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &chunk) != nil {
			return
		}
		for _, choice := range chunk.Choices {
			call.refusal = append(append(call.refusal, choice.Delta.Refusal...), choice.Message.Refusal...)
		}
	}
}

// fail keeps the first error a callback returned and ends the run; failure is that error.
func (r *run) fail(err error) {
	r.mu.Lock()
	if r.failed == nil {
		r.failed = err
	}
	r.mu.Unlock()
	r.cancel()
}

func (r *run) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

// prepareStep offers no tool on the last call of the whole run, the last Bounds.Calls allows or
// the one after a dropped reply out of form, so that call must answer, and on the kinds Anthropic
// serves, where the run places cache marks, moves the newest message's mark: the history keeps
// the marks Run put in it, because the step takes a mark off no message of the history, the
// newest message gets one, and the one an earlier step put on its newest message is taken off
// (the SDK carries a step's message overrides into later steps; Anthropic allows four marks,
// which is this one and the three markAnthropicHistory places). Where the request's Input is
// empty, the first call's newest message is the history's own last message, which keeps the
// history's mark for the whole run. An ask after Request.Check refused a reply starts from the
// run's own messages, which carry the history's marks and no other, so its newest message, the
// check's sentence, takes the fourth. A call after a dropped one, a cut call made again or the
// call after a reply out of form, starts from the messages the dropped call was sent, so its
// newest message is the dropped call's and takes the mark the dropped call put there.
//
// The last call also carries Request.LastCall, after every other message and after the mark:
// that call is the run's last, so nothing re-sends the sentence, a mark on it would pay for a
// cache write nothing reads, and without one the call's marks and its cached prefix are the ones
// it would have without the sentence. No later step takes the override on, and no Step holds it:
// the SDK builds a step's response messages from the step's own output.
func (r *run) prepareStep(state aisdk.PrepareStepState) (*aisdk.PrepareStepResult, error) {
	result := &aisdk.PrepareStepResult{}
	last := r.before+state.StepNumber == r.bound-1
	if last {
		result.ToolChoice = &provider.ToolChoice{Type: provider.ToolChoiceNone}
	}
	messages := state.Messages
	if anthropicKind(r.kind) && !r.req.NoCache && len(messages) > 0 {
		messages = slices.Clone(messages)
		for i := afterHistory(messages, r.history); i < len(messages); i++ {
			if _, marked := messages[i].ProviderOptions["anthropic"].(anthropic.AnthropicCacheControl); marked {
				options := maps.Clone(messages[i].ProviderOptions)
				delete(options, "anthropic")
				messages[i].ProviderOptions = options
			}
		}
		newest := len(messages) - 1
		messages[newest].ProviderOptions = withOption(messages[newest].ProviderOptions, anthropic.CacheControl("ephemeral"))
		result.Messages = messages
	}
	if last && r.req.LastCall != "" {
		result.Messages = slices.Concat(messages, []provider.Message{provider.UserText(r.req.LastCall)})
	}
	return result, nil
}

// afterHistory is the index, in a step's messages, of the first message after the run's own
// history, which is how far into them a step's marks reach. A step is given the system prompt
// the SDK puts before the run's messages, and the history the request handed over sits after
// it; no message of the request's own is a system message.
func afterHistory(messages []provider.Message, history int) int {
	after := history
	for _, m := range messages {
		if m.Role != provider.RoleSystem {
			break
		}
		after++
	}
	return after
}

// stepFinished keeps one model call that finished and gives it to the caller's OnStep. The
// call's own output items are read once the run is over, so the step the callback is given
// carries no Native.
func (r *run) stepFinished(state aisdk.OnStepFinishState) {
	step := Step{
		Blocks: blocksOf(state.Content, r.kind),
		Usage:  priced(state.Usage, r.req.Model.Price),
		Finish: string(state.FinishReason.Unified),
	}
	r.mu.Lock()
	r.steps = append(r.steps, step)
	r.text, r.reason = state.Text, state.FinishReason
	r.mu.Unlock()
	if r.req.OnStep == nil || r.failure() != nil {
		return
	}
	if err := r.req.OnStep(step); err != nil {
		r.fail(err)
	}
}

// result is the run's steps, their usage summed and the last reply's text, as they stand.
func (r *run) result() Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := Result{Text: r.text, Steps: slices.Clone(r.steps)}
	for _, step := range r.steps {
		result.Usage = SumUsage(result.Usage, step.Usage)
	}
	return result
}

// finish reads the last model call into result: the refusal the call may have ended on, the
// fault it ended on, and the object the reply was required to be when the request asked for one.
// It runs once drain has returned, so every chunk of every call is in by now; the output items
// those chunks carry are native's, which Run gives every result.
func (r *run) finish(result *Result, stream *aisdk.StreamTextResult) {
	r.mu.Lock()
	reason := r.reason
	r.mu.Unlock()
	if readsRawChunks(r.kind) && len(r.calls) < len(result.Steps) {
		// A step whose chunks never came is a step that cannot be given its own, which
		// is an error rather than a guess.
		result.Fault, result.Detail = FaultProviderError,
			fmt.Sprintf("%d model calls finished but %d sent their chunks", len(result.Steps), len(r.calls))
		return
	}
	switch r.kind {
	case OpenAIResponses:
		if refused(r.calls[len(result.Steps)-1].items) {
			result.Fault, result.Detail = FaultRefusal, "the last reply is a refusal"
			return
		}
	case OpenAIChat:
		// Chat Completions says a refusal in the reply's chunks alone: it stops for
		// "stop" with no content, which is otherwise an empty answer or, where the
		// request asked for a Final object, a malformed one.
		if refusal := r.calls[len(result.Steps)-1].refusal; len(refusal) > 0 {
			result.Fault, result.Detail = FaultRefusal, fmt.Sprintf("the last reply is a refusal: %.500q", refusal)
			return
		}
	}
	switch reason.Unified {
	case provider.FinishReasonLength:
		// The provider's own reason is in the detail because more than one maps to
		// length: Anthropic ends a reply this way both for max_tokens, the run's own
		// bound, and for a prompt past the model's context window.
		result.Fault, result.Detail = FaultLength, fmt.Sprintf("the provider cut the last reply off for %s, with the run's output token bound at %d",
			reason.Raw, r.req.Bounds.OutputTokens)
		return
	case provider.FinishReasonContentFilter:
		result.Fault, result.Detail = FaultRefusal, fmt.Sprintf("the provider stopped the last reply: %s", reason.Raw)
		return
	case provider.FinishReasonError:
		result.Fault, result.Detail = FaultProviderError, fmt.Sprintf("the last reply ended in an error: %s", reason.Raw)
		return
	case provider.FinishReasonToolCalls:
		result.Fault, result.Detail = FaultCalls, fmt.Sprintf("the last of %d model calls still called a tool", r.bound)
		return
	}
	if r.req.Final == nil {
		return
	}
	if err := stream.OutputError(); err != nil {
		result.Fault, result.Detail = FaultMalformed, fmt.Sprintf("%v: %.500q", err, result.Text)
		return
	}
	value, ok := stream.OutputValue().(json.RawMessage)
	if !ok {
		result.Fault, result.Detail = FaultMalformed, fmt.Sprintf("the last reply is no object of the form: %.500q", result.Text)
		return
	}
	result.Value = value
}

// native gives each step of result, on a kind whose steps carry their call's own output items
// (OpenAI Responses), those items, which drain kept from the call's raw chunks. A step is in the
// result only once its call finished, so the items of that call are whole; r.calls may hold one
// call more than the steps, the one in progress when the run ended, which no step names, and a
// step whose chunks never came gets none (finish faults such a run).
func (r *run) native(result *Result) {
	if r.kind != OpenAIResponses {
		return
	}
	for i := range result.Steps {
		if i < len(r.calls) {
			result.Steps[i].Native = r.calls[i].items
		}
	}
}

// refused is whether a call's output items hold a refusal.
func refused(items []json.RawMessage) bool {
	for _, raw := range items {
		var item struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "refusal" {
				return true
			}
		}
	}
	return false
}

// blocksOf is one model call's content as this package's blocks, in the order the model
// produced it: its thinking or its reasoning, its text with the phase it was produced in, its
// tool calls and their results.
func blocksOf(content []aisdk.ContentPart, kind Kind) []Block {
	var blocks []Block
	for _, part := range content {
		switch part := part.(type) {
		case aisdk.ReasoningContent:
			blocks = append(blocks, reasoningBlock(part, kind))
		case aisdk.TextContent:
			if part.Text != "" {
				blocks = append(blocks, Block{Kind: TextBlock, Text: part.Text, Phase: textPhase(part, kind)})
			}
		case aisdk.ToolCallContent:
			blocks = append(blocks, Block{Kind: ToolCallBlock, ToolCallID: part.ToolCallID, Name: part.ToolName, Input: part.Input})
		case aisdk.ToolResultContent:
			blocks = append(blocks, Block{
				Kind: ToolResultBlock, ToolCallID: part.ToolCallID, Name: part.ToolName,
				Content: []Block{{Kind: TextBlock, Text: toolText(part.Output)}},
			})
		case aisdk.ToolErrorContent:
			blocks = append(blocks, Block{
				Kind: ToolResultBlock, ToolCallID: part.ToolCallID, Name: part.ToolName,
				Content: []Block{{Kind: TextBlock, Text: fmt.Sprint(part.Error)}}, IsError: true,
			})
		}
	}
	return blocks
}

// toolText is a tool result's output as the model is told it: a tool answers with a JSON string
// of its output, which is what the SDK sends on as the result's text, and anything else is read
// as the JSON it is.
func toolText(output json.RawMessage) string {
	var text string
	if json.Unmarshal(output, &text) == nil {
		return text
	}
	return string(output)
}

// openaiMeta is the part options the OpenAI provider put in a part's own metadata, and the zero
// options where it put none. The SDK's own type names every field, so a key is never spelled
// out here.
func openaiMeta(metadata provider.ProviderMetadata) openai.OpenAIPartOptions {
	var meta openai.OpenAIPartOptions
	if raw, ok := metadata["openai"]; ok {
		_ = json.Unmarshal(raw, &meta)
	}
	return meta
}

// textPhase is the phase an openai-responses text part was produced in, which the provider puts
// in its own metadata and Block.Phase keeps so the part goes back as it came. No other protocol
// gives its text one.
func textPhase(part aisdk.TextContent, kind Kind) string {
	if kind != OpenAIResponses {
		return ""
	}
	return openaiMeta(part.ProviderMetadata).Phase
}

// reasoningBlock is one reasoning part as the protocol's own block: an OpenAI reasoning item
// with the id and the encrypted content its provider metadata carries, or Anthropic thinking
// with its signature, or redacted thinking's payload. Step.Native stays the authoritative copy.
func reasoningBlock(part aisdk.ReasoningContent, kind Kind) Block {
	if kind == OpenAIResponses {
		meta := openaiMeta(part.ProviderMetadata)
		block := Block{Kind: ReasoningBlock, Text: part.Text, ItemID: meta.ItemID}
		if meta.ReasoningEncryptedContent != nil {
			block.Encrypted = *meta.ReasoningEncryptedContent
		}
		return block
	}
	var meta struct {
		Signature    string  `json:"signature"`
		RedactedData *string `json:"redactedData"`
	}
	if raw, ok := part.ProviderMetadata["anthropic"]; ok {
		_ = json.Unmarshal(raw, &meta)
	}
	if meta.RedactedData != nil {
		return Block{Kind: ThinkingBlock, Redacted: *meta.RedactedData}
	}
	return Block{Kind: ThinkingBlock, Text: part.Text, Signature: meta.Signature}
}

// priced is one model call's usage and its cost at price, USD per million tokens: input not
// read from the cache, output with its reasoning counted in it, cache writes and cache reads.
// A call the provider reported no count at all for is not metered.
func priced(usage provider.Usage, price Price) Usage {
	value := func(n *int) int {
		if n == nil {
			return 0
		}
		return *n
	}
	read, write := value(usage.InputTokens.CacheRead), value(usage.InputTokens.CacheWrite)
	input := value(usage.InputTokens.NoCache)
	if usage.InputTokens.NoCache == nil {
		input = max(value(usage.InputTokens.Total)-read-write, 0)
	}
	output := value(usage.OutputTokens.Total)
	counted := []*int{
		usage.InputTokens.Total, usage.InputTokens.NoCache, usage.InputTokens.CacheRead, usage.InputTokens.CacheWrite,
		usage.OutputTokens.Total, usage.OutputTokens.Text, usage.OutputTokens.Reasoning,
	}
	counts := Usage{
		Input: input, Output: output, CacheWrite: write, CacheRead: read,
		Reasoning: value(usage.OutputTokens.Reasoning),
		Metered:   slices.ContainsFunc(counted, func(n *int) bool { return n != nil }),
	}
	counts.CostUSD = price.cost(counts)
	return counts
}

// SumUsage is two usages added, their costs included; the sum is metered where either side is.
func SumUsage(a, b Usage) Usage {
	return Usage{
		Input: a.Input + b.Input, Output: a.Output + b.Output,
		CacheWrite: a.CacheWrite + b.CacheWrite, CacheRead: a.CacheRead + b.CacheRead,
		Reasoning: a.Reasoning + b.Reasoning,
		CostUSD:   a.CostUSD + b.CostUSD,
		Metered:   a.Metered || b.Metered,
	}
}

// anthropicOptions is Anthropic's adaptive thinking with summarized display, at the model's
// effort, and thinking off where the run asks for none. A Final reply's schema rides in
// output_config.format rather than in a forced json tool, which would override the tool choice
// the last call is made with.
func anthropicOptions(effort string) anthropic.AnthropicOptions {
	options := anthropic.AnthropicOptions{StructuredOutputMode: anthropic.StructuredOutputFormat}
	if effort == "off" {
		options.Thinking = &anthropic.ThinkingConfig{Type: anthropic.ThinkingDisabled}
		return options
	}
	if effort == "minimal" {
		effort = "low"
	}
	options.Effort = effort
	options.Thinking = &anthropic.ThinkingConfig{Type: anthropic.ThinkingAdaptive, Display: anthropic.ThinkingDisplaySummarized}
	return options
}

// openaiEffort is the loop's own effort words in OpenAI's: a model asked for no reasoning at all
// is configured "off", which both of OpenAI's protocols name "none". Every other word is the
// provider's own and rides as it is.
func openaiEffort(effort string) string {
	if effort == "off" {
		return "none"
	}
	return effort
}

// openaiOptions is a stateless Responses call (store false, the reasoning's encrypted content
// returned so the next call can carry it) at the model's reasoning effort.
func openaiOptions(effort string) openai.OpenAIResponsesOptions {
	store := false
	return openai.OpenAIResponsesOptions{
		ReasoningEffort: openaiEffort(effort),
		Store:           &store,
		Include:         []string{"reasoning.encrypted_content"},
	}
}

// chatOptions is a Chat Completions call at the model's reasoning effort, with a Final reply's
// schema sent as response_format.json_schema in OpenAI's strict mode, so the reply is
// constrained as it is generated rather than only checked afterwards. Strict mode refuses a
// schema outside the subset it compiles — a property left out of required and a union at the
// root among them — and every Final schema is held to that subset before the first call
// (strict.go), so the schemas this package serves are the schemas strict mode takes. The reply
// is still read back against the schema itself, raising FaultMalformed where it does not fit:
// an endpoint that speaks the Chat Completions protocol is not always OpenAI.
func chatOptions(effort string) openaicompatible.OpenAIOptions {
	strict := true
	return openaicompatible.OpenAIOptions{
		ReasoningEffort:  openaiEffort(effort),
		StrictJSONSchema: &strict,
	}
}
