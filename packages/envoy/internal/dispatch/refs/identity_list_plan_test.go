package refs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The person whose list the test reads, the deliveries the store holds into them - the far end of
// each of their mail, which no arm of graph_edges answers - and the tuples it holds out of a
// document's blocks, each block's part_of, which every store holds many of and no person's list
// answers either: an arm keyed by `from_kind = 'block'` would read each of them.
const (
	listedPerson     = "person-1@example.com"
	listedDeliveries = 5000
	listedBlockParts = 5000
)

// A person's list (Edges inbound at the person) reads the store, graph_tuples, only through
// searches keyed by that person's address, and reads only what it answers. A person is the far end
// of far more tuples than any arm answers - a delivered_to for each of their mail, an authored_by
// for each of their writes - so an arm that reads every tuple into the person and filters them
// costs a read of the list their whole history, and an arm that reads its tuples by another key and
// tests the person's address on each costs it every tuple of that key (migration 20261126045004
// says how each arm avoids both). Over a store holding many deliveries into one person, many
// tuples out of a document's blocks and three comments mentioning that person, whose rows carry
// the marker, the list answers the three mentions, and its plan - sequential scans off, as every
// plan test here reads one, and analyzed, so each search says what it read - holds:
//   - no read of graph_tuples whole, and every search of it a search of graph_tuples_as_of_in
//     keyed by (to_kind, to_id) and naming the person's address;
//   - one search of the identity arm, bound by kind = 'mentions', reading the three mentions;
//   - every other search - the tuple arm's and the item block arm's; the reply arms, whose far
//     kind is a literal, are no part of the plan - reading no tuple: the rows its index answered
//     and its filter refused are none.
func TestAPersonsListKeysEveryReadOfTheStoreOnThePerson(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	mentioning := seedAMailedPerson(t, database)

	recorder := &recordingQueryer{inner: database.Pool}
	edges, err := Edges(ctx, recorder, model.Actor{Kind: "user", ID: "alice"},
		Query{Direction: "in", Kind: graphstore.KindPerson, ID: listedPerson})
	if err != nil {
		t.Fatalf("read the list of %s: %v", listedPerson, err)
	}
	listed := make([]string, 0, len(edges))
	for _, edge := range edges {
		listed = append(listed, edge.Kind+" from "+edge.Node.Kind+":"+edge.Node.ID)
	}
	want := make([]string, 0, len(mentioning))
	for _, comment := range mentioning {
		want = append(want, graphstore.TupleMentions+" from "+graphstore.KindComment+":"+comment)
	}
	slices.Sort(listed)
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Fatalf("the list of %s answers %v, want the three mentions %v, so its plan below would be read over a list that is wrong",
			listedPerson, listed, want)
	}

	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plan: %v", err)
	}
	defer connection.Release()
	plan := analyzedPlan(t, connection, recorder.statements[0], recorder.arguments[0]...)
	if explaintest.SeqScansRelation(t, plan, "graph_tuples") {
		t.Errorf("the list of %s reads graph_tuples whole:\n%s", listedPerson, plan)
	}
	keys := explaintest.NewIndexKeys(database.Pool)
	person := "(to_id = '" + listedPerson + "'::text)"
	identity := 0
	for _, node := range explaintest.Nodes(t, plan) {
		if !strings.HasPrefix(node.Index, "graph_tuples") {
			continue
		}
		if strings.Contains(node.IndexCond, "(kind = '"+graphstore.TupleRepliesTo+"'::text)") {
			t.Errorf("the list of %s runs a reply arm's search %s under %q, want none: a reply arm's far kind is a literal no person's is, so a read of a person drops it:\n%s",
				listedPerson, node, node.IndexCond, plan)
			continue
		}
		if node.Index != "graph_tuples_as_of_in" || !explaintest.CondNamesFirstKey(node.IndexCond, keys.FirstKey(t, node.Index)) ||
			!strings.Contains(node.IndexCond, person) {
			t.Errorf("the list of %s reads %s under %q, want a search of graph_tuples_as_of_in keyed by (to_kind, to_id) and naming %s:\n%s",
				listedPerson, node, node.IndexCond, person, plan)
			continue
		}
		read := (node.Rows + node.RowsRemovedByFilter) * node.Loops
		if strings.Contains(node.IndexCond, "(kind = '"+graphstore.TupleMentions+"'::text)") {
			identity++
			if read != float64(len(mentioning)) {
				t.Errorf("the identity arm's search %q reads %.0f tuples, want the %d mentions of %s:\n%s",
					node.IndexCond, read, len(mentioning), listedPerson, plan)
			}
			continue
		}
		if read != 0 {
			t.Errorf("the search %q reads %.0f tuples into %s (%.0f answered, %.0f refused by its filter, over %.0f loops), want none: no arm but the identity arm answers a tuple the store holds into this person:\n%s",
				node.IndexCond, read, listedPerson, node.Rows, node.RowsRemovedByFilter, node.Loops, plan)
		}
	}
	if identity != 1 {
		t.Errorf("the list of %s holds %d searches bound by kind = 'mentions', want the identity arm's one:\n%s", listedPerson, identity, plan)
	}
}

// seedAMailedPerson writes three comments on an issue mentioning the person - each with its node,
// its mention tuple, appended with the person's node by graphstore.ToPerson, and its row marked,
// as this release's writer of a comment leaves them - and listedDeliveries deliveries into the
// person and listedBlockParts tuples out of a document's blocks, each written as the store holds
// it with no source the list could answer, and answers the three comments' ids. It analyzes
// graph_tuples after, as autovacuum has the store a person's list is read from.
func seedAMailedPerson(t *testing.T, database *store.Store) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the seed: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	createReferenceIssue(t, tx, "ACME-1", "ACME")
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	nodes := make([]graphstore.Node, 0, 3)
	var tuples []graphstore.Tuple
	mentioning := make([]string, 0, 3)
	for part := range 3 {
		comment := scanFixture(t, tx, "write a comment mentioning the person", `
			insert into comments (issue_key, author, body)
			values ('ACME-1', '{"kind":"user","id":"alice"}', $1)
			returning id::text
		`, fmt.Sprintf("Over to dispatch://person/%s, part %d.", listedPerson, part+1))
		node, _ := graphstore.CommentFacts(nil, comment, alice, at, graphstore.NoEvent, "", "", "ACME", nil)
		nodes = append(nodes, node)
		tuples = append(tuples, graphstore.Tuple{
			FromKind: graphstore.KindComment, FromID: comment, Kind: graphstore.TupleMentions,
			ToKind: graphstore.KindPerson, ToID: listedPerson, At: at, Who: alice, Asserted: true,
		})
		mentioning = append(mentioning, comment)
	}
	if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
		t.Fatalf("write the comments mentioning %s: %v", listedPerson, err)
	}
	if err := graphstore.ToPerson(ctx, tx, tuples); err != nil {
		t.Fatalf("write the mentions of %s: %v", listedPerson, err)
	}
	execFixture(t, tx, "mark the mentioning comments", `
		update comments set node_kind = 'comment', node_address = id::text, tuples_stored = true
		where id = any($1::uuid[])
	`, mentioning)
	execFixture(t, tx, "deliver the person's mail", `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select 'message', '00000000-0000-4000-8000-' || lpad(n::text, 12, '0'), 'delivered_to', 'person', $1,
		       $2::timestamptz, '{"kind":"user","id":"alice"}'::jsonb, $3, true
		from generate_series(1, $4::int) n
	`, listedPerson, at, graphstore.NoEvent, listedDeliveries)
	execFixture(t, tx, "place a document's blocks", `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select 'block', 'ACME-1/spec#b' || n, 'part_of', 'artifact', 'ACME-1/spec',
		       $1::timestamptz, '{"kind":"user","id":"alice"}'::jsonb, $2, true
		from generate_series(1, $3::int) n
	`, at, graphstore.NoEvent, listedBlockParts)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the seed: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `analyze graph_tuples`); err != nil {
		t.Fatalf("analyze graph_tuples: %v", err)
	}
	return mentioning
}
