package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// wakeClaimSelect reads a claim of this daemon's project as a wake reads it. It is deliberately
// not claimSelect: a wake reads no delivery and no launch state, only who the claim is, the
// session it kept, the run it was serving, and its state with the source of a suspension, which
// together say whether a wake brings it back (supervise.Claim.Released).
const wakeClaimSelect = `select token, project, tree, issue, role, session, serving_generation, state, suspended_by
	from claims where project = $1`

// releasedClaimSelect reads the released claims among them: suspended with the source `released`
// (supervise.Claim.Released), which is the one state a wake acts on. Every other suspended claim
// is somebody else's to resume — an operator's, a close's — and a path that takes a released
// claim over (an operator's suspend, a linger) re-stamps its source, which takes it out of every
// read of it.
const releasedClaimSelect = wakeClaimSelect + ` and state = $2 and suspended_by = $3`

func scanWakeClaims(rows pgx.Rows) ([]supervise.Claim, error) {
	defer rows.Close()
	var out []supervise.Claim
	for rows.Next() {
		var c supervise.Claim
		var generation int64
		if err := rows.Scan(&c.Token, &c.Project, &c.Tree, &c.Issue, &c.Role, &c.Session, &generation, &c.State, &c.SuspendedBy); err != nil {
			return nil, err
		}
		if generation > 0 {
			c.ServingGeneration = uint64(generation)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReleasedClaimsOf is every released claim of issue, in token order, which is the set a wake of
// the whole pod resumes.
func ReleasedClaimsOf(ctx context.Context, tx pgx.Tx, project, issue string) ([]supervise.Claim, error) {
	rows, err := tx.Query(ctx, releasedClaimSelect+` and issue = $4 order by token`,
		project, string(supervise.StateSuspended), string(supervise.SuspendedByReleased), issue)
	if err != nil {
		return nil, fmt.Errorf("read the released claims of %s: %w", issue, err)
	}
	claims, err := scanWakeClaims(rows)
	if err != nil {
		return nil, fmt.Errorf("read the released claims of %s: %w", issue, err)
	}
	return claims, nil
}

// ClaimByToken is the claim a listener's report names by the role token a message was published
// to, in whatever state it is, and whether this daemon's project holds one. The listener knows
// nothing of which daemon owns a token, so every daemon reads its own claims and the one that
// holds the claim is the one that acts. The report marks the claim whatever its state
// (MarkMessage), since its wake may wait in the pacing in any of them, and wakes it only where it
// is released.
func (p *PodRecords) ClaimByToken(ctx context.Context, tx pgx.Tx, token string) (supervise.Claim, bool, error) {
	return p.listenerClaim(ctx, tx, `and token = $2`, token, "role token")
}

// ClaimBySession is the claim that keeps session, the session a listener's report names a message
// was sent to, and whether this daemon's project holds one. A released claim keeps its session
// id, which is what lets a message addressed to that session find the pod that must come back
// for it.
func (p *PodRecords) ClaimBySession(ctx context.Context, tx pgx.Tx, session string) (supervise.Claim, bool, error) {
	return p.listenerClaim(ctx, tx, `and session = $2`, session, "session")
}

func (p *PodRecords) listenerClaim(ctx context.Context, tx pgx.Tx, where, value, what string) (supervise.Claim, bool, error) {
	if value == "" {
		return supervise.Claim{}, false, nil
	}
	rows, err := tx.Query(ctx, wakeClaimSelect+" "+where+` order by token limit 1`, p.project, value)
	if err != nil {
		return supervise.Claim{}, false, fmt.Errorf("read the claim of %s %s: %w", what, value, err)
	}
	claims, err := scanWakeClaims(rows)
	if err != nil {
		return supervise.Claim{}, false, fmt.Errorf("read the claim of %s %s: %w", what, value, err)
	}
	if len(claims) == 0 {
		return supervise.Claim{}, false, nil
	}
	return claims[0], true, nil
}

// MarkMessage is MarkClaimMessageTx, as the workflow reaches it for a listener's report.
func (p *PodRecords) MarkMessage(ctx context.Context, tx pgx.Tx, token claim.Token, at time.Time) (time.Time, error) {
	return MarkClaimMessageTx(ctx, tx, token, at)
}

// MarkClaimMessage is MarkClaimMessageTx in a transaction of its own: the write the outbox makes
// before it runs a start row whose wake carried a message.
func MarkClaimMessage(ctx context.Context, pool *Pool, token claim.Token, at time.Time) (time.Time, error) {
	var mark time.Time
	err := Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		mark, err = MarkClaimMessageTx(ctx, tx, token, at)
		return err
	})
	return mark, err
}

// MarkClaimMessageTx marks the claim token for an Envoy message that has waited since at
// (claims.message_mark_at, dispatch://INFRA-5682 R1), and answers the mark it wrote, or else the
// mark that stands once it has run, the zero time for neither. A mark stands while it is later
// than the claim's last turn start (supervise.Claim.MessageMarked), and of two that stand the
// earlier is kept, since it is the message that has waited longest and the first one Envoy drops.
// So at replaces a mark the role has worked past, and never an earlier one that stands. Where no
// mark stands, at is written even when the role has taken a turn since it, and the answer is then
// a mark that does not stand, which moves no wake (the machine moves one only for a mark that
// stands, Claim.MessageMarked).
//
// A retired claim takes no mark, and neither does a token of no claim. Nothing waits for a
// retired claim and nothing is coming, and its token's next run is another run's: a mark left on
// it would put that run's first wake in the message queue and call it late for a message Envoy
// dropped long before (PutClaim clears the mark a claim holds as it retires).
//
// It is the only write of the column but that clear. A machine writes the whole claim it holds in
// memory (PutClaim), so a mark written with the claim would be undone by the next write of a
// machine that read the claim before the mark was made.
func MarkClaimMessageTx(ctx context.Context, tx pgx.Tx, token claim.Token, at time.Time) (time.Time, error) {
	var mark *time.Time
	err := tx.QueryRow(ctx, `with marked as (
			update claims set message_mark_at = $2 where token = $1 and state <> $3 and (
				message_mark_at is null or message_mark_at <= last_turn_start or
				($2 < message_mark_at and (last_turn_start is null or $2 > last_turn_start)))
			returning message_mark_at)
		select message_mark_at from marked
		union all
		select message_mark_at from claims where token = $1 and state <> $3
			and message_mark_at > coalesce(last_turn_start, '-infinity') and not exists (select 1 from marked)`,
		string(token), at, string(supervise.StateRetired)).Scan(&mark)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("mark the message wake of %s: %w", token, err)
	}
	return orZero(mark), nil
}

// CloseOpenResumes closes every resume of issue no first turn has stamped: what the transaction
// that marks a release which suspends a role writes, so the role's next wake records a resume of
// its own rather than meeting the one left open by a pod that has come and gone. A release that
// suspends no role calls it not at all, since it is no wake's to end (ReleaseIdleIssue).
//
// Closing is its own column (closed_at), never a first turn: first_turn_at is the turn the daemon
// observed, which the release records serve as firstTurnAt and the plugin's statistics measure the
// wake's wait by, and a resume closed here is precisely one whose first turn the daemon never saw.
// Written at the transaction's own now(), as the release's mark is, so the close and the mark it
// belongs to carry one time.
func CloseOpenResumes(ctx context.Context, tx pgx.Tx, project, issue string) error {
	_, err := tx.Exec(ctx, `update pod_resumes set closed_at = now()
		where project = $1 and issue = $2 and first_turn_at is null and closed_at is null`, project, issue)
	if err != nil {
		return fmt.Errorf("close the open resumes of %s: %w", issue, err)
	}
	return nil
}

// CancelPodBackstops drops the open backstops of issue that role armed, and every open backstop
// of the issue when role is empty. A wake resumes one role, and it is that role's backstop alone
// that has nothing left to do: another role of the same pod may be waiting on a deploy run with
// nothing but its own backstop to bring it back, and a pod woken for one role may be held again
// before it ever reaches that other role's work. A path that takes the whole released pod over
// (CancelPodBackstopsOf) names no role, because no backstop of it is left to act on.
func CancelPodBackstops(ctx context.Context, tx pgx.Tx, project, issue string, role claim.Role) error {
	_, err := tx.Exec(ctx, `delete from pod_wake_backstops
		where project = $1 and issue = $2 and not fired and ($3 = '' or role = $3)`,
		project, issue, string(role))
	if err != nil {
		return fmt.Errorf("cancel the backstops of %s: %w", issue, err)
	}
	return nil
}

// CancelPodBackstopsOf cancels, in a transaction of its own, the open backstops of issue that role
// armed, and every open backstop of the issue when role is empty (CancelPodBackstops), for a
// caller outside one. The outbox's `suspend` row takes one role of a released pod over and drops
// that role's backstop alone (decision 17 of dispatch://INFRA-3759), so a stop of another role of
// the issue — a pause's, a park's of the architect — never cancels a worker's backstop. The close
// of an issue or of its tree takes the whole released pod over, and the arming of a release's
// backstops drops what the release before it armed; both name no role. Dropping the backstops is
// the whole of what taking a released pod over leaves to do, since a wake's own record is the
// resume the index holds (TakePodWake).
func (s *Store) CancelPodBackstopsOf(ctx context.Context, project, issue string, role claim.Role) error {
	return Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		return CancelPodBackstops(ctx, tx, project, issue, role)
	})
}

// TakePodWake takes one wake outside any fact transaction, for the wakes the outbox takes: a
// notice, a hook prompt, a merge-queue removal or a merge forward's message that found the role it
// is for released (internal/daemon's wakeForDelivery). Inside a fact's transaction the workflow
// takes its wakes through PodRecords.TakePodWake, which is where taking a wake is written out.
//
// token is the claim the wake brings back, and the role it names is the role woken: a token that
// names no role is an error. mark is the pause that held the outbox row the wake is taken for
// (record.OutboxRow.HeldByPause, which the resume writes on every row its pause holds), 0 for a
// row no pause held. PodRecords.PauseOfWake reads which pause holds the wake from the token and
// the mark. A project's pause holds a row, whatever its due time, and the row runs, and wakes its
// role, only after the resume: its wake is a held wake of that pause, marked with it and paired
// with no release (PodRecords.TakePodWake, decision 15 of dispatch://INFRA-3759), as the wakes the
// workflow takes while the project is paused are, and so is the wake of a row written after the
// resume for a role that pause stopped. So the update round over the role's pod counts that
// pause's hold on to the role's let-out (PausePodHoldsSince), and the release records leave the
// wait out.
//
// project is the project token the claim-keyed tables hold, as it is for every other
// project-keyed method of this file: the daemon derives it once at boot (claim.ProjectToken,
// internal/daemon/bootreads.go) and its outbox carries that, so nothing is derived again here.
func (s *Store) TakePodWake(ctx context.Context, project, issue string, token claim.Token, kind string, at time.Time, mark int64) (bool, error) {
	role, ok := token.Role()
	if !ok {
		return false, fmt.Errorf("take a wake of %s: claim token %q names no role", issue, token)
	}
	records := &PodRecords{store: s, project: project}
	var taken bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		pause, _, err := records.PauseOfWake(ctx, tx, issue, token, mark)
		if err != nil {
			return err
		}
		taken, err = records.TakePodWake(ctx, tx, issue, role, kind, at, pause)
		return err
	})
	return taken, err
}

// ReleasedClaimsIn is every released claim of issue, in a transaction of its own: what the
// release's own arming of backstops reads, and what a caller outside a fact transaction asks.
func (s *Store) ReleasedClaimsIn(ctx context.Context, project, issue string) ([]supervise.Claim, error) {
	var claims []supervise.Claim
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		claims, err = ReleasedClaimsOf(ctx, tx, project, issue)
		return err
	})
	return claims, err
}

// ClaimRoleState is one claim of an issue as a log line names it: the role, and the state the
// record holds that role's claim in. It is the claim's state alone and never the whole claim,
// because the one reader of it logs what a pod's roles were doing when the pod went, and a
// reader of that line needs no session, no locator and no budget to understand it.
type ClaimRoleState struct {
	Role  claim.Role
	State supervise.ClaimState
}

// ClaimStates is every claim of one issue, in role order, in the one shape a log line renders:
// `architect=retired implementer=failed`. The release of a pod no role can use logs it beside
// the issue and the Sandbox it deleted (outbox.releaseIssue), since the whole case for that
// release is that every one of those claims is failed, retired or suspended, and the line is
// what lets a reader of the daemon's log check that for themselves.
type ClaimStates []ClaimRoleState

// String is the states as that line renders them, one `role=state` per claim, space separated.
func (c ClaimStates) String() string {
	states := make([]string, 0, len(c))
	for _, s := range c {
		states = append(states, string(s.Role)+"="+string(s.State))
	}
	return strings.Join(states, " ")
}

// MarshalText is String for a handler that encodes an attribute rather than formatting it:
// slog's text handler reads a TextMarshaler and its JSON handler marshals the value, and
// encoding/json writes a TextMarshaler as its text, so the line reads the same under either and
// a deployment that ships JSON logs gets one field rather than an array of objects.
func (c ClaimStates) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// ClaimStatesIn is the state of every claim of issue, in role order, whatever state it is in:
// what the teardown of a release that suspends no role reads before it deletes the pod
// (outbox.releaseIssue). It is ReleasedClaimsIn's neighbour and not its sibling — that read
// answers the released claims a wake resumes, and this one answers every claim, since a release
// whose whole case is that no claim of the issue can take a turn must say so claim by claim.
//
// One statement reads it in a read-only transaction of its own: there is nothing here for a wider
// transaction to make consistent, and the read is of a pod the release is about to delete anyway.
func (s *Store) ClaimStatesIn(ctx context.Context, project, issue string) (ClaimStates, error) {
	var states ClaimStates
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select role, state from claims
			where project = $1 and issue = $2 order by role`, project, issue)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var role, state string
			if err := rows.Scan(&role, &state); err != nil {
				return err
			}
			states = append(states, ClaimRoleState{Role: claim.Role(role), State: supervise.ClaimState(state)})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read the claim states of %s: %w", issue, err)
	}
	return states, nil
}

// PodWakeBackstop is one armed backstop: the phase worker it wakes and when it comes due.
type PodWakeBackstop struct {
	ID      int64
	Project string
	Issue   string
	Role    claim.Role
	Release int64
	DueAt   time.Time
}

// ArmPodBackstop arms the backstop of issue's role at due, over the release release names. A role
// that already has an open backstop keeps the one it has, so a release retried after a failure
// arms one backstop and not two.
func (s *Store) ArmPodBackstop(ctx context.Context, b PodWakeBackstop) error {
	return Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		return armPodBackstop(ctx, tx, b)
	})
}

// ArmPodBackstopTx is ArmPodBackstop inside a caller's transaction: the stop a project's pause
// makes of an open phase's worker arms that worker's backstop in the transaction that writes the
// stop (decision 6 of dispatch://INFRA-3759), so a committed stop has its backstop and a
// rolled-back one has none. It cancels no other backstop of the issue.
func ArmPodBackstopTx(ctx context.Context, tx pgx.Tx, b PodWakeBackstop) error {
	return armPodBackstop(ctx, tx, b)
}

func armPodBackstop(ctx context.Context, tx pgx.Tx, b PodWakeBackstop) error {
	if b.Project == "" || b.Issue == "" || b.Role == "" || b.DueAt.IsZero() {
		return errors.New("a pod wake backstop needs its project, issue, role and due time")
	}
	_, err := tx.Exec(ctx, `insert into pod_wake_backstops (project, issue, role, release_id, due_at)
		values ($1, $2, $3, $4, $5) on conflict do nothing`,
		b.Project, b.Issue, string(b.Role), b.Release, b.DueAt)
	if err != nil {
		return fmt.Errorf("arm the backstop of %s's %s: %w", b.Issue, b.Role, err)
	}
	return nil
}

// DuePodBackstops are project's open backstops due at or before now, oldest first: the ones this
// pass fires. One that came due while the daemon was down is in this read at the next pass after
// the daemon starts, which is what makes the backstop a stored row rather than a timer.
func (s *Store) DuePodBackstops(ctx context.Context, project string, now time.Time) ([]PodWakeBackstop, error) {
	return ReadOnly(ctx, s.pool, "read the due backstops of "+project, func(ctx context.Context, tx pgx.Tx) ([]PodWakeBackstop, error) {
		return duePodBackstops(ctx, tx, project, now)
	})
}

func duePodBackstops(ctx context.Context, tx pgx.Tx, project string, now time.Time) ([]PodWakeBackstop, error) {
	rows, err := tx.Query(ctx, `select id, project, issue, role, release_id, due_at
		from pod_wake_backstops where project = $1 and not fired and due_at <= $2 order by due_at, id`,
		project, now)
	if err != nil {
		return nil, fmt.Errorf("read the due backstops of %s: %w", project, err)
	}
	defer rows.Close()
	var out []PodWakeBackstop
	for rows.Next() {
		var b PodWakeBackstop
		var role string
		if err := rows.Scan(&b.ID, &b.Project, &b.Issue, &role, &b.Release, &b.DueAt); err != nil {
			return nil, fmt.Errorf("read the due backstops of %s: %w", project, err)
		}
		b.Role = claim.Role(role)
		out = append(out, b)
	}
	return out, rows.Err()
}

// RecordPodWakeTx is RecordPodWake inside a caller's transaction: a wake decided by an intake
// fact records itself with the outbox row that brings the pod back, so a committed wake is a
// recorded wake and a rolled-back one is neither. It writes the same pod_resumes row.
//
// The insert is the whole of taking a wake. A role has at most one resume no first turn has
// stamped (pod_resumes_one_open_per_role), so a second wake of a role whose pod is already
// coming back conflicts and writes nothing, and `taken` is false: the index decides it inside
// the transaction, where a read could be overtaken between its answer and the insert. The
// caller's transaction is what makes that atomic, so the workflow's four wakes and the outbox's
// delivery wake are held to it alike.
//
// The row's stuck clock starts at the wake's own moment (clock_at); a reservation of its launch
// or a resume of the project moves it later (SetResumeClock, SetProjectResumeClocksTx). res.Pause
// is the pause that holds the wake, 0 for none; a held wake pairs with no release
// (pod_resumes_held_wake_pairs_with_no_release), so its caller passes release 0.
func RecordPodWakeTx(ctx context.Context, tx pgx.Tx, res PodResume) (id int64, taken bool, err error) {
	if res.Project == "" || res.Issue == "" || res.Kind == "" {
		return 0, false, errors.New("a pod resume record needs its project, issue and wake kind")
	}
	if res.Pause < 0 || (res.Pause > 0 && res.Release != 0) {
		return 0, false, fmt.Errorf("a wake held by pause %d of %s pairs with release 0, not %d", res.Pause, res.Issue, res.Release)
	}
	// The release the wake was taken after, of whatever kind, which is what the heal of a stuck
	// resume compares against (newestPodRelease, ClosePodGoneResume). It is read here, in the
	// take's own transaction, so the row answers the heal's question with the reading the heal
	// makes, and the two cannot drift. It is not res.Release, the release this wake ends, which
	// is read with the roles filter the woken share rests on (LatestPodRelease).
	seen, err := newestPodRelease(ctx, tx, res.Project, res.Issue)
	if err != nil {
		return 0, false, err
	}
	err = tx.QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at, seen_release, clock_at, pause_id)
		values ($1, $2, $3, $4, $5, $6, $7, $6, nullif($8::bigint, 0)) on conflict do nothing returning id`,
		res.Release, res.Project, res.Issue, res.Role, res.Kind, res.At, seen, res.Pause).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("record the wake of %s: %w", res.Issue, err)
	}
	return id, true, nil
}

// newestPodRelease is the newest release of issue of whatever kind, or 0 when the issue has none.
// It is the one reading of "the pod of this issue was taken away": a wake records it as the
// release it was taken after (RecordPodWakeTx), and the heal asks whether a release has been
// recorded since (ClosePodGoneResume), so the writer and the healer read the same thing.
//
// It counts a release that suspended no role, where LatestPodRelease below passes over one. The
// two answer different questions: this one is whether the pod went away, and every release takes
// the pod away, while that one is which release a wake ends, which a release that woke nobody
// never is.
func newestPodRelease(ctx context.Context, tx pgx.Tx, project, issue string) (int64, error) {
	var id int64
	if err := tx.QueryRow(ctx, `select coalesce(max(id), 0) from pod_releases
		where project = $1 and issue = $2`, project, issue).Scan(&id); err != nil {
		return 0, fmt.Errorf("read the newest release of %s: %w", issue, err)
	}
	return id, nil
}

// LatestPodRelease is the newest release of issue that suspended a role, or 0 when none is: the
// release a wake of that pod ends, which is what pairs a resume with the release it measures.
//
// A release that suspended no role is passed over, whichever rule wrote it, and the filter on a
// roles column that is not empty is what says so. Two releases record their roles empty: every
// release of a pod no claim of the issue can use (workflow.UnownedPodRelease), which changes no
// claim and saves no bundle, and the rare idle release whose every role stop found a claim that
// had failed or retired since the mark, which suspends nobody and so records nobody
// (pod_releases.roles, written from the set ReleaseSuspendedClaim accumulated). Neither comes
// back on a wake — the first's pod comes back at the next start of a claim of the issue, and
// the second suspended no claim for a wake to resume — so no wake ends either, and a wake that
// paired with one would leave the release it really ends with no woken moment of its own: the
// pod hours of that release would run from its own moment to every later read, for ever, and
// the woken share would count it as a release nothing ever woke. The claim a wake is for was
// suspended by the release it pairs with, and that release named a role. So the filter is of
// what a release did and never of which rule wrote it: narrowing it to the second rule would
// pair a wake with an idle release that ended nothing.
func LatestPodRelease(ctx context.Context, tx pgx.Tx, project, issue string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `select id from pod_releases where project = $1 and issue = $2
		and roles <> '' order by id desc limit 1`, project, issue).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the newest release of %s: %w", issue, err)
	}
	return id, nil
}

// OpenPodResume is the newest wake of issue's role with no first turn yet, the project pause that
// held it (pod_resumes.pause_id, 0 for a wake no pause held), and whether there is one: the resume
// a pod-ready or first-turn observation stamps, and the resume a delivery wake meets when the take
// refuses it (internal/daemon's wakeForDelivery), which says by the pause whether that resume is a
// pause's held wake waiting on its start (decision 15 of dispatch://INFRA-3759). It is read rather
// than remembered so a daemon that restarted between the wake and the first turn still stamps it.
func (s *Store) OpenPodResume(ctx context.Context, project, issue string, role claim.Role) (id, pause int64, found bool, err error) {
	err = TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		id, pause, found, err = openPodResume(ctx, tx, project, issue, role)
		return err
	})
	return id, pause, found, err
}

// openPodResume is OpenPodResume inside a transaction.
func openPodResume(ctx context.Context, tx pgx.Tx, project, issue string, role claim.Role) (id, pause int64, found bool, err error) {
	err = tx.QueryRow(ctx, `select id, coalesce(pause_id, 0) from pod_resumes
		where project = $1 and issue = $2 and role = $3 and first_turn_at is null and closed_at is null
		order by id desc limit 1`, project, issue, string(role)).Scan(&id, &pause)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("read the open resume of %s's %s: %w", issue, role, err)
	}
	return id, pause, true, nil
}

// CloseRoleOpenResumeTx closes, at at, the open resume of issue's role — the one no first turn has
// stamped and nothing has closed — and answers whether one was open: the stop transaction of a
// pause closes it (decision 6), since the role it waited for has stopped again before it took a
// turn. project is the project token, as pod_resumes holds it.
func CloseRoleOpenResumeTx(ctx context.Context, tx pgx.Tx, project, issue string, role claim.Role, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `update pod_resumes set closed_at = $4
		where project = $1 and issue = $2 and role = $3 and first_turn_at is null and closed_at is null`,
		project, issue, string(role), at)
	if err != nil {
		return false, fmt.Errorf("close the open resume of %s's %s: %w", issue, role, err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetResumeClock moves the stuck clock (pod_resumes.clock_at) of the open resume of issue's role
// to at, the moment that role's launch stopped waiting for the wake pacing (decision 14), and
// starts a clock that wait had stopped (pod_resumes.queued, SetResumeQueued). It answers whether
// the role had an open resume and never moves its clock backwards (greatest): a clock a resume of
// the project already moved stays. project is the project token, as pod_resumes holds it.
func (s *Store) SetResumeClock(ctx context.Context, project, issue string, role claim.Role, at time.Time) (bool, error) {
	var changed bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update pod_resumes set clock_at = greatest(clock_at, $4), queued = false
			where project = $1 and issue = $2 and role = $3 and first_turn_at is null and closed_at is null`,
			project, issue, string(role), at)
		if err != nil {
			return fmt.Errorf("set the stuck clock of %s's %s: %w", issue, role, err)
		}
		changed = tag.RowsAffected() > 0
		return nil
	})
	return changed, err
}

// SetResumeQueued stops the stuck clock of the open resume of issue's role (pod_resumes.queued):
// that role's launch has begun to wait in the wake pacing's queue, where it has no moment until
// the pacer lets it out, so there is none to move the clock to (dispatch://INFRA-5682 R1). The
// stuck warning and the stranded report pass over a resume whose clock is stopped
// (podResumesWithoutAFirstTurn), and the end of the wait starts it again (SetResumeClock). project
// is the project token, as pod_resumes holds it.
func (s *Store) SetResumeQueued(ctx context.Context, project, issue string, role claim.Role) error {
	return Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update pod_resumes set queued = true
			where project = $1 and issue = $2 and role = $3 and first_turn_at is null and closed_at is null and not queued`,
			project, issue, string(role))
		if err != nil {
			return fmt.Errorf("stop the stuck clock of %s's %s: %w", issue, role, err)
		}
		return nil
	})
}

// StartQueuedResumeClocks starts every stuck clock of project that a wait in the wake pacing's
// queue had stopped (pod_resumes.queued), at at and never backwards, and answers how many it
// started. It is the boot's: the queue is the daemon's memory, so the launches the previous daemon
// left waiting in it wait in none now, and a clock nothing starts again would keep its resume out
// of the stuck warning and the stranded report for good. project is the project token, as
// pod_resumes holds it.
func (s *Store) StartQueuedResumeClocks(ctx context.Context, project string, at time.Time) (int64, error) {
	var started int64
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update pod_resumes set clock_at = greatest(clock_at, $2), queued = false
			where project = $1 and queued`, project, at)
		if err != nil {
			return fmt.Errorf("start the stopped stuck clocks of %s: %w", project, err)
		}
		started = tag.RowsAffected()
		return nil
	})
	return started, err
}

// SetProjectResumeClocksTx moves the stuck clock of every open resume of project to at, the
// resume's moment (decision 14), never backwards (greatest), and answers how many resumes are
// open. A wake that waited out the pause is no wake that is stuck. Only the resume's own
// transaction (Resume) sets every clock of a project at once, so it has no form on the pool.
// project is the project token, as pod_resumes holds it.
func SetProjectResumeClocksTx(ctx context.Context, tx pgx.Tx, project string, at time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `update pod_resumes set clock_at = greatest(clock_at, $2)
		where project = $1 and first_turn_at is null and closed_at is null`, project, at)
	if err != nil {
		return 0, fmt.Errorf("set the stuck clocks of %s's open resumes: %w", project, err)
	}
	return tag.RowsAffected(), nil
}

// ListenerReadyAt is when the Envoy listener last reported itself ready for project, and whether
// it ever has. A daemon that has read no report releases nothing.
func (s *Store) ListenerReadyAt(ctx context.Context, project string) (time.Time, bool, error) {
	var at time.Time
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select ready_at from listener_reports where project = $1`, project).Scan(&at)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the listener's report for %s: %w", project, err)
	}
	return at.UTC(), true, nil
}

// ListenerLive is R7's verdict for the idle rule: whether the Envoy listener has reported itself
// ready for project within window of now. It answers the report's time and whether there was one
// too, so a caller can say in its log which of the three it is — never reported, reported too
// long ago, or live.
//
// A daemon with no fresh report releases nothing. Without a positive signal "the listener sends
// no reports" and "the listener has nothing to report" are the same silence, and a pod released
// into that silence would not come back for a role message or an agent message, because only the
// listener sees either.
func (s *Store) ListenerLive(ctx context.Context, project string, now time.Time, window time.Duration) (bool, time.Time, bool, error) {
	at, found, err := s.ListenerReadyAt(ctx, project)
	if err != nil || !found {
		return false, at, found, err
	}
	return now.UTC().Sub(at) <= window, at, true, nil
}

// PodResumeReadyAt is when resume id's replacement pod was observed ready, zero while nothing
// has observed it. The stuck warning reads it to name the step the resume waits on: the pod, or
// the woken role's first turn.
func (s *Store) PodResumeReadyAt(ctx context.Context, id int64) (time.Time, error) {
	var at *time.Time
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select pod_ready_at from pod_resumes where id = $1`, id).Scan(&at)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read the pod-ready stamp of resume %d: %w", id, err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return at.UTC(), nil
}

// PodRecords is the pod tables of one project as a caller outside the store reads and writes
// them: the listener's liveness report, the claims a release left released, the wake that brings
// one back and the backstop a wake fires. It derives the project token the claim-keyed tables
// hold (claim.ProjectToken) once, from the project key its caller already has, so no caller
// carries a second spelling of the project — which is what the workflow engine's own copy of it
// was for, and what filed the listener's report under a project the daemon never read.
//
// The workflow takes it as an interface (workflow.Pods), so nothing of the engine reaches
// Postgres directly.
type PodRecords struct {
	store   *Store
	project string
}

// PodRecordsOf is project's pod tables, refusing a project key that is no project
// (claim.ProjectToken).
func (s *Store) PodRecordsOf(project string) (*PodRecords, error) {
	token, err := claim.ProjectToken(project)
	if err != nil {
		return nil, fmt.Errorf("read the pod records of %s: %w", project, err)
	}
	return &PodRecords{store: s, project: token}, nil
}

// RecordListenerReady stamps when the Envoy listener last said it is alive and reporting, which
// is the whole of R7's gate: the daemon releases no pod while that stamp is older than the
// freshness window.
func (p *PodRecords) RecordListenerReady(ctx context.Context, tx pgx.Tx, at time.Time) error {
	_, err := tx.Exec(ctx, `insert into listener_reports (project, ready_at) values ($1, $2)
		on conflict (project) do update set ready_at = greatest(listener_reports.ready_at, excluded.ready_at)`,
		p.project, at)
	if err != nil {
		return fmt.Errorf("record the listener's report for %s: %w", p.project, err)
	}
	return nil
}

// ReleasedClaimOfRole is the released claim of issue's role, and whether there is one.
func (p *PodRecords) ReleasedClaimOfRole(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (supervise.Claim, bool, error) {
	claims, err := ReleasedClaimsOf(ctx, tx, p.project, issue)
	if err != nil {
		return supervise.Claim{}, false, err
	}
	for _, c := range claims {
		if c.Role == role {
			return c, true, nil
		}
	}
	return supervise.Claim{}, false, nil
}

// TakePodWake takes one wake of issue's role inside the caller's transaction, and is the whole
// of what taking a wake is: the mark under the wake's own key, the resume recorded against the
// release that wake ends, and the role's own backstop cancelled. It answers whether this call
// took the wake; false is a wake another pass already took, and then the caller resumes nothing,
// because the first wake is already bringing the pod back.
//
// A role is woken once while its pod is coming back, whatever else reaches it: at most one
// resume of a role has no first turn stamped on it (pod_resumes_one_open_per_role), so the
// insert that records this wake is also what takes it. A CI settlement wakes the pod and, in the
// same transaction, sends that worker a role message, whose listener report then arrives while
// the claim is still released; both are the same pod coming back for the same role, and the
// second writes nothing rather than leaving two resumes and two start rows — one describing the
// daemon's own settlement to the worker as a message somebody published — against R6's one task
// per wake. Another role of the same pod is not held by it: a wake resumes one claim, and a role
// whose wake was dropped would have no process in a pod that is running.
//
// The index decides it inside the caller's transaction, so the workflow's four wakes and the
// outbox's delivery wake, which runs in a transaction of its own, are held to it alike; a read
// could be overtaken between its answer and the insert, and this cannot. A wake refused here
// writes nothing of its own, and the cause is not lost, since the message or settlement that
// wakes a role still reaches it once the pod is back — but the resume that refused it stands,
// so a caller whose claim is still not running resumes it all the same rather than reading the
// refusal as a pod already on its way: the outbox's wakeForDelivery resumes the claim in process,
// and the workflow's wakeReleased closes a resume whose pod has gone away before it takes its
// wake (ClosePodGoneResume). The transaction of a release that suspends a role closes the resumes
// left open (CloseOpenResumes), so a pod that came back and whose role took no turn the daemon
// saw holds nothing back at the next release; a release that suspends none closes no resume,
// because it ends no wake, and the resumes its pod leaves behind are the heal's.
//
// Every wake runs through here — the four the workflow applies inside a fact's transaction, the
// delivery wake the outbox takes in one of its own (Store.TakePodWake) and the stop transaction's
// owed launch (pods.WakePause) — so there is one account of what a wake writes and one order it
// writes it in.
//
// pause is the pause that holds the wake (PauseOfWake decides it for every wake but the stop's
// owed launch, which names its own pause), and 0 is a wake no pause holds. A held wake records its
// pause on the resume (pod_resumes.pause_id) and pairs with release 0 rather than the issue's
// latest release (decision 15 of dispatch://INFRA-3759): the pause, not a release, is what kept
// the role down, so the release measurements leave its wait out.
func (p *PodRecords) TakePodWake(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, kind string, at time.Time, pause int64) (bool, error) {
	var release int64
	if pause == 0 {
		var err error
		if release, err = LatestPodRelease(ctx, tx, p.project, issue); err != nil {
			return false, err
		}
	}
	_, taken, err := RecordPodWakeTx(ctx, tx, PodResume{Release: release, Project: p.project, Issue: issue,
		Role: string(role), Kind: kind, At: at, Pause: pause})
	if err != nil || !taken {
		return false, err
	}
	return true, CancelPodBackstops(ctx, tx, p.project, issue, role)
}

// PauseOfWake is the one rule of which pause holds a wake of claim token, issue's role (decision
// 15 of dispatch://INFRA-3759), and every wake but the stop's owed launch reads it: the outbox's
// delivery wake (Store.TakePodWake) and the workflow's wakes (its wakeReleased). The stop
// transaction's owed launch (pods.WakePause) names its own pause. held is the first of these that
// names a pause, and 0 where none does:
//
//   - the project's open pause, read on tx with its row locked for share
//     (LockOpenPauseByTokenTx), so the resume's end of it waits for the wake to commit: a wake
//     taken while the project is paused is that pause's, the wake of a row a lane leased before
//     the pause and ran while it stood among them. It is read by the token of the project whose
//     pod tables these are (readOpenPauseByToken says why);
//   - mark, the pause that held the outbox row the wake is taken for (record.OutboxRow.HeldByPause),
//     0 for a wake taken for no row, as the workflow's are;
//   - stopping, the pause that stopped the claim (StoppingPauseTx).
//
// stopping is answered whatever held is, because it is also what the woken role's task names as
// what stopped its session (workflow.WakeTask, decision 16).
func (p *PodRecords) PauseOfWake(ctx context.Context, tx pgx.Tx, issue string, token claim.Token, mark int64) (held, stopping int64, err error) {
	// open is the zero Pause, whose id is 0, while the project runs (scanOpenPause).
	open, _, err := LockOpenPauseByTokenTx(ctx, tx, p.project)
	if err != nil {
		return 0, 0, err
	}
	if stopping, err = StoppingPauseTx(ctx, tx, p.project, issue, token); err != nil {
		return 0, 0, err
	}
	return cmp.Or(open.ID, mark, stopping), stopping, nil
}

// ClosePodGoneResume closes the open resume of issue's role when the pod that resume was waiting
// for has gone away: a release of the issue, of whatever kind, recorded since the wake was taken
// (newestPodRelease, seen_release). Without it that row would refuse every later wake of the role
// for as long as the issue lives, because a release that suspends no role closes no resume and a
// pod fully released is never released again.
//
// It is one update that matches nothing in the ordinary case, so a caller runs it before the take
// rather than after a refusal: a resume of a pod still coming back was written after the newest
// release there is, so no release stands above its own, and the row stands.
//
// It answers the issue's newest release where it closed a row, and 0 where it closed none, so the
// caller can say in the log that a pod went away under an open resume. That release is the one
// standing above the resume's own, which with two releases since the wake is the later of them
// rather than the one that took the pod. Nothing else records that: the row it
// closes leaves the stuck warning and the stranded report with it (release_records.go's
// podResumesWithoutAFirstTurn), so without the line a pod vanishing under a waiting role is
// written down nowhere.
//
// The other way a resume sticks — a daemon that stopped between the take and the resume it leads
// to, with no release since — is the outbox's to heal, in process, because the claim's own state
// is what tells it (the delivery wake, internal/daemon's wakeForDelivery).
func (p *PodRecords) ClosePodGoneResume(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (int64, error) {
	newest, err := newestPodRelease(ctx, tx, p.project, issue)
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `update pod_resumes set closed_at = now()
		where project = $1 and issue = $2 and role = $3 and first_turn_at is null and closed_at is null
		and seen_release < $4`, p.project, issue, string(role), newest)
	if err != nil {
		return 0, fmt.Errorf("close the stale resume of %s's %s: %w", issue, role, err)
	}
	if tag.RowsAffected() == 0 {
		return 0, nil
	}
	return newest, nil
}

// FirePodBackstop marks backstop id fired and answers whether this call was the one that fired
// it, so a backstop read by two passes wakes its pod once.
func (p *PodRecords) FirePodBackstop(ctx context.Context, tx pgx.Tx, id int64) (bool, error) {
	tag, err := tx.Exec(ctx, `update pod_wake_backstops set fired = true where id = $1 and not fired`, id)
	if err != nil {
		return false, fmt.Errorf("fire backstop %d: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// OpenPodBackstop is whether issue's role has a backstop armed and not yet fired
// (pod_wake_backstops, `not fired`): the release that suspended the role with its phase open
// armed it, and it wakes the role when it comes due unless a wake takes it first (TakePodWake
// cancels it) or a path that takes the pod over drops it (CancelPodBackstops).
func (p *PodRecords) OpenPodBackstop(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (bool, error) {
	var open bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from pod_wake_backstops
		where project = $1 and issue = $2 and role = $3 and not fired)`, p.project, issue, string(role)).Scan(&open); err != nil {
		return false, fmt.Errorf("read the open backstop of %s's %s: %w", issue, role, err)
	}
	return open, nil
}
