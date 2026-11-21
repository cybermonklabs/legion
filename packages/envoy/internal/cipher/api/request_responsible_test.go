// request_responsible_test.go pins, over the broker's own routes, who is asked to decide a request
// for a shared human-tier secret and who may: GET /v1/pending, which Dispatch relays to each
// person's Inbox, lists it for the person responsible for the agent that made it alone, and anyone
// signed in to the broker's approval page still decides it there.
package api_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestAnotherPersonDoesNotSeeACaseOneRequestButMayApproveIt: a session acting for alice asks for
// SHARED_KEY. Dispatch's list for bob omits it and its list for alice holds it; bob signs in to
// the approval page, opens the record and approves; the request is granted, and its decision
// names bob.
func TestAnotherPersonDoesNotSeeACaseOneRequestButMayApproveIt(t *testing.T) {
	const alice, bob = "alice@example.com", "bob@example.com"
	ts := newTestServer(t)
	session, key := ts.newSessionEnrollment(t, "session-"+t.Name(), alice)
	asked := ts.ask(t, key, session, "SHARED_KEY")
	if asked.State != "pending" || asked.RecordID == nil || string(asked.Approver) != approverJSON(record.AnyoneApprover) {
		t.Fatalf("POST /v1/requests SHARED_KEY = %+v; want it pending on %s", asked, record.AnyoneApprover)
	}
	recordID := *asked.RecordID
	if pendingFor(t, ts, bob, recordID) {
		t.Errorf("GET /v1/pending?approver=%s lists record %s; want it only in the Inbox of %s, whose session asked", bob, recordID, alice)
	}
	if !pendingFor(t, ts, alice, recordID) {
		t.Errorf("GET /v1/pending?approver=%s omits record %s; want it listed for the person whose session asked", alice, recordID)
	}

	ts.decide(t, bob, recordID, true)
	if read, body := ts.statusOf(t, key, session, asked.RequestID); read.State != "granted" {
		t.Fatalf("GET /v1/requests/{id} after %s approved = %s; want granted", bob, body)
	}
	if decided := ts.record(t, recordID).Decided; decided == nil || decided.Event != "approved" || decided.Login != bob {
		t.Fatalf("record %s's decision = %+v; want approved by %s", recordID, decided, bob)
	}
}
