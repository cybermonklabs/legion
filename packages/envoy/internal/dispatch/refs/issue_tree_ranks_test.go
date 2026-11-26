package refs

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The tests of this file hold the block index to the rank, the text and the placement it states of
// every block (R1 and R15 of the issue-tree spec, D1, D9 and D15 of its plan). Each writes its
// versions as a version write does and indexes them through IndexBlocks, as block_parent_test.go
// does, and reads the values back through the store's own current read (graphstore.CurrentValue).
// The attribute names are spelled out rather than read off the writer, so a writer that renamed
// one is read here as one that wrote nothing.
const (
	rankAttribute      = "rank"
	textAttribute      = "text"
	placementAttribute = "placement"
	textRankAttribute  = "text_rank"
	textParentKind     = "text_parent"
)

// issueDocuments are the four documents of one issue: its spec, its Verification document, its
// plan and an upload of its own, each as a version write writes it.
type issueDocuments struct {
	spec, verification, plan, upload parentDocument
}

// newIssueDocuments creates one issue of a project of its own holding a spec (the primary
// document), a Verification document and a plan (each its role, under the slug that role's
// document carries), and an upload that plays no role. The first three are documents of the
// issue's tree, whose top-level blocks hang under the issue; the upload's hang under its own node.
func newIssueDocuments(t *testing.T, database *store.Store, name string) issueDocuments {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the issue fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	project := parentProject(name)
	issue := project + "-1"
	createReferenceProject(t, tx, project)
	createReferenceIssue(t, tx, issue, project)
	document := func(slug string, primary bool, role *string) parentDocument {
		var id, refKey string
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
			values ($1, $2, $3, $3, 'doc', $4, $5, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, issue, project, slug, primary, role).Scan(&id, &refKey); err != nil {
			t.Fatalf("create document %s of %s: %v", slug, issue, err)
		}
		topKind, topID := graphstore.KindIssue, issue
		if !primary && role == nil {
			topKind, topID = graphstore.KindArtifact, refKey
		}
		return parentDocument{database: database, issue: issue, artifact: id, refKey: refKey, topKind: topKind, topID: topID}
	}
	verification, plan := model.ArtifactRoleVerification, model.ArtifactRolePlan
	documents := issueDocuments{
		spec:         document("spec", true, nil),
		verification: document("verification-md", false, &verification),
		plan:         document("plan-md", false, &plan),
		upload:       document("notes-md", false, nil),
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the issue fixture: %v", err)
	}
	return documents
}

// lines is a version of top-level paragraphs, one a line, each block's id its words' first.
func lines(ids ...string) blockDoc {
	var doc blockDoc
	for _, id := range ids {
		line := id + " words\n"
		doc.blocks = append(doc.blocks, model.ArtifactBlock{ID: id, Type: "paragraph", From: len(doc.markdown), To: len(doc.markdown) + len(line)})
		doc.markdown += line
	}
	return doc
}

// indexed writes version number of the document and indexes it, failing unless the pass stands,
// and answers what the pass appended.
func indexed(t *testing.T, document parentDocument, number int, doc blockDoc) []graphstore.Tuple {
	t.Helper()
	record := document.writeVersion(t, number, doc.markdown, model.Actor{Kind: "user", ID: "alice"})
	won, stated := document.indexVersion(t, number, doc, record)
	if !won {
		t.Fatalf("version %d of %s did not become the version its blocks stand at", number, document.refKey)
	}
	return stated
}

// currentValue is a block's current value of attribute, and whether it holds one.
func (d parentDocument) currentValue(t *testing.T, blockID, attribute string) (string, bool) {
	t.Helper()
	value, held, err := graphstore.CurrentValue(context.Background(), d.database.Pool, BlockNodeKind, d.address(blockID), attribute)
	if err != nil {
		t.Fatalf("read the %s of %s: %v", attribute, blockID, err)
	}
	return value, held
}

// rankOf is a block's current rank, failing the test where it holds none.
func (d parentDocument) rankOf(t *testing.T, blockID string) string {
	t.Helper()
	rank, held := d.currentValue(t, blockID, rankAttribute)
	if !held {
		t.Fatalf("block %s holds no rank", d.address(blockID))
	}
	return rank
}

// childrenOf is every block node the store hangs under the node kind/id now, in the tree's order:
// by rank compared byte for byte, then by node id (R14's tie), each with its rank.
func childrenOf(t *testing.T, database *store.Store, kind, id string) (children, ranks []string) {
	t.Helper()
	ctx := context.Background()
	held, err := graphstore.CurrentIn(ctx, database.Pool, kind, id, []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read the children of %s/%s: %v", kind, id, err)
	}
	type child struct{ id, rank string }
	var placed []child
	for _, tuple := range held {
		if tuple.FromKind != BlockNodeKind {
			continue
		}
		rank, _, err := graphstore.CurrentValue(ctx, database.Pool, BlockNodeKind, tuple.FromID, rankAttribute)
		if err != nil {
			t.Fatalf("read the rank of %s: %v", tuple.FromID, err)
		}
		placed = append(placed, child{tuple.FromID, rank})
	}
	slices.SortFunc(placed, func(a, b child) int {
		if order := strings.Compare(a.rank, b.rank); order != 0 {
			return order
		}
		return strings.Compare(a.id, b.id)
	})
	for _, c := range placed {
		children, ranks = append(children, c.id), append(ranks, c.rank)
	}
	return children, ranks
}

// stateValue appends one value of a block's node in the store, as a writer of another relation
// than the block index would - a rank the block holds from another document's pass - dated after
// the version that wrote the block.
func stateValue(t *testing.T, document parentDocument, blockID, attribute, value string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, document.database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
			FromKind: BlockNodeKind, FromID: document.address(blockID), Kind: attribute,
			ToKind: graphstore.KindValue, ToID: value,
			At: time.Now(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true,
		}})
	}); err != nil {
		t.Fatalf("state the %s of %s: %v", attribute, blockID, err)
	}
}

// statedOutOf is the tuples of stated out of the blocks of document, of the kinds given (every
// kind where none is).
func statedOutOf(stated []graphstore.Tuple, document parentDocument, kinds ...string) []graphstore.Tuple {
	var out []graphstore.Tuple
	for _, tuple := range stated {
		if tuple.FromKind == BlockNodeKind && strings.HasPrefix(tuple.FromID, document.refKey+"#") &&
			(len(kinds) == 0 || slices.Contains(kinds, tuple.Kind)) {
			out = append(out, tuple)
		}
	}
	return out
}

// Under one parent the spec's blocks come first, whatever the other documents hold (D15).
//
// An issue's three documents are indexed in R21's write order, the Verification document, the
// plan and then the spec, with an upload of the issue's beside them. The issue's children in the
// tree's order are the spec's top-level blocks, then the Verification document's, then the plan's,
// each in its document's order, so the spec's first block is the issue's first child; each
// document's ranks stand in its band; the upload's top-level block hangs under the upload's own
// node and not under the issue. The Verification document then gains a block at its start: the
// spec's first block is the issue's first child still, the new block stands after the spec's
// blocks and before the Verification document's others, and that pass states nothing of the
// spec's blocks.
func TestTheSpecsFirstTopLevelBlockIsTheIssuesFirstChild(t *testing.T) {
	database := parentDatabase(t)
	issue := newIssueDocuments(t, database, "BAND")
	indexed(t, issue.verification, 1, lines("check-one", "check-two"))
	indexed(t, issue.plan, 1, lines("task-one"))
	indexed(t, issue.spec, 1, lines("memo", "requirement-one"))
	indexed(t, issue.upload, 1, lines("note-one"))

	want := []string{
		issue.spec.address("memo"), issue.spec.address("requirement-one"),
		issue.verification.address("check-one"), issue.verification.address("check-two"),
		issue.plan.address("task-one"),
	}
	children, ranks := childrenOf(t, database, graphstore.KindIssue, issue.spec.issue)
	if !slices.Equal(children, want) {
		t.Fatalf("the issue's children in the tree's order are %v ranked %v, want %v: the spec's first block is the issue's first child",
			children, ranks, want)
	}
	for index, band := range []byte{'1', '1', '3', '3', '4'} {
		if ranks[index] == "" || ranks[index][0] != band {
			t.Errorf("block %s is ranked %q, want a rank of band %c", children[index], ranks[index], band)
		}
	}
	if parent := issue.upload.currentParent(t, "note-one"); parent.ToKind != graphstore.KindArtifact || parent.ToID != issue.upload.refKey {
		t.Errorf("the upload's top-level block is part of %s/%s, want the upload's own node artifact/%s",
			parent.ToKind, parent.ToID, issue.upload.refKey)
	}
	if rank := issue.upload.rankOf(t, "note-one"); rank[0] != '5' {
		t.Errorf("the upload's top-level block is ranked %q, want a rank of band 5", rank)
	}

	specRanks := []string{issue.spec.rankOf(t, "memo"), issue.spec.rankOf(t, "requirement-one")}
	stated := indexed(t, issue.verification, 2, lines("check-zero", "check-one", "check-two"))
	want = slices.Insert(want, 2, issue.verification.address("check-zero"))
	if children, ranks := childrenOf(t, database, graphstore.KindIssue, issue.spec.issue); !slices.Equal(children, want) {
		t.Errorf("after the Verification document gained a block at its start the issue's children are %v ranked %v, want %v",
			children, ranks, want)
	}
	if got := statedOutOf(stated, issue.spec); len(got) != 0 {
		t.Errorf("the Verification document's pass stated %+v of the spec's blocks, want nothing", got)
	}
	if now := []string{issue.spec.rankOf(t, "memo"), issue.spec.rankOf(t, "requirement-one")}; !slices.Equal(now, specRanks) {
		t.Errorf("the spec's blocks were ranked %v and are ranked %v after another document's pass, want them kept", specRanks, now)
	}
}

// A block that holds a rank of another band - as one moved into this document's fragment from
// another document's holds the rank that document gave it - takes a rank of its own document's
// band at the next pass, and its siblings keep theirs: only a rank already in the block's band is
// one the pass keeps (D15). The rank stated before is planted as another document's pass would
// leave it: of band 3, the Verification document's, on a block of the plan, band 4.
func TestABlockHoldingAnotherDocumentsBandTakesARankOfItsOwn(t *testing.T) {
	database := parentDatabase(t)
	issue := newIssueDocuments(t, database, "MOVE")
	indexed(t, issue.plan, 1, lines("task-one", "task-two"))
	first := issue.plan.rankOf(t, "task-one")
	stateValue(t, issue.plan, "task-two", rankAttribute, "3U")

	stated := indexed(t, issue.plan, 2, lines("task-one", "task-two"))
	moved := issue.plan.rankOf(t, "task-two")
	if moved[0] != '4' {
		t.Errorf("the block that held the Verification document's rank 3U is ranked %q after the plan's pass, want a rank of band 4", moved)
	}
	if kept := issue.plan.rankOf(t, "task-one"); kept != first || kept >= moved {
		t.Errorf("its sibling is ranked %q, want its first rank %q kept and below the moved block's %q", kept, first, moved)
	}
	if ranks := statedOutOf(stated, issue.plan, rankAttribute); len(ranks) != 1 || ranks[0].FromID != issue.plan.address("task-two") {
		t.Errorf("the plan's pass stated the ranks %+v, want one, the moved block's", ranks)
	}
}

// Two siblings that hold one rank are told apart inside their own document's band, and no other
// document's rank moves (D15). The spec's second block is given its first block's rank, and the
// Verification document's two blocks are given one rank between them too; a pass of the spec then
// ranks the spec's three blocks in increasing order with one new rank, the tied block's, and
// states nothing of the Verification document, whose tie stands for its own pass to repair.
func TestATieIsRepairedInsideOneDocumentsBandAlone(t *testing.T) {
	database := parentDatabase(t)
	issue := newIssueDocuments(t, database, "TIE")
	indexed(t, issue.spec, 1, lines("requirement-one", "requirement-two", "requirement-three"))
	indexed(t, issue.verification, 1, lines("check-one", "check-two"))
	stateValue(t, issue.spec, "requirement-two", rankAttribute, issue.spec.rankOf(t, "requirement-one"))
	tie := issue.verification.rankOf(t, "check-one")
	stateValue(t, issue.verification, "check-two", rankAttribute, tie)

	stated := indexed(t, issue.spec, 2, lines("requirement-one", "requirement-two", "requirement-three"))
	ranks := []string{issue.spec.rankOf(t, "requirement-one"), issue.spec.rankOf(t, "requirement-two"), issue.spec.rankOf(t, "requirement-three")}
	if !(ranks[0] < ranks[1] && ranks[1] < ranks[2]) || ranks[1][0] != '1' {
		t.Errorf("after the spec's pass its blocks are ranked %v, want three increasing ranks of band 1", ranks)
	}
	if got := statedOutOf(stated, issue.spec, rankAttribute); len(got) != 1 || got[0].FromID != issue.spec.address("requirement-two") {
		t.Errorf("the spec's pass stated the ranks %+v, want one, the tied block's", got)
	}
	if got := statedOutOf(stated, issue.verification); len(got) != 0 {
		t.Errorf("the spec's pass stated %+v of the Verification document's blocks, want nothing", got)
	}
	if one, two := issue.verification.rankOf(t, "check-one"), issue.verification.rankOf(t, "check-two"); one != tie || two != tie {
		t.Errorf("the Verification document's blocks are ranked %q and %q after the spec's pass, want the tie %q left to its own pass", one, two, tie)
	}
}

// requireTextsProjected holds document_block_texts to the text values: the newest row of each
// block the table holds and the block's current text value are one text, character for character,
// and a row that records the block deleted stands beside no current text. Each block the document
// holds also holds the placement text.
func (d parentDocument) requireTextsProjected(t *testing.T, after string, doc blockDoc) {
	t.Helper()
	rows, err := d.database.Pool.Query(context.Background(), `
		select distinct on (block_id) block_id, text from document_block_texts
		where artifact_id = $1::uuid order by block_id, version desc
	`, d.artifact)
	if err != nil {
		t.Fatalf("read the block texts of %s: %v", d.refKey, err)
	}
	newest := map[string]*string{}
	for rows.Next() {
		var id string
		var text *string
		if err := rows.Scan(&id, &text); err != nil {
			t.Fatalf("scan a block text of %s: %v", d.refKey, err)
		}
		newest[id] = text
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the block texts of %s: %v", d.refKey, err)
	}
	if len(newest) == 0 {
		t.Fatalf("after %s document_block_texts holds no row of %s, so no text was compared", after, d.refKey)
	}
	for id, text := range newest {
		value, held := d.currentValue(t, id, textAttribute)
		if text == nil {
			// The row records the block deleted: its node is gone, whatever value it still holds.
			if node, found, err := graphstore.ReadNode(context.Background(), d.database.Pool, BlockNodeKind, d.address(id)); err != nil ||
				!found || node.State != graphstore.StateGone {
				t.Errorf("after %s block %s's newest document_block_texts row records it deleted, but its node reads %+v (found %t, %v)",
					after, id, node, found, err)
			}
			continue
		}
		if !held || value != *text {
			t.Errorf("after %s block %s's newest document_block_texts row is %q and its text value is %q (held %t)", after, id, *text, value, held)
		}
	}
	for _, block := range doc.blocks {
		if want := doc.markdown[block.From:block.To]; newest[block.ID] == nil || *newest[block.ID] != want {
			t.Errorf("after %s block %s's text is not its slice of the version %q", after, block.ID, want)
		}
		if placement, _ := d.currentValue(t, block.ID, placementAttribute); placement != "text" {
			t.Errorf("after %s block %s's placement is %q, want text", after, block.ID, placement)
		}
	}
}

// A block's text value is its whole slice of the version, children included, and
// document_block_texts is that value's projection (D9; R15). After each version the two agree for
// every block the version holds. A version that changes one block's text states that block's text
// value alone; one that drops a block states no text of it, its node gone beside the row that
// records it deleted; one that restores it states its text again; and a block that holds no text
// value - one an earlier build indexed, whose text tuples are taken out here as that build left
// them - takes one at the next pass, its text unchanged.
func TestEveryBlocksTextValueIsItsDocumentBlockTextsRow(t *testing.T) {
	ctx := context.Background()
	database := parentDatabase(t)
	document := newParentDocument(t, database, "TEXT", "texts-md")

	first := nestedDocument("tail one\n")
	indexed(t, document, 1, first)
	document.requireTextsProjected(t, "the first version", first)
	if top, _ := document.currentValue(t, "top1", textAttribute); top != first.markdown[:8] {
		t.Errorf("top1's text value is %q, want its whole slice %q, the blocks it holds included", top, first.markdown[:8])
	}

	second := nestedDocument("tail two\n")
	stated := indexed(t, document, 2, second)
	if texts := statedOutOf(stated, document, textAttribute); len(texts) != 1 || texts[0].FromID != document.address("top2") {
		t.Errorf("the version that changed top2 alone stated the texts %+v, want top2's alone", texts)
	}
	document.requireTextsProjected(t, "the version that changed top2", second)

	withoutLeaf := nestedDocument("tail two\n")
	withoutLeaf.blocks = slices.DeleteFunc(withoutLeaf.blocks, func(block model.ArtifactBlock) bool { return block.ID == "leaf" })
	stated = indexed(t, document, 3, withoutLeaf)
	if texts := statedOutOf(stated, document, textAttribute); len(texts) != 0 {
		t.Errorf("the version that dropped leaf stated the texts %+v, want none", texts)
	}
	document.requireTextsProjected(t, "the version that dropped leaf", withoutLeaf)

	stated = indexed(t, document, 4, second)
	if texts := statedOutOf(stated, document, textAttribute); len(texts) != 1 || texts[0].FromID != document.address("leaf") {
		t.Errorf("the version that restored leaf stated the texts %+v, want leaf's alone", texts)
	}
	document.requireTextsProjected(t, "the version that restored leaf", second)

	if _, err := database.Pool.Exec(ctx, `delete from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3`,
		BlockNodeKind, document.address("mid"), textAttribute); err != nil {
		t.Fatalf("take mid's text values out as an earlier build left them: %v", err)
	}
	stated = indexed(t, document, 5, second)
	if texts := statedOutOf(stated, document, textAttribute); len(texts) != 1 || texts[0].FromID != document.address("mid") ||
		texts[0].ToID != second.markdown[4:8] {
		t.Errorf("the pass over a block holding no text value stated the texts %+v, want mid's %q alone", texts, second.markdown[4:8])
	}
	document.requireTextsProjected(t, "the pass that gave mid its text", second)
}

// A state the node-visibility spec sets stands through every pass of the block index that moves the
// block's text or its place, and the pass states the block's values all the same: a settled
// requirement whose words change and that moves after its sibling stays settled and takes its new
// text and its new rank; an overtaken one stays overtaken; and a requirement the version drops is
// gone, and live again where a version restores it, each state in the log once (decision 4).
func TestAReindexKeepsTheStateANodeWasGiven(t *testing.T) {
	ctx := context.Background()
	database := parentDatabase(t)
	writer := model.Actor{Kind: "user", ID: "alice"}
	document := newParentDocument(t, database, "STATE", "states-md")
	const requirement = "r-ship"
	version := func(words string, first bool) blockDoc {
		text, other := words+"\n", "bb\n"
		if first {
			return blockDoc{markdown: text + other, blocks: []model.ArtifactBlock{
				{ID: requirement, Type: "requirement", From: 0, To: len(text)},
				{ID: "b", Type: "paragraph", From: len(text), To: len(text) + len(other)},
			}}
		}
		return blockDoc{markdown: other + text, blocks: []model.ArtifactBlock{
			{ID: "b", Type: "paragraph", From: 0, To: len(other)},
			{ID: requirement, Type: "requirement", From: len(other), To: len(other) + len(text)},
		}}
	}
	mark := func(state string) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			return graphstore.Write(ctx, tx, []graphstore.Node{{
				Kind: BlockNodeKind, ID: document.address(requirement), Supertag: requirementType,
				Visibility: graphstore.VisibilityProject, State: state,
				MadeAt: time.Now(), MadeBy: writer, SourceEvent: graphstore.NoEvent,
			}}, nil)
		}); err != nil {
			t.Fatalf("mark the requirement %s: %v", state, err)
		}
	}

	indexed(t, document, 1, version("We ship it.", true))
	mark(graphstore.StateSettled)
	moved := version("We ship it today.", false)
	indexed(t, document, 2, moved)
	requireBlockState(t, document, requirement, graphstore.StateSettled, graphstore.VisibilityProject,
		[]string{graphstore.StateLive, graphstore.StateSettled})
	if text, _ := document.currentValue(t, requirement, textAttribute); text != "We ship it today.\n" {
		t.Errorf("the settled requirement's text is %q after the version that changed it, want the new words", text)
	}
	if mine, sibling := document.rankOf(t, requirement), document.rankOf(t, "b"); mine <= sibling {
		t.Errorf("the settled requirement is ranked %q and the block now before it %q, want it after", mine, sibling)
	}

	mark(graphstore.StateOvertaken)
	indexed(t, document, 3, version("We ship it this week.", false))
	requireBlockState(t, document, requirement, graphstore.StateOvertaken, graphstore.VisibilityProject,
		[]string{graphstore.StateLive, graphstore.StateSettled, graphstore.StateOvertaken})

	indexed(t, document, 4, blockDoc{markdown: "bb\n", blocks: []model.ArtifactBlock{{ID: "b", Type: "paragraph", From: 0, To: 3}}})
	requireBlockState(t, document, requirement, graphstore.StateGone, graphstore.VisibilityProject,
		[]string{graphstore.StateLive, graphstore.StateSettled, graphstore.StateOvertaken, graphstore.StateGone})
	indexed(t, document, 5, moved)
	requireBlockState(t, document, requirement, graphstore.StateLive, graphstore.VisibilityProject,
		[]string{graphstore.StateLive, graphstore.StateSettled, graphstore.StateOvertaken, graphstore.StateGone, graphstore.StateLive})
}

// currentTextParent is the one far end a block's text_parent names now, failing unless the
// block holds exactly one: a block stands in one place of its document's text at a time.
func (d parentDocument) currentTextParent(t *testing.T, blockID string) graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), d.database.Pool, BlockNodeKind, d.address(blockID), []string{textParentKind})
	if err != nil {
		t.Fatalf("read the text parent of %s: %v", blockID, err)
	}
	if len(held) != 1 {
		t.Fatalf("block %s holds %d current text_parent tuples, want exactly one", blockID, len(held))
	}
	return held[0]
}

// textRankOf is a block's current text_rank, failing the test where it holds none.
func (d parentDocument) textRankOf(t *testing.T, blockID string) string {
	t.Helper()
	held, ok := d.currentValue(t, blockID, textRankAttribute)
	if !ok {
		t.Fatalf("block %s holds no text_rank", d.address(blockID))
	}
	return held
}

// textPlaceDocument is a version of a spec whose box holds a paragraph, a block whose id is past
// text.MaxBlockIDBytes holding a paragraph of its own, and a requirement that names no parent,
// followed by the top-level paragraphs given, in order, each one line.
func textPlaceDocument(paragraphs ...string) blockDoc {
	const (
		inner = "inner words\n"
		deep  = "deep words\n"
		req   = "req words\n"
	)
	long := strings.Repeat("l", text.MaxBlockIDBytes+1)
	holder := "long:" + deep
	box := "box:" + inner + holder + req
	doc := blockDoc{markdown: box, blocks: []model.ArtifactBlock{
		{ID: "box", Type: "paragraph", From: 0, To: len(box)},
		{ID: "inner", Type: "paragraph", From: 4, To: 4 + len(inner)},
		{ID: long, Type: "paragraph", From: 4 + len(inner), To: 4 + len(inner) + len(holder)},
		{ID: "deep", Type: "paragraph", From: 4 + len(inner) + 5, To: 4 + len(inner) + len(holder)},
		{ID: "req", Type: "requirement", From: 4 + len(inner) + len(holder), To: len(box)},
	}}
	for _, id := range paragraphs {
		line := id + " words\n"
		doc.blocks = append(doc.blocks, model.ArtifactBlock{ID: id, Type: "paragraph", From: len(doc.markdown), To: len(doc.markdown) + len(line)})
		doc.markdown += line
	}
	return doc
}

// Every block that is a node holds its place in its document's text as two dated facts beside its
// rank: a text_parent tuple to the tightest block of the version
// that holds it and is a node, or to its document's node where no such block holds it, and a
// text_rank value among the blocks that share that text parent, in the version's text order.
//
// The first index of a spec states both of every block, dated at the pass's own time by the
// version's writer: the box's paragraph, its requirement and the paragraph inside the block whose
// id is past the bound, which is no node, each name the box, whose own text parent and the
// top-level paragraphs' is the spec's node; the requirement's part_of is the issue (it names no
// parent of its own), which its text parent is not; and the text ranks of each text parent's
// children increase in text order, in the spec's band. graph_edges renders no text_parent. A
// re-index nothing moved states neither fact. A version that swaps two top-level paragraphs states
// one text_rank, the one that moved forward, no text_parent, and moves no changed_version. A
// version of the plan that moves a block under another holder, changing no text, retracts its old
// text parent and asserts the new one at one time, the pass's own, and states no text_rank, the
// block being the one text child of either holder. And a block whose text facts were taken out,
// as a build before this one leaves a block, takes both again at the next pass, and no other block
// takes either.
func TestEveryBlockHoldsItsPlaceInItsDocumentsText(t *testing.T) {
	ctx := context.Background()
	database := parentDatabase(t)
	issue := newIssueDocuments(t, database, "PLACE")
	spec := issue.spec
	writer := model.Actor{Kind: "user", ID: "alice"}
	document := Target{Kind: graphstore.KindArtifact, ID: spec.refKey}
	box := Target{Kind: BlockNodeKind, ID: spec.address("box")}

	first := textPlaceDocument("p1", "p2")
	record := spec.writeVersion(t, 1, first.markdown, writer)
	won, stated, passAt := spec.indexVersionAt(t, 1, first, record)
	if !won {
		t.Fatalf("the spec's first index did not become the version its blocks stand at")
	}
	want := map[string]Target{"box": document, "inner": box, "deep": box, "req": box, "p1": document, "p2": document}
	for id, parent := range want {
		if held := spec.currentTextParent(t, id); held.ToKind != parent.Kind || held.ToID != parent.ID {
			t.Errorf("after the first index %s's text parent is %s/%s, want %s/%s", id, held.ToKind, held.ToID, parent.Kind, parent.ID)
		}
	}
	if parent := spec.currentParent(t, "req"); parent.ToKind != graphstore.KindIssue || parent.ToID != spec.issue {
		t.Errorf("the requirement that names no parent is part of %s/%s, want the issue %s: its text parent is no part_of", parent.ToKind, parent.ToID, spec.issue)
	}
	for _, run := range [][]string{{"box", "p1", "p2"}, {"inner", "deep", "req"}} {
		for index, id := range run {
			held := spec.textRankOf(t, id)
			if held[0] != '1' {
				t.Errorf("%s's text_rank is %q, want one of the spec's band 1", id, held)
			}
			if index > 0 && spec.textRankOf(t, run[index-1]) >= held {
				t.Errorf("%s's text_rank %q does not stand after %s's %q, the block before it in the text", id, held, run[index-1], spec.textRankOf(t, run[index-1]))
			}
		}
	}
	facts := statedOutOf(stated, spec, textParentKind, textRankAttribute)
	if len(facts) != 2*len(want) {
		t.Errorf("the first index stated %d text facts, want a text_parent and a text_rank of each of the %d blocks that are nodes: %+v", len(facts), len(want), facts)
	}
	for _, fact := range facts {
		if !fact.Asserted || fact.At.Before(passAt) || !fact.Who.SameAs(record.writer) || fact.SourceEvent != record.event {
			t.Errorf("the first index stated %+v, want an assertion at or after the pass's time %s by %+v under event %d", fact, passAt, record.writer, record.event)
		}
	}
	var edges int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_edges where kind = $1`, textParentKind).Scan(&edges); err != nil {
		t.Fatalf("read graph_edges: %v", err)
	}
	if edges != 0 {
		t.Errorf("graph_edges holds %d text_parent edges, want none: no block_refs row projects one", edges)
	}

	if stated := indexed(t, spec, 2, first); len(statedOutOf(stated, spec, textParentKind, textRankAttribute)) != 0 {
		t.Errorf("a re-index nothing moved stated %+v, want no text fact", statedOutOf(stated, spec, textParentKind, textRankAttribute))
	}

	ranks := map[string]string{"box": spec.textRankOf(t, "box"), "p1": spec.textRankOf(t, "p1")}
	changed := spec.changes(t)
	swapped := textPlaceDocument("p2", "p1")
	stated = indexed(t, spec, 3, swapped)
	if got := statedOutOf(stated, spec, textParentKind); len(got) != 0 {
		t.Errorf("the version that swapped two paragraphs stated the text parents %+v, want none", got)
	}
	if got := statedOutOf(stated, spec, textRankAttribute); len(got) != 1 || got[0].FromID != spec.address("p2") {
		t.Errorf("the version that swapped two paragraphs stated the text ranks %+v, want one, of p2, which moved forward", got)
	}
	if now := spec.textRankOf(t, "p2"); !(ranks["box"] < now && now < ranks["p1"]) || spec.textRankOf(t, "p1") != ranks["p1"] {
		t.Errorf("after the swap p2's text_rank is %q and p1's %q, want p2's between the box's %q and p1's kept %q",
			now, spec.textRankOf(t, "p1"), ranks["box"], ranks["p1"])
	}
	if now := spec.changes(t); !maps.Equal(now, changed) {
		t.Errorf("the swap moved the blocks' changed versions from %v to %v, want none moved", changed, now)
	}

	plan := issue.plan
	planWriter := model.Actor{Kind: "user", ID: "bob"}
	indexed(t, plan, 1, moverUnderLeft())
	rank := plan.textRankOf(t, "mover")
	planChanged := plan.changes(t)
	moved := plan.writeVersion(t, 2, moverUnderRight().markdown, planWriter)
	_, stated, passAt = plan.indexVersionAt(t, 2, moverUnderRight(), moved)
	parents := statedOutOf(stated, plan, textParentKind)
	if len(parents) != 2 || parents[0].Asserted || parents[0].ToID != plan.address("left") ||
		!parents[1].Asserted || parents[1].ToID != plan.address("right") || !parents[0].At.Equal(parents[1].At) {
		t.Fatalf("the version that moved mover under right stated the text parents %+v, want left's retraction and right's assertion at one time", parents)
	}
	for _, parent := range parents {
		if parent.At.Before(passAt) || !parent.Who.SameAs(moved.writer) || parent.SourceEvent != moved.event {
			t.Errorf("the move stated %+v, want it at or after the pass's time %s by %+v under event %d", parent, passAt, moved.writer, moved.event)
		}
	}
	if got := statedOutOf(stated, plan, textRankAttribute); len(got) != 0 || plan.textRankOf(t, "mover") != rank {
		t.Errorf("the move stated the text ranks %+v and left mover at %q, want none stated and its %q kept: it is the one text child of either holder",
			got, plan.textRankOf(t, "mover"), rank)
	}
	if now := plan.changes(t); !maps.Equal(now, planChanged) {
		t.Errorf("the move changed the plan's changed versions from %v to %v, want none moved", planChanged, now)
	}

	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		held := spec.currentTextParent(t, "p1")
		return graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
			FromKind: BlockNodeKind, FromID: spec.address("p1"), Kind: textParentKind, ToKind: held.ToKind, ToID: held.ToID,
			At: time.Now(), Who: writer, SourceEvent: graphstore.NoEvent, Asserted: false,
		}, {
			FromKind: BlockNodeKind, FromID: spec.address("p1"), Kind: textRankAttribute, ToKind: graphstore.KindValue, ToID: ranks["p1"],
			At: time.Now(), Who: writer, SourceEvent: graphstore.NoEvent, Asserted: false,
		}})
	}); err != nil {
		t.Fatalf("take p1's text facts out as a build before this one leaves a block: %v", err)
	}
	stated = indexed(t, spec, 4, swapped)
	if got := statedOutOf(stated, spec, textParentKind, textRankAttribute); len(got) != 2 || got[0].FromID != spec.address("p1") || got[1].FromID != spec.address("p1") {
		t.Errorf("the pass over a block holding no text facts stated %+v, want p1's text_parent and text_rank alone", got)
	}
	if held := spec.currentTextParent(t, "p1"); held.ToKind != document.Kind || held.ToID != document.ID {
		t.Errorf("p1's text parent is %s/%s after the pass, want the spec's node", held.ToKind, held.ToID)
	}
}
