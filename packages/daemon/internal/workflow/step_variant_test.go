package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Each write of a step record names the experiment and the variant of its issue's group, the row
// of its draw that says how the issue is done (groupOf): the start and the end of a step, the
// retro's rating and a late completion's report alike, so Dispatch can tell each run of an issue in
// an experiment by its variant. An issue whose draw names no group, here one with no row at all,
// writes neither key in any payload, so its rows are the rows the release before queued. The three
// walks are the opening of an issue's gate, which starts its planner, and the planner's
// completion, which ends the planner and starts the implementer; the sign-off of an issue at
// accepting, which ends the acceptance and starts the retro, and the retro's completion with its
// rating, which ends the retro, rates the plan and starts the merge; and the review's approval
// before the reviewer completed, which ends the review and starts the acceptance, and the
// reviewer's completion after it, whose report is written onto the ended review
// (reportEndedStep). Each walk runs for an issue drawn into the variant b of planning and for one
// with no row, and every step record row of each is read as the outbox holds it, its payload as
// raw JSON.
func TestEachStepRecordNamesTheVariantOfItsIssue(t *testing.T) {
	walks := []struct {
		name string
		// from is the phase LEGION-208 stands at before the walk.
		from phase.Phase
		// walk drives LEGION-208 from there to where the walk ends, on pool.
		walk func(t *testing.T, pool *pgxpool.Pool)
		// writes is how many step record rows the walk queues: what this test reads.
		writes int
		// marked is the member one row of the walk carries: the write the walk is here to read,
		// which no other walk queues ("" for none).
		marked string
	}{
		{name: "the planner's start and end, and the implementer's start", from: phase.Admitted, writes: 3, walk: func(t *testing.T, pool *pgxpool.Pool) {
			engine := stepEngine(nil)
			firstStep(t, pool, engine, "LEGION-208")
			move(t, pool, engine, "LEGION-208", TriggerPlannerCompleted, record.TaskPlace{}, record.PhaseRow{})
		}},
		{name: "the acceptance's end, the retro's start, end and rating, and the merge's start", from: phase.Accepting, writes: 5, marked: "rating", walk: func(t *testing.T, pool *pgxpool.Pool) {
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := stepEngine(nil)
			move(t, pool, engine, "LEGION-208", TriggerSignOff, record.TaskPlace{}, record.PhaseRow{})
			applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer,
				Claim: "implement-claim", Summary: "retro done", Commit: "head",
				PlanRating: &record.PlanRating{Rating: 4, Reason: "The plan fit the work."}})
			assertPhase(t, pool, phase.Merging)
		}},
		// The review's run is one the boot opens (bootBefore, as reviewApprovedNow runs), so its
		// start queues no row.
		{name: "the review's end, the acceptance's start, and the reviewer's late report", from: phase.Reviewing, writes: 3, marked: "report", walk: func(t *testing.T, pool *pgxpool.Pool) {
			engine := returnEngine()
			reviewer := seedPhaseWorker(t, pool, "LEGION-208", claim.RoleReviewer, "")
			reviewApprovedNow(t, pool, engine, "LEGION-208")
			assertPhase(t, pool, phase.Accepting)
			if result := applyFacts(t, pool, engine)("late-report", reportOf(claim.RoleReviewer, reviewer, "review-1")); result.Refusal != nil {
				t.Fatalf("the reviewer's completion after the approval = %+v, want it taken, with its report on the ended review", result.Refusal)
			}
		}},
	}
	for _, walk := range walks {
		for _, drawn := range []bool{true, false} {
			name := walk.name + ", of an issue with no group"
			if drawn {
				name = walk.name + ", of an issue in the variant b of planning"
			}
			t.Run(name, func(t *testing.T) {
				pool := migratedPool(t)
				stepRoot(t, pool, walk.from)
				if drawn {
					drawnInto(t, pool, "LEGION-208", "planning", "b", "default")
				}
				walk.walk(t, pool)
				rows := issueOutboxOf(t, pool, "LEGION-208", string(record.OutboxKindStepRecord))
				if len(rows) != walk.writes {
					payloads := make([]string, len(rows))
					for i, row := range rows {
						payloads[i] = string(row.payload)
					}
					t.Fatalf("the walk queued %d step record rows, want %d:\n%s", len(rows), walk.writes, strings.Join(payloads, "\n"))
				}
				marked := false
				for _, row := range rows {
					fields := stepRecordFields(t, row.payload)
					_, carries := fields[walk.marked]
					marked = marked || carries
					assertNamesTheVariant(t, row.payload, fields, drawn)
				}
				if walk.marked != "" && !marked {
					t.Fatalf("no row of the walk carries %s, so this test reads no such write", walk.marked)
				}
			})
		}
	}
}

// The start the daemon's boot opens for a run that no start of this release counted
// (OpenUncountedRuns) names the experiment and the variant of its issue's group, as every other
// start does. The boot queues no record, so its write is the start the place's row keeps
// (step_starts.record): what an end written from the kept start writes again (endOffWalk, and the
// roll forward of a rollback of the release that records each step). An issue with no group keeps
// a start that names neither. The run is the tester's, which the release before started, so its
// place has no row until the boot.
func TestTheStartTheBootOpensNamesTheVariantOfItsIssue(t *testing.T) {
	for _, drawn := range []bool{true, false} {
		name := "of an issue with no group"
		if drawn {
			name = "of an issue in the variant b of planning"
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			stepRoot(t, pool, phase.Implementing)
			if drawn {
				drawnInto(t, pool, "LEGION-208", "planning", "b", "default")
			}
			move(t, pool, stepEngine(nil), "LEGION-208", TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
			execSQL(t, pool, `delete from step_starts where issue = 'LEGION-208'`)
			if opened := openUncountedRuns(t, pool, stepEngine(nil)); opened != 1 {
				t.Fatalf("the boot opened %d runs, want the tester's", opened)
			}
			testing3 := placeOf("step", 3)
			var kept []byte
			if err := pool.QueryRow(t.Context(), `select record from step_starts where issue = 'LEGION-208' and task = $1 and kind = $2 and place = $3`,
				testing3.Task, testing3.Kind, testing3.Place).Scan(&kept); err != nil {
				t.Fatalf("read the start the boot kept: %v", err)
			}
			assertNamesTheVariant(t, kept, stepRecordFields(t, kept), drawn)
		})
	}
}

// stepRecordFields is the members of a step record's JSON, each as it is written.
func stepRecordFields(t *testing.T, payload []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("read a step record %s: %v", payload, err)
	}
	return fields
}

// assertNamesTheVariant holds fields, the members of the step record payload, to the variant b of
// planning where drawn, and to neither key where the issue has no group.
func assertNamesTheVariant(t *testing.T, payload []byte, fields map[string]json.RawMessage, drawn bool) {
	t.Helper()
	experiment, named := fields["experiment"]
	variant, varied := fields["variant"]
	switch {
	case drawn && (string(experiment) != `"planning"` || string(variant) != `"b"`):
		t.Errorf("the record %s names the experiment %s and the variant %s, want planning and b, its issue's", payload, experiment, variant)
	case !drawn && (named || varied):
		t.Errorf("the record %s of an issue with no group names the experiment %s and the variant %s, want neither key", payload, experiment, variant)
	}
}
