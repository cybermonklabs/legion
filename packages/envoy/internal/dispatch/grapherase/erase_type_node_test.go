package grapherase

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// typeNodeHeld is what the store holds of one node: whether its row is there, what that row
// says, and how many tuples run out of it and into it.
type typeNodeHeld struct {
	row             bool
	attributes      string
	out, in, erased int64
}

// readTypeNode reads what the store holds of node kind:id, and how many erasures name it.
func readTypeNode(t *testing.T, database *store.Store, kind, id string) typeNodeHeld {
	t.Helper()
	var held typeNodeHeld
	if err := database.Pool.QueryRow(context.Background(), `
		select exists (select 1 from graph_nodes where kind = $1 and id = $2),
		       coalesce((select attributes::text from graph_nodes where kind = $1 and id = $2), ''),
		       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2),
		       (select count(*) from graph_tuples where to_kind = $1 and to_id = $2),
		       (select count(*) from graph_erasures where node_kind = $1 and node_id = $2)
	`, kind, id).Scan(&held.row, &held.attributes, &held.out, &held.in, &held.erased); err != nil {
		t.Fatalf("read what the store holds of %s:%s: %v", kind, id, err)
	}
	return held
}

// R6: no route and no command creates, retitles or deletes a type node. `envoy-dispatch erase`
// deletes a node and every tuple at it, so the erase refuses a supertag and an attribute, the
// two kinds a type is made of, before it writes anything: confirmed, a run over either answers
// the refusal, and the node's row, its tuples and the tuples into it are all still there, with
// no erasure recorded.
//
// The supertag is `callout`, which holds attributes, so tuples run into it as well as out of
// it; the attribute is one of its own. The test fails if the run read the live state of a
// document, since a type node is on none.
func TestTheEraseRefusesATypeNodeAndLeavesItWhole(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var attribute string
	if err := database.Pool.QueryRow(ctx,
		`select id from graph_nodes where kind = $1 and id like 'callout/%' order by id limit 1`,
		schemanodes.KindAttribute).Scan(&attribute); err != nil {
		t.Fatalf("read an attribute node of the callout supertag: %v", err)
	}
	const refusal = "a type node is data the block schema seeds, and no command deletes it"
	for _, node := range []struct{ kind, id string }{
		{schemanodes.KindSupertag, "callout"},
		{schemanodes.KindAttribute, attribute},
	} {
		before := readTypeNode(t, database, node.kind, node.id)
		if !before.row || before.out == 0 {
			t.Fatalf("the seed holds %s:%s as %+v, want a node with tuples, so this test would prove nothing", node.kind, node.id, before)
		}
		if node.kind == schemanodes.KindSupertag && before.in == 0 {
			t.Fatalf("no tuple runs into %s:%s, so this test would not read the tuples an erase deletes from the far side", node.kind, node.id)
		}
		erasure, err := Erase(ctx, database.Pool.Config().ConnString(), eraseServerURL, node.kind, node.id, "any digest", 0)
		if err == nil || !strings.Contains(err.Error(), refusal) {
			t.Errorf("erase --node %s:%s --confirm answered %v (applied %t), want the refusal %q",
				node.kind, node.id, err, erasure != nil && erasure.Applied, refusal)
		}
		if erasure != nil {
			for _, document := range erasure.Documents {
				t.Errorf("the erase of a type node read the live text of document %s", document.Document)
			}
		}
		if after := readTypeNode(t, database, node.kind, node.id); after != before {
			t.Errorf("erase --node %s:%s --confirm left the store holding %+v of it, want %+v as before the run",
				node.kind, node.id, after, before)
		}
	}
}

// A role, a page and a session hold no text anybody wrote, so no erasure is owed for one, and the
// event log keeps addressing each, so deleting one would leave the census a difference no erase
// can close. The erase refuses all three before it opens a connection: confirmed, a run over each
// answers the refusal even given a database URL nothing listens on, and over the real database the
// node's row, its tuples and the tuples into it are all still there, with no erasure recorded.
//
// The seed is a role with its page, and a session that claimed the role, written as the store
// writer writes them, so tuples run into the role and out of the page and the session.
func TestTheEraseRefusesARoleAPageAndASessionAndLeavesThemWhole(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	const role, session = "erase-refusal", "erase-refusal-session"
	page := graphstore.RolePageID(role)
	seedAgents(t, database, role, page, session)

	const refusal = "holds no text anybody wrote, so no erasure is owed for it, and the event log keeps addressing it"
	for _, node := range []struct{ kind, id string }{
		{graphstore.KindRole, role},
		{graphstore.KindPage, page},
		{graphstore.KindSession, session},
	} {
		before := readTypeNode(t, database, node.kind, node.id)
		if !before.row || before.out+before.in == 0 {
			t.Fatalf("the seed holds %s:%s as %+v, want a node with tuples, so this test would prove nothing", node.kind, node.id, before)
		}
		for _, url := range []string{"postgres://nobody@127.0.0.1:1/nothing?connect_timeout=1", database.Pool.Config().ConnString()} {
			erasure, err := Erase(ctx, url, eraseServerURL, node.kind, node.id, "any digest", 0)
			if err == nil || !strings.Contains(err.Error(), "refuses "+node.kind+":"+node.id+": a "+node.kind+" node "+refusal) {
				t.Errorf("erase --node %s:%s --confirm over %s answered %v (applied %t), want the refusal %q before any connection",
					node.kind, node.id, url, err, erasure != nil && erasure.Applied, refusal)
			}
		}
		if after := readTypeNode(t, database, node.kind, node.id); after != before {
			t.Errorf("erase --node %s:%s --confirm left the store holding %+v of it, want %+v as before the run",
				node.kind, node.id, after, before)
		}
	}
}

// An erase with no dashboard origin refuses before it opens a connection, as rebuild-refs
// refuses one: a redacted excerpt it stored would be cut otherwise than the server's writers cut
// the block's own (refs.BlockExcerpt).
func TestTheEraseRefusesAnEmptyServerURLBeforeAnyConnection(t *testing.T) {
	for _, serverURL := range []string{"", "  "} {
		erasure, err := Erase(context.Background(), "postgres://nobody@127.0.0.1:1/nothing?connect_timeout=1", serverURL,
			commentNodeKind, "00000000-0000-4000-8000-000000000001", "", 0)
		if erasure != nil || err == nil || !strings.Contains(err.Error(), "dispatch.server_url is required") {
			t.Errorf("erase with the server URL %q answered %+v, %v; want the refusal naming dispatch.server_url and no erasure", serverURL, erasure, err)
		}
	}
}

// seedAgents writes role, its page and session as nodes, with the page's part_of into the role
// and the session's part_of into the role it claimed.
func seedAgents(t *testing.T, database *store.Store, role, page, session string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the agents' write: %v", err)
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	who := model.Actor{Kind: "session", ID: session}
	partOf := func(kind, id string) graphstore.Tuple {
		return graphstore.Tuple{FromKind: kind, FromID: id, Kind: graphstore.TuplePartOf, ToKind: graphstore.KindRole, ToID: role,
			At: now, Who: who, SourceEvent: graphstore.NoEvent, Asserted: true}
	}
	if err := graphstore.Write(ctx, tx, []graphstore.Node{
		graphstore.PageNode(page, who, now, graphstore.NoEvent),
		graphstore.RoleNode(role, who, now, graphstore.NoEvent),
		graphstore.SessionNode(session, who, now, graphstore.NoEvent),
	}, []graphstore.Tuple{partOf(graphstore.KindPage, page), partOf(graphstore.KindSession, session)}); err != nil {
		t.Fatalf("write the agents: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the agents' write: %v", err)
	}
}
