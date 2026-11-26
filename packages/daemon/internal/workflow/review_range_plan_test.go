package workflow

// The review step of an owner's stored plan `implementer tester reviewer` requests changes at
// headOne, the implementer and the tester run again, and the second review step's task names
// headOne as the commit the review before it read; the first review step names none (` Last
// reviewed commit: none.`). The control removes the record of the first review, and the second
// step's task then names none too.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// plannedReviewRoundOne is entersRoundOne on a run with plan rows: LEGION-208 in testing with its
// pull request at headOne, the owner's plan `implementer tester reviewer` with task a running at
// step 2 (testing), and the tester's pass that takes it into its first review step, step 3.
func plannedReviewRoundOne(t *testing.T) (*pgxpool.Pool, *Engine, func(string, intake.Fact) intake.Result) {
	t.Helper()
	pool := migratedPool(t)
	seedReviewOf(t, pool, headOne, "green")
	standsIn(t, pool, phase.Testing, "testing")
	seedStepsPlan(t, pool, "LEGION-208", 1, "implementer", "tester", "reviewer")
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
	})
	engine, apply := entersRoundOne(t, pool)
	assertWalksAt(t, pool, engine, "LEGION-208", phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 3})
	return pool, engine, apply
}

func TestAReviewStepOfAnOwnersPlanIsToldTheCommitTheReviewBeforeItRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		removed bool
	}{
		{name: "the record kept", removed: false},
		{name: "control: the record of the first review removed", removed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, engine, apply := plannedReviewRoundOne(t)
			first := reviewerRow(t, pool)
			readsReviewingTask(t, "review step 1", first.EntryTask, 1)
			if !strings.Contains(first.EntryTask, reviewedPartOf("none")) {
				t.Fatalf("the first review step's task = %q, want it to name no reviewed commit (%q): no review came before it", first.EntryTask, reviewedPartOf("none"))
			}
			apply("request-changes", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
			assertWalksAt(t, pool, engine, "LEGION-208", phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
			if tc.removed {
				if _, err := pool.Exec(context.Background(), "update phases set reviewed_head = '' where issue = 'LEGION-208' and role = 'reviewer'"); err != nil {
					t.Fatalf("remove the reviewed commit: %v", err)
				}
			}
			backToReviewingAs(t, apply, 2, func(role claim.Role) claim.Token {
				return workerOr(t, pool, "LEGION-208", role, claim.Token(string(role)+"-claim"))
			})
			assertWalksAt(t, pool, engine, "LEGION-208", phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 3})
			second := reviewerRow(t, pool)
			if second.Entry <= first.Entry {
				t.Fatalf("the second review step's row is on entry %d, want one above the first's %d", second.Entry, first.Entry)
			}
			readsReviewingTask(t, "review step 2", second.EntryTask, 2)
			named := strings.Contains(second.EntryTask, reviewedPartOf(headOne))
			if tc.removed {
				if named || !strings.Contains(second.EntryTask, reviewedPartOf("none")) {
					t.Fatalf("control: the second review step's task = %q, want it to name no reviewed commit (%q): the record of the first review was removed", second.EntryTask, reviewedPartOf("none"))
				}
				return
			}
			if !named {
				t.Fatalf("the second review step's task = %q, want it to name %s, the commit the first review read", second.EntryTask, headOne)
			}
		})
	}
}
