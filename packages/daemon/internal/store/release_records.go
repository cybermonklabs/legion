package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PodRelease is one release of an issue pod: which roles were in it, when their claims were
// suspended as released, and how large the workspace bundle it saved was. It is the row every
// resume of that pod hangs from, so a reader can pair a release with what woke it and how long
// the agent waited.
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
	// BundleBytes is the size of the bundle the release saved; zero when it saved none because
	// the workspace held nothing unpushed.
	BundleBytes int
	// WakeTask is the workflow's rule for the task of a wake taken under the release's mark
	// (workflow.ReleasedWakeTask), which RecordPodRelease uses and no read fills: it answers, from
	// the kind of the wake's resume and the task its start carries, the task that start carries
	// from now on. The store does not restate the wake's words. nil rewrites no task.
	WakeTask func(kind, task string) string
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
// The record is also the mark's one-way step: it writes the release's id on the mark
// (issue_releases.released_id), in the transaction that inserts the row. The teardown records the
// release once the runtime has answered it without an error, before the teardown's own later
// steps (internal/daemon's releaseIssue), so a mark that names its release is a release whose pod
// is gone. A teardown that runs again after one of those steps failed reads it: IssueRelease
// answers ReleaseActs and the runtime is not asked again, and a cancel refuses the mark
// (CancelIssueRelease). A step of the runtime that fails after the pod is gone comes before the
// record; the mark's commit (CommitIssueRelease) keeps that release from being cancelled. Its
// record answers the id the mark names and writes nothing, so the release has one row, and the
// wakes it pointed at itself and retold stay as the first record left them. With no mark, as in a
// record a test writes alone, there is nothing to stamp, and each call writes a row.
func (s *Store) RecordPodRelease(ctx context.Context, rel PodRelease) (int64, error) {
	if rel.Project == "" || rel.Issue == "" {
		return 0, errors.New("a pod release record needs its project and issue")
	}
	var id int64
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		id = 0
		var recorded int64
		err := tx.QueryRow(ctx, `select released_id from issue_releases where project = $1 and issue = $2 for update`,
			rel.Project, rel.Issue).Scan(&recorded)
		marked := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock the release mark: %w", err)
		}
		if recorded != 0 {
			id = recorded
			return nil
		}
		if _, _, err := LockOpenPauseByTokenTx(ctx, tx, rel.Project); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `insert into pod_releases (project, issue, generation, roles, at, bundle_bytes)
			values ($1, $2, $3, $4, $5, $6) returning id`,
			rel.Project, rel.Issue, int64(rel.Generation), strings.Join(rel.Roles, ","), rel.At, rel.BundleBytes).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update pod_resumes r set release_id = case when r.pause_id is null then $3::bigint else 0 end,
			seen_release = $3, at = greatest(r.at, $4), clock_at = greatest(r.clock_at, $4)
			from issue_releases m where m.project = $1 and m.issue = $2 and r.project = m.project and r.issue = m.issue
			and r.id > m.wake_row and r.role = any(m.suspended) and r.first_turn_at is null and r.closed_at is null`,
			rel.Project, rel.Issue, id, rel.At); err != nil {
			return fmt.Errorf("point the wakes taken during the release at it: %w", err)
		}
		if err := retellWakesUnderRelease(ctx, tx, rel.Project, rel.Issue, rel.WakeTask); err != nil {
			return err
		}
		if marked {
			if _, err := tx.Exec(ctx, `update issue_releases set released_id = $3 where project = $1 and issue = $2`,
				rel.Project, rel.Issue, id); err != nil {
				return fmt.Errorf("write the release on its mark: %w", err)
			}
		}
		return endPodHoldStreaks(ctx, tx, rel.Project, rel.Issue, "")
	})
	if err != nil {
		return 0, fmt.Errorf("record the release of %s: %w", rel.Issue, err)
	}
	return id, nil
}

// retellWakesUnderRelease rewrites the task of each start that a wake taken under the release's
// mark queued, for the release's record (RecordPodRelease). The wakes are the ones the record
// points at itself: a resume of a role of the mark's suspended set above the mark's wake_row,
// with no first turn and not closed. Their starts are the starts of that role above the mark's
// last_row that have a wake's own shape (wakeStartRow). A root architect's entry start and a
// production check's look are no wake's start, and keep their task. retell answers the task a
// start carries from now on, from the kind of the role's newest such resume and the task the
// start carries now (PodRelease.WakeTask). A start of a role with no such resume is no such
// wake's, and keeps its task. With no mark or a nil retell, it writes nothing.
func retellWakesUnderRelease(ctx context.Context, tx pgx.Tx, project, issue string, retell func(kind, task string) string) error {
	if retell == nil {
		return nil
	}
	return retellStarts(ctx, tx, issue, retell, `select outbox.id, coalesce(outbox.payload->>'task', ''), woken.kind
		from issue_releases m join outbox on outbox.issue = m.issue and outbox.id > m.last_row and `+wakeStartRow+`
			and outbox.payload->>'role' = any(m.suspended)
		join lateral (select r.kind from pod_resumes r where r.project = m.project and r.issue = m.issue
			and r.id > m.wake_row and r.role = outbox.payload->>'role' and r.first_turn_at is null and r.closed_at is null
			order by r.id desc limit 1) woken on true
		where m.project = $1 and m.issue = $2
		order by outbox.id`, project, issue)
}

// MarkPodResumeReady stamps when the replacement pod was Ready. It is written once, by the first
// observation: a second observation of the same resume leaves the first time, which is the one
// R5 measures.
func (s *Store) MarkPodResumeReady(ctx context.Context, id int64, at time.Time) error {
	_, err := s.pool.Exec(ctx, `update pod_resumes set pod_ready_at = $2 where id = $1 and pod_ready_at is null`, id, at)
	if err != nil {
		return fmt.Errorf("stamp pod resume %d ready: %w", id, err)
	}
	return nil
}

// MarkPodResumeFirstTurn stamps the woken role's first turn, which ends the resume R5 measures.
// It is written once, as the pod-ready stamp is.
func (s *Store) MarkPodResumeFirstTurn(ctx context.Context, id int64, at time.Time) error {
	_, err := s.pool.Exec(ctx, `update pod_resumes set first_turn_at = $2 where id = $1 and first_turn_at is null`, id, at)
	if err != nil {
		return fmt.Errorf("stamp pod resume %d with its first turn: %w", id, err)
	}
	return nil
}

// MarkPodResumeStuck records a resume with no first turn ten minutes after its wake.
func (s *Store) MarkPodResumeStuck(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `update pod_resumes set stuck = true where id = $1`, id)
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
	return podResumesWithoutAFirstTurn(ctx, s.pool, project, before, true)
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

// podResumesWithoutAFirstTurn is the pool read and the transaction read above, on whichever
// querier the caller runs it on: the pool, or the caller's own transaction. A resume a release
// closed (closed_at) is left out of each: it is a wake of a pod that is gone, which no warning
// and no stranded report has anything to say about, and before closing had a column of its own
// such a row was indeed gone from here, because closing wrote first_turn_at.
//
// Each reads the resume's stuck clock (clock_at), never the wake's own moment (at), and passes
// over every resume of a project paused now (decision 14 of dispatch://INFRA-3759): the pause
// holds the launch the resume waits for, and its resume starts the clock again. The pause is the
// daemon's project's, matched to the resume's project token as readOpenPauseByToken matches it,
// which says why.
func podResumesWithoutAFirstTurn(ctx context.Context, q rowsQuerier, project string, before time.Time, unmarked bool) ([]PodResume, error) {
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
	rows, err := q.Query(ctx, query+` order by id`, project, before)
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
	if hold.Project == "" || hold.Issue == "" || hold.Reason == "" {
		return errors.New("a pod hold record needs its project, issue and reason")
	}
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := endPodHoldStreaks(ctx, tx, hold.Project, hold.Issue, hold.Reason); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into pod_holds (project, issue, reason, detail, first_at, at, passes)
			values ($1, $2, $3, $4, $5, $5, 1)
			on conflict (project, issue, reason) where not ended do update
			set detail = excluded.detail, at = greatest(pod_holds.at, excluded.at), passes = pod_holds.passes + 1`,
			hold.Project, hold.Issue, hold.Reason, hold.Detail, hold.At)
		return err
	})
	if err != nil {
		return fmt.Errorf("record the hold of %s: %w", hold.Issue, err)
	}
	return nil
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
	if err := s.Tx(ctx, func(tx pgx.Tx) error {
		return endPodHoldStreaks(ctx, tx, project, issue, "")
	}); err != nil {
		return fmt.Errorf("end the hold streaks of %s: %w", issue, err)
	}
	return nil
}

// OpenPodHold is whether a hold streak of the pod is running, on the pool: the same question
// readIdleView answers as IdleView.Holding, asked outside any transaction. The release of a pod
// no claim owns needs it there, because the one pass that reads no view at all — the issue whose
// Sandbox the runtime no longer holds — still has a streak to end, and opening ReleaseIdleIssue
// to learn that would cost the transaction, and the daemon's global fact serializer with it,
// that reading the Sandbox first exists to save (daemon.releaseUnownedPod). The answer bounds a
// write and nothing else: a pod with no streak is left alone, since an update that matches no
// row costs a write transaction all the same.
func (s *Store) OpenPodHold(ctx context.Context, project, issue string) (bool, error) {
	return openPodHold(ctx, s.pool, project, issue)
}

// openPodHold is that question as one statement, read through db: the pool for OpenPodHold's
// own read, and the release's own transaction for the idle view it decides on (readIdleView,
// pods.IdleView.Holding). One predicate for both, so a reader of a view's Holding and a reader
// of the answer outside a transaction can never be reading two different questions.
func openPodHold(ctx context.Context, db queryRower, project, issue string) (bool, error) {
	var holding bool
	if err := db.QueryRow(ctx, `select exists (
		select 1 from pod_holds where project = $1 and issue = $2 and not ended)`,
		project, issue).Scan(&holding); err != nil {
		return false, fmt.Errorf("read whether a hold streak of %s is running: %w", issue, err)
	}
	return holding, nil
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
	var out PodReleaseRecords
	releases, err := s.pool.Query(ctx, `select id, project, issue, generation, roles, at, bundle_bytes
		from pod_releases where project = $1 and at >= $2 order by id`, project, since)
	if err != nil {
		return out, fmt.Errorf("read the release records of %s: %w", project, err)
	}
	for releases.Next() {
		var r PodRelease
		var generation int64
		var roles string
		if err := releases.Scan(&r.ID, &r.Project, &r.Issue, &generation, &roles, &r.At, &r.BundleBytes); err != nil {
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
	resumes, err := s.pool.Query(ctx, `select id, release_id, project, issue, role, kind, at, clock_at, coalesce(pause_id, 0),
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

	holds, err := s.pool.Query(ctx, `select id, project, issue, reason, detail, first_at, at, passes from pod_holds
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
