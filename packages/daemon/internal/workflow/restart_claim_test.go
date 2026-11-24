package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A restart of a phase asked for one claim (api's restartWorkedPhase) names that claim, and the
// workflow holds it to the claim the record names on the role's step when the fact applies: the
// start the restart queues hands the step's entry to that claim, so a step entered for another
// claim since the caller read it is refused with nothing changed — the issue stays where it stood
// and no start is queued. That holds for the operator's restart of an issue standing in its phase
// (intake.RestartPhase) and for the retry of one held from it (intake.RetryOrEscalate). The control
// of each is the same fact naming the claim the step is: the phase is restarted. A retry naming no
// claim, the architect's own, is never held to one. An issue held from admitted, where the
// architect works, for a stopped task, is refused as its plan's hold before any claim is read.
func TestARestartNamingAClaimRestartsNoStepAnotherClaimWorks(t *testing.T) {
	keyed := claim.Token(token(t, entryIssue, claim.RoleImplementer))
	other := claim.Token(token(t, entryIssue, claim.RoleReviewer))
	for _, tc := range []struct {
		name  string
		held  bool
		fact  intake.Fact
		moved bool
	}{
		{name: "a restart of the claim on the step", fact: intake.RestartPhase{Issue: entryIssue, Role: claim.RoleTester, Claim: keyed}},
		{name: "a restart of a claim the step is no longer", fact: intake.RestartPhase{Issue: entryIssue, Role: claim.RoleTester, Claim: other}, moved: true},
		{name: "a retry of the held phase for the claim on the step", held: true,
			fact: intake.RetryOrEscalate{Issue: entryIssue, Decision: intake.RetryDecision, Claim: keyed}},
		{name: "a retry of the held phase for a claim the step is no longer", held: true,
			fact: intake.RetryOrEscalate{Issue: entryIssue, Decision: intake.RetryDecision, Claim: other}, moved: true},
		{name: "the architect's retry, which names no claim", held: true,
			fact: intake.RetryOrEscalate{Issue: entryIssue, Decision: intake.RetryDecision}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			issue := record.Issue{Key: entryIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: entryIssue,
				Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true}
			if tc.held {
				issue.Phase, issue.Hold = phase.Held, &record.Hold{From: phase.Testing}
			}
			seedIssue(t, pool, issue)
			seedTreeLifecycle(t, pool, entryIssue)
			seedPhase(t, pool, record.PhaseRow{Issue: entryIssue, Role: claim.RoleTester, Claim: keyed, StepTask: "a", StepPlace: 2, Entry: seededEntry})
			engine := testEngine(config.DesignGateRootIssues, nil)

			result := applyFacts(t, pool, engine)("restart", tc.fact)
			starts := superviseRowsOf(t, pool, entryIssue, "start")
			if !tc.moved {
				if result.Refusal != nil || len(starts) != 1 || starts[0].Role != claim.RoleTester {
					t.Fatalf("the restart = %+v queuing %+v, want it applied with the tester's one start", result.Refusal, starts)
				}
				assertPhase(t, pool, phase.Testing)
				return
			}
			if result.Refusal == nil || result.Refusal.Code != "PHASE_NOT_RESTARTED" {
				t.Fatalf("the restart for %s = %+v, want PHASE_NOT_RESTARTED: the step is %s's", other, result.Refusal, keyed)
			}
			if len(starts) != 0 {
				t.Fatalf("the refused restart queued %+v, want nothing", starts)
			}
			if tc.held {
				assertPhase(t, pool, phase.Held)
			} else {
				assertPhase(t, pool, phase.Testing)
			}
		})
	}

	t.Run("a retry naming a claim of an issue held from admitted for a stopped task", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
		if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}
		result := applyFacts(t, pool, engine)("retry", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Claim: keyed})
		if result.Refusal == nil || result.Refusal.Code == "PHASE_NOT_RESTARTED" {
			t.Fatalf("the retry = %+v, want the plan hold's own refusal, before any claim is read", result.Refusal)
		}
		if starts := superviseRowsOf(t, pool, root.Key, "start"); len(starts) != 0 {
			t.Fatalf("the refused retry queued %+v, want nothing: no step runs at admitted", starts)
		}
		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Held {
			t.Fatalf("%s stands in %s, want it still held for its stopped task", root.Key, issue.Phase)
		}
	})
}

// token is the claim token of issue's role in the project these tests use, as a string. This
// branch's tests name a keyed agent's claim by it.
func token(t *testing.T, issue string, role claim.Role) string {
	t.Helper()
	got, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatalf("token of %s %s: %v", issue, role, err)
	}
	return string(got)
}
