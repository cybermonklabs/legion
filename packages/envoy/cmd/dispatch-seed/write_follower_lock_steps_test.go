package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestTheSeedsFollowerWriteTakesTheStoreLockOrder holds writeFollowerNodes, the write of the
// session nodes of the followers one ask batch copies, to the lock order written on
// api/inbox_read.go's markInboxReadThreads, against a real Postgres. It takes no owner row and
// appends no event, and it writes node rows alone, in one graphstore.Write: it must take them in
// ascending (kind, id) whatever order the batch lists them in. The two nodes are new, so each is
// held as a row another transaction inserted and has not committed, and they are listed with the
// higher one first.
func TestTheSeedsFollowerWriteTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	lower := graphstore.SessionNode("seed-lock-follower-a", seedAuthor, baseTime, graphstore.NoEvent)
	upper := graphstore.SessionNode("seed-lock-follower-b", seedAuthor, baseTime, graphstore.NoEvent)
	var holds []*lockstep.Hold
	for _, node := range []graphstore.Node{lower, upper} {
		hold := &lockstep.Hold{
			Name: "the node row of " + node.ID, Step: lockstep.StepNode, Taken: true,
			Kind: node.Kind, ID: node.ID,
		}
		lockstep.Begin(t, database, hold)
		if err := graphstore.Write(ctx, hold.Tx, []graphstore.Node{node}, nil); err != nil {
			t.Fatalf("hold %s: %v", hold.Name, err)
		}
		holds = append(holds, hold)
	}
	single := storetest.SingleConnection(t, database)
	var pid int64
	if err := single.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backend the seed runs on: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- pgx.BeginFunc(ctx, single.Pool, func(tx pgx.Tx) error {
			return writeFollowerNodes(ctx, tx, []graphstore.Node{upper, lower})
		})
	}()
	queued := lockstep.Follow(t, database, "writeFollowerNodes", uint32(pid), done, holds)
	if len(queued) != 2 || queued[0] != holds[0] {
		t.Fatalf("the write queued behind %s, want the node row of %s before the one of %s",
			lockstep.HoldNames(queued), lower.ID, upper.ID)
	}
	var written int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes where kind = $1 and id = any($2)`,
		graphstore.KindSession, []string{lower.ID, upper.ID}).Scan(&written); err != nil {
		t.Fatalf("read the followers' nodes: %v", err)
	}
	if written != 2 {
		t.Fatalf("the write left %d of the two followers' nodes", written)
	}
}
