package outbox

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestAClaimStatedAgainUnderItsEventLeavesTheRoleHeld is D5's case of a retried event. The
// publisher dates a role's holder by the event that resolved it (deliveredGraph), and a record is
// retried, so an event recorded once can be recorded again after a later event resolved the same
// role: event 10 resolves s1, event 11 resolves s2, and the retry of event 10 resolves s1 again.
// Written row by row, the retry appends the retraction of s2 under event 10 and loses the assertion
// of s1 to the repeat key, which already holds s1 under event 10, so nobody holds the role. A claim
// is stated whole or not at all (graphstore's claimArms): the retry, whose event already stated
// this claim, appends nothing, and the role keeps the holder the later event resolved. A session's
// part_of the role it claimed is a claim of the same shape and is held to the same rule.
func TestAClaimStatedAgainUnderItsEventLeavesTheRoleHeld(t *testing.T) {
	const (
		role    = "claim-retry-role"
		before  = "claim-retry-before"
		first   = "019a1c0a-3333-7000-8000-000000000001"
		second  = "019a1c0a-3333-7000-8000-000000000002"
		earlier = int64(9430010)
		later   = int64(9430011)
	)
	ctx := context.Background()
	database := storetest.Open(t)
	deps := Deps{Store: database}
	deps.lanes = newLanes(func() {})
	at := time.Now()
	record := func(event int64, holder, parent string) {
		t.Helper()
		claims := graphWrite{claims: []graphstore.Claim{
			{
				FromKind: graphstore.KindRole, FromID: role, Kind: graphstore.TupleHeldBy,
				ToKind: graphstore.KindSession, ToID: holder, At: at, Who: graphWriter, SourceEvent: event,
			},
			partOf(graphstore.KindSession, first, graphstore.KindRole, parent, at, event),
		}}
		if _, err := recordBatch(ctx, deps, []outcome{{id: event, state: released}}, uuid.New(), claims); err != nil {
			t.Fatalf("record event %d: %v", event, err)
		}
	}
	record(earlier, first, role)
	record(later, second, before)
	record(earlier, first, role)

	if holder := currentHolder(t, database, role); holder != second {
		t.Fatalf("the role is held by %q after the retry of event %d, want %s, the holder event %d resolved: a retry that loses its assertion and keeps its retraction leaves nobody holding the role",
			holder, earlier, second, later)
	}
	want := []claimTuple{
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: first, Asserted: true},
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: first, Asserted: false},
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: second, Asserted: true},
	}
	if history := claimTuples(t, database, role); !slices.Equal(history, want) {
		t.Fatalf("the role's claim reads %+v, want %+v: the retry appends no row", history, want)
	}
	parts, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindSession, first, []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read what %s is part of: %v", first, err)
	}
	if len(parts) != 1 || parts[0].ToID != before {
		t.Fatalf("%s is part of %+v after the retry, want %s alone", first, parts, before)
	}
}

// TestAFallbackCopyThePassPublishedIsADelivery holds the fallback pass's record to R14 and R15: a
// copy of a comment the pass published to a session's own topic, once the mention's attempt past
// its lease failed, is a delivery, so the record appends its delivered_to tuple from the comment to
// the session, caused by the comment.created event the copy is the envelope of, and the session's
// node beside it. A copy the pass did not publish delivered nothing and states nothing.
//
// recordFallbackCopies is a store write call site graphstore's TestEveryStoreWriteCallSiteIsListed
// lists, and this is its lock order: one statement on a pooled connection, like recordBatch, that
// takes no owner row and appends no event, writes the session's node row first and the tuple
// after it. Both are held, each in a transaction of its own, and the record must queue behind the
// node before the tuple (lockstep.RequireTheStoreLockOrder).
func TestAFallbackCopyThePassPublishedIsADelivery(t *testing.T) {
	const (
		comment = "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f30"
		reached = "019a1c0a-4444-7000-8000-000000000001"
		missed  = "019a1c0a-4444-7000-8000-000000000002"
		created = int64(9440001)
	)
	ctx := context.Background()
	database := storetest.Open(t)
	single := storetest.SingleConnection(t, database)
	at := time.Now()
	node := graphstore.SessionNode(reached, graphWriter, at, created)
	tuple := deliveredTo(graphstore.KindComment, comment, graphstore.KindSession, reached, at, created)

	nodeHold := &lockstep.Hold{
		Name: "the reached session's node row", Step: lockstep.StepNode, Taken: true,
		Kind: node.Kind, ID: node.ID, Statement: "with hold as (",
	}
	holdStoreWrite(t, database, nodeHold, []graphstore.Node{node}, nil)
	tupleHold := &lockstep.Hold{
		Name: "the comment's delivered_to tuple", Step: lockstep.StepTuple, Taken: true, Statement: "with hold as (",
	}
	holdStoreWrite(t, database, tupleHold, nil, []graphstore.Tuple{tuple})

	var pid int64
	if err := single.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backend the record runs on: %v", err)
	}
	records := []fallbackRecord{
		{held: fallbackCopy{commentID: comment, destination: contracts.LegacyAgentSubject(reached), eventID: created, claim: at}, settled: true, published: true},
		{held: fallbackCopy{commentID: comment, destination: contracts.LegacyAgentSubject(missed), eventID: created, claim: at}, failed: true},
	}
	done := make(chan error, 1)
	go func() {
		_, err := recordFallbackCopies(ctx, Deps{Store: single}, uuid.New(), records)
		done <- err
	}()
	lockstep.Follow(t, database, "recordFallbackCopies", uint32(pid), done, []*lockstep.Hold{nodeHold, tupleHold})

	requireTuple(t, database, graphstore.KindComment, comment, graphstore.TupleDeliveredTo, graphstore.KindSession, reached, true)
	for _, check := range []struct {
		session    string
		nodes, ins int
	}{{reached, 1, 1}, {missed, 0, 0}} {
		var nodes, ins int
		if err := database.Pool.QueryRow(ctx, `
			select (select count(*) from graph_nodes where kind = $1 and id = $2),
			       (select count(*) from graph_tuples where to_kind = $1 and to_id = $2 and kind = $3 and source_event = $4)
		`, graphstore.KindSession, check.session, graphstore.TupleDeliveredTo, created).Scan(&nodes, &ins); err != nil {
			t.Fatalf("read what the record stated of %s: %v", check.session, err)
		}
		if nodes != check.nodes || ins != check.ins {
			t.Errorf("the record left %d node(s) of %s and %d delivered_to tuple(s) into it, want %d and %d",
				nodes, check.session, ins, check.nodes, check.ins)
		}
	}
	// A later pass that publishes the same copy again states nothing new under the repeat key.
	if _, err := recordFallbackCopies(ctx, Deps{Store: database}, uuid.New(), records[:1]); err != nil {
		t.Fatalf("record the copy again: %v", err)
	}
	var repeated int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples where from_id = $1 and kind = $2 and to_id = $3
	`, comment, graphstore.TupleDeliveredTo, reached).Scan(&repeated); err != nil {
		t.Fatalf("count the copy's deliveries: %v", err)
	}
	if repeated != 1 {
		t.Fatalf("the copy recorded twice holds %d delivered_to tuple(s), want one", repeated)
	}
}

// holdStoreWrite writes nodes and tuples in hold's own transaction and leaves it uncommitted: a
// writer's insert of the same key waits to learn whether this row commits.
func holdStoreWrite(t *testing.T, database *store.Store, hold *lockstep.Hold, nodes []graphstore.Node, tuples []graphstore.Tuple) {
	t.Helper()
	lockstep.Begin(t, database, hold)
	if err := graphstore.Write(context.Background(), hold.Tx, nodes, tuples); err != nil {
		t.Fatalf("hold %s: %v", hold.Name, err)
	}
}
