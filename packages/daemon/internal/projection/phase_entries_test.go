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

	got, err := Project(context.Background(), nil, store, project, nil, claims, nil, projectedAt)
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

// A phase no role works carries neither entry number, however far behind the claim of its last
// phase stands: no row of it is read, the sweep passes it over and a difference written for it
// would stand for ever. Four phases are in that case — awaiting_person, awaiting_merge, done and
// held — and admitted is in it no longer, since the architect is the role that works a root's own
// phase (phase.RoleFor).
func TestTheArchitectHandshakeIsAbsentForAPhaseNoRoleWorks(t *testing.T) {
	const project = "LEGION"
	issue := func(key string, p phase.Phase, status string) record.Issue {
		return record.Issue{Key: key, Project: project, Tree: key, Phase: p, Status: status,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: key}
	}
	row := func(key string, role claim.Role) []record.PhaseRow {
		return []record.PhaseRow{{Issue: key, Role: role, Claim: claim.Token(key + "-" + string(role)),
			Entry: 2, EntryTask: "Carry the work of " + key + " on.", EntryRaisedAt: projectedAt}}
	}
	held := func(key string, role claim.Role) supervise.Claim {
		return supervise.Claim{Token: claim.Token(key + "-" + string(role)), Issue: key, Role: role,
			State: supervise.StateReady, Session: "ses_" + key, GivenEntry: 1,
			Pending: &supervise.Delivery{ID: "d-" + key, Entry: 1, QueuedAt: projectedAt}}
	}
	store := projectionStore{
		issues: []record.Issue{
			issue("LEGION-70", phase.AwaitingPerson, "in_progress"),
			issue("LEGION-80", phase.AwaitingMerge, "in_progress"),
			issue("LEGION-90", phase.Done, "done"),
			issue("LEGION-100", phase.Held, "in_progress"),
		},
		phases: map[string][]record.PhaseRow{
			"LEGION-70":  row("LEGION-70", claim.RoleArchitect),
			"LEGION-80":  row("LEGION-80", claim.RoleMerger),
			"LEGION-90":  row("LEGION-90", claim.RoleImplementer),
			"LEGION-100": row("LEGION-100", claim.RoleImplementer),
		},
	}
	got, err := Project(context.Background(), nil, store, project, nil, []supervise.Claim{
		held("LEGION-70", claim.RoleArchitect),
		held("LEGION-80", claim.RoleMerger),
		held("LEGION-90", claim.RoleImplementer),
		held("LEGION-100", claim.RoleImplementer),
	}, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	for _, key := range []string{"LEGION-70", "LEGION-80", "LEGION-90", "LEGION-100"} {
		view, known := got.Issues[key]
		if !known {
			t.Fatalf("%s is in no issue of the projected state; this test proves nothing", key)
		}
		if view.PhaseEntry != 0 || view.GivenEntry != 0 {
			t.Errorf("%s at %s is projected with entry %d given %d, want neither: phase.RoleFor names no role for that phase",
				key, view.Phase, view.PhaseEntry, view.GivenEntry)
		}
	}
}
