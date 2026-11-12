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
// That holds only because the clear takes the same lock first (ClearMergeJump): this read and
// the departure's delete are the two halves of one lock order, and the `for update` here is the
// first half of it. It is doing work twice over — it serializes the Stopped check against the
// departure's own row write, and it is what the clear's lock orders against — so it stays.
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

// ClearMergeJump deletes issue's jump mark, under the issue row's lock, and answers whether a
// row went.
//
// The lock is the clear's half of the one order every writer of merge_jumps keeps: the issue row
// first, the mark's row second. SetMergeJump already locks the issue row before it inserts
// (`select … for update` above), so without this the two sides lock the two rows in opposite
// orders, and both halves of that were driven against PostgreSQL at 56e69560, each half failing
// in a way of its own:
//
//   - With no mark standing, the clear deletes nothing and locks nothing, so the mark's
//     transaction never waits for the departure. It locks the issue row, reads it running,
//     inserts, and commits; the departure's row write and commit follow, and a mark stands on an
//     issue that has left the workflow — the very thing workflow's leave clears it to prevent.
//   - With a mark standing, the departure holds the deleted merge_jumps row and then wants the
//     issue row, while the mark's transaction holds the issue row and wants that merge_jumps row
//     in its `on conflict`. That is a cycle, and PostgreSQL kills one of the two (40P01). Which
//     one it kills is its choice, so neither side may be written as the survivor.
//
// With this lock both sides take issues then merge_jumps, and the two orderings are the only two
// outcomes: the departure first, so the mark's read waits for it, sees a record the departure
// stopped (Stopped) and writes nothing; or the mark first, so the clear waits for it and deletes
// the row it just wrote. No cycle, and no mark outliving a departure.
//
// Why the lock lives here, and not in workflow's leave, is the third writer of this table: the
// daemon's own spending of a mark (daemon's readyMergeMarks.Jump) calls this clear in a
// transaction of its own, as does the controller's route for taking a mark off (api's
// mergeJumpClear). A delete taking no lock is, as the first case above shows, exactly what lets
// another transaction's insert slip past it; putting the lock in the one function all three call
// leaves no writer of merge_jumps outside the order, and costs one statement in each.
//
// The spend takes this lock behind one of its own, and that is no second cycle. markWrite
// replaces its ready_merge row's payload under the row's lease first (daemon's markWrite,
// Store.ReplaceOutboxPayload) and calls this clear after it, so the spend's order is that outbox
// row, then issues, then merge_jumps — while a departure holds the issue row and writes outbox
// rows after it. A cycle would need the departure to wait on the spend's row, and no statement
// of a departure can: its outbox rows all go through one insert (Store.Enqueue), whose only
// arbiter index is the partial unique index over ready_merge rows alone (readyMergeConflict), so
// a row of any other kind enters that index and conflicts on nothing; no departure enqueues a
// ready_merge row at all (the one place that builds one is workflow's ready, which leave does
// not reach); and the one outbox row a departure deletes is filtered on the status kind
// (DropStatusWrites), which never matches a ready_merge row. Driven at this head with the fix:
// a spend holding its leased ready_merge row, with a whole departure of that issue run beside it
// — its status drop, this clear, and nine outbox rows (auto_merge_disable, six supervise,
// issue_suspend, notice) — and the spend's clear after it; both committed, neither saw 40P01.
//
// The alternative weighed and not taken was the fact lock (intake.SerializeFacts, the advisory
// lock intake.ApplyFact takes at internal/intake/facts.go:1179), on the mark's routes. It would
// close the same window, since every departure does reach leave under that lock today — the two
// fact handlers (workflow/engine.go:295, :1015), close_root through the API's applyFact
// (api/architect.go:1312, which calls intake.ApplyFact at architect.go:261) and LeaveUnslotted
// inside the admission pass (admit/admit.go:1720, reached from Admission.Apply, a fact handler,
// and from Admission.Reconcile, whose transaction takes SerializeFacts at
// daemon/workflow.go:803). Three things decided against it. It is one lock for the whole daemon,
// so a controller's mark call would queue behind whatever fact is in flight, for a window it has
// no stake in. Its correctness rests on that four-caller reading staying true, and internal/admit
// takes no SerializeFacts of its own, so nothing in the package the fourth caller lives in keeps
// the rule the fix would depend on. And it reaches only the callers that take it: the spend's
// clear above runs outside the fact lock, so it would stay outside the fix, resting on the
// separate argument that two deletes of one row cannot leave a mark standing. The row lock needs
// none of the three: it is taken by the writers themselves, wherever they are called from.
//
// A lock of its own keyed to the issue (an advisory lock on the key) would also order the two
// sides, but the issue row's lock is one SetMergeJump must take anyway — its Stopped check is
// read under it — so a second lock would add a name to learn without removing a wait.
func (s *Postgres) ClearMergeJump(ctx context.Context, tx pgx.Tx, issue string) (bool, error) {
	// An issue with no record locks nothing and clears nothing: a tree's cleanup deleted the row
	// and took the mark with it (migration 20261110235352's `on delete cascade`), and that
	// deletion locked the issue row itself before the cascade reached merge_jumps, so it keeps
	// the order too.
	if _, err := tx.Exec(ctx, "select 1 from issues where key = $1 for update", issue); err != nil {
		return false, fmt.Errorf("lock the issue the jump mark of %s is for: %w", issue, err)
	}
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
