package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// placeRoleCase is a plan of one task a and the types its rows record, as the table of
// record.PlaceWorker holds it (record/place_worker_test.go).
type placeRoleCase struct {
	steps string
	types []plan.Type
}

// placeRoleCases are the plans of record.PlaceWorker's table: a retro that is the implementer's, a
// keyed implementer's, the owner's for a task with no step of the type implementation, for a task
// whose only such step is the owner's and for one whose last such step is, and a coder's, a role of
// the deployment of the type implementation.
var placeRoleCases = []placeRoleCase{
	{"implementer tester", []plan.Type{plan.TypeImplementation, plan.TypeTesting}},
	{"implementer:x reviewer implementer:y", []plan.Type{plan.TypeImplementation, plan.TypeReview, plan.TypeImplementation}},
	{"tester", []plan.Type{plan.TypeTesting}},
	{"implementer:owner reviewer", []plan.Type{plan.TypeImplementation, plan.TypeReview}},
	{"implementer reviewer implementer:owner", []plan.Type{plan.TypeImplementation, plan.TypeReview, plan.TypeImplementation}},
	{"coder checker", []plan.Type{plan.TypeImplementation, plan.TypeTesting}},
	{"checker", []plan.Type{plan.TypeTesting}},
}

// seedPlaceRolePlan writes key's plan of c at generation, each step's type as c records it, its task
// at no act.
func seedPlaceRolePlan(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, c placeRoleCase) {
	t.Helper()
	words := strings.Fields(c.steps)
	seedStepsPlan(t, pool, key, generation, words...)
	for i, typ := range c.types {
		if _, err := pool.Exec(t.Context(), "update plan_steps set type = $3 where issue = $1 and task = 'a' and position = $2", key, i+1, string(typ)); err != nil {
			t.Fatalf("store the type of step %d of %s: %v", i+1, key, err)
		}
	}
}

// moveTaskTo moves key's task to place, running, standing in to, through the one writer of where a
// task stands (moveTask), and answers the issue it moved, as that move left it in memory. Like the
// reads below (issueAndWalkAt), it runs in a fact's transaction with no boot pass before it: the
// move counts no start, as no test of the running role needs one.
func moveTaskTo(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, to phase.Phase, place record.TaskPlace) record.Issue {
	t.Helper()
	var moved record.Issue
	factTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		w, err := engine.walk(ctx, tx, *issue)
		if err != nil {
			return err
		}
		if err := engine.moveTask(ctx, tx, issue, &w, to, place, record.TaskRunning); err != nil {
			return err
		}
		moved = *issue
		return engine.store.PutIssue(ctx, tx, *issue)
	})
	return moved
}

// issueAndWalkAt is key as the store reads it now and the walk of its run, read in one transaction
// with no boot pass before it.
func issueAndWalkAt(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) (record.Issue, Walk) {
	t.Helper()
	var issue *record.Issue
	var w Walk
	factTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if issue, err = engine.store.Issue(ctx, tx, key); err != nil || issue == nil {
			return err
		}
		w, err = engine.walk(ctx, tx, *issue)
		return err
	})
	if issue == nil {
		t.Fatalf("the store holds no %s", key)
	}
	return *issue, w
}

// The running role of an issue names the claim that works the place its task stands at: the
// architect at a step or a retro the owner does, the agent acceptance at a step acceptance with no
// key, the role of the retro's agent (record.PlaceWorker) at the retro, and the step's role otherwise.
// The move of the task sets it (moveTask), and a read of the issue gives the same (Issue.PlaceRole);
// RoleAt answers it at each phase a place runs as, and OpenRoleOf, which names no architect, answers
// none at the owner's place. A hold keeps it for the phase it left, and an issue with no plan rows
// reads none, and the role of each phase as before. The control is the running role read from the
// step's role alone (RoleAt's StepRole and phase.RoleFor): the owner's step reads the implementer,
// whose worker a boot, a wake, a silence repair or a catch-up would start for it.
func TestTheRunningRoleNamesTheClaimThatWorksThePlace(t *testing.T) {
	retro := record.TaskPlace{Kind: record.PlaceRetro}
	for _, tc := range []struct {
		name, steps string
		types       map[string]string
		to          phase.Phase
		place       record.TaskPlace
		want, open  claim.Role
	}{
		{name: "the owner's implementer step", steps: "implementer:owner reviewer", to: phase.Implementing, place: stepPlace(1), want: claim.RoleArchitect, open: ""},
		{name: "a fresh agent of acceptance", steps: "implementer reviewer acceptance", to: phase.Accepting, place: stepPlace(3), want: claim.RoleAcceptance, open: claim.RoleAcceptance},
		{name: "the owner's retro of the plan tester", steps: "tester", to: phase.Retro, place: retro, want: claim.RoleArchitect, open: ""},
		{name: "the implementer's retro", steps: "implementer tester", to: phase.Retro, place: retro, want: claim.RoleImplementer, open: claim.RoleImplementer},
		{name: "a coder's retro", steps: "coder checker", types: map[string]string{"coder": "implementation", "checker": "testing"}, to: phase.Retro, place: retro, want: "coder", open: "coder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := coderEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, strings.Fields(tc.steps)...)
			for role, typ := range tc.types {
				if _, err := pool.Exec(t.Context(), "update plan_steps set type = $3 where issue = $1 and task = 'a' and role = $2", root.Key, role, typ); err != nil {
					t.Fatalf("store the type of %s: %v", role, err)
				}
			}
			moved := moveTaskTo(t, pool, engine, root.Key, tc.to, tc.place)
			read, _ := issueAndWalkAt(t, pool, engine, root.Key)
			for name, issue := range map[string]record.Issue{"the move": moved, "the read": read} {
				if issue.PlaceRole != tc.want || issue.RoleAt(issue.Phase) != tc.want || OpenRoleOf(issue) != tc.open {
					t.Errorf("%s of %s at %+v: PlaceRole %q, RoleAt(%s) %q, OpenRoleOf %q; want %q, %q, %q",
						name, tc.steps, tc.place, issue.PlaceRole, issue.Phase, issue.RoleAt(issue.Phase), OpenRoleOf(issue), tc.want, tc.want, tc.open)
				}
			}
		})
	}

	t.Run("a hold keeps the role of the place it left", func(t *testing.T) {
		pool := migratedPool(t)
		engine := coderEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "reviewer")
		moved := moveTaskTo(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
		moved.Phase, moved.Hold = phase.Held, &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated}
		seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutIssue(t.Context(), tx, moved) })
		held, _ := issueAndWalkAt(t, pool, engine, root.Key)
		if held.RoleAt(phase.Held) != "" || held.RoleAt(held.Hold.From) != claim.RoleArchitect {
			t.Errorf("the held owner's step: RoleAt(held) %q, RoleAt(%s) %q; want none and the architect", held.RoleAt(phase.Held), held.Hold.From, held.RoleAt(held.Hold.From))
		}
	})

	t.Run("an issue with no plan rows reads none", func(t *testing.T) {
		pool := migratedPool(t)
		engine := coderEngine()
		root := stepRoot(t, pool, phase.Implementing)
		issue, _ := issueAndWalkAt(t, pool, engine, root.Key)
		if issue.PlaceRole != "" || issue.RoleAt(phase.Implementing) != claim.RoleImplementer || issue.RoleAt(phase.Accepting) != claim.RoleArchitect {
			t.Errorf("an issue with no plan rows: PlaceRole %q, RoleAt(implementing) %q, RoleAt(accepting) %q; want none, the implementer, the architect",
				issue.PlaceRole, issue.RoleAt(phase.Implementing), issue.RoleAt(phase.Accepting))
		}
	})
}

// The store's read of the running role (Issue.PlaceRole, a subquery of the issue's plan rows) and
// the walk's (Walk.PlaceRole) give one answer at every place of every plan of record.PlaceWorker's
// table: the architect exactly where the agent at the place is the owner (record.Worker.Owner),
// else the role that works the place. So the SQL and the one rule
// cannot drift apart.
func TestThePlaceRoleOfTheStoreIsTheWalks(t *testing.T) {
	for _, c := range placeRoleCases {
		t.Run(c.steps, func(t *testing.T) {
			pool := migratedPool(t)
			engine := coderEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedPlaceRolePlan(t, pool, root.Key, root.Generation, c)
			places := []record.TaskPlace{{Kind: record.PlaceRetro}}
			for i := range c.types {
				places = append(places, stepPlace(i+1))
			}
			for _, place := range places {
				seedRecord(t, pool, func(tx pgx.Tx) error {
					return record.NewStore().MoveTask(t.Context(), tx, root.Key, root.Generation, "a", place, record.TaskRunning)
				})
				issue, w := issueAndWalkAt(t, pool, engine, root.Key)
				if got, want := issue.PlaceRole, w.PlaceRole(place); got != want || want == "" {
					t.Errorf("%s at %+v: the store reads %q, the walk %q; want one answer, a role", c.steps, place, got, want)
				}
			}
		})
	}
}
