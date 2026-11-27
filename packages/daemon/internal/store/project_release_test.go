package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// releaseStart and releaseBound are the start and the bound every release of this file records,
// and releaseLength the bound a start of one is given, from releaseStart to releaseBound.
var (
	releaseStart  = time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	releaseBound  = releaseStart.Add(releaseLength)
	releaseLength = 30 * time.Minute
)

// atReleaseStart is the clock a release of this file is started on: it reads releaseStart every
// time, so each run of the start records the same start.
func atReleaseStart() time.Time { return releaseStart }

// seedProjectClaim writes one claim of project in state, suspended by by when it is suspended,
// with a session of its own. The controller's claim is on no issue and no tree.
func seedProjectClaim(t *testing.T, st *Store, project, issue string, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource) claim.Token {
	t.Helper()
	token := claim.ControllerToken(project)
	tree := ""
	if role != claim.RoleController {
		var err error
		if token, err = claim.NewToken(project, issue, role); err != nil {
			t.Fatal(err)
		}
		tree = "LEGION-208"
	}
	if err := st.PutClaim(context.Background(), supervise.Claim{Token: token, Project: project, Tree: tree, Issue: issue,
		Role: role, TreeEpoch: 1, Generation: 1, Session: "ses-" + string(token), State: state, SuspendedBy: by}); err != nil {
		t.Fatalf("seed the claim %s: %v", token, err)
	}
	return token
}

// queueReleaseRow queues one outbox row of issue and answers its id.
func queueReleaseRow(t *testing.T, st *Store, issue, kind, payload string) int64 {
	t.Helper()
	var id int64
	if err := st.Pool().QueryRow(context.Background(), `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, $2, $3, 0, now(), '') returning id`, kind, issue, payload).Scan(&id); err != nil {
		t.Fatalf("queue the %s row of %s: %v", kind, issue, err)
	}
	return id
}

// markIdleRelease writes the mark an idle release of issue in project leaves, over the rows first
// through last, of kind with the roles its stops have suspended so far.
func markIdleRelease(t *testing.T, st *Store, project, issue, kind string, first, last int64, suspended ...string) {
	t.Helper()
	role := "architect"
	if kind == releaseKindUnowned {
		role = ""
	}
	if suspended == nil {
		suspended = []string{}
	}
	if _, err := st.Pool().Exec(context.Background(), `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, suspended, kind, marked_at)
		values ($1, $2, 3, $3, $3, $4, 0, $5, $6, $7, now())`, project, issue, first, last, role, suspended, kind); err != nil {
		t.Fatalf("mark the %s release of %s: %v", kind, issue, err)
	}
}

// outboxIDs is every outbox row's id, in id order.
func outboxIDs(t *testing.T, st *Store) []int64 {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `select id from outbox order by id`)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read the outbox: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	return ids
}

// faultedStore is st's database through a commit-fault proxy, opened as a store of its own: the
// start runs on it, and st reads what committed.
func faultedStore(t *testing.T, st *Store) (*Store, *testdb.CommitFault) {
	t.Helper()
	dsn, fault := testdb.CommitFaults(t, st.Pool().Config().ConnString())
	faulted, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store through the fault proxy: %v", err)
	}
	t.Cleanup(faulted.Close)
	return faulted, fault
}

// A project release's record is written in one transaction with the two things the release reads
// from that same moment, and this case reads all three answers.
//
// The record: the release's id, its project, its start and its bound, unfinished and unended,
// with the empty result.
//
// The take-over of every idle release of the project in flight: each mark goes, and the rows of
// that release still queued — the workspace check, the role stops and the teardown — are finished
// without acting, while a delivery the issue holds inside the mark's range and a start queued after
// it stand, and a claim the idle release had already suspended stays `released`. The answer names
// each release with its issue, its generation, its kind (a quiet pod's, or one no role can use) and
// the roles it had stopped. The queued workspace check of a left pod's close, which writes no mark,
// is finished too. Another project's idle release and close check are none of this release's,
// though a claim of this project names that project's issue (the operator's spawn of it): only that
// project's daemon runs the issue's rows (claim.KeyOfProject). A release of this project on another
// project's issue, marked before #4115 refused a row keyed so, is taken over whole, its workspace
// check with it, though that check's key is not the project's.
//
// The stop set: every claim of the project stored in a state whose process is up or coming up,
// launching through idle, the controller's and the one on another project's issue among them, with
// its tree, issue, role, session and state, in token order — and none that is queued, suspended,
// failed or retired, and none of another project.
func TestStartProjectReleaseTakesOverTheIdleReleasesAndAnswersItsStopSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := migratedStore(t)

	running := []struct {
		issue string
		role  claim.Role
		state supervise.ClaimState
	}{
		{"LEGION-208", claim.RoleArchitect, supervise.StateIdle},
		{"LEGION-208", claim.RoleImplementer, supervise.StateWorking},
		{"LEGION-209", claim.RoleTester, supervise.StateLaunching},
		{"LEGION-209", claim.RoleArchitect, supervise.StateShimConnected},
		{"LEGION-210", claim.RolePlanner, supervise.StateRegistered},
		{"LEGION-211", claim.RoleImplementer, supervise.StateReady},
		{"", claim.RoleController, supervise.StateReady},
		// The operator's spawn of another project's issue, made under this project's token: a claim
		// of the project, so the release stops it, whatever issue it names.
		{"OTHER-1", claim.RoleTester, supervise.StateWorking},
	}
	want := map[claim.Token]ProjectReleaseClaim{}
	for _, c := range running {
		token := seedProjectClaim(t, st, "legion", c.issue, c.role, c.state, "")
		tree := "LEGION-208"
		if c.role == claim.RoleController {
			tree = ""
		}
		want[token] = ProjectReleaseClaim{Token: token, Tree: tree, Issue: c.issue, Role: c.role, Session: "ses-" + string(token), State: c.state}
	}
	released := seedProjectClaim(t, st, "legion", "LEGION-209", claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased)
	seedProjectClaim(t, st, "legion", "LEGION-210", claim.RoleMerger, supervise.StateFailed, "")
	seedProjectClaim(t, st, "legion", "LEGION-210", claim.RoleTester, supervise.StateQueued, "")
	seedProjectClaim(t, st, "legion", "LEGION-211", claim.RoleReviewer, supervise.StateRetired, "")
	seedProjectClaim(t, st, "legion", "LEGION-211", claim.RoleArchitect, supervise.StateSuspended, supervise.SuspendedByOperator)
	seedProjectClaim(t, st, "other", "OTHER-1", claim.RoleImplementer, supervise.StateWorking, "")

	// An idle release of LEGION-209 whose stop of the reviewer has run: its workspace check, the
	// reviewer's stop, a notice another transaction numbered inside its range, the tester's stop
	// and the teardown. A start of LEGION-209 queued after it is none of its rows.
	check := queueReleaseRow(t, st, "LEGION-209", "issue_bundle", `{"tree":"LEGION-208","generation":3,"treeGeneration":1,"role":"tester"}`)
	stopReviewer := queueReleaseRow(t, st, "LEGION-209", "supervise", `{"op":"release","tree":"LEGION-208","role":"reviewer","generation":3}`)
	notice := queueReleaseRow(t, st, "LEGION-209", "notice", `{}`)
	stopTester := queueReleaseRow(t, st, "LEGION-209", "supervise", `{"op":"release","tree":"LEGION-208","role":"tester","generation":3}`)
	teardown := queueReleaseRow(t, st, "LEGION-209", "issue_release", `{"tree":"LEGION-208","generation":3,"treeGeneration":1}`)
	markIdleRelease(t, st, "legion", "LEGION-209", releaseKindIdle, check, teardown, "reviewer")
	start := queueReleaseRow(t, st, "LEGION-209", "supervise", `{"op":"start","tree":"LEGION-208","role":"tester","generation":3}`)
	// The release of LEGION-210's pod, which no role can use: its teardown alone.
	unowned := queueReleaseRow(t, st, "LEGION-210", "issue_release", `{"tree":"LEGION-208","generation":3,"treeGeneration":1}`)
	markIdleRelease(t, st, "legion", "LEGION-210", releaseKindUnowned, unowned, unowned)
	// An idle release of OTHER-2 under this project's token (the operator's spawn of another
	// project's issue), written before #4115, since which insertOutboxRow refuses a row keyed by
	// another project's issue: its workspace check and its teardown. The check's key is not the
	// project's, so only the delete of the mark's own rows finishes it.
	otherCheck := queueReleaseRow(t, st, "OTHER-2", "issue_bundle", `{"tree":"OTHER-2","generation":3,"treeGeneration":1,"role":"architect"}`)
	otherTeardown := queueReleaseRow(t, st, "OTHER-2", "issue_release", `{"tree":"OTHER-2","generation":3,"treeGeneration":1}`)
	markIdleRelease(t, st, "legion", "OTHER-2", releaseKindIdle, otherCheck, otherTeardown)
	// Another project's idle release.
	otherRow := queueReleaseRow(t, st, "OTHER-1", "issue_release", `{"tree":"OTHER-1","generation":1,"treeGeneration":1}`)
	markIdleRelease(t, st, "other", "OTHER-1", releaseKindUnowned, otherRow, otherRow)
	// The workspace check of the close of LEGION-211's pod, whose issue left the workflow, and one
	// of another project's: a close writes no mark (the daemon's leftPodCloseRows).
	closeCheck := queueReleaseRow(t, st, "LEGION-211", "issue_bundle",
		`{"tree":"LEGION-208","generation":2,"treeGeneration":1,"role":"implementer","closeRoles":["implementer"]}`)
	otherClose := queueReleaseRow(t, st, "OTHER-1", "issue_bundle",
		`{"tree":"OTHER-1","generation":1,"treeGeneration":1,"role":"implementer","closeRoles":["implementer"]}`)

	release, takenOver, stop, err := st.StartProjectRelease(ctx, "legion", atReleaseStart, releaseLength)
	if err != nil {
		t.Fatalf("StartProjectRelease: %v", err)
	}

	t.Run("the record", func(t *testing.T) {
		if release.ID <= 0 || release.Project != "legion" || !release.StartedAt.Equal(releaseStart) || !release.BoundAt.Equal(releaseBound) {
			t.Fatalf("the record = %+v, want the project's release started at %s and bound at %s", release, releaseStart, releaseBound)
		}
		if !release.FinishedAt.IsZero() || !release.EndedAt.IsZero() || release.EndedBy != "" || string(release.Result) != "{}" {
			t.Fatalf("the record = %+v, want it unfinished, unended and with the empty result", release)
		}
		read, found, err := st.ReadProjectRelease(ctx, "legion")
		if err != nil || !found || read.ID != release.ID {
			t.Fatalf("ReadProjectRelease = %+v, %t, %v; want the release just recorded", read, found, err)
		}
	})

	t.Run("the take-over of the idle releases in flight", func(t *testing.T) {
		// The release of the pod no role can use (LEGION-210) suspended no role, and OTHER-2's,
		// whose check is still queued, has stopped none yet, so neither names one.
		wantTaken := []TakenOverIdleRelease{
			{Issue: "LEGION-209", Suspended: []claim.Role{claim.RoleReviewer}},
			{Issue: "LEGION-210"},
			{Issue: "OTHER-2"},
		}
		if len(takenOver) != len(wantTaken) {
			t.Fatalf("taken over %+v, want %+v", takenOver, wantTaken)
		}
		for i := range wantTaken {
			if got, want := takenOver[i], wantTaken[i]; got.Issue != want.Issue || !slices.Equal(got.Suspended, want.Suspended) {
				t.Fatalf("taken over %+v, want %+v", takenOver, wantTaken)
			}
		}
		var marks []string
		rows, err := st.Pool().Query(ctx, `select project || '/' || issue from issue_releases order by project, issue`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var mark string
			if err := rows.Scan(&mark); err != nil {
				t.Fatal(err)
			}
			marks = append(marks, mark)
		}
		rows.Close()
		if !slices.Equal(marks, []string{"other/OTHER-1"}) {
			t.Fatalf("marks after the take-over = %v, want the other project's alone", marks)
		}
		if got, wantIDs := outboxIDs(t, st), []int64{notice, start, otherRow, otherClose}; !slices.Equal(got, wantIDs) {
			t.Fatalf("outbox rows after the take-over = %v, want the notice %d, the later start %d and the other project's rows %d and %d (the release's rows %d, %d, %d, %d and %d, OTHER-2's check %d and teardown %d and the close check %d gone)",
				got, notice, start, otherRow, otherClose, check, stopReviewer, stopTester, teardown, unowned, otherCheck, otherTeardown, closeCheck)
		}
		var by string
		if err := st.Pool().QueryRow(ctx, `select suspended_by from claims where token = $1`, string(released)).Scan(&by); err != nil || by != "released" {
			t.Fatalf("the reviewer the idle release stopped reads suspended by %q (%v), want it left released", by, err)
		}
	})

	t.Run("the stop set", func(t *testing.T) {
		if len(stop) != len(want) {
			t.Fatalf("stop set = %+v, want the %d running or starting claims of the project", stop, len(want))
		}
		if !slices.IsSortedFunc(stop, func(a, b ProjectReleaseClaim) int { return strings.Compare(string(a.Token), string(b.Token)) }) {
			t.Fatalf("stop set = %+v, want it in token order", stop)
		}
		for _, got := range stop {
			if got != want[got.Token] {
				t.Fatalf("stop set member %+v, want %+v", got, want[got.Token])
			}
		}
	})
}

// A release's start refuses a release with no project, one with no clock to read its start from
// and one bound before it starts, and writes nothing for any of them.
func TestStartProjectReleaseRefusesAReleaseItCouldNotCarryOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := migratedStore(t)
	if _, _, _, err := st.StartProjectRelease(ctx, "", atReleaseStart, releaseLength); err == nil {
		t.Fatal("a release with no project was recorded")
	}
	if _, _, _, err := st.StartProjectRelease(ctx, "legion", nil, releaseLength); err == nil {
		t.Fatal("a release with no clock was recorded")
	}
	if _, _, _, err := st.StartProjectRelease(ctx, "legion", atReleaseStart, -time.Second); err == nil {
		t.Fatal("a release bound before its start was recorded")
	}
	if _, found, err := st.ReadProjectRelease(ctx, "legion"); err != nil || found {
		t.Fatalf("ReadProjectRelease after three refusals = %t, %v; want no release", found, err)
	}
}

// A release's start runs until one run commits (transactUntilCommitted). A run whose commit landed
// while its reply was lost, and whose outcome the store could not say until it answered again,
// answers nil once the store says it committed, with that run's record, take-over and stop set,
// and its record is the project's one. The negative control is plain Transact: the start answers
// ErrCommitOutcomeUnknown.
func TestAProjectReleaseStartWhoseCommitReplyIsLostAnswersTheRunThatCommitted(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	token := seedProjectClaim(t, st, "legion", "LEGION-208", claim.RoleImplementer, supervise.StateWorking, "")
	teardown := queueReleaseRow(t, st, "LEGION-210", "issue_release", `{"tree":"LEGION-208","generation":3,"treeGeneration":1}`)
	markIdleRelease(t, st, "legion", "LEGION-210", releaseKindUnowned, teardown, teardown)
	faulted, fault := faultedStore(t, st)

	// The proxy refuses every connection once the reply is lost, so the commit's own read of the
	// outcome fails, and passes them again two seconds later, while the start still waits.
	fired := fault.Cut("insert into project_releases", testdb.CutAfterCommitRefusing)
	fault.PassAgainAfter(fired)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, takenOver, stop, err := faulted.StartProjectRelease(ctx, "legion", atReleaseStart, releaseLength)
	testdb.Met(t, fired)
	if err != nil {
		t.Fatalf("the start whose commit landed and whose reply was lost = %v, want nil", err)
	}
	read, found, err := st.ReadProjectRelease(context.Background(), "legion")
	if err != nil || !found || read.ID != release.ID || !read.Runs() {
		t.Fatalf("the project's newest release = %+v, %t, %v; want the running release %d the start answered", read, found, err, release.ID)
	}
	var records int
	if err := st.Pool().QueryRow(context.Background(), `select count(*) from project_releases where project = 'legion'`).Scan(&records); err != nil || records != 1 {
		t.Fatalf("the project holds %d release records (%v), want the one of the run that committed", records, err)
	}
	if len(takenOver) != 1 || takenOver[0].Issue != "LEGION-210" || len(takenOver[0].Suspended) != 0 {
		t.Fatalf("taken over %+v, want the release of LEGION-210's pod, which stopped no role", takenOver)
	}
	want := ProjectReleaseClaim{Token: token, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer,
		Session: "ses-" + string(token), State: supervise.StateWorking}
	if len(stop) != 1 || stop[0] != want {
		t.Fatalf("stop set = %+v, want %+v alone", stop, want)
	}
}

// A release's start whose first run did not commit runs again (transactUntilCommitted), and each
// run reads its start from the clock again: the record holds the start the run that committed
// read, and the whole bound counted from it, so a run that did not commit takes nothing of the time
// the asked roles have to push their work. Here the first run's commit never reaches the server,
// which rolls it back, and the clock has moved on a minute when the second run reads it. The
// negative control is a start read once before the first run: the record then holds the first
// run's start, a minute before the run that committed.
func TestAProjectReleaseStartRunAgainCountsItsBoundFromTheRunThatCommitted(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	faulted, fault := faultedStore(t, st)

	// The clock moves on a minute at each read, so the start a run reads tells which run it was.
	var reads []time.Time
	clock := func() time.Time {
		at := releaseStart.Add(time.Duration(len(reads)) * time.Minute)
		reads = append(reads, at)
		return at
	}
	fired := fault.Cut("insert into project_releases", testdb.CutBeforeCommit)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, _, _, err := faulted.StartProjectRelease(ctx, "legion", clock, releaseLength)
	testdb.Met(t, fired)
	if err != nil {
		t.Fatalf("the start whose first run's commit never reached the server = %v, want nil", err)
	}
	read, found, err := st.ReadProjectRelease(context.Background(), "legion")
	if err != nil || !found || read.ID != release.ID {
		t.Fatalf("the project's newest release = %+v, %t, %v; want the release %d the start answered", read, found, err, release.ID)
	}
	committed := releaseStart.Add(time.Minute)
	for _, got := range []struct {
		name    string
		release ProjectRelease
	}{{"the start's answer", release}, {"the record", read}} {
		if !got.release.StartedAt.Equal(committed) {
			t.Fatalf("%s started at %s, want %s, the start the run that committed read (the clock read %v)",
				got.name, got.release.StartedAt, committed, reads)
		}
		if bound := got.release.BoundAt.Sub(got.release.StartedAt); bound != releaseLength {
			t.Fatalf("%s is bound %s after its start, want the whole bound of %s", got.name, bound, releaseLength)
		}
	}
	var records int
	if err := st.Pool().QueryRow(context.Background(), `select count(*) from project_releases where project = 'legion'`).Scan(&records); err != nil || records != 1 {
		t.Fatalf("the project holds %d release records (%v), want the one of the run that committed", records, err)
	}
}

// A release's record grows as it acts, finishes once with its last result, and is ended by the
// next boot of its project's daemon and by nothing else: the newest release of the project is
// what a read answers, a write after the finish is refused, and a boot ends every release of its
// project that no boot ended and none of another project's.
func TestAProjectReleasesRecordGrowsFinishesAndEndsAtTheBoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := migratedStore(t)
	if _, found, err := st.ReadProjectRelease(ctx, "legion"); err != nil || found {
		t.Fatalf("ReadProjectRelease of a project that never released = %t, %v; want none", found, err)
	}
	earlier, _, _, err := st.StartProjectRelease(ctx, "legion", func() time.Time { return releaseStart.Add(-time.Hour) }, releaseLength)
	if err != nil {
		t.Fatal(err)
	}
	other, _, _, err := st.StartProjectRelease(ctx, "other", atReleaseStart, releaseLength)
	if err != nil {
		t.Fatal(err)
	}
	release, _, _, err := st.StartProjectRelease(ctx, "legion", atReleaseStart, releaseLength)
	if err != nil {
		t.Fatal(err)
	}

	growing := json.RawMessage(`{"sandboxes":[{"name":"legion-legion-208","outcome":"deleted_clean"}]}`)
	if err := st.RecordProjectRelease(ctx, release.ID, growing); err != nil {
		t.Fatalf("RecordProjectRelease: %v", err)
	}
	read, found, err := st.ReadProjectRelease(ctx, "legion")
	if err != nil || !found || read.ID != release.ID || !sameJSON(t, read.Result, growing) || !read.FinishedAt.IsZero() {
		t.Fatalf("the newest release after a record = %+v, %t, %v; want release %d running with the result recorded", read, found, err, release.ID)
	}

	finishedAt := releaseStart.Add(12 * time.Minute)
	last := json.RawMessage(`{"sandboxes":[{"name":"legion-legion-208","outcome":"deleted_clean"}],"claims":[]}`)
	if err := st.FinishProjectRelease(ctx, release.ID, last, finishedAt); err != nil {
		t.Fatalf("FinishProjectRelease: %v", err)
	}
	read, _, err = st.ReadProjectRelease(ctx, "legion")
	if err != nil || !read.FinishedAt.Equal(finishedAt) || !sameJSON(t, read.Result, last) || !read.EndedAt.IsZero() {
		t.Fatalf("the release after its finish = %+v, %v; want it finished at %s with its last result and unended", read, err, finishedAt)
	}
	if err := st.RecordProjectRelease(ctx, release.ID, growing); err == nil {
		t.Fatal("a result recorded after the finish was taken")
	}
	if err := st.FinishProjectRelease(ctx, release.ID, last, finishedAt); !errors.Is(err, ErrNoUnfinishedProjectRelease) {
		t.Fatalf("a second finish = %v, want it refused as no unfinished release", err)
	}
	if err := st.RecordProjectRelease(ctx, release.ID+100, growing); err == nil {
		t.Fatal("a result of a release no row holds was taken")
	}

	bootAt := releaseStart.Add(time.Hour)
	ended, err := st.EndProjectReleasesAtBoot(ctx, "legion", bootAt)
	if err != nil || ended != 2 {
		t.Fatalf("EndProjectReleasesAtBoot = %d, %v; want both of the project's releases ended", ended, err)
	}
	read, _, err = st.ReadProjectRelease(ctx, "legion")
	if err != nil || read.ID != release.ID || !read.EndedAt.Equal(bootAt) || read.EndedBy != ProjectReleaseEndedByBoot || !sameJSON(t, read.Result, last) {
		t.Fatalf("the release after the boot = %+v, %v; want it ended by the boot at %s with its result kept", read, err, bootAt)
	}
	var earlierEnded ProjectReleaseEnder
	if err := st.Pool().QueryRow(ctx, `select coalesce(ended_by, '') from project_releases where id = $1`, earlier.ID).Scan(&earlierEnded); err != nil || earlierEnded != ProjectReleaseEndedByBoot {
		t.Fatalf("the earlier release, which never finished, reads ended by %q (%v), want the boot", earlierEnded, err)
	}
	if ended, err := st.EndProjectReleasesAtBoot(ctx, "legion", bootAt.Add(time.Hour)); err != nil || ended != 0 {
		t.Fatalf("a second boot ended %d, %v; want none, since the first boot ended both", ended, err)
	}
	read, _, err = st.ReadProjectRelease(ctx, "legion")
	if err != nil || !read.EndedAt.Equal(bootAt) {
		t.Fatalf("the release after a second boot = %+v, %v; want the first boot's end kept", read, err)
	}
	theOther, _, err := st.ReadProjectRelease(ctx, "other")
	if err != nil || theOther.ID != other.ID || !theOther.EndedAt.IsZero() || theOther.EndedBy != "" {
		t.Fatalf("the other project's release after this project's boot = %+v, %v; want it running, unended", theOther, err)
	}
}

// dispatch://INFRA-3583 R13: a pause that holds the project refuses a project release before it
// records anything. While the project is paused, the refusal names the pause that stands and each
// pod a pause holds, with the claims of the pod stored suspended by `released` in token order. Once
// the resume has ended the pause, a pod whose hold still stands — no role of it has run again —
// still refuses the release, naming that pod and no pause. Once that hold has ended at a role's
// ready, the release is recorded. The refusal reads alike on the pool, where the release asks it
// before it sets its mark, and in the release's own transaction, which then records nothing: no
// release, and no take-over of the idle release in flight.
func TestAProjectReleaseIsRefusedWhileAPauseHoldsItsProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := migratedStore(t)
	seedPauseClaims(t, st)
	paused, _ := pauseLegion(t, st, pausedAt)
	// The pause's stop of LEGION-208's implementer, as its stop transaction leaves it: the claim
	// stored suspended by `released` beside the architect an idle release left so, and its pod
	// held.
	implementer := claim.Token("legion-legion-legion-208-implementer")
	if err := st.PutClaim(ctx, supervise.Claim{Token: implementer, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleImplementer, Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased}); err != nil {
		t.Fatal(err)
	}
	heldAt := pausedAt.Add(time.Second)
	if opened, err := holdPod(st, paused, "LEGION-208", heldAt); err != nil || !opened {
		t.Fatalf("hold the pod of LEGION-208 = %t, %v; want it held", opened, err)
	}
	markIdleRelease(t, st, "legion", "LEGION-210", releaseKindIdle, 1, 1)
	held := HeldPausePod{Issue: "LEGION-208", Pause: paused.ID, HeldAt: heldAt,
		Claims: []claim.Token{"legion-legion-legion-208-architect", implementer}}

	refuses := func(when string, wantPaused bool) {
		t.Helper()
		for _, read := range []struct {
			name string
			err  func() error
		}{
			{"the read on the pool", func() error { return st.PauseRefusesProjectRelease(ctx, "legion") }},
			{"the release's transaction", func() error {
				_, _, _, err := st.StartProjectRelease(ctx, "legion", atReleaseStart, releaseLength)
				return err
			}},
		} {
			var refused *ProjectPausedError
			if err := read.err(); !errors.As(err, &refused) {
				t.Fatalf("%s %s = %v, want a *ProjectPausedError", when, read.name, err)
			}
			if refused.Paused != wantPaused || wantPaused && refused.Pause.ID != paused.ID || len(refused.Held) != 1 ||
				refused.Held[0].Issue != held.Issue || refused.Held[0].Pause != held.Pause || !refused.Held[0].HeldAt.Equal(held.HeldAt) ||
				!slices.Equal(refused.Held[0].Claims, held.Claims) {
				t.Fatalf("%s %s refused %+v, want paused %t (pause %d) and LEGION-208's hold %+v", when, read.name, refused, wantPaused, paused.ID, held)
			}
		}
		if n := pauseCount(t, st, `select count(*) from project_releases`); n != 0 {
			t.Fatalf("%s the refused release left %d records, want none", when, n)
		}
		if n := pauseCount(t, st, `select count(*) from issue_releases where issue = 'LEGION-210'`); n != 1 {
			t.Fatalf("%s the refused release took over the idle release in flight (%d marks left), want it untouched", when, n)
		}
	}
	refuses("while the project is paused,", true)

	if _, err := st.Resume(ctx, ResumeRequest{Project: "LEGION", By: PauseActor{Kind: PauseByOperator}, At: pausedAt.Add(time.Minute)}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	refuses("after the resume, with the pod still held,", false)

	if ended, err := st.EndPausePodHold(ctx, "LEGION", "LEGION-208", PausePodHoldReady, pausedAt.Add(2*time.Minute)); err != nil || !ended {
		t.Fatalf("end the hold of LEGION-208 at a role's ready = %t, %v", ended, err)
	}
	if err := st.PauseRefusesProjectRelease(ctx, "legion"); err != nil {
		t.Fatalf("the read once the last hold ended = %v, want no refusal", err)
	}
	release, _, _, err := st.StartProjectRelease(ctx, "legion", atReleaseStart, releaseLength)
	if err != nil || release.ID == 0 {
		t.Fatalf("the release once the last hold ended = %+v, %v; want it recorded", release, err)
	}
}

// dispatch://INFRA-3583 R13: while a project release runs — recorded, not finished and not ended
// by a boot — a pause is refused with a *ProjectReleasingError naming the release, and writes
// nothing: no pause, no stop and no `pause_stop` row of the live roles. A pause is taken once the
// release has finished, and once a boot ended a release that had not.
func TestAPauseIsRefusedWhileAProjectReleaseRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, end := range []struct {
		name string
		end  func(st *Store, id int64) error
	}{
		{"finished", func(st *Store, id int64) error {
			return st.FinishProjectRelease(ctx, id, json.RawMessage(`{"sandboxes":[],"claims":[]}`), releaseStart.Add(time.Minute))
		}},
		{"ended by a boot", func(st *Store, _ int64) error {
			_, err := st.EndProjectReleasesAtBoot(ctx, "legion", releaseStart.Add(time.Minute))
			return err
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			st := migratedStore(t)
			seedPauseClaims(t, st)
			release, _, _, err := st.StartProjectRelease(ctx, "legion", atReleaseStart, releaseLength)
			if err != nil {
				t.Fatalf("record the release: %v", err)
			}
			_, _, err = st.Pause(ctx, PauseRequest{Project: "LEGION", By: PauseActor{Kind: PauseByOperator},
				Reason: "the model provider is down", At: pausedAt, StopRow: pauseStopRow, Notice: pauseNoticeRow(record.ProjectPausedNotice)})
			var releasing *ProjectReleasingError
			if !errors.As(err, &releasing) || releasing.Release.ID != release.ID || !strings.Contains(err.Error(), "runs") {
				t.Fatalf("a pause while release %d runs = %v, want a *ProjectReleasingError naming it", release.ID, err)
			}
			for _, table := range []string{"project_pauses", "project_pause_stops", "outbox"} {
				if n := pauseCount(t, st, `select count(*) from `+table); n != 0 {
					t.Fatalf("the refused pause left %d rows of %s, want none", n, table)
				}
			}

			if err := end.end(st, release.ID); err != nil {
				t.Fatalf("end the release: %v", err)
			}
			pause, stops := pauseLegion(t, st, pausedAt.Add(time.Hour))
			if pause.ID == 0 || len(stops) != 2 {
				t.Fatalf("the pause once the release %s = %+v asking %d stops, want it taken and the two live roles asked", end.name, pause, len(stops))
			}
		})
	}
}

// sameJSON is whether got and want are the same JSON value, whatever the spacing and key order
// the database gives back.
func sameJSON(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatalf("decode %s: %v", want, err)
	}
	encodedA, _ := json.Marshal(a)
	encodedB, _ := json.Marshal(b)
	return string(encodedA) == string(encodedB)
}
