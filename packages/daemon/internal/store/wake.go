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

// releasedClaimSelect reads the columns a wake needs of a released claim: a claim this daemon's
// project holds, suspended with the source `released` (supervise.Claim.Released), which is the
// one state a wake acts on. Every other suspended claim is somebody else's to resume — an
// operator's, a close's — and a path that takes a released claim over (an operator's suspend, a
// linger) re-stamps its source, which takes it out of every read here.
//
// It is deliberately not claimSelect: a wake reads no delivery and no launch state, only who the
// claim is, the session it kept and the run it was serving.
const releasedClaimSelect = `select token, project, tree, issue, role, session, serving_generation
	from claims where project = $1 and state = $2 and suspended_by = $3`

func scanReleasedClaims(rows pgx.Rows) ([]supervise.Claim, error) {
	defer rows.Close()
	var out []supervise.Claim
	for rows.Next() {
		c := supervise.Claim{State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased}
		var generation int64
		if err := rows.Scan(&c.Token, &c.Project, &c.Tree, &c.Issue, &c.Role, &c.Session, &generation); err != nil {
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
	claims, err := scanReleasedClaims(rows)
	if err != nil {
		return nil, fmt.Errorf("read the released claims of %s: %w", issue, err)
	}
	return claims, nil
}

// oneReleasedClaim is the one released claim of project that where selects, and whether there is
// one.
func oneReleasedClaim(ctx context.Context, tx pgx.Tx, where, project, value, what string) (supervise.Claim, bool, error) {
	if value == "" {
		return supervise.Claim{}, false, nil
	}
	rows, err := tx.Query(ctx, releasedClaimSelect+" "+where+` order by token limit 1`,
		project, string(supervise.StateSuspended), string(supervise.SuspendedByReleased), value)
	if err != nil {
		return supervise.Claim{}, false, fmt.Errorf("read the released claim of %s %s: %w", what, value, err)
	}
	claims, err := scanReleasedClaims(rows)
	if err != nil {
		return supervise.Claim{}, false, fmt.Errorf("read the released claim of %s %s: %w", what, value, err)
	}
	if len(claims) == 0 {
		return supervise.Claim{}, false, nil
	}
	return claims[0], true, nil
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

// CancelPodBackstops drops the open backstop of issue's claim token, and every open backstop of the
// issue when token is empty. A wake resumes one claim, and it is that claim's backstop alone that
// has nothing left to do: another claim of the same pod, of another role or of the same one, may be
// waiting on a deploy run with nothing but its own backstop to bring it back, and a pod woken for
// one claim may be held again before it ever reaches that other claim's work. A path that takes
// the whole released pod over (CancelPodBackstopsOf) names no claim, because no backstop of it is
// left to act on.
func CancelPodBackstops(ctx context.Context, tx pgx.Tx, project, issue string, token claim.Token) error {
	_, err := tx.Exec(ctx, `delete from pod_wake_backstops
		where project = $1 and issue = $2 and not fired and ($3 = '' or claim = $3)`,
		project, issue, string(token))
	if err != nil {
		return fmt.Errorf("cancel the backstops of %s: %w", issue, err)
	}
	return nil
}

// CancelPodBackstopsOf cancels, in a transaction of its own, the open backstop of issue's claim
// token, and every open backstop of the issue when token is empty (CancelPodBackstops), for a
// caller outside one. The outbox's `suspend` row takes one claim of a released pod over and drops
// that claim's backstop alone (decision 17 of dispatch://INFRA-3759), so a stop of another claim of
// the issue — a pause's, a park's of the architect, a stop of the role's own claim beside the claim
// record.WorkerClaim gives its step — never cancels a worker's backstop. The
// close of an issue or of its tree takes
// the whole released pod over, and the arming of a release's backstops drops what the release
// before it armed; both name no claim. Dropping the backstops is the whole of what taking a
// released pod over leaves to do, since a wake's own record is the resume the index holds
// (TakePodWake).
func (s *Store) CancelPodBackstopsOf(ctx context.Context, project, issue string, token claim.Token) error {
	return Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		return CancelPodBackstops(ctx, tx, project, issue, token)
	})
}

// TakePodWake takes one wake outside any fact transaction, for the wakes the outbox takes: a
// notice, a hook prompt, a merge-queue removal or a merge forward's message that found the role it
// is for released (internal/daemon's wakeForDelivery). Inside a fact's transaction the workflow
// takes its wakes through PodRecords.TakePodWake, which is where taking a wake is written out.
//
// token is the claim the wake brings back and role the role that claim holds now (claims.role),
// which the caller has read with the claim: a claim's token names its agent and no role, so it is
// never read off the token. mark is the pause that held the outbox row the wake is taken for
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
func (s *Store) TakePodWake(ctx context.Context, project, issue string, token claim.Token, role claim.Role, kind string, at time.Time, mark int64) (bool, error) {
	records := &PodRecords{store: s, project: project}
	var taken bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		pause, _, err := records.PauseOfWake(ctx, tx, issue, token, mark)
		if err != nil {
			return err
		}
		taken, err = records.TakePodWake(ctx, tx, issue, role, token, kind, at, pause)
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

// PodWakeBackstop is one armed backstop: the phase worker it wakes and when it comes due. Claim
// is the token of that worker's claim (pod_wake_backstops.claim), the claim record.WorkerClaim
// gave the open role's step when it was armed; Role is the role that claim held then.
type PodWakeBackstop struct {
	ID      int64
	Project string
	Issue   string
	Claim   claim.Token
	Role    claim.Role
	Release int64
	DueAt   time.Time
}

// ArmPodBackstop arms the backstop of issue's claim at due, over the release release names. A
// claim that already has an open backstop keeps the one it has, so a release retried after a
// failure arms one backstop and not two; another claim of the same role gets one of its own
// (pod_wake_backstops_open_claim).
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
	if b.Project == "" || b.Issue == "" || b.Claim == "" || b.Role == "" || b.DueAt.IsZero() {
		return errors.New("a pod wake backstop needs its project, issue, claim, role and due time")
	}
	_, err := tx.Exec(ctx, `insert into pod_wake_backstops (project, issue, claim, role, release_id, due_at)
		values ($1, $2, $3, $4, $5, $6) on conflict do nothing`,
		b.Project, b.Issue, string(b.Claim), string(b.Role), b.Release, b.DueAt)
	if err != nil {
		return fmt.Errorf("arm the backstop of %s's claim %s: %w", b.Issue, b.Claim, err)
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
	rows, err := tx.Query(ctx, `select id, project, issue, claim, role, release_id, due_at
		from pod_wake_backstops where project = $1 and not fired and due_at <= $2 order by due_at, id`,
		project, now)
	if err != nil {
		return nil, fmt.Errorf("read the due backstops of %s: %w", project, err)
	}
	defer rows.Close()
	var out []PodWakeBackstop
	for rows.Next() {
		var b PodWakeBackstop
		var token, role string
		if err := rows.Scan(&b.ID, &b.Project, &b.Issue, &token, &role, &b.Release, &b.DueAt); err != nil {
			return nil, fmt.Errorf("read the due backstops of %s: %w", project, err)
		}
		b.Claim, b.Role = claim.Token(token), claim.Role(role)
		out = append(out, b)
	}
	return out, rows.Err()
}

// RecordPodWakeTx is RecordPodWake inside a caller's transaction: a wake decided by an intake
// fact records itself with the outbox row that brings the pod back, so a committed wake is a
// recorded wake and a rolled-back one is neither. It writes the same pod_resumes row.
//
// The insert is the whole of taking a wake. A claim has at most one resume no first turn has
// stamped and nothing has closed (pod_resumes_one_open_per_claim), so a second wake of a claim
// whose pod is already coming back for it conflicts and writes nothing, and `taken` is false: the
// index decides it inside the transaction, where a read could be overtaken between its answer and
// the insert. Another claim of the same role takes a wake of its own: two claims of one issue can
// hold one role (record.WorkerClaim), and each is a claim of its own to bring back. The
// caller's transaction is what makes that atomic, so the workflow's four wakes and the outbox's
// delivery wake are held to it alike.
//
// The row's stuck clock starts at the wake's own moment (clock_at); a reservation of its launch
// or a resume of the project moves it later (SetResumeClock, SetProjectResumeClocksTx). res.Pause
// is the pause that holds the wake, 0 for none; a held wake pairs with no release
// (pod_resumes_held_wake_pairs_with_no_release), so its caller passes release 0.
func RecordPodWakeTx(ctx context.Context, tx pgx.Tx, res PodResume) (id int64, taken bool, err error) {
	if res.Project == "" || res.Issue == "" || res.Claim == "" || res.Kind == "" {
		return 0, false, errors.New("a pod resume record needs its project, issue, claim and wake kind")
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
	err = tx.QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, claim, kind, at, seen_release, clock_at, pause_id)
		values ($1, $2, $3, $4, $9, $5, $6, $7, $6, nullif($8::bigint, 0))
		on conflict do nothing returning id`,
		res.Release, res.Project, res.Issue, res.Role, res.Kind, res.At, seen, res.Pause, res.Claim).Scan(&id)
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

// OpenPodResume is the newest wake with no first turn yet that names token, whatever role the
// claim held when its wake took it, the project pause that held it (pod_resumes.pause_id, 0 for a
// wake no pause held), and whether there is one: the resume a pod-ready or first-turn observation
// stamps, and the resume a delivery wake meets when the take refuses it (internal/daemon's
// wakeForDelivery), which says by the pause whether that resume is a pause's held wake waiting on
// its start (decision 15 of dispatch://INFRA-3759). The agent of a key takes the role of each step
// it works, so its role can change between its wake and its first turn, and the resume stays its
// own. It is read rather than remembered so a daemon that restarted between the wake and the
// first turn still stamps it.
func (s *Store) OpenPodResume(ctx context.Context, project, issue string, token claim.Token) (id, pause int64, found bool, err error) {
	err = TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		id, pause, found, err = openPodResume(ctx, tx, project, issue, token)
		return err
	})
	return id, pause, found, err
}

// openPodResume is OpenPodResume inside a transaction: OpenPodResume's own, or the transaction of
// a cancel of a release made while the project is paused, which reads by it whether a wake
// already owes a claim the release stopped its launch (stopReleasedForPause). Two claims can hold
// one role, and the wake brought back one of them, so the resume is read for the claim
// (pod_resumes.claim).
func openPodResume(ctx context.Context, tx pgx.Tx, project, issue string, token claim.Token) (id, pause int64, found bool, err error) {
	err = tx.QueryRow(ctx, `select id, coalesce(pause_id, 0) from pod_resumes
		where project = $1 and issue = $2 and claim = $3 and first_turn_at is null and closed_at is null
		order by id desc limit 1`, project, issue, string(token)).Scan(&id, &pause)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("read the open resume of %s's claim %s: %w", issue, token, err)
	}
	return id, pause, true, nil
}

// CloseRoleOpenResumeTx closes, at at, the open resume that names token, whatever role the claim
// held at its wake — the one no first turn has stamped and nothing has closed — and answers
// whether one was open: the stop transaction of a pause closes it (decision 6), since the claim it
// waited for has stopped again before it took a turn. Another claim of the role keeps its resume.
// project is the project token, as pod_resumes holds it.
func CloseRoleOpenResumeTx(ctx context.Context, tx pgx.Tx, project, issue string, token claim.Token, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `update pod_resumes set closed_at = $3
		where project = $1 and issue = $2 and claim = $4 and first_turn_at is null and closed_at is null`,
		project, issue, at, string(token))
	if err != nil {
		return false, fmt.Errorf("close the open resume of %s's claim %s: %w", issue, token, err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetResumeClock moves the stuck clock (pod_resumes.clock_at) of the open resume that names
// token, whatever role the claim held at its wake, to at, the moment the wake pacing lets that
// claim's launch out (decision 14), and answers whether the claim had an open resume. Two claims
// can hold one role, and the let-out is one claim's, so another claim's resume keeps its clock. It
// never moves a clock backwards (greatest): a clock a resume of the project or a later reservation
// already moved stays. project is the project token, as pod_resumes holds it.
func (s *Store) SetResumeClock(ctx context.Context, project, issue string, token claim.Token, at time.Time) (bool, error) {
	var changed bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update pod_resumes set clock_at = greatest(clock_at, $3)
			where project = $1 and issue = $2 and claim = $4 and first_turn_at is null and closed_at is null`,
			project, issue, at, string(token))
		if err != nil {
			return fmt.Errorf("set the stuck clock of %s's claim %s: %w", issue, token, err)
		}
		changed = tag.RowsAffected() > 0
		return nil
	})
	return changed, err
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

// ReleasedClaimByToken is the released claim whose token is token, and whether this daemon's
// project holds one. The listener reports the token of a role topic it saw a message published
// to, a claim token, which names the claim's agent, and knows nothing of which daemon owns it, so
// every daemon reads its own claims and the one that holds a released claim of that token is the
// one that acts.
func (p *PodRecords) ReleasedClaimByToken(ctx context.Context, tx pgx.Tx, token string) (supervise.Claim, bool, error) {
	return oneReleasedClaim(ctx, tx, `and token = $4`, p.project, token, "role token")
}

// ReleasedClaimBySession is the released claim that kept session, and whether this daemon's
// project holds one. A released claim keeps its session id, which is what lets a message
// addressed to that session find the pod that must come back for it.
func (p *PodRecords) ReleasedClaimBySession(ctx context.Context, tx pgx.Tx, session string) (supervise.Claim, bool, error) {
	return oneReleasedClaim(ctx, tx, `and session = $4`, p.project, session, "session")
}

// TakePodWake takes one wake of issue's role inside the caller's transaction, and is the whole
// of what taking a wake is: the mark under the wake's own key, the resume recorded against the
// release that wake ends, and the woken claim's own backstop cancelled. It answers whether this call
// took the wake; false is a wake another pass already took, and then the caller resumes nothing,
// because the first wake is already bringing the pod back.
//
// A claim is woken once while its pod is coming back, whatever else reaches it: at most one
// resume of a claim has no first turn stamped on it (pod_resumes_one_open_per_claim), so the
// insert that records this wake is also what takes it. A CI settlement wakes the pod and, in the
// same transaction, sends that worker a role message, whose listener report then arrives while
// the claim is still released; both are the same pod coming back for the same claim, and the
// second writes nothing rather than leaving two resumes and two start rows — one describing the
// daemon's own settlement to the worker as a message somebody published — against R6's one task
// per wake. Another claim of the same pod is not held by it, of another role or of the same one:
// a wake resumes one claim, and a claim whose wake was dropped would have no process in a pod that
// is running.
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
func (p *PodRecords) TakePodWake(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, token claim.Token, kind string, at time.Time, pause int64) (bool, error) {
	var release int64
	if pause == 0 {
		var err error
		if release, err = LatestPodRelease(ctx, tx, p.project, issue); err != nil {
			return false, err
		}
	}
	_, taken, err := RecordPodWakeTx(ctx, tx, PodResume{Release: release, Project: p.project, Issue: issue,
		Role: string(role), Claim: string(token), Kind: kind, At: at, Pause: pause})
	if err != nil || !taken {
		return false, err
	}
	return true, CancelPodBackstops(ctx, tx, p.project, issue, token)
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

// ClosePodGoneResume closes the open resume of issue's claim token, which holds role now, when the
// pod that resume was waiting for has gone away: a release of the issue, of whatever kind,
// recorded since the wake was taken (newestPodRelease, seen_release). Without it that row would
// refuse every later wake of the claim for as long as the issue lives, because a release that
// suspends no role closes no resume and a pod fully released is never released again. It closes
// such a resume of role too, of any claim, since that pod is gone for every claim of it, and the
// claim's own whatever role it held at its wake: a claim re-roled since names its old role there.
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
func (p *PodRecords) ClosePodGoneResume(ctx context.Context, tx pgx.Tx, issue string, token claim.Token, role claim.Role) (int64, error) {
	newest, err := newestPodRelease(ctx, tx, p.project, issue)
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `update pod_resumes set closed_at = now()
		where project = $1 and issue = $2 and (claim = $3 or role = $4) and first_turn_at is null and closed_at is null
		and seen_release < $5`, p.project, issue, string(token), string(role), newest)
	if err != nil {
		return 0, fmt.Errorf("close the stale resume of %s's claim %s: %w", issue, token, err)
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

// OpenPodBackstop is whether issue's claim token has a backstop armed and not yet fired
// (pod_wake_backstops, `not fired`). The release that suspended the claim with its phase open armed
// it, and it wakes the claim when it comes due unless a wake takes it first (TakePodWake cancels
// it) or a path that takes the pod over drops it (CancelPodBackstops). Another claim of the same
// role is not read: its backstop wakes it, not this one.
func (p *PodRecords) OpenPodBackstop(ctx context.Context, tx pgx.Tx, issue string, token claim.Token) (bool, error) {
	var open bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from pod_wake_backstops
		where project = $1 and issue = $2 and claim = $3 and not fired)`,
		p.project, issue, string(token)).Scan(&open); err != nil {
		return false, fmt.Errorf("read the open backstop of %s's claim %s: %w", issue, token, err)
	}
	return open, nil
}
