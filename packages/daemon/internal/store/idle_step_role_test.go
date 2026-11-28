package store

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The idle view reads the role that runs the issue's step as the issue's own record reads it
// (record.Issue.StepRole): store and record import neither each other, so the view writes the
// record's subquery a second time (issueColumns), and this holds the two to one answer. An issue
// whose task stands at step 2 of `implementer checker` reads checker in both, one at step 1 reads
// implementer in both, and an issue with no plan rows reads none in both.
func TestTheIdleViewAndTheIssueReadOneRunningRole(t *testing.T) {
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
		steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
			{Issue: key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: key, Generation: 1, Task: "a", Position: 2, Role: "checker", Type: "testing"},
		})
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

// The idle view carries each role's phases row as the record holds it (pods.IdleView.Phases), the
// claim it names and the task of the step it runs, and the project it was read under, so the worker
// of a role is read from it by record.WorkerClaim, the one rule: here the tester's row names key
// a's agent on step 2, which works the tester's step, and the implementer's names another agent
// with no step, so the implementer's worker is the role's own token.
func TestTheIdleViewCarriesEachRolesPhasesRow(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := record.NewStore()
	const keyed, stepless = claim.Token("legion-legion-legion-208-implementer"), claim.Token("legion-legion-legion-208-implementer-2")
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Testing,
			Generation: 1, Status: "testing", Rank: "A", LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}); err != nil {
			return err
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: keyed, StepTask: "a", StepPlace: 2, Entry: 1}); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: stepless})
	}); err != nil {
		t.Fatalf("seed the issue and its rows: %v", err)
	}
	view := idleViewOf(t, st, "LEGION-208")
	want := map[claim.Role]pods.IdlePhase{claim.RoleTester: {Claim: keyed, StepTask: "a"}, claim.RoleImplementer: {Claim: stepless}}
	if !view.Found || view.Project != "legion" || !maps.Equal(view.Phases, want) {
		t.Fatalf("the idle view of project %q carries the phases rows %v, want %v under legion", view.Project, view.Phases, want)
	}
	for role, worker := range map[claim.Role]claim.Token{claim.RoleTester: keyed, claim.RoleImplementer: "legion-legion-legion-208-implementer"} {
		row := view.Phases[role]
		got, err := record.WorkerClaim(view.Project, view.Issue, role, []record.PhaseRow{{Issue: view.Issue, Role: role, Claim: row.Claim, StepTask: row.StepTask}})
		if err != nil || got != worker {
			t.Errorf("the %s's worker read from the idle view = %q, %v; want %s", role, got, err, worker)
		}
	}
}

// A role that stands at two steps of a task keeps a phases row for each, and the idle view carries
// the role's row now, the one record.PhaseRowOf picks: the highest entry. Here the implementer of
// `implementer:a reviewer implementer:b` worked step 3 as key b's agent (entry 2), and the task went
// back to step 1, which key a's agent works now (entry 3). Both the table's own order and its
// primary key's (step 1 before step 3) end on step 3's row, so a read in either would name key b's
// agent, which no longer works the implementer's step.
func TestAnIdleViewOfARoleAtTwoStepsCarriesItsRowNow(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := record.NewStore()
	const keyA, keyB = claim.Token("legion-legion-legion-208-implementer"), claim.Token("legion-legion-legion-208-implementer-2")
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing,
			Generation: 1, Status: "implementing", Rank: "A", LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}); err != nil {
			return err
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: keyA, StepTask: "a", StepPlace: 1, Entry: 3}); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: keyB, StepTask: "a", StepPlace: 3, Entry: 2})
	}); err != nil {
		t.Fatalf("seed the issue and its rows: %v", err)
	}
	view := idleViewOf(t, st, "LEGION-208")
	if got, want := view.Phases[claim.RoleImplementer], (pods.IdlePhase{Claim: keyA, StepTask: "a"}); got != want {
		t.Fatalf("the idle view carries the implementer's row %+v, want %+v, the row of step 1 it works now", got, want)
	}
}
