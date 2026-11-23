package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// countingDepsJudgedBy is countingDeps with the role-route holders judged by windows
// (depsJudgedBy), so a row can judge one holder's record at a clock moved past the live window.
func (r *roleRig) countingDepsJudgedBy(windows roles.Windows) (Deps, *metrics.Registry) {
	registry := metrics.New()
	deps := r.depsJudgedBy(windows)
	deps.PublishFailures = registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
		"Destinations of an outbox publish attempt that were not reached")
	deps.RoleForwardsHeld = registry.NewCounter("envoy_dispatch_outbox_role_forwards_held_total",
		"Role forwards a holder's resolution held pending with nothing sent")
	return deps, registry
}

// probeRelaySubject asks once, over conn, whether anything subscribes sessionID's relay subject
// (bus.DeliverSubject), as a connection probe asks it: a core request carrying
// bus.ConnectionProbeHeader and no body. A subscriber's answer is nil, and nats.ErrNoResponders
// says nothing subscribes the subject.
func probeRelaySubject(conn *natsgo.Conn, sessionID string) error {
	msg := natsgo.NewMsg(bus.DeliverSubject(sessionID))
	msg.Header.Set(bus.ConnectionProbeHeader, "1")
	_, err := conn.RequestMsg(msg, time.Second)
	return err
}

// awaitNoRelayResponder waits until a probe of sessionID's relay subject meets no responder: a
// closed connection leaves its subscriptions for the moment the server reads the close.
func awaitNoRelayResponder(t *testing.T, conn *natsgo.Conn, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := probeRelaySubject(conn, sessionID)
		if errors.Is(err, natsgo.ErrNoResponders) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("something still subscribes %s's relay subject: the last probe answered %v", sessionID, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Dispatch's outbox judges a role route's holder by the age of its last registration alone, as
// the listener's role routes do (roles.ResolveHolder over roles.Buckets): a holder registered in
// consumer mode whose process stopped - its relay subscriber gone, so a probe of its relay subject
// meets no responder - and whose last registration is less than session.LiveWindow (5 minutes)
// old is live, and an event routed to its role is forwarded to the holder's own subject and
// published on the stream's acknowledgement, where the holder's session consumer hands it over
// when it registers again. The outbox probes no relay.
//
// The row past the live window is the control: the same holder and the same record, judged at a
// clock moved past the window, is unresponsive, and the outbox holds its event pending with nothing
// sent and the hold counted under that state. So the forward before it comes from the
// registration's age and from nothing else of the rig.
func TestTheOutboxForwardsToARoleHolderInsideTheLiveWindow(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "window-holder", "ses_window_outbox_holder"
	alice := model.Actor{Kind: "user", ID: "alice"}
	roleTopic, holderSubject := contracts.RoleTopicPrefix+role, contracts.LegacyAgentSubject(holder)

	// The holder's process: its relay subscriber, answering a connection probe as a plugin
	// answers one, and its registration in consumer mode with its claim of the role. Then the
	// process stops as a crash stops it: the subscriber's connection closes and it registers no
	// more, so its record stays as it last wrote it.
	relay := testnats.Connect(t, rig.url)
	if _, err := relay.Subscribe(bus.DeliverSubject(holder), func(msg *natsgo.Msg) {
		_ = msg.Respond([]byte("done"))
	}); err != nil {
		t.Fatalf("subscribe the holder's relay subject: %v", err)
	}
	if err := relay.Flush(); err != nil {
		t.Fatalf("flush the holder's relay subscription: %v", err)
	}
	rig.claim(t, role, holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Delivery: session.DeliveryConsumer})
	if err := probeRelaySubject(rig.conn, holder); err != nil {
		t.Fatalf("a probe of %s's relay subject while its subscriber holds it answered %v, want the subscriber's answer", holder, err)
	}
	relay.Close()
	awaitNoRelayResponder(t, rig.conn, holder)
	record, err := rig.sessions.Refresh(context.Background(), holder)
	if err != nil {
		t.Fatalf("read the holder's record: %v", err)
	}
	if record.Delivery != session.DeliveryConsumer {
		t.Fatalf("the holder's record states delivery %q, want %q: the record a relay probe is asked about", record.Delivery, session.DeliveryConsumer)
	}
	if age := time.Since(time.UnixMilli(record.UpdatedAt)); age >= session.LiveWindow {
		t.Fatalf("the holder's last registration is %s old, want less than %s", age, session.LiveWindow)
	}
	rig.silent(t, roleTopic)

	t.Run("the outbox forwards an event routed to the role to the holder's own subject and publishes it", func(t *testing.T) {
		deps, registry := rig.countingDepsJudgedBy(roles.Windows{})
		event := rig.routedEvent(t, "T-1", role, alice)

		holding(t, deps).scan(context.Background(), unfenced{})

		attempts, failure, destinations := eventState(t, rig.database, event.ID)
		if publishedAt(t, rig.database, event.ID) == nil {
			t.Fatalf("the event routed to %s was not published: attempt_count %d, publish_error %s; want it forwarded to %s, live inside its window",
				role, attempts, publishFailure(failure), holder)
		}
		if attempts != 0 || failure != nil || !strings.Contains(strings.Join(destinations, ","), roleTopic) {
			t.Fatalf("attempt_count %d, publish_error %s, published_destinations %v: want no attempt, no error and the role route recorded",
				attempts, publishFailure(failure), destinations)
		}
		if copies := storedCopies(t, rig.client, holderSubject); copies != 1 {
			t.Fatalf("the stream holds %d messages on %s, want the one forward", copies, holderSubject)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stored, err := rig.client.LastStoredMessage(ctx, holderSubject)
		if err != nil {
			t.Fatalf("read the forward on %s: %v", holderSubject, err)
		}
		var forward contracts.Envelope
		if err := json.Unmarshal(stored.Data, &forward); err != nil {
			t.Fatalf("decode the forward on %s: %v", holderSubject, err)
		}
		if forward.EventID != fmt.Sprintf("dispatch-%d", event.ID) || forward.Topic != roleTopic {
			t.Fatalf("the forward on %s carries event id %q on topic %q, want the role forward of event %d",
				holderSubject, forward.EventID, forward.Topic, event.ID)
		}
		if body := failureReasons(t, registry); strings.Contains(body, "envoy_dispatch_outbox_role_forwards_held_total{") {
			t.Errorf("/metrics body = %q counts a hold, want the forward sent", body)
		}
	})

	t.Run("past the live window the outbox holds the event pending for the holder as unresponsive", func(t *testing.T) {
		past := roles.Windows{Now: func() time.Time { return time.Now().Add(session.LiveWindow + 30*time.Second) }}
		if age := past.Now().Sub(time.UnixMilli(record.UpdatedAt)); age <= session.LiveWindow || age >= session.GoneHorizon {
			t.Fatalf("the holder's last registration is %s old at the moved clock, want more than %s and less than %s", age, session.LiveWindow, session.GoneHorizon)
		}
		deps, registry := rig.countingDepsJudgedBy(past)
		event := rig.routedEvent(t, "T-2", role, alice)
		before := storedCopies(t, rig.client, holderSubject)

		holding(t, deps).scan(context.Background(), unfenced{})

		if published := publishedAt(t, rig.database, event.ID); published != nil {
			t.Fatalf("the event routed to %s was published at %s, want it pending for its unresponsive holder", role, published)
		}
		attempts, failure, destinations := eventState(t, rig.database, event.ID)
		if attempts != 1 || failure == nil || !strings.Contains(*failure, "is unresponsive") ||
			strings.Contains(strings.Join(destinations, ","), roleTopic) {
			t.Fatalf("attempt_count %d, publish_error %s, published_destinations %v: want it backed off naming the unresponsive holder with the role route still pending",
				attempts, publishFailure(failure), destinations)
		}
		if body := failureReasons(t, registry); !strings.Contains(body, `envoy_dispatch_outbox_role_forwards_held_total{holder_state="unresponsive"} 1`) {
			t.Errorf("/metrics body = %q, want the forward counted held under unresponsive", body)
		}
		if copies := storedCopies(t, rig.client, holderSubject); copies != before {
			t.Errorf("the stream holds %d messages on %s after the scan, want the %d before it: nothing is sent to an unresponsive holder", copies, holderSubject, before)
		}
		if claimed, err := rig.registry.RoleHolder(role); err != nil || claimed != holder {
			t.Errorf("the claim after the scan names %q (%v), want the unresponsive holder's kept", claimed, err)
		}
	})
}
