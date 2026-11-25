package workflow

import (
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A phase worker's move back is the decision of a turn on the task of the entry its claim serves,
// which the route reads and the move carries (intake.BackwardMove.PhaseEntry), and the workflow
// judges it on the requester's phases row under the fact's lock. Here the checker, a role of the
// deployment, took the task of entry `first` at step 2 of `implementer checker`; a move back the
// checker did not make sent the work back while that turn ran, and the implementer's completion
// entered step 2 again on entry `second`. The checker's move back of entry `first` is refused
// BACKWARD_REFUSED, unspent, and the task stays at step 2 with the checker's row as it stood. The
// controls: the same move of entry `second`, and a move that names no entry (the architect's, or
// a claim that serves none), each send the task back to implementing.
func TestAMoveBackOfAnEntryRaisedSinceMovesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry func(first, second int64) int64
		moves bool
	}{
		{"the checker's move of the entry the earlier move back left", func(first, _ int64) int64 { return first }, false},
		{"the control: the checker's move of the entry that stands", func(_, second int64) int64 { return second }, true},
		{"the control: a move that names no entry", func(int64, int64) int64 { return 0 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := deploymentEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "checker")
			if _, err := pool.Exec(t.Context(), "update plan_steps set type = 'testing' where issue = $1 and task = 'a' and position = 2", root.Key); err != nil {
				t.Fatalf("store the type of checker: %v", err)
			}
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "built", "impl-1", 0)
			assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(2))
			first := roleRowOf(t, pool, engine, root.Key, "checker").Entry

			// A move back the checker did not make, while its turn of entry `first` still runs; then
			// the implementer's fix enters step 2 again.
			move(t, pool, engine, root.Key, TriggerBackward, stepPlace(1), record.PhaseRow{})
			stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "rebased", "impl-2", 0)
			assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(2))
			second := roleRowOf(t, pool, engine, root.Key, "checker")
			if second.Entry <= first {
				t.Fatalf("the checker stands on entry %d after the re-entry, want one above %d", second.Entry, first)
			}

			entry := tc.entry(first, second.Entry)
			result := applyFacts(t, pool, engine)("checker-back", intake.BackwardMove{Issue: root.Key, Requester: "checker", To: phase.Implementing,
				Reason: "the build is broken", PhaseEntry: entry})
			if tc.moves {
				if result.Refusal != nil {
					t.Fatalf("the checker's move back of entry %d, with entry %d standing, was refused: %+v", entry, second.Entry, result.Refusal)
				}
				assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
				return
			}
			if refusalCode(result) != "BACKWARD_REFUSED" || !result.Unspent {
				t.Fatalf("the checker's move back of entry %d, with entry %d standing: refusal %+v, unspent %t; want BACKWARD_REFUSED, unspent",
					entry, second.Entry, result.Refusal, result.Unspent)
			}
			assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(2))
			if after := roleRowOf(t, pool, engine, root.Key, "checker"); after.Entry != second.Entry || after.EntryTask != second.EntryTask {
				t.Fatalf("the refused move left the checker's row on entry %d with the task %q, want entry %d with %q", after.Entry, after.EntryTask, second.Entry, second.EntryTask)
			}
		})
	}
}

// Two claims of one issue can hold the checker's role: key a's agent, `…-implementer`, which the
// checker's phases row names on step 2, and the checker's own claim. A move back is the step's
// worker's decision (intake.BackwardMove.By on a phase worker's move): the checker's own claim
// asking one is refused BACKWARD_REFUSED, unspent, and the task stays at step 2. Key a's agent's
// move, and a move that names no claim (from a route before the claim was carried), send the
// task back to implementing.
func TestOnlyTheStepsWorkerMovesItsIssueBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		by    func(keyed, own claim.Token) claim.Token
		moves bool
	}{
		{"the checker's own claim, which works no step", func(_, own claim.Token) claim.Token { return own }, false},
		{"key a's agent, the step's worker", func(keyed, _ claim.Token) claim.Token { return keyed }, true},
		{"a move that names no claim", func(claim.Token, claim.Token) claim.Token { return "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := deploymentEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "checker")
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "built", "impl-1", 0)
			assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(2))
			keyed, err := claim.AgentToken("legion", root.Key, claim.Agent(claim.RoleImplementer))
			if err != nil {
				t.Fatal(err)
			}
			own, err := claim.AgentToken("legion", root.Key, "checker")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), "update phases set claim = $1 where issue = $2 and role = 'checker'", string(keyed), root.Key); err != nil {
				t.Fatalf("name key a's agent on the checker's step: %v", err)
			}

			by := tc.by(keyed, own)
			result := applyFacts(t, pool, engine)("checker-back", intake.BackwardMove{Issue: root.Key, Requester: "checker", To: phase.Implementing,
				Reason: "the build is broken", By: by})
			if tc.moves {
				if result.Refusal != nil {
					t.Fatalf("the move back by %q was refused: %+v", by, result.Refusal)
				}
				assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
				return
			}
			if refusalCode(result) != "BACKWARD_REFUSED" || !result.Unspent {
				t.Fatalf("the move back by %s: refusal %+v, unspent %t; want BACKWARD_REFUSED, unspent", by, result.Refusal, result.Unspent)
			}
			assertWalksAt(t, pool, engine, root.Key, phase.Step, stepPlace(2))
		})
	}
}

// A move's refusal names the old and current issue-wide entries, not their distance as a count
// of entries missed by the requesting role.
func TestAStaleMoveBackReportsItsTwoEntryIdentities(t *testing.T) {
	refusal := staleBackwardMove("LEGION-208", claim.RoleTester, phase.Testing, 9, 2)
	if refusal.Status != http.StatusConflict || refusal.Code != "BACKWARD_REFUSED" {
		t.Fatalf("stale move refusal = %+v, want 409 BACKWARD_REFUSED", refusal)
	}
	for _, receiver := range []string{"LEGION-208", string(claim.RoleTester), string(phase.Testing)} {
		if !strings.Contains(refusal.Message, receiver) {
			t.Fatalf("stale move refusal names no receiver %q: %s", receiver, refusal.Message)
		}
	}
	reason, _, _ := strings.Cut(refusal.Message, ". ")
	numbers := make(map[string]int)
	for _, number := range regexp.MustCompile(`[0-9]+`).FindAllString(reason, -1) {
		numbers[number]++
	}
	if want := (map[string]int{"208": 1, "2": 1, "9": 1}); !reflect.DeepEqual(numbers, want) {
		t.Fatalf("stale move refusal reports numeric values %v, want only receiver and entry identities %v: %s", numbers, want, reason)
	}
}
