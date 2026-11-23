package events

import (
	"context"
	"time"
)

// EventCursorCheck reads the newest event's id (0 when the log is empty) and the created_at of the
// event at cursor (nil when no row holds it): a reader of the log by a global id cursor compares
// them with what it read for its cursor to tell an event log that was reset (EventLogWasReset).
// Its readers are the event stream relay (api's resetCursor), an events subscription (api's
// feedEvents, through eventSubscriptionCheck, which reads the same two in a statement of its own)
// and the keyword fill (textversions), each with its own cursor.
//
// Both come from one statement, so they describe one log. A `truncate events restart identity`
// (e2e/seed.ts) takes an ACCESS EXCLUSIVE lock, and a statement holds its ACCESS SHARE lock on
// events until it ends, so the reset lands wholly before or wholly after this read. Read as two
// statements, a reset committing between them answered with the old log's newest id and no row at
// the cursor: the relay then took the old log's head for the new log's and ended the generation
// opened after the reset as a second reset.
func EventCursorCheck(ctx context.Context, q Queryer, cursor int64) (maxID int64, cursorAt *time.Time, err error) {
	if err := q.QueryRow(ctx, `
		select coalesce((select max(id) from events), 0), (select created_at from events where id = $1)
	`, cursor).Scan(&maxID, &cursorAt); err != nil {
		return 0, nil, err
	}
	return maxID, cursorAt, nil
}

// EventLogWasReset is whether the event log was reset under a reader's cursor, from what
// EventCursorCheck read for it (maxID, cursorAt) and what the reader read when it moved there (at):
// a log whose newest id is below the cursor, or whose row at the cursor is gone or carries another
// created_at — a `truncate events restart identity` refilled past it, which is what e2e/seed.ts
// does between browser scenarios. The cursor is always an existing row's id, or 0 before the
// first, so until a reset the row at it carries the created_at the reader read.
func EventLogWasReset(cursor int64, at time.Time, maxID int64, cursorAt *time.Time) bool {
	return maxID < cursor || (cursor > 0 && (cursorAt == nil || !cursorAt.Equal(at)))
}
