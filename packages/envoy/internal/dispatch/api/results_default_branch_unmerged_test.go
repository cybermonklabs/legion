package api

import (
	"testing"
)

// A default_branch check does not wait for this work's merge when no repository holds one.
// Its result counts at any commit the default branch carries, without a merge requirement.
func TestADefaultBranchResultOfUnmergedWorkCountsAnyTrunkCommit(t *testing.T) {
	t.Parallel()
	rig := newAfterMergeRig(t)
	issue := rig.issue(t, "An unmerged default-branch check", afterMergeAndDefaultChecks)
	rig.github.carry("main", afterMergeC0)
	result := rig.record(t, issue, "k5", "tester", "pass", afterMergeC0)
	check := readAfterMerge(t, rig.handler, issue)["k5"]
	wantBases(t, "an unmerged default-branch check", check, branchBasis("main", ""))
	placed := check.result(t, result).CommitStands
	if placed == nil || placed.Answer != "at_basis" || placed.Basis == nil || placed.Basis.Branch != "main" ||
		placed.Basis.MergeCommit != "" || placed.Basis.Repo != "" {
		t.Fatalf("the unmerged default-branch result stands %+v, want main with no merge requirement", placed)
	}
	wantStands(t, "the unmerged default-branch pass", check.PassStands, "yes", "", []string{result}, afterMergeC0)
}
