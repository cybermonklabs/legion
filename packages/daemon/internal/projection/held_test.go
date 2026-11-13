package projection

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// treeRoot is a tree's own root as the two tests here record one: a labelled issue whose key is
// its own tree, ranked by that key so the waiting line's order is the keys' own (lineIssue).
func treeRoot(key, status string, stands phase.Phase) record.Issue {
	return lineIssue(key, key, status, stands, key)
}

// treeMember is an issue inside tree, hanging under parent: the shape every word of the ladder
// but label_off is read over.
func treeMember(key, tree, parent, status string, stands phase.Phase) record.Issue {
	issue := lineIssue(key, tree, status, stands, key)
	issue.Parent = record.ParentOf(parent)
	return issue
}

// openGate is a root's design gate with a person's approval standing on its current version,
// given at approvedAt: the gate of a tree whose work may run (record.TreeGate.Open), and the
// moment the line reads as the root's own work coming back (record.TreeGate.OpenedAt).
func openGate(root string, approvedAt time.Time) map[record.DesignStep]record.DesignGate {
	approved := 1
	return map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {Issue: root, Step: record.DesignStepSpec,
		ArtifactID: "artifact-" + root, LatestVersion: 1, ApprovedVersion: &approved,
		ApprovedMark: record.EventMark{Seq: 7, At: approvedAt}, EverOpened: true}}
}

// shutGate is a root's design gate at a version later than the one a person approved: shut now,
// and open once, so the root's own record says this generation has run and it is no root still
// waiting for its tree to be opened (record.QueuedRoot).
func shutGate(root string) map[record.DesignStep]record.DesignGate {
	approved := 1
	return map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {Issue: root, Step: record.DesignStepSpec,
		ArtifactID: "artifact-" + root, LatestVersion: 2, ApprovedVersion: &approved, EverOpened: true}}
}

// C1 of dispatch://INFRA-3107: every issue of the project the daemon's record holds in the
// workflow stands in admission.active, in admission.waiting or in admission.held, and no issue
// stands in held and in one of the other two. The fixture carries one issue for each of ten of
// the eleven words of the ladder — a member of a closed tree, a member of a parked tree, a root
// whose label is off, a root whose Dispatch chain reaches another live root, a member of a tree
// whose root waits on a person, a member under a shut design gate, a child an unfinished Dispatch
// link holds, a parent whose own children are still open, an issue beneath an accepted ancestor,
// and an issue none of the ten is true of — and the rows are read from the projected state, which
// is where a reader meets them. No issue stands for at_work: cybermonklabs/legion#2539 deleted the
// phase awaiting_merge, so no phase holds no admission slot and nothing produces that word today.
//
// Five controls stand beside them, each an issue that must take no held row. Two are the shapes
// the requirement names: a slot holder, and a root a person answered, which stands in active and
// in waiting at once, the one overlap the contract states. Three more are the running tree's
// ordinary shapes, and they are what the disjointness of the lists turns on: a root holds one of
// the cap's slots from the moment its architect begins, so a root whose design gate is shut, one
// whose children go first and one an unfinished link holds each stand in active with no word
// against them. A held list built from the ladder alone, rather than from the issues the slots
// and the line leave behind, would give all three a word and claim the daemon runs nothing of a
// tree it is running.
//
// admission.blocked is read as well: it keeps its own rows and its overlaps with all three lists,
// so the child a link holds keeps its row there while it stands in held, and the root whose links
// its promotion reads keeps its row there while it stands in waiting.
func TestEveryIssueInTheWorkflowHasAPlace(t *testing.T) {
	opened := projectedAt.Add(-time.Hour)
	answeredAt := projectedAt.Add(-30 * time.Minute)

	// A tree a person closed and a tree a person parked. Neither root is in the workflow, so
	// neither is a candidate for a word of its own, and each holds its member where it stands.
	closedRoot := treeRoot("LEGION-100", "done", phase.Done)
	closedMember := treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)
	parkedRoot := treeRoot("LEGION-110", "backlog", phase.Admitted)
	parkedMember := treeMember("LEGION-111", "LEGION-110", "LEGION-110", "todo", phase.Admitted)
	// A root the record says the Legion label is off: nothing hands it to the daemon.
	unlabelled := treeRoot("LEGION-120", "todo", phase.Admitted)
	unlabelled.HandedOver = false
	// A root waiting for its tree to be opened, which is a live tree already, and the root whose
	// Dispatch chain reaches it: that one is the first tree's unreleased work.
	queuedRoot := treeRoot("LEGION-130", "todo", phase.Admitted)
	underQueued := treeRoot("LEGION-140", "todo", phase.Admitted)
	underQueued.Parent, underQueued.Ancestry = record.ParentOf("LEGION-130"), []string{"LEGION-130"}
	// A root that handed work to a person, holding the slot its wait keeps, and a member of its
	// tree: the member starts with that root's resume. The root's gate stands open, so the word
	// its member reads is the wait and nothing else.
	askingRoot := treeRoot("LEGION-150", "in_progress", phase.AwaitingPerson)
	askingRoot.Awaiting = &record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: 5}}
	askingMember := treeMember("LEGION-151", "LEGION-150", "LEGION-150", "todo", phase.Admitted)
	// A root at admitted whose architect is revising the design, holding its slot under a shut
	// gate, and a member of its tree.
	gatedRoot := treeRoot("LEGION-160", "todo", phase.Admitted)
	gatedMember := treeMember("LEGION-161", "LEGION-160", "LEGION-160", "todo", phase.Admitted)
	// A running tree under an open gate: its root holds a slot and waits for the one open child
	// Dispatch shows under it, and its members carry three of the words. The issue in merging is
	// an ancestor the daemon has accepted (record.Accepted) and holds its slot, the issue under it
	// starts at its close, the issue at work that holds no row is the one the pass names nowhere,
	// and the issue at work that holds one is the plain slot holder. The accepted ancestor stood
	// at awaiting_merge, which held no slot and read at_work, until cybermonklabs/legion#2539
	// deleted that phase; it stands in merging now, the accepted phase that still exists, so it
	// holds a slot row and stands in active, and the issue under it reads what it read before.
	openRoot := treeRoot("LEGION-170", "todo", phase.Admitted)
	blockedChild := treeMember("LEGION-171", "LEGION-170", "LEGION-170", "todo", phase.Admitted)
	merging := treeMember("LEGION-172", "LEGION-170", "LEGION-170", "needs_review", phase.Merging)
	beneathMerging := treeMember("LEGION-173", "LEGION-170", "LEGION-172", "todo", phase.Admitted)
	unplaced := treeMember("LEGION-174", "LEGION-170", "LEGION-170", "in_progress", phase.Implementing)
	slotHolder := treeMember("LEGION-175", "LEGION-170", "LEGION-170", "in_progress", phase.Implementing)
	// A second running tree, whose root an unfinished Dispatch link names and whose member waits
	// for a child nobody has released.
	linkedRoot := treeRoot("LEGION-180", "todo", phase.Admitted)
	parentOfOpen := treeMember("LEGION-181", "LEGION-180", "LEGION-180", "todo", phase.Admitted)
	// A root a person answered, which waits in the line on the slot its wait kept.
	answeredRoot := treeRoot("LEGION-190", "in_progress", phase.AwaitingPerson)
	answeredRoot.Awaiting = &record.Awaiting{Ask: "ask-q", AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByAsk,
		Since: record.EventMark{Seq: 5}}

	store := projectionStore{
		issues: []record.Issue{closedRoot, closedMember, parkedRoot, parkedMember, unlabelled, queuedRoot, underQueued,
			askingRoot, askingMember, gatedRoot, gatedMember, openRoot, blockedChild, merging, beneathMerging, unplaced,
			slotHolder, linkedRoot, parentOfOpen, answeredRoot},
		slots: []record.Slot{
			{Issue: "LEGION-150", Index: 0, AdmittedAt: projectedAt},
			{Issue: "LEGION-160", Index: 1, AdmittedAt: projectedAt},
			{Issue: "LEGION-170", Index: 2, AdmittedAt: projectedAt},
			{Issue: "LEGION-175", Index: 3, AdmittedAt: projectedAt},
			{Issue: "LEGION-180", Index: 4, AdmittedAt: projectedAt},
			{Issue: "LEGION-190", Index: 5, AdmittedAt: projectedAt},
			{Issue: "LEGION-172", Index: 6, AdmittedAt: projectedAt},
		},
		lifecycles: []string{"LEGION-150", "LEGION-160", "LEGION-170", "LEGION-180", "LEGION-190"},
		gates: map[string]map[record.DesignStep]record.DesignGate{
			"LEGION-150": openGate("LEGION-150", opened),
			"LEGION-160": shutGate("LEGION-160"),
			"LEGION-170": openGate("LEGION-170", opened),
			"LEGION-180": openGate("LEGION-180", opened),
		},
		// The mirror names a Dispatch parent only where this fixture means the children-first
		// rule to read one: the child a link holds, which is the open child of its tree's root,
		// and the child nobody has released, which is the open child of its own parent. A row
		// with no parent is Dispatch's own answer that the issue hangs under nothing.
		mirror: []record.DispatchIssueRow{
			{Key: "LEGION-130", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-903"}},
			{Key: "LEGION-171", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900"}, Parent: "LEGION-170"},
			{Key: "LEGION-180", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-901"}},
			{Key: "LEGION-182", Project: "LEGION", Status: "backlog", Parent: "LEGION-181"},
			{Key: "LEGION-900", Project: "LEGION", Status: "todo"},
			{Key: "LEGION-901", Project: "LEGION", Status: "todo"},
			{Key: "LEGION-903", Project: "LEGION", Status: "todo"},
		},
	}

	got, err := Project(context.Background(), nil, store, "LEGION", nil, nil, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	wantHeld := []api.HeldIssue{
		{Issue: "LEGION-101", Reason: record.HeldTreeClosed, HeldBy: []string{"LEGION-100"}},
		{Issue: "LEGION-111", Reason: record.HeldTreeParked, HeldBy: []string{"LEGION-110"}},
		{Issue: "LEGION-120", Reason: record.HeldLabelOff},
		{Issue: "LEGION-140", Reason: record.HeldAncestry, HeldBy: []string{"LEGION-130"}},
		{Issue: "LEGION-151", Reason: record.HeldRootAwaitingPerson, HeldBy: []string{"LEGION-150"}},
		{Issue: "LEGION-161", Reason: record.HeldDesignGate, HeldBy: []string{"LEGION-160"}},
		{Issue: "LEGION-171", Reason: record.HeldDispatchBlocker, HeldBy: []string{"LEGION-900"}},
		{Issue: "LEGION-173", Reason: record.HeldAcceptedAncestor, HeldBy: []string{"LEGION-172"}},
		{Issue: "LEGION-174", Reason: record.HeldUnclassified},
		{Issue: "LEGION-181", Reason: record.HeldChildrenFirst, HeldBy: []string{"LEGION-182"}},
	}
	if !reflect.DeepEqual(got.Admission.Held, wantHeld) {
		t.Fatalf("admission.held = %#v, want %#v", got.Admission.Held, wantHeld)
	}

	// LEGION-172, the accepted ancestor in merging, holds the last slot row: it stood at
	// awaiting_merge, which held none, until cybermonklabs/legion#2539 deleted that phase.
	wantActive := []string{"LEGION-150", "LEGION-160", "LEGION-170", "LEGION-175", "LEGION-180", "LEGION-190", "LEGION-172"}
	if !reflect.DeepEqual(got.Admission.Active, wantActive) {
		t.Fatalf("admission.active = %#v, want %#v: every issue holding a slot row, the three roots a hold keeps out of the line among them",
			got.Admission.Active, wantActive)
	}
	// The root whose gate opened and the root a person answered are the work that came back,
	// oldest first; the root waiting for its tree to be opened starts after them.
	wantWaiting := []string{"LEGION-180", "LEGION-190", "LEGION-130"}
	if !reflect.DeepEqual(got.Admission.Waiting, wantWaiting) {
		t.Fatalf("admission.waiting = %#v, want %#v", got.Admission.Waiting, wantWaiting)
	}

	held := make([]string, 0, len(got.Admission.Held))
	for _, row := range got.Admission.Held {
		held = append(held, row.Issue)
	}
	for _, issue := range store.issues {
		if !record.InWorkflow(issue) {
			continue
		}
		active, waiting := slices.Contains(got.Admission.Active, issue.Key), slices.Contains(got.Admission.Waiting, issue.Key)
		if standing := slices.Contains(held, issue.Key); !standing && !active && !waiting {
			t.Errorf("%s stands in none of active, waiting and held, though the record holds it in the workflow", issue.Key)
		} else if standing && (active || waiting) {
			t.Errorf("%s stands in held and in active or waiting: the held list names no issue either of the two names", issue.Key)
		}
	}
	var both []string
	for _, key := range got.Admission.Active {
		if slices.Contains(got.Admission.Waiting, key) {
			both = append(both, key)
		}
	}
	// The one overlap the contract states: an issue holding a slot whose work came back waits in
	// the line for its next turn and is promoted without taking a second row.
	if want := []string{"LEGION-180", "LEGION-190"}; !reflect.DeepEqual(both, want) {
		t.Fatalf("the issues standing in active and in waiting = %#v, want %#v: the root whose gate opened and the root a person answered",
			both, want)
	}

	// heldBy is absent where no issue holds the one that waits — a root whose label is off, an
	// issue in a phase that keeps no slot (none since cybermonklabs/legion#2539), an issue the
	// pass names nowhere — which is a nil slice the wire
	// form then omits, and not an empty array.
	for _, row := range got.Admission.Held {
		wire, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal the held row of %s: %v", row.Issue, err)
		}
		holds := !slices.Contains([]record.HeldReason{record.HeldLabelOff, record.HeldAtWork, record.HeldUnclassified}, row.Reason)
		if (row.HeldBy != nil) != holds || strings.Contains(string(wire), `"heldBy"`) != holds {
			t.Errorf("the held row of %s reads %s, want heldBy carried %t under the word %s", row.Issue, wire, holds, row.Reason)
		}
	}

	// admission.blocked keeps its rows and its overlaps with all three lists: the child a link
	// holds stands in held, the root whose links its promotion reads stands in waiting, and the
	// root whose own first phase waits for its open child stands in active.
	wantBlocked := []api.BlockedIssue{
		{Issue: "LEGION-130", BlockedBy: []string{"LEGION-903"}},
		{Issue: "LEGION-170", BlockedBy: []string{"LEGION-171"}},
		{Issue: "LEGION-171", BlockedBy: []string{"LEGION-900"}},
		{Issue: "LEGION-181", BlockedBy: []string{"LEGION-182"}},
	}
	if !reflect.DeepEqual(got.Admission.Blocked, wantBlocked) {
		t.Fatalf("admission.blocked = %#v, want %#v", got.Admission.Blocked, wantBlocked)
	}
}

// wordFor is the held row the projected state gives one issue: the word for why nothing of it
// runs and the issues that hold it (heldRow).
func wordFor(t *testing.T, store projectionStore, key string) api.HeldIssue {
	t.Helper()
	state, err := Project(context.Background(), nil, store, "LEGION", nil, nil, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	return heldRow(t, state, key)
}

// heldRow is the row of admission.held that state gives key. It fails where the issue stands in
// no held row at all, and says where it stands instead, because that is the other way a case of
// the ladder can go wrong.
func heldRow(t *testing.T, state api.State, key string) api.HeldIssue {
	t.Helper()
	for _, row := range state.Admission.Held {
		if row.Issue == key {
			return row
		}
	}
	t.Fatalf("%s stands in no held row; held = %#v, active = %v, waiting = %v",
		key, state.Admission.Held, state.Admission.Active, state.Admission.Waiting)
	return api.HeldIssue{}
}

// The word one issue reads is the first of the eleven that is true of it, and not whichever hold
// a reader noticed first (C1 of dispatch://INFRA-3107). Each case below makes two of the words
// true of one issue at once and requires the earlier one, so the order itself is what is proved
// rather than the eleven clauses one at a time; the last case is the control, an issue one word
// only is true of, which must read that word whatever the order says.
func TestTheHeldReasonIsTheFirstThatIsTrue(t *testing.T) {
	opened := projectedAt.Add(-time.Hour)
	lingering, expired := projectedAt.Add(time.Hour), projectedAt.Add(-time.Hour)
	openRoot := treeRoot("LEGION-1", "todo", phase.Admitted)
	slotted := []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: projectedAt}}

	// A root parked in backlog whose linger is still running, and the same root once it has run
	// out. The two cases differ in nothing but the deadline against the moment the pass reads the
	// record (record.LineFacts.Now), and they are here because that moment is the one fact of the
	// ladder a clock decides: every park arms a linger (the workflow's beginLinger) and nothing
	// but a re-admission clears it, so a word that read Issue.Lingers instead would give every
	// parked tree's member tree_closed for ever and leave tree_parked unreachable.
	parkedWithLinger := func(until time.Time) projectionStore {
		root := treeRoot("LEGION-1", "backlog", phase.Admitted)
		root.LingerUntil = &until
		return projectionStore{issues: []record.Issue{root,
			treeMember("LEGION-2", "LEGION-1", "LEGION-1", "todo", phase.Admitted)}}
	}

	for _, tc := range []struct {
		name  string
		store projectionStore
		issue string
		want  api.HeldIssue
	}{
		{
			// tree_closed before design_gate: nothing of a closed tree runs again until a
			// re-admission opens it, whatever its gate says.
			name: "a closed tree under a shut gate",
			store: projectionStore{
				issues: []record.Issue{treeRoot("LEGION-1", "done", phase.Done),
					treeMember("LEGION-2", "LEGION-1", "LEGION-1", "todo", phase.Admitted)},
				gates: map[string]map[record.DesignStep]record.DesignGate{"LEGION-1": shutGate("LEGION-1")},
			},
			issue: "LEGION-2",
			want:  api.HeldIssue{Issue: "LEGION-2", Reason: record.HeldTreeClosed, HeldBy: []string{"LEGION-1"}},
		},
		{
			// tree_parked before dispatch_blocker: the link would hold the child where it stands
			// if its tree ran, and its tree is stopped.
			name: "a parked tree with an unfinished Dispatch link",
			store: projectionStore{
				issues: []record.Issue{treeRoot("LEGION-1", "backlog", phase.Admitted),
					treeMember("LEGION-2", "LEGION-1", "LEGION-1", "todo", phase.Admitted)},
				mirror: []record.DispatchIssueRow{
					{Key: "LEGION-2", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900"}},
					{Key: "LEGION-900", Project: "LEGION", Status: "todo"},
				},
			},
			issue: "LEGION-2",
			want:  api.HeldIssue{Issue: "LEGION-2", Reason: record.HeldTreeParked, HeldBy: []string{"LEGION-1"}},
		},
		{
			// label_off before ancestry: a root nothing hands to Legion waits for nothing at all,
			// so the release that would bring it into the tree its chain reaches is not its wait.
			name: "a root whose label is off whose chain reaches a live root",
			store: projectionStore{issues: []record.Issue{func() record.Issue {
				root := treeRoot("LEGION-1", "todo", phase.Admitted)
				root.HandedOver = false
				root.Parent, root.Ancestry = record.ParentOf("LEGION-9"), []string{"LEGION-9"}
				return root
			}(), treeRoot("LEGION-9", "todo", phase.Admitted)}},
			issue: "LEGION-1",
			want:  api.HeldIssue{Issue: "LEGION-1", Reason: record.HeldLabelOff},
		},
		{
			// root_awaiting_person before design_gate: the members of such a tree start with the
			// root's resume, which comes before anything its architect does to the design.
			name: "a root that waits on a person whose gate is also shut",
			store: projectionStore{
				issues: []record.Issue{func() record.Issue {
					root := treeRoot("LEGION-1", "in_progress", phase.AwaitingPerson)
					root.Awaiting = &record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: 5}}
					return root
				}(), treeMember("LEGION-2", "LEGION-1", "LEGION-1", "todo", phase.Admitted)},
				gates: map[string]map[record.DesignStep]record.DesignGate{"LEGION-1": shutGate("LEGION-1")},
			},
			issue: "LEGION-2",
			want:  api.HeldIssue{Issue: "LEGION-2", Reason: record.HeldRootAwaitingPerson, HeldBy: []string{"LEGION-1"}},
		},
		{
			// dispatch_blocker before accepted_ancestor: both hold the child at admitted, and the
			// link is the nearer wait, which a person can end by closing the issue it names. The
			// ancestor stood at awaiting_merge until cybermonklabs/legion#2539 deleted that phase;
			// it stands in merging now, the accepted phase that still exists (record.Accepted).
			name: "a child a link holds under an accepted ancestor",
			store: projectionStore{
				issues: []record.Issue{openRoot,
					treeMember("LEGION-2", "LEGION-1", "LEGION-1", "needs_review", phase.Merging),
					treeMember("LEGION-3", "LEGION-1", "LEGION-2", "todo", phase.Admitted)},
				slots:      slotted,
				lifecycles: []string{"LEGION-1"},
				gates:      map[string]map[record.DesignStep]record.DesignGate{"LEGION-1": openGate("LEGION-1", opened)},
				mirror: []record.DispatchIssueRow{
					{Key: "LEGION-3", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900"}},
					{Key: "LEGION-900", Project: "LEGION", Status: "todo"},
				},
			},
			issue: "LEGION-3",
			want:  api.HeldIssue{Issue: "LEGION-3", Reason: record.HeldDispatchBlocker, HeldBy: []string{"LEGION-900"}},
		},
		{
			// dispatch_blocker before children_first: the child waits on both, and the link is
			// read first, so the row names the issue a person can close rather than the child
			// nobody has released.
			name: "a child a link holds whose own children are open",
			store: projectionStore{
				issues: []record.Issue{openRoot,
					treeMember("LEGION-2", "LEGION-1", "LEGION-1", "todo", phase.Admitted)},
				slots:      slotted,
				lifecycles: []string{"LEGION-1"},
				gates:      map[string]map[record.DesignStep]record.DesignGate{"LEGION-1": openGate("LEGION-1", opened)},
				mirror: []record.DispatchIssueRow{
					{Key: "LEGION-2", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900"}},
					{Key: "LEGION-3", Project: "LEGION", Status: "backlog", Parent: "LEGION-2"},
					{Key: "LEGION-900", Project: "LEGION", Status: "todo"},
				},
			},
			issue: "LEGION-2",
			want:  api.HeldIssue{Issue: "LEGION-2", Reason: record.HeldDispatchBlocker, HeldBy: []string{"LEGION-900"}},
		},
		{
			name:  "a member of a parked tree whose linger is still running",
			store: parkedWithLinger(lingering),
			issue: "LEGION-2",
			want:  api.HeldIssue{Issue: "LEGION-2", Reason: record.HeldTreeClosed, HeldBy: []string{"LEGION-1"}},
		},
		{
			name:  "a member of a parked tree whose linger has run out",
			store: parkedWithLinger(expired),
			issue: "LEGION-2",
			want:  api.HeldIssue{Issue: "LEGION-2", Reason: record.HeldTreeParked, HeldBy: []string{"LEGION-1"}},
		},
		{
			// C2: none of the ten words is true of an issue at work in a running tree that holds
			// no slot, and the pass names it nowhere. It takes a row all the same, so that a
			// reader can see the gap is in the words and not in the list.
			name: "an issue the pass names nowhere",
			store: projectionStore{
				issues: []record.Issue{openRoot,
					treeMember("LEGION-2", "LEGION-1", "LEGION-1", "in_progress", phase.Implementing)},
				slots:      slotted,
				lifecycles: []string{"LEGION-1"},
				gates:      map[string]map[record.DesignStep]record.DesignGate{"LEGION-1": openGate("LEGION-1", opened)},
			},
			issue: "LEGION-2",
			want:  api.HeldIssue{Issue: "LEGION-2", Reason: record.HeldUnclassified},
		},
		{
			// The control: one word only is true of this member — its tree runs, its root holds
			// its slot and waits on nobody, no link and no child holds it, no ancestor above it
			// is accepted and it keeps a slot where it stands — so the order decides nothing and
			// it must read that word.
			name: "a member under a shut gate and nothing else",
			store: projectionStore{
				issues: []record.Issue{treeRoot("LEGION-1", "todo", phase.Admitted),
					treeMember("LEGION-2", "LEGION-1", "LEGION-1", "todo", phase.Admitted)},
				slots:      slotted,
				lifecycles: []string{"LEGION-1"},
				gates:      map[string]map[record.DesignStep]record.DesignGate{"LEGION-1": shutGate("LEGION-1")},
			},
			issue: "LEGION-2",
			want:  api.HeldIssue{Issue: "LEGION-2", Reason: record.HeldDesignGate, HeldBy: []string{"LEGION-1"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wordFor(t, tc.store, tc.issue); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the held row of %s = %#v, want %#v", tc.issue, got, tc.want)
			}
		})
	}
}
