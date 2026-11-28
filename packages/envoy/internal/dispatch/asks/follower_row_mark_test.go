package asks

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A follower row an earlier build wrote names no node. Once the ask's node stands, a follow by the
// same session marks the row with that node (queueFollow's `on conflict ... do update`) and states
// the ask node's followed_by to the session once, dated by the row's own since; a second follow
// finds the row marked and states nothing more. root-deletes.txt lists that mark as
// follower-row-mark.
func TestAFollowMarksTheFollowerRowAnEarlierBuildWroteWithNoNode(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	ask := seedFollowLockAsk(t, database)
	writeFollowLockNode(t, database, graphstore.AskNode(ask, "FLOCK", nil, model.Actor{Kind: "user", ID: "alice"}, followLockAt))
	who := model.Actor{Kind: "session", ID: "follow-mark-session"}
	since := followLockAt.Add(-time.Hour)
	if _, err := database.Pool.Exec(ctx, `insert into ask_followers (ask_id, session_id, since) values ($1, $2, $3)`,
		ask, who.ID, since); err != nil {
		t.Fatalf("write the follower row as an earlier build wrote it: %v", err)
	}

	for _, follow := range []string{"the follow that marks the row", "a second follow"} {
		runFollow(t, database, follow, func(ctx context.Context, tx pgx.Tx) error {
			followed, err := Follow(ctx, tx, ask, who.ID, who, followLockAt, graphstore.NoEvent)
			if err != nil {
				return err
			}
			if followed.Added {
				t.Errorf("%s answered a new follower row, want the row the earlier build wrote", follow)
			}
			return graphstore.Caused(ctx, tx, followed.Uncaused, graphstore.NoEvent)
		})
	}

	var kind, address *string
	if err := database.Pool.QueryRow(ctx, `select node_kind, node_address from ask_followers where ask_id = $1 and session_id = $2`,
		ask, who.ID).Scan(&kind, &address); err != nil {
		t.Fatalf("read the follower row: %v", err)
	}
	if kind == nil || *kind != graphstore.KindAsk || address == nil || *address != ask {
		t.Fatalf("the follower row names the node %v %v, want the ask's node %s %s", kind, address, graphstore.KindAsk, ask)
	}
	rows, err := database.Pool.Query(ctx, `
		select at, asserted from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
	`, graphstore.KindAsk, ask, graphstore.TupleFollowedBy, graphstore.KindSession, who.ID)
	if err != nil {
		t.Fatalf("read the ask's followed_by: %v", err)
	}
	type statement struct {
		At       time.Time
		Asserted bool
	}
	stated, err := pgx.CollectRows(rows, pgx.RowToStructByPos[statement])
	if err != nil {
		t.Fatalf("read the ask's followed_by: %v", err)
	}
	if len(stated) != 1 || !stated[0].Asserted || !stated[0].At.Equal(since) {
		t.Fatalf("the ask's node states followed_by to %s as %+v, want one assertion at the row's since, %s", who.ID, stated, since)
	}
}
