package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// claimTuple is one tuple of a role's claim as this test compares them, in the order the log
// holds them.
type claimTuple struct {
	Kind, ToKind, ToID string
	Asserted           bool
}

// TestTheStoreHoldsNodesAndAppendedTuples is this half of dispatch://INFRA-1149's c-store-tuples:
// the facts a resolved role holder states, which a shell driver cannot reach - the holder lives
// in Envoy's role claim bucket, and this pod's harness has none - so the check words them as
// driven on the write path in process. The claim store is injected (roleClaimStub), so the run
// needs no NATS at all: a resolution that reads a holder, and one that reads the role unclaimed.
//
// The other half of the check, which drives a comment through the real write path and reads the
// node, its supertag, its attribute values and the event that caused each tuple, is the test of
// the same name in internal/dispatch/api. One name, two write paths: the store writer is reached
// from a request there and from the publisher's record here, and neither package can run the
// other's path.
//
// What this half reads: the role's `held_by` tuple to the session the forward resolved, the
// session `part_of` the role it claimed, the role's page under the role, and then - after a
// resolution that reads the role unclaimed - the retraction that ends the claim, with the
// earlier claim still readable in the history, in the order the two were written. Nothing here
// updates a row: both facts are rows of their own.
func TestTheStoreHoldsNodesAndAppendedTuples(t *testing.T) {
	const (
		key    = "T-1"
		role   = "store-tuples-role"
		holder = "019a1c0a-1111-7000-8000-000000000001"
	)
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "role:" + role
	seedIssue(t, database, key, &route)

	// A resolution that reads a live holder: the claim store names it, and its record was
	// written now, so the holder is live by every window the resolution judges it with.
	held := roleClaimStub{holder: holder, lastSeen: time.Now().UnixMilli()}
	deps := Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker,
		RoleClaims: held, Sessions: held}
	appendEvent(t, database, broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f10", IssueKey: new(key), Body: "for the role"},
	})
	holding(t, deps).scan(context.Background(), unfenced{})

	claim := claimTuples(t, database, role)
	want := []claimTuple{{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: holder, Asserted: true}}
	if len(claim) != 1 || claim[0] != want[0] {
		t.Fatalf("the role's claim reads %+v, want %+v", claim, want)
	}
	requireTuple(t, database, graphstore.KindSession, holder, graphstore.TuplePartOf,
		graphstore.KindRole, role, true)
	requireTuple(t, database, graphstore.KindPage, graphstore.RolePageID(role), graphstore.TuplePartOf,
		graphstore.KindRole, role, true)

	// A resolution that reads the role unclaimed ends the claim. The holder is gone from the
	// claim store, which is what a session that stopped leaves behind it.
	unheld := roleClaimStub{lastSeen: time.Now().UnixMilli()}
	deps = Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker,
		RoleClaims: unheld, Sessions: unheld}
	appendEvent(t, database, broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f11", IssueKey: new(key), Body: "for a role nobody holds"},
	})
	holding(t, deps).scan(context.Background(), unfenced{})

	history := claimTuples(t, database, role)
	ended := []claimTuple{
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: holder, Asserted: true},
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: holder, Asserted: false},
	}
	if len(history) != len(ended) || history[0] != ended[0] || history[1] != ended[1] {
		t.Fatalf("the claim's history reads %+v, want the claim asserted and then retracted: %+v",
			history, ended)
	}
	if current := currentHolder(t, database, role); current != "" {
		t.Fatalf("the role still holds %s after the resolution that read it unclaimed", current)
	}
}

// claimTuples is every held_by tuple of one role, in the order the log holds them.
func claimTuples(t *testing.T, database *store.Store, role string) []claimTuple {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select kind, to_kind, to_id, asserted from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3
		order by seq
	`, graphstore.KindRole, role, graphstore.TupleHeldBy)
	if err != nil {
		t.Fatalf("read the claims of %s: %v", role, err)
	}
	defer rows.Close()
	var claims []claimTuple
	for rows.Next() {
		var claim claimTuple
		if err := rows.Scan(&claim.Kind, &claim.ToKind, &claim.ToID, &claim.Asserted); err != nil {
			t.Fatalf("scan a claim of %s: %v", role, err)
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the claims of %s: %v", role, err)
	}
	return claims
}

// currentHolder is the session a role holds now, read the way every current read reads a
// relation: the newest tuple per far end, kept while that newest one asserts.
func currentHolder(t *testing.T, database *store.Store, role string) string {
	t.Helper()
	tuples, err := graphstore.CurrentOut(context.Background(), database.Pool,
		graphstore.KindRole, role, []string{graphstore.TupleHeldBy})
	if err != nil {
		t.Fatalf("read the holder of %s: %v", role, err)
	}
	if len(tuples) == 0 {
		return ""
	}
	return tuples[0].ToID
}

// requireTuple fails unless the store holds that one tuple, asserted as stated.
func requireTuple(t *testing.T, database *store.Store, fromKind, fromID, kind, toKind, toID string, asserted bool) {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		  and asserted = $6
	`, fromKind, fromID, kind, toKind, toID, asserted).Scan(&rows); err != nil {
		t.Fatalf("read the %s tuple of %s %s: %v", kind, fromKind, fromID, err)
	}
	if rows != 1 {
		t.Fatalf("the store holds %d %s tuple(s) from %s %s to %s %s, want one",
			rows, kind, fromKind, fromID, toKind, toID)
	}
}
