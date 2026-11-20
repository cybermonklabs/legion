// results_older_readers_test.go holds GET /api/v1/issues/{key}/results to the rule that each fact
// this slice adds reaches the answer in a field of its own, and each field a dispatch command
// released before it reads keeps its meaning there: compared_with names no branch head (branch_heads
// does), a result in results names no branch head as its basis (at_branch_head does), and results
// of a check CI proves holds the derived result alone (recorded_results holds what was recorded).
package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// olderReadersWire is the read of an issue's results as the wire carries it, as far as these rows
// read it.
type olderReadersWire struct {
	Checks []struct {
		BlockID      string `json:"block_id"`
		ComparedWith []struct {
			Kind   string `json:"kind"`
			Commit string `json:"commit"`
			Branch string `json:"branch"`
		} `json:"compared_with"`
		BranchHeads []struct {
			Kind   string `json:"kind"`
			Commit string `json:"commit"`
			Branch string `json:"branch"`
		} `json:"branch_heads"`
		Results         []olderReadersResult `json:"results"`
		RecordedResults []olderReadersResult `json:"recorded_results"`
		PassStands      struct {
			Answer string `json:"answer"`
		} `json:"pass_stands"`
	} `json:"checks"`
}

type olderReadersResult struct {
	ID           string `json:"id"`
	Role         string `json:"role"`
	Branch       string `json:"branch"`
	CommitStands *struct {
		Answer string `json:"answer"`
		Basis  *struct {
			Kind string `json:"kind"`
		} `json:"basis"`
	} `json:"commit_stands"`
	AtBranchHead *struct {
		Kind   string `json:"kind"`
		Branch string `json:"branch"`
		Commit string `json:"commit"`
	} `json:"at_branch_head"`
}

// TestEachFieldAnOlderCommandReadsKeepsItsMeaning: ACME-1 has no pull request; the job passed c1 on
// a push of legion/ACME-1 at its head, and a tester passed k1 at that head. A command released
// before this slice reads compared_with, results and each result's commit_stands: none of them
// names a branch head, and c1's results hold no recorded row. The facts are in the new fields:
// branch_heads names the head, recorded_results holds the job's result, and the tester's result
// names the head in at_branch_head. The gate's answer, pass_stands, is yes on both, as before.
func TestEachFieldAnOlderCommandReadsKeepsItsMeaning(t *testing.T) {
	rig := newCIJobRig(t)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	job := jobRecorded(t, "the job's pass of c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	tester := rig.post(t, rig.issue, rig.sessionToken(), resultBody("k1", "pass", ciJobCommit, map[string]any{"role": "tester"}))
	if tester.Code != http.StatusCreated {
		t.Fatalf("the tester's pass of k1: status=%d body=%s", tester.Code, tester.Body.String())
	}

	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+rig.issue+"/results", nil, "alice")
	var read olderReadersWire
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the read: %v (%s)", err, response.Body.String())
	}
	for _, check := range read.Checks {
		for _, basis := range check.ComparedWith {
			if basis.Kind == branchHeadKind {
				t.Errorf("%s's compared_with names the branch head %s, which an older command prints as a commit it named", check.BlockID, basis.Branch)
			}
		}
		for _, result := range check.Results {
			if result.CommitStands != nil && result.CommitStands.Basis != nil && result.CommitStands.Basis.Kind == branchHeadKind {
				t.Errorf("%s's result %s stands at a branch_head basis in commit_stands", check.BlockID, result.ID)
			}
		}
		if len(check.BranchHeads) != 1 || check.BranchHeads[0].Branch != ciJobBranch || check.BranchHeads[0].Commit != ciJobCommit {
			t.Errorf("%s's branch_heads = %+v, want the head of %s at %s", check.BlockID, check.BranchHeads, ciJobBranch, ciJobCommit)
		}
		if check.PassStands.Answer != "yes" {
			t.Errorf("%s's pass_stands = %q, want yes", check.BlockID, check.PassStands.Answer)
		}
		switch check.BlockID {
		case "c1":
			if len(check.Results) != 0 {
				t.Errorf("c1's results = %+v, want none: it has no derived result, and an older command reads results[0] as the derived one", check.Results)
			}
			if len(check.RecordedResults) != 1 || check.RecordedResults[0].ID != job.ID {
				t.Errorf("c1's recorded_results = %+v, want the job's result %s", check.RecordedResults, job.ID)
			}
		case "k1":
			if len(check.Results) != 1 || check.Results[0].AtBranchHead == nil || check.Results[0].AtBranchHead.Branch != ciJobBranch ||
				check.Results[0].CommitStands == nil || check.Results[0].CommitStands.Answer != "other_commit" {
				t.Errorf("k1's results = %+v, want the tester's pass, other_commit against compared_with, at the head of %s", check.Results, ciJobBranch)
			}
		}
	}
}
