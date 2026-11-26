package store

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The idle view carries every phases row of each role as the record holds it
// (pods.IdleView.Phases): the claim the row names and the task of the step it runs. It also carries
// the project it was read under. So the worker of a role is read from it by record.WorkerClaim, the
// one rule. Here the tester's row names key a's agent on step 2, which works the tester's step. The
// implementer's row names another agent with no step, so the implementer's worker is the role's own
// token.
func TestTheIdleViewCarriesEachRolesPhasesRow(t *testing.T) {
	t.Parallel()
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
	want := map[claim.Role][]pods.IdlePhase{claim.RoleTester: {{Claim: keyed, StepTask: "a"}}, claim.RoleImplementer: {{Claim: stepless}}}
	if !view.Found || view.Project != "legion" || !maps.EqualFunc(view.Phases, want, slices.Equal) {
		t.Fatalf("the idle view of project %q carries the phases rows %v, want %v under legion", view.Project, view.Phases, want)
	}
	for role, worker := range map[claim.Role]claim.Token{claim.RoleTester: keyed, claim.RoleImplementer: "legion-legion-legion-208-implementer"} {
		var rows []record.PhaseRow
		for _, row := range view.Phases[role] {
			rows = append(rows, record.PhaseRow{Issue: view.Issue, Role: role, Claim: row.Claim, StepTask: row.StepTask})
		}
		got, err := record.WorkerClaim(view.Project, view.Issue, role, rows)
		if err != nil || got != worker {
			t.Errorf("the %s's worker read from the idle view = %q, %v; want %s", role, got, err, worker)
		}
	}
}
