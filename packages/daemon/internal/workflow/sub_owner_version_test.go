package workflow

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// partNeeding is the block of part id of task of, with one implementer's step keyed id, needing
// needs, as putOwnedVersion stores it.
func partNeeding(id, of string, needs ...string) plan.Task {
	return plan.Task{ID: id, Text: "Do " + id + ".", PartOf: of, Needs: needs, Steps: []plan.Step{{Role: "implementer", Key: id}}}
}

// A sub-owner acts inside the parts of its own task, and which tasks merge is the owner's to
// decide: a sub-owner's version that changes whether a task it does not write merges is refused,
// naming that task, and nothing is written. Merging reads every task's needs, a part's among them.
// In the plan a (running), f (owner=architect, at its parts), g (running, needed by none, so it
// merges) and h (needs a and f), the sub-owner of f gives a version that adds f1 needing g: g would
// no longer merge, and the version is refused. Over the plan that holds f1 needing g, its version
// that removes f1 would make g merge, and is refused too. Its version that adds f1 needing a, which
// h needs already, changes no task's merging, and is taken. The owner's own version that adds k
// needing g, the same change of g's merging, is taken, as g stands at a step.
//
// The fault it catches is a part's need that takes the retro and the merge from a task of the
// owner's (its control: mergeChangeRefusals with no rule of the giver, as at the base, takes the
// sub-owner's two versions).
func TestASubOwnerLeavesWhetherTheOwnersTasksMerge(t *testing.T) {
	t.Parallel()
	a := stepped("a", "implementer").at(record.TaskRunning, atStep(1))
	f := subOwned("f").at(record.TaskRunning, atParts)
	g := stepped("g", "implementer").at(record.TaskRunning, atStep(1))
	h := stepped("h", "implementer", "a", "f")
	blocks := []plan.Task{block("a", ""), ownedBlock("f"), block("g", ""), block("h", "", "a", "f")}
	for _, tc := range []struct {
		name    string
		seeded  []ownedTask
		version func(t *testing.T, key string, generation uint64) intake.PlanGiven
		want    *intake.Refusal
	}{
		{name: "a part's need that takes g's merge is refused", seeded: []ownedTask{a, f, g, h},
			version: func(t *testing.T, key string, generation uint64) intake.PlanGiven {
				return subOwnersVersion(t, key, generation, "f", 2, append(blocks, partNeeding("f1", "f", "g"))...)
			},
			want: intake.PlanRefused([]plan.Refusal{intake.MergeOfOwnersTask("LEGION-208", "g", "f", true)})},
		{name: "a removed part whose need kept g from merging is refused",
			seeded: []ownedTask{a, f, g, h, {id: "f1", role: "implementer", partOf: "f", needs: []string{"g"}, state: record.TaskWaiting}},
			version: func(t *testing.T, key string, generation uint64) intake.PlanGiven {
				return subOwnersVersion(t, key, generation, "f", 2, blocks...)
			},
			want: intake.PlanRefused([]plan.Refusal{intake.MergeOfOwnersTask("LEGION-208", "g", "f", false)})},
		{name: "a part's need that changes no task's merging is taken", seeded: []ownedTask{a, f, g, h},
			version: func(t *testing.T, key string, generation uint64) intake.PlanGiven {
				return subOwnersVersion(t, key, generation, "f", 2, append(blocks, partNeeding("f1", "f", "a"))...)
			}},
		{name: "the owner's version that takes g's merge is taken", seeded: []ownedTask{a, f, g, h},
			version: func(t *testing.T, key string, generation uint64) intake.PlanGiven {
				return laterVersion(t, key, generation, 2, append(blocks, block("k", "", "g"))...)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedOwnedVersion(t, pool, root.Key, root.Generation, tc.seeded...)
			before := planStandingOf(t, pool, engine, root.Key, root.Generation)
			result := applyFacts(t, pool, engine)("version-2", tc.version(t, root.Key, root.Generation))
			if tc.want != nil {
				assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "version-2", result, tc.want, before)
				return
			}
			mustTake(t, "version 2", result)
		})
	}
}

// startReadyRuns is a record.Store that counts the runs of startReady at a door (runs): each door
// runs it through startReadyNow, which reads the plan once for the run and hands it to startReady.
type startReadyRuns struct {
	record.Store
	runs *int
}

func (s startReadyRuns) Plan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (*record.StoredPlan, error) {
	if pc, _, _, ok := runtime.Caller(1); ok && strings.HasSuffix(runtime.FuncForPC(pc).Name(), ".(*Engine).startReadyNow") {
		*s.runs++
	}
	return s.Store.Plan(ctx, tx, issue, generation)
}

// The start of each task that waits runs once for a fact, at its door: a version that starts a
// stopped task with a sub-owner again at its parts runs startReady once (afterVersion), and not once
// more inside the start again. With a running, f (owner=architect) stopped at its parts, its part
// f1 waiting and h needing f, the owner's version that changes f reads the plan from startReady
// once, since no task startReady starts stands at its parts.
//
// The fault it catches is the rule of what starts after a change written at two places, which run
// it twice (its control: startAtFirstStep that runs startReadyNow at the parts, as at the base,
// which counts two runs).
func TestAVersionThatStartsATaskWithASubOwnerAgainStartsTasksOnce(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	runs := 0
	engine.store = startReadyRuns{Store: record.NewStore(), runs: &runs}
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedOwnedVersion(t, pool, root.Key, root.Generation, stepped("a", "implementer").at(record.TaskRunning, atStep(1)),
		subOwned("f").at(record.TaskRunning, atParts), part("f1", "f"), stepped("h", "implementer", "f"))
	stopTasks(t, pool, root.Key, root.Generation, "f")

	f := ownedBlock("f")
	f.Text = "Do f. Again."
	given := laterVersion(t, root.Key, root.Generation, 2, block("a", ""), f, partBlock("f1", "f", "implementer"), block("h", "", "f"))
	mustTake(t, "the owner's version 2", applyFacts(t, pool, engine)("owner-version-2", given))
	if runs != 1 {
		t.Fatalf("the version that starts f again ran startReady %d times, want once, at its door", runs)
	}
}
