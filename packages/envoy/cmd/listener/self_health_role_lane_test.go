package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// roleLaneProbePeriod is the self-health period the tests here run the monitor at, in place of
// main's 30 s, so that many periods pass in a few seconds.
const roleLaneProbePeriod = 100 * time.Millisecond

// roleLaneInactiveText is the error /healthz answers, and the self-health probe fails with, while
// the role lane forwards nothing.
const roleLaneInactiveText = "role lane subscription inactive"

// roleLaneHealthRig is a listener's role lane on a NATS server of its own, beside the /healthz of a
// listener whose bus, caches and bound durable are on another server, with the self-health monitor
// running over both at roleLaneProbePeriod. The lane server's one user may subscribe to the role
// lane's subject under granted and not under revoked, and the test changes that grant by a reload
// or a restart. The server traces every protocol line, so the test counts the subscriptions the
// lane sends and the server's refusals of them.
type roleLaneHealthRig struct {
	server           *testnats.Server
	uri, seed        string
	granted, revoked string
	lane             *bus.Client
	received         chan string
	logs             *lockedBuffer
	probes           atomic.Int64
	terminated       atomic.Bool
	get              func() (int, map[string]any)
	stop             func()
}

func newRoleLaneHealthRig(t *testing.T, consumer string) *roleLaneHealthRig {
	t.Helper()
	rig := &roleLaneHealthRig{received: make(chan string, 16), logs: &lockedBuffer{}}
	var public string
	rig.seed, public = testnats.User(t)
	rig.granted = testnats.SubscribeGrantConfig(public, "_INBOX.>", contracts.RoleTopicPrefix+">")
	rig.revoked = testnats.SubscribeGrantConfig(public, "_INBOX.>")
	rig.server, rig.uri = testnats.StartConfig(t, rig.granted, []string{"-DV", "-js"}, false)
	seed := rig.seed
	lane, _, err := bus.ConnectRoleLane([]string{rig.uri}, "role-lane-health", func(msg *natsgo.Msg) { rig.received <- msg.Subject },
		bus.WithEnvironment(func(name string) (string, bool) {
			if name == bus.NkeySeedEnvVar {
				return seed, true
			}
			return "", false
		}))
	if err != nil {
		t.Fatalf("open the role lane: %v", err)
	}
	t.Cleanup(lane.Close)
	rig.lane = lane

	client := setupTestNATS(t)
	deps := openHealthzDeps(t, client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	deps.roleLane = lane
	rig.get = serveHealthz(deps)
	if code, body := rig.get(); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("before any grant changes: /healthz = %d %v, want 200 healthy", code, body)
	}

	// The probe main runs: checkSelfHealth over the caches, the role lane and the durable probe,
	// here with no durable probe, since the durable stays bound throughout.
	probe := func() error { return checkSelfHealth(deps.caches, lane, nil) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSelfHealthMonitor(ctx, logging.NewWithWriter("test", rig.logs),
			func() error {
				err := probe()
				rig.probes.Add(1)
				return err
			},
			func(err error) bool { return isUnrecoverableSelfHealthFailure(err, client, deps.caches) },
			client.Connected,
			nil,
			func() { rig.terminated.Store(true) },
			roleLaneProbePeriod,
			3,
		)
	}()
	var once sync.Once
	rig.stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(rig.stop)
	return rig
}

// failures counts the monitor's `self-health probe failed` lines that name the role lane.
func (r *roleLaneHealthRig) failures() int {
	n := 0
	for line := range strings.SplitSeq(r.logs.String(), "\n") {
		if strings.Contains(line, `"msg":"self-health probe failed"`) && strings.Contains(line, roleLaneInactiveText) {
			n++
		}
	}
	return n
}

// recovered reports whether the monitor logged `self-health recovered`.
func (r *roleLaneHealthRig) recovered() bool {
	return strings.Contains(r.logs.String(), `"msg":"self-health recovered"`)
}

// traceFrom is where the server's log ends now, for sent and refusals to count from.
func (r *roleLaneHealthRig) traceFrom() int { return len(r.server.Log()) }

// sent counts the subscriptions to the role lane's subject the server received after from.
func (r *roleLaneHealthRig) sent(from int) int {
	return strings.Count(r.server.Log()[from:], "<<- [SUB "+contracts.RoleTopicPrefix+"> ")
}

// refusals counts the server's refusals of a subscription to the role lane's subject after from:
// its revocation of one it held, and its refusal of each one it was sent while it holds no grant.
func (r *roleLaneHealthRig) refusals(from int) int {
	return strings.Count(r.server.Log()[from:], `->> [-ERR Permissions Violation for Subscription to "`+contracts.RoleTopicPrefix+`>"`)
}

// revoke reloads the server without the user's grant on the role lane's subject: the server keeps
// the lane's connection and revokes its subscription.
func (r *roleLaneHealthRig) revoke(t *testing.T) {
	t.Helper()
	if err := r.server.Reload(t, r.revoked); err != nil {
		t.Fatalf("reload without the role lane's grant: %v", err)
	}
}

// requireFailing waits until the probe has failed on the role lane at least twice, so across at
// least two periods, and /healthz answers 503 for it.
func (r *roleLaneHealthRig) requireFailing(t *testing.T) {
	t.Helper()
	waitFor(t, 30*time.Second, "the self-health probe to fail on the role lane twice", func() bool { return r.failures() >= 2 })
	var code int
	var body map[string]any
	waitFor(t, 30*time.Second, "/healthz to answer 503 for the role lane", func() bool {
		code, body = r.get()
		return code == http.StatusServiceUnavailable && body["error"] == roleLaneInactiveText
	})
}

// requireStopped waits for the monitor's `self-health recovered`, then for five more probes, and
// fails when any of them failed on the role lane or /healthz does not answer 200 healthy.
func (r *roleLaneHealthRig) requireStopped(t *testing.T) {
	t.Helper()
	waitFor(t, 30*time.Second, "the monitor to log self-health recovered", r.recovered)
	failed, probed := r.failures(), r.probes.Load()
	waitFor(t, 30*time.Second, "five more probes", func() bool { return r.probes.Load() >= probed+5 })
	if after := r.failures(); after != failed {
		t.Fatalf("the probe failed on the role lane %d more times after it recovered; the monitor logged:\n%s", after-failed, r.logs.String())
	}
	if code, body := r.get(); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("once the role lane is back: /healthz = %d %v, want 200 healthy", code, body)
	}
}

// requireDelivers publishes a role message as the lane's user and waits for the lane to receive
// it, so a lane read as healthy is one the server delivers to.
func (r *roleLaneHealthRig) requireDelivers(t *testing.T) {
	t.Helper()
	conn, err := testnats.ConnectSeed(t, r.uri, r.seed)
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	defer conn.Close()
	topic := contracts.RoleTopicPrefix + "health-probe"
	if err := conn.Publish(topic, []byte("{}")); err != nil {
		t.Fatalf("publish %s: %v", topic, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush %s: %v", topic, err)
	}
	select {
	case got := <-r.received:
		if got != topic {
			t.Fatalf("the role lane received %s, want %s", got, topic)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the role lane received nothing on %s", topic)
	}
}

// When the hub takes the role lane's grant away, the server reports the refusal once and nothing
// retried it. /healthz then answered 503 for as long as the refusal lasted, but the self-health
// probe did not read the role lane, so `self-health probe failed`, the line the deployment's alert
// matches, was never logged. The probe now fails on the lane every period while it is refused, and
// stops once the lane is back. The hub changes a grant by a reload, which keeps the connection, so
// the probe sends the refused subscription again each period: a reload that gives the grant back
// brings the lane back with no reconnect. A reconnect, which sends every subscription again, brings
// it back too.
func TestTheSelfHealthProbeFailsWhileTheRoleLaneIsRefusedAndStopsOnceItIsBack(t *testing.T) {
	t.Run("a reload gives the grant back", func(t *testing.T) {
		rig := newRoleLaneHealthRig(t, "role-lane-health-reload")
		rig.revoke(t)
		rig.requireFailing(t)

		if err := rig.server.Reload(t, rig.granted); err != nil {
			t.Fatalf("reload with the role lane's grant: %v", err)
		}
		rig.requireStopped(t)
		rig.requireDelivers(t)
		if reconnects := rig.lane.Conn.Stats().Reconnects; reconnects != 0 {
			t.Fatalf("the role lane reconnected %d times; this way out must need no reconnect", reconnects)
		}
		if rig.terminated.Load() {
			t.Fatal("the monitor terminated the listener over the role lane")
		}
	})

	t.Run("a reconnect", func(t *testing.T) {
		rig := newRoleLaneHealthRig(t, "role-lane-health-reconnect")
		rig.revoke(t)
		rig.requireFailing(t)

		// A restart onto the grant drops every connection: the lane reconnects and nats.go sends
		// its subscription again.
		rig.server.Restart(t, rig.granted)
		rig.requireStopped(t)
		rig.requireDelivers(t)
		if reconnects := rig.lane.Conn.Stats().Reconnects; reconnects == 0 {
			t.Fatal("the role lane never reconnected; this row proves nothing about a reconnect")
		}
		if rig.terminated.Load() {
			t.Fatal("the monitor terminated the listener over the role lane")
		}
	})
}

// While the grant stays missing, each probe sends the refused subscription once and no more: the
// lane sends one SUB per failed probe, the server refuses each one, and nothing else sends one.
func TestTheSelfHealthProbeSendsARefusedRoleLaneAgainOncePerPeriod(t *testing.T) {
	rig := newRoleLaneHealthRig(t, "role-lane-health-missing")
	from := rig.traceFrom()
	rig.revoke(t)
	rig.requireFailing(t)
	waitFor(t, 30*time.Second, "the self-health probe to fail on the role lane four times", func() bool { return rig.failures() >= 4 })
	rig.stop()
	// The flush returns once the server has handled every protocol line the lane sent before it,
	// so the trace holds every SUB the probes sent.
	if err := rig.lane.Conn.Flush(); err != nil {
		t.Fatalf("flush the role lane: %v", err)
	}

	failures, probes, sent, refusals := rig.failures(), int(rig.probes.Load()), rig.sent(from), rig.refusals(from)
	if sent != failures || sent > probes {
		t.Fatalf("the role lane sent its subscription %d times over %d probes, %d of them failed on it; want one SUB per failed probe", sent, probes, failures)
	}
	// One refusal is the reload's revocation, and each other one answers a SUB.
	if refusals != sent+1 {
		t.Fatalf("the server refused the role lane's subscription %d times for %d SUBs; want the revocation and one refusal per SUB", refusals, sent)
	}
	if code, body := rig.get(); code != http.StatusServiceUnavailable || body["error"] != roleLaneInactiveText {
		t.Fatalf("with the grant still missing: /healthz = %d %v, want 503 %q", code, body, roleLaneInactiveText)
	}
	if rig.recovered() {
		t.Fatalf("the monitor logged self-health recovered while the grant was missing:\n%s", rig.logs.String())
	}
	if rig.terminated.Load() {
		t.Fatal("the monitor terminated the listener over the role lane")
	}
}

// A role lane that stays granted fails no probe, and the probe sends it no subscription.
func TestTheSelfHealthProbeLogsNothingForAHealthyRoleLane(t *testing.T) {
	rig := newRoleLaneHealthRig(t, "role-lane-health-control")
	from := rig.traceFrom()
	waitFor(t, 30*time.Second, "ten probes", func() bool { return rig.probes.Load() >= 10 })
	rig.stop()
	if err := rig.lane.Conn.Flush(); err != nil {
		t.Fatalf("flush the role lane: %v", err)
	}
	if strings.Contains(rig.logs.String(), "self-health probe failed") {
		t.Fatalf("a healthy role lane failed the probe; the monitor logged:\n%s", rig.logs.String())
	}
	if sent := rig.sent(from); sent != 0 {
		t.Fatalf("the probe sent a healthy role lane's subscription %d times, want 0", sent)
	}
	if code, body := rig.get(); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("with the role lane healthy: /healthz = %d %v, want 200 healthy", code, body)
	}
	rig.requireDelivers(t)
}
