package record

import (
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The line and the held list read the pass's one chain (LineFacts.Chains) and nothing else of where
// an issue stands, and that chain answers only for what it was read for. It holds the daemon's
// record of every key it read (Chains.Record), so a copy of the pass's records, or a part of them,
// reads from it what the records themselves read, and the line needs no check of which records it
// was handed. A record the read neither started from nor reached reads an unread chain at its own
// key (ChainUnread): the line starts nothing for it and the held list holds it chain_unknown,
// naming itself, as it holds every issue whose chain is unknown (R16 of dispatch://INFRA-3320), so
// no walk reads a chain of records it was not read for.
func TestTheLineReadsThePassChainOnlyOverItsOwnRecords(t *testing.T) {
	root := chainIssue("LEGION-900", "LEGION-900", "", phase.Admitted, "in_progress")
	child := chainIssue("LEGION-903", "LEGION-900", "LEGION-902", phase.Admitted, "todo")
	doneParent := chainIssue("LEGION-902", "LEGION-900", "LEGION-900", phase.Done, "done")
	unread := chainIssue("LEGION-905", "LEGION-900", "LEGION-900", phase.Admitted, "todo")
	facts := LineFacts{Chains: chainsOf([]Issue{root, child, doneParent}, copyOf(root, child, doneParent))}
	issues := []Issue{root, child}
	// judge is the line and the held list of records under facts, as a pass reads them.
	judge := func(records []Issue) ([]LineEntry, []HeldIssue) {
		line, dropped := Waiting(records, nil, facts)
		return line, HeldList(records, nil, line, dropped, facts)
	}
	_, want := judge(issues)
	if row := heldRow(want, child.Key); row.Reason != HeldAncestry || !slices.Equal(row.HeldBy, []string{root.Key}) {
		t.Fatalf("the held row of %s = %+v, want ancestry held by %s past the done parent %s: without the chain's walk this test proves nothing",
			child.Key, row, root.Key, doneParent.Key)
	}
	for _, tc := range []struct {
		name    string
		records []Issue
	}{
		{"a copy of the records", slices.Clone(issues)},
		{"a part of the records", issues[1:]},
	} {
		if _, got := judge(tc.records); !reflect.DeepEqual(heldRow(got, child.Key), heldRow(want, child.Key)) {
			t.Fatalf("%s: the held row of %s = %+v, want %+v, what the records the chain was read for read", tc.name, child.Key,
				heldRow(got, child.Key), heldRow(want, child.Key))
		}
	}
	line, rows := judge(append(slices.Clone(issues), unread))
	if row := heldRow(rows, unread.Key); inLine(line, unread.Key) || row.Reason != HeldChainUnknown || !slices.Equal(row.HeldBy, []string{unread.Key}) {
		t.Fatalf("%s, which the chain was not read for: in the line %t, held as %+v; want it out of the line and held chain_unknown at itself",
			unread.Key, inLine(line, unread.Key), row)
	}
}
