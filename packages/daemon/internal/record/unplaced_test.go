package record

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// openTree is the lifecycle of a tree the workflow has opened and whose cleanup has not started,
// which is what makes a recorded root's tree live (treeLive).
var openTree = treelifecycle.Lifecycle{Authority: treelifecycle.AuthorityWorkflow}

// approvedAt is a moment the fixtures below hang a gate approval and a person's answer on; the
// rules read here care only that each is set.
var approvedAt = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// gateAt is one tree's design gate at a version a person has approved up to: open where approved
// and latest agree, shut and once-opened where they do not — the root whose architect has
// revised the document its person approved.
func gateAt(root string, latest, approved int) map[DesignStep]DesignGate {
	return map[DesignStep]DesignGate{DesignStepSpec: {Issue: root, Step: DesignStepSpec,
		LatestVersion: latest, ApprovedVersion: &approved, ApprovedMark: EventMark{Seq: 7, At: approvedAt},
		EverOpened: true}}
}

// treeGates is the gate of each root, as LineFacts.Gates carries them: one entry per root, each
// holding the steps the deployment gates (none here, which gates the spec step alone).
func treeGates(rows map[string]map[DesignStep]DesignGate) map[string]TreeGate {
	gates := make(map[string]TreeGate, len(rows))
	for root, steps := range rows {
		gates[root] = TreeGate{Rows: steps}
	}
	return gates
}

// Every issue the records call admitted that the pass places nowhere gets exactly one row, and
// that row names what holds it and the issue the hold turns on. The line drops each of these
// silently, and before dispatch://INFRA-2964 nothing reported any of them: the issue stood at
// admitted in the daemon's own records, holding no slot, in no line, in no list a reader could
// see, and no later pass looked at it again.
//
// The cases are the line's own drops, one each, with the three kinds of issue it places (a slot
// holder, a line member, an issue another decider already names) and the two it never judges (a
// record past admitted, and one Dispatch has taken out of the workflow) beside them. Each case
// is read through record.Waiting first, so what the table asserts is what the line actually
// did, not a second rule written to agree with it.
func TestUnplacedNamesWhatHoldsEveryAdmittedIssueTheLineDrops(t *testing.T) {
	const root, member, other = "LEGION-10", "LEGION-11", "LEGION-20"
	admittedRoot := func(key string) Issue {
		return Issue{Key: key, Tree: key, Project: "LEGION", Status: "todo", Phase: phase.Admitted, HandedOver: true}
	}
	admittedMember := func(key, tree string) Issue {
		return Issue{Key: key, Tree: tree, Parent: ParentOf(tree), Project: "LEGION", Status: "todo", Phase: phase.Admitted}
	}
	lingersUntil := approvedAt.Add(72 * time.Hour)

	for _, tc := range []struct {
		name   string
		issues []Issue
		slots  []Slot
		facts  LineFacts
		want   []BlockedIssue
	}{{
		name: "a member of a tree whose root has closed waits on the hand-over that close owes it",
		issues: []Issue{
			{Key: root, Tree: root, Project: "LEGION", Status: "done", Phase: phase.Done, HandedOver: true},
			admittedMember(member, root),
		},
		want: []BlockedIssue{{Issue: member, Wait: &Wait{Kind: WaitTreeClosed, Issue: root}}},
	}, {
		name: "a member of a tree whose root lingers after its close waits on the same hand-over",
		issues: func() []Issue {
			closing := admittedRoot(root)
			closing.LingerUntil = &lingersUntil
			return []Issue{closing, admittedMember(member, root)}
		}(),
		facts: LineFacts{Lifecycles: map[string]treelifecycle.Lifecycle{root: openTree}},
		// The root itself queues again in the line, which is what its todo asks for: the
		// re-admission its promotion makes is what ends the linger. Its member does not go
		// with it — nothing of the tree that ended starts again until that re-admission — so
		// the member alone is held, on the root.
		want: []BlockedIssue{{Issue: member, Wait: &Wait{Kind: WaitTreeClosed, Issue: root}}},
	}, {
		name: "a released member waits on the approval of its tree's revised design gate",
		issues: []Issue{
			{Key: root, Tree: root, Project: "LEGION", Status: "in_progress", Phase: phase.Admitted, HandedOver: true},
			admittedMember(member, root),
		},
		slots: []Slot{{Issue: root, Index: 0, AdmittedAt: approvedAt}},
		facts: LineFacts{
			Lifecycles: map[string]treelifecycle.Lifecycle{root: openTree},
			Gates:      treeGates(map[string]map[DesignStep]DesignGate{root: gateAt(root, 2, 1)}),
		},
		want: []BlockedIssue{{Issue: member, Wait: &Wait{Kind: WaitDesignGate, Issue: root}}},
	}, {
		name: "a root whose own gate is shut and that holds no slot waits on that same approval",
		issues: []Issue{
			{Key: root, Tree: root, Project: "LEGION", Status: "in_progress", Phase: phase.Admitted, HandedOver: true},
		},
		facts: LineFacts{
			Lifecycles: map[string]treelifecycle.Lifecycle{root: openTree},
			Gates:      treeGates(map[string]map[DesignStep]DesignGate{root: gateAt(root, 2, 1)}),
		},
		want: []BlockedIssue{{Issue: root, Wait: &Wait{Kind: WaitDesignGate, Issue: root}}},
	}, {
		name: "a member of a tree whose root has asked a person waits on that answer",
		issues: func() []Issue {
			asking := admittedRoot(root)
			asking.Status, asking.Phase = "in_progress", phase.AwaitingPerson
			asking.Awaiting = &Awaiting{Ask: "ask-1", Since: EventMark{Seq: 3}}
			return []Issue{asking, admittedMember(member, root)}
		}(),
		slots: []Slot{{Issue: root, Index: 0, AdmittedAt: approvedAt}},
		facts: LineFacts{
			Lifecycles: map[string]treelifecycle.Lifecycle{root: openTree},
			Gates:      treeGates(map[string]map[DesignStep]DesignGate{root: gateAt(root, 1, 1)}),
		},
		want: []BlockedIssue{{Issue: member, Wait: &Wait{Kind: WaitAwaitingPerson, Issue: root}}},
	}, {
		name: "a member beneath an ancestor the daemon has accepted waits on that ancestor's close",
		issues: func() []Issue {
			accepted := admittedMember(member, root)
			accepted.Phase, accepted.Status = phase.Merging, "retro"
			grandchild := admittedMember("LEGION-12", root)
			grandchild.Parent = ParentOf(member)
			return []Issue{
				{Key: root, Tree: root, Project: "LEGION", Status: "in_progress", Phase: phase.Implementing, HandedOver: true},
				accepted, grandchild,
			}
		}(),
		slots: []Slot{{Issue: root, Index: 0, AdmittedAt: approvedAt}, {Issue: member, Index: 1, AdmittedAt: approvedAt}},
		facts: LineFacts{
			Lifecycles: map[string]treelifecycle.Lifecycle{root: openTree},
			Gates:      treeGates(map[string]map[DesignStep]DesignGate{root: gateAt(root, 1, 1)}),
		},
		want: []BlockedIssue{{Issue: "LEGION-12", Wait: &Wait{Kind: WaitAcceptedAncestor, Issue: member}}},
	}, {
		name: "a root whose Dispatch chain reaches a live tree waits on that tree's release",
		issues: func() []Issue {
			queued := admittedRoot(other)
			queued.Ancestry = []string{root}
			return []Issue{
				{Key: root, Tree: root, Project: "LEGION", Status: "in_progress", Phase: phase.Implementing, HandedOver: true},
				queued,
			}
		}(),
		slots: []Slot{{Issue: root, Index: 0, AdmittedAt: approvedAt}},
		facts: LineFacts{
			Lifecycles: map[string]treelifecycle.Lifecycle{root: openTree},
			Gates:      treeGates(map[string]map[DesignStep]DesignGate{root: gateAt(root, 1, 1)}),
		},
		want: []BlockedIssue{{Issue: other, Wait: &Wait{Kind: WaitUnreleased, Issue: root}}},
	}, {
		name: "a root a person has taken the legion label off is reported as the pause it is",
		issues: func() []Issue {
			unlabelled := admittedRoot(root)
			unlabelled.HandedOver = false
			return []Issue{unlabelled}
		}(),
		want: []BlockedIssue{{Issue: root, Wait: &Wait{Kind: WaitUnlabelled}}},
	}, {
		name: "a member a Dispatch link holds is left to the decider that holds it",
		issues: []Issue{
			{Key: root, Tree: root, Project: "LEGION", Status: "in_progress", Phase: phase.Implementing, HandedOver: true},
			admittedMember(member, root),
		},
		slots: []Slot{{Issue: root, Index: 0, AdmittedAt: approvedAt}},
		facts: LineFacts{
			Lifecycles: map[string]treelifecycle.Lifecycle{root: openTree},
			Gates:      treeGates(map[string]map[DesignStep]DesignGate{root: gateAt(root, 1, 1)}),
			Held:       map[string]struct{}{member: {}},
		},
	}, {
		name: "a parent whose own children go first is left to the decider that holds it",
		issues: []Issue{
			{Key: root, Tree: root, Project: "LEGION", Status: "in_progress", Phase: phase.Implementing, HandedOver: true},
			admittedMember(member, root),
		},
		slots: []Slot{{Issue: root, Index: 0, AdmittedAt: approvedAt}},
		facts: LineFacts{
			Lifecycles: map[string]treelifecycle.Lifecycle{root: openTree},
			Gates:      treeGates(map[string]map[DesignStep]DesignGate{root: gateAt(root, 1, 1)}),
			ChildWaits: map[string]ChildWait{member: {Parent: member, Open: []OpenChild{{Key: "LEGION-99", Status: "todo"}}}},
		},
	}, {
		name: "a record past admitted and one Dispatch has taken out of the workflow are never judged",
		issues: func() []Issue {
			parked := admittedMember(member, root)
			parked.Status = "backlog"
			return []Issue{
				{Key: root, Tree: root, Project: "LEGION", Status: "in_progress", Phase: phase.Implementing, HandedOver: true},
				parked,
			}
		}(),
		slots: []Slot{{Issue: root, Index: 0, AdmittedAt: approvedAt}},
		facts: LineFacts{
			Lifecycles: map[string]treelifecycle.Lifecycle{root: openTree},
			Gates:      treeGates(map[string]map[DesignStep]DesignGate{root: gateAt(root, 1, 1)}),
		},
	}, {
		name:   "a root the line carries is placed there and gets no row",
		issues: []Issue{admittedRoot(root)},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			line := Waiting(tc.issues, tc.slots, tc.facts)
			got := Unplaced(tc.issues, tc.slots, line, tc.facts)
			if len(got) == 0 && len(tc.want) == 0 {
				assertEveryAdmittedIssueIsAccountedFor(t, tc.issues, tc.slots, line, tc.facts, got)
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Unplaced = %s, want %s", showRows(got), showRows(tc.want))
			}
			assertEveryAdmittedIssueIsAccountedFor(t, tc.issues, tc.slots, line, tc.facts, got)
		})
	}
}

// Unplaced resolves the holding root from the complete recorded ancestry, so a stranded member
// is reported alike to both callers: the state's pass, whose records carry the closed root
// because it reads every issue of the project (projection.Project), and the controller's tick
// notice, whose records are the live ones alone (Store.LiveIssues) and whose closed root reaches
// it only as a parent the pass read above its own (ReadParents, LineFacts.Parents).
//
// The difference is the defect this closes: with the root resolved from the pass's own records
// alone, the notice reported nothing at all for the child, while the state reported it, and the
// fixture that carried the closed root directly hid the difference between the two.
func TestUnplacedReportsAStrandedMemberToBothOfItsCallers(t *testing.T) {
	const root, member = "LEGION-320", "LEGION-321"
	closed := Issue{Key: root, Tree: root, Project: "LEGION", Status: "done", Phase: phase.Done, HandedOver: true}
	stranded := Issue{Key: member, Tree: root, Parent: ParentOf(root), Project: "LEGION", Status: "todo", Phase: phase.Admitted}
	want := []BlockedIssue{{Issue: member, Wait: &Wait{Kind: WaitTreeClosed, Issue: root}}}

	for _, tc := range []struct {
		name   string
		issues []Issue
		facts  LineFacts
	}{
		{name: "the state's pass, whose records carry the closed root", issues: []Issue{closed, stranded}},
		{name: "the tick notice's pass, whose live records leave it out", issues: []Issue{stranded},
			facts: LineFacts{Parents: map[string]Issue{root: closed}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := Waiting(tc.issues, nil, tc.facts)
			if len(line) != 0 {
				t.Fatalf("the line is %v, want nothing in it: this case proves nothing otherwise", line)
			}
			if got := Unplaced(tc.issues, nil, line, tc.facts); !reflect.DeepEqual(got, want) {
				t.Fatalf("Unplaced = %s, want %s", showRows(got), showRows(want))
			}
		})
	}
}

// assertEveryAdmittedIssueIsAccountedFor is the completeness half of the invariant, read over
// one pass: every issue the records call admitted and in the workflow holds a slot, stands in
// the line, is named by one of the two deciders this defers to, or carries a row here. An issue
// in none of those is one no reader can see.
func assertEveryAdmittedIssueIsAccountedFor(t *testing.T, issues []Issue, slots []Slot, line []LineEntry,
	facts LineFacts, rows []BlockedIssue,
) {
	t.Helper()
	named := make(map[string]string, len(issues))
	note := func(key, where string) {
		if named[key] == "" {
			named[key] = where
		}
	}
	for _, slot := range slots {
		note(slot.Issue, "active")
	}
	for _, entry := range line {
		note(entry.Issue.Key, "waiting")
	}
	for key := range facts.Held {
		note(key, "held by a link")
	}
	for key := range facts.ChildWaits {
		note(key, "waiting on its children")
	}
	for _, row := range rows {
		if row.Wait == nil {
			t.Errorf("the row for %s carries no wait: every row here names one", row.Issue)
		}
		note(row.Issue, "unplaced")
	}
	for _, issue := range issues {
		if issue.Phase != phase.Admitted || !InWorkflow(issue) {
			continue
		}
		if named[issue.Key] == "" {
			t.Errorf("%s is recorded admitted and is in none of the pass's lists: nothing starts it where it stands and the state says nothing about it",
				issue.Key)
		}
	}
}

// showRows renders blocked rows with the wait each carries, which %#v prints as a pointer.
func showRows(rows []BlockedIssue) string {
	out := "["
	for i, row := range rows {
		if i > 0 {
			out += " "
		}
		out += row.Issue + "("
		switch {
		case row.Wait != nil:
			out += string(row.Wait.Kind) + ":" + row.Wait.Issue
		default:
			out += "blockedBy "
			for j, by := range row.BlockedBy {
				if j > 0 {
					out += ","
				}
				out += by
			}
		}
		out += ")"
	}
	return out + "]"
}
