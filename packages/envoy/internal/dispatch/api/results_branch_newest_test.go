// results_branch_newest_test.go holds GET /api/v1/issues/{key}/results to R5: an `after_merge`
// or `default_branch` check takes the newest result that counts at a branch, or that Dispatch could
// not place on one. A pass stands only when that result is a pass that counts, and a result Dispatch
// could not place makes the answer not known. The test reads the route's answer as JSON into the
// types of this file, so it builds at a commit before the rule and fails there on its own assertion.
package api

import (
	"encoding/json"
	"net/http"
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

// The commits of these tests. branchNewestC is the head the issue's pull request merged at, and
// X, Y and Z three more commits that main holds. branchNewestW is a commit whose compare read
// fails.
const (
	branchNewestC = "1d0adea3bdada61b2c393bfd3bff5888093cbe8d"
	branchNewestX = "7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a"
	branchNewestY = "7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b"
	branchNewestZ = "7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c"
	branchNewestW = "7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d"
)

// branchNewestStands is a check's pass_stands, with only the members this test reads.
type branchNewestStands struct {
	Answer  string   `json:"answer"`
	Reason  string   `json:"reason"`
	Roles   []string `json:"roles"`
	Results []string `json:"results"`
}

// branchNewestRig is a Dispatch whose issue's pull request merged into main at branchNewestC, and
// whose GitHub says main holds each commit but branchNewestW, whose compare read answers 502.
type branchNewestRig struct {
	handler http.Handler
	issue   string
}

func newBranchNewestRig(t *testing.T) branchNewestRig {
	t.Helper()
	handler, database := resultsGitHubAnswering(t, map[string]bool{"acme/widgets": true}, nil, "main",
		func(_, head string) (int, string) {
			if head == branchNewestW {
				return http.StatusBadGateway, ""
			}
			return http.StatusOK, "behind"
		})
	issue := issueInRepository(t, handler, "RESNW", "acme/widgets", "A check of the deploy")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", branchNewestChecks)
	writeHeadOnBase(t, database, "acme/widgets", 31, branchNewestC, "merged", issue.Key, "main")
	return branchNewestRig{handler: handler, issue: issue.Key}
}

// record records role's verdict of check at commit, and answers the stored result's id.
func (r branchNewestRig) record(t *testing.T, check, role, verdict, commit string) string {
	t.Helper()
	fields := map[string]any{"role": role}
	if verdict == "fail" {
		fields["failed"] = 1
	}
	response := recordResult(t, r.handler, r.issue, resultBody(check, verdict, commit, fields))
	if response.Code != http.StatusCreated {
		t.Fatalf("record %s's %s of %s at %s: status=%d body=%s", role, verdict, check, commit, response.Code, response.Body.String())
	}
	var recorded struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &recorded); err != nil || recorded.Result.ID == "" {
		t.Fatalf("decode the recorded %s: %v (body %s)", verdict, err, response.Body.String())
	}
	return recorded.Result.ID
}

// stands is check's pass_stands as the results read answers it.
func (r branchNewestRig) stands(t *testing.T, check string) branchNewestStands {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/issues/"+r.issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", r.issue, response.Code, response.Body.String())
	}
	var read struct {
		Checks []struct {
			BlockID    string             `json:"block_id"`
			PassStands branchNewestStands `json:"pass_stands"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s: %v", r.issue, err)
	}
	for _, block := range read.Checks {
		if block.BlockID == check {
			return block.PassStands
		}
	}
	t.Fatalf("the read of %s holds no check %s: %s", r.issue, check, response.Body.String())
	return branchNewestStands{}
}

// TestTheNewestResultDecidesACheckComparedWithABranch: R5. The newest result that counts at a
// branch, or that Dispatch could not place, decides the check, and a later fail cancels an earlier
// pass.
func TestTheNewestResultDecidesACheckComparedWithABranch(t *testing.T) {
	t.Parallel()
	// The case production printed as a pass from tester: the tester's pass at the merged commit,
	// then two fails at the same commit after a deploy moved back.
	t.Run("a pass then two fails", func(t *testing.T) {
		rig := newBranchNewestRig(t)
		rig.record(t, "c9", "tester", "pass", branchNewestC)
		rig.record(t, "c9", "tester", "fail", branchNewestC)
		newer := rig.record(t, "c9", "tester", "fail", branchNewestC)
		if got := rig.stands(t, "c9"); got.Answer != "no" || !slices.Equal(got.Results, []string{newer}) {
			t.Fatalf("c9 answers %+v, want no, decided by the newer fail %s", got, newer)
		}
	})
	t.Run("a fail then a pass", func(t *testing.T) {
		rig := newBranchNewestRig(t)
		rig.record(t, "c9", "tester", "fail", branchNewestC)
		pass := rig.record(t, "c9", "tester", "pass", branchNewestC)
		if got := rig.stands(t, "c9"); got.Answer != "yes" || !slices.Equal(got.Roles, []string{"tester"}) || !slices.Equal(got.Results, []string{pass}) {
			t.Fatalf("c9 answers %+v, want yes from tester, decided by %s", got, pass)
		}
	})
	t.Run("three commits", func(t *testing.T) {
		rig := newBranchNewestRig(t)
		rig.record(t, "c9", "tester", "pass", branchNewestX)
		rig.record(t, "c9", "tester", "fail", branchNewestY)
		atZ := rig.record(t, "c9", "tester", "pass", branchNewestZ)
		if got := rig.stands(t, "c9"); got.Answer != "yes" || !slices.Equal(got.Results, []string{atZ}) {
			t.Fatalf("c9 answers %+v, want yes, decided by the pass at Z %s", got, atZ)
		}
	})
	// The roles are those of the passes after the newest fail, of two: the reviewer's pass came
	// between the two fails, so a walk that crosses the newest fail names the reviewer too, and a
	// walk of the deciding pass alone names the tester alone.
	t.Run("roles after the newest fail", func(t *testing.T) {
		rig := newBranchNewestRig(t)
		rig.record(t, "c9", "tester", "fail", branchNewestC)
		rig.record(t, "c9", "reviewer", "pass", branchNewestC)
		rig.record(t, "c9", "tester", "fail", branchNewestC)
		rig.record(t, "c9", "implementer", "pass", branchNewestC)
		pass := rig.record(t, "c9", "tester", "pass", branchNewestC)
		got := rig.stands(t, "c9")
		if roles := slices.Sorted(slices.Values(got.Roles)); got.Answer != "yes" || !slices.Equal(roles, []string{"implementer", "tester"}) || !slices.Equal(got.Results, []string{pass}) {
			t.Fatalf("c9 answers %+v, want yes from the implementer and the tester alone, whose passes came after the newest fail, decided by %s: the reviewer's pass came before that fail", got, pass)
		}
	})
	t.Run("default_branch", func(t *testing.T) {
		rig := newBranchNewestRig(t)
		rig.record(t, "d9", "tester", "pass", branchNewestC)
		fail := rig.record(t, "d9", "tester", "fail", branchNewestC)
		if got := rig.stands(t, "d9"); got.Answer != "no" || !slices.Equal(got.Results, []string{fail}) {
			t.Fatalf("d9 answers %+v, want no, decided by the fail %s", got, fail)
		}
	})
	t.Run("newest not placed", func(t *testing.T) {
		rig := newBranchNewestRig(t)
		rig.record(t, "c9", "tester", "pass", branchNewestC)
		atW := rig.record(t, "c9", "tester", "fail", branchNewestW)
		if got := rig.stands(t, "c9"); got.Answer != "unknown" || !slices.Equal(got.Results, []string{atW}) {
			t.Fatalf("c9 answers %+v, want unknown, as Dispatch could not place the newest result %s", got, atW)
		}
	})
}
