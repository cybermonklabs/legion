package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
)

// deregister sends DELETE /v1/sessions/<sessionID><query> through the rig's routes and returns
// the answer; query carries the condition the caller names, "" for a request that names none.
// Every outcome of this route is a 200, so any other status is the row's failure.
func (rig *sessionConsumerRig) deregister(t *testing.T, sessionID, query string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID+query, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/sessions/%s%s: status %d: %s", sessionID, query, recorder.Code, recorder.Body.String())
	}
	return recorder
}

// requireNoConsumer fails the test unless the stream holds no consumer named for sessionID
// (bus.SessionConsumerName, s-<session id>), which is the slot the deregistration frees.
func (rig *sessionConsumerRig) requireNoConsumer(t *testing.T, sessionID string) {
	t.Helper()
	if info, err := rig.consumers.Info(context.Background(), sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		name, _ := bus.SessionConsumerName(sessionID)
		t.Fatalf("the stream holds %s (%+v, %v); want it deleted with the registration", name, info, err)
	}
}

// awaitNoConsumer waits for the stream to hold no consumer named for sessionID and fails the
// test when it still does 5 s on. The deregistration answers before its delete runs
// (sessionDelivery.deleteAfterAnswer), so a row that reads the stream the moment the answer
// comes reads it before the delete.
func (rig *sessionConsumerRig) awaitNoConsumer(t *testing.T, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := rig.consumers.Info(context.Background(), sessionID)
		if errors.Is(err, bus.ErrSessionConsumerNotFound) {
			return
		}
		if time.Now().After(deadline) {
			name, _ := bus.SessionConsumerName(sessionID)
			t.Fatalf("the stream holds %s still (%v); want it deleted with the registration", name, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// requireConsumer fails the test unless sessionID's consumer is on the stream.
func (rig *sessionConsumerRig) requireConsumer(t *testing.T, sessionID string) {
	t.Helper()
	if _, err := rig.consumers.Info(context.Background(), sessionID); err != nil {
		name, _ := bus.SessionConsumerName(sessionID)
		t.Fatalf("the stream holds no %s (%v); want the consumer of the live registration", name, err)
	}
}

// A deregistration that names no condition removes the registration whoever wrote it, so nothing
// is left to deliver to: its session consumer goes with it rather than holding a slot of the
// stream's consumer cap until the server's inactivity threshold reaches it.
func TestDeregistrationWithNoConditionDeletesTheSessionsConsumer(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_deregister_plain"
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"e"}}`)
	rig.requireConsumer(t, sessionID)

	recorder := rig.deregister(t, sessionID, "")

	if recorder.Body.Len() != 0 {
		t.Fatalf("the answer carries a body: %s; want none, as this route has always answered a request naming no condition", recorder.Body.String())
	}
	rig.awaitNoConsumer(t, sessionID)
}

// The deregistration a client sends for its own instance deletes the consumer too: the answer
// says the registration is gone, and nothing stands behind it.
func TestDeregistrationOfTheSessionsOwnInstanceDeletesItsConsumer(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const (
		sessionID  = "ses_deregister_own_instance"
		instanceID = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	)
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"client_instance_id":"`+instanceID+`","delivery":{"consumer":true,"epoch":"e"}}`)
	rig.requireConsumer(t, sessionID)

	recorder := rig.deregister(t, sessionID, "?client_instance_id="+instanceID)

	if got := strings.TrimSpace(recorder.Body.String()); got != `{"deleted":true}` {
		t.Fatalf("the answer is %s, want {\"deleted\":true}", got)
	}
	rig.awaitNoConsumer(t, sessionID)
}

// A deregistration another client instance sends leaves a live registration standing, and that
// registration's session still reads its consumer: the consumer stays, as the entry does.
func TestDeregistrationOfAnotherInstanceKeepsTheSessionConsumer(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const (
		sessionID = "ses_deregister_other_instance"
		holding   = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
		leaving   = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	)
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"client_instance_id":"`+holding+`","delivery":{"consumer":true,"epoch":"e"}}`)

	recorder := rig.deregister(t, sessionID, "?client_instance_id="+leaving)

	var answer deleteSessionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Deleted || answer.Reason != "the registration belongs to another client instance" {
		t.Fatalf("the answer is %+v, want the registration kept for another client instance", answer)
	}
	if entry := rig.entry(t, sessionID); entry.Delivery != session.DeliveryConsumer {
		t.Fatalf("the session entry is %+v, want the other instance's registration still in consumer delivery", entry)
	}
	rig.requireConsumer(t, sessionID)
}

// A consumer deleted with its registration stays deleted while this listener keeps relaying: the
// delete dropped the pending start the relay's restarts leave behind, so a restart that was
// already past its wanted check finds the consumer missing and ends (bus.ErrSessionConsumerNotFound)
// rather than creating one at that start.
//
// No heartbeat runs during the DELETE here, and that is what the row rests on: a registration
// behind the delete writes the entry again and creates the consumer again by design, since the
// session is live again and must be delivered to. What must not happen is the listener's own
// retry bringing back the consumer of a session that registered no more.
func TestADeletedSessionsConsumerIsNotBroughtBackByTheRelaysRetry(t *testing.T) {
	// The relay's windows are shortened to a tenth of production's, so the row spends its own
	// wait over several of the relay's retries rather than seconds of the clock.
	const window = 100 * time.Millisecond
	rig := newRelayRigReceipt(t, window, 2*window, window)
	sessionID := "ses_deregister_retry_" + id.New()
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	// A message nothing answers: the relay sends its frame, pauses for want of a receipt, and
	// restarts the consumer at its ack floor again and again, each restart deleting the consumer
	// and creating it at that floor. That is the retry the delete must outlast.
	rig.storeEvent(t, contracts.AgentSubject(sessionID))
	rig.awaitPending(t, sessionID, 0)

	rig.deregister(t, sessionID, "")

	rig.awaitNoConsumer(t, sessionID)
	for deadline := time.Now().Add(10 * window); time.Now().Before(deadline); time.Sleep(window / 2) {
		rig.requireNoConsumer(t, sessionID)
	}
}

// Each lane the listener owns is its own to write and nobody else's: the kept lane holds the
// copies it keeps for a role's next holder and the cursors of its drains, and the resume lane
// its record of where a deleted session consumer stopped, which is where that session's
// consumer is created again (bus.SessionResumeSubject). A publish to either through the route
// would let any bearer rewind a role's drain or choose where another session's delivery
// resumes, so the route refuses every topic under one (bus.ListenerOwnLane) and stores nothing.
// The rows are the listener's own lanes themselves (bus.ListenerOwnLanes), so a lane the stream
// gains is covered here rather than waiting for somebody to name it.
func TestAPublishToALaneTheListenerOwnsIsRefused(t *testing.T) {
	d, client := keptRoleDeps(t)
	lanes := bus.ListenerOwnLanes()
	if len(lanes) == 0 {
		t.Fatal("the listener owns no lane of the stream; this row has nothing to refuse")
	}
	for _, lane := range lanes {
		// bus spells a lane as every topic under it begins and as a refusal names it
		// (bus.ListenerOwnLanes), so a topic of that lane is the lane and a name.
		subject := lane + "mine"
		t.Run(subject, func(t *testing.T) {
			recorder, _ := publishThrough(d,
				`{"topic":"`+subject+`","source":"agent","message":"{\"floor\":1,\"stream_id\":\"mine\"}"}`)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("publish to %s: status %d, body %s; want 400", subject, recorder.Code, recorder.Body.String())
			}
			if body := recorder.Body.String(); !strings.Contains(body, lane) {
				t.Fatalf("the refusal of %s is %q, want it to name the lane %s it lies under", subject, body, lane)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := client.LastStoredMessage(ctx, subject); !errors.Is(err, natsgo.ErrMsgNotFound) {
				t.Fatalf("the stream holds a message on %s (%v); the refused publish stored nothing", subject, err)
			}
		})
	}
}

// A deregistration answers before the consumer's delete it leaves running: the delete waits on
// the session's lock with no bound of its own - a relay restart of this listener's can hold it
// for its own 5 s - and then makes up to four calls to the server, where the client's attempt
// ends at 5 s (LISTENER_CALL_TIMEOUT_MS) and the route promises an answer inside it. The
// consumer goes once the wait ends, so nothing is given up for the earlier answer.
func TestADeregistrationAnswersWhileItsConsumerDeleteWaits(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_deregister_answers_first"
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"e"}}`)
	rig.requireConsumer(t, sessionID)
	// The session's lock, held as one of this listener's own consumer calls holds it: the
	// delete waits for it, and the answer must not.
	unlock := rig.deps.consumers.locks.lock(sessionID)

	recorder := httptest.NewRecorder()
	answered := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID, nil))
		answered <- time.Since(started)
	}()

	select {
	case took := <-answered:
		if took >= sendRequestBudget {
			unlock()
			t.Fatalf("the deregistration answered %s in, outside the %s of bus work this route gives itself", took, sendRequestBudget)
		}
	case <-time.After(sendRequestBudget):
		unlock()
		t.Fatalf("the deregistration had not answered %s in while its consumer's delete waited: it waits for that delete", sendRequestBudget)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/sessions/%s: status %d: %s", sessionID, recorder.Code, recorder.Body.String())
	}
	rig.requireConsumer(t, sessionID)

	unlock()

	rig.awaitNoConsumer(t, sessionID)
}

// A registration that lands while a clean stop is on its way keeps its consumer. The stop names
// if_not_registered_after, reads the entry the relaunch replaces - the relaunch has not written
// its own yet - and answers EntryDeleted, which is the order the Legion daemon's clean stop of a
// relaunched role takes (dispatch://INFRA-392). What keeps the live session's consumer is the
// lock: the registration holds it from its consumer call through the write of its entry, and the
// delete reads the session bucket under that same lock and finds the record there. Without both
// the session would be live with no consumer, and its relay paused on
// bus.ErrSessionConsumerNotFound until it registered again.
func TestACleanStopKeepsTheConsumerOfARegistrationThatLandedUnderIt(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_deregister_races_registration"
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"epoch-1"}}`)
	rig.requireConsumer(t, sessionID)
	// The moment the stop belongs to: the registration above was written at or before it, so
	// the stop removes that entry and answers that it did.
	stopAt := strconv.FormatInt(time.Now().UnixMilli(), 10)

	// The relaunch, held where the race is: it waits for the session's lock inside its consumer
	// call, with its entry unwritten.
	unlock := rig.deps.consumers.locks.lock(sessionID)
	relaunched := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
			strings.NewReader(`{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
				"delivery":{"consumer":true,"epoch":"epoch-2"}}`)))
		relaunched <- recorder
	}()
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 2)

	answer := rig.deregister(t, sessionID, "?if_not_registered_after="+stopAt)

	if got := strings.TrimSpace(answer.Body.String()); got != `{"deleted":true}` {
		unlock()
		t.Fatalf("the clean stop answered %s, want {\"deleted\":true}: this row is about the delete that outcome starts", got)
	}
	// Its delete queues on the lock behind the relaunch, which the release below hands the lock
	// to first; the wait for an empty lock is the wait for both to be done with it.
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 3)
	unlock()
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 0)

	recorder := <-relaunched
	if recorder.Code != http.StatusOK {
		t.Fatalf("the relaunch's registration: status %d: %s", recorder.Code, recorder.Body.String())
	}
	if entry := rig.entry(t, sessionID); entry.Epoch != "epoch-2" || entry.Delivery != session.DeliveryConsumer {
		t.Fatalf("the session entry is %+v, want the relaunch's registration in consumer delivery at epoch-2", entry)
	}
	rig.requireConsumer(t, sessionID)
}

// laggingFollowerBucket is the session bucket as a copy that has not caught up answers a direct
// get: a key whose record the stream's leader holds reads nats.ErrKeyNotFound, and every such
// read is counted. The leader's read of a key does not come through here - it is a stream
// message get sent on the bucket's own jetstream context (bus.KeyValue.GetFromLeader), not a
// call on this handle - so the count is the direct gets alone.
type laggingFollowerBucket struct {
	natsgo.KeyValue
	gets *atomic.Int64
}

func (kv laggingFollowerBucket) Get(string) (natsgo.KeyValueEntry, error) {
	kv.gets.Add(1)
	return nil, natsgo.ErrKeyNotFound
}

// The read a consumer delete makes under the session's lock is the one the leader of the
// session bucket's stream answers, not the direct get every other read of that bucket makes
// (session.SessionRegistry.RefreshFromLeader). On a replicated bucket a direct get is answered
// by whichever copy is near enough to current, so a copy that has not applied a relaunch's
// registration answers not found for a session that is live, and a delete reading it would
// take the live relaunch's consumer - pausing that relay until its next heartbeat registered
// the session again.
//
// The server these rows run against is one node holding one copy, so it can lag by nothing and
// no row here can make a follower answer late. What this one shows instead is which read the
// delete asks for, with the loss the other read costs put in front of it: the bucket's direct
// get is made to answer not found for a live registration, as a lagging follower answers it,
// and the delete is run against that bucket. A delete reading the direct get takes this live
// registration's consumer; the delete as it stands reads the leader, which holds the record,
// keeps the consumer, and makes no direct get at all. That the leader read is really answered
// by the stream's leader is read off the wire in bus's own row (internal/bus, GetFromLeader).
func TestTheConsumerDeleteReadsTheSessionBucketsLeader(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_delete_reads_the_leader"
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"e"}}`)
	rig.requireConsumer(t, sessionID)
	directGets := &atomic.Int64{}
	swapSessionBucket(t, rig.harness.sessions, func(kv natsgo.KeyValue) natsgo.KeyValue {
		return laggingFollowerBucket{KeyValue: kv, gets: directGets}
	})
	ctx := context.Background()
	if _, err := rig.harness.sessions.Refresh(ctx, sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the direct get of %s answers %v, want %v: this row needs the answer a copy that is behind gives", sessionID, err, natsgo.ErrKeyNotFound)
	}
	if _, err := rig.harness.sessions.RefreshFromLeader(ctx, sessionID); err != nil {
		t.Fatalf("the leader's read of %s: %v, want the live registration the bucket holds", sessionID, err)
	}
	// The two reads above are the row's own; what the delete reads is counted from here.
	directGets.Store(0)

	gone := rig.deps.consumers.deleteConsumer(ctx, rig.harness.sessions, sessionID, func(ctx context.Context) error {
		return rig.deps.consumers.consumers.Delete(ctx, sessionID)
	})

	if gone {
		t.Error("the delete reported the consumer gone; the bucket's leader holds this session's registration, so the consumer that delivers to it must stand")
	}
	if made := directGets.Load(); made != 0 {
		t.Errorf("the delete made %d direct get(s) of the session bucket, want none: a copy that is behind answers one, and its not-found takes the consumer of a live session", made)
	}
	rig.requireConsumer(t, sessionID)
}
