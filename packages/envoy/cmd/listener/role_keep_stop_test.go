package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The listener's stop (stopListener, every step of shutdownListener but its exit) ends the roles'
// handovers and waits for each to return before it drains the role lane and the main client. A
// handover is under way with its forward held: the forward returns only once its context has ended
// and the row has let it go. The row lets it go only once the stop has begun its wait for the
// handovers (roleKeepGuards.awaiting), and then wants the stop to return after the handover has,
// with no handover left running and no warning that one was. Every wait ends only at the test's
// deadline.
//
// The controls are the stop with either step taken out of main.go. With the end taken out, the
// forward's context never ends, the handover never returns, and the stop's wait gives up at its 10 s
// budget with "role handovers still running at shutdown": the row fails at that line. With the
// wait taken out, the stop returns before any wait for the handover begins: the row fails at its
// first check.
func TestTheListenersStopEndsTheRoleHandoversAndWaitsForThem(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	guards := newRoleKeepGuards()
	awaiting := make(chan struct{})
	var began sync.Once
	guards.awaiting = func() { began.Do(func() { close(awaiting) }) }
	cfg.roleKeepDrains = guards
	const (
		role   = "kept-stop-wiring"
		holder = "ses_kept_stop_wiring"
	)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-1")
	if _, err := keepRoleMessage(cfg, role, item); err != nil {
		t.Fatalf("keep the role message; the listener log says why:\n%s", harness.logs.String())
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	forwarding := make(chan struct{})
	proceed := make(chan struct{})
	var entered sync.Once
	var released sync.Once
	release := func() { released.Do(func() { close(proceed) }) }
	defer release()
	cfg.forwardRole = func(ctx context.Context, _ string, _ contracts.Envelope, _ string, _ time.Duration) (func() error, error) {
		return func() error {
			entered.Do(func() { close(forwarding) })
			<-ctx.Done()
			<-proceed
			return ctx.Err()
		}, nil
	}
	laneClient, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the role lane's client: %v", err)
	}
	t.Cleanup(laneClient.Close)

	startKeptRoleDrain(cfg, role)
	select {
	case <-forwarding:
	case <-time.After(testnats.Remaining(t)):
		t.Fatalf("the handover forwarded nothing by the test's deadline; log:\n%s", harness.logs.String())
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stopListener(cfg.logger, listenerParts{
			server:    &http.Server{},
			client:    harness.client,
			lane:      roleLane{client: laneClient, forwards: newRoleForwarder(cfg), stopWatch: func() {}},
			handovers: guards,
		}, listenerLoops{})
	}()

	select {
	case <-awaiting:
	case <-stopped:
		t.Fatalf("the stop returned before it began a wait for the handover, which still runs; log:\n%s", harness.logs.String())
	case <-time.After(testnats.Remaining(t)):
		t.Fatalf("the stop neither waited for the handover nor returned by the test's deadline; log:\n%s", harness.logs.String())
	}
	release()
	select {
	case <-stopped:
	case <-time.After(testnats.Remaining(t)):
		t.Fatalf("the stop had not returned by the test's deadline; log:\n%s", harness.logs.String())
	}
	if logs := harness.logs.String(); strings.Contains(logs, "role handovers still running at shutdown") {
		t.Fatalf("the stop gave up its wait for the handover, which never ended:\n%s", logs)
	}
	guards.mu.Lock()
	running := guards.running
	guards.mu.Unlock()
	if running != 0 {
		t.Fatalf("the stop returned with %d handovers still running", running)
	}
}
