package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The idle view reads the role that runs the issue's step as the issue's own record reads it
// (record.Issue.StepRole): store and record import neither each other, so the view writes the
// record's subquery a second time (issueColumns), and this holds the two to one answer. An issue
// whose task stands at step 2 of `implementer checker` reads checker in both, one at step 1 reads
// implementer in both, and an issue with no plan rows reads none in both.
func TestTheIdleViewAndTheIssueReadOneRunningRole(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	records := record.NewStore()
	givenAt := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	issue := func(key string, p phase.Phase) record.Issue {
		return record.Issue{Key: key, Tree: key, Project: "LEGION", Title: key, Phase: p, Generation: 1,
			Status: "in_progress", Rank: "A", LastDispatchSeq: 1,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	}
	// planAt writes key's plan `implementer checker`, its task a running at step position.
	planAt := func(tx pgx.Tx, key string, position int) error {
		project, err := claim.ProjectToken("LEGION")
		if err != nil {
			return err
		}
		owner, err := claim.NewToken(project, key, claim.RoleArchitect)
		if err != nil {
			return err
		}
		v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-artifact",
			Source: record.PlanSourceOwner, GivenBy: owner, GivenAt: givenAt}
		task := record.PlanTask{Issue: key, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := []record.PlanStep{
			{Issue: key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: key, Generation: 1, Task: "a", Position: 2, Role: "checker", Type: "testing"},
		}
		if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
			return err
		}
		return records.MoveTask(ctx, tx, key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: position}, record.TaskRunning)
	}
	want := map[string]claim.Role{"LEGION-208": "checker", "LEGION-209": claim.RoleImplementer, "LEGION-210": ""}
	if err := Transact(ctx, st.pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, seeded := range []record.Issue{issue("LEGION-208", phase.Step), issue("LEGION-209", phase.Implementing), issue("LEGION-210", phase.Implementing)} {
			if err := records.PutIssue(ctx, tx, seeded); err != nil {
				return err
			}
		}
		if err := planAt(tx, "LEGION-208", 2); err != nil {
			return err
		}
		return planAt(tx, "LEGION-209", 1)
	}); err != nil {
		t.Fatalf("seed the issues and their plans: %v", err)
	}

	for key, role := range want {
		view := idleViewOf(t, st, key)
		var recorded *record.Issue
		if err := TransactWith(ctx, st.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			recorded, err = records.Issue(ctx, tx, key)
			return err
		}); err != nil || recorded == nil {
			t.Fatalf("read %s as the record does: %v", key, err)
		}
		if !view.Found || view.StepRole != recorded.StepRole || view.StepRole != role {
			t.Errorf("%s: the idle view reads the running role %q and the issue's record %q, want both %q",
				key, view.StepRole, recorded.StepRole, role)
		}
	}
}
