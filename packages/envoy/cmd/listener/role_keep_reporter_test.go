package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A kept copy the role's holder refuses for its delivery mode is reported - its line, its count and
// its delivery_failed exception - before the copies that follow it on the lane are forwarded: the
// walk owes the report the moment its cursor write lands, and the handover's reporter runs it at
// once, off the guard (keptRoleOwed), while the walk goes on. The row holds the forward of the next
// copy until the refusal's exception has gone out, bounded only by the test's deadline, and then
// reads the order of the lines.
//
// The control is the handover at 7eb4320b67, which ran its reports only once the whole handover had
// ended: the exception does not go out while the next forward waits for it, and the row fails at
// its check of that forward.
func TestAKeptCopysRefusalIsReportedBeforeTheForwardsThatFollowIt(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	const (
		role   = "kept-report-order"
		holder = "ses_kept_report_order"
	)
	refused := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-refused")
	refused.Payload = `{"delivery":{"mode":"steer"}}`
	for _, item := range []contracts.Envelope{
		refused,
		listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-1"),
		listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-2"),
		listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-3"),
	} {
		if _, err := keepRoleMessage(cfg, role, item); err != nil {
			t.Fatalf("keep %s; the listener log says why:\n%s", item.DedupeKey, harness.logs.String())
		}
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}})
	exceptions, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + role)
	if err != nil {
		t.Fatalf("subscribe to the role's exceptions: %v", err)
	}
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}
	var (
		first     sync.Once
		reported  atomic.Bool
		forwarded atomic.Int32
	)
	cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
		first.Do(func() {
			if _, err := exceptions.NextMsg(testnats.Remaining(t)); err == nil {
				reported.Store(true)
			}
		})
		forwarded.Add(1)
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	err = drainKeptRole(ctx, cfg, role, time.Now)

	// The order is read first: a handover that held the report while its forward waited for it
	// runs out its walk's window there, and its answer says only that.
	if !reported.Load() {
		t.Fatalf("the refused copy's exception had not gone out when the next copy was forwarded, by the test's deadline (the handover answered %v); log:\n%s", err, harness.logs.String())
	}
	if err != nil {
		t.Fatalf("the handover answered %v", err)
	}
	logs := harness.logs.String()
	refusal := strings.Index(logs, keptRoleRefusalLines.modeUnadvertised)
	forward := strings.Index(logs, "listener role forwarded")
	if n := forwarded.Load(); refusal < 0 || forward < 0 || refusal > forward || n != 3 {
		t.Fatalf("the refusal's line is at %d and the first forward's at %d, with %d copies forwarded; want the refusal first and 3 forwarded:\n%s", refusal, forward, n, logs)
	}
}

// A shutdown made while a handover is under way ends the handover and waits for it to return, with
// what it owes reported: the walk's forward ends with the shutdown's context (roleKeepGuards.end),
// and the stop's wait (roleKeepGuards.await) returns only once the handover has, its reporter having
// run the refused copy's report. The forward that the shutdown ends is held until then, bounded
// only by the test's deadline.
//
// The control is the handover at 7eb4320b67, started under context.Background with no shutdown step
// that ends it or waits for it (the row's end and await stand in for none): the refused copy's report
// waits for the end of the whole handover, which the held forward never reaches, and the row fails
// at its read of the exception. The second control keeps the end and drops the wait: the stop
// returns while the handover still runs, and the row fails at its check that it returned.
func TestAShutdownMidHandoverEndsItAndKeepsItsReports(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	guards := newRoleKeepGuards()
	cfg.roleKeepDrains = guards
	const (
		role   = "kept-shutdown-mid-handover"
		holder = "ses_kept_shutdown_mid_handover"
	)
	refused := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-refused")
	refused.Payload = `{"delivery":{"mode":"steer"}}`
	for _, item := range []contracts.Envelope{refused, listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-1")} {
		if _, err := keepRoleMessage(cfg, role, item); err != nil {
			t.Fatalf("keep %s; the listener log says why:\n%s", item.DedupeKey, harness.logs.String())
		}
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}})
	exceptions, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + role)
	if err != nil {
		t.Fatalf("subscribe to the role's exceptions: %v", err)
	}
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}
	forwarding := make(chan struct{})
	var entered sync.Once
	cfg.forwardRole = func(ctx context.Context, _ string, _ contracts.Envelope, _ string, _ time.Duration) (func() error, error) {
		return func() error {
			entered.Do(func() { close(forwarding) })
			<-ctx.Done()
			return ctx.Err()
		}, nil
	}

	startKeptRoleDrain(cfg, role)
	select {
	case <-forwarding:
	case <-time.After(testnats.Remaining(t)):
		t.Fatalf("the handover forwarded nothing by the test's deadline; log:\n%s", harness.logs.String())
	}
	guards.end()
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	if !guards.await(ctx) {
		t.Fatalf("the handover had not returned by the test's deadline after the shutdown ended it; log:\n%s", harness.logs.String())
	}
	if _, err := exceptions.NextMsg(testnats.Remaining(t)); err != nil {
		t.Fatalf("no delivery exception for the refused copy by the test's deadline (%v); log:\n%s", err, harness.logs.String())
	}
	guards.mu.Lock()
	running := guards.running
	guards.mu.Unlock()
	if running != 0 {
		t.Fatalf("the stop's wait returned with %d handovers still running", running)
	}
}
