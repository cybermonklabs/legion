package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A completion of a phase its role has left is recorded on the role's row, and moves nothing,
// where the daemon started the role and the role's run has reported nothing (leftItsPhase): on an
// issue with no plan rows, the tester's findings after the daemon moved the issue on without it.
// The route judged its entry on a read before the fact's lock; the workflow judges it again on the
// row its own transaction reads, with the route's one rule (intake.EntryFence). Here, on today's
// line, the tester's turn took the task of entry `first`; a move back the tester did not make sent
// the work back, the implementer's completion handed testing to the tester again on entry
// `second`, and a second such move sent the work back before that run reported. The tester's
// completion of entry `first` is refused HANDOFF_NOT_CURRENT_PHASE, unspent, and the row keeps what
// it held: the route answers the same for a role that does not work the issue's phase, behind its
// row's entry. The controls, the completion of entry `second` and one of no entry, are recorded on
// the row.
func TestACompletionOfALeftPhaseOfAnEntryRaisedSinceIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		entry    func(first, second int64) int64
		recorded bool
	}{
		{"the tester's completion of the entry the first move back left", func(first, _ int64) int64 { return first }, false},
		{"the control: the tester's completion of the entry its row stands on", func(_, second int64) int64 { return second }, true},
		{"the control: a completion of no entry", func(int64, int64) int64 { return 0 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			root := stepRoot(t, pool, phase.Implementing)
			tester := seedPhaseWorker(t, pool, root.Key, claim.RoleTester, "")
			stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "built", "impl-1", 0)
			assertPhase(t, pool, phase.Testing)
			first := roleRowOf(t, pool, engine, root.Key, claim.RoleTester).Entry

			move(t, pool, engine, root.Key, TriggerBackward, stepPlace(2), record.PhaseRow{})
			assertPhase(t, pool, phase.Implementing)
			stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "rebased", "impl-2", 0)
			assertPhase(t, pool, phase.Testing)
			move(t, pool, engine, root.Key, TriggerBackward, stepPlace(2), record.PhaseRow{})
			assertPhase(t, pool, phase.Implementing)
			second := roleRowOf(t, pool, engine, root.Key, claim.RoleTester)
			if second.Entry <= first || second.Completed() {
				t.Fatalf("the tester's row = %+v after the second entry, want an unreported run above entry %d", second, first)
			}

			entry := tc.entry(first, second.Entry)
			result := applyFacts(t, pool, engine)("tester-late", intake.HandoffComplete{Generation: root.Generation, Issue: root.Key, Role: claim.RoleTester,
				Claim: tester, Summary: "tested", Commit: stepHead, Verdict: "pass", Entry: entry, HandoffRecord: "test-late", HandoffVersion: 1})
			after := roleRowOf(t, pool, engine, root.Key, claim.RoleTester)
			assertPhase(t, pool, phase.Implementing)
			if tc.recorded {
				if result.Refusal != nil || after.HandoffRecord != "test-late" {
					t.Fatalf("the tester's completion of entry %d, its row on entry %d: refusal %+v, row %+v; want it recorded on the row",
						entry, second.Entry, result.Refusal, after)
				}
				return
			}
			if refusalCode(result) != "HANDOFF_NOT_CURRENT_PHASE" || !result.Unspent {
				t.Fatalf("the tester's completion of entry %d, its row on entry %d: refusal %+v, unspent %t; want HANDOFF_NOT_CURRENT_PHASE, unspent",
					entry, second.Entry, result.Refusal, result.Unspent)
			}
			if after.HandoffRecord != second.HandoffRecord || after.Verdict != second.Verdict || !after.CompletedAt.Equal(second.CompletedAt) {
				t.Fatalf("the refused completion wrote the tester's row: %+v, want it as it stood: %+v", after, second)
			}
		})
	}
}

// The completion route judges the entry a completion is of on its own read, before the fact's
// lock (api's answerCompletion); the workflow judges it again on the role's phases row its own
// transaction reads, with the route's one rule (intake.EntryFence), so a completion of an entry
// raised in between moves nothing. Here the route read the role's row on entry `first` and passed
// the completion; then a move back the role did not make sent the work back to implementing while
// that turn still ran, and the implementer's completion entered the role's step again on entry
// `second`. The completion carries entry `first`, as the route builds it.
//
// The checker, a role of the deployment, runs as step, which ends with no handoff record: its
// completion of entry `first` is refused HANDOFF_STALE_ENTRY whatever commit it names, records
// nothing on the row, and leaves the fact unspent; the issue stays at step 2. The tester's testing
// ends with a record: its behind completion is refused at a commit the pull request has left, and
// taken at the head the pull request is at, as the route takes it. Each control is the completion
// of the entry that stands, which moves the issue on.
func TestACompletionOfAnEntryRaisedAfterTheRoutesReadMovesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		role   claim.Role
		record string
		stale  bool
		commit string
		// refused is the refusal's code, "" for a completion that is taken.
		refused string
		// then is the phase the issue stands in after the completion.
		then phase.Phase
	}{
		{"the checker's pass of the entry the move back left", "checker", "", true, stepHead, "HANDOFF_STALE_ENTRY", phase.Step},
		{"the control: the checker's pass of the entry that stands", "checker", "", false, stepHead, "", phase.Retro},
		{"the tester's pass of the entry the move back left, at a commit the pull request left", claim.RoleTester, "test-2", true, stepHead, "HANDOFF_STALE_ENTRY", phase.Testing},
		{"the tester's pass of the entry the move back left, at the pull request's head", claim.RoleTester, "test-2", true, "head", "", phase.Retro},
		{"the control: the tester's pass of the entry that stands", claim.RoleTester, "test-2", false, stepHead, "", phase.Retro},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := deploymentEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", string(tc.role))
			at, version := phase.Testing, 1
			if tc.role != claim.RoleTester {
				at, version = phase.Step, 0
				if _, err := pool.Exec(t.Context(), "update plan_steps set type = 'testing' where issue = $1 and task = 'a' and position = 2", root.Key); err != nil {
					t.Fatalf("store the type of %s: %v", tc.role, err)
				}
			}
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "built", "impl-1", 0)
			assertWalksAt(t, pool, engine, root.Key, at, stepPlace(2))
			first := roleRowOf(t, pool, engine, root.Key, tc.role).Entry

			// A move back the role did not make, while its turn of entry `first` still runs; then
			// the implementer's fix enters step 2 again.
			move(t, pool, engine, root.Key, TriggerBackward, stepPlace(1), record.PhaseRow{})
			assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
			stepReports(t, pool, engine, root.Key, claim.RoleImplementer, "", "rebased", "impl-2", 0)
			assertWalksAt(t, pool, engine, root.Key, at, stepPlace(2))
			second := roleRowOf(t, pool, engine, root.Key, tc.role)
			if second.Entry <= first {
				t.Fatalf("the %s stands on entry %d after the re-entry, want one above %d", tc.role, second.Entry, first)
			}

			entry := second.Entry
			if tc.stale {
				entry = first
			}
			result := applyFacts(t, pool, engine)(string(tc.role)+"-pass", intake.HandoffComplete{Generation: root.Generation, Issue: root.Key, Role: tc.role,
				Claim: second.Claim, Summary: "checked", Commit: tc.commit, Verdict: "pass", Entry: entry,
				HandoffRecord: tc.record, HandoffVersion: version})
			got := issueOf(t, pool, engine, root.Key)
			if refusalCode(result) != tc.refused || got.Phase != tc.then {
				t.Fatalf("the %s's pass of entry %d at %s, with entry %d standing at step 2: refusal %+v, %s now stands in %s; want refusal %q and %s",
					tc.role, entry, tc.commit, second.Entry, result.Refusal, root.Key, got.Phase, tc.refused, tc.then)
			}
			if tc.refused == "" {
				return
			}
			if !result.Unspent {
				t.Fatalf("the refused completion of entry %d left its fact spent, want it unspent so the same report can apply once it is the work of the entry that stands", entry)
			}
			if after := roleRowOf(t, pool, engine, root.Key, tc.role); after.Verdict != second.Verdict || !after.CompletedAt.Equal(second.CompletedAt) ||
				after.HandoffRecord != second.HandoffRecord || after.Entry != second.Entry {
				t.Fatalf("the refused completion wrote the %s's row: %+v, want it as it stood: %+v", tc.role, after, second)
			}
		})
	}
}
