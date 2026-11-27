package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

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
	var ended bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update project_pause_stops set ended_at = $3, ended_as = $4
			where pause_id = $1 and claim = $2 and ended_at is null`, pause, string(token), at, string(end))
		ended = tag.RowsAffected() > 0
		return err
	})
	if err != nil {
		return false, fmt.Errorf("end pause %d's stop of %s: %w", pause, token, err)
	}
	return ended, nil
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
	var tokens []string
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select distinct s.claim from project_pause_stops s
			join claims c on c.token = s.claim
			where s.project = $1 and (s.pause_id = $2 or s.ended_at >= $3) and s.ended_as = $4 and s.launch_owed
				and c.state = $5 and c.suspended_by = $6
				and not exists (select 1 from issues i where i.key = s.issue and i.project = s.project)
			order by s.claim`, held.Project, held.ID, held.Since, string(PauseStopStopped),
			string(supervise.StateSuspended), string(supervise.SuspendedByReleased))
		if err != nil {
			return err
		}
		tokens, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the launches pause %d owes claims of no issue record: %w", held.ID, err)
	}
	owed := make([]claim.Token, len(tokens))
	for i, token := range tokens {
		owed[i] = claim.Token(token)
	}
	return owed, nil
}

// podReleaseEnded is the moment issue's pod last went away, as the two reads of a pause's stop
// compare a stop's end against it, $1 being the project token and $2 the issue: the later of the
// issue's newest recorded release (pod_releases.at) and its newest release whose teardown gave up
// on the record (pod_release_give_ups.at, the moment SettleUnrecordedRelease gave up), and
// -infinity where it has neither. A release that gave up took the pod as a recorded one did, and
// writes no pod_releases row, so a read of pod_releases alone would take a pause's stop before it
// as what stopped the role last. The close of a resume whose pod went away reads the give-ups too,
// each against the resume's own time (ClosePodGoneResume). Every other read of the issue's newest
// release reads pod_releases alone (LatestPodRelease, the release measurements).
const podReleaseEnded = `coalesce(greatest(
	(select max(r.at) from pod_releases r where r.project = $1 and r.issue = $2),
	(select max(g.at) from pod_release_give_ups g where g.project = $1 and g.issue = $2)), '-infinity'::timestamptz)`

// StoppedByAPause is the id of the pause whose stop of claim token is the claim's newest stop that
// ended `stopped`, where that stop ended after issue's pod last went away (podReleaseEnded: its
// newest recorded release, or a release whose teardown gave up on its record, at the moment it
// gave up), whichever pause made it, and 0 otherwise. It is the stop heldMessageOwed reads
// (internal/daemon's outbox.heldMessageOwed, the one statement of the held role message's debt, R4
// of dispatch://INFRA-3759): a role message a pause held is owed to the claim a pause stopped until
// that claim's session holds the topic of its token, and a release recorded after the stop, or one
// whose teardown gave up on its record after it, ends that debt.
// A later stop of the claim that is still open, or that ended any other way, is passed over: a
// cancelled stop stopped nothing, and what a taken-over or failed one signals, the claim's state or
// a later pod release already says. It reads no ready, unlike StoppingPauseTx: a woken agent says
// it is ready (supervise.SaidReady) before its plugin claims the Envoy topic of its claim token,
// and the message is still owed in that gap. project is the project token, as pod_releases holds it.
func (s *Store) StoppedByAPause(ctx context.Context, project, issue string, token claim.Token) (int64, error) {
	var pause int64
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select coalesce((select case when s.ended_at > `+podReleaseEnded+` then s.pause_id else 0 end
			from project_pause_stops s where s.claim = $3 and s.ended_as = $4 order by s.id desc limit 1), 0)`,
			project, issue, string(token), string(PauseStopStopped)).Scan(&pause)
	})
	if err != nil {
		return 0, fmt.Errorf("read the newest pause that stopped %s: %w", token, err)
	}
	return pause, nil
}

// StoppingPauseTx is the id of the pause that stopped claim token, through tx: the pause of the
// claim's newest stop, whatever its end, where that stop ended `stopped` after both the moment
// issue's pod last went away (podReleaseEnded) and the claim's last ready (claims.last_ready), and
// 0 otherwise. It is the statement of which pause a wake of the claim belongs to, and a wake of
// the claim reads it twice over (PodRecords.PauseOfWake): the session the wake brings back is one
// that pause stopped in a pod it kept, which the wake's task says (decision 16 of
// dispatch://INFRA-3759, workflow.WakeTask), and a wake taken once that pause has ended can be that
// pause's held wake, which pairs with no release (decision 15, PodRecords.TakePodWake). A release
// recorded after that stop took the pod, and the release's words are the true ones. So did a
// release whose teardown gave up on its record after that stop (SettleUnrecordedRelease): it
// records no pod release, and the moment it gave up stands in pod_release_give_ups in its place. A
// ready after that stop is the agent of a launch since, such as the resume's, so something later
// stopped that session: an idle release suspends the role (its outbox `suspend` row) before its
// teardown records the release, and a wake taken in between is the release's. The held role
// message's debt reads StoppedByAPause, through heldMessageOwed, not this. project is the project
// token, as pod_releases holds it.
func StoppingPauseTx(ctx context.Context, tx pgx.Tx, project, issue string, token claim.Token) (int64, error) {
	var pause int64
	if err := tx.QueryRow(ctx, `select coalesce((select case when s.ended_as = $4
			and s.ended_at > `+podReleaseEnded+`
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
	var moved int64
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update outbox set next_at = $2
			where `+pauseStopRowsOfPause+` and next_at > $2`,
			pause, at)
		moved = tag.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("make the stops of pause %d due: %w", pause, err)
	}
	return moved, nil
}
