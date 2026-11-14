package projection

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Each issue carries the entry the role of its current phase stands on and the entry that role
// was given, in every shape the claim of that role can be in: a live claim level with its entry,
// a claim the daemon suspended behind it, a retired claim behind it, and a role the daemon holds
// no claim row of at all, which was given nothing. The given number is the claim's and not the
// phases row's: LEGION-30's row names a token of an earlier run, and the number served is the
// one the claim of that issue and role holds, which is what the admission pass weighs
// (admit's resumeSlottedWorkers).
//
// The row of a role the issue has moved past is read by nothing: LEGION-20's planner stands on
// an entry of its own and the issue's view carries its implementer's.
func TestProjectShowsThePhaseEntryAndTheEntryTheRoleWasGiven(t *testing.T) {
	const project = "LEGION"
	issue := func(key string, p phase.Phase, status string) record.Issue {
		return record.Issue{Key: key, Project: project, Tree: key, Phase: p, Status: status,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: key}
	}
	store := projectionStore{
		issues: []record.Issue{
			issue("LEGION-20", phase.Implementing, "in_progress"),
			issue("LEGION-30", phase.Testing, "testing"),
			issue("LEGION-40", phase.Reviewing, "needs_review"),
			issue("LEGION-50", phase.Planning, "in_progress"),
			issue("LEGION-60", phase.Held, "in_progress"),
		},
		phases: map[string][]record.PhaseRow{
			"LEGION-20": {
				{Issue: "LEGION-20", Role: claim.RolePlanner, Claim: "legion-20-planner", Entry: 1},
				{Issue: "LEGION-20", Role: claim.RoleImplementer, Claim: "legion-20-implementer", Entry: 4},
			},
			// The token of a claim an earlier run retired, which this tester's own claim has
			// replaced: the given number below is read from the claim and not from this token.
			"LEGION-30": {{Issue: "LEGION-30", Role: claim.RoleTester, Claim: "legion-30-tester-gone", Entry: 3}},
			"LEGION-40": {{Issue: "LEGION-40", Role: claim.RoleReviewer, Claim: "legion-40-reviewer", Entry: 2}},
			"LEGION-50": {{Issue: "LEGION-50", Role: claim.RolePlanner, Entry: 1}},
			"LEGION-60": {{Issue: "LEGION-60", Role: claim.RoleImplementer, Claim: "legion-60-implementer", Entry: 5}},
		},
	}
	claims := []supervise.Claim{
		{Token: "legion-20-planner", Issue: "LEGION-20", Role: claim.RolePlanner, State: supervise.StateIdle,
			Session: "ses_planner_20", GivenEntry: 1},
		{Token: "legion-20-implementer", Issue: "LEGION-20", Role: claim.RoleImplementer, State: supervise.StateWorking,
			Session: "ses_implementer_20", GivenEntry: 4},
		{Token: "legion-30-tester", Issue: "LEGION-30", Role: claim.RoleTester, State: supervise.StateSuspended,
			Session: "ses_tester_30", GivenEntry: 2},
		{Token: "legion-40-reviewer", Issue: "LEGION-40", Role: claim.RoleReviewer, State: supervise.StateRetired,
			Session: "ses_reviewer_40", GivenEntry: 1},
		{Token: "legion-60-implementer", Issue: "LEGION-60", Role: claim.RoleImplementer, State: supervise.StateSuspended,
			Session: "ses_implementer_60", GivenEntry: 5},
	}

	got, err := Project(context.Background(), Inputs{Store: store, Project: project, Claims: claims, Now: projectedAt})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	for key, want := range map[string]api.Issue{
		// A live claim holding the work its row names, and a claim the daemon suspended behind
		// its entry, which the admission pass sends that entry's work to.
		"LEGION-20": {PhaseEntry: 4, GivenEntry: 4},
		"LEGION-30": {PhaseEntry: 3, GivenEntry: 2},
		// A retired claim keeps what it was given, and a role with no claim row at all was given
		// nothing, which is the zero the wire leaves out.
		"LEGION-40": {PhaseEntry: 2, GivenEntry: 1},
		"LEGION-50": {PhaseEntry: 1, GivenEntry: 0},
		// Held names no phase worker role (phase.RoleFor), so neither number is written,
		// however far behind the claim of its last phase stands.
		"LEGION-60": {PhaseEntry: 0, GivenEntry: 0},
	} {
		view, known := got.Issues[key]
		if !known {
			t.Fatalf("%s is in no issue of the projected state; this test proves nothing", key)
		}
		if view.PhaseEntry != want.PhaseEntry || view.GivenEntry != want.GivenEntry {
			t.Errorf("%s is projected with entry %d given %d, want entry %d given %d",
				key, view.PhaseEntry, view.GivenEntry, want.PhaseEntry, want.GivenEntry)
		}
	}
}

// A root at admitted whose design gate has never opened is at work and its architect is writing
// the design, but no phase worker role works that phase: the admission pass passes it over, so a
// difference written for it would stand for ever and the projection writes neither number. The
// architect's claim is live beside it, which is what makes the rule the phase's rather than the
// claim's.
func TestProjectWritesNoPhaseEntryForAPhaseNoWorkerRoleWorks(t *testing.T) {
	const project = "LEGION"
	store := projectionStore{
		issues: []record.Issue{{Key: "LEGION-70", Project: project, Tree: "LEGION-70", Phase: phase.Admitted,
			Status: "in_progress", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: "A"}},
		phases: map[string][]record.PhaseRow{
			"LEGION-70": {{Issue: "LEGION-70", Role: claim.RoleArchitect, Claim: "legion-70-architect", Entry: 2}},
		},
	}
	got, err := Project(context.Background(), Inputs{Store: store, Project: project, Claims: []supervise.Claim{
		{Token: "legion-70-architect", Issue: "LEGION-70", Role: claim.RoleArchitect, State: supervise.StateReady,
			Session: "ses_architect_70", GivenEntry: 1},
	}, Now: projectedAt})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if view := got.Issues["LEGION-70"]; view.PhaseEntry != 0 || view.GivenEntry != 0 {
		t.Fatalf("the admitted root is projected with entry %d given %d, want neither: phase.RoleFor names no phase worker role for admitted",
			view.PhaseEntry, view.GivenEntry)
	}
}
