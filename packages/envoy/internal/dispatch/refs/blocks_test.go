package refs

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// part is one block of a test document: its id, its node type and its text.
type part struct{ id, nodeType, text string }

// documentOf renders parts as one markdown document, a blank line between each, with the blocks
// as a renderer hands them to IndexBlocks: in document order, each with its offsets into the
// markdown.
func documentOf(parts []part) (string, []model.ArtifactBlock) {
	var markdown strings.Builder
	blocks := make([]model.ArtifactBlock, len(parts))
	for i, p := range parts {
		if i > 0 {
			markdown.WriteString("\n\n")
		}
		from := markdown.Len()
		markdown.WriteString(p.text)
		blocks[i] = model.ArtifactBlock{ID: p.id, Type: p.nodeType, From: from, To: markdown.Len()}
	}
	return markdown.String(), blocks
}

// windowRow is one embeddings window row as its owner's indexer wrote it: the blocks it covers,
// the text it embeds, the hash of that text and xmin, which any write of the row moves, so an
// unchanged xmin proves the row was not rewritten.
type windowRow struct {
	covers []string
	text   string
	hash   string
	xmin   string
}

// untouched reports whether two readings of a window row are the same row, with no write
// between them: equal values and the same xmin.
func (row windowRow) untouched(other windowRow) bool {
	return slices.Equal(row.covers, other.covers) && row.text == other.text && row.hash == other.hash && row.xmin == other.xmin
}

// windowRowsOf is every window row of owner ownerKey of kind kind, by the id suffix after the
// owner key.
func windowRowsOf(t *testing.T, tx pgx.Tx, kind, ownerKey string) map[string]windowRow {
	t.Helper()
	rows, err := tx.Query(context.Background(), `
		select id, covers, text_snapshot, content_hash, xmin::text from embeddings
		where kind = $1 and split_part(id, '#', 1) = $2
	`, kind, ownerKey)
	if err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, ownerKey, err)
	}
	defer rows.Close()
	windows := map[string]windowRow{}
	for rows.Next() {
		var id string
		var row windowRow
		if err := rows.Scan(&id, &row.covers, &row.text, &row.hash, &row.xmin); err != nil {
			t.Fatalf("scan a window of %s %s: %v", kind, ownerKey, err)
		}
		windows[strings.TrimPrefix(id, ownerKey+"#")] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, ownerKey, err)
	}
	return windows
}

// windowFixture opens a database, begins the transaction the test reads, rolled back when it
// ends, and creates the project and the issue its items hang on.
func windowFixture(t *testing.T) pgx.Tx {
	t.Helper()
	database := storetest.Open(t)
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the fixture transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	createReferenceProject(t, tx, "WIND")
	createReferenceIssue(t, tx, "WIND-1", "WIND")
	return tx
}

// indexVersionAndWindow indexes one version of a document's blocks and writes the windows
// those blocks stand in. A version write records the blocks alone: the block-index tick's
// window arm writes a document's windows, in a transaction of its own over the markdown that
// version holds (IndexBlocks, docs.windowDocument). This is that arm's write, run here in the
// fixture's transaction.
func indexVersionAndWindow(t *testing.T, tx pgx.Tx, artifact string, version int, markdown string, blocks []model.ArtifactBlock, what string) {
	t.Helper()
	ctx := context.Background()
	if _, _, _, err := IndexBlocks(ctx, tx, artifact, version, time.Now(), markdown, blocks, nil, nil, "", TypedFacts{}); err != nil {
		t.Fatalf("index %s: %v", what, err)
	}
	if err := WriteDocumentWindows(ctx, tx, artifact, markdown, blocks); err != nil {
		t.Fatalf("window %s: %v", what, err)
	}
}

// A window's id is its owner's key and the first block it covers, its covers are the blocks
// whose text it holds in order, and its text is the context and then those blocks: the
// document's title and the heading above for a document, under the artifact id; the issue's
// title for a comment, under comment/<uuid>; the question for an ask's parts, under
// ask/<uuid>.
func TestAWindowRecordsTheBlocksItCovers(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "notes-md")
	markdown, blocks := documentOf([]part{
		{"b1", "heading", "# Windows"},
		{"b2", "paragraph", "A window holds neighbouring blocks."},
		{"b3", "heading", "# Pieces"},
		{"b4", "paragraph", "A long block becomes pieces."},
	})
	indexVersionAndWindow(t, tx, artifact, 1, markdown, blocks, "the document")
	document := windowRowsOf(t, tx, DocumentWindowKind, artifact)
	if got := slices.Sorted(maps.Keys(document)); !slices.Equal(got, []string{"b1", "b3"}) {
		t.Fatalf("the document's windows are %v, want one for each heading section", got)
	}
	if got := document["b1"].covers; !slices.Equal(got, []string{"b1", "b2"}) {
		t.Errorf("window b1 covers %v, want the heading and the paragraph under it", got)
	}
	if want := "notes-md\n\n# Windows\n\n# Windows\n\nA window holds neighbouring blocks."; document["b1"].text != want {
		t.Errorf("window b1 embeds %q, want %q: the title and the heading above it, then its blocks", document["b1"].text, want)
	}
	if got := document["b3"].covers; !slices.Equal(got, []string{"b3", "b4"}) {
		t.Errorf("window b3 covers %v, want the second section's blocks", got)
	}

	var comment string
	if err := tx.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('WIND-1', '{"kind":"user","id":"alice"}', $1) returning id::text
	`, "The first paragraph.\n\nThe second paragraph.").Scan(&comment); err != nil {
		t.Fatalf("insert the comment: %v", err)
	}
	if _, _, err := IndexItem(ctx, tx, ItemComment, comment, "The first paragraph.\n\nThe second paragraph.", "", time.Now(), nil, nil); err != nil {
		t.Fatalf("index the comment: %v", err)
	}
	// An item's index writes no window (IndexItem): the block-index tick's window arm writes
	// them in a transaction of its own (docs.windowItem, WriteItemWindows). This is that arm's
	// write, run here in the fixture's transaction, and what it writes is read below.
	if written, err := WriteItemWindows(ctx, tx, ItemComment, comment); err != nil || !written {
		t.Fatalf("window the comment = %v, %v; want the window arm to write them", written, err)
	}
	comments := windowRowsOf(t, tx, "comment_block", "comment/"+comment)
	if got := slices.Sorted(maps.Keys(comments)); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("the comment's windows are %v, want one covering both its blocks", got)
	}
	if got := comments["b1"]; !slices.Equal(got.covers, []string{"b1", "b2"}) ||
		got.text != "Reference issue\n\nThe first paragraph.\n\nThe second paragraph." {
		t.Errorf("the comment's window is %+v, want both blocks under the issue's title", got)
	}

	ask := insertAsk(t, tx, "WIND-1", "Which region?", model.AskOption{Label: "eu", Description: "for latency"})
	indexAskPartsAt(t, tx, ask, 1)
	if written, err := WriteItemWindows(ctx, tx, ItemAsk, ask.ID); err != nil || !written {
		t.Fatalf("window the ask = %v, %v; want the window arm to write them", written, err)
	}
	parts := windowRowsOf(t, tx, "ask_part", "ask/"+ask.ID)
	if got := slices.Sorted(maps.Keys(parts)); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("the ask's windows are %v, want one covering its question and its option", got)
	}
	if got := parts["b1"]; !slices.Equal(got.covers, []string{"b1", "b2"}) ||
		got.text != "Which region?\n\nWhich region?\n\neu — for latency" {
		t.Errorf("the ask's window is %+v, want its parts under the question that gives them meaning", got)
	}
}

// An item gone since the window arm listed it is no one's to window: WriteItemWindows answers
// (false, nil) for it rather than an error. A comment's and a message's lock and window text
// are one statement now (itemWindowText), so no row from that statement is the whole of what
// says the item is gone, where it used to be no row from the lock; an ask still takes its lock
// on its own and reads its parts after it.
func TestWindowingAnItemThatIsGoneWritesNothing(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	body := "The comment deleted below."
	var comment string
	if err := tx.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('WIND-1', '{"kind":"user","id":"alice"}', $1) returning id::text
	`, body).Scan(&comment); err != nil {
		t.Fatalf("insert the comment: %v", err)
	}
	if _, _, err := IndexItem(ctx, tx, ItemComment, comment, body, "", time.Now(), nil, nil); err != nil {
		t.Fatalf("index the comment: %v", err)
	}
	if _, err := tx.Exec(ctx, `delete from comments where id = $1::uuid`, comment); err != nil {
		t.Fatalf("delete the comment: %v", err)
	}
	// A uuid no row of any of the three tables holds, which is the same case for a kind this
	// test writes no row of.
	never := "00000000-0000-0000-0000-0000000000ff"
	for _, item := range []struct {
		kind ItemKind
		id   string
		what string
	}{
		{ItemComment, comment, "a comment deleted after it was indexed"},
		{ItemComment, never, "a comment that never existed"},
		{ItemMessage, never, "a message that never existed"},
		{ItemAsk, never, "an ask that never existed"},
	} {
		written, err := WriteItemWindows(ctx, tx, item.kind, item.id)
		if written || err != nil {
			t.Errorf("window %s = %v, %v; want (false, nil)", item.what, written, err)
		}
	}
}

// An edit rewrites the windows the edited block is in and leaves every other row untouched: a
// reworded paragraph moves the hash of its own window alone, and an edited heading moves the
// hash of every window of its section, since the heading is the context they embed.
func TestOneParagraphEditReEmbedsOnlyItsWindows(t *testing.T) {
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "notes-md")
	parts := []part{
		{"b1", "heading", "# Windows"},
		{"b2", "paragraph", "A window holds neighbouring blocks."},
		{"b3", "heading", "# Pieces"},
		{"b4", "paragraph", "A long block becomes pieces."},
	}
	markdown, blocks := documentOf(parts)
	indexVersionAndWindow(t, tx, artifact, 1, markdown, blocks, "version 1")
	before := windowRowsOf(t, tx, DocumentWindowKind, artifact)

	edited := slices.Clone(parts)
	edited[1].text = "A window holds the blocks beside each other."
	markdown, blocks = documentOf(edited)
	indexVersionAndWindow(t, tx, artifact, 2, markdown, blocks, "the edited paragraph")
	after := windowRowsOf(t, tx, DocumentWindowKind, artifact)
	if before["b1"].hash == after["b1"].hash {
		t.Errorf("window b1 still embeds %q after its paragraph was reworded", after["b1"].text)
	}
	if !before["b3"].untouched(after["b3"]) {
		t.Errorf("window b3 was rewritten (%+v, was %+v); an edit under another heading must not touch it", after["b3"], before["b3"])
	}

	edited = slices.Clone(edited)
	edited[0].text = "# What a window is"
	markdown, blocks = documentOf(edited)
	indexVersionAndWindow(t, tx, artifact, 3, markdown, blocks, "the edited heading")
	headed := windowRowsOf(t, tx, DocumentWindowKind, artifact)
	if headed["b1"].hash == after["b1"].hash {
		t.Errorf("window b1 still embeds %q after its heading was edited", headed["b1"].text)
	}
	if !strings.HasPrefix(headed["b1"].text, "notes-md\n\n# What a window is\n\n") {
		t.Errorf("window b1 embeds %q, want the edited heading as its context", headed["b1"].text)
	}
	if !headed["b3"].untouched(after["b3"]) {
		t.Errorf("window b3 was rewritten (%+v, was %+v); an edited heading moves its own section's windows and no other", headed["b3"], after["b3"])
	}
}

// A version that no longer holds a block leaves no window of it: the rows the new set does not
// name are deleted, so a deleted section's window goes with it.
func TestADeletedBlockTakesItsWindowRow(t *testing.T) {
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "notes-md")
	parts := []part{
		{"b1", "heading", "# Windows"},
		{"b2", "paragraph", "A window holds neighbouring blocks."},
		{"b3", "heading", "# Pieces"},
		{"b4", "paragraph", "A long block becomes pieces."},
	}
	markdown, blocks := documentOf(parts)
	indexVersionAndWindow(t, tx, artifact, 1, markdown, blocks, "version 1")
	markdown, blocks = documentOf(parts[:2])
	indexVersionAndWindow(t, tx, artifact, 2, markdown, blocks, "the version without the second section")
	if got := slices.Sorted(maps.Keys(windowRowsOf(t, tx, DocumentWindowKind, artifact))); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("the document's windows are %v, want the first section's alone", got)
	}
	markdown, blocks = documentOf(nil)
	indexVersionAndWindow(t, tx, artifact, 3, markdown, blocks, "the empty version")
	if got := windowRowsOf(t, tx, DocumentWindowKind, artifact); len(got) != 0 {
		t.Fatalf("the emptied document holds %d windows, want none", len(got))
	}
}

// respelledDocument is version 1 of a document as another renderer version stored it, in a
// spelling no build writes that reads as the tree this build writes otherwise: a top-level loose
// list and a quote holding one, each with a blank line between its last two items alone, where
// a build writes the same blank line between every two. respelledDocumentBlocks is each of its
// blocks in document order, with its type and its slice of the document as that renderer's build
// indexed it.
const respelledDocument = "Opening paragraph.\n\n- first\n- second\n\n- third\n\n> - alpha\n> - bravo\n>\n> - charlie\n\nClosing paragraph.\n"

var respelledDocumentBlocks = []struct{ nodeType, text string }{
	{"paragraph", "Opening paragraph."},
	{"bullet_list", "- first\n- second\n\n- third"},
	{"paragraph", "first"},
	{"paragraph", "second"},
	{"paragraph", "third"},
	{"blockquote", "> - alpha\n> - bravo\n>\n> - charlie"},
	{"bullet_list", "- alpha\n> - bravo\n>\n> - charlie"},
	{"paragraph", "alpha"},
	{"paragraph", "bravo"},
	{"paragraph", "charlie"},
	{"paragraph", "Closing paragraph."},
}

// respelledVersionBlocks is the blocks of markdown, which reads as tree, as another renderer's
// build handed them to IndexBlocks: tree's blocks in document order, each with its id and type
// from tree and its offsets from its slice of markdown in cuts.
func respelledVersionBlocks(t *testing.T, tree *pmdoc.Node, markdown string, cuts []struct{ nodeType, text string }) []model.ArtifactBlock {
	t.Helper()
	_, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatalf("render the tree of %q: %v", markdown, err)
	}
	if len(offsets) != len(cuts) {
		t.Fatalf("the tree of %q holds %d blocks (%+v), want %d", markdown, len(offsets), offsets, len(cuts))
	}
	blocks := make([]model.ArtifactBlock, len(offsets))
	from := 0
	for index, offset := range offsets {
		want := cuts[index]
		at := strings.Index(markdown[from:], want.text)
		if offset.Type != want.nodeType || at < 0 {
			t.Fatalf("block %d of the tree of %q is a %s, want a %s whose text %q follows offset %d", index, markdown, offset.Type, want.nodeType, want.text, from)
		}
		from += at
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: from, To: from + len(want.text)}
	}
	return blocks
}

// insertRenderedVersion stores version number of document artifact, written at at, holding
// markdown, as a build of renderer version stamp writes it.
func insertRenderedVersion(t *testing.T, tx pgx.Tx, artifact string, number int, markdown string, stamp int, at time.Time) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors, renderer_version, created_at)
		values ($1, $2, $3, '[{"kind":"user","id":"alice"}]', $4, $5)
	`, artifact, number, markdown, stamp, at); err != nil {
		t.Fatalf("store version %d of %s: %v", number, artifact, err)
	}
}

// versionOneAt is when version 1 of a test document is written (indexOtherRendererVersionOne),
// and version 2 is written an hour later (indexVersionTwo).
var versionOneAt = time.Date(2026, time.November, 1, 9, 0, 0, 0, time.UTC)

// indexOtherRendererVersionOne stores markdown as version 1 of document artifact, written at
// versionOneAt by a build of the renderer version before this build's, and indexes its blocks as
// that build handed them to IndexBlocks (respelledVersionBlocks, cut by cuts). It answers the tree
// markdown reads as and those blocks.
func indexOtherRendererVersionOne(t *testing.T, tx pgx.Tx, artifact, markdown string, cuts []struct{ nodeType, text string }) (*pmdoc.Node, []model.ArtifactBlock) {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("read %q: %v", markdown, err)
	}
	stored := respelledVersionBlocks(t, tree, markdown, cuts)
	insertRenderedVersion(t, tx, artifact, 1, markdown, pmdoc.RendererVersion-1, versionOneAt)
	if _, won, _, err := IndexBlocks(context.Background(), tx, artifact, 1, versionOneAt, markdown, stored, nil, nil, "", TypedFacts{}); err != nil || !won {
		t.Fatalf("index version 1 (won %v): %v", won, err)
	}
	return tree, stored
}

// indexVersionTwo stores markdown as version 2 of document artifact, as this build writes it, an
// hour after versionOneAt, and indexes blocks for it. It answers when version 2 was written and
// the targets IndexBlocks answers for it.
func indexVersionTwo(t *testing.T, tx pgx.Tx, artifact, markdown string, blocks []model.ArtifactBlock) (time.Time, []Target) {
	t.Helper()
	versionTwoAt := versionOneAt.Add(time.Hour)
	insertRenderedVersion(t, tx, artifact, 2, markdown, pmdoc.RendererVersion, versionTwoAt)
	moved, won, _, err := IndexBlocks(context.Background(), tx, artifact, 2, versionTwoAt, markdown, blocks, nil, nil, "", TypedFacts{})
	if err != nil || !won {
		t.Fatalf("index version 2 (won %v): %v", won, err)
	}
	return versionTwoAt, moved
}

// checkKeptAcrossRenderers stores document as version 1 of a document artifact named slug, as
// another renderer version stored it, its blocks cut by cuts (indexOtherRendererVersionOne), and
// its tree with the closing paragraph reworded as version 2, as this build writes it
// (indexVersionTwo). The test needs this build to write every block but a paragraph otherwise
// than version 1 recorded it, and each block markers names with its marker at its start. It
// checks that each block takesVersionTwo names takes changed version 2 and its time, with its
// text recorded at version 2, and every other block keeps changed version 1 and its time with no
// text recorded at version 2, each with the sum of its text now. markers and takesVersionTwo name
// blocks by their index in version 1's blocks.
func checkKeptAcrossRenderers(t *testing.T, slug, document string, cuts []struct{ nodeType, text string }, markers map[int]string, takesVersionTwo ...int) {
	t.Helper()
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", slug)
	tree, stored := indexOtherRendererVersionOne(t, tx, artifact, document, cuts)
	recorded := make(map[string]string, len(stored))
	for _, block := range stored {
		recorded[block.ID] = document[block.From:block.To]
	}
	markerOf := make(map[string]string, len(markers))
	for index, marker := range markers {
		markerOf[stored[index].ID] = marker
	}
	versionTwo := make(map[string]bool, len(takesVersionTwo))
	for _, index := range takesVersionTwo {
		versionTwo[stored[index].ID] = true
	}

	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "text" && node.Text == "Closing paragraph." {
			node.Text = "Closing paragraph, reworded."
		}
		return true
	})
	markdown, blocks := renderedBlocks(t, tree)
	for _, block := range blocks {
		body := markdown[block.From:block.To]
		if block.Type != "paragraph" && body == recorded[block.ID] {
			t.Fatalf("this build writes %s %s as version 1 recorded it, %q; the test needs another spelling", block.Type, block.ID, body)
		}
		if marker, named := markerOf[block.ID]; named && !strings.HasPrefix(body, marker) {
			t.Fatalf("this build writes %s %s as %q; the test needs it written with %q", block.Type, block.ID, body, marker)
		}
	}
	versionTwoAt, _ := indexVersionTwo(t, tx, artifact, markdown, blocks)

	for _, block := range blocks {
		body := markdown[block.From:block.To]
		wantVersion, wantAt, wantTexts := 1, versionOneAt, []string{}
		if versionTwo[block.ID] {
			wantVersion, wantAt, wantTexts = 2, versionTwoAt, []string{body}
		}
		changedVersion, changedAt, sum, texts := indexedChange(t, tx, artifact, block.ID, 2)
		if changedVersion != wantVersion || !changedAt.Equal(wantAt) || sum != BlockTextSum(body) {
			t.Errorf("block %s, a %s written %q and recorded at version 1 as %q, changed at version %d (%s) with the sum %s, want version %d (%s) with the sum of its text",
				block.ID, block.Type, body, recorded[block.ID], changedVersion, changedAt.UTC(), sum, wantVersion, wantAt)
		}
		if !slices.Equal(texts, wantTexts) {
			t.Errorf("version 2 records block %s as %q, want %q", block.ID, texts, wantTexts)
		}
	}
}

// A version another renderer version stored spells its tree otherwise than this build does, so
// the next version moves the sum of a block whose tree did not change. That version, which
// rewords the closing paragraph, keeps the changed version and time of each top-level block whose
// text recorded at version 1 stood at a line start and reads as its text now, both alone - the
// top-level list and the quote - and gives each the new text's sum and excerpt with no text
// recorded at version 2, so a citation of the list is not counted as moved. The list inside the
// quote, a block inside a container, whose text read alone loses the quote's prefix, takes
// version 2, as does the reworded paragraph, and a citation of it is counted.
func TestABlockKeepsItsChangedVersionAcrossAnotherRenderersSpelling(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	artifact, refKey := createReferenceArtifact(t, tx, "WIND-1", "WIND", "respelled-md")
	tree, stored := indexOtherRendererVersionOne(t, tx, artifact, respelledDocument, respelledDocumentBlocks)
	recorded := make(map[string]string, len(stored))
	for _, block := range stored {
		recorded[block.ID] = respelledDocument[block.From:block.To]
	}
	list, quote, quotedList, closing := stored[1].ID, stored[5].ID, stored[6].ID, stored[10].ID
	citing := createReferenceComment(t, tx, "WIND-1")
	insertReference(t, tx, "comment", citing, "block", refKey+"#"+list)
	insertReference(t, tx, "comment", citing, "block", refKey+"#"+quotedList)

	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "text" && node.Text == "Closing paragraph." {
			node.Text = "Closing paragraph, reworded."
		}
		return true
	})
	markdown, blocks := renderedBlocks(t, tree)
	versionTwoAt, moved := indexVersionTwo(t, tx, artifact, markdown, blocks)

	type indexedBlock struct {
		changedVersion int
		changedAt      time.Time
		sum, excerpt   string
	}
	rows, err := tx.Query(ctx, `
		select block_id, changed_version, changed_at, text_sha256, excerpt from document_blocks where artifact_id = $1
	`, artifact)
	if err != nil {
		t.Fatalf("read the blocks of version 2: %v", err)
	}
	indexed := map[string]indexedBlock{}
	for rows.Next() {
		var id string
		var block indexedBlock
		if err := rows.Scan(&id, &block.changedVersion, &block.changedAt, &block.sum, &block.excerpt); err != nil {
			t.Fatalf("scan a block of version 2: %v", err)
		}
		indexed[id] = block
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of version 2: %v", err)
	}
	// The list inside the quote and the reworded paragraph take version 2. Every other block
	// keeps version 1: the top-level list and the quote, whose texts recorded at version 1 stood
	// at line starts and read alone as their texts now, and the blocks whose texts did not move.
	for _, block := range blocks {
		body := markdown[block.From:block.To]
		if (block.ID == list || block.ID == quote) && body == recorded[block.ID] {
			t.Fatalf("this build writes block %s as version 1 recorded it, %q; the test needs a spelling no build writes", block.ID, body)
		}
		wantVersion, wantAt := 1, versionOneAt
		if block.ID == quotedList || block.ID == closing {
			wantVersion, wantAt = 2, versionTwoAt
		}
		got := indexed[block.ID]
		if got.changedVersion != wantVersion || !got.changedAt.Equal(wantAt) {
			t.Errorf("block %s, a %s written %q and recorded at version 1 as %q, changed at version %d (%s), want version %d (%s)",
				block.ID, block.Type, body, recorded[block.ID], got.changedVersion, got.changedAt.UTC(), wantVersion, wantAt)
		}
		if got.sum != BlockTextSum(body) || got.excerpt != text.HeadRunes(body, BlockExcerptRunes) {
			t.Errorf("block %s holds the sum %s and the excerpt %q, want the sum and excerpt of %q", block.ID, got.sum, got.excerpt, body)
		}
	}
	var textsAtTwo []string
	if err := tx.QueryRow(ctx, `
		select coalesce(array_agg(block_id), '{}') from document_block_texts where artifact_id = $1 and version = 2
	`, artifact).Scan(&textsAtTwo); err != nil {
		t.Fatalf("read the block texts of version 2: %v", err)
	}
	slices.Sort(textsAtTwo)
	want := []string{quotedList, closing}
	slices.Sort(want)
	if !slices.Equal(textsAtTwo, want) {
		t.Errorf("version 2 records the texts of blocks %v, want those of the list inside the quote and the reworded paragraph alone (%v)", textsAtTwo, want)
	}
	if slices.Contains(moved, Target{Kind: "block", ID: refKey + "#" + list}) {
		t.Errorf("version 2 counts the cited top-level list as moved (%v); its text holds the tree it held", moved)
	}
	if !slices.Contains(moved, Target{Kind: "block", ID: refKey + "#" + quotedList}) {
		t.Errorf("version 2 does not count the cited list inside the quote as moved (%v); it took version 2", moved)
	}
}

// otherMarkerDocument is version 1 of a document as another renderer version stored it: a bullet
// list and a second one that the renderer writes with the other marker, `*`, an ordered list and a
// second one it writes with `N)`, so each pair reads back as two lists, and a closing paragraph.
// Each list is loose in renderer version 1's spelling, two blank lines between its items, where
// this build writes one. otherMarkerDocumentBlocks is each of its blocks in document order, with
// its type and its slice of the document as that renderer's build indexed it.
const otherMarkerDocument = "- first\n\n\n- second\n\n* third\n\n\n* fourth\n\n1. fifth\n\n\n2. sixth\n\n1) seventh\n\n\n2) eighth\n\nClosing paragraph.\n"

var otherMarkerDocumentBlocks = []struct{ nodeType, text string }{
	{"bullet_list", "- first\n\n\n- second"},
	{"paragraph", "first"},
	{"paragraph", "second"},
	{"bullet_list", "* third\n\n\n* fourth"},
	{"paragraph", "third"},
	{"paragraph", "fourth"},
	{"ordered_list", "1. fifth\n\n\n2. sixth"},
	{"paragraph", "fifth"},
	{"paragraph", "sixth"},
	{"ordered_list", "1) seventh\n\n\n2) eighth"},
	{"paragraph", "seventh"},
	{"paragraph", "eighth"},
	{"paragraph", "Closing paragraph."},
}

// The renderer writes a list that follows a list of its kind with that kind's other marker (`*`
// after `-`, `N)` after `N.`), so the two read back as two lists, and the same list read alone
// renders with `-` or `N.`. Version 1, which another renderer version stored, holds two such pairs
// of top-level lists in that renderer's spelling (otherMarkerDocument); version 2 rewords the
// closing paragraph. Every list keeps changed version 1 and its time, and takes the new text's
// sum with no text recorded at version 2: the second list of each pair as well, whose recorded
// text read alone renders with `-` or `N.` while its text now keeps the other marker, since its
// text now read alone renders so too. The reworded paragraph takes version 2.
func TestAListWrittenWithTheOtherMarkerKeepsItsChangedVersion(t *testing.T) {
	// Blocks 3 and 9 are the second list of each pair, with the marker this build writes each
	// with; block 12 is the reworded paragraph.
	checkKeptAcrossRenderers(t, "other-marker-md", otherMarkerDocument, otherMarkerDocumentBlocks, map[int]string{3: "* ", 9: "1) "}, 12)
}

// typedDocument is version 1 of a document as another renderer version stored it, the text
// renderer version 1 writes for its tree exactly: a bullet list; a callout holding a bullet list
// and a second one that the renderer writes with the other marker, `*`; a callout holding a bullet
// list whose item holds a bullet list; a bullet list whose item holds a callout holding a bullet
// list; a quote holding a bullet list and a callout holding a bullet list; and a closing
// paragraph. Each list of two items is loose, and version 1 wrote the blank line between its items
// as two lines: first the list's prefix as it stands (`> ` in the quote, two spaces in each list a
// list item holds, nothing elsewhere), then that prefix with its trailing spaces trimmed, the one
// line this build writes there. So this build spells every block but a paragraph otherwise.
// typedDocumentBlocks is each of its blocks in document order, with its type and its slice of the
// document as that renderer's build indexed it.
const typedDocument = "- top\n\n\n- level\n\n" +
	":::callout{#t1 kind=\"note\" title=\"T\"}\n- one\n\n\n- two\n\n* three\n\n\n* four\n:::\n\n" +
	":::callout{#t2 kind=\"note\" title=\"T\"}\n- outer\n\n  - five\n  \n\n  - six\n:::\n\n" +
	"- item\n\n  :::callout{#t3 kind=\"note\" title=\"T\"}\n  - seven\n  \n\n  - eight\n  :::\n\n" +
	"> - alpha\n> \n>\n> - bravo\n>\n> :::callout{#t4 kind=\"note\" title=\"T\"}\n> - charlie\n> \n>\n> - delta\n> :::\n\n" +
	"Closing paragraph.\n"

var typedDocumentBlocks = []struct{ nodeType, text string }{
	{"bullet_list", "- top\n\n\n- level"},
	{"paragraph", "top"},
	{"paragraph", "level"},
	{"callout", ":::callout{#t1 kind=\"note\" title=\"T\"}\n- one\n\n\n- two\n\n* three\n\n\n* four\n:::"},
	{"bullet_list", "- one\n\n\n- two"},
	{"paragraph", "one"},
	{"paragraph", "two"},
	{"bullet_list", "* three\n\n\n* four"},
	{"paragraph", "three"},
	{"paragraph", "four"},
	{"callout", ":::callout{#t2 kind=\"note\" title=\"T\"}\n- outer\n\n  - five\n  \n\n  - six\n:::"},
	{"bullet_list", "- outer\n\n  - five\n  \n\n  - six"},
	{"paragraph", "outer"},
	{"bullet_list", "- five\n  \n\n  - six"},
	{"paragraph", "five"},
	{"paragraph", "six"},
	{"bullet_list", "- item\n\n  :::callout{#t3 kind=\"note\" title=\"T\"}\n  - seven\n  \n\n  - eight\n  :::"},
	{"paragraph", "item"},
	{"callout", ":::callout{#t3 kind=\"note\" title=\"T\"}\n  - seven\n  \n\n  - eight\n  :::"},
	{"bullet_list", "- seven\n  \n\n  - eight"},
	{"paragraph", "seven"},
	{"paragraph", "eight"},
	{"blockquote", "> - alpha\n> \n>\n> - bravo\n>\n> :::callout{#t4 kind=\"note\" title=\"T\"}\n> - charlie\n> \n>\n> - delta\n> :::"},
	{"bullet_list", "- alpha\n> \n>\n> - bravo"},
	{"paragraph", "alpha"},
	{"paragraph", "bravo"},
	{"callout", ":::callout{#t4 kind=\"note\" title=\"T\"}\n> - charlie\n> \n>\n> - delta\n> :::"},
	{"bullet_list", "- charlie\n> \n>\n> - delta"},
	{"paragraph", "charlie"},
	{"paragraph", "delta"},
	{"paragraph", "Closing paragraph."},
}

// A typed block writes the blocks it holds from a line's start with no prefix, so a block that
// only typed blocks enclose reads alone as it stands there, as a top-level block does. A list item
// or a quote writes its prefix on the lines of each block it holds, a typed block and the blocks
// in it included, and that block's text read alone loses the prefix. Version 1, which another
// renderer version stored, holds in that renderer's spelling (typedDocument) a top-level list, a
// callout holding two lists, a callout holding a list whose item holds a list, a list whose item
// holds a callout holding a list, and a quote holding a list and a callout holding a list; version
// 2 rewords the closing paragraph, and this build spells every block but a paragraph otherwise.
// Each such block that no list item or quote encloses keeps changed version 1 and its time, and
// takes the new text's sum with no text recorded at version 2, since its text recorded at version
// 1 started a line there and reads alone as its text now: the top-level list, the two top-level
// callouts and the lists that stand directly in them, the second list in the first callout too,
// which this build writes with the other marker, the list whose item holds a callout, and the
// quote. Each such block that a list item or a quote encloses takes version 2, as does the
// reworded paragraph, and each has its text recorded at version 2: the list in the item of the
// second callout's list, the callout in the list item and the list in that callout, and the list
// in the quote, the callout in the quote and the list in that callout.
func TestABlockInsideATypedBlockKeepsItsChangedVersion(t *testing.T) {
	// Block 7 is the second list in the first callout, with the marker this build writes it with;
	// blocks 13, 18, 19, 23, 26 and 27 are the lists and callouts a list item or a quote encloses,
	// and block 30 is the reworded paragraph.
	checkKeptAcrossRenderers(t, "typed-md", typedDocument, typedDocumentBlocks, map[int]string{7: "* "}, 13, 18, 19, 23, 26, 27, 30)
}

// footnotedDocument is version 1 of a document that defines the footnote 1, as another renderer
// version stored it: its opening paragraph holds text shaped like a reference to that footnote,
// which a renderer escapes because the document defines the label. footnotedDocumentBlocks is
// each of its blocks in document order, with its type and its slice of the document.
const footnotedDocument = "Opening paragraph, see \\[^1] here.\n\n[^1]: A note.\n"

var footnotedDocumentBlocks = []struct{ nodeType, text string }{
	{"paragraph", "Opening paragraph, see \\[^1] here."},
	{"footnote_definition", "[^1]: A note."},
	{"paragraph", "A note."},
}

// A block's text read alone holds none of its document's footnote definitions. The opening
// paragraph's text recorded at version 1, which another renderer version stored, is the literal
// text `\[^1]`, escaped because the document defines 1; version 2 makes that text a reference
// to the footnote, `[^1]`. Read alone, the two read as one, though the paragraph holds another
// tree, so the paragraph is compared by its text's sum (pmdoc.FragmentReadsAs): it takes version
// 2, and its text is recorded at version 2.
func TestABlockWhoseFootnoteShapedTextBecomesAReferenceTakesTheNewVersion(t *testing.T) {
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "footnoted-md")
	tree, stored := indexOtherRendererVersionOne(t, tx, artifact, footnotedDocument, footnotedDocumentBlocks)
	paragraph, recorded := stored[0].ID, footnotedDocument[stored[0].From:stored[0].To]

	tree.Children[0].Children = []*pmdoc.Node{
		{Type: "text", Text: "Opening paragraph, see "},
		{Type: "footnote_reference", Attrs: pmdoc.Attrs{"label": "1"}},
		{Type: "text", Text: " here."},
	}
	markdown, blocks := renderedBlocks(t, tree)
	body := markdown[blocks[0].From:blocks[0].To]
	// The reference and the recorded literal read alone as one
	// (TestAStoredTextReadsAsAnotherSpellingOfTheSameTree), which is the case FragmentReadsAs refuses.
	if blocks[0].ID != paragraph || body != "Opening paragraph, see [^1] here." {
		t.Fatalf("version 2's first block is %s written %q; the test needs paragraph %s written as a reference", blocks[0].ID, body, paragraph)
	}
	versionTwoAt, _ := indexVersionTwo(t, tx, artifact, markdown, blocks)

	changedVersion, changedAt, sum, textsAtTwo := indexedChange(t, tx, artifact, paragraph, 2)
	if changedVersion != 2 || !changedAt.Equal(versionTwoAt) || sum != BlockTextSum(body) {
		t.Errorf("paragraph %s, recorded at version 1 as %q and written %q, changed at version %d (%s) with the sum %s, want version 2 (%s) with the sum of its text",
			paragraph, recorded, body, changedVersion, changedAt.UTC(), sum, versionTwoAt)
	}
	if !slices.Equal(textsAtTwo, []string{body}) {
		t.Errorf("version 2 records paragraph %s as %q, want one text, %q", paragraph, textsAtTwo, body)
	}
}

// indexedChange is the changed version, time and sum the index holds for block blockID of
// document artifact, and every text document_block_texts records for it at version.
func indexedChange(t *testing.T, tx pgx.Tx, artifact, blockID string, version int) (int, time.Time, string, []string) {
	t.Helper()
	ctx := context.Background()
	var changedVersion int
	var changedAt time.Time
	var sum string
	if err := tx.QueryRow(ctx, `
		select changed_version, changed_at, text_sha256 from document_blocks where artifact_id = $1 and block_id = $2
	`, artifact, blockID).Scan(&changedVersion, &changedAt, &sum); err != nil {
		t.Fatalf("read block %s: %v", blockID, err)
	}
	var texts []string
	if err := tx.QueryRow(ctx, `
		select coalesce(array_agg(text), '{}') from document_block_texts where artifact_id = $1 and block_id = $2 and version = $3
	`, artifact, blockID, version).Scan(&texts); err != nil {
		t.Fatalf("read the texts version %d records for block %s: %v", version, blockID, err)
	}
	return changedVersion, changedAt, sum, texts
}

// renderedBlocks is tree rendered as this build renders it, with its blocks as the version
// writer hands them to IndexBlocks.
func renderedBlocks(t *testing.T, tree *pmdoc.Node) (string, []model.ArtifactBlock) {
	t.Helper()
	markdown, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatalf("render %+v: %v", tree, err)
	}
	blocks := make([]model.ArtifactBlock, len(offsets))
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To}
	}
	return markdown, blocks
}

// A block nested in a container, read alone, loses the container's prefix. Version 1,
// `- a\n  - b\n    - c\n`, which another renderer version stored, holds c under b; version 2,
// `- a\n  - b\n  - c\n`, lifts c beside b. The inner list's slices, `- b\n    - c` and
// `- b\n  - c`, read alone as one tree, c under b, though the two documents hold two trees: in
// version 2 the prefix of c's line is also the content indent of b's `- ` item. So the inner list,
// a block inside a list item, takes version 2, and its text is recorded at version 2.
func TestAListInsideAListItemTakesTheNewVersionThoughItsTextReadsAloneAsBefore(t *testing.T) {
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "nested-md")
	const versionOne, versionTwo = "- a\n  - b\n    - c\n", "- a\n  - b\n  - c\n"
	tree, stored := indexOtherRendererVersionOne(t, tx, artifact, versionOne, []struct{ nodeType, text string }{
		{"bullet_list", "- a\n  - b\n    - c"},
		{"paragraph", "a"},
		{"bullet_list", "- b\n    - c"},
		{"paragraph", "b"},
		{"bullet_list", "- c"},
		{"paragraph", "c"},
	})
	inner, recorded := stored[2].ID, versionOne[stored[2].From:stored[2].To]

	// c's item leaves the list under b and stands beside b in the inner list.
	innerList := tree.Children[0].Children[0].Children[1]
	itemB := innerList.Children[0]
	itemC := itemB.Children[1].Children[0]
	itemB.Children = itemB.Children[:1]
	innerList.Children = append(innerList.Children, itemC)
	markdown, blocks := renderedBlocks(t, tree)
	// The two slices read alone as one tree, which is why a block inside a list item is no
	// candidate (aloneCandidates).
	if markdown != versionTwo || len(blocks) < 3 || blocks[2].ID != inner || markdown[blocks[2].From:blocks[2].To] != "- b\n  - c" {
		t.Fatalf("version 2 = %q with blocks %+v; the test needs %q with list %s written %q", markdown, blocks, versionTwo, inner, "- b\n  - c")
	}
	body := markdown[blocks[2].From:blocks[2].To]
	versionTwoAt, _ := indexVersionTwo(t, tx, artifact, markdown, blocks)

	changedVersion, changedAt, sum, texts := indexedChange(t, tx, artifact, inner, 2)
	if changedVersion != 2 || !changedAt.Equal(versionTwoAt) || sum != BlockTextSum(body) {
		t.Errorf("list %s inside item a, recorded at version 1 as %q and written %q, changed at version %d (%s) with the sum %s, want version 2 (%s) with the sum of its text",
			inner, recorded, body, changedVersion, changedAt.UTC(), sum, versionTwoAt)
	}
	if !slices.Equal(texts, []string{body}) {
		t.Errorf("version 2 records list %s as %q, want one text, %q", inner, texts, body)
	}
}

// A block that stands at the top level now but stood inside a container at its changed version
// was recorded with that container's prefix on its later lines. Version 1, which another renderer
// version stored, holds the list X inside item a, its items b and c beside each other, recorded
// as `* b\n  * c`; version 2 holds X alone at the top level as `- b\n  - c`, c under b. Read
// alone, the recorded text reads as c under b too, though X held c beside b, so X, whose
// recorded text did not stand at a line start of version 1, takes version 2, and its text is
// recorded at version 2.
func TestABlockThatStoodInsideAListItemTakesTheNewVersionAtTheTopLevel(t *testing.T) {
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "lifted-md")
	const versionOne, versionTwo = "- a\n  * b\n  * c\n", "- b\n  - c\n"
	tree, stored := indexOtherRendererVersionOne(t, tx, artifact, versionOne, []struct{ nodeType, text string }{
		{"bullet_list", "- a\n  * b\n  * c"},
		{"paragraph", "a"},
		{"bullet_list", "* b\n  * c"},
		{"paragraph", "b"},
		{"paragraph", "c"},
	})
	list, recorded := stored[2].ID, versionOne[stored[2].From:stored[2].To]

	// X keeps its id and stands alone at the top level, c's item in a new list under b.
	x := tree.Children[0].Children[0].Children[1]
	itemB, itemC := x.Children[0], x.Children[1]
	itemB.Children = append(itemB.Children, &pmdoc.Node{Type: "bullet_list", Attrs: pmdoc.Attrs{}, Children: []*pmdoc.Node{itemC}})
	x.Children = []*pmdoc.Node{itemB}
	tree.Children = []*pmdoc.Node{x}
	pmdoc.EnsureBlockIDs(tree)
	markdown, blocks := renderedBlocks(t, tree)
	// The recorded text reads alone as X's text here, c under b, which is the case the
	// line-start rule refuses.
	if markdown != versionTwo || len(blocks) == 0 || blocks[0].ID != list || markdown[blocks[0].From:blocks[0].To] != "- b\n  - c" {
		t.Fatalf("version 2 = %q with blocks %+v; the test needs %q with list %s written %q", markdown, blocks, versionTwo, list, "- b\n  - c")
	}
	body := markdown[blocks[0].From:blocks[0].To]
	versionTwoAt, _ := indexVersionTwo(t, tx, artifact, markdown, blocks)

	changedVersion, changedAt, sum, texts := indexedChange(t, tx, artifact, list, 2)
	if changedVersion != 2 || !changedAt.Equal(versionTwoAt) || sum != BlockTextSum(body) {
		t.Errorf("list %s, recorded inside item a at version 1 as %q and written %q at the top level, changed at version %d (%s) with the sum %s, want version 2 (%s) with the sum of its text",
			list, recorded, body, changedVersion, changedAt.UTC(), sum, versionTwoAt)
	}
	if !slices.Equal(texts, []string{body}) {
		t.Errorf("version 2 records list %s as %q, want one text, %q", list, texts, body)
	}
}

// A mention starts at the tightest block whose [From, To) holds its start: a child inside its
// parent, the parent between its children, and no block at all between top-level blocks or past
// the last one. A block ending where a mention starts does not hold it.
func TestInnermostBlocksPicksTheTightestRangeHoldingEachStart(t *testing.T) {
	blocks := []model.ArtifactBlock{{ID: "a", From: 0, To: 100}, {ID: "b", From: 10, To: 20}, {ID: "c", From: 30, To: 40}, {ID: "d", From: 120, To: 130}}
	got := innermostBlocks(blocks, []int{5, 15, 20, 25, 35, 50, 110, 125, 200})
	want := []int{0, 1, 0, 0, 2, 0, -1, 3, -1}
	if !slices.Equal(got, want) {
		t.Fatalf("innermostBlocks = %v, want %v", got, want)
	}
}

// A candidate is kept when no block encloses it or only typed blocks do: a block encloses each
// block that starts after it and at or before its end, so an empty block at the end of its
// container is inside it. A block a quote, a list or a footnote definition encloses is dropped,
// however deep, a typed block inside one and the blocks that typed block holds included. Each case
// makes every block a candidate, and then its last block alone.
func TestAloneCandidatesKeepsTheBlocksOnlyTypedBlocksEnclose(t *testing.T) {
	block := func(id, nodeType string, from, to int) model.ArtifactBlock {
		return model.ArtifactBlock{ID: id, Type: nodeType, From: from, To: to}
	}
	for _, test := range []struct {
		name   string
		blocks []model.ArtifactBlock
		kept   []string
	}{
		{"a top-level block", []model.ArtifactBlock{block("p", "paragraph", 0, 10)}, []string{"p"}},
		{"a block inside a callout",
			[]model.ArtifactBlock{block("c", "callout", 0, 40), block("p", "paragraph", 30, 36)}, []string{"c", "p"}},
		{"a block inside a callout inside a callout",
			[]model.ArtifactBlock{block("c1", "callout", 0, 90), block("c2", "callout", 30, 80), block("p", "paragraph", 60, 66)},
			[]string{"c1", "c2", "p"}},
		{"a block inside a callout inside a blockquote",
			[]model.ArtifactBlock{block("q", "blockquote", 0, 70), block("c", "callout", 2, 70), block("p", "paragraph", 34, 40)},
			[]string{"q"}},
		{"a block inside a bullet_list inside a callout",
			[]model.ArtifactBlock{block("c", "callout", 0, 60), block("l", "bullet_list", 30, 50), block("p", "paragraph", 32, 50)},
			[]string{"c", "l"}},
		{"an empty block whose From is its callout's To",
			[]model.ArtifactBlock{block("c", "callout", 0, 40), block("p", "paragraph", 40, 40)}, []string{"c", "p"}},
		{"an empty block whose From is its bullet_list's To",
			[]model.ArtifactBlock{block("l", "bullet_list", 0, 10), block("p", "paragraph", 10, 10)}, []string{"l"}},
		{"a top-level block after a callout's end",
			[]model.ArtifactBlock{block("c", "callout", 0, 40), block("p1", "paragraph", 30, 36), block("p2", "paragraph", 42, 50)},
			[]string{"c", "p1", "p2"}},
		{"a top-level block after a blockquote's end",
			[]model.ArtifactBlock{block("q", "blockquote", 0, 20), block("p1", "paragraph", 2, 20), block("p2", "paragraph", 22, 30)},
			[]string{"q", "p2"}},
		{"a block inside a footnote_definition",
			[]model.ArtifactBlock{block("f", "footnote_definition", 0, 20), block("p", "paragraph", 6, 20)}, []string{"f"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			every := make([]respellCandidate, len(test.blocks))
			for index, block := range test.blocks {
				every[index] = respellCandidate{block: block}
			}
			var kept []string
			for _, candidate := range aloneCandidates(test.blocks, every) {
				kept = append(kept, candidate.block.ID)
			}
			if !slices.Equal(kept, test.kept) {
				t.Errorf("aloneCandidates of every block of %+v keeps %v, want %v", test.blocks, kept, test.kept)
			}
			last := test.blocks[len(test.blocks)-1]
			alone := aloneCandidates(test.blocks, []respellCandidate{{block: last}})
			if want := slices.Contains(test.kept, last.ID); (len(alone) == 1) != want {
				t.Errorf("aloneCandidates of block %s alone of %+v keeps %d candidates, want it kept: %t", last.ID, test.blocks, len(alone), want)
			}
		})
	}
}

// A rendered version can repeat an id, and document_blocks holds one row per id: the first holder
// is the node. An id Postgres cannot store in a text[] (empty, holding a NUL, or not UTF-8) is
// no node, nor is one longer than text.MaxBlockIDBytes, which no key of the index can hold; those
// are counted, with the longest one's bytes. An id at the bound is a node.
func TestUniqueBlocksKeepsTheFirstHolderOfEachID(t *testing.T) {
	atBound := strings.Repeat("é", text.MaxBlockIDBytes/2)
	got, long, longest := uniqueBlocks([]model.ArtifactBlock{
		{ID: "a", From: 0, To: 10},
		{ID: "a", From: 20, To: 30},
		{ID: "", From: 40, To: 50},
		{ID: "x\x00", From: 60, To: 70},
		{ID: "\xff", From: 70, To: 75},
		{ID: atBound + "x", From: 75, To: 78},
		{ID: atBound, From: 78, To: 79},
		{ID: strings.Repeat("f", 3200), From: 79, To: 80},
		{ID: "b", From: 80, To: 90},
	})
	type node struct {
		ID       string
		From, To int
	}
	nodes := make([]node, len(got))
	for i, block := range got {
		nodes[i] = node{block.ID, block.From, block.To}
	}
	want := []node{{"a", 0, 10}, {atBound, 78, 79}, {"b", 80, 90}}
	if !slices.Equal(nodes, want) || long != 2 || longest != 3200 {
		t.Fatalf("uniqueBlocks = %#v, %d long, longest %d; want %#v, 2 long, longest 3200", nodes, long, longest, want)
	}
}
