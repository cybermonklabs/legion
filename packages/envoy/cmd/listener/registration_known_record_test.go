package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A registration whose store work before its known-session record spent the whole store budget
// still writes that record before it answers 200 (answerRegistration): the write is bounded on its
// own, so a send to the session once it has stopped is stored on the record rather than refused
// 404. On a box under 32 busy loops that work took the budget, the write was never sent, and the
// registration was answered 200 with no record (dispatch://INFRA-3149); here the spent budget is
// made, not waited for. The routes run on a fake clock (stillClock), and the registration's own
// repair of the session's consumer, which runs after its entry is written and before the renewal
// and the record (confirmRegistered, beforeRepairPlan), moves that clock past sendRequestBudget: from
// there on every bucket call the budget bounds is a context that has ended, which is what the
// loaded box left the record write with. The repair itself runs on a bound of the real clock, so
// it is unchanged.
//
// The answer is read as the client reads it: a 200 must leave the record in the bucket, read
// there rather than from the listener's copy, and the send after the session's clean stop must be
// the stored 200 with state stopped and recipient_state gone. A registration refused here instead
// would be as good an answer, since its client registers again; what may never happen is the 200
// with no record.
func TestARegistrationWhoseBudgetTheStepsBeforeItsRecordSpentStillWritesTheRecordBeforeIts200(t *testing.T) {
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
// bound with it. Before the fix the write shared that budget and its failure was a WARN beside a
// 200.
func TestARegistrationWhoseKnownSessionRecordIsNotWrittenIsRefusedForItsClientToRetry(t *testing.T) {
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
