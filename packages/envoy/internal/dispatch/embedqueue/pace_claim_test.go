package embedqueue

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A call the shared pace holds at its slowest, retry.MaxDelay (5 minutes, past claimWindow's 2),
// keeps its rows claimed through the whole wait, however late after the claim's last renewal the
// wait starts: each renewal comes renewClaimInterval (half claimWindow) after the one before it,
// and the call goes out with a claim young enough to run to callBound, so another process's scan
// cannot take the rows mid-wait or mid-call and embed them again. A canary that meets an outage
// drives the pace there (confirmSoloFailure). The rows run on a synctest fake clock, where the wait
// takes no real time and no Postgres statement may run, so each renewal is recorded
// (claimRenewal.extend) rather than written. A claim 45 s old or older is renewed as the call is
// made, before the pace (embedDocuments), so the latest a wait starts after a renewal is just
// under that: with the wait's first renewal a whole slice after the wait began, a wait that began
// 44.9 s after a renewal renewed first 104.9 s after it.
func TestAClaimIsRenewedThroughThePacesWaitAtItsSlowest(t *testing.T) {
	t.Parallel()
	for _, late := range []time.Duration{0, renewClaimInterval / 2, latestWait} {
		t.Run(fmt.Sprintf("the wait starts %v after the last renewal", late), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				inner := &fakeEmbedder{}
				requireRenewedThroughThePacesWait(t, Deps{Embedder: inner}.Paced(), retry.MaxDelay, late)
				if len(inner.calls) != 2 {
					t.Errorf("the embedder got %d calls, want 2", len(inner.calls))
				}
			})
		})
	}
}

// latestWait is the latest a wait can start after the claim's last renewal: a claim
// claimWindow-callBound-callSlack (45 s) old or older is renewed as the call is made. A var, as
// callBound is, so it follows callBound's value rather than repeating it.
var latestWait = claimWindow - callBound - callSlack - 100*time.Millisecond

// A call the pace holds, from a claim old enough that the wait ends just before the wait's own
// renewal falls due (renewClaimInterval), goes out with the claim renewed: the wait renews nothing,
// so only the renewal as the call goes out (embedDocuments, needing callBound and callSlack) keeps
// the claim young enough. The hold is the first of the pace's back-off steps that leaves the claim
// too young for that renewal as the call starts, and the claim's age is what remains of the
// interval, both computed from the constants (24 s from 35.9 s at today's values), so the row keeps
// testing that renewal when a constant moves. Unrenewed, the call went out with the claim 59.9 s
// old, past claimWindow less callBound less callSlack, and a call run to callBound ended with it
// 119.9 s old.
func TestACallThePaceHeldRenewsItsClaimAsItGoesOut(t *testing.T) {
	t.Parallel()
	needed := claimWindow - callBound - callSlack
	end := renewClaimInterval - 100*time.Millisecond
	steps := embed.NewRateLimitedEmbedder(&fakeEmbedder{})
	for steps.Interval() <= end-needed {
		steps.BackOff()
	}
	held := steps.Interval()
	synctest.Test(t, func(t *testing.T) {
		requireRenewedThroughThePacesWait(t, Deps{Embedder: &fakeEmbedder{}}.Paced(), held, end-held)
	})
}

// A wrapper of the embedder (metrics, tracing) given to Paced sits inside the pace and sees every
// call, and the pace stays in force: the wait renews the claim, and a canary that meets an outage
// backs the pace off.
func TestAWrappedEmbedderIsStillPaced(t *testing.T) {
	t.Parallel()
	paced := func(inner embed.Embedder) (Deps, *passThrough) {
		wrapper := &passThrough{Embedder: inner}
		return Deps{Embedder: wrapper}.Paced(), wrapper
	}
	t.Run("the pace's wait renews the claim", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			deps, wrapper := paced(&fakeEmbedder{})
			requireRenewedThroughThePacesWait(t, deps, retry.MaxDelay, latestWait)
			if wrapper.calls != 2 {
				t.Errorf("the wrapper saw %d calls, want 2", wrapper.calls)
			}
		})
	})
	t.Run("a canary that meets an outage backs the pace off", func(t *testing.T) {
		database := storetest.Open(t)
		seedIssue(t, database, "WRAP", "WRAP-1", "Title of WRAP-1")
		deps, wrapper := paced(&fakeEmbedder{err: simulatedOutage{}})
		deps.Store = database
		pace := deps.pace.Interval()
		succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), deps)
		if err != nil || succeeded != 0 || failed != 1 || blocked || throttled {
			t.Fatalf("ProcessBatch = %d, %d, %v, %v, %v; want 0, 1, not blocked, not throttled", succeeded, failed, blocked, throttled, err)
		}
		if wrapper.calls != 2 {
			t.Errorf("the wrapper saw %d calls, want 2", wrapper.calls)
		}
		if got := deps.pace.Interval(); got != 2*pace {
			t.Errorf("the canary's outage left the pace at %v, want it backed off from %v to %v", got, pace, 2*pace)
		}
	})
}

// requireRenewedThroughThePacesWait backs deps' pace off to interval, then sends a call whose claim
// was last renewed late before it. It requires each renewal through the pace's wait to come no
// later than renewClaimInterval after the renewal before it, and the call to go out no later than
// claimWindow less callBound less callSlack after the last, so a call run to callBound ends with
// its claim no older than claimWindow less callSlack.
func requireRenewedThroughThePacesWait(t *testing.T, deps Deps, interval, late time.Duration) {
	t.Helper()
	ctx := context.Background()
	backOffPace(t, deps, interval)
	started := time.Now()
	renewal := newClaimRenewal(deps, []pendingRow{{kind: "issue", id: "pace", text: "second"}}, started.Add(-late))
	claimed := []time.Time{renewal.last}
	renewal.extend = func(context.Context, []pendingRow) error {
		claimed = append(claimed, time.Now())
		return nil
	}
	if _, err := deps.embedDocuments(ctx, []string{"second"}, renewal); err != nil {
		t.Fatalf("second call: %v", err)
	}
	called := time.Now()
	if waited := called.Sub(started); waited != interval {
		t.Fatalf("the second call went out after %v; want after the pace's %v", waited, interval)
	}
	for i, until := range append(claimed[1:], called) {
		bound := renewClaimInterval
		if i == len(claimed)-1 {
			bound = claimWindow - callBound - callSlack
		}
		if gap := until.Sub(claimed[i]); gap > bound {
			t.Errorf("the claim renewed at %v was next renewed or sent at %v, %v later: past %v; renewals at %v",
				claimed[i].Sub(started), until.Sub(started), gap, bound, offsets(claimed, started))
		}
	}
}

// backOffPace makes deps' pace hold its next call interval, as failing canaries back it off: a
// first call, which deps' embedder must answer at once, takes the pace's one token, and the pace
// is backed off to interval.
func backOffPace(t *testing.T, deps Deps, interval time.Duration) {
	t.Helper()
	if _, err := deps.embedDocuments(context.Background(), []string{"first"}, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	for deps.pace.Interval() < interval {
		deps.pace.BackOff()
	}
	if got := deps.pace.Interval(); got != interval {
		t.Fatalf("the pace holds a call %v, want %v", got, interval)
	}
}

// A wait whose renewals are slow still ends by itself, and renews the claim each time it falls
// due. Cut into slices at the claim's next due time and counting only the slices, a wait whose
// renewal took renewClaimInterval or longer got slices of 0 and never shrank, and one just short of
// the interval shrank a second a minute here: each ran until the caller's context ended. Each
// renewal here takes renewClaimInterval and a second, or less a second, on a synctest fake clock,
// and the wait of three intervals runs under a context with an hour to live. From the claim's last
// renewal before the wait to the wait's return, each renewal, and the return, must come once the
// renewal before it has ended or renewClaimInterval after that one began, whichever is later: one
// held back longer lets the claim run out between them.
func TestAWaitWhoseRenewalsAreSlowStillEnds(t *testing.T) {
	t.Parallel()
	for _, took := range []time.Duration{renewClaimInterval + time.Second, renewClaimInterval - time.Second} {
		t.Run(fmt.Sprintf("each renewal takes %v", took), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
				defer cancel()
				started := time.Now()
				renewal := newClaimRenewal(Deps{}, []pendingRow{{kind: "issue", id: "slow", text: "slow"}}, started.Add(-renewClaimInterval))
				renewed := []time.Time{renewal.last}
				renewal.extend = func(context.Context, []pendingRow) error {
					renewed = append(renewed, time.Now())
					time.Sleep(took)
					return nil
				}
				waitOutDebt(ctx, 3*renewClaimInterval, renewal)
				ended := time.Now()
				if err := ctx.Err(); err != nil {
					t.Fatalf("the wait ended only with its context (%v), after %v and %d renewals; want it to end by itself", err, ended.Sub(started), len(renewed)-1)
				}
				for i, until := range append(renewed[1:], ended) {
					if gap := until.Sub(renewed[i]); gap > max(took, renewClaimInterval) {
						t.Errorf("the claim renewed at %v was next renewed or the wait returned at %v, %v later: want at most %v; renewals at %v", renewed[i].Sub(started), until.Sub(started), gap, max(took, renewClaimInterval), offsets(renewed, started))
					}
				}
			})
		})
	}
}

// passThrough wraps an embedder as a metrics or tracing wrapper would, counting the calls that
// reach it.
type passThrough struct {
	embed.Embedder
	calls int
}

func (p *passThrough) Embed(ctx context.Context, texts []string, inputType embed.InputType) ([][]float32, error) {
	p.calls++
	return p.Embedder.Embed(ctx, texts, inputType)
}

// simulatedOutage is an embedder's error marked as an outage (embed.IsOutage), as Azure's 500 is.
type simulatedOutage struct{}

func (simulatedOutage) Error() string { return "simulated: the model service failed" }
func (simulatedOutage) Outage() bool  { return true }

// offsets is each of times as its distance from start.
func offsets(times []time.Time, start time.Time) []time.Duration {
	distances := make([]time.Duration, len(times))
	for i, at := range times {
		distances[i] = at.Sub(start)
	}
	return distances
}
