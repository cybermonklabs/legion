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

// heldFixture is the store one entry of heldSentences is walked over, and the held row the
// projected state must give the issue it names.
type heldFixture struct {
	store projectionStore
	row   api.HeldIssue
}

// The sentence table of `why` (heldSentences) is walked entry by entry through the whole pass:
// for each word and each state the table serves it under, a fixture whose issue takes that word
// is projected, and the issue must carry that word in its held row, stand in `admission.blocked`
// under `blocked` and outside it under `unlisted`, and read that state with the table's sentence
// for that word and state over its row's heldBy. Each fixture is one this package already proves
// the word of (held_test.go, held_triaged_root_test.go); a blocked one adds a Dispatch link that
// holds the issue, or, for a root whose label is off, an open Dispatch child under it, which the
// children-first rule holds it for.
//
// The walk is read off a projected state, never from the renderer alone, so the row the pass
// publishes and the sentence it serves are proved of one state. An entry no fixture walks fails
// the test: an entry added to the table is given a fixture here. at_work is the one exception,
// because no issue reads that word (record.HeldAtWork), so no fixture can stand for it.
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
	// An issue beneath an ancestor the daemon has accepted, which stands in merging and holds a
	// slot, in a running tree under an open gate.
	acceptedAncestor := projectionStore{
		issues: []record.Issue{treeRoot("LEGION-100", "todo", phase.Admitted),
			treeMember("LEGION-101", "LEGION-100", "LEGION-100", "needs_review", phase.Merging),
			treeMember("LEGION-102", "LEGION-100", "LEGION-101", "todo", phase.Admitted)},
		slots: []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: projectedAt},
			{Issue: "LEGION-101", Index: 1, AdmittedAt: projectedAt}},
		lifecycles: []string{"LEGION-100"},
		gates:      map[string]map[record.DesignStep]record.DesignGate{"LEGION-100": openGate("LEGION-100", opened)},
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
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldTreeClosed, HeldBy: byRoot}},
		{record.HeldTreeClosed, api.WhyUnlisted}: {closedTree,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldTreeClosed, HeldBy: byRoot}},
		{record.HeldTreeParked, api.WhyBlocked}: {linked(parkedTree),
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldTreeParked, HeldBy: byRoot}},
		{record.HeldTreeParked, api.WhyUnlisted}: {parkedTree,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldTreeParked, HeldBy: byRoot}},
		{record.HeldLabelOff, api.WhyBlocked}: {labelOffWithChild,
			api.HeldIssue{Issue: "LEGION-100", Reason: record.HeldLabelOff}},
		{record.HeldLabelOff, api.WhyUnlisted}: {labelOff,
			api.HeldIssue{Issue: "LEGION-100", Reason: record.HeldLabelOff}},
		{record.HeldAncestry, api.WhyBlocked}: {ancestryWithChild,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldAncestry, HeldBy: byRoot}},
		{record.HeldAncestry, api.WhyUnlisted}: {ancestry,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldAncestry, HeldBy: byRoot}},
		{record.HeldRootAwaitingPerson, api.WhyBlocked}: {linked(awaitingPerson),
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldRootAwaitingPerson, HeldBy: byRoot}},
		{record.HeldRootAwaitingPerson, api.WhyUnlisted}: {awaitingPerson,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldRootAwaitingPerson, HeldBy: byRoot}},
		{record.HeldDesignGate, api.WhyBlocked}: {linked(shutTree),
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldDesignGate, HeldBy: byRoot}},
		{record.HeldDesignGate, api.WhyUnlisted}: {shutTree,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldDesignGate, HeldBy: byRoot}},
		{record.HeldAcceptedAncestor, api.WhyUnlisted}: {acceptedAncestor,
			api.HeldIssue{Issue: "LEGION-102", Reason: record.HeldAcceptedAncestor, HeldBy: []string{"LEGION-101"}}},
		{record.HeldUnclassified, api.WhyUnlisted}: {unclassified,
			api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldUnclassified}},
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
				want := api.WhyView{State: state, Reason: entry.sentence(englishIssues(row.HeldBy))}
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
// what does not start it, and never promises what will start it. The phrases below are the ways
// such a promise has been written, so a sentence that carries one breaks the rule.
func TestNoHeldSentencePromisesAStart(t *testing.T) {
	for word, entry := range heldSentences {
		sentence := entry.sentence("LEGION-1")
		for _, promise := range []string{"starts when", "start when", "will start", "is started"} {
			if strings.Contains(sentence, promise) {
				t.Errorf("the sentence of %s promises a start (%q): %s", word, promise, sentence)
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
