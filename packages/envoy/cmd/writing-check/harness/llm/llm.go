// Package llm makes the harness's model calls: one system prompt and one user message in, one
// reply out, through agentloop.Run, with no cache mark, with a bound on the calls in flight and a
// pace of uncached input tokens a minute that every client of a run shares, retries with a
// backoff, and each call's tokens priced at the model's price. It holds the Foundry client. The
// Claude client is package claude, which is the only package that can read a writer session's
// text: this package cannot name that type, since package claude imports it.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// The families of the models a run calls.
const (
	// FamilyClaude is a Claude model, called through the operator's model gateway.
	FamilyClaude = "claude"
	// FamilyFoundry is a chat deployment of Dispatch's Azure AI Foundry account.
	FamilyFoundry = "foundry"
)

// The bounds of one call where a Request or a Runner sets none.
const (
	// DefaultMaxTokens bounds one reply.
	DefaultMaxTokens = 4096
	// DefaultTimeout bounds one attempt of a call.
	DefaultTimeout = 180 * time.Second
	// DefaultRetries is the retries after a provider error that a retry can clear (429, a 5xx,
	// a network error) or an attempt past its time bound.
	DefaultRetries = 6
	// DefaultBackoff is the wait before the first of those retries; each next wait doubles.
	DefaultBackoff = 2 * time.Second
	// replyRetries is the retries, at once, after a reply that is not the object the request's
	// schema names.
	replyRetries = 2
	// maxReplyTokens is the bound a reply cut at its own bound is asked again up to, the bound
	// doubling each time.
	maxReplyTokens = 32000
)

// Request is one model call.
type Request struct {
	// Purpose names what the call is for, as the outputs record it ("reader", "grade:answered").
	Purpose string
	// System is the system prompt and User the one user message.
	System, User string
	// Schema is the JSON schema the reply must be one object of; nil asks for free text.
	Schema json.RawMessage
	// MaxTokens bounds the reply; zero is DefaultMaxTokens.
	MaxTokens int
}

// Response is one call's reply and what it cost.
type Response struct {
	// Text is the reply's prose, and Value the object a request with a Schema asked for.
	Text  string
	Value json.RawMessage
	// Requested is the model id the call named, and Served the id the provider says served it.
	Requested, Served string
	// Usage is the tokens of every attempt, priced at the model's price.
	Usage agentloop.Usage
	// Attempts is how many attempts the call took, and Elapsed how long they took in all.
	Attempts int
	Elapsed  time.Duration
}

// Client is one model a run calls. Only package claude's client also takes a writer session's
// text, by a method of its own.
type Client interface {
	// Name is the model id the candidate list names.
	Name() string
	// Family is FamilyClaude or FamilyFoundry.
	Family() string
	// Call makes one call.
	Call(ctx context.Context, req Request) (Response, error)
}

// Limit bounds the model calls in flight across every client that shares it.
type Limit chan struct{}

// NewLimit is a bound of n calls in flight; n below 1 is 1.
func NewLimit(n int) Limit { return make(Limit, max(n, 1)) }

// acquire waits for a free slot, or for ctx to end.
func (l Limit) acquire(ctx context.Context) error {
	select {
	case l <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l Limit) release() { <-l }

// enter takes a slot of the runner's bound, then passes its pacer with the attempt's estimate of
// tokens, and answers the attempt's charge. On an error it holds no slot.
func (r *Runner) enter(ctx context.Context, tokens int) (paced, error) {
	if r.Limit != nil {
		if err := r.Limit.acquire(ctx); err != nil {
			return paced{}, err
		}
	}
	charge, err := r.Pacer.admit(ctx, tokens)
	if err != nil && r.Limit != nil {
		r.Limit.release()
	}
	return charge, err
}

// leave corrects the attempt's charge to the usage its reply reported, and gives its slot back.
func (r *Runner) leave(charge paced, used agentloop.Usage) {
	charge.settle(used)
	if r.Limit != nil {
		r.Limit.release()
	}
}

// Hold takes one slot of the runner's bound and passes its pacer, for a call that does not go
// through Run, such as the detector's own reader. The harness does not build that call's prompt,
// so the pacer charges it nothing before it starts: it waits while the last minute leaves no
// room, and it counts the tokens the call's model calls report. Hold answers the function that
// gives the slot back, given the usage of each model call the call made.
func (r *Runner) Hold(ctx context.Context) (func(used ...agentloop.Usage), error) {
	charge, err := r.enter(ctx, 0)
	if err != nil {
		return nil, err
	}
	return func(used ...agentloop.Usage) {
		var sum agentloop.Usage
		for _, u := range used {
			addUsage(&sum, u)
		}
		r.leave(charge, sum)
	}, nil
}

// Runner makes calls on any model: within the shared Limit and Pacer, again after a backoff on a
// failure a retry can clear, and again at once on a reply its schema does not fit.
type Runner struct {
	// Limit bounds the calls in flight; nil bounds none.
	Limit Limit
	// Pacer paces each attempt's uncached input tokens; nil paces and counts none.
	Pacer *Pacer
	// Timeout bounds one attempt; zero is DefaultTimeout.
	Timeout time.Duration
	// Retries and Backoff are the retries after a failure a retry can clear and the first
	// wait; a negative Retries is none, zero is DefaultRetries, and zero Backoff is
	// DefaultBackoff.
	Retries int
	Backoff time.Duration
	// Sleep waits d, or until ctx ends, and answers whether the wait ran out; nil is a timer.
	Sleep func(ctx context.Context, d time.Duration) bool
}

// CallError is a call that gave no usable reply after its retries.
type CallError struct {
	// Purpose and Model name the call.
	Purpose, Model string
	// Fault is the last attempt's fault, empty where Err ended it.
	Fault agentloop.Fault
	// Detail says what happened, and Err is the error that ended the call before a reply.
	Detail string
	Err    error
	// Attempts is how many attempts the call took.
	Attempts int
}

func (e *CallError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s call to %s failed after %d attempts: %v", e.Purpose, e.Model, e.Attempts, e.Err)
	}
	return fmt.Sprintf("%s call to %s ended on %s after %d attempts: %s", e.Purpose, e.Model, e.Fault, e.Attempts, e.Detail)
}

func (e *CallError) Unwrap() error { return e.Err }

// statusPattern finds the HTTP status a provider error's detail names.
var statusPattern = regexp.MustCompile(`status (\d{3})`)

// retryableDetail is whether a provider error's detail names a status a retry can clear: a 429
// or a 5xx, or no status at all (a network error).
func retryableDetail(detail string) bool {
	m := statusPattern.FindStringSubmatch(detail)
	if m == nil {
		return true
	}
	code, _ := strconv.Atoi(m[1])
	return code == 429 || code >= 500
}

// Run makes req on model. Every attempt's tokens count in the Response's Usage, priced at
// model.Price, whether the call answered or not: a cache write at the model's cache write price
// and a cache read at its cache read price. Every attempt passes the runner's Pacer, which
// charges its estimate before it starts and the uncached input its reply reports after.
func (r *Runner) Run(ctx context.Context, model agentloop.Model, req Request) (resp Response, err error) {
	resp = Response{Requested: model.ID}
	started := time.Now()
	defer func() { resp.Elapsed = time.Since(started) }()
	retries := r.Retries
	switch {
	case retries == 0:
		retries = DefaultRetries
	case retries < 0:
		retries = 0
	}
	wait := r.Backoff
	if wait == 0 {
		wait = DefaultBackoff
	}
	sleep := r.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}
	// A call with no reasoning asks for temperature 0, so a judgement is as repeatable as the
	// provider makes it; a model that reasons first takes the provider's own temperature, which
	// Anthropic requires of a call with thinking on. The Anthropic client drops the setting for a
	// model that takes no sampling settings (claude-opus-5 and the -5 families among them), which
	// then samples at the provider's own setting.
	var temperature *float64
	if model.Effort == "" || model.Effort == "off" {
		zero := 0.0
		temperature = &zero
	}
	var est int
	if r.Pacer != nil {
		est = estimateOf(req)
	}
	backoffs, replies := 0, 0
	fail := func(fault agentloop.Fault, detail string, err error) (Response, error) {
		return resp, &CallError{Purpose: req.Purpose, Model: model.ID, Fault: fault, Detail: detail, Err: err, Attempts: resp.Attempts}
	}
	for {
		charge, err := r.enter(ctx, est)
		if err != nil {
			return fail("", "", err)
		}
		resp.Attempts++
		result, err := agentloop.Run(ctx, agentloop.Request{
			Model:       model,
			System:      req.System,
			Input:       []agentloop.Message{{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: req.User}}}},
			NoCache:     true,
			Bounds:      agentloop.Bounds{Time: timeout, Calls: 1, OutputTokens: maxTokens},
			Final:       req.Schema,
			Temperature: temperature,
		})
		r.leave(charge, result.Usage)
		addUsage(&resp.Usage, result.Usage)
		if n := len(result.Steps); n > 0 {
			resp.Served = result.Steps[n-1].Model
		}
		switch {
		case err != nil:
			return fail("", "", err)
		case result.Fault == agentloop.FaultProviderError || result.Fault == agentloop.FaultTime:
			if ctx.Err() != nil {
				return fail(result.Fault, result.Detail, ctx.Err())
			}
			if (result.Fault == agentloop.FaultProviderError && !retryableDetail(result.Detail)) || backoffs >= retries {
				return fail(result.Fault, result.Detail, nil)
			}
			if !sleep(ctx, wait) {
				return fail(result.Fault, result.Detail, ctx.Err())
			}
			backoffs++
			wait *= 2
			continue
		case result.Fault == agentloop.FaultMalformed || result.Fault == agentloop.FaultRefusal:
			// A reply out of its schema, or a refusal: each is asked again at once, up to
			// replyRetries times.
			if replies >= replyRetries {
				return fail(result.Fault, result.Detail, nil)
			}
			replies++
			continue
		case result.Fault == agentloop.FaultLength && maxTokens < maxReplyTokens:
			// A reply cut at its bound, such as a long list for a long document: asked again
			// with twice the bound.
			maxTokens = min(2*maxTokens, maxReplyTokens)
			continue
		case result.Fault != "":
			return fail(result.Fault, result.Detail, nil)
		}
		resp.Text, resp.Value = result.Text, result.Value
		if req.Schema != nil && !json.Valid(resp.Value) {
			if replies >= replyRetries {
				return fail(agentloop.FaultMalformed, "the reply is not one JSON object", nil)
			}
			replies++
			continue
		}
		return resp, nil
	}
}

// addUsage adds one attempt's usage to a call's.
func addUsage(to *agentloop.Usage, u agentloop.Usage) {
	to.Input += u.Input
	to.Output += u.Output
	to.CacheWrite += u.CacheWrite
	to.CacheRead += u.CacheRead
	to.Reasoning += u.Reasoning
	to.CostUSD += u.CostUSD
	to.Metered = to.Metered || u.Metered
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// FoundryConfig is how a run reaches Dispatch's Azure AI Foundry account: its models inference
// endpoint (the account's base URL with /models on the end), the api-version the chat route
// takes, and the keyless Entra sign-in the account's dev identity admits.
type FoundryConfig struct {
	Endpoint, APIVersion string
	Entra                embed.EntraConfig
}

// Tokens is the Entra token source the config names, shared by every Foundry client of a run so
// one exchange serves them all.
func (c FoundryConfig) Tokens() (func(context.Context) (string, error), error) {
	if c.Endpoint == "" {
		return nil, errors.New("no Foundry endpoint: give --foundry-endpoint")
	}
	source, err := embed.NewEntraTokenSource(c.Entra)
	if err != nil {
		return nil, fmt.Errorf("the Foundry sign-in: %w", err)
	}
	return source.Token, nil
}

// Foundry is a chat deployment of the Foundry account. It reads the text of a case and of
// Dispatch's record alone: it has no method that takes a writer session's text.
type Foundry struct {
	model  agentloop.Model
	runner *Runner
}

// NewFoundry is the client of deployment id at the config's endpoint, signed in with tokens, asked
// for the reasoning effort effort in agentloop's words ("" sends none), and priced at price.
func NewFoundry(cfg FoundryConfig, tokens func(context.Context) (string, error), id, effort string, price agentloop.Price, runner *Runner) *Foundry {
	var query url.Values
	if cfg.APIVersion != "" {
		query = url.Values{"api-version": {cfg.APIVersion}}
	}
	return &Foundry{
		model: agentloop.Model{
			Endpoint: agentloop.Endpoint{Kind: agentloop.OpenAIChat, BaseURL: strings.TrimRight(cfg.Endpoint, "/"), Query: query, Token: tokens},
			ID:       id,
			Effort:   effort,
			Price:    price,
		},
		runner: runner,
	}
}

// Name is the deployment's id.
func (f *Foundry) Name() string { return f.model.ID }

// Family is FamilyFoundry.
func (f *Foundry) Family() string { return FamilyFoundry }

// Model is the deployment as agentloop calls it, for a component that makes its own call (the
// reviewer's Rewrite and Define, the detector's reader).
func (f *Foundry) Model() agentloop.Model { return f.model }

// Call makes one call on the deployment.
func (f *Foundry) Call(ctx context.Context, req Request) (Response, error) {
	return f.runner.Run(ctx, f.model, req)
}
