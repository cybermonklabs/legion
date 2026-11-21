package agent

import (
	"context"
	"errors"
	"time"

	"golang.org/x/time/rate"

	"github.com/sjawhar/envoy/agentloop"
)

// quotaShare is the agent's share of the chat deployment's tokens a minute
// (DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE): its runs take 75% of the quota, and the server's short
// answers the rest, each process with a pacer of its own, so a deployment with a smaller quota
// keeps the same split.
const quotaShare = 0.75

// charsPerToken is how many characters of a request count as one token: the rough rule the
// server's answerer paces its own share with (going.charsPerToken).
const charsPerToken = 4

// errQuotaPastDeadline is a model call the pacer cannot make inside the time its step has left.
var errQuotaPastDeadline = errors.New("the chat model's quota cannot pace this call inside the time the step has left")

// pacer paces the agent's model calls to its share of the deployment's quota: one rate.Limiter
// of the share's tokens a second with a burst of a minute's share, which each HTTP attempt draws
// the estimate of what it sends from before it is made.
type pacer struct {
	limiter *rate.Limiter
	now     func() time.Time
}

// newPacer is the pacer of a deployment whose quota is tokensPerMinute, nil for none (the
// endpoint form names no quota).
func newPacer(tokensPerMinute int) *pacer {
	if tokensPerMinute <= 0 {
		return nil
	}
	share := int(float64(tokensPerMinute) * quotaShare)
	return &pacer{limiter: rate.NewLimiter(rate.Limit(float64(share)/60), share), now: time.Now}
}

// delay reserves tokens at now and answers how long the call waits for them; false where no wait
// serves them, the call costing more than a minute's whole share, which reserves nothing.
func (p *pacer) delay(now time.Time, tokens int) (*rate.Reservation, time.Duration, bool) {
	reservation := p.limiter.ReserveN(now, min(tokens, p.limiter.Burst()))
	if !reservation.OK() {
		return nil, 0, false
	}
	return reservation, reservation.DelayFrom(now), true
}

// wait waits out the pacing of tokens, refusing a wait that ends past ctx's deadline rather than
// spending the step's time on a call it cannot make.
func (p *pacer) wait(ctx context.Context, tokens int) error {
	now := p.now()
	reservation, wait, ok := p.delay(now, tokens)
	if !ok {
		return errQuotaPastDeadline
	}
	if wait <= 0 {
		return nil
	}
	if deadline, has := ctx.Deadline(); has && now.Add(wait).After(deadline) {
		reservation.CancelAt(now)
		return errQuotaPastDeadline
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		reservation.CancelAt(p.now())
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// paced is model with the pacing taken in its endpoint's Token, which agentloop calls before
// every HTTP attempt, the retry included: each attempt is charged estimate's tokens. A nil pacer
// paces nothing.
func (p *pacer) paced(model agentloop.Model, estimate func() int) agentloop.Model {
	if p == nil {
		return model
	}
	token := model.Endpoint.Token
	model.Endpoint.Token = func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := p.wait(ctx, estimate()); err != nil {
			return "", err
		}
		return token(ctx)
	}
	return model
}
