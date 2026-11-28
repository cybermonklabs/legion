package store

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The agents of an issue's plan the sandbox runtime builds its pod's slots from are the agents of
// the plan in force, each named by the one rule of a stored step (plan.StoredAgentName): the plan in
// force is the issue's generation's, so a step of an earlier generation names no agent; a step of
// the key owner names none, whatever it holds; every other step holds its name, which
// 20261124213820 wrote on each step a release before the column wrote; and each agent carries the
// role of its first step. An issue of another project's record, and one with no plan rows, have
// none.
func TestThePlanAgentsAreTheAgentsOfThePlanInForce(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := record.NewStore()
	givenAt := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	issue := func(key, project string, generation uint64) record.Issue {
		return record.Issue{Key: key, Tree: key, Project: project, Title: key, Phase: phase.Implementing, Generation: generation,
			Status: "in_progress", Rank: "A", LastDispatchSeq: 1,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	}
	put := func(tx pgx.Tx, key string, generation uint64, steps []record.PlanStep) error {
		owner, err := claim.NewToken("legion", key, claim.RoleArchitect)
		if err != nil {
			return err
		}
		v := record.PlanVersion{Issue: key, Generation: generation, Version: 1, ArtifactID: "plan-artifact",
			Source: record.PlanSourceOwner, GivenBy: owner, GivenAt: givenAt}
		task := record.PlanTask{Issue: key, Generation: generation, Task: "t", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		for i := range steps {
			steps[i].Issue, steps[i].Generation, steps[i].Task, steps[i].Position = key, generation, "t", i+1
		}
		return records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps)
	}
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, seeded := range []record.Issue{issue("LEGION-208", "LEGION", 2), issue("LEGION-209", "LEGION", 1), issue("OTHER-7", "OTHER", 1)} {
			if err := records.PutIssue(ctx, tx, seeded); err != nil {
				return err
			}
		}
		if err := put(tx, "LEGION-208", 1, []record.PlanStep{
			{Role: "implementer", Type: plan.TypeImplementation, Agent: "implementer"},
			{Role: "tester", Type: plan.TypeTesting, Agent: "tester-3"},
		}); err != nil {
			return err
		}
		if err := put(tx, "LEGION-208", 2, []record.PlanStep{
			{Role: "implementer", Type: plan.TypeImplementation, Agent: "implementer"},
			{Role: "reviewer", Type: plan.TypeReview, Agent: "reviewer"},
			{Role: "tester", Type: plan.TypeTesting, AgentKey: "a", Agent: "implementer"},
			{Role: "implementer", Type: plan.TypeImplementation, Agent: "implementer-2"},
			{Role: "acceptance", Type: plan.TypeReview, AgentKey: "owner", Agent: "acceptance"},
		}); err != nil {
			return err
		}
		return put(tx, "OTHER-7", 1, []record.PlanStep{{Role: "implementer", Type: plan.TypeImplementation, Agent: "implementer-9"}})
	}); err != nil {
		t.Fatalf("seed the issues and their plans: %v", err)
	}

	want := map[claim.Agent]claim.Role{"implementer": claim.RoleImplementer, "reviewer": claim.RoleReviewer, "implementer-2": claim.RoleImplementer}
	got, err := st.PlanAgents(ctx, "legion", "LEGION-208")
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("PlanAgents(LEGION-208) = %v, %v; want %v", got, err, want)
	}
	for _, key := range []string{"LEGION-209", "OTHER-7"} {
		if got, err := st.PlanAgents(ctx, "legion", key); err != nil || len(got) != 0 {
			t.Errorf("PlanAgents(%s) = %v, %v; want none", key, got, err)
		}
	}
}
