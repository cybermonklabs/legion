package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The holds a project's pause puts on its issues' pods (project_pause_pod_holds, decisions 12 and
// 13 of dispatch://INFRA-3759, as migrations/20261115194357_project_pause.up.sql defines them):
// the issue's lock every writer of a hold takes first, the open and the ends of a hold, and its
// reads. A hold keeps the pod of a role the pause stopped, with that role's unpushed work, from an
// update round past its bound and from the idle rule. A hold's project is the pause's Dispatch
// key ("LEGION"), which may differ from the project its issue's key names, so every read of a
// hold is by the project, never by the issue key's prefix, as every read of a pause for a claim
// is (readOpenPauseByToken says why). Each function below says which spelling of the project it
// takes: the Dispatch key, or the token (claim.ProjectToken), which is the key lowercased for
// every key project_pauses admits (^[A-Z][A-Z0-9]*$).

// LockIssueForPause takes the transaction-scoped advisory lock every writer of issue's pod hold
// takes first, after treelifecycle.Serialize where that writer takes it at all (decisions 6 and 13
// of dispatch://INFRA-3759): two stops of one pod's roles, a resume's end of the pod's hold and
// a role's ready then run one after another, and the later reads what the earlier wrote. The
// lock is released when tx ends.
//
// It is keyed on the issue as record.Postgres.ClaimRow keys an issue's lifecycle lane, but in
// the two-key form, pg_advisory_xact_lock(int, int): Postgres keeps the two-key space apart from
// the one-key space, where every other advisory lock of the daemon lives (ClaimRow's
// 'legion.outbox.lifecycle:' and 'legion.outbox.catch-up:' keys, record's countIntakeGiveUp's
// 'legion.intake_give_up_notice:' key, treelifecycle.Serialize, the migrations' lock), so this
// lock can never be mistaken for one of those whatever the hashes give. The first key names the
// lock and the second the issue; two issues whose keys hash alike only serialize their pod holds.
func LockIssueForPause(ctx context.Context, tx pgx.Tx, issue string) error {
	if !claim.IsIssueKey(issue) {
		return fmt.Errorf("lock %q for its pod hold: not an issue key", issue)
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('legion.pause.issue'), hashtext($1::text))`, issue); err != nil {
		return fmt.Errorf("lock %s for its pod hold: %w", issue, err)
	}
	return nil
}

// PausePodHoldEnd is why a pause's hold of a pod ended (project_pause_pod_holds.ended_why).
type PausePodHoldEnd string

const (
	// PausePodHoldResumed: the resume found a live role in the pod (decision 13).
	PausePodHoldResumed PausePodHoldEnd = "resumed"
	// PausePodHoldReady: a role of the pod became ready.
	PausePodHoldReady PausePodHoldEnd = "ready"
	// PausePodHoldDeleted: a close, a park, a linger, a production check's look or a teardown
	// deleted the pod.
	PausePodHoldDeleted PausePodHoldEnd = "deleted"
)

// PausePodHold is one hold a pause put on an issue's pod. EndedAt is zero and End empty while it
// stands.
type PausePodHold struct {
	ID    int64
	Pause int64
	// Project is the pause's project, the Dispatch key.
	Project string
	Issue   string
	HeldAt  time.Time
	EndedAt time.Time
	End     PausePodHoldEnd
	// LetOut is, as PausePodHoldsSince reads it, the latest let-out of a launch the hold's pause
	// held for a role of the pod: the greatest stuck clock (pod_resumes.clock_at) of the issue's
	// resumes that pause held (pod_resumes.pause_id), which the resume starts at its own moment
	// (SetProjectResumeClocksTx) and the reservation of each launch moves to the moment the wake
	// pacing lets that launch out (SetResumeClock). It is zero where the pause held no launch of
	// the pod, and every other read of a hold leaves it zero. An update round's bound is its one
	// reader (internal/daemon's heldUntil). Whether the pause still keeps the pod for one role is
	// never read from it.
	LetOut time.Time
	// Owed is, as PausePodHoldsSince reads it, each role of the pod that the hold's pause still
	// owes a launch: the role has a resume of that pause for the issue that no first turn stamped,
	// nothing closed, and whose pod no ready stamped (pod_resumes.first_turn_at, closed_at and
	// pod_ready_at). A role that became ready has run in the pod again since the pause stopped it,
	// so the pause owes it nothing more, even where its resume stays open because the daemon
	// suspended it, or it failed or retired, before its first turn. A role whose held launch failed
	// before it became ready has not run since the pause stopped it, so it stays owed. A role has
	// one open resume at most (pod_resumes_one_open_per_role), so it is named once, and the roles
	// are in order. Every other read of a hold leaves Owed nil. The daemon reads it for the one
	// exception of an update round (internal/daemon's podHolds.owes).
	Owed []claim.Role
}

const pausePodHoldColumns = `id, pause_id, project, issue, held_at, ended_at, coalesce(ended_why, '')`

func scanPausePodHold(row pgx.Row) (PausePodHold, error) {
	var h PausePodHold
	var end string
	var ended *time.Time
	if err := row.Scan(&h.ID, &h.Pause, &h.Project, &h.Issue, &h.HeldAt, &ended, &end); err != nil {
		return PausePodHold{}, err
	}
	h.End, h.EndedAt = PausePodHoldEnd(end), orZero(ended)
	return h, nil
}

// OpenPausePodHoldTx opens held's hold of issue's pod at at, unless a hold of that pod already
// stands (project_pause_pod_holds_one_open), and answers whether it opened one. The caller holds
// issue's lock (LockIssueForPause) and has read held, as the stop transaction and a paused
// cancel of a release do. The hold carries held's project, so a pause of one project cannot hold
// a pod under another project's pause (the table's foreign key on (pause_id, project)).
func OpenPausePodHoldTx(ctx context.Context, tx pgx.Tx, held Pause, issue string, at time.Time) (bool, error) {
	var id int64
	err := tx.QueryRow(ctx, `insert into project_pause_pod_holds (pause_id, project, issue, held_at)
		values ($1, $2, $3, $4) on conflict do nothing returning id`, held.ID, held.Project, issue, at).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("hold the pod of %s for pause %d: %w", issue, held.ID, err)
	}
	return true, nil
}

// EndPausePodHold ends the hold of issue's pod in a transaction of its own; see
// EndPausePodHoldTx.
func (s *Store) EndPausePodHold(ctx context.Context, project, issue string, end PausePodHoldEnd, at time.Time) (bool, error) {
	var ended bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		ended, err = EndPausePodHoldTx(ctx, tx, project, issue, end, at)
		return err
	})
	return ended, err
}

// EndPausePodHoldTx takes issue's lock (LockIssueForPause) and ends the hold that stands on
// issue's pod under project's pauses, whichever of them put it there, at at for the reason end,
// and answers whether one stood. project is the Dispatch key.
func EndPausePodHoldTx(ctx context.Context, tx pgx.Tx, project, issue string, end PausePodHoldEnd, at time.Time) (bool, error) {
	if err := validPausePodHoldEnd(issue, end); err != nil {
		return false, err
	}
	if err := LockIssueForPause(ctx, tx, issue); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `update project_pause_pod_holds set ended_at = $3, ended_why = $4
		where project = $1 and issue = $2 and ended_at is null`, project, issue, at, string(end))
	if err != nil {
		return false, fmt.Errorf("end the pod hold of %s: %w", issue, err)
	}
	return tag.RowsAffected() > 0, nil
}

func validPausePodHoldEnd(issue string, end PausePodHoldEnd) error {
	switch end {
	case PausePodHoldResumed, PausePodHoldReady, PausePodHoldDeleted:
		return nil
	}
	return fmt.Errorf("end the pod hold of %s: %q is no reason a hold ends", issue, end)
}

// PausePodHoldOf is the hold that stands on issue's pod under project's pauses, and whether one
// does. project is the Dispatch key.
func (s *Store) PausePodHoldOf(ctx context.Context, project, issue string) (PausePodHold, bool, error) {
	h, err := scanPausePodHold(s.pool.QueryRow(ctx, `select `+pausePodHoldColumns+` from project_pause_pod_holds
		where project = $1 and issue = $2 and ended_at is null`, project, issue))
	if errors.Is(err, pgx.ErrNoRows) {
		return PausePodHold{}, false, nil
	}
	if err != nil {
		return PausePodHold{}, false, fmt.Errorf("read the pod hold of %s: %w", issue, err)
	}
	return h, true, nil
}

// OpenPausePodHolds is every hold standing on a pod of project, by issue: the pods the resume
// ends the holds of where a role is live (decision 22), and the pods a project release names when
// it refuses (departure 1). project is the Dispatch key.
func (s *Store) OpenPausePodHolds(ctx context.Context, project string) ([]PausePodHold, error) {
	rows, err := s.pool.Query(ctx, `select `+pausePodHoldColumns+` from project_pause_pod_holds
		where project = $1 and ended_at is null order by issue`, project)
	if err != nil {
		return nil, fmt.Errorf("read the pod holds of %s: %w", project, err)
	}
	holds, err := scanPausePodHolds(rows)
	if err != nil {
		return nil, fmt.Errorf("read the pod holds of %s: %w", project, err)
	}
	return holds, nil
}

// PausePodHoldsSince is every hold of project's pods that stands, that ended after since, whose
// pause let out a launch of the pod after since, or whose pause still owes a role of the pod a
// launch, by issue and then by when it began, each with its latest let-out (PausePodHold.LetOut)
// and each role its pause still owes a launch (PausePodHold.Owed), read in the one statement:
// what one sweep of the update rounds reads (decision 12 of dispatch://INFRA-3759). A round waits
// over a pod a hold stands on, and its bound moves by the time a hold kept its pod after the round
// opened, which runs on to the let-out of the last launch its pause held for a role of the pod,
// so since is the opening of the oldest round the sweep reads.
//
// A launch the pause still owes a role is a resume of that pause for the issue and that role that
// no first turn stamped, nothing closed (OpenPodResume), and whose pod no ready stamped
// (MarkPodResumeReady). It is read for each role. The stuck clock of such a resume can be before
// since: a round can open after the hold ended and before the wake pacing lets the launch out
// (SetResumeClock). The pause keeps the pod for that role's work until the role runs in the pod
// again, so the read returns such a hold whatever since. A resume that a ready stamped and that
// stays open owes nothing, so it brings no hold into the read. project is the Dispatch key.
func (s *Store) PausePodHoldsSince(ctx context.Context, project string, since time.Time) ([]PausePodHold, error) {
	rows, err := s.pool.Query(ctx, `select h.id, h.pause_id, h.project, h.issue, h.held_at, h.ended_at, coalesce(h.ended_why, ''),
			l.let_out, l.owed_roles
		from project_pause_pod_holds h
		left join lateral (select max(r.clock_at) as let_out,
				coalesce(array_agg(r.role order by r.role) filter (where r.first_turn_at is null and r.closed_at is null and r.pod_ready_at is null), '{}') as owed_roles
			from pod_resumes r
			where r.project = lower(h.project) and r.issue = h.issue and r.pause_id = h.pause_id) l on true
		where h.project = $1 and (h.ended_at is null or h.ended_at > $2 or l.let_out > $2 or cardinality(l.owed_roles) > 0)
		order by h.issue, h.held_at`, project, since)
	if err != nil {
		return nil, fmt.Errorf("read the pod holds of %s since %s: %w", project, since.Format(time.RFC3339), err)
	}
	defer rows.Close()
	var holds []PausePodHold
	for rows.Next() {
		var h PausePodHold
		var end string
		var ended, letOut *time.Time
		var owedRoles []string
		if err := rows.Scan(&h.ID, &h.Pause, &h.Project, &h.Issue, &h.HeldAt, &ended, &end, &letOut, &owedRoles); err != nil {
			return nil, fmt.Errorf("read the pod holds of %s since %s: %w", project, since.Format(time.RFC3339), err)
		}
		h.End, h.EndedAt, h.LetOut = PausePodHoldEnd(end), orZero(ended), orZero(letOut)
		if len(owedRoles) > 0 {
			h.Owed = make([]claim.Role, len(owedRoles))
			for i, role := range owedRoles {
				h.Owed[i] = claim.Role(role)
			}
		}
		holds = append(holds, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the pod holds of %s since %s: %w", project, since.Format(time.RFC3339), err)
	}
	return holds, nil
}

func scanPausePodHolds(rows pgx.Rows) ([]PausePodHold, error) {
	defer rows.Close()
	var holds []PausePodHold
	for rows.Next() {
		h, err := scanPausePodHold(rows)
		if err != nil {
			return nil, err
		}
		holds = append(holds, h)
	}
	return holds, rows.Err()
}

// EndLivePausePodHold ends, at at for the reason end, the hold that stands on issue's pod where a
// role of the pod is live, in a transaction of its own, and answers whether it ended one: the
// resume's end of the holds of pods with a live role (PausePodHoldResumed) and the end at a role's
// ready (PausePodHoldReady, decision 13 of dispatch://INFRA-3759). A live role is a claim that is
// not suspended, failed or retired (supervise.GoneStates) and is not queued or launching with no
// process recorded (no locator): a claim nothing has launched yet, or a launch that waits its turn
// in the wake pacing or that the gate holds, runs nothing. The resume ends the holds after its
// commit has made the held rows claimable and after it has posted the resume by rule of each
// operator's spawn owed a launch (internal/daemon's projectPause.Resumed), so a held start the
// outbox runs in between, or such a spawn's machine, can park a launch so while no role of the
// pod runs. It takes the issue's lock (LockIssueForPause) and then ends the hold in one statement
// that reads the live role itself, so a stop transaction of a role of the same pod commits first
// and the statement sees it: a hold that stop opened stays, since no role of the pod runs then.
// project is the project token, as the claims table holds it; the hold's own Dispatch key is read
// as the key it lowercases.
func (s *Store) EndLivePausePodHold(ctx context.Context, project, issue string, end PausePodHoldEnd, at time.Time) (bool, error) {
	if err := validPausePodHoldEnd(issue, end); err != nil {
		return false, err
	}
	var ended bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := LockIssueForPause(ctx, tx, issue); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `update project_pause_pod_holds h set ended_at = $3, ended_why = $4
			where lower(h.project) = $1 and h.issue = $2 and h.ended_at is null
			and exists (select 1 from claims c where c.project = $1 and c.issue = h.issue and c.state <> all($5)
				and not (c.state = any($6) and c.locator is null))`,
			project, issue, at, string(end), goneStates(), notStartedStates())
		if err != nil {
			return fmt.Errorf("end the pod hold of %s: %w", issue, err)
		}
		ended = tag.RowsAffected() > 0
		return nil
	})
	return ended, err
}

// notStartedStates are the states in which a claim that records no process (no locator) has none
// running or coming up yet, as the text array a query binds: queued, for which nothing was
// launched, and launching, a launch that waits its turn in the wake pacing or that the pause's
// gate holds (EndLivePausePodHold).
func notStartedStates() []string {
	return claimStateNames([]supervise.ClaimState{supervise.StateQueued, supervise.StateLaunching})
}

// endTreePausePodHoldsTx ends, at at, the hold that stands on the pod of each issue of tree: the
// tree's cleanup has deleted every Sandbox of the tree (CleanupReservedTree), so a hold of one of
// its pods holds nothing (PausePodHoldDeleted, decision 13 of dispatch://INFRA-3759). An issue of
// the tree is one the issues table files under it under the hold's project, or one a claim of
// project's names under it: a claim of the operator's spawn has no issue record, of this project's
// issue or of another's, and a record another project's daemon keeps of the same key on a shared
// database is not this project's (internal/daemon's projectRecords.Issue). Each hold ends under
// its issue's lock, in key order. project is the project token, as the claims table holds it.
func endTreePausePodHoldsTx(ctx context.Context, tx pgx.Tx, project, tree string, at time.Time) error {
	rows, err := tx.Query(ctx, `select h.project, h.issue from project_pause_pod_holds h
		where lower(h.project) = $1 and h.ended_at is null
		and (exists (select 1 from issues i where i.key = h.issue and i.project = h.project and i.tree = $2)
			or exists (select 1 from claims c where c.project = $1 and c.issue = h.issue and c.tree = $2))
		order by h.issue`, project, tree)
	if err != nil {
		return fmt.Errorf("read the pod holds of tree %s: %w", tree, err)
	}
	type held struct{ project, issue string }
	holds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (held, error) {
		var h held
		err := row.Scan(&h.project, &h.issue)
		return h, err
	})
	if err != nil {
		return fmt.Errorf("read the pod holds of tree %s: %w", tree, err)
	}
	for _, h := range holds {
		if _, err := EndPausePodHoldTx(ctx, tx, h.project, h.issue, PausePodHoldDeleted, at); err != nil {
			return err
		}
	}
	return nil
}
