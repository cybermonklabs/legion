package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
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

// awaitNoConsumer waits for the asynchronous deregistration delete to remove the consumer.
func (rig *sessionConsumerRig) awaitNoConsumer(t *testing.T, sessionID string) {
	t.Helper()
	awaitConsumerGone(t, rig.consumers, sessionID, "after deregistration")
}

func awaitConsumerGone(t *testing.T, consumers *bus.SessionConsumers, sessionID, when string) {
	t.Helper()
	ctx := testwait.Context(t)
	testwait.Until(t, "the consumer of "+sessionID+" to be gone "+when, func() (string, bool) {
		info, err := consumers.Info(ctx, sessionID)
		return fmt.Sprintf("%+v, %v", info, err), errors.Is(err, bus.ErrSessionConsumerNotFound)
	})
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	rig := newSessionConsumerRig(t)
	const (
		sessionID = "ses_deregister_other_instance"
		holding   = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
		leaving   = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	)
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"client_instance_id":"`+holding+`","delivery":{"consumer":true,"epoch":"e"}}`)

	recorder := rig.deregister(t, sessionID, "?client_instance_id="+leaving)

	var answer listenerwire.DeleteSessionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Deleted == nil || *answer.Deleted || answer.Reason != "the registration belongs to another client instance" {
		t.Fatalf("the answer is %+v, want the registration kept for another client instance", answer)
	}
	if entry := rig.entry(t, sessionID); entry.Delivery != session.DeliveryConsumer {
		t.Fatalf("the session entry is %+v, want the other instance's registration still in consumer delivery", entry)
	}
	rig.requireConsumer(t, sessionID)
}

// A restart whose create failed leaves its start pending. Deregistering after that failure
// stops the run and clears the pending start. Even a retry already past its ownership check
// must then find no consumer, instead of creating one at the old start.
//
// No heartbeat runs during the DELETE here, and that is what the row rests on: a registration
// behind the delete writes the entry again and creates the consumer again by design, since the
// session is live again and must be delivered to. What must not happen is the listener's own
// retry bringing back the consumer of a session that registered no more.
func TestADeletedSessionsConsumerIsNotBroughtBackByTheRelaysRetry(t *testing.T) {
	t.Parallel()
	rig, fake := newRelayRigOnFakeClock(t)
	sessionID := "ses_deregister_retry_" + id.New()
	standIn := startSessionStandIn(t, rig.url, sessionID, nil)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	stored, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	frames := standIn.receive(t, 1)
	requireFrames(t, frames, stored)
	var run *relayRun
	withHeldRelayFrame(t, rig, frames[0], func(frame *relayFrame) { run = frame.run })

	expireReceiptWait(t, fake)
	pause := fake.Now().Add(relayPauseFirst)
	fake.AwaitArmedAt(t, "the unresponsive relay's pause backoff", pause)
	failed := failCreates(t, rig, sessionID, 1)
	fake.Set(pause)
	awaitCreateFailure(t, failed)
	fake.AwaitArmedAt(t, "the relay's retry after its failed create", pause.Add(relayRetryFirst))

	rig.deregister(t, sessionID, "")
	ctx := testwait.Context(t)
	if !rig.deps.consumers.deletes.wait(ctx) {
		t.Fatal("the consumer delete did not finish")
	}
	testwait.Receive(t, "the deregistered session's relay to end", run.done)

	// Model a retry that already passed its ownership check by calling its restart step.
	// This reads the same pending start the failed create left on the delivery connection.
	_, pull, outcome := rig.relays.restart(ctx, &relayRun{sessionID: sessionID})
	if pull != nil {
		pull.Stop()
	}
	rig.requireNoConsumer(t, sessionID)
	if outcome != relayEnded {
		t.Fatalf("restart after deregistration returned %v, want relayEnded", outcome)
	}
}

// Each lane the listener owns is its own to write and nobody else's: the kept lane holds the
// copies it keeps for a role's next holder and the cursors of its drains, and the resume lane
// its record of where a deleted session consumer stopped, which is where that session's
// consumer is created again (bus.SessionResumeSubject). A publish to either through the route
// would let any bearer rewind a role's drain or choose where another session's delivery
// resumes, so the route refuses every topic under one (bus.ListenerOwnLaneOf) and stores nothing.
// The rows are the listener's own lanes themselves (bus.ListenerOwnLanes), so a lane the stream
// gains is covered here rather than waiting for somebody to name it.
func TestAPublishToALaneTheListenerOwnsIsRefused(t *testing.T) {
	t.Parallel()
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
			if _, err := client.LastStoredMessage(testwait.Context(t), subject); !errors.Is(err, natsgo.ErrMsgNotFound) {
				t.Fatalf("the stream holds a message on %s (%v); the refused publish stored nothing", subject, err)
			}
		})
	}
}

// A deregistration answers while its consumer's delete waits for the session lock. The route's
// budget cannot run out on a clock that never moves, so a route that waited for the delete
// could never answer while the test holds the lock. Once the lock is released, the consumer
// must still go.
func TestADeregistrationAnswersWhileItsConsumerDeleteWaits(t *testing.T) {
	t.Parallel()
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_deregister_answers_first"
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"e"}}`)
	rig.requireConsumer(t, sessionID)
	freezeRegistrationBudgets(rig.deps)
	// The session's lock, held as one of this listener's own consumer calls holds it: the
	// delete waits for it, and the answer must not.
	unlock := sync.OnceFunc(rig.deps.consumers.locks.lock(sessionID))
	defer unlock()

	recorder := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() {
		rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID, nil))
		close(answered)
	}()

	testwait.Receive(t, "the deregistration answer while the consumer's delete waits", answered)
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
	t.Parallel()
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_deregister_races_registration"
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"epoch-1"}}`)
	rig.requireConsumer(t, sessionID)
	freezeRegistrationBudgets(rig.deps)
	// The moment the stop belongs to: the registration above was written at or before it, so
	// the stop removes that entry and answers that it did.
	stopAt := strconv.FormatInt(rig.entry(t, sessionID).UpdatedAt, 10)

	// The relaunch, held where the race is: it waits for the session's lock inside its consumer
	// call, with its entry unwritten.
	unlock := sync.OnceFunc(rig.deps.consumers.locks.lock(sessionID))
	defer unlock()
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

	recorder := testwait.Receive(t, "the relaunch's registration to answer", relaunched)
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
// read is counted. The leader's read of a key does not come through here - it is nats.go's
// stream message get on the bucket's connection (bus.KeyValue.GetFromLeader), not a call on
// this handle - so the count is the direct gets alone.
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
	t.Parallel()
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_delete_reads_the_leader"
	rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],
		"delivery":{"consumer":true,"epoch":"e"}}`)
	rig.requireConsumer(t, sessionID)
	// The registry's watcher is retired before its bucket handle is swapped, as every other
	// caller of swapSessionBucket retires it: that handle is the watcher's own field, and a
	// watcher still delivering records reads it while the swap writes it. Nothing below needs
	// the cache to follow the bucket - both reads here go to the bucket itself.
	rig.harness.sessions.StopWatch()
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

	gone := rig.deps.consumers.deleteConsumer(ctx, rig.deps, sessionID, nil)

	if gone {
		t.Error("the delete reported the consumer gone; the bucket's leader holds this session's registration, so the consumer that delivers to it must stand")
	}
	if made := directGets.Load(); made != 0 {
		t.Errorf("the delete made %d direct get(s) of the session bucket, want none: a copy that is behind answers one, and its not-found takes the consumer of a live session", made)
	}
	rig.requireConsumer(t, sessionID)
}
