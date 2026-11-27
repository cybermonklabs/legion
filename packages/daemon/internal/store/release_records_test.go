package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// holdRows is every hold of the pod, oldest first, with whether its streak is still running.
func holdRows(t *testing.T, st *Store, issue string) []struct {
	PodHold
	Ended bool
} {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `select id, project, issue, reason, detail, first_at, at, passes, ended
		from pod_holds where issue = $1 order by id`, issue)
	if err != nil {
		t.Fatalf("read the holds of %s: %v", issue, err)
	}
	defer rows.Close()
	var out []struct {
		PodHold
		Ended bool
	}
	for rows.Next() {
		var row struct {
			PodHold
			Ended bool
		}
		if err := rows.Scan(&row.ID, &row.Project, &row.Issue, &row.Reason, &row.Detail,
			&row.FirstAt, &row.At, &row.Passes, &row.Ended); err != nil {
			t.Fatalf("read the holds of %s: %v", issue, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the holds of %s: %v", issue, err)
	}
	return out
}

// unfinishedResumes is the stranded read a test makes, in a transaction of its own:
// UnfinishedPodResumesTx is the only form of it, because every caller of those rows in the
// daemon already holds a transaction of this pool (release_records.go). The error comes back
// rather than failing here, so each caller keeps saying what it expected of the rows.
func unfinishedResumes(ctx context.Context, st *Store, project string, before time.Time) ([]PodResume, error) {
	var out []PodResume
	err := Transact(ctx, st.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = st.UnfinishedPodResumesTx(ctx, tx, project, before)
		return err
	})
	return out, err
}

// A hold row is a streak and not a tally of a pod's whole life: every pass of the same reason
// counts into the running streak, and the streak ends when the pod is released, when the rule
// reads another reason for the pod, or when a pass reads no reason worth a row. A pod held for
// the same reason after any of those opens a streak of its own, with its own first pass, so a
// window's rows are the holds of that window.
func TestAPodsHoldStreakEndsWhenItIsReleasedOrItsReasonChanges(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	hold := func(reason string, pass int) {
		t.Helper()
		if err := st.RecordPodHold(ctx, PodHold{Project: "legion", Issue: rootIssue, Reason: reason,
			At: at.Add(time.Duration(pass) * time.Minute)}); err != nil {
			t.Fatalf("record the %s hold: %v", reason, err)
		}
	}

	hold("image", 0)
	hold("image", 1)
	if rows := holdRows(t, st, rootIssue); len(rows) != 1 || rows[0].Passes != 2 || rows[0].Ended ||
		!rows[0].FirstAt.Equal(at) || !rows[0].At.Equal(at.Add(time.Minute)) {
		t.Fatalf("after two passes of one reason the holds are %+v, want one running streak of two passes", rows)
	}

	// Monday's streak ends when the pod is released; Friday's hold for the same reason is its
	// own streak, with its own first pass and its own count.
	if _, _, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer"}, At: at.Add(2 * time.Minute)}); err != nil {
		t.Fatalf("record the release: %v", err)
	}
	hold("image", 3)
	rows := holdRows(t, st, rootIssue)
	if len(rows) != 2 {
		t.Fatalf("after the release and a later hold the holds are %+v, want two streaks", rows)
	}
	if !rows[0].Ended || rows[0].Passes != 2 || !rows[0].FirstAt.Equal(at) {
		t.Fatalf("the first streak is %+v, want it ended at two passes from %s", rows[0], at)
	}
	if rows[1].Ended || rows[1].Passes != 1 || !rows[1].FirstAt.Equal(at.Add(3*time.Minute)) {
		t.Fatalf("the second streak is %+v, want a running streak of one pass from %s", rows[1], at.Add(3*time.Minute))
	}

	// A pass that reads another reason ends the running streak and opens that reason's own.
	hold("unreported", 4)
	rows = holdRows(t, st, rootIssue)
	if len(rows) != 3 || !rows[1].Ended || rows[2].Reason != "unreported" || rows[2].Ended {
		t.Fatalf("after a pass under another reason the holds are %+v, want the image streak ended and an unreported one running", rows)
	}

	// A pass that holds the pod for nothing worth a row ends what is running, so the next hold
	// of that reason is a streak of its own rather than a continuation days later.
	if err := st.EndPodHoldStreaks(ctx, "legion", rootIssue); err != nil {
		t.Fatalf("end the streaks: %v", err)
	}
	hold("unreported", 5)
	rows = holdRows(t, st, rootIssue)
	if len(rows) != 4 || !rows[2].Ended || rows[3].Passes != 1 || !rows[3].FirstAt.Equal(at.Add(5*time.Minute)) {
		t.Fatalf("after a pass that held nothing the holds are %+v, want a fourth streak of one pass", rows)
	}
	// Every streak is still readable in a window that covers it, which is what R8 reads.
	records, err := st.PodReleaseRecords(ctx, "legion", at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(records.Holds) != 4 || len(records.Releases) != 1 {
		t.Fatalf("the window holds %d holds and %d releases, want 4 and 1", len(records.Holds), len(records.Releases))
	}
}

// R6: the stranded report reads the wakes of a pod's newest release alone. A wake of an older
// release that never reached a first turn is a pod the daemon has released again since, not one
// a wake never brought back, and reading it would list that role as stranded the moment its next
// release was recorded. The stuck pass, which warns about a wake once whatever came after it,
// keeps reading every unfinished wake.
func TestTheStrandedReadSeesOnlyTheWakesOfTheNewestRelease(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	first, _, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer"}, At: at})
	if err != nil {
		t.Fatalf("record the first release: %v", err)
	}
	wake := func(release int64, role string, when time.Time) {
		t.Helper()
		if err := Transact(ctx, st.pool, func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := RecordPodWakeTx(ctx, tx, PodResume{Release: release, Project: "legion", Issue: rootIssue,
				Role: role, Kind: pods.WakeDelivery, At: when})
			return err
		}); err != nil {
			t.Fatalf("record the wake of %s: %v", role, err)
		}
	}
	wake(first, "implementer", at.Add(time.Minute))

	bound := time.Now().UTC()
	if unfinished, err := unfinishedResumes(ctx, st, "legion", bound); err != nil || len(unfinished) != 1 {
		t.Fatalf("unfinished wakes of the only release = %+v, %v; want the one wake", unfinished, err)
	}

	// The pod was released again: the wake of the release before it is over, whatever became of
	// it, and only the new release's wakes measure anything.
	second, _, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer"}, At: at.Add(30 * time.Minute)})
	if err != nil {
		t.Fatalf("record the second release: %v", err)
	}
	unfinished, err := unfinishedResumes(ctx, st, "legion", bound)
	if err != nil || len(unfinished) != 0 {
		t.Fatalf("unfinished wakes after the second release = %+v, %v; want none of the release that is over", unfinished, err)
	}
	if stuck, err := st.StuckPodResumes(ctx, "legion", bound); err != nil || len(stuck) != 1 {
		t.Fatalf("the stuck pass reads %+v, %v; want the unfinished wake it has still to warn about", stuck, err)
	}

	wake(second, "tester", at.Add(31*time.Minute))
	unfinished, err = unfinishedResumes(ctx, st, "legion", bound)
	if err != nil || len(unfinished) != 1 || unfinished[0].Role != "tester" {
		t.Fatalf("unfinished wakes of the newest release = %+v, %v; want the tester's alone", unfinished, err)
	}
}

// A teardown that gives up on its release's record settles the wakes taken under the mark in its
// place (SettleUnrecordedRelease, R3's bound of dispatch://INFRA-3589), writes no release and
// clears the mark, in one transaction. In the first row no record names the mark: the wake taken
// under it pairs with no release, keeps the newest release it saw, and is dated to the give-up; a
// wake of a role the release never recorded is left as it was; and the mark is gone. The second
// row is the control of the first: a record already names the mark, so the settle answers that,
// writes nothing and leaves the mark, and the wake stays pointed at that record.
func TestAnUnrecordedReleaseSettlesTheWakeUnderItsMarkUnlessARecordNamesIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		recorded bool
	}{
		{name: "no record names the mark"},
		{name: "the control: a record names the mark", recorded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := migratedStore(t)
			ctx := context.Background()
			at := time.Now().UTC().Truncate(time.Millisecond)
			earlier, _, err := store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue,
				Generation: 1, Roles: []string{"implementer"}, At: at.Add(-2 * time.Hour)})
			if err != nil {
				t.Fatalf("record the earlier release: %v", err)
			}
			markRelease(t, store, 1, 1, 1, 0, claim.RoleImplementer)
			var mark int64
			if err := store.Pool().QueryRow(ctx, `select id from issue_releases where issue = $1`, rootIssue).Scan(&mark); err != nil {
				t.Fatalf("read the mark: %v", err)
			}
			take := func(role claim.Role) int64 {
				t.Helper()
				token, err := claim.NewToken("legion", rootIssue, role)
				if err != nil {
					t.Fatal(err)
				}
				if taken, err := store.TakePodWake(ctx, "legion", rootIssue, token, pods.WakeAgentMessage, at.Add(-10*time.Minute), 0); err != nil || !taken {
					t.Fatalf("take the %s's wake = %t, %v; want it taken", role, taken, err)
				}
				id, _, found, err := store.OpenPodResume(ctx, "legion", rootIssue, role)
				if err != nil || !found {
					t.Fatalf("the %s's open resume = %t, %v; want the one its wake recorded", role, found, err)
				}
				return id
			}
			woken := take(claim.RoleImplementer)
			outside := take(claim.RoleArchitect)
			release := int64(0)
			if tc.recorded {
				if release, _, err = store.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
					Roles: []string{"implementer"}, At: at, Release: mark}); err != nil {
					t.Fatalf("record the release: %v", err)
				}
			}

			gaveUp := at.Add(time.Hour)
			recorded, err := store.SettleUnrecordedRelease(ctx, "legion", rootIssue, mark, gaveUp, nil)
			if err != nil || recorded != tc.recorded {
				t.Fatalf("the settle answered recorded %t, %v; want %t", recorded, err, tc.recorded)
			}
			// The settle writes no release: the earlier one stands, and the record where one names
			// the mark.
			wantReleases := 1
			if tc.recorded {
				wantReleases = 2
			}
			var releases int
			if err := store.Pool().QueryRow(ctx, `select count(*) from pod_releases where issue = $1`, rootIssue).Scan(&releases); err != nil ||
				releases != wantReleases {
				t.Fatalf("releases recorded = %d (%v), want %d", releases, err, wantReleases)
			}
			// The settle clears the mark where no record names it. Where one does, the mark stands,
			// and the teardown that runs again reads that record.
			wantMarks := 0
			if tc.recorded {
				wantMarks = 1
			}
			var marks int
			if err := store.Pool().QueryRow(ctx, `select count(*) from issue_releases where issue = $1`, rootIssue).Scan(&marks); err != nil ||
				marks != wantMarks {
				t.Fatalf("release marks after the settle = %d (%v), want %d", marks, err, wantMarks)
			}
			want := []struct {
				what          string
				id            int64
				release, seen int64
				at            time.Time
			}{
				{what: "the wake taken under the mark", id: woken, release: 0, seen: earlier, at: gaveUp},
				{what: "the wake of a role the release never recorded", id: outside, release: earlier, seen: earlier, at: at.Add(-10 * time.Minute)},
			}
			if tc.recorded {
				want[0].release, want[0].seen, want[0].at = release, release, at
			}
			for _, want := range want {
				var release, seen int64
				var resumed, clock time.Time
				if err := store.Pool().QueryRow(ctx, `select release_id, seen_release, at, clock_at from pod_resumes where id = $1`,
					want.id).Scan(&release, &seen, &resumed, &clock); err != nil {
					t.Fatalf("read %s: %v", want.what, err)
				}
				if release != want.release || seen != want.seen || !resumed.Equal(want.at) || !clock.Equal(want.at) {
					t.Errorf("%s has release %d, seen release %d, time %s and stuck clock %s; want %d, %d, and %s for both",
						want.what, release, seen, resumed.UTC().Format(time.RFC3339Nano), clock.UTC().Format(time.RFC3339Nano),
						want.release, want.seen, want.at.Format(time.RFC3339Nano))
				}
			}
		})
	}
}

// R3 of dispatch://INFRA-3589. A project's pause stops the implementer after the release's
// workspace check, and the release's stop then suspends it as `released` and records it on the
// mark. Every record of the release fails, and the teardown gives up on it
// (SettleUnrecordedRelease), which writes no pod release. That release took the pod all
// the same, so the moment it gave up ends what the pause's stop says of the role, as a recorded
// release's time does: before the settle the stop is the pause's, for a wake's words and the pause
// that holds it (StoppingPauseTx) and for the held role message's debt (StoppedByAPause), and after
// it neither read answers a pause. The settle records the give-up of its mark once, at its moment,
// and the newest release a wake pairs with is still the one before it (LatestPodRelease). The
// control is a later stop: the role comes back, and a second pause stops it after the give-up, so
// both reads answer the second pause.
//
// The negative control is the settle without its record of the give-up (recordReleaseGiveUp):
// both reads still answer the first pause after the settle.
func TestAReleaseThatGaveUpOnItsRecordEndsThePausesStopBeforeIt(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	rootClaim(t, st, claim.RoleImplementer, supervise.StateIdle, "")
	rootClaim(t, st, claim.RoleArchitect, supervise.StateIdle, "")
	at := time.Now().UTC().Truncate(time.Microsecond)
	earlier, _, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{string(claim.RoleImplementer)}, At: at.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatalf("record the earlier release: %v", err)
	}
	pauseOf := func(began, ended time.Time) int64 {
		t.Helper()
		var id int64
		if err := st.Pool().QueryRow(ctx, `insert into project_pauses (project, began_at, by_kind, reason, ended_at, ended_by_kind, ended_by_session)
			values ('LEGION', $1, 'operator', 'a pause of the test', $2, 'operator', '') returning id`, began, ended).Scan(&id); err != nil {
			t.Fatalf("record a pause: %v", err)
		}
		return id
	}
	stop := func(pause int64, ended time.Time) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, `insert into project_pause_stops (pause_id, project, claim, issue, role, asked_at, ended_at, ended_as)
			values ($1, 'LEGION', $2, $3, 'implementer', $4, $4, $5)`, pause, string(releasedToken), rootIssue, ended, string(PauseStopStopped)); err != nil {
			t.Fatalf("record pause %d's stop of the implementer: %v", pause, err)
		}
	}
	// reads are what the two reads of a pause's stop answer for the implementer, and the newest
	// release a wake of it pairs with.
	reads := func() (stopping, owed, latest int64) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) error {
			var err error
			if stopping, err = StoppingPauseTx(ctx, tx, "legion", rootIssue, releasedToken); err != nil {
				return err
			}
			latest, err = LatestPodRelease(ctx, tx, "legion", rootIssue)
			return err
		})
		owed, err := st.StoppedByAPause(ctx, "legion", rootIssue, releasedToken)
		if err != nil {
			t.Fatalf("read the pause the held role message's debt names: %v", err)
		}
		return stopping, owed, latest
	}

	first := pauseOf(at.Add(-30*time.Minute), at.Add(-20*time.Minute))
	stop(first, at.Add(-25*time.Minute))
	boundary, _, _ := releaseTwoRolesThroughTheStore(t, st)
	stoppedByTheRelease(t, st, claim.RoleImplementer, boundary)
	var mark int64
	if err := st.Pool().QueryRow(ctx, `select id from issue_releases where issue = $1`, rootIssue).Scan(&mark); err != nil {
		t.Fatalf("read the release's mark: %v", err)
	}
	if stopping, owed, _ := reads(); stopping != first || owed != first {
		t.Fatalf("before the give-up StoppingPauseTx = %d and StoppedByAPause = %d, want pause %d for both: the case's premise",
			stopping, owed, first)
	}

	if recorded, err := st.SettleUnrecordedRelease(ctx, "legion", rootIssue, 0, at, nil); err != nil || recorded {
		t.Fatalf("SettleUnrecordedRelease = recorded %t, %v; want the give-up settled", recorded, err)
	}
	if stopping, owed, latest := reads(); stopping != 0 || owed != 0 || latest != earlier {
		t.Errorf("after the give-up StoppingPauseTx = %d, StoppedByAPause = %d and LatestPodRelease = %d; want no pause for either and the earlier release %d: the release that gave up took the pod and recorded no release",
			stopping, owed, latest, earlier)
	}
	var gaveUp []string
	rows, err := st.Pool().Query(ctx, `select issue_release_id::text || ' at ' || to_char(at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')
		from pod_release_give_ups where project = 'legion' and issue = $1 order by id`, rootIssue)
	if err != nil {
		t.Fatalf("read the give-ups of the release: %v", err)
	}
	if gaveUp, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatalf("read the give-ups of the release: %v", err)
	}
	if want := fmt.Sprintf("%d at %s", mark, at.Format("2006-01-02T15:04:05.000000")); len(gaveUp) != 1 || gaveUp[0] != want {
		t.Errorf("the give-ups of the release = %q, want one, %q: the mark the settle cleared, at its moment", gaveUp, want)
	}

	if _, err := st.Pool().Exec(ctx, `update claims set last_ready = $2 where token = $1`, string(releasedToken), at.Add(10*time.Minute)); err != nil {
		t.Fatalf("bring the implementer back: %v", err)
	}
	second := pauseOf(at.Add(20*time.Minute), at.Add(40*time.Minute))
	stop(second, at.Add(30*time.Minute))
	if stopping, owed, _ := reads(); stopping != second || owed != second {
		t.Errorf("control: after a second pause stopped the role again, StoppingPauseTx = %d and StoppedByAPause = %d, want pause %d for both",
			stopping, owed, second)
	}
}
