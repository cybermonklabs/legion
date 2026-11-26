package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// windowRig is a relayRig - the listener's /v1 routes, its session consumers relayed to the
// stand-ins of the sessions it serves as main relays them - whose session registry judges every
// record on the compiled windows (session.LiveWindow, session.GoneHorizon) at a clock the test
// moves (stateClock), with the publish route's confirmation record and the role lane's own
// connection as main opens them (bus.ConnectRoleLane over the harness's role-lane handler), so a
// core publish on a role topic reaches the lane as it does in production.
//
// The session bucket keeps a record for windowBucketAge, past the gone horizon, which is the
// deployed shape (ENVOY_SESSION_MAX_AGE past the gone horizon) in which a holder past the live
// window still has a record to be read as unresponsive. Under the bucket's own default age
// (session.BucketMaxAge, five minutes, the live window itself) the record has expired once the
// live window has passed, and the same holder reads gone.
type windowRig struct {
	*relayRig
	clock *stateClock
}

// windowBucketAge is how long a windowRig's session bucket keeps a record: past the compiled gone
// horizon.
const windowBucketAge = 2 * session.GoneHorizon

func newWindowRig(t *testing.T) *windowRig {
	t.Helper()
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	clock := &stateClock{}
	consumers := newSessionConsumerRigOn(t, client, url, session.WithSessionTTL(windowBucketAge), session.WithSessionClock(clock.Now))
	relays, err := startSessionRelays(consumers.deps.consumers, consumers.harness.config)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		relays.stop(ctx)
	})
	// Set before the first relay starts, which gaining the binding is what does.
	relays.pauseFirst, relays.pauseMax, relays.receiptWait = relayPauseFirst, relayPauseMax, relayReceiptWait
	relays.setOwned(true)
	// The routes read the deps they were built over at each request, so the record set here is
	// the one the publish route answers a role send from.
	consumers.deps.confirmed = newConfirmedRecord(t)
	lane, _, err := bus.ConnectRoleLane([]string{url}, "test-machine", consumers.harness.coreHandler)
	if err != nil {
		t.Fatalf("open the role lane: %v", err)
	}
	t.Cleanup(lane.Close)
	return &windowRig{relayRig: &relayRig{sessionConsumerRig: consumers, relays: relays, url: url}, clock: clock}
}

// windowRoleAnswer is what a row reads of the role lookup (GET /v1/roles/<role>) and of a role
// publish (POST /v1/messages/publish): the status, the raw body, and the fields both answers name
// the role's holder by.
type windowRoleAnswer struct {
	status int
	raw    string

	Reason        string `json:"reason"`
	Holder        string `json:"holder"`
	HolderState   string `json:"holder_state"`
	LastSeen      *int64 `json:"last_seen"`
	ClaimReleased *bool  `json:"claim_released"`
	EventID       string `json:"event_id"`
	DedupeKey     string `json:"dedupe_key"`
	Kept          bool   `json:"kept"`
	KeptReason    string `json:"kept_reason"`
}

// serve serves request on the rig's own /v1 routes and decodes the answer.
func (rig *windowRig) serve(t *testing.T, request *http.Request) windowRoleAnswer {
	t.Helper()
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, request)
	answer := windowRoleAnswer{status: recorder.Code, raw: recorder.Body.String()}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", answer.raw, err)
	}
	return answer
}

// lookupRole is GET /v1/roles/<role>.
func (rig *windowRig) lookupRole(t *testing.T, role string) windowRoleAnswer {
	t.Helper()
	return rig.serve(t, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
}

// publishToRole publishes message to role's topic through the publish route, from no sender
// session.
func (rig *windowRig) publishToRole(t *testing.T, role, message string) windowRoleAnswer {
	t.Helper()
	body, err := json.Marshal(roleMessage(role, "", message))
	if err != nil {
		t.Fatal(err)
	}
	return rig.serve(t, postJSON("/v1/messages/publish", string(body)))
}

// claimRole has sessionID claim role outright through POST /v1/roles/set, which starts the
// handover of the role's kept lane and the takeover passes a granted claim starts
// (runTakenRoleDrain).
func (rig *windowRig) claimRole(t *testing.T, sessionID, role string) {
	t.Helper()
	grantRoleClaim(t, rig.mux, sessionID, role, "", false)
}

// heldForSession is every message the stream holds on sessionID's own subject
// (notifications.agent.<session id>), in stream order: what the session's consumer hands it.
func (rig *windowRig) heldForSession(t *testing.T, sessionID string) []contracts.Envelope {
	t.Helper()
	stored := storedEnvelopes(t, rig.harness.client, contracts.LegacyAgentSubject(sessionID))
	held := make([]contracts.Envelope, len(stored))
	for i, message := range stored {
		held[i] = message.item
	}
	return held
}

// heldEventIDs is the event id of each message in held.
func heldEventIDs(held []contracts.Envelope) []string {
	ids := make([]string, len(held))
	for i, item := range held {
		ids[i] = item.EventID
	}
	return ids
}

// awaitHeldForSession waits until the stream holds a message of eventID on sessionID's own subject
// and returns it, and fails the row when it holds none within 10 s: a forward off the route's own
// goroutine (the role lane's forwarder, a claim's handover) is stored after the call that started
// it has returned.
func (rig *windowRig) awaitHeldForSession(t *testing.T, sessionID, eventID, what string) contracts.Envelope {
	t.Helper()
	deadline := time.Now().Add(listenerWaitBound(t))
	for {
		held := rig.heldForSession(t, sessionID)
		if index := slices.IndexFunc(held, func(item contracts.Envelope) bool { return item.EventID == eventID }); index >= 0 {
			return held[index]
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the stream holds %v on %s's own subject in the time it was given, want event %s among them",
				what, heldEventIDs(held), sessionID, eventID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A role's holder is judged by the age of its last registration alone, on every role route: a
// holder whose process stopped - its relay subscriber gone, so a probe of its relay subject meets
// no responder - and whose last registration is less than session.LiveWindow (5 minutes) old is
// live to the role lookup, the role publish, the role lane and the kept lane's drain, and each of
// them stores the message on the holder's own subject, where its session consumer hands it to the
// holder when it registers again. Once that registration is more than 5 minutes old the same
// holder is unresponsive: the lookup answers 404 holder_lapsed, and a publish is kept for the
// role's next live holder and answered 202. No role route probes the holder's relay.
//
// The holder registers in consumer mode through the listener's own route, as every Oh My Pi and
// Claude Code session does (session.DeliveryConsumer), so its record is the one a relay probe
// would be asked about. Its relay subscriber stops before it claims the role and is shown gone
// (awaitNoRelaySubscriber) before any row runs.
//
// The rows past the live window are the control: the same holder, the same record and the same
// routes, with the registry's clock moved past the window, read the holder unresponsive. So the
// live answers before them come from the registration's age and from nothing else of the rig.
func TestARoleHolderInsideTheLiveWindowReadsLiveOnEveryRoleRoute(t *testing.T) {
	t.Parallel()
	rig := newWindowRig(t)
	const (
		holder = "ses_window_holder"
		role   = "window-holder"
	)

	// A copy kept on the role's kept lane while no session holds the role, for the drain's row.
	kept := rig.publishToRole(t, role, "kept while nobody holds the role")
	if kept.status != http.StatusAccepted || !kept.Kept || kept.KeptReason != "unclaimed" {
		t.Fatalf("publish to the unclaimed role: status %d, body %s; want 202 kept unclaimed", kept.status, kept.raw)
	}
	waitForKeptRoleMessage(t, rig.harness.client, role, kept.EventID)

	// The holder's process: its relay subscriber, and its registration in consumer mode through
	// the listener's own route. Then the process stops as a crash stops it: its relay
	// subscriber's connection closes and it registers no more, so its record stays as it last
	// wrote it.
	standIn := startSessionStandIn(t, rig.url, holder, answerDone)
	rig.register(t, holder, nil)
	standIn.close()
	awaitNoRelaySubscriber(t, rig.url, holder)
	// The claim lands after the subscriber stopped. It renews the holder's record, as the claim
	// route refreshes its claimant's registration, and starts the handover of the role's kept
	// lane (runKeptRoleDrain), which the first row reads.
	rig.claimRole(t, holder, role)
	entry := rig.entry(t, holder)
	if entry.Delivery != session.DeliveryConsumer {
		t.Fatalf("the holder's record states delivery %q, want %q: the record a relay probe is asked about", entry.Delivery, session.DeliveryConsumer)
	}
	if age := rig.clock.Now().Sub(time.UnixMilli(entry.UpdatedAt)); age >= session.LiveWindow {
		t.Fatalf("the holder's last registration is %s old before the rows inside the live window, want less than %s", age, session.LiveWindow)
	}

	t.Run("the kept lane's drain hands the holder the copy kept while the role had no live holder", func(t *testing.T) {
		forwarded := rig.awaitHeldForSession(t, holder, kept.EventID, "the claim's handover of the kept lane")
		if forwarded.KeptAt == nil || *forwarded.KeptAt <= 0 {
			t.Errorf("the copy on %s's own subject carries kept_at %v, want the time the kept lane stored it", holder, forwarded.KeptAt)
		}
		if forwarded.DedupeKey != roleForwardDedupePrefix+kept.DedupeKey {
			t.Errorf("the copy on %s's own subject carries dedupe key %q, want the role forward's %q", holder, forwarded.DedupeKey, roleForwardDedupePrefix+kept.DedupeKey)
		}
	})

	t.Run("the role lookup answers 200 naming the holder live", func(t *testing.T) {
		lookup := rig.lookupRole(t, role)
		if lookup.status != http.StatusOK || lookup.Holder != holder || lookup.HolderState != string(session.Live) {
			t.Fatalf("GET /v1/roles/%s: status %d, body %s; want 200 naming %s live", role, lookup.status, lookup.raw, holder)
		}
		if lookup.LastSeen == nil || *lookup.LastSeen != entry.UpdatedAt {
			t.Errorf("GET /v1/roles/%s: body %s; want last_seen the holder's last registration %d", role, lookup.raw, entry.UpdatedAt)
		}
	})

	t.Run("the role publish answers 200 naming the holder live and stores the message on the holder's own subject", func(t *testing.T) {
		publish := rig.publishToRole(t, role, "are you there?")
		if publish.status != http.StatusOK || publish.Holder != holder || publish.HolderState != string(session.Live) || publish.Kept {
			t.Fatalf("publish to %s: status %d, body %s; want 200 stored for %s live", role, publish.status, publish.raw, holder)
		}
		// The route answers once the stream stored the forward, so the copy is there now.
		held := rig.heldForSession(t, holder)
		if !slices.Contains(heldEventIDs(held), publish.EventID) {
			t.Fatalf("the stream holds %v on %s's own subject after the publish answered, want event %s among them",
				heldEventIDs(held), holder, publish.EventID)
		}
		if newest, ok := readKeptRoleMessage(t, rig.harness.client, role); ok && newest.EventID == publish.EventID {
			t.Errorf("role %s's kept lane holds the published event %s, want it stored for the live holder alone", role, publish.EventID)
		}
	})

	t.Run("the role lane forwards a core publish on the role topic to the holder's own subject", func(t *testing.T) {
		publisher := testnats.Connect(t, rig.url)
		t.Cleanup(publisher.Close)
		item := publishRoleMessage(t, publisher, role, "on the role lane")
		if err := publisher.Flush(); err != nil {
			t.Fatalf("flush the core publish on %s: %v", item.Topic, err)
		}
		forwarded := rig.awaitHeldForSession(t, holder, item.EventID, "the role lane's forward")
		if forwarded.Topic != contracts.RoleTopicPrefix+role {
			t.Errorf("the forward on %s's own subject is on topic %q, want the role topic", holder, forwarded.Topic)
		}
		if newest, ok := readKeptRoleMessage(t, rig.harness.client, role); ok && newest.EventID == item.EventID {
			t.Errorf("role %s's kept lane holds the lane's event %s, want it forwarded to the live holder alone", role, item.EventID)
		}
	})

	// Past the live window from here on: the registry judges the same record at a clock moved
	// past session.LiveWindow, and well inside session.GoneHorizon.
	rig.clock.advance(session.LiveWindow + 30*time.Second)
	if age := rig.clock.Now().Sub(time.UnixMilli(entry.UpdatedAt)); age <= session.LiveWindow || age >= session.GoneHorizon {
		t.Fatalf("the holder's last registration is %s old for the rows past the live window, want more than %s and less than %s", age, session.LiveWindow, session.GoneHorizon)
	}

	t.Run("past the live window the role lookup answers 404 holder_lapsed naming the holder unresponsive", func(t *testing.T) {
		lookup := rig.lookupRole(t, role)
		if lookup.status != http.StatusNotFound || lookup.Reason != "holder_lapsed" || lookup.Holder != holder ||
			lookup.HolderState != string(session.Unresponsive) {
			t.Fatalf("GET /v1/roles/%s: status %d, body %s; want 404 holder_lapsed naming %s unresponsive", role, lookup.status, lookup.raw, holder)
		}
		if lookup.ClaimReleased == nil || *lookup.ClaimReleased {
			t.Errorf("GET /v1/roles/%s answered claim_released %v, want false", role, lookup.ClaimReleased)
		}
		if lookup.LastSeen == nil || *lookup.LastSeen != entry.UpdatedAt {
			t.Errorf("GET /v1/roles/%s: body %s; want last_seen the holder's last registration %d", role, lookup.raw, entry.UpdatedAt)
		}
	})

	t.Run("past the live window a role publish is kept for the role and answered 202", func(t *testing.T) {
		publish := rig.publishToRole(t, role, "are you still there?")
		if publish.status != http.StatusAccepted || !publish.Kept || publish.KeptReason != "holder_lapsed" ||
			publish.Holder != holder || publish.HolderState != string(session.Unresponsive) {
			t.Fatalf("publish to %s: status %d, body %s; want 202 kept holder_lapsed naming %s unresponsive", role, publish.status, publish.raw, holder)
		}
		waitForKeptRoleMessage(t, rig.harness.client, role, publish.EventID)
		if held := rig.heldForSession(t, holder); slices.Contains(heldEventIDs(held), publish.EventID) {
			t.Errorf("the stream holds the kept event %s on %s's own subject, want it on the role's kept lane alone", publish.EventID, holder)
		}
	})
}
