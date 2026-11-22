package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
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
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
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
