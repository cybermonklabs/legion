package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// The controller role and its notice lane every row of this file publishes on, and the session
// each publish names as its source. The project is spelled as the claim route takes a role token
// (roles.Valid), so a takeover can go through that route.
const (
	lostLaneRole   = "legion-pub-controller"
	lostLane       = "notifications.legion.pub.controller"
	lostLaneSender = "ses_lost_lane_sender"
)

// newLostLaneRig is newRelayRig with the known-session store the publish route counts its
// recipients from, as main wires the route with every store open.
func newLostLaneRig(t *testing.T) *relayRig {
	t.Helper()
	rig := newRelayRig(t)
	rig.deps.known = openKnownSessionsOn(t, rig.harness.client.Conn, time.Now)
	rig.deps.confirmed = newConfirmedRecord(t)
	waitForRegistry(t, rig.harness.registry)
	return rig
}

// storeRawRow writes sessionID's interest row with topics added to whatever row the bucket holds,
// straight into the bucket, as an earlier build's write left a row (no write of this build stores
// a topic the session's filter would drop), and waits until the registry's copy, which the count
// reads, holds it.
func storeRawRow(t *testing.T, registry *store.Registry, client *bus.Client, sessionID string, topics ...string) {
	t.Helper()
	row, _, err := registry.RefreshAt(context.Background(), sessionID)
	if err != nil {
		row = store.Interest{SessionID: sessionID, MachineID: "test-machine", UpdatedAt: time.Now().UnixMilli()}
	}
	row.Topics = append(row.Topics, topics...)
	value, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode %s's row: %v", sessionID, err)
	}
	interests, err := client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	if _, err := interests.Put(sessionID, value); err != nil {
		t.Fatalf("store %s's row naming %v: %v", sessionID, topics, err)
	}
	waitFor(t, "the registry's copy to hold "+sessionID+"'s row as stored", func() bool {
		cached, ok := registry.Cached(sessionID)
		return ok && !slices.ContainsFunc(topics, func(topic string) bool { return !slices.Contains(cached.Topics, topic) })
	})
}

// rebuildFilter has sessionID's relay start again, which filters its consumer on its stored row
// (sessionRelays.storedFilter, sessionDelivery.storedRowFilter): the filter a session's consumer
// carries after a listener restart, whatever filter its registration set.
func rebuildFilter(t *testing.T, rig *relayRig, sessionID string) {
	t.Helper()
	unlock := rig.deps.consumers.locks.lock(sessionID)
	rig.relays.registered(sessionID, true, rig.deps.consumers.mark())
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 2)
	unlock()
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 0)
}

// claimThroughRoute claims role for sessionID through POST /v1/roles/set, a hard claim, as a
// Legion controller claims its role: the path a takeover takes in production.
func claimThroughRoute(t *testing.T, rig *relayRig, sessionID, role string) {
	t.Helper()
	rig.post(t, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"`+role+`"}`)
}

// laneNotice is a publish on lostLane through the publish route: its status, its counts and the
// event id the message was stored under.
type laneNotice struct {
	status     int
	body       string
	recipients *int
	connected  *int
	eventID    string
}

// publishLaneNotice publishes one message on lostLane from lostLaneSender through the publish
// route, as an agent or the Legion daemon does.
func publishLaneNotice(t *testing.T, rig *relayRig) laneNotice {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"topic":          lostLane,
		"message":        "a notice for PUB's controller",
		"source":         "agent",
		"source_session": lostLaneSender,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, postJSON("/v1/messages/publish", string(body)))
	notice := laneNotice{status: recorder.Code, body: recorder.Body.String()}
	var answer listenerwire.PublishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the publish answer %s: %v", notice.body, err)
	}
	notice.recipients, notice.connected, notice.eventID = answer.Recipients, answer.RecipientsConnected, answer.EventID
	if notice.status != 200 || notice.eventID == "" {
		t.Fatalf("the publish on %s answered %d %s, want 200 with the stored envelope", lostLane, notice.status, notice.body)
	}
	return notice
}

// requireNoticeCounts fails unless the publish counted wantTotal recipients, wantConnected of
// them running now.
func requireNoticeCounts(t *testing.T, notice laneNotice, wantTotal, wantConnected int) {
	t.Helper()
	if notice.recipients == nil || notice.connected == nil || *notice.recipients != wantTotal || *notice.connected != wantConnected {
		t.Fatalf("the publish on %s answered %s, want recipients %d and recipients_connected %d", lostLane, notice.body, wantTotal, wantConnected)
	}
}

// receivedNotice reports whether sessionID's stand-in received the notice stored under eventID.
// It stores a message on the session's own agent subject after the notice and reads frames until
// that one arrives: a consumer relays in stream order, so a consumer holding the notice relays it
// first. The session's own message is the row's liveness control: a stand-in it never reaches
// proves nothing about the notice, and fails the row as UNKNOWN.
func receivedNotice(t *testing.T, rig *relayRig, standIn *sessionStandIn, sessionID, eventID string) bool {
	t.Helper()
	own, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	received := false
	deadline := time.After(listenerWaitBound(t))
	for {
		select {
		case frame := <-standIn.frames:
			switch frame.item.EventID {
			case eventID:
				received = true
			case own.EventID:
				return received
			}
		case <-deadline:
			t.Fatalf("UNKNOWN: %s's own message never reached its stand-in, so its relay was not live and the notice's absence proves nothing", sessionID)
		}
	}
}

// A publish on a Legion controller's notice lane counts a session as a recipient only while its
// consumer would deliver the message: while the claim of the lane's controller role names it, the
// rule its consumer filter is built by (storedTopicFiltered). Two stores can still name
// the lane for a session whose role was lost, and each was counted: an interest row an earlier
// build stored with the lane and no claim naming the session, and the known-session record of a
// session that registered the lane while it held the role and lost it to another session's claim.
// Each such publish answered 200 with recipients 1 while its consumer, filtered on the stored row
// after a restart, delivered nothing. Now each answers recipients 0, as every stored topic no
// session follows does, and still delivers nothing. Control: the same session holding the role is
// counted and receives the notice.
func TestAPublishOnALaneNoConsumerFiltersOnCountsNoRecipient(t *testing.T) {
	t.Run("control: the session holds the role", func(t *testing.T) {
		rig := newLostLaneRig(t)
		const sessionID = "ses_lost_lane_held"
		standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
		claimRoles(t, rig.harness.registry, sessionID, lostLaneRole)
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
		storeRawRow(t, rig.harness.registry, rig.harness.client, sessionID, lostLane)
		rebuildFilter(t, rig, sessionID)
		requireFilters(t, rig.sessionConsumerRig, sessionID, lostLane)
		notice := publishLaneNotice(t, rig)
		requireNoticeCounts(t, notice, 1, 1)
		if !receivedNotice(t, rig, standIn, sessionID, notice.eventID) {
			t.Fatalf("the holder of %s did not receive the notice on its lane", lostLaneRole)
		}
	})
	t.Run("an earlier build's row holds the lane and no claim names the session", func(t *testing.T) {
		rig := newLostLaneRig(t)
		const sessionID = "ses_lost_lane_never_claimed"
		standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
		storeRawRow(t, rig.harness.registry, rig.harness.client, sessionID, lostLane)
		rebuildFilter(t, rig, sessionID)
		requireFilters(t, rig.sessionConsumerRig, sessionID)
		notice := publishLaneNotice(t, rig)
		requireNoticeCounts(t, notice, 0, 0)
		if receivedNotice(t, rig, standIn, sessionID, notice.eventID) {
			t.Fatalf("%s, which holds no claim of %s, received the notice on its lane", sessionID, lostLaneRole)
		}
	})
	t.Run("the role was taken over after the session registered the lane", func(t *testing.T) {
		rig := newLostLaneRig(t)
		const sessionID = "ses_lost_lane_taken_over"
		standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
		claimRoles(t, rig.harness.registry, sessionID, lostLaneRole)
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", lostLane))
		requireFilters(t, rig.sessionConsumerRig, sessionID, lostLane)
		// The claim is written through the store, not the claim route, so the known-session
		// record keeps the lane exactly as a former holder's record does whenever the route's
		// own rewrite of it did not run (TestAStoppedFormerHolderIsNoRecipientOfItsLostLane).
		claimRoles(t, rig.harness.registry, "ses_lost_lane_new_holder", lostLaneRole)
		waitFor(t, "the role claim view to show the role taken over", func() bool {
			held, err := rig.harness.registry.RolesHeldBy(sessionID)
			return err == nil && !slices.Contains(held, lostLaneRole)
		})
		if record, ok := rig.deps.known.Get(sessionID); !ok || !slices.Contains(record.Topics, lostLane) {
			t.Fatalf("UNKNOWN: %s's known-session record is %+v (%t), not one naming %s, so the row tests nothing", sessionID, record, ok, lostLane)
		}
		rebuildFilter(t, rig, sessionID)
		requireFilters(t, rig.sessionConsumerRig, sessionID)
		notice := publishLaneNotice(t, rig)
		requireNoticeCounts(t, notice, 0, 0)
		if receivedNotice(t, rig, standIn, sessionID, notice.eventID) {
			t.Fatalf("%s, whose role was taken over, received the notice on its lane", sessionID)
		}
	})
}

// A session that stopped while it held a controller role keeps its known-session record, the lane
// in it, for the record's 30 days (a clean stop keeps the record it had), and the claim route's
// takeover cannot rewrite the record of a session with no entry (recordFollowedTopics). So once
// another session takes the role, the stopped session is no recipient of the lane: when it runs
// again its registration's filter drops the lane, as the claim names another session. Before,
// every publish on the lane counted it for those 30 days. Control: while its claim still stands it
// is counted, stopped and not running, since its filter keeps the lane when it returns.
func TestAStoppedFormerHolderIsNoRecipientOfItsLostLane(t *testing.T) {
	for _, takenOver := range []bool{false, true} {
		name := "control: its claim still stands"
		if takenOver {
			name = "another session took the role through the claim route"
		}
		t.Run(name, func(t *testing.T) {
			rig := newLostLaneRig(t)
			const sessionID, next = "ses_lost_lane_stopped", "ses_lost_lane_next"
			claimRoles(t, rig.harness.registry, sessionID, lostLaneRole)
			rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", lostLane))
			stopSession(t, rig.mux, sessionID, "")
			if record, ok := rig.deps.known.Get(sessionID); !ok || !slices.Contains(record.Topics, lostLane) {
				t.Fatalf("UNKNOWN: the stopped %s's known-session record is %+v (%t), not one naming %s, so the row tests nothing", sessionID, record, ok, lostLane)
			}
			if !takenOver {
				requireNoticeCounts(t, publishLaneNotice(t, rig), 1, 0)
				return
			}
			rig.subscribe(t, consumerRegistration(next, "epoch-1"))
			claimThroughRoute(t, rig, next, lostLaneRole)
			if record, ok := rig.deps.known.Get(sessionID); !ok || !slices.Contains(record.Topics, lostLane) {
				t.Fatalf("UNKNOWN: after the takeover the stopped %s's known-session record is %+v (%t), not one naming %s, so the row tests nothing", sessionID, record, ok, lostLane)
			}
			requireNoticeCounts(t, publishLaneNotice(t, rig), 0, 0)
		})
	}
}

// A hard claim that takes a controller role from a live session narrows that session's consumer
// filter before the claim is answered (narrowFormerHolder): the takeover filters the former
// holder's consumer on its stored row again, which the claim cleaned and whose lane the claim it no
// longer holds drops (storedFilterWrite), and rewrites its known-session record from that row
// (recordFollowedTopics), as an unsubscribe does. So a notice published once the claim is answered
// reaches the new holder alone.
// Before, the former holder's consumer kept filtering on the lane until its next registration, up
// to a heartbeat (90 s) later, or until its relay restarted, and received the new holder's notices
// in that window. Control: with no takeover the holder receives the notice.
func TestATakeoverNarrowsTheFormerHoldersFilterAtOnce(t *testing.T) {
	for _, takenOver := range []bool{false, true} {
		name := "control: no takeover"
		if takenOver {
			name = "another session takes the role by a hard claim"
		}
		t.Run(name, func(t *testing.T) {
			rig := newLostLaneRig(t)
			const former, next = "ses_takeover_former", "ses_takeover_next"
			formerStandIn := startSessionStandIn(t, rig.url, former, answerDone)
			rig.subscribe(t, consumerRegistration(former, "epoch-1"))
			claimThroughRoute(t, rig, former, lostLaneRole)
			rig.subscribe(t, consumerRegistration(former, "epoch-1", lostLane))
			requireFilters(t, rig.sessionConsumerRig, former, lostLane)
			if !takenOver {
				notice := publishLaneNotice(t, rig)
				requireNoticeCounts(t, notice, 1, 1)
				if !receivedNotice(t, rig, formerStandIn, former, notice.eventID) {
					t.Fatalf("the holder %s did not receive the notice on its lane", former)
				}
				return
			}
			nextStandIn := startSessionStandIn(t, rig.url, next, answerDone)
			rig.subscribe(t, consumerRegistration(next, "epoch-1"))
			claimThroughRoute(t, rig, next, lostLaneRole)
			// Read the moment the claim is answered, with no wait: the narrowing is the claim's.
			// Each check below reports and goes on, so a row that fails shows every one it fails.
			if filters := rig.filters(t, former); slices.Contains(filters, lostLane) {
				t.Errorf("once the claim of %s by %s was answered, the former holder's consumer still filters on %v", lostLaneRole, next, filters)
			}
			if record, ok := rig.deps.known.Get(former); !ok || slices.Contains(record.Topics, lostLane) {
				t.Errorf("once the claim was answered, the former holder's known-session record is %+v (%t), want one without %s", record, ok, lostLane)
			}
			rig.subscribe(t, consumerRegistration(next, "epoch-1", lostLane))
			requireFilters(t, rig.sessionConsumerRig, next, lostLane)
			notice := publishLaneNotice(t, rig)
			if notice.recipients == nil || notice.connected == nil || *notice.recipients != 1 || *notice.connected != 1 {
				t.Errorf("the publish on %s after the takeover answered %s, want recipients 1 and recipients_connected 1: the new holder alone", lostLane, notice.body)
			}
			if !receivedNotice(t, rig, nextStandIn, next, notice.eventID) {
				t.Errorf("the new holder %s did not receive the notice on its lane", next)
			}
			if receivedNotice(t, rig, formerStandIn, former, notice.eventID) {
				t.Errorf("the former holder %s received the notice published after %s took the role", former, next)
			}
		})
	}
}

// A takeover that cannot narrow the former holder says so: a WARN line naming the role, its lane,
// the former holder and the claimant, since the lane then stays on the former holder's consumer
// until its next registration or relay restart. Here a registration of the former holder holds its
// session lock across the claim, as a registration does across its consumer call and its writes,
// so the narrowing's wait for that lock runs out with the claim route's budget, and the claim is
// still answered inside that budget. Before, the narrowing waited on the lock with no bound, and a
// failed narrowing said nothing that named the role or the lane. Control: with the lock free the
// same takeover narrows the filter and logs no such line.
func TestATakeoverThatLeavesTheFormerHoldersFilterSaysSo(t *testing.T) {
	const message = "listener role takeover left the former holder's consumer filter unnarrowed; the role's notice lane may reach that session until its next registration or relay restart"
	for _, held := range []bool{false, true} {
		name := "control: the former holder's lock is free"
		if held {
			name = "a registration holds the former holder's lock through the claim"
		}
		t.Run(name, func(t *testing.T) {
			rig := newLostLaneRig(t)
			const former, next = "ses_takeover_said_former", "ses_takeover_said_next"
			rig.subscribe(t, consumerRegistration(former, "epoch-1"))
			claimThroughRoute(t, rig, former, lostLaneRole)
			rig.subscribe(t, consumerRegistration(former, "epoch-1", lostLane))
			requireFilters(t, rig.sessionConsumerRig, former, lostLane)
			rig.subscribe(t, consumerRegistration(next, "epoch-1"))
			if !held {
				claimThroughRoute(t, rig, next, lostLaneRole)
				if lines := rig.logLines(t, message, former); len(lines) != 0 {
					t.Fatalf("a takeover that narrowed the former holder logged %v", lines)
				}
				requireFilters(t, rig.sessionConsumerRig, former)
				return
			}
			// The lock is let go 2 s past the route's budget whatever the claim does, so a
			// narrowing that waits on it with no bound answers late rather than never.
			var once sync.Once
			unlock := rig.deps.consumers.locks.lock(former)
			release := func() { once.Do(unlock) }
			timer := time.AfterFunc(sendRequestBudget+2*time.Second, release)
			defer timer.Stop()
			started := time.Now()
			claimThroughRoute(t, rig, next, lostLaneRole)
			took := time.Since(started)
			filters := rig.filters(t, former)
			release()
			if took > sendRequestBudget+time.Second {
				t.Errorf("the claim was answered %s after it was sent, past the route's budget of %s: the narrowing's wait for the former holder's lock is not bounded by it", took, sendRequestBudget)
			} else if !slices.Contains(filters, lostLane) {
				t.Fatalf("UNKNOWN: with the former holder's lock held through the claim, its consumer filters on %v, not on %s, so nothing was left to log", filters, lostLane)
			}
			rig.requireOneLogLine(t, "WARN", message, former, map[string]any{
				"role":                lostLaneRole,
				"lane":                lostLane,
				"claimant_session_id": next,
			})
		})
	}
}
