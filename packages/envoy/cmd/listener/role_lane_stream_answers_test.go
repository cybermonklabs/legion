package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// fullStreamMaxBytes is the byte limit of the small stream these tests fill (bus.WithStreamMaxBytes):
// a stream the listener creates with a limit refuses new messages once full (DiscardNew).
const fullStreamMaxBytes = 64 << 10

// emptyFill purges what bustest.FillStream stored, so the stream has room again.
func emptyFill(t *testing.T, client *bus.Client) {
	t.Helper()
	if err := client.JS().PurgeStream(bus.Stream, &natsgo.StreamPurgeRequest{Subject: "notifications.github.fill.>"}); err != nil {
		t.Fatalf("purge the fill: %v", err)
	}
}

// A role forward that a full stream refuses is held, not ended: the stream answers 10077 "maximum
// bytes exceeded", which the bus names bus.ErrStreamFull, and the same forward is stored once the
// stream has room. The forward is the listener's own (startRoleForward) to a real stream with a
// 64 KiB byte limit. Once the fill is purged, the holder's agent subject holds the one copy, the
// listener logs one forwarded line and no failed one, and no exception is published.
func TestRoleLaneHoldsAForwardAFullStreamRefusesUntilItHasRoom(t *testing.T) {
	harness := newListenerDeliveryHarnessOn(t, setupPublishTestClient(t, bus.WithStreamMaxBytes(fullStreamMaxBytes)))
	const (
		role   = "full-stream"
		holder = "ses_full_stream"
	)
	if err := harness.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim role: %v", err)
	}
	bustest.FillStream(t, harness.client, 1000)
	cfg := wireTestRoleForwarder(t, harness.config)
	handler := coreNATSDeliveryHandler(cfg)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "full-stream")
	item.Payload = `{"type":"worker-queued"}`
	probe, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + item.Topic)
	if err != nil {
		t.Fatalf("subscribe exception probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush exception probe: %v", err)
	}

	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	waitFor(t, 20*time.Second, "the forward a full stream refused to be held", func() bool {
		for _, record := range linesIn(harness.logs.String(), roleHeldLine, map[string]any{"event_id": item.EventID}) {
			if strings.Contains(fmt.Sprint(record["error"]), bus.ErrStreamFull.Error()) {
				return true
			}
		}
		return false
	})
	emptyFill(t, harness.client)
	cfg.roleForwards.settled(t)

	logs := harness.logs.String()
	if countLogged(logs, "listener role forwarded", item.EventID) != 1 || strings.Contains(logs, `"msg":"listener role forward failed"`) {
		t.Fatalf("want one forwarded line and no failed line:\n%s", logs)
	}
	if stored := storedOnSubjects(t, harness.client, contracts.LegacyAgentSubject(holder), holder); stored != 1 {
		t.Fatalf("the holder's agent subject holds %d copies, want 1", stored)
	}
	if message, err := probe.NextMsg(250 * time.Millisecond); !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("a held message published an exception (or the probe failed): %v %v", message, err)
	}
}

// A publish to a role with no live holder whose keep fails on NATS - here a full stream - is
// answered the bus-unavailable 503, which a sender retries, not the 404 that says nothing could be
// kept: a sender that treats 404 as final (the stall teller) would drop a message the next keep
// stores. The controls: the same publish with keep: false is still the 404, and once the stream has
// room the same publish is kept and answered 202.
func TestAPublishWhoseKeepFailsOnNATSIsAnsweredUnavailable(t *testing.T) {
	client := setupPublishTestClient(t, bus.WithStreamMaxBytes(fullStreamMaxBytes))
	registry, sessions := setupSessionsTest(t, nil, nil)
	d := withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions})
	const role = "kept-on-a-full-stream"
	body := func(keep string) string {
		return `{"topic":"` + contracts.RoleTopicPrefix + role + `","source":"agent","message":"wait for the next holder"` + keep + `}`
	}
	bustest.FillStream(t, client, 1000)

	recorder, _ := publishThrough(d, body(""))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("publish to an unheld role whose keep a full stream refused: status %d, Retry-After %q, body %s; want 503 with Retry-After",
			recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), streamFullPrefix) {
		t.Fatalf("the 503's body %q does not name the full stream (%s)", recorder.Body.String(), streamFullPrefix)
	}
	recorder, _ = publishThrough(d, body(`,"keep":false`))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("publish with keep: false on a full stream: status %d, body %s; want the 404 that keeps nothing", recorder.Code, recorder.Body.String())
	}

	emptyFill(t, client)
	recorder, _ = publishThrough(d, body(""))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("publish once the stream has room: status %d, body %s; want 202 kept", recorder.Code, recorder.Body.String())
	}
	var answer keptPublishAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || !answer.Kept {
		t.Fatalf("publish once the stream has room: answer %s (%v), want kept", recorder.Body.String(), err)
	}
}

// On a three-copy stream whose leader keeps changing, a forward to a subject the stream captures is
// sometimes answered no responders (nats.go's own JetStream publish retries that answer for this
// reason). The role lane holds such a forward and sends it again under the same Nats-Msg-Id, so
// every message the lane takes is stored once on the holder's agent subject and none is reported
// delivery_failed. The run goes on until at least three forwards were answered no responders -
// counted off the held lines and the failed lines alike, so a lane that ends them is seen too - a
// minute at most; a run that saw none cannot show the case and fails as such.
func TestRoleLaneHoldsForwardsALeaderChangeAnswersNoResponders(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	client, err := bus.ConnectOwningStream(cluster.URLs(), bus.WithReplicas(3))
	if err != nil {
		t.Fatalf("connect to the cluster: %v", err)
	}
	t.Cleanup(client.Close)
	js := cluster.JetStream(t)
	testnats.AwaitSettledStream(t, js, bus.Stream)
	harness := newListenerDeliveryHarnessOn(t, client)
	const (
		role   = "leader-change"
		holder = "ses_leader_change"
	)
	if err := harness.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim role: %v", err)
	}
	cfg := wireTestRoleForwarder(t, harness.config)
	handler := coreNATSDeliveryHandler(cfg)
	admin := testnats.Connect(t, cluster.URL())
	t.Cleanup(admin.Close)

	var stepdowns atomic.Int32
	stop := make(chan struct{})
	stepped := make(chan struct{})
	go func() {
		defer close(stepped)
		for {
			select {
			case <-stop:
				return
			case <-time.After(700 * time.Millisecond):
			}
			if _, err := admin.Request("$JS.API.STREAM.LEADER.STEPDOWN."+bus.Stream, nil, 5*time.Second); err == nil {
				stepdowns.Add(1)
			}
		}
	}()
	noResponders := func() int {
		count := 0
		logs := harness.logs.String()
		for _, name := range []string{roleHeldLine, "listener role forward failed"} {
			for _, record := range linesIn(logs, name, nil) {
				if strings.Contains(fmt.Sprint(record["error"]), natsgo.ErrNoResponders.Error()) {
					count++
				}
			}
		}
		return count
	}
	sent := 0
	deadline := time.Now().Add(time.Minute)
	for (noResponders() < 3 || sent < 300) && time.Now().Before(deadline) {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("leader-change-%d", sent))
		item.Payload = `{"type":"worker-queued"}`
		handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
		sent++
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	<-stepped
	seen := noResponders()
	if seen == 0 {
		t.Fatalf("UNKNOWN: %d leader changes over %d forwards drew no no-responders answer, so this run cannot show how the lane treats one", stepdowns.Load(), sent)
	}
	testnats.AwaitSettledStream(t, js, bus.Stream)
	cfg.roleForwards.settled(t)

	logs := harness.logs.String()
	if failed := linesIn(logs, "listener role forward failed", nil); len(failed) != 0 {
		t.Fatalf("%d forwards reported failed during the leader changes, want none: %v", len(failed), failed)
	}
	if stored := storedOnSubjects(t, harness.client, contracts.LegacyAgentSubject(holder), holder); stored != uint64(sent) {
		t.Fatalf("the holder's agent subject holds %d copies of %d messages, want one each", stored, sent)
	}
	t.Logf("%d leader changes; %d messages, each stored once; %d forwards answered no responders, each held", stepdowns.Load(), sent, seen)
}

// A role message whose keep fails on NATS is held, not given up: the role nobody claims is kept for
// its next holder, but the stream is full, so the keep fails (bus.ErrStreamFull, which
// keepFailureHolds holds for) and the lane holds the message and runs its delivery again. Once the
// stream has room the next run keeps it, under the reason unclaimed, with no no_holder exception.
func TestRoleLaneHoldsAMessageWhoseKeepFailedOnNATS(t *testing.T) {
	harness := newListenerDeliveryHarnessOn(t, setupPublishTestClient(t, bus.WithStreamMaxBytes(fullStreamMaxBytes)))
	const role = "kept-on-a-full-stream"
	bustest.FillStream(t, harness.client, 1000)
	cfg := wireTestRoleForwarder(t, harness.config)
	handler := coreNATSDeliveryHandler(cfg)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-on-a-full-stream")
	item.Payload = `{"type":"worker-queued"}`
	probe, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + item.Topic)
	if err != nil {
		t.Fatalf("subscribe exception probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush exception probe: %v", err)
	}

	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	waitFor(t, 20*time.Second, "the message whose keep a full stream refused to be held", func() bool {
		for _, record := range linesIn(harness.logs.String(), roleHeldLine, map[string]any{"event_id": item.EventID}) {
			if strings.Contains(fmt.Sprint(record["error"]), bus.ErrStreamFull.Error()) {
				return true
			}
		}
		return false
	})
	emptyFill(t, harness.client)
	cfg.roleForwards.settled(t)

	kept := linesIn(harness.logs.String(), "listener kept role message for the role's next holder", map[string]any{"role": role})
	if len(kept) != 1 || kept[0]["reason"] != string(roleKeptUnclaimed) {
		t.Fatalf("the kept lines for role %s are %v, want one naming %s:\n%s", role, kept, roleKeptUnclaimed, harness.logs.String())
	}
	if strings.Contains(harness.logs.String(), `"msg":"listener role has no holder"`) {
		t.Fatalf("a message whose keep failed on NATS was given up as no_holder:\n%s", harness.logs.String())
	}
	if message, err := probe.NextMsg(250 * time.Millisecond); !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("a held message published an exception (or the probe failed): %v %v", message, err)
	}
}

// A held role message the listener's stop cannot keep either - the stream is full - is the one end
// of a held message that leaves no copy anywhere, and is logged at ERROR with its event id and the
// reason, held_at_shutdown.
func TestARoleMessageHeldAtShutdownThatCannotBeKeptIsLoggedAtError(t *testing.T) {
	harness := newListenerDeliveryHarnessOn(t, setupPublishTestClient(t, bus.WithStreamMaxBytes(fullStreamMaxBytes)))
	const (
		role   = "unkept-at-shutdown"
		holder = "ses_unkept_at_shutdown"
	)
	if err := harness.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim role: %v", err)
	}
	cfg := harness.config
	cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
		return errors.New("nats: connection closed")
	})
	cfg = wireTestRoleForwarder(t, cfg)
	handler := coreNATSDeliveryHandler(cfg)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "unkept-at-shutdown")
	item.Payload = `{"type":"worker-queued"}`

	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	waitFor(t, 20*time.Second, "the message to be held", func() bool {
		return countLogged(harness.logs.String(), roleHeldLine, item.EventID) >= 1
	})
	bustest.FillStream(t, harness.client, 1000)
	cfg.roleForwards.stopHolding()
	cfg.roleForwards.keepHeldNow()
	cfg.roleForwards.settled(t)

	unkept := linesIn(harness.logs.String(), roleHeldUnkeptLine, map[string]any{"event_id": item.EventID})
	if len(unkept) != 1 || unkept[0]["level"] != "ERROR" || unkept[0]["reason"] != string(roleKeptAtShutdown) {
		t.Fatalf("the unkept lines for %s are %v, want one ERROR naming %s:\n%s", item.EventID, unkept, roleKeptAtShutdown, harness.logs.String())
	}
	assertNothingKept(t, harness.client, role)
}

// A keep that a stream refuses for the message itself - a sealed stream, which no attempt gets
// past - ends the message as the same answer ends a forward (bus.StreamRefusesMessage), not as a
// keep that failed on NATS: the role lane gives a message to a role nobody claims up as no_holder,
// with its exception and no hold, and the publish route answers its 404 that keeps nothing, not
// the 503 that has its sender send again.
func TestAKeepASealedStreamRefusesEndsTheMessage(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "kept-on-a-sealed-stream"
	info, err := harness.client.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	config := info.Config
	config.Sealed = true
	if _, err := harness.client.JS().UpdateStream(&config); err != nil {
		t.Fatalf("seal the stream: %v", err)
	}
	cfg := wireTestRoleForwarder(t, harness.config)
	handler := coreNATSDeliveryHandler(cfg)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-on-a-sealed-stream")
	item.Payload = `{"type":"worker-queued"}`
	probe := exceptionProbe(t, harness.client)

	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	assertDeliveryException(t, probe, item, "no_holder")
	cfg.roleForwards.settled(t)
	if held := countLogged(harness.logs.String(), roleHeldLine, item.EventID); held != 0 {
		t.Fatalf("a message whose keep a sealed stream refused was held %d times:\n%s", held, harness.logs.String())
	}

	recorder, _ := publishThrough(harness.publishDeps(t),
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"a sealed stream keeps nothing"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("publish to an unheld role on a sealed stream: status %d, body %s; want the 404 that keeps nothing", recorder.Code, recorder.Body.String())
	}
}

// A forward the stream refuses for the message itself is answered at once, and as a refusal: the
// publish route makes one forward and answers 409 under streamRefusedPrefix, recorded
// stream_refused, with no Retry-After. Retried to the send's window it would answer the 504 that
// a sender re-sends on, so a sealed stream would have every sender re-sending for as long as the
// stream stands. The count of forwards is what holds that: a route that retried the refusal makes
// more than one inside its window, whatever the box's load.
func TestAForwardASealedStreamRefusesIsAnsweredAtOnce(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "forward-on-a-sealed-stream"
		holder = "ses_sealed_forward"
	)
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	deps := harness.publishDeps(t)
	var forwards atomic.Int32
	delivery := *deps.delivery
	delivery.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
		forwards.Add(1)
		return fmt.Errorf("%w: %w", bus.ErrForwardNotStored,
			&natsgo.APIError{Code: 400, ErrorCode: 10109, Description: "invalid operation on sealed stream"})
	})
	deps.delivery = &delivery

	recorder, _ := publishThrough(deps,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"a sealed stream stores nothing"}`)

	if recorder.Code != http.StatusConflict || recorder.Header().Get("Retry-After") != "" {
		t.Fatalf("publish whose forward a sealed stream refused: status %d, Retry-After %q, body %s; want 409 with no Retry-After",
			recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), streamRefusedPrefix) {
		t.Fatalf("the 409's body %q does not name the stream's refusal (%s)", recorder.Body.String(), streamRefusedPrefix)
	}
	if got := forwards.Load(); got != 1 {
		t.Fatalf("the route made %d forwards, want 1: a refusal is not retried", got)
	}
}

// A role message the lane takes once the listener stopped is kept at once, with no holder resolved
// (deliverRoleMessage): a message to a role nobody claims is kept under held_at_shutdown, not under
// the unclaimed a resolution would name.
func TestARoleMessageTakenAfterTheStopIsKeptWithoutAResolution(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "taken-after-the-stop"
	cfg := wireTestRoleForwarder(t, harness.config)
	handler := coreNATSDeliveryHandler(cfg)
	cfg.roleForwards.stopHolding()
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "taken-after-the-stop")
	item.Payload = `{"type":"worker-queued"}`

	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	cfg.roleForwards.settled(t)

	kept := linesIn(harness.logs.String(), "listener kept role message for the role's next holder", map[string]any{"role": role})
	if len(kept) != 1 || kept[0]["reason"] != string(roleKeptAtShutdown) {
		t.Fatalf("the kept lines for role %s are %v, want one naming %s:\n%s", role, kept, roleKeptAtShutdown, harness.logs.String())
	}
}

// A role message the lane takes once the listener stopped, to a role token no claim can name, is
// refused by its keep under a rule (errRoleNotKeepable): it was never held, so it is given up as
// delivery_failed with its exception, not reported as a held message no keep could store.
func TestARoleMessageTakenAfterTheStopThatNoKeepTakesIsDeliveryFailed(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "legion-demo-Unkeepable-architect"
	cfg := wireTestRoleForwarder(t, harness.config)
	handler := coreNATSDeliveryHandler(cfg)
	cfg.roleForwards.stopHolding()
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "unkeepable-after-the-stop")
	item.Payload = `{"type":"worker-queued"}`
	probe := exceptionProbe(t, harness.client)

	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	assertDeliveryException(t, probe, item, "delivery_failed")
	cfg.roleForwards.settled(t)

	logs := harness.logs.String()
	if strings.Contains(logs, roleHeldUnkeptLine) || countLogged(logs, roleKeepRefusedLine, item.EventID) != 1 {
		t.Fatalf("want one %q line and no %q line:\n%s", roleKeepRefusedLine, roleHeldUnkeptLine, logs)
	}
	assertNothingKept(t, harness.client, role)
}
