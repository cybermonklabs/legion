package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan" // the package's own `plan` type holds the bare name
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A start that names no agent and makes its claim, which only a start the release before the
// record of a claim's agent queued is among the starts of a step, records on that claim the agent
// the steps give the claim's name (claim_agent_from_steps, the rule 20261125053850 fills each claim
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
				if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
					steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
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
		})
	}
}

// A start that makes its claim and then fails before the claim is brought back leaves the claim
// stored, queued, recording no agent and having run nothing, and its retry finds that claim. The
// retry records on it the agent the steps give the claim's name, as the try that made it would
// have: a start asks the steps wherever neither it nor its claim names an agent and the claim has
// run nothing, whichever try made the claim. Here the first try is cut short as it reads the issue
// again after it stored the claim, and the plan in force runs key k's agent at step 1,
// `implementer:k`. The fault: the steps were asked only on the try that made the claim, the retry
// took the start's own agent, which a start of the release before names none of, and the claim
// recorded no agent for good.
func TestAStartWhoseFirstTryFailsAfterItMakesItsClaimRecordsTheStepsAgentOnItsRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	token := mustClaimToken(t, issue.Key, claim.RoleImplementer)
	owner := mustClaimToken(t, issue.Key, claim.RoleArchitect)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
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

	// The first try ends the moment the claim it makes is stored: its context is cancelled, so
	// its next read, of the issue again, fails.
	first, cut := context.WithCancel(ctx)
	defer cut()
	claims := sup.deps.Store.(*outboxClaimStore)
	var once sync.Once
	claims.afterPut(func(c supervise.Claim) {
		if c.Token == token && c.State == supervise.StateQueued && c.LastStartRow == 0 {
			once.Do(cut)
		}
	})
	if err := runner.execute(first, row); err == nil {
		t.Fatal("the first try = nil, want it to fail after it stored its claim")
	}
	claims.afterPut(nil)
	machine, found := sup.Machine(token)
	if !found {
		t.Fatalf("the first try made no claim %s: the case's premise", token)
	}
	if got := machine.Claim(); got.State != supervise.StateQueued || got.SessionAgent != "" || got.Generation != 0 {
		t.Fatalf("the first try left the claim %s at generation %d recording the agent %q, want it queued, never launched and recording none: the case's premise",
			got.State, got.Generation, got.SessionAgent)
	}

	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("the retry = %v, want it to start the claim", err)
	}
	if got := machine.Claim().SessionAgent; got != "key:k" {
		t.Errorf("the claim the retry started records the agent %q of its session, want key:k, the agent the steps give it", got)
	}
}
