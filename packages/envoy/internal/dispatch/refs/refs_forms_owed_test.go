package refs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// formsOwed is every row of the forms queue as "kind id", in kind then id order.
func formsOwed(t *testing.T, q Queryer) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select kind || ' ' || id::text from refs_forms_owed order by kind, id
	`)
	if err != nil {
		t.Fatalf("read refs_forms_owed: %v", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan refs_forms_owed: %v", err)
	}
	return owed
}

// settleFormsQueue empties the forms queue, as this build's index of every text it holds would.
func settleFormsQueue(t *testing.T, q Execer) {
	t.Helper()
	if _, err := q.Exec(context.Background(), `delete from refs_forms_owed`); err != nil {
		t.Fatalf("empty refs_forms_owed: %v", err)
	}
}

// edgesFrom is every refs row of one source as "kind id", sorted.
func edgesFrom(t *testing.T, q Queryer, fromKind, fromID string) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select to_kind || ' ' || to_id from refs where from_kind = $1 and from_id = $2 order by 1
	`, fromKind, fromID)
	if err != nil {
		t.Fatalf("read the edges of %s %s: %v", fromKind, fromID, err)
	}
	edges, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the edges of %s %s: %v", fromKind, fromID, err)
	}
	return edges
}

// blockEdgesOf is every row of a block table whose owner column holds owner, as "block_id kind
// id", sorted.
func blockEdgesOf(t *testing.T, q Queryer, table, column, owner string) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select block_id || ' ' || to_kind || ' ' || to_id from `+table+` where `+column+` = $1::uuid order by 1
	`, owner)
	if err != nil {
		t.Fatalf("read the %s rows of %s: %v", table, owner, err)
	}
	edges, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the %s rows of %s: %v", table, owner, err)
	}
	return edges
}

// insertSecondIssue creates issue CORE-2 of project CORE, which the fixture's texts cite.
func insertSecondIssue(t *testing.T, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-2', 'CORE', 2, 'Two', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create issue CORE-2: %v", err)
	}
}

// indexDocumentVersion writes version number of document as this build's version write indexes
// it: its blocks from the one rendering (documentOf), then its references and the excerpt of each
// (replaceDocumentVersion), settling the forms queue after both.
func indexDocumentVersion(t *testing.T, tx pgx.Tx, document string, number int, parts []part) {
	t.Helper()
	ctx := context.Background()
	markdown, blocks := documentOf(parts)
	versionAt := time.Date(2026, 1, 2, 3, 4, number, 0, time.UTC)
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, created_at) values ($1, $2, $3, $4)
	`, document, number, markdown, versionAt); err != nil {
		t.Fatalf("write version %d of %s: %v", number, document, err)
	}
	located := NewReader(ctx, tx, "artifact", document, refsGrammarServer).Mentions(markdown)
	if _, err := IndexBlocks(ctx, tx, document, number, versionAt, markdown, blocks, located, nil, refsGrammarServer); err != nil {
		t.Fatalf("index the blocks of version %d of %s: %v", number, document, err)
	}
	replaceDocumentVersion(t, tx, document, number, versionAt, located, MentionExcerpts(markdown, blocks, located, refsGrammarServer), true)
}

// replaceDocumentVersion reconciles a document version's references as docs' version write does
// (replaceVersionRefs): the replacement, then the excerpts it cut, or with none the mark that
// they are owed. blocksIndexed says the version's blocks were indexed first, which is what lets
// the replacement settle the forms queue (Replacement.SettlingForms).
func replaceDocumentVersion(t *testing.T, tx pgx.Tx, document string, number int, versionAt time.Time, located []text.Located, excerpts map[Target]model.GraphExcerpt, blocksIndexed bool) {
	t.Helper()
	ctx := context.Background()
	replacement, err := PlanReplace(ctx, tx, "artifact", document, text.Refs(located), &versionAt)
	if err != nil {
		t.Fatalf("plan the references of version %d of %s: %v", number, document, err)
	}
	if blocksIndexed {
		replacement = replacement.SettlingForms()
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		if excerpts == nil {
			ClearExcerpts(b, document)
		} else {
			QueueRecordExcerpts(b, document, number, excerpts)
		}
		return nil
	}); err != nil {
		t.Fatalf("index version %d of %s: %v", number, document, err)
	}
}

// writeAskText indexes ask's whole text as this build's writes do, through reader. questionFirst
// is a write of the ask's question - its creation, an edit - which reconciles the question's
// references before the parts and settles the forms queue in the statement that ends the parts'
// index (ItemIndexing.SettlingForms); otherwise it is a write of the parts alone - an answer -
// which reconciles the question's references after them, dated at, and settles there
// (Replacement.SettlingForms).
func writeAskText(t *testing.T, tx pgx.Tx, reader *Reader, ask model.Ask, at time.Time, questionFirst bool) {
	t.Helper()
	ctx := context.Background()
	index, err := ReadItemIndex(ctx, tx, ItemAsk, ask.ID)
	if err != nil {
		t.Fatalf("read the part index of ask %s: %v", ask.ID, err)
	}
	parts, err := PlanAskParts(index, reader, ask, at)
	if err != nil {
		t.Fatalf("plan the parts of ask %s: %v", ask.ID, err)
	}
	dated := &at
	if questionFirst {
		dated = nil
	}
	question, err := PlanReplace(ctx, tx, "ask", ask.ID, text.Refs(reader.Mentions(ask.Question)), dated)
	if err != nil {
		t.Fatalf("plan the question of ask %s: %v", ask.ID, err)
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		if questionFirst {
			question.Queue(b)
			parts.SettlingForms().Queue(b)
			return nil
		}
		parts.Queue(b)
		question.SettlingForms().Queue(b)
		return nil
	}); err != nil {
		t.Fatalf("index ask %s: %v", ask.ID, err)
	}
}

// A write this build makes owes the forms queue nothing, the deletes of the edges its new text no
// longer cites included: the row's own trigger enqueues the source, the write's delete from its
// blocks' table and its delete from refs each owe it again (migrations 20261117025906-20261117025910),
// and the write's settle, after all of them, deletes the row. A bare delete of the same edges -
// which is what an older build's re-index of the text is at the database, its scope drain's and
// its block index's included - leaves the source owed.
func TestAWriteLeavesNothingOwedInTheFormsQueue(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	comment := createReferenceComment(t, tx, "CORE-1")
	if owed := formsOwed(t, tx); !slices.Equal(owed, []string{"comment " + comment}) {
		t.Fatalf("the comment's own write owes %v, want the comment alone", owed)
	}

	write := func(body string) {
		t.Helper()
		indexItemAsAWriteDoes(t, tx, ItemComment, comment, body, refsGrammarServer, time.Now())
	}
	write("The plan: dispatch://CORE-1/plan.")
	if edges := edgesFrom(t, tx, "comment", comment); !slices.Equal(edges, []string{"issue CORE-1"}) {
		t.Fatalf("the comment's edges are %v, want the tab address's issue", edges)
	}
	if edges := blockEdgesOf(t, tx, "comment_block_refs", "comment_id", comment); len(edges) != 1 {
		t.Fatalf("the comment's blocks hold %v, want the tab address's edge", edges)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the queue holds %v after the write indexed its own text, want it empty", owed)
	}
	write("Nothing cited now.")
	if edges := edgesFrom(t, tx, "comment", comment); len(edges) != 0 {
		t.Fatalf("the comment's edges are %v after an edit that cites nothing, want none", edges)
	}
	if edges := blockEdgesOf(t, tx, "comment_block_refs", "comment_id", comment); len(edges) != 0 {
		t.Fatalf("the comment's blocks hold %v after an edit that cites nothing, want none", edges)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the queue holds %v after a write that deleted an edge and a block's edge, want it empty", owed)
	}

	// A document version is the same: its write indexes its blocks, then its references, deletes
	// the edges its new text drops from both, and settles after both.
	document, _ := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	for number, body := range []string{"See https://dispatch.example/issues/CORE-1/plan.", "Nothing cited now."} {
		indexDocumentVersion(t, tx, document, number+1, []part{{"b1", "paragraph", body}})
		if owed := formsOwed(t, tx); len(owed) != 0 {
			t.Fatalf("the queue holds %v after version %d indexed its own text, want it empty", owed, number+1)
		}
	}
	if edges := blockEdgesOf(t, tx, "block_refs", "artifact_id", document); len(edges) != 0 {
		t.Fatalf("the document's blocks hold %v after a version that cites nothing, want none", edges)
	}

	// What an older build's re-index deletes is owed until a build that reads these forms indexes
	// the text again.
	insertReference(t, tx, "comment", comment, "issue", "CORE-1")
	insertReference(t, tx, "artifact", document, "issue", "CORE-1")
	if _, err := tx.Exec(ctx, `delete from refs where from_id = any($1)`, []string{comment, document}); err != nil {
		t.Fatalf("delete the edges as an older build's re-index does: %v", err)
	}
	if owed, want := formsOwed(t, tx), []string{"artifact " + document, "comment " + comment}; !slices.Equal(owed, want) {
		t.Fatalf("after a bare delete the queue holds %v, want %v", owed, want)
	}
}

// A document version with no rendering to cut its blocks and excerpts from indexes its references
// alone and leaves its blocks to the block index (docs' indexUnrenderedVersion), so its write
// settles nothing of the forms queue: the document stays owed, by its version's own trigger, until
// a write of this build that indexes its blocks too.
func TestAVersionThatLeavesItsBlocksToTheBlockIndexStaysOwed(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	document, _ := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	const markdown = "See dispatch://CORE-1/plan."
	versionAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, created_at) values ($1, 1, $2, $3)
	`, document, markdown, versionAt); err != nil {
		t.Fatalf("write version 1: %v", err)
	}
	located := NewReader(ctx, tx, "artifact", document, refsGrammarServer).Mentions(markdown)

	replaceDocumentVersion(t, tx, document, 1, versionAt, located, nil, false)

	if edges := edgesFrom(t, tx, "artifact", document); !slices.Equal(edges, []string{"issue CORE-1"}) {
		t.Fatalf("the document's edges are %v, want the tab address's issue", edges)
	}
	if owed := formsOwed(t, tx); !slices.Equal(owed, []string{"artifact " + document}) {
		t.Fatalf("the queue holds %v after a version that left its blocks to the block index, want the document owed", owed)
	}
}

// An older build answers an ask with the one row write its transition makes (`update asks set
// state = 'answered', answer = $2`, api's closeAskTx) and then re-indexes the ask's parts with
// the grammar it knows: the answer's part, which holds the option it selected,
// `dispatch://CORE-2/plan`, gets no edge, and the option's part loses the edge this build wrote.
// The row write alone owes the ask (migration 20261117025902), before the part re-index deletes
// anything, and the drain then indexes the question and both parts again. An edit of the options
// alone owes the ask the same way.
func TestAnOlderBuildsAnswerLeavesTheAskOwed(t *testing.T) {
	tx := askPartFixture(t)
	ctx := context.Background()
	insertSecondIssue(t, tx)
	ask := insertAsk(t, tx, "CORE-1", "Which tab holds it?",
		model.AskOption{Label: "dispatch://CORE-2/plan"}, model.AskOption{Label: "Neither"})
	writeAskText(t, tx, NewReader(ctx, tx, "ask", ask.ID, askPartServer), ask, time.Now(), true)
	cite := Target{Kind: "issue", ID: "CORE-2"}
	if got := askPartTargets(t, tx, ask.ID); !slices.Equal(got, []Target{cite}) {
		t.Fatalf("the ask's parts link %v, want the option's tab address", got)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the queue holds %v after this build wrote the ask, want it empty", owed)
	}

	writeAskAnswer(t, tx, &ask, "bob", []string{"dispatch://CORE-2/plan"}, "")
	if owed := formsOwed(t, tx); !slices.Equal(owed, []string{"ask " + ask.ID}) {
		t.Fatalf("after an older build's answer wrote the ask's row the queue holds %v, want the ask owed", owed)
	}
	// Its part re-index reads no tab address: the option's part loses its edge.
	if _, err := tx.Exec(ctx, `delete from ask_block_refs where ask_id = $1::uuid`, ask.ID); err != nil {
		t.Fatalf("re-index the parts as an older build does: %v", err)
	}

	listed, err := ListOwed(ctx, tx, RefsFormsOwed, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != ask.ID {
		t.Fatalf("list what the forms queue owes = %#v (%v), want the ask", listed, err)
	}
	if indexed, err := ReindexOwed(ctx, tx, listed[0], askPartServer); err != nil || !indexed {
		t.Fatalf("drain the ask = %v (%v), want it indexed", indexed, err)
	}
	if got := askPartTargets(t, tx, ask.ID); !slices.Equal(got, []Target{cite, cite}) {
		t.Fatalf("after the drain the ask's parts link %v, want the option's and the answer's tab address", got)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the queue holds %v after the drain, want it empty", owed)
	}

	writeAskOptions(t, tx, &ask, append(ask.Options, model.AskOption{Label: "dispatch://CORE-2/verification"})...)
	if owed := formsOwed(t, tx); !slices.Equal(owed, []string{"ask " + ask.ID}) {
		t.Fatalf("after an older build's edit of the options alone the queue holds %v, want the ask owed", owed)
	}
}

// A bare delete from each block table - an older build's re-index of an owner's blocks, which may
// delete no refs row, since the owner's text can still name the target elsewhere - owes the
// owner (migrations 20261117025907-20261117025910): a document for block_refs, a comment, an
// issue message and an ask for theirs. A delete that removes nothing owes nothing, and neither
// does one that cascades from the owner's own delete: a gone owner owes no text.
func TestABareDeleteFromEachBlockTableLeavesItsOwnerOwed(t *testing.T) {
	tx := askPartFixture(t)
	ctx := context.Background()
	document, _ := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	indexDocumentVersion(t, tx, document, 1, []part{{"b1", "paragraph", "See dispatch://CORE-1/plan."}})
	comment := insertIndexedItem(t, tx, ItemComment, "CORE-1", "See dispatch://CORE-1/plan.")
	message := insertIndexedItem(t, tx, ItemMessage, "CORE-1", "See dispatch://CORE-1/plan.")
	ask := insertAsk(t, tx, "CORE-1", "Is dispatch://CORE-1/plan right?", model.AskOption{Label: "Yes"})
	writeAskText(t, tx, NewReader(ctx, tx, "ask", ask.ID, askPartServer), ask, time.Now(), true)
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the queue holds %v after this build indexed every owner, want it empty", owed)
	}

	for _, owner := range []struct {
		table, column, kind, id string
	}{
		{"block_refs", "artifact_id", "artifact", document},
		{"comment_block_refs", "comment_id", "comment", comment},
		{"message_block_refs", "message_id", "message", message},
		{"ask_block_refs", "ask_id", "ask", ask.ID},
	} {
		if edges := blockEdgesOf(t, tx, owner.table, owner.column, owner.id); len(edges) != 1 {
			t.Fatalf("%s holds %v for the %s, want its one edge", owner.table, edges, owner.kind)
		}
		if _, err := tx.Exec(ctx, `delete from `+owner.table+` where `+owner.column+` = $1::uuid and to_id = 'CORE-9'`, owner.id); err != nil {
			t.Fatalf("delete nothing from %s: %v", owner.table, err)
		}
		if owed := formsOwed(t, tx); len(owed) != 0 {
			t.Fatalf("a delete from %s that removed nothing owes %v, want nothing", owner.table, owed)
		}
		if _, err := tx.Exec(ctx, `delete from `+owner.table+` where `+owner.column+` = $1::uuid`, owner.id); err != nil {
			t.Fatalf("delete the %s's edges from %s as an older build's re-index does: %v", owner.kind, owner.table, err)
		}
		if owed := formsOwed(t, tx); !slices.Equal(owed, []string{owner.kind + " " + owner.id}) {
			t.Fatalf("after a bare delete from %s the queue holds %v, want the %s owed", owner.table, owed, owner.kind)
		}
		settleFormsQueue(t, tx)
	}

	reply := insertIndexedItem(t, tx, ItemComment, "CORE-1", "Again: dispatch://CORE-1/plan.")
	settleFormsQueue(t, tx)
	if _, err := tx.Exec(ctx, `delete from comments where id = $1::uuid`, reply); err != nil {
		t.Fatalf("delete the comment: %v", err)
	}
	if edges := blockEdgesOf(t, tx, "comment_block_refs", "comment_id", reply); len(edges) != 0 {
		t.Fatalf("the deleted comment's block edges %v outlived it", edges)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the delete its owner's delete cascaded to owes %v, want nothing", owed)
	}
}

// Every write of an ask this build makes settles its forms row after its parts' delete as well
// as its question's: an edit whose parts drop an edge (ask_block_refs' delete owes the ask,
// migration 20261117025910) leaves nothing owed. An answer holds the parts alone: its row write
// owes the ask (20261117025902), and its index of the parts reconciles the question's references
// too and settles after both, so a stale edge of the question is dropped and nothing is owed. The
// block index of an ask left owed (IndexOwedItemBlocks) is the same, its question's edge written
// back beside its parts.
func TestThisBuildsAskWritesSettleAfterTheirParts(t *testing.T) {
	tx := askPartFixture(t)
	ctx := context.Background()
	insertSecondIssue(t, tx)
	ask := insertAsk(t, tx, "CORE-1", "Is dispatch://CORE-2/plan right?",
		model.AskOption{Label: "dispatch://CORE-2/plan"}, model.AskOption{Label: "No"})
	writeAskText(t, tx, NewReader(ctx, tx, "ask", ask.ID, askPartServer), ask, time.Now(), true)
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the queue holds %v after the ask's first write, want it empty", owed)
	}

	// An edit that drops the option citing CORE-2: the question keeps its edge, the option's part
	// loses its own.
	writeAskOptions(t, tx, &ask, model.AskOption{Label: "No"})
	writeAskText(t, tx, NewReader(ctx, tx, "ask", ask.ID, askPartServer), ask, time.Now(), true)
	cite := Target{Kind: "issue", ID: "CORE-2"}
	if got := askPartTargets(t, tx, ask.ID); !slices.Equal(got, []Target{cite}) {
		t.Fatalf("after the edit the ask's parts link %v, want the question's alone", got)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the queue holds %v after an edit whose parts dropped an edge, want it empty", owed)
	}

	// An answer: its row write owes the ask, and its parts index, which no question write came
	// before, reconciles the question and drops the stale edge an older build's index left.
	insertReference(t, tx, "ask", ask.ID, "issue", "CORE-9")
	writeAskAnswer(t, tx, &ask, "bob", []string{"No"}, "See dispatch://CORE-2/verification.")
	if owed := formsOwed(t, tx); !slices.Equal(owed, []string{"ask " + ask.ID}) {
		t.Fatalf("the answer's row write owes %v, want the ask", owed)
	}
	writeAskText(t, tx, NewReader(ctx, tx, "ask", ask.ID, askPartServer), ask, ask.Answer.At, false)
	if edges := edgesFrom(t, tx, "ask", ask.ID); !slices.Equal(edges, []string{"issue CORE-2"}) {
		t.Fatalf("after the answer the ask's edges are %v, want its question's alone", edges)
	}
	if got := askPartTargets(t, tx, ask.ID); !slices.Equal(got, []Target{cite, cite}) {
		t.Fatalf("after the answer the ask's parts link %v, want the question's and the answer's", got)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the queue holds %v after this build's answer, want it empty", owed)
	}

	// An older build's re-index left the ask owed, its question's edge and its parts' gone, and
	// its blocks owed a block index; this build's block index writes all of them back and settles.
	for _, statement := range []string{
		`delete from refs where from_kind = 'ask' and from_id = $1`,
		`delete from ask_block_refs where ask_id = $1::uuid`,
		`update ask_block_state set owed_at = now() where ask_id = $1::uuid`,
	} {
		if _, err := tx.Exec(ctx, statement, ask.ID); err != nil {
			t.Fatalf("leave the ask as an older build's re-index does: %v", err)
		}
	}
	if indexed, err := IndexOwedItemBlocks(ctx, tx, OwedItemBlock{Kind: ItemAsk, ID: ask.ID}, askPartServer); err != nil || !indexed {
		t.Fatalf("block-index the owed ask = %v (%v), want it indexed", indexed, err)
	}
	if edges := edgesFrom(t, tx, "ask", ask.ID); !slices.Equal(edges, []string{"issue CORE-2"}) {
		t.Fatalf("after the block index the ask's edges are %v, want its question's", edges)
	}
	if got := askPartTargets(t, tx, ask.ID); !slices.Equal(got, []Target{cite, cite}) {
		t.Fatalf("after the block index the ask's parts link %v, want the question's and the answer's", got)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the queue holds %v after this build's block index of the ask, want it empty", owed)
	}
}

// The forms queue is drained by the one list, settle and re-index pair every queue is
// (refs.ListOwed, refs.ReindexOwed): a comment an older build wrote is re-indexed with this
// build's grammar, so the tab address it writes is an edge, and both queues' rows are settled in
// that transaction. A row a write moves between the listing and the re-index is left owed, with
// nothing written for it.
func TestTheFormsQueueDrainReindexesWithThisBuildsGrammar(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedRefsGrammarIssue(t, database)
	raw := func(body string) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', $1)
			returning id::text
		`, body).Scan(&id); err != nil {
			t.Fatalf("write the comment %q: %v", body, err)
		}
		return id
	}
	comment := raw("The plan: dispatch://CORE-1/plan.")
	listed, err := ListOwed(ctx, database.Pool, RefsFormsOwed, 10)
	if err != nil {
		t.Fatalf("list what the forms queue owes: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != comment || listed[0].Queue != RefsFormsOwed {
		t.Fatalf("owed = %#v, want the comment alone, of the forms queue", listed)
	}

	if indexed, err := ReindexOwed(ctx, database.Pool, listed[0], refsGrammarServer); err != nil || !indexed {
		t.Fatalf("re-index the comment = %v (%v), want it indexed", indexed, err)
	}

	if edges := edgesFrom(t, database.Pool, "comment", comment); !slices.Equal(edges, []string{"issue CORE-1"}) {
		t.Fatalf("the comment's edges are %v, want the tab address's issue", edges)
	}
	if owed := formsOwed(t, database.Pool); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after the re-index, want it empty", owed)
	}
	if owed := owedRows(t, database.Pool); len(owed) != 0 {
		t.Fatalf("the whole grammar's queue holds %v after the re-index, want it settled with the forms queue", owed)
	}

	moved := raw("The verification tab: dispatch://CORE-1/verification.")
	listed, err = ListOwed(ctx, database.Pool, RefsFormsOwed, 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list what the forms queue owes = %#v (%v), want the second comment", listed, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		update refs_forms_owed set enqueued_at = enqueued_at + interval '1 second' where kind = 'comment' and id = $1
	`, moved); err != nil {
		t.Fatalf("move the comment's enqueued_at: %v", err)
	}
	if indexed, err := ReindexOwed(ctx, database.Pool, listed[0], refsGrammarServer); err != nil || indexed {
		t.Fatalf("re-index a comment a write moved = %v (%v), want it left owed", indexed, err)
	}
	if owed := formsOwed(t, database.Pool); !slices.Equal(owed, []string{"comment " + moved}) {
		t.Fatalf("the forms queue holds %v, want the moved comment still owed", owed)
	}
	if edges := edgesFrom(t, database.Pool, "comment", moved); len(edges) != 0 {
		t.Fatalf("the drain wrote %v from text it left owed, want nothing", edges)
	}
}

// ListOwedAfter walks the queue in its order from the row after a cursor, so a drain that leaves
// a row owed lists the rows past it on its next pass rather than that row again.
func TestTheFormsQueueListsPastACursor(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedRefsGrammarIssue(t, database)
	var comments []string
	for index := range 3 {
		var id string
		if err := database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Text.')
			returning id::text
		`).Scan(&id); err != nil {
			t.Fatalf("write comment %d: %v", index, err)
		}
		comments = append(comments, id)
	}
	whole, err := ListOwed(ctx, database.Pool, RefsFormsOwed, 10)
	if err != nil || len(whole) != 3 {
		t.Fatalf("list the whole queue = %#v (%v), want the three comments", whole, err)
	}
	for index := 1; index < len(whole); index++ {
		if !whole[index-1].Before(whole[index]) {
			t.Fatalf("row %d (%#v) does not sort before row %d (%#v) as the queue lists them", index-1, whole[index-1], index, whole[index])
		}
	}

	past, err := ListOwedAfter(ctx, database.Pool, RefsFormsOwed, whole[0], 10)
	if err != nil {
		t.Fatalf("list past the first row: %v", err)
	}
	if !slices.Equal(past, whole[1:]) {
		t.Fatalf("listed past the first row %#v, want %#v", past, whole[1:])
	}
	if last, err := ListOwedAfter(ctx, database.Pool, RefsFormsOwed, whole[2], 10); err != nil || len(last) != 0 {
		t.Fatalf("listed past the last row %#v (%v), want nothing", last, err)
	}
}

// A source that is gone has its edges deleted by the drain and its row settled after that delete,
// which owes the forms queue the source again (migration 20261117025906): it is left owing
// nothing.
func TestTheFormsQueueDrainOfAGoneSourceLeavesNothingOwed(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedRefsGrammarIssue(t, database)
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'See CORE-1.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id) values ('comment', $1, 'issue', 'CORE-1');
	`, comment); err != nil {
		t.Fatalf("write the comment's edge: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `delete from comments where id = $1`, comment); err != nil {
		t.Fatalf("delete the comment: %v", err)
	}
	listed, err := ListOwed(ctx, database.Pool, RefsFormsOwed, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != comment {
		t.Fatalf("list what the forms queue owes = %#v (%v), want the gone comment", listed, err)
	}

	if indexed, err := ReindexOwed(ctx, database.Pool, listed[0], refsGrammarServer); err != nil || !indexed {
		t.Fatalf("drain the gone comment = %v (%v), want it drained", indexed, err)
	}

	if edges := edgesFrom(t, database.Pool, "comment", comment); len(edges) != 0 {
		t.Fatalf("the gone comment keeps edges %v, want none", edges)
	}
	if owed := formsOwed(t, database.Pool); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after the drain deleted a gone source's edges, want it empty", owed)
	}
}

// A document's removal deletes its edges and then what it owes every queue, so the delete's own
// debt to the forms queue goes with it; the same delete, bare, leaves the document owed.
func TestForgetDocumentLeavesNothingOwedInTheFormsQueue(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	document, _ := createReferenceArtifact(t, tx, "CORE-1", "CORE", "notes")
	if _, err := tx.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown) values ($1, 1, 'See CORE-1.')`, document); err != nil {
		t.Fatalf("write version 1: %v", err)
	}
	insertReference(t, tx, "artifact", document, "issue", "CORE-1")

	if err := ForgetDocument(ctx, tx, document); err != nil {
		t.Fatalf("forget the document: %v", err)
	}

	if edges := edgesFrom(t, tx, "artifact", document); len(edges) != 0 {
		t.Fatalf("the removed document keeps edges %v, want none", edges)
	}
	if owed := formsOwed(t, tx); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after the removal, want it empty", owed)
	}
	if owed := owedRows(t, tx); len(owed) != 0 {
		t.Fatalf("the whole grammar's queue holds %v after the removal, want it empty", owed)
	}
	insertReference(t, tx, "artifact", document, "issue", "CORE-1")
	if _, err := tx.Exec(ctx, `delete from refs where from_kind = 'artifact' and from_id = $1`, document); err != nil {
		t.Fatalf("delete the document's edges bare: %v", err)
	}
	if owed := formsOwed(t, tx); !slices.Equal(owed, []string{"artifact " + document}) {
		t.Fatalf("after a bare delete the forms queue holds %v, want the document owed", owed)
	}
}

// rebuild-refs re-indexes each source's references alone and leaves its blocks to the block index
// it marks owed, which a process of an older build may take, so it settles nothing of the forms
// queue: the comment stays owed until this build's block index of it writes its blocks and settles
// it after its whole text. The orphan sweep, whose delete owes the forms queue every gone source
// it sweeps, settles those in its own transaction, so a gone source is left owing nothing.
func TestRebuildRefsLeavesEachSourceOwedUntilThisBuildsBlockIndex(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedRefsGrammarIssue(t, database)
	var comment, gone string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'The plan: dispatch://CORE-1/plan.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&gone); err != nil {
		t.Fatalf("draw a uuid: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id) values
			('comment', $1, 'issue', 'CORE-9'),
			('comment', $2, 'issue', 'CORE-1')
	`, comment, gone); err != nil {
		t.Fatalf("write a stale edge and an orphan: %v", err)
	}

	report, err := RebuildAll(ctx, database.Pool, refsGrammarServer)
	if err != nil {
		t.Fatalf("rebuild-refs: %v", err)
	}

	if report.Comments != 1 || report.Orphans != 1 {
		t.Fatalf("rebuild = %#v, want the comment re-indexed and the orphan swept", report)
	}
	if edges := edgesFrom(t, database.Pool, "comment", comment); !slices.Equal(edges, []string{"issue CORE-1"}) {
		t.Fatalf("the comment's edges are %v after rebuild-refs, want the tab address's issue alone", edges)
	}
	if owed := formsOwed(t, database.Pool); !slices.Equal(owed, []string{"comment " + comment}) {
		t.Fatalf("the forms queue holds %v after rebuild-refs, want the comment, whose blocks wait for the block index, and nothing gone", owed)
	}

	if indexed, err := IndexOwedItemBlocks(ctx, database.Pool, OwedItemBlock{Kind: ItemComment, ID: comment}, refsGrammarServer); err != nil || !indexed {
		t.Fatalf("block-index the comment = %v (%v), want it indexed", indexed, err)
	}
	if edges := blockEdgesOf(t, database.Pool, "comment_block_refs", "comment_id", comment); len(edges) != 1 {
		t.Fatalf("the comment's blocks hold %v after this build's block index, want the tab address's edge", edges)
	}
	if owed := formsOwed(t, database.Pool); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after this build's block index of the comment, want it empty", owed)
	}
}
