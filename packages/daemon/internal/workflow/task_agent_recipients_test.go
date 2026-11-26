package workflow

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func TestAGateChangeReachesBothAgentsOfOneRole(t *testing.T) {
	pool, engine := migratedPool(t), returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-2"}, state: record.TaskRunning, place: atStep(1)})
	for _, task := range []string{"a", "b"} {
		agent := claim.Agent("implementer")
		if task == "b" {
			agent = "implementer-2"
		}
		seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: agentClaim(t, root.Key, agent), StepTask: task, StepPlace: 1, Entry: 1})
	}
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.tellGateMoved(ctx, tx, root.Key, record.DesignStepSpec, "spec-document", 2, false)
	})
	rows, err := pool.Query(t.Context(), `select payload->>'role' from outbox where issue = $1 and kind = 'role_message' order by id`, root.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var recipients []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatal(err)
		}
		recipients = append(recipients, token)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []claim.Agent{"implementer", "implementer-2"} {
		if token := string(agentClaim(t, root.Key, agent)); !slices.Contains(recipients, token) {
			t.Fatalf("gate change reached %v, missing task agent %s", recipients, token)
		}
	}
}

func TestAMergeForwardReachesOnlyItsTasksAgents(t *testing.T) {
	pool, engine := migratedPool(t), returnEngine()
	root := stepRoot(t, pool, phase.Testing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "tester", "reviewer"}, agents: []claim.Agent{"implementer", "tester", "reviewer"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "b", steps: []string{"implementer", "tester", "reviewer"}, agents: []claim.Agent{"implementer-2", "tester-2", "reviewer-2"}, state: record.TaskRunning, place: atStep(2)})
	seedTaskPRs(t, pool, root.Key, "a", "b")
	for _, task := range []string{"a", "b"} {
		for i, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			agent := claim.Agent(role)
			if task == "b" {
				agent += "-2"
			}
			seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: role, Claim: agentClaim(t, root.Key, agent), StepTask: task, StepPlace: i + 1, Entry: 1})
		}
	}
	onTask(t, pool, engine, root.Key, "a", func(ctx context.Context, tx pgx.Tx, s TaskScope) error {
		pr, err := engine.taskPullRequest(ctx, tx, s)
		if err != nil {
			return err
		}
		pr.Base = "main"
		return engine.mergeForward(ctx, tx, s.issue, s.walk, pr, record.ConflictCheckNotGreen)
	})
	rows, err := pool.Query(t.Context(), `select payload->>'claim' from outbox where issue = $1 and kind = 'merge_forward' order by id`, root.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var recipients []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatal(err)
		}
		recipients = append(recipients, token)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{string(agentClaim(t, root.Key, "implementer")), string(agentClaim(t, root.Key, "tester"))}
	if !slices.Equal(recipients, want) {
		t.Fatalf("task a's merge forward reached %v, want only %v", recipients, want)
	}
}

func TestEveryClaimReachesBothPendingAgentsOfOneRole(t *testing.T) {
	pool, engine := migratedPool(t), returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	for _, task := range []string{"a", "b"} {
		agent := claim.Agent("implementer")
		if task == "b" {
			agent = "implementer-2"
		}
		seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: agentClaim(t, root.Key, agent), StepTask: task, StepPlace: 1, Entry: 1})
	}
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.everyClaim(ctx, tx, root, "suspend", "the task work stopped", nil)
	})
	var recipients []string
	for _, row := range everyClaimRows(t, pool, root.Key) {
		if row.role == string(claim.RoleImplementer) {
			recipients = append(recipients, row.claim)
		}
	}
	want := []string{string(agentClaim(t, root.Key, "implementer")), string(agentClaim(t, root.Key, "implementer-2"))}
	if !slices.Equal(recipients, want) {
		t.Fatalf("the stop reached %v, want both pending claims %v", recipients, want)
	}
}

func TestStrandedMatchesTheAgentRatherThanAnotherTasksRole(t *testing.T) {
	issue := strandedIssue("LEGION-208", "LEGION-208", phase.Implementing)
	issue.Running = []record.RunningStep{{Task: "b", Role: claim.RoleImplementer, Agent: "implementer-2", Phase: phase.Implementing}}
	stale := strandedClaim(t, issue.Key, issue.Key, claim.RoleImplementer, supervise.StateFailed)
	current := stale
	current.Token = agentClaim(t, issue.Key, "implementer-2")
	claims := []supervise.Claim{stale, current}
	s := &strandedStore{issues: []record.Issue{issue}, slots: slotsOf(issue.Key), claims: map[claim.Token]supervise.Claim{stale.Token: stale, current.Token: current}}
	got, err := Stranded(t.Context(), nil, s, issue.Project, nil, claims, nil, strandedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Token != current.Token {
		t.Fatalf("stranded claims = %v, want only the current task's agent %s, not the completed task's %s", names(got), current.Token, stale.Token)
	}
}

func TestACheckSettlementReachesOnlyItsTasksAgents(t *testing.T) {
	pool, engine := migratedPool(t), returnEngine()
	root := stepRoot(t, pool, phase.Testing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "tester"}, agents: []claim.Agent{"implementer", "tester"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "b", steps: []string{"implementer", "tester"}, agents: []claim.Agent{"implementer-2", "tester-2"}, state: record.TaskRunning, place: atStep(2)})
	seedTaskPRs(t, pool, root.Key, "a", "b")
	onTask(t, pool, engine, root.Key, "b", func(ctx context.Context, tx pgx.Tx, s TaskScope) error {
		pr, err := engine.taskPullRequest(ctx, tx, s)
		if err != nil {
			return err
		}
		pr.CheckedHead = pr.HeadSHA
		return engine.tellChecks(ctx, tx, pr, "", nil)
	})
	rows, err := pool.Query(t.Context(), `select payload->>'role' from outbox where issue = $1 and kind = 'role_message' order by id`, root.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var recipients []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatal(err)
		}
		recipients = append(recipients, token)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{string(agentClaim(t, root.Key, "architect")), string(agentClaim(t, root.Key, "tester-2")), string(agentClaim(t, root.Key, "implementer-2"))}
	if !slices.Equal(recipients, want) {
		t.Fatalf("task b's settled checks reached %v, want only %v", recipients, want)
	}
}

func TestACheckSettlementDoesNotTellAnAgentItAlreadyWokeInAnotherRole(t *testing.T) {
	pool, engine := migratedPool(t), returnEngine()
	root := stepRoot(t, pool, phase.Testing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:k", "tester:k"}, agents: []claim.Agent{"implementer", "implementer"}, state: record.TaskRunning, place: atStep(2)})
	seedTaskPRs(t, pool, root.Key, "a")
	onTask(t, pool, engine, root.Key, "a", func(ctx context.Context, tx pgx.Tx, s TaskScope) error {
		pr, err := engine.taskPullRequest(ctx, tx, s)
		if err != nil {
			return err
		}
		pr.CheckedHead = pr.HeadSHA
		return engine.tellChecks(ctx, tx, pr, "", []claim.Role{claim.RoleTester})
	})
	var recipients []string
	rows, err := pool.Query(t.Context(), `select payload->>'role' from outbox where issue = $1 and kind = 'role_message' order by id`, root.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatal(err)
		}
		recipients = append(recipients, token)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{string(agentClaim(t, root.Key, "architect"))}
	if !slices.Equal(recipients, want) {
		t.Fatalf("the already woken agent was told again: got %v, want only %v", recipients, want)
	}
}
