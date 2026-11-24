// results_ci_kind_test.go holds the results routes to the rule that the kind of a check comes from
// its check block at each read: a check whose ci_check names a run and that keeps the three rules
// of such a block is CI's, whatever rows stand against it. A role's row on such a check (one a
// browser's write, or a write that raced the result, left beside the block's ci_check until the
// next settlement takes that ci_check off) counts for nothing and says why.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// rolesRow writes a tester's pass of check blockID at commit as a role's row stands: role tester,
// no branch, against the check's words now.
func rolesRow(t *testing.T, rig *ciJobRig, blockID, commit string) {
	t.Helper()
	check := checkOf(t, readResults(t, rig.handler, rig.issue), blockID)
	if _, err := rig.database.Pool.Exec(context.Background(), `
		insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version, check_sha256, role, commit_sha, verdict, actor)
		values ($1, $1, $2::uuid, $3, $4, $5, 'tester', $6, 'pass', '{"kind":"session","id":"older-build"}')
	`, rig.issue, rig.verification, blockID, check.Version, check.SHA256, commit); err != nil {
		t.Fatalf("write the tester's row of %s: %v", blockID, err)
	}
}

// notCountedOf is, by check, the not_counted member of each result with role role, as the wire
// carries it in results and in recorded_results.
func notCountedOf(t *testing.T, handler http.Handler, issue, role string) map[string][]string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	type wireResult struct {
		Role       string `json:"role"`
		NotCounted string `json:"not_counted"`
	}
	var read struct {
		Checks []struct {
			BlockID         string       `json:"block_id"`
			Results         []wireResult `json:"results"`
			RecordedResults []wireResult `json:"recorded_results"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the read of %s: %v (%s)", issue, err, response.Body.String())
	}
	held := map[string][]string{}
	for _, check := range read.Checks {
		for _, result := range append(check.Results, check.RecordedResults...) {
			if result.Role == role {
				held[check.BlockID] = append(held[check.BlockID], result.NotCounted)
			}
		}
	}
	return held
}

// TestARolesRowOnACICheckCountsForNothing: a tester's pass of c1 stands at the open pull request's
// head beside c1's ci_check, the state a browser's write, or a write that raced the result, leaves
// until the next settlement. c1's block names ci_check go-checks and keeps the three rules, so c1
// is still a check CI proves at each read; the tester's row counts for nothing, says so in
// not_counted, and makes no pass stand; and a further role's result of c1 is still 409
// CHECK_PROVED_BY_CI. The control is k1, an ordinary check holding the same row, on which the
// tester's pass stands and counts. The next settlement then holds c1 to the fourth rule, as the
// release before does: it takes c1's ci_check off with one block.invalid naming the rule, and c1 is
// an ordinary check. Its tester's row then counts, under words that moved with the repair, since a
// block's digest covers its attributes, so no pass of c1 stands. The rig's own upload is settled
// first, so the rows are read before any settlement can judge them.
func TestARolesRowOnACICheckCountsForNothing(t *testing.T) {
	t.Parallel()
	rig := newCIJobRig(t)
	documents := rig.deps.Docs.(*docs.Service)
	if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatalf("settle the rig's upload: %v", err)
	}
	writeHead(t, rig.database, ciJobRepository, 70, ciJobCommit, "open", rig.issue)
	rolesRow(t, rig, "c1", ciJobCommit)
	rolesRow(t, rig, "k1", ciJobCommit)

	read := readResults(t, rig.handler, rig.issue)
	if k1 := checkOf(t, read, "k1"); k1.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("the control: the tester's pass on the ordinary check k1 reads %+v, want it standing", k1.PassStands)
	}
	c1 := checkOf(t, read, "c1")
	if c1.CI == nil {
		t.Fatalf("c1, whose block names ci_check %q, reads as no check CI proves once a role's row stands against it: ci_check %q, results %+v",
			ciJobName, c1.CICheck, c1.Results)
	}
	if c1.PassStands.Answer == model.PassStandsYes {
		t.Fatalf("the role's row on c1 makes a pass stand: %+v", c1.PassStands)
	}
	notCounted := notCountedOf(t, rig.handler, rig.issue, "tester")
	if len(notCounted["c1"]) != 1 || notCounted["c1"][0] != model.ResultNotCountedRoleOnCICheck {
		t.Fatalf("the tester's row on c1 carries not_counted %q, want the reason it counts for nothing, %q", notCounted["c1"], model.ResultNotCountedRoleOnCICheck)
	}
	if len(notCounted["k1"]) != 1 || notCounted["k1"][0] != "" {
		t.Fatalf("the tester's row on k1 carries not_counted %q, want none", notCounted["k1"])
	}

	refused := rig.post(t, rig.issue, rig.sessionToken(), resultBody("c1", "pass", ciJobCommit, map[string]any{"role": "tester"}))
	if code, _ := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "CHECK_PROVED_BY_CI" {
		t.Fatalf("a further tester's result of c1: status=%d body=%s, want 409 CHECK_PROVED_BY_CI", refused.Code, refused.Body.String())
	}

	if edited := applyOp(t, rig.handler, rig.verification, map[string]any{
		"op": "insert", "after": "end", "markdown": "A note below the checks.\n",
	}); edited.Code != http.StatusOK {
		t.Fatalf("an edit beside c1: status=%d body=%s, want 200", edited.Code, edited.Body.String())
	}
	if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatalf("settle the edit: %v", err)
	}
	settled := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	if settled.CICheck != "" || settled.CI != nil || settled.PassStands.Answer != model.PassStandsNo {
		t.Fatalf("c1 once settled reads ci_check %q, ci %+v, pass_stands %+v, want an ordinary check whose pass does not stand",
			settled.CICheck, settled.CI, settled.PassStands)
	}
	if len(settled.Results) != 1 || settled.Results[0].NotCounted != "" || settled.Results[0].CheckWordsMoved == nil || !*settled.Results[0].CheckWordsMoved {
		t.Fatalf("c1's tester's row once settled reads %+v, want it counted, under words that moved with the repair", settled.Results)
	}
	want := []ciCheckRefusalEvent{{BlockID: "c1", Reason: ciCheckRecordedRule, BlockType: "check"}}
	if got := ciCheckRefusalEvents(t, rig.handler, rig.issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("block.invalid events = %+v, want %+v", got, want)
	}
}
