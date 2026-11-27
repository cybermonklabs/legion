package embed

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"golang.org/x/time/rate"
)

const (
	// rateLimitFloor is the fastest RateLimitedEmbedder ever paces calls: the steady-state ceiling
	// it climbs back to after a run of successful batches. Chosen to leave headroom below the
	// sustainable rate of the model service for a live search's own query embedding, which never
	// goes through this limiter at all (cmd/dispatch wires the unwrapped client there). The slowest
	// this limiter ever paces calls to, reached only after several consecutive throttled batches
	// in a row, is retry.MaxDelay (not a duplicate constant of its own): the same ceiling
	// embedqueue's own batchBackoff already uses, so the two mechanisms agree on how bad
	// "sustained throttling" gets before both are maxed out.
	rateLimitFloor = 750 * time.Millisecond
	// rateLimitRecoveryStep is how much one untroubled call (neither a throttle nor an outage) adds
	// to the pace, in calls per second - additive increase on the rate, the slow half of AIMD. The
	// floor's rate is 1/0.75 s = 4/3 calls per second and the ceiling's (retry.MaxDelay, 300 s) is
	// 1/300, so at 1/24 per call the limiter climbs from the ceiling back to the floor in
	// ceil((4/3 - 1/300) x 24) = ceil(31.92) = 32 untroubled calls, which wait 300 s (the
	// ceiling's interval, before the first of them) plus the sum over k = 1..31 of
	// 1/(1/300 + k/24) s, 93.7 s: 393.7 s in all. From one throttle at the floor (1.5 s, 2/3 calls
	// per second) it is back in (4/3 - 2/3) x 24 = 16 calls. Stepping the interval down by a fixed
	// 50 ms instead took (300 - 0.75) / 0.05 = 5,985 calls from the ceiling, about 10 days of
	// waits, so one short incident slowed every background embedding for days.
	rateLimitRecoveryStep = 1.0 / 24
	// rateLimitBackoffFactor is how much one throttled call divides the pace by, lengthening the
	// interval by as much - multiplicative decrease, the fast half of AIMD: back off hard and
	// immediately on the first sign the model service's shared quota is exceeded, the same shape
	// TCP congestion control uses for the same reason (a shared resource signalled "too much", not
	// a per-caller limit). A sustained run of throttles halves the pace on each until the interval
	// reaches retry.MaxDelay, and holds it there.
	rateLimitBackoffFactor = 2
)

// RateLimitedEmbedder paces calls to an inner Embedder so Dispatch's background embedding work -
// internal/dispatch/embedqueue, its only caller - never saturates the model service's quota to
// the point a live search request's own query embedding gets caught in the same throttling.
// embedqueue.Deps.Paced wraps one around the queue's embedder, which cmd/dispatch asks for;
// api.Deps keeps the unwrapped client directly, so a live query is never paced by this limiter
// and never queues behind background work, however deep into backoff that work currently is -
// bounded instead only by its own ordinary searchEmbedTimeout.
//
// The pacing is AIMD (additive increase, multiplicative decrease) on the rate of calls, as TCP's
// is on its window: it starts at rateLimitFloor, halves the rate (doubles the interval, by
// rateLimitBackoffFactor) the moment a call comes back throttled, or when embedqueue calls
// BackOff because its canary met an outage (fast, immediate backoff on the first sign of
// shared-resource pressure or of a failing service), and adds rateLimitRecoveryStep to the rate
// on every call that was neither a throttle nor an outage (slow, incremental recovery, so a
// provider that is only barely keeping up is not immediately hit with a burst back at full rate,
// and bounded: 32 calls from the slowest pace back to the fastest). A call that ran out of time is
// an outage (IsOutage, timedOut), so it does not count as untroubled. An outage on a call other
// than embedqueue's canary moves the pace not at all: a text the service fails on every time
// gives one on every failing half of its bisection, about eight calls a cycle for a batch of 96
// texts, which would take the pace to its ceiling within two cycles though the service is well.
// rateLimitFloor is deliberately below the model service's sustainable rate, which is what
// "leaves headroom" means in practice: background work's own steady-state ceiling stops short of
// the full quota, leaving room for a live query's call - never gated by this limiter at all - to
// get through even while background work is actively pacing itself near that ceiling.
type RateLimitedEmbedder struct {
	inner Embedder

	mu       sync.Mutex
	interval time.Duration
	limiter  *rate.Limiter
}

// NewRateLimitedEmbedder wraps inner for background callers; see RateLimitedEmbedder's own
// doc comment for who should (embedqueue) and should not (search.go's live query path) hold one.
func NewRateLimitedEmbedder(inner Embedder) *RateLimitedEmbedder {
	return &RateLimitedEmbedder{
		inner:    inner,
		interval: rateLimitFloor,
		limiter:  rate.NewLimiter(rate.Every(rateLimitFloor), 1),
	}
}

// Embed paces itself to the limiter's current interval via golang.org/x/time/rate's own
// reservation bookkeeping (burst 1, so every call waits out the full interval since the last
// one) rather than a hand-rolled elapsed-time computation, then calls the inner Embedder and
// adjusts that interval from the result: longer (the rate halved) on a throttled error
// (IsThrottled), unchanged on an outage (IsOutage, a call that ran out of time among them, which
// only BackOff acts on), and shorter (the rate raised by rateLimitRecoveryStep, the interval
// floored at rateLimitFloor) on anything else - an ordinary success or any other failure both
// count as "the model service was not signalling capacity pressure or failing this time." It
// reserves with Reserve rather than calling the package's own ctx-aware Wait, because Wait fails
// fast with its own error the moment it can tell the reservation's delay would outlast ctx's
// deadline, without ever waiting for ctx to actually end - and embedqueue's callers depend on
// ctx.Err() being set by the time this call returns (handleGroupFailure's own ctx.Err() check,
// which every embedRows loop iteration relies on), to tell a cancelled wait apart from every
// other failure. Reserve, by contrast, hands back a delay to wait out ourselves, so the wait
// (sleep) only ever gives up once ctx.Done() has genuinely fired.
func (r *RateLimitedEmbedder) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	return r.EmbedAfter(ctx, texts, inputType, sleep)
}

// EmbedAfter is Embed with the pace's wait before the call made by wait, which waits d and returns
// whether ctx is still live once it has: on false EmbedAfter gives its reservation back and returns
// ctx.Err(), so a wait returns false only once ctx has ended. embedqueue's wait renews the claim on
// the rows the call carries, since at the pace's slowest (retry.MaxDelay, 5 minutes) the wait
// outlasts that claim (2 minutes).
func (r *RateLimitedEmbedder) EmbedAfter(ctx context.Context, texts []string, inputType InputType, wait func(ctx context.Context, d time.Duration) bool) ([][]float32, error) {
	reservation := r.limiter.Reserve()
	if delay := reservation.Delay(); delay > 0 && !wait(ctx, delay) {
		reservation.Cancel()
		return nil, ctx.Err()
	}
	vectors, err := r.inner.Embed(ctx, texts, inputType)
	r.adjust(err)
	return vectors, err
}

// sleep waits d, and returns false without waiting the rest of it once ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Identity is the inner Embedder's: pacing calls changes nothing about which model embeds them.
func (r *RateLimitedEmbedder) Identity() string { return r.inner.Identity() }

// Limits is the inner Embedder's own.
func (r *RateLimitedEmbedder) Limits() Limits { return r.inner.Limits() }

// BackOff halves the rate as a throttled call does. embedqueue calls it when the canary it sent
// after a row's own failure meets an outage (IsOutage): the service itself is failing, not one
// text, and should not be asked again at full pace.
func (r *RateLimitedEmbedder) BackOff() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backOff()
}

// adjust moves interval (multiplicative backoff and additive recovery of the rate, as
// RateLimitedEmbedder's own doc comment describes) and pushes a changed value onto the
// rate.Limiter that Embed actually waits on; interval itself stays the authoritative, exact state
// (Interval and the tests read it directly) rather than being derived back out of the limiter's
// floating-point Limit, and each recovery step derives the rate from it, adds the step and takes
// the interval back, to the nanosecond. SetLimit takes the limiter's own internal mutex and
// re-derives its token-bucket state even when the value is identical, so adjust skips it at the
// floor and ceiling - the two steady states a run of consecutive successes or throttles settles
// into and stays at call after call.
func (r *RateLimitedEmbedder) adjust(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case IsThrottled(err):
		r.backOff()
	case IsOutage(err):
		// Unchanged: only BackOff, on embedqueue's canary, reads an outage as the service failing.
	default:
		perSecond := float64(time.Second)/float64(r.interval) + rateLimitRecoveryStep
		r.setLimit(max(time.Duration(float64(time.Second)/perSecond), rateLimitFloor))
	}
}

// backOff doubles interval, capped at retry.MaxDelay; the caller holds mu.
func (r *RateLimitedEmbedder) backOff() {
	r.setLimit(min(r.interval*rateLimitBackoffFactor, retry.MaxDelay))
}

// setLimit makes interval d and pushes a changed one onto the rate.Limiter; the caller holds mu.
func (r *RateLimitedEmbedder) setLimit(d time.Duration) {
	if d != r.interval {
		r.interval = d
		r.limiter.SetLimit(rate.Every(d))
	}
}

// Interval reports the limiter's current pacing interval, for logging and tests.
func (r *RateLimitedEmbedder) Interval() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interval
}

// setInterval forces the limiter's interval (and the rate.Limiter that actually paces Embed) to
// d, bypassing the floor/ceiling and AIMD step adjust applies - test-only, for scaling the floor
// down to something a test can wait out in milliseconds rather than rateLimitFloor's 750ms.
func (r *RateLimitedEmbedder) setInterval(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interval = d
	r.limiter.SetLimit(rate.Every(d))
}

// Bounded returns inner with each of its calls ended at bound through the call's context, whatever
// the place: embedqueue bounds every call it makes so, at its callBound, inside its
// RateLimitedEmbedder. A call the bound ended while its caller's context was live ran out of time,
// which is an outage (IsOutage), as a call that ran out of time inside a client is: it does not
// speed RateLimitedEmbedder up as an answered call does, and embedqueue's canary that meets it backs
// the pace off. Identity and Limits are inner's.
func Bounded(inner Embedder, bound time.Duration) Embedder {
	return bounded{Embedder: inner, bound: bound}
}

type bounded struct {
	Embedder
	bound time.Duration
}

func (b bounded) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	call, cancel := context.WithTimeout(ctx, b.bound)
	defer cancel()
	vectors, err := b.Embedder.Embed(call, texts, inputType)
	if err != nil && ctx.Err() == nil && call.Err() != nil && !IsOutage(err) && !IsThrottled(err) {
		return nil, &outageError{fmt.Errorf("the call ran past its %v bound: %w", b.bound, err)}
	}
	return vectors, err
}
