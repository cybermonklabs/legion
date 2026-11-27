package workflow

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// A step of the type review whose role the owner's plan defines gets the commit its run before
// read, as a reviewer and a role of the deployment's do: the plan defines
// critic, of the type review, and task a is `implementer critic`; the critic's first task names no
// commit, it fails at X, the implementer pushes Y, and the critic's second task names X. The
// deployment defines no critic, so the type that keeps the commit is the one the plan's role block
// gives the step. The two controls stay in the suite: with the commit the critic's phases row holds
// cleared between the rounds, the second task names none, so the commit comes from that row alone;
// and with the role of the type testing no commit is kept, so the commit follows the role's type.
func TestAReviewStepOfARoleOfAPlanGetsTheCommitTheReviewBeforeItRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     plan.Type
		cleared bool
		names   bool
	}{
		{"the critic, of the type review", plan.TypeReview, false, true},
		{"the control: the critic with its step's commit cleared between the rounds", plan.TypeReview, true, false},
		{"the control: the critic of the type testing", plan.TypeTesting, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			if _, defined := engine.cfg.Deployment.Roles["critic"]; defined {
				t.Fatal("the engine's deployment defines critic, want it a role of the plan alone")
			}
			root := stepRoot(t, pool, phase.Admitted)
			key, gen := root.Key, root.Generation
			given := ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget, then criticise it.", "implementer", "critic"))
			given.Plan.Roles = []plan.PlanRole{{Name: "critic", Type: tc.typ, Prompt: "# Critic\n\nCriticise the change."}}
			mustTake(t, "the plan that defines critic", applyFacts(t, pool, engine)(planID(key, gen, 1), given))
			stored := planStandingOf(t, pool, engine, key, gen)
			if got, want := stepsOfTask(stored.plan, "a"), []string{"implementer/implementation", "critic/" + string(tc.typ)}; !slices.Equal(got, want) {
				t.Fatalf("the steps of a = %v, want %v: the critic's step is stored with the type its plan gives it", got, want)
			}

			move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			reportsAtEntry(t, pool, engine, "impl-1", key, gen, claim.RoleImplementer, "", stepCommitW, 0)
			first := roleRowOf(t, pool, engine, key, "critic")
			if first.EntryTask == "" {
				t.Fatalf("the critic's row after the implementer's pass = %+v, want the task of its first start", first)
			}
			if strings.Contains(first.EntryTask, reviewround.CommitLabel) {
				t.Fatalf("the critic's first task = %q, want no %s", first.EntryTask, reviewround.CommitLabel)
			}
			reportsAtEntry(t, pool, engine, "fail-1", key, gen, "critic", "fail", stepCommitX, first.Entry)
			kept := stepCommitX
			if tc.typ != plan.TypeReview {
				kept = ""
			}
			if head := stepReviewedHead(t, pool, key, "critic"); head != kept {
				t.Fatalf("the critic's step holds the commit %q after its fail at %s, want %q", head, stepCommitX, kept)
			}
			if tc.cleared {
				if _, err := pool.Exec(t.Context(), "update phases set reviewed_head = '' where issue = $1 and role = 'critic'", key); err != nil {
					t.Fatalf("clear the step's commit: %v", err)
				}
			}
			reportsAtEntry(t, pool, engine, "impl-2", key, gen, claim.RoleImplementer, "", stepCommitY, 0)

			// Each task is the one the critic's row holds for its entry (EntryTask), which each start
			// writes beside the entry it raises.
			second := roleRowOf(t, pool, engine, key, "critic")
			if second.Entry <= first.Entry {
				t.Fatalf("the critic stands on entry %d after the implementer's second pass, want one above %d", second.Entry, first.Entry)
			}
			says := reviewedPartOf(stepCommitX)
			if got := strings.Contains(second.EntryTask, says); got != tc.names {
				t.Fatalf("the critic's second task = %q; saying %q is %t, want %t", second.EntryTask, says, got, tc.names)
			}
			if !tc.names && strings.Contains(second.EntryTask, reviewround.CommitLabel) {
				t.Fatalf("the critic's second task = %q, want no %s", second.EntryTask, reviewround.CommitLabel)
			}
		})
	}
}
