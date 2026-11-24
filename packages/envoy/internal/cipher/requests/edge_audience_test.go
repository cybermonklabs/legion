// edge_audience_test.go holds which of a broker's addresses a stored session request is read
// back by: the origin of its own address in any spelling, and never its edge's, since the edge
// opens machine logins and nothing else (record.VerifyStoredRequestObject).
package requests

import (
	"context"
	"testing"
)

// edgeAddress is the broker's address at its edge (BROKER_EDGE_URL) in these tests.
const edgeAddress = "https://secrets.edge.example"

// TestASessionRequestSignedForTheEdgeIsAnotherBrokers: a session's request whose record names the
// broker's edge address is no request of this broker. No approver's list shows it, and the
// session's next ask of this broker for the same name ends it as a request signed for another
// broker address, as one of a third address is ended.
//
// The named revert: take the edge address for a record of every kind in
// record.VerifyStoredRequestObject, and the request is listed.
func TestASessionRequestSignedForTheEdgeIsAnotherBrokers(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	m.Audience = edgeAddress
	stale, err := m.Create(ctx, enr, signRequest(t, m, key, "asked of the edge", "DEEL_API_KEY"), "")
	m.Audience = testAudience
	if err != nil || stale.State != "pending" || stale.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) signed for %s = %+v, %v; want a pending request with a record", edgeAddress, stale, err)
	}
	m.EdgeAudience = edgeAddress
	listed, err := m.PendingForApprover(ctx, approver, PendingStates{})
	if err != nil || len(listed) != 0 {
		t.Fatalf("PendingForApprover(%s) = %+v, %v; want nothing: a session's request signed for the edge is no request of this broker", approver, listed, err)
	}
	again, err := m.Create(ctx, enr, signRequest(t, m, key, "asked again on this broker", "DEEL_API_KEY"), "")
	if err != nil || again.ID == stale.ID || again.Coalesced || again.State != "pending" {
		t.Fatalf("Create(DEEL_API_KEY) again = %+v, %v; want a new pending request, not coalesced onto %s", again, err, stale.ID)
	}
	requireEndedAsBrokerMoved(t, m, stale)
}

// TestARecordSignedForAnySpellingOfTheBrokersOriginIsListed: a record whose request object names
// the broker's own address with the scheme's default port is this broker's, so its approver's list
// shows it once the broker's setting is written without the port, as a DPoP proof of either
// spelling verifies here.
//
// The named revert: compare a stored record's aud with the broker's address as text, and the
// record is left out.
func TestARecordSignedForAnySpellingOfTheBrokersOriginIsListed(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	m.Audience = "https://secrets.internal.example:443"
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "asked with the default port", "DEEL_API_KEY"), "")
	m.Audience = "https://secrets.internal.example"
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) = %+v, %v; want a pending request with a record", req, err)
	}
	listed, err := m.PendingForApprover(ctx, approver, PendingStates{})
	if err != nil || len(listed) != 1 || listed[0].RecordID != *req.RecordID {
		t.Fatalf("PendingForApprover(%s) = %+v, %v; want the record %s", approver, listed, err, *req.RecordID)
	}
}
