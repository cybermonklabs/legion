package textversions

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
)

// TestTheFillReadsAgainThePostEventsItPassed holds the fill to the events that rules version 1
// passed with no row: the edit of an issue message and the delete of a comment and of a message
// (dispatch://INFRA-1523), written while the cursor row recorded version 1 and below its cursor.
// While the row records version 1 the keyword table has reached no time, since it lacks their
// rows. The next fill reads them again before it goes on: the edit starts a version with the
// edited text, each delete a version with empty text and the vector of empty text, and the
// versions before them keep their text; it writes the event above the cursor too, and records
// version 2. A fill that meets version 1 again, as a batch of an older build writes it during a
// rolling deploy, writes no row twice.
func TestTheFillReadsAgainThePostEventsItPassed(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()

	const firstMessage = "Can the rotation wait for the freeze?"
	const editedMessage = "Can the rotation wait until the freeze ends on Friday?"
	const commentBody = "The verifiers read the old key until Monday."
	comment := writeComment(t, database, broker, commentBody, true)
	message, created := writeMessage(t, database, broker, firstMessage)
	edit := changePost(t, database, broker, "message", "message.edited", message, editedMessage)
	commentDelete := changePost(t, database, broker, "comment", "comment.deleted", comment.id, "")
	messageDelete := changePost(t, database, broker, "message", "message.deleted", message, "")
	passed := []int64{edit, commentDelete, messageDelete}

	var logs bytes.Buffer
	if err := testFiller(database, events.NewHorizon(database.Pool), &logs).toHorizon(t.Context()); err != nil {
		t.Fatalf("the first fill: %v", err)
	}
	// The table as a fill of rules version 1 leaves it: the rows of every event but the three
	// that version passed, its cursor past them, and the version in the cursor row.
	if _, err := database.Pool.Exec(t.Context(), `delete from search_text_versions where event_id = any($1)`, passed); err != nil {
		t.Fatalf("take out the rows rules version 1 never wrote: %v", err)
	}
	if _, err := database.Pool.Exec(t.Context(), `update search_text_versions_fill set rules = 1`); err != nil {
		t.Fatalf("record rules version 1 in the cursor row: %v", err)
	}
	if cursor := readCursorRow(t, database); cursor.eventID < messageDelete {
		t.Fatalf("the cursor is at event %d, below the passed events %v", cursor.eventID, passed)
	}
	after := time.Now()
	var refusal *store.TextVersionsNotReachedError
	if _, err := database.TextVersionsAt(t.Context(), after); !errors.As(err, &refusal) || refusal.Reached {
		t.Fatalf("TextVersionsAt while the cursor row records rules version 1 = %v, want a refusal that has reached no time", err)
	}

	later := commitComment(t, database, broker, textversionstest.Issue)
	var rereadLogs bytes.Buffer
	if err := testFiller(database, events.NewHorizon(database.Pool), &rereadLogs).toHorizon(t.Context()); err != nil {
		t.Fatalf("the fill that reads again: %v", err)
	}
	requireVersion(t, database, edit, "message", message, editedMessage)
	requireVersion(t, database, commentDelete, "comment", comment.id, "")
	requireVersion(t, database, messageDelete, "message", message, "")
	requireVersion(t, database, comment.event, "comment", comment.id, commentBody)
	requireVersion(t, database, created, "message", message, firstMessage)
	if rows := textversionstest.RowsOf(t, database, later.id); rows != 1 {
		t.Fatalf("the comment written above the cursor has %d rows, want 1", rows)
	}
	if cursor := readCursorRow(t, database); cursor.rules != rulesVersion || cursor.eventID < later.id {
		t.Fatalf("the cursor row records rules %d at event %d, want rules %d at or past event %d", cursor.rules, cursor.eventID, rulesVersion, later.id)
	}
	if !loggedMessage(t, &rereadLogs, rulesReadMessage) {
		t.Errorf("the fill that read again logged no line saying so:\n%s", rereadLogs.String())
	}
	awaitReachPast(t, database, after)
	if _, err := database.TextVersionsAt(t.Context(), after); err != nil {
		t.Fatalf("TextVersionsAt once the fill read the passed events again: %v", err)
	}

	// An older build's batch records rules version 1 again; the next fill reads again from the
	// first event and passes over each one that has its row.
	rows := countRows(t, database)
	if _, err := database.Pool.Exec(t.Context(), `update search_text_versions_fill set rules = 1`); err != nil {
		t.Fatalf("record rules version 1 again: %v", err)
	}
	if err := testFiller(database, events.NewHorizon(database.Pool), &logs).toHorizon(t.Context()); err != nil {
		t.Fatalf("the fill that meets rules version 1 again: %v", err)
	}
	if again := countRows(t, database); again != rows {
		t.Fatalf("the keyword table holds %d rows after a second reading again, want the %d it held", again, rows)
	}
	if cursor := readCursorRow(t, database); cursor.rules != rulesVersion {
		t.Fatalf("the cursor row records rules %d after a second reading again, want %d", cursor.rules, rulesVersion)
	}
}

// requireVersion holds the row event started to a version of item (kind, id) whose body is body,
// from the event's payload, with the vector the item's own search column is made from that body
// (search_vector over search_text, 0087): for an empty body, the vector of empty text.
func requireVersion(t *testing.T, database *store.Store, event int64, kind, id, body string) {
	t.Helper()
	var gotKind, gotID, gotBody, bodyFrom string
	var vectorOfBody bool
	if err := database.Pool.QueryRow(t.Context(), `
		select kind, id, body, body_from, search = search_vector('', search_text($2))
		  from search_text_versions where event_id = $1`, event, body).Scan(&gotKind, &gotID, &gotBody, &bodyFrom, &vectorOfBody); err != nil {
		t.Fatalf("read the version event %d started: %v", event, err)
	}
	if gotKind != kind || gotID != id || gotBody != body || bodyFrom != bodyFromEvent || !vectorOfBody {
		t.Fatalf("event %d started %s %s with body %q from %s (its vector that of %q: %v), want %s %s with body %q from the event",
			event, gotKind, gotID, abridged(gotBody, 80), bodyFrom, abridged(body, 80), vectorOfBody, kind, id, abridged(body, 80))
	}
}

// writeMessage writes an issue message of body on textversionstest.Issue with its message.created
// event, in one transaction, as the create route does, and answers its id and the event's id.
func writeMessage(t *testing.T, database *store.Store, broker *events.Broker, body string) (string, int64) {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the message's transaction: %v", err)
	}
	defer tx.Rollback(t.Context())
	var id string
	if err := tx.QueryRow(t.Context(), `
		insert into messages (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2)
		returning id::text`, textversionstest.Issue, body).Scan(&id); err != nil {
		t.Fatalf("insert the message: %v", err)
	}
	issue := textversionstest.Issue
	appended, err := broker.Append(t.Context(), tx, model.Event{IssueKey: &issue, Type: "message.created",
		Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{"id": id, "issue_key": issue, "body": body}})
	if err != nil {
		t.Fatalf("append message.created: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit the message: %v", err)
	}
	return id, appended.ID
}

// changePost writes one change of post id of kind (a comment or a message) on
// textversionstest.Issue in one transaction, as the edit and delete routes write it: for an edit,
// body as the row's body and edited_at the transaction's now(); for a delete (an empty body), the
// cleared body and deleted_at; and the event of eventType, whose payload carries the new body, and
// none for a delete. It answers the event's id.
func changePost(t *testing.T, database *store.Store, broker *events.Broker, kind, eventType, id, body string) int64 {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the %s: %v", eventType, err)
	}
	defer tx.Rollback(t.Context())
	update := `update ` + kind + `s set body = $2, edited_at = now() where id = $1::uuid`
	payload := map[string]any{"id": id, "issue_key": textversionstest.Issue, "body": body}
	if body == "" {
		update = `update ` + kind + `s set body = $2, deleted_at = now(), deleted_by = '{"kind":"user","id":"alice"}' where id = $1::uuid`
		delete(payload, "body")
	}
	if _, err := tx.Exec(t.Context(), update, id, body); err != nil {
		t.Fatalf("write the %s: %v", eventType, err)
	}
	issue := textversionstest.Issue
	appended, err := broker.Append(t.Context(), tx, model.Event{IssueKey: &issue, Type: eventType,
		Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: payload})
	if err != nil {
		t.Fatalf("append %s: %v", eventType, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit the %s: %v", eventType, err)
	}
	return appended.ID
}

// loggedMessage reports whether logs, a JSON handler's output, holds a line whose message is msg.
func loggedMessage(t *testing.T, logs *bytes.Buffer, msg string) bool {
	t.Helper()
	for _, line := range logLines(t, logs) {
		if line.Msg == msg {
			return true
		}
	}
	return false
}
