package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PodRelease is one release of an issue pod: which roles were in it and when their claims were
// suspended as released. It is the row every resume of that pod hangs from, so a reader can pair
// a release with what woke it and how long the agent waited.
type PodRelease struct {
	// ID is the row's own id, which the store fills.
	ID int64
	// Project and Issue name the pod.
	Project string
	Issue   string
	// Generation is the issue's generation at the release.
	Generation uint64
	// Roles are the roles whose claims the release suspended, sorted.
	Roles []string
	// At is when the release completed.
	At time.Time
	// BundleBytes is the size of the git bundle of unpushed work an earlier release saved, which
	// pod_releases.bundle_bytes keeps for the rows those releases wrote. No release saves one
	// now, so every release this daemon records writes zero; the column stays because the release
	// before still writes it, and a rollback runs that release over this table.
	BundleBytes int
	// WakeTask is the workflow's rule for the task of a wake taken under the release's mark
	// (workflow.ReleasedWakeTask), which RecordPodRelease uses and no read fills: it answers, from
	// the wake kind, the detail and the task the wake's start carries, the task that start carries
	// from now on. The store does not restate the wake's words. nil rewrites no task.
	WakeTask func(kind, detail, task string) string
	// Release is the id of the release's mark (issue_releases.id) the record stands for,
	// pod_releases.issue_release_id; zero for a record that names no mark. Where it is zero,
	// RecordPodRelease names the issue's mark it locks, if one stands. A release is recorded once
	// per mark (RecordPodRelease), and a record read back names the mark it was written with.
	Release int64
}

// PodResume is one wake of a released pod and what it cost: which wake brought it back, when the
// daemon took the wake, when the pod was Ready, and when the woken role took its first turn.
// Stuck is a resume with no first turn ten minutes after its wake, which also writes a warning to
// the daemon's log.
type PodResume struct {
	ID int64
	// Release is the PodRelease this wake ended; zero when no release row is recorded for it.
	Release int64
	Project string
	Issue   string
	// Role is the role the wake was for.
	Role string
	// Kind names the wake: a daemon delivery, a CI settlement, a role message, an agent message
	// or the backstop.
	Kind string
	// At is when the daemon took the wake, by the daemon's clock. A listener's report can wait
	// for a release before it is taken, and At is the take, not the report's own time.
	At time.Time
	// ClockAt is when the resume's stuck clock started (pod_resumes.clock_at): the wake's own
	// moment, moved later to the moment the wake pacing let its launch out and to the moment a
	// project's resume ended the pause that held it (decision 14 of dispatch://INFRA-3759). The
	// stuck warning and the stranded report measure from it.
	ClockAt time.Time
	// Pause is the project pause that held the wake (pod_resumes.pause_id), 0 for a wake no pause
	// held. A held wake pairs with no release (Release is 0), and the release measurements leave
	// its wait out (decision 15).
	Pause int64
	// PodReadyAt and FirstTurnAt are zero until each is observed.
	PodReadyAt  time.Time
	FirstTurnAt time.Time
	// Stuck is whether the resume had no first turn ten minutes after its stuck clock started.
	Stuck bool
}

// PodHold is one streak of the idle rule keeping a pod past the idle period for one reason:
// when the rule first read that reason for that pod, when it last did, and how many passes it
// has made. Holds are what tell a deployment whether release is working: a pod held for
// background work is working as designed, and one held by an old image or an unsent report is
// not. A streak is one row because a pod can be held for days and the tick runs every minute,
// which would otherwise be 1,440 rows a day saying the same thing.
//
// A streak ends when the pod is released (RecordPodRelease), when the rule reads another reason
// for that pod, and when a pass of the rule holds it for no reason worth a row
// (EndPodHoldStreaks): the next pass of that reason then opens a streak of its own, with its own
// FirstAt and Passes. That is what makes the rows of a window the holds of that window, rather
// than one row per pod and reason carrying every hold there has ever been.
type PodHold struct {
	ID      int64
	Project string
	Issue   string
	// Reason is the idle rule's own name for what held the pod (workflow.IdleHold's String).
	// Detail is what that one word leaves out, empty for a reason that has nothing to add: a
	// background-work hold carries the kinds the session reported, which is what separates a pod
	// kept by a subagent from one kept by a background job. The reason itself stays the rule's
	// own word, so a reader can group holds by it.
	Reason string
	Detail string
	// FirstAt is the first pass of the streak and At the newest; Passes counts them. A streak
	// still running reads in every window whose end is at or after At, FirstAt and Passes
	// included, so a reader sees how long a pod has been held rather than only that it is.
	FirstAt time.Time
	At      time.Time
	Passes  int64
}

// PodReleaseRecords is what the daemon serves a pod that reads the records of a window: every
// release, resume and hold recorded at or after a time, oldest first.
type PodReleaseRecords struct {
	Releases []PodRelease
	Resumes  []PodResume
	Holds    []PodHold
}

// RecordPodRelease writes one release and answers the row's id, which every resume of that pod
// names. A release records what it did even when nothing ever wakes the pod, so a window's count
// of releases is complete without reading any resume. It ends every hold streak of that pod: the
// pod is gone, so whatever was keeping it is over, and a pod held again after it comes back is
// a streak of its own.
//
// It also points at this release each wake taken while the release still stood: a resume of the
// issue above the mark's wake_row, of a role in the mark's suspended set, with no first turn and
// not closed. Such a wake came after a stop of this release recorded the role as `released`, so
// it ends this release, but the take read the release before it, since this one had no row yet
// (TakePodWake). Its release_id and seen_release become this release's id, and its time and its
// stuck clock (clock_at) become this release's time when they were earlier, because the pod starts
// to come back at the release's end. A resume dated before its release would count negative pod
// hours, and a stuck clock started at the take would warn of a pod still being built. A
// seen_release below this release would let the role's next wake close the resume as one whose pod
// went away (ClosePodGoneResume), and take a second wake with a second start. A held wake, one a
// project's pause holds (pod_resumes.pause_id), keeps release_id 0, as every held wake does
// (pod_resumes_held_wake_pairs_with_no_release, decision 15 of dispatch://INFRA-3759): its
// seen_release and its times move, and the release measurements leave its wait out. The teardown
// records the release before it clears the mark (internal/daemon's releaseIssue), so the mark is
// there to read in this transaction. With no mark, or no such resume, the update changes nothing.
//
// The same transaction retells the start of each such wake (PodRelease.WakeTask,
// retellWakesUnderRelease). A wake of a role that a project's pause stopped, taken under the mark,
// reads that pause as what stopped the role (StoppingPauseTx), because no release is recorded
// after the pause's stop yet. So its start's task says that the pause kept the pod. This record
// is the moment the release takes that pod, and the start's task becomes the task of the same
// wake in the release's words. The retell changes only the start's task: a held wake keeps its
// pause and release 0.
//
// The transaction locks the mark `for update` before it inserts the release's row. A take
// (PodRecords.TakePodWake) locks the same mark `for share` before it reads the newest release, so
// the take and this record run one after the other. When the take locks first, this lock waits
// until the take commits. The update then reads the take's resume, and the retell reads the
// start that the wake queued in the take's transaction. When this lock comes first, the take
// waits until this transaction commits, and then reads this release. Without the lock, a take
// still open when this transaction commits keeps the release before, and the update never reads
// its resume.
//
// After the mark, and before it writes anything, the transaction locks the project's open pause
// row `for share` (LockOpenPauseByTokenTx). A project's resume (Resume) locks that row `for
// update`. It then writes the starts the pause holds (ResumeRequest.MarkHeld) and the project's
// open resumes (SetProjectResumeClocksTx). This record writes a held wake's resume and then its
// start, which is the other order. So the pause row makes the record and the resume run one after
// the other. Without the lock, each could wait for a row the other wrote, and Postgres would abort
// one of the two as a deadlock. An aborted record writes no release, and the held wake's start
// keeps the pause's words. While the project runs there is no open pause, and this lock locks
// nothing. The record holds no lock when it locks the mark, and only the mark when it locks the
// pause row, so it cannot deadlock with a take, a cancel or a resume; PodRecords.TakePodWake
// gives the whole lock order.
//
// The record is also the mark's one-way step: the row it inserts names the release's mark
// (pod_releases.issue_release_id). The teardown records the release once the runtime has answered
// it without an error, before the teardown's own later steps (internal/daemon's releaseIssue), so
// a mark that a record names is a release whose pod is gone. A teardown that runs again after one
// of those steps failed reads it: IssueRelease answers ReleaseActs and the runtime is not asked
// again, and a cancel refuses the mark (CancelIssueRelease). A step of the runtime that fails
// after the pod is gone comes before the record; the mark's commit (CommitIssueRelease) keeps
// that release from being cancelled.
//
// A record names the mark rel.Release names, or, where that is zero, the issue's mark it locks. A
// record with neither names no mark, as a record a test writes alone does, and each such call
// writes a row. A mark is recorded once (pod_releases_one_per_issue_release): the teardown records
// the release and clears the mark in transactions of their own, so a teardown that failed, or
// whose daemon died, between the two runs again and records the release again. That second record
// takes the record that names the mark, under the mark's lock and before the pause row's, and
// writes nothing: it answers the first record's id with wrote false, ends no streak, and leaves the
// wakes the first record pointed at itself and retold as it left them. The index and the insert's
// `on conflict` are the second guard, for a record whose mark no longer stands. wrote is true where
// this call wrote the row, which a record that names no mark always does.
//
// A write of this record can fail every time, for a fault of the database or of this row. The
// teardown runs again on the outbox's backoff while it does, until workflow.WakeHeldForRelease
// after the release's mark, and its first attempt at or past that bound gives up on the record
// (internal/daemon's releaseIssue): its last write is SettleUnrecordedRelease, in place of the
// record and of the clear of the mark, which settles the wakes taken under the mark without a row
// here and clears the mark in the same transaction.
func (s *Store) RecordPodRelease(ctx context.Context, rel PodRelease) (id int64, wrote bool, err error) {
	if rel.Project == "" || rel.Issue == "" {
		return 0, false, errors.New("a pod release record needs its project and issue")
	}
	err = Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		id, wrote = 0, false
		var mark, last int64
		err := tx.QueryRow(ctx, `select id, last_row from issue_releases where project = $1 and issue = $2 for update`,
			rel.Project, rel.Issue).Scan(&mark, &last)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock the release mark: %w", err)
		}
		named := rel.Release
		if named == 0 {
			named = mark
		}
		if named != 0 {
			err := tx.QueryRow(ctx, `select id from pod_releases where issue_release_id = $1`, named).Scan(&id)
			if err == nil {
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the record of the release's mark: %w", err)
			}
		}
		if _, _, err := LockOpenPauseByTokenTx(ctx, tx, rel.Project); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `insert into pod_releases (project, issue, generation, roles, at, bundle_bytes, issue_release_id)
			values ($1, $2, $3, $4, $5, $6, nullif($7::bigint, 0))
			on conflict (issue_release_id) where issue_release_id is not null do nothing returning id`,
			rel.Project, rel.Issue, int64(rel.Generation), strings.Join(rel.Roles, ","), rel.At, rel.BundleBytes, named).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx, `select id from pod_releases where issue_release_id = $1`, named).Scan(&id)
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `with pointed as (update pod_resumes r set release_id = case when r.pause_id is null then $3::bigint else 0 end,
			seen_release = $3, at = greatest(r.at, $4), clock_at = greatest(r.clock_at, $4)
			from issue_releases m where m.project = $1 and m.issue = $2 and r.project = m.project and r.issue = m.issue
			and r.id > m.wake_row and r.role = any(m.suspended) and r.first_turn_at is null and r.closed_at is null
			returning r.role)
			select distinct role from pointed`,
			rel.Project, rel.Issue, id, rel.At)
		if err != nil {
			return fmt.Errorf("point the wakes taken during the release at it: %w", err)
		}
		woken, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("point the wakes taken during the release at it: %w", err)
		}
		if err := retellWakesUnderRelease(ctx, tx, rel.Issue, woken, last, rel.WakeTask); err != nil {
			return err
		}
		wrote = true
		return endPodHoldStreaks(ctx, tx, rel.Project, rel.Issue, "")
	})
	if err != nil {
		return 0, false, fmt.Errorf("record the release of %s: %w", rel.Issue, err)
	}
	return id, wrote, nil
}

// SettleUnrecordedRelease is the last write of a teardown that gives up on the release's record
// (internal/daemon's releaseIssue): a write of the record that still fails at or past
// workflow.WakeHeldForRelease after the release's mark. It writes nothing to pod_releases, so the
// release records hold no row of this release, and it clears the mark, in place of
// ClearIssueRelease. release is the mark the record names, or 0 for the issue's mark, as for
// RecordPodRelease. Where a record names that mark after all (a write that committed and then
// answered an error), the record stands: this writes nothing, leaves the mark, and answers
// recorded true, and the teardown runs again and reads that record (ReleaseDecision.PodTaken). So
// no resume that a record pointed at its release is changed here.
//
// Otherwise it settles each wake taken while the release stood, the resumes the record would
// point at itself: a resume of the issue above the mark's wake_row, of a role in the mark's
// suspended set, with no first turn and not closed. The start that such a wake queued waits for
// the teardown row (record's outboxInOrder), so it is the role's one start, in the pod that comes
// back, as after a recorded release. The resume pairs with release 0. The release it ends has no
// row, and the release before it, which the take read (LatestPodRelease), did not take this pod. A
// resume of release 0 that no pause holds is the resume of no release, as the first wake of an
// issue is: the stranded report leaves it out (UnfinishedPodResumesTx), and the stuck warning
// still reads it. Its seen_release stays the issue's newest release when the wake was taken
// (RecordPodWakeTx). While the mark stands, a record of the issue's release names that mark
// (RecordPodRelease), none does, and this writes none, so that is still the issue's newest
// release. So the role's next wake does not close the resume as one whose pod went away
// (ClosePodGoneResume), the open resume refuses that wake's take (pod_resumes_one_open_per_role),
// and the role gets no second start. A release recorded later, whose pod went away again, lets
// the next wake close it, as it does for every such resume. Its time and its stuck clock
// (clock_at) move to at where they were earlier, as the record moves them, because the pod starts
// to come back at the release's end. A held wake keeps its pause. The same transaction retells the
// start of each such wake in the release's words (retellWakesUnderRelease, wakeTask), since the
// pod is gone, ends every running hold streak of the pod, as the record does, and then deletes the
// mark (deleteIssueRelease). With no mark, or no such resume, it changes no resume.
//
// The settle and the clear of the mark are one transaction, so no wake is taken between them. A
// take locks the mark first (lockReleaseMark; PodRecords.TakePodWake). A take that committed
// before this locked the mark is a wake taken under the mark, and this settles it. A take that
// comes while this runs waits for its commit and then finds no mark: it pairs with the newest
// release recorded (LatestPodRelease), as every wake after a release's end does. With the clear in
// a later transaction, a wake taken between the two would pair with the release before this one,
// which did not take this pod, and its start would not be told in this release's words.
//
// The transaction takes RecordPodRelease's lock order: the issue's release mark `for update`,
// then, before it writes anything, the project's open pause row `for share`
// (LockOpenPauseByTokenTx). So it orders against a take, a cancel and a project's resume as the
// record does (PodRecords.TakePodWake gives the whole lock order), and it reads the record that
// names the mark under the mark's lock.
func (s *Store) SettleUnrecordedRelease(ctx context.Context, project, issue string, release int64, at time.Time,
	wakeTask func(kind, detail, task string) string) (recorded bool, err error) {
	if project == "" || issue == "" {
		return false, errors.New("settling the wakes of an unrecorded release needs its project and issue")
	}
	err = Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		recorded = false
		var mark, last int64
		err := tx.QueryRow(ctx, `select id, last_row from issue_releases where project = $1 and issue = $2 for update`,
			project, issue).Scan(&mark, &last)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock the release mark: %w", err)
		}
		named := release
		if named == 0 {
			named = mark
		}
		if named != 0 {
			var id int64
			err := tx.QueryRow(ctx, `select id from pod_releases where issue_release_id = $1`, named).Scan(&id)
			if err == nil {
				recorded = true
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the record of the release's mark: %w", err)
			}
		}
		if _, _, err := LockOpenPauseByTokenTx(ctx, tx, project); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `with settled as (update pod_resumes r set release_id = 0,
			at = greatest(r.at, $3), clock_at = greatest(r.clock_at, $3)
			from issue_releases m where m.project = $1 and m.issue = $2 and r.project = m.project and r.issue = m.issue
			and r.id > m.wake_row and r.role = any(m.suspended) and r.first_turn_at is null and r.closed_at is null
			returning r.role)
			select distinct role from settled`,
			project, issue, at)
		if err != nil {
			return fmt.Errorf("settle the wakes taken during the release: %w", err)
		}
		woken, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("settle the wakes taken during the release: %w", err)
		}
		if err := retellWakesUnderRelease(ctx, tx, issue, woken, last, wakeTask); err != nil {
			return err
		}
		if err := endPodHoldStreaks(ctx, tx, project, issue, ""); err != nil {
			return err
		}
		return deleteIssueRelease(ctx, tx, project, issue)
	})
	if err != nil {
		return false, fmt.Errorf("settle the wakes of the unrecorded release of %s: %w", issue, err)
	}
	return recorded, nil
}

// retellWakesUnderRelease rewrites the task of each start that a wake taken under the release's
// mark queued, for the release's record (RecordPodRelease) and for the teardown that gives up on
// that record (SettleUnrecordedRelease). roles are the roles of the wakes that the record points at
// itself, or that the give-up settles, as the update returns them: each role of the mark's
// suspended set that has a resume above the mark's wake_row with no first turn and not closed. The
// update changes none of the columns that choose those resumes, so this reads the starts of the
// same roles and does not choose them again. The starts are the starts of those roles above last,
// the mark's last_row, that have a wake's own shape (wakeStartRow). A root architect's entry start
// and a production check's look are no wake's start, and keep their task. retell answers the
// task a start carries from now on, from the wake kind and the detail the start carries
// (record.SuperviseRequest.WakeKind and WakeDetail), which its task was rendered with, and from
// the task it carries now (PodRelease.WakeTask). The role's resume shows only that such a wake was
// taken: its kind can be another wake's. A start of a role with no such resume is no such wake's,
// and keeps its task, and so does a start an earlier daemon queued with no detail (retellStarts).
// With no role or a nil retell, it writes nothing. A record with no mark points at no resume, so
// it has no role.
func retellWakesUnderRelease(ctx context.Context, tx pgx.Tx, issue string, roles []string, last int64, retell func(kind, detail, task string) string) error {
	if retell == nil || len(roles) == 0 {
		return nil
	}
	return retellStarts(ctx, tx, issue, retell, `select outbox.id, outbox.payload->>'role', coalesce(outbox.payload->>'task', ''),
		outbox.payload->>'wakeKind', outbox.payload->>'wakeDetail' from outbox
		where outbox.issue = $1 and outbox.id > $2 and `+wakeStartRow+` and outbox.payload->>'role' = any($3)
		order by outbox.id`, issue, last, roles)
}

// retellStarts rewrites, through tx, the task of each start that query reads of the wakes taken
// while the release of issue stood: a cancel's (retellWakesOf) and the release's record's
// (retellWakesUnderRelease). query reads, for each start, its outbox id, its role, its task (""
// for none), the wake kind it carries (record.SuperviseRequest.WakeKind), and the wake's detail
// (record.SuperviseRequest.WakeDetail), NULL where the payload carries none. That kind and that
// detail are what WakeTask rendered the start's task with. retell answers the task the start
// carries from now on: WakeTask of that kind and detail in other words, rendered again
// (workflow's retellWakeTask). Only the task changes: the start keeps its role, generation, wake
// kind, detail, pause and the rest of its payload. A start whose task retell answers unchanged is
// not written.
//
// A start that carries no detail and a task is a start an earlier daemon queued, before the start
// carried the detail. Its task cannot be rendered again, and no words are cut out of it either. So
// it keeps its task as it is, and the transaction logs why, once it commits (logStartWithoutDetail).
// Its words are the words the earlier daemon chose for the state it saw. A start with no task, such
// as a pause's owed launch, has nothing to retell. Each query reads only a wake's own start
// (wakeStartRow), which carries a wake kind. A start with a detail and no wake kind is no wake's
// start: record's validateOutboxPayload refuses one when it is written and when it is read, and
// no query here reads one, so its task is not rendered again either.
func retellStarts(ctx context.Context, tx pgx.Tx, issue string, retell func(kind, detail, task string) string, query string, args ...any) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the starts of the wakes taken while the release of %s stood: %w", issue, err)
	}
	type retold struct {
		id   int64
		task string
	}
	var writes []retold
	for rows.Next() {
		var id int64
		var role, task, kind string
		var detail *string
		if err := rows.Scan(&id, &role, &task, &kind, &detail); err != nil {
			rows.Close()
			return fmt.Errorf("read the starts of the wakes taken while the release of %s stood: %w", issue, err)
		}
		if task == "" {
			continue
		}
		if detail == nil {
			logStartWithoutDetail(ctx, issue, id, role, kind, task, "its task is kept as it is")
			continue
		}
		if told := retell(kind, *detail, task); told != task {
			writes = append(writes, retold{id: id, task: told})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the starts of the wakes taken while the release of %s stood: %w", issue, err)
	}
	for _, w := range writes {
		if _, err := tx.Exec(ctx, `update outbox set payload = jsonb_set(payload, '{task}', to_jsonb($2::text)) where id = $1`,
			w.id, w.task); err != nil {
			return fmt.Errorf("rewrite the task of outbox row %d, the start of a wake taken while the release of %s stood: %w", w.id, issue, err)
		}
	}
	return nil
}

// logStartWithoutDetail logs, once the caller's transaction commits, that outbox row id, the start
// of issue's role for a wake of kind, carries no detail (record.SuperviseRequest.WakeDetail), and
// what the transaction did with it instead of rendering its task again: what. Only a start an
// earlier daemon queued is such a start, so the line is rare. The line carries task, the task of
// that start, as one value, which slog writes on the same line. A retell keeps the start in the
// outbox (retellStarts), and the line's task is the task the role reads. A cancel deletes the
// start in the same transaction (dropWakesUnderRelease), and the line is then the one record of
// that task, such as the text of a CI settlement that the cancel's resume does not carry.
//
// The line goes to the logger of the pool the caller's transaction is open on (logOf), as the
// store's other lines about a transaction do, and to the process's default logger for a context
// no transaction scopes.
func logStartWithoutDetail(ctx context.Context, issue string, id int64, role, kind, task, what string) {
	log := slog.Default()
	if scope := scopeOf(ctx); scope != nil {
		log = logOf(scope.pool)
	}
	OnCommit(ctx, func() {
		log.Warn("store: the start of a wake that an earlier daemon queued carries no detail, so its task cannot be told again; "+what,
			"issue", issue, "row", id, "role", role, "kind", kind, "task", task)
	})
}

// MarkPodResumeReady stamps when the replacement pod was Ready. It is written once, by the first
// observation: a second observation of the same resume leaves the first time, which is the one
// R5 measures.
func (s *Store) MarkPodResumeReady(ctx context.Context, id int64, at time.Time) error {
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update pod_resumes set pod_ready_at = $2 where id = $1 and pod_ready_at is null`, id, at)
		return err
	})
	if err != nil {
		return fmt.Errorf("stamp pod resume %d ready: %w", id, err)
	}
	return nil
}

// MarkPodResumeFirstTurn stamps the woken role's first turn, which ends the resume R5 measures.
// It is written once, as the pod-ready stamp is.
func (s *Store) MarkPodResumeFirstTurn(ctx context.Context, id int64, at time.Time) error {
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update pod_resumes set first_turn_at = $2 where id = $1 and first_turn_at is null`, id, at)
		return err
	})
	if err != nil {
		return fmt.Errorf("stamp pod resume %d with its first turn: %w", id, err)
	}
	return nil
}

// MarkPodResumeStuck records a resume with no first turn ten minutes after its wake.
func (s *Store) MarkPodResumeStuck(ctx context.Context, id int64) error {
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update pod_resumes set stuck = true where id = $1`, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("mark pod resume %d stuck: %w", id, err)
	}
	return nil
}

// StuckPodResumes are the resumes whose stuck clock started at or before the bound, with no first
// turn and no stuck mark yet: the ones the daemon warns about and counts, read once per pass so
// each warns once. The stranded report reads UnfinishedPodResumesTx instead, which keeps a marked
// row.
func (s *Store) StuckPodResumes(ctx context.Context, project string, before time.Time) ([]PodResume, error) {
	return ReadOnly(ctx, s.pool, "read the unfinished resumes of "+project, func(ctx context.Context, tx pgx.Tx) ([]PodResume, error) {
		return podResumesWithoutAFirstTurn(ctx, tx, project, before, true)
	})
}

// UnfinishedPodResumesTx are the resumes of project the stranded report judges R6 on, read
// through tx's snapshot: those of each pod's newest release that suspended a role, whose stuck
// clock started at or before the bound and whose woken role has not taken its first turn, the
// ones a stuck pass has already marked included — a resume marked stuck is exactly a pod a wake
// never brought back, so dropping it would take the claim out of the report the moment the
// warning was written.
//
// A resume of an older release that suspended a role is left out, whatever became of it: the
// claim it was for has been released again since, which is a pod the daemon has just let go
// rather than one a wake never brought back. Reading those too would list a role as stranded
// the moment its next release was recorded.
//
// A release that suspended no role is no such later release, whichever rule wrote it, and the
// subquery's filter on a roles column that is not empty is what keeps it out. Two releases
// record their roles empty: every release of a pod no claim of the issue can use
// (workflow.UnownedPodRelease), which changes no claim, and the rare idle release whose every
// role stop found a claim that had failed or retired since the mark. After either one a role an
// earlier release left `released` is still released, still woken and still without a first
// turn, which is the stranded claim this read is for, and counting either as the pod's newest
// release would drop exactly those rows from the report's released-claim arm. So the filter is
// of what a release did and never of which rule wrote it.
//
// A resume a pause held pairs with no release (Release 0, decision 15 of dispatch://INFRA-3759),
// so the newest release names none of it. It is kept until a release that suspended a role is
// recorded after it, by the same filter: such a release has let its pod go again.
//
// The stranded report and the state route both read it beside the claims they judge it against,
// in the transaction they have already begun, so the wakes and the released claims they belong
// to come from one moment. There is no form of this read on the pool: every caller of it is
// already inside a transaction of this pool, and asking the pool for a second connection while
// holding one of its own is how a pool of a busy deployment deadlocks (INFRA-3169).
func (s *Store) UnfinishedPodResumesTx(ctx context.Context, tx pgx.Tx, project string, before time.Time) ([]PodResume, error) {
	return podResumesWithoutAFirstTurn(ctx, tx, project, before, false)
}

// podResumesWithoutAFirstTurn is the transaction read of the resumes an unowned pass or a
// stranded report needs. A resume a release closed (closed_at) is left out of each: it is a wake
// of a pod that is gone, which no warning and no stranded report has anything to say about.
//
// Each reads the resume's stuck clock (clock_at), never the wake's own moment (at), and passes
// over every resume of a project paused now: the pause holds the launch the resume waits for, and
// its resume starts the clock again.
func podResumesWithoutAFirstTurn(ctx context.Context, tx pgx.Tx, project string, before time.Time, unmarked bool) ([]PodResume, error) {
	query := `select id, release_id, project, issue, role, kind, at, clock_at, coalesce(pause_id, 0), stuck
		from pod_resumes r where project = $1 and first_turn_at is null and closed_at is null and clock_at <= $2
		and not exists (select 1 from project_pauses p where lower(p.project) = r.project and p.ended_at is null)`
	if unmarked {
		query += ` and not stuck`
	} else {
		query += ` and (release_id = (select max(id) from pod_releases
			where project = r.project and issue = r.issue and roles <> '')
			or (pause_id is not null and not exists (select 1 from pod_releases
				where project = r.project and issue = r.issue and roles <> '' and id > r.seen_release)))`
	}
	rows, err := tx.Query(ctx, query+` order by id`, project, before)
	if err != nil {
		return nil, fmt.Errorf("read the unfinished resumes of %s: %w", project, err)
	}
	defer rows.Close()
	var out []PodResume
	for rows.Next() {
		var r PodResume
		if err := rows.Scan(&r.ID, &r.Release, &r.Project, &r.Issue, &r.Role, &r.Kind, &r.At, &r.ClockAt, &r.Pause, &r.Stuck); err != nil {
			return nil, fmt.Errorf("read the unfinished resumes of %s: %w", project, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordPodHold records one pass of the idle rule that kept a pod past the idle period, as the
// first pass of a streak or as another pass of the one that is running: the row is keyed by the
// pod and the rule's reason, so a reason read again only moves the streak's end on and counts
// the pass. greatest keeps At the newest pass read, so a pass recorded out of order never moves
// the streak's end backwards.
//
// Every streak of that pod under another reason ends in the same transaction: the rule reads one
// reason per pass — the first condition that held the pod — so a pass that reads a new reason is
// a pass that no longer reads the old one.
func (s *Store) RecordPodHold(ctx context.Context, hold PodHold) error {
	if err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		return RecordPodHoldTx(ctx, tx, hold)
	}); err != nil {
		return fmt.Errorf("record the hold of %s: %w", hold.Issue, err)
	}
	return nil
}

// RecordPodHoldTx records hold inside the caller's transaction.
func RecordPodHoldTx(ctx context.Context, tx pgx.Tx, hold PodHold) error {
	if hold.Project == "" || hold.Issue == "" || hold.Reason == "" {
		return errors.New("a pod hold record needs its project, issue and reason")
	}
	if err := endPodHoldStreaks(ctx, tx, hold.Project, hold.Issue, hold.Reason); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `insert into pod_holds (project, issue, reason, detail, first_at, at, passes)
		values ($1, $2, $3, $4, $5, $5, 1)
		on conflict (project, issue, reason) where not ended do update
		set detail = excluded.detail, at = greatest(pod_holds.at, excluded.at), passes = pod_holds.passes + 1`,
		hold.Project, hold.Issue, hold.Reason, hold.Detail, hold.At)
	return err
}

// EndPodHoldStreaks ends every hold streak of a pod: the daemon's own call for a pass of the
// idle rule that held the pod for no reason worth a row — it was released, it is not idle yet,
// or it has no pod at all — after which the next pass that does read a reason opens a streak of
// its own. A pass that reads a reason records it instead (RecordPodHold), which ends the
// streaks of every other reason in the same transaction.
func (s *Store) EndPodHoldStreaks(ctx context.Context, project, issue string) error {
	if project == "" || issue == "" {
		return errors.New("ending a pod's hold streaks needs its project and issue")
	}
	if err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		return endPodHoldStreaks(ctx, tx, project, issue, "")
	}); err != nil {
		return fmt.Errorf("end the hold streaks of %s: %w", issue, err)
	}
	return nil
}

// OpenPodHold is whether a hold streak of the pod is running. The release of a pod no claim owns
// needs it before it opens its decision transaction, because an issue whose Sandbox the runtime
// no longer holds still has a streak to end. The answer bounds a write and nothing else: a pod
// with no streak is left alone, since an update that matches no row costs a write transaction all
// the same.
func (s *Store) OpenPodHold(ctx context.Context, project, issue string) (bool, error) {
	return ReadOnly(ctx, s.pool, "read whether a hold streak of "+issue+" is running", func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return openPodHold(ctx, tx, project, issue)
	})
}

// openPodHold is the question that both OpenPodHold and readIdleView ask, through the transaction
// they each own. One predicate for both keeps a view's Holding and this answer from reading two
// different questions.
func openPodHold(ctx context.Context, tx pgx.Tx, project, issue string) (bool, error) {
	var holding bool
	if err := tx.QueryRow(ctx, `select exists (
		select 1 from pod_holds where project = $1 and issue = $2 and not ended)`,
		project, issue).Scan(&holding); err != nil {
		return false, fmt.Errorf("read whether a hold streak of %s is running: %w", issue, err)
	}
	return holding, nil
}

// StandingPodHoldsTx is the standing hold of each pod of project, keyed by its issue, read
// through the caller's transaction: the hold streak of that pod that no pass, release or other
// reason has ended. The state route reads it in its own snapshot, so the holds it shows and every
// other fact of that state come from one moment, and it is one statement for the whole project
// rather than one read per issue.
//
// A pod holds one running streak at most, because RecordPodHold ends every other reason's
// streak in the transaction that records a reason. The rows that were running when
// 20261102154500_pod_hold_streaks gave a streak its end were all left running, one per reason a
// pod was ever held for, so a pod may still carry more than one; the read answers the streak of
// the newest pass, which is the reason the rule read last. A row's detail is answered whole:
// what of it a reader is shown is that reader's to cut.
func (s *Store) StandingPodHoldsTx(ctx context.Context, tx pgx.Tx, project string) (map[string]PodHold, error) {
	rows, err := tx.Query(ctx, `select distinct on (issue) id, project, issue, reason, detail, first_at, at, passes
		from pod_holds where project = $1 and not ended order by issue, at desc, id desc`, project)
	if err != nil {
		return nil, fmt.Errorf("read the standing pod holds of %s: %w", project, err)
	}
	defer rows.Close()
	holds := map[string]PodHold{}
	for rows.Next() {
		var h PodHold
		if err := rows.Scan(&h.ID, &h.Project, &h.Issue, &h.Reason, &h.Detail, &h.FirstAt, &h.At, &h.Passes); err != nil {
			return nil, fmt.Errorf("read the standing pod holds of %s: %w", project, err)
		}
		holds[h.Issue] = h
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the standing pod holds of %s: %w", project, err)
	}
	return holds, nil
}

// endPodHoldStreaks marks the running streaks of the pod ended through the caller's transaction.
// A streak already ended is left as it is, so its first_at, at and passes stay what the passes
// of that streak made them.
func endPodHoldStreaks(ctx context.Context, tx pgx.Tx, project, issue, except string) error {
	_, err := tx.Exec(ctx, `update pod_holds set ended = true
		where project = $1 and issue = $2 and reason <> $3 and not ended`, project, issue, except)
	return err
}

// PodReleaseRecords are every release, resume and hold of project recorded at or after since,
// oldest first, which is what a pod reads to measure a window.
func (s *Store) PodReleaseRecords(ctx context.Context, project string, since time.Time) (PodReleaseRecords, error) {
	return ReadOnly(ctx, s.pool, "read the pod release records of "+project, func(ctx context.Context, tx pgx.Tx) (PodReleaseRecords, error) {
		return podReleaseRecords(ctx, tx, project, since)
	})
}

func podReleaseRecords(ctx context.Context, tx pgx.Tx, project string, since time.Time) (PodReleaseRecords, error) {
	var out PodReleaseRecords
	releases, err := tx.Query(ctx, `select id, project, issue, generation, roles, at, bundle_bytes, coalesce(issue_release_id, 0)
		from pod_releases where project = $1 and at >= $2 order by id`, project, since)
	if err != nil {
		return out, fmt.Errorf("read the release records of %s: %w", project, err)
	}
	for releases.Next() {
		var r PodRelease
		var generation int64
		var roles string
		if err := releases.Scan(&r.ID, &r.Project, &r.Issue, &generation, &roles, &r.At, &r.BundleBytes, &r.Release); err != nil {
			releases.Close()
			return out, fmt.Errorf("read the release records of %s: %w", project, err)
		}
		r.Generation = uint64(generation)
		if roles != "" {
			r.Roles = strings.Split(roles, ",")
		}
		out.Releases = append(out.Releases, r)
	}
	releases.Close()
	if err := releases.Err(); err != nil {
		return out, fmt.Errorf("read the release records of %s: %w", project, err)
	}

	// Each resume carries the pause that held it (decision 15 of dispatch://INFRA-3759), so a
	// reader of the window tells a held wake's wait from a released pod's.
	resumes, err := tx.Query(ctx, `select id, release_id, project, issue, role, kind, at, clock_at, coalesce(pause_id, 0),
		pod_ready_at, first_turn_at, stuck
		from pod_resumes where project = $1 and at >= $2 order by id`, project, since)
	if err != nil {
		return out, fmt.Errorf("read the resume records of %s: %w", project, err)
	}
	for resumes.Next() {
		var r PodResume
		var ready, first *time.Time
		if err := resumes.Scan(&r.ID, &r.Release, &r.Project, &r.Issue, &r.Role, &r.Kind, &r.At, &r.ClockAt, &r.Pause,
			&ready, &first, &r.Stuck); err != nil {
			resumes.Close()
			return out, fmt.Errorf("read the resume records of %s: %w", project, err)
		}
		if ready != nil {
			r.PodReadyAt = *ready
		}
		if first != nil {
			r.FirstTurnAt = *first
		}
		out.Resumes = append(out.Resumes, r)
	}
	resumes.Close()
	if err := resumes.Err(); err != nil {
		return out, fmt.Errorf("read the resume records of %s: %w", project, err)
	}

	holds, err := tx.Query(ctx, `select id, project, issue, reason, detail, first_at, at, passes from pod_holds
		where project = $1 and at >= $2 order by id`, project, since)
	if err != nil {
		return out, fmt.Errorf("read the hold records of %s: %w", project, err)
	}
	defer holds.Close()
	for holds.Next() {
		var h PodHold
		if err := holds.Scan(&h.ID, &h.Project, &h.Issue, &h.Reason, &h.Detail, &h.FirstAt, &h.At, &h.Passes); err != nil {
			return out, fmt.Errorf("read the hold records of %s: %w", project, err)
		}
		out.Holds = append(out.Holds, h)
	}
	return out, holds.Err()
}
