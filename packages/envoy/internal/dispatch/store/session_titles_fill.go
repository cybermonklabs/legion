package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// SessionTitlesFillBatch is how many sessions one transaction of FillSessionTitles takes.
const SessionTitlesFillBatch = 1000

// fillSessionTitlesCursorQuery reads the cursor the next batch starts from, and no row once the
// pass has finished (20261113090413). It is a statement of its own so that the batch below is
// planned with the cursor bound as a parameter, which the planner keys the index scan on
// (store.fillEmbeddingProjectKeysCursorQuery says what a cursor the planner cannot see cost
// that fill). The row is read `for update`, which is what makes two processes filling at once
// take different batches rather than the same one: the second waits for the first's batch to
// commit and then reads the cursor it moved, or no row.
const fillSessionTitlesCursorQuery = `select session_id from session_titles_backfill for update`

// fillSessionTitlesBatchQuery is one batch: it takes the next $2 sessions above the cursor $1,
// writes each one's newest stamped title into session_titles, and answers the rows it wrote and
// the highest session it took, null when it took none.
//
// `batch` is the keyset: the next distinct session ids above the cursor, in the order
// events_session_actor_created_at_idx (migration 0026) keys them, so the scan starts where the
// last batch stopped instead of counting past it as an `offset` would. It takes every session,
// one that never stamped a title included, because only a read of its events can tell.
//
// `latest` is the rule ResolveSessionTitles read from events before 20261113082156 (its
// statement is held in delivery/session_titles_plan_test.go): each session's newest event whose
// origin stamps a non-empty title, one ordered read a session of the same index, and no row for
// a session that never stamped one. `e.id` breaks the tie 20261113090412's trigger breaks, two
// titled events at one created_at: the first one inserted keeps the title.
//
// `filled` writes under that trigger's own guard, `where excluded.titled_at >
// session_titles.titled_at`, so a row the trigger wrote while the pass runs, which names a newer
// event than any the pass reads, is never replaced by an older one. The guard is also what makes
// a batch idempotent: a session whose row already names the same event is not written again, so
// the pass is safe to run twice and from two processes at once.
//
// The cursor comes from `batch` and never from `filled`'s `returning`: a session whose row the
// guard skips, or one that stamped no title, returns nothing, and a cursor taken from the write
// would stall on it.
const fillSessionTitlesBatchQuery = `
	with batch as (
		select distinct e.actor->>'id' as session_id
		  from events e
		 where e.actor->>'kind' = 'session' and e.actor->>'id' > $1
		 order by 1
		 limit $2
	), latest as (
		select b.session_id, t.title, t.created_at
		  from batch b
		 cross join lateral (
			select e.actor->'origin'->>'session_title' as title, e.created_at
			  from events e
			 where e.actor->>'kind' = 'session' and e.actor->>'id' = b.session_id
			   and nullif(e.actor->'origin'->>'session_title', '') is not null
			 order by e.created_at desc, e.id
			 limit 1
		 ) t
	), filled as (
		insert into session_titles (session_id, title, titled_at)
		select session_id, title, created_at from latest
		on conflict (session_id) do update
		   set title = excluded.title, titled_at = excluded.titled_at
		 where excluded.titled_at > session_titles.titled_at
		returning 1
	)
	select (select count(*) from filled), (select max(session_id) from batch)
`

// FillSessionTitles fills 20261113082156's session_titles for the sessions whose titled events
// were stored before 20261113090412's trigger went live, and answers how many rows it wrote.
// The boot runs it after the migrations and before the server serves (cmd/dispatch/main.go):
// delivery.ResolveSessionTitles reads session_titles alone, so a process serving with the table
// unfilled would name those sessions by their ids.
//
// A pass already finished costs one read of an empty table: its last batch deleted the row
// 20261113090413 creates. Otherwise it loops, each iteration one short transaction that takes
// the next batch of sessions by keyset, writes their titles and commits, so no row it writes
// stays uncommitted for longer than its own batch and an event write that meets one of its keys
// at its commit waits for that batch alone. 20261113090413's comment says why it is not a
// migration.
func (s *Store) FillSessionTitles(ctx context.Context) (int64, error) {
	var written, batches int64
	for {
		rows, finished, err := s.fillSessionTitlesBatch(ctx)
		if err != nil {
			return written, err
		}
		written += rows
		if finished {
			if batches > 0 {
				slog.Info("dispatch: session_titles fill finished", "rows", written, "batches", batches)
			}
			return written, nil
		}
		batches++
		slog.Info("dispatch: session_titles fill committed a batch", "rows", rows, "written", written, "batches", batches)
	}
}

// fillSessionTitlesBatch runs one batch in a transaction of its own and commits it, answering
// the rows it wrote and whether the pass is finished: the cursor's row was already gone, or this
// batch took no further session and deleted it.
func (s *Store) fillSessionTitlesBatch(ctx context.Context) (int64, bool, error) {
	// A batch is a transaction like any other, so it is marked like any other: nothing it runs
	// may take a second pooled connection while it is open.
	ctx = WithTransactionTracking(ctx)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin a session_titles fill batch: %w", err)
	}
	defer tx.Rollback(ctx)
	var cursor string
	if err := tx.QueryRow(ctx, fillSessionTitlesCursorQuery).Scan(&cursor); errors.Is(err, pgx.ErrNoRows) {
		return 0, true, nil
	} else if err != nil {
		return 0, false, fmt.Errorf("read the session_titles fill's progress: %w", err)
	}
	var written int64
	var last *string
	if err := tx.QueryRow(ctx, fillSessionTitlesBatchQuery, cursor, SessionTitlesFillBatch).Scan(&written, &last); err != nil {
		return 0, false, fmt.Errorf("fill a batch of session_titles: %w", err)
	}
	if last == nil {
		if _, err := tx.Exec(ctx, `delete from session_titles_backfill`); err != nil {
			return 0, false, fmt.Errorf("finish the session_titles fill: %w", err)
		}
	} else if _, err := tx.Exec(ctx, `update session_titles_backfill set session_id = $1`, *last); err != nil {
		return 0, false, fmt.Errorf("move the session_titles fill's cursor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit a session_titles fill batch: %w", err)
	}
	return written, last == nil, nil
}
