package main

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// sessionDirectGetDenied is the listener's grant with the session bucket's direct gets left out
// and its gets from the stream's leader kept. An agent delivery reads the session's record with a
// direct get (session.SessionRegistry.Refresh), so that read does not answer; the hand-off reads
// the same record from the leader (RefreshFromLeader, a STREAM.MSG.GET), so its read answers that
// no record stands.
func sessionDirectGetDenied() string {
	bucket := "KV_" + bus.SessionBucket
	return listenerGrant("$JS.API.DIRECT.GET."+bucket, "$JS.API.DIRECT.GET."+bucket+".>")
}

// unreadHandoffRig is a delivery harness on a NATS server of its own whose session bucket holds no
// record of sessionID, with the bucket's direct gets denied (sessionDirectGetDenied), and with a
// hand-off over a Legion agent registry in which the project infra answers. named puts sessionID
// in that registry as a stopped implementer of infra. The harness's session cache follows nothing,
// so every delivery reads the record from the bucket.
func unreadHandoffRig(t *testing.T, sessionID string, named bool) (*bus.Client, listenerDeliveryHarness) {
	t.Helper()
	server, uri := testnats.StartConfig(t, listenerGrant(), []string{"-js"}, false)
	uri = asListenerUser(t, uri)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's user: %v", err)
	}
	t.Cleanup(client.Close)
	harness := newListenerDeliveryHarnessOn(t, client)
	harness.sessions.StopWatch()
	agents, err := legionagents.Open(t.Context(), client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	if named {
		putLegionAgent(t, agents, legionAgentEntry(sessionID, "INFRA-204", "implementer"))
	}
	stateAnswering(t, agents, true)
	harness.config.handoff = &legionHandoff{
		client: harness.config.client, registry: harness.config.registry, sessions: harness.config.sessions,
		agents: newLegionAgentRegistry(agents), logger: logging.NewWithWriter("test", harness.logs),
	}
	if err := server.Reload(t, sessionDirectGetDenied()); err != nil {
		t.Fatalf("leave the session bucket's direct gets out of the grant: %v", err)
	}
	return client, harness
}

// A read of the session's record that does not answer is neither a record nor the lack of one, so
// the delivery sends the message back (unreadAgentDelivery), and the stream's last delivery, where
// the read still does not answer, decides the message as one with no record
// (decideNoRecordAgentMessage). That decision offers the hand-off before it reports any wake, as
// the decision after the agent wait does, and the hand-off reads the session's record again from
// the bucket stream's leader before it hands anything off. So a stopped Legion agent the registry
// names, whose project answers and whose record that leader read finds gone, has the message
// handed to its daemon at that last delivery, and no wake is reported. The control is the same
// message to a session the registry does not name: the offer takes nothing, and the last delivery
// reports the wake. Each row sends back every delivery before the last two itself, so the handler
// first sees the one before the stream's last, which the unanswered read sends back for the wait.
func TestTheLastDeliveryAfterAnUnansweredRecordReadOffersTheHandoff(t *testing.T) {
	t.Parallel()
	// deliverLastTwo runs one envelope for sessionID through the harness's durable handler, which
	// takes only the stream's last two deliveries, and waits until it has settled both.
	deliverLastTwo := func(t *testing.T, harness listenerDeliveryHarness, sessionID string) (unplaceableDeliveryRun, uint64) {
		t.Helper()
		run, seen := startAloneDelivery(t, harness, sessionID, func(msg *natsgo.Msg, meta *natsgo.MsgMetadata) bool {
			if meta.NumDelivered < consumerMaxDeliver-1 {
				if err := msg.Nak(); err != nil {
					t.Errorf("send back an early delivery of %s: %v", sessionID, err)
				}
				return false
			}
			return true
		}, nil)
		seen.awaitSeen(t, 2, 45*time.Second)
		count, _, seq := seen.seen()
		if len(count) != 2 || count[0] != consumerMaxDeliver-1 || count[1] != consumerMaxDeliver {
			t.Errorf("the handler saw deliveries %v, want %d and %d", count, consumerMaxDeliver-1, consumerMaxDeliver)
		}
		unread := deliveryRecords(t, harness.logs.String(), "listener agent record unread")
		if len(unread) != 1 || unread[0].Level != "WARN" || unread[0].SessionID != sessionID || unread[0].EventID != run.item.EventID || unread[0].Error == "" {
			t.Errorf("want one WARN that the record of %s was not read, naming event %s and the read's error, got %+v", sessionID, run.item.EventID, unread)
		}
		return run, seq
	}

	t.Run("a stopped Legion agent the registry names is handed to its daemon, with no wake", func(t *testing.T) {
		t.Parallel()
		const sessionID = "ses_unread_last_handoff"
		client, harness := unreadHandoffRig(t, sessionID, true)
		run, seq := deliverLastTwo(t, harness, sessionID)

		logs := harness.logs.String()
		count, data := handoffs(t, client, sessionID)
		if count != 1 {
			t.Fatalf("the stream holds %d hand-offs for %s, want the last delivery's one:\n%s", count, sessionID, logs)
		}
		if wire := decodeHandoff(t, data); wire.TargetSession != sessionID || wire.OriginalTopic != run.item.Topic || wire.Project != "infra" {
			t.Errorf("the stored hand-off is %+v, want the message on %s handed to %s under the project infra", wire, run.item.Topic, sessionID)
		}
		if lines := deliveryRecords(t, logs, "listener agent handed off to its legion daemon"); len(lines) != 1 || lines[0].SessionID != sessionID || lines[0].EventID != run.item.EventID {
			t.Errorf("want one hand-off line naming %s and event %s, got %+v", sessionID, run.item.EventID, lines)
		}
		if got := counterValue(t, metricsBody(t, harness), deliveredSeries("handed_off")); got != 1 {
			t.Errorf("%s = %d, want 1", deliveredSeries("handed_off"), got)
		}
		if got := storedWakeCount(t, client); got != 0 {
			t.Errorf("the stream holds %d wake reports, want none: the message was handed off", got)
		}
		if reports := deliveryRecords(t, logs, "listener agent wake reported"); len(reports) != 0 {
			t.Errorf("want no wake line beside the hand-off, got %+v", reports)
		}
		if message, err := run.exceptions.NextMsg(500 * time.Millisecond); err == nil {
			t.Errorf("a delivery exception was published: %s", message.Data)
		}
		awaitDurableState(t, client, run.consumer, 0, seq)
	})

	t.Run("control: a session the registry does not name is woken", func(t *testing.T) {
		t.Parallel()
		const sessionID = "ses_unread_last_wake"
		client, harness := unreadHandoffRig(t, sessionID, false)
		run, seq := deliverLastTwo(t, harness, sessionID)

		logs := harness.logs.String()
		if got := storedWakeCount(t, client); got != 1 {
			t.Errorf("the stream holds %d wake reports after the last delivery, want one", got)
		}
		if reports := deliveryRecords(t, logs, "listener agent wake reported"); len(reports) != 1 || reports[0].SessionID != sessionID || reports[0].EventID != run.item.EventID {
			t.Errorf("want one wake line naming %s and event %s, got %+v", sessionID, run.item.EventID, reports)
		}
		if count, data := handoffs(t, client, sessionID); count != 0 {
			t.Errorf("the stream holds %d hand-offs for %s, want none: the registry names no such agent; the last: %s", count, sessionID, data)
		}
		if lines := deliveryRecords(t, logs, "listener agent handed off to its legion daemon"); len(lines) != 0 {
			t.Errorf("want no hand-off line, got %+v", lines)
		}
		if message, err := run.exceptions.NextMsg(500 * time.Millisecond); err == nil {
			t.Errorf("a delivery exception was published: %s", message.Data)
		}
		awaitDurableState(t, client, run.consumer, 0, seq)
	})
}
