package projection

import (
	"context"
	"testing"
	"time"

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

	got, err := Project(context.Background(), nil, store, project, nil, claims, nil, nil, projectedAt)
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

// A root carries the two numbers as a child does: phase.RoleFor answers the architect for
// admitted, the phase a root's own architect works, so the entry its architect's row stands on
// and the entry its claim was queued are the root's. Beside them the view carries the handshake:
// the time the task of the last entry the claim was queued was first queued and the time a turn
// of its worker first confirmed it (api.Issue.EntryQueuedAt and EntryConfirmedAt), read from the
// claim's own record of it (supervise.Claim.Handshake) and not from the task it holds, so the two
// times outlast the turn that confirmed the task.
//
// A root whose claim has recorded no handshake carries neither time, which is what a root of the
// release before this one reads as: its migration gives the claim the entry and no handshake. A
// task of no entry the claim holds — a wake, a resume, an operator's own — is no part of the
// handshake and hides none of it.
func TestProjectShowsARootsArchitectHandshake(t *testing.T) {
	const project = "LEGION"
	queued := projectedAt.Add(-12 * time.Minute)
	confirmed := projectedAt.Add(-11 * time.Minute)
	root := func(key string) record.Issue {
		return record.Issue{Key: key, Project: project, Tree: key, Phase: phase.Admitted, Generation: 2,
			Status: "in_progress", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: key}
	}
	architectRow := func(key string, entry int64) record.PhaseRow {
		return record.PhaseRow{Issue: key, Role: claim.RoleArchitect, Claim: claim.Token(key + "-architect"),
			Entry: entry, EntryTask: "Claim " + key + " and write its spec.", EntryRaisedAt: queued}
	}
	store := projectionStore{
		issues: []record.Issue{root("LEGION-70"), root("LEGION-80"), root("LEGION-90"), root("LEGION-100")},
		phases: map[string][]record.PhaseRow{
			"LEGION-70":  {architectRow("LEGION-70", 1)},
			"LEGION-80":  {architectRow("LEGION-80", 1)},
			"LEGION-90":  {architectRow("LEGION-90", 2)},
			"LEGION-100": {architectRow("LEGION-100", 1)},
		},
	}
	architect := func(key string, given int64, handshake supervise.EntryHandshake, pending *supervise.Delivery) supervise.Claim {
		return supervise.Claim{Token: claim.Token(key + "-architect"), Issue: key, Tree: key, Role: claim.RoleArchitect,
			State: supervise.StateIdle, Session: "ses_architect_" + key, GivenEntry: given, Handshake: handshake, Pending: pending}
	}
	taken := supervise.EntryHandshake{Entry: 1, Generation: 2, QueuedAt: queued, ConfirmedAt: confirmed}
	claims := []supervise.Claim{
		// The task of entry 1, queued and confirmed by a turn of its worker, which ended and
		// retired the task: the claim holds nothing and its handshake stands.
		architect("LEGION-70", 1, taken, nil),
		// The same task, which no turn has confirmed: the root whose architect took nothing.
		architect("LEGION-80", 1, supervise.EntryHandshake{Entry: 1, Generation: 2, QueuedAt: queued},
			&supervise.Delivery{ID: "d-80", Entry: 1, Generation: 2, QueuedAt: queued}),
		// A wake's task, which carries no entry and leaves the given number and the handshake of
		// entry 1 where they stand, while the row has moved to entry 2.
		architect("LEGION-90", 1, taken, &supervise.Delivery{ID: "d-90", Generation: 2, QueuedAt: confirmed}),
		// A claim whose migration gave it the entry and recorded no handshake, which a root of the
		// release before this one is.
		architect("LEGION-100", 1, supervise.EntryHandshake{}, nil),
	}

	got, err := Project(context.Background(), nil, store, project, nil, claims, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	for key, want := range map[string]api.Issue{
		"LEGION-70":  {PhaseEntry: 1, GivenEntry: 1, EntryQueuedAt: &queued, EntryConfirmedAt: &confirmed},
		"LEGION-80":  {PhaseEntry: 1, GivenEntry: 1, EntryQueuedAt: &queued},
		"LEGION-90":  {PhaseEntry: 2, GivenEntry: 1, EntryQueuedAt: &queued, EntryConfirmedAt: &confirmed},
		"LEGION-100": {PhaseEntry: 1, GivenEntry: 1},
	} {
		view, known := got.Issues[key]
		if !known {
			t.Fatalf("%s is in no issue of the projected state; this test proves nothing", key)
		}
		if view.PhaseEntry != want.PhaseEntry || view.GivenEntry != want.GivenEntry {
			t.Errorf("%s is projected with entry %d given %d, want entry %d given %d: the architect is the role that works admitted",
				key, view.PhaseEntry, view.GivenEntry, want.PhaseEntry, want.GivenEntry)
		}
		if !sameInstant(view.EntryQueuedAt, want.EntryQueuedAt) || !sameInstant(view.EntryConfirmedAt, want.EntryConfirmedAt) {
			t.Errorf("%s is projected with its entry queued %v and confirmed %v, want %v and %v",
				key, view.EntryQueuedAt, view.EntryConfirmedAt, want.EntryQueuedAt, want.EntryConfirmedAt)
		}
	}
}

// sameInstant is whether two of the view's times are the same moment, nil counting as the absent
// key the wire leaves out.
func sameInstant(got, want *time.Time) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return got.Equal(*want)
}

// A tree's own root carries its architect's handshake of its current run in every phase it stands
// in, and the entry numbers of the role of that phase beside it (dispatch://INFRA-2614, k5 of the
// Verification document at version 10). A real architect registers its design gate in its first
// turn, so its root waits on a person and then stands in a phase a phase role works: the two
// times are still the architect's handshake there, read as the architect's and never as that
// role's, while the two numbers are that role's and are absent in awaiting_person, a phase no role
// works. The architect's handshake of an earlier run reads as absent, which is a re-admitted root
// whose new architect entry is not queued yet, and so does a root whose architect recorded no
// handshake, whatever its phase role recorded.
//
// An issue that is not a tree's root carries the handshake of the role of its current phase, as it
// carries that role's numbers, and in a phase no role works — done and held — neither the numbers
// nor the times, whichever role's row and claim it holds and however far behind that claim
// stands: no row of it is read, the sweep passes it over and a difference written for it would
// stand for ever.
func TestARootCarriesItsArchitectHandshakeOfThisRunInEveryPhase(t *testing.T) {
	const project = "LEGION"
	architectQueued, architectConfirmed := projectedAt.Add(-40*time.Minute), projectedAt.Add(-39*time.Minute)
	plannerQueued, plannerConfirmed := projectedAt.Add(-10*time.Minute), projectedAt.Add(-9*time.Minute)
	implementerQueued, implementerConfirmed := projectedAt.Add(-5*time.Minute), projectedAt.Add(-4*time.Minute)
	issue := func(key, tree string, p phase.Phase, generation uint64) record.Issue {
		return record.Issue{Key: key, Project: project, Tree: tree, Phase: p, Generation: generation, Status: "in_progress",
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: key}
	}
	row := func(key string, role claim.Role, entry int64) record.PhaseRow {
		return record.PhaseRow{Issue: key, Role: role, Claim: claim.Token(key + "-" + string(role)),
			Entry: entry, EntryTask: "Carry the work of " + key + " on.", EntryRaisedAt: projectedAt}
	}
	held := func(key string, role claim.Role, given int64, handshake supervise.EntryHandshake) supervise.Claim {
		return supervise.Claim{Token: claim.Token(key + "-" + string(role)), Issue: key, Role: role,
			State: supervise.StateIdle, Session: "ses_" + key + "_" + string(role), GivenEntry: given, Handshake: handshake}
	}
	architects := func(entry int64, generation uint64) supervise.EntryHandshake {
		return supervise.EntryHandshake{Entry: entry, Generation: generation, QueuedAt: architectQueued, ConfirmedAt: architectConfirmed}
	}
	planners := supervise.EntryHandshake{Entry: 1, Generation: 2, QueuedAt: plannerQueued, ConfirmedAt: plannerConfirmed}
	implementers := supervise.EntryHandshake{Entry: 1, Generation: 2, QueuedAt: implementerQueued, ConfirmedAt: implementerConfirmed}

	store := projectionStore{
		issues: []record.Issue{
			issue("LEGION-70", "LEGION-70", phase.AwaitingPerson, 2),
			issue("LEGION-71", "LEGION-71", phase.Accepting, 2),
			issue("LEGION-72", "LEGION-72", phase.Planning, 2),
			issue("LEGION-73", "LEGION-73", phase.Admitted, 3),
			issue("LEGION-74", "LEGION-74", phase.Planning, 2),
			issue("LEGION-90", "LEGION-72", phase.Done, 2),
			issue("LEGION-100", "LEGION-72", phase.Held, 2),
			issue("LEGION-110", "LEGION-72", phase.Implementing, 2),
		},
		phases: map[string][]record.PhaseRow{
			"LEGION-70":  {row("LEGION-70", claim.RoleArchitect, 1)},
			"LEGION-71":  {row("LEGION-71", claim.RoleArchitect, 2)},
			"LEGION-72":  {row("LEGION-72", claim.RoleArchitect, 1), row("LEGION-72", claim.RolePlanner, 1)},
			"LEGION-73":  {row("LEGION-73", claim.RoleArchitect, 2)},
			"LEGION-74":  {row("LEGION-74", claim.RoleArchitect, 1), row("LEGION-74", claim.RolePlanner, 1)},
			"LEGION-90":  {row("LEGION-90", claim.RoleImplementer, 2)},
			"LEGION-100": {row("LEGION-100", claim.RoleImplementer, 2)},
			"LEGION-110": {row("LEGION-110", claim.RoleImplementer, 1)},
		},
	}
	got, err := Project(context.Background(), nil, store, project, nil, []supervise.Claim{
		// The root waiting on a person after its architect's first turn registered its gate.
		held("LEGION-70", claim.RoleArchitect, 1, architects(1, 2)),
		// The root its architect accepts, whose numbers are the architect's too.
		held("LEGION-71", claim.RoleArchitect, 2, architects(2, 2)),
		// The root its planner works: the planner's numbers and its own handshake, beside the
		// architect's handshake.
		held("LEGION-72", claim.RoleArchitect, 1, architects(1, 2)),
		held("LEGION-72", claim.RolePlanner, 1, planners),
		// The root re-admitted at run 3, whose architect's entry 2 is raised and not queued yet:
		// the handshake it holds is of run 2.
		held("LEGION-73", claim.RoleArchitect, 1, architects(1, 2)),
		// The root whose architect claim recorded no handshake, the release before this one's,
		// while its planner recorded one.
		held("LEGION-74", claim.RoleArchitect, 1, supervise.EntryHandshake{}),
		held("LEGION-74", claim.RolePlanner, 1, planners),
		// The children: two in a phase no role works, each behind its row, and one its
		// implementer works.
		held("LEGION-90", claim.RoleImplementer, 1, implementers),
		held("LEGION-100", claim.RoleImplementer, 1, implementers),
		held("LEGION-110", claim.RoleImplementer, 1, implementers),
	}, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	for key, want := range map[string]api.Issue{
		"LEGION-70":  {EntryQueuedAt: &architectQueued, EntryConfirmedAt: &architectConfirmed},
		"LEGION-71":  {PhaseEntry: 2, GivenEntry: 2, EntryQueuedAt: &architectQueued, EntryConfirmedAt: &architectConfirmed},
		"LEGION-72":  {PhaseEntry: 1, GivenEntry: 1, EntryQueuedAt: &architectQueued, EntryConfirmedAt: &architectConfirmed},
		"LEGION-73":  {PhaseEntry: 2, GivenEntry: 1},
		"LEGION-74":  {PhaseEntry: 1, GivenEntry: 1},
		"LEGION-90":  {},
		"LEGION-100": {},
		"LEGION-110": {PhaseEntry: 1, GivenEntry: 1, EntryQueuedAt: &implementerQueued, EntryConfirmedAt: &implementerConfirmed},
	} {
		view, known := got.Issues[key]
		if !known {
			t.Fatalf("%s is in no issue of the projected state; this test proves nothing", key)
		}
		if view.PhaseEntry != want.PhaseEntry || view.GivenEntry != want.GivenEntry {
			t.Errorf("%s at %s is projected with entry %d given %d, want entry %d given %d: the numbers are the role's that works that phase (phase.RoleFor)",
				key, view.Phase, view.PhaseEntry, view.GivenEntry, want.PhaseEntry, want.GivenEntry)
		}
		if !sameInstant(view.EntryQueuedAt, want.EntryQueuedAt) || !sameInstant(view.EntryConfirmedAt, want.EntryConfirmedAt) {
			t.Errorf("%s at %s at run %d is projected with its entry queued %v and confirmed %v, want %v and %v",
				key, view.Phase, view.Generation, view.EntryQueuedAt, view.EntryConfirmedAt, want.EntryQueuedAt, want.EntryConfirmedAt)
		}
	}
}
