package workflow

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// projectScope is a store that reads the records of one project alone, as the daemon's store
// reads them (internal/daemon's projectRecords): a record of another project is none, by key
// (Issue), in the list of every record (Issues), in a tree (TreeIssues) and under a key
// (QueuedRootsUnder).
type projectScope struct {
	record.Store
	project string
}

func (s projectScope) own(issues []record.Issue) []record.Issue {
	return slices.DeleteFunc(issues, func(issue record.Issue) bool { return issue.Project != s.project })
}

func (s projectScope) Issue(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	issue, err := s.Store.Issue(ctx, tx, key)
	if err != nil || issue == nil || issue.Project != s.project {
		return nil, err
	}
	return issue, nil
}

func (s projectScope) Issues(ctx context.Context, tx pgx.Tx) ([]record.Issue, error) {
	all, err := s.Store.Issues(ctx, tx)
	return s.own(all), err
}

func (s projectScope) TreeIssues(ctx context.Context, tx pgx.Tx, tree string) ([]record.Issue, error) {
	all, err := s.Store.TreeIssues(ctx, tx, tree)
	return s.own(all), err
}

func (s projectScope) QueuedRootsUnder(ctx context.Context, tx pgx.Tx, key string) ([]record.Issue, error) {
	all, err := s.Store.QueuedRootsUnder(ctx, tx, key)
	return s.own(all), err
}

// A give of a plan before an issue's first step, the wait of a plan given at generation 0, and
// that first step judge an earlier owner by one read of the issue's tree: the engine's store, which
// the daemon scopes to its project (projectScope), as the routes and the owner's judgement under
// the fact lock read it. Here the tree LEGION-208 holds LEGION-209, a record of the project OTHER,
// whose architect gave version 1 of LEGION-210's plan while the engine read every project's
// records; the engine then reads LEGION's alone, so that architect owns LEGION-210 no more, and its
// version is an earlier owner's to all three:
//   - the root's architect's give of version 1 sets it aside and stores the root's own version;
//   - the first step passes it over and runs the default steps;
//   - a plan the root's architect gives at generation 0 waits for the first step, which passes
//     the version over and runs the plan of generation 0.
//
// The faults it catches are a judgement through a read of its own, each in its row: the give's
// (its control: versionRefusal judging through record.NewStore(), under which version 1 names both
// architects), the first step's (its control: record.PlansOfEarlierOwners judging through
// record.NewStore(), under which the first step runs version 1 of LEGION-209's architect, in the
// second and the third row), and the wait's (its control: waitsAtZero judging through
// record.NewStore(), under which the plan of generation 0 is refused PLAN_STALE_GENERATION).
func TestTheGiveAndTheFirstStepJudgeAnEarlierOwnerByOneRead(t *testing.T) {
	t.Parallel()
	root, other := architectOf(t), claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleArchitect))
	for _, tc := range []struct {
		name string
		then func(t *testing.T, pool *pgxpool.Pool, engine *Engine)
	}{
		{"the root's architect gives the version before the first step", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			mustTake(t, "the root's architect's version 1", applyFacts(t, pool, engine)(planID("LEGION-210", 1, 1),
				ownersPlan(t, "LEGION-210", 1, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
			if got := planStandingOf(t, pool, engine, "LEGION-210", 1).plan.Version.Givers(); !reflect.DeepEqual(got, []claim.Token{root}) {
				t.Errorf("version 1 of LEGION-210 names the givers %q, want the root's architect alone: the version of OTHER's architect set aside", got)
			}
		}},
		{"the first step", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			firstStep(t, pool, engine, "LEGION-210")
			if got := issueOf(t, pool, engine, "LEGION-210").Phase; got != phase.Planning {
				t.Errorf("LEGION-210 after its first step stands in %s, want planning, the first of the default steps: the version of OTHER's architect passed over", got)
			}
		}},
		{"a plan given at generation 0 before the first step", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			mustTake(t, "the root's architect's version 1 at generation 0", applyFacts(t, pool, engine)(planID("LEGION-210", 0, 1),
				ownersPlan(t, "LEGION-210", 0, 1, givenTask(t, "Build the widget.", "implementer", "tester"))))
			firstStep(t, pool, engine, "LEGION-210")
			if got := planStandingOf(t, pool, engine, "LEGION-210", 1).plan.Version.Givers(); !reflect.DeepEqual(got, []claim.Token{root}) {
				t.Errorf("the first step of LEGION-210 runs a version given by %q, want the root's architect's of generation 0, which waited for it", got)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			recordedAt(t, pool, "LEGION-208", "LEGION-208")
			parent := "LEGION-208"
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Parent: &parent, Project: "OTHER", Title: "LEGION-209", Phase: phase.Implementing, Generation: 1,
				Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			recordedAt(t, pool, "LEGION-210", "LEGION-208")
			given := ownersPlan(t, "LEGION-210", 1, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))
			given.GivenBy = other
			mustTake(t, "premise: version 1 of OTHER's architect, read with every project's records", applyFacts(t, pool, engine)(planID("LEGION-210", 1, 1), given))

			engine.store = projectScope{Store: engine.store, project: "LEGION"}
			tc.then(t, pool, engine)
		})
	}
}
