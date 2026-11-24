// Ordinary, always-visible unit tests for the Stage 4a update-round check's pure logic that
// live_pod_update_pure_test.go defines with no e2e build tag.
package sandbox

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Both workers of pod-update's issue push that issue's branch onto the run's repository, which
// every Stage 4a run shares, so two runs at once must push different branches, and neither may
// push cache-b's. The key is a claim token's issue, the same key every time the run asks for it,
// and of the shape a later run's sweep takes a killed run's leftover by.
func TestEachRunsPodUpdateIssueNamesABranchOfItsOwn(t *testing.T) {
	runs := []string{"s4a-20261019144300-4108", "s4a-20261019144300-4109", "s4a-20261019145627-1308"}
	branches := map[string]string{}
	for _, project := range runs {
		issue := podUpdateRunIssue(project)
		if !claim.IsIssueKey(issue) {
			t.Fatalf("podUpdateRunIssue(%q) = %q, not an issue key", project, issue)
		}
		if _, err := claim.NewToken("s4a20261019144300", issue, claim.RoleArchitect); err != nil {
			t.Fatalf("podUpdateRunIssue(%q) = %q, which a claim token refuses: %v", project, issue, err)
		}
		if again := podUpdateRunIssue(project); again != issue {
			t.Fatalf("podUpdateRunIssue(%q) is %q, then %q", project, issue, again)
		}
		if cached := cacheRunIssue(project); cached == issue {
			t.Fatalf("run %s gives pod-update and cache-b the one issue %q, so they push one branch", project, issue)
		}
		branch := workspace.Bookmark(issue)
		if other, taken := branches[branch]; taken {
			t.Fatalf("runs %s and %s both push %s", other, project, branch)
		}
		branches[branch] = project
		// A run killed past its test timeout leaves this branch behind, and the next run's
		// cache-lost-volume sweep is what deletes it: that sweep takes the branch only at the
		// harness's own commit and only once it is older than any run lives.
		old := time.Now().Add(-staleRunBranchAge - time.Minute)
		left := []runBranch{{name: branch, message: runCommitMessage(branch), committed: old}}
		if stale := staleRunBranches(left, time.Now()); !slices.Equal(stale, []string{branch}) {
			t.Fatalf("a killed run's %s is swept as %q, want the branch itself: its shape is not the sweep's", branch, stale)
		}
	}
}

// The live checks write belowTheFloor as the maker of a running pod and then hold the runtime's own
// answer to one reason: a pod so recorded gives exactly one reason, of the floor's kind, which
// breaks, under the floor every daemon of this release holds at the least, the first floor.
func TestThePodUpdateChecksRecordReadsBelowTheFloorAlone(t *testing.T) {
	if belowTheFloor >= api.DaemonAPIFirstFloor {
		t.Fatalf("the checks write contract %d, want one below the first floor %d", belowTheFloor, api.DaemonAPIFirstFloor)
	}
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	holdContract(t, g, g.r, loc.Sandbox.Name, belowTheFloorRecord)
	drifted := driftReasonsOf(t, g, g.r, loc)
	if len(drifted) != 1 || drifted[0].Kind != runtime.DriftFloor || drifted[0].Kind.Drains() {
		t.Fatalf("PodDrift of a pod recorded at contract %d = %+v, want one reason of the floor's kind", belowTheFloor, drifted)
	}
}

// pod-update-fleet drifts the pods of two issues with one change, and both issues' workers push
// their own issue's branch onto the run's repository, which every Stage 4a run shares. The two
// keys must therefore be two, neither of them cache-b's, and each of the shape a later run's sweep
// takes a killed run's leftover by.
func TestTheFleetRoundsTwoIssuesNameBranchesOfTheirOwn(t *testing.T) {
	runs := []string{"s4a-20261030101500-4108", "s4a-20261030101500-4109", "s4a-20261030112233-1308"}
	branches := map[string]string{}
	for _, project := range runs {
		keys := map[string]string{
			"pod-update":       podUpdateRunIssue(project),
			"pod-update-fleet": podUpdateFleetRunIssue(project),
			"cache-b":          cacheRunIssue(project),
		}
		if keys["pod-update-fleet"] == keys["pod-update"] || keys["pod-update-fleet"] == keys["cache-b"] {
			t.Fatalf("run %s gives %v one issue key, so two checkpoints push one branch", project, keys)
		}
		if again := podUpdateFleetRunIssue(project); again != keys["pod-update-fleet"] {
			t.Fatalf("podUpdateFleetRunIssue(%q) is %q, then %q", project, keys["pod-update-fleet"], again)
		}
		for name, issue := range keys {
			if !claim.IsIssueKey(issue) {
				t.Fatalf("%s's issue in run %s is %q, not an issue key", name, project, issue)
			}
			if _, err := claim.NewToken("s4a20261030101500", issue, claim.RoleArchitect); err != nil {
				t.Fatalf("%s's issue %q is one a claim token refuses: %v", name, issue, err)
			}
			branch := workspace.Bookmark(issue)
			if other, taken := branches[branch]; taken {
				t.Fatalf("%s of run %s and %s both push %s", name, project, other, branch)
			}
			branches[branch] = name + " of run " + project
			// A run killed past its test timeout leaves this branch behind, and the next run's
			// cache-lost-volume sweep is what deletes it.
			old := time.Now().Add(-staleRunBranchAge - time.Minute)
			left := []runBranch{{name: branch, message: runCommitMessage(branch), committed: old}}
			if stale := staleRunBranches(left, time.Now()); !slices.Equal(stale, []string{branch}) {
				t.Fatalf("a killed run's %s is swept as %q, want the branch itself: its shape is not the sweep's", branch, stale)
			}
		}
	}
}
