package api

import (
	"net/http"
	"strings"
	"testing"
)

// The agent of the retro Legion runs after a done issue's close reports it with handoff_complete.
// When that call leaves out plan_rating, the plugin does not take its retro path (legion-phase-ops.ts
// completeHandoff posts a done issue's completion with no rating on the step path) and attaches
// what findHandoffReport reports for the implementer's left phase: the commit the workspace stands
// on, with the implement handoff record where the issue has one. The agent gave neither and cannot
// leave either out, so a refusal that names them (RETRO_COMMIT_UNEXPECTED, HANDOFF_RECORD_MISMATCH)
// asks for a change the agent cannot make, and the same call is refused the same way again. The
// refusal names what the report lacks and the agent can add: the rating of the plan
// (PLAN_RATING_REQUIRED, INFRA-5602's plan, state row s3), and nothing is recorded.
func TestARetroReportWithNoRatingIsRefusedForTheRatingWhateverThePluginAttached(t *testing.T) {
	for _, tc := range []struct {
		name   string
		attach func(req *HandoffCompleteRequest)
	}{
		{name: "the commit the workspace stands on", attach: func(req *HandoffCompleteRequest) { req.Commit = headSHA }},
		{name: "the commit and the implement handoff record", attach: func(req *HandoffCompleteRequest) {
			req.Commit, req.HandoffID, req.HandoffVersion = headSHA, "00000000-0000-0000-0000-000000000001", 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			implementer, _ := newRetroAfterCloseClaim(t, h)

			req := HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "the retro of the closed run"}
			tc.attach(&req)
			if message := refused(t, h.complete(req), http.StatusBadRequest, "PLAN_RATING_REQUIRED"); !strings.Contains(message, "planRating") {
				t.Fatalf("the PLAN_RATING_REQUIRED refusal is %q, want it to name planRating", message)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts = %#v, want none: the refusal is written before any fact is applied", got)
			}
		})
	}
}
