package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// PendingMergeRead is a request for new work that waits for its merge read: the standing
// merge_read row of its issue (MergeRead) with the row's outbox id.
type PendingMergeRead struct {
	ID   int64
	Read MergeRead
}

// PendingMergeRead is issue's standing merge_read row, locked until the transaction ends, and nil
// where none stands. One stands for an issue at a time (QueueMergeRead).
func (s *Postgres) PendingMergeRead(ctx context.Context, tx pgx.Tx, issue string) (*PendingMergeRead, error) {
	var id int64
	var payload []byte
	err := tx.QueryRow(ctx, `select id, payload from outbox where issue = $1 and kind = $2 order by id limit 1 for update`,
		issue, string(OutboxKindMergeRead)).Scan(&id, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the merge read of %s: %w", issue, err)
	}
	var read MergeRead
	if err := json.Unmarshal(payload, &read); err != nil {
		return nil, fmt.Errorf("decode the merge read of %s: %w", issue, err)
	}
	return &PendingMergeRead{ID: id, Read: read}, nil
}

// QueueMergeRead queues read as its issue's one merge_read row, due at now, where none stands, and
// answers whether it queued one. A row that stands is left as it is: a later request reaches it
// through RequestMergeRead alone.
func (s *Postgres) QueueMergeRead(ctx context.Context, tx pgx.Tx, read MergeRead, now time.Time) (bool, error) {
	pending, err := s.PendingMergeRead(ctx, tx, read.Issue)
	if err != nil || pending != nil {
		return false, err
	}
	row, err := NewOutboxRow(read.Issue, read, now)
	if err != nil {
		return false, fmt.Errorf("queue the merge read of %s: %w", read.Issue, err)
	}
	if err := s.Enqueue(ctx, tx, row); err != nil {
		return false, err
	}
	return true, nil
}

// RequestMergeRead writes requester and request over the asker and the text of issue's standing
// merge_read row, and answers whether one stood. It writes those two fields alone, so the
// executor's saved progress stays as the executor left it, and the apply of the read's fact reads
// who asked from the row as it stands then. Its caller judges that the later request is a person's.
func (s *Postgres) RequestMergeRead(ctx context.Context, tx pgx.Tx, issue string, requester claim.Role, request string) (bool, error) {
	tag, err := tx.Exec(ctx, `update outbox set payload = payload || jsonb_build_object('requester', $3::text, 'request', $4::text)
		where issue = $1 and kind = $2`, issue, string(OutboxKindMergeRead), string(requester), request)
	if err != nil {
		return false, fmt.Errorf("write the later request of %s over its merge read: %w", issue, err)
	}
	return tag.RowsAffected() > 0, nil
}

// RefreshMergeRead asks issue's standing merge_read row for one more read of the issue's links
// (MergeReadProgress.Refreshes, counted up by one) and makes it due at now, and answers whether one
// stood. A new observation of the issue is what asks it: a link may have come with it. Each ask is
// counted, so one made while a links read runs outlives that read's save.
func (s *Postgres) RefreshMergeRead(ctx context.Context, tx pgx.Tx, issue string, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `update outbox set payload = jsonb_set(payload, '{progress}', coalesce(payload->'progress', '{}'::jsonb)
			|| jsonb_build_object('refreshes', coalesce((payload->'progress'->>'refreshes')::int, 0) + 1)),
		next_at = least(next_at, $3) where issue = $1 and kind = $2`, issue, string(OutboxKindMergeRead), now)
	if err != nil {
		return false, fmt.Errorf("ask the merge read of %s to read its links again: %w", issue, err)
	}
	return tag.RowsAffected() > 0, nil
}

// CancelMergeRead deletes issue's standing merge_read row, and answers whether one stood: the
// request it waited for is over.
func (s *Postgres) CancelMergeRead(ctx context.Context, tx pgx.Tx, issue string) (bool, error) {
	tag, err := tx.Exec(ctx, `delete from outbox where issue = $1 and kind = $2`, issue, string(OutboxKindMergeRead))
	if err != nil {
		return false, fmt.Errorf("cancel the merge read of %s: %w", issue, err)
	}
	return tag.RowsAffected() > 0, nil
}

// UnreadMergeReads is each standing merge_read row of project whose read ended with no answering
// candidate or stopped after its third failed attempt (MergeReadStateUnread, MergeReadStateStopped),
// by issue.
func (s *Postgres) UnreadMergeReads(ctx context.Context, tx pgx.Tx, project string) ([]PendingMergeRead, error) {
	rows, err := tx.Query(ctx, `select id, payload from outbox where kind = $1 and split_part(issue, '-', 1) = $2
		and payload->'progress'->>'state' = any($3) order by issue, id`,
		string(OutboxKindMergeRead), project, []string{MergeReadStateUnread, MergeReadStateStopped})
	if err != nil {
		return nil, fmt.Errorf("read the unread merge reads of %s: %w", project, err)
	}
	defer rows.Close()
	var reads []PendingMergeRead
	for rows.Next() {
		var pending PendingMergeRead
		var payload []byte
		if err := rows.Scan(&pending.ID, &payload); err != nil {
			return nil, fmt.Errorf("read the unread merge reads of %s: %w", project, err)
		}
		if err := json.Unmarshal(payload, &pending.Read); err != nil {
			return nil, fmt.Errorf("decode merge read row %d: %w", pending.ID, err)
		}
		reads = append(reads, pending)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the unread merge reads of %s: %w", project, err)
	}
	return reads, nil
}

// MarkMergeUnreadTold records at as the moment the controller was last told of merge_read row
// id's empty read (MergeRead.ToldAt), writing that one field.
func (s *Postgres) MarkMergeUnreadTold(ctx context.Context, tx pgx.Tx, id int64, at time.Time) error {
	if _, err := tx.Exec(ctx, `update outbox set payload = jsonb_set(payload, '{toldAt}', to_jsonb($2::timestamptz)) where id = $1`, id, at.UTC()); err != nil {
		return fmt.Errorf("mark the controller told of merge read row %d: %w", id, err)
	}
	return nil
}
