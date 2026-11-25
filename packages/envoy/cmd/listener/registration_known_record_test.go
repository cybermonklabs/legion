package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A registration whose store work before its known-session record spent the whole store budget
// still writes that record before it answers 200 (answerRegistration): the write is bounded on its
// own, so a send to the session once it has stopped is stored on the record rather than refused
// 404. On a box under 32 busy loops that work can take the budget, and a write that shared it
// would then never be sent, so the registration would be answered 200 with no record
// (dispatch://INFRA-3149). Here the spent budget is made, not waited for. The routes run on a fake
// clock (stillClock), and the registration's own repair of the session's consumer, which runs
// after its entry is written and before the renewal and the record (confirmRegistered,
// beforeRepairPlan), moves that clock past sendRequestBudget: from there on every bucket call the
// budget bounds is a context that has ended, which is what a loaded box leaves the record write
// with. The repair itself runs on a bound of the real clock, so the clock's move does not change
// it.
//
// The answer is read as the client reads it: a 200 must leave the record in the bucket, read
// there rather than from the listener's copy, and the send after the session's clean stop must be
// the stored 200 with state stopped and recipient_state gone. A registration refused here instead
// would be as good an answer, since its client registers again; what may never happen is the 200
// with no record.
func TestARegistrationWhoseBudgetTheStepsBeforeItsRecordSpentStillWritesTheRecordBeforeIts200(t *testing.T) {
	t.Parallel()
	rig := setupKnownRig(t)
	clock := rig.stillClock()
	const sessionID = "ses_spent_budget"

	var spent sync.Once
	var advanced atomic.Bool
	rig.deps.consumers.beforeRepairPlan = func() {
		spent.Do(func() {
			clock.Advance(sendRequestBudget)
			advanced.Store(true)
		})
	}
	t.Cleanup(func() { rig.deps.consumers.beforeRepairPlan = nil })
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, postJSON("/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-"+sessionID)))
	rig.deps.consumers.beforeRepairPlan = nil

	if !advanced.Load() {
		t.Fatal("the registration ran no repair of its consumer, so nothing spent its budget and this row shows nothing")
	}
	if recorder.Code != http.StatusOK {
		t.Logf("the registration whose budget was spent answered %d %s: refused, so its client registers again", recorder.Code, recorder.Body.String())
		return
	}
	if revision := rig.revision(t, sessionID); revision == 0 {
		t.Fatalf("the registration whose budget was spent answered 200 %s with no known-session record in the bucket: a send to the session once it stops is refused 404 rather than stored",
			recorder.Body.String())
	}
	rig.stop(t, sessionID)
	rig.sendOnStillClock(t, sessionID, nil).requireAnswer(t, "a send to the stopped session the registration recorded", sessionStateStopped, session.Gone)
}

// A registration whose known-session record write does not land is refused with the
// bus-unavailable 503 and Retry-After: 1, which every client registers again on, and never
// answered 200: a 200 there is a session registered with no record, one a send cannot be stored
// for once it stops. The listener's bus runs through a relay that swallows the record's write and
// everything after it, so the write is sent and never answered; the routes run on a fake clock,
// which this moves past the store budget once the write has been swallowed, past the write's own
// bound with it. A write that shared that budget would fail as a WARN beside a 200.
func TestARegistrationWhoseKnownSessionRecordIsNotWrittenIsRefusedForItsClientToRetry(t *testing.T) {
	t.Parallel()
	url := testnats.URL(t)
	relay := startListenerRelay(t, url)
	client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's bus through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newKnownSessionRigOn(t, client, url)
	clock := rig.stillClock()
	const sessionID = "ses_record_unwritten"

	relay.arm("PUB $KV."+bus.KnownSessionBucket+"."+sessionID, 1)
	answers := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		rig.mux.ServeHTTP(recorder, postJSON("/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-"+sessionID)))
		answers <- recorder
	}()
	testwait.Receive(t, "the registration's known-session record write to reach the silent bus", relay.swallowed)
	select {
	case recorder := <-answers:
		t.Fatalf("the registration answered %d %s while its record write was still unanswered", recorder.Code, recorder.Body.String())
	default:
	}
	clock.Advance(sendRequestBudget)
	recorder := testwait.Receive(t, "the registration's answer once its record write's bound had passed", answers)
	if recorder.Code == http.StatusOK {
		t.Fatalf("the registration whose known-session record write never landed answered 200 %s: the session is registered with no record", recorder.Body.String())
	}
	wantWriteBusUnanswered(t, "POST /v1/interests/subscribe with its record write unanswered", recorder)
}

// A registration whose known-session record is stored in chunks (kvchunk.Write), and one of whose
// chunk writes does not land, is refused as a whole record's unanswered write is: with the
// bus-unavailable 503, never 200. The record counts as written only once its last chunk and its
// manifest have landed. The registration also leaves no record of the session that reads as
// written: the manifest is written after every chunk, so a reader of the bucket, and the
// listener's own copy, find no record rather than part of one. The relay swallows the first chunk
// write of the known-session bucket and everything after it. The control is
// TestAStoppedSessionPastTheMaxPayloadIsStillSentToAndCounted, where a known-session record of the
// same size is written in chunks, answered 200, and read back with every topic.
func TestARegistrationWhoseKnownSessionChunkIsNotWrittenIsRefusedAndLeavesNoRecord(t *testing.T) {
	t.Parallel()
	url := testnats.URL(t)
	relay := startListenerRelay(t, url)
	client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's bus through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newKnownSessionRigOn(t, client, url)
	clock := rig.stillClock()
	const sessionID = "ses_record_chunk_unwritten"
	// The wide topics of the size rows, past the server's max payload, less their last topic, a
	// role lane, which would make the registration a claim of that role too.
	wide := wideTopics(300, 3900, "legion-controller")
	topics := wide[:len(wide)-1]
	direct := testnats.Connect(t, url)
	t.Cleanup(direct.Close)
	if size, maxPayload := len(mustJSON(t, topics)), int(direct.MaxPayload()); size <= maxPayload {
		t.Fatalf("the topics take %d bytes as JSON, within the server's max payload of %d bytes: the record would be stored whole", size, maxPayload)
	}

	relay.arm("$KV."+bus.KnownSessionBucket+"."+kvchunk.Prefix, 1)
	answers := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		rig.mux.ServeHTTP(recorder, postJSON("/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-"+sessionID, topics...)))
		answers <- recorder
	}()
	testwait.Receive(t, "the first chunk write of the registration's known-session record to reach the silent bus", relay.swallowed)
	select {
	case recorder := <-answers:
		t.Fatalf("the registration answered %d %s while a chunk write of its record was still unanswered", recorder.Code, recorder.Body.String())
	default:
	}
	clock.Advance(sendRequestBudget)
	recorder := testwait.Receive(t, "the registration's answer once its record write's bound had passed", answers)
	if recorder.Code == http.StatusOK {
		t.Fatalf("the registration whose record chunk never landed answered 200 %s: the session is registered with no record", recorder.Body.String())
	}
	wantWriteBusUnanswered(t, "POST /v1/interests/subscribe with a chunk of its record unwritten", recorder)

	js, err := direct.JetStream()
	if err != nil {
		t.Fatalf("open a JetStream context on a connection of the test's own: %v", err)
	}
	kv, err := js.KeyValue(bus.KnownSessionBucket)
	if err != nil {
		t.Fatalf("open the known-session bucket: %v", err)
	}
	if entry, err := kv.Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the bucket holds %s's record after a chunk of it was not written (%v, %v), want none: no part of a record reads as written", sessionID, entry, err)
	}
	if record, ok := rig.known.Get(sessionID); ok {
		t.Fatalf("the listener's copy holds %s's record %+v after a chunk of it was not written, want none", sessionID, record)
	}
}
