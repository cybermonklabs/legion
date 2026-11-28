package api

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// recordField is one field of a query's record as docs' IssueTree.QueryRecord writes it: its byte
// length, one space, then its bytes.
func recordField(field string) string { return strconv.Itoa(len(field)) + " " + field }

// The spec query takes the component and interface blocks an issue proposes (R22), in the record's
// one layout, and QueryDefinitionVersion stays 1: a store that holds no component block reads the
// record TestTheRecordOfOneNodeIsItsLayoutByteForByte pins, byte for byte, from the tree that test
// writes (pinnedTree), as every record an approval before this change covered was built; and a
// component block written beside the requirement stands after it in the walk's order as one more
// node of the same seven fields. No approval of a version written under one definition reads as
// one of another, so the version does not rise.
func TestTheSpecQueryTakesComponentsUnderOneDefinition(t *testing.T) {
	if graphstore.QueryDefinitionVersion != 1 {
		t.Fatalf("QueryDefinitionVersion is %d, want 1: adding the component to the spec query changes no record's layout", graphstore.QueryDefinitionVersion)
	}
	database := storetest.Open(t)
	rig := &digestRig{database: database}
	pinned := pinnedIssue(t)
	component := pinned + "/spec#billing"
	rig.state(t, pinnedTree(pinned)...)
	read := func() docs.QueryRecord {
		t.Helper()
		record, err := docs.ReadQueryRecord(context.Background(), database.Pool, pinned, graphstore.QuerySpec, refs.TreeCut{})
		if err != nil {
			t.Fatalf("read the spec record of %s: %v", pinned, err)
		}
		return record
	}
	if before := read(); string(before.Record) != pinnedRecord || before.Digest != pinnedDigest {
		t.Fatalf("with no component block the spec record of %s reads %q (%s), want the pinned record %q (%s)", pinned, before.Record, before.Digest, pinnedRecord, pinnedDigest)
	}

	const text = ":::component{#billing name=\"Billing\" kind=\"interface\" joins=\"bank shop\"}\nIt bills.\n:::\n"
	rig.state(t,
		blockTuple(component, graphstore.TuplePartOf, graphstore.KindIssue, pinned, true),
		blockValue(component, refs.AttributeRank, "1W"),
		blockValue(component, graphstore.AttributeText, text),
		blockValue(component, "joins", "bank shop"),
		blockValue(component, graphstore.AttributeState, graphstore.StateLive),
		blockValue(component, graphstore.AttributeVisibility, graphstore.VisibilityProject),
	)
	values := recordField("joins") + recordField("bank shop") + recordField(graphstore.AttributeState) + recordField(graphstore.StateLive) +
		recordField(graphstore.AttributeVisibility) + recordField(graphstore.VisibilityProject)
	want := pinnedRecord + recordField(refs.BlockNodeKind+":"+component) + recordField("component") + recordField(graphstore.KindIssue+":"+pinned) +
		recordField("1W") + recordField(values) + recordField(text) + recordField(recordField(graphstore.TuplePartOf)+recordField(graphstore.KindIssue+":"+pinned))
	after := read()
	if string(after.Record) != want {
		t.Errorf("with a component block the spec record of %s reads\n%q\nwant\n%q", pinned, after.Record, want)
	}
	if after.Digest == pinnedDigest || !strings.Contains(string(after.Record), "component") {
		t.Errorf("the component block leaves the spec digest at %s", after.Digest)
	}
}

// pinnedIssue is the issue of the tree whose spec record pinnedRecord pins, read from the record's
// first field, the node of its requirement (`block:<issue>/spec#r1`), so this test writes its tree
// under the issue that record names.
func pinnedIssue(t *testing.T) string {
	t.Helper()
	length, rest, _ := strings.Cut(pinnedRecord, " ")
	size, err := strconv.Atoi(length)
	if err != nil || size > len(rest) {
		t.Fatalf("the pinned record %q opens with no field", pinnedRecord)
	}
	node, isBlock := strings.CutPrefix(rest[:size], refs.BlockNodeKind+":")
	issue, _, inIssue := strings.Cut(node, "/")
	if !isBlock || !inIssue {
		t.Fatalf("the pinned record's first field is %q, want the node of a block of an issue", rest[:size])
	}
	return issue
}
