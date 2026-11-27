package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mergeReadLock is the merge read slot's session advisory lock, by the name D17 gives it.
const mergeReadLock = "select %s(hashtext('legion.merge_read'))"

// MergeReadSlot is the one database-wide slot of the merge read: a session advisory lock named
// legion.merge_read, held on a connection of its own outside any transaction, so that at most one
// request of any project's merge read to GitHub or Dispatch stands at a time. A reader takes it
// for one request (TakeMergeReadSlot), reserves that request's attempt on its connection
// (Transact), sends the request, and gives it back (Release) before it applies the answer.
//
// The connection counts as one the caller holds (acquire), so the guard refuses the caller a
// second pooled connection while it holds the slot: a reader reads what it needs before it takes
// the slot, and writes the answer after it gave the slot back.
type MergeReadSlot struct {
	conn    *pgxpool.Conn
	release func()
}

// TakeMergeReadSlot takes the merge read's slot where no other holder has it
// (pg_try_advisory_lock), and answers false, holding nothing, where one does: it never waits, so
// a busy slot spends no request and no lease time of the caller's.
func (p *Pool) TakeMergeReadSlot(ctx context.Context) (*MergeReadSlot, bool, error) {
	conn, release, err := p.acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var taken bool
	if err := conn.QueryRow(ctx, fmt.Sprintf(mergeReadLock, "pg_try_advisory_lock")).Scan(&taken); err != nil {
		slot := &MergeReadSlot{conn: conn, release: release}
		slot.close(ctx)
		return nil, false, fmt.Errorf("take the merge read's slot: %w", err)
	}
	if !taken {
		conn.Release()
		release()
		return nil, false, nil
	}
	return &MergeReadSlot{conn: conn, release: release}, true, nil
}

// Transact runs write in one transaction on the slot's connection, which commits before the
// request it reserves is sent: the reservation of the request's attempt (CheckpointMergeRead), so
// a crash after the send cannot give a spent attempt back.
func (s *MergeReadSlot) Transact(ctx context.Context, write func(ctx context.Context, tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.conn, func(tx pgx.Tx) error { return write(ctx, tx) })
}

// Release gives the slot back once the request it held has ended. A holder whose ctx ended — a
// cancelled turn, a stopping daemon — closes the slot's connection, which ends its session and the
// lock with it, as an unlock that could not run would leave the lock held. Otherwise it unlocks and
// returns the connection to the pool, and closes it where the unlock fails.
func (s *MergeReadSlot) Release(ctx context.Context) {
	if ctx.Err() != nil || s.conn.Conn().IsClosed() {
		s.close(ctx)
		return
	}
	var unlocked bool
	if err := s.conn.QueryRow(ctx, fmt.Sprintf(mergeReadLock, "pg_advisory_unlock")).Scan(&unlocked); err != nil || !unlocked {
		slog.Warn("store: the merge read's slot did not unlock, so its connection is closed, which gives the lock back",
			"unlocked", unlocked, "error", err)
		s.close(ctx)
		return
	}
	s.conn.Release()
	s.release()
}

// close closes the slot's connection, on a context ctx's end does not cancel, bounded by
// sessionResetBound, and removes it from the pool: the server ends the session, and the lock with
// it.
func (s *MergeReadSlot) close(ctx context.Context) {
	closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionResetBound)
	defer cancel()
	_ = s.conn.Hijack().Close(closing)
	s.release()
}

// CheckpointMergeRead writes progress, a merge read's saved progress (the record's
// MergeReadProgress, as JSON), over the progress of the merge_read outbox row id, only while lease
// still holds the row, and answers whether it did. It writes that one field, so who asked and what
// they asked, which a door may write over while the read runs, stay as the door left them. A door
// may also ask one links refresh meanwhile (the progress's refresh): a refresh the stored row asks
// for stays asked unless consumedRefresh says this turn made it.
func CheckpointMergeRead(ctx context.Context, tx pgx.Tx, id int64, lease string, progress json.RawMessage, consumedRefresh bool) (bool, error) {
	tag, err := tx.Exec(ctx, `update outbox set payload = jsonb_set(payload, '{progress}',
			($3::jsonb - 'refresh') || case when not $4 and coalesce((payload->'progress'->>'refresh')::boolean, false)
				then '{"refresh": true}'::jsonb else '{}'::jsonb end)
		where id = $1 and lease_token = $2 and kind = 'merge_read'`, id, lease, string(progress), consumedRefresh)
	if err != nil {
		return false, fmt.Errorf("save the progress of merge read row %d: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}
