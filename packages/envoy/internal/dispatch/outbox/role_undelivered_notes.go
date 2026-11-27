package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
)

// undeliveredEvent is what the message about an undelivered event names of it, as appendUndelivered
// reads it: its own issue, the id of the post it carries, and the document it is about - the
// event's own document, or the document an issue's document event names (payload artifact_id, as
// artifact.version and artifact.human_edited carry it) - with what that document's address is
// written from (refs.ArtifactRef): its issue, project, session, slug, kind and whether it is its
// issue's primary document.
type undeliveredEvent struct {
	issueKey        *string
	postID          string
	documentID      *string
	documentIssue   *string
	documentProject string
	documentSession *string
	documentSlug    string
	documentKind    string
	documentPrimary bool
}

// undeliveredNoteWindow is the hour one message about the events recorded undelivered to one role
// on one issue covers: the first such event opens it, every one recorded before it ends joins it,
// and the message is posted once it has ended (postUndeliveredNotes). A role nobody holds misses
// each event routed to it, so this is what keeps the issue to one message an hour for that role
// rather than one per event; the role.undelivered record of each is in the history at once.
const undeliveredNoteWindow = time.Hour

// noteUndelivered notes, in tx, the event record names as undelivered to its role, for the
// message postUndeliveredNotes posts about it: on the event's own issue, or on the issue of its
// document, which is the issue whose route the role is. It joins the window of that issue and role
// opened less than undeliveredNoteWindow before whose message is not posted yet, and opens one at
// now when none was. The note names the post an issue's comment or message event carries
// (dispatch://<issue>/<kind>/<id>), or else the document a document event is about, by the
// address the dashboard copies for it (refs.ArtifactRef: dispatch://<issue>/spec for an issue's
// primary document), and names nothing for any other event, which the message names by its id and
// type alone. An event of neither an issue nor an issue's document is noted nowhere, with an Error
// line (appendUndelivered says why no write makes one). A repeat of the note for one event and
// role is no new note.
func noteUndelivered(ctx context.Context, tx pgx.Tx, record model.RoleUndeliveredEventPayload, about undeliveredEvent) error {
	var issueKey string
	switch {
	case about.issueKey != nil:
		issueKey = *about.issueKey
	case about.documentIssue != nil:
		issueKey = *about.documentIssue
	default:
		slog.Error("dispatch outbox: an event recorded undelivered to a role is of neither an issue nor an issue's document, so no message is posted about it",
			"event_id", record.EventID, "role", record.Role)
		return nil
	}
	var ref string
	var document *string
	switch kind := dispatchPostKind(record.EventType); {
	case about.issueKey != nil && kind != "" && about.postID != "":
		ref = "dispatch://" + issueKey + "/" + kind + "/" + about.postID
	case about.documentID != nil && about.documentSlug != "":
		ref = refs.ArtifactRef(about.documentIssue, about.documentProject, about.documentSession, about.documentSlug,
			about.documentKind, about.documentPrimary)
		document = about.documentID
	}
	if _, err := tx.Exec(ctx, `
		insert into outbox_role_undelivered_notes
			(event_id, role, issue_key, artifact_id, event_type, keeps, reason, item_ref, window_opened_at)
		values ($1, $2, $3, $4::uuid, $5, $6, $7, nullif($8, ''), coalesce(
			(select max(window_opened_at) from outbox_role_undelivered_notes
			 where issue_key = $3 and role = $2 and window_opened_at > now() - $9::interval and posted_at is null),
			now()))
		on conflict (event_id, role) do nothing
	`, record.EventID, record.Role, issueKey, document, record.EventType, record.Keeps, record.Reason, ref,
		undeliveredNoteWindow); err != nil {
		return fmt.Errorf("note event %d undelivered to role %s: %w", record.EventID, record.Role, err)
	}
	return nil
}

// dispatchPostKind is the kind of post an event of type eventType carries, as a reference names
// it - a suggestion is a comment - and "" for an event that carries no post.
func dispatchPostKind(eventType string) string {
	switch prefix, _, _ := strings.Cut(eventType, "."); prefix {
	case "comment", "suggestion":
		return "comment"
	case "message":
		return "message"
	}
	return ""
}

// undeliveredWindow is one window of events recorded undelivered to role whose message goes on
// issue issueKey (noteUndelivered): the hour from openedAt, its notes in event order, and how many
// of the posts of its message have failed so far.
type undeliveredWindow struct {
	issueKey string
	role     string
	openedAt time.Time
	notes    []undeliveredNote
	failures int
}

// undeliveredNote is one event of a window: what the message names of it.
type undeliveredNote struct {
	eventID   int64
	eventType string
	keeps     int
	reason    string
	itemRef   string
	// documentID is the id of the document a document event is about, "" for any other event.
	documentID string
}

// undeliveredWindowsQuery reads the windows due a message: unposted, an hour old by the database's
// clock (undeliveredNoteWindow), and past any back-off a failed post left, oldest first, batchSize
// at most, each with its notes in event order. It reads none while instance does not hold the
// publisher hold, on the database's clock, so a process the hold has left posts nothing.
const undeliveredWindowsQuery = `
	with hold as (
		select exists (select 1 from duty_holds where name = $3 and holder = $4 and expires_at > now()) as mine
	), due as (
		select issue_key, role, window_opened_at
		from outbox_role_undelivered_notes
		where posted_at is null and window_opened_at <= now() - $1::interval
		  and (next_attempt_at is null or next_attempt_at <= now())
		  and (select mine from hold)
		group by issue_key, role, window_opened_at
		order by window_opened_at, issue_key, role
		limit $2
	)
	select n.issue_key, n.role, n.window_opened_at, n.event_id, n.event_type, n.keeps, n.reason,
	       coalesce(n.item_ref, ''), coalesce(n.artifact_id::text, ''), n.attempt_count
	from due d
	join outbox_role_undelivered_notes n
	  on n.issue_key = d.issue_key and n.role = d.role and n.window_opened_at = d.window_opened_at
	where n.posted_at is null
	order by n.window_opened_at, n.issue_key, n.role, n.event_id
`

// undeliveredMessageNamespace is the namespace a window's message id is derived in
// (undeliveredWindow.messageID).
var undeliveredMessageNamespace = uuid.MustParse("6d3f4b2e-9a0c-5e1f-8b7a-2c4d6e8f0a1b")

// messageID is the id of w's message: derived from its issue, its role, the moment it opened and
// the events it names, so every post of the same notes, by any process and after any restart,
// names the one message and the issue message writer stores it once
// (api.Background.PostSystemMessage). A note that joined after the window's message was posted
// changes the set its next read finds unposted, so it is posted in a message of its own that names
// it, rather than marked posted under one that does not.
func (w undeliveredWindow) messageID() string {
	key := []byte(w.issueKey + "\x00" + w.role + "\x00" + strconv.FormatInt(w.openedAt.UnixMicro(), 10))
	for _, note := range w.notes {
		key = strconv.AppendInt(append(key, 0), note.eventID, 10)
	}
	return uuid.NewSHA1(undeliveredMessageNamespace, key).String()
}

// postUndeliveredNotes posts one message for each window of events recorded undelivered to a role
// that has ended (undeliveredNoteWindow), on the window's issue, naming every event of the window
// (undeliveredMessage), and marks the window's notes posted with the message's id. The message is
// written by the outbox's own system actor through the issue message route's own writer
// (Deps.PostIssueMessage), so it wakes no route and reaches no role, which is the very thing that
// did not take the events: a person reads it on the issue. It runs with the expiry pass
// (passKeptExpiries), under the publisher hold, and reports full when it read a whole batch of
// windows and failed when its read failed.
//
// A window's state is all in outbox_role_undelivered_notes, so a window a process opened is posted
// by whichever process holds the hold once it has ended, a process that restarted meanwhile
// included. Each window's message has an id of its own (undeliveredWindow.messageID), which the
// writer stores once, so a post made again - by a process that died between the post and its mark,
// or that lost the hold in between - finds the message the first post wrote, and the window is
// marked posted with it. A post that failed counts an attempt on each of the window's notes and
// waits retry.Delay of that count before the window is read again. The windows are read, and each
// mark written, only while the hold is this process's; a Deps with no poster posts nothing and
// leaves every window unposted.
func postUndeliveredNotes(ctx context.Context, deps Deps, hold fence, instance uuid.UUID) (full, failed bool) {
	if deps.PostIssueMessage == nil || ctx.Err() != nil || !time.Now().Before(hold.Deadline()) {
		return false, false
	}
	windows, err := dueUndeliveredWindows(ctx, deps, instance)
	if err != nil {
		if !lostToContextEnd(ctx, err) {
			slog.Error("dispatch outbox: read the windows of undelivered events due a message", "error", err)
		}
		return false, true
	}
	for _, window := range windows {
		if ctx.Err() != nil || !time.Now().Before(hold.Deadline()) {
			return false, false
		}
		messageID, err := deps.PostIssueMessage(ctx, graphWriter, model.SystemMessage{
			ID: window.messageID(), IssueKey: window.issueKey, Body: undeliveredMessage(window),
		})
		if err != nil {
			if lostToContextEnd(ctx, err) {
				return false, false
			}
			slog.Error("dispatch outbox: post the message about events undelivered to a role",
				"issue_key", window.issueKey, "role", window.role, "events", len(window.notes), "error", err)
			if err := backOffUndeliveredWindow(ctx, deps, instance, window, retry.Delay(window.failures+1)); err != nil {
				slog.Error("dispatch outbox: back off the message about events undelivered to a role",
					"issue_key", window.issueKey, "role", window.role, "error", err)
			}
			continue
		}
		if err := markUndeliveredWindowPosted(ctx, deps, instance, window, messageID); err != nil {
			slog.Error("dispatch outbox: mark the events undelivered to a role posted",
				"issue_key", window.issueKey, "role", window.role, "message_id", messageID, "error", err)
			continue
		}
		slog.Info("dispatch outbox: posted the message about events undelivered to a role",
			"issue_key", window.issueKey, "role", window.role, "events", len(window.notes), "message_id", messageID)
	}
	return len(windows) == batchSize, false
}

// dueUndeliveredWindows reads the windows due a message (undeliveredWindowsQuery), each with its
// notes in event order, and none while instance does not hold the publisher hold.
func dueUndeliveredWindows(ctx context.Context, deps Deps, instance uuid.UUID) ([]undeliveredWindow, error) {
	rows, err := deps.Store.Pool.Query(ctx, undeliveredWindowsQuery, undeliveredNoteWindow, batchSize, publisherHold, instance)
	if err != nil {
		return nil, err
	}
	var windows []undeliveredWindow
	var issueKey, role string
	var openedAt time.Time
	var note undeliveredNote
	var failures int
	if _, err := pgx.ForEachRow(rows, []any{&issueKey, &role, &openedAt, &note.eventID, &note.eventType, &note.keeps,
		&note.reason, &note.itemRef, &note.documentID, &failures}, func() error {
		last := len(windows) - 1
		if last < 0 || windows[last].issueKey != issueKey || windows[last].role != role || !windows[last].openedAt.Equal(openedAt) {
			windows = append(windows, undeliveredWindow{issueKey: issueKey, role: role, openedAt: openedAt})
			last++
		}
		windows[last].notes = append(windows[last].notes, note)
		windows[last].failures = max(windows[last].failures, failures)
		return nil
	}); err != nil {
		return nil, err
	}
	return windows, nil
}

// markUndeliveredWindowPosted marks each of window's notes posted with messageID, the id of the
// window's message, while instance holds the publisher hold.
func markUndeliveredWindowPosted(ctx context.Context, deps Deps, instance uuid.UUID, window undeliveredWindow, messageID string) error {
	_, err := deps.Store.Pool.Exec(ctx, `
		update outbox_role_undelivered_notes set posted_at = now(), message_id = $3::uuid, next_attempt_at = null
		where event_id = any($1) and role = $2 and posted_at is null
		  and exists (select 1 from duty_holds where name = $5 and holder = $4 and expires_at > now())
	`, window.eventIDs(), window.role, messageID, instance, publisherHold)
	return err
}

// backOffUndeliveredWindow counts one more failed post on each of window's notes and has the window
// wait retryAfter before it is read again, while instance holds the publisher hold.
func backOffUndeliveredWindow(ctx context.Context, deps Deps, instance uuid.UUID, window undeliveredWindow, retryAfter time.Duration) error {
	_, err := deps.Store.Pool.Exec(ctx, `
		update outbox_role_undelivered_notes set attempt_count = attempt_count + 1, next_attempt_at = now() + $3::interval
		where event_id = any($1) and role = $2 and posted_at is null
		  and exists (select 1 from duty_holds where name = $5 and holder = $4 and expires_at > now())
	`, window.eventIDs(), window.role, retryAfter, instance, publisherHold)
	return err
}

// eventIDs is the id of each event window's notes name, in note order.
func (w undeliveredWindow) eventIDs() []int64 {
	ids := make([]int64, 0, len(w.notes))
	for _, note := range w.notes {
		ids = append(ids, note.eventID)
	}
	return ids
}

// A window's events are named in lists of at most undeliveredMessageListItems events and
// undeliveredMessageListUnits UTF-16 units. The issue message writer refuses a body with a block
// over contracts.DispatchBlockUnits (32,000 units; itemblocks.Check), and a Markdown list is one
// block however many items it holds, so the events of a long window go in several lists, each led
// by a paragraph that ends the list before it. An item names one reference, and a document's can
// run to a few thousand units (its slug is as long as its name), so a list is bounded by its
// length as well as by its count.
//
// A reference over undeliveredReferenceUnits is not written whole: a document kept from before the
// creation bound on names (api/artifacts.go keeps its longer key) can have a slug of any length,
// and one such item alone would pass the block bound and keep its message from ever being posted.
// The item names that document by its id instead, which GET /api/v1/artifacts/{id} reads, and
// says how long the reference is. The cap is above every reference a document made today has, so
// those are still linked, and an item stays far under the list budget, which stays far under the
// block bound.
const (
	undeliveredMessageListItems = 50
	undeliveredMessageListUnits = contracts.DispatchBlockUnits / 2
	undeliveredReferenceUnits   = 4096
)

// undeliveredMessage is the body of window's message: the role, how many events of this issue it
// did not take in the window, each event by its id and type with the post or document it is about,
// how many times it was kept and why it ended, and what a person does about it. A window whose
// events do not fit one list names them in several (undeliveredMessageListItems,
// undeliveredMessageListUnits), each led by a line "Events <first> to <last> of <count>:", so every
// event is named in the one message and no block of it is over the writer's bound.
func undeliveredMessage(window undeliveredWindow) string {
	var lists [][]string
	units := 0
	for _, note := range window.notes {
		line := undeliveredNoteLine(note)
		size := itemblocks.Units16(line)
		if last := len(lists) - 1; last < 0 || len(lists[last]) == undeliveredMessageListItems || units+size > undeliveredMessageListUnits {
			lists, units = append(lists, nil), 0
		}
		lists[len(lists)-1] = append(lists[len(lists)-1], line)
		units += size
	}
	var body strings.Builder
	count := "1 event"
	if len(window.notes) != 1 {
		count = fmt.Sprintf("%d events", len(window.notes))
	}
	fmt.Fprintf(&body, "Dispatch did not deliver %s to role `%s`. Dispatch kept each event for the role for an hour at a time, and no session took the role:\n",
		count, window.role)
	first := 1
	for _, list := range lists {
		if len(lists) > 1 {
			fmt.Fprintf(&body, "\nEvents %d to %d of %d:\n", first, first+len(list)-1, len(window.notes))
		}
		body.WriteString("\n")
		for _, line := range list {
			body.WriteString(line)
		}
		first += len(list)
	}
	fmt.Fprintf(&body, "\nEach event has a role.undelivered record in this issue's history. To deliver one, find the session that should hold role `%s`, then send it the work again.",
		window.role)
	return body.String()
}

// undeliveredNoteLine is note as one list item of a window's message, with its line end. A
// reference over undeliveredReferenceUnits is named by its document's id and its length, or, for
// a reference that names no document, by its length alone.
func undeliveredNoteLine(note undeliveredNote) string {
	var line strings.Builder
	fmt.Fprintf(&line, "- event %d (%s)", note.eventID, note.eventType)
	long := itemblocks.Units16(note.itemRef) > undeliveredReferenceUnits
	switch {
	case long && note.documentID != "":
		fmt.Fprintf(&line, ", on the document with id %s (its reference is %d characters long, too long to write here)",
			note.documentID, len([]rune(note.itemRef)))
	case long:
		fmt.Fprintf(&line, " (its reference is %d characters long, too long to write here)", len([]rune(note.itemRef)))
	case note.itemRef != "" && note.documentID != "":
		fmt.Fprintf(&line, ", on the document %s", note.itemRef)
	case note.itemRef != "":
		fmt.Fprintf(&line, ", %s", note.itemRef)
	}
	keeps := fmt.Sprintf("%d times", note.keeps)
	if note.keeps == 1 {
		keeps = "1 time"
	}
	if note.reason == model.RoleUndeliveredCopyGone {
		fmt.Fprintf(&line, ": kept %s, and Envoy no longer held the copy to keep it again\n", keeps)
	} else {
		fmt.Fprintf(&line, ": kept %s, and no session took it\n", keeps)
	}
	return line.String()
}
