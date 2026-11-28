package workflow

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// walkTodaysLineTo takes key, admitted at a new generation, through its first step, which stores
// the project's default steps as every run of this release does, and along today's line to at:
// accepting, or held from implementing.
func walkTodaysLineTo(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, at phase.Phase) {
	t.Helper()
	firstStep(t, pool, engine, key)
	move(t, pool, engine, key, TriggerPlannerCompleted, record.TaskPlace{}, record.PhaseRow{})
	if at == phase.Held {
		holdFrom(t, pool, key, phase.Implementing)
	} else {
		move(t, pool, engine, key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	}
	if got := issueOf(t, pool, engine, key); got.Phase != at {
		t.Fatalf("premise: %s stands in %s after its walk, want %s", key, got.Phase, at)
	}
}

// A run the release before started has no plan rows, and walks today's line. An architect's
// sign_off, request_backward_move and retry_or_escalate on such a run are judged again on the
// record their fact reads, as they are on a run with plan rows
// (TestAnArchitectsRequestIsJudgedOnTheRecordItsFactReads). Between the route's read and the fact,
// the root can leave and be admitted again with its work back where it stood, and in accepting a
// move back can send the work back so that accepting starts again in the same generation. Each
// such request is refused, unspent, with nothing changed: its STALE_GENERATION, or for accepting
// its STALE_ENTRY. Each control is the same request on the same record with nothing between, and
// it acts.
func TestAnArchitectsRequestOnARunWithNoPlanRowsIsJudgedOnTheRecordItsFactReads(t *testing.T) {
	t.Parallel()
	// window is what commits between the route's read and the fact (between; nil for the
	// control), and the refusal the fact then answers: its code, and the whole refusal from the
	// fact, the issue as it stands after between and the run of the step it stands in then.
	type window struct {
		name    string
		between func(t *testing.T, pool *pgxpool.Pool, engine *Engine)
		code    string
		want    func(fact intake.Fact, now record.Issue, nowEntry record.StepEntry) *intake.Refusal
	}
	codes := map[string]struct{ generation, entry string }{
		"sign_off":              {"SIGNOFF_STALE_GENERATION", "SIGNOFF_STALE_ENTRY"},
		"request_backward_move": {"BACKWARD_STALE_GENERATION", "BACKWARD_STALE_ENTRY"},
		"retry_or_escalate":     {generation: "RETRY_STALE_GENERATION"},
	}
	for _, request := range architectsRequests {
		windows := []window{
			{name: "control: the request of the record its route read"},
			{"the root left and was admitted again, and its work came back to the same place", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
				readmitRoot(t, pool, engine, "LEGION-208")
				walkTodaysLineTo(t, pool, engine, "LEGION-208", request.at)
			}, codes[request.name].generation, func(fact intake.Fact, now record.Issue, _ record.StepEntry) *intake.Refusal {
				return fact.(intake.ArchitectRequest).StaleGeneration(now.Generation)
			}},
		}
		if request.at == phase.Accepting {
			windows = append(windows, window{"a move back sent the work back and accepting started again in the same generation", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
				move(t, pool, engine, "LEGION-208", TriggerBackward, stepPlace(2), record.PhaseRow{})
				move(t, pool, engine, "LEGION-208", TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
				move(t, pool, engine, "LEGION-208", TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
				move(t, pool, engine, "LEGION-208", TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
			}, codes[request.name].entry, func(fact intake.Fact, _ record.Issue, nowEntry record.StepEntry) *intake.Refusal {
				return fact.(intake.EnteredRequest).StaleEntry(nowEntry)
			}})
		}
		for _, window := range windows {
			t.Run(request.name+": "+window.name, func(t *testing.T) {
				pool := migratedPool(t)
				engine := stepEngine(nil)
				// LEGION-208 at generation 1 as the release before left it: in reviewing, which the
				// reviewer's approval ends into accepting, or held from implementing.
				if request.at == phase.Accepting {
					stepRoot(t, pool, phase.Reviewing)
					move(t, pool, engine, "LEGION-208", TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
				} else {
					stepRoot(t, pool, phase.Implementing)
					holdFrom(t, pool, "LEGION-208", phase.Implementing)
				}
				by, _, readEntry := treeArchitectsRead(t, pool, engine, "LEGION-208")
				read := issueOf(t, pool, engine, "LEGION-208")
				if rows := planRowsOf(t, pool, read.Key); read.Phase != request.at || read.Generation != 1 || rows != [3]int{} {
					t.Fatalf("premise: the route read %s in %s at generation %d with plan rows %v, want %s at generation 1 with none",
						read.Key, read.Phase, read.Generation, rows, request.at)
				}
				fact := request.fact(by, read, readEntry)
				if window.between != nil {
					window.between(t, pool, engine)
				}
				planned := planRowsOf(t, pool, read.Key) != [3]int{}
				before, standingEntry := standingOf(t, pool, engine, read.Key, planned), openStepOf(t, pool, engine, read.Key)
				result := applyFacts(t, pool, engine)("request", fact)
				if window.between == nil {
					if after := issueOf(t, pool, engine, read.Key); result.Refusal != nil || !request.moved(after) {
						t.Fatalf("the %s = %+v, and %s stands in %s; want it taken", request.name, result.Refusal, read.Key, after.Phase)
					}
					return
				}
				if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != window.code || !result.Unspent {
					t.Fatalf("the %s = %+v (unspent %v), want the 409 %s, unspent", request.name, result.Refusal, result.Unspent, window.code)
				}
				if want := window.want(fact, before.issue, standingEntry); !reflect.DeepEqual(result.Refusal, want) {
					t.Fatalf("the %s = %+v, want %+v", request.name, result.Refusal, want)
				}
				if now := standingOf(t, pool, engine, read.Key, planned); !reflect.DeepEqual(now, before) {
					t.Fatalf("the refused %s changed %s:\n before %+v\n after  %+v", request.name, read.Key, before, now)
				}
			})
		}
	}
}
