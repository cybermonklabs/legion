package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// seedTwoTasks gives key, at generation 1, an owner's plan of two tasks, each with the steps
// implementer and tester, each step its own agent's: task a's steps are of the agents implementer-2
// and tester, and it stands at its tester's step 2 at pass 3; task b's are of implementer and
// tester-2, and it stands at its implementer's step 1 at pass 1. The issue stands in implementing,
// where b works (the first phase of its running tasks in today's order).
func seedTwoTasks(t *testing.T, h *harness, key string) {
	t.Helper()
	ctx, records := context.Background(), record.NewStore()
	v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-" + key, Source: record.PlanSourceOwner,
		GivenBy: "owner", GivenAt: time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)}
	var tasks []record.PlanTask
	var steps []record.PlanStep
	for _, task := range []struct {
		id                string
		implementer, test claim.Agent
	}{{"a", "implementer-2", "tester"}, {"b", "implementer", "tester-2"}} {
		tasks = append(tasks, record.PlanTask{Issue: key, Generation: 1, Task: task.id, PlanVersion: 1, BlockText: "Task " + task.id + ".", State: record.TaskWaiting})
		steps = append(steps,
			record.PlanStep{Issue: key, Generation: 1, Task: task.id, Position: 1, Role: "implementer", Type: "implementation", Agent: task.implementer},
			record.PlanStep{Issue: key, Generation: 1, Task: task.id, Position: 2, Role: "tester", Type: "testing", Agent: task.test})
	}
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		for _, at := range []struct {
			task        string
			place, pass int
		}{{"a", 2, 3}, {"b", 1, 1}} {
			if err := records.MoveTask(ctx, tx, key, 1, at.task, record.TaskPlace{Kind: record.PlaceStep, Position: at.place}, record.TaskRunning); err != nil {
				return err
			}
			// The move writes the wait of a start for its join; this seed stands for a task whose join ended.
			if err := records.SetStepWait(ctx, tx, key, 1, at.task, record.StepWaitNone, nil); err != nil {
				return err
			}
			if err := records.PutReturnCounts(ctx, tx, key, 1, at.task, record.ReturnCounts{Pass: at.pass}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the plan of %s: %v", key, err)
	}
}

// A completion of an issue whose plan holds several tasks is of the task of the step its claim's
// agent runs (record.Issue.StepOfAgent): with task a at its tester's step and task b at its
// implementer's, while the issue stands in implementing, the tester's completion is keyed by a's
// phase, testing, and a's pass, never the issue's phase, and the fact names task a. A completion
// of an agent that runs no step of the issue, the reviewer's, is refused HANDOFF_NOT_CURRENT_PHASE
// and nothing is applied. The key of a plan of one task stays as it was
// (TestTheCompletionKeyNamesTheTaskOfAnIssueWithPlanRows).
//
// The fault it catches is a completion keyed by, and so taken as, the issue's phase rather than
// its own task's (its control: the route's read of the plan's one task, which a plan of two
// refuses, and the issue's phase in the key).
func TestACompletionsKeyNamesItsTasksPhase(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	const key = "LEGION-211"
	seedIssueAt(t, h, key, phase.Implementing)
	seedTwoTasks(t, h, key)

	tester := newLiveClaim(t, h, key, claim.RoleTester)
	written := tester.writeHandoff("test", 1)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", HandoffID: written, HandoffVersion: 1}))
	if got, want := completionKeys(t, h, key), "handoff:"+key+":1:1:1:tester:testing:a:3:"+written+":pass"; len(got) != 1 || got[0] != want {
		t.Fatalf("the completion keys of %s = %q, want [%q]: the tester's completion is of task a, at testing", key, got, want)
	}
	applied := facts.recorded()
	if fact, ok := applied[len(applied)-1].(intake.HandoffComplete); !ok || fact.Task != "a" {
		t.Fatalf("the tester's completion applied %#v, want a completion of task a", applied[len(applied)-1])
	}

	reviewer := newLiveClaim(t, h, key, claim.RoleReviewer)
	refused := h.complete(HandoffCompleteRequest{GrantID: reviewer.grant(t), Summary: "reviewed", HandoffID: reviewer.writeHandoff("review", 1), HandoffVersion: 1})
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), "HANDOFF_NOT_CURRENT_PHASE") {
		t.Fatalf("the completion of the reviewer, which runs no step of %s = %d: %s, want 409 HANDOFF_NOT_CURRENT_PHASE", key, refused.Code, refused.Body)
	}
	if got := len(facts.recorded()); got != len(applied) {
		t.Fatalf("the reviewer's completion applied %d facts, want none", got-len(applied))
	}
}

// The completion route tells the daemon's hook (Options.HandoffCompleted) the issue and the task
// whose step completed, so the read of GitHub the hook queues judges that task's pull request:
// with task a at its tester's step and task b at its implementer's, the tester's completion tells
// the hook task a. A completion of an issue whose plan holds one task tells the hook that task
// (TestACompletionOfAPlanOfOneTaskNamesItsTask).
//
// The fault it catches is a hook told the issue alone, so the read after a completion judges the
// task the implementer's phases row names rather than the one that completed (its control: the
// route telling the hook no task).
func TestTheCompletionHookIsToldTheCompletingTask(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	const several, one = "LEGION-211", "LEGION-213"
	seedIssueAt(t, h, several, phase.Implementing)
	seedTwoTasks(t, h, several)
	tester := newLiveClaim(t, h, several, claim.RoleTester)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}))

	seedIssueAt(t, h, one, phase.Testing)
	seedTaskPlan(t, h, one, taskStanding{task: "a", state: record.TaskRunning, place: 2})
	alone := newLiveClaim(t, h, one, claim.RoleTester)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: alone.grant(t), Summary: "tests pass", Verdict: "pass", HandoffID: alone.writeHandoff("test", 1), HandoffVersion: 1}))

	if got, want := facts.completions(), []toldCompletion{{issue: several, task: "a"}, {issue: one, task: "a"}}; !slices.Equal(got, want) {
		t.Fatalf("the completion hook was told %+v, want %+v: the task whose step completed, a plan of one task's among them", got, want)
	}
}

// The completion of an issue whose plan holds one task names that task, as the completion of a plan
// of several names the task of its step: the fact the route applies names it
// (intake.HandoffComplete.Task), and so does the daemon's hook (Options.HandoffCompleted), so the
// read of GitHub after the completion judges the pull request of the task the route read. Task a of
// LEGION-213 stands at its tester's step, its start delivered, so the tester runs that step; task a
// of LEGION-214 stands there with its start not yet delivered, so the tester runs no step, and its
// completion is of the task that merges, the plan's one task. An issue with no plan rows,
// LEGION-215, names none (the control).
//
// The fault it catches is a plan of one task read as no task, so its fact and the read after it
// name none and the read judges its task by the implementer's phases row (its control: the route
// naming no task for a plan of one task).
func TestACompletionOfAPlanOfOneTaskNamesItsTask(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	const delivered, waiting, none = "LEGION-213", "LEGION-214", "LEGION-215"
	for _, key := range []string{delivered, waiting, none} {
		seedIssueAt(t, h, key, phase.Testing)
	}
	seedTaskPlan(t, h, delivered, taskStanding{task: "a", state: record.TaskRunning, place: 2})
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().SetStepWait(context.Background(), tx, delivered, 1, "a", record.StepWaitNone, nil)
	}); err != nil {
		t.Fatalf("deliver the start of task a of %s: %v", delivered, err)
	}
	seedTaskPlan(t, h, waiting, taskStanding{task: "a", state: record.TaskRunning, place: 2})

	var named []string
	for _, key := range []string{delivered, waiting, none} {
		tester := newLiveClaim(t, h, key, claim.RoleTester)
		completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}))
		applied := facts.recorded()
		fact, ok := applied[len(applied)-1].(intake.HandoffComplete)
		if !ok {
			t.Fatalf("the tester's completion of %s applied %#v, want a completion", key, applied[len(applied)-1])
		}
		named = append(named, fact.Task)
	}
	if want := []string{"a", "a", ""}; !slices.Equal(named, want) {
		t.Fatalf("the completions named the tasks %q, want %q: the plan's one task, and none for no plan rows", named, want)
	}
	if got, want := facts.completions(), []toldCompletion{{issue: delivered, task: "a"}, {issue: waiting, task: "a"}, {issue: none}}; !slices.Equal(got, want) {
		t.Fatalf("the completion hook was told %+v, want %+v", got, want)
	}
}

// A completion can reach the route after its step ended: the daemon ends no turn when a task moves,
// so a tester whose task moved back to its implementer's step completes the run the move ended. Its
// agent runs no step now, and its completion is of the task of the step that the latest phases row
// naming its claim runs (record.PhaseRow.StepTask). The late completion of a plan of one task
// (LEGION-212) is taken as before, keyed by its issue's phase and its task's pass, and now names its
// task; that of a plan of two tasks that no task needs, so that none merges (LEGION-211), is taken
// as task a's, which its row names, keyed by a's phase and a's pass, where before it was refused
// HANDOFF_NOT_CURRENT_PHASE and its report lost.
//
// The fault it catches is a late completion read by its agent's running step alone (its controls:
// the route reading no phases row, which leaves the plan of two tasks no task, since none merges;
// and the running step alone, which refuses the late completion of a plan of one task too).
func TestALateCompletionIsOfTheTaskItsPhasesRowNames(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	const one, several = "LEGION-212", "LEGION-211"
	ctx, records := context.Background(), record.NewStore()
	seedIssueAt(t, h, one, phase.Implementing)
	seedTaskPlan(t, h, one, taskStanding{task: "a", state: record.TaskRunning, place: 1, pass: 2})
	seedIssueAt(t, h, several, phase.Implementing)
	seedTwoTasks(t, h, several)
	// Task a's tester's step ended: a moved back to its implementer's step, which runs.
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.MoveTask(ctx, tx, several, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning); err != nil {
			return err
		}
		return records.SetStepWait(ctx, tx, several, 1, "a", record.StepWaitNone, nil)
	}); err != nil {
		t.Fatalf("move task a of %s back to its implementer's step: %v", several, err)
	}

	for _, tc := range []struct{ key, want string }{
		{one, "handoff:" + one + ":1:1:1:tester:implementing:a:2:%s:pass"},
		{several, "handoff:" + several + ":1:1:1:tester:implementing:a:3:%s:pass"},
	} {
		tester := newLiveClaim(t, h, tc.key, claim.RoleTester)
		// The row the start of a's tester's step raised, which names its claim and its step.
		if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return records.PutPhase(ctx, tx, record.PhaseRow{Issue: tc.key, Role: claim.RoleTester, Claim: tester.token, StepTask: "a", StepPlace: 2})
		}); err != nil {
			t.Fatalf("seed the tester's row of %s: %v", tc.key, err)
		}
		written := tester.writeHandoff("test", 1)
		completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", HandoffID: written, HandoffVersion: 1}))
		if got, want := completionKeys(t, h, tc.key), fmt.Sprintf(tc.want, written); len(got) != 1 || got[0] != want {
			t.Fatalf("the completion keys of %s = %q, want [%q]: the late completion is of task a, at the phase a stands in", tc.key, got, want)
		}
		applied := facts.recorded()
		if fact, ok := applied[len(applied)-1].(intake.HandoffComplete); !ok || fact.Task != "a" {
			t.Fatalf("the tester's late completion of %s applied %#v, want a completion of task a", tc.key, applied[len(applied)-1])
		}
	}
	if got, want := facts.completions(), []toldCompletion{{issue: one, task: "a"}, {issue: several, task: "a"}}; !slices.Equal(got, want) {
		t.Fatalf("the completion hook was told %+v, want %+v", got, want)
	}
}

// A phase worker's move back is of the task of its own step for a plan of any count of tasks, one
// among them (taskOfAgent): the tester of LEGION-216, whose plan's one task a stands at the tester's
// step, moves it back, and the fact names task a, whether the request names no task or names a. The
// move of an issue with no plan rows, LEGION-217, names none (the control).
//
// The fault it catches is the move of a plan of one task read as no task's, so its fact names none
// and a request that names the task is refused TASK_NOT_GRANTS (its control: the route reading no
// task for a plan of one task).
func TestAWorkersMoveBackOfAPlanOfOneTaskNamesItsTask(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	const one, none = "LEGION-216", "LEGION-217"
	seedIssueAt(t, h, one, phase.Testing)
	seedTaskPlan(t, h, one, taskStanding{task: "a", state: record.TaskRunning, place: 2})
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().SetStepWait(context.Background(), tx, one, 1, "a", record.StepWaitNone, nil)
	}); err != nil {
		t.Fatalf("deliver the start of task a of %s: %v", one, err)
	}
	seedIssueAt(t, h, none, phase.Testing)
	testers := map[string]liveClaim{one: newLiveClaim(t, h, one, claim.RoleTester), none: newLiveClaim(t, h, none, claim.RoleTester)}

	for _, tc := range []struct{ key, named, want string }{{one, "", "a"}, {one, "a", "a"}, {none, "", ""}} {
		moved := h.request(http.MethodPost, "/legion/v1/phase/backward", PhaseBackwardRequest{GrantID: testers[tc.key].grant(t), To: phase.Implementing,
			Reason: "the suite needs a fix", Task: tc.named}, nil)
		if moved.Code != http.StatusOK {
			t.Fatalf("the tester's move back of %s naming task %q = %d: %s, want 200", tc.key, tc.named, moved.Code, moved.Body)
		}
		applied := facts.recorded()
		if fact, ok := applied[len(applied)-1].(intake.BackwardMove); !ok || fact.Task != tc.want {
			t.Fatalf("the tester's move back of %s naming task %q applied %#v, want a move of task %q", tc.key, tc.named, applied[len(applied)-1], tc.want)
		}
	}
}

// A completion sent again is refused HANDOFF_ALREADY_RECORDED in words that name its task, since
// the pass the refusal counts is that task's own (completionPosition): with task a of LEGION-211 at
// its tester's step at pass 3 and task b at its implementer's at pass 1, the tester's completion
// sent again is refused naming task a, a's phase and a's pass, and nothing of b. The completion of
// LEGION-215, which has no plan rows, sent again is refused in the words it was before plans.
//
// The fault it catches is a refusal that tells one task's pass as the issue's, so the worker reading
// it cannot say whose pass it was (its control: the task left out of the refusal's words).
func TestACompletionSentAgainIsRefusedNamingItsTask(t *testing.T) {
	h, _ := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	const several, none = "LEGION-211", "LEGION-215"
	seedIssueAt(t, h, several, phase.Implementing)
	seedTwoTasks(t, h, several)
	seedIssueAt(t, h, none, phase.Testing)

	for _, tc := range []struct{ key, words string }{
		{several, "the tester completion of phase testing of task a (pass 3 through it)"},
		{none, "the tester completion of phase testing (pass 0 through it)"},
	} {
		tester := newLiveClaim(t, h, tc.key, claim.RoleTester)
		written := tester.writeHandoff("test", 1)
		request := HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", HandoffID: written, HandoffVersion: 1}
		completion(t, h.complete(request))
		request.GrantID = tester.grant(t)
		refusedSaying(t, h.complete(request), http.StatusConflict, "HANDOFF_ALREADY_RECORDED",
			tc.words+" naming handoff record "+written+" (test version 1) was already received; this call changed nothing")
	}
}
