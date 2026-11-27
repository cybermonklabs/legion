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
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The idle view is one read per issue and it reads the four activity columns from the store, not
// from a machine: a report that arrived between a machine's writes is exactly the fact the rule
// turns on, and putClaim never carries it. It also reads the pending delivery's turn marks, the
// claims' states and contracts, the rows queued for a role of the pod, and the issue's place in
// the workflow beside its root's.
func TestTheIdleViewReadsTheStoredActivityReportAndTheQueuedRows(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	now := time.Now().UTC().Truncate(time.Millisecond)

	token := claim.Token("legion-legion-legion-208-implementer")
	c := supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateIdle, PluginContract: 47}
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into pending_task_deliveries
		(claim_token, delivery_id, task, queued_at, delivered_at, confirmed_at, phase, generation)
		values ($1, 'd-1', 'implement it', $2, $3, $4, 'implementing', 1)`,
		string(token), now.Add(-time.Hour), now.Add(-50*time.Minute), now.Add(-45*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The report arrives the way the route sends it, after the machine's own last write.
	reminder := now.Add(-5 * time.Minute)
	reported := claim.SessionActivity{
		At:             now.Add(-40 * time.Minute),
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job", "subagent"}},
		Phase:          claim.PhaseActivity{NextReminderAt: &reminder},
	}
	if wrote, err := st.PutClaimActivity(ctx, token, c.Generation, reported); err != nil || !wrote {
		t.Fatalf("PutClaimActivity = %t, %v", wrote, err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error) values
		(31, 'supervise', $1, '{"op":"start","tree":"LEGION-208","role":"tester","generation":1}', 0, now(), ''),
		(32, 'dispatch_status', $1, '{"status":"testing","observedStatus":"in_progress"}', 0, now(), '')`, rootIssue); err != nil {
		t.Fatal(err)
	}

	view := idleViewOf(t, st, rootIssue)
	if !view.Found || view.Tree != rootIssue || view.Phase != phase.Implementing || view.Generation != 1 || view.Status != "in_progress" {
		t.Fatalf("view = %+v, want the running issue", view)
	}
	if !view.IsRoot() || view.Root.AwaitingPerson || view.Root.LingerUntil != nil {
		t.Fatalf("root = %+v, want the running root", view.Root)
	}
	if len(view.Claims) != 1 {
		t.Fatalf("claims = %+v, want the one claim", view.Claims)
	}
	got := view.Claims[0]
	if got.Role != claim.RoleImplementer || got.State != supervise.StateIdle || got.PluginContract != 47 || got.ServingGeneration != 1 {
		t.Fatalf("claim = %+v, want the stored implementer", got)
	}
	if !got.Live() {
		t.Fatalf("claim %+v reads as not live", got)
	}
	if !got.ActivityAt.Equal(reported.At) || !got.ActivityBackground ||
		len(got.ActivityKinds) != 2 || got.ActivityKinds[0] != "async-job" || got.ActivityKinds[1] != "subagent" ||
		!got.ActivityNextReminderAt.Equal(reminder) {
		t.Fatalf("activity = %+v, want the session's own report", got)
	}
	if !got.LastTurnAt.Equal(now.Add(-45 * time.Minute)) {
		t.Fatalf("last turn = %s, want the delivery's confirmation %s", got.LastTurnAt, now.Add(-45*time.Minute))
	}
	if claim.ReadSessionWork(got.ActivityAt, got.LastTurnAt, got.ActivityBackground) == claim.WorkUnreported {
		t.Fatalf("claim %+v reads as unreported, though its report is newer than its turn", got)
	}
	if since, ok := got.ActivityAt, got.UpdatedAt.IsZero(); ok || got.UpdatedAt.Before(since) {
		t.Fatalf("updated_at = %s, want it at or after the report at %s", got.UpdatedAt, since)
	}
	// The start waits for a role of the pod; the Dispatch status write reaches no session, so
	// the view would read nothing waiting without it.
	if !view.Waiting {
		t.Fatal("the view reads nothing waiting, though a start of the pod's tester is queued")
	}
	if view.Releasing {
		t.Fatal("the view reports a release in progress with no mark written")
	}

	markRelease(t, st, 30, 31, 32, 0)
	if marked := idleViewOf(t, st, rootIssue); !marked.Releasing {
		t.Fatalf("view after the mark = %+v, want it releasing", marked.Releasing)
	}
}

// The view carries three facts beside the rule's own conditions: the release the daemon last
// declined for the issue, with its last check's moment, the newest start row of its claims at that
// check and the pod it read; each claim's newest turn start, which a decline is ended by
// (pods.IdleDecline.Stands); and whether a hold streak of the pod is running, which is what the
// daemon's pass reads before it ends one. The last is in the view because the pass already takes
// this read, and ending streaks a pod does not have would cost a write transaction per pod per
// tick.
func TestTheIdleViewReadsTheDeclinedReleaseTheTurnStartAndTheRunningHoldStreak(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	now := time.Now().UTC().Truncate(time.Millisecond)
	turn := now.Add(-2 * time.Hour)
	if err := st.PutClaim(ctx, supervise.Claim{Token: "legion-legion-legion-208-architect", Project: "legion", Tree: rootIssue,
		Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateIdle, LastStartRow: 12, LastTurnStart: turn}); err != nil {
		t.Fatal(err)
	}

	view := idleViewOf(t, st, rootIssue)
	if view.Declined != (pods.IdleDecline{}) || view.Holding {
		t.Fatalf("view = %+v, want no declined release and no running streak", view)
	}
	if len(view.Claims) != 1 || !view.Claims[0].LastTurnStart.Equal(turn) {
		t.Fatalf("claims = %+v, want the architect's turn start %s", view.Claims, turn)
	}

	markRelease(t, st, 18, 20, 22, 12)
	if declined, err := st.DeclineIssueRelease(ctx, "legion", rootIssue, IdleReleaseDecline{At: now, Role: claim.RoleArchitect,
		Reason: pods.DeclineUnpushedWork, Head: "abc", Commits: 1, PodUID: "pod-a"}, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: now}); err != nil || !declined.Declined {
		t.Fatalf("decline the release of %s = %+v, %v; want it declined", rootIssue, declined, err)
	}
	if err := st.RecordPodHold(ctx, PodHold{Project: "legion", Issue: rootIssue, Reason: "unpushed_work", At: now}); err != nil {
		t.Fatalf("record a hold of %s: %v", rootIssue, err)
	}
	view = idleViewOf(t, st, rootIssue)
	want := pods.IdleDecline{At: now, CheckedAt: now, Reason: pods.DeclineUnpushedWork, StartedRow: 12, PodUID: "pod-a"}
	if !view.Declined.At.Equal(want.At) || !view.Declined.CheckedAt.Equal(want.CheckedAt) || view.Declined.Reason != want.Reason ||
		view.Declined.StartedRow != want.StartedRow || view.Declined.PodUID != want.PodUID || view.PodUID != "" || !view.Holding {
		t.Fatalf("view = %+v, want the decline %+v, no pod of the runtime's (the daemon's pass reads that), and the running streak", view, want)
	}
	if view.Releasing {
		t.Fatal("the view reads a release in progress after its decline deleted the mark")
	}

	if err := st.EndPodHoldStreaks(ctx, "legion", rootIssue); err != nil {
		t.Fatalf("end the hold streaks of %s: %v", rootIssue, err)
	}
	if err := st.ForgetIdleDecline(ctx, "legion", rootIssue); err != nil {
		t.Fatalf("forget the declined release of %s: %v", rootIssue, err)
	}
	if view := idleViewOf(t, st, rootIssue); view.Declined != (pods.IdleDecline{}) || view.Holding {
		t.Fatalf("view = %+v, want the forgotten decline and the ended streak gone from it", view)
	}
}

// The view gives each claim what the reminder hold asks of a claim that works the open phase
// (record.PhaseOwed): the run and the entry the claim serves, by supervise.Claim.ServingAt's rule
// (pods.IdleClaim.ReadServing), and, through the record's reader the daemon hands the store
// (ReadIdleRecordsWith, record.FillIdleClaims), the entry its role's phases row stands on, whether
// that row records a completion, whether a merge forward of the issue's pull request holds the
// role, and whether that pull request is merged while the issue stands in merging. A claim that
// has served no entry, a role with no row and an issue with no pull request read 0 and false. A
// claim that has retired no run and holds a task of the issue's run and phase that its agent
// neither read nor confirmed reads that task's run and entry, where its claims row records run 0.
func TestTheIdleViewReadsTheEntryServedAndTheRecordOfEachClaimsRole(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	st.ReadIdleRecordsWith(func(ctx context.Context, tx pgx.Tx, issue string, at phase.Phase, claims []pods.IdleClaim) error {
		return record.FillIdleClaims(ctx, record.NewStore(), tx, issue, at, claims)
	})
	if _, err := st.Pool().Exec(ctx, `update issues set phase = 'testing' where key = $1`, rootIssue); err != nil {
		t.Fatalf("move %s into testing: %v", rootIssue, err)
	}
	token := claim.Token("legion-legion-legion-208-tester")
	tester := supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleTester, TreeEpoch: 1, Generation: 1, ServingGeneration: 1, State: supervise.StateIdle}
	if err := st.PutClaim(ctx, tester); err != nil {
		t.Fatal(err)
	}
	if got := idleViewOf(t, st, rootIssue).Claims[0]; got.ServingEntry != 0 || got.RowEntry != 0 || got.RowCompleted ||
		got.MergeForwardHeld || got.MergedInMerging {
		t.Fatalf("claim = %+v, want no entry served, no phases row, no merge forward and no merged pull request", got)
	}

	// The tester left entry 2 behind and a turn confirmed the task of entry 3, which is the entry
	// it serves; its row stands on entry 4 and records a completion, and the daemon asked the
	// implementer in testing to merge the base forward onto the head the pull request stands at.
	tester.ServedEntry = 2
	if err := st.PutClaim(ctx, tester); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := st.Pool().Exec(ctx, `insert into pending_task_deliveries
		(claim_token, delivery_id, task, queued_at, delivered_at, confirmed_at, phase, generation, entry)
		values ($1, 'd-3', 'test it', $2, $2, $2, 'testing', 1, 3)`, string(token), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: rootIssue, Role: claim.RoleTester, Claim: token,
			Entry: 4, HandoffCommit: "c0de"}); err != nil {
			return err
		}
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: rootIssue,
			Repo: "acme/widgets", Number: 42, Branch: "legion/" + rootIssue, HeadSHA: "c0de", Failing: []string{},
			MergeForward: &record.MergeForward{Head: "c0de", Base: "main", Phase: phase.Testing, AskedAt: now}})
	}); err != nil {
		t.Fatalf("record the tester's row and the pull request of %s: %v", rootIssue, err)
	}
	if got := idleViewOf(t, st, rootIssue).Claims[0]; got.ServingEntry != 3 || got.RowEntry != 4 || !got.RowCompleted ||
		!got.MergeForwardHeld || got.MergedInMerging {
		t.Fatalf("claim = %+v, want entry 3 served, its row on entry 4 with its completion, the merge forward holding it and the pull request open", got)
	}

	// The issue moves into merging and its pull request merges: the claim reads the merging
	// finished (record.MergedInMerging), and the ask made in testing does not hold the tester,
	// which works no phase in merging.
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `update issues set phase = 'merging' where key = $1`, rootIssue); err != nil {
			return err
		}
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestMerged, Issue: rootIssue,
			Repo: "acme/widgets", Number: 42, Branch: "legion/" + rootIssue, HeadSHA: "c0de", Failing: []string{},
			MergeForward: &record.MergeForward{Head: "c0de", Base: "main", Phase: phase.Testing, AskedAt: now}})
	}); err != nil {
		t.Fatalf("move %s into merging on its merged pull request: %v", rootIssue, err)
	}
	if got := idleViewOf(t, st, rootIssue).Claims[0]; !got.MergedInMerging || got.MergeForwardHeld {
		t.Fatalf("claim = %+v, want the merging finished by the merged pull request and no merge forward holding it", got)
	}

	// The merger has retired no run, and holds the task of run 1 and merging at entry 2 that its
	// agent neither read nor confirmed, as a send that failed before any turn leaves it.
	merger := claim.Token("legion-legion-legion-208-merger")
	if err := st.PutClaim(ctx, supervise.Claim{Token: merger, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleMerger, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into pending_task_deliveries
		(claim_token, delivery_id, task, queued_at, phase, generation, entry)
		values ($1, 'd-merge', 'merge it', $2, 'merging', 1, 2)`, string(merger), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var read *pods.IdleClaim
	for _, c := range idleViewOf(t, st, rootIssue).Claims {
		if c.Role == claim.RoleMerger {
			read = &c
		}
	}
	if read == nil || read.ServingGeneration != 1 || read.ServingEntry != 2 {
		t.Fatalf("the merger = %+v, want run 1 and entry 2 served: it has retired no run and holds the task of that run, phase and entry", read)
	}
}

// idleViewOf is the pod's view as the release's own transaction reads it (readIdleView, which
// ReleaseIdleIssue hands to the rule): the store serves no view of its own, because nothing but
// that decision reads one.
func idleViewOf(t *testing.T, st *Store, issue string) pods.IdleView {
	t.Helper()
	ctx := context.Background()
	var view pods.IdleView
	if err := Transact(ctx, st.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		view, err = readIdleView(ctx, tx, "legion", issue, st.idleRecordsOf())
		return err
	}); err != nil {
		t.Fatalf("read the idle view of %s: %v", issue, err)
	}
	return view
}

// A machine's own claim write never carries the activity report, which is why an idle decision
// reads it from the store: this pins that a PutClaim after a report leaves the four columns
// standing, so a view taken later still sees what the session said.
func TestAClaimWriteAfterAReportLeavesTheReportStanding(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	token := claim.Token("legion-legion-legion-208-implementer")
	c := supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateWorking}
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	reported := claim.SessionActivity{At: time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)}
	if _, err := st.PutClaimActivity(ctx, token, c.Generation, reported); err != nil {
		t.Fatal(err)
	}
	// The machine, which never read the report, persists its next state.
	c.State = supervise.StateIdle
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	view := idleViewOf(t, st, rootIssue)
	if got := view.Claims[0]; !got.ActivityAt.Equal(reported.At) {
		t.Fatalf("activity_at = %s after the machine's write, want the report's %s", got.ActivityAt, reported.At)
	}
}

// The release's one transaction: the view it decides on, the rows it queues and the mark that
// records it commit together, and the mark's boundary is the first row's id. A decision that
// releases nothing writes nothing, and a second release never starts behind the first.
func TestReleaseIdleIssueWritesItsRowsAndItsMarkTogether(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	token := claim.Token("legion-legion-legion-208-implementer")
	if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatal(err)
	}

	kept, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(pods.IdleView) (IdleReleaseMark, error) {
		return IdleReleaseMark{}, nil
	})
	if err != nil || kept {
		t.Fatalf("a decision that releases nothing = %t, %v; want no release", kept, err)
	}
	var rows int
	if err := st.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1`, rootIssue).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("outbox rows = %d, want none written by a decision that released nothing", rows)
	}

	var seen pods.IdleView
	marked, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view pods.IdleView) (IdleReleaseMark, error) {
		seen = view
		return IdleReleaseMark{Generation: view.Generation, Role: claim.RoleImplementer, Rows: []IdleReleaseRow{
			{Kind: "issue_bundle", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"implementer"}`)},
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"implementer","generation":1}`), Boundary: true},
			{Kind: "issue_release", Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1}`)},
		}}, nil
	})
	if err != nil || !marked {
		t.Fatalf("ReleaseIdleIssue = %t, %v; want the release marked", marked, err)
	}
	if len(seen.Claims) != 1 || seen.Claims[0].Role != claim.RoleImplementer {
		t.Fatalf("the decision saw %+v, want the issue's one claim", seen.Claims)
	}

	var ids []int64
	var kinds []string
	queued, err := st.Pool().Query(ctx, `select id, kind from outbox where issue = $1 order by id`, rootIssue)
	if err != nil {
		t.Fatal(err)
	}
	for queued.Next() {
		var id int64
		var kind string
		if err := queued.Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		ids, kinds = append(ids, id), append(kinds, kind)
	}
	queued.Close()
	if len(kinds) != 3 || kinds[0] != "issue_bundle" || kinds[1] != "supervise" || kinds[2] != "issue_release" ||
		ids[1] <= ids[0] || ids[2] <= ids[1] {
		t.Fatalf("queued rows = %v (%v), want the workspace check, then the role stop, then the teardown", kinds, ids)
	}
	var first, boundary, last, generation int64
	var role string
	if err := st.Pool().QueryRow(ctx, `select first_row, boundary_row, last_row, generation, role from issue_releases
		where project='legion' and issue=$1`, rootIssue).Scan(&first, &boundary, &last, &generation, &role); err != nil {
		t.Fatal(err)
	}
	// The mark names its own rows: the workspace check, the role stop that is its boundary, and the
	// teardown, which is how a row of this release is told from a row of one that is over.
	if first != ids[0] || boundary != ids[1] || last != ids[2] || generation != 1 || role != "implementer" {
		t.Fatalf("mark = rows %d..%d boundary %d, generation %d, role %s; want the rows %v", first, last, boundary, generation, role, ids)
	}

	// A second release never starts behind the first: the decision is asked, sees the mark, and
	// nothing more is written.
	again, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view pods.IdleView) (IdleReleaseMark, error) {
		if !view.Releasing {
			t.Error("the second decision was handed a view that does not report the release in progress")
		}
		return IdleReleaseMark{Generation: 1, Role: claim.RoleImplementer, Rows: []IdleReleaseRow{
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"implementer","generation":1}`), Boundary: true},
		}}, nil
	})
	if err != nil || again {
		t.Fatalf("a second release = %t, %v; want none", again, err)
	}
	if err := st.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1`, rootIssue).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Fatalf("outbox rows = %d, want the first release's three alone", rows)
	}

	if err := st.ClearIssueRelease(ctx, "legion", rootIssue); err != nil {
		t.Fatal(err)
	}
	if cleared := idleViewOf(t, st, rootIssue); cleared.Releasing {
		t.Fatalf("view after the mark was cleared = %t, want no release in progress", cleared.Releasing)
	}
}

// The idle release queues no row keyed by another project's issue (insertOutboxRow,
// claim.KeyOfProject): an operator's spawn of that issue runs under this project's token, so its
// pod is one the release reads, but a row keyed by the issue is run only by the other project's
// daemon, against that daemon's own claims. A release whose rows are keyed so is refused whole
// (claim.ErrKeyOfAnotherProject), with neither a row nor its mark written. The same release of this
// project's own issue is marked.
func TestTheIdleReleaseQueuesNoRowOfAnotherProjectsIssue(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	const theirs = "OTHERPROJECT-9"
	marked, err := st.ReleaseIdleIssue(ctx, "legion", theirs, func(pods.IdleView) (IdleReleaseMark, error) {
		return IdleReleaseMark{Generation: 1, Role: claim.RoleArchitect, Rows: []IdleReleaseRow{
			{Kind: "issue_bundle", Payload: []byte(`{"tree":"OTHERPROJECT-9","generation":1,"treeGeneration":1,"role":"architect"}`)},
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"OTHERPROJECT-9","role":"architect","generation":1}`), Boundary: true},
		}}, nil
	})
	if !errors.Is(err, claim.ErrKeyOfAnotherProject) || marked {
		t.Errorf("the release of %s = %t, %v; want it refused with claim.ErrKeyOfAnotherProject: its rows are keyed by OTHERPROJECT's issue",
			theirs, marked, err)
	}
	var rows, marks int
	if err := st.Pool().QueryRow(ctx, `select (select count(*) from outbox where issue = $1),
		(select count(*) from issue_releases where issue = $1)`, theirs).Scan(&rows, &marks); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || marks != 0 {
		t.Errorf("after the release of %s the outbox holds %d rows keyed by it and %d marks of it, want none", theirs, rows, marks)
	}
	releaseThroughTheStore(t, st, rootIssue)
}

// The release of a pod no role can use (workflow.UnownedPodRelease) suspends no role and reads
// no workspace, so its mark names no role and records the rule that wrote it instead: every row of
// that release, and the cancel that ends it, read the kind to know what the release did. A mark
// that names no role and is not that release is the rule's own fault, and is refused before
// anything is written.
func TestAReleaseThatSuspendsNoRoleMarksItsKindAndNamesNoRole(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	teardown := IdleReleaseRow{Kind: "issue_release",
		Payload:  []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1}`),
		Boundary: true}

	refused, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view pods.IdleView) (IdleReleaseMark, error) {
		return IdleReleaseMark{Generation: view.Generation, Rows: []IdleReleaseRow{teardown}}, nil
	})
	if err == nil || refused {
		t.Fatalf("a mark naming neither a role nor the second rule = %t, %v; want it refused before anything is written", refused, err)
	}
	var rows int
	if err := st.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1`, rootIssue).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("outbox rows = %d, want none written by a mark the store refused", rows)
	}

	marked, err := st.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view pods.IdleView) (IdleReleaseMark, error) {
		return IdleReleaseMark{Generation: view.Generation, SuspendsNoRole: true, Rows: []IdleReleaseRow{teardown}}, nil
	})
	if err != nil || !marked {
		t.Fatalf("ReleaseIdleIssue = %t, %v; want the release of the pod no role can use marked", marked, err)
	}
	var kind, role string
	var last int64
	if err := st.Pool().QueryRow(ctx, `select kind, role, last_row from issue_releases
		where project = 'legion' and issue = $1`, rootIssue).Scan(&kind, &role, &last); err != nil {
		t.Fatal(err)
	}
	if kind != "unowned" || role != "" {
		t.Fatalf("mark = kind %q role %q, want the unowned kind and no role: that release suspends none", kind, role)
	}

	// Every row of the release reads the kind back out of the mark, whatever the fences then
	// decide for the row itself.
	decision, err := st.IssueRelease(ctx, "legion",
		IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: last},
		record.OutOfWorkflow)
	if err != nil || !decision.SuspendsNoRole {
		t.Fatalf("the teardown's decision = %+v, %v; want it to carry the release that suspends no role", decision, err)
	}

	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if err != nil || !cancelled.SuspendsNoRole || len(cancelled.Resumed) != 0 {
		t.Fatalf("the cancel = %+v, %v; want no role resumed and the release named as the one that suspends none",
			cancelled, err)
	}
}

// IdlePods is the tick's candidate list: every issue of the project with a live claim, and no
// issue whose roles have all stopped — there is no role there to read the workspace through and
// no claim to suspend.
func TestIdlePodsListsOnlyIssuesWithALiveClaim(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	for _, c := range []supervise.Claim{
		{Token: claim.Token("legion-legion-legion-208-implementer"), Issue: rootIssue, Role: claim.RoleImplementer, State: supervise.StateIdle},
		{Token: claim.Token("legion-legion-legion-208-tester"), Issue: rootIssue, Role: claim.RoleTester, State: supervise.StateSuspended},
		{Token: claim.Token("legion-legion-legion-209-implementer"), Issue: childIssue, Role: claim.RoleImplementer, State: supervise.StateRetired},
	} {
		c.Project, c.Tree, c.TreeEpoch, c.Generation = "legion", rootIssue, 1, 1
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	pods, err := st.IdlePods(ctx, "legion")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0] != rootIssue {
		t.Fatalf("pods = %v, want %s alone", pods, rootIssue)
	}
}

// UnownedPods is the other candidate list: the issues whose claims have all stopped and whose
// pod no role can take a turn in, which is what the second release rule acts on. The claim
// states alone are not the answer — every issue of the deployment whose roles ever stopped
// would be one, and each candidate costs a transaction that takes the daemon's global fact
// serializer — so the workflow filter is read here beside them: the member of a lingering tree,
// the issue whose own phase is done and the one closed long ago all hold claims that have
// stopped and are no candidates, and the pod with a live claim is the idle rule's.
func TestTheCandidateAnswerForIssuesWithNoLiveClaim(t *testing.T) {
	t.Parallel()
	const (
		lingeringRoot = "LEGION-300"
		unowned       = "LEGION-301"
		underLinger   = "LEGION-302"
		phaseDone     = "LEGION-303"
		closedLongAgo = "LEGION-304"
		stillLive     = "LEGION-305"
	)
	st := migratedStore(t)
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq) values
		($1, $1, 'LEGION', 'a tree lingering after its close', 'done', 1, 'done', 'A', now() + interval '1 hour', 1),
		($2, $2, 'LEGION', 'a pod whose claims have all stopped', 'implementing', 1, 'in_progress', 'B', null, 1),
		($3, $1, 'LEGION', 'a member of the lingering tree', 'implementing', 1, 'in_progress', 'C', null, 1),
		($4, $4, 'LEGION', 'an issue whose own phase is done', 'done', 1, 'in_progress', 'D', null, 1),
		($5, $5, 'LEGION', 'an issue closed long ago', 'done', 1, 'done', 'E', null, 1),
		($6, $6, 'LEGION', 'a pod a live claim still holds', 'implementing', 1, 'in_progress', 'F', null, 1)`,
		lingeringRoot, unowned, underLinger, phaseDone, closedLongAgo, stillLive); err != nil {
		t.Fatalf("seed the project's issues: %v", err)
	}
	for _, c := range []supervise.Claim{
		{Token: claim.Token("legion-legion-legion-301-implementer"), Issue: unowned, Tree: unowned, Role: claim.RoleImplementer, State: supervise.StateFailed},
		{Token: claim.Token("legion-legion-legion-301-tester"), Issue: unowned, Tree: unowned, Role: claim.RoleTester, State: supervise.StateSuspended},
		{Token: claim.Token("legion-legion-legion-302-implementer"), Issue: underLinger, Tree: lingeringRoot, Role: claim.RoleImplementer, State: supervise.StateRetired},
		{Token: claim.Token("legion-legion-legion-303-implementer"), Issue: phaseDone, Tree: phaseDone, Role: claim.RoleImplementer, State: supervise.StateRetired},
		{Token: claim.Token("legion-legion-legion-304-implementer"), Issue: closedLongAgo, Tree: closedLongAgo, Role: claim.RoleImplementer, State: supervise.StateSuspended},
		{Token: claim.Token("legion-legion-legion-305-implementer"), Issue: stillLive, Tree: stillLive, Role: claim.RoleImplementer, State: supervise.StateIdle},
	} {
		c.Project, c.TreeEpoch, c.Generation = "legion", 1, 1
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := st.UnownedPods(ctx, "LEGION", "legion", record.OutOfWorkflowStatuses)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0] != unowned {
		t.Fatalf("the candidates = %v, want %s alone: the pod no claim owns under a tree the workflow still runs", candidates, unowned)
	}
}

// A cancel and a park of the same issue in flight together cannot end with a resume standing
// above the close. The park is the act production runs: one fact transaction that takes the
// issue out of the workflow and queues the close's own row together, under the shared
// serializer. The cancel reads the issue's standing and takes its resume's id from the same
// outbox sequence, so with no lock of its own it can read the status from before that
// transaction and still insert above the row it queued. The resume then supersedes the close:
// the close's suspension finishes without acting, the issue's Sandbox is never deleted, and the
// roles of a parked issue run on in it with their launcher tokens and `gh` credentials.
//
// The interleaving is driven, not hoped for: the park holds its transaction open with its row
// already queued while the cancel runs, which is the whole window. Without
// treelifecycle.Serialize at the head of the cancel's transaction the cancel runs straight
// through it — nothing it reads or writes meets a row the park holds — and the close stops
// acting; with it the cancel waits for the park's commit and reads the issue as one that left.
// Both outcomes are read here rather than only the wait, so the failure the lock prevents is
// reported as the end state it leaves.
func TestACancelRacingAParkLeavesNoResumeAboveTheClose(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	releasedClaim(t, st, claim.RoleImplementer)
	markRelease(t, st, first, boundary, last, 0, claim.RoleImplementer)

	park, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = park.Rollback(ctx) }()
	if err := treelifecycle.Serialize(ctx, park); err != nil {
		t.Fatal(err)
	}
	if _, err := park.Exec(ctx, `update issues set phase = 'done', status = 'backlog',
		linger_until = now() + interval '1 hour' where key = $1`, rootIssue); err != nil {
		t.Fatalf("park the root: %v", err)
	}
	var closeRow int64
	if err := park.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ('issue_suspend', $1, '{"tree":"LEGION-208","generation":1,"treeGeneration":1}', 0, now(), '', '')
		returning id`, rootIssue).Scan(&closeRow); err != nil {
		t.Fatalf("queue the close's own row: %v", err)
	}

	type cancelled struct {
		roles []claim.Role
		err   error
	}
	done := make(chan cancelled, 1)
	go func() {
		answer, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
		done <- cancelled{roles: answer.Resumed, err: err}
	}()

	var got cancelled
	var finished bool
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case got = <-done:
			finished = true
		default:
		}
		if finished || waitingOnTheSerializer(t, st) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancel neither finished nor waited for the park's serializer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := park.Commit(ctx); err != nil {
		t.Fatalf("commit the park: %v", err)
	}
	if !finished {
		got = <-done
	}
	if got.err != nil {
		t.Fatalf("carry the cancel out: %v", got.err)
	}

	if len(got.roles) != 0 {
		t.Fatalf("resumed = %v, want none: the claims of an issue a park took out of the workflow are its close's", got.roles)
	}
	var above int
	if err := st.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and id > $2`, rootIssue, closeRow).Scan(&above); err != nil {
		t.Fatal(err)
	}
	if above != 0 {
		t.Fatalf("%d resume rows stand above the close's row %d; the close is superseded, its Sandbox is never deleted, and the parked issue's roles run on in it", above, closeRow)
	}
	if releaseMarked(t, st) {
		t.Fatal("the cancel left its mark behind; every later decision would read a release that is over")
	}

	// The consequence itself: the close's own suspension acts, which is what deletes the issue's
	// Sandbox and takes the pod's role Secrets with it.
	act, err := st.IssueSuspension(ctx, "legion",
		IssueClose{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: closeRow},
		record.OutOfWorkflow)
	if err != nil || !act {
		t.Fatalf("the close after the race = %t, %v; want it to act", act, err)
	}
}

// A paused cancel whose ReleaseCancelPause carries a backstop and lacks a rule of who gets it, the
// workflow's open phase's worker (OpenPhaseRole) or whether that worker gets one (GetsBackstop), is
// refused with an error that names each rule it lacks, before it reads or writes anything
// (checkBackstopRules): the release stays marked, and no stop, pod hold, backstop, decline or start
// is recorded. Where the cancel skipped the backstop in silence instead, it armed none for a worker
// that gets one (dispatch://INFRA-2746 R1). A workspace check's decline is a cancel
// (DeclineIssueRelease), and it is refused the same way. The control is the same cancel, and the
// same decline, with both rules: each records the implementer stopped by the pause, holds the pod
// and arms the implementer's backstop.
func TestACancelWhileThePauseHoldsABackstopAndNoBackstopRuleIsRefused(t *testing.T) {
	t.Parallel()
	const openPhaseRole, getsBackstopRule = "ReleaseCancelPause.OpenPhaseRole", "ReleaseCancelPause.GetsBackstop"
	at := pausedAt.Add(time.Minute)
	for _, act := range []struct {
		name     string
		declines bool
		run      func(st *Store, paused ReleaseCancelPause) (ReleaseCancelled, error)
	}{
		{name: "a cancel", run: func(st *Store, paused ReleaseCancelPause) (ReleaseCancelled, error) {
			return st.CancelIssueRelease(context.Background(), "legion", rootIssue, resumeRow, record.OutOfWorkflow, paused)
		}},
		{name: "a decline", declines: true, run: func(st *Store, paused ReleaseCancelPause) (ReleaseCancelled, error) {
			return st.DeclineIssueRelease(context.Background(), "legion", rootIssue, IdleReleaseDecline{At: at,
				Role: claim.RoleImplementer, Reason: pods.DeclineUnpushedWork, Head: "abc", Commits: 1, PodUID: "pod-a"},
				resumeRow, record.OutOfWorkflow, paused)
		}},
	} {
		t.Run(act.name, func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)
			openRunningTree(t, st)
			servingReleasedClaim(t, st, claim.RoleImplementer)
			markRelease(t, st, 18, 20, 22, 0, claim.RoleImplementer)
			pause, _ := pauseLegion(t, st, pausedAt)

			for _, refused := range []struct {
				name    string
				paused  ReleaseCancelPause
				missing []string
			}{
				{name: "no GetsBackstop", paused: ReleaseCancelPause{At: at, Backstop: time.Hour, OpenPhaseRole: implementingWorker},
					missing: []string{getsBackstopRule}},
				{name: "no OpenPhaseRole", paused: ReleaseCancelPause{At: at, Backstop: time.Hour, GetsBackstop: getsBackstop},
					missing: []string{openPhaseRole}},
				{name: "neither rule", paused: ReleaseCancelPause{At: at, Backstop: time.Hour},
					missing: []string{openPhaseRole, getsBackstopRule}},
			} {
				_, err := act.run(st, refused.paused)
				if err == nil {
					t.Fatalf("%s with a backstop and %s while paused went through, want it refused naming the rule it lacks",
						act.name, refused.name)
				}
				for _, rule := range []string{openPhaseRole, getsBackstopRule} {
					if named, want := strings.Contains(err.Error(), rule), slices.Contains(refused.missing, rule); named != want {
						t.Fatalf("%s with a backstop and %s = %v; names %s %t, want %t", act.name, refused.name, err, rule, named, want)
					}
				}
				if !releaseMarked(t, st) {
					t.Fatalf("%s with a backstop and %s was refused and deleted the release's mark, want it standing",
						act.name, refused.name)
				}
				for what, n := range map[string]int{
					"stops":     pauseCount(t, st, `select count(*) from project_pause_stops where issue = $1`, rootIssue),
					"backstops": pauseCount(t, st, `select count(*) from pod_wake_backstops where issue = $1`, rootIssue),
					"declines":  pauseCount(t, st, `select count(*) from idle_release_declines where issue = $1`, rootIssue),
					"starts":    len(startRows(t, st)),
				} {
					if n != 0 {
						t.Fatalf("%s with a backstop and %s was refused and recorded %d %s, want none: a refused cancel writes nothing",
							act.name, refused.name, n, what)
					}
				}
				if hold, held, err := st.PausePodHoldOf(ctx, "LEGION", rootIssue); err != nil || held {
					t.Fatalf("%s with a backstop and %s was refused and the pod's hold = %+v, %t, %v; want none",
						act.name, refused.name, hold, held, err)
				}
			}

			// The control: the same act with both rules records the implementer stopped by the
			// pause, holds the pod and arms the implementer's backstop an hour after the cancel.
			cancelled, err := act.run(st, ReleaseCancelPause{At: at, Backstop: time.Hour, OpenPhaseRole: implementingWorker,
				GetsBackstop: getsBackstop})
			if err != nil || cancelled.Pause != pause.ID || cancelled.Declined != act.declines ||
				!slices.Equal(cancelled.Stopped, []claim.Role{claim.RoleImplementer}) {
				t.Fatalf("%s with both rules while paused = %+v, %v; want pause %d, the implementer stopped and declined %t",
					act.name, cancelled, err, pause.ID, act.declines)
			}
			if releaseMarked(t, st) {
				t.Fatalf("%s with both rules left the release's mark standing", act.name)
			}
			if n := pauseCount(t, st, `select count(*) from project_pause_stops where pause_id = $1 and issue = $2
				and role = 'implementer' and ended_as = 'stopped'`, pause.ID, rootIssue); n != 1 {
				t.Fatalf("%s with both rules recorded %d stops of the implementer by pause %d, want 1", act.name, n, pause.ID)
			}
			if n := pauseCount(t, st, `select count(*) from pod_wake_backstops where issue = $1 and role = 'implementer'
				and not fired and due_at = $2`, rootIssue, at.Add(time.Hour)); n != 1 {
				t.Fatalf("%s with both rules armed %d backstops of the implementer due an hour after it, want 1", act.name, n)
			}
			if hold, held, err := st.PausePodHoldOf(ctx, "LEGION", rootIssue); err != nil || !held || hold.Pause != pause.ID {
				t.Fatalf("%s with both rules left the pod's hold = %+v, %t, %v; want pause %d's", act.name, hold, held, err, pause.ID)
			}
		})
	}
}

// The idle rule holds an issue's pod for every row of the role-message lane: each of those rows
// speaks to a role of that pod, and a release stops the role that the row was written for. The
// lane's own list is record.LaneKinds(record.LaneRoleMessage) and idleWaitingKinds writes its
// kinds out as the strings the SQL compares, so nothing but this test holds the two together: a
// kind added to the lane and left out of the idle list lets the daemon release the pod out from
// under a row nobody has read yet.
func TestEveryRoleMessageKindWaitsForARoleOfThePod(t *testing.T) {
	t.Parallel()
	for _, kind := range record.LaneKinds(record.LaneRoleMessage) {
		if !slices.Contains(idleWaitingKinds, string(kind)) {
			t.Errorf("the role-message kind %q stands in no idleWaitingKinds %v: a release would stop the role before it reads the row", kind, idleWaitingKinds)
		}
	}
}

// A start queued for news of the approved design alone (record.SuperviseRequest.News) holds no
// pod and cancels no release (idleWaitingRow). It reaches only a claim already given its phase,
// and a claim an idle release suspended is one: the start resumes it, so the role comes back with
// its news. It can also wait for a long time: a merger's waits for the whole time its tree's design
// gate stays closed. So a pod whose only queued row for a role is such a start reads nothing
// waiting, and such a start queued after the release was marked does not cancel it at its
// boundary. A start that carries a task of the phase still does both.
func TestAStartQueuedForNewsAloneHoldsNoPodAndCancelsNoRelease(t *testing.T) {
	t.Parallel()
	const newsOnly = `{"op":"start","tree":"LEGION-208","role":"architect","generation":1,"news":true}`
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	token := claim.Token("legion-legion-legion-208-architect")
	if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatal(err)
	}
	queue := func(payload string) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('supervise', $1, $2, 3, now() + interval '1 minute', 'a delivery is already pending')`, rootIssue, payload); err != nil {
			t.Fatal(err)
		}
	}

	queue(newsOnly)
	if view := idleViewOf(t, st, rootIssue); view.Waiting {
		t.Fatal("the view reads a row waiting for a role of the pod, though the only one queued is a start of news alone")
	}

	_, boundary, _ := releaseThroughTheStore(t, st, rootIssue)
	queue(newsOnly)
	decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
	if err != nil {
		t.Fatalf("IssueReleaseStop: %v", err)
	}
	if decision.Act != ReleaseActs {
		t.Fatalf("the boundary = %s for %s with only starts of news alone queued beside the release, want %s", decision.Act, decision.Cause, ReleaseActs)
	}

	queue(`{"op":"start","tree":"LEGION-208","role":"architect","generation":1,"task":"Continue."}`)
	if view := idleViewOf(t, st, rootIssue); !view.Waiting {
		t.Fatal("the view reads nothing waiting, though a start carrying a task is queued for the pod's architect")
	}
	if decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary); err != nil || decision.Act != ReleaseCancel {
		t.Fatalf("the boundary = %s (%v) with a start carrying a task queued after the mark, want %s", decision.Act, err, ReleaseCancel)
	}
}

// keptRoleMessageRows are the two kinds of row the daemon publishes with keep to a role and no
// source session, each to a role of rootIssue: a hook prompt to the implementer and a phase-ended
// message to the tester.
var keptRoleMessageRows = []struct {
	kind    string
	role    claim.Role
	payload record.OutboxPayload
}{
	{kind: "a hook prompt", role: claim.RoleImplementer, payload: record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer,
		From: "reviewing", To: "implementing", Count: 1, Counts: map[string]int{"reviewing-implementing": 1}, Prompt: "Read the review."}},
	{kind: "a phase-ended message", role: claim.RoleTester, payload: record.PhaseEnded{Role: claim.RoleTester, From: "testing",
		To: "awaiting_children", Reason: "the admission pass stopped the tester's testing phase"}},
}

// queueWaitingKeptRow queues payload's row on rootIssue as a row that has waited three attempts for
// a session to hold its role (the daemon's publishKeptToRole).
func queueWaitingKeptRow(t *testing.T, st *Store, payload record.OutboxPayload) {
	t.Helper()
	row, err := record.NewOutboxRow(rootIssue, payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(context.Background(), `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, $2, $3, 3, now() + interval '1 minute', 'no live session holds the role')`,
		string(row.Kind), rootIssue, row.Payload); err != nil {
		t.Fatal(err)
	}
}

// A hook prompt's or a phase-ended message's row waits up to an hour for a session to hold its role
// (the daemon's publishKeptToRole), and it holds the issue's pod for that wait, and cancels a
// release at its boundary, only while its role has a claim this pod answers: one that runs or is
// starting, or one an idle release suspended, which the row's own first attempt wakes. A role with
// no claim, or one whose claim failed, was retired or was suspended for another cause, comes back
// only by a start or a resume, the operator's or the controller's, and either brings a pod of its
// own, so such a row holds no pod and cancels no release (idleWaitingRow). The rows are each
// other's control: a term that let every such row go would fail the rows that hold, and the
// predicate before the term, which held the pod for every such row, fails the rest.
func TestAKeptRoleMessageHoldsThePodOnlyWhileItsRoleHasAClaimThisPodAnswers(t *testing.T) {
	t.Parallel()
	for _, message := range keptRoleMessageRows {
		for _, tc := range []struct {
			name      string
			unclaimed bool
			state     supervise.ClaimState
			by        supervise.SuspendSource
			holds     bool
		}{
			{name: "an idle claim", state: supervise.StateIdle, holds: true},
			{name: "a claim an idle release suspended", state: supervise.StateSuspended, by: supervise.SuspendedByReleased, holds: true},
			{name: "no claim", unclaimed: true},
			{name: "a claim the daemon suspended", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon},
			{name: "a failed claim", state: supervise.StateFailed},
			{name: "a retired claim", state: supervise.StateRetired},
		} {
			t.Run(message.kind+" to a role with "+tc.name, func(t *testing.T) {
				st := migratedStore(t)
				ctx := context.Background()
				openRunningTree(t, st)
				claims := []supervise.Claim{{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue,
					Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, ServingGeneration: 1, State: supervise.StateIdle}}
				if !tc.unclaimed {
					claims = append(claims, supervise.Claim{Token: claim.Token("legion-legion-legion-208-" + string(message.role)), Project: "legion",
						Tree: rootIssue, Issue: rootIssue, Role: message.role, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
						State: tc.state, SuspendedBy: tc.by})
				}
				for _, c := range claims {
					if err := st.PutClaim(ctx, c); err != nil {
						t.Fatal(err)
					}
				}
				queueWaitingKeptRow(t, st, message.payload)

				if view := idleViewOf(t, st, rootIssue); view.Waiting != tc.holds {
					t.Fatalf("the view reads a row waiting for a role of the pod = %t, with %s to a %s with %s, want %t", view.Waiting, message.kind, message.role, tc.name, tc.holds)
				}
				_, boundary, _ := releaseThroughTheStore(t, st, rootIssue)
				decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
				if err != nil {
					t.Fatalf("IssueReleaseStop: %v", err)
				}
				want := ReleaseActs
				if tc.holds {
					want = ReleaseCancel
				}
				if decision.Act != want {
					t.Fatalf("the boundary = %s for %s, with %s to a %s with %s queued before the mark, want %s", decision.Act, decision.Cause, message.kind, message.role, tc.name, want)
				}
			})
		}
	}
}

// The idle rule's two exemptions meet on one role: a tester owed a start of its phase that waits for
// its issue's admission slot (slotWaitingStart), and a phase-ended message to that tester waiting
// for a session to hold its role (heldMessageForNoClaimThisPodAnswers). Each term decides its own
// row. The start holds no pod while no slot stands, whatever the claim, and the phase-ended row
// holds the pod while the tester's claim is one this pod answers. So with the claim an idle release
// suspended, the pod is held, by the phase-ended row alone; with the claim the daemon suspended,
// nothing holds it. Once the slot comes, the start holds the pod too. Each row is the other's
// control: a predicate that let the slot term exempt the phase-ended row would fail the first, and
// one that let the claim term hold the start would fail the second.
func TestAPhaseEndedMessageToARoleWhoseStartWaitsForItsSlotHoldsThePodByItsOwnRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		by    supervise.SuspendSource
		holds bool
	}{
		{name: "the tester's claim an idle release suspended", by: supervise.SuspendedByReleased, holds: true},
		{name: "the tester's claim the daemon suspended", by: supervise.SuspendedByDaemon},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-tester"), Project: "legion",
				Tree: rootIssue, Issue: rootIssue, Role: claim.RoleTester, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
				State: supervise.StateSuspended, SuspendedBy: tc.by}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
				values ('supervise', $1, '{"op":"start","tree":"LEGION-208","role":"tester","generation":1,"phase":"testing","task":"Continue.","needsSlot":true}', 0, now(), '')`,
				rootIssue); err != nil {
				t.Fatal(err)
			}
			if view := idleViewOf(t, st, rootIssue); view.Waiting {
				t.Fatal("UNKNOWN: the start that waits for its slot alone holds the pod, so the phase-ended row tests nothing")
			}
			queueWaitingKeptRow(t, st, keptRoleMessageRows[1].payload)

			if view := idleViewOf(t, st, rootIssue); view.Waiting != tc.holds {
				t.Fatalf("the view reads a row waiting for a role of the pod = %t, with %s, a start waiting for its slot and a phase-ended message, want %t", view.Waiting, tc.name, tc.holds)
			}
			if _, err := st.Pool().Exec(ctx, `insert into slots (issue, index, admitted_at) values ($1, 0, now())`, rootIssue); err != nil {
				t.Fatal(err)
			}
			if view := idleViewOf(t, st, rootIssue); !view.Waiting {
				t.Fatalf("the view reads nothing waiting once the slot came, with %s: the start is due and holds the pod", tc.name)
			}
		})
	}
}

// A worker start that waits for its issue's admission slot (record.SuperviseRequest.NeedsSlot)
// holds no pod and cancels no release while no slot of its issue stands (idleWaitingRow). The
// outbox holds such a start back until then (record's outboxInOrder), and an issue whose work came
// back waits in the admission line for as long as its blocker stays open or the line is ahead of
// it: hours, and for a blocked issue days. A pod held for it would stand idle all that time. Once
// the slot comes the start runs, and it wakes the worker a release suspended as any start does. So
// with the slot given, the same row holds the pod and cancels a release as any start that carries
// a task does: at the boundary when the slot comes before it, and at the teardown when it comes
// after the boundary ran, since a start left out there that has come due since is the pod about
// to be used again.
func TestAStartWaitingForItsSlotHoldsNoPodUntilTheSlotComes(t *testing.T) {
	t.Parallel()
	type release struct {
		st                    *Store
		ctx                   context.Context
		worker                supervise.Claim
		first, boundary, last int64
	}
	// setUp is an issue whose one live role is its implementer, idle in the pod, owed a start of
	// its phase that waits for the issue's admission slot, and the release of that pod marked.
	setUp := func(t *testing.T) release {
		st := migratedStore(t)
		ctx := context.Background()
		openRunningTree(t, st)
		worker := supervise.Claim{Token: claim.Token("legion-legion-legion-208-implementer"), Project: "legion",
			Tree: rootIssue, Issue: rootIssue, Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}
		if err := st.PutClaim(ctx, worker); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('supervise', $1, '{"op":"start","tree":"LEGION-208","role":"implementer","generation":1,"phase":"implementing","task":"Continue.","needsSlot":true}', 0, now(), '')`,
			rootIssue); err != nil {
			t.Fatal(err)
		}
		if view := idleViewOf(t, st, rootIssue); view.Waiting {
			t.Fatal("the view reads a row waiting for a role of the pod, though the only one queued is a start that waits for an admission slot the issue does not hold")
		}
		first, boundary, last := releaseRoleThroughTheStore(t, st, rootIssue, claim.RoleImplementer)
		return release{st: st, ctx: ctx, worker: worker, first: first, boundary: boundary, last: last}
	}
	// stopRan is the release's check and its role stop having run: the lane finished both rows,
	// and the stop suspended the worker as released.
	stopRan := func(t *testing.T, r release) {
		if _, err := r.st.Pool().Exec(r.ctx, `delete from outbox where id in ($1, $2)`, r.first, r.boundary); err != nil {
			t.Fatal(err)
		}
		r.worker.State, r.worker.SuspendedBy = supervise.StateSuspended, supervise.SuspendedByReleased
		if err := r.st.PutClaim(r.ctx, r.worker); err != nil {
			t.Fatal(err)
		}
	}
	slotComes := func(t *testing.T, r release) {
		if _, err := r.st.Pool().Exec(r.ctx, `insert into slots (issue, index, admitted_at) values ($1, 0, now())`, rootIssue); err != nil {
			t.Fatal(err)
		}
		if view := idleViewOf(t, r.st, rootIssue); !view.Waiting {
			t.Fatal("the view reads nothing waiting, though the issue now holds its slot and its start is due for a role of the pod")
		}
	}
	boundaryDecides := func(t *testing.T, r release, act ReleaseAct, cause ReleaseCause) {
		decision, err := r.st.IssueReleaseStop(r.ctx, "legion", rootIssue, r.boundary)
		if err != nil || decision.Act != act || decision.Cause != cause {
			t.Fatalf("the boundary = %s for %s, %v; want %s for %s", decision.Act, decision.Cause, err, act, cause)
		}
	}
	teardownDecides := func(t *testing.T, r release, act ReleaseAct, cause ReleaseCause) {
		decision, err := r.st.IssueRelease(r.ctx, "legion",
			IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: r.last}, record.OutOfWorkflow)
		if err != nil || decision.Act != act || decision.Cause != cause {
			t.Fatalf("the teardown = %s for %s, %v; want %s for %s", decision.Act, decision.Cause, err, act, cause)
		}
	}

	t.Run("no slot comes", func(t *testing.T) {
		t.Parallel()
		r := setUp(t)
		boundaryDecides(t, r, ReleaseActs, ReleaseCauseNone)
		stopRan(t, r)
		teardownDecides(t, r, ReleaseActs, ReleaseCauseNone)
	})
	t.Run("the slot comes before the boundary runs", func(t *testing.T) {
		t.Parallel()
		r := setUp(t)
		slotComes(t, r)
		boundaryDecides(t, r, ReleaseCancel, ReleaseCauseOvertaken)
	})
	t.Run("the slot comes after the boundary ran", func(t *testing.T) {
		t.Parallel()
		r := setUp(t)
		boundaryDecides(t, r, ReleaseActs, ReleaseCauseNone)
		stopRan(t, r)
		slotComes(t, r)
		teardownDecides(t, r, ReleaseCancel, ReleaseCauseOvertaken)
	})
}

// The store's rule for a start that waits for its admission slot (slotWaitingStart, read by the
// idle view and by WaitsForItsSlot) repeats record's outboxInOrder, which holds such a start back
// until the slot comes. The two must agree about one row encoded as the workflow writes it, or the
// idle rule holds the pod of an issue waiting in the line again, or releases a pod under a start
// that is due. For a start marked to wait: without a slot the outbox lists no due start, the idle
// view reads nothing waiting, and the issue waits for its slot; with a slot the start is due, the
// view reads it as waiting, and the issue no longer waits. An ordinary start, whose payload carries
// no `needsSlot`, is due, holds the pod and makes no wait, whether a slot stands or not.
func TestTheIdleRuleAndTheOutboxAgreeOnAStartWaitingForItsSlot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		needsSlot bool
	}{
		{name: "a start marked to wait for its slot", needsSlot: true},
		{name: "an ordinary start", needsSlot: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			row, err := record.NewOutboxRow(rootIssue, record.SuperviseRequest{Op: "start", Tree: rootIssue, Role: claim.RoleImplementer,
				Generation: 1, Phase: phase.Implementing, Task: "Continue.", NeedsSlot: tc.needsSlot}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			var id int64
			if err := Transact(ctx, st.pool, func(ctx context.Context, tx pgx.Tx) error {
				id, err = record.NewStore().EnqueueID(ctx, tx, row)
				return err
			}); err != nil {
				t.Fatalf("queue the start: %v", err)
			}
			records, err := st.PodRecordsOf("LEGION")
			if err != nil {
				t.Fatal(err)
			}
			check := func(slot bool) {
				t.Helper()
				var due, waits bool
				if err := Transact(ctx, st.pool, func(ctx context.Context, tx pgx.Tx) error {
					rows, err := record.NewStore().DueRows(ctx, tx, "LEGION", rootIssue, []record.OutboxKind{record.OutboxKindSupervise}, time.Now().Add(time.Minute))
					if err != nil {
						return err
					}
					due = slices.ContainsFunc(rows, func(r record.OutboxRow) bool { return r.ID == id })
					waits, err = records.WaitsForItsSlot(ctx, tx, rootIssue)
					return err
				}); err != nil {
					t.Fatalf("read the outbox: %v", err)
				}
				held := tc.needsSlot && !slot
				view := idleViewOf(t, st, rootIssue)
				if due == held || waits != held || view.Waiting == held {
					t.Fatalf("with a slot %t: due %t, waits for its slot %t, the idle view waiting %t; want due %t, waits %t, waiting %t",
						slot, due, waits, view.Waiting, !held, held, !held)
				}
			}

			check(false)
			if _, err := st.Pool().Exec(ctx, `insert into slots (issue, index, admitted_at) values ($1, 0, now())`, rootIssue); err != nil {
				t.Fatal(err)
			}
			check(true)
		})
	}
}

// A step record is history: its row reaches no session of the pod, so a record Dispatch has not
// taken yet, due now or backing off for an hour behind a Dispatch that is down, keeps no idle pod
// standing and cancels no release at its boundary, whether it was queued before the release's rows
// or after them. A start of a role of the pod queued beside it does both, which shows the reads
// see the queue at all.
func TestAPendingStepRecordHoldsNoIdlePod(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		attempts int
		nextAt   string
	}{
		{name: "due now", nextAt: "now()"},
		{name: "backing off behind a Dispatch that is down", attempts: 6, nextAt: "now() + interval '1 hour'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"),
				Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1,
				Generation: 1, ServingGeneration: 1, State: supervise.StateIdle}); err != nil {
				t.Fatal(err)
			}
			started := time.Now().UTC()
			queueRecord := func() {
				t.Helper()
				row, err := record.NewOutboxRow(rootIssue, record.StepRecord{ID: "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b",
					Task: "default", Step: 2, Kind: "step", Role: "implementer", Type: "implementation",
					Agent: "legion-legion-legion-208-implementer", Generation: 1, StartedAt: &started}, started)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
					values ($1, $2, $3, $4, `+tc.nextAt+`, 'Dispatch is unreachable')`,
					string(row.Kind), rootIssue, row.Payload, tc.attempts); err != nil {
					t.Fatal(err)
				}
			}

			queueRecord()
			if view := idleViewOf(t, st, rootIssue); view.Waiting {
				t.Fatal("the idle view reads a step record as waiting for a role of the pod, which would keep the idle pod standing")
			}
			candidates, err := st.IdlePods(ctx, "legion")
			if err != nil || !slices.Contains(candidates, rootIssue) {
				t.Fatalf("IdlePods = %v, %v; want %s, whose claim is live, a candidate", candidates, err, rootIssue)
			}
			_, boundary, _ := releaseThroughTheStore(t, st, rootIssue)
			queueRecord()
			decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil || decision.Act != ReleaseActs {
				t.Fatalf("the boundary with step records queued below and above the release's rows = %s for %s, %v; want %s",
					decision.Act, decision.Cause, err, ReleaseActs)
			}

			if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
				values ('supervise', $1, '{"op":"start","tree":"LEGION-208","role":"tester","generation":1}', 0, now(), '')`,
				rootIssue); err != nil {
				t.Fatal(err)
			}
			if view := idleViewOf(t, st, rootIssue); !view.Waiting {
				t.Fatal("control: the idle view reads nothing waiting, though a start of the pod's tester is queued")
			}
			decision, err = st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil || decision.Act != ReleaseCancel || decision.Cause != ReleaseCauseOvertaken {
				t.Fatalf("control: the boundary with a start of the tester queued = %s for %s, %v; want %s for %s",
					decision.Act, decision.Cause, err, ReleaseCancel, ReleaseCauseOvertaken)
			}
		})
	}
}

// waitingOnTheSerializer is whether a backend of this test's database other than the reader waits
// on an advisory lock: the shared serializer (treelifecycle.Serialize) is the only one the store
// takes by that name outside a migration, so a waiter here is a transaction held at its head.
func waitingOnTheSerializer(t *testing.T, st *Store) bool {
	t.Helper()
	var waiting bool
	if err := st.Pool().QueryRow(context.Background(), `select exists (select 1 from pg_stat_activity
		where datname = current_database() and pid <> pg_backend_pid()
		and wait_event_type = 'Lock' and wait_event = 'advisory')`).Scan(&waiting); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return waiting
}
