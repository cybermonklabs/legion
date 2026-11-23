package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// pausedAt is the moment the pause tests begin their pause.
var pausedAt = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

// seedPauseClaims is project legion as a pause finds it: an idle implementer of LEGION-208 and a
// working tester of LEGION-209, which the pause stops; the architect of LEGION-208 an idle release
// left released, a failed reviewer and a retired merger, which run nothing to stop; the project's
// controller, which the pause never stops; and an idle implementer of another project on the same
// database, which is no claim of this one.
func seedPauseClaims(t *testing.T, st *Store) {
	t.Helper()
	for _, c := range []supervise.Claim{
		{Token: "legion-legion-legion-208-implementer", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
			Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle},
		{Token: "legion-legion-legion-209-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209",
			Role: claim.RoleTester, Generation: 2, State: supervise.StateWorking},
		{Token: "legion-legion-legion-208-architect", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
			Role: claim.RoleArchitect, Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased},
		{Token: "legion-legion-legion-210-reviewer", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-210",
			Role: claim.RoleReviewer, Generation: 1, State: supervise.StateFailed},
		{Token: "legion-legion-legion-210-merger", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-210",
			Role: claim.RoleMerger, Generation: 1, State: supervise.StateRetired},
		{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController, State: supervise.StateIdle},
		{Token: "legion-infra-infra-5-implementer", Project: "infra", Tree: "INFRA-5", Issue: "INFRA-5",
			Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle},
	} {
		if err := st.PutClaim(context.Background(), c); err != nil {
			t.Fatalf("seed claim %s: %v", c.Token, err)
		}
	}
}

// pauseStopRow is the pause service's stop row, encoded and keyed as the daemon does (pause.StopRow
// and pause.StopKey, which this package's tests cannot import): by the claim's issue, or by the
// pause's project, naming the claim's issue, for a claim of another project's issue.
func pauseStopRow(pause Pause, stop PauseStopClaim) (PauseRow, error) {
	request := record.SuperviseRequest{Op: "pause_stop", Tree: stop.Tree, Role: stop.Role,
		Generation: stop.Generation, Pause: pause.ID}
	key := stop.Issue
	if project, _, _ := strings.Cut(stop.Issue, "-"); project != pause.Project {
		key, request.Issue = pause.Project, stop.Issue
	}
	row, err := record.NewOutboxRow(key, request, pause.Since)
	if err != nil {
		return PauseRow{}, err
	}
	return PauseRow{Issue: row.Issue, Kind: string(row.Kind), Payload: row.Payload}, nil
}

// pauseNoticeRow is the controller's notice of a pause or a resume, encoded as the daemon encodes it.
func pauseNoticeRow(kind record.NoticeKind) func(Pause) (PauseRow, error) {
	return func(p Pause) (PauseRow, error) {
		account := &record.ProjectPause{ID: p.ID, By: record.PauseActor{Kind: string(p.By.Kind), Session: p.By.Session}, At: p.Since, Reason: p.Reason}
		if kind == record.ProjectResumedNotice {
			account = &record.ProjectPause{ID: p.ID, By: record.PauseActor{Kind: string(p.EndedBy.Kind), Session: p.EndedBy.Session}, At: p.EndedAt}
		}
		row, err := record.NewOutboxRow(p.Project, record.ControllerNotice{Kind: kind, Pause: account}, p.Since)
		if err != nil {
			return PauseRow{}, err
		}
		return PauseRow{Issue: row.Issue, Kind: string(row.Kind), Payload: row.Payload}, nil
	}
}

// pauseLegion pauses project LEGION as the operator, through the store's own transaction.
func pauseLegion(t *testing.T, st *Store, at time.Time) (Pause, []PauseStopClaim) {
	t.Helper()
	return pauseProject(t, st, "LEGION", at)
}

// pauseProject pauses project, a Dispatch key, as the operator, through the store's own
// transaction.
func pauseProject(t *testing.T, st *Store, project string, at time.Time) (Pause, []PauseStopClaim) {
	t.Helper()
	pause, stops, err := st.Pause(context.Background(), PauseRequest{Project: project, By: PauseActor{Kind: PauseByOperator},
		Reason: "the model provider is down", At: at, StopRow: pauseStopRow, Notice: pauseNoticeRow(record.ProjectPausedNotice)})
	if err != nil {
		t.Fatalf("pause %s: %v", project, err)
	}
	return pause, stops
}

// pauseCount is the one number sql counts.
func pauseCount(t *testing.T, st *Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// openPauseStops is pause's open stops, read in a transaction of its own (OpenPauseStopsTx).
func openPauseStops(t *testing.T, st *Store, pause int64) []PauseStop {
	t.Helper()
	var open []PauseStop
	if err := st.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		open, err = OpenPauseStopsTx(context.Background(), tx, pause)
		return err
	}); err != nil {
		t.Fatalf("read the open stops of pause %d: %v", pause, err)
	}
	return open
}

// holdPod opens pause's hold of issue's pod at at in a transaction of its own, under the issue's
// lock, as a stop transaction opens one (LockIssueForPause, OpenPausePodHoldTx), and answers
// whether it opened one.
func holdPod(st *Store, pause Pause, issue string, at time.Time) (bool, error) {
	ctx := context.Background()
	var opened bool
	err := st.Tx(ctx, func(tx pgx.Tx) error {
		if err := LockIssueForPause(ctx, tx, issue); err != nil {
			return err
		}
		var err error
		opened, err = OpenPausePodHoldTx(ctx, tx, pause, issue, at)
		return err
	})
	return opened, err
}

// The pause's transaction writes one stop row and one pause_stop outbox row for each live claim
// of the project, and for none of the rest: not a claim already released, failed or retired, not
// the controller's, and not another project's on the same database. The operator's pause queues
// the controller's notice under the project's key. A second pause is refused with the pause that
// stands and writes nothing.
func TestAPauseStopsEveryLiveRoleButTheControllerAndRefusesASecond(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedPauseClaims(t, st)

	pause, stops := pauseLegion(t, st, pausedAt)
	if pause.ID == 0 || pause.Project != "LEGION" || !pause.Since.Equal(pausedAt) || pause.By.Kind != PauseByOperator || pause.Ended() {
		t.Fatalf("pause = %+v, want an open pause of LEGION by the operator at %s", pause, pausedAt)
	}
	want := []claim.Token{"legion-legion-legion-208-implementer", "legion-legion-legion-209-tester"}
	if len(stops) != len(want) {
		t.Fatalf("the pause asked to stop %+v, want %v", stops, want)
	}
	for i, stop := range stops {
		if stop.Claim != want[i] {
			t.Fatalf("the pause asked to stop %+v, want %v in token order", stops, want)
		}
	}
	open := openPauseStops(t, st, pause.ID)
	if len(open) != 2 || open[0].Claim != want[0] || open[1].Claim != want[1] || !open[0].AskedAt.Equal(pausedAt) {
		t.Fatalf("open stops = %+v, want the two live claims asked at %s", open, pausedAt)
	}

	rows, err := st.Pool().Query(ctx, `select issue, payload from outbox where kind = 'supervise' order by id`)
	if err != nil {
		t.Fatalf("read the stop rows: %v", err)
	}
	var stopRows []record.OutboxRow
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.Issue, &row.Payload); err != nil {
			t.Fatal(err)
		}
		row.Kind = record.OutboxKindSupervise
		stopRows = append(stopRows, row)
	}
	rows.Close()
	if len(stopRows) != 2 {
		t.Fatalf("supervise rows = %d, want one pause_stop row per live claim", len(stopRows))
	}
	for i, row := range stopRows {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode the stop row of %s: %v", row.Issue, err)
		}
		request := payload.(record.SuperviseRequest)
		if request.Op != "pause_stop" || request.Pause != pause.ID || row.Issue != stops[i].Issue || request.Role != stops[i].Role {
			t.Fatalf("stop row %d = %s %+v, want the pause_stop of %s's %s for pause %d", i, row.Issue, request, stops[i].Issue, stops[i].Role, pause.ID)
		}
	}
	if got := pauseCount(t, st, `select count(*) from outbox where kind = 'controller_notice' and issue = 'LEGION'
		and payload->>'kind' = 'project-paused' and (payload->'pause'->>'id')::bigint = $1`, pause.ID); got != 1 {
		t.Fatalf("project-paused notices = %d, want the operator's pause to tell the controller once", got)
	}
	if got := pauseCount(t, st, `select count(*) from outbox where issue like 'INFRA-%'`); got != 0 {
		t.Fatalf("rows of another project = %d, want none: its claims are not this project's", got)
	}

	_, _, err = st.Pause(ctx, PauseRequest{Project: "LEGION", By: PauseActor{Kind: PauseByController, Session: "session-1"},
		Reason: "again", At: pausedAt.Add(time.Minute), StopRow: pauseStopRow})
	var standing *AlreadyPausedError
	if !errors.As(err, &standing) || standing.Standing.ID != pause.ID {
		t.Fatalf("a second pause = %v, want an AlreadyPausedError naming pause %d", err, pause.ID)
	}
	t.Logf("a second pause is refused: %v", err)
	if got := pauseCount(t, st, `select count(*) from project_pauses`); got != 1 {
		t.Fatalf("pauses = %d after the refusal, want the one that stands", got)
	}
	if got := pauseCount(t, st, `select count(*) from project_pause_stops`); got != 2 {
		t.Fatalf("stops = %d after the refusal, want the first pause's two", got)
	}

	read, found, err := st.OpenPause(ctx, "LEGION")
	if err != nil || !found || read.ID != pause.ID || read.Reason != pause.Reason {
		t.Fatalf("OpenPause = %+v, %t, %v; want pause %d", read, found, err, pause.ID)
	}
	if _, found, err := st.LastEndedPause(ctx, "LEGION"); err != nil || found {
		t.Fatalf("LastEndedPause while the pause stands = %t, %v; want none", found, err)
	}
	if _, found, err := st.OpenPause(ctx, "INFRA"); err != nil || found {
		t.Fatalf("OpenPause of another project = %t, %v; want none", found, err)
	}
}

// The resume ends the pause with who ended it, deletes the pause_stop rows no lane is running,
// cancels the stops still open, starts the clock of the project's open resumes at its moment
// (never moving one backwards, and touching no closed or turned resume nor another project's),
// hands the rows the pause held to its reader before the pause ends, and tells the controller. A
// resume of a project that is not paused is refused. A stop that ends after the resume is still
// recorded as stopped.
func TestAResumeEndsThePauseAndWhatItStillWaitedOn(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedPauseClaims(t, st)
	records := record.NewStore()
	operator := PauseActor{Kind: PauseByOperator}

	_, err := st.Resume(ctx, ResumeRequest{Project: "LEGION", By: operator, At: pausedAt})
	var notPaused *NotPausedError
	if !errors.As(err, &notPaused) {
		t.Fatalf("resuming a running project = %v, want a NotPausedError", err)
	}
	t.Logf("a resume of a running project is refused: %v", err)

	pause, stops := pauseLegion(t, st, pausedAt)
	implementer, tester := stops[0], stops[1]
	// The tester's stop row is running when the resume commits; the implementer's has not run.
	if _, err := st.Pool().Exec(ctx, `update outbox set lease_token = 'lane', lease_until = $2
		where kind = 'supervise' and issue = $1`, tester.Issue, pausedAt.Add(time.Hour)); err != nil {
		t.Fatalf("lease the tester's stop row: %v", err)
	}
	// A notice for the architect is held by the pause; a controller notice is not.
	held, err := record.NewOutboxRow("LEGION-208", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-210 is done"}, pausedAt)
	if err != nil {
		t.Fatal(err)
	}
	tick, err := record.NewOutboxRow("LEGION", record.ControllerNotice{Kind: record.TickNotice}, pausedAt)
	if err != nil {
		t.Fatal(err)
	}
	inTx(t, st, func(tx pgx.Tx) error {
		if err := records.Enqueue(ctx, tx, held); err != nil {
			return err
		}
		return records.Enqueue(ctx, tx, tick)
	})

	resumedAt := pausedAt.Add(30 * time.Minute)
	resume := func(issue, role string, woke time.Time) int64 {
		var id int64
		if err := st.Pool().QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at)
			values (0, $1, $2, $3, 'delivery', $4) returning id`, "legion", issue, role, woke).Scan(&id); err != nil {
			t.Fatalf("seed the resume of %s's %s: %v", issue, role, err)
		}
		return id
	}
	waiting := resume("LEGION-211", "implementer", pausedAt.Add(-time.Hour))
	ahead := resume("LEGION-212", "implementer", pausedAt)
	if _, err := st.Pool().Exec(ctx, `update pod_resumes set clock_at = $2 where id = $1`, ahead, resumedAt.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	turned := resume("LEGION-213", "implementer", pausedAt.Add(-time.Hour))
	closed := resume("LEGION-214", "implementer", pausedAt.Add(-time.Hour))
	if _, err := st.Pool().Exec(ctx, `update pod_resumes set first_turn_at = $2 where id = $1`, turned, pausedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `update pod_resumes set closed_at = $2 where id = $1`, closed, pausedAt); err != nil {
		t.Fatal(err)
	}
	var other int64
	if err := st.Pool().QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at)
		values (0, 'infra', 'INFRA-5', 'implementer', 'delivery', $1) returning id`, pausedAt.Add(-time.Hour)).Scan(&other); err != nil {
		t.Fatal(err)
	}

	var heldRows []record.PauseHeldRow
	resumed, err := st.Resume(ctx, ResumeRequest{Project: "LEGION", By: operator, At: resumedAt,
		MarkHeld: func(ctx context.Context, tx pgx.Tx, held Pause) error {
			var err error
			heldRows, err = records.MarkPauseHeldRows(ctx, tx, "LEGION", held.ID, resumedAt)
			return err
		},
		Notice: pauseNoticeRow(record.ProjectResumedNotice)})
	if err != nil {
		t.Fatalf("resume LEGION: %v", err)
	}
	if resumed.Pause.ID != pause.ID || !resumed.Pause.EndedAt.Equal(resumedAt) || resumed.Pause.EndedBy != operator {
		t.Fatalf("resumed pause = %+v, want pause %d ended at %s by the operator", resumed.Pause, pause.ID, resumedAt)
	}
	if resumed.DeletedStopRows != 1 {
		t.Fatalf("deleted stop rows = %d, want the implementer's, which had not run", resumed.DeletedStopRows)
	}
	if got := pauseCount(t, st, `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'pause_stop' and issue = $1`, tester.Issue); got != 1 {
		t.Fatalf("the tester's running stop row: %d left, want it kept to run to its end", got)
	}
	if got := pauseCount(t, st, `select count(*) from outbox where kind = 'supervise' and issue = $1`, implementer.Issue); got != 0 {
		t.Fatalf("the implementer's unrun stop row: %d left, want it deleted", got)
	}
	if len(resumed.Cancelled) != 2 || resumed.Cancelled[0].End != PauseStopCancelled || !resumed.Cancelled[0].EndedAt.Equal(resumedAt) {
		t.Fatalf("cancelled stops = %+v, want both open stops cancelled at %s", resumed.Cancelled, resumedAt)
	}
	if open := openPauseStops(t, st, pause.ID); len(open) != 0 {
		t.Fatalf("open stops after the resume = %+v; want none", open)
	}
	if len(heldRows) != 1 || heldRows[0].ID == 0 || heldRows[0].Kind != record.OutboxKindNotice || heldRows[0].Issue != "LEGION-208" ||
		heldRows[0].WaitedSeconds != int64(resumedAt.Sub(pausedAt).Seconds()) || heldRows[0].HeldByPause != pause.ID {
		t.Fatalf("held rows marked in the resume = %+v, want the architect's notice alone, held 1800s and marked with pause %d", heldRows, pause.ID)
	}
	if resumed.Clocks != 2 {
		t.Fatalf("clocks set = %d, want the project's two open resumes", resumed.Clocks)
	}
	clockOf := func(id int64) time.Time {
		var clock time.Time
		if err := st.Pool().QueryRow(ctx, `select clock_at from pod_resumes where id = $1`, id).Scan(&clock); err != nil {
			t.Fatal(err)
		}
		return clock.UTC()
	}
	for _, tc := range []struct {
		name string
		id   int64
		want time.Time
	}{
		{"the open resume", waiting, resumedAt},
		{"the open resume whose clock stood later", ahead, resumedAt.Add(10 * time.Minute)},
		{"the resume that took its first turn", turned, pausedAt.Add(-time.Hour)},
		{"the closed resume", closed, pausedAt.Add(-time.Hour)},
		{"another project's resume", other, pausedAt.Add(-time.Hour)},
	} {
		if got := clockOf(tc.id); !got.Equal(tc.want) {
			t.Fatalf("the clock of %s = %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := pauseCount(t, st, `select count(*) from outbox where kind = 'controller_notice' and issue = 'LEGION'
		and payload->>'kind' = 'project-resumed' and (payload->'pause'->>'id')::bigint = $1`, pause.ID); got != 1 {
		t.Fatalf("project-resumed notices = %d, want the operator's resume to tell the controller once", got)
	}

	if _, err := st.Resume(ctx, ResumeRequest{Project: "LEGION", By: operator, At: resumedAt}); !errors.As(err, &notPaused) {
		t.Fatalf("a second resume = %v, want a NotPausedError", err)
	}
	if _, found, err := st.OpenPause(ctx, "LEGION"); err != nil || found {
		t.Fatalf("OpenPause after the resume = %t, %v; want none", found, err)
	}
	last, found, err := st.LastEndedPause(ctx, "LEGION")
	if err != nil || !found || last.ID != pause.ID || !last.EndedAt.Equal(resumedAt) {
		t.Fatalf("LastEndedPause = %+v, %t, %v; want pause %d", last, found, err, pause.ID)
	}

	// The tester's stop ends after the resume: its stop row, which the resume cancelled, ends
	// stopped, with the launch it was owed.
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		_, err := RecordPauseStoppedTx(ctx, tx, pause, tester, resumedAt.Add(time.Minute), true)
		return err
	}); err != nil {
		t.Fatalf("record the tester's late stop: %v", err)
	}
	var end string
	var owed bool
	if err := st.Pool().QueryRow(ctx, `select ended_as, launch_owed from project_pause_stops where pause_id = $1 and claim = $2`,
		pause.ID, string(tester.Claim)).Scan(&end, &owed); err != nil {
		t.Fatalf("read the tester's stop: %v", err)
	}
	if end != string(PauseStopStopped) || !owed {
		t.Fatalf("the tester's stop ended %s, owed %t; want stopped with its launch owed", end, owed)
	}
}

// A pause holds one pod at most once: a second open finds the hold that stands and opens none,
// the end takes it away, and once it has ended the pod can be held again. The pod of a claim of
// another project's issue, which the operator's spawn makes under this project's token, is held
// under the pause's own project, and each read and end of a hold finds it by that project, never
// by the issue key's prefix.
func TestAPausePodHoldOpensOnceAndEnds(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	pause, _ := pauseLegion(t, st, pausedAt)

	if opened, err := holdPod(st, pause, "LEGION-208", pausedAt); err != nil || !opened {
		t.Fatalf("hold LEGION-208's pod = %t, %v; want it held", opened, err)
	}
	if opened, err := holdPod(st, pause, "LEGION-208", pausedAt.Add(time.Minute)); err != nil || opened {
		t.Fatalf("hold LEGION-208's pod again = %t, %v; want the hold that stands kept", opened, err)
	}
	hold, found, err := st.PausePodHoldOf(ctx, "LEGION", "LEGION-208")
	if err != nil || !found || hold.Pause != pause.ID || hold.Project != "LEGION" || !hold.HeldAt.Equal(pausedAt) {
		t.Fatalf("PausePodHoldOf = %+v, %t, %v; want pause %d's hold from %s", hold, found, err, pause.ID, pausedAt)
	}
	if holds, err := st.OpenPausePodHolds(ctx, "LEGION"); err != nil || len(holds) != 1 || holds[0].Issue != "LEGION-208" {
		t.Fatalf("OpenPausePodHolds = %+v, %v; want LEGION-208's", holds, err)
	}
	if opened, err := holdPod(st, pause, "INFRA-5", pausedAt); err != nil || !opened {
		t.Fatalf("hold INFRA-5's pod under LEGION's pause = %t, %v; want it held", opened, err)
	}
	if hold, found, err := st.PausePodHoldOf(ctx, "LEGION", "INFRA-5"); err != nil || !found || hold.Pause != pause.ID || hold.Project != "LEGION" {
		t.Fatalf("LEGION's hold of INFRA-5's pod = %+v, %t, %v; want pause %d's", hold, found, err, pause.ID)
	}
	if _, found, err := st.PausePodHoldOf(ctx, "INFRA", "INFRA-5"); err != nil || found {
		t.Fatalf("INFRA's hold of INFRA-5's pod = %t, %v; want none: the hold is LEGION's", found, err)
	}
	if ended, err := st.EndPausePodHold(ctx, "INFRA", "INFRA-5", PausePodHoldDeleted, pausedAt.Add(time.Minute)); err != nil || ended {
		t.Fatalf("INFRA's end of INFRA-5's hold = %t, %v; want nothing ended", ended, err)
	}
	if ended, err := st.EndPausePodHold(ctx, "LEGION", "INFRA-5", PausePodHoldDeleted, pausedAt.Add(time.Minute)); err != nil || !ended {
		t.Fatalf("LEGION's end of INFRA-5's hold = %t, %v; want it ended", ended, err)
	}

	if ended, err := st.EndPausePodHold(ctx, "LEGION", "LEGION-208", PausePodHoldReady, pausedAt.Add(time.Hour)); err != nil || !ended {
		t.Fatalf("end LEGION-208's hold = %t, %v; want it ended", ended, err)
	}
	if ended, err := st.EndPausePodHold(ctx, "LEGION", "LEGION-208", PausePodHoldReady, pausedAt.Add(time.Hour)); err != nil || ended {
		t.Fatalf("end LEGION-208's hold again = %t, %v; want nothing left to end", ended, err)
	}
	if _, found, err := st.PausePodHoldOf(ctx, "LEGION", "LEGION-208"); err != nil || found {
		t.Fatalf("PausePodHoldOf after the end = %t, %v; want none", found, err)
	}
	var why string
	if err := st.Pool().QueryRow(ctx, `select ended_why from project_pause_pod_holds where issue = 'LEGION-208'`).Scan(&why); err != nil || why != string(PausePodHoldReady) {
		t.Fatalf("the ended hold's reason = %q, %v; want %q", why, err, PausePodHoldReady)
	}
	if opened, err := holdPod(st, pause, "LEGION-208", pausedAt.Add(2*time.Hour)); err != nil || !opened {
		t.Fatalf("hold LEGION-208's pod after its hold ended = %t, %v; want it held again", opened, err)
	}
}

// waitingOnThePauseLock is whether a backend waits for issue's pause lock (LockIssueForPause), read
// by its two keys in pg_locks.
func waitingOnThePauseLock(t *testing.T, st *Store, issue string) bool {
	t.Helper()
	var waiting bool
	if err := st.Pool().QueryRow(context.Background(), `select exists (select 1 from pg_locks
		where locktype = 'advisory' and not granted and objsubid = 2
		and database = (select oid from pg_database where datname = current_database())
		and classid = hashtext('legion.pause.issue')::oid and objid = hashtext($1)::oid)`, issue).Scan(&waiting); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return waiting
}

// Two writers of one pod's hold run one after the other: while a transaction that opened the hold
// of LEGION-208 has not committed, a second writer of that hold waits on the issue's lock, and a
// writer of another issue's hold does not. The end of the hold is the case the lock exists for:
// without it the end would read no hold — the first transaction's row is not committed — end
// nothing, and leave the hold standing once the first commits. With it the end waits, reads the
// committed hold, and ends it.
func TestTheIssueLockSerializesTwoWritersOfOnePodsHold(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	pause, _ := pauseLegion(t, st, pausedAt)

	first, err := st.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	if err := LockIssueForPause(ctx, first, "LEGION-208"); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenPausePodHoldTx(ctx, first, pause, "LEGION-208", pausedAt); err != nil || !opened {
		t.Fatalf("the first writer's hold = %t, %v; want it opened", opened, err)
	}

	type result struct {
		ended bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		ended, err := st.EndPausePodHold(ctx, "LEGION", "LEGION-208", PausePodHoldResumed, pausedAt.Add(time.Minute))
		done <- result{ended, err}
	}()
	testwait.Eventually(t, "the second writer to wait on LEGION-208's pause lock", func() bool {
		return waitingOnThePauseLock(t, st, "LEGION-208")
	})
	select {
	case r := <-done:
		t.Fatalf("the second writer finished (%t, %v) while the first held the issue's lock", r.ended, r.err)
	default:
	}
	if opened, err := holdPod(st, pause, "LEGION-209", pausedAt); err != nil || !opened {
		t.Fatalf("hold another issue's pod while LEGION-208's lock is held = %t, %v; want it held at once", opened, err)
	}

	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first writer: %v", err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(testwait.Remaining(t)):
		t.Fatal("the second writer never finished after the first committed")
	}
	if r.err != nil || !r.ended {
		t.Fatalf("the second writer's end = %t, %v; want it to end the hold the first committed", r.ended, r.err)
	}
	if _, found, err := st.PausePodHoldOf(ctx, "LEGION", "LEGION-208"); err != nil || found {
		t.Fatalf("LEGION-208's hold after both writers = %t, %v; want it ended", found, err)
	}
}

// A wake a pause holds records its pause and pairs with release 0, whatever release the issue
// had, and starts its stuck clock at its own moment; a reservation moves that clock later and
// never back; a wake no pause holds is the wake it always was, paired with the issue's release.
func TestAHeldWakeRecordsItsPauseAndPairsWithNoRelease(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	pause, _ := pauseLegion(t, st, pausedAt)
	if _, err := st.Pool().Exec(ctx, `insert into pod_releases (project, issue, roles, at) values ('legion', 'LEGION-208', 'implementer tester', $1)`,
		pausedAt.Add(-time.Hour)); err != nil {
		t.Fatalf("record the release of LEGION-208: %v", err)
	}
	records := podRecords(t, st)
	inTx(t, st, func(tx pgx.Tx) error {
		if taken, err := records.TakePodWake(ctx, tx, "LEGION-208", claim.RoleImplementer, pods.WakePause, pausedAt, pause.ID); err != nil || !taken {
			t.Fatalf("take the held wake = %t, %v; want it taken", taken, err)
		}
		if taken, err := records.TakePodWake(ctx, tx, "LEGION-208", claim.RoleTester, pods.WakeChecks, pausedAt, 0); err != nil || !taken {
			t.Fatalf("take the tester's wake = %t, %v; want it taken", taken, err)
		}
		return nil
	})
	read := func(role claim.Role) (release int64, pauseID *int64, clock time.Time) {
		if err := st.Pool().QueryRow(ctx, `select release_id, pause_id, clock_at from pod_resumes where issue = 'LEGION-208' and role = $1`,
			string(role)).Scan(&release, &pauseID, &clock); err != nil {
			t.Fatalf("read the %s's resume: %v", role, err)
		}
		return release, pauseID, clock
	}
	release, held, clock := read(claim.RoleImplementer)
	if release != 0 || held == nil || *held != pause.ID || !clock.Equal(pausedAt) {
		t.Fatalf("the held wake's resume = release %d, pause %v, clock %s; want release 0, pause %d, clock %s", release, held, clock, pause.ID, pausedAt)
	}
	release, held, clock = read(claim.RoleTester)
	if release == 0 || held != nil || !clock.Equal(pausedAt) {
		t.Fatalf("the unheld wake's resume = release %d, pause %v, clock %s; want the issue's release, no pause, clock %s", release, held, clock, pausedAt)
	}

	if set, err := st.SetResumeClock(ctx, "legion", "LEGION-208", claim.RoleImplementer, pausedAt.Add(5*time.Minute)); err != nil || !set {
		t.Fatalf("set the implementer's clock = %t, %v; want it set", set, err)
	}
	if set, err := st.SetResumeClock(ctx, "legion", "LEGION-208", claim.RoleImplementer, pausedAt.Add(time.Minute)); err != nil || !set {
		t.Fatalf("set the implementer's clock earlier = %t, %v; want the open resume found", set, err)
	}
	if _, _, clock := read(claim.RoleImplementer); !clock.Equal(pausedAt.Add(5 * time.Minute)) {
		t.Fatalf("the implementer's clock = %s, want %s: a clock never moves back", clock, pausedAt.Add(5*time.Minute))
	}
	inTx(t, st, func(tx pgx.Tx) error {
		if closed, err := CloseRoleOpenResumeTx(ctx, tx, "legion", "LEGION-208", claim.RoleImplementer, pausedAt.Add(time.Hour)); err != nil || !closed {
			t.Fatalf("close the implementer's open resume = %t, %v; want it closed", closed, err)
		}
		return nil
	})
	if set, err := st.SetResumeClock(ctx, "legion", "LEGION-208", claim.RoleImplementer, pausedAt.Add(2*time.Hour)); err != nil || set {
		t.Fatalf("set the clock of a closed resume = %t, %v; want no open resume found", set, err)
	}
}

// Decision 15 of dispatch://INFRA-3759, for a claim of another project's issue, which the
// operator's spawn (`legion claims spawn`) makes under this project's token: a wake of it is held
// by this project's pause, as every other claim's is. The rule of a wake's pause (PauseOfWake)
// reads the open pause of the project whose pod tables it is, never another project's: it answers
// no pause while only OTHER is paused, and LEGION's pause once LEGION is paused too.
func TestAWakeOfAClaimOfAnotherProjectsIssueIsHeldByThisProjectsPause(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := podRecords(t, st)
	token, err := claim.NewToken("legion", "OTHER-5", claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	pauseOf := func() int64 {
		t.Helper()
		var pause int64
		inTx(t, st, func(tx pgx.Tx) error {
			var err error
			pause, _, err = records.PauseOfWake(ctx, tx, "OTHER-5", token, 0)
			return err
		})
		return pause
	}

	other, _ := pauseProject(t, st, "OTHER", pausedAt)
	if got := pauseOf(); got != 0 {
		t.Fatalf("the pause of a wake through legion's pod records while only OTHER is paused = %d, want 0: OTHER's pause %d is not this project's",
			got, other.ID)
	}
	legion, _ := pauseLegion(t, st, pausedAt.Add(time.Minute))
	if got := pauseOf(); got != legion.ID {
		t.Fatalf("the pause of a wake through legion's pod records while LEGION is paused = %d, want LEGION's pause %d", got, legion.ID)
	}
}

// A delivery wake taken while the project is paused is the open pause's, whatever mark its row
// carries (PauseOfWake, decision 15 of dispatch://INFRA-3759): a row a lane leased before the
// pause runs to its end while the pause stands, and its mark is 0, or the id of an older pause
// that held it. Its resume names the open pause and pairs with no release, as a wake the workflow
// takes while the project is paused does. Before the fix the delivery wake read its row's mark
// alone.
func TestADeliveryWakeTakenWhileTheProjectIsPausedIsTheOpenPauses(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	recordRelease(t, st, pausedAt.Add(-3*time.Hour))
	older, _ := pauseLegion(t, st, pausedAt.Add(-2*time.Hour))
	endPause(t, st, older.ID, pausedAt.Add(-time.Hour))
	open, _ := pauseLegion(t, st, pausedAt)

	for _, wake := range []struct {
		token claim.Token
		role  claim.Role
		mark  int64
	}{
		{token: releasedToken, role: claim.RoleImplementer, mark: older.ID},
		{token: "legion-legion-legion-208-tester", role: claim.RoleTester},
	} {
		if taken, err := st.TakePodWake(ctx, "legion", rootIssue, wake.token, pods.WakeDelivery, pausedAt.Add(time.Minute), wake.mark); err != nil || !taken {
			t.Fatalf("take the %s's wake = %t, %v; want it taken", wake.role, taken, err)
		}
		var release, held int64
		if err := st.Pool().QueryRow(ctx, `select release_id, coalesce(pause_id, 0) from pod_resumes where issue = $1 and role = $2`,
			rootIssue, string(wake.role)).Scan(&release, &held); err != nil {
			t.Fatalf("read the %s's resume: %v", wake.role, err)
		}
		if held != open.ID || release != 0 {
			t.Fatalf("the %s's wake of a row marked %d = pause %d and release %d, want the open pause %d and no release",
				wake.role, wake.mark, held, release, open.ID)
		}
	}
}

// The resume launches by rule each operator's spawn its pause stopped owing a launch, since no
// start row can carry it (projectPause.Resumed in internal/daemon), and the store names them: a
// claim of an issue with no record whose stop of the pause, or a stop that ended while the pause
// stood, is `stopped` with a launch owed and still holds it suspended by the release's source; a
// spawn of another project's issue, whose stop the pause records under its own project, among
// them, and so is such a spawn whose issue another project's daemon records on the same
// database: that record is of another project, so it is no record of this one's. Not a stop owed
// nothing, not a claim of a recorded issue, not a claim that runs again or that the operator has
// suspended since, and not a stop of an earlier pause that ended before this one began.
func TestTheResumeNamesTheOperatorsSpawnsItsPauseStoppedOwingALaunch(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	operator := PauseActor{Kind: PauseByOperator}
	if _, err := st.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1),
			('OTHER-6', 'OTHER-6', 'OTHER', 'root', 'planning', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("record LEGION-208 and OTHER's OTHER-6: %v", err)
	}
	stopOf := func(issue string, state supervise.ClaimState, by supervise.SuspendSource) PauseStopClaim {
		t.Helper()
		token, err := claim.NewToken("legion", issue, claim.RoleArchitect)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue, Issue: issue, Role: claim.RoleArchitect,
			Generation: 1, State: state, SuspendedBy: by}); err != nil {
			t.Fatalf("seed the claim of %s: %v", issue, err)
		}
		return PauseStopClaim{Claim: token, Tree: issue, Issue: issue, Role: claim.RoleArchitect, Generation: 1}
	}
	suspended, released := supervise.StateSuspended, supervise.SuspendedByReleased
	stopped := func(pause Pause, stop PauseStopClaim, at time.Time, owed bool) {
		t.Helper()
		if err := st.Tx(ctx, func(tx pgx.Tx) error {
			_, err := RecordPauseStoppedTx(ctx, tx, pause, stop, at, owed)
			return err
		}); err != nil {
			t.Fatalf("record the stop of %s: %v", stop.Claim, err)
		}
	}

	earlier, _ := pauseLegion(t, st, pausedAt.Add(-2*time.Hour))
	stopped(earlier, stopOf("LEGION-81", suspended, released), pausedAt.Add(-90*time.Minute), true)
	if _, err := st.Resume(ctx, ResumeRequest{Project: "LEGION", By: operator, At: pausedAt.Add(-time.Hour)}); err != nil {
		t.Fatalf("resume the earlier pause: %v", err)
	}
	pause, _ := pauseLegion(t, st, pausedAt)
	spawn := stopOf("LEGION-77", suspended, released)
	stopped(pause, spawn, pausedAt.Add(time.Minute), true)
	stopped(pause, stopOf("LEGION-78", suspended, released), pausedAt.Add(time.Minute), false)
	stopped(pause, stopOf("LEGION-208", suspended, released), pausedAt.Add(time.Minute), true)
	stopped(pause, stopOf("LEGION-79", supervise.StateIdle, ""), pausedAt.Add(time.Minute), true)
	stopped(pause, stopOf("LEGION-80", suspended, supervise.SuspendedByOperator), pausedAt.Add(time.Minute), true)
	// A spawn of another project's issue, stopped under this project's pause, and one whose issue
	// the OTHER project's daemon records.
	foreign := stopOf("OTHER-5", suspended, released)
	stopped(pause, foreign, pausedAt.Add(time.Minute), true)
	recordedForeign := stopOf("OTHER-6", suspended, released)
	stopped(pause, recordedForeign, pausedAt.Add(time.Minute), true)
	late := stopOf("LEGION-82", suspended, released)
	stopped(earlier, late, pausedAt.Add(10*time.Minute), true)
	ended, err := st.Resume(ctx, ResumeRequest{Project: "LEGION", By: operator, At: pausedAt.Add(time.Hour)})
	if err != nil {
		t.Fatalf("resume LEGION: %v", err)
	}

	owed, err := st.OwedLaunchesWithNoIssueRecord(ctx, ended.Pause)
	if err != nil {
		t.Fatalf("read the spawns the pause owes a launch: %v", err)
	}
	if want := []claim.Token{spawn.Claim, late.Claim, foreign.Claim, recordedForeign.Claim}; !slices.Equal(owed, want) {
		t.Fatalf("the spawns pause %d owes a launch = %v, want %v", ended.Pause.ID, owed, want)
	}
}

// The pause a wake of a claim belongs to is the pause of the claim's newest stop, whatever that
// stop's end, where it ended `stopped` after both the issue's newest pod release and the claim's
// last ready (StoppingPauseTx, decision 15 of dispatch://INFRA-3759). Each claim here has a stop
// by the first pause that ended `stopped`, and some a later one: stopped again after a ready the
// resume's launch reported, the wake is the second pause's; a later stop that the resume
// cancelled answers no pause, since the claim's newest stop stopped nothing.
//
// The negative control is StoppingPauseTx reading the claim's stops oldest first: the claim
// stopped again answers 0, since its first stop is older than its ready, and the claim whose later
// stop was cancelled answers the first pause.
func TestAWakesStoppingPauseIsTheClaimsNewestStop(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	base := pausedAt
	pauseOf := func(began, ended time.Time) int64 {
		t.Helper()
		var id int64
		if err := st.pool.QueryRow(ctx, `insert into project_pauses (project, began_at, by_kind, reason, ended_at, ended_by_kind, ended_by_session)
			values ('LEGION', $1, 'operator', 'a pause of the test', $2, 'operator', '') returning id`, began, ended).Scan(&id); err != nil {
			t.Fatalf("record a pause: %v", err)
		}
		return id
	}
	first, second := pauseOf(base, base.Add(10*time.Minute)), pauseOf(base.Add(20*time.Minute), base.Add(30*time.Minute))
	type role struct {
		token claim.Token
		issue string
	}
	implementer := func(key string, lastReady time.Time) role {
		t.Helper()
		token, err := claim.NewToken("legion", key, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: key, Issue: key,
			Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle, LastReady: lastReady}); err != nil {
			t.Fatalf("record the claim of %s: %v", key, err)
		}
		return role{token: token, issue: key}
	}
	stop := func(pause int64, of role, end PauseStopEnd, at time.Time) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, `insert into project_pause_stops (pause_id, project, claim, issue, role, asked_at, ended_at, ended_as)
			values ($1, 'LEGION', $2, $3, 'implementer', $4, $4, $5)`, pause, string(of.token), of.issue, at, string(end)); err != nil {
			t.Fatalf("record pause %d's stop of %s: %v", pause, of.token, err)
		}
	}
	release := func(of role, at time.Time) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, `insert into pod_releases (project, issue, generation, roles, at) values ('legion', $1, 1, 'implementer', $2)`,
			of.issue, at); err != nil {
			t.Fatalf("record a release of %s's pod: %v", of.issue, err)
		}
	}

	once := implementer("LEGION-301", time.Time{})
	stop(first, once, PauseStopStopped, base.Add(5*time.Minute))
	readySince := implementer("LEGION-302", base.Add(15*time.Minute))
	stop(first, readySince, PauseStopStopped, base.Add(5*time.Minute))
	releasedSince := implementer("LEGION-303", time.Time{})
	stop(first, releasedSince, PauseStopStopped, base.Add(5*time.Minute))
	release(releasedSince, base.Add(15*time.Minute))
	stoppedAgain := implementer("LEGION-304", base.Add(15*time.Minute))
	stop(first, stoppedAgain, PauseStopStopped, base.Add(5*time.Minute))
	stop(second, stoppedAgain, PauseStopStopped, base.Add(25*time.Minute))
	cancelledSince := implementer("LEGION-305", time.Time{})
	stop(first, cancelledSince, PauseStopStopped, base.Add(5*time.Minute))
	stop(second, cancelledSince, PauseStopCancelled, base.Add(30*time.Minute))

	for _, c := range []struct {
		name string
		of   role
		want int64
	}{
		{"stopped once", once, first},
		{"stopped, then ready", readySince, 0},
		{"stopped, then its pod released", releasedSince, 0},
		{"stopped, ready, then stopped again by the second pause", stoppedAgain, second},
		{"stopped, then a later stop of it cancelled", cancelledSince, 0},
	} {
		var got int64
		inTx(t, st, func(tx pgx.Tx) error {
			var err error
			got, err = StoppingPauseTx(ctx, tx, "legion", c.of.issue, c.of.token)
			return err
		})
		if got != c.want {
			t.Errorf("%s: StoppingPauseTx = %d, want %d (the first pause is %d, the second %d)", c.name, got, c.want, first, second)
		}
	}
}

// The last pause is the newest a resume ended (LastEndedPause), and the state's one read answers
// the open pause while one stands and that newest ended pause otherwise (OpenOrLastPauseTx). Two
// pauses end here before a third begins, so a read of the oldest ended pause names the first.
//
// The negative control is either read taking the ended pauses oldest first: each answers the first
// pause where the second is the newest.
func TestTheLastPauseIsTheNewestTheResumeEnded(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	operator := PauseActor{Kind: PauseByOperator}
	resume := func(at time.Time) {
		t.Helper()
		if _, err := st.Resume(ctx, ResumeRequest{Project: "LEGION", By: operator, At: at}); err != nil {
			t.Fatalf("resume at %s: %v", at, err)
		}
	}
	openOrLast := func() Pause {
		t.Helper()
		var got Pause
		inTx(t, st, func(tx pgx.Tx) error {
			p, found, err := OpenOrLastPauseTx(ctx, tx, "LEGION")
			if err == nil && !found {
				err = errors.New("no pause found")
			}
			got = p
			return err
		})
		return got
	}

	first, _ := pauseLegion(t, st, pausedAt)
	resume(pausedAt.Add(time.Hour))
	second, _ := pauseLegion(t, st, pausedAt.Add(2*time.Hour))
	resume(pausedAt.Add(3 * time.Hour))
	if last, found, err := st.LastEndedPause(ctx, "LEGION"); err != nil || !found || last.ID != second.ID {
		t.Fatalf("LastEndedPause after two resumes = %d, %t, %v; want the second pause %d (the first is %d)", last.ID, found, err, second.ID, first.ID)
	}
	if got := openOrLast(); got.ID != second.ID || got.EndedAt.IsZero() {
		t.Fatalf("OpenOrLastPauseTx after two resumes = pause %d ended %s; want the second pause %d, ended (the first is %d)",
			got.ID, got.EndedAt, second.ID, first.ID)
	}
	third, _ := pauseLegion(t, st, pausedAt.Add(4*time.Hour))
	if got := openOrLast(); got.ID != third.ID || !got.EndedAt.IsZero() {
		t.Fatalf("OpenOrLastPauseTx while the third pause stands = pause %d ended %s; want the open pause %d", got.ID, got.EndedAt, third.ID)
	}
}

// A pause's own stop of a role is recorded on the role's newest stop row of that pause
// (RecordPauseStoppedTx): a stop the role's takeover ended stays as it ended, the stop the gate
// records after it is a row of its own, and a later stop the pause's suspension makes of the role
// only adds the launch it owes to that newest row, since the role is already stopped.
//
// The negative control is RecordPauseStoppedTx reading the pause's stops of the role oldest
// first: the third record finds the taken-over row, writes a third row and answers that it
// recorded a stop, where the role's newest row already says it stopped.
func TestAPausesStopOfARoleIsRecordedOnItsNewestStopRow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedPauseClaims(t, st)
	pause, stops := pauseLegion(t, st, pausedAt)
	var implementer PauseStopClaim
	for _, stop := range stops {
		if stop.Claim == "legion-legion-legion-208-implementer" {
			implementer = stop
		}
	}
	if implementer.Claim == "" {
		t.Fatalf("the pause asked no stop of the implementer of LEGION-208: %+v", stops)
	}
	if ended, err := st.EndPauseStop(ctx, pause.ID, implementer.Claim, PauseStopTakenOver, pausedAt.Add(time.Minute)); err != nil || !ended {
		t.Fatalf("end the implementer's stop as taken over = %t, %v; want it ended", ended, err)
	}
	recordStop := func(at time.Time, launchOwed bool) bool {
		t.Helper()
		var recorded bool
		inTx(t, st, func(tx pgx.Tx) error {
			var err error
			recorded, err = RecordPauseStoppedTx(ctx, tx, pause, implementer, at, launchOwed)
			return err
		})
		return recorded
	}
	if !recordStop(pausedAt.Add(2*time.Minute), false) {
		t.Fatalf("the stop after the takeover answered that the role was already stopped; want a stop of its own recorded")
	}
	if recordStop(pausedAt.Add(3*time.Minute), true) {
		t.Fatalf("the second stop answered that it recorded a stop; want the role's newest row, which already ended stopped, to stand")
	}
	rows := pauseCount(t, st, `select count(*) from project_pause_stops where pause_id = $1 and claim = $2`, pause.ID, string(implementer.Claim))
	owed := pauseCount(t, st, `select count(*) from project_pause_stops where pause_id = $1 and claim = $2
		and ended_as = 'stopped' and launch_owed and ended_at = $3`, pause.ID, string(implementer.Claim), pausedAt.Add(2*time.Minute))
	if rows != 2 || owed != 1 {
		t.Fatalf("the pause's stop rows of the implementer = %d, with %d stopped at the second record owing a launch; want 2 rows, the newest owing the launch", rows, owed)
	}
}
