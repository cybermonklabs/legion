// results_branch_newest_test.go holds GET /api/v1/issues/{key}/results to R19 for a check compared
// with a branch, an `after_merge` or a `default_branch` check: the newest result that counts
// decides, at a commit that holds the merge of the work. A pass and a fail that count at one
// commit make the answer not known, in either order, and a later pass there does not end it;
// across commits the newest result decides by its verdict; a newest result Dispatch could not
// place makes the answer not known. The answer names the results that decide it and their commit.
// The test reads the route's answer as JSON into the types of results_after_merge_rig_test.go, so
// it builds at a commit before the rule and fails there on its own assertion.
package api

import (
	"slices"
	"testing"
)

// branchNewestChecks is c9, a check compared with the branch the work merged into, and d9, a
// check compared with the default branch alone.
const branchNewestChecks = ":::check{#c9 when=\"after_merge\"}\n" +
	"Production runs the image of the merged commit.\n" +
	":::\n\n" +
	":::check{#d9 when=\"default_branch\"}\n" +
	"The trunk builds at the merged commit.\n" +
	":::\n"

// The commits of these tests. branchNewestC is the commit the issue's pull request merged into
// main as, and branchNewestH that pull request's final head. X, Y and Z are three later commits of
// main, each of whose history holds C. branchNewestW is a commit whose compare read fails.
const (
	branchNewestC = "1d0adea3bdada61b2c393bfd3bff5888093cbe8d"
	branchNewestH = "1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e"
	branchNewestX = "7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a"
	branchNewestY = "7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b"
	branchNewestZ = "7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c"
	branchNewestW = "7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d"
)

// branchNewestStands is a check's pass_stands, with only the members this test reads.
type branchNewestStands = afterMergeStands

// branchNewestRig is a Dispatch whose issue's pull request #31 merged into main as branchNewestC,
// and whose GitHub says main carries C, X, Y and Z, that the history of X, Y and Z holds C, and
// answers 502 for each compare read of branchNewestW.
type branchNewestRig struct {
	rig   *afterMergeRig
	issue string
}

func newBranchNewestRig(t *testing.T) branchNewestRig {
	t.Helper()
	rig := newAfterMergeRig(t)
	issue := rig.issue(t, "A check of the deploy", branchNewestChecks)
	rig.github.carry("main", branchNewestC, branchNewestX, branchNewestY, branchNewestZ)
	rig.github.hold(branchNewestC, branchNewestX, branchNewestY, branchNewestZ)
	rig.github.fail(branchNewestW)
	rig.github.setPull(31, afterMergePull(issue, 31, branchNewestH, "legion/"+issue, "main", branchNewestC, "2026-11-20T12:00:00Z"), 0)
	writeHeadOnBase(t, rig.database, "acme/widgets", 31, branchNewestH, "merged", issue, "main")
	return branchNewestRig{rig: rig, issue: issue}
}

// record records role's verdict of check at commit, and answers the stored result's id.
func (r branchNewestRig) record(t *testing.T, check, role, verdict, commit string) string {
	t.Helper()
	return r.rig.record(t, r.issue, check, role, verdict, commit)
}

// stands is check's pass_stands as the results read answers it.
func (r branchNewestRig) stands(t *testing.T, check string) branchNewestStands {
	t.Helper()
	read, held := r.rig.read(t, r.issue)[check]
	if !held {
		t.Fatalf("the read of %s holds no check %s", r.issue, check)
	}
	return read.PassStands
}

// TestTheNewestResultDecidesACheckComparedWithABranch: R19 and R30. The newest result that counts
// decides a check compared with a branch, and the answer names the results that decide it and
// their commit.
func TestTheNewestResultDecidesACheckComparedWithABranch(t *testing.T) {
	t.Parallel()
	// The case production printed as a pass from tester: the tester's pass at the merged commit,
	// then two fails at the same commit. A commit holds one answer, so a pass and a fail there make
	// it not known, named by the pass and the newer fail.
	t.Run("a pass then two fails", func(t *testing.T) {
		t.Parallel()
		rig := newBranchNewestRig(t)
		pass := rig.record(t, "c9", "tester", "pass", branchNewestC)
		rig.record(t, "c9", "tester", "fail", branchNewestC)
		newer := rig.record(t, "c9", "tester", "fail", branchNewestC)
		if got := rig.stands(t, "c9"); got.Answer != "unknown" || got.Reason != "pass_and_fail" ||
			!slices.Equal(got.Results, []string{pass, newer}) || got.Commit != branchNewestC {
			t.Fatalf("c9 answers %+v, want unknown for pass_and_fail, named by the pass %s and the newer fail %s at %s", got, pass, newer, branchNewestC)
		}
	})
	// The other order reads the same, and a later pass at that commit does not end it.
	t.Run("a fail then a pass", func(t *testing.T) {
		t.Parallel()
		rig := newBranchNewestRig(t)
		fail := rig.record(t, "c9", "tester", "fail", branchNewestC)
		pass := rig.record(t, "c9", "tester", "pass", branchNewestC)
		if got := rig.stands(t, "c9"); got.Answer != "unknown" || got.Reason != "pass_and_fail" ||
			!slices.Equal(got.Results, []string{pass, fail}) || got.Commit != branchNewestC {
			t.Fatalf("c9 answers %+v, want unknown for pass_and_fail, named by the pass %s and the fail %s at %s", got, pass, fail, branchNewestC)
		}
		later := rig.record(t, "c9", "tester", "pass", branchNewestC)
		if got := rig.stands(t, "c9"); got.Answer != "unknown" || got.Reason != "pass_and_fail" ||
			!slices.Equal(got.Results, []string{later, fail}) || got.Commit != branchNewestC {
			t.Fatalf("after a later pass c9 answers %+v, want unknown for pass_and_fail, named by the later pass %s and the fail %s at %s", got, later, fail, branchNewestC)
		}
	})
	// Over three commits of main that hold the merge, the newest result decides.
	t.Run("three commits", func(t *testing.T) {
		t.Parallel()
		rig := newBranchNewestRig(t)
		rig.record(t, "c9", "tester", "pass", branchNewestX)
		rig.record(t, "c9", "tester", "fail", branchNewestY)
		atZ := rig.record(t, "c9", "tester", "pass", branchNewestZ)
		if got := rig.stands(t, "c9"); got.Answer != "yes" || got.Reason != "" || !slices.Equal(got.Roles, []string{"tester"}) ||
			!slices.Equal(got.Results, []string{atZ}) || got.Commit != branchNewestZ {
			t.Fatalf("c9 answers %+v, want yes from tester, decided by the pass at Z %s", got, atZ)
		}
	})
	t.Run("default_branch", func(t *testing.T) {
		t.Parallel()
		rig := newBranchNewestRig(t)
		rig.record(t, "d9", "tester", "pass", branchNewestX)
		atY := rig.record(t, "d9", "tester", "fail", branchNewestY)
		if got := rig.stands(t, "d9"); got.Answer != "no" || got.Reason != "fail" ||
			!slices.Equal(got.Results, []string{atY}) || got.Commit != branchNewestY {
			t.Fatalf("d9 answers %+v, want no for fail, decided by the fail at Y %s", got, atY)
		}
	})
	t.Run("newest not placed", func(t *testing.T) {
		t.Parallel()
		rig := newBranchNewestRig(t)
		rig.record(t, "c9", "tester", "pass", branchNewestC)
		atW := rig.record(t, "c9", "tester", "fail", branchNewestW)
		if got := rig.stands(t, "c9"); got.Answer != "unknown" || got.Reason != "not_placed" ||
			!slices.Equal(got.Results, []string{atW}) || got.Commit != branchNewestW {
			t.Fatalf("c9 answers %+v, want unknown for not_placed, as Dispatch could not place the newest result %s", got, atW)
		}
	})
}
