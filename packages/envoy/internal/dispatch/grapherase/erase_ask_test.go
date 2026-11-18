package grapherase

import (
	"context"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// askFollower is the session that follows the ask seedMarkedAsk writes.
const askFollower = "erase-ask-session"

// seedMarkedAsk writes an ask alice asked on issue CORE-1, anchored to its document, and the
// follow of it by the session askFollower as the follow route records one - the follower row and
// the ask.follower_added naming the session - as a build before the store left them, then runs the
// backfill over them (graphcensus.Backfill), which writes the ask's node, states its relations, its
// author and its followed_by into the session, and marks the asks row and the follower row with
// the node (migration 20261116094058). It answers the ask's id.
func seedMarkedAsk(t *testing.T, database *store.Store) string {
	t.Helper()
	ctx := context.Background()
	document := seedPreStoreDocument(t, database)
	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, anchor)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Ship it this week?', jsonb_build_object('artifact_id', $1::text))
		returning id::text
	`, document.artifactID).Scan(&ask); err != nil {
		t.Fatalf("write the ask: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into ask_followers (ask_id, session_id) values ($1::uuid, $2)`,
		ask, askFollower); err != nil {
		t.Fatalf("write the ask's follower: %v", err)
	}
	appendEvent(t, database, "CORE-1", "ask.follower_added",
		`{"ask_id":"`+ask+`","session_id":"`+askFollower+`","by":{"kind":"session","id":"`+askFollower+`"}}`)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	var asks, followers, followedBy int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from asks where id::text = $1::text and node_kind = 'ask' and node_address = $1),
		       (select count(*) from ask_followers where ask_id::text = $1 and node_kind = 'ask' and node_address = $1),
		       (select count(*) from graph_tuples where from_kind = 'ask' and from_id = $1 and kind = $2
		          and to_kind = 'session' and to_id = $3)
	`, ask, graphstore.TupleFollowedBy, askFollower).Scan(&asks, &followers, &followedBy); err != nil {
		t.Fatalf("read the ask's marks: %v", err)
	}
	if asks != 1 || followers != 1 || followedBy != 1 {
		t.Fatalf("the backfill marked %d asks row(s) and %d follower row(s) and stated %d followed_by, want one of each, so this test would prove nothing",
			asks, followers, followedBy)
	}
	return ask
}

// An ask's node is projected onto its asks row and onto each of its follower rows, and each names
// the node through a foreign key to graph_nodes (asks_node, ask_followers_node, migration
// 20261116094058), so an erase that left either row would have its delete of the node refused.
// The erase of a marked ask lists both rows, deletes them, the tuples out of the node - its
// followed_by into the session that follows it among them - and the node, and leaves nothing that
// names ask:<id>. The session is a node the erase never deletes, and stays with the tuples out of
// it, named by the ask.follower_added that recorded the follow. The asks row is a projection like
// a comment's row: while the ask stands, the erase of alice, who asked it, refuses, naming the ask.
// Once the ask is erased the census reads zero, holding alice to her node by the record of that
// erase, which names her as the ask's writer (Erasure.Writers); then she is erased, and the census
// reads zero again, a record written before her own erase holding her to nothing.
func TestTheEraseRemovesAMarkedAskAndItsFollowerRows(t *testing.T) {
	database := storetest.Open(t)
	ask := seedMarkedAsk(t, database)
	requireCensusZero(t, database, "before the erase")
	requireRefused(t, database, graphstore.KindPerson, "alice", graphstore.KindAsk+":"+ask+" states "+graphstore.TupleAuthoredBy)
	session := readTypeNode(t, database, graphstore.KindSession, askFollower)
	var intoSession int64
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples where from_kind = 'ask' and from_id = $1 and to_kind = 'session' and to_id = $2
	`, ask, askFollower).Scan(&intoSession); err != nil {
		t.Fatalf("count the tuples of ask:%s into the session: %v", ask, err)
	}

	listing := dryRun(t, database, graphstore.KindAsk, ask)
	if !listed(listing, "delete asks (id)=("+ask+")") || !listed(listing, "delete ask_followers (ask_id, session_id)=("+ask+","+askFollower+")") {
		t.Errorf("the listing of ask:%s reads %q, want its asks row and its follower row named for deletion", ask, listing.Listing)
	}
	requireErased(t, database, graphstore.KindAsk, ask)

	var naming int64
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from graph_nodes where kind = 'ask' and id = $1::text)
		     + (select count(*) from graph_tuples where (from_kind = 'ask' and from_id = $1) or (to_kind = 'ask' and to_id = $1))
		     + (select count(*) from asks where id::text = $1 or (node_kind = 'ask' and node_address = $1))
		     + (select count(*) from ask_followers where ask_id::text = $1 or (node_kind = 'ask' and node_address = $1))
	`, ask).Scan(&naming); err != nil {
		t.Fatalf("count what names ask:%s: %v", ask, err)
	}
	if naming != 0 {
		t.Errorf("after the erase %d row(s) of the node, its tuples, its asks row or its follower rows name ask:%s, want none", naming, ask)
	}
	after := readTypeNode(t, database, graphstore.KindSession, askFollower)
	if !after.row || after.out != session.out || intoSession == 0 || after.in != session.in-intoSession {
		t.Errorf("the erase left the session %+v, want its node and the tuples out of it standing and the %d tuple(s) of the ask into it gone from %+v",
			after, intoSession, session)
	}
	requireCensusZero(t, database, "after the ask's erase")
	requireErased(t, database, graphstore.KindPerson, "alice")
	requireCensusZero(t, database, "after the erases")
}
