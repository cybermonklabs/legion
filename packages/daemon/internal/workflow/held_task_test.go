package workflow

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// failedClaimOf is the failure of the claim of agent on key, whose role is role, as the daemon
// applies it: with the claim, its cause, its counters and their limits.
func failedClaimOf(t *testing.T, key string, role claim.Role, agent claim.Agent) intake.ClaimFailed {
	t.Helper()
	failed := failedClaim(key, role)
	failed.Claim = agentClaim(t, key, agent)
	return failed
}

// mustSpend fails unless result is a fact taken: no refusal, and spent.
func mustSpend(t *testing.T, what string, result intake.Result) {
	t.Helper()
	if result.Refusal != nil || result.Unspent {
		t.Fatalf("%s = %+v, want it taken", what, result)
	}
}

// mustRefuse fails unless result is the refusal code, whose message is message.
func mustRefuse(t *testing.T, what string, result intake.Result, code, message string) {
	t.Helper()
	if result.Refusal == nil || result.Refusal.Code != code || result.Refusal.Message != message {
		t.Fatalf("%s = %+v, want %s: %q", what, result.Refusal, code, message)
	}
}

// assertHeld fails unless task of tasks is held at place for reason.
func assertHeld(t *testing.T, tasks map[string]record.PlanTask, task string, place record.TaskPlace, reason record.HoldReason) {
	t.Helper()
	assertTask(t, tasks, task, record.TaskHeld, place)
	if got := tasks[task].HoldReason; got != reason {
		t.Fatalf("task %s is held for %q, want %q", task, got, reason)
	}
}

// A failed claim holds the task of its agent's step and no other: task a implements and task b
// is at its review, each step its own agent's, and c, which needs both, waits. The claim of b's
// reviewer, reviewer-2, fails: b is held at its review for the failed claim, a runs on, the issue
// stands in implementing with no hold, and the owner's held notice names the task and the step,
// `task b, step 2`. a's implementation then moves a to its review. The owner's retry that names b
// releases b at its review and starts its reviewer's agent again, told b's own pull request; a
// retry that names no task while b alone is held does the same, and an escalation that names b
// holds it for that reason. With a
// and b both held, the issue stands held from the review, a retry that names no task is refused
// TASK_REQUIRED, and one that names c, which waits, is refused TASK_NOT_AT_PHASE.
//
// The fault it catches is a failed claim that stops the whole issue (its control: claimFailed of
// the release before, which holds the issue for the phase it stands in: reviewer-2 runs no step at
// implementing, so nothing is held and b runs on).
func TestAFailedClaimHoldsItsTaskAndTheOtherGoesOn(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer-2", "reviewer-2"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "c", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
	seedTaskBranch(t, pool, root.Key, "b", "legion/LEGION-208.b")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 45,
		Branch: "legion/LEGION-208.b", HeadSHA: "head-b", Failing: []string{}})
	apply := applyFacts(t, pool, engine)

	mustSpend(t, "the failure of b's reviewer", apply("reviewer-2-failed", failedClaimOf(t, root.Key, claim.RoleReviewer, "reviewer-2")))

	tasks := tasksOf(t, pool, engine, root.Key)
	assertHeld(t, tasks, "b", atStep(2), "")
	assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil {
		t.Fatalf("%s after b's reviewer failed stands in %s (hold %+v), want implementing with no hold: a runs on", root.Key, issue.Phase, issue.Hold)
	}
	held := noticesOfKind(t, pool, root.Key, "held")
	if len(held) != 1 || held[0].Role != claim.RoleReviewer || held[0].Phase != phase.Reviewing || held[0].Summary != "task b, step 2" || held[0].Reason != "" {
		t.Fatalf("the held notices of %s = %+v, want one of the reviewer's hold from reviewing naming task b, step 2, with no reason", root.Key, held)
	}

	taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))

	mustSpend(t, "the retry of b", apply("retry-b", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Task: "b", Reason: "try b again"}))
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "b", record.TaskRunning, atStep(2))
	if got := tasks["b"].HoldReason; got != "" {
		t.Fatalf("task b released holds the reason %q, want none", got)
	}
	retries := startsIn(t, pool, root.Key, claim.RoleReviewer, phase.Reviewing)
	if !slices.ContainsFunc(retries, func(start record.SuperviseRequest) bool {
		return start.Claim == agentClaim(t, root.Key, "reviewer-2") && start.ResendEntry && strings.Contains(start.Task, " Pull request: sjawhar/legion#45.")
	}) {
		t.Fatalf("the reviewer's starts of %s in reviewing = %+v, want the retry's of b's agent reviewer-2, naming b's own pull request #45", root.Key, retries)
	}

	mustSpend(t, "the second failure of b's reviewer", apply("reviewer-2-failed-again", failedClaimOf(t, root.Key, claim.RoleReviewer, "reviewer-2")))
	mustSpend(t, "the escalation of b", apply("escalate-b", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.EscalateDecision, Task: "b", Reason: "b keeps failing"}))
	assertHeld(t, tasksOf(t, pool, engine, root.Key), "b", atStep(2), record.HoldEscalated)
	mustSpend(t, "the retry of the one held task", apply("retry-the-held", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision}))
	assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskRunning, atStep(2))

	mustSpend(t, "the failure of a's reviewer", apply("reviewer-failed", failedClaimOf(t, root.Key, claim.RoleReviewer, "reviewer")))
	mustSpend(t, "the third failure of b's reviewer", apply("reviewer-2-failed-third", failedClaimOf(t, root.Key, claim.RoleReviewer, "reviewer-2")))
	tasks = tasksOf(t, pool, engine, root.Key)
	assertHeld(t, tasks, "a", atStep(2), "")
	assertHeld(t, tasks, "b", atStep(2), "")
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != (record.Hold{From: phase.Reviewing}) {
		t.Fatalf("%s with a and b held stands in %s (hold %+v), want held from reviewing: no task runs", root.Key, issue.Phase, issue.Hold)
	}
	mustRefuse(t, "a retry that names no task", apply("retry-none", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision}),
		"TASK_REQUIRED", "tasks a, b of LEGION-208 each stand at held: name the task with task.")
	mustRefuse(t, "a retry of c", apply("retry-c", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Task: "c"}),
		"TASK_NOT_AT_PHASE", "task c of LEGION-208 stands at admitted, not at held.")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertHeld(t, tasks, "a", atStep(2), "")
	assertHeld(t, tasks, "b", atStep(2), "")
}

// An issue held at the release before tasks were held goes on as it did: an issue of one task,
// `implementer tester reviewer`, held from testing, whose task stands at its tester's step, is
// retried by its owner with no task named. The retry restarts the tester, the issue stands in
// testing with no hold, and its task runs at step 2 with no hold reason, whether the store holds
// the task running beside the issue's hold, as the release before wrote it, or held, as the
// upgrade writes it.
//
// The fault it catches is a retry that releases the issue and leaves its task held, so that no
// path that resumes work reaches the step again (its control: retryPhase without the release of
// the task, which leaves the held task held).
func TestAnIssueHeldAtTheReleaseIsRetriedAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name string
		held bool
	}{{"its task running", false}, {"its task held", true}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Testing)
			holdFrom(t, pool, root.Key, phase.Testing)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
			forgeTaskPlace(t, pool, root.Key, root.Generation, atStep(2))
			if tc.held {
				seedRecord(t, pool, func(tx pgx.Tx) error {
					return record.NewStore().HoldTask(t.Context(), tx, root.Key, root.Generation, "a", "")
				})
			}

			mustSpend(t, "the retry", applyFacts(t, pool, engine)("retry", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision}))

			if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Testing || issue.Hold != nil {
				t.Fatalf("%s after the retry stands in %s (hold %+v), want testing with no hold", root.Key, issue.Phase, issue.Hold)
			}
			tasks := tasksOf(t, pool, engine, root.Key)
			assertTask(t, tasks, "a", record.TaskRunning, atStep(2))
			if got := tasks["a"].HoldReason; got != "" {
				t.Fatalf("task a after the retry holds the reason %q, want none", got)
			}
			if starts := startsIn(t, pool, root.Key, claim.RoleTester, phase.Testing); len(starts) != 1 || !starts[0].ResendEntry {
				t.Fatalf("the tester's starts of %s in testing = %+v, want the retry's one", root.Key, starts)
			}
		})
	}
}

// A failed claim of the one task of a plan of one holds that task, as it holds one task of
// several, and its issue with it, since no other task runs: task a, `implementer reviewer`, at its
// review. The claim of its reviewer fails: a is held at its review for the failed claim, and the
// issue stands held from reviewing. The owner's held notice names no task, since the plan holds no
// choice of them (heldSummary): its summary is empty, as a run with no plan rows says. The owner's
// retry that names a releases it at its review and starts its reviewer's agent again, told the
// issue's pull request #42; after a second failure an escalation that names a holds it for that
// reason, and its held notice names no task either; and a retry that names no task releases the
// one held task.
//
// The fault it catches is a held notice of a plan of one task that names the task, which a plan of
// one never did (its control: textNamesTask true for every plan with rows, so each summary reads
// `task a, step 2`).
func TestAFailedClaimOfThePlansOneTaskHoldsItsTaskAndNamesNone(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2)})
	apply := applyFacts(t, pool, engine)

	mustSpend(t, "the failure of a's reviewer", apply("reviewer-failed", failedClaimOf(t, root.Key, claim.RoleReviewer, "reviewer")))

	assertHeld(t, tasksOf(t, pool, engine, root.Key), "a", atStep(2), "")
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != (record.Hold{From: phase.Reviewing}) {
		t.Fatalf("%s after a's reviewer failed stands in %s (hold %+v), want held from reviewing: no task runs", root.Key, issue.Phase, issue.Hold)
	}
	held := noticesOfKind(t, pool, root.Key, "held")
	if len(held) != 1 || held[0].Role != claim.RoleReviewer || held[0].Phase != phase.Reviewing || held[0].Summary != "" || held[0].Reason != "" {
		t.Fatalf("the held notices of %s = %+v, want one of the reviewer's hold from reviewing with no summary and no reason", root.Key, held)
	}

	mustSpend(t, "the retry of a", apply("retry-a", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Task: "a", Reason: "try a again"}))
	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(2))
	if got := tasks["a"].HoldReason; got != "" {
		t.Fatalf("task a released holds the reason %q, want none", got)
	}
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Reviewing || issue.Hold != nil {
		t.Fatalf("%s after the retry of a stands in %s (hold %+v), want reviewing with no hold", root.Key, issue.Phase, issue.Hold)
	}
	retries := startsIn(t, pool, root.Key, claim.RoleReviewer, phase.Reviewing)
	if !slices.ContainsFunc(retries, func(start record.SuperviseRequest) bool {
		return start.Claim == agentClaim(t, root.Key, "reviewer") && start.ResendEntry && strings.Contains(start.Task, " Pull request: sjawhar/legion#42.")
	}) {
		t.Fatalf("the reviewer's starts of %s in reviewing = %+v, want the retry's of a's agent reviewer, naming the pull request #42", root.Key, retries)
	}

	mustSpend(t, "the second failure of a's reviewer", apply("reviewer-failed-again", failedClaimOf(t, root.Key, claim.RoleReviewer, "reviewer")))
	mustSpend(t, "the escalation of a", apply("escalate-a", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.EscalateDecision, Task: "a", Reason: "a keeps failing"}))
	assertHeld(t, tasksOf(t, pool, engine, root.Key), "a", atStep(2), record.HoldEscalated)
	held = noticesOfKind(t, pool, root.Key, "held")
	if last := held[len(held)-1]; len(held) != 3 || last.Phase != phase.Reviewing || last.Reason != string(record.HoldEscalated) || last.Escalation != "a keeps failing" ||
		last.Summary != "" || held[1].Summary != "" {
		t.Fatalf("the held notices of %s = %+v, want three, the last the escalation from reviewing for %q, and no summary in any", root.Key, held, "a keeps failing")
	}

	mustSpend(t, "the retry of the one held task", apply("retry-the-held", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision}))
	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Reviewing || issue.Hold != nil {
		t.Fatalf("%s after the retry of the one held task stands in %s (hold %+v), want reviewing with no hold", root.Key, issue.Phase, issue.Hold)
	}
}
