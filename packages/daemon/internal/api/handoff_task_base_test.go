package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An implementer's completion after a version is held to the base its task's start fixed: d needs
// a and c, which are done, and d's start fixed its base main, when no task needed it; a later
// version added f, which needs d, so the plan alone gives d its join branch. d's implementer
// completes with its pull request #4 open against main: the completion is applied, as a completion
// of task d.
//
// The fault it catches is a completion refused for a base no start of the task made, which asks its
// agent to move the pull request to a branch that does not exist (its control: BaseOf as the plan's
// base alone, the revision before, under which the completion is refused 409
// HANDOFF_PULL_REQUEST_BASE naming legion/LEGION-214.d.join).
func TestAnImplementersCompletionAfterAVersionKeepsTheStartsBase(t *testing.T) {
	github, served := newBaseGitHub(t)
	h, facts := newHandoffHarness(t, served, nil, nil)
	const key = "LEGION-214"
	seedIssueAt(t, h, key, phase.Implementing)
	ctx, records := context.Background(), record.NewStore()
	v := record.PlanVersion{Issue: key, Generation: 1, Version: 2, ArtifactID: "plan-" + key, Source: record.PlanSourceOwner,
		GivenBy: "owner", GivenAt: time.Date(2026, 11, 23, 22, 0, 0, 0, time.UTC)}
	var tasks []record.PlanTask
	var steps []record.PlanStep
	for _, task := range []struct {
		id    string
		needs []string
		agent claim.Agent
	}{{"a", nil, "implementer-2"}, {"c", nil, "implementer-3"}, {"d", []string{"a", "c"}, "implementer"}, {"f", []string{"d"}, "implementer-4"}} {
		tasks = append(tasks, record.PlanTask{Issue: key, Generation: 1, Task: task.id, PlanVersion: 2, BlockText: "Task " + task.id + ".", Needs: task.needs, State: record.TaskWaiting})
		steps = append(steps, record.PlanStep{Issue: key, Generation: 1, Task: task.id, Position: 1, Role: "implementer", Type: "implementation", Agent: task.agent})
	}
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		if err := records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		for _, at := range []struct {
			task  string
			state record.TaskState
		}{{"a", record.TaskDone}, {"c", record.TaskDone}, {"d", record.TaskRunning}} {
			if _, err := records.AssignTaskBranch(ctx, tx, key, 1, at.task); err != nil {
				return err
			}
			if err := records.MoveTask(ctx, tx, key, 1, at.task, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, at.state); err != nil {
				return err
			}
		}
		// d's start, when no task needed it, fixed main, and its join ended.
		if err := records.SetTaskBase(ctx, tx, key, 1, "d", "main"); err != nil {
			return err
		}
		return records.SetStepWait(ctx, tx, key, 1, "d", record.StepWaitNone, nil)
	}); err != nil {
		t.Fatalf("seed the plan of %s: %v", key, err)
	}
	github.opens("legion/LEGION-214.d", openPull{number: 4, base: "main"})

	implementer := newLiveClaim(t, h, key, claim.RoleImplementer)
	written := implementer.writeHandoff("implement", 1)
	complete := func() *httptest.ResponseRecorder {
		t.Helper()
		return h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: headSHA, HandoffID: written, HandoffVersion: 1})
	}
	completion(t, complete())
	if fact := lastCompletion(t, facts); fact.Task != "d" || fact.Role != claim.RoleImplementer {
		t.Fatalf("the completion applied %+v, want the implementer's completion of task d", fact)
	}
}
