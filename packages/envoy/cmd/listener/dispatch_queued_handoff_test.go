package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
)

// The copy Dispatch queues itself for a session no store of the listener's holds
// (internal/dispatch/api's queueForUnknownSession) reaches the daemon of a stopped Legion agent
// through this listener's give-up: the session is gone, the Legion agent registry names it, and
// its project answers. The envelope is the one Dispatch publishes, built by the two
// calls its queuedEnvelope makes (dispatchenvoy.SendInput.EnvelopeMessage, then
// contracts.AgentSendEnvelope) from the input its listenerSendInput builds for a direct message,
// and it is published as Dispatch's NATS client publishes it (bus.Client's publish of an
// envelope on its own topic). The delivery is held for the retry window, here one deferral
// (oneDeferralWindow), and the give-up then hands it off (offerAgentDeliveryToLegion) in place of
// its no_holder notice and its wake: the hand-off on the agent's daemon subject carries the
// queued copy whole, and nothing is given up or woken. So the person's message is answered by the
// daemon about a window after it was queued, rather than waiting for a run that never comes.
func TestDispatchsQueuedCopyForAStoppedLegionAgentIsHandedToItsDaemonAtTheGiveUp(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const target = "ses_dispatch_queued_impl"

	agents, err := legionagents.Open(t.Context(), harness.client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	entry := legionagents.Entry{
		SessionID: target, LegionProject: "infra", Issue: "INFRA-204", Role: "implementer",
		Harness: "omp", StartedAt: "2026-10-26T03:00:00Z",
	}
	entry.RoleToken, entry.Title = legionagents.RoleToken("infra", entry.Issue, entry.Role), "Legion implementer · INFRA-204"
	if _, err := agents.Put(ctx, entry); err != nil {
		t.Fatalf("record the stopped implementer %s: %v", target, err)
	}
	if _, err := agents.PutProject(ctx, "infra", legionagents.Project{
		Answering: true, UpdatedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}); err != nil {
		t.Fatalf("state that the project answers: %v", err)
	}

	// Dispatch's queued copy of a person's steer to the session: the idempotency key is the
	// message's id and its mode, the frame is the attempt's, and a direct message invites a reply.
	const messageID, mode = "0193f2c1-7a7d-7c4e-9c1a-5b1d2e3f4a5b", "steer"
	frame := json.RawMessage(`{"delivery":{"message_id":"` + messageID + `","attempt":1,"mode":"` + mode + `"},"body":"Did the migration land?"}`)
	item := contracts.AgentSendEnvelope(target, messageID+":"+mode, dispatchenvoy.SendInput{
		TargetSession:  target,
		Message:        "Did the migration land?",
		Payload:        frame,
		IdempotencyKey: messageID + ":" + mode,
		ExpectsReply:   contracts.ExpectsReplyOptional,
	}.EnvelopeMessage())
	if err := item.Validate(); err != nil {
		t.Fatalf("Dispatch's queued copy does not validate: %v", err)
	}
	if item.Topic != contracts.LegacyAgentSubject(target) || item.Source != "dispatch" ||
		item.DedupeKey != contracts.AgentSendDedupeKey(target, messageID+":"+mode) {
		t.Fatalf("the queued copy = %#v, want Dispatch's send on %s's own subject under its send's key", item, target)
	}

	cfg := harness.config
	cfg.unplaceableRetryWindow = oneDeferralWindow
	cfg.handoff = &legionHandoff{
		client: cfg.client, registry: cfg.registry, sessions: cfg.sessions,
		agents: newLegionAgentRegistry(agents), logger: logging.NewWithWriter("test", harness.logs),
	}
	lifetime, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	cfg.wakes = newWakeReporter(lifetime, harness.client)
	wakes := subscribeWakes(t, harness.client.Conn)
	run := startEnvelopeDeliveryThrough(t, harness, jetStreamDeliveryHandler(cfg), target, item, 0)

	waitFor(t, "the queued copy to be handed to "+target+"'s daemon", func() bool {
		count, _ := handoffs(t, harness.client, target)
		return count == 1
	})
	_, data := handoffs(t, harness.client, target)
	wire := decodeHandoff(t, data)
	if wire.TargetSession != target || wire.OriginalTopic != item.Topic || wire.Project != "infra" || wire.RoleToken != entry.RoleToken {
		t.Fatalf("the stored hand-off is %+v, want the queued copy's topic handed to %s under the project infra", wire, target)
	}
	var handed contracts.Envelope
	if err := json.Unmarshal(wire.Envelope, &handed); err != nil {
		t.Fatalf("decode the handed-off envelope %s: %v", wire.Envelope, err)
	}
	if handed.EventID != item.EventID || handed.DedupeKey != item.DedupeKey || handed.Source != "dispatch" ||
		handed.PayloadSummary != item.PayloadSummary || handed.Payload != item.Payload {
		t.Fatalf("the handed-off envelope = %#v, want Dispatch's queued copy %#v whole", handed, item)
	}

	logs := harness.logs.String()
	if deferrals := recordsFor(deliveryRecords(t, logs, "listener agent delivery deferred"), target); len(deferrals) != 1 {
		t.Fatalf("%d deferrals of the queued copy, want the one its window buys before the give-up:\n%s", len(deferrals), logs)
	}
	if handedOff := recordsFor(deliveryRecords(t, logs, "listener agent handed off to its legion daemon"), target); len(handedOff) != 1 || handedOff[0].EventID != item.EventID {
		t.Fatalf("want one hand-off line for the queued copy %s, got %#v:\n%s", item.EventID, handedOff, logs)
	}
	if giveUps := recordsFor(deliveryRecords(t, logs, "listener agent delivery given up"), target); len(giveUps) != 0 {
		t.Fatalf("the queued copy was given up as well as handed off:\n%s", logs)
	}
	if notice, err := run.exceptions.NextMsg(time.Second); err == nil {
		t.Fatalf("a delivery exception was published for the queued copy in place of nothing: %s", notice.Data)
	}
	if woken := agentWakesFor(t, wakes, target, time.Second, 500*time.Millisecond); woken != 0 {
		t.Fatalf("%d agent wakes were reported for %s, want none: the daemon answers the copy, and no pod is coming", woken, target)
	}
}
