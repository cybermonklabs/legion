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

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
)

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
	document  bool
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
	       coalesce(n.item_ref, ''), n.artifact_id is not null, n.attempt_count
	from due d
	join outbox_role_undelivered_notes n
	  on n.issue_key = d.issue_key and n.role = d.role and n.window_opened_at = d.window_opened_at
	where n.posted_at is null
	order by n.window_opened_at, n.issue_key, n.role, n.event_id
`

// undeliveredMessageNamespace is the namespace a window's message id is derived in
// (undeliveredWindow.messageID).
var undeliveredMessageNamespace = uuid.MustParse("6d3f4b2e-9a0c-5e1f-8b7a-2c4d6e8f0a1b")

// messageID is the id of w's message: derived from its issue, its role and the moment it opened, so
// every post of that window, by any process and after any restart, names the one message and the
// issue message writer stores it once (api.Background.PostSystemMessage).
func (w undeliveredWindow) messageID() string {
	return uuid.NewSHA1(undeliveredMessageNamespace,
		[]byte(w.issueKey+"\x00"+w.role+"\x00"+strconv.FormatInt(w.openedAt.UnixMicro(), 10))).String()
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
		messageID, err := deps.PostIssueMessage(ctx, graphWriter, window.messageID(), window.issueKey, undeliveredMessage(window))
		if err != nil {
			if lostToContextEnd(ctx, err) {
				return false, false
			}
			slog.Error("dispatch outbox: post the message about events undelivered to a role",
				"issue_key", window.issueKey, "role", window.role, "events", len(window.notes), "error", err)
			if err := markUndeliveredWindow(ctx, deps, instance, window, "", retry.Delay(window.failures+1)); err != nil {
				slog.Error("dispatch outbox: back off the message about events undelivered to a role",
					"issue_key", window.issueKey, "role", window.role, "error", err)
			}
			continue
		}
		if err := markUndeliveredWindow(ctx, deps, instance, window, messageID, 0); err != nil {
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
		&note.reason, &note.itemRef, &note.document, &failures}, func() error {
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

// markUndeliveredWindow writes what the post of window's message came to on each of its notes,
// while instance holds the publisher hold: posted with messageID, or, for an empty messageID, one
// more failed attempt and a wait of retryAfter.
func markUndeliveredWindow(ctx context.Context, deps Deps, instance uuid.UUID, window undeliveredWindow, messageID string, retryAfter time.Duration) error {
	ids := make([]int64, 0, len(window.notes))
	for _, note := range window.notes {
		ids = append(ids, note.eventID)
	}
	_, err := deps.Store.Pool.Exec(ctx, `
		update outbox_role_undelivered_notes set
			posted_at = case when $3 <> '' then now() end,
			message_id = nullif($3, '')::uuid,
			attempt_count = attempt_count + case when $3 = '' then 1 else 0 end,
			next_attempt_at = case when $3 = '' then now() + $4::interval end
		where event_id = any($1) and role = $2 and posted_at is null
		  and exists (select 1 from duty_holds where name = $6 and holder = $5 and expires_at > now())
	`, ids, window.role, messageID, retryAfter, instance, publisherHold)
	return err
}

// undeliveredMessageListItems is how many events one list of a window's message names. The issue
// message writer refuses a body with a block over contracts.DispatchBlockUnits (32,000 UTF-16
// units; itemblocks.Check), and a Markdown list is one block however many items it holds, so the
// events of a long window go in lists of this many, each led by a paragraph that ends the list
// before it. An item is at most a few hundred units - an event id, its type, one reference and a
// line about its keeps - so a list of 50 stays far under the bound, and the message's block count
// (two blocks for each 50 events, and three more) stays under contracts.DispatchItemBlocks for any
// window shorter than about 800,000 events.
const undeliveredMessageListItems = 50

// undeliveredMessage is the body of window's message: the role, how many events of this issue it
// did not take in the window, each event by its id and type with the post or document it is about,
// how many times it was kept and why it ended, and what a person does about it. A window of more
// than undeliveredMessageListItems events names them in lists of that many, each led by a line
// "Events <first> to <last> of <count>:", so every event is named in the one message and no block
// of it is over the writer's bound.
func undeliveredMessage(window undeliveredWindow) string {
	var body strings.Builder
	count := "1 event"
	if len(window.notes) != 1 {
		count = fmt.Sprintf("%d events", len(window.notes))
	}
	fmt.Fprintf(&body, "Dispatch did not deliver %s to role `%s`. Dispatch kept each event for the role for an hour at a time, and no session took the role:\n",
		count, window.role)
	for start := 0; start < len(window.notes); start += undeliveredMessageListItems {
		end := min(start+undeliveredMessageListItems, len(window.notes))
		if len(window.notes) > undeliveredMessageListItems {
			fmt.Fprintf(&body, "\nEvents %d to %d of %d:\n", start+1, end, len(window.notes))
		}
		body.WriteString("\n")
		for _, note := range window.notes[start:end] {
			writeUndeliveredNote(&body, note)
		}
	}
	fmt.Fprintf(&body, "\nEach event has a role.undelivered record in this issue's history. To deliver one, find the session that should hold role `%s`, then send it the work again.",
		window.role)
	return body.String()
}

// writeUndeliveredNote writes note as one list item of a window's message.
func writeUndeliveredNote(body *strings.Builder, note undeliveredNote) {
	fmt.Fprintf(body, "- event %d (%s)", note.eventID, note.eventType)
	switch {
	case note.itemRef != "" && note.document:
		fmt.Fprintf(body, ", on the document %s", note.itemRef)
	case note.itemRef != "":
		fmt.Fprintf(body, ", %s", note.itemRef)
	}
	keeps := fmt.Sprintf("%d times", note.keeps)
	if note.keeps == 1 {
		keeps = "1 time"
	}
	if note.reason == model.RoleUndeliveredCopyGone {
		fmt.Fprintf(body, ": kept %s, and Envoy no longer held the copy to keep it again\n", keeps)
	} else {
		fmt.Fprintf(body, ": kept %s, and no session took it\n", keeps)
	}
}
