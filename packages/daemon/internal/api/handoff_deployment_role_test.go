package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// checkerDeployment is Legion's own roles of a step and checker, a role of the deployment of the
// type testing.
func checkerDeployment() plan.Deployment {
	roles := plan.BuiltinRoles()
	roles["checker"] = plan.RoleDef{Name: "checker", Type: plan.TypeTesting}
	return plan.Deployment{Roles: roles}
}

// seedCheckerStep writes the plan `implementer checker` of key at generation 1 with its task
// running at step 2, the checker's, so the store reads key's running role as the checker.
func seedCheckerStep(t *testing.T, h *harness, key string) {
	t.Helper()
	err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		v := record.PlanVersion{Issue: key, Generation: 1, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: "legion-legion-" + claim.Token(key) + "-architect"}
		task := record.PlanTask{Issue: key, Generation: 1, Task: "a", PlanVersion: 2, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
			{Issue: key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: plan.TypeImplementation},
			{Issue: key, Generation: 1, Task: "a", Position: 2, Role: "checker", Type: plan.TypeTesting},
		})
		if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
			return err
		}
		return records.MoveTask(ctx, tx, key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
	})
	if err != nil {
		t.Fatalf("seed the checker's step of %s: %v", key, err)
	}
}

// assertFailureText holds recorder to one refusal of status, code and sentence.
func assertFailureText(t *testing.T, recorder *httptest.ResponseRecorder, status int, code, text string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
	var failure Failure
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body, err)
	}
	if failure.Code != code || failure.Error != text {
		t.Fatalf("refusal = %s %q, want %s %q", failure.Code, failure.Error, code, text)
	}
}

// A role the deployment defines reports its step with the verdict pass or fail and a commit, and
// names no handoff record: the checker at step with pass and a commit is taken, as one
// HandoffComplete; with no verdict it is refused STEP_VERDICT_REQUIRED, with no commit
// MISSING_FIELD, and with a handoff record HANDOFF_RECORD_MISMATCH; with fail and returnTo 1 it is
// taken. A verdict of one of Legion's roles but the tester is refused VERDICT_ROLE_FORBIDDEN.
func TestADeploymentRoleReportsWithAVerdictAndACommit(t *testing.T) {
	const key = "LEGION-208"
	one := 1
	for _, tc := range []struct {
		name   string
		role   claim.Role
		req    HandoffCompleteRequest
		status int
		code   string
		text   string
	}{
		{name: "pass with a commit", role: "checker", req: HandoffCompleteRequest{Summary: "checked", Verdict: "pass", Commit: headSHA}, status: http.StatusOK},
		{name: "no verdict", role: "checker", req: HandoffCompleteRequest{Summary: "checked", Commit: headSHA}, status: http.StatusBadRequest,
			code: "STEP_VERDICT_REQUIRED", text: "a step of the role checker reports the verdict pass or fail; nothing was recorded"},
		{name: "no commit", role: "checker", req: HandoffCompleteRequest{Summary: "checked", Verdict: "pass"}, status: http.StatusBadRequest,
			code: "MISSING_FIELD", text: "commit is required"},
		{name: "a handoff record", role: "checker", req: HandoffCompleteRequest{Summary: "checked", Verdict: "pass", Commit: headSHA, HandoffID: "record-1", HandoffVersion: 1},
			status: http.StatusConflict, code: "HANDOFF_RECORD_MISMATCH",
			text: "phase step of LEGION-208 ends with no handoff record, and this completion names one: complete without handoffId and handoffVersion; this completion changed nothing"},
		{name: "fail naming returnTo 1", role: "checker", req: HandoffCompleteRequest{Summary: "a gap", Verdict: "fail", Commit: headSHA, ReturnTo: &one}, status: http.StatusOK},
		{name: "a reviewer's verdict", role: claim.RoleReviewer, req: HandoffCompleteRequest{Summary: "looked", Verdict: "pass", Commit: headSHA},
			status: http.StatusBadRequest, code: "VERDICT_ROLE_FORBIDDEN", text: "only a tester, a role of the deployment or a role of the issue's plan reports a verdict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, facts := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
				Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3, Deployment: checkerDeployment()})
			seedIssueAt(t, h, key, phase.Step)
			seedCheckerStep(t, h, key)
			seedPullRequest(t, h, key, headSHA)
			seedEntry(t, h, key, tc.role, 1)
			worker := newLiveClaim(t, h, key, tc.role)
			takeTask(t, h, worker.token, "outbox:77", "check "+key, 1, 1)
			req := tc.req
			req.GrantID = worker.grant(t)

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
			if len(taken) != 1 || taken[0].Verdict != tc.req.Verdict || taken[0].Commit != headSHA {
				t.Fatalf("the %s's completions = %+v, want the one, with its verdict and commit", tc.role, taken)
			}
		})
	}
}

// A claim that has served no run takes the run and the entry of the task it holds only where that
// task is the issue's current work (heldTaskNotCurrent): at step, the work of the running step's
// role alone (record.Issue.StepRole), since every role the deployment defines runs as step. In
// `implementer checker` at step 2, the checker holding a step task of the issue's generation is
// taken; a critic holding a step task of the same generation, of a step the task is not at, is
// refused HANDOFF_NO_RUN naming the checker, and nothing is recorded.
func TestAHeldStepTaskBacksOnlyTheRunningStepsRole(t *testing.T) {
	const key = "LEGION-208"
	deployment := checkerDeployment()
	deployment.Roles["critic"] = plan.RoleDef{Name: "critic", Type: plan.TypeReview}
	for _, tc := range []struct {
		role  claim.Role
		taken bool
	}{{"checker", true}, {"critic", false}} {
		t.Run(string(tc.role), func(t *testing.T) {
			h, _, facts := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
				Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3, Deployment: deployment})
			seedIssueAt(t, h, key, phase.Step)
			seedCheckerStep(t, h, key)
			seedPullRequest(t, h, key, headSHA)
			seedEntry(t, h, key, tc.role, 1)
			worker := untaskedClaim(t, h, key, tc.role)
			holdingTask(t, h, worker, supervise.RequestDeliver{Task: "the step of " + key, ID: "outbox:77", Generation: 1, Phase: phase.Step, Entry: 1})

			answer := h.complete(HandoffCompleteRequest{GrantID: worker.grant(t), Summary: "checked", Verdict: "pass", Commit: headSHA})

			if tc.taken {
				completion(t, answer)
				if taken := lastCompletion(t, facts); taken.Role != tc.role || taken.Generation != 1 || taken.Entry != 1 {
					t.Fatalf("the completion = %+v, want the checker's, of the held task's generation 1 and entry 1", taken)
				}
				return
			}
			message := refused(t, answer, http.StatusConflict, "HANDOFF_NO_RUN")
			if want := "the task this claim holds (outbox:77) is the step phase's, and the running step of LEGION-208 is the checker's, not this claim's critic"; !strings.Contains(message, want) {
				t.Fatalf("the refusal = %q, want it to say %q", message, want)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("the refused completion recorded %#v, want nothing", got)
			}
		})
	}
}
