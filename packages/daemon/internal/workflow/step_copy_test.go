package workflow

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// standsThreeTasks is LEGION-209, a root in implementing with its admission slot, holding
// threeTasks' plan: a at the implementer's step, b at the reviewer's, c held at the tester's.
func standsThreeTasks(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: silenceIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: silenceIssue, Project: "LEGION", Title: "a root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U",
		HandedOver: true})
	seedSlot(t, pool, record.Slot{Issue: silenceIssue, Index: 0, AdmittedAt: silenceClock()})
	threeTasks(t, pool, silenceIssue, 1)
}

// reviewerResumed fails unless the reviewer's newest start of LEGION-209 is the resume task of
// b's step: the phase's resume, composed on the copy read for task b, which names it.
func reviewerResumed(t *testing.T, pool *pgxpool.Pool, door string) {
	t.Helper()
	starts := roleStartsOf(t, pool, silenceIssue, claim.RoleReviewer)
	if len(starts) == 0 {
		t.Fatalf("%s queued no start of the reviewer of %s", door, silenceIssue)
	}
	start := starts[len(starts)-1]
	if start.Phase != phase.Reviewing || !strings.Contains(start.Task, " Task: b. Step 2 of 3: reviewer.") ||
		!strings.Contains(start.Task, "Resume the existing phase work.") {
		t.Fatalf("%s's start of the reviewer of %s = %+v, want the resume of b's step 2 in reviewing", door, silenceIssue, start)
	}
}

// Each door that resumes the worker of one running step of a plan of several tasks composes that
// worker's task on the copy of the issue read for the step's task (StepCopy): of LEGION-209's
// tasks a at the implementer's step, b at the reviewer's and c held at the tester's, the
// reviewer, level with the entry its phases row names, is sent the resume of b's step 2 by the
// boot's resume of its claim the daemon suspended (booted), by the ready of its claim
// (readyWorkerTask) and by the repair of its silence (workerSilent). Each door read the issue at
// the step's phase alone, which names no task, so the resume's walk of a plan of several tasks
// failed, and with it the whole fact. The start of such a copy names the claim of its step's
// agent, so the boot's resumes of a's implementer and b's reviewer stand side by side, each the
// resume of its own task's step.
//
// The fault it catches is the start of one step composed on a copy of the issue that names no
// task (its control: StepCopy answering the issue at the step's phase, as each door read it
// before, under which each door's fact fails with the walk that names no task), and a resume of
// one task's step that takes out another's (its control: the start of a copy that names no claim,
// under which the boot leaves one of the two resumes).
func TestEachResumeOfAStepIsComposedOnItsTasksCopy(t *testing.T) {
	t.Run("the boot's resume by rule", func(t *testing.T) {
		pool := migratedPool(t)
		standsThreeTasks(t, pool)
		project, err := claim.ProjectToken("LEGION")
		if err != nil {
			t.Fatal(err)
		}
		seedRuleSuspendedOwner(t, pool, project, silenceIssue, claim.RoleReviewer)

		mustSpend(t, "the boot", applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))("boot", intake.DaemonBooted{}))

		reviewerResumed(t, pool, "the boot")
	})

	t.Run("the boot's resumes of two steps", func(t *testing.T) {
		pool := migratedPool(t)
		standsThreeTasks(t, pool)
		project, err := claim.ProjectToken("LEGION")
		if err != nil {
			t.Fatal(err)
		}
		seedRuleSuspendedOwner(t, pool, project, silenceIssue, claim.RoleImplementer)
		seedRuleSuspendedOwner(t, pool, project, silenceIssue, claim.RoleReviewer)

		mustSpend(t, "the boot", applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))("boot", intake.DaemonBooted{}))

		reviewerResumed(t, pool, "the boot")
		implementer := roleStartsOf(t, pool, silenceIssue, claim.RoleImplementer)
		if len(implementer) != 1 || implementer[0].Phase != phase.Implementing || !strings.Contains(implementer[0].Task, " Task: a. Step 1 of 3: implementer.") {
			t.Fatalf("the boot's starts of the implementer of %s = %+v, want the one resume of a's step 1 beside the reviewer's", silenceIssue, implementer)
		}
	})

	t.Run("the ready of the claim", func(t *testing.T) {
		pool := migratedPool(t)
		standsThreeTasks(t, pool)
		token := seedPhaseWorker(t, pool, silenceIssue, claim.RoleReviewer, "")
		if _, err := pool.Exec(t.Context(), `update claims set generation = 1 where token = $1`, string(token)); err != nil {
			t.Fatalf("put the reviewer's claim at generation 1: %v", err)
		}

		if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "supervise:reviewer:2:ready",
			intake.ClaimReady{Issue: silenceIssue, Role: claim.RoleReviewer, Launch: 2}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("apply the ready: %v", err)
		}

		reviewerResumed(t, pool, "the ready")
	})

	t.Run("the repair of its silence", func(t *testing.T) {
		pool := migratedPool(t)
		standsThreeTasks(t, pool)
		token := silenceToken(t, claim.RoleReviewer)
		seedPhase(t, pool, record.PhaseRow{Issue: silenceIssue, Role: claim.RoleReviewer, Claim: token,
			Entry: silenceEntry, EntryTask: "Review b.", EntryRaisedAt: silenceStep()})
		if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, step_at, given_entry, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', $2, $2, 'reviewer', 1, 'ses', '/sessions/ses.jsonl', 'ready', $3, $4, 0, 0, 0, 0)`,
			string(token), silenceIssue, silenceStep(), silenceEntry); err != nil {
			t.Fatalf("seed the reviewer's claim: %v", err)
		}

		if err := applySilence(t, pool, testEngine(config.DesignGateRootIssues, nil), aSilenceOf(claim.RoleReviewer, 1)); err != nil {
			t.Fatalf("apply the silence of the reviewer: %v", err)
		}

		reviewerResumed(t, pool, "the silence repair")
	})
}

// The retro of a parent held for the issues beneath it starts on the copy of its plan's task that
// is held for them (startHeldRetro): of LEGION-208's tasks a done and b, `implementer reviewer`,
// needing a, b's approved review passes its last step while LEGION-209 is open beneath it, so b is
// held before its retro for the issues beneath, and the issue, in which no task runs, is held from
// retro for them. The child's done, whose settle releases the parent, takes b out of its hold to
// run at its retro, writes the issue in retro and starts b's implementer there once. The settle
// walked the parent itself, which names no task, so the walk of a plan of several tasks failed,
// and with it the child's leave.
//
// The fault it catches is the release of a parent's hold for its children read on the issue rather
// than on the task held for them (its control: startHeldRetro walking the parent itself).
func TestTheLastChildsLeaveStartsTheRetroOfTheTaskHeldForIt(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-a"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, needs: []string{"a"},
			state: record.TaskRunning, place: atStep(2)})
	stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	retro := record.TaskPlace{Kind: record.PlaceRetro}
	taskMoves(t, pool, engine, root.Key, "b", TriggerReviewApproved, "")
	assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskHeld, retro)
	if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil ||
		*held.Hold != (record.Hold{From: phase.Retro, Reason: record.HoldForChildren}) {
		t.Fatalf("premise: %s after b's approval stands in %s with hold %+v, want held from retro for its children", root.Key, held.Phase, held.Hold)
	}

	childLeavesDone(t, pool, engine)

	assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskRunning, retro)
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Retro || issue.Hold != nil {
		t.Fatalf("%s after its child's leave stands in %s with hold %+v, want retro with no hold", root.Key, issue.Phase, issue.Hold)
	}
	if starts := startsIn(t, pool, root.Key, claim.RoleImplementer, phase.Retro); len(starts) != 1 || !strings.Contains(starts[0].Task, " Task: b.") {
		t.Fatalf("the starts of the implementer of %s in retro = %+v, want the one start of b's retro", root.Key, starts)
	}
}

// A person's todo on a held root of a plan of several tasks releases each held task on its own
// copy (releaseHeldRoot): of LEGION-208's tasks a, `implementer reviewer` at its implementer, and b,
// the same at its reviewer, each its own agents', with c, `tester`, needing both, the failures of
// a's implementer and of b's reviewer hold a and b, so no task runs and the root stands held from
// implementing, its status still b's review's, needs_review. The todo retries both: a runs again
// at step 1 and b at step 2, the root stands in implementing with no hold, a's implementer and b's
// reviewer are each started again on their own task, and the status of implementing is queued
// twice, once by the write of a's release over the record's needs_review and once over the todo
// Dispatch shows, and no more: b's release reads the issue as a's left it. The release walked the
// root itself, which names no task, so the walk of a plan of several tasks failed, and with it the
// todo.
//
// The fault it catches is the release of a root's hold read on the issue rather than on each task
// its hold is made of (its control: releaseHeldRoot walking the root itself).
func TestATodoOnAHeldRootReleasesEachHeldTask(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer-2", "reviewer-2"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "c", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
	apply := applyFacts(t, pool, engine)
	mustSpend(t, "the failure of a's implementer", apply("implementer-failed", failedClaimOf(t, root.Key, claim.RoleImplementer, "implementer")))
	mustSpend(t, "the failure of b's reviewer", apply("reviewer-2-failed", failedClaimOf(t, root.Key, claim.RoleReviewer, "reviewer-2")))
	if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil || *held.Hold != (record.Hold{From: phase.Implementing}) {
		t.Fatalf("premise: %s with a and b held stands in %s (hold %+v), want held from implementing", root.Key, held.Phase, held.Hold)
	}
	statuses := len(statusWrites(t, pool, root.Key))

	mustSpend(t, "the todo", apply("todo", personsTodo(root.Key)))

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "b", record.TaskRunning, atStep(2))
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil {
		t.Fatalf("%s after the todo stands in %s (hold %+v), want implementing with no hold", root.Key, issue.Phase, issue.Hold)
	}
	for _, want := range []struct {
		role  claim.Role
		at    phase.Phase
		agent claim.Agent
		task  string
	}{{claim.RoleImplementer, phase.Implementing, "implementer", "a"}, {claim.RoleReviewer, phase.Reviewing, "reviewer-2", "b"}} {
		if starts := startsIn(t, pool, root.Key, want.role, want.at); !slices.ContainsFunc(starts, func(start record.SuperviseRequest) bool {
			return start.Claim == agentClaim(t, root.Key, want.agent) && start.ResendEntry && strings.Contains(start.Task, " Task: "+want.task+".")
		}) {
			t.Fatalf("the %s's starts of %s in %s = %+v, want the retry of %s's agent %s", want.role, root.Key, want.at, starts, want.task, want.agent)
		}
	}
	if got, want := statusWrites(t, pool, root.Key)[statuses:], []record.StatusWrite{record.OrdinaryStatusWrite("in_progress", "needs_review", ""),
		record.OrdinaryStatusWrite("in_progress", "todo", "")}; !slices.Equal(got, want) {
		t.Fatalf("the todo queued the status writes %+v, want %+v: implementing's over the record's status, and over the todo", got, want)
	}
}

// An operator's retry of a held issue of a plan of several tasks that names a claim whose agent
// runs no step is read for the held or stopped task whose step at its place is that agent's
// (heldView). Of LEGION-208's tasks a, `implementer:k reviewer`, and b, `tester`, needing a, the
// owner stops a at its first step, so no task runs and the root stands held for the stopped task:
// a retry naming k's claim is the retry of a, refused as a stopped task's, and one naming the claim
// of b's agent, which stands at no held or stopped task, names no task and is refused
// TASK_REQUIRED. Of tasks a, `implementer:k`, and b, `tester:k`, each at k's step, with c needing
// both, the owner stops a and then b, and a retry naming k's claim names neither and is refused
// TASK_REQUIRED. The decision was read on the issue itself, which names no task, so its refusal's
// walk of a plan of several tasks failed, and with it the retry.
//
// The fault it catches is a decision on a hold read on the issue where the claim it names runs no
// step (its control: heldView answering the issue itself for such a claim).
func TestARetryNamingAClaimThatRunsNoStepReadsItsAgentsTask(t *testing.T) {
	retry := func(t *testing.T, agent claim.Agent) intake.RetryOrEscalate {
		return intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision, Claim: agentClaim(t, "LEGION-208", agent)}
	}
	t.Run("one stopped task", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer:k", "reviewer"}, agents: []claim.Agent{"k", "reviewer"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "b", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"a"}, state: record.TaskWaiting})
		apply := applyFacts(t, pool, engine)
		mustSpend(t, "the owner's stop of a", apply("stop-a", ownersStop(t, root, "a")))
		if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil || held.Hold.Reason != record.HoldTaskStopped {
			t.Fatalf("premise: %s after a's stop stands in %s (hold %+v), want held for the stopped task", root.Key, held.Phase, held.Hold)
		}

		mustRefuse(t, "a retry of k's claim", apply("retry-k", retry(t, "k")),
			"TASK_STOPPED", intake.RetryOfStoppedTask(root.Key, "a", tasksOf(t, pool, engine, root.Key)["a"].StoppedReason).Message)
		mustRefuse(t, "a retry of the claim of b's agent", apply("retry-tester", retry(t, "tester")),
			"TASK_REQUIRED", "tasks a of LEGION-208 each stand at held: name the task with task.")
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskStopped, atStep(1))
	})

	t.Run("two stopped tasks of one agent", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "b", steps: []string{"tester:k"}, agents: []claim.Agent{"k"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "c", steps: []string{"reviewer"}, agents: []claim.Agent{"reviewer"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
		apply := applyFacts(t, pool, engine)
		mustSpend(t, "the owner's stop of a", apply("stop-a", ownersStop(t, root, "a")))
		mustSpend(t, "the owner's stop of b", apply("stop-b", ownersStop(t, root, "b")))

		mustRefuse(t, "a retry of k's claim", apply("retry-k", retry(t, "k")),
			"TASK_REQUIRED", "tasks a, b of LEGION-208 each stand at held: name the task with task.")
		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "a", record.TaskStopped, atStep(1))
		assertTask(t, tasks, "b", record.TaskStopped, atStep(1))
	})
}

// The owner's move back of a plan of several tasks that names no task while none stands at its
// acceptance is refused before anything is read for a task (backwardView): of LEGION-208's tasks a,
// `implementer acceptance:owner`, and b, `implementer`, each implementing, the move back to
// implementing that names no task is refused BACKWARD_REFUSED, and nothing moves. The move was read
// on the issue itself, which names no task, so its walk of a plan of several tasks failed, and with
// it the move.
//
// The fault it catches is an owner's move back read on the issue where no task stands at the act it
// is of (its control: backwardView answering the issue itself there, as actView does).
func TestAnOwnersMoveBackWithNoTaskAtItsAcceptanceIsRefused(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "acceptance:owner"}, agents: []claim.Agent{"implementer", ""}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-2"}, state: record.TaskRunning, place: atStep(1)})

	mustRefuse(t, "a move back that names no task", applyFacts(t, pool, engine)("back-none",
		rootsMoveBack(t, pool, engine, root.Key, phase.Implementing, "the evidence does not cover the migration")),
		"BACKWARD_REFUSED", "no task of LEGION-208 stands at accepting, the act the owner's move back is of, so the architect's move to implementing changed nothing")
	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "b", record.TaskRunning, atStep(1))
}
