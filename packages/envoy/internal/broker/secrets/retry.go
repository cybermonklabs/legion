package secrets

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/sjawhar/envoy/internal/metrics"
)

// The retry's numbers. A mint runs inside the release of a grant, which its caller waits for
// whole, so the bounds are the mint's: at most mintAttemptsPerCall attempts of each upstream
// call, and at most mintWaitBudget of added wait over every call one mint makes (a GitHub mint
// lists installation pages, asks for a token for each owner, perhaps once more without actions,
// perhaps revokes one). Each attempt runs under mintAttemptTimeout, since the production minters'
// http.DefaultClient has none and the airlock mints under a caller's request that has no
// deadline: without it a hung attempt would spend the whole release and leave no time to retry.
const (
	mintAttemptsPerCall = 3
	mintWaitBudget      = 10 * time.Second
	mintAttemptTimeout  = 5 * time.Second
	// rateLimitedWait is the wait a rate-limited answer that names none counts as: GitHub's
	// documented minimum for a secondary rate limit, and the default
	// internal/dispatch/githubapp's rateLimitWait uses. It is past mintWaitBudget, so such an
	// answer ends the mint.
	rateLimitedWait = time.Minute
	// retryBudgetSize retries a minute, refilling one each retryBudgetRefill, is the process-wide
	// budget every mint's retries draw from: the server-wide retry budget of Google's SRE book, so
	// an upstream brownout cannot triple every release's calls at once.
	retryBudgetSize   = 60
	retryBudgetRefill = time.Second
)

// backoffCeilings is the ceiling of the jittered wait before each retry: before the second
// attempt and before the third. The wait is full jitter, drawn from zero to the ceiling, and is
// waited only when the server named no wait of its own.
var backoffCeilings = [mintAttemptsPerCall - 1]time.Duration{2 * time.Second, 4 * time.Second}

// The outcome of one attempt, as broker_mint_attempts_total names it: the answer the call wanted;
// an upstream failure (a 5xx, no usable answer, an answer past its cap or of a status no call
// wants); a rate limit (rateLimited); a 4xx that is not one; and a call the caller's context
// ended.
const (
	attemptOK          = "ok"
	attemptUpstream    = "upstream"
	attemptRateLimited = "rate_limited"
	attemptRefused     = "refused"
	attemptCancelled   = "cancelled"
)

// Retry is the one policy every call of the three HTTP minters (GitHubApp, GoogleLogin and
// GoogleServiceAccount) is retried under (mintCall), and the one process-wide budget their
// retries draw from. The broker builds one (NewRetry) and sets it on all three, so a GitHub
// brownout that spends the budget leaves a Google mint's failure unretried as well. A nil Retry
// makes one attempt of each call and counts nothing.
//
// Now, Sleep and Draw are the clock, the wait and the jitter's draw, each the real one when nil,
// so a test sets all three and never sleeps.
type Retry struct {
	// Now is the clock the budget refills by and a server's HTTP date or rate-limit reset is read
	// against; time.Now when nil. A context's deadline is read on the real clock, as the context
	// keeps it.
	Now func() time.Time
	// Sleep waits wait, or until ctx ends, when it answers ctx's error; a timer when nil.
	Sleep func(ctx context.Context, wait time.Duration) error
	// Draw is the jitter: a wait from zero to ceiling, both included, uniform at random when nil.
	// What it answers is held to that range.
	Draw func(ceiling time.Duration) time.Duration
	// attempts counts each attempt in broker_mint_attempts_total by kind, outcome and attempt;
	// nil counts nothing. NewRetry sets it.
	attempts *metrics.Counter

	// budget is the process-wide retry budget, built at the first draw (take), since a Retry is
	// a struct a caller may build itself; once guards that build, and the limiter its own draws.
	once   sync.Once
	budget *rate.Limiter
}

// NewRetry is the broker's Retry, counting its attempts on r as broker_mint_attempts_total.
func NewRetry(r *metrics.Registry) *Retry {
	return &Retry{attempts: r.NewCounter("broker_mint_attempts_total",
		"Attempts of the calls the minters make upstream, by the secret's kind, the outcome (ok, upstream, rate_limited, refused, cancelled) and the attempt's number (1 to 3).")}
}

// MintAttempts is the attempts the upstream call a mint failed at made, which the broker's
// failure line names, and 0 for a mint that failed before or after any call failed (a stored
// value it refused, an answer it could not decode).
func MintAttempts(err error) int {
	var attempts *attemptsError
	if errors.As(err, &attempts) {
		return attempts.attempts
	}
	return 0
}

// attemptsError is the error of a call that failed, carrying how many attempts it made. It says
// what the last attempt's error says and unwraps to it.
type attemptsError struct {
	err      error
	attempts int
}

func (e *attemptsError) Error() string { return e.err.Error() }

func (e *attemptsError) Unwrap() error { return e.err }

// withAttempts is err carrying the attempts named by call, a failed call's error, for a minter
// that names that failure in words of its own (a Google login's invalid_grant, a narrowing GitHub
// refused), so the failure line still names the attempts the call made.
func withAttempts(err, call error) error {
	if attempts := MintAttempts(call); attempts > 0 {
		return &attemptsError{err: err, attempts: attempts}
	}
	return err
}

func (r *Retry) now() time.Time {
	if r == nil || r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *Retry) sleep(ctx context.Context, wait time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, wait)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Retry) draw(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	if r.Draw == nil {
		return rand.N(ceiling + 1)
	}
	return min(max(r.Draw(ceiling), 0), ceiling)
}

// take draws one retry from the process-wide budget, and reports false, drawing nothing, when it
// is spent. The budget is a token bucket on the Retry's clock: it starts full, at
// retryBudgetSize, gains one each retryBudgetRefill up to that size, and banks nothing while it
// is full.
func (r *Retry) take() bool {
	r.once.Do(func() { r.budget = rate.NewLimiter(rate.Every(retryBudgetRefill), retryBudgetSize) })
	return r.budget.AllowN(r.now(), 1)
}

// mintRetry is one mint's share of its minter's Retry: the kind its attempts are counted under,
// and what is left of the mint's mintWaitBudget, which every call the mint makes waits from. A
// share of a nil Retry makes one attempt of each call.
type mintRetry struct {
	retry    *Retry
	kind     string
	waitLeft time.Duration
}

// start is the share a Mint of kind begins with: the whole of mintWaitBudget.
func (r *Retry) start(kind string) *mintRetry {
	return &mintRetry{retry: r, kind: kind, waitLeft: mintWaitBudget}
}

// count counts one attempt of a call in broker_mint_attempts_total.
func (m *mintRetry) count(outcome string, attempt int) {
	if m.retry == nil {
		return
	}
	m.retry.attempts.Inc([2]string{"kind", m.kind}, [2]string{"outcome", outcome}, [2]string{"attempt", strconv.Itoa(attempt)})
}

// again reports whether a call whose attempt numbered attempt failed with err, a failure classify
// retries, is made once more, having waited first. A wait the server named (mintStatusError) is
// waited in full or not at all: when it is past what is left of the mint's wait budget, or past
// the context's deadline, the call ends with no further attempt. Otherwise the wait is full
// jitter under its backoffCeilings entry, which never draws past what is left of the budget. A
// retry draws one from the process-wide budget once its wait fits, and a spent budget ends the
// call; so does a context that ends during the wait.
func (m *mintRetry) again(ctx context.Context, attempt int, err error) bool {
	if m.retry == nil || attempt >= mintAttemptsPerCall {
		return false
	}
	var wait time.Duration
	var status *mintStatusError
	if errors.As(err, &status) && status.waitNamed {
		wait = status.wait
	} else {
		wait = m.retry.draw(min(backoffCeilings[attempt-1], m.waitLeft))
	}
	if wait > m.waitLeft || !waitable(ctx, wait) || !m.retry.take() {
		return false
	}
	m.waitLeft -= wait
	return m.retry.sleep(ctx, wait) == nil
}

// waitable is whether the context has the time to wait out a retry: it is not already done, and
// it either carries no deadline or has more than wait left (agentloop's transport reads it so).
func waitable(ctx context.Context, wait time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > wait
}
