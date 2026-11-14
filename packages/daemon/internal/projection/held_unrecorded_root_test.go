package projection

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// C2 of dispatch://INFRA-3107 names two kinds of issue that read `unclassified`: the issues at
// `accepting`, which hold no slot and owe their worker no start, and "an issue whose tree root
// the record does not hold". The second kind reads `design_gate` instead, with `heldBy` naming
// the key no record holds, because record.treeRoots names only the issues that are their own
// tree root, so ReadTreeGates builds no gate for a root the record does not hold and
// TreeGate.Open() over no rows is false, which is the ladder's design_gate arm
// (record/held.go). The word is wrong twice over: it tells a reader of admission.held to wait
// for a person's approval of a document of an issue that does not exist, and it keeps the
// daemon's own log line for an unclassified issue — the one thing that names a record nothing
// accounts for — from ever being written.
//
// The fix is a guard on the two words that speak of the root's own record,
// `root_awaiting_person` and `design_gate`, and not a clause that answers `unclassified` for
// every issue whose tree root the record does not hold: C1's words are an order, and
// `dispatch_blocker`, `children_first` and `accepted_ancestor` all stand above `unclassified`
// in it, so an issue with an unrecorded root that a Dispatch link holds reads
// `dispatch_blocker`. C2's own opening says the same: the reason is `unclassified` "where the
// record holds an issue in the workflow that no word above is true of". Every reachable case
// keeps the word it reads today, because a root the pass's own records leave out is read by
// record.ReadTreeRoots.
//
// The architect of dispatch://INFRA-2964 ruled on 2026-11-12T07:23Z that C2 binds, after this
// round measured the word: "A requirement is read as written; that the state is unreachable
// today changes what it costs to meet, not whether it is required." It ruled the form of the
// fix, and these rows, at 07:28Z.
func TestTheIssueWhoseTreeRootTheRecordDoesNotHoldIsUnclassified(t *testing.T) {
	// Each issue names no Dispatch parent: the parent chain is read from the store
	// (record.ReadParents), and these rows are about the tree root alone.
	unrecorded := lineIssue("LEGION-301", "LEGION-300", "todo", phase.Admitted, "LEGION-301")
	gatedRoot := lineIssue("LEGION-310", "LEGION-310", "todo", phase.Admitted, "LEGION-310")
	gatedMember := lineIssue("LEGION-311", "LEGION-310", "todo", phase.Admitted, "LEGION-311")
	gatedMember.Parent = record.ParentOf("LEGION-310")

	for _, tc := range []struct {
		name  string
		store projectionStore
		issue string
		want  api.HeldIssue
	}{
		{
			name:  "an unrecorded tree root and nothing else true of the issue",
			store: projectionStore{issues: []record.Issue{unrecorded}},
			issue: "LEGION-301",
			want:  api.HeldIssue{Issue: "LEGION-301", Reason: record.HeldUnclassified},
		},
		{
			// The gate is a row of its own, so the word must not turn on whether one was
			// written for a tree whose root the record does not hold.
			name: "an unrecorded tree root with a gate row standing open",
			store: projectionStore{issues: []record.Issue{unrecorded},
				gates: map[string]map[record.DesignStep]record.DesignGate{
					"LEGION-300": openGate("LEGION-300", projectedAt.Add(-time.Hour))}},
			issue: "LEGION-301",
			want:  api.HeldIssue{Issue: "LEGION-301", Reason: record.HeldUnclassified},
		},
		{
			// The ladder's order is not jumped: a word above `unclassified` that is true of
			// the issue itself is still the word, whatever the record holds of its tree root.
			name: "an unrecorded tree root and an unfinished Dispatch link",
			store: projectionStore{issues: []record.Issue{unrecorded},
				mirror: []record.DispatchIssueRow{
					{Key: "LEGION-301", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900"}},
					{Key: "LEGION-900", Project: "LEGION", Status: "todo"},
				}},
			issue: "LEGION-301",
			want:  api.HeldIssue{Issue: "LEGION-301", Reason: record.HeldDispatchBlocker, HeldBy: []string{"LEGION-900"}},
		},
		{
			// The reachable case the guard must leave alone: a tree root the record holds,
			// under a gate a person has not approved at its current version.
			name: "a recorded tree root under a shut gate",
			store: projectionStore{issues: []record.Issue{gatedRoot, gatedMember},
				slots:      []record.Slot{{Issue: "LEGION-310", Index: 0, AdmittedAt: projectedAt}},
				lifecycles: []string{"LEGION-310"},
				gates:      map[string]map[record.DesignStep]record.DesignGate{"LEGION-310": shutGate("LEGION-310")}},
			issue: "LEGION-311",
			want:  api.HeldIssue{Issue: "LEGION-311", Reason: record.HeldDesignGate, HeldBy: []string{"LEGION-310"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := wordFor(t, tc.store, tc.issue)
			if got.Reason != tc.want.Reason || !equalKeys(got.HeldBy, tc.want.HeldBy) {
				t.Fatalf("the held row of %s = %#v, want %#v", tc.issue, got, tc.want)
			}
		})
	}
}

// equalKeys is whether two lists of issue keys are the same list, an empty one and a nil one
// counting as one: the row's own wire form is what says whether `heldBy` is carried, and that is
// read by the test of C1's shapes rather than here.
func equalKeys(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
