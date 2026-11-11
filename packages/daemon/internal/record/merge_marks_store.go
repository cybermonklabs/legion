package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// mergeJumpColumns is the jump mark's columns in the order scanMergeJump reads them, so the two
// reads of the table (one issue's mark, a project's marks) cannot drift apart.
const mergeJumpColumns = `issue, reason, session, set_at`

// mergeHoldColumns is the hold's columns in the order scanMergeHold reads them, which both reads
// of the hold list share (MergeHolds and LiveMergeHolds).
const mergeHoldColumns = `paths, repository, reason, ends_at, session, set_at`

// mergeJumpRecordColumns is the jump record's columns in the order scanMergeJumpRecord reads them.
const mergeJumpRecordColumns = `repository, issue, number, head, at, reason, entries, entries_total, entries_unknown, outcome, answer`

// mergeHeldPayload is the SQL that says a ready_merge row's payload carries an open held
// stretch (ReadyMerge.HeldSince), which is what makes the row one a merge hold caught: a run
// that finds a hold writes the moment, and the first run that finds none clears it again
// (daemon's readyMergeMarks.tellHeld and the close above the submission's arms). Both reads of
// the rows a hold is keeping back share it (MergeHeldIssues and HeldReadyMerges), so the two
// cannot drift apart, as mergeJumpColumns keeps the two reads of the jump marks together.
const mergeHeldPayload = `payload ? 'heldSince' and payload->>'heldSince' <> ''`

// SetMergeJump writes issue's jump mark, and lands only while the issue's own record is still
// one the daemon runs (Stopped). It answers whether it wrote, as SetMergeJumpOutcome answers
// whether its own conditional update landed.
//
// The condition is read in this very transaction and under the issue row's lock, which is what
// makes it a condition rather than a second guess. A departure clears the mark it finds in the
// transaction that records the departure (workflow's leave), so a mark written between that
// transaction's read and its commit would outlive the departure: it would stand in the record
// until a re-admission brought the issue back, and be spent by whatever merge that run reached,
// against a decision nobody made about that merge. The route's own check of the issue runs
// before any of this and still stands — it answers a controller without writing anything — but
// it reads in a transaction of its own, so the window between it and this write is exactly what
// the lock closes: either this read waits for the departure and sees it, or the departure waits
// for this write and clears the mark it just wrote.
//
// An issue with no record at all answers false too: its record was deleted mid-request, by a
// tree's cleanup, which takes any mark with it (migration 20261110235352's `on delete cascade`)
// and leaves no issue for a mark to govern.
func (s *Postgres) SetMergeJump(ctx context.Context, tx pgx.Tx, jump MergeJump) (bool, error) {
	issue, err := scanIssue(tx.QueryRow(ctx, "select "+issueColumns+" from issues where key = $1 for update", jump.Issue))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the issue the jump mark of %s is for: %w", jump.Issue, err)
	}
	if Stopped(*issue) {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `insert into merge_jumps (issue, reason, session, set_at) values ($1, $2, $3, $4)
		on conflict (issue) do update set reason = excluded.reason, session = excluded.session, set_at = excluded.set_at`,
		jump.Issue, jump.Reason, jump.Session, jump.SetAt); err != nil {
		return false, fmt.Errorf("set the jump mark of %s: %w", jump.Issue, err)
	}
	return true, nil
}

func (s *Postgres) ClearMergeJump(ctx context.Context, tx pgx.Tx, issue string) (bool, error) {
	tag, err := tx.Exec(ctx, "delete from merge_jumps where issue = $1", issue)
	if err != nil {
		return false, fmt.Errorf("clear the jump mark of %s: %w", issue, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Postgres) MergeJumps(ctx context.Context, tx pgx.Tx, project string) ([]MergeJump, error) {
	rows, err := tx.Query(ctx, `select `+mergeJumpColumns+` from merge_jumps
		where split_part(issue, '-', 1) = $1 order by issue`, project)
	if err != nil {
		return nil, fmt.Errorf("list the jump marks of %s: %w", project, err)
	}
	defer rows.Close()
	jumps := []MergeJump{}
	for rows.Next() {
		jump, err := scanMergeJump(rows)
		if err != nil {
			return nil, fmt.Errorf("list the jump marks of %s: %w", project, err)
		}
		jumps = append(jumps, jump)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the jump marks of %s: %w", project, err)
	}
	return jumps, nil
}

func (s *Postgres) MergeJumpOf(ctx context.Context, tx pgx.Tx, issue string) (*MergeJump, error) {
	jump, err := scanMergeJump(tx.QueryRow(ctx, `select `+mergeJumpColumns+` from merge_jumps where issue = $1`, issue))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the jump mark of %s: %w", issue, err)
	}
	return &jump, nil
}

// scanMergeJump reads one row of mergeJumpColumns, with its time in UTC as every time this
// package answers is.
func scanMergeJump(row pgx.Row) (MergeJump, error) {
	var jump MergeJump
	if err := row.Scan(&jump.Issue, &jump.Reason, &jump.Session, &jump.SetAt); err != nil {
		return MergeJump{}, err
	}
	jump.SetAt = jump.SetAt.UTC()
	return jump, nil
}

func (s *Postgres) ReplaceMergeHolds(ctx context.Context, tx pgx.Tx, project string, holds []MergeHold) error {
	if _, err := tx.Exec(ctx, "delete from merge_holds where project = $1", project); err != nil {
		return fmt.Errorf("replace the merge holds of %s: %w", project, err)
	}
	for ordinal, hold := range holds {
		paths := hold.Paths
		if paths == nil {
			paths = []string{}
		}
		_, err := tx.Exec(ctx, `insert into merge_holds (project, ordinal, paths, repository, reason, ends_at, session, set_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			project, ordinal, paths, hold.Repository, hold.Reason, hold.EndsAt, hold.Session, hold.SetAt)
		if err != nil {
			return fmt.Errorf("replace the merge holds of %s: %w", project, err)
		}
	}
	return nil
}

// MergeHolds deletes the project's ended holds and answers the rest, which is LiveMergeHolds'
// own answer: once the holds that ended are gone, every row of the project is a live one. Keep
// the two together: a reader that follows one must find the other beside it.
func (s *Postgres) MergeHolds(ctx context.Context, tx pgx.Tx, project string, now time.Time) ([]MergeHold, error) {
	if _, err := tx.Exec(ctx, "delete from merge_holds where project = $1 and ends_at <= $2", project, now); err != nil {
		return nil, fmt.Errorf("delete the ended merge holds of %s: %w", project, err)
	}
	return s.LiveMergeHolds(ctx, tx, project, now)
}

// LiveMergeHolds is the project's holds that have not ended at now, in the list's own order,
// deleting nothing: the read a caller makes that only shows the holds, where MergeHolds is the
// same read made by the merge itself, which clears the ended ones as it goes.
func (s *Postgres) LiveMergeHolds(ctx context.Context, tx pgx.Tx, project string, now time.Time) ([]MergeHold, error) {
	rows, err := tx.Query(ctx, `select `+mergeHoldColumns+` from merge_holds
		where project = $1 and ends_at > $2 order by ordinal`, project, now)
	if err != nil {
		return nil, fmt.Errorf("list the live merge holds of %s: %w", project, err)
	}
	return scanMergeHolds(rows, project)
}

// scanMergeHolds reads a hold list of mergeHoldColumns, in the order the query gave, with each
// end and set time in UTC. It closes rows, so both reads of the list share one body.
func scanMergeHolds(rows pgx.Rows, project string) ([]MergeHold, error) {
	defer rows.Close()
	holds := []MergeHold{}
	for rows.Next() {
		var hold MergeHold
		if err := rows.Scan(&hold.Paths, &hold.Repository, &hold.Reason, &hold.EndsAt, &hold.Session, &hold.SetAt); err != nil {
			return nil, fmt.Errorf("list the merge holds of %s: %w", project, err)
		}
		hold.EndsAt = hold.EndsAt.UTC()
		hold.SetAt = hold.SetAt.UTC()
		holds = append(holds, hold)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the merge holds of %s: %w", project, err)
	}
	return holds, nil
}

func (s *Postgres) PutMergeJumpRecord(ctx context.Context, tx pgx.Tx, project string, record MergeJumpRecord) error {
	// A record with no entry read carries an empty array, never null: the column is `not null`,
	// and an unread queue says so with EntriesUnknown rather than with a missing array. The nil
	// is replaced rather than the slice copied, as ReplaceMergeHolds does for a hold's paths:
	// the copy buys nothing the encoding can see and pays for up to a page of entries.
	queue := record.Entries
	if queue == nil {
		queue = []MergeQueueEntryRecord{}
	}
	entries, err := json.Marshal(queue)
	if err != nil {
		return fmt.Errorf("put the jump record of %s in %s: encode the queue entries: %w", record.Repository, project, err)
	}
	_, err = tx.Exec(ctx, `insert into merge_jump_records (project, repository, issue, number, head, at, reason,
		entries, entries_total, entries_unknown, outcome, answer)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		on conflict (project, repository) do update set issue = excluded.issue, number = excluded.number,
		head = excluded.head, at = excluded.at, reason = excluded.reason, entries = excluded.entries,
		entries_total = excluded.entries_total, entries_unknown = excluded.entries_unknown,
		outcome = excluded.outcome, answer = excluded.answer`,
		project, record.Repository, record.Issue, record.Number, record.Head, record.At, record.Reason,
		entries, record.EntriesTotal, record.EntriesUnknown, string(record.Outcome), record.Answer)
	if err != nil {
		return fmt.Errorf("put the jump record of %s in %s: %w", record.Repository, project, err)
	}
	return nil
}

// SetMergeJumpOutcome writes what became of one jump — its outcome and the answer that decided
// it (MergeJumpOutcome) — and lands only while the row of (project, the record's repository) is
// still that jump's own: its issue and its head, which is the identity a jump record has, since
// the table keeps one row per repository and no history (MergeJumpRecord). A row a newer jump of
// the same repository has written answers false and is left exactly as that jump wrote it.
//
// It is the one write that changes a record after the fact, and the only one that writes an
// outcome other than MergeJumpAsked: the jump's own transaction writes the row with that value
// before the mutation is sent (PutMergeJumpRecord), and this follows GitHub's answer to that
// mutation, or its failure to answer. Only outcome and answer are written. Everything else of
// the row — the queue the jump asked to go ahead of among all — is what the jump's own write put
// there, which is what makes this write safe to lose: a record this never reached still says
// MergeJumpAsked, which is what is then true of it.
func (s *Postgres) SetMergeJumpOutcome(ctx context.Context, tx pgx.Tx, project string, record MergeJumpRecord) (bool, error) {
	tag, err := tx.Exec(ctx, `update merge_jump_records set outcome = $5, answer = $6
		where project = $1 and repository = $2 and issue = $3 and head = $4`,
		project, record.Repository, record.Issue, record.Head, string(record.Outcome), record.Answer)
	if err != nil {
		return false, fmt.Errorf("record what became of the jump of %s in %s: %w", record.Repository, project, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Postgres) MergeJumpRecords(ctx context.Context, tx pgx.Tx, project string) ([]MergeJumpRecord, error) {
	rows, err := tx.Query(ctx, `select `+mergeJumpRecordColumns+` from merge_jump_records
		where project = $1 order by repository`, project)
	if err != nil {
		return nil, fmt.Errorf("list the jump records of %s: %w", project, err)
	}
	defer rows.Close()
	records := []MergeJumpRecord{}
	for rows.Next() {
		record, err := scanMergeJumpRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("list the jump records of %s: %w", project, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the jump records of %s: %w", project, err)
	}
	return records, nil
}

// scanMergeJumpRecord reads one row of mergeJumpRecordColumns, decoding the queue snapshot the
// jsonb column holds. Every time it answers is in UTC: the jump's own, and each entry's, which
// JSON carries with the offset it was written at, so without this a snapshot read back would
// differ from the one written only in the zone its times are spelled in.
func scanMergeJumpRecord(row pgx.Row) (MergeJumpRecord, error) {
	var record MergeJumpRecord
	var entries []byte
	if err := row.Scan(&record.Repository, &record.Issue, &record.Number, &record.Head, &record.At,
		&record.Reason, &entries, &record.EntriesTotal, &record.EntriesUnknown, &record.Outcome,
		&record.Answer); err != nil {
		return MergeJumpRecord{}, err
	}
	if err := json.Unmarshal(entries, &record.Entries); err != nil {
		return MergeJumpRecord{}, fmt.Errorf("decode the queue entries of %s: %w", record.Repository, err)
	}
	for i := range record.Entries {
		record.Entries[i].EnqueuedAt = record.Entries[i].EnqueuedAt.UTC()
	}
	record.At = record.At.UTC()
	return record, nil
}

func (s *Postgres) MergeHeldIssues(ctx context.Context, tx pgx.Tx, project string) ([]MergeHeldIssue, error) {
	// The hold's mark is read from the payload's own key rather than through a decoded
	// ReadyMerge: the question is whether the row carries a held moment at all, which the key's
	// presence and a non-empty value answer without decoding a payload this read never uses.
	rows, err := tx.Query(ctx, `select issue, next_at from outbox
		where kind = $1 and split_part(issue, '-', 1) = $2
		and `+mergeHeldPayload+` order by issue`,
		string(OutboxKindReadyMerge), project)
	if err != nil {
		return nil, fmt.Errorf("list the held merges of %s: %w", project, err)
	}
	defer rows.Close()
	held := []MergeHeldIssue{}
	for rows.Next() {
		var issue MergeHeldIssue
		if err := rows.Scan(&issue.Issue, &issue.DueAt); err != nil {
			return nil, fmt.Errorf("list the held merges of %s: %w", project, err)
		}
		issue.DueAt = issue.DueAt.UTC()
		held = append(held, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the held merges of %s: %w", project, err)
	}
	return held, nil
}

// HeldReadyMerges is every ready_merge row of project that a merge hold caught and that runs
// later than now, oldest row first, with everything of the row the outbox holds. It answers the
// rows themselves, where MergeHeldIssues answers the issues a hold keeps back, because its
// caller hands each row to ExpediteOutbox, which takes a row and not an id so that the lane it
// wakes on commit is the row's own (Store.ExpediteOutbox).
//
// A deferred row the payload marks held is one Legion itself put off to the end of a hold, and
// the controller replacing the hold list has said those merges may go. A row deferred by
// anything else keeps its next_at and is no part of this read: a submission GitHub rate-limited
// runs again at the moment GitHub's own Retry-After named (daemon's (*outbox).retry), and that
// deadline is GitHub's, not the hold's, so nothing a controller says about holds may move it —
// the same rule a duplicate enqueue keeps to (store.go's readyMergeConflict). A row a hold caught
// and that then failed on GitHub carries no held moment either: the run that was not held closed
// its stretch, so it too waits out the backoff it earned.
//
// The read itself is WaitingNotices', one outbox kind and the held condition apart, so both
// call the one body that reads a project's rows of a kind that are not due (store.go's
// waitingRowsOfKind).
func (s *Postgres) HeldReadyMerges(ctx context.Context, tx pgx.Tx, project string, now time.Time) ([]OutboxRow, error) {
	return waitingRowsOfKind(ctx, tx, OutboxKindReadyMerge, project, now, mergeHeldPayload,
		fmt.Sprintf("list the held merges of %s", project))
}
