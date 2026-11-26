package refs

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// typedVersion is one version of a document as the typed-node pass and the block index read it:
// its markdown, the tree it was rendered from, its blocks, the mentions in it and its pointers.
type typedVersion struct {
	markdown string
	tree     *pmdoc.Node
	blocks   []model.ArtifactBlock
	located  []text.Located
	pointers []Pointer
}

// renderTypedVersion renders markdown as a version write renders it, every block holding an id
// (pmdoc.EnsureBlockIDs, renderTypedTree).
func renderTypedVersion(t *testing.T, markdown string) typedVersion {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the version: %v", err)
	}
	pmdoc.EnsureBlockIDs(tree)
	return renderTypedTree(t, tree)
}

// renderTypedTree renders tree as a version write renders it (pmdoc.RenderWithBlockOffsets), and
// reads its pointers off the tree, as docs.blockPointers does.
func renderTypedTree(t *testing.T, tree *pmdoc.Node) typedVersion {
	t.Helper()
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatalf("render the version: %v", err)
	}
	version := typedVersion{markdown: rendered, tree: tree, located: text.ExtractAt(rendered, "https://dispatch.example")}
	for _, offset := range offsets {
		version.blocks = append(version.blocks, model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To})
	}
	version.pointers = pointersOf(tree)
	return version
}

// pointersOf is the pointers the blocks of tree hold, as docs.blockPointers reads them.
func pointersOf(tree *pmdoc.Node) []Pointer {
	var pointers []Pointer
	for _, pointer := range pmdoc.BlockPointers(tree) {
		pointers = append(pointers, Pointer{Block: pointer.Block, BlockType: pointer.BlockType, Kind: pointer.Attribute, Target: pointer.Target, Targets: pointer.Targets})
	}
	return pointers
}

// versionFacts is what DecisionFacts reads of version number of the document artifact, written at
// at, as a version write reads it: against what the version's own row says of it
// (ReadIndexedVersion), and dated at that one version (OneVersion).
func versionFacts(ctx context.Context, tx pgx.Tx, artifact string, number int, at time.Time, version typedVersion, sources TypedSources) (TypedFacts, error) {
	record, err := ReadIndexedVersion(ctx, tx, artifact, artifact, number)
	if err != nil {
		return TypedFacts{}, err
	}
	return DecisionFacts(ctx, tx, artifact, record, OneVersion(artifact, number, at), version.blocks, version.tree, version.pointers, sources)
}

// indexTypedAlone runs the typed-node pass on its own over the version facts were read of, of the
// document artifact, as a write above the deferral bound runs it: planned against the document's
// block index (PlanDecisionNodes) and written as a batch of its own. It answers what the pass
// stated.
func indexTypedAlone(ctx context.Context, tx pgx.Tx, artifact string, facts TypedFacts) (graphstore.Uncaused, error) {
	index, err := ReadBlockIndex(ctx, tx, artifact)
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	plan, err := PlanDecisionNodes(ctx, tx, index, artifact, facts, graphstore.Uncaused{}, "")
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	if err := store.RunWrite(ctx, tx, plan.Queue); err != nil {
		return graphstore.Uncaused{}, err
	}
	return plan.Stated(), nil
}

// typedRow is one block_refs row of a typed kind, with the transaction that last wrote it.
type typedRow struct {
	block, kind, toKind, toID, node, xmin string
}

// typedRows is every block_refs row of the document of a kind the typed-node pass states.
func typedRows(t *testing.T, q Queryer, artifactID string) []typedRow {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select block_id, kind, to_kind, to_id, coalesce(node_kind, '') || ':' || coalesce(node_address, ''), xmin::text
		from block_refs where artifact_id = $1 and kind = any($2)
		order by block_id, kind, to_kind, to_id
	`, artifactID, typedKinds)
	if err != nil {
		t.Fatalf("read the typed rows: %v", err)
	}
	defer rows.Close()
	var found []typedRow
	for rows.Next() {
		var row typedRow
		if err := rows.Scan(&row.block, &row.kind, &row.toKind, &row.toID, &row.node, &row.xmin); err != nil {
			t.Fatalf("scan a typed row: %v", err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the typed rows: %v", err)
	}
	return found
}

// mentionRows counts the document's block_refs rows of kind mentions.
func mentionRows(t *testing.T, q Queryer, artifactID string) int {
	t.Helper()
	var count int
	if err := q.QueryRow(context.Background(), `select count(*) from block_refs where artifact_id = $1 and kind = 'mentions'`, artifactID).Scan(&count); err != nil {
		t.Fatalf("count the mention rows: %v", err)
	}
	return count
}

// answeredAsk stores an ask whose row names its node and whose node holds an answer, held by
// block blockID of the document, as asks.Settle leaves one a person answered.
func answeredAsk(t *testing.T, d parentDocument, blockID string, at time.Time) string {
	t.Helper()
	ctx := context.Background()
	tx, err := d.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the ask fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	const id = "7c2a1e40-0a1b-4c3d-8e9f-a0b1c2d3e4f5"
	person := model.Actor{Kind: "user", ID: "bob"}
	if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.AskNode(id, parentProject("REC"), nil, person, at)}, []graphstore.Tuple{{
		FromKind: graphstore.KindAsk, FromID: id, Kind: graphstore.TupleAnswerState,
		ToKind: graphstore.KindValue, ToID: asks.StateAnswered, At: at, Who: person, Asserted: true,
	}}); err != nil {
		t.Fatalf("write the ask's node: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into asks (id, issue_key, author, question, block_artifact_id, block_id, node_kind, node_address)
		values ($1::uuid, $2, '{"kind":"user","id":"alice"}', 'Which transport?', $3, $4, $5, $6)
	`, id, d.issue, d.artifact, blockID, graphstore.KindAsk, id); err != nil {
		t.Fatalf("insert the ask's row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the ask fixture: %v", err)
	}
	return id
}

// writerSources are the sources of a version one writer wrote whole, as a write that seeds a
// document or adds every block it holds gives them (docs' typedSources): every block is author's,
// and an ask block shows its own row's ask, answered where its node's newest answer_state is.
func writerSources(tx pgx.Tx, artifactID string, author model.Actor) TypedSources {
	ctx := context.Background()
	return TypedSources{
		Authors: func(ids []string) (map[string]model.Actor, error) {
			authors := make(map[string]model.Actor, len(ids))
			for _, id := range ids {
				authors[id] = author
			}
			return authors, nil
		},
		Shown: func(ids []string) (map[string]ShownAsk, error) {
			rows, err := tx.Query(ctx, `
				select k.block_id, k.id::text, k.node_kind is not null,
				       coalesce((select t.to_id = $3 from graph_tuples t
				                 where t.from_kind = $4 and t.from_id = k.id::text and t.kind = $5
				                 order by t.seq desc limit 1), false)
				from asks k where k.block_artifact_id = $1 and k.block_id = any($2)
			`, artifactID, ids, asks.StateAnswered, graphstore.KindAsk, graphstore.TupleAnswerState)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			shown := map[string]ShownAsk{}
			for rows.Next() {
				var block string
				var ask ShownAsk
				if err := rows.Scan(&block, &ask.ID, &ask.Named, &ask.Answered); err != nil {
					return nil, err
				}
				shown[block] = ask
			}
			return shown, rows.Err()
		},
	}
}

// pairKeys is pairs as comparable keys, sorted, so two lists are equal exactly when they write the
// same rows from the same nodes.
func pairKeys(pairs []blockPair) []string {
	keys := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		keys = append(keys, fmt.Sprintf("%s %s -> %s %s on %s:%s", pair.blockID, pair.kind, pair.target.Kind, pair.target.ID, pair.nodeKind, pair.nodeAddress))
	}
	slices.SortFunc(keys, cmp.Compare[string])
	return keys
}

// Above the deferral bound a write runs the typed-node pass alone, which writes the block_refs
// rows of its own ties - each decision's shaped, from_ask and decided_by and each ask block's
// shaped, on the ask's node - and deletes no other row of the document. The block-index
// reconciliation then indexes the same version with every block's pairs, and the typed ties among
// them are the very pairs the typed pass handed its scoped reconcile: the reconciler moves no row
// the typed pass wrote and states no tie of those kinds again, while it does delete the mention
// the version dropped, which the typed pass left standing.
func TestTheReconcilerMovesNoRowTheTypedPassWrote(t *testing.T) {
	database := parentDatabase(t)
	ctx := context.Background()
	document := newParentDocument(t, database, "REC", "decisions-md")
	alice := model.Actor{Kind: "user", ID: "alice"}

	// Version 1, indexed whole: a mention the next version drops.
	first := renderTypedVersion(t, ":::requirement{#r1}\nA requirement.\n:::\n\nSee dispatch://"+document.issue+".\n")
	firstRecord := document.writeVersion(t, 1, first.markdown, alice)
	commitIndex(t, database.Pool, func(tx pgx.Tx) (graphstore.Uncaused, error) {
		facts, err := versionFacts(ctx, tx, document.artifact, 1, firstRecord.at, first, writerSources(tx, document.artifact, alice))
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		_, _, write, _, err := IndexBlocks(ctx, tx, document.artifact, OneVersion(document.artifact, 1, firstRecord.at), first.markdown, first.blocks, first.located, first.pointers, "", facts)
		return write, err
	}, firstRecord.event)
	if got := mentionRows(t, database.Pool, document.artifact); got != 1 {
		t.Fatalf("version 1 left %d mention rows, want its one", got)
	}

	ask := answeredAsk(t, document, "a1", firstRecord.at)
	spec := "dispatch://" + document.issue + "/artifact/decisions-md#b-r1"
	specRef, _ := text.ParseRef(spec, "")
	specTarget, _ := TargetOf(specRef)
	second := renderTypedVersion(t, ":::requirement{#r1}\nA requirement.\n:::\n\n"+
		"::::decision{#d1 shaped=\""+spec+"\" from_ask=\"dispatch://"+document.issue+"/ask/"+ask+"\" uncertainty=\"low\"}\nWe chose REST.\n\n"+
		":::reading{#g0}\nThe agent reads it as REST everywhere.\n:::\n::::\n\n"+
		"::::ask{#a1 shaped=\""+spec+"\"}\nWhich transport?\n\n"+
		":::reading{#g1}\nThe agent reads the answer as REST.\n:::\n::::\n")
	secondRecord := document.writeVersion(t, 2, second.markdown, alice)

	// The typed pass alone, as a write above the deferral bound runs it.
	before := document.highestTuple(t)
	commitIndex(t, database.Pool, func(tx pgx.Tx) (graphstore.Uncaused, error) {
		facts, err := versionFacts(ctx, tx, document.artifact, 2, secondRecord.at, second, writerSources(tx, document.artifact, alice))
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		return indexTypedAlone(ctx, tx, document.artifact, facts)
	}, secondRecord.event)
	typed := typedRows(t, database.Pool, document.artifact)
	want := []string{
		"a1 shaped -> block " + specTarget.ID + " on ask:" + ask,
		"d1 decided_by -> person alice on block:" + document.address("d1"),
		"d1 from_ask -> ask " + ask + " on block:" + document.address("d1"),
		"d1 shaped -> block " + specTarget.ID + " on block:" + document.address("d1"),
	}
	var got []string
	for _, row := range typed {
		got = append(got, row.block+" "+row.kind+" -> "+row.toKind+" "+row.toID+" on "+row.node)
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the typed pass wrote the rows\n\t%v\nwant\n\t%v", got, want)
	}
	if left := mentionRows(t, database.Pool, document.artifact); left != 1 {
		t.Fatalf("the typed pass left %d mention rows, want version 1's one standing: it deletes no row of another kind", left)
	}
	typedStated := 0
	for _, tuple := range document.tuplesAfter(t, database.Pool, before) {
		if slices.Contains(typedKinds, tuple.Kind) {
			typedStated++
		}
		if tuple.FromID == document.address("g1") && tuple.Kind == graphstore.TuplePartOf &&
			(tuple.ToKind != graphstore.KindAsk || tuple.ToID != ask) {
			t.Errorf("the reading in the answered ask states part_of %s/%s, want the ask's node %s", tuple.ToKind, tuple.ToID, ask)
		}
	}
	if typedStated != len(want) {
		t.Fatalf("the typed pass stated %d tuples of its kinds, want one for each of its %d rows", typedStated, len(want))
	}

	// The reconciler over the same version, as the block-index tick runs it.
	before = document.highestTuple(t)
	commitIndex(t, database.Pool, func(tx pgx.Tx) (graphstore.Uncaused, error) {
		facts, err := versionFacts(ctx, tx, document.artifact, 2, secondRecord.at, second, writerSources(tx, document.artifact, alice))
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		// The two branches' pairs: the typed pass's own, and the typed ones among the pairs the
		// block index reconciles for every block (PlanBlocks).
		nodes, _, _ := uniqueBlocks(second.blocks)
		scope := facts.typedScope()
		var full []blockPair
		for _, pair := range facts.pairs(blockPairs(nodes, second.located, facts.withTies(second.pointers))) {
			if slices.Contains(scope.kinds, pair.kind) && slices.Contains(scope.blocks, pair.blockID) {
				full = append(full, pair)
			}
		}
		ownPairs, err := facts.typedPairs(ctx, tx, document.artifact)
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		if own, all := pairKeys(ownPairs), pairKeys(full); !reflect.DeepEqual(own, all) {
			t.Fatalf("the typed pass's pairs\n\t%v\ndiffer from the reconciler's typed pairs\n\t%v", own, all)
		}
		// The block index plans the typed-node pass's own facts of the version into its one
		// write (TypedFacts.indexedFacts), as the reconciler indexes it.
		_, won, write, _, err := IndexBlocks(ctx, tx, document.artifact, OneVersion(document.artifact, 2, secondRecord.at), second.markdown, second.blocks, second.located, second.pointers, "", facts)
		if err == nil && !won {
			err = fmt.Errorf("the reconciler lost the revision fence over version 2")
		}
		return write, err
	}, secondRecord.event)
	if after := typedRows(t, database.Pool, document.artifact); !reflect.DeepEqual(after, typed) {
		t.Fatalf("the reconciler moved the typed pass's rows:\n\tbefore %+v\n\tafter  %+v", typed, after)
	}
	for _, tuple := range document.tuplesAfter(t, database.Pool, before) {
		if slices.Contains(typedKinds, tuple.Kind) {
			t.Errorf("the reconciler stated %s %s -> %s/%s again (asserted %t), which the typed pass already stated", tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.Asserted)
		}
		if tuple.Kind == graphstore.TuplePartOf && slices.Contains([]string{document.address("d1"), document.address("g0"), document.address("g1")}, tuple.FromID) {
			t.Errorf("the reconciler stated %s part_of %s/%s again, which the typed pass already placed", tuple.FromID, tuple.ToKind, tuple.ToID)
		}
	}
	if left := mentionRows(t, database.Pool, document.artifact); left != 0 {
		t.Fatalf("the reconciler left %d mention rows, want the one version 2 dropped deleted", left)
	}
}

// A version that moves one word of one decision, in a document of several decisions each holding
// a reading, writes the nodes of the blocks whose text it moved and no other: the block index
// writes that decision's node and its paragraph's, and the typed-node pass it plans beside them
// (TypedFacts.indexedFacts) writes no node of its own, since every other decision and reading the
// store holds already stands as the version leaves it - live, under its type's supertag. A pass
// that wrote every typed node of every version would lock each of their rows on every one-word
// edit.
func TestAOneWordEditWritesOnlyTheNodesOfTheBlocksItMoved(t *testing.T) {
	database := parentDatabase(t)
	ctx := context.Background()
	document := newParentDocument(t, database, "ONE", "decisions-md")
	alice := model.Actor{Kind: "user", ID: "alice"}
	first := renderTypedVersion(t, "::::decision{#d1}\nWe chose REST.\n\n:::reading{#g1}\nThe agent reads it as REST.\n:::\n::::\n\n"+
		"::::decision{#d2}\nWe chose Postgres.\n\n:::reading{#g2}\nThe agent reads it as Postgres.\n:::\n::::\n\n"+
		"::::decision{#d3}\nWe chose Go.\n\n:::reading{#g3}\nThe agent reads it as Go.\n:::\n::::\n")
	firstRecord := document.writeVersion(t, 1, first.markdown, alice)
	index := func(version typedVersion, number int, record versionRecord) (blocks, typed []string) {
		commitIndex(t, database.Pool, func(tx pgx.Tx) (graphstore.Uncaused, error) {
			facts, err := versionFacts(ctx, tx, document.artifact, number, record.at, version, writerSources(tx, document.artifact, alice))
			if err != nil {
				return graphstore.Uncaused{}, err
			}
			// The typed-node pass's own nodes, as the block index plans them beside its own, and
			// every node the one write of the index writes.
			own, _ := facts.indexedFacts(nil)
			_, won, write, _, err := IndexBlocks(ctx, tx, document.artifact, OneVersion(document.artifact, number, record.at), version.markdown, version.blocks, version.located, version.pointers, "", facts)
			if err == nil && !won {
				err = fmt.Errorf("version %d lost the revision fence", number)
			}
			if err != nil {
				return graphstore.Uncaused{}, err
			}
			for _, node := range write.Nodes {
				blocks = append(blocks, node.ID)
			}
			for _, node := range own {
				typed = append(typed, node.ID)
			}
			return write, nil
		}, record.event)
		return blocks, typed
	}
	index(first, 1, firstRecord)

	// One word of d2's own paragraph, edited in the tree the first version was rendered from, so
	// every block keeps its id.
	var paragraph string
	pmdoc.Walk(first.tree, func(node *pmdoc.Node) bool {
		if node.Type == "decision" && node.Attrs[pmdoc.BlockIDAttr] == "d2" {
			paragraph, _ = node.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
			node.Children[0].Children[0].Text = "We chose SQLite."
		}
		return true
	})
	second := renderTypedTree(t, first.tree)
	secondRecord := document.writeVersion(t, 2, second.markdown, alice)
	blocks, typed := index(second, 2, secondRecord)
	slices.Sort(blocks)
	if want := []string{document.address("d2"), document.address(paragraph)}; !slices.Equal(blocks, sorted(want)) {
		t.Errorf("the block index of the one-word edit wrote the nodes %v, want %v: the decision whose text moved and its paragraph", blocks, sorted(want))
	}
	if len(typed) != 0 {
		t.Errorf("the typed-node pass of the one-word edit wrote the nodes %v, want none: every decision and reading already stands as the version leaves it", typed)
	}
	t.Logf("the one-word edit wrote the block nodes %v and no typed node of its own", blocks)
}

// sorted is ids in order.
func sorted(ids []string) []string {
	slices.Sort(ids)
	return ids
}

// commitIndex runs one index pass in a transaction of its own, appends what it stated under
// event, and commits.
func commitIndex(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, pass func(pgx.Tx) (graphstore.Uncaused, error), event int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the index pass: %v", err)
	}
	defer tx.Rollback(ctx)
	write, err := pass(tx)
	if err != nil {
		t.Fatalf("run the index pass: %v", err)
	}
	if err := graphstore.Caused(ctx, tx, write, event); err != nil {
		t.Fatalf("append what the index pass stated: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the index pass: %v", err)
	}
}

// indexWhole runs the block index over version number of the document, which the test wrote at
// record, with the typed-node pass's facts read as a version write reads them, and appends what
// it stated under the version's event.
func indexWhole(t *testing.T, database *store.Store, document parentDocument, number int, version typedVersion, record versionRecord) {
	t.Helper()
	ctx := context.Background()
	commitIndex(t, database.Pool, func(tx pgx.Tx) (graphstore.Uncaused, error) {
		facts, err := versionFacts(ctx, tx, document.artifact, number, record.at, version, writerSources(tx, document.artifact, record.writer))
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		_, won, write, _, err := IndexBlocks(ctx, tx, document.artifact, OneVersion(document.artifact, number, record.at), version.markdown, version.blocks, version.located, version.pointers, "", facts)
		if err == nil && !won {
			err = fmt.Errorf("version %d lost the revision fence", number)
		}
		return write, err
	}, record.event)
}

// typedRowsOf is the document's block_refs rows of the typed kinds as the tests compare them:
// block, kind, far end and the node the row names.
func typedRowsOf(t *testing.T, q Queryer, artifactID string) []string {
	t.Helper()
	var rows []string
	for _, row := range typedRows(t, q, artifactID) {
		rows = append(rows, row.block+" "+row.kind+" -> "+row.toKind+" "+row.toID+" on "+row.node)
	}
	return rows
}

// currentTies is each tie of kinds node kind/id states now, as its kind and its far end.
func currentTies(t *testing.T, q Queryer, kind, id string, kinds ...string) []string {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), q, kind, id, kinds)
	if err != nil {
		t.Fatalf("read the ties %s/%s states: %v", kind, id, err)
	}
	var ties []string
	for _, tuple := range held {
		ties = append(ties, tuple.Kind+" -> "+tuple.ToKind+" "+tuple.ToID)
	}
	return ties
}

// A block retyped between an ask and a decision that keeps its id and its shaped keeps the one
// block_refs row of that tie, which hangs on another node now: an ask block's shaped on the ask's
// node, and a decision's on its block's own (TypedFacts.pairs). The index moves the row and its
// tuple together (rehome), so graph_edges answers the tie out of the node it hangs on and not out
// of the one it left, and the row and the tuples agree as the census holds them (graphcensus'
// tieProjection): the node the row names states the tie, and the node it left states none. Each
// direction is the first retype of one case, and the retype back is the other direction again.
func TestARetypeBetweenAnAskAndADecisionMovesItsShapedTie(t *testing.T) {
	for _, test := range []struct {
		name string
		// asked is whether the first version holds the ask block, and the second the decision.
		asked bool
	}{
		{"an ask block retyped into a decision, and back", true},
		{"a decision retyped into an ask block, and back", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := parentDatabase(t)
			ctx := context.Background()
			document := newParentDocument(t, database, "RETYPE", "notes")
			alice := model.Actor{Kind: "user", ID: "alice"}
			spec := "dispatch://" + document.issue + "/artifact/notes#b-r1"
			specRef, _ := text.ParseRef(spec, "")
			specTarget, _ := TargetOf(specRef)
			asked := renderTypedVersion(t, ":::requirement{#r1}\nA requirement.\n:::\n\n:::ask{#a1 shaped=\""+spec+"\"}\nWhich transport?\n:::\n")
			decided := renderTypedVersion(t, ":::requirement{#r1}\nA requirement.\n:::\n\n:::decision{#a1 shaped=\""+spec+"\"}\nWe chose REST.\n:::\n")
			versions := []typedVersion{asked, decided, asked}
			if !test.asked {
				versions = []typedVersion{decided, asked, decided}
			}
			// The ask a1's row names its node from the start: the block of a decision is no ask
			// block, and only an ask block's shaped hangs on it.
			first := document.writeVersion(t, 1, versions[0].markdown, alice)
			ask := answeredAsk(t, document, "a1", first.at)
			for index, version := range versions {
				number := index + 1
				record := first
				if number > 1 {
					record = document.writeVersion(t, number, version.markdown, alice)
				}
				indexWhole(t, database, document, number, version, record)
				kind, id := graphstore.KindAsk, ask
				if version.markdown == decided.markdown {
					kind, id = BlockNodeKind, document.address("a1")
				}
				when := fmt.Sprintf("version %d", number)
				var shaped []string
				for _, row := range typedRows(t, database.Pool, document.artifact) {
					if row.kind == TupleShaped {
						shaped = append(shaped, row.block+" -> "+row.toKind+" "+row.toID+" on "+row.node)
					}
				}
				if want := []string{"a1 -> " + specTarget.Kind + " " + specTarget.ID + " on " + kind + ":" + id}; !slices.Equal(shaped, want) {
					t.Fatalf("%s: the shaped rows are %v, want %v", when, shaped, want)
				}
				rows, err := database.Pool.Query(ctx, `
					select from_kind || ':' || from_id from graph_edges
					where kind = $1 and to_kind = $2 and to_id = $3 order by 1
				`, TupleShaped, specTarget.Kind, specTarget.ID)
				if err != nil {
					t.Fatalf("%s: read the shaped edges graph_edges answers: %v", when, err)
				}
				edges, err := pgx.CollectRows(rows, pgx.RowTo[string])
				if err != nil {
					t.Fatalf("%s: read the shaped edges graph_edges answers: %v", when, err)
				}
				if want := []string{kind + ":" + id}; !slices.Equal(edges, want) {
					t.Errorf("%s: graph_edges answers the shaped edges %v into %s, want %v", when, edges, specTarget.ID, want)
				}
				for _, node := range [...][2]string{{graphstore.KindAsk, ask}, {BlockNodeKind, document.address("a1")}} {
					var want []string
					if node[0] == kind && node[1] == id {
						want = []string{TupleShaped + " -> " + specTarget.Kind + " " + specTarget.ID}
					}
					if ties := currentTies(t, database.Pool, node[0], node[1], TupleShaped); !slices.Equal(ties, want) {
						t.Errorf("%s: %s/%s states %v, want %v, as the row naming %s:%s holds", when, node[0], node[1], ties, want, kind, id)
					}
				}
			}
		})
	}
}

// Above the deferral bound the typed-node pass runs alone (PlanDecisionNodes), and a version that
// removes a decision and an ask block drops the rows of their ties in that pass, each tuple
// retracted out of the node its row names: the ask block's shaped out of the ask's node, and the
// decision's shaped and decided_by out of its block's. A version that holds no typed block at all
// reads bare facts, and drops them as one that keeps another decision does. The release before
// would otherwise delete the ask's row when it indexes the document, and retract out of the
// block's own node, which leaves the ask's tuple standing with no row.
func TestADeferredVersionDropsTheTiesOfTheTypedBlocksItRemoved(t *testing.T) {
	for _, test := range []struct {
		name string
		// keeps is whether the version keeps a decision of its own, d2.
		keeps bool
	}{
		{name: "a version that holds no typed block"},
		{name: "a version that keeps another decision", keeps: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := parentDatabase(t)
			ctx := context.Background()
			document := newParentDocument(t, database, "DROP", "notes")
			alice := model.Actor{Kind: "user", ID: "alice"}
			spec := "dispatch://" + document.issue + "/artifact/notes#b-r1"
			specRef, _ := text.ParseRef(spec, "")
			specTarget, _ := TargetOf(specRef)
			requirement := ":::requirement{#r1}\nA requirement.\n:::\n"
			tied := renderTypedVersion(t, requirement+"\n:::decision{#d1 shaped=\""+spec+"\"}\nWe chose REST.\n:::\n\n"+
				":::ask{#a1 shaped=\""+spec+"\"}\nWhich transport?\n:::\n")
			first := document.writeVersion(t, 1, tied.markdown, alice)
			ask := answeredAsk(t, document, "a1", first.at)
			indexWhole(t, database, document, 1, tied, first)
			to := specTarget.Kind + " " + specTarget.ID
			if got, want := typedRowsOf(t, database.Pool, document.artifact), []string{
				"a1 shaped -> " + to + " on ask:" + ask,
				"d1 decided_by -> person alice on block:" + document.address("d1"),
				"d1 shaped -> " + to + " on block:" + document.address("d1"),
			}; !slices.Equal(got, want) {
				t.Fatalf("version 1 wrote the typed rows %v, want %v", got, want)
			}

			markdown, want := requirement, []string(nil)
			if test.keeps {
				markdown += "\n:::decision{#d2 shaped=\"" + spec + "\"}\nWe chose gRPC.\n:::\n"
				want = []string{
					"d2 decided_by -> person alice on block:" + document.address("d2"),
					"d2 shaped -> " + to + " on block:" + document.address("d2"),
				}
			}
			removed := renderTypedVersion(t, markdown)
			second := document.writeVersion(t, 2, removed.markdown, alice)
			commitIndex(t, database.Pool, func(tx pgx.Tx) (graphstore.Uncaused, error) {
				facts, err := versionFacts(ctx, tx, document.artifact, 2, second.at, removed, writerSources(tx, document.artifact, alice))
				if err != nil {
					return graphstore.Uncaused{}, err
				}
				return indexTypedAlone(ctx, tx, document.artifact, facts)
			}, second.event)
			if got := typedRowsOf(t, database.Pool, document.artifact); !slices.Equal(got, want) {
				t.Errorf("the typed pass of version 2 left the typed rows %v, want %v", got, want)
			}
			if ties := currentTies(t, database.Pool, graphstore.KindAsk, ask, TupleShaped); len(ties) != 0 {
				t.Errorf("the ask's node states %v after the version that removed its block, want no shaped tie", ties)
			}
			if ties := currentTies(t, database.Pool, BlockNodeKind, document.address("d1"), typedKinds...); len(ties) != 0 {
				t.Errorf("the removed decision's node states %v, want no tie of the typed kinds", ties)
			}
		})
	}
}
