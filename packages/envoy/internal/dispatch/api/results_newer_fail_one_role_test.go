// results_newer_fail_one_role_test.go holds GET /api/v1/issues/{key}/results to one shape that a
// results read printed as `A pass stands: yes, from implementer`: the implementer's pass at a
// commit, then, two weeks later, the implementer's fail at the same commit, and no other result.
// A commit holds one answer (R4): a pass and a fail that count there leave the check not known,
// in either order, for an `after_merge` check and for a `before_merge` one, and no pass stands.
// The test is made only of helpers a commit before those rules holds, and reads the route's
// answer as JSON into the types of this file, so it builds at such a commit and fails there on
// its own assertion.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// newerFailCommit is the commit both results of these tests name: the head of the issue's one
// pull request for the `before_merge` check, and, for the `after_merge` one, the commit that
// pull request merged into main as, which main's history holds with no read (R36: a result
// counts at a branch only at a commit that holds the merge of the work).
const newerFailCommit = "5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"

// newerFailHead is the final head of that pull request where it merged, a commit no result of
// these tests names.
const newerFailHead = "5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d"

// newerFailStands is check k5's pass_stands, with only the members this test reads.
type newerFailStands struct {
	Answer   string   `json:"answer"`
	Reason   string   `json:"reason"`
	Roles    []string `json:"roles"`
	Results  []string `json:"results"`
	Subjects []struct {
		Answer  string   `json:"answer"`
		Reason  string   `json:"reason"`
		Results []string `json:"results"`
	} `json:"subjects"`
}

// newerFailShape records the shape for k5, a check of the given `when` on the issue's one pull
// request: the implementer's pass at newerFailCommit, moved two weeks back, then the
// implementer's fail there. A pull request that is open heads that commit; one that merged
// merged into main as that commit, from the final head newerFailHead, so the commit holds the
// merge of the work and a result there counts at main (R36). GitHub says main holds the commit.
// It answers k5's pass_stands as the results read answers it, and the ids of the pass and the
// fail.
func newerFailShape(t *testing.T, when, state string) (newerFailStands, string, string) {
	t.Helper()
	handler, database := resultsGitHubAnswering(t, map[string]bool{"acme/widgets": true}, nil, "main",
		func(_, _ string) (int, string) { return http.StatusOK, "behind" })
	issue := issueInRepository(t, handler, "RESNF", "acme/widgets", "A check one role passed, then failed")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		":::check{#k5 when=\""+when+"\"}\nThe service answers at the commit.\n:::\n")
	if state == "merged" {
		writeKnownMergeOnBase(t, database, "acme/widgets", 41, newerFailHead, issue.Key, "main", newerFailCommit)
	} else {
		writeHeadOnBase(t, database, "acme/widgets", 41, newerFailCommit, state, issue.Key, "main")
	}
	record := func(verdict string) string {
		t.Helper()
		fields := map[string]any{"role": "implementer"}
		if verdict == "fail" {
			fields["failed"] = 1
		}
		response := recordResult(t, handler, issue.Key, resultBody("k5", verdict, newerFailCommit, fields))
		if response.Code != http.StatusCreated {
			t.Fatalf("record the implementer's %s of k5: status=%d body=%s", verdict, response.Code, response.Body.String())
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
	pass := record("pass")
	moved, err := database.Pool.Exec(context.Background(),
		`update check_results set created_at = created_at - interval '14 days' where id = $1`, pass)
	if err != nil || moved.RowsAffected() != 1 {
		t.Fatalf("move the pass %s two weeks back: %v (%d rows)", pass, err, moved.RowsAffected())
	}
	fail := record("fail")
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue.Key, response.Code, response.Body.String())
	}
	var read struct {
		Checks []struct {
			BlockID    string          `json:"block_id"`
			PassStands newerFailStands `json:"pass_stands"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s: %v", issue.Key, err)
	}
	for _, check := range read.Checks {
		if check.BlockID == "k5" {
			return check.PassStands, pass, fail
		}
	}
	t.Fatalf("the read of %s holds no check k5: %s", issue.Key, response.Body.String())
	return newerFailStands{}, "", ""
}

// TestOneRolesPassThenItsNewerFailAtOneCommitStandsNoPass: R4. The implementer's pass and its
// fail two weeks later, at one commit, leave no pass standing, for an `after_merge` check and
// for a `before_merge` one: a commit holds one answer, so both checks are not known for
// pass_and_fail and name the pass and the fail. The pass names the implementer in no answer.
func TestOneRolesPassThenItsNewerFailAtOneCommitStandsNoPass(t *testing.T) {
	t.Parallel()
	t.Run("after_merge", func(t *testing.T) {
		stands, pass, fail := newerFailShape(t, "after_merge", "merged")
		if stands.Answer != "unknown" || stands.Reason != "pass_and_fail" || len(stands.Roles) != 0 ||
			!slices.Equal(stands.Results, []string{pass, fail}) {
			t.Fatalf("k5 answers %+v, want unknown for pass_and_fail, from no role, naming the pass %s and the fail %s",
				stands, pass, fail)
		}
	})
	t.Run("before_merge", func(t *testing.T) {
		stands, pass, fail := newerFailShape(t, "before_merge", "open")
		if stands.Answer != "unknown" || stands.Reason != "pass_and_fail" || len(stands.Roles) != 0 {
			t.Fatalf("k5 answers %+v, want unknown for pass_and_fail, from no role", stands)
		}
		if len(stands.Subjects) != 1 || stands.Subjects[0].Answer != "unknown" ||
			stands.Subjects[0].Reason != "pass_and_fail" || !slices.Equal(stands.Subjects[0].Results, []string{pass, fail}) {
			t.Fatalf("k5's heads read %+v, want the one head unknown for pass_and_fail, naming the pass %s and the fail %s",
				stands.Subjects, pass, fail)
		}
	})
}
