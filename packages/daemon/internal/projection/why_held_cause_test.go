package projection

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// heldEntry names one entry of heldSentences: a held word and one state of `why` the table
// serves it under.
type heldEntry struct {
	word  record.HeldReason
	state api.WhyState
}

// heldFixture is the store one entry of heldSentences is walked over, the held row the
// projected state must give the issue it names, and the sentence that issue's `why` must read,
// written out in full here rather than read from the table, so that a changed row of the table
// fails its row of the walk.
type heldFixture struct {
	store    projectionStore
	row      api.HeldIssue
	sentence string
}

// The sentence table of `why` (heldSentences) is walked entry by entry through the whole pass:
// for each word and each state the table serves it under, a fixture whose issue takes that word
// is projected, and the issue must carry that word in its held row, stand in `admission.blocked`
// under `blocked` and outside it under `unlisted`, and read that state with the table's sentence
// for that word and state over its row's heldBy, which each fixture writes out in full: the walk
// compares the served sentence with that literal text, never with heldSentences itself, so a
// change to a row's text fails that row here. Each fixture is one this package already proves
// the word of (held_test.go, held_triaged_root_test.go); a blocked one adds a Dispatch link that
// holds the issue, or, for a root whose label is off and for the root of an ancestry hold, an
// open Dispatch child under it, which the children-first rule holds it for (the ancestry root's
// gate opened as well, since that rule reads only the children of a root under an open gate).
//
// The walk is read off a projected state, never from the renderer alone, so the row the pass
// publishes and the sentence it serves are proved of one state. An entry no fixture walks fails
// the test: an entry added to the table is given a fixture here. at_work is the one exception:
// the issues that read it stand past their review and are not held (record.HeldAtWork), one
// whose tree runs is served no answer (atWorkPastReview), and no fixture here builds the member of
// a tree that runs nothing that the entry is served to.
func TestEveryEntryOfTheHeldSentenceTableIsServedThroughTheWholePass(t *testing.T) {
	opened := projectedAt.Add(-time.Hour)
	// The Dispatch link a blocked fixture adds: LEGION-101 waits on LEGION-900, which is open.
	link := []record.DispatchIssueRow{
		{Key: "LEGION-101", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900"}},
		{Key: "LEGION-900", Project: "LEGION", Status: "todo"},
	}
	linked := func(store projectionStore) projectionStore {
		store.mirror = link
		return store
	}

	// A member of a tree whose root has finished.
	closedTree := projectionStore{issues: []record.Issue{treeRoot("LEGION-100", "done", phase.Done),
		treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)}}
	// A member of a tree whose root a person parked in backlog, its linger run out.
	expired := projectedAt.Add(-time.Hour)
	parkedRoot := treeRoot("LEGION-100", "backlog", phase.Admitted)
	parkedRoot.LingerUntil = &expired
	parkedTree := projectionStore{issues: []record.Issue{parkedRoot,
		treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)}}
	// A root whose label is off.
	unlabelled := treeRoot("LEGION-100", "todo", phase.Admitted)
	unlabelled.HandedOver = false
	labelOff := projectionStore{issues: []record.Issue{unlabelled},
		gates: map[string]map[record.DesignStep]record.DesignGate{"LEGION-100": openGate("LEGION-100", opened)}}
	labelOffWithChild := labelOff
	labelOffWithChild.mirror = []record.DispatchIssueRow{{Key: "LEGION-101", Project: "LEGION", Status: "todo", Parent: "LEGION-100"}}
	// A root whose Dispatch chain reaches a live root waiting for its tree to be opened.
	underLive := treeRoot("LEGION-101", "todo", phase.Admitted)
	underLive.Parent, underLive.Ancestry = record.ParentOf("LEGION-100"), []string{"LEGION-100"}
	ancestry := projectionStore{issues: []record.Issue{treeRoot("LEGION-100", "todo", phase.Admitted), underLive}}
	// The children-first rule reads the Dispatch children of a root under an open gate alone
	// (record.ChildrenFirstCandidates), so the blocked fixture opens the root's gate as well.
	ancestryWithChild := ancestry
	ancestryWithChild.gates = map[string]map[record.DesignStep]record.DesignGate{"LEGION-101": openGate("LEGION-101", opened)}
	ancestryWithChild.mirror = []record.DispatchIssueRow{{Key: "LEGION-102", Project: "LEGION", Status: "todo", Parent: "LEGION-101"}}
	// A member of a tree whose root handed work to a person, holding the slot its wait keeps
	// under an open gate.
	askingRoot := treeRoot("LEGION-100", "in_progress", phase.AwaitingPerson)
	askingRoot.Awaiting = &record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: 5}}
	awaitingPerson := projectionStore{
		issues:     []record.Issue{askingRoot, treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)},
		slots:      []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: projectedAt}},
		lifecycles: []string{"LEGION-100"},
		gates:      map[string]map[record.DesignStep]record.DesignGate{"LEGION-100": openGate("LEGION-100", opened)},
	}
	// A member of a running tree whose design gate stands shut.
	shutTree := projectionStore{
		issues: []record.Issue{treeRoot("LEGION-100", "todo", phase.Admitted),
			treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)},
		slots:      []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: projectedAt}},
		lifecycles: []string{"LEGION-100"},
		gates:      map[string]map[record.DesignStep]record.DesignGate{"LEGION-100": shutGate("LEGION-100")},
	}
	// An issue beneath an ancestor the daemon has accepted, in a running tree under an open
	// gate, with the rows the admission pass leaves such a tree. The ancestor, LEGION-101, stands
	// in merging, which holds no slot (record.KeepsSlot), so it holds no row. Dispatch names it a
	// child of the root, and it is open, so the root's own first phase at admitted waits for it
	// (the children-first wait): the root gave its row back for that wait and carries the mark of
	// the release (ReleasedForWaitAt), as LEGION-170 does in held_test.go. No issue of the tree
	// holds a row.
	releasedRoot := treeRoot("LEGION-100", "todo", phase.Admitted)
	releasedRoot.ReleasedForWaitAt = &opened
	acceptedAncestor := projectionStore{
		issues: []record.Issue{releasedRoot,
			treeMember("LEGION-101", "LEGION-100", "LEGION-100", "needs_review", phase.Merging),
			treeMember("LEGION-102", "LEGION-100", "LEGION-101", "todo", phase.Admitted)},
		lifecycles: []string{"LEGION-100"},
		gates:      map[string]map[record.DesignStep]record.DesignGate{"LEGION-100": openGate("LEGION-100", opened)},
		mirror: []record.DispatchIssueRow{
			{Key: "LEGION-101", Project: "LEGION", Status: "needs_review", Parent: "LEGION-100"},
			{Key: "LEGION-102", Project: "LEGION", Status: "todo", Parent: "LEGION-101"},
		},
	}
	// A member of a tree whose root stands out of the workflow at triage, at phase done and
	// under a gate a person approved, so no word above unclassified is true of it.
	unclassified := projectionStore{
		issues: []record.Issue{treeRoot("LEGION-100", "triage", phase.Done),
			treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)},
		gates: map[string]map[record.DesignStep]record.DesignGate{"LEGION-100": openGate("LEGION-100", opened)},
	}

	byRoot := []string{"LEGION-100"}
	fixtures := map[heldEntry]heldFixture{
		{record.HeldTreeClosed, api.WhyBlocked}: {linked(closedTree),
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldTreeClosed, HeldBy: byRoot},
			"Its tree is closed: LEGION-100 has finished, or lingers after leaving the workflow, and the daemon starts no issue of that tree."},
		{record.HeldTreeClosed, api.WhyUnlisted}: {closedTree,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldTreeClosed, HeldBy: byRoot},
			"Its tree is closed: LEGION-100 has finished, or lingers after leaving the workflow, and the daemon starts no issue of that tree."},
		{record.HeldTreeParked, api.WhyBlocked}: {linked(parkedTree),
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldTreeParked, HeldBy: byRoot},
			"Its tree is parked: LEGION-100 stands in backlog or icebox, and the daemon starts no issue of that tree until that root is put back at todo."},
		{record.HeldTreeParked, api.WhyUnlisted}: {parkedTree,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldTreeParked, HeldBy: byRoot},
			"Its tree is parked: LEGION-100 stands in backlog or icebox, and the daemon starts no issue of that tree until that root is put back at todo."},
		{record.HeldLabelOff, api.WhyBlocked}: {labelOffWithChild,
			api.HeldIssue{Issue: "LEGION-100", Reason: record.HeldLabelOff},
			"It is the root of its own tree and its record says the legion label is off, so nothing hands it to the daemon."},
		{record.HeldLabelOff, api.WhyUnlisted}: {labelOff,
			api.HeldIssue{Issue: "LEGION-100", Reason: record.HeldLabelOff},
			"It is the root of its own tree and its record says the legion label is off, so nothing hands it to the daemon."},
		{record.HeldAncestry, api.WhyBlocked}: {ancestryWithChild,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldAncestry, HeldBy: byRoot},
			"Its Dispatch chain reaches live work above it: LEGION-100. It stands as that tree's unreleased work."},
		{record.HeldAncestry, api.WhyUnlisted}: {ancestry,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldAncestry, HeldBy: byRoot},
			"Its Dispatch chain reaches live work above it: LEGION-100. It stands as that tree's unreleased work."},
		{record.HeldRootAwaitingPerson, api.WhyBlocked}: {linked(awaitingPerson),
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldRootAwaitingPerson, HeldBy: byRoot},
			"Its tree's root, LEGION-100, has handed work to a person, and the daemon starts no issue of that tree before that root resumes."},
		{record.HeldRootAwaitingPerson, api.WhyUnlisted}: {awaitingPerson,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldRootAwaitingPerson, HeldBy: byRoot},
			"Its tree's root, LEGION-100, has handed work to a person, and the daemon starts no issue of that tree before that root resumes."},
		{record.HeldDesignGate, api.WhyBlocked}: {linked(shutTree),
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldDesignGate, HeldBy: byRoot},
			"The design gate of its tree stands shut: LEGION-100 waits for a person's approval of each gated step's document at its current version, and the daemon starts no issue of that tree until it opens."},
		{record.HeldDesignGate, api.WhyUnlisted}: {shutTree,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldDesignGate, HeldBy: byRoot},
			"The design gate of its tree stands shut: LEGION-100 waits for a person's approval of each gated step's document at its current version, and the daemon starts no issue of that tree until it opens."},
		{record.HeldAcceptedAncestor, api.WhyUnlisted}: {acceptedAncestor,
			api.HeldIssue{Issue: "LEGION-102", Reason: record.HeldAcceptedAncestor, HeldBy: []string{"LEGION-101"}},
			"The daemon has accepted the reviewed head of LEGION-101, which stands above it, and nothing of its own work starts before that issue closes."},
		{record.HeldUnclassified, api.WhyUnlisted}: {unclassified,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldUnclassified},
			"The daemon holds it in the workflow, and it stands in neither the issues at work nor the waiting line, so no slot has been offered to it. No rule of the held list says what it waits for."},
	}

	for word, entry := range heldSentences {
		for _, state := range entry.states {
			if _, walked := fixtures[heldEntry{word, state}]; !walked && word != record.HeldAtWork {
				t.Errorf("heldSentences serves %s under %s, and no fixture here walks that entry", word, state)
			}
		}
	}

	for _, word := range record.HeldReasons {
		for _, state := range []api.WhyState{api.WhyBlocked, api.WhyUnlisted} {
			fixture, walked := fixtures[heldEntry{word, state}]
			if !walked {
				continue
			}
			t.Run(string(word)+" under "+string(state), func(t *testing.T) {
				entry, listed := heldSentences[word]
				if !listed || !slices.Contains(entry.states, state) {
					t.Fatalf("a fixture walks %s under %s, which heldSentences does not serve", word, state)
				}
				key := fixture.row.Issue
				projected, err := Project(context.Background(), nil, fixture.store, "LEGION", nil, nil, nil, nil, projectedAt)
				if err != nil {
					t.Fatalf("Project: %v", err)
				}
				// The fixture's premise: its rows are the ones the admission pass leaves. Every
				// row names an issue whose phase keeps one (record.KeepsSlot) and that waits on no
				// other issue, since the pass takes the row of such a waiter back
				// (blockers.waitsOf), and every issue marked released for a wait still waits.
				issues := make(map[string]record.Issue, len(fixture.store.issues))
				for _, issue := range fixture.store.issues {
					issues[issue.Key] = issue
				}
				waits := make(map[string]bool, len(projected.Admission.Blocked))
				for _, blocked := range projected.Admission.Blocked {
					waits[blocked.Issue] = true
				}
				for _, slot := range fixture.store.slots {
					if holder := issues[slot.Issue]; !record.KeepsSlot(holder) || waits[slot.Issue] {
						t.Fatalf("the fixture gives %s, in %s, a row the pass does not leave it: its phase keeps a row %t, it waits on another issue %t",
							slot.Issue, holder.Phase, record.KeepsSlot(holder), waits[slot.Issue])
					}
				}
				for _, issue := range fixture.store.issues {
					if issue.ReleasedForWaitAt != nil && !waits[issue.Key] {
						t.Fatalf("the fixture marks %s released for a wait, and it waits on no other issue; blocked = %#v",
							issue.Key, projected.Admission.Blocked)
					}
				}
				row := heldRow(t, projected, key)
				if row.Reason != fixture.row.Reason || !slices.Equal(row.HeldBy, fixture.row.HeldBy) {
					t.Fatalf("the held row of %s = %#v, want %#v", key, row, fixture.row)
				}
				blocked := slices.ContainsFunc(projected.Admission.Blocked, func(b api.BlockedIssue) bool { return b.Issue == key })
				if blocked != (state == api.WhyBlocked) {
					t.Fatalf("%s stands in admission.blocked = %t, want %t; blocked = %#v",
						key, blocked, state == api.WhyBlocked, projected.Admission.Blocked)
				}
				view := projected.Issues[key]
				if view.Why == nil {
					t.Fatalf("%s carries no why; held = %#v, active = %v, waiting = %v",
						key, projected.Admission.Held, projected.Admission.Active, projected.Admission.Waiting)
				}
				want := api.WhyView{State: state, Reason: fixture.sentence}
				if *view.Why != want {
					t.Errorf("%s's why = %+v, want %+v", key, *view.Why, want)
				}
			})
		}
	}
}

// heldSentences serves under `blocked` exactly the words the ladder reads before
// dispatch_blocker (record.HeldBefore), and under `unlisted` every word of the ladder but
// dispatch_blocker and children_first, which no unlisted issue carries because every issue the
// ladder gives either stands in `admission.blocked`. Each of its words is a word of the ladder.
// A reorder of the ladder, or an entry that drifts from it, fails here.
func TestTheHeldSentenceTableServesTheWordsTheLadderSendsToEachArm(t *testing.T) {
	for word := range heldSentences {
		if !slices.Contains(record.HeldReasons, word) {
			t.Errorf("heldSentences has an entry for %q, which is no word of the ladder (record.HeldReasons)", word)
		}
	}
	for _, word := range record.HeldReasons {
		_, blocked := heldWordReason(api.WhyBlocked, record.HeldIssue{Reason: word})
		if want := record.HeldBefore(word, record.HeldDispatchBlocker); blocked != want {
			t.Errorf("heldSentences serves %s under blocked = %t, want %t: the ladder reads it before dispatch_blocker = %t",
				word, blocked, want, want)
		}
		_, unlisted := heldWordReason(api.WhyUnlisted, record.HeldIssue{Reason: word})
		if want := word != record.HeldDispatchBlocker && word != record.HeldChildrenFirst; unlisted != want {
			t.Errorf("heldSentences serves %s under unlisted = %t, want %t", word, unlisted, want)
		}
	}
}

// The sentence rule heldSentences states: every sentence says what is true of the issue now and
// what does not start it, and never promises what will start it. The phrases below are some of
// the ways such a promise has been written, so a sentence that carries one breaks the rule; the
// literal-text walk above (TestEveryEntryOfTheHeldSentenceTableIsServedThroughTheWholePass) is
// what holds each sentence to its approved text.
//
// The last phrase is a claim the rule forbids for another reason: "nothing of that tree runs" or
// "starts" is false while a done member's production check is open, since a check look is the
// one start an issue out of the workflow still runs. What is true of such a tree is that the
// daemon starts no issue of it, which is what the sentences say.
func TestNoHeldSentencePromisesAStart(t *testing.T) {
	for word, entry := range heldSentences {
		sentence := entry.sentence("LEGION-1")
		for _, promise := range []string{"starts when", "start when", "will start", "is started",
			"nothing of that tree", "nothing of the tree", "nothing of this tree"} {
			if strings.Contains(sentence, promise) {
				t.Errorf("the sentence of %s breaks the sentence rule (%q): %s", word, promise, sentence)
			}
		}
	}
}

// The control of the blocked arm: a child of a running tree under an open gate that a Dispatch
// link holds carries dispatch_blocker, which heldSentences does not serve under `blocked`, so
// its `why` is the link's own text (blockedReason) and not a held word's.
func TestABlockedIssueWhoseWordIsDispatchBlockerReadsItsLinks(t *testing.T) {
	store := projectionStore{
		issues: []record.Issue{treeRoot("LEGION-100", "todo", phase.Admitted),
			treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)},
		slots:      []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: projectedAt}},
		lifecycles: []string{"LEGION-100"},
		gates: map[string]map[record.DesignStep]record.DesignGate{
			"LEGION-100": openGate("LEGION-100", projectedAt.Add(-time.Hour))},
		mirror: []record.DispatchIssueRow{
			{Key: "LEGION-101", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900"}},
			{Key: "LEGION-900", Project: "LEGION", Status: "todo"},
		},
	}
	blockedWhy(t, store,
		api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldDispatchBlocker, HeldBy: []string{"LEGION-900"}},
		[]string{"LEGION-900"},
		api.WhyView{State: api.WhyBlocked, Reason: "It waits on LEGION-900, which is not done."})
}

// A child whose own blocked_by link holds it under an issue above it in its tree reads the
// cycle sentence whatever its held word, because nothing else in the state says that the issue
// it waits on is waiting for it; `admission.held` still names its held word beside it. The
// child is a tree of its own under a shut design gate, so its word is design_gate, which
// heldSentences serves under `blocked`; its Dispatch parent is a root under an open gate, which
// is what makes the children-first rule read the children under it and name the cycle.
func TestABlockedChildInACycleReadsTheCycleWhateverItsHeldWord(t *testing.T) {
	store := projectionStore{
		issues: []record.Issue{treeRoot("LEGION-100", "todo", phase.Admitted),
			treeRoot("LEGION-101", "todo", phase.Admitted)},
		gates: map[string]map[record.DesignStep]record.DesignGate{
			"LEGION-100": openGate("LEGION-100", projectedAt.Add(-time.Hour)),
			"LEGION-101": shutGate("LEGION-101")},
		mirror: []record.DispatchIssueRow{
			{Key: "LEGION-101", Project: "LEGION", Status: "todo", Parent: "LEGION-100", Blockers: []string{"LEGION-100"}},
		},
	}
	blockedWhy(t, store,
		api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldDesignGate, HeldBy: []string{"LEGION-101"}},
		[]string{"LEGION-100"},
		api.WhyView{State: api.WhyBlocked,
			Reason: "A blocked_by link holds it under LEGION-100, which stands above it in its own tree."})
}

// blockedWhy projects store through the whole pass and proves of the issue wantRow names that
// its held row is wantRow, that it stands in `admission.blocked` held by wantBlocked — so its
// `why` is the blocked arm's — and that its `why` is want.
func blockedWhy(t *testing.T, store projectionStore, wantRow api.HeldIssue, wantBlocked []string, want api.WhyView) {
	t.Helper()
	key := wantRow.Issue
	state, err := Project(context.Background(), nil, store, "LEGION", nil, nil, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	row := heldRow(t, state, key)
	if row.Issue != wantRow.Issue || row.Reason != wantRow.Reason || !slices.Equal(row.HeldBy, wantRow.HeldBy) {
		t.Fatalf("the held row of %s = %#v, want %#v", key, row, wantRow)
	}
	if !slices.ContainsFunc(state.Admission.Blocked, func(b api.BlockedIssue) bool {
		return b.Issue == key && slices.Equal(b.BlockedBy, wantBlocked)
	}) {
		t.Fatalf("%s stands in no blocked row held by %v, so its why is not the blocked arm's; blocked = %#v",
			key, wantBlocked, state.Admission.Blocked)
	}
	view := state.Issues[key]
	if view.Why == nil {
		t.Fatalf("%s carries no why", key)
	}
	if *view.Why != want {
		t.Errorf("%s's why = %+v, want %+v", key, *view.Why, want)
	}
}
