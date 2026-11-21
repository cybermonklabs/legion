package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// ProjectReleaseEnder is who ended a project release (project_releases.ended_by).
type ProjectReleaseEnder string

// ProjectReleaseEndedByBoot is the one value of project_releases.ended_by: a boot of the daemon on
// this database ended the release (EndProjectReleasesAtBoot). Nothing else ends one.
const ProjectReleaseEndedByBoot ProjectReleaseEnder = "boot"

// errProjectReleaseRunning is an idle release, or the release of a pod no claim owns, refused
// because the project has a project release no boot has ended (ReleaseIdleIssue): that release
// holds every pod of the project until the daemon's next boot, and an idle release started beside
// it would stop roles and save work the project release has already taken over.
var errProjectReleaseRunning = errors.New("a project release no boot has ended holds every pod of the project")

// ErrNoUnfinishedProjectRelease is a write of a release's record refused because no release of its
// id is unfinished (RecordProjectRelease, FinishProjectRelease): it finished already, or no
// release holds that id. No later try of the same write changes that.
var ErrNoUnfinishedProjectRelease = errors.New("no unfinished release has that id")

// projectReleaseRunning is whether project has a release no boot has ended (ended_at null),
// finished or not, read in the caller's transaction. Its callers hold the shared serializer, as
// StartProjectRelease does, so the answer stands until they commit.
func projectReleaseRunning(ctx context.Context, tx pgx.Tx, project string) (bool, error) {
	var running bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from project_releases where project = $1 and ended_at is null)`,
		project).Scan(&running); err != nil {
		return false, fmt.Errorf("read whether a project release of %s runs: %w", project, err)
	}
	return running, nil
}

// ProjectRelease is one project release as its record holds it (project_releases): its id, the
// project in the claim token's spelling (claims.project), when it started, the time it deletes
// every pod still waiting (BoundAt), when it finished its acts and when a boot ended it, each zero
// until it happens, who ended it ("" until a boot did, then ProjectReleaseEndedByBoot), and its
// result as the release last wrote it. The result is the daemon's own JSON, which the store keeps
// and never reads: an object, `{}` until the release first records one.
type ProjectRelease struct {
	ID         int64
	Project    string
	StartedAt  time.Time
	BoundAt    time.Time
	FinishedAt time.Time
	EndedAt    time.Time
	EndedBy    ProjectReleaseEnder
	Result     json.RawMessage
}

// Runs is whether the release's acts still run: it has not finished them and no boot has ended
// it. A release reads each pod's unpushed work through a running role and asks its roles to push
// until it finishes; from then on it reads, asks, stops and deletes nothing more, and only its
// mark, which holds every start until the next boot, stands (internal/daemon's projectRelease). A
// pause is refused while a release runs (ProjectReleasingError) and taken once it finished.
func (r ProjectRelease) Runs() bool { return r.FinishedAt.IsZero() && r.EndedAt.IsZero() }

// TakenOverIdleRelease is one idle release the project release took over as it started
// (StartProjectRelease): the issue whose pod it was releasing, and the roles whose claims its own
// stops had already suspended as `released` (issue_releases.suspended), in the order they were
// recorded. Those claims stay `released`, and the caller arms the backstop the release's teardown
// would have armed for them, once the take-over has committed. The release of a pod no role can
// use (issue_releases.kind `unowned`) suspends no role, so it names none and arms nothing.
type TakenOverIdleRelease struct {
	Issue     string
	Suspended []claim.Role
}

// ProjectReleaseClaim is one claim of a project release's stop set: a claim of the project
// stored in a state whose process is up or coming up (supervise.LiveStates, launching through
// idle) when the release's record committed, with what the release stops it by and records it
// as: its token, tree, issue, role, session and the state it was stored in. The project's own
// controller is among them where the daemon runs it, on no issue and no tree.
type ProjectReleaseClaim struct {
	Token   claim.Token
	Tree    string
	Issue   string
	Role    claim.Role
	Session string
	State   supervise.ClaimState
}

// StartProjectRelease records a project release of project, started at startedAt and bound at
// boundAt, in one transaction with the two things the release must read from that same moment.
//
// It takes over every idle release of the project still in flight: each issue_releases mark of
// the project is deleted, and the outbox rows of that release still queued — its save
// (issue_bundle), its role stops (`release` supervise rows) and its teardown (issue_release),
// every one of them inside the mark's own range (releaseMark.holds) — are finished without
// acting, which for an outbox row is its delete. No bundle is saved, no role stopped and no pod
// torn down by them after this, and nothing of what the idle release already did is undone: a
// bundle it stored stays, and a claim its stops suspended stays `released`. A row of that release
// a lane is running now finds no mark when it next asks (IssueReleaseStop, IssueRelease), which is
// the release being over, so it acts on nothing more either. It answers each release it took
// over, so the caller can arm the backstops that release's teardown would have armed for the
// claims it had stopped.
//
// It answers the stop set as well: every claim of the project stored running or starting, in
// token order, read as the last statement of the transaction. That set is the one the release
// stops, and no later read: a claim that lands `released` after the commit, from an idle stop
// already past its fence or a held suspension whose turn ends, is still the release's to stop.
//
// The transaction takes the shared serializer first, as every writer of an idle release's mark
// does (ReleaseIdleIssue, CancelIssueRelease, IssueRelease), so the take-over and a mark being
// written or cancelled commit in a line and none is half taken over. The pause's transaction takes
// it too (Pause), so the read of the project's pause that comes next (releasePauseRefusal) and a
// pause commit in a line: a pause that committed first refuses this release with a
// *ProjectPausedError before anything is written, and a pause that comes after it reads this
// release's record and is refused (ProjectReleasingError).
func (s *Store) StartProjectRelease(ctx context.Context, project string, startedAt, boundAt time.Time) (ProjectRelease, []TakenOverIdleRelease, []ProjectReleaseClaim, error) {
	if project == "" {
		return ProjectRelease{}, nil, nil, errors.New("a project release needs its project")
	}
	if startedAt.IsZero() || boundAt.Before(startedAt) {
		return ProjectRelease{}, nil, nil, fmt.Errorf("a project release of %s needs a start and a bound no earlier than it, not %s and %s",
			project, startedAt.Format(time.RFC3339), boundAt.Format(time.RFC3339))
	}
	var (
		release   ProjectRelease
		takenOver []TakenOverIdleRelease
		stop      []ProjectReleaseClaim
	)
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		takenOver, stop = nil, nil
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		if refused, err := releasePauseRefusal(ctx, tx, project); err != nil {
			return err
		} else if refused != nil {
			return refused
		}
		var err error
		release, err = scanProjectRelease(tx.QueryRow(ctx, `insert into project_releases (project, started_at, bound_at)
			values ($1, $2, $3) returning `+projectReleaseColumns, project, startedAt, boundAt))
		if err != nil {
			return fmt.Errorf("record the release: %w", err)
		}
		if takenOver, err = takeOverIdleReleases(ctx, tx, project); err != nil {
			return err
		}
		stop, err = projectReleaseStopSet(ctx, tx, project)
		return err
	})
	if err != nil {
		return ProjectRelease{}, nil, nil, fmt.Errorf("start the project release of %s: %w", project, err)
	}
	return release, takenOver, stop, nil
}

// ProjectPausedError is a project release refused because a pause holds the project
// (dispatch://INFRA-3583 R13, and departure 1 of the plan of dispatch://INFRA-3759): the project is
// paused, or a pod whose roles a pause stopped is still held after the resume
// (project_pause_pod_holds), or both. A release reads a pod's unpushed work only through a running
// role, and deletes a pod in which no role runs with no read. A pod a pause stopped is such a pod,
// with the work its roles did not push on its disk, until a role of it runs again. So the release
// is refused before it records, stops or deletes anything.
type ProjectPausedError struct {
	// Project is the release's project, in the claim token's spelling.
	Project string
	// Pause is the project's open pause, and Paused whether one stands.
	Pause  Pause
	Paused bool
	// Held is each pod a pause still holds, in issue order.
	Held []HeldPausePod
}

// HeldPausePod is one pod a pause still holds, as a refused release names it: the pod's issue,
// the pause that holds it and since when, and the claims of the issue stored suspended by
// `released`, the source a pause stops a role by (supervise.SuspendedByReleased), in token order.
// The hold ends at the first ready of a role of the pod (EndLivePausePodHold), so an operator who
// resumes one of those claims (`legion claims resume --claim <token>`) ends it.
type HeldPausePod struct {
	Issue  string
	Pause  int64
	HeldAt time.Time
	Claims []claim.Token
}

func (e *ProjectPausedError) Error() string {
	var why []string
	if e.Paused {
		why = append(why, fmt.Sprintf("the project is paused (pause %d)", e.Pause.ID))
	}
	if len(e.Held) > 0 {
		issues := make([]string, len(e.Held))
		for i, held := range e.Held {
			issues[i] = held.Issue
		}
		why = append(why, "a pause holds the pods of "+strings.Join(issues, ", "))
	}
	return fmt.Sprintf("a project release of %s is refused: %s", e.Project, strings.Join(why, ", and "))
}

// PauseRefusesProjectRelease is the *ProjectPausedError with which the project's pause refuses a
// project release of project, the claim token's spelling, read on the pool, and nil while neither
// the pause nor a pod hold stands (releasePauseRefusal). The release asks it before it sets its
// mark, and a dry run before it reads anything; StartProjectRelease asks the same again inside its
// own transaction, across which no pause commits.
func (s *Store) PauseRefusesProjectRelease(ctx context.Context, project string) error {
	refused, err := releasePauseRefusal(ctx, s.pool, project)
	if err != nil {
		return err
	}
	if refused != nil {
		return refused
	}
	return nil
}

// pauseReader is the part of a pool and of a transaction a read of the project's pause goes
// through: one row, and many.
type pauseReader interface {
	queryRower
	rowsQuerier
}

// releasePauseRefusal reads through db what of the project's pause refuses a project release of
// project, the claim token's spelling: the open pause (readOpenPauseByToken), and each pod hold
// that stands (openPausePodHolds), with the claims of its issue stored suspended by `released`
// (heldPausePods). It answers nil while neither stands.
func releasePauseRefusal(ctx context.Context, db pauseReader, project string) (*ProjectPausedError, error) {
	open, paused, err := readOpenPauseByToken(ctx, db, project, "")
	if err != nil {
		return nil, err
	}
	holds, err := openPausePodHolds(ctx, db, project)
	if err != nil {
		return nil, err
	}
	if !paused && len(holds) == 0 {
		return nil, nil
	}
	held, err := heldPausePods(ctx, db, project, holds)
	if err != nil {
		return nil, err
	}
	return &ProjectPausedError{Project: project, Pause: open, Paused: paused, Held: held}, nil
}

// heldPausePods is each of holds as a refused release names it, in the order of holds: its issue,
// its pause and since when, and the claims of project, the claim token's spelling, on its issue
// stored suspended by `released` (supervise.SuspendedByReleased), read through q in token order
// and never nil. It reads nothing where holds is empty, and answers nil.
func heldPausePods(ctx context.Context, q rowsQuerier, project string, holds []PausePodHold) ([]HeldPausePod, error) {
	if len(holds) == 0 {
		return nil, nil
	}
	issues := make([]string, len(holds))
	for i, hold := range holds {
		issues[i] = hold.Issue
	}
	rows, err := q.Query(ctx, `select issue, token from claims
		where project = $1 and issue = any($2) and state = $3 and suspended_by = $4 order by token`,
		project, issues, string(supervise.StateSuspended), string(supervise.SuspendedByReleased))
	if err != nil {
		return nil, fmt.Errorf("read the claims of the pods a pause of %s holds: %w", project, err)
	}
	defer rows.Close()
	released := make(map[string][]claim.Token, len(holds))
	for rows.Next() {
		var issue, token string
		if err := rows.Scan(&issue, &token); err != nil {
			return nil, fmt.Errorf("read the claims of the pods a pause of %s holds: %w", project, err)
		}
		released[issue] = append(released[issue], claim.Token(token))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the claims of the pods a pause of %s holds: %w", project, err)
	}
	held := make([]HeldPausePod, len(holds))
	for i, hold := range holds {
		claims := released[hold.Issue]
		if claims == nil {
			claims = []claim.Token{}
		}
		held[i] = HeldPausePod{Issue: hold.Issue, Pause: hold.Pause, HeldAt: hold.HeldAt, Claims: claims}
	}
	return held, nil
}

// runningProjectReleaseTx is project's release whose acts still run (ProjectRelease.Runs), read
// in the caller's transaction, and whether one runs. The pause's transaction reads it under the
// shared serializer, which StartProjectRelease takes too, so a release whose record committed
// before that read is found, and one that starts after it reads the pause (releasePauseRefusal).
func runningProjectReleaseTx(ctx context.Context, tx pgx.Tx, project string) (ProjectRelease, bool, error) {
	release, found, err := readProjectRelease(tx.QueryRow(ctx, newestProjectRelease, project), project)
	if err != nil || !found || !release.Runs() {
		return ProjectRelease{}, false, err
	}
	return release, true, nil
}

// takeOverIdleReleases deletes every idle release mark of project and finishes the rows of each
// that are still queued, without acting (StartProjectRelease), and answers the releases it took
// over in issue order.
func takeOverIdleReleases(ctx context.Context, tx pgx.Tx, project string) ([]TakenOverIdleRelease, error) {
	rows, err := tx.Query(ctx, `with taken as (delete from issue_releases where project = $1
		returning issue, first_row, last_row, suspended, kind)
		select issue, first_row, last_row, suspended, kind from taken order by issue`, project)
	if err != nil {
		return nil, fmt.Errorf("take over the idle releases of %s: %w", project, err)
	}
	type takenMark struct {
		issue string
		mark  releaseMark
	}
	var marks []takenMark
	for rows.Next() {
		var taken takenMark
		if err := rows.Scan(&taken.issue, &taken.mark.first, &taken.mark.last, &taken.mark.suspended, &taken.mark.kind); err != nil {
			rows.Close()
			return nil, fmt.Errorf("take over the idle releases of %s: %w", project, err)
		}
		marks = append(marks, taken)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("take over the idle releases of %s: %w", project, err)
	}
	takenOver := make([]TakenOverIdleRelease, 0, len(marks))
	for _, taken := range marks {
		// The kinds and the op are the release's own rows and nothing else (daemon's
		// idleReleaseRows, unownedReleaseRows), so a row of the issue another transaction numbered
		// inside the range — a delivery, a start — is left to run.
		if _, err := tx.Exec(ctx, `delete from outbox where issue = $1 and id between $2 and $3
			and (kind in ('issue_bundle', 'issue_release') or (kind = 'supervise' and payload->>'op' = 'release'))`,
			taken.issue, taken.mark.first, taken.mark.last); err != nil {
			return nil, fmt.Errorf("finish the rows of the idle release of %s: %w", taken.issue, err)
		}
		takenOver = append(takenOver, TakenOverIdleRelease{Issue: taken.issue, Suspended: taken.mark.roles()})
	}
	return takenOver, nil
}

// ProjectReleaseStopSet is the stop set a project release of project would read now
// (projectReleaseStopSet), read through the pool: a dry run's, which records nothing and so reads
// in no transaction of its own.
func (s *Store) ProjectReleaseStopSet(ctx context.Context, project string) ([]ProjectReleaseClaim, error) {
	return projectReleaseStopSet(ctx, s.pool, project)
}

// projectReleaseStopSet is every claim of project stored in a state whose process is up or coming
// up (supervise.LiveStates), in token order: a project release's stop set (StartProjectRelease).
// The states are supervise's, bound as text (claimStateNames), so the store names none of them
// itself.
func projectReleaseStopSet(ctx context.Context, q rowsQuerier, project string) ([]ProjectReleaseClaim, error) {
	rows, err := q.Query(ctx, `select token, tree, issue, role, session, state from claims
		where project = $1 and state = any($2) order by token`, project, claimStateNames(supervise.LiveStates()))
	if err != nil {
		return nil, fmt.Errorf("read the claims the project release of %s stops: %w", project, err)
	}
	defer rows.Close()
	var stop []ProjectReleaseClaim
	for rows.Next() {
		var c ProjectReleaseClaim
		if err := rows.Scan(&c.Token, &c.Tree, &c.Issue, &c.Role, &c.Session, &c.State); err != nil {
			return nil, fmt.Errorf("read the claims the project release of %s stops: %w", project, err)
		}
		stop = append(stop, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the claims the project release of %s stops: %w", project, err)
	}
	return stop, nil
}

// RecordProjectRelease writes result over the result the release of id recorded before, while
// the release has not finished: the result grows as the release acts, and a reader of the record
// sees the newest of it. A release that finished, or an id no release holds, is refused, since a
// result written after the finish would change what the finish recorded.
func (s *Store) RecordProjectRelease(ctx context.Context, id int64, result json.RawMessage) error {
	if len(result) == 0 {
		return fmt.Errorf("record the result of project release %d: the result is empty", id)
	}
	tag, err := s.pool.Exec(ctx, `update project_releases set result = $2::jsonb where id = $1 and finished_at is null`, id, string(result))
	if err != nil {
		return fmt.Errorf("record the result of project release %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record the result of project release %d: %w", id, ErrNoUnfinishedProjectRelease)
	}
	return nil
}

// FinishProjectRelease records that the release of id finished its acts at at, with result, its
// last, in one write, so no reader sees a release finished beside a result older than its end. A
// release already finished, or an id no release holds, is refused.
func (s *Store) FinishProjectRelease(ctx context.Context, id int64, result json.RawMessage, at time.Time) error {
	if len(result) == 0 {
		return fmt.Errorf("finish project release %d: the result is empty", id)
	}
	tag, err := s.pool.Exec(ctx, `update project_releases set result = $2::jsonb, finished_at = $3
		where id = $1 and finished_at is null`, id, string(result), at)
	if err != nil {
		return fmt.Errorf("finish project release %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finish project release %d: %w", id, ErrNoUnfinishedProjectRelease)
	}
	return nil
}

// EndProjectReleasesAtBoot ends every release of project that no boot has ended yet, finished or
// not, stamping at and ProjectReleaseEndedByBoot on each, and answers how many it ended. The boot
// calls it right after the migration and before anything of the workflow runs: a release lives
// in the process that started it, so a row no boot has ended is one an earlier process left, and
// its holds on every start end with that process. The claims it stopped stay suspended by
// `project_release`, which is what the boot reads to start them again (workflow's booted), so
// nothing of them is lost here.
//
// It ends the project's rows alone. One database can hold the records of several projects'
// daemons, and the boot of one ends no release another daemon is running.
func (s *Store) EndProjectReleasesAtBoot(ctx context.Context, project string, at time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `update project_releases set ended_at = $2, ended_by = $3
		where project = $1 and ended_at is null`, project, at, ProjectReleaseEndedByBoot)
	if err != nil {
		return 0, fmt.Errorf("end the project releases of %s at the boot: %w", project, err)
	}
	return tag.RowsAffected(), nil
}

// ReadProjectRelease is the newest release of project, and whether one was ever recorded.
func (s *Store) ReadProjectRelease(ctx context.Context, project string) (ProjectRelease, bool, error) {
	return readProjectRelease(s.pool.QueryRow(ctx, newestProjectRelease, project), project)
}

// ReadProjectReleaseTx is ReadProjectRelease through tx, for a reader whose answer must come from
// the same snapshot as everything else it reads: the state route's (api.State.ProjectRelease).
func (s *Store) ReadProjectReleaseTx(ctx context.Context, tx pgx.Tx, project string) (ProjectRelease, bool, error) {
	return readProjectRelease(tx.QueryRow(ctx, newestProjectRelease, project), project)
}

// newestProjectRelease reads the newest release of the project $1.
const newestProjectRelease = `select ` + projectReleaseColumns + ` from project_releases
		where project = $1 order by id desc limit 1`

// readProjectRelease scans row, newestProjectRelease's one row, answering no row as no release.
func readProjectRelease(row pgx.Row, project string) (ProjectRelease, bool, error) {
	release, err := scanProjectRelease(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectRelease{}, false, nil
	}
	if err != nil {
		return ProjectRelease{}, false, fmt.Errorf("read the newest project release of %s: %w", project, err)
	}
	return release, true, nil
}

// projectReleaseColumns are the columns scanProjectRelease reads, in its order.
const projectReleaseColumns = `id, project, started_at, bound_at, finished_at, ended_at, coalesce(ended_by, ''), result`

// scanProjectRelease reads one row of projectReleaseColumns.
func scanProjectRelease(row pgx.Row) (ProjectRelease, error) {
	var (
		release         ProjectRelease
		finished, ended *time.Time
		result          []byte
	)
	if err := row.Scan(&release.ID, &release.Project, &release.StartedAt, &release.BoundAt, &finished, &ended,
		&release.EndedBy, &result); err != nil {
		return ProjectRelease{}, err
	}
	release.FinishedAt, release.EndedAt = orZero(finished), orZero(ended)
	release.Result = json.RawMessage(result)
	return release, nil
}
