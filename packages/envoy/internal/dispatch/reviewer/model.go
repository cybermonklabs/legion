package reviewer

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
)

// Bounds of one model call where the caller's Model sets none.
const (
	// DefaultTimeout bounds one call, its wait for the pacer included: the default of
	// DISPATCH_REVIEWER_TIMEOUT.
	DefaultTimeout = 120 * time.Second
	// DefaultOutputTokens bounds one reply, the model's reasoning included: a model that reasons
	// spends its reasoning against this bound before it writes a review's findings and a rewrite.
	// A caller that rewrites longer texts sets Model.OutputTokens.
	DefaultOutputTokens = 32768
	// callTries is the most tries one call makes: a reply that is not the object its schema
	// names, or one its caller cannot use, is asked again up to twice more.
	callTries = 3
)

// The purposes of a model call, as Call.Purpose names them. A review's, a rewrite's and a
// definition's are each the name of the call's system prompt (prompts/<purpose>.md).
const (
	PurposeReview   = "review"
	PurposeDetector = "detector"
	PurposeRewrite  = "rewrite"
	PurposeDefine   = "define"
)

// Model is the model the component calls and how. The caller chooses it: the server builds it
// from a model group's settings (DISPATCH_REVIEWER_MODEL_* or DISPATCH_REWRITE_DEFINE_MODEL_*,
// pointed at the stand-in under DISPATCH_TEST_MODEL_URL), and the harness from any candidate.
type Model struct {
	// Model is the model and its endpoint.
	Model agentloop.Model
	// Pace is the caller's share of the model's deployment on the process's one pacer
	// (modelpace); nil paces nothing.
	Pace *modelpace.Caller
	// Timeout bounds one call, its wait for the pacer included; zero is DefaultTimeout.
	Timeout time.Duration
	// OutputTokens bounds one reply; zero is DefaultOutputTokens.
	OutputTokens int
	// Temperature is the sampling temperature of every call; nil leaves the provider's default.
	Temperature *float64
}

// deployment is the deployment the model's requests name: an Azure deployment where the endpoint
// names one apart from the model, else the model id, which is a Foundry chat deployment's name.
func (m Model) deployment() string {
	return cmp.Or(m.Model.Endpoint.Deployment, m.Model.ID)
}

// strict is whether the model's calls send their schema in OpenAI's strict mode
// (agentloop.Request.StrictFinal): on openai-chat, where a model asked for no reasoning may write
// its reasoning as the reply in place of the object, and the endpoint holds the reply to the
// schema only in strict mode. Each schema a call sends keeps strict mode's rules where it asks
// for it (reviewSchema).
func (m Model) strict() bool {
	return m.Model.Endpoint.Kind == agentloop.OpenAIChat
}

// Call is one model call the component made: what it was for, the deployment its request named,
// the tries it made (Attempts), the model id the provider says served its last reply ("" where
// no reply came), what its tries used and cost together, and the fault its last try ended on, if
// any.
type Call struct {
	Purpose    string          `json:"purpose"`
	Deployment string          `json:"deployment"`
	Attempts   int             `json:"attempts"`
	Served     string          `json:"served"`
	Usage      agentloop.Usage `json:"usage"`
	Fault      agentloop.Fault `json:"fault,omitempty"`
}

// ModelError is a model call that gave no usable answer: the pacer could not serve it in time,
// the run could not be made, it ended on a fault (a provider error, the time bound, a refusal, a
// reply cut at its length), or its reply was not the object its schema names. The server posts a
// held text as written on any of them and counts it (R5).
type ModelError struct {
	// Purpose is the call's purpose (Call.Purpose).
	Purpose string
	// Fault is the run's fault, and FaultMalformed for a reply this package could not read;
	// empty where Err ended the call.
	Fault agentloop.Fault
	// Detail says what happened, for a log.
	Detail string
	// Err is the error that ended the call before an answer: the pacer's, the run's or the
	// context's.
	Err error
}

func (e *ModelError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("reviewer: the %s call failed: %v", e.Purpose, e.Err)
	}
	return fmt.Sprintf("reviewer: the %s call ended on %s: %s", e.Purpose, e.Fault, cmp.Or(e.Detail, "no detail"))
}

func (e *ModelError) Unwrap() error { return e.Err }

// call makes one model call for purpose: system as the system prompt, input as the one user
// message, and a reply that must be one object of schema, decoded as T, which usable may still
// refuse with a detail ("" for a reply it can use; nil refuses none). A reply that is not such an
// object, or that usable refuses, is FaultMalformed, and the call asks again, callTries tries in
// all. Every try waits for the pacer first, charged with the prompt's estimated tokens and the
// whole reply's bound, so every try is paid for. Every try runs inside the call's one time bound
// (Model.Timeout, and ctx's own deadline where it is sooner): a try that cannot start inside it
// is not made, and the call ends on the fault of the try before it. So a retry never makes a
// call last longer. Every failure is a *ModelError, and the Call holds every try made.
func call[T any](ctx context.Context, m Model, purpose, system, input string, schema json.RawMessage, usable func(T) string) (T, Call, error) {
	var none T
	c := Call{Purpose: purpose, Deployment: m.deployment()}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(m.Timeout, DefaultTimeout))
	defer cancel()
	var last error
	for c.Attempts < callTries {
		if last != nil && ctx.Err() != nil {
			break
		}
		value, made, err := m.try(ctx, &c, system, input, schema)
		switch {
		case !made && last != nil:
			return none, c, last
		case err != nil:
			var failed *ModelError
			if !errors.As(err, &failed) || failed.Fault != agentloop.FaultMalformed {
				return none, c, err
			}
			last = err
			continue
		}
		var reply T
		if err := json.Unmarshal(value, &reply); err != nil {
			last = malformed(&c, err.Error())
			continue
		}
		if usable != nil {
			if detail := usable(reply); detail != "" {
				last = malformed(&c, detail)
				continue
			}
		}
		return reply, c, nil
	}
	return none, c, last
}

// try makes one try of c: it waits for the pacer, runs the model and reads the object its reply
// must be, and adds to c what the try used, its served model and its fault. made is false where
// no try was made, because the pacer could not serve it inside the call's bound or the bound was
// spent; the error then says so.
func (m Model) try(ctx context.Context, c *Call, system, input string, schema json.RawMessage) (value json.RawMessage, made bool, err error) {
	output := cmp.Or(m.OutputTokens, DefaultOutputTokens)
	if m.Pace != nil {
		if err := m.Pace.WaitFor(ctx, utf8.RuneCountInString(system)+utf8.RuneCountInString(input), output); err != nil {
			return nil, false, &ModelError{Purpose: c.Purpose, Err: fmt.Errorf("pace the call: %w", err)}
		}
	}
	// The run's own bound is the time the call has left after the pacer's wait.
	left := cmp.Or(m.Timeout, DefaultTimeout)
	if deadline, ok := ctx.Deadline(); ok {
		left = time.Until(deadline)
	}
	if left <= 0 {
		return nil, false, &ModelError{Purpose: c.Purpose, Err: fmt.Errorf("pace the call: %w", context.DeadlineExceeded)}
	}
	c.Attempts++
	result, err := agentloop.Run(ctx, agentloop.Request{
		Model:       m.Model,
		System:      system,
		Input:       []agentloop.Message{{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: input}}}},
		NoCache:     true,
		Bounds:      agentloop.Bounds{Time: left, Calls: 1, OutputTokens: output},
		Final:       schema,
		StrictFinal: m.strict(),
		Temperature: m.Temperature,
	})
	c.Usage, c.Fault = plus(c.Usage, result.Usage), result.Fault
	if n := len(result.Steps); n > 0 {
		c.Served = result.Steps[n-1].Model
	}
	switch {
	case err != nil:
		return nil, true, &ModelError{Purpose: c.Purpose, Err: err}
	case result.Fault != "":
		return nil, true, &ModelError{Purpose: c.Purpose, Fault: result.Fault, Detail: result.Detail}
	}
	return result.Value, true, nil
}

// plus is two usages added, as agentloop adds a run's calls: the sum is metered where either
// side is.
func plus(a, b agentloop.Usage) agentloop.Usage {
	return agentloop.Usage{
		Input: a.Input + b.Input, Output: a.Output + b.Output,
		CacheWrite: a.CacheWrite + b.CacheWrite, CacheRead: a.CacheRead + b.CacheRead,
		Reasoning: a.Reasoning + b.Reasoning,
		CostUSD:   a.CostUSD + b.CostUSD,
		Metered:   a.Metered || b.Metered,
	}
}

// malformed is a *ModelError for a reply that fits its schema and still cannot be used, and marks
// the call with it.
func malformed(c *Call, detail string) error {
	c.Fault = agentloop.FaultMalformed
	return &ModelError{Purpose: c.Purpose, Fault: agentloop.FaultMalformed, Detail: detail}
}
