package textversions

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestAnEditedMessageStartsAVersionOfItsNewBody writes an issue message and two edits of it, as
// the edit route writes them (correctPost): the first changes its body, the second writes the
// same body again. The message's create starts its first version, the first edit a version of its
// new body, which its event carries whole, and the second edit none.
func TestAnEditedMessageStartsAVersionOfItsNewBody(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	const first, edited = "Can the rotation wait for the freeze to end?", "Can the rotation wait until the freeze ends on Friday?"
	message := writeIssueMessage(t, database, broker, first)
	edit := correctPost(t, database, broker, kindMessage, message.id, edited, false)
	same := correctPost(t, database, broker, kindMessage, message.id, edited, false)

	var logs logtest.Buffer
	got := decideAndWrite(t, database, &logs, readLoggedEvents(t, database, message.event, edit, same))
	if len(got.Versions) != 2 {
		t.Fatalf("the message's create and its two edits start %d versions, want the create's and the first edit's", len(got.Versions))
	}
	if v := startedBy(t, got, message.event); v.Body != first || v.BodyFrom != bodyFromEvent {
		t.Fatalf("the message.created version holds %q from %q, want %q from %q", v.Body, v.BodyFrom, first, bodyFromEvent)
	}
	if v := startedBy(t, got, edit); v.Kind != kindMessage || v.ID != message.id || v.Body != edited || v.BodyFrom != bodyFromEvent {
		t.Fatalf("the message.edited version is %s %s holding %q from %q, want message %s holding %q from %q",
			v.Kind, v.ID, v.Body, v.BodyFrom, message.id, edited, bodyFromEvent)
	}
	requireRowIsPostsColumn(t, database, kindMessage, message.id, edit)
}

// TestAnEditedThenDeletedPostEndsWithAnEmptyVersion writes a comment and an issue message, each
// then edited and deleted as the routes write them (correctPost). Each post's three events start
// three versions, the last the delete's, of the empty body from its event, whose row's vector is
// the post's own search column after the delete, the vector of the empty body.
func TestAnEditedThenDeletedPostEndsWithAnEmptyVersion(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	for _, kind := range []string{kindComment, kindMessage} {
		t.Run(kind, func(t *testing.T) {
			post := writePost(t, database, broker, kind, "Rotate the staging key first.")
			edit := correctPost(t, database, broker, kind, post.id, "Rotate the staging key, then the production key.", false)
			deleted := correctPost(t, database, broker, kind, post.id, "", true)

			var logs logtest.Buffer
			got := decideAndWrite(t, database, &logs, readLoggedEvents(t, database, post.event, edit, deleted))
			if len(got.Versions) != 3 {
				t.Fatalf("the %s's create, edit and delete start %d versions, want 3", kind, len(got.Versions))
			}
			if v := startedBy(t, got, edit); v.Body != "Rotate the staging key, then the production key." {
				t.Fatalf("the %s's edit holds %q, want its new body", kind, v.Body)
			}
			last := got.Versions[len(got.Versions)-1]
			if last.EventID != deleted || last.Kind != kind || last.ID != post.id || last.Body != "" || last.BodyFrom != bodyFromEvent || last.BodyTruncated {
				t.Fatalf("the %s's last version is event %d's, %s %s holding %q from %q (cut %v); want the delete's (event %d) holding the empty body from %q",
					kind, last.EventID, last.Kind, last.ID, last.Body, last.BodyFrom, last.BodyTruncated, deleted, bodyFromEvent)
			}
			requireRowIsPostsColumn(t, database, kind, post.id, deleted)
		})
	}
}

// TestACutBodyIsReadWholeFromThePostsBodyHistory holds the body history as the source of a body
// an event cut, where the post's present row no longer holds it: the body the first later edit or
// delete of the post kept, exactly, read before revision 1 of a comment's blocks. Where a later
// change of the post kept no history, as a build before the history wrote its edits, the history
// gives no body, rather than the one another change kept.
func TestACutBodyIsReadWholeFromThePostsBodyHistory(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	long := longCommentBody()
	if _, truncated := model.EventBody(long); !truncated {
		t.Fatal("the body is one an event carries whole, which tests no cut")
	}

	for _, kind := range []string{kindComment, kindMessage} {
		t.Run("the first body of a "+kind+" edited since", func(t *testing.T) {
			// A comment's blocks hold its first body too, so the history is read first.
			post := writePost(t, database, broker, kind, long)
			edit := correctPost(t, database, broker, kind, post.id, "The plan moved to the issue's spec.", false)
			var logs logtest.Buffer
			got := decideAndWrite(t, database, &logs, readLoggedEvents(t, database, post.event, edit))
			requireWholeFromHistory(t, database, got, &logs, kind, post.id, post.event, long)
		})
	}

	t.Run("a cut edit, then a delete", func(t *testing.T) {
		post := writePost(t, database, broker, kindMessage, "Rotate the staging key first.")
		edit := correctPost(t, database, broker, kindMessage, post.id, long, false)
		deleted := correctPost(t, database, broker, kindMessage, post.id, "", true)
		var logs logtest.Buffer
		got := decideAndWrite(t, database, &logs, readLoggedEvents(t, database, post.event, edit, deleted))
		requireWholeFromHistory(t, database, got, &logs, kindMessage, post.id, edit, long)
		if v := startedBy(t, got, deleted); v.Body != "" {
			t.Fatalf("the delete's version holds %d bytes, want the empty body", len(v.Body))
		}
	})

	t.Run("a later change that kept no history", func(t *testing.T) {
		// The first edit is an older build's, which kept no history; the second keeps the body
		// the first wrote, the one row of the comment's history. That row is no body of the
		// comment.created's version, which takes its own from its first revision's blocks; nor,
		// since the comment's changes and its history rows are not as many, is it read for the
		// older edit's version, which keeps its event's cut text.
		comment := writeComment(t, database, broker, long, true)
		older := "An older build's edit.\n\n" + long
		olderEdit := editComment(t, database, broker, comment.id, older)
		laterEdit := correctPost(t, database, broker, kindComment, comment.id, "The plan moved to the issue's spec.", false)
		var logs logtest.Buffer
		got := decideAndWrite(t, database, &logs, readLoggedEvents(t, database, comment.event, olderEdit, laterEdit))
		if v := startedBy(t, got, comment.event); v.BodyFrom != bodyFromBlocks || v.Body == older {
			t.Fatalf("the comment.created version holds %d bytes from %q; want its own body from %q, its first revision's blocks, and not the one the history kept of the older build's edit",
				len(v.Body), v.BodyFrom, bodyFromBlocks)
		}
		if v := startedBy(t, got, olderEdit); v.BodyFrom != bodyFromCut {
			t.Fatalf("the older build's edit's version holds %d bytes from %q, want its event's cut text (%q)", len(v.Body), v.BodyFrom, bodyFromCut)
		}
	})
}

// requireWholeFromHistory holds the version event starts of post id of kind, whose event cut
// body, to the whole body from the post's history, as decided and as written, with no cut body
// in the batch and no warning.
func requireWholeFromHistory(t *testing.T, database *store.Store, got decided, logs *logtest.Buffer, kind, id string, event int64, body string) {
	t.Helper()
	v := startedBy(t, got, event)
	if !v.BodyTruncated || v.BodyFrom != bodyFromHistory || v.Body != body {
		t.Fatalf("the version of event %d holds %d bytes from %q (cut %v), want the %s's whole %d bytes from %q",
			event, len(v.Body), v.BodyFrom, v.BodyTruncated, kind, len(body), bodyFromHistory)
	}
	var stored, bodyFrom string
	if err := database.Pool.QueryRow(t.Context(), `
		select body, body_from from search_text_versions where kind = $1 and id = $2 and event_id = $3
	`, kind, id, event).Scan(&stored, &bodyFrom); err != nil {
		t.Fatalf("read the row of event %d: %v", event, err)
	}
	if stored != body || bodyFrom != bodyFromHistory {
		t.Fatalf("the row of event %d holds %d bytes from %q, want the whole %d bytes from %q", event, len(stored), bodyFrom, len(body), bodyFromHistory)
	}
	if got.Cut != 0 || len(loggedWarnings(t, logs)) != 0 {
		t.Fatalf("the batch kept %d cut bodies and logged %q, want none", got.Cut, logs.String())
	}
}

// requireRowIsPostsColumn holds the row event started of post id of kind to the post's search
// column as it stands: the version of an unchanged item equals that column.
func requireRowIsPostsColumn(t *testing.T, database *store.Store, kind, id string, event int64) {
	t.Helper()
	table := map[string]string{kindComment: "comments", kindMessage: "messages"}[kind]
	var equal bool
	if err := database.Pool.QueryRow(t.Context(), `
		select v.search = p.search from search_text_versions v, public.`+table+` p
		 where v.kind = $1 and v.id = $2 and v.event_id = $3 and p.id = $2::uuid
	`, kind, id, event).Scan(&equal); err != nil {
		t.Fatalf("read the row of event %d: %v", event, err)
	}
	if !equal {
		t.Fatalf("the row of event %d holds a vector other than the %s's search column", event, kind)
	}
}

// writePost writes a post of kind, of body, on textversionstest.Issue, as the create route
// writes one: a comment with its block index (writeComment), or an issue message
// (writeIssueMessage).
func writePost(t *testing.T, database *store.Store, broker *events.Broker, kind, body string) writtenComment {
	t.Helper()
	if kind == kindMessage {
		return writeIssueMessage(t, database, broker, body)
	}
	return writeComment(t, database, broker, body, true)
}

// writeIssueMessage writes an issue message of body on textversionstest.Issue, with its
// message.created event, whose body the event carries as model.EventBody cuts it, in one
// transaction, as the create route does.
func writeIssueMessage(t *testing.T, database *store.Store, broker *events.Broker, body string) writtenComment {
	t.Helper()
	var message writtenComment
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		var createdAt time.Time
		if err := tx.QueryRow(t.Context(), `
			insert into messages (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2)
			returning id::text, created_at
		`, textversionstest.Issue, body).Scan(&message.id, &createdAt); err != nil {
			t.Fatalf("insert the message: %v", err)
		}
		message.event = appendPostEvent(t, tx, broker, "message.created", message.id, body)
	})
	return message
}

// correctPost writes an edit of post id of kind to body, or with deleted its delete, in a
// transaction of its own, as the edit and delete routes write them (api's editComment,
// editMessage and clearDeletedPostBody): the body it replaces kept in post_body_history first
// (postBodyHistoryStep), then the post's row, its body written with its edited_at, or emptied
// with its deleted_at and deleted_by, and then its comment.edited, message.edited,
// comment.deleted or message.deleted, whose id it answers. A delete's event carries the emptied
// body, as the route builds it from the cleared post.
func correctPost(t *testing.T, database *store.Store, broker *events.Broker, kind, id, body string, deleted bool) int64 {
	t.Helper()
	table, column := "comments", "comment_id"
	if kind == kindMessage {
		table, column = "messages", "message_id"
	}
	eventType, update := kind+".edited", `update public.`+table+` set body = $2, edited_at = now() where id = $1::uuid`
	if deleted {
		body = ""
		eventType = kind + ".deleted"
		update = `update public.` + table + ` set body = $2, deleted_at = now(), deleted_by = '{"kind":"user","id":"alice"}' where id = $1::uuid`
	}
	var event int64
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		ctx := t.Context()
		if _, err := tx.Exec(ctx, `
			insert into post_body_history (`+column+`, body, edited_by)
			select id, body, '{"kind":"user","id":"alice"}' from public.`+table+` where id = $1::uuid
		`, id); err != nil {
			t.Fatalf("keep the body %s %s had: %v", kind, id, err)
		}
		if _, err := tx.Exec(ctx, update, id, body); err != nil {
			t.Fatalf("write %s %s: %v", kind, id, err)
		}
		event = appendPostEvent(t, tx, broker, eventType, id, body)
	})
	return event
}

// appendPostEvent appends the event of type of post id on textversionstest.Issue, whose body is
// body, through the broker, with the payload the rules read of it: its id, its issue, and its
// body as model.EventBody cuts it, saying so where it does.
func appendPostEvent(t *testing.T, tx pgx.Tx, broker *events.Broker, eventType, id, body string) int64 {
	t.Helper()
	issue := textversionstest.Issue
	carried, truncated := model.EventBody(body)
	payload := map[string]any{"id": id, "issue_key": issue, "body": carried}
	if truncated {
		payload["body_truncated"] = true
	}
	appended, err := broker.Append(t.Context(), tx, model.Event{
		IssueKey: &issue, Type: eventType, Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: payload,
	})
	if err != nil {
		t.Fatalf("append %s: %v", eventType, err)
	}
	return appended.ID
}
