package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The cancel of a release that did not finish (CancelIssueRelease, release_cancel.go): what it
// resumes, what it deletes in its one transaction, and what it leaves. The release's mark, its
// rows' decisions and its record are in issue_release_test.go, and the wakes taken under the mark
// in wake_start_test.go.

// A cancel decided at a role stop is not carried out by that decision: the mark stands until
// the resumes of the claims the release suspended are queued, and the two happen in one
// transaction. A mark deleted at the decision left the row's retry reading a release that is
// over — nothing to resume, nothing to undo — with the roles the release had already suspended
// `released` in a pod that still stands, which is what a resume that fails, or a daemon that
// stops between the two, produces.
func TestACancelKeepsTheMarkUntilItsResumesAreQueued(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)
	releasedClaim(t, st, claim.RoleArchitect)
	// A notice the pod must answer, queued above the release's own rows: the cancel at the
	// boundary that comes of it is the one every other cancel is shaped like.
	if _, err := st.Pool().Exec(ctx, `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error)
		values ($1,'notice',$2,'{"kind":"todo","reason":"r"}',0,now(),'')`, last+1, rootIssue); err != nil {
		t.Fatal(err)
	}

	decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
	if err != nil || decision.Act != ReleaseCancel {
		t.Fatalf("the boundary = %s, %v; want it cancelled", decision.Act, err)
	}
	if !releaseMarked(t, st) {
		t.Fatal("the decision deleted the mark; the cancel it decided had not been carried out")
	}
	// The row is retried — its resume failed, or the daemon stopped — and reads the same cancel.
	retried, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
	if err != nil || retried.Act != ReleaseCancel {
		t.Fatalf("the retried stop = %s, %v; want the cancel read again", retried.Act, err)
	}

	refused := errors.New("the resume could not be encoded")
	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, func(claim.Role, IssueRun, DroppedWake) (IdleReleaseRow, error) {
		return IdleReleaseRow{}, refused
	}, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()}); !errors.Is(err, refused) {
		t.Fatalf("a cancel whose resume could not be built = %v, want %v", err, refused)
	}
	if !releaseMarked(t, st) {
		t.Fatal("a cancel that queued no resume still deleted the mark")
	}
	if queued := startRows(t, st); len(queued) != 0 {
		t.Fatalf("starts queued by the refused cancel = %v, want none", queued)
	}

	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if err != nil {
		t.Fatalf("carry the cancel out: %v", err)
	}
	if resumed := cancelled.Resumed; len(resumed) != 1 || resumed[0] != claim.RoleArchitect {
		t.Fatalf("resumed = %v, want the one claim the release suspended", resumed)
	}
	if queued := startRows(t, st); len(queued) != 1 || queued[0] != string(claim.RoleArchitect) {
		t.Fatalf("starts queued by the cancel = %v, want the architect's", queued)
	}
	if releaseMarked(t, st) {
		t.Fatal("the cancel queued its resumes and left the mark standing")
	}
}

// A cancel resumes the claims its own release suspended and no others. A release that finishes
// leaves its claims `released` until a wake brings them back, so an issue being released again
// can hold claims an earlier release left: resuming those puts a role back into a pod that has
// no work for it, with no wake, no task and no record of why it is there.
func TestACancelResumesNoClaimAnEarlierReleaseSuspended(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	// The implementer was released by an earlier release and never woken; this release was
	// marked over the architect alone, which is the one role its stop suspended.
	releasedClaim(t, st, claim.RoleImplementer)
	releasedClaim(t, st, claim.RoleArchitect)
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)

	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if err != nil {
		t.Fatalf("cancel the release: %v", err)
	}
	if resumed := cancelled.Resumed; len(resumed) != 1 || resumed[0] != claim.RoleArchitect {
		t.Fatalf("resumed = %v, want the architect alone: the implementer belongs to the release before this one", resumed)
	}
	if queued := startRows(t, st); len(queued) != 1 || queued[0] != string(claim.RoleArchitect) {
		t.Fatalf("starts queued by the cancel = %v, want the architect's alone", queued)
	}
}

// A release cancelled because its issue is no longer the workflow's queues no resume at all, and
// clears its mark all the same: those claims are the close's to end, and the close is already
// running. A resume queued for them lands above the close's own rows and supersedes them — the
// close's suspension then finishes without acting, its Sandbox is never deleted, and the roles
// of an issue that has left run on in it with their launcher tokens and `gh` credentials.
//
// The sequence is the one production runs: the release's stops have suspended the pod's claims,
// the root is parked while the check runs so the linger's rows are queued above the release's,
// and the release's teardown is the next row of the lane.
func TestACancelOfAnIssueOutOfTheWorkflowQueuesNoResumeSoItsCloseActs(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	releasedClaim(t, st, claim.RoleImplementer)
	first, boundary, last := releaseThroughTheStore(t, st, rootIssue)
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleImplementer, boundary); err != nil {
		t.Fatalf("record the implementer's suspension: %v", err)
	}

	// The root is parked: its tree lingers, its phase is done, and its close queues its own rows
	// above the release's (workflow's beginLinger).
	if _, err := st.Pool().Exec(ctx, `update issues set phase = 'done', status = 'backlog',
		linger_until = now() + interval '1 hour' where key = $1`, rootIssue); err != nil {
		t.Fatalf("park the root: %v", err)
	}
	var closeRow int64
	if err := st.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ('issue_suspend', $1, '{"tree":"LEGION-208","generation":1,"treeGeneration":1}', 0, now(), '', '')
		returning id`, rootIssue).Scan(&closeRow); err != nil {
		t.Fatalf("queue the close's own row: %v", err)
	}

	release := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: last}
	teardown, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
	if err != nil || teardown.Act != ReleaseCancel || teardown.Cause != ReleaseCauseUnauthorized {
		t.Fatalf("the teardown = %s for %s, %v; want %s for %s", teardown.Act, teardown.Cause, err,
			ReleaseCancel, ReleaseCauseUnauthorized)
	}
	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if err != nil {
		t.Fatalf("carry the cancel out: %v", err)
	}
	if len(cancelled.Resumed) != 0 {
		t.Fatalf("resumed = %v, want none: the claims of an issue that has left are its close's", cancelled.Resumed)
	}
	if queued := startRows(t, st); len(queued) != 0 {
		t.Fatalf("starts queued by the cancel = %v, want none", queued)
	}
	if releaseMarked(t, st) {
		t.Fatal("the cancel queued no resume and left its mark behind; every later decision would read a release that is over")
	}

	// The lane deleted the release's own rows as it ran them, so the only rows left below the
	// close's are its own.
	if _, err := st.Pool().Exec(ctx, `delete from outbox where issue = $1 and id between $2 and $3`,
		rootIssue, first, last); err != nil {
		t.Fatalf("finish the release's rows: %v", err)
	}
	act, err := st.IssueSuspension(ctx, "legion",
		IssueClose{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: closeRow},
		record.OutOfWorkflow)
	if err != nil || !act {
		t.Fatalf("the close = %t, %v; want it to act: its Sandbox deletion is what takes the pod's role Secrets with it", act, err)
	}
}

// A release can still be cancelled after such a wake, for another cause, such as a role stop that
// met a start of another role; this test calls the cancel as the outbox does then. The pod never
// goes away, so the wake brings nothing back. The cancel deletes the wake's resume and the start
// the wake queued, and queues the role's own resume, which carries no task: the role starts once,
// in the pod it was in, told nothing about a release that did not happen, and no resume stands
// for that release. A wake taken before the mark is no wake of this release, and it stays.
func TestACancelAfterAWakeOfARoleTheReleaseRecordedStartsTheRoleOnceWithNoWake(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	rootClaim(t, st, claim.RoleImplementer, supervise.StateIdle, "")
	rootClaim(t, st, claim.RoleArchitect, supervise.StateIdle, "")
	at := time.Now().UTC()
	recordRelease(t, st, at.Add(-2*time.Hour))
	if taken, err := st.TakePodWake(ctx, "legion", rootIssue, releasedToken,
		pods.WakeDelivery, at.Add(-time.Hour), 0); err != nil || !taken {
		t.Fatalf("take the wake before the mark = %t, %v; want it taken", taken, err)
	}

	boundary, _, _ := releaseTwoRolesThroughTheStore(t, st)
	stoppedByTheRelease(t, st, claim.RoleImplementer, boundary)
	if taken, err := st.TakePodWake(ctx, "legion", rootIssue, releasedToken,
		pods.WakeAgentMessage, at, 0); err != nil || !taken {
		t.Fatalf("take the wake after the record = %t, %v; want it taken", taken, err)
	}
	queueWakeStart(t, st, claim.RoleImplementer, pods.WakeAgentMessage)

	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if resumed := cancelled.Resumed; err != nil || len(resumed) != 1 || resumed[0] != claim.RoleImplementer {
		t.Fatalf("CancelIssueRelease = %v, %v; want the implementer's resume queued", resumed, err)
	}
	rows, err := st.Pool().Query(ctx, `select payload->>'role', coalesce(payload->>'task', '') from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, rootIssue)
	if err != nil {
		t.Fatalf("read the queued starts: %v", err)
	}
	var starts []string
	for rows.Next() {
		var role, task string
		if err := rows.Scan(&role, &task); err != nil {
			t.Fatalf("read a queued start: %v", err)
		}
		starts = append(starts, role+":"+task)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the queued starts: %v", err)
	}
	if len(starts) != 1 || starts[0] != string(claim.RoleImplementer)+":" {
		t.Fatalf("queued starts = %q, want the cancel's one start of the implementer, with no task: the wake's start goes with the wake", starts)
	}
	var kinds []string
	resumes, err := st.Pool().Query(ctx, `select kind from pod_resumes where issue = $1 order by id`, rootIssue)
	if err != nil {
		t.Fatalf("read the resumes: %v", err)
	}
	for resumes.Next() {
		var kind string
		if err := resumes.Scan(&kind); err != nil {
			t.Fatalf("read a resume: %v", err)
		}
		kinds = append(kinds, kind)
	}
	resumes.Close()
	if err := resumes.Err(); err != nil {
		t.Fatalf("read the resumes: %v", err)
	}
	if len(kinds) != 1 || kinds[0] != pods.WakeDelivery {
		t.Fatalf("resumes = %q, want the wake before the mark alone: the pod never went away, so the wake after the record brought nothing back", kinds)
	}
	if releaseMarked(t, st) {
		t.Fatal("the cancel left its mark behind")
	}
}

// R3 on the release's turn fence: a turn that starts after the release's mark is one of the ends
// of a release, and it ends it as a cancel. The implementer's stop has recorded its claim
// `released`, and a wake of the implementer is taken under the mark, which does not cancel the
// release. The architect, which the release has not stopped yet, then starts a turn: its stop
// reads the pod given work since the mark (releaseOvertaken, issue_releases.turn_start) and
// decides the cancel. The cancel does for the woken implementer what every cancel does: it deletes
// the wake's start and resume and queues the implementer's one resume. The control row has no
// turn, and the architect's stop acts.
func TestATurnAfterTheMarkCancelsTheReleaseAndStartsTheWokenRoleOnce(t *testing.T) {
	t.Parallel()
	for _, turned := range []bool{false, true} {
		name := "no turn after the mark"
		if turned {
			name = "the architect starts a turn after the mark"
		}
		t.Run(name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			rootClaim(t, st, claim.RoleImplementer, supervise.StateIdle, "")
			rootClaim(t, st, claim.RoleArchitect, supervise.StateIdle, "")
			boundary, stop, _ := releaseTwoRolesThroughTheStore(t, st)
			stoppedByTheRelease(t, st, claim.RoleImplementer, boundary)
			if taken, err := st.TakePodWake(ctx, "legion", rootIssue, releasedToken,
				pods.WakeAgentMessage, time.Now().UTC(), 0); err != nil || !taken {
				t.Fatalf("take the wake after the record = %t, %v; want it taken", taken, err)
			}
			queueWakeStart(t, st, claim.RoleImplementer, pods.WakeAgentMessage)
			if turned {
				if _, err := st.Pool().Exec(ctx, `update claims set last_turn_start = now() where issue = $1 and role = $2`,
					rootIssue, string(claim.RoleArchitect)); err != nil {
					t.Fatalf("start the architect's turn: %v", err)
				}
			}

			decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, stop)
			if err != nil {
				t.Fatalf("the architect's stop: %v", err)
			}
			if !turned {
				if decision.Act != ReleaseActs {
					t.Fatalf("the architect's stop with no turn = %s for %s, want it to act: the wake of the recorded implementer cancels nothing", decision.Act, decision.Cause)
				}
				return
			}
			if decision.Act != ReleaseCancel || decision.Cause != ReleaseCauseOvertaken {
				t.Fatalf("the architect's stop after its turn = %s for %s, want %s for %s", decision.Act, decision.Cause, ReleaseCancel, ReleaseCauseOvertaken)
			}
			cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
			if resumed := cancelled.Resumed; err != nil || len(resumed) != 1 || resumed[0] != claim.RoleImplementer {
				t.Fatalf("CancelIssueRelease = %v, %v; want the implementer's resume queued", resumed, err)
			}
			var starts, resumes int
			if err := st.Pool().QueryRow(ctx, `select
				(select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'),
				(select count(*) from pod_resumes where issue = $1)`, rootIssue).Scan(&starts, &resumes); err != nil {
				t.Fatalf("read the starts and resumes: %v", err)
			}
			if starts != 1 || resumes != 0 {
				t.Fatalf("starts = %d and resumes = %d, want the cancel's one start of the implementer and no resume: the pod never went away", starts, resumes)
			}
			if releaseMarked(t, st) {
				t.Fatal("the cancel left its mark behind")
			}
		})
	}
}

// R3 of dispatch://INFRA-3589, for the resume. The release's stop suspends the implementer as
// `released`, and its record of that on the mark fails, so the mark does not record the role
// (unrecordedStops). A wake of the implementer is taken while the project runs and the release
// stands, after an earlier release of the pod, so its resume names that earlier release. No cancel
// resumes the implementer while that wake's start is queued, so
// the start is its one start, and the cancel keeps the resume. This release took no pod, so the
// cancel makes the resume name release 0, whether the project runs or is paused. No pause holds
// it, and its kind and its seen release stay. The control is a wake of the architect, whose stop
// the mark records, taken in the same way: a cancel made while the project is paused keeps it as a
// held wake of the pause (holdWakesUnderPause).
func TestACancelPairsTheKeptWakeOfARoleItsMarkDoesNotRecordWithNoRelease(t *testing.T) {
	t.Parallel()
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "the project runs", true: "the project is paused"}[paused], func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			rootClaim(t, st, claim.RoleImplementer, supervise.StateIdle, "")
			rootClaim(t, st, claim.RoleArchitect, supervise.StateIdle, "")
			at := time.Now().UTC()
			earlier, _, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
				Roles: []string{string(claim.RoleImplementer)}, At: at.Add(-2 * time.Hour)})
			if err != nil {
				t.Fatalf("record the earlier release: %v", err)
			}
			_, stop, _ := releaseTwoRolesThroughTheStore(t, st)
			stoppedByTheRelease(t, st, claim.RoleArchitect, stop)
			// The implementer's stop suspended its claim and did not record the role on the mark,
			// so its row stays queued.
			releasedClaim(t, st, claim.RoleImplementer)
			if taken, err := st.TakePodWake(ctx, "legion", rootIssue, releasedToken, pods.WakeRoleMessage, at, 0); err != nil || !taken {
				t.Fatalf("take the implementer's wake = %t, %v; want it taken", taken, err)
			}
			queueWakeStart(t, st, claim.RoleImplementer, pods.WakeRoleMessage)
			resume := func() (release, pause, seen int64, kind string) {
				t.Helper()
				if err := st.Pool().QueryRow(ctx, `select release_id, coalesce(pause_id, 0), seen_release, kind from pod_resumes
					where project = 'legion' and issue = $1 and role = 'implementer'`, rootIssue).Scan(&release, &pause, &seen, &kind); err != nil {
					t.Fatalf("read the implementer's resume: %v", err)
				}
				return release, pause, seen, kind
			}
			if release, held, _, _ := resume(); release != earlier || held != 0 {
				t.Fatalf("the implementer's wake = release %d, pause %d; want release %d and no pause: the case's premise",
					release, held, earlier)
			}
			var pause Pause
			var architectsPause func() int64
			resumed := []claim.Role{claim.RoleArchitect}
			if paused {
				architectsPause = architectsWake(t, st, at)
				pause, _ = pauseLegion(t, st, at)
				resumed = nil
			}

			cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow,
				ReleaseCancelPause{At: at.Add(time.Minute)})
			if err != nil || cancelled.Pause != pause.ID || !slices.Equal(cancelled.Resumed, resumed) {
				t.Fatalf("CancelIssueRelease = %+v, %v; want pause %d and %v resumed", cancelled, err, pause.ID, resumed)
			}
			if release, held, seen, kind := resume(); release != 0 || held != 0 || seen != earlier || kind != pods.WakeRoleMessage {
				t.Errorf("the implementer's resume after the cancel = release %d, pause %d, seen release %d, kind %s; want release 0, no pause, seen release %d and kind %s: the release took no pod, and the wake's start still resumes the claim",
					release, held, seen, kind, earlier, pods.WakeRoleMessage)
			}
			if paused {
				if held := architectsPause(); held != pause.ID {
					t.Errorf("control: the architect's resume after the cancel is held by pause %d, want pause %d: the mark records the architect, so the paused cancel keeps its wake as a held wake of the pause",
						held, pause.ID)
				}
			}
		})
	}
}

// architectsWake takes a wake of the root's architect at at, and answers a read of the pause
// that holds the resume of that wake, 0 for none (pod_resumes.pause_id).
func architectsWake(t *testing.T, st *Store, at time.Time) func() int64 {
	t.Helper()
	ctx := context.Background()
	if taken, err := st.TakePodWake(ctx, "legion", rootIssue, "legion-legion-legion-208-architect", pods.WakeRoleMessage, at, 0); err != nil || !taken {
		t.Fatalf("take the architect's wake = %t, %v; want it taken", taken, err)
	}
	return func() int64 {
		t.Helper()
		var pause int64
		if err := st.Pool().QueryRow(ctx, `select coalesce(pause_id, 0) from pod_resumes
			where project = 'legion' and issue = $1 and role = 'architect'`, rootIssue).Scan(&pause); err != nil {
			t.Fatalf("read the architect's resume: %v", err)
		}
		return pause
	}
}

// The reviewer's read-ahead of dispatch://INFRA-3589 at 9e3e6d92, minor 2. The release's stop
// suspends the implementer as `released`, and its record of that on the mark fails, so the mark
// does not record the role (unrecordedStops). A wake of the implementer takes a resume and queues
// its start. Then an operator suspends the claim. That start now waits for the operator's resume,
// so the role takes no first turn from it. A kept resume would be warned and recorded stuck, and it
// would refuse the role's next wake. So the cancel deletes that resume, by the rule it keeps for a
// role of the mark's set that another path took over (dropTakenOverWakes), whether the project
// runs or is paused. The wake's start stays, for the operator's resume to run, and the role's next
// wake is taken. The control is the same trace with the claim still `released`: the cancel keeps
// the resume and pairs it with release 0 (pairUnrecordedWakesWithNoRelease), and that resume
// refuses the next wake, since the start brings the role back. No pause holds that kept resume.
// The control of that is a wake of the architect, whose stop the mark records, in the paused
// rows: the paused cancel keeps it as a held wake of the pause (holdWakesUnderPause).
func TestACancelDeletesTheWakeOfARoleItsMarkDoesNotRecordThatAnotherPathTookOver(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		takenOver, paused bool
	}{
		{name: "an operator took the claim over while the project runs", takenOver: true},
		{name: "an operator took the claim over while the project is paused", takenOver: true, paused: true},
		{name: "the claim is still released while the project runs"},
		{name: "the claim is still released while the project is paused", paused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			rootClaim(t, st, claim.RoleImplementer, supervise.StateIdle, "")
			rootClaim(t, st, claim.RoleArchitect, supervise.StateIdle, "")
			at := time.Now().UTC()
			_, stop, _ := releaseTwoRolesThroughTheStore(t, st)
			stoppedByTheRelease(t, st, claim.RoleArchitect, stop)
			// The implementer's stop suspended its claim and did not record the role on the mark,
			// so its row stays queued.
			releasedClaim(t, st, claim.RoleImplementer)
			if taken, err := st.TakePodWake(ctx, "legion", rootIssue, releasedToken, pods.WakeChecks, at, 0); err != nil || !taken {
				t.Fatalf("take the implementer's wake = %t, %v; want it taken", taken, err)
			}
			queueWakeStart(t, st, claim.RoleImplementer, pods.WakeChecks)
			if tc.takenOver {
				rootClaim(t, st, claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByOperator)
			}
			var pause Pause
			var architectsPause func() int64
			if tc.paused {
				architectsPause = architectsWake(t, st, at)
				pause, _ = pauseLegion(t, st, at)
			}

			if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow,
				ReleaseCancelPause{At: at.Add(time.Minute)}); err != nil {
				t.Fatalf("CancelIssueRelease: %v", err)
			}
			kept := 1
			if tc.takenOver {
				kept = 0
			}
			resumes := pauseCount(t, st, `select count(*)::int from pod_resumes where issue = $1 and role = 'implementer'`, rootIssue)
			paired := pauseCount(t, st, `select count(*)::int from pod_resumes where issue = $1 and role = 'implementer'
				and release_id = 0 and pause_id is null`, rootIssue)
			if resumes != kept || paired != kept {
				t.Errorf("the implementer's resumes after the cancel = %d, %d of them paired with release 0; want %d of each", resumes, paired, kept)
			}
			if tc.paused {
				if held := architectsPause(); held != pause.ID {
					t.Errorf("control: the architect's resume after the cancel is held by pause %d, want pause %d: the mark records the architect, so the paused cancel keeps its wake as a held wake of the pause",
						held, pause.ID)
				}
			}
			if starts := startsOfTheRoot(t, st); !slices.Contains(starts, "implementer||the wake's task|"+pods.WakeChecks+"||") {
				t.Errorf("queued starts after the cancel = %q, want the implementer's wake start among them", starts)
			}
			next, err := st.TakePodWake(ctx, "legion", rootIssue, releasedToken, pods.WakeRoleMessage, at.Add(2*time.Minute), 0)
			if err != nil || next != tc.takenOver {
				t.Errorf("the implementer's next wake taken = %t, %v; want %t", next, err, tc.takenOver)
			}
		})
	}
}

// R3 of dispatch://INFRA-3589: a cancel queues a start for the role. The release's stop suspends
// the implementer as `released`, and its record of that on the mark fails (ReleaseSuspendedClaim),
// so the mark does not record the role and the stop's row stays queued (unrecordedStops). The
// architect's stop has not run when the cancel comes. No wake of the implementer was taken, so no
// start brings it back, and its stop's row, run again, reads the release over: the cancel queues
// the implementer's one resume and answers it resumed, as it does for a role its mark records.
// While the project is paused, it records the implementer stopped by the pause in place of that
// resume, and queues no start. The control is the same trace with a wake of the implementer whose
// start is queued: that start is the role's one start, so the cancel queues no second start and
// answers no resume.
//
// The negative control is the cancel without its resume of such a role (the loop over
// unrecordedReleased in cancelIssueRelease): the two rows with no wake fail, with no start of the
// implementer queued and the implementer neither resumed nor stopped.
func TestACancelResumesARoleItsMarkDoesNotRecordThatNoStartBringsBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		woken, paused bool
	}{
		{name: "no wake while the project runs"},
		{name: "no wake while the project is paused", paused: true},
		{name: "the control: a wake whose start is queued", woken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			rootClaim(t, st, claim.RoleImplementer, supervise.StateIdle, "")
			rootClaim(t, st, claim.RoleArchitect, supervise.StateIdle, "")
			at := time.Now().UTC()
			releaseTwoRolesThroughTheStore(t, st)
			// The implementer's stop suspended its claim and did not record the role on the mark,
			// so its row stays queued.
			releasedClaim(t, st, claim.RoleImplementer)
			if tc.woken {
				if taken, err := st.TakePodWake(ctx, "legion", rootIssue, releasedToken, pods.WakeChecks, at, 0); err != nil || !taken {
					t.Fatalf("take the implementer's wake = %t, %v; want it taken", taken, err)
				}
				queueWakeStart(t, st, claim.RoleImplementer, pods.WakeChecks)
			}
			var pause Pause
			if tc.paused {
				pause, _ = pauseLegion(t, st, at)
			}

			cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow,
				ReleaseCancelPause{At: at.Add(time.Minute)})
			if err != nil {
				t.Fatalf("CancelIssueRelease: %v", err)
			}
			implementer := []claim.Role{claim.RoleImplementer}
			// The cancel's resume is resumeRow's start, with no task; the wake's start is
			// queueWakeStart's.
			resumed, stopped, starts := implementer, []claim.Role(nil), []string{"implementer|||||"}
			switch {
			case tc.paused:
				resumed, stopped, starts = nil, implementer, nil
			case tc.woken:
				resumed, starts = nil, []string{"implementer||the wake's task|" + pods.WakeChecks + "||"}
			}
			if !slices.Equal(cancelled.Resumed, resumed) || !slices.Equal(cancelled.Stopped, stopped) || cancelled.Pause != pause.ID {
				t.Errorf("CancelIssueRelease = %+v; want %v resumed, %v stopped and pause %d", cancelled, resumed, stopped, pause.ID)
			}
			if got := startsOfTheRoot(t, st); !slices.Equal(got, starts) {
				t.Errorf("queued starts after the cancel = %q, want %q: the implementer starts once", got, starts)
			}
			if tc.paused {
				if n := pauseCount(t, st, `select count(*)::int from project_pause_stops where pause_id = $1 and claim = $2
					and ended_as = 'stopped'`, pause.ID, string(releasedToken)); n != 1 {
					t.Errorf("stops of the implementer by pause %d that ended stopped = %d, want 1", pause.ID, n)
				}
			}
			if releaseMarked(t, st) {
				t.Error("the cancel left its mark behind")
			}
		})
	}
}
