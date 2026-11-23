package natscallout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// withLiveness runs the rest of the test with the heartbeat's tick and staleness bound set short,
// so a stalled loop reads stale in well under a second.
func withLiveness(t *testing.T, tick, bound time.Duration) {
	t.Helper()
	wasTick, wasBound := livenessTick, livenessBound
	livenessTick, livenessBound = tick, bound
	t.Cleanup(func() { livenessTick, livenessBound = wasTick, wasBound })
}

// livenessStatus is the status GET /livez answers for service, nil a broker with none.
func livenessStatus(service *Service) int {
	recorder := httptest.NewRecorder()
	Livez(func() *Service { return service }).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/livez", nil))
	return recorder.Code
}

// A broker that runs no auth service answers /livez 200: nothing of its can stall.
func TestLivezIsLiveWithNoService(t *testing.T) {
	require.Equal(t, http.StatusOK, livenessStatus(nil))
}

// /livez answers 200 while the callout loop is turning, idle included, and 503 once it stops:
// here every decision slot is held by a decision that never returns, so no request can be
// decided, and the heartbeat, which beats only when it can take a slot, goes stale. The control
// is the same service idle, which stays live for as long as the stalled one takes to go stale.
func TestTheLivenessHandlerFailsWhileTheCalloutLoopIsStalled(t *testing.T) {
	withLiveness(t, 20*time.Millisecond, 200*time.Millisecond)
	withDecisionSlots(t, 1)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)

	idle := startService(t, url, keys, Deps{}, Decide)
	time.Sleep(3 * livenessBound)
	require.Equal(t, http.StatusOK, livenessStatus(idle), "an idle service read as stalled")

	release := make(chan struct{})
	stuck := func(context.Context, Deps, []config.NATSService, Request) (Decision, error) {
		<-release
		return Decision{}, nil
	}
	stalled := startService(t, url, keys, Deps{}, stuck)
	// Registered after startService's own Close, so it runs first: Close waits for the decision
	// it holds, which ends only here.
	t.Cleanup(func() { close(release) })
	idle.Close()
	a := newSession(t, "ses_stuck", 15*time.Minute)
	go func() { _, _ = connectSession(t, url, a, nil) }()
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the stalled service's liveness to fail", func() bool {
		return livenessStatus(stalled) == http.StatusServiceUnavailable
	})
}

// Close ends the heartbeat before it returns, so nothing of a closed replica runs on after it: a
// heartbeat that Close only signalled is still running, or ended unseen, when the next test starts.
func TestCloseEndsTheHeartbeatBeforeItReturns(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	service := startService(t, url, keys, Deps{}, Decide)
	service.Close()
	select {
	case <-service.beaten:
	default:
		t.Fatal("Close returned with the heartbeat still running")
	}
}

// A running replica reads the liveness settings and the two bounds as it starts and never again
// (start), so a test that changes or restores them while a replica's heartbeat ticks, its delivery
// goroutine delivers and its liveness is read races with nothing. Here they are written while all
// three run. Under -race a replica that read the package's variables from those goroutines fails
// this test with a data race; a plain build cannot see one.
func TestARunningReplicaReadsNoSettingATestChanges(t *testing.T) {
	withLiveness(t, 10*time.Millisecond, time.Second)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	a := newSession(t, "ses_a", 15*time.Minute)
	service := startService(t, url, keys, Deps{Introspect: introspector(a)}, Decide)
	time.Sleep(50 * time.Millisecond)
	withLiveness(t, 20*time.Millisecond, 2*time.Second)
	withWaitingBound(t, 5)
	withRefusingBound(t, 5)
	nc, err := connectSession(t, url, a, nil)
	require.NoError(t, err, "the session was not admitted")
	nc.Close()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, http.StatusOK, livenessStatus(service))
}
