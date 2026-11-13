package projection

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The third fill of `unlisted`: the sentence an issue reads from the cause the record carries
// for it (C1 of dispatch://INFRA-3107, record.HeldReason). The three cases below are the two
// ends of the fill and one word in the middle — a member of a closed tree, which reads the
// sentence of the word the ladder gave it with the root that holds it named; a member whose
// word is `unclassified`, which no sentence of the fill answers and which reads the fallback;
// and a member of a tree whose root waits on a person, which names that root.
//
// Each is read off a projected state through the whole pass, never from the renderer alone, so
// the row the pass publishes and the sentence it serves are proved of one state: a fixture
// whose issue stops taking the word this test names fails here rather than reading a sentence
// of some other word's.

// heldWhy is the held row and the rendered answer one projection gives one issue: the word for
// why nothing of it runs (admission.held, heldRow) and the sentence a reader meets on the
// issue's own view (api.Issue.Why). Both come from one pass, because the point of this fill is
// that the two agree. It fails where the issue carries no answer at all or stands in no held
// row, which are the other ways the fill can go wrong — a sentence that renders nothing is
// served as no `why` (whyStates).
func heldWhy(t *testing.T, store projectionStore, key string) (api.HeldIssue, api.WhyView) {
	t.Helper()
	state, err := Project(context.Background(), nil, store, "LEGION", nil, nil, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	view, projected := state.Issues[key]
	if !projected {
		t.Fatalf("%s is absent from the projected state, so its answer proves nothing", key)
	}
	if view.Why == nil {
		t.Fatalf("%s carries no why; held = %#v, active = %v, waiting = %v",
			key, state.Admission.Held, state.Admission.Active, state.Admission.Waiting)
	}
	return heldRow(t, state, key), *view.Why
}

// A member of a tree a person closed reads the sentence of `tree_closed` with the root that
// holds it named: the root is what the row carries beside the word (record.HeldIssue.HeldBy),
// and the reader asking why nothing of this issue runs is asking which issue to look at.
func TestTheUnlistedSentenceOfAClosedTreesMemberNamesItsRoot(t *testing.T) {
	root := treeRoot("LEGION-100", "done", phase.Done)
	member := treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)
	store := projectionStore{issues: []record.Issue{root, member}}

	row, why := heldWhy(t, store, "LEGION-101")
	if row.Reason != record.HeldTreeClosed || len(row.HeldBy) != 1 || row.HeldBy[0] != "LEGION-100" {
		t.Fatalf("the held row of LEGION-101 = %#v, want tree_closed held by LEGION-100", row)
	}
	want := api.WhyView{State: api.WhyUnlisted,
		Reason: "Its tree is closed: LEGION-100 has finished, or lingers after leaving the workflow, " +
			"and nothing of that tree runs again until a re-admission opens it."}
	if why != want {
		t.Errorf("LEGION-101's why = %+v, want %+v", why, want)
	}
}

// A member whose word is `unclassified` reads the fallback: the ladder's last word says that no
// rule of the held list names what the issue waits for, and the sentence says that and no more.
// The fixture is the one the word is proved over elsewhere (held_triaged_root_test.go): a tree
// whose root stands out of the workflow at triage, at phase done and under a gate a person
// approved, so no word above `unclassified` is true of its member.
func TestTheUnlistedSentenceOfAnUnclassifiedIssueIsTheFallback(t *testing.T) {
	root := treeRoot("LEGION-200", "triage", phase.Done)
	member := treeMember("LEGION-201", "LEGION-200", "LEGION-200", "todo", phase.Admitted)
	store := projectionStore{
		issues: []record.Issue{root, member},
		gates:  map[string]map[record.DesignStep]record.DesignGate{"LEGION-200": openGate("LEGION-200", projectedAt.Add(-time.Hour))},
	}

	row, why := heldWhy(t, store, "LEGION-201")
	if row.Reason != record.HeldUnclassified || len(row.HeldBy) != 0 {
		t.Fatalf("the held row of LEGION-201 = %#v, want unclassified holding nothing", row)
	}
	want := api.WhyView{State: api.WhyUnlisted,
		Reason: "The daemon holds it in the workflow, and it stands in neither the issues at work nor the waiting line, " +
			"so no slot has been offered to it. No rule of the held list says what it waits for."}
	if why != want {
		t.Errorf("LEGION-201's why = %+v, want %+v", why, want)
	}
}

// A member of a tree whose root handed work to a person reads the sentence of
// `root_awaiting_person` with that root named: its own start is the root's resume. The root
// holds the slot its wait keeps and its gate stands open, so the word its member reads is this
// wait and not the gate, and the member itself stands in no list that places an issue.
func TestTheUnlistedSentenceOfAMemberUnderARootAwaitingAPersonNamesThatRoot(t *testing.T) {
	root := treeRoot("LEGION-300", "in_progress", phase.AwaitingPerson)
	root.Awaiting = &record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: 5}}
	member := treeMember("LEGION-301", "LEGION-300", "LEGION-300", "todo", phase.Admitted)
	store := projectionStore{
		issues:     []record.Issue{root, member},
		slots:      []record.Slot{{Issue: "LEGION-300", Index: 0, AdmittedAt: projectedAt}},
		lifecycles: []string{"LEGION-300"},
		gates: map[string]map[record.DesignStep]record.DesignGate{
			"LEGION-300": openGate("LEGION-300", projectedAt.Add(-time.Hour))},
	}

	row, why := heldWhy(t, store, "LEGION-301")
	if row.Reason != record.HeldRootAwaitingPerson || len(row.HeldBy) != 1 || row.HeldBy[0] != "LEGION-300" {
		t.Fatalf("the held row of LEGION-301 = %#v, want root_awaiting_person held by LEGION-300", row)
	}
	want := api.WhyView{State: api.WhyUnlisted,
		Reason: "Its tree's root waits on a person: LEGION-300 waits for an answer or for a review of its spec, " +
			"and the issues of the tree start when that root resumes."}
	if why != want {
		t.Errorf("LEGION-301's why = %+v, want %+v", why, want)
	}
}

// An issue that stands in `admission.blocked` and in a held row reads one cause in both: where
// its held word ranks above dispatch_blocker in the ladder (record.HeldBefore), `why` names that
// word's sentence, because closing the link or child it waits on frees nothing; where the word
// is dispatch_blocker itself, `why` names the link. The state word is `blocked` in every case,
// since every issue here stands in that list. The first case is a member of a closed tree a
// Dispatch link holds, whose `why` would otherwise say that it waits on the link; the second is
// a root whose label is off, which the children-first rule holds on the one open Dispatch child
// under it, whose `why` would otherwise name that child; the third is the control, a child of a
// running tree under an open gate that the same link as the first holds.
func TestABlockedIssueWhoseHeldWordRanksAboveTheLinksReadsThatWord(t *testing.T) {
	link := []record.DispatchIssueRow{
		{Key: "LEGION-101", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900"}},
		{Key: "LEGION-900", Project: "LEGION", Status: "todo"},
	}
	unlabelled := treeRoot("LEGION-100", "todo", phase.Admitted)
	unlabelled.HandedOver = false
	for _, tc := range []struct {
		name        string
		store       projectionStore
		wantRow     api.HeldIssue
		wantBlocked []string
		want        api.WhyView
	}{
		{
			name: "a member of a closed tree a link holds",
			store: projectionStore{
				issues: []record.Issue{treeRoot("LEGION-100", "done", phase.Done),
					treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)},
				mirror: link,
			},
			wantRow:     api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldTreeClosed, HeldBy: []string{"LEGION-100"}},
			wantBlocked: []string{"LEGION-900"},
			want: api.WhyView{State: api.WhyBlocked,
				Reason: "Its tree is closed: LEGION-100 has finished, or lingers after leaving the workflow, " +
					"and nothing of that tree runs again until a re-admission opens it."},
		},
		{
			name: "a root whose label is off whose own Dispatch child is open",
			store: projectionStore{
				issues: []record.Issue{unlabelled},
				gates: map[string]map[record.DesignStep]record.DesignGate{
					"LEGION-100": openGate("LEGION-100", projectedAt.Add(-time.Hour))},
				mirror: []record.DispatchIssueRow{
					{Key: "LEGION-101", Project: "LEGION", Status: "todo", Parent: "LEGION-100"},
				},
			},
			wantRow:     api.HeldIssue{Issue: "LEGION-100", Reason: record.HeldLabelOff},
			wantBlocked: []string{"LEGION-101"},
			want: api.WhyView{State: api.WhyBlocked,
				Reason: "It is the root of its own tree and its record says the legion label is off, " +
					"so nothing hands it to the daemon."},
		},
		{
			name: "a child of a running tree under an open gate a link holds",
			store: projectionStore{
				issues: []record.Issue{treeRoot("LEGION-100", "todo", phase.Admitted),
					treeMember("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted)},
				slots:      []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: projectedAt}},
				lifecycles: []string{"LEGION-100"},
				gates: map[string]map[record.DesignStep]record.DesignGate{
					"LEGION-100": openGate("LEGION-100", projectedAt.Add(-time.Hour))},
				mirror: link,
			},
			wantRow:     api.HeldIssue{Issue: "LEGION-101", Reason: record.HeldDispatchBlocker, HeldBy: []string{"LEGION-900"}},
			wantBlocked: []string{"LEGION-900"},
			want:        api.WhyView{State: api.WhyBlocked, Reason: "It waits on LEGION-900, which is not done."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blockedWhy(t, tc.store, tc.wantRow, tc.wantBlocked, tc.want)
		})
	}
}

// A child whose own blocked_by link holds it under an issue above it in its tree reads the
// cycle sentence whatever its held word, because nothing else in the state says that the issue
// it waits on is waiting for it; `admission.held` still names its held word beside it. The
// child is a tree of its own under a shut design gate, so its word is design_gate, which the
// ladder reads before dispatch_blocker; its Dispatch parent is a root under an open gate, which
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
