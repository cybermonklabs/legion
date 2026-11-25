package refs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The two statements TreeFactsOf read before its one read (treeFactsStatement), kept here alone as
// the reference that read is held to: twoReadsValuesStatement, the newest value tuple of each
// attribute out of each node, kept while it asserts; and twoReadsRelationsStatement, the newest
// tuple of each kind and far end out of each node, kept while it asserts, in the order its
// distinct-on sort left them.
var (
	twoReadsValuesStatement = `
	select node.position, newest.kind, newest.to_id
	from unnest($1::text[], $2::text[]) with ordinality as node (kind, id, position)
	cross join lateral (
		select distinct on (t.kind) t.kind, t.to_id, t.asserted
		from graph_tuples t
		where t.from_kind = node.kind and t.from_id = node.id and t.to_kind = '` + graphstore.KindValue + `' and ` + cutBound(`$3`, `$4`) + `
		order by t.kind, t.at desc, t.seq desc
	) newest
	where newest.asserted
	order by node.position`

	twoReadsRelationsStatement = `
	select node.position, newest.kind, newest.to_kind, newest.to_id
	from unnest($1::text[], $2::text[]) with ordinality as node (kind, id, position)
	cross join lateral (
		select distinct on (t.kind, t.to_kind, t.to_id) t.kind, t.to_kind, t.to_id, t.asserted
		from graph_tuples t
		where t.from_kind = node.kind and t.from_id = node.id and t.to_kind <> '` + graphstore.KindValue + `' and ` + cutBound(`$3`, `$4`) + `
		order by t.kind, t.to_kind, t.to_id, t.at desc, t.seq desc
	) newest
	where newest.asserted
	order by node.position`
)

// treeFactsOfTwoReads is TreeFactsOf as it read before its one read: the values, then the
// relations, each in a statement of its own.
func treeFactsOfTwoReads(ctx context.Context, q Queryer, nodes []Target, cut TreeCut) (map[Target]TreeFacts, error) {
	facts := make(map[Target]TreeFacts, len(nodes))
	if len(nodes) == 0 {
		return facts, nil
	}
	kinds, ids := targetColumns(nodes)
	seq, at := cut.bounds()
	rows, err := q.Query(ctx, twoReadsValuesStatement, kinds, ids, seq, at)
	if err != nil {
		return nil, fmt.Errorf("read the values of %d nodes %s: %w", len(nodes), cut, err)
	}
	for rows.Next() {
		var position int
		var attribute, value string
		if err := rows.Scan(&position, &attribute, &value); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read a value %s: %w", cut, err)
		}
		node := nodes[position-1]
		held := facts[node]
		if held.Values == nil {
			held.Values = map[string]string{}
		}
		held.Values[attribute] = value
		facts[node] = held
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the values of %d nodes %s: %w", len(nodes), cut, err)
	}
	rows, err = q.Query(ctx, twoReadsRelationsStatement, kinds, ids, seq, at)
	if err != nil {
		return nil, fmt.Errorf("read the relations of %d nodes %s: %w", len(nodes), cut, err)
	}
	defer rows.Close()
	for rows.Next() {
		var position int
		var relation TreeRelation
		if err := rows.Scan(&position, &relation.Kind, &relation.To.Kind, &relation.To.ID); err != nil {
			return nil, fmt.Errorf("read a relation %s: %w", cut, err)
		}
		node := nodes[position-1]
		held := facts[node]
		held.Relations = append(held.Relations, relation)
		facts[node] = held
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the relations of %d nodes %s: %w", len(nodes), cut, err)
	}
	return facts, nil
}

// factsSeedEpoch is the time the facts seed dates its tuples from.
var factsSeedEpoch = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// factsSeed is the store TestTheOneFactsReadAnswersAsTheTwo reads: the nodes it reads, in the order
// it asks for them, and the seq the tuples of its first part end at.
type factsSeed struct {
	nodes  []Target
	middle int64
}

// The nodes of the facts seed: a block that states values and relations, its serves among them
// to far ends ordered one way by the database's collation and the other byte for byte; the issue,
// which states a value and a relation; a block that states relations alone; a block that states
// nothing; and a comment whose one relation is retracted.
var (
	factsBlock     = Target{Kind: BlockNodeKind, ID: "FACT-1/spec#a"}
	factsIssue     = Target{Kind: graphstore.KindIssue, ID: "FACT-1"}
	factsPlaced    = Target{Kind: BlockNodeKind, ID: "FACT-1/spec#c"}
	factsSilent    = Target{Kind: BlockNodeKind, ID: "FACT-1/spec#d"}
	factsCommented = Target{Kind: graphstore.KindComment, ID: "00000000-0000-4000-8000-00000000fac7"}
)

// seedFactsStore writes the facts seed in two parts, the second's tuples at seqs above the
// first's. Its first part, each tuple dated a minute or two past factsSeedEpoch: the block's text,
// rank and state values, its part_of the issue, its text_parent, its serves of rB, ra and rc, and
// a value and a relation of one kind, mark; the issue's title and its has_kind; the second block's
// part_of the first; and the comment's anchored_in the first block. Its second part: a newer text
// of the block, dated five minutes past; a rank dated before every other, which an as-of read
// passes over for the older-seq rank dated later; the retraction of its serves of rc and of its
// state; the second block moved under the issue, six minutes past; the comment's anchored_in
// retracted, three minutes past; and a second has_kind of the issue.
func seedFactsStore(t *testing.T, database *store.Store) factsSeed {
	t.Helper()
	ctx := context.Background()
	type tuple struct {
		from               Target
		kind, toKind, toID string
		minutes            int
		asserted           bool
	}
	insert := func(tuples ...tuple) {
		t.Helper()
		for _, written := range tuples {
			if _, err := database.Pool.Exec(ctx, `
				insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
				values ($1, $2, $3, $4, $5, $6, '{}'::jsonb, 0, $7)`,
				written.from.Kind, written.from.ID, written.kind, written.toKind, written.toID,
				factsSeedEpoch.Add(time.Duration(written.minutes)*time.Minute), written.asserted); err != nil {
				t.Fatalf("seed the facts: %+v: %v", written, err)
			}
		}
	}
	value := graphstore.KindValue
	insert(
		tuple{factsBlock, graphstore.AttributeText, value, ":::requirement{#a}\nThe first words.\n:::\n", 1, true},
		tuple{factsBlock, AttributeRank, value, "1a", 1, true},
		tuple{factsBlock, graphstore.AttributeState, value, graphstore.StateLive, 1, true},
		tuple{factsBlock, graphstore.TuplePartOf, graphstore.KindIssue, factsIssue.ID, 1, true},
		tuple{factsBlock, TupleTextParent, graphstore.KindArtifact, "FACT-1/spec", 1, true},
		tuple{factsBlock, "serves", BlockNodeKind, "FACT-1/spec#rB", 2, true},
		tuple{factsBlock, "serves", BlockNodeKind, "FACT-1/spec#ra", 2, true},
		tuple{factsBlock, "serves", BlockNodeKind, "FACT-1/spec#rc", 2, true},
		tuple{factsBlock, "mark", value, "x", 2, true},
		tuple{factsBlock, "mark", BlockNodeKind, "FACT-1/spec#rc", 2, true},
		tuple{factsIssue, "title", value, "The facts", 1, true},
		tuple{factsIssue, graphstore.TupleHasKind, graphstore.KindLensKind, "kind-b", 1, true},
		tuple{factsPlaced, graphstore.TuplePartOf, BlockNodeKind, factsBlock.ID, 1, true},
		tuple{factsCommented, graphstore.TupleAnchoredIn, BlockNodeKind, factsBlock.ID, 1, true},
	)
	var seed factsSeed
	if err := database.Pool.QueryRow(ctx, `select max(seq) from graph_tuples`).Scan(&seed.middle); err != nil {
		t.Fatalf("read the seq the first part of the facts ends at: %v", err)
	}
	insert(
		tuple{factsBlock, graphstore.AttributeText, value, ":::requirement{#a}\nThe second words.\n:::\n", 5, true},
		tuple{factsBlock, AttributeRank, value, "1b", 0, true},
		tuple{factsBlock, "serves", BlockNodeKind, "FACT-1/spec#rc", 5, false},
		tuple{factsBlock, graphstore.AttributeState, value, graphstore.StateLive, 5, false},
		tuple{factsPlaced, graphstore.TuplePartOf, BlockNodeKind, factsBlock.ID, 6, false},
		tuple{factsPlaced, graphstore.TuplePartOf, graphstore.KindIssue, factsIssue.ID, 6, true},
		tuple{factsCommented, graphstore.TupleAnchoredIn, BlockNodeKind, factsBlock.ID, 3, false},
		tuple{factsIssue, graphstore.TupleHasKind, graphstore.KindLensKind, "Kind-A", 5, true},
	)
	seed.nodes = []Target{factsBlock, factsIssue, factsPlaced, factsSilent, factsCommented, factsBlock}
	return seed
}

// TreeFactsOf reads each node's values and relations in one statement, which replaced its two:
// over a seed of values and relations asserted, superseded, retracted and dated out of seq order -
// a node stating both, a value and a relation of one kind, one stating relations alone, one
// stating nothing, and a node asked for twice - the one read answers what the two answered, node
// for node, each value and each relation, the relations in the order the two answered them, at
// two record cuts and at two dated cuts. The block's serves stand under far ends the database's
// collation orders ra before rB and a byte comparison the other way, so a read that answered its
// relations in the order it grouped them, byte for byte, fails here naming the first relation out
// of place.
func TestTheOneFactsReadAnswersAsTheTwo(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var collated bool
	if err := database.Pool.QueryRow(ctx, `select 'FACT-1/spec#ra' < 'FACT-1/spec#rB'`).Scan(&collated); err != nil {
		t.Fatalf("read the database's collation of ra and rB: %v", err)
	}
	if !collated {
		t.Fatal("this database orders rB before ra by its own collation, as a byte comparison does, so it cannot tell the facts read's order from the order it groups in: run this test where select 'a' < 'B' is true")
	}
	seed := seedFactsStore(t, database)
	var head int64
	if err := database.Pool.QueryRow(ctx, `select max(seq) from graph_tuples`).Scan(&head); err != nil {
		t.Fatalf("read the head's seq: %v", err)
	}
	cuts := []struct {
		name string
		cut  TreeCut
	}{
		{"at the seq the first part ends at", TreeCut{ReadSeq: seed.middle}},
		{"at the head's seq", TreeCut{ReadSeq: head}},
		{"as of four minutes past the epoch", TreeCut{At: factsSeedEpoch.Add(4 * time.Minute), Dated: true}},
		{"as of ten minutes past the epoch", TreeCut{At: factsSeedEpoch.Add(10 * time.Minute), Dated: true}},
	}
	for _, cut := range cuts {
		two, err := treeFactsOfTwoReads(ctx, database.Pool, seed.nodes, cut.cut)
		if err != nil {
			t.Fatalf("%s: read the facts in two reads: %v", cut.name, err)
		}
		one, err := TreeFactsOf(ctx, database.Pool, seed.nodes, cut.cut)
		if err != nil {
			t.Fatalf("%s: read the facts in one read: %v", cut.name, err)
		}
		requireSameFacts(t, cut.name, seed.nodes, one, two)
	}

	// The seed holds what the test means it to: at the first part's seq the block serves ra, rB
	// and rc in that order, and at the head rc no more.
	two, err := treeFactsOfTwoReads(ctx, database.Pool, []Target{factsBlock}, cuts[0].cut)
	if err != nil {
		t.Fatalf("read the block's facts in two reads: %v", err)
	}
	if served := relationsOfKind(two[factsBlock], "serves"); !slices.Equal(served, []string{"FACT-1/spec#ra", "FACT-1/spec#rB", "FACT-1/spec#rc"}) {
		t.Fatalf("at the first part's seq the block serves %q in the two reads, want ra, rB and rc in that order: the seed does not hold the order it is meant to", served)
	}
}

// relationsOfKind is the far end ids of facts' relations of kind, in order.
func relationsOfKind(facts TreeFacts, kind string) []string {
	var ids []string
	for _, relation := range facts.Relations {
		if relation.Kind == kind {
			ids = append(ids, relation.To.ID)
		}
	}
	return ids
}

// requireSameFacts fails unless one answers each of nodes as two does: whether it states anything,
// each value, and each relation at its place in order, naming the first that differs.
func requireSameFacts(t *testing.T, cut string, nodes []Target, one, two map[Target]TreeFacts) {
	t.Helper()
	if len(one) != len(two) {
		t.Errorf("%s: the one read answers %d nodes and the two %d", cut, len(one), len(two))
	}
	for _, node := range nodes {
		got, gotHeld := one[node]
		want, wantHeld := two[node]
		if gotHeld != wantHeld {
			t.Errorf("%s: %s:%s states something in the one read %t and in the two %t", cut, node.Kind, node.ID, gotHeld, wantHeld)
			continue
		}
		if (got.Values == nil) != (want.Values == nil) || !maps.Equal(got.Values, want.Values) {
			t.Errorf("%s: %s:%s states the values %q in the one read and %q in the two", cut, node.Kind, node.ID, got.Values, want.Values)
		}
		for index := range max(len(got.Relations), len(want.Relations)) {
			var gotRelation, wantRelation string
			if index < len(got.Relations) {
				gotRelation = relationLine(got.Relations[index])
			}
			if index < len(want.Relations) {
				wantRelation = relationLine(want.Relations[index])
			}
			if gotRelation != wantRelation {
				t.Errorf("%s: relation %d of %s:%s is %q in the one read and %q in the two, the first relation out of place",
					cut, index+1, node.Kind, node.ID, gotRelation, wantRelation)
				break
			}
		}
	}
}

// relationLine is a relation as a failure names it: its kind and its far end's address.
func relationLine(relation TreeRelation) string {
	return relation.Kind + " " + relation.To.Kind + ":" + relation.To.ID
}
