// results_after_merge_test.go holds GET /api/v1/issues/{key}/results to R36 for a check compared
// with a branch: a result counts only at a commit that holds the merge of the work on that branch,
// in the issue's own repository and in each repository where the issue links a merged pull
// request. The merge on the branch a pull request merged into is its own merge commit, and on the
// default branch, where it merged into another branch, the merge commit of the pull request that
// landed that branch there. A merge Dispatch does not hold is read from GitHub once, and one it
// cannot learn leaves the answer not known. The tests read the route's answer as JSON into the
// types of results_after_merge_rig_test.go.
package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// afterMergeChecks is k5, a check compared with the branch the work merged into, and k6, a check
// compared with the default branch alone.
const afterMergeChecks = ":::check{#k5 when=\"after_merge\"}\n" +
	"Production answers at the merged commit.\n" +
	":::\n\n" +
	":::check{#k6 when=\"default_branch\"}\n" +
	"The trunk answers at the merged commit.\n" +
	":::\n"

// The commits of v-after-merge. #70 merged into main as afterMergeM from its final head
// afterMergeH1, which main does not carry. afterMergeC0 is a commit of main before M, and
// afterMergeC1 one after it, whose history holds M.
const (
	afterMergeM  = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
	afterMergeH1 = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	afterMergeC0 = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"
	afterMergeC1 = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	// afterMergeT is when GitHub says #70 merged.
	afterMergeT = "2026-11-20T12:00:00Z"
)

// mergedSeventy gives rig's issue the pull request #70, merged into base as merge from its final
// head afterMergeH1: the row writeHeadOnBase writes, which holds no merge commit, and GitHub's
// answer for it, which names the merge commit and afterMergeT.
func mergedSeventy(t *testing.T, rig *afterMergeRig, issue, base, merge string) {
	t.Helper()
	rig.github.setPull(70, afterMergePull(issue, 70, afterMergeH1, "legion/"+issue, base, merge, afterMergeT), 0)
	writeHeadOnBase(t, rig.database, "acme/widgets", 70, afterMergeH1, "merged", issue, base)
}

// wantStands fails t unless stands is answer, reason, results and commit, what naming the step.
func wantStands(t *testing.T, what string, stands afterMergeStands, answer, reason string, results []string, commit string) {
	t.Helper()
	if stands.Answer != answer || stands.Reason != reason || !slices.Equal(stands.Results, results) || stands.Commit != commit {
		t.Fatalf("%s: the check answers %+v, want %s (reason %q) with the results %v at the commit %q",
			what, stands, answer, reason, results, commit)
	}
}

// wantBases fails t unless the check compares with exactly want, what naming the read.
func wantBases(t *testing.T, what string, check afterMergeCheck, want ...afterMergeBasis) {
	t.Helper()
	if check.ComparedNothing != "" || !slices.Equal(check.ComparedWith, want) {
		t.Fatalf("%s: the check compares with %+v (compared_nothing %q), want %+v", what, check.ComparedWith, check.ComparedNothing, want)
	}
}

// branchBasis is a branch basis of the issue's own repository, with the merge it must hold.
func branchBasis(branch, merge string) afterMergeBasis {
	return afterMergeBasis{Kind: "branch", Branch: branch, MergeCommit: merge}
}

// TestAnAfterMergeResultCountsOnlyAtACommitThatHoldsTheMerge: v-after-merge, cases 1, 2 and 3. An
// `after_merge` check counts a result only at a commit of the branch that holds the merge, and the
// newest that counts decides; a `default_branch` check of an issue with no merged pull request
// counts any commit of the default branch. It is serial: its live case raises the block schema's
// manifest, which parallel tests read.
func TestAnAfterMergeResultCountsOnlyAtACommitThatHoldsTheMerge(t *testing.T) {
	rig := newAfterMergeRig(t)
	issue := rig.issue(t, "A merged change", afterMergeChecks)
	rig.github.carry("main", afterMergeC0, afterMergeM, afterMergeC1)
	rig.github.hold(afterMergeM, afterMergeC1)
	mergedSeventy(t, rig, issue, "main", afterMergeM)
	k5 := func() afterMergeCheck { t.Helper(); return rig.read(t, issue)["k5"] }

	wantStands(t, "no result", k5().PassStands, "no", "no_result", nil, "")
	atC0 := rig.record(t, issue, "k5", "tester", "pass", afterMergeC0)
	read := k5()
	if stands := read.result(t, atC0).CommitStands; stands.Answer != "other_commit" {
		t.Fatalf("the pass at C0, a commit of main before the merge, stands %+v, want other_commit", stands)
	}
	wantStands(t, "a pass at C0 alone", read.PassStands, "no", "no_result", nil, "")
	rig.record(t, issue, "k5", "tester", "pass", afterMergeH1)
	wantStands(t, "and a pass at H1, #70's head", k5().PassStands, "no", "no_result", nil, "")
	atM := rig.record(t, issue, "k5", "tester", "pass", afterMergeM)
	read = k5()
	wantBases(t, "after the pass at M", read, branchBasis("main", afterMergeM))
	wantStands(t, "a pass at M", read.PassStands, "yes", "", []string{atM}, afterMergeM)
	if asked := rig.github.comparesOf(afterMergeM, afterMergeM); asked != 0 {
		t.Fatalf("GitHub was asked whether M holds M %d times, want never: a commit holds itself", asked)
	}
	failAtC1 := rig.record(t, issue, "k5", "tester", "fail", afterMergeC1)
	wantStands(t, "a later fail at C1", k5().PassStands, "no", "fail", []string{failAtC1}, afterMergeC1)
	passAtC1 := rig.record(t, issue, "k5", "tester", "pass", afterMergeC1)
	wantStands(t, "a pass at C1 after that fail", k5().PassStands, "unknown", "pass_and_fail", []string{passAtC1, failAtC1}, afterMergeC1)
	secondAtC1 := rig.record(t, issue, "k5", "reviewer", "pass", afterMergeC1)
	wantStands(t, "a second pass at C1", k5().PassStands, "unknown", "pass_and_fail", []string{secondAtC1, failAtC1}, afterMergeC1)

	// Case 2: a late observation names an old commit, not the program's current deploy.
	restore := useTheNextReleasesManifest(t)
	liveIssue := rig.issue(t, "A live observation after its deploy", ":::check{#live when=\"after_merge\" subject=\"production\" program=\"renderer\"}\nThe running program answers.\n:::\n")
	restore()
	writeKnownMergeOnBase(t, rig.database, "acme/widgets", 71, afterMergeH1, liveIssue, "main", afterMergeM)
	current := postDeploy(t, rig.handler, deployBody("renderer", afterMergeC1), http.StatusCreated)
	latePass := rig.record(t, liveIssue, "live", "tester", "pass", afterMergeC0)
	live := readLiveCheck(t, rig.handler, liveIssue, "")
	requireNotCounted(t, live, latePass, "it names a commit that the current deploy of the program this check reads does not run")
	requireLiveDecision(t, "a late pass at an old commit", live, "unknown", "no_result_from_deploy", "", &current)

	// Case 3: an issue with no pull request at all. Its `default_branch` check names main with no
	// merge to hold, so a pass at a commit of main counts.
	unmerged := rig.issue(t, "A change with no pull request", afterMergeChecks)
	atC1 := rig.record(t, unmerged, "k6", "tester", "pass", afterMergeC1)
	k6 := rig.read(t, unmerged)["k6"]
	wantBases(t, "the default_branch check of an issue with no pull request", k6, branchBasis("main", ""))
	wantStands(t, "case 3, a pass at C1", k6.PassStands, "yes", "", []string{atC1}, afterMergeC1)
}

// TestAnIntegrationBranchReachesTheDefaultBranchThroughItsLanding: #70 merged into the
// integration branch legion/ACME-0 as M1, and #5, whose head is that branch, merged it into main
// as M2 from a final head that holds M1. The `after_merge` check compares with legion/ACME-0 at
// M1 and with main at M2, and a pass at a commit of main that holds M2 counts. Before #5 merges,
// main has no merge of the work, and the check compares with legion/ACME-0 alone.
func TestAnIntegrationBranchReachesTheDefaultBranchThroughItsLanding(t *testing.T) {
	t.Parallel()
	const (
		integration = "legion/ACME-0"
		m1          = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
		m2          = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
		f5          = "f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5"
		onMain      = "b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3"
	)
	rig := newAfterMergeRig(t)
	issue := rig.issue(t, "Work on an integration branch", afterMergeChecks)
	rig.github.carry(integration, m1, f5)
	rig.github.carry("main", m2, onMain)
	rig.github.hold(m1, f5, m2, onMain)
	rig.github.hold(m2, onMain)
	mergedSeventy(t, rig, issue, integration, m1)
	wantBases(t, "before #5 merges", rig.read(t, issue)["k5"], branchBasis(integration, m1))

	// #5 lands the integration branch: a pull request of no issue, whose row holds its time and no
	// merge commit, which GitHub's answer gives.
	mergedAt := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	rig.github.setPull(5, afterMergePull("ACME-0", 5, f5, integration, "main", m2, mergedAt.Format(time.RFC3339)), 0)
	if err := delivery.NewAttributor(refs.Renames{}).UpsertPullRequestHead(context.Background(), rig.database.Pool, nil, delivery.PullRequestHead{
		Repo: "acme/widgets", Number: 5, HeadSHA: f5, HeadRef: integration, BaseRef: "main", State: "merged",
		Title: "Land the integration branch", URL: "https://github.com/acme/widgets/pull/5", MergedAt: &mergedAt,
	}); err != nil {
		t.Fatalf("store the merge of #5: %v", err)
	}
	pass := rig.record(t, issue, "k5", "tester", "pass", onMain)
	k5 := rig.read(t, issue)["k5"]
	wantBases(t, "after #5 merged", k5, branchBasis(integration, m1), branchBasis("main", m2))
	if stands := k5.result(t, pass).CommitStands; stands.Answer != "at_basis" || stands.Basis == nil || *stands.Basis != branchBasis("main", m2) {
		t.Fatalf("the pass at a commit of main that holds M2 stands %+v, want at_basis on main at M2", stands)
	}
	wantStands(t, "a pass at a commit of main that holds M2", k5.PassStands, "yes", "", []string{pass}, onMain)
}

// TestAResultBeforeTheLinkedMergeDoesNotCount: an issue whose only merge is acme/tools#9, a pull
// request it links, merged into acme/tools' main as toolsMerge. Its `after_merge` check compares
// with its own main, which holds no merge of the work, and with acme/tools' main at toolsMerge,
// before any result of acme/tools exists. A result at a commit of acme/tools' main counts only
// where it holds toolsMerge, and toolsMerge holds itself with no read. An issue with no merge
// anywhere compares with nothing for not_merged. An issue with a merge of its own and the linked
// one walks its own results in its own repository alone.
func TestAResultBeforeTheLinkedMergeDoesNotCount(t *testing.T) {
	t.Parallel()
	const (
		toolsEarly   = "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"
		toolsMerge   = "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"
		toolsLate    = "d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2"
		toolsHead    = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
		widgetsEarly = "e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0"
		widgetsMerge = "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"
		widgetsHead  = "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3"
		toolsPull    = "https://github.com/acme/tools/pull/9"
	)
	linked := &linkedGitHub{
		held: map[string][]string{
			"acme/tools":   {toolsEarly, toolsMerge, toolsLate, toolsHead},
			"acme/widgets": {widgetsEarly, widgetsMerge, widgetsHead},
		},
		pulls: map[string]string{"acme/tools#9": "merged", "acme/widgets#3": "merged"},
		merged: map[string]linkedMerge{
			"acme/tools#9": {title: "The second half of the work", headRef: "work", headSHA: toolsHead, base: "main",
				mergeCommit: toolsMerge, mergedAt: "2026-11-20T12:00:00Z"},
		},
		branches: map[string][]string{
			"acme/tools:main":         {toolsEarly, toolsMerge, toolsLate},
			"acme/tools:" + toolsLate: {toolsMerge},
			"acme/widgets:main":       {widgetsEarly, widgetsMerge},
		},
	}
	handler, database := linkedRepositoriesGitHub(t, linked)
	onlyLinked := issueInRepository(t, handler, afterMergeProject, "acme/widgets", "Work merged in the second repository").Key
	uploadDocument(t, handler, "/api/v1/issues/"+onlyLinked, "verification.md", afterMergeAndDefaultChecks)
	linkPullRequests(t, handler, onlyLinked, toolsPull)
	writeHeadOnBase(t, database, "acme/tools", 9, toolsHead, "merged", "", "main")

	bases := []afterMergeBasis{branchBasis("main", ""), {Kind: "branch", Repo: "acme/tools", Branch: "main", MergeCommit: toolsMerge}}
	toolsMain := bases[1]
	k4 := func() afterMergeCheck {
		t.Helper()
		check := readAfterMerge(t, handler, onlyLinked)["k4"]
		wantBases(t, "k4", check, bases...)
		return check
	}
	k4()
	early := recordAfterMerge(t, handler, onlyLinked, "k4", "tester", "pass", toolsEarly)
	read := k4()
	if stands := read.result(t, early).CommitStands; stands.Answer != "other_commit" || stands.Repo != "acme/tools" {
		t.Fatalf("the pass at toolsEarly, before the linked merge, stands %+v, want other_commit naming acme/tools", stands)
	}
	wantStands(t, "a pass before the linked merge", read.PassStands, "no", "no_result", nil, "")
	atMerge := recordAfterMerge(t, handler, onlyLinked, "k4", "tester", "pass", toolsMerge)
	read = k4()
	if stands := read.result(t, atMerge).CommitStands; stands.Answer != "at_basis" || stands.Basis == nil || *stands.Basis != toolsMain {
		t.Fatalf("the pass at toolsMerge stands %+v, want at_basis on acme/tools' main at toolsMerge", stands)
	}
	wantStands(t, "a pass at the linked merge", read.PassStands, "yes", "", []string{atMerge}, toolsMerge)
	late := recordAfterMerge(t, handler, onlyLinked, "k4", "tester", "pass", toolsLate)
	if stands := k4().result(t, late).CommitStands; stands.Answer != "at_basis" || stands.Basis == nil || *stands.Basis != toolsMain {
		t.Fatalf("the pass at toolsLate, after the linked merge, stands %+v, want at_basis on acme/tools' main", stands)
	}

	unmerged := issueOfProject(t, handler, afterMergeProject, "Work merged nowhere", afterMergeAndDefaultChecks)
	if check := readAfterMerge(t, handler, unmerged)["k4"]; len(check.ComparedWith) != 0 || check.ComparedNothing != "not_merged" ||
		check.PassStands.Answer != "no" {
		t.Fatalf("the after_merge check of an issue with no merge anywhere compares with %+v for %q and answers %+v, want nothing for not_merged, and no",
			check.ComparedWith, check.ComparedNothing, check.PassStands)
	}

	// One pull request belongs to one issue. The third issue therefore needs its own pull
	// request, with the same merge, rather than taking the first issue's link.
	linked.mu.Lock()
	linked.pulls["acme/tools#10"] = "merged"
	linked.merged["acme/tools#10"] = linked.merged["acme/tools#9"]
	linked.mu.Unlock()
	both := issueOfProject(t, handler, afterMergeProject, "Work merged in both repositories", afterMergeAndDefaultChecks)
	linkPullRequests(t, handler, both, "https://github.com/acme/tools/pull/10")
	writeHeadOnBase(t, database, "acme/tools", 10, toolsHead, "merged", "", "main")
	linked.mu.Lock()
	linked.merged["acme/widgets#3"] = linkedMerge{title: both + ": the work", headRef: "legion/" + both, headSHA: widgetsHead,
		base: "main", mergeCommit: widgetsMerge, mergedAt: "2026-11-21T12:00:00Z"}
	linked.mu.Unlock()
	writeHeadOnBase(t, database, "acme/widgets", 3, widgetsHead, "merged", both, "main")
	atOwnMerge := recordAfterMerge(t, handler, both, "k4", "tester", "pass", widgetsMerge)
	beforeOwnMerge := recordAfterMerge(t, handler, both, "k4", "tester", "pass", widgetsEarly)
	toolsBefore := linked.compareReads("acme/tools")
	check := readAfterMerge(t, handler, both)["k4"]
	wantBases(t, "the issue merged in both repositories", check, branchBasis("main", widgetsMerge), toolsMain)
	if stands := check.result(t, atOwnMerge).CommitStands; stands.Answer != "at_basis" || stands.Basis == nil || *stands.Basis != branchBasis("main", widgetsMerge) {
		t.Fatalf("the pass at widgetsMerge stands %+v, want at_basis on the issue's own main at widgetsMerge", stands)
	}
	if stands := check.result(t, beforeOwnMerge).CommitStands; stands.Answer != "other_commit" || stands.Repo != "" {
		t.Fatalf("the pass at widgetsEarly, before the own merge, stands %+v, want other_commit of the issue's own repository", stands)
	}
	if asked := linked.compareReads("acme/tools") - toolsBefore; asked != 0 {
		t.Fatalf("the read of the issue's own results asked acme/tools %d compare reads, want none: an own result walks its own repository's bases alone", asked)
	}
}

// TestAMergeCommitDispatchHasNotStoredIsReadOnce: #70's row holds no merge commit. The first read
// refreshes the row from GitHub once, which stores the merge commit and the time GitHub gives,
// and names the merge; a second read asks GitHub nothing.
func TestAMergeCommitDispatchHasNotStoredIsReadOnce(t *testing.T) {
	t.Parallel()
	rig := newAfterMergeRig(t)
	issue := rig.issue(t, "A merge with no stored commit", afterMergeChecks)
	mergedSeventy(t, rig, issue, "main", afterMergeM)
	wantBases(t, "the first read", rig.read(t, issue)["k5"], branchBasis("main", afterMergeM))
	if reads := rig.github.pullReadsOf(70); reads != 1 {
		t.Fatalf("the first read asked GitHub for #70 %d times, want once", reads)
	}
	var mergeCommit string
	var mergedAt *time.Time
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select merge_commit_sha, merged_at from pull_request_heads where repo = 'acme/widgets' and number = 70`).Scan(&mergeCommit, &mergedAt); err != nil {
		t.Fatalf("read #70's row: %v", err)
	}
	if want, _ := time.Parse(time.RFC3339, afterMergeT); mergeCommit != afterMergeM || mergedAt == nil || !mergedAt.Equal(want) {
		t.Fatalf("#70's row holds the merge commit %q merged at %v, want %s merged at %s", mergeCommit, mergedAt, afterMergeM, afterMergeT)
	}
	wantBases(t, "the second read", rig.read(t, issue)["k5"], branchBasis("main", afterMergeM))
	if reads := rig.github.pullReadsOf(70); reads != 1 {
		t.Fatalf("after a second read GitHub was asked for #70 %d times, want once", reads)
	}
}

// TestAMergeThatCannotBeLearnedReadsNotKnown: #70's row holds no merge commit, and the refresh
// cannot give it one: GitHub's read fails, or its answer names none. The branch names no merge, a
// result at a commit main carries is not known for read_failed, and the check answers not known
// for not_placed, named by that result.
func TestAMergeThatCannotBeLearnedReadsNotKnown(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		answer func(rig *afterMergeRig, issue string)
	}{
		{"GitHub's read fails", func(rig *afterMergeRig, _ string) { rig.github.setPull(70, nil, 502) }},
		{"GitHub names no merge commit", func(rig *afterMergeRig, issue string) {
			rig.github.setPull(70, afterMergePull(issue, 70, afterMergeH1, "legion/"+issue, "main", "", afterMergeT), 0)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rig := newAfterMergeRig(t)
			issue := rig.issue(t, "A merge Dispatch cannot learn", afterMergeChecks)
			rig.github.carry("main", afterMergeC1)
			writeHeadOnBase(t, rig.database, "acme/widgets", 70, afterMergeH1, "merged", issue, "main")
			row.answer(rig, issue)
			pass := rig.record(t, issue, "k5", "tester", "pass", afterMergeC1)
			k5 := rig.read(t, issue)["k5"]
			wantBases(t, row.name, k5, branchBasis("main", ""))
			if stands := k5.result(t, pass).CommitStands; stands.Answer != "unknown" || stands.Reason != "read_failed" {
				t.Fatalf("%s: the pass at a commit of main stands %+v, want unknown for read_failed", row.name, stands)
			}
			wantStands(t, row.name, k5.PassStands, "unknown", "not_placed", []string{pass}, afterMergeC1)
		})
	}
}
