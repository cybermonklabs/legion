package admit

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// otherProject is a project whose daemon shares the test's database: its records and its rows of
// the copy of Dispatch's issues stand beside testProject's.
const otherProject = "OTHER"

// An issue whose Dispatch parent is of another project starts on its own todo as a root of its
// own (R19 of dispatch://INFRA-3320): a parent of another project is no issue above it, as
// Dispatch's own walk climbs only within the issue's project, whether the copy holds no row of
// that parent or holds the other project's row of it, at work in that project's daemon's records.
// The workflow records nothing of it under that parent, and admission records it as its own tree
// holding its own gate, gives it a slot and starts its own architect. Its architect is the one
// every notice of it goes to, the one above it included (record.OwnerWalk), and the parent's
// architect acts on nothing of it (record.ArchitectActsOn), so a call on it is its own architect's.
func TestAnIssueUnderAParentInAnotherProjectStartsAsARoot(t *testing.T) {
	const key, parent = "LEGION-30", "OTHER-7"
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, pool *pgxpool.Pool)
	}{
		{name: "the copy holds no row of the parent", seed: func(*testing.T, *pgxpool.Pool) {}},
		{name: "the copy holds the other project's row of the parent, at work", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedSlottedForProject(t, pool, otherProject, parent, "A")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 2)
			tc.seed(t, pool)

			apply(t, pool, admission, "todo", intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: "todo", Title: key, Parent: parent, Rank: "B", HandedOver: handed}, admission.engine)

			root := maybeIssue(t, pool, key)
			if root == nil || root.Tree != key || root.Project != testProject || !root.HoldsGate || root.Phase != phase.Admitted {
				t.Fatalf("%s after its todo = %#v, want a root of its own of %s at admitted, holding its own gate", key, root, testProject)
			}
			if !holdsSlot(t, pool, key) {
				t.Fatalf("slots after the todo of %s = %#v, want one of them its own", key, slots(t, pool))
			}
			if !slices.ContainsFunc(roleStarts(t, pool, key, claim.RoleArchitect), func(start record.SuperviseRequest) bool { return start.Tree == key }) {
				t.Fatalf("outbox = %#v, want the start of %s's own architect in its own tree", effects(t, pool), key)
			}

			project, err := claim.ProjectToken(testProject)
			if err != nil {
				t.Fatal(err)
			}
			own, above := architectToken(t, key), architectToken(t, parent)
			inTx(t, pool, func(tx pgx.Tx) {
				ctx, records := context.Background(), record.NewStore()
				walk, err := record.ReadOwnerWalk(ctx, records, tx, testProject, []string{key})
				if err != nil {
					t.Fatalf("read the owner walk of %s: %v", key, err)
				}
				for _, fromParent := range []bool{false, true} {
					if got, err := walk.Architect(key, fromParent); err != nil || got != own {
						t.Fatalf("the architect a notice of %s goes to (from its parent %t) = %q (%v), want its own, %s", key, fromParent, got, err, own)
					}
				}
				chains, err := record.ReadChainsOf(ctx, tx, records, testProject, []string{key})
				if err != nil {
					t.Fatalf("read the chain of %s: %v", key, err)
				}
				if record.ArchitectActsOn(project, chains, above, key) || !record.ArchitectActsOn(project, chains, own, key) {
					t.Fatalf("on %s, %s acts %t and %s acts %t; want only its own architect", key,
						above, record.ArchitectActsOn(project, chains, above, key), own, record.ArchitectActsOn(project, chains, own, key))
				}
			})
		})
	}
}
