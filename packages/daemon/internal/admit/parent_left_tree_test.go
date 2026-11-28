package admit

import (
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The keys of these tests: the tree a member stands in, the member, and a second tree.
const (
	plRoot   = "LEGION-1"
	plMember = "LEGION-2"
	plOther  = "LEGION-50"
)

// plMemberRecord is plMember as a member of plRoot's tree, under plRoot, at generation 1 and
// Dispatch sequence 1: at admitted in todo, labelled as labelled says, with shape applied.
func plMemberRecord(labelled bool, shape func(*record.Issue)) record.Issue {
	member := record.Issue{Key: plMember, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: plMember, Tree: plRoot, Parent: record.ParentOf(plRoot), Phase: phase.Admitted, Generation: 1, Status: "todo",
		DispatchStatus: "todo", Rank: "B", HandedOver: labelled, LastDispatchSeq: 1}
	if shape != nil {
		shape(&member)
	}
	return member
}

// plEvent is a Dispatch event of plMember at seq: its status, the parent Dispatch shows ("" for
// none) and whether it carries the legion label.
func plEvent(seq int64, status, parent string, labelled bool) intake.DispatchIssue {
	return intake.DispatchIssue{Key: plMember, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: status,
		Title: plMember, Parent: parent, Rank: "B", HandedOver: labelled}
}

// plStoppedIn says whether the outbox holds a suspend of a role of plMember that names tree: the
// stop of the member's roles in a tree it left (workflow's Engine.StopLeftTree).
func plStoppedIn(t *testing.T, pool *pgxpool.Pool, tree string) bool {
	t.Helper()
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == plMember && request.Op == "suspend" && request.Tree == tree {
			return true
		}
	}
	return false
}

// plAssertOwnRoot asserts plMember is a root of its own tree at generation, with no parent
// recorded, labelled as labelled says, and waiting at admitted: no phase of it started, and no
// child-status notice of it was sent, so nothing of it started under plRoot's design gate. Its
// roles in plRoot's tree were stopped when it left. A labelled root was promoted: it holds a slot,
// its tree is open and its own architect started in that tree. An unlabelled one holds no slot and
// starts nothing, as any root whose label is off.
func plAssertOwnRoot(t *testing.T, pool *pgxpool.Pool, generation uint64, labelled bool) {
	t.Helper()
	got := issue(t, pool, plMember)
	if got.Tree != plMember || got.Parent != nil || got.Generation != generation || got.Phase != phase.Admitted || got.HandedOver != labelled {
		t.Fatalf("%s = %#v, want a root of its own tree with no parent at generation %d, admitted, labelled %v", plMember, got, generation, labelled)
	}
	if starts := roleStarts(t, pool, plMember, claim.RolePlanner); len(starts) != 0 {
		t.Fatalf("planner starts of %s = %#v, want none: its first phase waits at its own design gate", plMember, starts)
	}
	if notices := joinedNotices(t, pool, plMember); len(notices) != 0 {
		t.Fatalf("child-status notices of %s = %q, want none: it is no member of %s", plMember, notices, plRoot)
	}
	if !plStoppedIn(t, pool, plRoot) {
		t.Fatalf("no suspend of %s names the tree of %s, want its roles there stopped", plMember, plRoot)
	}
	architects := architectStartsOf(t, pool, plMember)
	if !labelled {
		if holdsSlot(t, pool, plMember) || len(architects) != 0 {
			t.Fatalf("%s holds a slot %v with architect starts %#v, want neither while its label is off", plMember, holdsSlot(t, pool, plMember), architects)
		}
		return
	}
	if !holdsSlot(t, pool, plMember) || !treeOpen(t, pool, plMember) || len(architects) != 1 || architects[0].Tree != plMember {
		t.Fatalf("%s holds a slot %v, tree open %v, architect starts %#v; want a slot, its own tree open and one start of its own architect in it",
			plMember, holdsSlot(t, pool, plMember), treeOpen(t, pool, plMember), architects)
	}
}

// plOpenOwnGate registers plMember's own design gate and approves it, and asserts that its first
// phase then starts in its own tree at generation: the gate it waited at is its own.
func plOpenOwnGate(t *testing.T, pool *pgxpool.Pool, admission *Admission, generation uint64) {
	t.Helper()
	const artifact = "2b8c4e6a-1d3f-4a5b-9c7e-8f0a1b2c3d4e"
	applyFact(t, pool, admission, "own-gate-registered", intake.GateRegistered{Issue: plMember, ArtifactID: artifact, Version: 1})
	if got := issue(t, pool, plMember); got.Phase == phase.Planning {
		t.Fatalf("%s = %#v, want its first phase to wait for the approval of its own gate", plMember, got)
	}
	applyFact(t, pool, admission, "own-gate-approved", intake.DispatchArtifact{Key: plMember, ArtifactID: artifact, Kind: intake.DispatchArtifactApproved, Version: 1})
	if got := issue(t, pool, plMember); got.Tree != plMember || got.Phase != phase.Planning {
		t.Fatalf("%s after the approval of its own gate = %#v, want planning in its own tree", plMember, got)
	}
	starts := roleStarts(t, pool, plMember, claim.RolePlanner)
	if len(starts) != 1 || starts[0].Tree != plMember || starts[0].Generation != generation {
		t.Fatalf("planner starts of %s = %#v, want one in its own tree at generation %d", plMember, starts, generation)
	}
}

// A member of a running tree whose Dispatch parent link a person removes is a root of its own
// tree from that event on: its tree follows Dispatch. Its old tree's design gate is open and a slot
// is free, so before this rule the member kept the old tree in its record and started its first
// phase there at the free slot, under a design nobody approved for it, and a todo that set it back
// ran it under the old tree again. Now it waits in the line as a root, its promotion starts its
// own architect, and its first phase waits for its own design gate. It does not follow its old
// root out of the workflow either. A member without the legion label becomes a root whose label is
// off, which waits for a person to label it; the label then hands it to the line.
func TestAMemberWhoseParentLinkIsRemovedBecomesARootOfItsOwnTree(t *testing.T) {
	quietLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("a labelled member waiting at admitted", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 2, quietLog)
		seedOpenTree(t, pool, plRoot)
		putIssue(t, pool, plMemberRecord(true, nil))

		apply(t, pool, admission, "parent-removed", plEvent(2, "todo", "", true), admission.engine)
		plAssertOwnRoot(t, pool, 2, true)
		plOpenOwnGate(t, pool, admission, 2)
	})

	t.Run("an unlabelled member, labelled after the parent link is removed", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 2, quietLog)
		seedOpenTree(t, pool, plRoot)
		putIssue(t, pool, plMemberRecord(false, nil))

		apply(t, pool, admission, "parent-removed", plEvent(2, "todo", "", false), admission.engine)
		plAssertOwnRoot(t, pool, 2, false)
		apply(t, pool, admission, "labelled", plEvent(3, "todo", "", true), admission.engine)
		plAssertOwnRoot(t, pool, 2, true)
	})

	t.Run("a member set back to todo from the backlog with no parent", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 2, quietLog)
		seedOpenTree(t, pool, plRoot)
		putIssue(t, pool, plMemberRecord(true, func(member *record.Issue) {
			member.Phase, member.Status, member.DispatchStatus = phase.Done, "backlog", "backlog"
		}))

		apply(t, pool, admission, "todo-with-no-parent", plEvent(2, "todo", "", true), admission.engine)
		plAssertOwnRoot(t, pool, 2, true)
	})

	t.Run("a member whose old root then leaves the workflow", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 2, quietLog)
		seedOpenTree(t, pool, plRoot)
		putIssue(t, pool, plMemberRecord(true, nil))

		apply(t, pool, admission, "parent-removed", plEvent(2, "todo", "", true), admission.engine)
		plAssertOwnRoot(t, pool, 2, true)
		apply(t, pool, admission, "old-root-parked", intake.DispatchIssue{Key: plRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
			Status: "backlog", Title: plRoot, Rank: "A", HandedOver: true}, admission.engine)
		if got := issue(t, pool, plRoot); got.Status != "backlog" {
			t.Fatalf("%s = %#v, want it out of the workflow in backlog", plRoot, got)
		}
		plAssertOwnRoot(t, pool, 2, true)
	})
}

// A member whose Dispatch parent a person moves under another running tree joins that tree at its
// next run, as an unrecorded todo child under that parent is recorded there: its roles in the old
// tree stop, the architect that owns its new parent is told it joined, and its first phase starts
// under the new tree's design gate at a free slot. Before this rule it kept the old tree and
// started there.
func TestAMemberMovedUnderAnotherRunningTreeJoinsThatTree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedOpenTree(t, pool, plRoot)
	seedOpenTree(t, pool, plOther)
	putIssue(t, pool, plMemberRecord(false, nil))

	apply(t, pool, admission, "moved", plEvent(2, "todo", plOther, false), admission.engine)
	got := issue(t, pool, plMember)
	if got.Tree != plOther || got.Parent == nil || *got.Parent != plOther || got.Generation != 2 || got.Phase != phase.Planning {
		t.Fatalf("%s = %#v, want it planning in the tree of %s under %s at generation 2", plMember, got, plOther, plOther)
	}
	starts := roleStarts(t, pool, plMember, claim.RolePlanner)
	if len(starts) != 1 || starts[0].Tree != plOther || starts[0].Generation != 2 {
		t.Fatalf("planner starts of %s = %#v, want one in the tree of %s at generation 2", plMember, starts, plOther)
	}
	if !plStoppedIn(t, pool, plRoot) {
		t.Fatalf("no suspend of %s names the tree of %s, want its roles there stopped", plMember, plRoot)
	}
	if notices := joinedNotices(t, pool, plMember); len(notices) == 0 || notices[0] != plMember+" joined the tree of "+plOther+" from the tree of "+plRoot {
		t.Fatalf("child-status notices of %s = %q, want the first to say it joined the tree of %s from the tree of %s", plMember, notices, plOther, plRoot)
	}
}

// The boot listing reaches a record that kept its old tree before this rule, or while the daemon
// was down, which no event of the issue need ever reach again: a member at admitted in todo whose
// recorded parent is already none, under a running tree and under a tree whose root left the
// workflow. Each is a root of its own after the listing. Under the parked tree the member would
// otherwise stand held with that tree for good.
func TestTheBootListingTakesAMemberWithNoParentOutOfItsTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, *pgxpool.Pool)
		root dispatch.IssueSummary
	}{
		{
			name: "under a running tree",
			seed: func(t *testing.T, pool *pgxpool.Pool) { seedOpenTree(t, pool, plRoot) },
			root: dispatch.IssueSummary{Key: plRoot, ResolvedRepo: "acme/widgets", Title: plRoot, Status: "in_progress", Rank: "A", HandedOver: true},
		},
		{
			name: "under a tree whose root left the workflow",
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				putIssue(t, pool, record.Issue{Key: plRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
					Title: plRoot, Tree: plRoot, Phase: phase.Done, Generation: 1, Status: "backlog", DispatchStatus: "backlog", Rank: "A", HandedOver: true, LastDispatchSeq: 1})
			},
			root: dispatch.IssueSummary{Key: plRoot, ResolvedRepo: "acme/widgets", Title: plRoot, Status: "backlog", Rank: "A", HandedOver: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
			tc.seed(t, pool)
			putIssue(t, pool, plMemberRecord(true, func(member *record.Issue) { member.Parent, member.LastDispatchSeq = nil, 2 }))

			reconcile(t, pool, admission, []dispatch.IssueSummary{tc.root,
				{Key: plMember, ResolvedRepo: "acme/widgets", Title: plMember, Status: "todo", Rank: "B", HandedOver: true, LastSeq: 2}})
			plAssertOwnRoot(t, pool, 2, true)
		})
	}
}

// The controls: the rule moves only a todo member whose Dispatch parent left its tree. A member
// whose parent stays in the tree starts in that tree at the free slot, and a member at work keeps
// its tree when its parent link is removed, since moving it would move its running claims and
// pod: the tree of an issue at work changes by adoption alone.
func TestAMemberWhoseParentStaysOrThatIsAtWorkKeepsItsTree(t *testing.T) {
	quietLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("a member whose parent stays in the tree", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 2, quietLog)
		seedOpenTree(t, pool, plRoot)
		putIssue(t, pool, plMemberRecord(false, nil))

		apply(t, pool, admission, "renamed", plEvent(2, "todo", plRoot, false), admission.engine)
		if got := issue(t, pool, plMember); got.Tree != plRoot || got.Generation != 1 || got.Phase != phase.Planning {
			t.Fatalf("%s = %#v, want it planning in the tree of %s at generation 1", plMember, got, plRoot)
		}
		if plStoppedIn(t, pool, plRoot) {
			t.Fatalf("a suspend of %s names the tree of %s, want none: it never left", plMember, plRoot)
		}
	})

	t.Run("a member at work whose parent link is removed", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 2, quietLog)
		seedOpenTree(t, pool, plRoot)
		member := plMemberRecord(false, func(member *record.Issue) {
			member.Phase, member.Status, member.DispatchStatus = phase.Planning, "in_progress", "in_progress"
		})
		putIssue(t, pool, member)

		apply(t, pool, admission, "parent-removed", plEvent(2, "in_progress", "", false), admission.engine)
		got := issue(t, pool, plMember)
		if got.Tree != plRoot || got.Generation != 1 || got.Phase != phase.Planning || got.Parent != nil {
			t.Fatalf("%s = %#v, want it planning in the tree of %s at generation 1 with no parent recorded", plMember, got, plRoot)
		}
		if plStoppedIn(t, pool, plRoot) {
			t.Fatalf("a suspend of %s names the tree of %s, want none: a member at work keeps its tree", plMember, plRoot)
		}
	})
}
