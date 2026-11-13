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

// TestDeliveredToIsAPointFactTheFenceDoesNotDrop is the outbox half of dispatch://INFRA-1149's
// c-revision-fence, its point-fact paragraph: a delivered_to tuple takes no revision fence. The
// fence of R23 refuses a write older than the revision a document's row holds, which is right
// for a document's parent, where the newer edit replaced the older one. A delivery replaces
// nothing: a later event delivered does not undo an earlier one, so each delivery is a fact of
// its own, and the one thing that refuses a delivery's tuple is its repeat key, under which the
// same event's delivery is stated once. docs' TestAStaleWriteCannotAssertAParentANewerEditReplaced
// cannot reach this tuple, which only the publisher's record writes (recordBatch).
//
// The run records two deliveries out of order, as the publisher does when it batches and backs
// off: a comment on each of two issues, both routed to one role, so each comment's delivery is a
// delivered_to tuple into that role. The earlier event's sends fail, so the first scan backs it
// off and records the later event's delivery alone; the second scan sends the earlier event once
// its back-off is over and records its delivery after the later one's. Both tuples have to be in
// the store afterwards. The test then puts the earlier event back as it was before its send, so
// the third scan sends the same batch and records it again, and the store has to hold no second
// row of that delivery.
func TestDeliveredToIsAPointFactTheFenceDoesNotDrop(t *testing.T) {
	const role = "point-fact-role"
	database := storetest.Open(t)
	ctx := context.Background()
	broker := events.NewBroker()
	route := "role:" + role
	seedIssue(t, database, "T-1", &route)
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ('T-2', 'TT', 2, 'Second issue', $1, '{"kind":"user","id":"alice"}', 'V')
	`, route); err != nil {
		t.Fatalf("create issue T-2: %v", err)
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	const earlierComment, laterComment = "6a1f0c2e-1111-4c1a-9a51-0d2f3b4c5d01", "6a1f0c2e-2222-4c1a-9a51-0d2f3b4c5d02"
	earlier := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: map[string]any{"id": earlierComment, "body": "The earlier delivery."},
	})
	later := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-2"), Type: "comment.created", Actor: alice,
		Payload: map[string]any{"id": laterComment, "body": "The later delivery."},
	})
	if earlier.ID >= later.ID {
		t.Fatalf("the earlier event is %d and the later %d; the test needs them in that order", earlier.ID, later.ID)
	}

	// The claim store names the session recordingPublisher takes a role forward to, so a
	// forward is acknowledged unless the publisher fails it.
	claim := roleClaimStub{holder: roleHolder, lastSeen: time.Now().UnixMilli()}
	publisher := &recordingPublisher{failSummary: payloadSummary(earlier, "")}
	outbox := holding(t, Deps{Store: database, Publisher: publisher, Broker: broker,
		RoleClaims: claim, Sessions: claim})

	outbox.scan(ctx, unfenced{})
	requirePublished(t, database, later, true)
	requirePublished(t, database, earlier, false)
	if deliveries := roleDeliveries(t, database, role); len(deliveries) != 1 || deliveries[0] != (roleDelivery{laterComment, later.ID}) {
		t.Fatalf("after the first scan the role's deliveries read %+v, want the later event's alone: the earlier event's sends failed", deliveries)
	}

	// The back-off is over, and the publisher sends again.
	publisher.mu.Lock()
	publisher.failSummary = ""
	publisher.mu.Unlock()
	if _, err := database.Pool.Exec(ctx, `update events set next_attempt_at = now() where id = $1`, earlier.ID); err != nil {
		t.Fatalf("end event %d's back-off: %v", earlier.ID, err)
	}
	outbox.scan(ctx, unfenced{})
	requirePublished(t, database, earlier, true)
	both := []roleDelivery{{laterComment, later.ID}, {earlierComment, earlier.ID}}
	if deliveries := roleDeliveries(t, database, role); len(deliveries) != 2 || deliveries[0] != both[0] || deliveries[1] != both[1] {
		t.Fatalf("after the earlier event's send the role's deliveries read %+v, want %+v: a delivery is a point fact, and the write of event %d, recorded after event %d's, is a delivery of its own rather than a stale write the later one replaced",
			deliveries, both, earlier.ID, later.ID)
	}

	// The same batch again: the earlier event as it was before its send, so the publisher
	// sends it and records what it reached once more.
	if _, err := database.Pool.Exec(ctx, `
		update events set published_at = null, published_destinations = '{}', next_attempt_at = now()
		where id = $1
	`, earlier.ID); err != nil {
		t.Fatalf("put event %d back as it was before its send: %v", earlier.ID, err)
	}
	sent := len(publisher.all())
	outbox.scan(ctx, unfenced{})
	if len(publisher.all()) == sent {
		t.Fatalf("the third scan sent nothing, so the batch was not sent again")
	}
	requirePublished(t, database, earlier, true)
	if deliveries := roleDeliveries(t, database, role); len(deliveries) != 2 || deliveries[0] != both[0] || deliveries[1] != both[1] {
		t.Fatalf("after the same batch was sent again the role's deliveries read %+v, want %+v still: the repeat key refuses a second row of one event's delivery",
			deliveries, both)
	}
}

// roleDelivery is one delivered_to tuple into a role: the comment delivered and the event that
// delivered it.
type roleDelivery struct {
	comment string
	event   int64
}

// roleDeliveries is every delivered_to tuple into one role, in the order the log holds them. It
// fails on a tuple that does not assert, since a delivery is never retracted.
func roleDeliveries(t *testing.T, database *store.Store, role string) []roleDelivery {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select from_kind, from_id, source_event, asserted from graph_tuples
		where kind = $1 and to_kind = $2 and to_id = $3
		order by seq
	`, graphstore.TupleDeliveredTo, graphstore.KindRole, role)
	if err != nil {
		t.Fatalf("read the deliveries into role %s: %v", role, err)
	}
	defer rows.Close()
	var deliveries []roleDelivery
	for rows.Next() {
		var kind string
		var delivery roleDelivery
		var asserted bool
		if err := rows.Scan(&kind, &delivery.comment, &delivery.event, &asserted); err != nil {
			t.Fatalf("read a delivery into role %s: %v", role, err)
		}
		if kind != "comment" || !asserted {
			t.Fatalf("a delivered_to tuple into role %s is from %s %s, asserted %t; want a comment's delivery, asserted",
				role, kind, delivery.comment, asserted)
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the deliveries into role %s: %v", role, err)
	}
	return deliveries
}

// requirePublished fails unless the event's published state is want.
func requirePublished(t *testing.T, database *store.Store, event model.Event, want bool) {
	t.Helper()
	var published bool
	if err := database.Pool.QueryRow(context.Background(),
		`select published_at is not null from events where id = $1`, event.ID).Scan(&published); err != nil {
		t.Fatalf("read event %d: %v", event.ID, err)
	}
	if published != want {
		t.Fatalf("event %d is published %t, want %t", event.ID, published, want)
	}
}
