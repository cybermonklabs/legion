package text

import (
	"strings"
	"testing"
)

// An item reference is the place a node's parent names when that parent is an ask, a comment or
// a message, or a block of one (dispatch://INFRA-1253/spec R2): FormatItemBlockRef writes an ask,
// a comment and a message, and a part of an ask, a block of a comment and a block of a message,
// and CanonicalItemBlockRef reads each back as the reference it was written from, so a parent
// survives a round trip through markdown with no normalising. Each form the reader alone accepts
// is refused: an item a project document holds, a uuid prefix, an upper-case uuid, a pin, a
// needless or lower-case percent-encoding, a trailing character, and a document or a block of
// one, which CanonicalBlockRef reads instead.
func TestAnItemBlockReferenceReadsBackAsItIsWritten(t *testing.T) {
	const id = "4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f"
	readsBack := func(ref Ref, want string) {
		t.Helper()
		written, ok := FormatItemBlockRef(ref)
		if !ok || written != want {
			t.Fatalf("FormatItemBlockRef(%#v) = %q, %t; want %q", ref, written, ok, want)
		}
		read, ok := CanonicalItemBlockRef(written)
		if !ok || read != ref {
			t.Errorf("CanonicalItemBlockRef(%q) = %#v, %t; want %#v", written, read, ok, ref)
		}
		if _, document := CanonicalBlockRef(written); document {
			t.Errorf("CanonicalBlockRef(%q) reads it as a block of a document", written)
		}
	}
	for _, item := range []string{"ask", "comment", "message"} {
		readsBack(Ref{Kind: item, IssueKey: "CORE-1", ID: id}, "dispatch://CORE-1/"+item+"/"+id)
		for _, block := range []string{"b1", "q:1", "décision:1", "it@v2"} {
			readsBack(Ref{Kind: "block", Item: item, IssueKey: "CORE-1", ID: id, Block: block},
				"dispatch://CORE-1/"+item+"/"+id+"#b-"+EncodeBlockID(block))
		}
	}

	for _, value := range []string{
		"dispatch://CORE/artifact/handbook-md/comment/" + id + "#b-b1",
		"dispatch://CORE/artifact/handbook-md/ask/" + id + "#b-b1",
		"dispatch://CORE/artifact/handbook-md/comment/" + id,
		"dispatch://CORE-1/comment/11111111#b-b1",
		"dispatch://CORE-1/comment/11111111",
		"dispatch://CORE-1/comment/" + strings.ToUpper(id) + "#b-b1",
		"dispatch://CORE-1/comment/" + strings.ToUpper(id),
		"dispatch://CORE-1/comment/" + id + "#b-b1@v1",
		"dispatch://CORE-1/ask/" + id + "#b-b1@v2",
		"dispatch://CORE-1/comment/" + id + "@v2",
		"dispatch://CORE-1/comment/" + id + "#b-%62",
		"dispatch://CORE-1/comment/" + id + "#b-q%3a1",
		"dispatch://CORE-1/comment/" + id + "#b-b1 ",
		"dispatch://CORE-1/comment/" + id + "#b-b1.",
		"dispatch://CORE-1/comment/" + id + ".",
		" dispatch://CORE-1/comment/" + id + "#b-b1",
		"dispatch://CORE-1/spec#b-b1",
		"dispatch://CORE-1/spec",
		"dispatch://CORE-1",
		"dispatch://CORE-1/artifact/notes-md#b-b1",
		"dispatch://CORE-1/issue/" + id + "#b-b1",
	} {
		if ref, ok := CanonicalItemBlockRef(value); ok {
			t.Errorf("CanonicalItemBlockRef(%q) = %#v; want it refused", value, ref)
		}
	}

	for _, ref := range []Ref{
		{Kind: "block", Item: "comment", Project: "CORE", Document: "handbook-md", ID: id, Block: "b1"},
		{Kind: "comment", Project: "CORE", Document: "handbook-md", ID: id},
		{Kind: "block", Item: "comment", IssueKey: "CORE-1", ID: "11111111", Block: "b1"},
		{Kind: "comment", IssueKey: "CORE-1", ID: "11111111"},
		{Kind: "block", Item: "comment", IssueKey: "CORE-1", ID: strings.ToUpper(id), Block: "b1"},
		{Kind: "block", Item: "comment", IssueKey: "CORE-1", ID: id, Block: "b1", Pin: 1},
		{Kind: "block", Item: "comment", IssueKey: "CORE-1", ID: id},
		{Kind: "block", IssueKey: "CORE-1", ID: "spec", Block: "b1"},
		{Kind: "comment", Item: "comment", IssueKey: "CORE-1", ID: id},
		{Kind: "issue", IssueKey: "CORE-1", ID: "CORE-1"},
	} {
		if written, ok := FormatItemBlockRef(ref); ok {
			t.Errorf("FormatItemBlockRef(%#v) = %q; want no item reference", ref, written)
		}
	}
}
