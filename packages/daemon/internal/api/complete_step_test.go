package api

import (
	"context"
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
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// seedOwnerPlan writes the plan of key, at the generation its record is on, as its one task a whose
// steps are words (`role` or `role:key`, each one of Legion's roles of a step, with that role's
// type), with the task running at place: the store reads key's running role as the claim that
// works that place, the architect where the owner does it. Each step's agent is named as the
// daemon names it when it stores a plan (record.NameAgents), since a stored step's name is read
// from that column alone.
func seedOwnerPlan(t *testing.T, h *harness, key, words string, place record.TaskPlace) {
	t.Helper()
	named, err := plan.ParseSteps(words)
	if err != nil {
		t.Fatalf("parse the steps %q: %v", words, err)
	}
	generation := issueGeneration(t, h, key)
	roles := plan.BuiltinRoles()
	steps := make([]record.PlanStep, len(named))
	for i, step := range named {
		steps[i] = record.PlanStep{Issue: key, Generation: generation, Task: "a", Position: i + 1, Role: step.Role,
			Type: roles[step.Role].Type, AgentKey: step.Key}
	}
	ctx := context.Background()
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		v := record.PlanVersion{Issue: key, Generation: generation, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: "legion-legion-" + claim.Token(key) + "-architect"}
		task := record.PlanTask{Issue: key, Generation: generation, Task: "a", PlanVersion: 2, BlockText: "Build the widget.", State: record.TaskWaiting}
		if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, record.NameAgents(nil, nil, nil, steps)); err != nil {
			return err
		}
		return records.MoveTask(ctx, tx, key, generation, "a", place, record.TaskRunning)
	}); err != nil {
		t.Fatalf("seed the plan %q of %s at %+v: %v", words, key, place, err)
	}
}

// completeStep posts body to the owner's report route.
func completeStep(h *harness, body map[string]any) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/steps/complete", body, nil)
}

// stepAt is the task's place of step from 1.
func stepAt(position int) record.TaskPlace {
	return record.TaskPlace{Kind: record.PlaceStep, Position: position}
}

// The owner reports a step of its own with complete_step, and only that: the route takes a report
// from the issue's own architect alone, of the step where its task stands now and whose agent is
// the owner, held to the rules of a worker's completion of that place, and applies it as the
// completion of the place's role under the owner's claim, or as sign_off and the move back for the
// owner's acceptance.
//
//  1. implementer:owner reviewer at step 1: the owner's report of step 1 with its commit, its
//     implement handoff and an open pull request is one HandoffComplete of the implementer, under
//     the owner's claim, of step 1, at the entry of the implementer's row.
//  2. The same issue, a report of step 2 (a reviewer's report, which that step would take): 409
//     OWNER_STEP_NOT_RUNNING, naming where the task stands, and no fact.
//  3. implementer reviewer at step 1, whose agent is no owner: 409 OWNER_STEP_NOT_RUNNING.
//  4. tester:owner with no verdict: 400 TESTER_VERDICT_REQUIRED.
//  5. implementer reviewer:owner at step 2 with an open pull request: no verdict is 400
//     STEP_VERDICT_REQUIRED, since no review on GitHub ends the owner's review step; fail with
//     returnTo 1 and a commit is one fact with the verdict.
//  6. tester at its retro, which the owner does: the report of step 0 with no rating, from a plugin
//     at the rating's contract, is refused PLAN_RATING_REQUIRED by the workflow; with a rating it is
//     one fact of the implementer, the retro's role, and the issue, which has no pull request, is
//     done.
//  7. implementer acceptance:owner at accepting: pass is one SignOff, fail with returnTo 1 one
//     BackwardMove returning to step 1. acceptance:owner implementer at step 1, and tester
//     acceptance:owner at step 2: a fail naming no step, with no step of the type implementation
//     before it, is one HandoffComplete of the acceptance with the verdict fail, which stops the
//     task; at step 1 a returnTo is 400 INVALID_RETURN_TO, since no step stands before it.
//  8. The architect of another tree: 403 NOT_THE_OWNER.
//  9. LEGION-1 > LEGION-2 > LEGION-3: LEGION-2's sub-architect, which sign_off takes for LEGION-3,
//     reporting LEGION-3's step: 403 NOT_THE_OWNER, no fact.
func TestCompleteStepIsTheOwnersReportOfItsOwnStep(t *testing.T) {
	const key = "LEGION-208"
	type harnessed struct {
		h     *harness
		facts *factRecorder
	}
	recorded := func(t *testing.T) harnessed {
		h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
		return harnessed{h, facts}
	}
	noFact := func(t *testing.T, at harnessed) {
		t.Helper()
		if got := at.facts.recorded(); len(got) != 0 {
			t.Fatalf("the refused report recorded %#v, want nothing", got)
		}
	}
	oneFact := func(t *testing.T, at harnessed) intake.Fact {
		t.Helper()
		got := at.facts.recorded()
		if len(got) != 1 {
			t.Fatalf("the report recorded %#v, want one fact", got)
		}
		return got[0]
	}
	answered := func(t *testing.T, recorder *httptest.ResponseRecorder) {
		t.Helper()
		if recorder.Code != http.StatusOK {
			t.Fatalf("the report = %d: %s", recorder.Code, recorder.Body)
		}
	}

	t.Run("1 the owner's report of its implementer step", func(t *testing.T) {
		at := recorded(t)
		seedIssueAt(t, at.h, key, phase.Implementing)
		seedOwnerPlan(t, at.h, key, "implementer:owner reviewer", stepAt(1))
		seedPullRequest(t, at.h, key, headSHA)
		seedEntry(t, at.h, key, claim.RoleImplementer, 3)
		owner := newLiveClaim(t, at.h, key, claim.RoleArchitect)
		written := owner.writeHandoff("implement", 1)
		answered(t, completeStep(at.h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 1, "summary": "built it",
			"commit": headSHA, "handoffId": written, "handoffVersion": 1}))
		completed, ok := oneFact(t, at).(intake.HandoffComplete)
		if !ok || completed.Role != claim.RoleImplementer || completed.Claim != owner.token || completed.Step != 1 || completed.Entry != 3 ||
			completed.Commit != headSHA || completed.HandoffRecord != written || completed.Issue != key {
			t.Fatalf("the report = %#v, want the implementer's completion of step 1 under %s, at entry 3, naming its commit and record", completed, owner.token)
		}
	})

	t.Run("2 a report of a step where the task does not stand", func(t *testing.T) {
		at := recorded(t)
		seedIssueAt(t, at.h, key, phase.Implementing)
		seedOwnerPlan(t, at.h, key, "implementer:owner reviewer", stepAt(1))
		seedPullRequest(t, at.h, key, headSHA)
		owner := newLiveClaim(t, at.h, key, claim.RoleArchitect)
		refused(t, completeStep(at.h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 2, "summary": "reviewed",
			"verdict": "pass", "commit": headSHA}), http.StatusConflict, "OWNER_STEP_NOT_RUNNING")
		noFact(t, at)
	})

	t.Run("3 a step whose agent is no owner", func(t *testing.T) {
		at := recorded(t)
		seedIssueAt(t, at.h, key, phase.Implementing)
		seedOwnerPlan(t, at.h, key, "implementer reviewer", stepAt(1))
		owner := newLiveClaim(t, at.h, key, claim.RoleArchitect)
		refused(t, completeStep(at.h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 1, "summary": "built it",
			"commit": headSHA}), http.StatusConflict, "OWNER_STEP_NOT_RUNNING")
		noFact(t, at)
	})

	t.Run("4 the owner's tester step with no verdict", func(t *testing.T) {
		at := recorded(t)
		seedIssueAt(t, at.h, key, phase.Testing)
		seedOwnerPlan(t, at.h, key, "tester:owner", stepAt(1))
		owner := newLiveClaim(t, at.h, key, claim.RoleArchitect)
		refused(t, completeStep(at.h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 1, "summary": "tested",
			"handoffId": owner.writeHandoff("test", 1), "handoffVersion": 1}), http.StatusBadRequest, "TESTER_VERDICT_REQUIRED")
		noFact(t, at)
	})

	t.Run("5 the owner's review step reports its verdict", func(t *testing.T) {
		at := recorded(t)
		seedIssueAt(t, at.h, key, phase.Reviewing)
		seedOwnerPlan(t, at.h, key, "implementer reviewer:owner", stepAt(2))
		seedPullRequest(t, at.h, key, headSHA)
		owner := newLiveClaim(t, at.h, key, claim.RoleArchitect)
		written := owner.writeHandoff("review", 1)
		refused(t, completeStep(at.h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 2, "summary": "reviewed",
			"commit": headSHA, "handoffId": written, "handoffVersion": 1}), http.StatusBadRequest, "STEP_VERDICT_REQUIRED")
		noFact(t, at)
		answered(t, completeStep(at.h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 2, "summary": "the parser drops a field",
			"verdict": "fail", "returnTo": 1, "commit": headSHA, "handoffId": written, "handoffVersion": 1}))
		completed, ok := oneFact(t, at).(intake.HandoffComplete)
		if !ok || completed.Role != claim.RoleReviewer || completed.Claim != owner.token || completed.Step != 2 || completed.Verdict != "fail" ||
			completed.ReturnTo != 1 || completed.Commit != headSHA {
			t.Fatalf("the report = %#v, want the reviewer's fail of step 2 under %s returning to step 1 at %s", completed, owner.token, headSHA)
		}
	})

	t.Run("6 the owner's retro is rated", func(t *testing.T) {
		h, _, facts := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
			Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3})
		seedIssueAt(t, h, key, phase.Retro)
		seedOwnerPlan(t, h, key, "tester", record.TaskPlace{Kind: record.PlaceRetro})
		owner := newLiveClaim(t, h, key, claim.RoleArchitect)
		if got := pluginContractOf(t, h, owner.token); got < PlanRatingContract {
			t.Fatalf("the owner's plugin declared contract %d, below PlanRatingContract %d: this case proves nothing", got, PlanRatingContract)
		}
		refused(t, completeStep(h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 0, "summary": "retro done", "commit": headSHA}),
			http.StatusBadRequest, "PLAN_RATING_REQUIRED")
		assertIssuePhase(t, h, phase.Retro)
		answered(t, completeStep(h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 0, "summary": "retro done", "commit": headSHA,
			"planRating": 4, "planReason": "The steps fit the work."}))
		got := facts.recorded()
		completed, ok := got[len(got)-1].(intake.HandoffComplete)
		if !ok || completed.Role != claim.RoleImplementer || completed.Claim != owner.token || completed.Step != 0 ||
			completed.PlanRating == nil || completed.PlanRating.Rating != 4 {
			t.Fatalf("the rated report = %#v, want the implementer's completion of the retro under %s with the rating 4", got[len(got)-1], owner.token)
		}
		assertIssuePhase(t, h, phase.Done)
	})

	t.Run("7 the owner's acceptance passes as sign_off", func(t *testing.T) {
		at := recorded(t)
		seedIssueAt(t, at.h, key, phase.Accepting)
		seedOwnerPlan(t, at.h, key, "implementer acceptance:owner", stepAt(2))
		owner := newLiveClaim(t, at.h, key, claim.RoleArchitect)
		answered(t, completeStep(at.h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 2, "summary": "it does what the issue asks",
			"verdict": "pass", "commit": headSHA}))
		if signed, ok := oneFact(t, at).(intake.SignOff); !ok || signed.Issue != key || signed.By != owner.token {
			t.Fatalf("the pass = %#v, want one sign-off of %s by %s", signed, key, owner.token)
		}
	})

	t.Run("7 the owner's acceptance fails as a move back", func(t *testing.T) {
		at := recorded(t)
		seedIssueAt(t, at.h, key, phase.Accepting)
		seedOwnerPlan(t, at.h, key, "implementer acceptance:owner", stepAt(2))
		owner := newLiveClaim(t, at.h, key, claim.RoleArchitect)
		answered(t, completeStep(at.h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 2, "summary": "the widget does not render",
			"verdict": "fail", "returnTo": 1, "commit": headSHA}))
		moved, ok := oneFact(t, at).(intake.BackwardMove)
		if !ok || moved.Issue != key || moved.ReturnTo != 1 || moved.To != phase.Implementing || moved.By != owner.token ||
			moved.Requester != claim.RoleArchitect || moved.Reason != "the widget does not render" {
			t.Fatalf("the fail = %#v, want one move back of %s to step 1 (implementing) by %s with the report's summary", moved, key, owner.token)
		}
	})

	for _, tc := range []struct {
		name, words string
		step        int
	}{
		{"7 the owner's first-step acceptance fails as the step's fail", "acceptance:owner implementer", 1},
		{"7 the owner's acceptance with no implementation before it fails as the step's fail", "tester acceptance:owner", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := recorded(t)
			seedIssueAt(t, at.h, key, phase.Accepting)
			seedOwnerPlan(t, at.h, key, tc.words, stepAt(tc.step))
			seedEntry(t, at.h, key, claim.RoleAcceptance, 4)
			owner := newLiveClaim(t, at.h, key, claim.RoleArchitect)
			answered(t, completeStep(at.h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": tc.step, "summary": "the widget does not render",
				"verdict": "fail", "commit": headSHA}))
			failed, ok := oneFact(t, at).(intake.HandoffComplete)
			if !ok || failed.Issue != key || failed.Role != claim.RoleAcceptance || failed.Claim != owner.token || failed.Step != tc.step || failed.Verdict != "fail" ||
				failed.ReturnTo != 0 || failed.Entry != 4 || failed.Commit != headSHA || failed.Summary != "the widget does not render" {
				t.Fatalf("the fail = %#v, want the acceptance's fail of step %d under %s at entry 4, naming no step", failed, tc.step, owner.token)
			}
		})
	}

	t.Run("7 the owner's first-step acceptance names no earlier step", func(t *testing.T) {
		at := recorded(t)
		seedIssueAt(t, at.h, key, phase.Accepting)
		seedOwnerPlan(t, at.h, key, "acceptance:owner implementer", stepAt(1))
		owner := newLiveClaim(t, at.h, key, claim.RoleArchitect)
		refused(t, completeStep(at.h, map[string]any{"grantId": owner.grant(t), "issue": key, "step": 1, "summary": "the widget does not render",
			"verdict": "fail", "returnTo": 1, "commit": headSHA}), http.StatusBadRequest, "INVALID_RETURN_TO")
		noFact(t, at)
	})

	t.Run("8 the architect of another tree", func(t *testing.T) {
		at := recorded(t)
		seedIssueAt(t, at.h, key, phase.Retro)
		seedOwnerPlan(t, at.h, key, "tester", record.TaskPlace{Kind: record.PlaceRetro})
		seedIssueAt(t, at.h, "LEGION-900", phase.Implementing)
		other := newLiveClaim(t, at.h, "LEGION-900", claim.RoleArchitect)
		message := refused(t, completeStep(at.h, map[string]any{"grantId": other.grant(t), "issue": key, "step": 0, "summary": "retro done",
			"commit": headSHA, "planRating": 4, "planReason": "The steps fit the work."}), http.StatusForbidden, "NOT_THE_OWNER")
		if !strings.Contains(message, "legion-legion-legion-208-architect") || !strings.Contains(message, string(other.token)) {
			t.Fatalf("the refusal says %q, want it to name the owner and the grant's claim", message)
		}
		noFact(t, at)
	})

	t.Run("9 a sub-architect above the issue", func(t *testing.T) {
		at := recorded(t)
		seedTree(t, at.h, "LEGION-1", "LEGION-2")
		parent := "LEGION-2"
		if err := store.Transact(context.Background(), at.h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return record.NewStore().PutIssue(context.Background(), tx, record.Issue{Key: "LEGION-3", Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: testProject, Title: "LEGION-3", Parent: &parent, Phase: phase.Retro,
				Generation: 1, Status: "in_progress"})
		}); err != nil {
			t.Fatalf("seed LEGION-3 under LEGION-2: %v", err)
		}
		seedOwnerPlan(t, at.h, "LEGION-3", "tester", record.TaskPlace{Kind: record.PlaceRetro})
		above := newLiveClaimIn(t, at.h, "LEGION-1", "LEGION-2", claim.RoleArchitect)
		refused(t, completeStep(at.h, map[string]any{"grantId": above.grant(t), "issue": "LEGION-3", "step": 0, "summary": "retro done",
			"commit": headSHA, "planRating": 4, "planReason": "The steps fit the work."}), http.StatusForbidden, "NOT_THE_OWNER")
		noFact(t, at)
	})
}
