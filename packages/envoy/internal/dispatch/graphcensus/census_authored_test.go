package graphcensus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// requireRowDifference fails unless the census reads, of kind name, a row difference at a row
// whose label starts with at (a kind held triple by triple writes the triple after its label),
// and none of any other kind.
func requireRowDifference(t *testing.T, what string, report *StoreReport, name, at string) {
	t.Helper()
	for _, kind := range report.Kinds {
		found := slices.ContainsFunc(kind.ReportedRows, func(difference Difference) bool { return strings.HasPrefix(difference.At, at) })
		switch {
		case kind.Kind == name && !found:
			t.Errorf("%s reads the row differences %+v of kind %s, want one at %q", what, kind.ReportedRows, name, at)
		case kind.Kind != name && kind.RowDifferences+kind.NodeDifferences != 0:
			t.Errorf("%s reads %+v and %+v of kind %s, want no difference there", what, kind.ReportedRows, kind.ReportedNodes, kind.Kind)
		}
	}
}

// The store census holds an authored node's place and its authors by the rules the block index
// writes them by (T7 of dispatch://INFRA-1253's plan). Beside a store it agrees with, a document
// of the issue holds a criterion whose parent is a person's comment and a requirement under the
// issue, first written in a version bob wrote, who wrote nothing else. The census agrees: each
// row's null parent_block_id stands for a part_of to no block of its artifact (the comment, the
// issue), and each node's authored_by names bob, whose node the store holds. A parent_block_id
// naming a block of the artifact under the criterion is a difference of the block kind. The
// requirement with its authored_by taken off, as a build before this one left a node, is a
// difference of the block_author kind while its document owes nothing, none while the drain has
// it owed, and none once the pass that drains it has run, which gives it its authors.
func TestTheCensusHoldsAnAuthoredNodesPlaceAndItsAuthors(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	seedAgreeingStore(t, database)
	var comment, checks, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'I want the urgent asks first.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("write alice's comment: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'checks-md', 'checks.md', 'doc', false, '{"kind":"user","id":"bob"}')
		returning id::text, ref_key
	`).Scan(&checks, &refKey); err != nil {
		t.Fatalf("create checks.md: %v", err)
	}
	const criterion, requirement = "I want the urgent asks first.", "Urgent asks come first."
	markdown := criterion + "\n\n" + requirement
	blocks := []model.ArtifactBlock{
		{ID: "k1", Type: "criterion", From: 0, To: len(criterion)},
		{ID: "r1", Type: "requirement", From: len(criterion) + 2, To: len(markdown)},
	}
	var at time.Time
	if err := database.Pool.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 1, $2, '[{"kind":"user","id":"bob"}]') returning created_at
	`, checks, markdown).Scan(&at); err != nil {
		t.Fatalf("write version 1 of checks.md: %v", err)
	}
	// The comment, the document and its version take their nodes as every pre-store row does.
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the comment and checks.md: %v", err)
	}
	pointers := []refs.Pointer{{Block: "k1", BlockType: "criterion", Kind: pmdoc.ParentAttribute,
		Target: text.Ref{Kind: "comment", IssueKey: "CORE-1", ID: comment}}}
	version := 1
	index := func(what string) {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %s: %v", what, err)
		}
		defer tx.Rollback(ctx)
		_, won, write, err := refs.IndexBlocks(ctx, tx, checks, refs.OneVersion(checks, version, at), markdown, blocks, nil, pointers, refs.TypedFacts{})
		if err != nil || !won {
			t.Fatalf("%s: won %t, %v", what, won, err)
		}
		if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
			t.Fatalf("append what %s stated: %v", what, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", what, err)
		}
	}
	index("the index of checks.md")
	for _, placed := range []struct{ block, kind, id string }{{"k1", graphstore.KindComment, comment}, {"r1", graphstore.KindIssue, "CORE-1"}} {
		held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, refKey+"#"+placed.block, []string{graphstore.TuplePartOf})
		if err != nil || len(held) != 1 || held[0].ToKind != placed.kind || held[0].ToID != placed.id {
			t.Fatalf("%s stands under %+v (%v), want %s:%s", placed.block, held, err, placed.kind, placed.id)
		}
	}
	requireTheCensusAgrees(t, "the census beside an authored node under a comment and one under the issue", storeCensusOf(t, database))

	setParent := func(parent *string) {
		t.Helper()
		if tag, err := database.Pool.Exec(ctx, `
			update document_blocks set parent_block_id = $2 where artifact_id = $1::uuid and block_id = 'k1'
		`, checks, parent); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("set k1's parent_block_id: %d rows, %v", tag.RowsAffected(), err)
		}
	}
	wrong := "r1"
	setParent(&wrong)
	requireRowDifference(t, "the census over k1's row naming r1", storeCensusOf(t, database), "block", "block "+refKey+"#k1")
	setParent(nil)

	if tag, err := database.Pool.Exec(ctx, `
		delete from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3
	`, refs.BlockNodeKind, refKey+"#r1", graphstore.TupleAuthoredBy); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("take r1's authored_by off: %d rows, %v", tag.RowsAffected(), err)
	}
	requireRowDifference(t, "the census over r1 with no authored_by", storeCensusOf(t, database),
		"block_author", "block_author "+refKey+"#r1 "+graphstore.KindPerson+":bob")
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set store_owed_at = now() where artifact_id = $1::uuid`, checks); err != nil {
		t.Fatalf("mark checks.md owed, as the drain marks it: %v", err)
	}
	if owed := censusKind(t, storeCensusOf(t, database), "block_author"); owed.RowDifferences+owed.NodeDifferences != 0 {
		t.Errorf("the census of a document the drain owes reads %+v and %+v of kind block_author, want none", owed.ReportedRows, owed.ReportedNodes)
	}
	index("the pass that drains checks.md")
	requireTheCensusAgrees(t, "the census after the pass that drains checks.md", storeCensusOf(t, database))

	// r1 retyped to a plain paragraph, in a version carol writes: it records its authors no
	// longer, keeps the authored_by bob's first version gave it, which still says who wrote it,
	// and hangs under its document's top as its text places it now.
	const prose = "Urgent asks come first, written as prose."
	markdown = criterion + "\n\n" + prose
	blocks = []model.ArtifactBlock{
		{ID: "k1", Type: "criterion", From: 0, To: len(criterion)},
		{ID: "r1", Type: "paragraph", From: len(criterion) + 2, To: len(markdown)},
	}
	version = 2
	if err := database.Pool.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, $2, '[{"kind":"user","id":"carol"}]') returning created_at
	`, checks, markdown).Scan(&at); err != nil {
		t.Fatalf("write version 2 of checks.md: %v", err)
	}
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill version 2 of checks.md: %v", err)
	}
	index("the index of the version retyping r1")
	if held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, refKey+"#r1", []string{graphstore.TupleAuthoredBy}); err != nil ||
		len(held) != 1 || held[0].ToKind != graphstore.KindPerson || held[0].ToID != "bob" {
		t.Fatalf("the retyped r1 states the authored_by %+v (%v), want bob's alone", held, err)
	}
	requireTheCensusAgrees(t, "the census after r1 is retyped to a paragraph", storeCensusOf(t, database))
}

// requireBlockAuthorsAgree fails unless the census reads no difference of kind block_author while
// it reads rows and tuples of that kind both, so its agreement compared something.
func requireBlockAuthorsAgree(t *testing.T, what string, report *StoreReport) {
	t.Helper()
	kind := censusKind(t, report, "block_author")
	for _, difference := range append(append([]Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
		t.Errorf("%s reads a block_author difference at %s: %s", what, difference.At, difference.Cause)
	}
	if kind.RowDifferences+kind.NodeDifferences != 0 || kind.Rows == 0 || kind.Nodes == 0 {
		t.Errorf("%s reads %d and %d block_author differences over %d rows and %d tuples, want none over rows and tuples of it",
			what, kind.RowDifferences, kind.NodeDifferences, kind.Rows, kind.Nodes)
	}
}

// censusIssue writes project CORE and its issue CORE-1, which holds no document yet.
func censusIssue(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue CORE-1: %v", err)
	}
}

// censusDocument writes an artifact of CORE-1 of kind kind and slug slug, its issue's primary one
// where primary says so, and answers its id and ref_key.
func censusDocument(t *testing.T, database *store.Store, slug, kind string, primary bool) (string, string) {
	t.Helper()
	var id, refKey string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', $1, $1, $2, $3, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, slug, kind, primary).Scan(&id, &refKey); err != nil {
		t.Fatalf("create %s: %v", slug, err)
	}
	return id, refKey
}

// censusVersion writes version number of artifact, holding markdown, with authors in the order a
// settlement stores them (docs' actorSlice), and answers its time.
func censusVersion(t *testing.T, database *store.Store, artifact string, number int, markdown string, authors ...model.Actor) time.Time {
	t.Helper()
	encoded, err := json.Marshal(authors)
	if err != nil {
		t.Fatalf("encode the authors of version %d: %v", number, err)
	}
	var at time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors) values ($1::uuid, $2, $3, $4) returning created_at
	`, artifact, number, markdown, encoded).Scan(&at); err != nil {
		t.Fatalf("write version %d of %s: %v", number, artifact, err)
	}
	return at
}

// censusPass runs pass in a transaction of its own, appends what it stated at the sentinel, as a
// caller with no event of its own does, and commits.
func censusPass(t *testing.T, database *store.Store, what string, pass func(ctx context.Context, tx pgx.Tx) (graphstore.Uncaused, error)) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s: %v", what, err)
	}
	defer tx.Rollback(ctx)
	write, err := pass(ctx, tx)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what %s stated: %v", what, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", what, err)
	}
}

// blockIndex is a pass of the block index alone over versions of artifact.
func blockIndex(artifact string, versions refs.Versions, markdown string, blocks []model.ArtifactBlock) func(context.Context, pgx.Tx) (graphstore.Uncaused, error) {
	return func(ctx context.Context, tx pgx.Tx) (graphstore.Uncaused, error) {
		_, won, write, err := refs.IndexBlocks(ctx, tx, artifact, versions, markdown, blocks, nil, nil, refs.TypedFacts{})
		if err == nil && !won {
			err = errLostFence
		}
		return write, err
	}
}

// errLostFence is what a test's pass answers where the block index lost its revision fence.
var errLostFence = errors.New("the block index lost its revision fence")

// renderedVersion is a version as a version write renders it (pmdoc.RenderWithBlockOffsets): its
// tree, its markdown, its blocks with their offsets, the mentions located in it and the pointers
// its blocks write.
type renderedVersion struct {
	tree     *pmdoc.Node
	markdown string
	blocks   []model.ArtifactBlock
	located  []text.Located
	pointers []refs.Pointer
}

// renderVersion renders tree as a version write renders it, every block holding an id.
func renderVersion(t *testing.T, tree *pmdoc.Node) renderedVersion {
	t.Helper()
	pmdoc.EnsureBlockIDs(tree)
	markdown, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatalf("render the version: %v", err)
	}
	version := renderedVersion{tree: tree, markdown: markdown, located: text.ExtractAt(markdown, "https://dispatch.example")}
	for _, offset := range offsets {
		version.blocks = append(version.blocks, model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To})
	}
	for _, pointer := range pmdoc.BlockPointers(tree) {
		version.pointers = append(version.pointers, refs.Pointer{Block: pointer.Block, BlockType: pointer.BlockType, Kind: pointer.Attribute, Target: pointer.Target, Targets: pointer.Targets})
	}
	return version
}

// typedIndex is the pass a version write runs over version number of artifact, written at at: the
// typed-node pass reads its decisions, readings and asks, with credited the author of every block
// the document service is asked for (refs.TypedSources.Authors) and no ask answered, and the block
// index and the typed-node pass state what each states.
func typedIndex(artifact string, number int, at time.Time, version renderedVersion, credited model.Actor) func(context.Context, pgx.Tx) (graphstore.Uncaused, error) {
	sources := refs.TypedSources{Authors: func(ids []string) (map[string]model.Actor, error) {
		authors := make(map[string]model.Actor, len(ids))
		for _, id := range ids {
			authors[id] = credited
		}
		return authors, nil
	}}
	return func(ctx context.Context, tx pgx.Tx) (graphstore.Uncaused, error) {
		versions := refs.OneVersion(artifact, number, at)
		facts, err := refs.DecisionFacts(ctx, tx, artifact, versions, version.blocks, version.tree, version.pointers, sources)
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		_, won, write, err := refs.IndexBlocks(ctx, tx, artifact, versions, version.markdown, version.blocks, version.located, version.pointers, facts)
		if err == nil && !won {
			err = errLostFence
		}
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		_, typed, err := refs.IndexDecisionNodes(ctx, tx, artifact, facts, true)
		write.Nodes, write.Tuples = append(write.Nodes, typed.Nodes...), append(write.Tuples, typed.Tuples...)
		return write, err
	}
}

// parsedTree is markdown read as a version write reads it.
func parsedTree(t *testing.T, markdown string) *pmdoc.Node {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	return tree
}

// requireAuthoredBy fails unless the block node address states exactly the authored_by tuples to
// the people logins name, and its node stands at supertag.
func requireAuthoredBy(t *testing.T, database *store.Store, what, address, supertag string, logins ...string) {
	t.Helper()
	ctx := context.Background()
	held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, address, []string{graphstore.TupleAuthoredBy})
	if err != nil {
		t.Fatalf("read the authored_by of %s: %v", what, err)
	}
	var got []string
	for _, tuple := range held {
		got = append(got, tuple.ToKind+":"+tuple.ToID)
	}
	want := make([]string, len(logins))
	for index, login := range logins {
		want[index] = graphstore.KindPerson + ":" + login
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s states the authored_by %v, want %v", what, got, want)
	}
	var stands string
	if err := database.Pool.QueryRow(ctx, `select supertag from graph_nodes where kind = $1 and id = $2`, refs.BlockNodeKind, address).Scan(&stands); err != nil || stands != supertag {
		t.Fatalf("%s's node stands at supertag %q (%v), want %q", what, stands, err, supertag)
	}
}

// The census holds no author of a block of an issue room that has stood in more than one fragment
// (T7 and T12a of dispatch://INFRA-1253's plan): its text rows, keyed by the room's tree, record
// versions of more than one document and no row says which, so no row tells the version that
// first held it. x is written in the spec's fragment at the spec's version 4, which alice wrote,
// and moved into the Verification document's fragment at that document's version 2, which carol
// wrote, so the lowest of its rows is the Verification document's version 2. x records alice, and
// the census reads no difference of who wrote it, nor of y, which stood in one fragment.
func TestTheCensusHoldsNoAuthorOfARoomBlockThatMovedBetweenFragments(t *testing.T) {
	database := storetest.Open(t)
	censusIssue(t, database)
	spec, _ := censusDocument(t, database, "spec-md", "doc", true)
	verification, _ := censusDocument(t, database, "verification-md", "doc", false)
	tree, treeRef := censusDocument(t, database, "tree", "tree", false)
	if _, err := database.Pool.Exec(context.Background(), `
		with held as (insert into issue_trees (issue_key, artifact_id) values ('CORE-1', $1::uuid))
		insert into document_rooms (artifact_id, room) values ($2::uuid, $1::uuid), ($3::uuid, $1::uuid)
	`, tree, spec, verification); err != nil {
		t.Fatalf("move the documents into the issue room: %v", err)
	}
	alice, carol, erin := model.Actor{Kind: "user", ID: "alice"}, model.Actor{Kind: "user", ID: "carol"}, model.Actor{Kind: "user", ID: "erin"}
	const check, words = "Check.", "Moved words."
	markdown := check + "\n\n" + words
	room := func(xIn string) []model.ArtifactBlock {
		return []model.ArtifactBlock{
			{ID: "y", Type: "paragraph", From: 0, To: len(check), Document: verification},
			{ID: "x", Type: "paragraph", From: len(check) + 2, To: len(markdown), Document: xIn},
		}
	}
	treeAt := censusVersion(t, database, tree, 1, "", erin)
	specAt := censusVersion(t, database, spec, 4, words, alice)
	verificationAt := censusVersion(t, database, verification, 1, check, erin)
	censusPass(t, database, "the index of the room with x in the spec's fragment", blockIndex(tree, refs.Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 4, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, markdown, room(spec)))
	requireBlockAuthorsAgree(t, "the census of the room with x in the spec's fragment", storeCensusOf(t, database))

	specNext := censusVersion(t, database, spec, 5, "", carol)
	verificationNext := censusVersion(t, database, verification, 2, markdown, carol)
	censusPass(t, database, "the index of the room with x in the Verification document's fragment", blockIndex(tree, refs.Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 5, At: specNext}, verification: {Number: 2, At: verificationNext},
	}, markdown, room(verification)))
	requireAuthoredBy(t, database, "the moved x", treeRef+"#x", refs.BlockSupertag, "alice")
	requireBlockAuthorsAgree(t, "the census of the room once x moved into the Verification document's fragment", storeCensusOf(t, database))
}

// The census leaves a reading out of both sides of block_author by one attribute, the block's
// type (T7 of dispatch://INFRA-1253's plan): a reading's authored_by is the typed-node pass's,
// naming the one actor who introduced it (refs.TypedSources.Authors), which no row the census
// reads holds. A reading of a decision, in a version alice and bob wrote, is credited to alice
// alone; then, beside a requirement whose authors the census does hold, either it stops being a
// reading - a version retypes it to a paragraph, and its node keeps that authored_by and takes
// supertag block - or a version moves it, reworded, into an ask no one has answered, where it is a
// reading by its type and a node of supertag block. Neither side holds its authored_by, and the
// census reads no difference.
func TestTheCensusLeavesAReadingOutOfBothSidesByItsType(t *testing.T) {
	const first = ":::requirement{#r1}\nIt is sized.\n:::\n\n" +
		"::::decision{#d1}\nWe chose REST.\n\n:::reading{#g1}\nThe agent reads it as REST.\n:::\n::::\n"
	alice, bob, carol := model.Actor{Kind: "user", ID: "alice"}, model.Actor{Kind: "user", ID: "bob"}, model.Actor{Kind: "user", ID: "carol"}
	for _, test := range []struct {
		name, second string
		retyped      bool
	}{
		{name: "a reading retyped to a paragraph", second: ":::requirement{#r1}\nIt is sized.\n:::\n\n::::decision{#d1}\nWe chose REST.\n::::\n",
			retyped: true},
		{name: "a reading in an ask no one has answered", second: ":::requirement{#r1}\nIt is sized.\n:::\n\n::::decision{#d1}\nWe chose REST.\n::::\n\n" +
			"::::ask{#a1}\nWhich transport?\n\n:::reading{#g1}\nThe agent reads the question as REST.\n:::\n::::\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := storetest.Open(t)
			censusIssue(t, database)
			checks, refKey := censusDocument(t, database, "checks-md", "doc", false)
			version := renderVersion(t, parsedTree(t, first))
			at := censusVersion(t, database, checks, 1, version.markdown, alice, bob)
			censusPass(t, database, "the index of version 1", typedIndex(checks, 1, at, version, alice))
			requireAuthoredBy(t, database, "the requirement", refKey+"#r1", refs.BlockSupertag, "alice", "bob")
			requireAuthoredBy(t, database, "the reading of the decision", refKey+"#g1", refs.ReadingSupertag, "alice")
			requireBlockAuthorsAgree(t, "the census of version 1", storeCensusOf(t, database))

			tree := parsedTree(t, test.second)
			if test.retyped {
				tree.Children = append(tree.Children, &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "g1"},
					Children: []*pmdoc.Node{{Type: "text", Text: "The agent reads it as REST."}}})
			}
			version = renderVersion(t, tree)
			at = censusVersion(t, database, checks, 2, version.markdown, carol)
			censusPass(t, database, "the index of version 2", typedIndex(checks, 2, at, version, carol))
			requireAuthoredBy(t, database, "g1 after version 2", refKey+"#g1", refs.BlockSupertag, "alice")
			requireBlockAuthorsAgree(t, "the census of version 2", storeCensusOf(t, database))
		})
	}
}
