package workflow

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// deploymentRoles is Legion's own roles of a step and the roles these tests' plans name, as the
// settings of a deployment that defines them give the engine (config.Config.Deployment). A step's
// type is read from its stored row, which each test writes; the types here are what a new plan's
// rows would take.
func deploymentRoles() plan.Deployment {
	roles := plan.BuiltinRoles()
	for name, typ := range map[string]plan.Type{"checker": plan.TypeTesting, "linter": plan.TypeTesting, "critic": plan.TypeReview, "audit": "security"} {
		roles[name] = plan.RoleDef{Name: name, Type: typ}
	}
	return plan.Deployment{Roles: roles}
}

// withDeployment is engine with deploymentRoles as its deployment.
func withDeployment(engine *Engine) *Engine {
	engine.cfg.Deployment = deploymentRoles()
	return engine
}

// deploymentEngine is returnEngine under a deployment that defines the roles these tests name.
func deploymentEngine() *Engine { return withDeployment(returnEngine()) }

// stepHead is the commit every completion of these tests reports.
const stepHead = "0123456789abcdef0123456789abcdef01234567"

// workerOr is the claim role's phases row of key now names on the step it runs, the claim a real
// route's grant carries (the step's worker), or fallback where the row names no agent of its own
// (record.NamesItsAgent): a completion from any other claim of the role is refused,
// HANDOFF_NOT_THE_STEPS_WORKER, or HANDOFF_NOT_CURRENT_PHASE where it is of an entry that claim
// served on a step the task has left (intake.AnotherAgentsStep). The row now is the role's row with
// the highest entry (record.PhaseRowOf), since a role that stands at two steps keeps a row for
// each. It is record.WorkerClaim's rule, with fallback in place of the role's own token.
func workerOr(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role, fallback claim.Token) claim.Token {
	t.Helper()
	if row := storedPhaseRow(t, pool, key, role); record.NamesItsAgent(row) {
		return row.Claim
	}
	return fallback
}

// stepReports applies role's completion of key at its generation 1 with verdict ("" for none),
// summary and returnTo (0 for none), as the route hands it to the engine, from the step's worker
// (workerOr), and fails the test on a refusal; handoffRecord is the handoff record it names (""
// for none). Where the role's row names no worker yet, it names the claim of the agent the role
// names, as the route names the claim whose grant reported, which the completion writes on the
// step's row and the claim of the role's worker is then read from (record.WorkerClaim).
func stepReports(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role, verdict, summary, handoffRecord string, returnTo int) {
	t.Helper()
	agent, err := claim.AgentToken("legion", key, claim.Agent(role))
	if err != nil {
		t.Fatal(err)
	}
	fact := intake.HandoffComplete{Generation: 1, Issue: key, Role: role, Claim: workerOr(t, pool, key, role, agent),
		Summary: summary, Commit: stepHead, Verdict: verdict, HandoffRecord: handoffRecord, HandoffVersion: 1, ReturnTo: returnTo}
	if result := applyFacts(t, pool, engine)(string(role)+"-"+verdict+"-"+handoffRecord+"-"+summary, fact); result.Refusal != nil {
		t.Fatalf("the %s's completion of %s was refused: %+v", role, key, result.Refusal)
	}
}

// stepRoleOf is the role of key's running step as the store reads it (record.Issue.StepRole).
func stepRoleOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) claim.Role {
	t.Helper()
	return issueOf(t, pool, engine, key).StepRole
}

// A step of a role the deployment defines runs as step whatever its type (C2): checker of the
// type testing, critic of the type review and audit of a type of the deployment's own. After the
// implementer's completion the issue stands at step with the role running and the role started
// once. The role's fail returns the task to the first step of the type implementation before it,
// whose task carries the role's report under its own name, not the tester's; the failed step's
// record says failed, returned to 1; a failed step of the type review counts a review round. The
// role's pass moves the task to its retro and starts the implementer's retro.
func TestADeploymentRoleOfAnyTypeStartsAndReports(t *testing.T) {
	for _, tc := range []struct {
		role   claim.Role
		typ    string
		rounds int
	}{
		{"checker", "testing", 0},
		{"critic", "review", 1},
		{"audit", "security", 0},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			pool := migratedPool(t)
			engine := deploymentEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", string(tc.role))
			if _, err := pool.Exec(t.Context(), "update plan_steps set type = $2 where issue = $1 and task = 'a' and position = 2", root.Key, tc.typ); err != nil {
				t.Fatalf("store the type of %s: %v", tc.role, err)
			}
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})

			stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "built", "impl-1", 0)
			assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(2))
			if got := stepRoleOf(t, pool, engine, root.Key); got != tc.role {
				t.Fatalf("the running role = %q, want %s", got, tc.role)
			}
			if starts := roleStartsOf(t, pool, root.Key, tc.role); len(starts) != 1 || starts[0].Phase != phase.Step {
				t.Fatalf("the starts of the %s = %+v, want one, of step", tc.role, starts)
			}

			implementerStarts := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer))
			stepReports(t, pool, engine, root.Key, tc.role, "fail", "The check found a gap.", "", 0)
			assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
			starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer)
			if len(starts) != implementerStarts+1 {
				t.Fatalf("the implementer's starts after the fail = %d, want one more than %d", len(starts), implementerStarts)
			}
			told := starts[len(starts)-1].Task
			if says := " Step 2 (" + string(tc.role) + ") reported: fail. The check found a gap."; !strings.Contains(told, says) {
				t.Fatalf("the implementer's task = %q, want it to say %q", told, says)
			}
			if strings.Contains(told, testerVerdictLabel) {
				t.Fatalf("the implementer's task = %q, want no %s: the report is the %s's", told, testerVerdictLabel, tc.role)
			}
			if end := lastEndOfStep(t, pool, root.Key, 2); resultOf(end) != "failed" || end.ReturnedTo == nil || *end.ReturnedTo != 1 {
				t.Fatalf("the end of step 2 = %+v, want failed, returned to 1", end)
			}
			if counts := taskCounts(t, pool, engine, root.Key, root.Generation); counts.Rounds != tc.rounds {
				t.Fatalf("the task's rounds = %d, want %d", counts.Rounds, tc.rounds)
			}

			stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "fixed", "impl-2", 0)
			assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(2))
			stepReports(t, pool, engine, root.Key, tc.role, "pass", "All clear.", "", 0)
			assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
			if retros := startsIn(t, pool, root.Key, claim.RoleImplementer, phase.Retro); len(retros) != 1 {
				t.Fatalf("the implementer's retro starts = %+v, want one", retros)
			}
		})
	}
}

// lastEndOfStep is the newest end of the step at place step in key's step records.
func lastEndOfStep(t *testing.T, pool *pgxpool.Pool, key string, step int) record.StepRecord {
	t.Helper()
	var last *record.StepRecord
	for _, write := range stepWrites(t, pool, key) {
		if isEnd(write) && write.Step == step {
			last = &write
		}
	}
	if last == nil {
		t.Fatalf("%s holds no end of step %d", key, step)
	}
	return *last
}

// Two steps of roles the deployment defines are two places of one phase: in `implementer checker
// linter` the checker's pass starts the linter, the issue stays at step with the linter running, the
// linter's row and the record of its step name the linter's own claim and not the checker's (the
// place the task stands at, not the first place that runs as step), a late completion of the
// checker moves nothing, and the linter's fail naming step 2 starts the checker again. In
// `implementer checker linter critic`, the critic's fail naming step 3 starts the linter again, and
// the critic's step ends failed, returned to 3.
func TestTwoDeploymentStepsAreTwoPlaces(t *testing.T) {
	types := map[string]string{"checker": "testing", "linter": "testing", "critic": "review"}

	t.Run("implementer checker linter", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Step)
		seedStepsAt(t, pool, root.Key, root.Generation, 2, types, "implementer", "checker", "linter")

		stepReports(t, pool, engine, root.Key, "checker", "pass", "Checked.", "", 0)
		assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(3))
		if got := stepRoleOf(t, pool, engine, root.Key); got != "linter" {
			t.Fatalf("the running role = %q, want linter", got)
		}
		if starts := roleStartsOf(t, pool, root.Key, "linter"); len(starts) != 1 {
			t.Fatalf("the linter's starts = %+v, want one", starts)
		}
		linter, err := stepClaim(root, root.Key, "linter")
		if err != nil {
			t.Fatal(err)
		}
		if got := workerOr(t, pool, root.Key, "linter", ""); got != linter {
			t.Fatalf("the linter's row names the claim %q, want the linter's own, %q", got, linter)
		}
		var started []string
		for _, write := range writesAt(stepWrites(t, pool, root.Key), "step", 3) {
			if isStart(write) {
				started = append(started, write.Agent)
			}
		}
		if len(started) != 1 || started[0] != string(linter) {
			t.Fatalf("the starts of step 3 name the agents %q, want one, the linter's claim %q", started, linter)
		}

		fact := intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: "checker", Claim: "checker-claim", Summary: "Checked again.", Commit: stepHead, Verdict: "pass"}
		applyFacts(t, pool, engine)("checker-late", fact)
		assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(3))
		if got := stepRoleOf(t, pool, engine, root.Key); got != "linter" {
			t.Fatalf("after the checker's late completion the running role = %q, want linter", got)
		}

		checkers := len(roleStartsOf(t, pool, root.Key, "checker"))
		stepReports(t, pool, engine, root.Key, "linter", "fail", "Lint failed.", "", 2)
		assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(2))
		if got := len(roleStartsOf(t, pool, root.Key, "checker")); got != checkers+1 {
			t.Fatalf("the checker's starts after the linter's fail = %d, want %d", got, checkers+1)
		}
	})

	t.Run("implementer checker linter critic", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Step)
		seedStepsAt(t, pool, root.Key, root.Generation, 4, types, "implementer", "checker", "linter", "critic")

		stepReports(t, pool, engine, root.Key, "critic", "fail", "Lint again.", "", 3)
		assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(3))
		if starts := roleStartsOf(t, pool, root.Key, "linter"); len(starts) != 1 {
			t.Fatalf("the linter's starts = %+v, want one", starts)
		}
		if end := lastEndOfStep(t, pool, root.Key, 4); resultOf(end) != "failed" || end.ReturnedTo == nil || *end.ReturnedTo != 3 {
			t.Fatalf("the end of step 4 = %+v, want failed, returned to 3", end)
		}
	})
}

// A role the deployment defines reports pass or fail and opens no pull request, so in `critic
// implementer` the critic's pass, with no pull request recorded, starts the implementer.
func TestADeploymentRoleBeforeTheImplementerNeedsNoPullRequest(t *testing.T) {
	pool := migratedPool(t)
	engine := deploymentEngine()
	root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Step, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	seedTreeLifecycle(t, pool, root.Key)
	seedStepsAt(t, pool, root.Key, root.Generation, 1, map[string]string{"critic": "review"}, "critic", "implementer")

	stepReports(t, pool, engine, root.Key, "critic", "pass", "The design holds.", "", 0)
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(2))
	if starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(starts) != 1 {
		t.Fatalf("the implementer's starts = %+v, want one", starts)
	}
}

// Each notice of a step of a role the deployment defines names that role: the failed claim of the
// checker that runs the issue's step holds the issue from step, and the held notice names the
// checker; the held notice of an issue held from step (HeldNotice) names its running role; and the
// checker's pass writes one phase-finished notice, which names the checker.
func TestAHeldStepOfTheDeploymentNamesItsRole(t *testing.T) {
	t.Run("the checker's failed claim", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Step)
		seedStepsAt(t, pool, root.Key, root.Generation, 2, map[string]string{"checker": "testing"}, "implementer", "checker")

		applyRefusingNothing(t, pool, engine, intake.ClaimFailed{Issue: root.Key, Role: "checker", Reason: "the pane died"})

		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Held || issue.Hold == nil || issue.Hold.From != phase.Step {
			t.Fatalf("%s stands in %s with hold %+v, want held from step", root.Key, issue.Phase, issue.Hold)
		}
		if roles := rolesOf(noticesOf(t, pool, root.Key, "held")); len(roles) != 1 || roles[0] != "checker" {
			t.Fatalf("the held notices name %v, want the checker once", roles)
		}
	})

	t.Run("HeldNotice", func(t *testing.T) {
		issue := record.Issue{Key: "LEGION-208", Phase: phase.Held, Hold: &record.Hold{From: phase.Step}, StepRole: "checker"}
		if got := HeldNotice(issue); got.Role != "checker" || got.Phase != phase.Step {
			t.Fatalf("HeldNotice of an issue held from step = %+v, want the checker's, of step", got)
		}
	})

	t.Run("the checker's pass", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Step)
		seedStepsAt(t, pool, root.Key, root.Generation, 2, map[string]string{"checker": "testing"}, "implementer", "checker")

		stepReports(t, pool, engine, root.Key, "checker", "pass", "Checked.", "", 0)

		if roles := rolesOf(noticesOf(t, pool, root.Key, "phase-finished")); len(roles) != 1 || roles[0] != "checker" {
			t.Fatalf("the phase-finished notices name %v, want the checker once", roles)
		}
	})
}

// A completion that arrives after an event moved the issue on to a step of a role the deployment
// defines is the first report of a run that event ended, as before any other step: in `implementer
// reviewer checker`, the approving review moves the task from the reviewer's step 2 to the checker's
// step 3 before the reviewer completes, and the reviewer's completion is taken onto its row and
// moves nothing: the task stays at step 3, the checker running.
func TestALateCompletionBeforeADeploymentStepIsTakenAndMovesNothing(t *testing.T) {
	pool := migratedPool(t)
	engine := deploymentEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer", "checker")
	if _, err := pool.Exec(t.Context(), "update plan_steps set type = 'testing' where issue = $1 and task = 'a' and role = 'checker'", root.Key); err != nil {
		t.Fatalf("store the type of checker: %v", err)
	}
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
	reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(3))

	result := applyFacts(t, pool, engine)("reviewer-late", intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleReviewer,
		Claim: reviewer, Summary: "Approved.", Commit: stepHead, HandoffRecord: "review-1", HandoffVersion: 1})

	if result.Refusal != nil {
		t.Fatalf("the reviewer's late completion = %+v, want it taken", result.Refusal)
	}
	if row := roleRowOf(t, pool, engine, root.Key, claim.RoleReviewer); row.HandoffRecord != "review-1" || row.LastHandoff != "review-1" {
		t.Fatalf("the reviewer's row = %+v, want the taken report recorded on it", row)
	}
	if got := stepRoleOf(t, pool, engine, root.Key); got != "checker" {
		t.Fatalf("after the reviewer's late completion the running role = %q, want checker", got)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(3))
}

// checkerAtStep2 is LEGION-208 at step 2 of `implementer checker`, the checker's step.
func checkerAtStep2(t *testing.T, pool *pgxpool.Pool) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, phase.Step)
	seedStepsAt(t, pool, root.Key, root.Generation, 2, map[string]string{"checker": "testing"}, "implementer", "checker")
	return root
}

// A retry or a restart of a step of a role the deployment defines starts that role: the
// architect's retry of an issue held from step for its checker's failed claim restarts the checker
// and puts the issue back at step; the operator's restart of the checker on an issue at step
// restarts it, and a restart of a role that works no step of it is refused; and a person's todo on
// a root whose stopped task's first step is the critic's starts the critic again at step 1.
func TestARetryOfADeploymentStepRestartsItsRole(t *testing.T) {
	t.Run("the retry of a hold from step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := checkerAtStep2(t, pool)
		applyRefusingNothing(t, pool, engine, intake.ClaimFailed{Issue: root.Key, Role: "checker", Reason: "the pane died"})

		applyRefusingNothing(t, pool, engine, intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Reason: "try again"})

		assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(2))
		if starts := roleStartsOf(t, pool, root.Key, "checker"); len(starts) != 1 || starts[0].Phase != phase.Step {
			t.Fatalf("the checker's starts after the retry = %+v, want one, of step", starts)
		}
	})

	// The operator's retry of a claim (api's retryClaim) names that claim, and the claim it names is
	// fenced against the claim on the step of the role the hold is from: the step's role, which the
	// held issue still records, and not the role of the phase step, which is none.
	t.Run("the operator's retry of a hold from step names the step's claim", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := checkerAtStep2(t, pool)
		applyRefusingNothing(t, pool, engine, intake.ClaimFailed{Issue: root.Key, Role: "checker", Reason: "the pane died"})
		checker, err := stepClaim(root, root.Key, "checker")
		if err != nil {
			t.Fatal(err)
		}

		applyRefusingNothing(t, pool, engine, intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Claim: checker})

		assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(2))
		if starts := roleStartsOf(t, pool, root.Key, "checker"); len(starts) != 1 || starts[0].Phase != phase.Step {
			t.Fatalf("the checker's starts after the operator's retry = %+v, want one, of step", starts)
		}
	})

	t.Run("the operator's restart", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := checkerAtStep2(t, pool)

		applyRefusingNothing(t, pool, engine, intake.RestartPhase{Issue: root.Key, Role: "checker"})
		if starts := roleStartsOf(t, pool, root.Key, "checker"); len(starts) != 1 {
			t.Fatalf("the checker's starts after its restart = %+v, want one", starts)
		}
		result := applyFacts(t, pool, engine)("restart-linter", intake.RestartPhase{Issue: root.Key, Role: "linter"})
		if result.Refusal == nil || result.Refusal.Code != "PHASE_NOT_RESTARTED" {
			t.Fatalf("the restart of the linter = %+v, want PHASE_NOT_RESTARTED", result.Refusal)
		}
	})

	t.Run("a todo on a task stopped at its critic's first step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := deploymentEngine()
		root := stepRoot(t, pool, phase.Step)
		seedStepsAt(t, pool, root.Key, root.Generation, 1, map[string]string{"critic": "review"}, "critic", "implementer")
		if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}

		if result := applyFacts(t, pool, engine)("todo", personsTodo(root.Key)); result.Refusal != nil {
			t.Fatalf("the todo was refused: %+v", result.Refusal)
		}

		assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(1))
		if starts := roleStartsOf(t, pool, root.Key, "critic"); len(starts) != 1 {
			t.Fatalf("the critic's starts after the todo = %+v, want one", starts)
		}
	})
}

// The start of a step of a role the deployment defines names the claim of its agent, here the agent
// named for that role, as its agent.
func TestTheStartOfADeploymentStepNamesItsAgent(t *testing.T) {
	pool := migratedPool(t)
	engine := deploymentEngine()
	root := checkerAtStep2(t, pool)
	issue := issueOf(t, pool, engine, root.Key)
	want, err := claim.NewToken("legion", root.Key, "checker")
	if err != nil {
		t.Fatal(err)
	}
	if got := startAgentOf(t, pool, engine, issue, phase.Step); got != want {
		t.Fatalf("the agent of the checker's step = %q, want %q", got, want)
	}
}

// A move back that names returnTo goes to the place it names, judged by the one judge
// of a named place the fail path uses (returnStep): in `implementer checker linter
// acceptance:owner` at accepting, the owner's move to step with returnTo 2 starts the checker, and
// with returnTo 3 the linter; a to that is not the phase the named step runs as is refused, and so
// is returnTo 4, the step that moves back, with the judge's own sentence; a refused move moves
// nothing.
func TestABackwardMoveNamesItsPlaceWithReturnTo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		to       phase.Phase
		returnTo int
		starts   claim.Role
		refusal  string
	}{
		{"to step, returnTo 2", phase.Step, 2, "checker", ""},
		{"to step, returnTo 3", phase.Step, 3, "linter", ""},
		{"to implementing, returnTo 2", phase.Implementing, 2, "",
			"returnTo 2 names step 2 (checker) of task a, which runs as step, and to names implementing; this move changed nothing."},
		{"to step, returnTo 4", phase.Step, 4, "",
			"returnTo is 4, and step 4 (acceptance) of task a of LEGION-208 is not before step 4 (acceptance), the step that moves back; " +
				"send the move again naming an earlier step, or without returnTo; nothing was recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := deploymentEngine()
			root := stepRoot(t, pool, phase.Accepting)
			seedStepsAt(t, pool, root.Key, root.Generation, 4, map[string]string{"checker": "testing", "linter": "testing"},
				"implementer", "checker", "linter", "acceptance:owner")

			moveBack := rootsMoveBack(t, pool, engine, root.Key, tc.to, "the check missed a case")
			moveBack.ReturnTo = tc.returnTo
			result := applyFacts(t, pool, engine)("move-back", moveBack)

			if tc.refusal != "" {
				if result.Refusal == nil || result.Refusal.Code != "BACKWARD_REFUSED" || result.Refusal.Message != tc.refusal {
					t.Fatalf("the move = %+v, want BACKWARD_REFUSED %q", result.Refusal, tc.refusal)
				}
				assertWalksAt(t, pool, engine, root.Key, phase.Accepting, stepPlace(4))
				return
			}
			if result.Refusal != nil {
				t.Fatalf("the move to step %d was refused: %+v", tc.returnTo, result.Refusal)
			}
			assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(tc.returnTo))
			if starts := roleStartsOf(t, pool, root.Key, tc.starts); len(starts) != 1 {
				t.Fatalf("the %s's starts = %+v, want one", tc.starts, starts)
			}
		})
	}
}

// A role the deployment defines moves its own task back: the checker at step 2 of `implementer
// checker` asks to go back to implementing, and the task goes to step 1 and starts the implementer.
func TestADeploymentRoleMovesItsTaskBack(t *testing.T) {
	pool := migratedPool(t)
	engine := deploymentEngine()
	root := checkerAtStep2(t, pool)

	if result := applyFacts(t, pool, engine)("checker-back", intake.BackwardMove{Issue: root.Key, Requester: "checker", To: phase.Implementing,
		Reason: "the build is broken"}); result.Refusal != nil {
		t.Fatalf("the checker's move back was refused: %+v", result.Refusal)
	}

	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
	if starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(starts) != 1 {
		t.Fatalf("the implementer's starts = %+v, want one", starts)
	}
}

// A move back to step does not say which step where two earlier steps run as step, so it is
// refused BACKWARD_REFUSED, the two nearest being the places backwardPlace names, and nothing
// moves; with one earlier step that runs as step, the move goes there and starts its role.
func TestABackwardMoveToStepNamesWhichStep(t *testing.T) {
	const reason = "the check missed a case"
	for _, tc := range []struct {
		name     string
		words    []string
		two      []record.TaskPlace
		returnTo record.TaskPlace
	}{
		{"two earlier steps", []string{"implementer", "checker", "linter", "acceptance:owner"}, []record.TaskPlace{stepPlace(2), stepPlace(3)}, record.TaskPlace{}},
		{"three earlier steps, the two nearest named", []string{"implementer", "checker", "critic", "linter", "acceptance:owner"},
			[]record.TaskPlace{stepPlace(3), stepPlace(4)}, record.TaskPlace{}},
		{"one earlier step", []string{"implementer", "checker", "acceptance:owner"}, nil, stepPlace(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := deploymentEngine()
			root := stepRoot(t, pool, phase.Accepting)
			at := len(tc.words)
			seedStepsAt(t, pool, root.Key, root.Generation, at, map[string]string{"checker": "testing", "critic": "review", "linter": "testing"}, tc.words...)

			_, w := issueAndWalkOf(t, pool, engine, root.Key)
			place, found, two := w.backwardPlace(phase.Step)
			if place != tc.returnTo || found != (tc.two == nil) || !slices.Equal(two, tc.two) {
				t.Fatalf("backwardPlace(step) = %+v, %t, %+v; want %+v, %t, %+v", place, found, two, tc.returnTo, tc.two == nil, tc.two)
			}

			result := applyFacts(t, pool, engine)("move-back", rootsMoveBack(t, pool, engine, root.Key, phase.Step, reason))
			if tc.two == nil {
				if result.Refusal != nil {
					t.Fatalf("the move back to the one earlier step was refused: %+v", result.Refusal)
				}
				assertWalksAt(t, pool, engine, root.Key, phase.Step, tc.returnTo)
				if starts := roleStartsOf(t, pool, root.Key, "checker"); len(starts) != 1 {
					t.Fatalf("the checker's starts = %+v, want one", starts)
				}
				return
			}
			if result.Refusal == nil || result.Refusal.Code != "BACKWARD_REFUSED" {
				t.Fatalf("the move back to step = %+v, want BACKWARD_REFUSED", result.Refusal)
			}
			if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Accepting {
				t.Fatalf("after the refused move %s stands in %s, want accepting", root.Key, issue.Phase)
			}
		})
	}
}

// closingTree seeds the root LEGION-208 implementing at generation 7 and its child LEGION-209
// awaiting its merge on sjawhar/legion#43, the child's checker having run (its phases row), and
// answers an engine whose production check is checkRole ("" for none).
func closingTree(t *testing.T, pool *pgxpool.Pool, checkRole claim.Role) *Engine {
	t.Helper()
	seedMergingChild(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: "checker", Claim: "child-checker", Verdict: "pass"})
	now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
	return New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckRole: checkRole,
		Repositories: testRepositories, Clock: func() time.Time { return now }}, nil)
}

// wantEachRole fails unless rows, the supervise requests of one op by issue/role, hold one for key's
// checker and one for each of Legion's six but skip.
func wantEachRole(t *testing.T, rows map[string]int, key string, skip claim.Role) {
	t.Helper()
	for _, role := range append(claim.Roles, "checker") {
		want := 1
		if role == skip {
			want = 0
		}
		if rows[key+"/"+string(role)] != want {
			t.Fatalf("requests %v, want one for each claim of %s, the checker's among them, and none for %q", rows, key, skip)
		}
	}
}

// A close reaches every claim its issue may hold, the claim of a role the deployment defines among
// them: one row for each stored claim of the issue, and one by role for each role the issue can
// hold, the roles its phases rows name among them, whose worker claim is none of those
// (claimTargets). A merged child's close retires the checker's claim with the six (and leaves out
// the tester that works a production check); a child moved to backlog suspends it; and a root's
// close suspends it as its tree lingers.
func TestAClosedIssueReachesItsDeploymentRolesClaim(t *testing.T) {
	merged := intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"}
	t.Run("a merged child", func(t *testing.T) {
		pool := migratedPool(t)
		applyRefusingNothing(t, pool, closingTree(t, pool, ""), merged)
		wantEachRole(t, superviseRequests(t, pool, "issue_close"), "LEGION-209", "")
	})
	t.Run("a merged child with a production check", func(t *testing.T) {
		pool := migratedPool(t)
		applyRefusingNothing(t, pool, closingTree(t, pool, claim.RoleTester), merged)
		wantEachRole(t, superviseRequests(t, pool, "issue_close"), "LEGION-209", claim.RoleTester)
	})
	t.Run("a child moved to backlog", func(t *testing.T) {
		pool := migratedPool(t)
		applyRefusingNothing(t, pool, closingTree(t, pool, ""), intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2,
			Type: "issue.updated", Status: "backlog", Title: "child", Parent: "LEGION-208", Rank: "V"})
		wantEachRole(t, superviseRequests(t, pool, "suspend"), "LEGION-209", "")
		if closed := superviseRequests(t, pool, "issue_close"); len(closed) != 0 {
			t.Fatalf("issue_close rows %v, want none for a move to backlog", closed)
		}
	})
	t.Run("a root", func(t *testing.T) {
		pool := migratedPool(t)
		root := stepRoot(t, pool, phase.Merging)
		seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleMerger, Claim: "merger", HandoffCommit: "head"})
		seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: "checker", Claim: "root-checker", Verdict: "pass"})
		applyRefusingNothing(t, pool, deploymentEngine(), intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-42"})
		wantEachRole(t, superviseRequests(t, pool, "suspend"), root.Key, "")
		if closed := superviseRequests(t, pool, "issue_close"); len(closed) != 0 {
			t.Fatalf("issue_close rows %v, want none for a root, whose tree lingers", closed)
		}
	})
}

// The claim of a role of a task a later version of the plan removed is reached too: its phases row
// stays when the plan in force names the role no more.
func TestAClosedIssueReachesTheClaimOfARoleOfARemovedTask(t *testing.T) {
	pool := migratedPool(t)
	engine := closingTree(t, pool, "")
	seedStepsPlan(t, pool, "LEGION-209", 7, "implementer")
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().MoveTask(t.Context(), tx, "LEGION-209", 7, "a", record.TaskPlace{Kind: record.PlaceMerge}, record.TaskRunning)
	})

	applyRefusingNothing(t, pool, engine, intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})

	if closed := superviseRequests(t, pool, "issue_close"); closed["LEGION-209/checker"] != 1 {
		t.Fatalf("issue_close rows %v, want one for the checker, whose task the plan in force no longer holds", closed)
	}
}

// A linger's end closes the claim of a role the deployment defines of each member, beside the six.
func TestALingersEndClosesADeploymentRolesClaim(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Merging)
	seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleMerger, Claim: "merger", HandoffCommit: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: "checker", Claim: "root-checker", Verdict: "pass"})
	// No production check, which would hold the tree's close open past the linger.
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateOff.Steps(), Linger: time.Hour, Repositories: testRepositories,
		Clock: func() time.Time { return stepClock }}, nil)
	applyRefusingNothing(t, pool, engine, intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-42"})

	applyRefusingNothing(t, pool, engine, intake.LingerExpired{Issue: root.Key, Generation: root.Generation})

	wantEachRole(t, superviseRequests(t, pool, "tree_close"), root.Key, "")
}

// lostChecker is an engine whose deployment holds Legion's own roles and no checker: the settings
// the daemon restarted with no longer define it.
func lostChecker() *Engine {
	engine := deploymentEngine()
	engine.cfg.Deployment = plan.Deployment{Roles: plan.BuiltinRoles()}
	return engine
}

// A step whose role neither the deployment nor the plan in force defines stops its task where the task would start it:
// in `implementer checker`, the implementer's completion starts no checker, the task stops naming
// step 2 and the checker, the issue is held from implementing for the stopped task, and the owner is
// told once, in a task-stopped notice that names the checker.
func TestAStepWhoseRoleTheDeploymentLostStopsTheTask(t *testing.T) {
	pool := migratedPool(t)
	engine := lostChecker()
	root := stepRoot(t, pool, phase.Implementing)
	seedStepsAt(t, pool, root.Key, root.Generation, 1, map[string]string{"checker": "testing"}, "implementer", "checker")

	stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "built", "impl-1", 0)

	if starts := roleStartsOf(t, pool, root.Key, "checker"); len(starts) != 0 {
		t.Fatalf("the implementer's completion started the checker %+v, want none: the deployment no longer defines it", starts)
	}
	const why = "step 2 names the role checker, which the deployment no longer defines"
	if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.StoppedReason != why {
		t.Fatalf("task a = %s stopped with %q, want stopped with %q", task.State, task.StoppedReason, why)
	}
	want := record.Hold{From: phase.Implementing, Reason: record.HoldTaskStopped}
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != want {
		t.Fatalf("%s stands in %s with hold %+v, want held %+v", root.Key, issue.Phase, issue.Hold, want)
	}
	stopped := noticesOf(t, pool, root.Key, "task-stopped")
	if len(stopped) != 1 || stopped[0].Role != "checker" || stopped[0].Reason != "task a of LEGION-208 was stopped: "+why {
		t.Fatalf("the task-stopped notices = %+v, want exactly one, the checker's, with the reason", stopped)
	}
}

// A daemon that restarts with settings that no longer define the role of a step its task still has
// to run, a role the plan in force does not define either, stops that task at its boot: the
// checker running at step 2 is stopped (step_stop), the task stops naming it, and the owner is told
// once. A task whose steps the deployment all defines is left alone.
func TestABootStopsATaskWhoseRoleTheDeploymentLost(t *testing.T) {
	pool := migratedPool(t)
	engine := lostChecker()
	root := checkerAtStep2(t, pool)
	seedTreeLifecycle(t, pool, root.Key)

	applyRefusingNothing(t, pool, engine, intake.DaemonBooted{Boot: 1})

	const why = "step 2 names the role checker, which the deployment no longer defines"
	if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.StoppedReason != why {
		t.Fatalf("task a after the boot = %s stopped with %q, want stopped with %q", task.State, task.StoppedReason, why)
	}
	if stops := superviseRequests(t, pool, "step_stop"); stops[root.Key+"/checker"] != 1 {
		t.Fatalf("step_stop rows %v, want one for the running checker", stops)
	}
	if roles := rolesOf(noticesOf(t, pool, root.Key, "task-stopped")); len(roles) != 1 || roles[0] != "checker" {
		t.Fatalf("the task-stopped notices name %v, want the checker once", roles)
	}

	kept := migratedPool(t)
	defined := checkerAtStep2(t, kept)
	seedTreeLifecycle(t, kept, defined.Key)
	applyRefusingNothing(t, kept, deploymentEngine(), intake.DaemonBooted{Boot: 1})
	if task := taskOf(t, kept, deploymentEngine(), defined.Key); task.State != record.TaskRunning {
		t.Fatalf("task a under a deployment that defines the checker = %s (%q) after the boot, want running", task.State, task.StoppedReason)
	}
}

// A keyed agent runs in the slot of its name, the role of its first step (record.NameAgents), so
// in `implementer checker:a tester:a` key a's agent is named checker and runs the tester's step in
// the checker's slot. A daemon that restarts with settings that no longer define the checker,
// while the task stands at that tester step, stops the task at its boot as it stops a step of the
// checker itself: the reason names the step, the agent and its slot's role, and the owner is told
// once, in a task-stopped notice that names the checker. The control is the same task under a
// deployment that defines the checker, which the boot leaves running.
func TestABootStopsATaskWhoseAgentIsNamedForARoleTheDeploymentLost(t *testing.T) {
	keyedAtTester := func(t *testing.T, pool *pgxpool.Pool) record.Issue {
		t.Helper()
		root := stepRoot(t, pool, phase.Testing)
		seedStepsAt(t, pool, root.Key, root.Generation, 3, map[string]string{"checker": "testing"}, "implementer", "checker:a", "tester:a")
		seedTreeLifecycle(t, pool, root.Key)
		return root
	}
	pool := migratedPool(t)
	engine := lostChecker()
	root := keyedAtTester(t, pool)

	applyRefusingNothing(t, pool, engine, intake.DaemonBooted{Boot: 1})

	const why = "step 3 is the work of the agent checker, which runs in the slot of the role checker, which the deployment no longer defines"
	if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.StoppedReason != why {
		t.Fatalf("task a after the boot = %s stopped with %q, want stopped with %q", task.State, task.StoppedReason, why)
	}
	stopped := noticesOf(t, pool, root.Key, "task-stopped")
	if len(stopped) != 1 || stopped[0].Role != "checker" || stopped[0].Reason != "task a of LEGION-208 was stopped: "+why {
		t.Fatalf("the task-stopped notices = %+v, want exactly one, naming the checker, with the reason", stopped)
	}

	kept := migratedPool(t)
	defined := keyedAtTester(t, kept)
	applyRefusingNothing(t, kept, deploymentEngine(), intake.DaemonBooted{Boot: 1})
	if task := taskOf(t, kept, deploymentEngine(), defined.Key); task.State != record.TaskRunning {
		t.Fatalf("task a under a deployment that defines the checker = %s (%q) after the boot, want running", task.State, task.StoppedReason)
	}
}

// A second or later agent of a role takes a numbered name of that role (claim.NumberedAgentRole),
// `implementer-2` or `checker-2`, which is no role itself: its slot is lost only where its role
// is. The fault: the name was read as a role, so a second implementer's step stopped its task as
// the work of a role the deployment no longer defines. The controls: an agent named for the lost
// checker itself, and a numbered agent of it, are lost; a step of the key owner, with no name, is
// not.
func TestANumberedAgentIsLostOnlyWhereItsRoleIs(t *testing.T) {
	withChecker := plan.BuiltinRoles()
	withChecker["checker"] = plan.RoleDef{Name: "checker", Type: plan.TypeTesting}
	defined, lost := plan.Deployment{Roles: withChecker}, plan.Deployment{Roles: plan.BuiltinRoles()}
	for _, tc := range []struct {
		name claim.Agent
		d    plan.Deployment
		lost bool
	}{
		{name: "implementer-2", d: lost},
		{name: "checker-2", d: defined},
		{name: "checker", d: defined},
		{name: "checker", d: lost, lost: true},
		{name: "checker-2", d: lost, lost: true},
		{name: "", d: lost},
	} {
		if got := lostAgent(tc.name, tc.d); got != tc.lost {
			t.Errorf("lostAgent(%q) under a deployment that defines the checker = %t: %t, want %t", tc.name, tc.d.Defines("checker"), got, tc.lost)
		}
	}
}

// A person's todo on a root held for its stopped task starts nothing while a step of the task names
// a role the deployment no longer defines: the task stays stopped, its reason now the lost step's,
// the issue stays held as it was, the status the record keeps is written back over the todo with
// that reason, and the owner is told again in a task-stopped notice that names the role. It holds
// for a task the boot stopped for the lost role, for one its owner stopped before the role was lost
// (which the boot leaves as it stands), and for one stopped before its first step went out.
func TestATodoStartsNoRoleTheDeploymentLost(t *testing.T) {
	const why = "step 1 names the role checker, which the deployment no longer defines"
	for _, tc := range []struct {
		name string
		stop func(t *testing.T, pool *pgxpool.Pool) record.Issue
	}{
		{"a task the boot stopped", func(t *testing.T, pool *pgxpool.Pool) record.Issue {
			root := stepRoot(t, pool, phase.Step)
			seedStepsAt(t, pool, root.Key, root.Generation, 1, map[string]string{"checker": "testing"}, "checker", "implementer")
			seedTreeLifecycle(t, pool, root.Key)
			applyRefusingNothing(t, pool, lostChecker(), intake.DaemonBooted{Boot: 1})
			return root
		}},
		{"a task its owner stopped before the role was lost", func(t *testing.T, pool *pgxpool.Pool) record.Issue {
			root := stepRoot(t, pool, phase.Step)
			seedStepsAt(t, pool, root.Key, root.Generation, 1, map[string]string{"checker": "testing"}, "checker", "implementer")
			if result := applyFacts(t, pool, deploymentEngine())("stop", ownersStop(t, root, "a")); result.Refusal != nil {
				t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
			}
			seedTreeLifecycle(t, pool, root.Key)
			applyRefusingNothing(t, pool, lostChecker(), intake.DaemonBooted{Boot: 1})
			return root
		}},
		{"a task stopped before its first step", func(t *testing.T, pool *pgxpool.Pool) record.Issue {
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "checker", "implementer")
			if result := applyFacts(t, pool, deploymentEngine())("stop", ownersStop(t, root, "a")); result.Refusal != nil {
				t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
			}
			return root
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := lostChecker()
			root := tc.stop(t, pool)
			held := issueOf(t, pool, engine, root.Key)
			if held.Phase != phase.Held || !record.HeldFor(held, record.HoldTaskStopped) {
				t.Fatalf("%s before the todo stands in %s with hold %+v, want held for its stopped task", root.Key, held.Phase, held.Hold)
			}
			told := len(noticesOf(t, pool, root.Key, "task-stopped"))

			if result := applyFacts(t, pool, engine)("todo", personsTodo(root.Key)); result.Refusal != nil {
				t.Fatalf("the todo was refused: %+v", result.Refusal)
			}

			if starts := roleStartsOf(t, pool, root.Key, "checker"); len(starts) != 0 {
				t.Fatalf("the todo started the checker %+v, want none: the deployment no longer defines it", starts)
			}
			issue := issueOf(t, pool, engine, root.Key)
			if issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != *held.Hold {
				t.Fatalf("%s after the todo stands in %s with hold %+v, want held %+v as it stood", root.Key, issue.Phase, issue.Hold, *held.Hold)
			}
			if issue.Status != held.Status || issue.DispatchStatus != "todo" || issue.LastDispatchSeq != 2 {
				t.Fatalf("%s after the todo has status %q, Dispatch at %q and sequence %d, want %q kept, todo and 2 recorded",
					root.Key, issue.Status, issue.DispatchStatus, issue.LastDispatchSeq, held.Status)
			}
			if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.StoppedReason != why {
				t.Fatalf("task a after the todo = %s stopped with %q, want stopped with %q", task.State, task.StoppedReason, why)
			}
			said := "task a of LEGION-208 was stopped: " + why
			stopped := noticesOf(t, pool, root.Key, "task-stopped")
			if len(stopped) != told+1 || stopped[told].Role != "checker" || stopped[told].Reason != said {
				t.Fatalf("the task-stopped notices = %+v, want one more after the todo, the checker's, with %q", stopped, said)
			}
			if writes := statusWrites(t, pool, root.Key); len(writes) == 0 || writes[len(writes)-1] != record.OrdinaryStatusWrite(held.Status, "todo", said) {
				t.Fatalf("the status writes are %+v, want the last to write %q back over the todo with the stop's reason", writes, held.Status)
			}
		})
	}
}

// An engine whose configuration sets no deployment holds Legion's own roles, so none of them is
// lost: `implementer tester` starts the implementer at the gate's opening and the tester at the
// implementer's completion, and no task stops.
func TestAConfigWithoutADeploymentStartsLegionsRoles(t *testing.T) {
	pool := migratedPool(t)
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateOff.Steps(), ReviewRoundCap: 3, Linger: time.Hour,
		ProductionCheckRole: claim.RoleTester, ReviewAppLogin: testReviewApp, Repositories: testRepositories, Clock: func() time.Time { return stepClock }}, nil)
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester")

	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "built", "impl-1", 0)

	if starts := roleStartsOf(t, pool, root.Key, claim.RoleTester); len(starts) != 1 {
		t.Fatalf("the tester's starts = %+v, want one", starts)
	}
	if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskRunning {
		t.Fatalf("task a is %s (%q), want running", task.State, task.StoppedReason)
	}
	if stopped := noticesOf(t, pool, root.Key, "task-stopped"); len(stopped) != 0 {
		t.Fatalf("task-stopped notices %+v, want none", stopped)
	}
}

// The owner's stop_task while a role the deployment defines runs stops that role's process and
// names it in the notice.
func TestAStopReachesTheDeploymentRoleThatRuns(t *testing.T) {
	pool := migratedPool(t)
	engine := deploymentEngine()
	root := checkerAtStep2(t, pool)

	if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
	}

	if stops := superviseRequests(t, pool, "step_stop"); stops[root.Key+"/checker"] != 1 {
		t.Fatalf("step_stop rows %v, want one for the checker", stops)
	}
	if roles := rolesOf(noticesOf(t, pool, root.Key, "task-stopped")); len(roles) != 1 || roles[0] != "checker" {
		t.Fatalf("the task-stopped notices name %v, want the checker once", roles)
	}
}
