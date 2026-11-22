package pmdoc

import (
	"slices"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// RecordChildEditsForTest reports each call a write makes to a fragment's children, in the order
// ygo receives them, and its restore puts back the recorder it found. Replacing the middle one of
// three paragraphs with a heading reads the three; reads the first and the last paragraph's
// children to choose which end to keep first, and each again as it keeps it; deletes the middle
// paragraph at index 1; inserts the heading's text into the heading the write builds; and inserts
// the heading at index 1. Replacing the heading with a paragraph again is the same, with the
// paragraph's text. A recorder set over another takes the write's calls while it stands; once it
// is restored the first one takes the next write's, and once that one is restored a write reports
// nothing.
func TestRecordChildEditsForTestReportsEachCallOfAWrite(t *testing.T) {
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	paragraphs := repeatedBlocks(t, "a\n\n", 3)
	writeUpdate(t, doc, fragment, paragraphs)
	withHeading, err := ParseRendering("a\n\n# b\n\na\n")
	if err != nil {
		t.Fatalf("parse the paragraphs around a heading: %v", err)
	}
	want := []ChildEdit{
		{Kind: ChildRead, Count: 3},
		{Kind: ChildRead, Count: 1},
		{Kind: ChildRead, Count: 1},
		{Kind: ChildRead, Count: 1},
		{Kind: ChildRead, Count: 1},
		{Kind: ChildDelete, Index: 1, Count: 1},
		{Kind: ChildInsert, Index: 0, Count: 1},
		{Kind: ChildInsert, Index: 1, Count: 1},
	}

	var outer, inner []ChildEdit
	restoreOuter := RecordChildEditsForTest(func(edit ChildEdit) { outer = append(outer, edit) })
	defer restoreOuter()
	restoreInner := RecordChildEditsForTest(func(edit ChildEdit) { inner = append(inner, edit) })
	writeUpdate(t, doc, fragment, withHeading)
	restoreInner()
	if !slices.Equal(inner, want) || len(outer) != 0 {
		t.Fatalf("replacing the middle paragraph with a heading reported %v to the inner recorder and %v to the outer, want %v and none", inner, outer, want)
	}

	writeUpdate(t, doc, fragment, paragraphs)
	restoreOuter()
	if !slices.Equal(outer, want) {
		t.Fatalf("replacing the heading with a paragraph reported %v to the outer recorder, want %v", outer, want)
	}
	writeUpdate(t, doc, fragment, withHeading)
	if len(outer) != len(want) || len(inner) != len(want) {
		t.Fatalf("a write after both recorders were restored reported to one of them: outer %v, inner %v", outer, inner)
	}
}
