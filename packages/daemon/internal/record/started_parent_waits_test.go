package record

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The started parents are the issues whose phases have started: each phase from planning to
// accepting, a hold from one of them, a start the issue owes its worker, and awaiting_children
// itself. An issue past its acceptance, one that never started, one waiting on a person and one
// out of the workflow are none of them, and no design gate is read: a started parent's next start
// is held by an open child whatever its tree's gate says.
func TestStartedParentsAreTheIssuesWhosePhasesStarted(t *testing.T) {
	at := func(key string, p phase.Phase) Issue {
		return Issue{Key: key, Tree: key, Project: "LEGION", Phase: p, Status: "in_progress"}
	}
	held := func(key string, from phase.Phase) Issue {
		issue := at(key, phase.Held)
		issue.Hold = &Hold{From: from}
		return issue
	}
	onBacklog := at("LEGION-20", phase.Implementing)
	onBacklog.Status = "backlog"
	issues := []Issue{
		at("LEGION-1", phase.Planning), at("LEGION-2", phase.Implementing), at("LEGION-3", phase.Testing),
		at("LEGION-4", phase.Reviewing), at("LEGION-5", phase.Accepting), at("LEGION-6", phase.AwaitingChildren),
		held("LEGION-7", phase.Implementing), held("LEGION-8", phase.Accepting),
		at("LEGION-9", phase.Admitted), at("LEGION-10", phase.AwaitingPerson), at("LEGION-11", phase.Retro),
		at("LEGION-12", phase.Merging), held("LEGION-13", phase.Retro), at("LEGION-14", phase.Done), onBacklog,
		at("LEGION-15", phase.Admitted),
	}
	owed := map[string]OwedStart{"LEGION-15": {Issue: "LEGION-15", Phase: phase.Planning}}

	got := MainRule.StartedParents(issues, owed)
	want := []string{"LEGION-1", "LEGION-15", "LEGION-2", "LEGION-3", "LEGION-4", "LEGION-5", "LEGION-6", "LEGION-7", "LEGION-8"}
	if !slices.Equal(got, want) {
		t.Fatalf("StartedParents = %v, want %v", got, want)
	}
	// Under a rule whose wait stops no phase under way (SlotRule.WaitStopsPhase off), the rules the
	// recorded windows ran before cybermonklabs/legion#4592, an open child holds no started
	// parent, so none is read.
	for _, name := range []string{"legion-v20.42.5", "legion-v19.57.3", "legion-v22.14.1"} {
		rule, kept := SlotRuleNamed(name)
		if !kept || rule.WaitStopsPhase {
			t.Fatalf("the rule %s = %+v (kept %t), want one whose wait stops no phase", name, rule, kept)
		}
		if parents := rule.StartedParents(issues, owed); len(parents) != 0 {
			t.Errorf("StartedParents under %s = %v, want none: its wait stops no phase", name, parents)
		}
	}
	// The children-first candidates are a judgement of their own: under every tree's open gate
	// they are the issues at admitted, LEGION-9 among them, which never started and so is no
	// started parent, and LEGION-15, whose owed start makes it one; no issue in a started phase is
	// among them.
	open := TreeGate{Rows: map[DesignStep]DesignGate{DesignStepSpec: {LatestVersion: 1, ApprovedVersion: new(1)}}}
	gates := make(map[string]TreeGate, len(issues))
	for _, issue := range issues {
		gates[issue.Tree] = open
	}
	if candidates := MainRule.ChildrenFirstCandidates(issues, gates); !slices.Equal(candidates, []string{"LEGION-15", "LEGION-9"}) {
		t.Fatalf("ChildrenFirstCandidates under open gates = %v, want LEGION-15 and LEGION-9, the issues at admitted", candidates)
	}
}

// The open children of a started parent are judged from the mirror and the daemon's records as
// the engine judges them (workflow's childrenOpen): a child filed and never released is open, a
// done one is not, and a child of the parent's own tree that the daemon holds at admitted under an
// accepted ancestor is left out, since that child starts nothing before the ancestor closes. A
// started parent with no open child has no entry, and the line's facts carry the judgement
// (ReadLineFacts) beside a children-first wait that reads none of it.
func TestStartedParentWaitsReadsTheOpenChildrenOfAStartedParent(t *testing.T) {
	const (
		root       = "LEGION-700" // merging: accepted
		parent     = "LEGION-701" // a member in accepting, its own children open
		heldChild  = "LEGION-702" // a member at admitted under the accepted root
		filed      = "LEGION-703" // filed under the parent, never released
		finished   = "LEGION-704" // done
		quiet      = "LEGION-710" // a started root whose only child is done
		quietChild = "LEGION-711"
	)
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	member := func(key, parentKey string, p phase.Phase, status string) Issue {
		issue := issueFixture(key)
		issue.Tree, issue.Parent, issue.Phase, issue.Status = root, ParentOf(parentKey), p, status
		return issue
	}
	rootIssue := issueFixture(root)
	rootIssue.Parent, rootIssue.Phase, rootIssue.Status = nil, phase.Merging, "retro"
	quietRoot := issueFixture(quiet)
	quietRoot.Parent = nil
	issues := []Issue{rootIssue, member(parent, root, phase.Accepting, "needs_review"),
		member(heldChild, parent, phase.Admitted, "todo"), quietRoot}

	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range issues {
			must(t, records.PutIssue(ctx, tx, issue))
		}
		// The root's gate stands open, so the children-first judgement below reads its tree.
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: root, Step: DesignStepSpec, ArtifactID: "artifact-700", LatestVersion: 1, ApprovedVersion: new(1)}))
		for _, reading := range []DispatchReading{
			{Key: heldChild, Project: "LEGION", Seq: 1, Status: Carry("todo"), Parent: Carry(parent), Blockers: Carry[[]string](nil)},
			{Key: filed, Project: "LEGION", Seq: 1, Status: Carry("backlog"), Parent: Carry(parent), Blockers: Carry[[]string](nil)},
			{Key: finished, Project: "LEGION", Seq: 1, Status: Carry("done"), Parent: Carry(parent), Blockers: Carry[[]string](nil)},
			{Key: quietChild, Project: "LEGION", Seq: 1, Status: Carry("done"), Parent: Carry(quiet), Blockers: Carry[[]string](nil)},
		} {
			must(t, records.PutDispatchIssue(ctx, tx, reading))
		}

		facts, err := ReadLineFacts(ctx, tx, records, "LEGION", issues, nil)
		must(t, err)
		want := map[string]ChildWait{parent: {Parent: parent, Open: []OpenChild{{Key: filed, Status: "backlog"}}}}
		if !reflect.DeepEqual(facts.StartedParentWaits, want) {
			t.Fatalf("the line's facts carry %+v, want %+v: %s is held under the accepted %s and %s is done",
				facts.StartedParentWaits, want, heldChild, root, finished)
		}
		if !facts.Gates[root].Open() {
			t.Fatalf("the gate of %s reads closed in the line's facts, want it open", root)
		}
		if childWaits := ChildWaitsOf(facts); len(childWaits) != 0 {
			t.Fatalf("the children-first waits under the open gate = %+v, want none: %s's open child is the started parent's to read, and %s, at admitted, has none",
				childWaits, parent, heldChild)
		}
	})
}

// The held word of a parent in awaiting_children names its open children, as the started
// parents' judgement reads them, and a parent whose last open child has gone stands with no
// issue beside it until the pass takes it back.
func TestTheAwaitingChildrenWordNamesTheStartedParentsOpenChildren(t *testing.T) {
	waiting := Issue{Key: "LEGION-801", Tree: "LEGION-800", Project: "LEGION", Phase: phase.AwaitingChildren,
		Status: "in_progress", Parent: ParentOf("LEGION-800")}
	root := Issue{Key: "LEGION-800", Tree: "LEGION-800", Project: "LEGION", Phase: phase.Admitted, Status: "in_progress", HandedOver: true}
	byKey := map[string]Issue{root.Key: root, waiting.Key: waiting}
	open := map[string]ChildWait{waiting.Key: {Parent: waiting.Key, Open: []OpenChild{
		{Key: "LEGION-802", Status: "todo"}, {Key: "LEGION-803", Status: "backlog"}}}}

	word, by := HeldReasonOf(waiting, HeldFacts{ByKey: byKey, Chain: byKey, Facts: LineFacts{StartedParentWaits: open}})
	if word != HeldAwaitingChildren || !slices.Equal(by, []string{"LEGION-802", "LEGION-803"}) {
		t.Fatalf("the held word = %s held by %v, want awaiting_children held by both open children", word, by)
	}
	word, by = HeldReasonOf(waiting, HeldFacts{ByKey: byKey, Chain: byKey, Facts: LineFacts{ChildWaits: open}})
	if word != HeldAwaitingChildren || len(by) != 0 {
		t.Fatalf("the held word over the children-first waits alone = %s held by %v, want awaiting_children held by nobody", word, by)
	}
}
