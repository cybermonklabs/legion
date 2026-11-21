package textversions

import (
	"context"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
)

// rulesVersion1 stands for the rules of version 1, which the fill of the build before this one
// ran: versionRules without the types version 2 added (rulesAdded), a comment's present row read
// past its later edits alone and a message's past no later event, no body history, and no mask of
// an issue's document version, nor any owner lock. A test runs it on a filler (filler.rules) to
// write the rows a fill of that build wrote during a roll.
var rulesVersion1 = newRuleSet(1, rulesBefore(2), map[string]postBodyRead{
	kindComment: postBodyReadOf("comments", "comment_id", "comment.edited"),
	kindMessage: postBodyReadOf("messages", "message_id"),
}, false, false)

// rulesBefore is versionRules without the types rules version version added (rulesAdded).
func rulesBefore(version int) map[string]rule {
	rules := maps.Clone(versionRules)
	for _, eventType := range rulesAdded[version] {
		delete(rules, eventType)
	}
	return rules
}

// insertedWord is the word a pending insert suggestion put in the issue's primary document, which
// no other text of the test holds.
const insertedWord = "zarquoninserted"

// TestTheReReadLeavesTheRowsAFillOfThisBuildAloneWrites writes, on one issue with a primary
// document, the posts a fill of rules version 1 decides otherwise than this build's: a comment and
// a message, each with a body an event cuts, then edited, then deleted; a message with such a
// body, then edited to another such body; and version 2 of the document, holding the words of a
// pending insert whose comment this build deletes, with the delete's rewrite of the keyword rows.
//
// A fill of rules version 1 (rulesVersion1) writes them: it passes over the deletes and the
// message's edits, reads the cut bodies from rows those changed, and writes version 2's row from
// its stored markdown, which matches the insert's word. A fill of this build then reads them
// again and raises the rules version, and no row matches the word. A fill of rules version 1 writes
// again, the edited message's second edit and its delete and a new message with a cut body, then
// edited, and lowers the version again; and a fill of this build reads again once more. At the end
// the table holds, column for column, body and body_from included, the rows a fill of this build
// alone writes of the same log from an empty table.
func TestTheReReadLeavesTheRowsAFillOfThisBuildAloneWrites(t *testing.T) {
	for _, eventType := range rulesAdded[2] {
		if _, ok := versionRules[eventType]; !ok {
			t.Fatalf("rules version 2 adds %s, which versionRules does not read", eventType)
		}
	}
	database := storetest.Open(t)
	textversionstest.SeedProject(t, database)
	broker := events.NewBroker()
	ctx := t.Context()
	horizon := events.NewHorizon(database.Pool)
	older := testFiller(database, horizon, io.Discard)
	older.rules = &rulesVersion1
	this := testFiller(database, horizon, io.Discard)
	fill := func(f *filler, what string, want int) {
		t.Helper()
		if err := f.toHorizon(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := readCursorRow(t, database).rules; got != want {
			t.Fatalf("after %s the cursor row holds rules version %d, want %d", what, got, want)
		}
	}

	createIssue(t, database, broker, 1, "Rotate the signing keys", "The brown fox jumps")
	document := primaryDocument(t, database, textversionstest.Issue)
	long := longCommentBody()
	comment := writePost(t, database, broker, kindComment, long)
	correctPost(t, database, broker, kindComment, comment.id, "The rotation moved to the spec.", false)
	correctPost(t, database, broker, kindComment, comment.id, "", true)
	message := writePost(t, database, broker, kindMessage, long)
	correctPost(t, database, broker, kindMessage, message.id, "Can the rotation wait for Friday?", false)
	correctPost(t, database, broker, kindMessage, message.id, "", true)
	edited := writePost(t, database, broker, kindMessage, "The edited message's first plan.\n\n"+long)
	correctPost(t, database, broker, kindMessage, edited.id, "The edited message's second plan.\n\n"+long, false)
	writeInsertVersion(t, database, broker, document, 2)
	suggestion := writeSuggestion(t, database, broker, document)
	deleteSuggestion(t, database, broker, document, suggestion)

	fill(older, "a fill of rules version 1", 1)
	// The controls: the rows the re-read changes are wrong before it.
	if matching := rowsMatching(t, database, insertedWord); matching == 0 {
		t.Fatalf("no row the fill of rules version 1 wrote matches the deleted insert's word %q, so the check of the mask cannot fail", insertedWord)
	}
	if body := createdBody(t, database, message.event); body == long {
		t.Fatal("the message's created row the fill of rules version 1 wrote holds its whole first body, so the check of the redo cannot fail")
	}
	fill(this, "a fill of this build after rules version 1's", rulesVersion)
	if matching := rowsMatching(t, database, insertedWord); matching != 0 {
		t.Errorf("after this build's re-read %d rows match the deleted insert's word %q, want none", matching, insertedWord)
	}

	correctPost(t, database, broker, kindMessage, edited.id, "Friday is fine.", false)
	correctPost(t, database, broker, kindMessage, edited.id, "", true)
	third := writePost(t, database, broker, kindMessage, "The third message's plan.\n\n"+long)
	correctPost(t, database, broker, kindMessage, third.id, "The third plan moved to the spec.", false)
	fill(older, "a second fill of rules version 1", 1)
	fill(this, "a second fill of this build", rulesVersion)
	if matching := rowsMatching(t, database, insertedWord); matching != 0 {
		t.Errorf("after this build's second re-read %d rows match the deleted insert's word %q, want none", matching, insertedWord)
	}
	got := keywordRows(t, database)

	if _, err := database.Pool.Exec(ctx, `delete from search_text_versions; delete from search_text_versions_fill`); err != nil {
		t.Fatalf("empty the keyword table: %v", err)
	}
	fill(testFiller(database, events.NewHorizon(database.Pool), io.Discard), "a fill of this build alone", rulesVersion)
	want := keywordRows(t, database)
	if len(want) == 0 {
		t.Fatal("the fill of this build alone wrote no row")
	}
	wanted := make(map[string]bool, len(want))
	for _, row := range want {
		wanted[row] = true
	}
	held := make(map[string]bool, len(got))
	for _, row := range got {
		held[row] = true
		if !wanted[row] {
			t.Errorf("the table holds a row a fill of this build alone does not write:\n\t%s", row)
		}
	}
	for _, row := range want {
		if !held[row] {
			t.Errorf("the table lacks a row a fill of this build alone writes:\n\t%s", row)
		}
	}
}

// primaryDocument is the id of issue key's primary document.
func primaryDocument(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(t.Context(), `select id::text from artifacts where issue_key = $1 and is_primary`, key).Scan(&id); err != nil {
		t.Fatalf("read the primary document of %s: %v", key, err)
	}
	return id
}

// insertMark is the id of the insert suggestion's mark writeInsertVersion writes.
const insertMark = "m-insert"

// writeInsertVersion writes version number of document, textversionstest.Issue's primary one, as
// a version write of this build stores it while an insert suggestion stands pending
// (insertVersionText), with the artifact.version event the write appends.
func writeInsertVersion(t *testing.T, database *store.Store, broker *events.Broker, document string, number int) {
	t.Helper()
	markdown, insertTree := insertVersionText(t)
	key := textversionstest.Issue
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		if _, err := tx.Exec(t.Context(), `
			insert into artifact_versions (artifact_id, number, markdown, authors, insert_tree) values ($1::uuid, $2, $3, '[]', $4)
		`, document, number, markdown, insertTree); err != nil {
			t.Fatalf("insert version %d of the document: %v", number, err)
		}
		textversionstest.AppendEvents(t, tx, broker, model.Event{IssueKey: &key, Type: "artifact.version",
			Actor:   model.Actor{Kind: "user", ID: "alice"},
			Payload: map[string]any{"artifact_id": document, "version": map[string]any{"number": number, "size": len(markdown)}}})
	})
}

// insertVersionText is a document version as a version write of this build stores it while an
// insert suggestion stands pending: its markdown, which holds insertedWord, and the tree it was
// rendered from, which marks the word as the insert's, of the mark insertMark (docs.InsertTree).
func insertVersionText(tb testing.TB) (string, []byte) {
	tb.Helper()
	tree := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{Type: "paragraph", Children: []*pmdoc.Node{
		{Type: "text", Text: "The "},
		{Type: "text", Text: insertedWord + " ", Marks: []pmdoc.Mark{{Type: string(docs.MarkSuggestion),
			Attrs: pmdoc.Attrs{"id": insertMark, "by": "user:alice", "kind": "insert"}}}},
		{Type: "text", Text: "brown fox jumps"},
	}}}}
	pmdoc.EnsureBlockIDs(tree)
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		tb.Fatalf("render the version holding the insert: %v", err)
	}
	insertTree, err := docs.InsertTree(tree)
	if err != nil || insertTree == nil || !strings.Contains(markdown, insertedWord) {
		tb.Fatalf("the version holding the insert renders %q with insert tree %s (%v); want the word %q and a tree", markdown, insertTree, err, insertedWord)
	}
	return markdown, insertTree
}

// writeSuggestion writes alice's comment on textversionstest.Issue anchored on the insert mark of
// document, with its comment.created, and answers it.
func writeSuggestion(t *testing.T, database *store.Store, broker *events.Broker, document string) writtenComment {
	t.Helper()
	const body = "Add this word."
	var comment writtenComment
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		if err := tx.QueryRow(t.Context(), `
			insert into comments (issue_key, author, body, anchor)
			values ($1, '{"kind":"user","id":"alice"}', $2, jsonb_build_object('artifact_id', $3::text, 'mark_id', $4::text))
			returning id::text
		`, textversionstest.Issue, body, document, insertMark).Scan(&comment.id); err != nil {
			t.Fatalf("insert the suggestion's comment: %v", err)
		}
		comment.event = appendPostEvent(t, tx, broker, "comment.created", comment.id, body)
	})
	return comment
}

// deleteSuggestion deletes suggestion's comment as this build's delete does (correctPost), and
// then rewrites the keyword rows of the versions of document holding its insert's words, as the
// delete's write phase does (docs.MaskedVersionsOnDelete, store.QueueTextVersionsMask), which
// finds none, since no fill has written a row of those versions yet. It fails t unless the mask
// takes the word out of a version.
func deleteSuggestion(t *testing.T, database *store.Store, broker *events.Broker, document string, suggestion writtenComment) {
	t.Helper()
	correctPost(t, database, broker, kindComment, suggestion.id, "", true)
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		ctx := t.Context()
		numbers, texts, err := docs.MaskedVersionsOnDelete(ctx, tx, document, insertMark, suggestion.id)
		if err != nil {
			t.Fatalf("mask the versions holding the insert: %v", err)
		}
		if len(numbers) == 0 || strings.Contains(texts[0], insertedWord) {
			t.Fatalf("the delete masks versions %v to %q, want a version without the word %q", numbers, texts, insertedWord)
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			store.QueueTextVersionsMask(b, document, numbers, texts)
			return nil
		}); err != nil {
			t.Fatalf("rewrite the keyword rows of the versions holding the insert: %v", err)
		}
	})
}

// rowsMatching counts the keyword rows whose vector matches word.
func rowsMatching(t *testing.T, database *store.Store, word string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(t.Context(),
		`select count(*) from search_text_versions where search @@ plainto_tsquery('english', $1)`, word).Scan(&count); err != nil {
		t.Fatalf("count the keyword rows matching %q: %v", word, err)
	}
	return count
}

// createdBody is the body of the keyword row event started.
func createdBody(t *testing.T, database *store.Store, event int64) string {
	t.Helper()
	var body string
	if err := database.Pool.QueryRow(t.Context(), `select body from search_text_versions where event_id = $1`, event).Scan(&body); err != nil {
		t.Fatalf("read the row of event %d: %v", event, err)
	}
	return body
}

// keywordRows is every keyword row, each as one line of every column, in the table's key order:
// the body and the vector as their lengths and md5 digests, so a line stays short and a change of
// either changes it.
func keywordRows(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select format('%s %s seq %s event %s issue %s project %s at %s body %s (%s bytes) from %s spec %s/%s search %s (%s bytes)',
		              kind, id, event_seq, event_id, coalesce(issue_key, '-'), project_key, started_at, md5(body), octet_length(body),
		              body_from, coalesce(spec_artifact_id::text, '-'), coalesce(spec_version::text, '-'),
		              md5(search::text), octet_length(search::text))
		  from search_text_versions
		 order by kind, id, event_seq`)
	if err != nil {
		t.Fatalf("read the keyword rows: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the keyword rows: %v", err)
	}
	return lines
}
