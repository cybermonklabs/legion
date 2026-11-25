package workflow

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A checks-red notice is low for the architect only while a worker holds the phase the issue
// stays in (texts.checksRedUrgency, the architect's ruling the audit's checks-red row records:
// "low while a worker holds the phase the issue stays in, med in accepting"), because then the
// daemon has asked the implementer to merge the base forward and told that worker to hold. An
// issue held from a worker's phase - its worker's claim failed (workflow's claimFailed), so it
// stands at phase.Held with Hold.From naming the phase - has no worker at that phase: the
// architect is the one that acts on it, as the CI settlement's rule reads a held issue
// (ciSettledUrgency: med where no worker holds the phase, held among them). A conflict confirmed
// on such an issue still reaches mergeForward, since StandsIn counts a hold from testing through
// merging as standing there, and the base push and the per-issue read both read its merge state.
//
// So the checks-red that conflict writes is med for the architect. The notice's urgency is read
// here as the daemon's publish reads it (daemon's noticeUrgency): texts.UrgencyOf with the variant
// that the notice's own payload carries (texts.Variant, whose json names are the payload's keys).
func TestAChecksRedOfAnIssueNoWorkerHoldsIsMedForTheArchitect(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, from := range []phase.Phase{phase.Testing, phase.Reviewing, phase.Merging} {
		t.Run("held from "+string(from), func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: from}, Generation: 1,
				Status: "in_progress", Rank: "U"})
			pr := settledGreen("lint")
			pr.State, pr.Issue, pr.Repo, pr.Number, pr.Branch, pr.Base, pr.HeadSHA =
				record.PullRequestOpen, "LEGION-208", "sjawhar/legion", 42, "legion/LEGION-208", "main", conflictHead
			seedPR(t, pool, pr)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Entry: 1})

			confirmsConflict(t, pool, requiresCI)

			if notices := noticeKinds(t, pool, "LEGION-208"); len(notices) != 1 || notices[0] != "checks-red" {
				t.Fatalf("notices %v, want the one checks-red the conflict on the held issue writes", notices)
			}
			red := checksRedNotice(t, pool)
			raw, err := json.Marshal(red)
			if err != nil {
				t.Fatalf("encode the checks-red notice: %v", err)
			}
			var variant texts.Variant
			if err := json.Unmarshal(raw, &variant); err != nil {
				t.Fatalf("read the variant of %s: %v", raw, err)
			}
			got := texts.UrgencyOf(red.Kind, claim.RoleArchitect, variant)
			t.Logf("issue held from %s, phase %s: checks-red %s goes to the architect at %s", from, issuePhase(t, pool), raw, got)
			if got != texts.Med {
				t.Fatalf("the checks-red of an issue held from %s goes to the architect at %s, want med: no worker holds the phase the issue stays in (it is held), so the architect's act cannot wait for a turn a worker's news would bring", from, got)
			}
		})
	}
}
