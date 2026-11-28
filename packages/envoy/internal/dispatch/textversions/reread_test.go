package textversions

import (
	"context"
	"io"
	"maps"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
)

// rulesVersion1 stands for the rules of version 1, which the fill of the build before this one
// ran: versionRules without the types version 2 added (rulesAdded), a comment's present row read
// past its later edits alone and a message's past no later event, and no body history. A test
// runs it on a filler (filler.rules) to write the rows a fill of that build wrote during a roll.
var rulesVersion1 = newRuleSet(1, rulesBefore(2), map[string]postBodyRead{
	kindComment: postBodyReadOf("comments", "comment_id", "comment.edited"),
	kindMessage: postBodyReadOf("messages", "message_id"),
}, false)

// rulesBefore is versionRules without the types rules version version added (rulesAdded).
func rulesBefore(version int) map[string]rule {
	rules := maps.Clone(versionRules)
	for _, eventType := range rulesAdded[version] {
		delete(rules, eventType)
	}
	return rules
}

// TestTheReReadLeavesTheRowsAFillOfThisBuildAloneWrites writes, on one issue, the posts a fill of
// rules version 1 decides otherwise than this build's: a comment and a message, each with a body an
// event cuts, then edited, then deleted; and a message with such a body, then edited to another
// such body.
//
// A fill of rules version 1 (rulesVersion1) writes them: it passes over the deletes and the
// message's edits, and reads the cut bodies from rows those changed. A fill of this build then
// reads them again and raises the rules version. A fill of rules version 1 writes again, the edited
// message's second edit and its delete and a new message with a cut body, then edited, and lowers
// the version again; and a fill of this build reads again once more. At the end the table holds,
// column for column, body and body_from included, the rows a fill of this build alone writes of
// the same log from an empty table.
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
	long := longCommentBody()
	comment := writePost(t, database, broker, kindComment, long)
	correctPost(t, database, broker, kindComment, comment.id, "The rotation moved to the spec.", false)
	correctPost(t, database, broker, kindComment, comment.id, "", true)
	message := writePost(t, database, broker, kindMessage, long)
	correctPost(t, database, broker, kindMessage, message.id, "Can the rotation wait for Friday?", false)
	correctPost(t, database, broker, kindMessage, message.id, "", true)
	edited := writePost(t, database, broker, kindMessage, "The edited message's first plan.\n\n"+long)
	correctPost(t, database, broker, kindMessage, edited.id, "The edited message's second plan.\n\n"+long, false)

	fill(older, "a fill of rules version 1", 1)
	// The control: the rows the re-read changes are wrong before it.
	if body := createdBody(t, database, message.event); body == long {
		t.Fatal("the message's created row the fill of rules version 1 wrote holds its whole first body, so the check of the redo cannot fail")
	}
	fill(this, "a fill of this build after rules version 1's", RulesVersion)

	correctPost(t, database, broker, kindMessage, edited.id, "Friday is fine.", false)
	correctPost(t, database, broker, kindMessage, edited.id, "", true)
	third := writePost(t, database, broker, kindMessage, "The third message's plan.\n\n"+long)
	correctPost(t, database, broker, kindMessage, third.id, "The third plan moved to the spec.", false)
	fill(older, "a second fill of rules version 1", 1)
	fill(this, "a second fill of this build", RulesVersion)
	got := keywordRows(t, database)

	if _, err := database.Pool.Exec(ctx, `delete from search_text_versions; delete from search_text_versions_fill`); err != nil {
		t.Fatalf("empty the keyword table: %v", err)
	}
	fill(testFiller(database, events.NewHorizon(database.Pool), io.Discard), "a fill of this build alone", RulesVersion)
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
