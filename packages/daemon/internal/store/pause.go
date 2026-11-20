package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The records of a project's pause (dispatch://INFRA-3759, decision 2 of its plan): each pause
// (project_pauses), each role a pause asks to stop (project_pause_stops) and each pod a pause
// keeps (project_pause_pod_holds), as migrations/20261115194357_project_pause.up.sql defines
// them. A pause's project is the project's Dispatch key ("LEGION"), the spelling the outbox reads
// off an issue key; the claim-keyed and pod-keyed tables hold the project token ("legion",
// claim.ProjectToken), and each function below says which spelling it takes.

// PauseActorKind is who can act on a pause: the project's controller or the operator.
type PauseActorKind string

const (
	// PauseByController is the project's controller, which names the session it acts from.
	PauseByController PauseActorKind = "controller"
	// PauseByOperator is the operator, through the operator's bearer, which names no session.
	PauseByOperator PauseActorKind = "operator"
)

// PauseActor is who paused a project or who ended its pause: the controller with its session id,
// or the operator with none. The table's checks hold the same shape (project_pauses_by_session,
// project_pauses_ended_by_session), and validate says it before the write does.
type PauseActor struct {
	Kind    PauseActorKind
	Session string
}

func (a PauseActor) validate() error {
	switch a.Kind {
	case PauseByController:
		if a.Session == "" {
			return errors.New("the controller acts on a pause from its session, and names none")
		}
	case PauseByOperator:
		if a.Session != "" {
			return fmt.Errorf("the operator acts on a pause with no session, and names %q", a.Session)
		}
	default:
		return fmt.Errorf("%q cannot act on a pause: only the controller and the operator can", a.Kind)
	}
	return nil
}

// Pause is one pause of a project. EndedAt and EndedBy are zero while it is open.
type Pause struct {
	ID int64
	// Project is the project's Dispatch key.
	Project string
	Since   time.Time
	By      PauseActor
	Reason  string
	EndedAt time.Time
	EndedBy PauseActor
}

// Ended is whether a resume has ended the pause.
func (p Pause) Ended() bool { return !p.EndedAt.IsZero() }

// AlreadyPausedError is a pause refused because the project's pause stands: the project has at
// most one open pause (project_pauses_one_open). Standing is that pause, which the refusal names.
type AlreadyPausedError struct {
	Standing Pause
}

func (e *AlreadyPausedError) Error() string {
	return fmt.Sprintf("project %s is already paused: pause %d, by %s, since %s",
		e.Standing.Project, e.Standing.ID, e.Standing.By.Kind, e.Standing.Since.UTC().Format(time.RFC3339))
}

// NotPausedError is a resume refused because the project has no open pause to end.
type NotPausedError struct {
	Project string
}

func (e *NotPausedError) Error() string {
	return fmt.Sprintf("project %s is not paused", e.Project)
}

// PauseRow is one outbox row a pause or a resume queues, encoded by its caller
// (record.NewOutboxRow). The store does not import record, whose own tests import the store, so
// the payload is the caller's to build, as every row of an idle release is (IdleReleaseRow).
// Issue is the row's key (record.OutboxRow.Issue), which its builder decides: a stop's row is
// keyed by its claim's issue or, for a claim of another project's issue, by the project
// (pause.StopKey in internal/pause), and a controller's notice by the project.
type PauseRow struct {
	Issue   string
	Kind    string
	Payload []byte
}

// PauseStopClaim is one live claim a pause asks to stop, as the claims table holds it.
type PauseStopClaim struct {
	Claim      claim.Token
	Tree       string
	Issue      string
	Role       claim.Role
	Generation uint64
}

// PauseRequest is what the pause's transaction is given.
type PauseRequest struct {
	// Project is the project's Dispatch key.
	Project string
	By      PauseActor
	// Reason is why the project is paused, which may not be empty or white space alone.
	Reason string
	// At is when the pause begins, on the daemon's clock.
	At time.Time
	// StopRow builds the `pause_stop` row of one claim the pause asks to stop: a `supervise` row
	// keyed by the claim's issue or by the project (PauseRow.Issue), which runs in that key's
	// lifecycle lane as a `suspend` row does.
	StopRow func(pause Pause, stop PauseStopClaim) (PauseRow, error)
	// Notice builds the controller's notice of the pause, queued under the project's key, or is
	// nil to queue none: the operator's pause tells the controller, and the controller's own pause
	// does not (decision 24).
	Notice func(pause Pause) (PauseRow, error)
}

// Pause is the pause's one transaction (decision 3 of dispatch://INFRA-3759). It takes the
// daemon's fact serializer first (treelifecycle.Serialize), as ReleaseIdleIssue does, so a pause
// and a fact's writes never interleave. It refuses a second open pause with an
// *AlreadyPausedError naming the pause that stands, writes the pause row, reads the project's live
// claims — every claim of the project that is not suspended, failed or retired, the controller's
// claim excepted (claim.IsController) — and writes for each one stop row and one `pause_stop`
// outbox row (req.StopRow), then queues the controller's notice when req.Notice is given. It
// answers the pause and the claims it asked to stop, in token order.
func (s *Store) Pause(ctx context.Context, req PauseRequest) (Pause, []PauseStopClaim, error) {
	if err := req.By.validate(); err != nil {
		return Pause{}, nil, fmt.Errorf("pause %s: %w", req.Project, err)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return Pause{}, nil, fmt.Errorf("pause %s: a pause needs a reason", req.Project)
	}
	if req.At.IsZero() || req.StopRow == nil {
		return Pause{}, nil, fmt.Errorf("pause %s: a pause needs its moment and the builder of its stop rows", req.Project)
	}
	token, err := claim.ProjectToken(req.Project)
	if err != nil {
		return Pause{}, nil, fmt.Errorf("pause %s: %w", req.Project, err)
	}
	var pause Pause
	var stops []PauseStopClaim
	err = Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		pause, stops = Pause{}, nil
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		standing, open, err := readOpenPause(ctx, tx, req.Project, "")
		if err != nil {
			return err
		}
		if open {
			return &AlreadyPausedError{Standing: standing}
		}
		if pause, err = scanPause(tx.QueryRow(ctx, `insert into project_pauses (project, began_at, by_kind, by_session, reason)
			values ($1, $2, $3, $4, $5) returning `+pauseColumns,
			req.Project, req.At, string(req.By.Kind), req.By.Session, req.Reason)); err != nil {
			return fmt.Errorf("write the pause: %w", err)
		}
		live, err := liveClaimsOf(ctx, tx, token)
		if err != nil {
			return err
		}
		for _, stop := range live {
			if _, err := OpenPauseStopTx(ctx, tx, pause, stop, req.At); err != nil {
				return err
			}
			if err := queuePauseStopRow(ctx, tx, pause, stop, req.StopRow); err != nil {
				return err
			}
			stops = append(stops, stop)
		}
		if req.Notice != nil {
			return queuePauseNotice(ctx, tx, pause, req.Notice)
		}
		return nil
	})
	if err != nil {
		return Pause{}, nil, fmt.Errorf("pause %s: %w", req.Project, err)
	}
	return pause, stops, nil
}

// queuePauseStopRow queues pause's `pause_stop` outbox row of one claim, built by stopRow
// (PauseRequest.StopRow), under the key the builder gave it, which runs it in that key's
// lifecycle lane: the claim's issue, or the pause's project for a claim of another project's
// issue. A row keyed by anything else would be run by no daemon's outbox or by another's, so it
// is refused, and so is a row keyed by another project's issue (insertOutboxRow,
// claim.KeyOfProject).
func queuePauseStopRow(ctx context.Context, tx pgx.Tx, pause Pause, stop PauseStopClaim, stopRow func(Pause, PauseStopClaim) (PauseRow, error)) error {
	row, err := stopRow(pause, stop)
	if err != nil {
		return fmt.Errorf("build the stop of %s: %w", stop.Claim, err)
	}
	if row.Issue != stop.Issue && row.Issue != pause.Project {
		return fmt.Errorf("the stop of %s is keyed by %q, neither its issue %s nor the project %s", stop.Claim, row.Issue, stop.Issue, pause.Project)
	}
	_, err = insertOutboxRow(ctx, tx, pause.Project, row.Issue, IdleReleaseRow{Kind: row.Kind, Payload: row.Payload})
	return err
}

// AskPauseStops asks, in one transaction, for pause's stop of each of claims the pause has asked
// no stop of yet: one stop row (OpenPauseStopTx) and one `pause_stop` outbox row (stopRow, the
// builder the pause's own transaction takes, PauseRequest.StopRow) per claim, as the pause's
// transaction writes them. It is the daemon's catch-up once a pause has committed: the claims
// its machines hold live that the transaction did not name, such as a launch that passed the gate
// before the flag was set (decision 3 of dispatch://INFRA-3759). It answers the claims it asked
// to stop; see askPauseStopsTx for the claims it passes over.
func (s *Store) AskPauseStops(ctx context.Context, pause int64, claims []PauseStopClaim, at time.Time,
	stopRow func(Pause, PauseStopClaim) (PauseRow, error)) ([]PauseStopClaim, error) {
	var asked []PauseStopClaim
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		held, err := LockPauseTx(ctx, tx, pause)
		if err != nil {
			return err
		}
		asked, err = askPauseStopsTx(ctx, tx, held, claims, at, stopRow)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("ask pause %d's stops: %w", pause, err)
	}
	return asked, nil
}

// AskLivePauseStops is AskPauseStops over every live claim of the pause's project as the claims
// table holds it in the same transaction (liveClaimsOf: not suspended, failed or retired, the
// controller's excepted): the boot's pass while the project is paused (decision 18 and R5 of
// dispatch://INFRA-3759), run once the supervisor has restored every claim, so a role a restart
// finds live is stopped for the pause whatever the daemon that stopped knew of it.
func (s *Store) AskLivePauseStops(ctx context.Context, pause int64, at time.Time,
	stopRow func(Pause, PauseStopClaim) (PauseRow, error)) ([]PauseStopClaim, error) {
	var asked []PauseStopClaim
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		held, err := LockPauseTx(ctx, tx, pause)
		if err != nil {
			return err
		}
		token, err := claim.ProjectToken(held.Project)
		if err != nil {
			return err
		}
		live, err := liveClaimsOf(ctx, tx, token)
		if err != nil {
			return err
		}
		asked, err = askPauseStopsTx(ctx, tx, held, live, at, stopRow)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("ask pause %d's stops of the live claims: %w", pause, err)
	}
	return asked, nil
}

// askPauseStopsTx opens held's stop of each of claims at at and queues its `pause_stop` row, and
// answers the claims it did. It asks nothing of a pause that has ended, since its stops would
// finish without acting (decision 4), and it passes over a claim of which held already has a
// stop, open or ended: an open one has its row queued already, and an ended one is a stop this
// pause already made, which a second row would only ask again. The caller has locked held's row
// for share (LockPauseTx), so a resume of it waits for this transaction.
func askPauseStopsTx(ctx context.Context, tx pgx.Tx, held Pause, claims []PauseStopClaim, at time.Time,
	stopRow func(Pause, PauseStopClaim) (PauseRow, error)) ([]PauseStopClaim, error) {
	if held.Ended() {
		return nil, nil
	}
	var asked []PauseStopClaim
	for _, stop := range claims {
		var stopped bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from project_pause_stops where pause_id = $1 and claim = $2)`,
			held.ID, string(stop.Claim)).Scan(&stopped); err != nil {
			return nil, fmt.Errorf("read pause %d's stop of %s: %w", held.ID, stop.Claim, err)
		}
		if stopped {
			continue
		}
		opened, err := OpenPauseStopTx(ctx, tx, held, stop, at)
		if err != nil {
			return nil, err
		}
		if !opened {
			continue
		}
		if err := queuePauseStopRow(ctx, tx, held, stop, stopRow); err != nil {
			return nil, err
		}
		asked = append(asked, stop)
	}
	return asked, nil
}

// ResumeRequest is what the resume's transaction is given.
type ResumeRequest struct {
	// Project is the project's Dispatch key.
	Project string
	By      PauseActor
	// At is when the resume ends the pause, on the daemon's clock.
	At time.Time
	// MarkHeld, when given, marks in the resume's transaction each outbox row the pause holds
	// with the pause, held, and reads those rows for the resume's log
	// (record.Postgres.MarkPauseHeldRows). It runs once the pause's row is locked and before it is
	// ended, so the outbox's rule still holds every row it marks, and no lane can run one of them
	// before the resume commits; it keeps what it read itself: the store does not import record.
	MarkHeld func(ctx context.Context, tx pgx.Tx, held Pause) error
	// Notice builds the controller's notice of the resume from the pause it ended, queued under
	// the project's key, or is nil to queue none.
	Notice func(ended Pause) (PauseRow, error)
}

// Resumed is what the resume's transaction did.
type Resumed struct {
	// Pause is the pause the resume ended, with its end.
	Pause Pause
	// Cancelled are the stops the pause still waited on, which the resume ended `cancelled`.
	Cancelled []PauseStop
	// DeletedStopRows counts the pause's `pause_stop` outbox rows that had not run, which the
	// resume deleted.
	DeletedStopRows int64
	// Clocks counts the project's open resumes whose stuck clock the resume set.
	Clocks int64
}

// Resume is the resume's one transaction (decision 22 of dispatch://INFRA-3759). It takes no
// treelifecycle.Serialize (decision 13). It locks the project's open pause row for update, which
// waits for every stop transaction that holds that row for share (LockPauseTx), and refuses with
// a *NotPausedError when no pause is open. It marks each outbox row the pause holds with the pause
// (req.MarkHeld), ends the pause with who ended it, deletes the pause's `pause_stop` outbox rows
// no lane is running (a row under a live lease runs to its end and finds its pause ended) — those
// of the project's issues and those keyed by the project itself, the stops of claims of another
// project's issue, since the split_part of a project key is that key — ends the pause's open stop
// rows `cancelled`, sets the stuck clock of the project's open resumes to the resume's moment
// (SetProjectResumeClocksTx), and queues the controller's notice when req.Notice is given.
func (s *Store) Resume(ctx context.Context, req ResumeRequest) (Resumed, error) {
	if err := req.By.validate(); err != nil {
		return Resumed{}, fmt.Errorf("resume %s: %w", req.Project, err)
	}
	if req.At.IsZero() {
		return Resumed{}, fmt.Errorf("resume %s: a resume needs its moment", req.Project)
	}
	token, err := claim.ProjectToken(req.Project)
	if err != nil {
		return Resumed{}, fmt.Errorf("resume %s: %w", req.Project, err)
	}
	var out Resumed
	err = Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		out = Resumed{}
		pause, open, err := readOpenPause(ctx, tx, req.Project, " for update")
		if err != nil {
			return err
		}
		if !open {
			return &NotPausedError{Project: req.Project}
		}
		if req.MarkHeld != nil {
			if err := req.MarkHeld(ctx, tx, pause); err != nil {
				return fmt.Errorf("mark the rows pause %d holds: %w", pause.ID, err)
			}
		}
		if out.Pause, err = scanPause(tx.QueryRow(ctx, `update project_pauses
			set ended_at = $2, ended_by_kind = $3, ended_by_session = $4 where id = $1 returning `+pauseColumns,
			pause.ID, req.At, string(req.By.Kind), req.By.Session)); err != nil {
			return fmt.Errorf("end pause %d: %w", pause.ID, err)
		}
		deleted, err := tx.Exec(ctx, `delete from outbox where `+pauseStopRowsOfPause+`
			and split_part(issue, '-', 1) = $2 and (lease_until is null or lease_until <= $3)`,
			pause.ID, req.Project, req.At)
		if err != nil {
			return fmt.Errorf("delete the stop rows of pause %d: %w", pause.ID, err)
		}
		out.DeletedStopRows = deleted.RowsAffected()
		rows, err := tx.Query(ctx, `update project_pause_stops set ended_at = $2, ended_as = $3
			where pause_id = $1 and ended_at is null returning `+pauseStopColumns, pause.ID, req.At, string(PauseStopCancelled))
		if err != nil {
			return fmt.Errorf("cancel the stops of pause %d: %w", pause.ID, err)
		}
		if out.Cancelled, err = scanPauseStops(rows); err != nil {
			return fmt.Errorf("cancel the stops of pause %d: %w", pause.ID, err)
		}
		if out.Clocks, err = SetProjectResumeClocksTx(ctx, tx, token, req.At); err != nil {
			return err
		}
		if req.Notice != nil {
			return queuePauseNotice(ctx, tx, out.Pause, req.Notice)
		}
		return nil
	})
	if err != nil {
		return Resumed{}, fmt.Errorf("resume %s: %w", req.Project, err)
	}
	return out, nil
}

// queuePauseNotice queues the controller's notice of a pause or a resume under the project's key,
// the issue column a project-level controller notice carries (record.OutboxRow.Issue); a notice
// its builder keyed otherwise is refused.
func queuePauseNotice(ctx context.Context, tx pgx.Tx, pause Pause, notice func(Pause) (PauseRow, error)) error {
	row, err := notice(pause)
	if err != nil {
		return fmt.Errorf("build the controller's notice of pause %d: %w", pause.ID, err)
	}
	if row.Issue != pause.Project {
		return fmt.Errorf("the controller's notice of pause %d is keyed by %q, not the project %s", pause.ID, row.Issue, pause.Project)
	}
	_, err = insertOutboxRow(ctx, tx, pause.Project, row.Issue, IdleReleaseRow{Kind: row.Kind, Payload: row.Payload})
	return err
}

// pauseColumns is a pause row as scanPause reads it.
const pauseColumns = `id, project, began_at, by_kind, by_session, reason, ended_at,
	coalesce(ended_by_kind, ''), coalesce(ended_by_session, '')`

func scanPause(row pgx.Row) (Pause, error) {
	var p Pause
	var byKind, endedByKind string
	var ended *time.Time
	if err := row.Scan(&p.ID, &p.Project, &p.Since, &byKind, &p.By.Session, &p.Reason, &ended,
		&endedByKind, &p.EndedBy.Session); err != nil {
		return Pause{}, err
	}
	p.By.Kind, p.EndedBy.Kind = PauseActorKind(byKind), PauseActorKind(endedByKind)
	p.EndedAt = orZero(ended)
	return p, nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readOpenPause is project's open pause and whether there is one, read with lock appended to the
// select (" for share", " for update", or "" for none). project is the Dispatch key.
func readOpenPause(ctx context.Context, db queryRower, project, lock string) (Pause, bool, error) {
	return scanOpenPause(db.QueryRow(ctx, `select `+pauseColumns+` from project_pauses
		where project = $1 and ended_at is null`+lock, project), project)
}

// readOpenPauseByToken is readOpenPause for the project whose token is token
// (claim.ProjectToken), the spelling the claim and pod tables hold. project_pauses holds the
// Dispatch key, which the token lowercases for every key the table admits (^[A-Z][A-Z0-9]*$),
// so the read matches lower(project).
//
// It is the one statement of why a path that acts for a claim, its pod or its resume reads the
// pause by the daemon's own project token and never by the claim's issue key's prefix, and each
// such path names it rather than saying it again, whether it calls this function or reads the
// table in a statement of its own (readIdleView, podResumesWithoutAFirstTurn): a claim of another
// project's issue, which the operator's spawn makes under this project's token, is part of this
// project's pause, whatever project its issue's key names.
func readOpenPauseByToken(ctx context.Context, db queryRower, token, lock string) (Pause, bool, error) {
	return scanOpenPause(db.QueryRow(ctx, `select `+pauseColumns+` from project_pauses
		where lower(project) = $1 and ended_at is null`+lock, token), token)
}

// scanOpenPause is the open pause row read, and whether it read one; project, in either
// spelling, names the project in an error.
func scanOpenPause(row pgx.Row, project string) (Pause, bool, error) {
	p, err := scanPause(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pause{}, false, nil
	}
	if err != nil {
		return Pause{}, false, fmt.Errorf("read the open pause of %s: %w", project, err)
	}
	return p, true, nil
}

// OpenPause is project's open pause, and whether one is open: what the daemon reads at boot and
// at each outbox sweep to set its flag (decision 18). project is the Dispatch key.
func (s *Store) OpenPause(ctx context.Context, project string) (Pause, bool, error) {
	return readOpenPause(ctx, s.pool, project, "")
}

// LockOpenPauseTx is project's open pause read with its row locked for share, which a stop
// transaction whose own pause has ended reads (decision 6): a resume of that open pause then
// waits for the stop transaction to commit.
func LockOpenPauseTx(ctx context.Context, tx pgx.Tx, project string) (Pause, bool, error) {
	return readOpenPause(ctx, tx, project, " for share")
}

// LockOpenPauseByTokenTx is the open pause of the project whose token is token
// (readOpenPauseByToken) with its row locked for share, and whether one is open. A wake reads it
// to mark a wake the pause holds (decision 15 of dispatch://INFRA-3759, PodRecords.PauseOfWake),
// and a cancel of an idle release reads it to record the roles it would resume as stopped by the
// pause (decision 12, CancelIssueRelease). The resume's end of that pause then waits for the
// caller to commit, and sets the stuck clock of a held wake it wrote (SetProjectResumeClocksTx).
func LockOpenPauseByTokenTx(ctx context.Context, tx pgx.Tx, token string) (Pause, bool, error) {
	return readOpenPauseByToken(ctx, tx, token, " for share")
}

// LockPauseTx is pause id read with its row locked for share: the stop transaction of decision 6
// takes it on the pause its suspension carries, open or ended, so a resume that would end it
// waits for the stop's records to commit. A pause no row holds is an error.
func LockPauseTx(ctx context.Context, tx pgx.Tx, id int64) (Pause, error) {
	p, err := scanPause(tx.QueryRow(ctx, `select `+pauseColumns+` from project_pauses where id = $1 for share`, id))
	if err != nil {
		return Pause{}, fmt.Errorf("read pause %d: %w", id, err)
	}
	return p, nil
}

// PauseByID is pause id as it stands, open or ended: the `pause_stop` executor reads it to finish
// without acting once the pause its row names has ended (decision 4). A pause no row holds is an
// error.
func (s *Store) PauseByID(ctx context.Context, id int64) (Pause, error) {
	p, err := scanPause(s.pool.QueryRow(ctx, `select `+pauseColumns+` from project_pauses where id = $1`, id))
	if err != nil {
		return Pause{}, fmt.Errorf("read pause %d: %w", id, err)
	}
	return p, nil
}

// LastEndedPause is the newest pause of project a resume ended, and whether there is one: what a
// resume refused because the project is not paused names (PROJECT_NOT_PAUSED). project is the
// Dispatch key.
func (s *Store) LastEndedPause(ctx context.Context, project string) (Pause, bool, error) {
	p, err := scanPause(s.pool.QueryRow(ctx, `select `+pauseColumns+` from project_pauses
		where project = $1 and ended_at is not null order by ended_at desc, id desc limit 1`, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return Pause{}, false, nil
	}
	if err != nil {
		return Pause{}, false, fmt.Errorf("read the last pause of %s: %w", project, err)
	}
	return p, true, nil
}

// OpenOrLastPauseTx is, through tx's snapshot, project's open pause or, while none is open, its
// newest ended pause, and whether there is either: the state's `paused` or `lastPause` (decision
// 25) in one read. A project has at most one open pause (project_pauses_one_open), and it sorts
// first. project is the Dispatch key.
func OpenOrLastPauseTx(ctx context.Context, tx pgx.Tx, project string) (Pause, bool, error) {
	p, err := scanPause(tx.QueryRow(ctx, `select `+pauseColumns+` from project_pauses
		where project = $1 order by ended_at is not null, ended_at desc, id desc limit 1`, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return Pause{}, false, nil
	}
	if err != nil {
		return Pause{}, false, fmt.Errorf("read the pause of %s: %w", project, err)
	}
	return p, true, nil
}

// PauseLetOutsTx is, through tx's snapshot and by issue, when the wake pacing let out the launch
// of that issue's role that pause held: the latest stuck clock (pod_resumes.clock_at) of the
// role's resumes the pause held (pod_resumes.pause_id). The resume starts that clock at its own
// moment (SetProjectResumeClocksTx), and the reservation of the launch moves it to the moment the
// pacing lets the launch out (SetResumeClock), so it is the let-out once the launch has reserved
// its turn. An issue whose role the pause held no wake of is absent. project is the project
// token, as pod_resumes holds it. The architect-stalled arms read it once per reading for the
// architects (admit's readPauseLetOuts), so an architect the pause stopped owing a launch counts
// no time before that launch was let out.
func PauseLetOutsTx(ctx context.Context, tx pgx.Tx, project string, pause int64, role claim.Role) (map[string]time.Time, error) {
	rows, err := tx.Query(ctx, `select issue, max(clock_at) from pod_resumes
		where project = $1 and pause_id = $2 and role = $3 group by issue`, project, pause, string(role))
	if err != nil {
		return nil, fmt.Errorf("read the let-outs of the %s launches pause %d held: %w", role, pause, err)
	}
	defer rows.Close()
	letOuts := map[string]time.Time{}
	for rows.Next() {
		var issue string
		var at time.Time
		if err := rows.Scan(&issue, &at); err != nil {
			return nil, fmt.Errorf("read the let-outs of the %s launches pause %d held: %w", role, pause, err)
		}
		letOuts[issue] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the let-outs of the %s launches pause %d held: %w", role, pause, err)
	}
	return letOuts, nil
}

// liveClaimsOf is every live claim of the project whose token is project, in token order: one
// that is not suspended, failed or retired, and is not the controller's (claim.IsController),
// which the pause never stops (spec Design item 8). A claim of another project's issue, which the
// operator's spawn makes under this project's token, is one of them: the daemon runs it.
func liveClaimsOf(ctx context.Context, tx pgx.Tx, project string) ([]PauseStopClaim, error) {
	rows, err := tx.Query(ctx, `select token, tree, issue, role, generation from claims
		where project = $1 and state <> all($2) order by token`, project, goneStates())
	if err != nil {
		return nil, fmt.Errorf("read the live claims of %s: %w", project, err)
	}
	defer rows.Close()
	var live []PauseStopClaim
	for rows.Next() {
		var c PauseStopClaim
		var token, role string
		var generation int64
		if err := rows.Scan(&token, &c.Tree, &c.Issue, &role, &generation); err != nil {
			return nil, fmt.Errorf("read the live claims of %s: %w", project, err)
		}
		c.Claim, c.Role, c.Generation = claim.Token(token), claim.Role(role), uint64(generation)
		if claim.IsController(c.Role, c.Issue, c.Tree) {
			continue
		}
		live = append(live, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the live claims of %s: %w", project, err)
	}
	return live, nil
}

// PauseStopEnd is how a pause's stop of one role ended (project_pause_stops.ended_as).
type PauseStopEnd string

const (
	// PauseStopStopped: the role stopped (RecordPauseStoppedTx), or stood queued with nothing to
	// stop (EndPauseStop).
	PauseStopStopped PauseStopEnd = "stopped"
	// PauseStopTakenOver: something else stopped the role first: a park, a close, a linger, an
	// operator, an earlier pause's late stop or an idle release (EndPauseStop).
	PauseStopTakenOver PauseStopEnd = "taken_over"
	// PauseStopFailed: the claim failed or retired.
	PauseStopFailed PauseStopEnd = "failed"
	// PauseStopCancelled: the resume ended the pause while the stop was still open.
	PauseStopCancelled PauseStopEnd = "cancelled"
)

// PauseStop is one stop a pause asked of one role. EndedAt is zero and End empty while it is
// open; LaunchOwed is whether the role was owed a launch when it stopped.
type PauseStop struct {
	ID         int64
	Pause      int64
	Claim      claim.Token
	Issue      string
	Role       claim.Role
	AskedAt    time.Time
	EndedAt    time.Time
	End        PauseStopEnd
	LaunchOwed bool
}

const pauseStopColumns = `id, pause_id, claim, issue, role, asked_at, ended_at, coalesce(ended_as, ''), launch_owed`

func scanPauseStops(rows pgx.Rows) ([]PauseStop, error) {
	defer rows.Close()
	var stops []PauseStop
	for rows.Next() {
		var s PauseStop
		var token, role, end string
		var ended *time.Time
		if err := rows.Scan(&s.ID, &s.Pause, &token, &s.Issue, &role, &s.AskedAt, &ended, &end, &s.LaunchOwed); err != nil {
			return nil, err
		}
		s.Claim, s.Role, s.End, s.EndedAt = claim.Token(token), claim.Role(role), PauseStopEnd(end), orZero(ended)
		stops = append(stops, s)
	}
	return stops, rows.Err()
}

// OpenPauseStopTx opens held's stop of one claim at at, unless that pause already has an open
// stop of the claim (project_pause_stops_one_open), and answers whether it opened one. The pause's
// own transaction opens one per live claim; the boot and the catch-up after a pause open one for
// a claim the pause meets later, in the transaction that queues its `pause_stop` row. The stop
// names the pause's project, whatever the issue's key names: a claim of another project's issue,
// which the operator's spawn makes under this project's token, is stopped by this project's
// pause. The table holds the stop to its pause (its foreign key on (pause_id, project)) and its
// claim to that project's token (project_pause_stops_claim_of_project).
func OpenPauseStopTx(ctx context.Context, tx pgx.Tx, held Pause, stop PauseStopClaim, at time.Time) (bool, error) {
	if err := validPauseStopClaim(stop); err != nil {
		return false, err
	}
	var id int64
	err := tx.QueryRow(ctx, `insert into project_pause_stops (pause_id, project, claim, issue, role, asked_at)
		values ($1, $2, $3, $4, $5, $6) on conflict do nothing returning id`,
		held.ID, held.Project, string(stop.Claim), stop.Issue, string(stop.Role), at).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open pause %d's stop of %s: %w", held.ID, stop.Claim, err)
	}
	return true, nil
}

func validPauseStopClaim(stop PauseStopClaim) error {
	if stop.Claim == "" || !claim.IsIssueKey(stop.Issue) || !claim.IsRole(stop.Role) {
		return fmt.Errorf("a pause's stop names a claim, its issue and its role; it names %q, %q and %q", stop.Claim, stop.Issue, stop.Role)
	}
	return nil
}

// EndPauseStop ends pause's open stop of token at at as end and answers whether one was open.
// The `pause_stop` executor ends a stop this way when the claim answers it is stopped other than
// by this pause's own suspension (decision 4): `taken_over` where a park, a close, the operator,
// an earlier pause's late stop or an idle release stopped it first, `failed` where it failed or
// retired, `stopped` where it is queued with nothing to stop; the resume ends them `cancelled`. A
// stop ended here owes no launch. A stop the pause's own suspension made is recorded by
// RecordPauseStoppedTx instead, with the launch it owes, and that also ends a stop the resume
// cancelled.
func (s *Store) EndPauseStop(ctx context.Context, pause int64, token claim.Token, end PauseStopEnd, at time.Time) (bool, error) {
	switch end {
	case PauseStopStopped, PauseStopTakenOver, PauseStopFailed, PauseStopCancelled:
	default:
		return false, fmt.Errorf("end pause %d's stop of %s: %q is no way a stop ends", pause, token, end)
	}
	tag, err := s.pool.Exec(ctx, `update project_pause_stops set ended_at = $3, ended_as = $4
		where pause_id = $1 and claim = $2 and ended_at is null`, pause, string(token), at, string(end))
	if err != nil {
		return false, fmt.Errorf("end pause %d's stop of %s: %w", pause, token, err)
	}
	return tag.RowsAffected() > 0, nil
}

// RecordPauseStoppedTx records that the claim stopped for held at at, and whether it was owed a
// launch, and answers whether this is the first record of that stop. The claim's newest stop of
// that pause decides how:
//
//   - open, or ended `cancelled` by the resume: it ends `stopped`, since the role did stop
//     (decision 6), and the call answers true;
//   - ended `stopped` already: the stop is on record — a stop transaction made again because the
//     answer of its commit was lost (supervise.Deps.PauseStopped), or a role stopped twice within
//     one pause — so the row stays, owing a launch where either stop owed one, and the call
//     answers false, which tells the caller its other records stand too;
//   - ended otherwise, or none: one stop already ended is written, as for the gate's own stop of
//     a launch (decision 7), so the role's newest stop is always on record, and the call answers
//     true. It names the pause's project, as OpenPauseStopTx's does.
//
// RecordPauseStopTx calls it under the issue's lock (LockIssueForPause), so two stops of one claim
// never write it side by side.
func RecordPauseStoppedTx(ctx context.Context, tx pgx.Tx, held Pause, stop PauseStopClaim, at time.Time, launchOwed bool) (bool, error) {
	if err := validPauseStopClaim(stop); err != nil {
		return false, err
	}
	var newest int64
	var end string
	err := tx.QueryRow(ctx, `select id, coalesce(ended_as, '') from project_pause_stops
		where pause_id = $1 and claim = $2 order by id desc limit 1`, held.ID, string(stop.Claim)).Scan(&newest, &end)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("record pause %d's stop of %s: %w", held.ID, stop.Claim, err)
	}
	found := err == nil
	switch {
	case found && PauseStopEnd(end) == PauseStopStopped:
		_, err = tx.Exec(ctx, `update project_pause_stops set launch_owed = launch_owed or $2 where id = $1`, newest, launchOwed)
	case found && (end == "" || PauseStopEnd(end) == PauseStopCancelled):
		_, err = tx.Exec(ctx, `update project_pause_stops set ended_at = $2, ended_as = $4, launch_owed = $3 where id = $1`,
			newest, at, launchOwed, string(PauseStopStopped))
	default:
		_, err = tx.Exec(ctx, `insert into project_pause_stops
			(pause_id, project, claim, issue, role, asked_at, ended_at, ended_as, launch_owed)
			values ($1, $2, $3, $4, $5, $6, $6, $8, $7)`,
			held.ID, held.Project, string(stop.Claim), stop.Issue, string(stop.Role), at, launchOwed, string(PauseStopStopped))
	}
	if err != nil {
		return false, fmt.Errorf("record pause %d's stop of %s: %w", held.ID, stop.Claim, err)
	}
	return !found || PauseStopEnd(end) != PauseStopStopped, nil
}

// PauseStopRecord is one pause's stop of one claim, as RecordPauseStopTx writes it: the pause,
// read under the issue's lock (LockIssueForPause) with its row for share; the claim; the moment
// its stop row and the pod's hold carry; whether the claim is owed a launch
// (RecordPauseStoppedTx); whether the stop holds the claim's pod; and the backstop it arms, nil
// for none, which names the claim's own issue and role. The caller decides the hold and the
// backstop, each by its own rule, and passes its own clock's moment.
type PauseStopRecord struct {
	Pause      Pause
	Stop       PauseStopClaim
	At         time.Time
	LaunchOwed bool
	HoldPod    bool
	Backstop   *PodWakeBackstop
}

// RecordPauseStopTx writes the records of a pause's stop of one claim (decision 6 of
// dispatch://INFRA-3759): the stop row (RecordPauseStoppedTx) and, on that stop's first record,
// the pod's hold where rec.HoldPod asks for one (OpenPausePodHoldTx) and rec.Backstop where the
// stop owes no launch (ArmPodBackstopTx) — a role owed a launch comes back by that launch, whose
// held wake cancels the role's backstop (PodRecords.TakePodWake). It answers whether this was
// the stop's first record, as RecordPauseStoppedTx does: a stop recorded again writes no second
// hold and no second backstop. The stop transaction (internal/daemon's projectPause.stopTx) and a
// cancel of a release made while the project is paused (stopReleasedForPause) record their stops
// through it, so the two write one set of records.
func RecordPauseStopTx(ctx context.Context, tx pgx.Tx, rec PauseStopRecord) (bool, error) {
	if b := rec.Backstop; b != nil && (b.Issue != rec.Stop.Issue || b.Role != rec.Stop.Role) {
		return false, fmt.Errorf("pause %d's stop of %s arms the backstop of %s's %s, not of its own issue and role",
			rec.Pause.ID, rec.Stop.Claim, b.Issue, b.Role)
	}
	first, err := RecordPauseStoppedTx(ctx, tx, rec.Pause, rec.Stop, rec.At, rec.LaunchOwed)
	if err != nil || !first {
		return false, err
	}
	if rec.HoldPod {
		if _, err := OpenPausePodHoldTx(ctx, tx, rec.Pause, rec.Stop.Issue, rec.At); err != nil {
			return false, err
		}
	}
	if rec.Backstop != nil && !rec.LaunchOwed {
		if err := ArmPodBackstopTx(ctx, tx, *rec.Backstop); err != nil {
			return false, err
		}
	}
	return true, nil
}

// OpenPauseStopsTx are pause's open stops through tx's snapshot, oldest first: the claims whose
// stop the pause still waits on, which the state names (decision 25).
func OpenPauseStopsTx(ctx context.Context, tx pgx.Tx, pause int64) ([]PauseStop, error) {
	rows, err := tx.Query(ctx, `select `+pauseStopColumns+` from project_pause_stops
		where pause_id = $1 and ended_at is null order by id`, pause)
	if err != nil {
		return nil, fmt.Errorf("read the open stops of pause %d: %w", pause, err)
	}
	stops, err := scanPauseStops(rows)
	if err != nil {
		return nil, fmt.Errorf("read the open stops of pause %d: %w", pause, err)
	}
	return stops, nil
}

// OwedLaunchesWithNoIssueRecord is every claim of held's project whose stop was recorded `stopped`
// with a launch owed while held stood — a stop of held itself, or one that ended at or after
// held began — whose issue the daemon keeps no record of (no row in issues under held's project)
// and which still stands suspended by the release's source, as the pause's stop left it, in token
// order. Such a claim is an operator's own spawn: no start row can carry its launch, since the
// outbox's start reads the issue's record, so held's resume launches it by rule
// (projectPause.Resumed in internal/daemon). A record another project's daemon keeps of the same
// key on a shared database is not this daemon's, and the stop transaction reads it as no record
// too (internal/daemon's projectRecords.Issue). A claim another source has suspended since, or
// that runs again, is not the resume's to launch. held's project is its Dispatch key, as the stops
// and the issues table hold it.
func (s *Store) OwedLaunchesWithNoIssueRecord(ctx context.Context, held Pause) ([]claim.Token, error) {
	rows, err := s.pool.Query(ctx, `select distinct s.claim from project_pause_stops s
		join claims c on c.token = s.claim
		where s.project = $1 and (s.pause_id = $2 or s.ended_at >= $3) and s.ended_as = $4 and s.launch_owed
			and c.state = $5 and c.suspended_by = $6
			and not exists (select 1 from issues i where i.key = s.issue and i.project = s.project)
		order by s.claim`, held.Project, held.ID, held.Since, string(PauseStopStopped),
		string(supervise.StateSuspended), string(supervise.SuspendedByReleased))
	if err != nil {
		return nil, fmt.Errorf("read the launches pause %d owes claims of no issue record: %w", held.ID, err)
	}
	tokens, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the launches pause %d owes claims of no issue record: %w", held.ID, err)
	}
	owed := make([]claim.Token, len(tokens))
	for i, token := range tokens {
		owed[i] = claim.Token(token)
	}
	return owed, nil
}

// StoppedByAPause is the id of the pause whose stop of claim token is the claim's newest stop that
// ended `stopped`, where that stop ended after issue's newest pod release, whichever pause made it,
// and 0 otherwise. It is the stop heldMessageOwed reads (internal/daemon's outbox.heldMessageOwed,
// the one statement of the held role message's debt, R4 of dispatch://INFRA-3759): a role message
// a pause held is owed to a role a pause stopped until the role's session holds its topic, and a
// release recorded after the stop ends that debt.
// A later stop of the claim that is still open, or that ended any other way, is passed over: a
// cancelled stop stopped nothing, and what a taken-over or failed one signals, the claim's state or
// a later pod release already says. It reads no ready, unlike StoppingPauseTx: a woken agent says
// it is ready (supervise.SaidReady) before its plugin claims the role's Envoy topic, and the
// message is still owed in that gap. project is the project token, as pod_releases holds it.
func (s *Store) StoppedByAPause(ctx context.Context, project, issue string, token claim.Token) (int64, error) {
	var pause int64
	if err := s.pool.QueryRow(ctx, `select coalesce((select case
			when s.ended_at > coalesce((select max(r.at) from pod_releases r where r.project = $1 and r.issue = $2), '-infinity'::timestamptz)
			then s.pause_id else 0 end
		from project_pause_stops s where s.claim = $3 and s.ended_as = $4 order by s.id desc limit 1), 0)`,
		project, issue, string(token), string(PauseStopStopped)).Scan(&pause); err != nil {
		return 0, fmt.Errorf("read the newest pause that stopped %s: %w", token, err)
	}
	return pause, nil
}

// StoppingPauseTx is the id of the pause that stopped claim token, through tx: the pause of the
// claim's newest stop, whatever its end, where that stop ended `stopped` after both issue's newest
// pod release and the claim's last ready (claims.last_ready), and 0 otherwise. It is the statement
// of which pause a wake of the claim belongs to, and a wake of the claim reads it twice over
// (PodRecords.PauseOfWake): the session the wake brings back is one that pause stopped in a pod it
// kept, which the wake's task says (decision 16 of dispatch://INFRA-3759, workflow.WakeTask), and a
// wake taken once that pause has ended can be that pause's held wake, which pairs with no release
// (decision 15, PodRecords.TakePodWake). A release recorded after that stop took the pod, and the
// release's words are the true ones. A ready after it is the agent of a launch since, such as the
// resume's, so something later stopped that session: an idle release suspends the role (its
// outbox `suspend` row) before its teardown records the release, and a wake taken in between is
// the release's. The held role message's debt reads StoppedByAPause, through heldMessageOwed, not
// this. project is the project token, as pod_releases holds it.
func StoppingPauseTx(ctx context.Context, tx pgx.Tx, project, issue string, token claim.Token) (int64, error) {
	var pause int64
	if err := tx.QueryRow(ctx, `select coalesce((select case when s.ended_as = $4
			and s.ended_at > coalesce((select max(r.at) from pod_releases r where r.project = $1 and r.issue = $2), '-infinity'::timestamptz)
			and s.ended_at > coalesce((select c.last_ready from claims c where c.token = $3), '-infinity'::timestamptz)
			then s.pause_id else 0 end
		from project_pause_stops s where s.claim = $3 order by s.id desc limit 1), 0)`,
		project, issue, string(token), string(PauseStopStopped)).Scan(&pause); err != nil {
		return 0, fmt.Errorf("read the pause that stopped %s: %w", token, err)
	}
	return pause, nil
}

// IssueHasLiveRoleTx is whether any claim of issue is live through tx: not suspended, failed or
// retired, as liveClaimsOf reads a live claim. Read under the issue's lock (LockIssueForPause), it
// is what the stop transaction of a pause that has ended asks of the stopped role's pod
// (projectPause.stopTx in internal/daemon, decision 6), which it holds only where no role of the
// pod is live; a stop made while its pause stands, and a paused cancel of a release
// (stopReleasedForPause), hold the pod whatever its other roles do. project is the project token,
// as the claims table holds it.
func IssueHasLiveRoleTx(ctx context.Context, tx pgx.Tx, project, issue string) (bool, error) {
	var live bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from claims where project = $1 and issue = $2 and state <> all($3))`,
		project, issue, goneStates()).Scan(&live); err != nil {
		return false, fmt.Errorf("read whether a role of %s is live: %w", issue, err)
	}
	return live, nil
}

// pauseStopRows is the predicate of the outbox's `pause_stop` rows (a `supervise` row whose op is
// `pause_stop`, record.SuperviseRequest), written once for every statement that reads or writes
// them; pauseStopRowsOfPause narrows it to the rows of the pause its statement binds as $1.
const (
	pauseStopRows        = `kind = 'supervise' and payload->>'op' = 'pause_stop'`
	pauseStopRowsOfPause = pauseStopRows + ` and payload->>'pause' = $1::bigint::text`
)

// ExpeditePauseStopRowsTx makes due at at every `pause_stop` row of the claim of issue's role that
// waits for a later moment, and answers the keys of the rows it moved, each once: a stop the claim
// held for its turn retries on a bounded wait (the outbox's pauseStop), and the stop transaction
// that records the claim stopped makes its row due at once, so it ends within a lane's wake of
// the turn's end (decision 4). The claim's rows are those of project, the Dispatch key, that its
// outbox runs: keyed by issue, or keyed by the project and naming issue in their payload
// (record.SuperviseRequest.Issue), as the row of a claim of another project's issue is. The
// caller wakes the lifecycle lane of each key once the transaction commits.
func ExpeditePauseStopRowsTx(ctx context.Context, tx pgx.Tx, project, issue string, role claim.Role, at time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `update outbox set next_at = $4
		where `+pauseStopRows+` and split_part(issue, '-', 1) = $1 and (issue = $2 or payload->>'issue' = $2)
		and payload->>'role' = $3 and next_at > $4 returning issue`,
		project, issue, string(role), at)
	if err != nil {
		return nil, fmt.Errorf("make the pause stops of %s's %s due: %w", issue, role, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("make the pause stops of %s's %s due: %w", issue, role, err)
	}
	slices.Sort(keys)
	return slices.Compact(keys), nil
}

// ExpeditePauseStops makes due at at every `pause_stop` row of pause that waits for a later
// moment, whatever its key (pause.StopKey in internal/pause), and answers how many it moved. A
// stop its claim held for a turn or a launch waits on the row's bounded retry (the outbox's
// pauseStop) while the hold itself lives in the machine's memory, which a restart loses: the boot
// of a paused project makes each of those rows due, so its stop is asked again as soon as the
// outbox runs and the restarted machine holds it again (decision 4 of dispatch://INFRA-3759: a
// restart holds the stop again).
func (s *Store) ExpeditePauseStops(ctx context.Context, pause int64, at time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `update outbox set next_at = $2
		where `+pauseStopRowsOfPause+` and next_at > $2`,
		pause, at)
	if err != nil {
		return 0, fmt.Errorf("make the stops of pause %d due: %w", pause, err)
	}
	return tag.RowsAffected(), nil
}
