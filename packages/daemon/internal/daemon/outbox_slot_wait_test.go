package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The start of a second agent of one role, `…-implementer-2` on step 3 of `implementer reviewer
// implementer`, waits on its row where the issue's pod lacks the slot of that agent's name, though
// the pod holds the role implementer: a launch would go into a pod with no slot of that name, and
// no start ends another agent's process to make room. It makes no claim, the runtime is asked for
// nothing, and the agent `…-implementer`, whose slot the pod holds, runs on. Once the pod holds the
// slot (its update round replaced it), the same row creates the claim and spawns it.
func TestAStartOfAnAgentWhosePodLacksItsSlotWaits(t *testing.T) {
	lacks := map[claim.Role]string{"implementer-2": "pod-1"}
	runner, sup, rt, issue := podWaitRunner(t, lacks)
	implementer, second := agentClaimToken(t, issue.Key, "implementer"), agentClaimToken(t, issue.Key, "implementer-2")
	start := func(id int64, token claim.Token) error {
		row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Claim: token, Task: "Build it.", Generation: issue.Generation}, time.Now())
		row.ID = id
		return runner.execute(context.Background(), row)
	}
	if err := start(97, implementer); err != nil {
		t.Fatalf("the start of %s = %v, want it to create its claim", implementer, err)
	}
	testwait.WithinPolling(t, 5*time.Second, 10*time.Millisecond, "the implementer's spawn", func() bool { return len(rt.CallsOf("Spawn")) == 1 })

	err := start(98, second)
	if !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Errorf("the start of %s = %v, want a wait that is runtime.ErrAwaitsPodUpdate", second, err)
	}
	if err == nil || !strings.HasPrefix(err.Error(), pods.StartWaitsForPodPrefix) || !strings.Contains(err.Error(), "without implementer-2, the slot of the claim's agent") {
		t.Errorf("the start's text = %v, want it to begin %q and name the slot implementer-2", err, pods.StartWaitsForPodPrefix)
	}
	if _, found := sup.Machine(second); found {
		t.Errorf("the start made the claim %s, want none while it waits", second)
	}
	if spawns, resumes, suspends := rt.CallsOf("Spawn"), rt.CallsOf("Resume"), rt.CallsOf("Suspend"); len(spawns) != 1 || len(resumes)+len(suspends) > 0 {
		t.Errorf("the runtime was asked %d spawns, %d resumes and %d suspends, want the implementer's one spawn alone", len(spawns), len(resumes), len(suspends))
	}
	if _, found := sup.Machine(implementer); !found {
		t.Errorf("the claim %s is gone, want it running beside the wait", implementer)
	}

	delete(lacks, "implementer-2")
	if err := start(98, second); err != nil {
		t.Fatalf("the start of %s once its pod holds its slot = %v, want it to create its claim", second, err)
	}
	testwait.WithinPolling(t, 5*time.Second, 10*time.Millisecond, "the spawn of implementer-2", func() bool {
		for _, call := range rt.CallsOf("Spawn") {
			if call.Spec.Claim == second {
				return true
			}
		}
		return false
	})
}

// seedSlotWaitPlan records the plan of issue at its generation, one task a whose steps are
// implementer and reviewer, and a third implementer's step named implementer-2 where second.
func seedSlotWaitPlan(t *testing.T, runner *outbox, issue record.Issue, second bool) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, runner.pool, func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: issue.Key, Generation: issue.Generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: mustClaimToken(t, issue.Key, claim.RoleArchitect), GivenAt: time.Now().UTC()}
		task := record.PlanTask{Issue: issue.Key, Generation: issue.Generation, Task: "a", PlanVersion: 1, BlockText: "Build it.", State: record.TaskWaiting}
		steps := []record.PlanStep{
			{Issue: issue.Key, Generation: issue.Generation, Task: "a", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
			{Issue: issue.Key, Generation: issue.Generation, Task: "a", Position: 2, Role: "reviewer", Type: "review", Agent: "reviewer"},
		}
		if second {
			steps = append(steps, record.PlanStep{Issue: issue.Key, Generation: issue.Generation, Task: "a", Position: 3, Role: "implementer",
				Type: "implementation", Agent: "implementer-2"})
		}
		return runner.records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps)
	}); err != nil {
		t.Fatalf("seed the plan of %s: %v", issue.Key, err)
	}
}

// A start that carries no task — re-admission's walk starts each stored claim this way — of a
// claim whose agent the plan in force names at no step, `…-implementer-2` after a later version
// dropped that agent, finishes without acting where the issue's pod lacks that agent's slot:
// a pod drifts on its slots toward the agents of the plan in force alone, so no update round
// opens for that slot, and the start would wait on its row until the idle release. It makes no
// claim and asks the runtime nothing. The controls: where the plan in force names implementer-2
// the pod drifts toward its slot and the start waits for that round, and a start that carries a
// task, a step's work, waits whatever the plan names. The fault: the start of an agent the plan
// no longer names waited for a round that never opens.
func TestAStartWithNoTaskOfAnAgentThePlanNoLongerNamesFinishesWithoutAWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second bool
		task   string
		waits  bool
	}{
		{name: "an agent the plan in force no longer names"},
		{name: "control: an agent the plan in force names", second: true, waits: true},
		{name: "control: a start that carries a task", task: "Build it.", waits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, sup, rt, issue := podWaitRunner(t, map[claim.Role]string{"implementer-2": "pod-1"})
			second := agentClaimToken(t, issue.Key, "implementer-2")
			seedSlotWaitPlan(t, runner, issue, tc.second)
			row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Claim: second,
				Task: tc.task, Generation: issue.Generation}, time.Now())
			row.ID = 99

			err := runner.execute(context.Background(), row)
			if tc.waits {
				if !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
					t.Fatalf("the start of %s = %v, want a wait for its pod's update round", second, err)
				}
			} else if err != nil {
				t.Fatalf("the start of %s = %v, want it finished without acting", second, err)
			}
			if _, found := sup.Machine(second); found {
				t.Errorf("the start made the claim %s, want none", second)
			}
			if calls := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume")); calls > 0 {
				t.Errorf("the runtime was asked %d launches, want none", calls)
			}
		})
	}
}
