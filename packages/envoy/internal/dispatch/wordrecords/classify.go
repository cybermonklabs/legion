package wordrecords

import (
	"cmp"
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
)

// Bounds of one classification call.
const (
	// DefaultCallTime bounds one call, its retry inside agentloop included.
	DefaultCallTime = 120 * time.Second
	// CallOutputTokens is the most tokens one reply may hold: one verdict and a few words. Each
	// call is charged it whole to the pacer before it is made, with the prompt's estimated tokens
	// (modelpace.Charge), so the job's model group needs a minute's quota of more than this and
	// the charge of SelectPrompt (cmd/dispatch: DISPATCH_WORD_RECORDS_MODEL_TOKENS_PER_MINUTE).
	CallOutputTokens = 1024
)

// Classifier gives one person's text its verdict with the selection prompt.
type Classifier struct {
	// Model is the records job's model group (cmd/dispatch: DISPATCH_WORD_RECORDS_MODEL_*).
	Model agentloop.Model
	// Pace is the group's share of its deployment on the process's one pacer (modelpace); nil
	// paces nothing.
	Pace *modelpace.Caller
	// CallTime bounds one call; zero is DefaultCallTime.
	CallTime time.Duration
}

// Verdict is what one call answered: the verdict, the words it is about, and the model the
// provider served.
type Verdict struct {
	Verdict string
	Words   []string
	Model   string
}

// Classify makes one call for input (Input), at temperature 0 with a reply of VerdictSchema. Any
// failure is an error: a call the pacer cannot serve in time, a provider error, the time bound, a
// refusal and a reply that is not one verdict of the schema. The job keeps the text for another
// attempt.
func (c Classifier) Classify(ctx context.Context, input string) (Verdict, error) {
	callTime := cmp.Or(c.CallTime, DefaultCallTime)
	ctx, cancel := context.WithTimeout(ctx, callTime)
	defer cancel()
	if c.Pace != nil {
		if err := c.Pace.WaitFor(ctx, utf8.RuneCountInString(SelectPrompt)+utf8.RuneCountInString(input), CallOutputTokens); err != nil {
			return Verdict{}, fmt.Errorf("pace the call: %w", err)
		}
	}
	zero := 0.0
	result, err := agentloop.Run(ctx, agentloop.Request{
		Model:       c.Model,
		System:      SelectPrompt,
		Input:       []agentloop.Message{{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: input}}}},
		NoCache:     true,
		Bounds:      agentloop.Bounds{Time: callTime, Calls: 1, OutputTokens: CallOutputTokens},
		Final:       VerdictSchema,
		Temperature: &zero,
	})
	if err != nil {
		return Verdict{}, err
	}
	if result.Fault != "" {
		return Verdict{}, fmt.Errorf("%s: %s", result.Fault, cmp.Or(result.Detail, "no detail"))
	}
	verdict := Verdict{}
	if n := len(result.Steps); n > 0 {
		verdict.Model = result.Steps[n-1].Model
	}
	verdict.Verdict, verdict.Words, err = ParseVerdict(result.Value)
	if err != nil {
		return Verdict{}, err
	}
	if !AboutWords(verdict.Verdict) {
		verdict.Words = []string{}
	}
	return verdict, nil
}
