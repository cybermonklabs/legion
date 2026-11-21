package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Change is one delivery_changes row (migration 0100): what changed, its run's kind when it is a
// run or a job, and the time that decides which windows it can touch, before and after (a pull
// request's merged_at; a run's later head commit or start). The row triggers write one only when a
// row's content changed, so a reconcile pass that finds nothing new logs none.
//
// An issue_facts row holds no repository: Repo carries the project key and Ref the issue key,
// because a stored Legion fact belongs to a project and an issue (migration 20261117100002). Its
// TOld and TNew are the issue's closed_at before and after the change (the same migration),
// since a Legion day row counts an issue on its close day alone: an insert carries TNew alone, a
// delete TOld alone, and each is nil where the issue was or is open, so a change of an open
// issue's facts carries neither.
//
// The three eval kinds hold none either: Repo carries the project key and Ref the eval id
// (migrations 20261117100012 to 20261117100014). An eval_readings row is one statement's readings
// of one eval, spanning their measured_at; an eval_versions row is one new version of a
// definition or of one of its series and carries no span, since it changes how every window of
// the eval reads; an eval_days row is one transaction's writes of one eval's day rows, spanning
// midnight UTC of the first and the last day written, and its RunKind names the settings rewrite
// where that wrote it (EvalDaysWrittenBySettingsRewrite).
//
// An issue_filings row holds none either: Repo carries the project key and Ref the filed issue's
// key (migration 20261117100019). TOld and TNew are the filing's filed_at before and after: an
// insert carries TNew alone, a delete TOld alone.
//
// A settings row is a write of the delivery settings, whose Repo is empty, or of a repository's
// mapping in repo_projects, whose Repo is that repository (migration 20261117100020); Ref, TOld
// and TNew are empty for both.
type Change struct {
	Seq     int64
	At      time.Time
	Kind    string // one of the ChangeKind constants
	RunKind string // "deploy" | "pr_checks" for a run or a job; EvalDaysWrittenBySettingsRewrite (above); "" for the rest
	Repo    string
	Ref     string
	TOld    *time.Time
	TNew    *time.Time
}

// The kinds of delivery change (Change.Kind), each the kind a table's trigger logs, so a reader of
// the log compares with these rather than spell one again. The delivery intake's five
// (pull_request, run, run_job, session_group, settings) are written into delivery_changes by their
// triggers (delivery_change_log, migration 0100), but for a settings change of a repository's
// mapping, which repo_projects' trigger records in the outbox (migration 20261117100020); the eval
// and Legion kinds (issue_facts and the rest) are recorded in the outbox too, and DrainPending
// moves the outbox's rows into the log.
const (
	ChangeKindPullRequest  = "pull_request"
	ChangeKindRun          = "run"
	ChangeKindRunJob       = "run_job"
	ChangeKindSessionGroup = "session_group"
	ChangeKindSettings     = "settings"
	ChangeKindIssueFacts   = "issue_facts"
	ChangeKindEvalReadings = "eval_readings"
	ChangeKindEvalVersions = "eval_versions"
	ChangeKindEvalDays     = "eval_days"
	ChangeKindIssueFilings = "issue_filings"
)

// EvalDaysWrittenBySettingsRewrite is the RunKind of an eval_days change that the refresher's
// rewrite of a delivery settings change wrote (evals.MarkSettingsRewrite). The rewrite's
// transaction sets the transaction-local setting dispatch.eval_days_cause to it before its first
// write of a day row, and eval_days_change_log records that setting as the change's run_kind
// (migration 20261117100014); no other writer of day rows sets it. The Delivery page's live hub
// recomputes an evals view such a change marks at once, where every other change waits the quiet
// that gathers a burst into one recompute.
const EvalDaysWrittenBySettingsRewrite = "settings_rewrite"

// ChangeRetention is how long delivery_changes keeps a row: a page away longer reads its window once.
const ChangeRetention = 24 * time.Hour

// ChangesChannel is the Postgres notification channel a delivery change announces itself on: the
// trigger that writes each delivery_changes row (migration 0100, delivery_change_log), the trigger
// that records each pending row (delivery_change_log_pending, 20261117100001) and the drain that
// moves pending rows into the log (DrainPending) each send it, and Postgres delivers it at their
// commit. The Delivery page's live hub, the eval day-row refresher and the Legion facts consumer
// listen on it, and this package owns the row, so the name is declared here alone, as
// SettingsChannel is beside the settings row.
const ChangesChannel = "delivery_changes"

// lockChangesStatement takes, for the rest of its transaction, the change log's lock: the
// transaction advisory lock (6900, 0) every insert into delivery_changes takes before it draws a
// seq, and holds to its commit (migration 0100). A seq is therefore drawn only once every row below it has committed or
// rolled back, so a reader's cursor never skips a change row: a reader that has read through seq
// never meets a row below it later.
//
// Each table whose rows a path that takes the lock locks has one position for the lock against
// those row locks, and every writer of the table takes it there: first, before any row lock of
// the table, or last, in the change triggers of the statement that ends its transaction, after
// the rows that statement writes. A writer that took it first beside one that takes it last on
// the same rows would wait for the row the other holds while the other waited for the lock, a
// deadlock. The writers that take it, by position:
//
//   - Last, in one statement: every one-statement write of a delivery row (a pull request, a run,
//     the settings), and a run's jobs write (UpsertRunJobs), which writes the run row and every job
//     in one statement.
//   - Last, in the statement that ends its transaction: the issue.deployed write
//     (recordDeployedEvent), which appends the event and then marks its pull request's row, in
//     the update that locks the row.
//   - First, with this statement queued ahead of its writes (queueLockChanges): every write of
//     the session groups (writeSessionGroup, after the reads that decide it).
//   - First, in SQL: the drain (DrainPending), whose function delivery_changes_drain takes it
//     before it locks the pending rows it moves.
//
// Last is not after every row lock in a statement that changes several rows of a table with a
// foreign key: Postgres fires the after-row triggers at the statement's end row by row, each row's
// foreign key check before its change trigger, so the checks of rows 2..n take FOR KEY SHARE on
// the rows they reference after row 1's trigger took the lock (AttributePullRequests' issues, and
// the fixtures' inserts of many pull requests or jobs). No cycle follows: FOR KEY SHARE waits only
// for FOR UPDATE, a delete or a change of the row's key, and no path that takes the lock holds one
// of those on an issue, a project or a run. TestNoForUpdateOnOwnerTables (api) refuses `for update`
// on issues and projects anywhere in the module, and no statement outside the tests deletes an
// issue, a project or a run, changes their keys or locks a run for update.
//
// A path that takes it last holds it for its last statement and its commit; one that takes it
// first, for the rest of its transaction, so those transactions are short and do their reads
// before it.
//
// No eval, refresher, Legion facts or repository mapping write takes it: their changes wait in the
// outbox (delivery_changes_pending, migrations 20261117100001 and 20261117100020), and the drain
// moves them into the log in a short transaction of its own after they commit.
const lockChangesStatement = `select pg_advisory_xact_lock(6900, 0)`

// queueLockChanges queues the change log's lock (lockChangesStatement) into batch, so the
// statements queued after it run under it in the batch's one round trip.
func queueLockChanges(batch *pgx.Batch) {
	store.QueueExec(batch, "take the delivery change log's lock", lockChangesStatement)
}

// drainBatch is the most pending rows one transaction of the drain moves. On the pod it was
// measured on, a batch of 1,000 answered in 13 to 30 ms while the pod was quiet and in up to 94 ms,
// a sampled hold up to 126 ms, while the api package's tests ran beside it, and 10,000 rows in one
// statement held the change log's lock 118 to 328 ms; a quarter of that batch stays well under
// R19's 100 ms under load too, and no burst, however large, holds the lock longer than one batch.
const drainBatch = 250

// DrainPending moves every committed pending change row (delivery_changes_pending, the outbox the
// eval and Legion tables' triggers and repo_projects' record their changes in) into
// delivery_changes, and answers how many it moved. It moves them a batch at a time
// (DrainPendingBatch), each batch one statement of its own, delivery_changes_drain (migration
// 20261117100001), never a statement of a caller's transaction, and moves another batch while a
// whole one moved. So it holds the change log's lock for one batch's statement and its commit at
// a time, whatever the burst: each batch takes the lock first, before the pending rows it deletes,
// and moves the lowest pending ids in id order, keeping each row's at. Where nothing is pending it
// answers 0 and takes no lock. Two drains take turns on the lock, and each pending row is moved
// exactly once.
//
// A pending row's id is drawn at its insert and not at its commit, so seq follows the order the
// drains move rows in, not the order their writers committed in; every seq is still drawn under
// the lock (lockChangesStatement), so a reader's cursor never skips a moved row. Its callers:
// each Legion facts pass, right after its commit; the live hub, before each read of the log; the refresher, at
// the start of each pass; and the server's boot, before it serves. An eval write, the registry's
// and the refresher's, and every write of a repository's mapping (DrainRepoProjectChange) drain
// one batch instead (DrainAfterCommit), so a route that answers after its drain waits for one
// batch at most, whatever backlog another writer left: those callers move the rest.
//
// A writer killed between its commit and its drain loses no change: its pending rows committed
// with its write, and the next drain of any process moves them, whichever runs first:
//
//   - a live hub's (api's deliveryHub), before its next read of the log, which the rows'
//     notification on ChangesChannel wakes at the writer's commit. Only a process where a page
//     subscribes has one running: a hub starts its listener at its first subscription and stops it
//     when its last one leaves, and drains nothing while no page subscribes;
//   - the next eval or mapping write's, one batch, the lowest ids first (evals' drainChanges,
//     DrainRepoProjectChange), so a row behind a whole batch of others waits for a later drain;
//   - the next Legion facts pass's, every pending row, after its commit (legionfacts' writePass);
//   - the day-row refresher's next pass, every pending row, at its start (evals' refresh). The
//     rows' notification wakes the refresher of every live process, and the process that holds the
//     refresher's lease, eval-days, starts that pass no sooner than 10 s after its last pass started
//     (evals.RefresherGap). Where the killed process held the lease, a woken process finds it held
//     and tries again every 10 s plus up to a tenth (duty.RunWoken), so it takes the lease at its
//     first try after the killed process's database session ends, and its pass drains;
//   - with no process alive, the next process's boot drain, before it serves (cmd/dispatch's main).
func DrainPending(ctx context.Context, pool *store.Pool) (int64, error) {
	var total int64
	for {
		moved, more, err := DrainPendingBatch(ctx, pool)
		total += moved
		if err != nil || !more {
			return total, err
		}
	}
}

// DrainPendingBatch moves one batch of the committed pending change rows into delivery_changes, the
// drainBatch lowest ids, in one statement of its own (drainOneBatch), and answers how many it
// moved and whether rows may remain: a whole batch moved, so another may be pending behind it.
// DrainPending moves batches while it says so; a caller that answers a request drains with it
// alone, so its answer waits for one batch whatever the backlog, which leaves its own rows pending
// where more than a batch was pending ahead of them.
func DrainPendingBatch(ctx context.Context, pool *store.Pool) (int64, bool, error) {
	moved, err := drainOneBatch(ctx, pool)
	return moved, err == nil && moved == drainBatch, err
}

// DrainAfterCommit is the drain a writer whose changes wait in the outbox runs right after its
// write committed: it moves one batch (DrainPendingBatch), the lowest ids first, so the writer's
// own rows are among them where no more than a batch was pending ahead of them. One batch and no
// more, so a route that drains before it answers waits for one batch whatever backlog another
// writer left. A write that finds more than a batch ahead of its rows leaves them pending, as it
// does when its drain fails. A failed drain fails nothing and logs warning, the caller's own text,
// unless ctx has ended: the rows committed with the write, and the next drain of any process moves
// them, or else the next process's boot before it serves (DrainPending states the bound). Its
// callers are every eval write (evals' drainChanges) and every writer of a mapping
// (DrainRepoProjectChange), each with its own warning.
func DrainAfterCommit(ctx context.Context, pool *store.Pool, warning string) {
	if _, _, err := DrainPendingBatch(ctx, pool); err != nil && ctx.Err() == nil {
		slog.Warn(warning, "error", err)
	}
}

// drainOneBatch is the statement that moves up to drainBatch pending rows into delivery_changes,
// and answers how many it moved (DrainPendingBatch).
func drainOneBatch(ctx context.Context, pool *store.Pool) (int64, error) {
	var moved int64
	if err := pool.QueryRow(ctx, `select delivery_changes_drain($1)`, drainBatch).Scan(&moved); err != nil {
		return 0, fmt.Errorf("move the pending delivery changes into the change log: %w", err)
	}
	return moved, nil
}

// ChangesSince reads up to limit changes after seq, oldest first. Every seq is drawn under the
// change log's lock (lockChangesStatement), so a reader that has read through seq never meets a
// row below it later: its cursor never skips a change row.
func ChangesSince(ctx context.Context, q Querier, seq int64, limit int) ([]Change, error) {
	rows, err := q.Query(ctx, `
		select seq, at, kind, coalesce(run_kind, ''), coalesce(repo, ''), coalesce(ref, ''), t_old, t_new
		from delivery_changes
		where seq > $1
		order by seq
		limit $2
	`, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []Change{}
	for rows.Next() {
		var change Change
		if err := rows.Scan(&change.Seq, &change.At, &change.Kind, &change.RunKind, &change.Repo, &change.Ref, &change.TOld, &change.TNew); err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

// OldestChangeSeq is the oldest kept change's seq, or 0 when the log is empty: a reader whose
// cursor is below it has missed changes PruneChanges removed.
func OldestChangeSeq(ctx context.Context, q Querier) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, `select coalesce(min(seq), 0) from delivery_changes`).Scan(&seq)
	return seq, err
}

// ChangeCursor is where one reader of the change log stands in that log, and is a cursor of that
// log alone: the seq of the last change it read (0 before it read any), when the log recorded that
// change (At, nil where the reader holds none: a position no change held), and when the reader last
// read the log to its end, by the reader's own clock (ReadAt). A page of the log moves it by one
// rule (Through), and a move to the log's head (ChangesHead, on a first read or a lost cursor) is a
// read of the log to its end too.
//
// A reader reads its clock afresh for each page, before that page's read: the one reading is the
// now its lost-cursor check is judged at (CursorLost) and the ReadAt a read to the end stamps, so
// a catch-up that runs for hours is judged at the moment of each page and not at its start.
type ChangeCursor struct {
	Seq    int64
	At     *time.Time
	ReadAt time.Time
}

// Through is c moved over changes, a page ChangesSince read after c.Seq with limit, where checked
// is the reader's clock as it read it before that page (ChangeCursor). Seq and At move to the
// page's last change, and an empty page leaves them where they stood. A page shorter than limit
// found no change after the last one it took, a read of the log to its end, and stamps ReadAt
// with checked; a full page carries the earlier ReadAt forward, since the reader has not seen the
// log's end since. At points at a copy of the change's time, so the cursor keeps none of the page
// alive.
func (c ChangeCursor) Through(changes []Change, limit int, checked time.Time) ChangeCursor {
	if n := len(changes); n > 0 {
		at := changes[n-1].At
		c.Seq, c.At = changes[n-1].Seq, &at
	}
	if len(changes) < limit {
		c.ReadAt = checked
	}
	return c
}

// CursorLost reports whether cursor may have passed over changes its reader never read, as of
// now, the reader's own clock as it read it for this page (ChangeCursor). It is the one rule every
// reader of the log judges its cursor by (the eval day-row refresher, the delivery live hub and the
// Legion facts consumer): a reader whose cursor is lost reads again everything the changes could
// have named, and any other reads on after its cursor.
//
// The cursor is lost where the log holds rows and its newest seq is below the cursor's: the
// sequence went backwards (a TRUNCATE ... RESTART IDENTITY, a restore), so a reader that read on
// after its cursor would read nothing until the new seqs passed it, whatever the age of the
// change it rested on. An empty log is not that: a quiet day's prune can take every row, and the
// next row continues the sequence above the cursor; once a row lands below the cursor after a
// restart, this rule sees it.
//
// The cursor is lost too where the change at its seq has left the log, or the log holds another
// change there (one recorded at another time than the cursor's At: a log reset and refilled past
// the cursor), and either of two holds:
//
//   - the change was recorded less than ChangeRetention ago by the database's clock, the clock
//     its at was written by and PruneChanges cuts by, so no prune took it: a log reset, or rows
//     deleted by hand;
//   - the reader has not read the log to its end for longer than ChangeRetention before now, by
//     its own clock, the one ReadAt is stamped by: since that read a prune may have taken changes
//     after its cursor that it never read, while it stood still or while it caught up a backlog
//     a full page at a time.
//
// A change gone after its retention while its reader read the log to its end within the retention
// was pruned in its time and is no loss: that read took every change the log then held, so every
// change the reader has not read was recorded since, is younger than the retention and is still in
// the log. That is the routine prune of a quiet day, which takes the change a cursor rests on
// whenever a day brings no newer one. A cursor with no At is judged by ReadAt alone. A cursor at 0
// has read nothing and lost nothing.
//
// The change at the cursor, the log's newest seq and the database's clock are one statement's,
// so they describe one log.
func CursorLost(ctx context.Context, q Querier, cursor ChangeCursor, now time.Time) (bool, error) {
	if cursor.Seq == 0 {
		return false, nil
	}
	var held *time.Time
	var latest int64
	var clock time.Time
	if err := q.QueryRow(ctx, `
		select (select at from delivery_changes where seq = $1),
		       (select coalesce(max(seq), 0) from delivery_changes),
		       now()
	`, cursor.Seq).Scan(&held, &latest, &clock); err != nil {
		return false, err
	}
	if latest > 0 && latest < cursor.Seq {
		return true, nil
	}
	moved := held == nil || (cursor.At != nil && !held.Equal(*cursor.At))
	young := cursor.At != nil && clock.Sub(*cursor.At) < ChangeRetention
	stalled := now.Sub(cursor.ReadAt) > ChangeRetention
	return moved && (young || stalled), nil
}

// ChangesHead is the change cursor at the newest change the log holds: its seq and when it was
// recorded, and Seq 0 with no At where the log is empty. It is where a reader's cursor rests once
// it is moved to the log's head (the eval day-row refresher's and the Legion facts consumer's on
// their first read and on a lost cursor, the delivery live hub's on its first read and on a lost
// one), and the position a hub's recompute reads through. The two are one row of one statement,
// so they name one change. ReadAt is zero: a move to the head is a read of the log to its end,
// which the reader that moves its cursor there stamps by its own clock (ChangeCursor).
func ChangesHead(ctx context.Context, q Querier) (ChangeCursor, error) {
	var head ChangeCursor
	err := q.QueryRow(ctx, `select seq, at from delivery_changes order by seq desc limit 1`).Scan(&head.Seq, &head.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChangeCursor{}, nil
	}
	if err != nil {
		return ChangeCursor{}, err
	}
	return head, nil
}

// PruneChanges deletes every change recorded more than ChangeRetention before the database's now(),
// and answers how many it deleted. Each row's at is the database's clock too, so the cutoff and the
// rows it compares share one clock whichever process prunes.
func PruneChanges(ctx context.Context, pool *store.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `delete from delivery_changes where at < now() - $1::interval`, ChangeRetention)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
