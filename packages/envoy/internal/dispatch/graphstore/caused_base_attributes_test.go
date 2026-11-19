package graphstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestASettlesNodeAndItsBaseTuplesNameOneEvent is this change's R1 and R8 for a node
// whose cause comes after it. A delivery's settle writes the session's node before its receipt
// exists, so the node goes in at the sentinel and Caused moves its row onto the receipt once
// that is appended. No tuple may be updated to learn its cause, so a base attribute stated at
// the sentinel would stand for ever under event 0 while the row it projects named the receipt:
// a row whose own log says no event recorded it. The settle therefore states no base attribute
// in its node statement, marks the node as made (nodeMark), and appends every base attribute the
// node carries under the receipt (WriteArmsUncaused, QueueCaused).
func TestASettlesNodeAndItsBaseTuplesNameOneEvent(t *testing.T) {
	database := storetest.Open(t)
	who := model.Actor{Kind: "user", ID: "alice"}
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	const receipt = int64(9500001)
	const session = "caused-base-attributes"

	runInOneTransaction(t, database, "the settle of a delivery attempt", func(ctx context.Context, tx pgx.Tx) error {
		reached, err := Reached(ctx, tx, model.DeliveryStateQueued, session, who, at)
		if err != nil {
			return err
		}
		return Caused(ctx, tx, reached, receipt)
	})

	requireOneCause(t, database, KindSession, session, receipt)
}

// runInOneTransaction runs write in a transaction of its own and commits it.
func runInOneTransaction(t *testing.T, database *store.Store, what string, write func(context.Context, pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s: %v", what, err)
	}
	defer tx.Rollback(ctx)
	if err := write(ctx, tx); err != nil {
		t.Fatalf("run %s: %v", what, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", what, err)
	}
}

// requireOneCause holds a node and the base attributes it carries to naming the one event: the
// node row's source_event is cause, and so is each base attribute tuple's, with exactly one of
// each attribute the node carries - visibility and state always, and its project or its parties
// where its row holds one - and none of an attribute it does not carry.
func requireOneCause(t *testing.T, database *store.Store, kind, id string, cause int64) {
	t.Helper()
	ctx := context.Background()
	var node int64
	var project, parties bool
	if err := database.Pool.QueryRow(ctx,
		`select source_event, project_key is not null, parties is not null from graph_nodes where kind = $1 and id = $2`,
		kind, id).Scan(&node, &project, &parties); err != nil {
		t.Fatalf("read the node %s/%s: %v", kind, id, err)
	}
	if node != cause {
		t.Errorf("the node row of %s/%s names event %d, want the event that caused the write, %d", kind, id, node, cause)
	}
	carried := map[string]bool{AttributeVisibility: true, AttributeState: true, AttributeProject: project, AttributeParties: parties}
	for _, attribute := range BaseAttributes() {
		rows, err := database.Pool.Query(ctx, `
			select source_event from graph_tuples
			where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4
			order by seq
		`, kind, id, attribute, KindValue)
		if err != nil {
			t.Fatalf("read the %s tuples of %s/%s: %v", attribute, kind, id, err)
		}
		events, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			t.Fatalf("read the %s tuples of %s/%s: %v", attribute, kind, id, err)
		}
		switch {
		case carried[attribute] && (len(events) != 1 || events[0] != node):
			t.Errorf("%s/%s states %s under the events %v, want exactly one under %d, the event its node row names",
				kind, id, attribute, events, node)
		case !carried[attribute] && len(events) != 0:
			t.Errorf("%s/%s states %s under the events %v, which its row does not carry", kind, id, attribute, events)
		}
	}
}
