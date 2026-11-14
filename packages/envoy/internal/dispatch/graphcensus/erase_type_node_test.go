package graphcensus

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

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
// it; the attribute is one of its own. The erase is handed writers that would fail the test if
// the run reached a document, since a type node is on none.
func TestTheEraseRefusesATypeNodeAndLeavesItWhole(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var attribute string
	if err := database.Pool.QueryRow(ctx,
		`select id from graph_nodes where kind = $1 and id like 'callout/%' order by id limit 1`,
		schemanodes.KindAttribute).Scan(&attribute); err != nil {
		t.Fatalf("read an attribute node of the callout supertag: %v", err)
	}
	stores := Stores{
		LiveText: func(_ context.Context, _ pgx.Tx, artifactID string, _ []string, _ string) (*LiveText, error) {
			t.Errorf("the erase of a type node read the live text of document %s", artifactID)
			return nil, nil
		},
		TermStatistics: func(context.Context, pgx.Tx, []string) (TermRecount, error) {
			return TermRecount{}, nil
		},
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
		erasure, err := Erase(ctx, database.Pool.Config().ConnString(), node.kind, node.id, true, stores)
		if err == nil || !strings.Contains(err.Error(), refusal) {
			t.Errorf("erase --node %s:%s --confirm answered %v (applied %t), want the refusal %q",
				node.kind, node.id, err, erasure != nil && erasure.Applied, refusal)
		}
		if after := readTypeNode(t, database, node.kind, node.id); after != before {
			t.Errorf("erase --node %s:%s --confirm left the store holding %+v of it, want %+v as before the run",
				node.kind, node.id, after, before)
		}
	}
}
