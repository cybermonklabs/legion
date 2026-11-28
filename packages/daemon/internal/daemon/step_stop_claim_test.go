package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The plan `implementer reviewer:k implementer:k`: key k's agent, named reviewer after its first
// step, works step 3, of the role implementer, in the claim it started on step 2, and the first
// step's agent is live in the pod from step 1. The owner stops task t at step 3, and the stop's row
// suspends the agent that ran step 3 and leaves the first step's agent running, also where a
// restart of the task commits between the stop and the row's run: a later version of the plan
// changing task t starts it again at step 1 (workflow's restartTask), and the entry of step 1 writes
// the first step's agent on the implementer's phases row. The stop is the engine's own, applied as
// the owner's stop_task (intake.TaskStopped), and its row runs on the outbox's executor; the
// restart is modelled as the write its entry makes. The control is the row run with no restart.
func TestAStepStopSuspendsTheAgentThatRanTheStoppedStep(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{name: "control: the stop's row runs before anything re-points the implementer's row"},
		{name: "a restart of the task commits between the stop and its row's run", restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true}
			putOutboxIssue(t, pool, records, issue)
			architect := mustClaimToken(t, issue.Key, claim.RoleArchitect)
			stoppedAgent := mustClaimToken(t, issue.Key, claim.RoleReviewer)
			firstAgent := mustClaimToken(t, issue.Key, claim.RoleImplementer)
			put := func(rows ...record.PhaseRow) {
				t.Helper()
				if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
					for _, row := range rows {
						if err := records.PutPhase(ctx, tx, row); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatalf("write the phases rows: %v", err)
				}
			}
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				v := record.PlanVersion{Issue: issue.Key, Generation: issue.Generation, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
					GivenBy: architect, GivenAt: time.Now()}
				task := record.PlanTask{Issue: issue.Key, Generation: issue.Generation, Task: "t", PlanVersion: 2, BlockText: "Build the widget.",
					State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 3}}
				steps := []record.PlanStep{
					{Issue: issue.Key, Generation: issue.Generation, Task: "t", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
					{Issue: issue.Key, Generation: issue.Generation, Task: "t", Position: 2, Role: "reviewer", Type: "review", AgentKey: "k", Agent: "reviewer"},
					{Issue: issue.Key, Generation: issue.Generation, Task: "t", Position: 3, Role: "implementer", Type: "implementation", AgentKey: "k", Agent: "reviewer"},
				}
				return records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps)
			}); err != nil {
				t.Fatalf("write the plan: %v", err)
			}
			put(record.PhaseRow{Issue: issue.Key, Role: claim.RoleReviewer, Claim: stoppedAgent, StepTask: "t", StepPlace: 2, Entry: 2},
				record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: stoppedAgent, StepTask: "t", StepPlace: 3, Entry: 3})
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			ready := func(token claim.Token, role claim.Role, session string) *supervise.Machine {
				t.Helper()
				m, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
					Role: role, State: supervise.StateQueued}, "")
				if err != nil {
					t.Fatalf("create claim %s: %v", token, err)
				}
				if err := m.Handle(ctx, supervise.RequestSpawn{Claim: token, Exempt: true}); err != nil {
					t.Fatalf("spawn claim %s: %v", token, err)
				}
				launch := m.Claim().Generation
				if err := m.Handle(ctx, supervise.RequestRegister{Claim: token, Generation: launch, Session: session, SessionFile: "/tmp/" + session + ".jsonl"}); err != nil {
					t.Fatalf("register claim %s: %v", token, err)
				}
				if err := m.Handle(ctx, supervise.RequestReady{Claim: token, Generation: launch, Session: session}); err != nil {
					t.Fatalf("ready claim %s: %v", token, err)
				}
				return m
			}
			stopped := ready(stoppedAgent, claim.RoleImplementer, "ses-k")
			first := ready(firstAgent, claim.RoleImplementer, "ses-first")
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
				trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
			result, err := intake.ApplyFact(ctx, pool, "test", "stop-task", intake.TaskStopped{Issue: issue.Key, Task: "t", By: architect}, engine)
			if err != nil || result.Refusal != nil {
				t.Fatalf("the owner's stop of task t = %+v, %v; want it applied", result.Refusal, err)
			}
			stops := stepStopRows(t, pool, issue.Key)
			if len(stops) != 1 {
				t.Fatalf("the stop wrote the step stops %+v, want one", stops)
			}
			if tc.restart {
				put(record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: firstAgent, StepTask: "t", StepPlace: 1, Entry: 4})
			}
			if err := runner.execute(ctx, stops[0]); err != nil {
				t.Fatalf("the stopped task's step stop: %v", err)
			}
			if got := stopped.Claim().State; got != supervise.StateSuspended {
				t.Errorf("the agent that ran the stopped step, %s, is %s after the step stop, want suspended", stoppedAgent, got)
			}
			if got := first.Claim().State; got != supervise.StateReady {
				t.Errorf("the first step's agent, %s, is %s after the step stop, want it untouched (ready)", firstAgent, got)
			}
		})
	}
}

// stepStopRows is every step_stop row the outbox holds for issue, oldest first.
func stepStopRows(t *testing.T, pool *store.Pool, issue string) []record.OutboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox
		where kind = $1 and issue = $2 and payload->>'op' = 'step_stop' order by id`, string(record.OutboxKindSupervise), issue)
	if err != nil {
		t.Fatalf("read the step stops of %s: %v", issue, err)
	}
	defer rows.Close()
	var held []record.OutboxRow
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan a step stop of %s: %v", issue, err)
		}
		held = append(held, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the step stops of %s: %v", issue, err)
	}
	return held
}
