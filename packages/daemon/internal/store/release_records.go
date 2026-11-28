package store

import (
	"context"
	"errors"
	"fmt"
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
	// Release is the id of the release's mark (issue_releases.id) the record stands for,
	// pod_releases.issue_release_id; zero for a record that names no mark. A release is recorded
	// once per mark (RecordPodRelease), and a record read back names the mark it was written with.
	Release int64
}

// PodResume is one wake of a released pod and what it cost: which wake brought it back, when the
// wake reached the daemon, when the pod was Ready, and when the woken role took its first turn.
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
	// At is when the wake reached the daemon or the Envoy listener.
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
// A record that names its release's mark (rel.Release) is written once per mark
// (pod_releases_one_per_issue_release): the teardown records the release and clears the mark in
// transactions of their own, so a teardown that failed, or whose daemon died, between the two runs
// again and records the release again. That second write takes the record the first one wrote,
// answers its id with wrote false and ends no streak, since the first one ended them. wrote is
// true where this call wrote the row, which a record that names no mark always does.
func (s *Store) RecordPodRelease(ctx context.Context, rel PodRelease) (id int64, wrote bool, err error) {
	if rel.Project == "" || rel.Issue == "" {
		return 0, false, errors.New("a pod release record needs its project and issue")
	}
	err = Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		wrote = false
		err := tx.QueryRow(ctx, `insert into pod_releases (project, issue, generation, roles, at, bundle_bytes, issue_release_id)
			values ($1, $2, $3, $4, $5, $6, nullif($7::bigint, 0))
			on conflict (issue_release_id) where issue_release_id is not null do nothing returning id`,
			rel.Project, rel.Issue, int64(rel.Generation), strings.Join(rel.Roles, ","), rel.At, rel.BundleBytes, rel.Release).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx, `select id from pod_releases where issue_release_id = $1`, rel.Release).Scan(&id)
		}
		if err != nil {
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
// A queued launch has not been let out, so its clock is not running either.
func podResumesWithoutAFirstTurn(ctx context.Context, tx pgx.Tx, project string, before time.Time, unmarked bool) ([]PodResume, error) {
	query := `select id, release_id, project, issue, role, kind, at, clock_at, coalesce(pause_id, 0), stuck
		from pod_resumes r where project = $1 and first_turn_at is null and closed_at is null and clock_at <= $2
		and not queued and not exists (select 1 from project_pauses p where lower(p.project) = r.project and p.ended_at is null)`
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
