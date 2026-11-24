package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A reviewer reports a verdict exactly where no review on GitHub can decide its step: the reviewer
// of `planner reviewer`, whose issue has no pull request record, reports pass with its commit and
// is taken, as one HandoffComplete naming the verdict; with no verdict it is refused
// STEP_VERDICT_REQUIRED, and with a verdict and no commit MISSING_FIELD, since a step of the type
// review that reports a verdict sends the commit it read. The reviewer of an issue with an open pull
// request, whose round a review on GitHub settles, is refused a verdict VERDICT_ROLE_FORBIDDEN,
// with the sentence that names every report that carries one. The fresh agent of acceptance
// reports one as before: none is STEP_VERDICT_REQUIRED.
func TestAReviewerReportsAVerdictWhereNoReviewOfGitHubDecides(t *testing.T) {
	const key = "LEGION-208"
	for _, tc := range []struct {
		name           string
		steps          string
		at             phase.Phase
		role           claim.Role
		pullRequest    bool
		verdict        string
		commit         string
		status         int
		code, text     string
		handoffRecords bool
	}{
		{name: "the reviewer with no pull request passes with its commit", steps: "planner reviewer", at: phase.Reviewing, role: claim.RoleReviewer,
			verdict: "pass", commit: headSHA, status: http.StatusOK, handoffRecords: true},
		{name: "the reviewer with no pull request names no verdict", steps: "planner reviewer", at: phase.Reviewing, role: claim.RoleReviewer,
			commit: headSHA, status: http.StatusBadRequest, code: "STEP_VERDICT_REQUIRED",
			text: "a step of the role reviewer reports the verdict pass or fail; nothing was recorded", handoffRecords: true},
		{name: "the reviewer with no pull request names no commit", steps: "planner reviewer", at: phase.Reviewing, role: claim.RoleReviewer,
			verdict: "pass", status: http.StatusBadRequest, code: "MISSING_FIELD", text: "commit is required", handoffRecords: true},
		{name: "the reviewer of an open pull request names a verdict", steps: "implementer reviewer", at: phase.Reviewing, role: claim.RoleReviewer,
			pullRequest: true, verdict: "pass", commit: headSHA, status: http.StatusBadRequest, code: "VERDICT_ROLE_FORBIDDEN",
			text:           "only a tester, a role of the deployment or of the plan, the role acceptance, a reviewer of an issue with no pull request, and the owner in a review step report a verdict",
			handoffRecords: true},
		{name: "the fresh agent of acceptance names no verdict", steps: "implementer acceptance", at: phase.Accepting, role: claim.RoleAcceptance,
			pullRequest: true, commit: headSHA, status: http.StatusBadRequest, code: "STEP_VERDICT_REQUIRED",
			text: "a step of the role acceptance reports the verdict pass or fail; nothing was recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, facts := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
				Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3})
			seedIssueAt(t, h, key, tc.at)
			seedOwnerPlan(t, h, key, tc.steps, stepAt(2))
			if tc.pullRequest {
				seedPullRequest(t, h, key, headSHA)
			}
			seedEntry(t, h, key, tc.role, 1)
			worker := newLiveClaim(t, h, key, tc.role)
			takeTask(t, h, worker.token, "outbox:77", "the step of "+key, 1, 1)
			req := HandoffCompleteRequest{GrantID: worker.grant(t), Summary: "read the work", Verdict: tc.verdict, Commit: tc.commit}
			if tc.handoffRecords {
				req.HandoffID, req.HandoffVersion = worker.writeHandoff("review", 1), 1
			}

			answer := h.complete(req)

			if tc.status != http.StatusOK {
				assertFailureText(t, answer, tc.status, tc.code, tc.text)
				if got := facts.recorded(); len(got) != 0 {
					t.Fatalf("a refused completion recorded %#v, want nothing", got)
				}
				return
			}
			completion(t, answer)
			var taken []intake.HandoffComplete
			for _, fact := range facts.recorded() {
				if completed, ok := fact.(intake.HandoffComplete); ok && completed.Role == tc.role {
					taken = append(taken, completed)
				}
			}
			if len(taken) != 1 || taken[0].Verdict != tc.verdict || taken[0].Commit != headSHA {
				t.Fatalf("the %s's completions = %+v, want the one, with its verdict and commit", tc.role, taken)
			}
		})
	}
}
