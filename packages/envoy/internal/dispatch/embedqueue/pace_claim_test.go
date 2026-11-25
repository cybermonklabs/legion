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
// and the call goes out no later than that after the last, so another process's scan cannot take
// the rows mid-wait and embed them again. A canary that meets an outage drives the pace there
// (confirmSoloFailure). The rows run on a synctest fake clock, where the wait takes no real time
// and no Postgres statement may run, so each renewal is recorded (claimRenewal.extend) rather than
// written. With the wait's first renewal a whole slice after the wait began, a wait that began
// 59.9 s after a renewal renewed first 119.9 s after it, 0.1 s inside claimWindow.
func TestAClaimIsRenewedThroughThePacesWaitAtItsSlowest(t *testing.T) {
	for _, late := range []time.Duration{0, renewClaimInterval / 2, renewClaimInterval - 100*time.Millisecond} {
		t.Run(fmt.Sprintf("the wait starts %v after the last renewal", late), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				inner := &fakeEmbedder{}
				requireRenewedThroughTheSlowestWait(t, Deps{Embedder: inner}.Paced(), late)
				if len(inner.calls) != 2 {
					t.Errorf("the embedder got %d calls, want 2", len(inner.calls))
				}
			})
		})
	}
}

// A wrapper of the embedder (metrics, tracing) leaves the queue's pace in force, wherever it goes.
// Given to Paced, it sits inside the pace and sees every call. Put around Embedder after Paced, it
// is not on the call path, since the queue calls its pace (Deps.pace), but Embedder's type is then
// the wrapper's: a queue that found its pace by Embedder's type lost it there, and the claim's
// renewal through the pace's wait and the canary's back-off both stopped with nothing failing. In
// either place the wait renews the claim and a canary that meets an outage backs the pace off.
func TestAWrappedEmbedderIsStillPaced(t *testing.T) {
	for _, place := range []struct {
		name string
		wrap func(inner embed.Embedder) (Deps, *passThrough)
		// calls is how many of the queue's two calls reach the wrapper.
		calls int
	}{
		{"given to Paced", func(inner embed.Embedder) (Deps, *passThrough) {
			wrapper := &passThrough{Embedder: inner}
			return Deps{Embedder: wrapper}.Paced(), wrapper
		}, 2},
		{"put around Embedder after Paced", func(inner embed.Embedder) (Deps, *passThrough) {
			deps := Deps{Embedder: inner}.Paced()
			wrapper := &passThrough{Embedder: deps.Embedder}
			deps.Embedder = wrapper
			return deps, wrapper
		}, 0},
	} {
		t.Run(place.name+": the pace's wait renews the claim", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				deps, wrapper := place.wrap(&fakeEmbedder{})
				requireRenewedThroughTheSlowestWait(t, deps, renewClaimInterval-100*time.Millisecond)
				if wrapper.calls != place.calls {
					t.Errorf("the wrapper saw %d calls, want %d", wrapper.calls, place.calls)
				}
			})
		})
		t.Run(place.name+": a canary that meets an outage backs the pace off", func(t *testing.T) {
			database := storetest.Open(t)
			seedIssue(t, database, "WRAP", "WRAP-1", "Title of WRAP-1")
			deps, wrapper := place.wrap(&fakeEmbedder{err: simulatedOutage{}})
			deps.Store = database
			pace := deps.pace.Interval()
			succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), deps)
			if err != nil || succeeded != 0 || failed != 1 || blocked || throttled {
				t.Fatalf("ProcessBatch = %d, %d, %v, %v, %v; want 0, 1, not blocked, not throttled", succeeded, failed, blocked, throttled, err)
			}
			if wrapper.calls != place.calls {
				t.Errorf("the wrapper saw %d calls, want %d", wrapper.calls, place.calls)
			}
			if got := deps.pace.Interval(); got != 2*pace {
				t.Errorf("the canary's outage left the pace at %v, want it backed off from %v to %v", got, pace, 2*pace)
			}
		})
	}
}

// requireRenewedThroughTheSlowestWait backs deps' pace off to its slowest, then sends a call whose
// claim was last renewed late before it, and requires each renewal through the pace's wait, and the
// call itself, to come no later than renewClaimInterval after the renewal before it.
func requireRenewedThroughTheSlowestWait(t *testing.T, deps Deps, late time.Duration) {
	t.Helper()
	ctx := context.Background()
	backOffPace(t, deps, retry.MaxDelay)
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
	if waited := called.Sub(started); waited != retry.MaxDelay {
		t.Fatalf("the second call went out after %v; want after the pace's slowest, %v", waited, retry.MaxDelay)
	}
	for i, until := range append(claimed[1:], called) {
		if gap := until.Sub(claimed[i]); gap > renewClaimInterval {
			t.Errorf("the claim renewed at %v was next renewed or sent at %v, %v later: past renewClaimInterval (%v), within %v of claimWindow (%v); renewals at %v",
				claimed[i].Sub(started), until.Sub(started), gap, renewClaimInterval, claimWindow-gap, claimWindow, offsets(claimed, started))
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
// and the wait of three intervals runs under a context with an hour to live. Each renewal must
// begin once the one before it has ended or renewClaimInterval after that one began, whichever is
// later: a renewal held back longer lets the claim run out between them.
func TestAWaitWhoseRenewalsAreSlowStillEnds(t *testing.T) {
	for _, took := range []time.Duration{renewClaimInterval + time.Second, renewClaimInterval - time.Second} {
		t.Run(fmt.Sprintf("each renewal takes %v", took), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
				defer cancel()
				started := time.Now()
				renewal := newClaimRenewal(Deps{}, []pendingRow{{kind: "issue", id: "slow", text: "slow"}}, started.Add(-renewClaimInterval))
				var renewed []time.Time
				renewal.extend = func(context.Context, []pendingRow) error {
					renewed = append(renewed, time.Now())
					time.Sleep(took)
					return nil
				}
				waitOutDebt(ctx, 3*renewClaimInterval, renewal)
				if err := ctx.Err(); err != nil {
					t.Fatalf("the wait ended only with its context (%v), after %v and %d renewals; want it to end by itself", err, time.Since(started), len(renewed))
				}
				for i := 1; i < len(renewed); i++ {
					if gap := renewed[i].Sub(renewed[i-1]); gap > max(took, renewClaimInterval) {
						t.Errorf("a renewal began %v after the one before it, want at most %v; renewals at %v", gap, max(took, renewClaimInterval), offsets(renewed, started))
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
