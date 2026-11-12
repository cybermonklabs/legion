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
// The fix C2 asks for is the ladder reading whether the root's record exists for the two words
// that speak of that record, `root_awaiting_person` and `design_gate`; every reachable case
// keeps the word it reads today, because a root the pass's own records leave out is read by
// record.ReadTreeRoots.
//
// The architect of dispatch://INFRA-2964 ruled on 2026-11-12T07:23Z that C2 binds, after this
// round measured the word: "A requirement is read as written; that the state is unreachable
// today changes what it costs to meet, not whether it is required."
func TestTheIssueWhoseTreeRootTheRecordDoesNotHoldIsUnclassified(t *testing.T) {
	// The issue names no Dispatch parent: the parent chain is read from the store
	// (record.ReadParents), and this fixture is about the tree root alone.
	member := lineIssue("LEGION-301", "LEGION-300", "todo", phase.Admitted, "LEGION-301")

	for _, tc := range []struct {
		name  string
		store projectionStore
	}{
		{
			name:  "no record and no gate row for the tree it names",
			store: projectionStore{issues: []record.Issue{member}},
		},
		{
			// The gate is a row of its own, so the word must not turn on whether one was
			// written for a tree whose root the record does not hold.
			name: "a gate row standing open for the tree it names",
			store: projectionStore{issues: []record.Issue{member},
				gates: map[string]map[record.DesignStep]record.DesignGate{
					"LEGION-300": openGate("LEGION-300", projectedAt.Add(-time.Hour))}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := wordFor(t, tc.store, "LEGION-301")
			want := api.HeldIssue{Issue: "LEGION-301", Reason: record.HeldUnclassified}
			if got.Reason != want.Reason || got.HeldBy != nil {
				t.Fatalf("the held row of %s = %#v, want %#v: C2 reads an issue whose tree root the record does not hold as unclassified, held by nobody",
					want.Issue, got, want)
			}
		})
	}
}
