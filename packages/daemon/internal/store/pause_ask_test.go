package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// stopsAsked is the claims of stops, in order.
func stopsAsked(stops []PauseStopClaim) []claim.Token {
	tokens := make([]claim.Token, len(stops))
	for i, stop := range stops {
		tokens[i] = stop.Claim
	}
	return tokens
}

// The daemon's catch-up after a pause and its boot's pass while paused ask, through the pause's own
// row builder, the stop of each claim the pause has asked none of: a claim with an open stop has
// its row queued already, and one whose stop has ended was stopped for this pause, so neither is
// asked again. The boot's pass reads the live claims the store holds itself. A pause that has
// ended asks nothing.
func TestThePausesCatchUpAsksOnlyTheStopsItHasNotAsked(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedPauseClaims(t, st)
	pause, _ := pauseLegion(t, st, pausedAt)
	implementer := PauseStopClaim{Claim: "legion-legion-legion-208-implementer", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleImplementer, Generation: 1}
	missed := PauseStopClaim{Claim: "legion-legion-legion-211-reviewer", Tree: "LEGION-208", Issue: "LEGION-211",
		Role: claim.RoleReviewer, Generation: 1}

	asked, err := st.AskPauseStops(ctx, pause.ID, []PauseStopClaim{implementer, missed}, pausedAt.Add(time.Second), pauseStopRow)
	if err != nil || !slices.Equal(stopsAsked(asked), []claim.Token{missed.Claim}) {
		t.Fatalf("the catch-up asked %v, %v; want %s alone: the pause's transaction asked the implementer", stopsAsked(asked), err, missed.Claim)
	}
	if got := pauseCount(t, st, `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'pause_stop'
		and issue = 'LEGION-211' and (payload->>'pause')::bigint = $1`, pause.ID); got != 1 {
		t.Fatalf("pause_stop rows of LEGION-211 = %d, want the one the catch-up queued", got)
	}

	// The implementer stops; a role the boot finds live — its claim written live since the pause
	// read the claims — has never been asked.
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		_, err := RecordPauseStoppedTx(ctx, tx, pause, implementer, pausedAt.Add(time.Minute), false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	late := supervise.Claim{Token: "legion-legion-legion-212-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-212",
		Role: claim.RoleTester, Generation: 3, State: supervise.StateLaunching}
	if err := st.PutClaim(ctx, late); err != nil {
		t.Fatal(err)
	}
	asked, err = st.AskLivePauseStops(ctx, pause.ID, pausedAt.Add(2*time.Minute), pauseStopRow)
	if err != nil || !slices.Equal(stopsAsked(asked), []claim.Token{late.Token}) {
		t.Fatalf("the boot's pass asked %v, %v; want %s alone: the implementer's stop ended and the tester's is open", stopsAsked(asked), err, late.Token)
	}
	if asked[0].Generation != late.Generation || asked[0].Tree != late.Tree {
		t.Fatalf("the boot's pass asked %+v, want the stored claim's tree and generation", asked[0])
	}

	if _, err := st.Resume(ctx, ResumeRequest{Project: "LEGION", By: PauseActor{Kind: PauseByOperator}, At: pausedAt.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if asked, err := st.AskLivePauseStops(ctx, pause.ID, pausedAt.Add(2*time.Hour), pauseStopRow); err != nil || len(asked) != 0 {
		t.Fatalf("the pass over an ended pause asked %v, %v; want nothing", stopsAsked(asked), err)
	}
}

// A live claim of another project's issue, which the operator's spawn makes under this project's
// token, is a session this daemon runs, so the pause's transaction asks its stop as it asks every
// other live role's: one stop row under the pause's own project, and one `pause_stop` row keyed by
// the project, since the outbox runs only the rows of its own project's keys, that names the
// claim's issue in its payload. The stop transaction's expedite finds that row by the claim's
// issue and answers its key, whose lane the daemon wakes; the boot's pass asks nothing more of a
// claim already asked; and the resume deletes the row while it has not run. A stop row its builder
// keys by neither the claim's issue nor the project is refused, and the pause with it.
func TestAPauseAsksTheStopOfALiveClaimOfAnotherProjectsIssueUnderItsProjectsKey(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedPauseClaims(t, st)
	foreign := supervise.Claim{Token: "legion-legion-infra-5-implementer", Project: "legion", Tree: "INFRA-5", Issue: "INFRA-5",
		Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle}
	if err := st.PutClaim(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	pause, asked, err := st.Pause(ctx, PauseRequest{Project: "LEGION", By: PauseActor{Kind: PauseByOperator},
		Reason: "the model provider is down", At: pausedAt, StopRow: pauseStopRow})
	if err != nil {
		t.Fatalf("pause with a live claim of another project's issue: %v", err)
	}
	if want := []claim.Token{foreign.Token, "legion-legion-legion-208-implementer", "legion-legion-legion-209-tester"}; !slices.Equal(stopsAsked(asked), want) {
		t.Fatalf("the pause asked %v, want %v: the claim of INFRA-5 is the daemon's to stop", stopsAsked(asked), want)
	}
	var project string
	if err := st.Pool().QueryRow(ctx, `select project from project_pause_stops where pause_id = $1 and claim = $2 and ended_at is null`,
		pause.ID, string(foreign.Token)).Scan(&project); err != nil || project != "LEGION" {
		t.Fatalf("the open stop of %s names project %q, %v; want the pause's own, LEGION", foreign.Token, project, err)
	}
	if got := pauseCount(t, st, `select count(*) from outbox where issue = 'INFRA-5'`); got != 0 {
		t.Fatalf("outbox rows keyed by INFRA-5 = %d, want none: no daemon's outbox runs them", got)
	}
	row := record.OutboxRow{Kind: record.OutboxKindSupervise}
	if err := st.Pool().QueryRow(ctx, `select id, issue, payload from outbox where `+pauseStopRows+` and issue = 'LEGION'`).
		Scan(&row.ID, &row.Issue, &row.Payload); err != nil {
		t.Fatalf("read the pause_stop row keyed by LEGION: %v", err)
	}
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("decode the stop row of %s: %v", foreign.Token, err)
	}
	if request := payload.(record.SuperviseRequest); request.Issue != foreign.Issue || request.Role != foreign.Role || request.Pause != pause.ID {
		t.Fatalf("the stop row keyed by LEGION = %+v, want pause %d's stop of INFRA-5's implementer", request, pause.ID)
	}

	// The claim's turn held the stop, whose row waits; the stop transaction makes it due by the
	// claim's issue, and answers the key whose lane runs it.
	if _, err := st.Pool().Exec(ctx, `update outbox set next_at = $2 where id = $1`, row.ID, pausedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var keys []string
	inTx(t, st, func(tx pgx.Tx) error {
		var err error
		keys, err = ExpeditePauseStopRowsTx(ctx, tx, "LEGION", foreign.Issue, foreign.Role, pausedAt.Add(time.Minute))
		return err
	})
	if !slices.Equal(keys, []string{"LEGION"}) {
		t.Fatalf("the expedite of INFRA-5's implementer moved the rows keyed %v, want LEGION's alone", keys)
	}
	if got := pauseCount(t, st, `select count(*) from outbox where id = $1 and next_at = $2`, row.ID, pausedAt.Add(time.Minute)); got != 1 {
		t.Fatalf("the stop row of %s is not due at the expedite's moment", foreign.Token)
	}

	if asked, err := st.AskLivePauseStops(ctx, pause.ID, pausedAt.Add(2*time.Minute), pauseStopRow); err != nil || len(asked) != 0 {
		t.Fatalf("the boot's pass asked %v, %v; want nothing: every live role's stop is asked", stopsAsked(asked), err)
	}
	resumed, err := st.Resume(ctx, ResumeRequest{Project: "LEGION", By: PauseActor{Kind: PauseByOperator}, At: pausedAt.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.DeletedStopRows != 3 || pauseCount(t, st, `select count(*) from outbox where `+pauseStopRows) != 0 {
		t.Fatalf("the resume deleted %d stop rows, want the three that had not run, the one keyed by LEGION among them", resumed.DeletedStopRows)
	}

	astray := func(p Pause, stop PauseStopClaim) (PauseRow, error) {
		row, err := pauseStopRow(p, stop)
		if stop.Issue == foreign.Issue {
			row.Issue = "INFRA"
		}
		return row, err
	}
	if _, _, err := st.Pause(ctx, PauseRequest{Project: "LEGION", By: PauseActor{Kind: PauseByOperator},
		Reason: "again", At: pausedAt.Add(2 * time.Hour), StopRow: astray}); err == nil || !strings.Contains(err.Error(), `keyed by "INFRA"`) {
		t.Fatalf("a pause whose stop row of INFRA-5 is keyed by INFRA = %v, want it refused", err)
	}
	if got := pauseCount(t, st, `select count(*) from project_pauses where ended_at is null`); got != 0 {
		t.Fatalf("open pauses after the refused pause = %d, want none", got)
	}
}

// A stop recorded twice for one pause and claim — the stop transaction made again because its
// commit's answer was lost — keeps one row and answers that the second was no first record, owing
// a launch where either did. A stop ended as stopped by the executor, for a claim queued with
// nothing to stop, owes no launch; one ended taken over is followed by a row of its own when the
// pause's own stop lands later.
func TestAPauseStopRecordedAgainKeepsItsOneRow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedPauseClaims(t, st)
	pause, stops := pauseLegion(t, st, pausedAt)
	implementer, tester := stops[0], stops[1]

	record := func(stop PauseStopClaim, owed bool) bool {
		t.Helper()
		var first bool
		if err := st.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			first, err = RecordPauseStoppedTx(ctx, tx, pause, stop, pausedAt.Add(time.Minute), owed)
			return err
		}); err != nil {
			t.Fatalf("record the stop of %s: %v", stop.Claim, err)
		}
		return first
	}
	if !record(implementer, false) {
		t.Fatal("the implementer's first stop answered no first record")
	}
	if record(implementer, true) {
		t.Fatal("the implementer's stop made again answered a first record")
	}
	var rows int
	var owed bool
	if err := st.Pool().QueryRow(ctx, `select count(*), bool_or(launch_owed) from project_pause_stops where pause_id = $1 and claim = $2`,
		pause.ID, string(implementer.Claim)).Scan(&rows, &owed); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || !owed {
		t.Fatalf("the implementer's stops = %d rows, owed %t; want one row owing the launch the second owed", rows, owed)
	}

	if ended, err := st.EndPauseStop(ctx, pause.ID, tester.Claim, PauseStopTakenOver, pausedAt.Add(time.Minute)); err != nil || !ended {
		t.Fatalf("end the tester's stop taken over = %t, %v", ended, err)
	}
	if !record(tester, true) {
		t.Fatal("the tester's own stop after a take-over answered no first record")
	}
	var ends []string
	rowsOf, err := st.Pool().Query(ctx, `select ended_as from project_pause_stops where pause_id = $1 and claim = $2 order by id`,
		pause.ID, string(tester.Claim))
	if err != nil {
		t.Fatal(err)
	}
	for rowsOf.Next() {
		var end string
		if err := rowsOf.Scan(&end); err != nil {
			t.Fatal(err)
		}
		ends = append(ends, end)
	}
	rowsOf.Close()
	if !slices.Equal(ends, []string{"taken_over", "stopped"}) {
		t.Fatalf("the tester's stops = %v, want the take-over and then its own stop", ends)
	}

	queued := PauseStopClaim{Claim: "legion-legion-legion-213-implementer", Tree: "LEGION-208", Issue: "LEGION-213",
		Role: claim.RoleImplementer, Generation: 1}
	if _, err := st.AskPauseStops(ctx, pause.ID, []PauseStopClaim{queued}, pausedAt, pauseStopRow); err != nil {
		t.Fatal(err)
	}
	if ended, err := st.EndPauseStop(ctx, pause.ID, queued.Claim, PauseStopStopped, pausedAt.Add(time.Minute)); err != nil || !ended {
		t.Fatalf("end the queued claim's stop stopped = %t, %v", ended, err)
	}
	if got := pauseCount(t, st, `select count(*) from project_pause_stops where claim = $1 and ended_as = 'stopped' and not launch_owed`,
		string(queued.Claim)); got != 1 {
		t.Fatalf("the queued claim's stops ended stopped owing nothing = %d, want 1", got)
	}
}

// A pause's stop arms the backstop of its own claim's issue and role alone: a record that hands it
// another role's backstop is refused, and its transaction writes nothing.
func TestAPauseStopRecordRefusesTheBackstopOfAnotherRole(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedPauseClaims(t, st)
	pause, stops := pauseLegion(t, st, pausedAt)
	implementer := stops[0]

	err := st.Tx(ctx, func(tx pgx.Tx) error {
		_, err := RecordPauseStopTx(ctx, tx, PauseStopRecord{Pause: pause, Stop: implementer, At: pausedAt.Add(time.Minute),
			HoldPod: true, Backstop: &PodWakeBackstop{Project: "legion", Issue: implementer.Issue, Role: claim.RoleTester,
				DueAt: pausedAt.Add(time.Hour)}})
		return err
	})
	if err == nil {
		t.Fatal("a stop of the implementer armed the tester's backstop, want it refused")
	}
	if got := pauseCount(t, st, `select count(*) from project_pause_stops where claim = $1 and ended_at is not null`,
		string(implementer.Claim)); got != 0 {
		t.Fatalf("the implementer's stops recorded by the refused record = %d, want none", got)
	}
	if got := pauseCount(t, st, `select count(*) from project_pause_pod_holds`) + pauseCount(t, st, `select count(*) from pod_wake_backstops`); got != 0 {
		t.Fatalf("holds and backstops written by the refused record = %d, want none", got)
	}
}
