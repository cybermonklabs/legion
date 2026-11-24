package outbox

import (
	"context"
	"fmt"
	"log/slog"
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
// at most, each with its notes in event order.
const undeliveredWindowsQuery = `
	with due as (
		select issue_key, role, window_opened_at
		from outbox_role_undelivered_notes
		where posted_at is null and window_opened_at <= now() - $1::interval
		  and (next_attempt_at is null or next_attempt_at <= now())
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
// included. A post that failed counts an attempt on each of the window's notes and waits
// retry.Delay of that count before the window is read again. The post is made before the mark, and
// the mark is written only while the hold is this process's: a process that dies, or loses the
// hold, between the two leaves the window unposted, and the holder's next pass posts it again,
// which is one message twice rather than none. A Deps with no poster posts nothing and leaves every
// window unposted.
func postUndeliveredNotes(ctx context.Context, deps Deps, hold fence, instance uuid.UUID) (full, failed bool) {
	if deps.PostIssueMessage == nil || ctx.Err() != nil || !time.Now().Before(hold.Deadline()) {
		return false, false
	}
	windows, err := dueUndeliveredWindows(ctx, deps)
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
		messageID, err := deps.PostIssueMessage(ctx, graphWriter, window.issueKey, undeliveredMessage(window))
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
// notes in event order.
func dueUndeliveredWindows(ctx context.Context, deps Deps) ([]undeliveredWindow, error) {
	rows, err := deps.Store.Pool.Query(ctx, undeliveredWindowsQuery, undeliveredNoteWindow, batchSize)
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

// undeliveredMessage is the body of window's message: the role, how many events of this issue it
// did not take in the window, each event by its id and type with the post or document it is about,
// how many times it was kept and why it ended, and what a person does about it.
func undeliveredMessage(window undeliveredWindow) string {
	var body strings.Builder
	count := "1 event"
	if len(window.notes) != 1 {
		count = fmt.Sprintf("%d events", len(window.notes))
	}
	fmt.Fprintf(&body, "Dispatch did not deliver %s to role `%s`. Dispatch kept each event for the role for an hour at a time, and no session took the role:\n\n",
		count, window.role)
	for _, note := range window.notes {
		fmt.Fprintf(&body, "- event %d (%s)", note.eventID, note.eventType)
		switch {
		case note.itemRef != "" && note.document:
			fmt.Fprintf(&body, ", on the document %s", note.itemRef)
		case note.itemRef != "":
			fmt.Fprintf(&body, ", %s", note.itemRef)
		}
		keeps := fmt.Sprintf("%d times", note.keeps)
		if note.keeps == 1 {
			keeps = "1 time"
		}
		if note.reason == model.RoleUndeliveredCopyGone {
			fmt.Fprintf(&body, ": kept %s, and Envoy no longer held the copy to keep it again\n", keeps)
		} else {
			fmt.Fprintf(&body, ": kept %s, and no session took it\n", keeps)
		}
	}
	fmt.Fprintf(&body, "\nEach event has a role.undelivered record in this issue's history. To deliver one, find the session that should hold role `%s`, then send it the work again.",
		window.role)
	return body.String()
}
