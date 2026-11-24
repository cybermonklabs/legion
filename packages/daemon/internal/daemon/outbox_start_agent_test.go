package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan" // the package's own `plan` type holds the bare name
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A start that names no agent and makes its claim, which only a start the release before the
// record of a claim's agent queued is among the starts of a step, records on that claim the agent
// the steps give the claim's name (claim_agent_from_steps, the rule 20261122120917 fills each claim
// of the release before by), so the next start of another agent that reaches the claim starts in a
// session of its own. The migration writes nothing into the start itself, so a daemon of the
// release before that reads the row finds it as that release wrote it. Here the plan in force runs
// key k's agent at step 1, `implementer:k`, so the claim implementer records key k. The control: on
// an issue with no plan rows the claim records no agent, as the release before ran it. The fault:
// the claim recorded no agent, and the next start of any agent took its session.
func TestAStartThatNamesNoAgentAndMakesItsClaimRecordsTheAgentTheStepsGive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		planned bool
		agent   stepplan.AgentID
	}{
		{name: "a plan whose step 1 is key k's", planned: true, agent: "key:k"},
		{name: "control: an issue with no plan rows", agent: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
			putOutboxIssue(t, pool, records, issue)
			token := mustClaimToken(t, issue.Key, claim.RoleImplementer)
			if tc.planned {
				owner := mustClaimToken(t, issue.Key, claim.RoleArchitect)
				if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
					steps := record.NameAgents(nil, nil, []record.PlanStep{
						{Issue: issue.Key, Generation: 1, Task: "p", Position: 1, Role: "implementer", Type: stepplan.TypeImplementation, AgentKey: "k"},
						{Issue: issue.Key, Generation: 1, Task: "p", Position: 2, Role: "reviewer", Type: stepplan.TypeReview},
					})
					v := record.PlanVersion{Issue: issue.Key, Generation: 1, Version: 1, ArtifactID: "plan-artifact",
						Source: record.PlanSourceOwner, GivenBy: owner, GivenAt: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}
					task := record.PlanTask{Issue: issue.Key, Generation: 1, Task: "p", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
					return records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps)
				}); err != nil {
					t.Fatalf("write the plan: %v", err)
				}
			}
			putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: token, StepTask: "p", StepPlace: 1, Entry: 2})
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
				trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Claim: token,
				Phase: phase.Implementing, Entry: 2, Task: "Build the widget.", Generation: 1}, time.Now())
			row.ID = 81

			if err := runner.execute(ctx, row); err != nil {
				t.Fatalf("the start = %v, want it to make and start the claim", err)
			}
			machine, found := sup.Machine(token)
			if !found {
				t.Fatalf("the start made no claim %s", token)
			}
			if got := machine.Claim().SessionAgent; got != tc.agent {
				t.Errorf("the claim the start made records the agent %q of its session, want %q", got, tc.agent)
			}
			if tc.agent != "" && !machine.Claim().SessionOfAnotherAgent(stepplan.AgentOf("p", 1, stepplan.Step{Role: "implementer"}).ID()) {
				t.Errorf("the claim records %q, so the start of another agent would take its session", machine.Claim().SessionAgent)
			}
		})
	}
}
