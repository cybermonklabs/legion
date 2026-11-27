// results_merged_order_test.go holds GET /api/v1/issues/{key}/results to the order of an issue's
// merged pull requests where none of them is open: the newest is the one that merged last, by the
// time GitHub gives each merge, and not the one of the highest number. A `before_merge` check's
// one subject is that pull request's final head, and a check CI proves is read at that head alone.
// A check compared with a branch compares with that pull request's merge commit on the branch it
// merged into. Where Dispatch cannot learn when one of them merged, it cannot tell which is the
// newest: the `before_merge` check names no subject, reads no head, and its answer is not known,
// and the branch names no merge, so a result it carries is not known. The tests read the route's
// answer as JSON into the types of this file and of results_after_merge_rig_test.go, so they build
// at a commit before the rule and fail there on their own assertions.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// mergedOrderF70 and mergedOrderF71 are the final heads of #70 and #71, the two merged pull
	// requests of the tests' issue, and mergedOrderM70 and mergedOrderM71 their merge commits.
	mergedOrderF70 = "7070707070707070707070707070707070707070"
	mergedOrderF71 = "7171717171717171717171717171717171717171"
	mergedOrderM70 = "a070a070a070a070a070a070a070a070a070a070"
	mergedOrderM71 = "a071a071a071a071a071a071a071a071a071a071"
	// mergedOrderT1 and mergedOrderT2 are when GitHub says #71 and #70 merged: #71 first, though its
	// number is the higher.
	mergedOrderT1 = "2026-11-20T11:00:00Z"
	mergedOrderT2 = "2026-11-20T12:00:00Z"
)

// mergedOrderVerification is the tests' Verification document: c1, a check CI proves, and k1, a
// `before_merge` check no CI job proves.
var mergedOrderVerification = ciCheckBlock("c1", ciCheckKept) + "\n" + ciCheckBlock("k1", ciCheckOrdinary)

// mergedOrderPulls is GitHub's answer for each pull request of acme/widgets, by number, as
// GET /repos/{owner}/{repo}/pulls/{number} gives it, and 404 for a number it holds none for.
type mergedOrderPulls struct {
	mu    sync.Mutex
	pulls map[string]map[string]any
}

func (p *mergedOrderPulls) set(number int, pull map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pulls[fmt.Sprint(number)] = pull
}

func (p *mergedOrderPulls) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	pull, held := p.pulls[r.PathValue("number")]
	p.mu.Unlock()
	if !held {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return
	}
	body, err := json.Marshal(pull)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
}

// mergedPull is pull request number of acme/widgets as GitHub answers it once it merged at
// mergedAt: closed and merged, its head the branch of issue at head, its merge commit merge, and
// its title naming issue, so a read of it attributes it to issue as before.
func mergedPull(issue string, number int, head, merge, mergedAt string) map[string]any {
	return map[string]any{
		"number": number, "title": issue + ": a change", "body": "",
		"html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		"user":     map[string]any{"login": "alice"}, "labels": []any{},
		"state": "closed", "merged": true, "merged_at": mergedAt, "merge_commit_sha": merge,
		"created_at": "2026-11-20T09:00:00Z", "updated_at": mergedAt,
		"head": map[string]any{"ref": "legion/" + issue, "sha": head, "repo": map[string]any{"full_name": "acme/widgets"}},
		"base": map[string]any{"ref": "main"},
	}
}

// writeMergeWithNoTime stores pull request number of acme/widgets as merged at its final head
// head and attributed to issueKey, through delivery.UpsertPullRequestHead with no time of merge,
// and then judged by the rule before this build's, with the one update its doc asks of a test that
// needs a row of an older rule: a row the release before this one wrote, which a read that orders
// it must ask GitHub for its time once.
func writeMergeWithNoTime(t *testing.T, database *store.Store, number int, head, issueKey string) {
	t.Helper()
	attributed := issueKey
	if err := delivery.UpsertPullRequestHead(context.Background(), database.Pool, nil, delivery.PullRequestHead{
		Repo: "acme/widgets", Number: number, HeadSHA: head, HeadRef: "legion/" + issueKey,
		BaseRef: "main", State: "merged", IssueKey: &attributed, AttributionSource: "title",
		AttributionComplete: true, Title: issueKey + ": a change",
		URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
	}); err != nil {
		t.Fatalf("store the merge of acme/widgets#%d: %v", number, err)
	}
	if _, err := database.Pool.Exec(context.Background(),
		`update pull_request_heads set attribution_rule = $3 where repo = $1 and number = $2`,
		"acme/widgets", number, delivery.AttributionRuleVersion-1); err != nil {
		t.Fatalf("judge acme/widgets#%d by the rule before this build's: %v", number, err)
	}
}

// mergedOrderRig is a Dispatch whose issue of acme/widgets holds mergedOrderVerification and no
// open pull request, and two merged ones, #70 at mergedOrderF70 and #71 at mergedOrderF71, each
// stored with no time of merge (writeMergeWithNoTime). GitHub says #71 merged at mergedOrderT1
// and #70 at mergedOrderT2, and its check runs pass c1 at each head (runs).
type mergedOrderRig struct {
	handler http.Handler
	runs    *checkRunsFake
	issue   string
}

// newMergedOrderRig is a mergedOrderRig of its own for t. Where seventyGone, GitHub answers 404
// for #70, so no read can learn when it merged.
func newMergedOrderRig(t *testing.T, seventyGone bool) mergedOrderRig {
	t.Helper()
	runs := newCheckRunsFake()
	pulls := &mergedOrderPulls{pulls: map[string]map[string]any{}}
	handler, database, _ := resultsGitHubServed(t, map[string]bool{"acme/widgets": true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" }, runs.serve,
		gitHubRoute{pattern: "GET /repos/{owner}/{repo}/pulls/{number}", handler: pulls.serve},
		gitHubRoute{pattern: "GET /repos/{owner}/{repo}/pulls/{number}/commits", handler: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `[]`)
		}})
	issue := issueInRepository(t, handler, "RESMO", "acme/widgets", "Two merges").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", mergedOrderVerification)
	pulls.set(71, mergedPull(issue, 71, mergedOrderF71, mergedOrderM71, mergedOrderT1))
	if !seventyGone {
		pulls.set(70, mergedPull(issue, 70, mergedOrderF70, mergedOrderM70, mergedOrderT2))
	}
	writeMergeWithNoTime(t, database, 70, mergedOrderF70, issue)
	writeMergeWithNoTime(t, database, 71, mergedOrderF71, issue)
	runs.set(mergedOrderF70, completedRun(170, ciCheckRun, "success"))
	runs.set(mergedOrderF71, completedRun(171, ciCheckRun, "success"))
	return mergedOrderRig{handler: handler, runs: runs, issue: issue}
}

// mergedOrderCheck is one check of the results read, with only the members these tests read.
type mergedOrderCheck struct {
	CI           map[string]any `json:"ci"`
	ComparedWith []struct {
		Commit      string `json:"commit"`
		PullRequest *int   `json:"pull_request"`
		Merged      bool   `json:"merged"`
	} `json:"compared_with"`
	PassStands struct {
		Answer   string `json:"answer"`
		Reason   string `json:"reason"`
		Subjects []struct {
			Basis struct {
				Commit      string `json:"commit"`
				PullRequest *int   `json:"pull_request"`
			} `json:"basis"`
		} `json:"subjects"`
	} `json:"pass_stands"`
}

// read is each check of one results read of the rig's issue, by its block id.
func (r mergedOrderRig) read(t *testing.T) map[string]mergedOrderCheck {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/issues/"+r.issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", r.issue, response.Code, response.Body.String())
	}
	var read struct {
		Checks []struct {
			BlockID string `json:"block_id"`
			mergedOrderCheck
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s: %v", r.issue, err)
	}
	checks := map[string]mergedOrderCheck{}
	for _, check := range read.Checks {
		checks[check.BlockID] = check.mergedOrderCheck
	}
	if len(checks) != 2 {
		t.Fatalf("the read of %s holds the checks %v, want c1 and k1: %s", r.issue, checks, response.Body.String())
	}
	return checks
}

// TestTheNewestMergedSubjectIsTheLastToMerge: the issue has no open pull request and two merged
// ones stored with no time of merge, #70 and #71. GitHub says #71 merged first and #70 after it.
// k1's one subject is #70's final head, the head of the pull request that merged last, and not
// #71's, whose number is the higher.
func TestTheNewestMergedSubjectIsTheLastToMerge(t *testing.T) {
	t.Parallel()
	rig := newMergedOrderRig(t, false)
	k1 := rig.read(t)["k1"]
	if subjects := k1.PassStands.Subjects; len(subjects) != 1 || subjects[0].Basis.Commit != mergedOrderF70 {
		t.Fatalf("k1's subjects are %+v, want #70's final head %s alone: #70 merged last", subjects, mergedOrderF70)
	}
}

// TestTheCIReadWithNoOpenHeadReadsTheLastToMerge: the same two merges, and c1, a check CI proves.
// With no open pull request, the read reads one head, the final head of the pull request that
// merged last: c1's read names #70's final head, and GitHub was asked for the check runs there and
// not at #71's.
func TestTheCIReadWithNoOpenHeadReadsTheLastToMerge(t *testing.T) {
	t.Parallel()
	rig := newMergedOrderRig(t, false)
	c1 := rig.read(t)["c1"]
	if commit := c1.CI["commit"]; commit != mergedOrderF70 {
		t.Fatalf("c1's read is %v, want the read at #70's final head %s: #70 merged last", c1.CI, mergedOrderF70)
	}
	if at70, at71 := rig.runs.reads(mergedOrderF70), rig.runs.reads(mergedOrderF71); at70 == 0 || at71 != 0 {
		t.Fatalf("GitHub was asked for the check runs at #70's head %d times and at #71's %d times, want at #70's alone", at70, at71)
	}
}

// TestAMergedSubjectThatCannotBeOrderedReadsNotKnown: the same two merges, and GitHub answers 404
// for #70, so Dispatch learns no time of #70's merge and cannot tell which of the two merged last.
// With no result, k1 names no subject and answers unknown for read_failed, and compares with both
// merged heads; c1 reads no head, so its read is unread for read_failed, its answer unknown for
// read_failed, and no check runs are read. With a pass at #71's final head, the older merge, k1
// reads unknown for read_failed too: while the order is not known, a pass at a merged head is no
// pass that stands. Each case has a Dispatch of its own.
func TestAMergedSubjectThatCannotBeOrderedReadsNotKnown(t *testing.T) {
	t.Parallel()
	t.Run("no result", func(t *testing.T) {
		t.Parallel()
		rig := newMergedOrderRig(t, true)
		checks := rig.read(t)
		k1 := checks["k1"]
		if k1.PassStands.Answer != "unknown" || k1.PassStands.Reason != "read_failed" || len(k1.PassStands.Subjects) != 0 {
			t.Fatalf("k1 reads %q (reason %q) with the subjects %+v, want unknown for read_failed with no subject",
				k1.PassStands.Answer, k1.PassStands.Reason, k1.PassStands.Subjects)
		}
		merged := map[int]string{}
		for _, basis := range k1.ComparedWith {
			if basis.PullRequest != nil && basis.Merged {
				merged[*basis.PullRequest] = basis.Commit
			}
		}
		if len(k1.ComparedWith) != 2 || merged[70] != mergedOrderF70 || merged[71] != mergedOrderF71 {
			t.Fatalf("k1 compares with %+v, want the merged heads of #70 (%s) and #71 (%s)", k1.ComparedWith, mergedOrderF70, mergedOrderF71)
		}
		c1 := checks["c1"]
		if c1.CI["state"] != "unread" || c1.CI["case"] != "read_failed" || c1.PassStands.Answer != "unknown" || c1.PassStands.Reason != "read_failed" {
			t.Fatalf("c1's read is %v and it reads %q (reason %q), want unread for read_failed, and unknown for read_failed",
				c1.CI, c1.PassStands.Answer, c1.PassStands.Reason)
		}
		if reads := rig.runs.allReads(); reads != 0 {
			t.Fatalf("GitHub was asked for %d pages of check runs, want none: no head is read while the order is not known", reads)
		}
	})
	t.Run("a pass at the older merged head", func(t *testing.T) {
		t.Parallel()
		rig := newMergedOrderRig(t, true)
		recorded := recordResult(t, rig.handler, rig.issue, resultBody("k1", "pass", mergedOrderF71, map[string]any{"role": "tester"}))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("the tester's pass of k1 at #71's head: status=%d body=%s", recorded.Code, recorded.Body.String())
		}
		if k1 := rig.read(t)["k1"]; k1.PassStands.Answer != "unknown" || k1.PassStands.Reason != "read_failed" {
			t.Fatalf("with a pass at #71's final head, the older merge, k1 reads %q (reason %q), want unknown for read_failed and never yes",
				k1.PassStands.Answer, k1.PassStands.Reason)
		}
	})
}

// The commits of the order tests of a check compared with a branch. #5 merged into main as
// mergeOrderM5 at mergedOrderT2, from its final head mergeOrderF5, and #6 as mergeOrderM6 at
// mergedOrderT1, before it, from mergeOrderF6, though #6's number is the higher. mergeOrderP is a
// commit of main whose history holds M6 and not M5.
const (
	mergeOrderF5 = "f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5"
	mergeOrderF6 = "f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6"
	mergeOrderM5 = "a005a005a005a005a005a005a005a005a005a005"
	mergeOrderM6 = "a006a006a006a006a006a006a006a006a006a006"
	mergeOrderP  = "9090909090909090909090909090909090909090"
)

// mergeOrderBranchChecks is a1, a check compared with the branch the work merged into.
const mergeOrderBranchChecks = ":::check{#a1 when=\"after_merge\"}\n" +
	"The branch the work merged into answers at the merged commit.\n" +
	":::\n"

// newMergeOrderBranchRig is an afterMergeRig and its issue, which holds mergeOrderBranchChecks,
// whose GitHub answers #5 and #6 as merged into main at their merge commits and times, and says
// that main carries mergeOrderP, whose history holds M6 alone. Each test writes the two rows.
func newMergeOrderBranchRig(t *testing.T) (*afterMergeRig, string) {
	t.Helper()
	rig := newAfterMergeRig(t)
	issue := rig.issue(t, "Two merges into main", mergeOrderBranchChecks)
	rig.github.setPull(5, afterMergePull(issue, 5, mergeOrderF5, "legion/"+issue, "main", mergeOrderM5, mergedOrderT2), 0)
	rig.github.setPull(6, afterMergePull(issue, 6, mergeOrderF6, "legion/"+issue, "main", mergeOrderM6, mergedOrderT1), 0)
	rig.github.carry("main", mergeOrderP)
	rig.github.hold(mergeOrderM6, mergeOrderP)
	return rig, issue
}

// wantMergeNotKnown fails t unless a1 names main with no merge, the pass at mergeOrderP is not
// known for read_failed, and a1 answers not known for not_placed: the order of the two merges is
// not known, and a1 never answers from M6.
func wantMergeNotKnown(t *testing.T, what string, a1 afterMergeCheck, pass string) {
	t.Helper()
	wantBases(t, what, a1, branchBasis("main", ""))
	if stands := a1.result(t, pass).CommitStands; stands.Answer != "unknown" || stands.Reason != "read_failed" {
		t.Fatalf("%s: the pass at a commit that holds M6 alone stands %+v, want unknown for read_failed", what, stands)
	}
	wantStands(t, what, a1.PassStands, "unknown", "not_placed", []string{pass}, mergeOrderP)
}

// TestTwoMergesWithNoTimeAreReadInTheOrderTheyMerged: #5 and #6 are stored with no time of merge.
// The read learns both times, so the merge on main is #5's, the one that merged last, and a pass at
// a commit that holds #6's merge alone is at another commit.
func TestTwoMergesWithNoTimeAreReadInTheOrderTheyMerged(t *testing.T) {
	t.Parallel()
	rig, issue := newMergeOrderBranchRig(t)
	writeMergeWithNoTime(t, rig.database, 5, mergeOrderF5, issue)
	writeMergeWithNoTime(t, rig.database, 6, mergeOrderF6, issue)
	pass := rig.record(t, issue, "a1", "tester", "pass", mergeOrderP)
	a1 := rig.read(t, issue)["a1"]
	wantBases(t, "two merges with no time", a1, branchBasis("main", mergeOrderM5))
	if stands := a1.result(t, pass).CommitStands; stands.Answer != "other_commit" {
		t.Fatalf("the pass at a commit that holds M6 and not M5 stands %+v, want other_commit", stands)
	}
}

// TestAFilledMergeAndAnUnfilledOneAreReadInTheOrderTheyMerged: #6's row holds the time it merged,
// and #5's, which merged after it, holds none. The read learns #5's, so the merge on main is #5's,
// and not #6's, which a row with no time ordered last would make it.
func TestAFilledMergeAndAnUnfilledOneAreReadInTheOrderTheyMerged(t *testing.T) {
	t.Parallel()
	rig, issue := newMergeOrderBranchRig(t)
	writeMergeWithNoTime(t, rig.database, 5, mergeOrderF5, issue)
	sixMerged, err := time.Parse(time.RFC3339, mergedOrderT1)
	if err != nil {
		t.Fatalf("read #6's time of merge: %v", err)
	}
	attributed := issue
	if err := delivery.UpsertPullRequestHead(context.Background(), rig.database.Pool, nil, delivery.PullRequestHead{
		Repo: "acme/widgets", Number: 6, HeadSHA: mergeOrderF6, HeadRef: "legion/" + issue, BaseRef: "main",
		State: "merged", IssueKey: &attributed, AttributionSource: "title", AttributionComplete: true,
		Title: issue + ": the work", URL: "https://github.com/acme/widgets/pull/6",
		MergeCommitSHA: mergeOrderM6, MergedAt: &sixMerged,
	}); err != nil {
		t.Fatalf("store the merge of acme/widgets#6: %v", err)
	}
	wantBases(t, "a filled merge and an unfilled one", rig.read(t, issue)["a1"], branchBasis("main", mergeOrderM5))
}

// TestAMergeTimeThatCannotBeReadLeavesTheMergeNotKnown: #5 and #6 are stored with no time of
// merge, and GitHub's read of #5 fails. Dispatch cannot tell which merged last, so main names no
// merge, a result main carries is not known, and the check never answers from #6's merge.
func TestAMergeTimeThatCannotBeReadLeavesTheMergeNotKnown(t *testing.T) {
	t.Parallel()
	rig, issue := newMergeOrderBranchRig(t)
	rig.github.setPull(5, nil, http.StatusBadGateway)
	writeMergeWithNoTime(t, rig.database, 5, mergeOrderF5, issue)
	writeMergeWithNoTime(t, rig.database, 6, mergeOrderF6, issue)
	pass := rig.record(t, issue, "a1", "tester", "pass", mergeOrderP)
	wantMergeNotKnown(t, "#5's time cannot be read", rig.read(t, issue)["a1"], pass)
}

// TestAMergeGitHubNoLongerAnswersLeavesTheOrderNotKnown: #5 and #6 are stored with no time of
// merge, and #5's row is stamped at this build's rule, as the reconcile stamps a pull request
// GitHub answers 404 for (delivery.MarkPullRequestHeadChecked). The read does not ask GitHub for
// #5 again, and the order stays not known: main names no merge, and the check never answers from
// #6's merge.
func TestAMergeGitHubNoLongerAnswersLeavesTheOrderNotKnown(t *testing.T) {
	t.Parallel()
	rig, issue := newMergeOrderBranchRig(t)
	writeMergeWithNoTime(t, rig.database, 5, mergeOrderF5, issue)
	writeMergeWithNoTime(t, rig.database, 6, mergeOrderF6, issue)
	if err := delivery.MarkPullRequestHeadChecked(context.Background(), rig.database.Pool, "acme/widgets", 5, time.Now()); err != nil {
		t.Fatalf("stamp #5 at this build's rule: %v", err)
	}
	pass := rig.record(t, issue, "a1", "tester", "pass", mergeOrderP)
	wantMergeNotKnown(t, "#5 stamped with no time", rig.read(t, issue)["a1"], pass)
	if reads := rig.github.pullReadsOf(5); reads != 0 {
		t.Fatalf("the read asked GitHub for #5 %d times, want none: the current rule already offered it", reads)
	}
}
