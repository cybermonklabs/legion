package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergeCloseNow is the clock of the engines these tests build, and mergeCloseRead the moment a read
// of the checks begins, after every entry into verifying they make.
var (
	mergeCloseNow  = time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	mergeCloseRead = mergeCloseNow.Add(time.Second)
)

// readPassing applies the outbox's read of issue's checks: its Verification document holds no
// check, and Dispatch lists no child, so the close waits for nothing the read decides.
func readPassing(t *testing.T, apply func(id string, fact intake.Fact) intake.Result, issue string) {
	t.Helper()
	apply("read-"+issue, intake.CloseChecksRead{Issue: issue, Generation: 7, ReadAt: mergeCloseRead,
		Answer: &record.CheckReadAnswer{Results: dispatch.IssueResults{Issue: issue}}})
}

// The merge of a child's pull request stands it in verifying, under a project that checks
// production and under one that does not: the child is not done, the architect above it is told
// close-waits and not child-closed, and the entry retires and suspends nothing. Every claim and the
// pod stand, so a look of its production check and its next change find them.
func TestAMergedChildWaitsInVerifyingAndRetiresNobody(t *testing.T) {
	for _, role := range []claim.Role{claim.RoleTester, ""} {
		t.Run(fmt.Sprintf("production_check %q", role), func(t *testing.T) {
			pool := migratedPool(t)
			seedMergingChild(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckBound: 48 * time.Hour, ProductionCheckRole: role, Clock: func() time.Time { return mergeCloseNow }}, nil)

			applyFacts(t, pool, engine)("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
			if got := issueAt(t, pool, "LEGION-209"); got != phase.Verifying {
				t.Fatalf("the merged child stands in %s, want verifying", got)
			}
			if kinds := noticeKinds(t, pool, "LEGION-209"); containsNotice(kinds, "child-closed") || !containsNotice(kinds, "close-waits") {
				t.Fatalf("child notices = %v, want close-waits and no child-closed", kinds)
			}
			for _, op := range []string{"issue_close", "suspend"} {
				if got := superviseRequests(t, pool, op); len(got) != 0 {
					t.Fatalf("%s rows at the merge = %v, want none", op, got)
				}
			}
			if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 0 {
				t.Fatalf("issue suspensions at the merge = %+v, want none", got)
			}
		})
	}
}

// A project whose `production_check` is off checks nothing in production, so the read after the
// merge closes its child outright: no production check record, no look and no bound row, every
// claim of the issue retired (issue_close, the tester's among them) and the issue's Sandbox deleted
// with the pod and the disk it ran on (an IssueSuspend).
func TestAnOffProjectsMergedChildCloseRetiresEveryClaimAndReleasesItsPod(t *testing.T) {
	pool := migratedPool(t)
	seedMergingChild(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour, Clock: func() time.Time { return mergeCloseNow }}, nil)
	apply := applyFacts(t, pool, engine)

	apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
	readPassing(t, apply, "LEGION-209")
	if got := issueAt(t, pool, "LEGION-209"); got != phase.Done {
		t.Fatalf("the child after the read stands in %s, want done", got)
	}
	if check := productionCheck(t, pool, "LEGION-209"); check != nil {
		t.Fatalf("the production check = %+v, want none where the project checks nothing in production", check)
	}
	for _, kind := range []string{"check_look", "check_bound"} {
		assertOutboxCount(t, pool, kind, 0)
	}
	if got := superviseRequests(t, pool, "start"); len(got) != 0 {
		t.Fatalf("start rows = %v, want none: no look was queued", got)
	}
	closed := superviseRequests(t, pool, "issue_close")
	for _, role := range claim.Roles {
		if closed["LEGION-209/"+string(role)] != 1 {
			t.Fatalf("issue_close rows = %v, want one for every claim of the issue", closed)
		}
	}
	if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 1 || got[0].Generation != 7 || got[0].TreeGeneration != 7 {
		t.Fatalf("issue suspensions = %+v, want the one row that takes the pod", got)
	}
}

// A merged root's close under a project whose `production_check` is off waits in verifying for its
// child, and the read after the child's departure closes it: it lingers its tree as any root's
// close does, with every claim of every member suspended and none spared, and no check record, no
// look and no bound row.
func TestAnOffProjectsMergedRootCloseLingersWithEveryClaimSuspended(t *testing.T) {
	rig := seedMergedRootUnder(t, "", record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
	pool := rig.pool
	assertPhase(t, pool, phase.Verifying)

	rig.apply("child-done", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.closed", Status: "done", Title: "LEGION-209", Parent: "LEGION-208", Rank: "V1"})
	assertPhase(t, pool, phase.Verifying)
	readPassing(t, rig.apply, "LEGION-208")
	assertPhase(t, pool, phase.Done)
	everyClaim := map[string]int{}
	for _, issue := range []string{"LEGION-208", "LEGION-209"} {
		for _, role := range claim.Roles {
			everyClaim[issue+"/"+string(role)]++
		}
	}
	if suspended := superviseRequests(t, pool, "suspend"); !sameCounts(suspended, everyClaim) {
		t.Fatalf("the linger suspended %v, want each tree claim once: %v", suspended, everyClaim)
	}
	if check := productionCheck(t, pool, "LEGION-208"); check != nil {
		t.Fatalf("the root's production check = %+v, want none where the project checks nothing in production", check)
	}
	for _, kind := range []string{"check_look", "check_bound"} {
		assertOutboxCount(t, pool, kind, 0)
	}
	if got := superviseRequests(t, pool, "start"); len(got) != 0 {
		t.Fatalf("start rows = %v, want none: no look was queued", got)
	}
	if got := issueSuspensions(t, pool, "LEGION-208"); len(got) != 1 {
		t.Fatalf("the root's issue suspensions = %+v, want the linger's one row", got)
	}
}

// A merge that lands while the tree lingers stands the member in verifying: its status is testing,
// nobody is told child-closed, no production check opens at the merge, and it retires nobody and
// suspends nothing: no issue_close and no IssueSuspend, which the linger's own rows already wrote and
// the tree's close completes.
func TestALingeringMembersMergeWaitsInVerifying(t *testing.T) {
	pool := migratedPool(t)
	until := mergeCloseNow.Add(time.Hour)
	seedMergingChild(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 7, Status: "done", Rank: "U", LingerUntil: &until, LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckRole: claim.RoleTester, Clock: func() time.Time { return mergeCloseNow }}, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "github", "merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact merge: %v", err)
	}
	var childPhase phase.Phase
	var childStatus string
	if err := pool.QueryRow(context.Background(), "select phase, status from issues where key = 'LEGION-209'").Scan(&childPhase, &childStatus); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if childPhase != phase.Verifying || childStatus != "testing" {
		t.Fatalf("the lingering member = %s/%s, want verifying/testing", childPhase, childStatus)
	}
	var statusWrites int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'dispatch_status' and issue = 'LEGION-209' and payload->>'status' = 'testing'`).Scan(&statusWrites); err != nil || statusWrites != 1 {
		t.Fatalf("testing status writes = %d, %v; want one", statusWrites, err)
	}
	if got := noticeKinds(t, pool, "LEGION-209"); containsNotice(got, "child-closed") {
		t.Fatalf("child notices = %v, want no child-closed: the member waits for its checks", got)
	}
	for _, op := range []string{"issue_close", "suspend", "start"} {
		if got := superviseRequests(t, pool, op); len(got) != 0 {
			t.Fatalf("%s rows = %v, want none: the linger's rows already stopped the member, and no look starts at the merge", op, got)
		}
	}
	if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("issue suspensions = %+v, want none", got)
	}
}

// A child closed as done with no pull request — a human's done on an issue short of its merge —
// has nothing to check in production: today's close, every claim closed and the Sandbox deleted
// with the pod and the disk that pod owns, and no check, no look and no bound.
func TestAChildClosedAsDoneWithNoPullRequestOpensNoCheck(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckRole: claim.RoleTester, Clock: func() time.Time { return mergeCloseNow }}, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "child-done", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.closed", Status: "done", Title: "child", Parent: "LEGION-208", Rank: "V"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact done: %v", err)
	}
	closed := superviseRequests(t, pool, "issue_close")
	for _, role := range claim.Roles {
		if closed["LEGION-209/"+string(role)] != 1 {
			t.Fatalf("issue_close rows = %v, want one for every role", closed)
		}
	}
	if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 1 {
		t.Fatalf("issue suspensions = %+v, want one row", got)
	}
	if check := productionCheck(t, pool, "LEGION-209"); check != nil {
		t.Fatalf("production check = %+v, want none for a child with no pull request", check)
	}
	for _, kind := range []string{"check_look", "check_bound"} {
		assertOutboxCount(t, pool, kind, 0)
	}
	if got := superviseRequests(t, pool, "start"); len(got) != 0 {
		t.Fatalf("start rows = %v, want none", got)
	}
}

// A root's merge while a child runs records the merge and waits in verifying: the status testing
// and no done written to Dispatch, no linger deadline and no member suspended, and its architect is
// told once, by name, which issue the close waits for. The merged root holds no slot while it
// waits, since verifying keeps none (record.KeepsSlot), and the merge takes none from the running
// child.
func TestAMergedRootWaitsWhileItsChildRuns(t *testing.T) {
	rig := seedMergedRoot(t, record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
	assertPhase(t, rig.pool, phase.Verifying)
	var lingering bool
	if err := rig.pool.QueryRow(context.Background(), "select linger_until is not null from issues where key = 'LEGION-208'").Scan(&lingering); err != nil || lingering {
		t.Fatalf("the waiting root lingers = %t, %v; want no linger deadline", lingering, err)
	}
	var state record.PullRequestState
	var mergeSHA string
	if err := rig.pool.QueryRow(context.Background(), "select state, merge_sha from pull_requests where issue = 'LEGION-208'").Scan(&state, &mergeSHA); err != nil {
		t.Fatalf("read the pull request: %v", err)
	}
	if state != record.PullRequestMerged || mergeSHA != "merge" {
		t.Fatalf("the pull request = %s at %q, want it recorded merged at merge", state, mergeSHA)
	}
	var done, testing int
	if err := rig.pool.QueryRow(context.Background(), `select count(*) filter (where payload->>'status' = 'done'), count(*) filter (where payload->>'status' = 'testing')
		from outbox where kind = 'dispatch_status' and issue = 'LEGION-208'`).Scan(&done, &testing); err != nil || done != 0 || testing != 1 {
		t.Fatalf("the root's status writes = %d done and %d testing, %v; want the one testing", done, testing, err)
	}
	assertOutboxCount(t, rig.pool, "linger_close", 0)
	for _, op := range []string{"suspend", "issue_close"} {
		if got := superviseRequests(t, rig.pool, op); len(got) != 0 {
			t.Fatalf("%s rows = %v, want none while the close waits", op, got)
		}
	}
	if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("the child's issue suspensions = %+v, want none: nothing of the tree is suspended", got)
	}
	if got := slottedIssues(t, rig.pool); len(got) != 1 || got[0] != "LEGION-209" {
		t.Fatalf("slots = %v, want the running child's alone: the merged root holds none while it waits", got)
	}
	assertWaitNotice(t, rig.pool, "LEGION-209")
}

// While its root's merged close waits, a child's tree is not lingering, so the child's implementer
// completes its phase and the child moves on. Before the wait the root's close lingered the tree
// mid-turn and that completion was refused 409 TREE_LINGERING.
func TestAWaitingRootsChildCompletesItsPhase(t *testing.T) {
	rig := seedMergedRoot(t, record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
	seedPhase(t, rig.pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: "child-implementer"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})

	result := rig.apply("child-implemented", intake.HandoffComplete{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: "child-implementer", Generation: 3, Commit: "head-43", HandoffRecord: "implement-1", HandoffVersion: 1, Summary: "implemented"})
	if result.Refusal != nil {
		t.Fatalf("the child's completion = %+v, want it accepted", result.Refusal)
	}
	if got := issueAt(t, rig.pool, "LEGION-209"); got != phase.Testing {
		t.Fatalf("the child after its completion = %s, want testing", got)
	}
	assertPhase(t, rig.pool, phase.Verifying)
}

// The architect is told once, when the wait begins. A second child leaving while a third still
// runs judges the wait again and tells it nothing more, and neither does a daemon boot that finds
// the root still waiting: notices never de-duplicate, and a root can wait for days across many
// boots.
func TestAWaitingRootTellsItsArchitectOnlyWhenTheWaitBegins(t *testing.T) {
	rig := seedMergedRoot(t,
		record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"},
		record.Issue{Key: "LEGION-210", Phase: phase.Testing, Status: "in_progress"},
		record.Issue{Key: "LEGION-211", Phase: phase.Reviewing, Status: "needs_review"})
	assertWaitNotice(t, rig.pool, "LEGION-209, LEGION-210, LEGION-211")

	rig.apply("child-parked", intake.DispatchIssue{Key: "LEGION-210", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "LEGION-210", Parent: "LEGION-208", Rank: "V2"})
	assertPhase(t, rig.pool, phase.Verifying)
	assertWaitNotice(t, rig.pool, "LEGION-209, LEGION-210, LEGION-211")

	rig.apply("boot", intake.DaemonBooted{Boot: 1})
	assertPhase(t, rig.pool, phase.Verifying)
	assertWaitNotice(t, rig.pool, "LEGION-209, LEGION-210, LEGION-211")
}

// A merged root closes once its last open child left the workflow and the read after that leave
// finds nothing else holding it, however the child leaves: that child's own merge and its own close
// on its checks, or the architect parking it to backlog (a Dispatch status event no session wrote).
// The close is the one a merge made before the wait: done written to Dispatch over testing, the
// linger armed, and every claim of the tree suspended.
func TestAMergedRootClosesWhenItsLastOpenChildLeaves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child record.Issue
		leave func(t *testing.T, rig mergedRootRig)
	}{
		{name: "the child's own merge", child: record.Issue{Key: "LEGION-209", Phase: phase.Merging, Status: "retro"}, leave: func(t *testing.T, rig mergedRootRig) {
			seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})
			rig.apply("child-merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
			rig.apply("child-read", intake.CloseChecksRead{Issue: "LEGION-209", Generation: 3, ReadAt: mergeCloseRead,
				Answer: &record.CheckReadAnswer{Results: dispatch.IssueResults{Issue: "LEGION-209"}}})
		}},
		{name: "the architect parks the child", child: record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"}, leave: func(t *testing.T, rig mergedRootRig) {
			rig.apply("child-parked", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "LEGION-209", Parent: "LEGION-208", Rank: "V1"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := seedMergedRootUnder(t, "", tc.child)
			tc.leave(t, rig)
			assertPhase(t, rig.pool, phase.Verifying)
			readPassing(t, rig.apply, "LEGION-208")
			assertPhase(t, rig.pool, phase.Done)
			var lingering bool
			if err := rig.pool.QueryRow(context.Background(), "select linger_until is not null from issues where key = 'LEGION-208'").Scan(&lingering); err != nil || !lingering {
				t.Fatalf("the closed root lingers = %t, %v; want its linger armed", lingering, err)
			}
			var statusWrites int
			if err := rig.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'dispatch_status' and issue = 'LEGION-208'
				and payload->>'status' = 'done' and payload->>'observedStatus' = 'testing'`).Scan(&statusWrites); err != nil || statusWrites != 1 {
				t.Fatalf("the root's done status writes = %d, %v; want one over testing", statusWrites, err)
			}
			assertOutboxCount(t, rig.pool, "linger_close", 1)
			// The linger suspends every claim of the tree; a child the architect parked was suspended
			// by its own departure first, so only the root's count is exactly one.
			suspended := superviseRequests(t, rig.pool, "suspend")
			for _, issue := range []string{"LEGION-208", "LEGION-209"} {
				for _, role := range claim.Roles {
					key := issue + "/" + string(role)
					if suspended[key] < 1 || issue == "LEGION-208" && suspended[key] != 1 {
						t.Fatalf("the close suspended %v, want every tree claim and the root's once", suspended)
					}
				}
			}
			assertWaitNotice(t, rig.pool, "LEGION-209")
		})
	}
}

// An issue that reaches merging on an already-merged pull request waits in verifying the same way
// while a child runs, and its architect is told there: the notice it got when the pull request
// merged could not say which issues the close waits for, since the issue had not reached merging
// then.
func TestAnIssueReachingMergingOnAMergedPullRequestWaitsForItsTree(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Retro, Generation: 1, Status: "retro", Rank: "U"})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer-claim"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
		MergeSHA: "merge", Failing: []string{}, CheckRuns: []record.AttemptRun{}})

	result := applyFacts(t, pool, mergingEngine())("retro", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer-claim", Generation: 1, Summary: "retro done", Commit: "head"})
	if result.Refusal != nil {
		t.Fatalf("the implementer's retro completion = %+v, want it accepted", result.Refusal)
	}
	assertPhase(t, pool, phase.Verifying)
	assertOutboxCount(t, pool, "linger_close", 0)
	if got := superviseRequests(t, pool, "suspend"); len(got) != 0 {
		t.Fatalf("suspend rows = %v, want none while the close waits", got)
	}
	assertWaitNotice(t, pool, "LEGION-209")
}

// Which members hold a merged root's close up: an issue is in the workflow when its status is one
// the workflow runs in and its phase is short of done, so a child in backlog and a child already
// done hold it not at all, while one held because its worker ran out of launches does. So does a
// released child that has not started a phase: it stood pinned at admitted under its root while the
// root stood in merging (record.AcceptedAncestor), and a root in verifying is no longer accepted,
// so the child is free to start and its root's close waits for it (D10).
func TestWhichChildrenHoldAMergedRootsCloseUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child record.Issue
		waits bool
	}{
		{name: "parked in backlog", child: record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "backlog"}},
		{name: "already done", child: record.Issue{Key: "LEGION-209", Phase: phase.Done, Status: "done"}},
		{name: "released, no phase started", child: record.Issue{Key: "LEGION-209", Phase: phase.Admitted, Status: "todo"}, waits: true},
		{name: "held out of launches", child: record.Issue{Key: "LEGION-209", Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing}, Status: "in_progress"}, waits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := seedMergedRoot(t, tc.child)
			assertPhase(t, rig.pool, phase.Verifying)
			var stored *record.Issue
			seedRecord(t, rig.pool, func(tx pgx.Tx) error {
				var err error
				stored, err = record.NewStore().Issue(t.Context(), tx, "LEGION-208")
				return err
			})
			wait, err := storedWaitOf(*stored)
			if err != nil {
				t.Fatal(err)
			}
			if held := len(wait.Beneath) == 1 && wait.Beneath[0].Key == "LEGION-209"; held != tc.waits {
				t.Fatalf("the stored wait's issues beneath = %+v, want LEGION-209 there %t", wait.Beneath, tc.waits)
			}
		})
	}
}

// A merged root stands in verifying for as long as its tree runs, so the facts that would take an
// open pull request back to implementing can land on a merge GitHub already made: a synchronize
// past the head the merger merged is older than the merge, since GitHub synchronizes only an open
// pull request, and moves nothing there (pullRequestSynchronized), and a red check moves nothing
// anywhere. There is nothing left to merge or to re-implement.
func TestNothingMovesAWaitingMergedRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact intake.Fact
	}{
		{name: "a push past the READY head", fact: intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "pushed"}},
		{name: "a red required check", fact: intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", Generation: 7, Snapshot: "settled",
			CheckRuns: []record.AttemptRun{{Name: "pr-checks-result", ID: 1}}, Failing: []string{"pr-checks-result"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := seedMergedRoot(t, record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
			rig.apply("moved", tc.fact)
			assertPhase(t, rig.pool, phase.Verifying)
			if got := superviseRequests(t, rig.pool, "start"); len(got) != 0 {
				t.Fatalf("start rows = %v, want none: nothing moved", got)
			}
			if got := messageBodies(t, rig.pool); len(got) != 0 {
				t.Fatalf("Dispatch messages = %q, want none: nothing moved", got)
			}
			for _, notice := range architectNotices(t, rig.pool) {
				if notice.Kind != "close-waits" {
					t.Fatalf("architect notices hold %s, want the wait's close-waits alone", notice.Kind)
				}
			}
		})
	}
}

// A merged root can stand in verifying for days, and a pull request opened from its branch in that
// time is new work, not the change the root waits to close on: GitHub has already merged that one
// and never sends the merge again. The root still closes on that merge when its last child leaves
// and the read after it passes, with pull request #42 still its record at merge.
func TestANewPullRequestFromAWaitingRootsBranchLeavesItsMergeStanding(t *testing.T) {
	rig := seedMergedRootUnder(t, "", record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
	rig.apply("follow-up-opened", intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 99, Branch: "legion/LEGION-208", HeadSHA: "follow-up",
		UpdatedAt: time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)})
	assertPhase(t, rig.pool, phase.Verifying)

	rig.apply("child-parked", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "LEGION-209", Parent: "LEGION-208", Rank: "V1"})
	readPassing(t, rig.apply, "LEGION-208")
	assertPhase(t, rig.pool, phase.Done)
	var number, snapshot int
	var merge, snapshotMerge string
	if err := rig.pool.QueryRow(context.Background(), `select p.number, p.merge_sha, i.merge_number, i.merge_sha from pull_requests p
		join issues i on i.key = p.issue where p.issue = 'LEGION-208'`).Scan(&number, &merge, &snapshot, &snapshotMerge); err != nil {
		t.Fatalf("read the root's pull request and merge snapshot: %v", err)
	}
	if number != 42 || merge != "merge" || snapshot != 42 || snapshotMerge != "merge" {
		t.Fatalf("the root's pull request = #%d at %q and its merge snapshot #%d at %q, want #42 at merge for both", number, merge, snapshot, snapshotMerge)
	}
	assertOutboxCount(t, rig.pool, "linger_close", 1)
}

// mergedRootRig is a root whose pull request merged while its tree still ran: the pool it was
// recorded in, and apply, which applies a fact and fails the test on an error.
type mergedRootRig struct {
	pool  *pgxpool.Pool
	apply func(id string, fact intake.Fact) intake.Result
}

// seedMergedRoot is seedMergedRootUnder for a project whose `production_check` names the tester,
// the setting a project that names none gets.
func seedMergedRoot(t *testing.T, children ...record.Issue) mergedRootRig {
	t.Helper()
	return seedMergedRootUnder(t, claim.RoleTester, children...)
}

// seedMergedRootUnder seeds the root LEGION-208 awaiting the merge of its open sjawhar/legion#42,
// its open tree, and one child of it per entry of children — each seeded with the key, phase,
// status and hold its entry names — then merges the root's pull request at merge, under an engine
// whose production checks are role's, and which opens none when role is empty. The slot rows are
// the ones the admission pass leaves such a tree: the root in merging stands past its review and
// holds none (record.KeepsSlot), and a child holds one where it has started in a phase that keeps
// one, which is every phase it is seeded in here but admitted.
func seedMergedRootUnder(t *testing.T, role claim.Role, children ...record.Issue) mergedRootRig {
	t.Helper()
	pool := migratedPool(t)
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckBound: 48 * time.Hour, ProductionCheckRole: role, Clock: func() time.Time { return mergeCloseNow }}, nil)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Merging, Generation: 7, Status: "retro", Rank: "U", LastDispatchSeq: 1})
	seedTreeLifecycle(t, pool, "LEGION-208")
	parent := "LEGION-208"
	slots := 0
	for i, child := range children {
		child.Repo, child.DefaultBranch, child.GitHubRepo = "sjawhar/legion", "main", "sjawhar/legion"
		child.Tree, child.Project, child.Title, child.Parent = "LEGION-208", "LEGION", child.Key, &parent
		child.Generation, child.Rank, child.LastDispatchSeq = 3, fmt.Sprintf("V%d", i+1), 1
		seedIssue(t, pool, child)
		if child.Phase != phase.Admitted && record.KeepsSlot(child) {
			slot := record.Slot{Issue: child.Key, Index: slots}
			seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutSlot(t.Context(), tx, slot) })
			slots++
		}
	}
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
	rig := mergedRootRig{pool: pool, apply: applyFacts(t, pool, engine)}
	rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"})
	return rig
}

// assertWaitNotice checks that the architect was told the close waits exactly once: one
// close-waits notice, naming the pull request and the keys the close waits for beneath the root.
// The architect's other notices — a child's close or its move out of the workflow among them — are
// not this one's business.
func assertWaitNotice(t *testing.T, pool *pgxpool.Pool, waiting string) {
	t.Helper()
	var got []record.Notice
	for _, notice := range architectNotices(t, pool) {
		if notice.Kind == "close-waits" {
			got = append(got, notice)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0].Reason, "#42") || !strings.Contains(got[0].Reason, "beneath it: "+waiting+".") {
		t.Fatalf("close-waits notices = %+v, want one naming #42 and the issues beneath it, %s", got, waiting)
	}
}

// issueAt is the phase issue stands in.
func issueAt(t *testing.T, pool *pgxpool.Pool, issue string) phase.Phase {
	t.Helper()
	var got phase.Phase
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = $1", issue).Scan(&got); err != nil {
		t.Fatalf("read %s's phase: %v", issue, err)
	}
	return got
}

// slottedIssues is the issues holding an admission slot, in slot order.
func slottedIssues(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select issue from slots order by index")
	if err != nil {
		t.Fatalf("read the slots: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect the slots: %v", err)
	}
	return got
}

// seedMergingChild seeds root, its child LEGION-209 awaiting its merge at generation 7 with status
// retro, the child's merger row and its open pull request sjawhar/legion#43.
func seedMergingChild(t *testing.T, pool *pgxpool.Pool, root record.Issue) {
	t.Helper()
	seedIssue(t, pool, root)
	parent := root.Key
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root.Key, Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Merging, Generation: 7, Status: "retro", Rank: "V", LastDispatchSeq: 1})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "child-merger", HandoffCommit: "head-43"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})
}

// productionCheck reads issue's production check, nil for none.
func productionCheck(t *testing.T, pool *pgxpool.Pool, issue string) *record.ProductionCheck {
	t.Helper()
	var check *record.ProductionCheck
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		check, err = record.NewStore().ProductionCheck(t.Context(), tx, issue)
		return err
	})
	return check
}

// outboxRow is one outbox row of an issue as the tests read it: its id, when it is due and its
// payload.
type outboxRow struct {
	id      int64
	nextAt  time.Time
	payload []byte
}

// issueOutboxOf is issue's outbox rows of kind, oldest first (outboxOf, repository_test.go, is
// every issue's of a kind).
func issueOutboxOf(t *testing.T, pool *pgxpool.Pool, issue, kind string) []outboxRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select id, next_at, payload from outbox where issue = $1 and kind = $2 order by id", issue, kind)
	if err != nil {
		t.Fatalf("read %s rows of %s: %v", kind, issue, err)
	}
	defer rows.Close()
	var got []outboxRow
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.id, &row.nextAt, &row.payload); err != nil {
			t.Fatalf("scan %s row: %v", kind, err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s rows: %v", kind, err)
	}
	return got
}

// superviseRowsOf decodes issue's supervise rows of op, oldest first, with no start passed over.
// In this package a phase worker's start is one row and carries its task (StartOrOwe); a child's
// own architect's plain start carries none (gateOpened, reviveArchitect), and the task-less starts
// admission brings back are written in internal/admit, never here.
func superviseRowsOf(t *testing.T, pool *pgxpool.Pool, issue, op string) []record.SuperviseRequest {
	t.Helper()
	var requests []record.SuperviseRequest
	for _, row := range issueOutboxOf(t, pool, issue, "supervise") {
		request := decodeSupervise(t, row)
		if string(request.Op) != op {
			continue
		}
		requests = append(requests, request)
	}
	return requests
}

// decodeSupervise is the supervise request a supervise row carries.
func decodeSupervise(t *testing.T, row outboxRow) record.SuperviseRequest {
	t.Helper()
	var request record.SuperviseRequest
	if err := json.Unmarshal(row.payload, &request); err != nil {
		t.Fatalf("decode supervise row %d: %v", row.id, err)
	}
	return request
}
