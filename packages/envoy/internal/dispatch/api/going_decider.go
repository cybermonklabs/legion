package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
)

// goingDecisionTime bounds one decision call. R7 holds the measured model's p95 under 500 ms, so
// a call still running at twice that is an endpoint in trouble rather than a slow answer, and the
// bar answers without a decision rather than keep the person waiting on it.
const goingDecisionTime = time.Second

// ErrDecisionModelUnreachable is a decision call that could not be made: the decision model's
// token file is absent, unreadable or empty at the moment of the call. No request reached the
// endpoint. It is the call's failure and never a refusal to boot: the file may be written after
// the process starts, and the next call reads it again.
var ErrDecisionModelUnreachable = errors.New("the decision model cannot be reached")

// Decider is the decision model a deployment configured, with the entry the committed
// measurement holds for it (going.DecisionRecord): the two thresholds the server reads are that
// entry's and nobody else's. Only NewDecider builds one, and it builds none for a model the
// record does not hold (R11), so a *Decider is always a measured model.
type Decider struct {
	model  agentloop.Model
	record going.Record
}

// NewDecider is the decider for model, a System One model at its endpoint, and false where the
// record holds no entry for model.ID: a model nobody measured is no decision model, and no
// threshold is made up for it.
func NewDecider(model agentloop.Model) (*Decider, bool) {
	record, measured := going.DecisionRecord(model.ID)
	if !measured {
		return nil, false
	}
	return &Decider{model: model, record: record}, true
}

// Model is the decision model's name, as the deployment configured it and the record holds it.
func (d *Decider) Model() string {
	return d.model.ID
}

// Record is the measured entry of the model: its fitted thresholds and the measurement behind
// them.
func (d *Decider) Record() going.Record {
	return d.record
}

// Reachable reports whether a decision call could be made now: whether the endpoint's token
// reads. The deployment's token is a file the source reads on every call, so this is one file
// read and no network call, cheap enough for every capability read; whether the endpoint answers
// is a call's to find out.
func (d *Decider) Reachable(ctx context.Context) bool {
	_, err := d.model.Endpoint.Token(ctx)
	return err == nil
}

// Decide puts questions about state to the decision model in one agentloop.Decide call, bounded
// by goingDecisionTime. A token that cannot be read is ErrDecisionModelUnreachable, wrapping why,
// with no request made; every other outcome is agentloop.Decide's own, a fault included.
func (d *Decider) Decide(ctx context.Context, questions map[string]agentloop.Question, state json.RawMessage) (agentloop.DecideResult, error) {
	model := d.model
	read := model.Endpoint.Token
	// The transport reads the token on the goroutine of the call, once per attempt, so the
	// attempts of this one call never read it at once.
	var unread error
	model.Endpoint.Token = func(ctx context.Context) (string, error) {
		token, err := read(ctx)
		if err != nil {
			unread = err
		}
		return token, err
	}
	result, err := agentloop.Decide(ctx, agentloop.DecideRequest{
		Model:     model,
		State:     state,
		Questions: questions,
		Bounds:    agentloop.Bounds{Time: goingDecisionTime},
	})
	if err == nil && unread != nil {
		return agentloop.DecideResult{}, fmt.Errorf("%w: %w", ErrDecisionModelUnreachable, unread)
	}
	return result, err
}
