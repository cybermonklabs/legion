package text

import (
	"reflect"
	"strings"
	"testing"
)

// The three code contexts and their edges, read as whole bodies: each case writes one reference
// and says whether the body holds it in code. The shared table (TestExtractMatchesTheSharedTextTable)
// holds the rule to the dashboard's reader; these rows hold the edges of the rule itself, which
// no row of the table writes.
func TestExtractMarksTheReferencesACodeContextHolds(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		inCode bool
	}{
		{"a fenced block", "```\ndispatch://CORE-1\n```", true},
		{"a fenced block with an info string", "```sh\ndispatch://CORE-1\n```", true},
		{"a fence of tildes", "~~~\ndispatch://CORE-1\n~~~", true},
		{"a fence closed by a longer run", "```\ndispatch://CORE-1\n`````", true},
		{"a fence the shorter run does not close", "````\n``\ndispatch://CORE-1\n````", true},
		{"a fence nothing closes", "```\ndispatch://CORE-1", true},
		{"a fence indented three spaces", "   ```\ndispatch://CORE-1\n   ```", true},
		{"after a fence that closed", "```\nx\n```\ndispatch://CORE-1", false},
		{"a tilde fence an info string holds a backtick in", "~~~`sh\ndispatch://CORE-1\n~~~", true},
		{"a backtick fence an info string holds a backtick in", "```a`b\ndispatch://CORE-1\n```", false},
		{"an indented block after a blank line", "See:\n\n    dispatch://CORE-1\n", true},
		{"an indented block at the start of the body", "    dispatch://CORE-1\n", true},
		{"an indented block opened by a tab", "See:\n\n\tdispatch://CORE-1\n", true},
		{"a blank line inside an indented block", "See:\n\n    x\n\n    dispatch://CORE-1\n", true},
		{"four spaces continuing a paragraph", "See:\n    dispatch://CORE-1\n", false},
		{"a line after an indented block", "See:\n\n    x\n\ndispatch://CORE-1", false},
		{"a code span", "Run `dispatch://CORE-1` now.", true},
		{"a span of two backticks", "Run ``dispatch://CORE-1`` now.", true},
		{"a longer run closing no shorter span", "Run `` dispatch://CORE-1 ``` ``", true},
		{"a backtick run nothing closes", "Run `dispatch://CORE-1 now.", false},
		{"between two spans", "`a` dispatch://CORE-1 `b`", false},
		{"a span across a line", "Run `one\ndispatch://CORE-1` now.", true},
		{"a backtick inside a fenced block", "```\n`\n```\ndispatch://CORE-1", false},
		{"no code at all", "See dispatch://CORE-1 now.", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			located := ExtractAt(test.body, "https://dispatch.test")
			if len(located) != 1 || located[0].Kind != "issue" {
				t.Fatalf("%q read as %#v, want the one issue reference", test.body, located)
			}
			if located[0].InCode != test.inCode {
				t.Errorf("%q read InCode=%t, want %t", test.body, located[0].InCode, test.inCode)
			}
		})
	}
}

// A body that writes the same party inside code and outside it names it: the occurrence outside
// carries no mark, which is what keeps it a delivering party (api.commentPartiesOf).
func TestExtractMarksEachOccurrenceOnItsOwn(t *testing.T) {
	body := "Run `dispatch://role/reviewer` and ask dispatch://role/reviewer about it."
	located := ExtractAt(body, "https://dispatch.test")
	if len(located) != 2 {
		t.Fatalf("read %#v, want both occurrences", located)
	}
	if !located[0].InCode || located[1].InCode {
		t.Errorf("read InCode=%t then %t, want true then false", located[0].InCode, located[1].InCode)
	}
}

// A bare key and a `#id` carry the mark as an address does, the one reading of the body's code
// contexts serving every reader of it.
func TestMentionsMarksBareCandidatesInCode(t *testing.T) {
	resolver := mapResolver{
		issues:    map[string]bool{"CORE-1": true},
		questions: map[string]Ref{"q2": {Kind: "ask", IssueKey: "CORE-1", ID: "a2"}},
	}
	located, err := Mentions("`CORE-1` and #q2 and\n\n    CORE-1\n", "https://dispatch.test", resolver)
	if err != nil {
		t.Fatalf("Mentions() error: %v", err)
	}
	want := []bool{true, false, true}
	got := make([]bool, len(located))
	for index, item := range located {
		got[index] = item.InCode
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Mentions() read %#v with InCode %v, want %v", located, got, want)
	}
}

// Code ranges are read once per body, however many references stand in them, and they run in
// order so a reference's offset can be looked up in them.
func TestCodeRangesAreOrderedAndDisjoint(t *testing.T) {
	body := "`a` and `b`\n\n```\nfenced `x`\n```\n\n    indented `y`\n\nlast `z`"
	ranges := codeRanges(body)
	if len(ranges) != 5 {
		t.Fatalf("codeRanges(%q) = %#v, want five", body, ranges)
	}
	for index, candidate := range ranges {
		if candidate.start >= candidate.end {
			t.Errorf("range %d is empty: %#v", index, candidate)
		}
		if index > 0 && ranges[index-1].end > candidate.start {
			t.Errorf("range %d starts inside range %d: %#v", index, index-1, ranges)
		}
	}
	// The fenced block's own backticks open no span: the block took them.
	if got := body[ranges[2].start:ranges[2].end]; got != "```\nfenced `x`\n```\n" {
		t.Errorf("the fenced range is %q", got)
	}
	if got := body[ranges[3].start:ranges[3].end]; !strings.HasPrefix(got, "    indented") {
		t.Errorf("the indented range is %q", got)
	}
}
