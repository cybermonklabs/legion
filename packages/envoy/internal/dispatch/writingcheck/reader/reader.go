// Package reader is the model reader of the writing check's detector: one model call per body
// reads the code stage's candidates and names those whose verb phrase is vacuous; every
// candidate it does not name states a fact.
//
// Every fault lets the write through: Read then answers no verdicts and an outcome naming the
// fault, and the caller reports that outcome rather than a finding.
package reader

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
)

// Prompt is the system prompt of every call: the definition of a vacuous verb phrase, its
// examples, and the rule that the body is data.
//
//go:embed prompt.md
var Prompt string

// The outcomes Read answers. A Read that read every candidate answers OutcomeRead.
const (
	// OutcomeRead is a call that answered a verdict for every candidate it was sent.
	OutcomeRead = ""
	// OutcomeCandidateCap is a call that answered for the first CandidateCap candidates; the
	// rest were not read.
	OutcomeCandidateCap = "candidate_cap"
	// OutcomeBudget is a call not made, because the token budget for the minute is spent.
	OutcomeBudget = "reader_budget"
	// OutcomeTimeout is a call past Config.Timeout.
	OutcomeTimeout = "reader_timeout"
	// OutcomeError is a call the provider failed or refused, or one that could not be made.
	OutcomeError = "reader_error"
	// OutcomeMalformed is a reply that is not the schema's shape, or that names a candidate it
	// was not sent or names one twice.
	OutcomeMalformed = "reader_malformed"
	// OutcomeModelMismatch is a reply served by a model other than Config.BundleModelID. The
	// reader then turns itself off for the life of the process.
	OutcomeModelMismatch = "model_mismatch"
	// OutcomeNoModel is the outcome of a check that has no reader at all; the caller that
	// holds a nil reader reports it.
	OutcomeNoModel = "no_model"
)

// CandidateCap is the most candidates one call reads.
const CandidateCap = 24

// Defaults for the zero Config fields.
const (
	DefaultTimeout         = 2 * time.Second
	DefaultMaxOutputTokens = 4096
)

// Config is how a Reader calls its model.
type Config struct {
	// Model is the model and its endpoint; AzureModel builds one for an Azure AI Foundry
	// chat deployment. Its Effort is set to "off" for every call, or left empty under
	// NoEffort.
	Model agentloop.Model
	// NoEffort sends no reasoning effort at all, for a Chat Completions deployment that
	// refuses the reasoning_effort parameter a call asking for effort "off" carries (a model
	// without reasoning). On the Anthropic protocols an empty effort turns thinking on, so
	// NoEffort is for openai-chat alone.
	NoEffort bool
	// BundleModelID is the model id the bundle was measured with. A reply served by another
	// model is OutcomeModelMismatch. An empty id pins no model, and no reply is checked.
	BundleModelID string
	// Timeout bounds one call; zero is DefaultTimeout.
	Timeout time.Duration
	// TokensPerMinute is the token budget of the calls, estimated before each call from the
	// prompt's length and MaxOutputTokens; zero sets no budget.
	TokensPerMinute int
	// MaxOutputTokens bounds one reply; zero is DefaultMaxOutputTokens.
	MaxOutputTokens int
	// Temperature is the sampling temperature of every call; nil leaves the provider's
	// default.
	Temperature *float64
	// Observe, when set, is called once after each model call, with what the provider
	// answered: a measurement reads the served model id and the fault from it.
	Observe func(Call)
}

// Verdict is the reader's answer for one candidate it sent: whether its verb phrase is vacuous,
// and the fix when it is. The model names only the vacuous candidates, so a candidate it does
// not name has Vacuous false and no Rewrite.
type Verdict struct {
	ID      string `json:"id"`
	Vacuous bool   `json:"vacuous"`
	Rewrite string `json:"rewrite"`
}

// Named is one candidate the model calls vacuous, as its reply carries it.
type Named struct {
	ID      string `json:"id"`
	Rewrite string `json:"rewrite"`
}

// Reply is the model's reply: the candidates it calls vacuous, and no others.
type Reply struct {
	Vacuous []Named `json:"vacuous"`
}

// Call is what one model call answered, as Config.Observe is given it: the model id the
// provider says served it ("" when no reply came), the run's fault and its detail, and the
// error that ended the run, if any.
type Call struct {
	Model  string
	Fault  agentloop.Fault
	Detail string
	Err    error
}

// Reader reads candidates with a model. It is safe for concurrent use.
type Reader struct {
	cfg      Config
	budget   *rate.Limiter
	calls    atomic.Int64
	mismatch atomic.Bool
}

// New is a Reader over cfg, its zero fields set to their defaults.
func New(cfg Config) *Reader {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = DefaultMaxOutputTokens
	}
	cfg.Model.Effort = "off"
	if cfg.NoEffort {
		cfg.Model.Effort = ""
	}
	budget := rate.NewLimiter(rate.Inf, 0)
	if cfg.TokensPerMinute > 0 {
		budget = rate.NewLimiter(rate.Limit(float64(cfg.TokensPerMinute)/60), cfg.TokensPerMinute)
	}
	return &Reader{cfg: cfg, budget: budget}
}

// AzureModel is the model of an Azure AI Foundry chat deployment: the Chat Completions shape at
// endpoint, the deployment named as the model, api-version on every request when apiVersion is
// set, and a bearer token from tokens (an *embed.EntraTokenSource's Token, for the keyless
// sign-in the answerer uses).
func AzureModel(endpoint, deployment, apiVersion string, tokens func(context.Context) (string, error)) agentloop.Model {
	var query url.Values
	if apiVersion != "" {
		query = url.Values{"api-version": {apiVersion}}
	}
	return agentloop.Model{
		Endpoint: agentloop.Endpoint{
			Kind:    agentloop.OpenAIChat,
			BaseURL: strings.TrimRight(endpoint, "/"),
			Query:   query,
			Token:   tokens,
		},
		ID:     deployment,
		Effort: "off",
	}
}

// Calls is the number of model calls the reader has made.
func (r *Reader) Calls() int64 { return r.calls.Load() }

// input is one candidate as the model is sent it.
type input struct {
	ID     string   `json:"id"`
	Field  string   `json:"field"`
	Text   string   `json:"text"`
	Before []string `json:"before"`
	After  []string `json:"after"`
}

// finalSchema is the JSON schema of the reply, Reply.
var finalSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"vacuous": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"rewrite": {"type": "string"}
				},
				"required": ["id", "rewrite"],
				"additionalProperties": false
			}
		}
	},
	"required": ["vacuous"],
	"additionalProperties": false
}`)

// Read reads up to CandidateCap candidates in one model call and answers a verdict for each
// candidate sent, in their order, with OutcomeRead, or OutcomeCandidateCap where more were
// given. On a fault it answers no verdicts and the fault's outcome. No candidates make no call.
func (r *Reader) Read(ctx context.Context, cands []candidates.Candidate) ([]Verdict, string) {
	if r.mismatch.Load() {
		return nil, OutcomeModelMismatch
	}
	if len(cands) == 0 {
		return nil, OutcomeRead
	}
	outcome := OutcomeRead
	if len(cands) > CandidateCap {
		cands, outcome = cands[:CandidateCap], OutcomeCandidateCap
	}
	sent := make([]input, len(cands))
	for i, c := range cands {
		sent[i] = input{ID: c.ID, Field: c.Sentence.Field, Text: c.Sentence.Text,
			Before: nonNil(c.Before), After: nonNil(c.After)}
	}
	user, err := json.Marshal(struct {
		Candidates []input `json:"candidates"`
	}{sent})
	if err != nil {
		return nil, OutcomeError
	}
	if !r.budget.AllowN(time.Now(), r.estimate(len(user))) {
		return nil, OutcomeBudget
	}

	r.calls.Add(1)
	result, err := agentloop.Run(ctx, agentloop.Request{
		Model:       r.cfg.Model,
		System:      Prompt,
		Input:       []agentloop.Message{{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: string(user)}}}},
		NoCache:     true,
		Bounds:      agentloop.Bounds{Time: r.cfg.Timeout, Calls: 1, OutputTokens: r.cfg.MaxOutputTokens},
		Final:       finalSchema,
		Temperature: r.cfg.Temperature,
	})
	if r.cfg.Observe != nil {
		call := Call{Fault: result.Fault, Detail: result.Detail, Err: err}
		if n := len(result.Steps); n > 0 {
			call.Model = result.Steps[n-1].Model
		}
		r.cfg.Observe(call)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, OutcomeTimeout
		}
		return nil, OutcomeError
	}
	if n := len(result.Steps); n > 0 && r.cfg.BundleModelID != "" && result.Steps[n-1].Model != r.cfg.BundleModelID {
		r.mismatch.Store(true)
		return nil, OutcomeModelMismatch
	}
	switch result.Fault {
	case "":
	case agentloop.FaultTime:
		return nil, OutcomeTimeout
	case agentloop.FaultMalformed, agentloop.FaultLength:
		return nil, OutcomeMalformed
	default:
		return nil, OutcomeError
	}
	verdicts, err := verdictsOf(result.Value, cands)
	if err != nil {
		return nil, OutcomeMalformed
	}
	return verdicts, outcome
}

// estimate is the tokens one call is counted at against the budget: the prompt at four bytes a
// token, and the most the reply may take.
func (r *Reader) estimate(userBytes int) int {
	return (len(Prompt)+userBytes+3)/4 + r.cfg.MaxOutputTokens
}

// verdictsOf is a verdict for each candidate sent, in their order, from the reply (which
// agentloop has read against finalSchema): vacuous for each candidate it names, not vacuous for
// each other. A reply that names a candidate not sent, or one twice, is an error.
func verdictsOf(value json.RawMessage, cands []candidates.Candidate) ([]Verdict, error) {
	var reply Reply
	if err := json.Unmarshal(value, &reply); err != nil {
		return nil, err
	}
	index := make(map[string]int, len(cands))
	verdicts := make([]Verdict, len(cands))
	for i, c := range cands {
		index[c.ID] = i
		verdicts[i] = Verdict{ID: c.ID}
	}
	for _, n := range reply.Vacuous {
		i, ok := index[n.ID]
		if !ok {
			return nil, fmt.Errorf("%s was not sent", n.ID)
		}
		if verdicts[i].Vacuous {
			return nil, fmt.Errorf("%s is named twice", n.ID)
		}
		verdicts[i].Vacuous, verdicts[i].Rewrite = true, n.Rewrite
	}
	return verdicts, nil
}

func nonNil(texts []string) []string {
	if texts == nil {
		return []string{}
	}
	return texts
}
