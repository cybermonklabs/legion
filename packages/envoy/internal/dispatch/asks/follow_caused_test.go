package asks

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAFollowersNodeAndItsBaseTuplesNameOneEvent is this change's R1 and R8 for the
// node a follow makes before its caller's event exists. The follow writes the session's node at
// the sentinel and graphstore.Caused moves that row onto the event the caller appends after it.
// No tuple may be updated to learn its cause, so a base attribute stated in the follow's own
// statement would stand for ever under event 0 while the row it projects named the caller's
// event: a row whose own log says no event recorded it. The follow therefore states neither
// there and answers both for the caller to append under that event.
//
// A caller that appends no event causes the follow with graphstore.NoEvent all the same, and
// the node and its two base attributes then name the sentinel together, which is the shape a
// backfill's node and a seed's node have.
func TestAFollowersNodeAndItsBaseTuplesNameOneEvent(t *testing.T) {
	database := storetest.Open(t)
	ask := seedFollowLockAsk(t, database)

	for _, run := range []struct {
		name    string
		session string
		cause   int64
	}{
		{"a follow whose caller appends an event", "follow-caused-session", followLockEvent},
		{"a follow whose caller appends none", "follow-uncaused-session", graphstore.NoEvent},
	} {
		t.Run(run.name, func(t *testing.T) {
			who := model.Actor{Kind: "session", ID: run.session}
			runFollow(t, database, run.name, func(ctx context.Context, tx pgx.Tx) error {
				followed, err := Follow(ctx, tx, ask, run.session, who, followLockAt, graphstore.NoEvent)
				if err != nil {
					return err
				}
				return graphstore.Caused(ctx, tx, followed.Uncaused, run.cause)
			})
			requireOneCause(t, database, run.session, run.cause)
		})
	}
}

// runFollow runs write in a transaction of its own and commits it.
func runFollow(t *testing.T, database *store.Store, what string, write func(context.Context, pgx.Tx) error) {
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

// requireOneCause holds a session's node and its base attributes to naming the one event: the
// node row's source_event is cause, and so is each base attribute tuple's, with exactly one of
// its visibility and of its state, and none of a project or parties, which a session's node
// does not carry.
func requireOneCause(t *testing.T, database *store.Store, session string, cause int64) {
	t.Helper()
	ctx := context.Background()
	var node int64
	if err := database.Pool.QueryRow(ctx,
		`select source_event from graph_nodes where kind = $1 and id = $2`,
		graphstore.KindSession, session).Scan(&node); err != nil {
		t.Fatalf("read the node of %s: %v", session, err)
	}
	if node != cause {
		t.Errorf("the node row of session %s names event %d, want the cause its follow was given, %d", session, node, cause)
	}
	for _, attribute := range graphstore.BaseAttributes() {
		rows, err := database.Pool.Query(ctx, `
			select source_event from graph_tuples
			where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4
			order by seq
		`, graphstore.KindSession, session, attribute, graphstore.KindValue)
		if err != nil {
			t.Fatalf("read the %s tuples of session %s: %v", attribute, session, err)
		}
		events, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			t.Fatalf("read the %s tuples of session %s: %v", attribute, session, err)
		}
		carried := attribute == graphstore.AttributeVisibility || attribute == graphstore.AttributeState
		switch {
		case carried && (len(events) != 1 || events[0] != node):
			t.Errorf("session %s states %s under the events %v, want exactly one under %d, the event its node row names",
				session, attribute, events, node)
		case !carried && len(events) != 0:
			t.Errorf("session %s states %s under the events %v, which a session's node does not carry", session, attribute, events)
		}
	}
}
