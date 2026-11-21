package natscallout

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// A decision never mixes two refreshes: it reads the refusal of the broker's token and its
// projects' answers under one lock. Two states alternate, each written by one whole refresh: in
// the first, Dispatch answers OTHER and says PUB's person cannot see PUB (no refusal, PUB not
// visible); in the second, Dispatch refuses the broker's token on OTHER and says PUB is visible
// (a refusal, PUB visible). Neither state admits PUB's daemon; only a torn read, the refusal from
// the first and PUB's answer from the second, admits it. The row runs until the refresher has made
// 5,000 refreshes and the decisions number 2,000,000, however long that takes; a deadline of five
// minutes fails only a stuck run. The control reads the refusal and the answers under two locks:
// PUB's daemon is admitted many times, and the test fails naming how many.
func TestADecisionNeverMixesTwoRefreshes(t *testing.T) {
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}, {Kind: KindDaemon, Account: otherDaemonAccount}}
	var second atomic.Bool
	access := func(_ context.Context, project, _ string) (projectaccess.Answer, error) {
		switch {
		case second.Load() && project == "OTHER":
			return projectaccess.Answer{}, projectaccess.ErrBrokerRefused
		case second.Load():
			return projectaccess.Answer{Project: project, Visible: true}, nil
		case project == "PUB":
			return projectaccess.Answer{Project: project, Visible: false}, nil
		default:
			return projectaccess.Answer{Project: project, Visible: true}, nil
		}
	}
	deps := Deps{Exchange: exchangeOf(map[string][]string{pubDaemonAccount: {"PUB"}, otherDaemonAccount: {"OTHER"}}), Access: access}
	deps.held = newHeldAccess(deps, services)
	require.NotNil(t, deps.held)
	deps.held.refresh(t.Context())

	stop, done := make(chan struct{}), make(chan struct{})
	var refreshes atomic.Int64
	go func() {
		defer close(done)
		for flip := true; ; flip = !flip {
			select {
			case <-stop:
				return
			default:
			}
			second.Store(flip)
			deps.held.refresh(context.Background())
			refreshes.Add(1)
		}
	}()
	const wantRefreshes, wantDecisions, stuck = 5000, 2000000, 5 * time.Minute
	admitted, decisions := 0, 0
	for start := time.Now(); refreshes.Load() < wantRefreshes || decisions < wantDecisions; decisions++ {
		if _, err := scopeGrant(deps, services[0], natsgrant.Daemon()); err == nil {
			admitted++
		}
		if decisions%4096 == 0 && time.Since(start) > stuck {
			t.Fatalf("stuck: %d decisions and %d refreshes after %s", decisions, refreshes.Load(), stuck)
		}
	}
	close(stop)
	<-done
	t.Logf("%d decisions over %d refreshes, %d admitted", decisions, refreshes.Load(), admitted)
	require.Zero(t, admitted, "PUB's daemon was admitted %d times on a refusal and a PUB answer from two different refreshes", admitted)
}

// run makes one refresh at a time, even when a refresh outlasts accessRefresh many times over, so
// refresh, which is not safe beside itself, never runs beside itself. Each ask here takes 20 ms
// and the service asks again every millisecond. The control starts each refresh on a goroutine of
// its own: many ask about PUB at once, and the test fails naming how many.
func TestRunNeverOverlapsTwoRefreshes(t *testing.T) {
	withAccessRefresh(t, time.Millisecond)
	var inflight, most, calls atomic.Int32
	access := func(ctx context.Context, project, _ string) (projectaccess.Answer, error) {
		n := inflight.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		calls.Add(1)
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
		}
		inflight.Add(-1)
		return projectaccess.Answer{Project: project, Visible: true}, nil
	}
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	h := newHeldAccess(Deps{Exchange: exchangeOf(map[string][]string{pubDaemonAccount: {"PUB"}}), Access: access}, services)
	require.NotNil(t, h)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); h.run(ctx) }()
	testwait.Eventually(t, "ten asks about PUB", func() bool {
		select {
		case <-done:
			t.Fatal("run returned before the context ended")
		default:
		}
		return calls.Load() >= 10
	})
	cancel()
	<-done
	t.Logf("%d asks about PUB, at most %d at once", calls.Load(), most.Load())
	require.EqualValues(t, 1, most.Load(), "%d refreshes asked about PUB at once", most.Load())
}
