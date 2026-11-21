// Package modelpace paces this process's calls to each model deployment it calls (INFRA-3231): one
// token limiter per deployment, which every caller of that deployment shares, so the process
// never spends more of a deployment's quota a minute than the deployment has, however many of its
// parts call it. Each caller's own tokens-per-minute setting is a ceiling on its share inside
// that limiter: a caller whose ceiling is under the deployment's rate leaves the rest of the rate
// to the others, and a caller whose ceiling is the rate can take all of it.
//
// A deployment's rate is, in order: its quota from DISPATCH_MODEL_DEPLOYMENT_QUOTAS (Config.Quotas),
// else the quota a caller's own setting states for it (Share.StatesQuota, the answerer's
// DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE), else the largest ceiling any caller sets for it. So
// where no quota is set, the caller with the largest ceiling can hold back every other caller of
// its deployment; a quota above each caller's ceiling leaves each of them a part. Each limiter, the
// deployment's and each caller's, holds a minute's tokens as its burst, as
// embed.RateLimitedEmbedder's quota does, and is charged with a call's estimated tokens before
// the call is made (Charge).
package modelpace

import (
	"cmp"
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ErrPastDeadline is a call the pacer cannot serve inside the time its context has left, or
// ever: a call that costs more than a minute's whole quota. It wraps context.DeadlineExceeded,
// because that is what it is to the caller - its deadline reached with no call made.
var ErrPastDeadline = fmt.Errorf("modelpace: the quota cannot pace this call inside the time left: %w", context.DeadlineExceeded)

// CharsPerToken is how many characters a token is taken to hold when a call is charged before it
// is made (Charge): a rough estimate for English prose, which pacing a shared quota needs.
const CharsPerToken = 4

// Charge is what a call is charged before it is made: its prompt's estimated tokens, promptRunes
// characters at CharsPerToken, and replyBound, the most tokens its reply may hold, whole, since
// the reply's length is not known before it comes. Every caller charges its calls so (WaitFor),
// the answerer (package going) with a reply bound of 0, since its request names none, and the
// boot (cmd/dispatch) refuses with it a quota that no call of a model group could fit.
func Charge(promptRunes, replyBound int) int {
	return promptRunes/CharsPerToken + replyBound
}

// Deployment names one model deployment, which is one limiter: the protocol its endpoint speaks
// (the caller's own kind, such as azure or anthropic), the host of that endpoint, and the
// deployment's name or the model's id.
type Deployment struct {
	Kind string
	Host string
	Name string
}

// Config configures a Pacer.
type Config struct {
	// Quotas is each deployment's tokens-per-minute quota, by the deployment's name
	// (DISPATCH_MODEL_DEPLOYMENT_QUOTAS).
	Quotas map[string]int
	// Now replaces time.Now.
	Now func() time.Time
	// Sleep waits d, or until ctx is done, which it answers with ctx's error; nil waits on a
	// timer. A test replaces it, with Now, to drive the pacer on a clock of its own.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Pacer holds the process's limiter of each deployment its callers joined. Build one at boot
// and hand it to every caller of a model, so callers of one deployment share its limiter.
type Pacer struct {
	quotas map[string]int
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error

	mu          sync.Mutex
	deployments map[Deployment]*deployment
}

// deployment is one deployment's limiter and the settings its rate is chosen from.
type deployment struct {
	limiter *rate.Limiter
	// quota is Config.Quotas' value for the deployment, 0 where it names none.
	quota int
	// stated is the largest quota a caller's setting states for it (Share.StatesQuota).
	stated int
	// ceiling is the largest ceiling any caller of it sets.
	ceiling int
}

// tokensPerMinute is the deployment's rate, 0 where nothing sets one.
func (d *deployment) tokensPerMinute() int {
	return cmp.Or(d.quota, d.stated, d.ceiling)
}

// New builds a Pacer that holds no deployment yet.
func New(cfg Config) *Pacer {
	p := &Pacer{quotas: cfg.Quotas, now: cfg.Now, sleep: cfg.Sleep, deployments: map[Deployment]*deployment{}}
	if p.now == nil {
		p.now = time.Now
	}
	if p.sleep == nil {
		p.sleep = sleep
	}
	return p
}

// Share is what one caller asks of a deployment.
type Share struct {
	Deployment Deployment
	// TokensPerMinute is the caller's own setting, the ceiling on its share of the deployment's
	// limiter; zero or less sets no ceiling of its own, and the deployment's limiter alone paces
	// it.
	TokensPerMinute int
	// StatesQuota says TokensPerMinute is the deployment's quota too, as the answerer's
	// DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE is: the deployment's rate where Config.Quotas names
	// none for it.
	StatesQuota bool
}

// Caller is one caller's share of a deployment: the deployment's limiter, which it shares, and
// its own ceiling's.
type Caller struct {
	pacer      *Pacer
	deployment *rate.Limiter
	// ceiling is nil where the caller sets no ceiling of its own.
	ceiling *rate.Limiter
}

// Join adds a caller to its deployment's limiter, creating the limiter for the deployment's first
// caller, and returns the caller's share. A caller that raises the deployment's rate (a larger
// ceiling where nothing else sets the rate) raises it for every caller of the deployment.
func (p *Pacer) Join(share Share) *Caller {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.deployments[share.Deployment]
	if d == nil {
		d = &deployment{quota: p.quotas[share.Deployment.Name]}
		p.deployments[share.Deployment] = d
	}
	before := d.tokensPerMinute()
	d.ceiling = max(d.ceiling, share.TokensPerMinute)
	if share.StatesQuota {
		d.stated = max(d.stated, share.TokensPerMinute)
	}
	switch tokens := d.tokensPerMinute(); {
	case d.limiter == nil:
		d.limiter = newLimiter(tokens)
	case tokens != before:
		now := p.now()
		d.limiter.SetBurstAt(now, tokens)
		d.limiter.SetLimitAt(now, limit(tokens))
	}
	caller := &Caller{pacer: p, deployment: d.limiter}
	if share.TokensPerMinute > 0 {
		caller.ceiling = newLimiter(share.TokensPerMinute)
	}
	return caller
}

// Wait charges tokens to the caller's share, the deployment's limiter and its own ceiling at once,
// and waits until both serve the call. A call neither could ever serve, or one they cannot serve
// inside the time ctx has left, is refused with ErrPastDeadline rather than waited on, since
// waiting would spend the caller's whole window to make no call; a ctx that ends during the wait
// ends it with ctx's error. A refused or abandoned call gives its tokens back.
func (c *Caller) Wait(ctx context.Context, tokens int) error {
	now := c.pacer.now()
	reservations := []*rate.Reservation{c.deployment.ReserveN(now, tokens)}
	if c.ceiling != nil {
		reservations = append(reservations, c.ceiling.ReserveN(now, tokens))
	}
	giveBack := func() {
		at := c.pacer.now()
		for _, reservation := range reservations {
			reservation.CancelAt(at)
		}
	}
	var delay time.Duration
	for _, reservation := range reservations {
		if !reservation.OK() {
			// The call costs more than a minute's whole quota, so no wait would ever serve it.
			giveBack()
			return ErrPastDeadline
		}
		delay = max(delay, reservation.DelayFrom(now))
	}
	if delay <= 0 {
		return nil
	}
	if deadline, ok := ctx.Deadline(); ok && now.Add(delay).After(deadline) {
		giveBack()
		return ErrPastDeadline
	}
	if err := c.pacer.sleep(ctx, delay); err != nil {
		giveBack()
		return err
	}
	return nil
}

// WaitFor is Wait charged with Charge(promptRunes, replyBound): a call of promptRunes characters
// of prompt whose reply may hold replyBound tokens.
func (c *Caller) WaitFor(ctx context.Context, promptRunes, replyBound int) error {
	return c.Wait(ctx, Charge(promptRunes, replyBound))
}

// newLimiter is a limiter of tokensPerMinute holding a minute's tokens, or one that serves every
// call at once where nothing sets a rate.
func newLimiter(tokensPerMinute int) *rate.Limiter {
	return rate.NewLimiter(limit(tokensPerMinute), tokensPerMinute)
}

// limit is tokensPerMinute as a rate.Limit, rate.Inf where it is no rate.
func limit(tokensPerMinute int) rate.Limit {
	if tokensPerMinute <= 0 {
		return rate.Inf
	}
	return rate.Limit(float64(tokensPerMinute) / 60)
}

// sleep waits d on a timer, or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
