package docs

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A refused find's refusal names the find that covers the very text the find named, selects as it
// does and is taken there, which the caller sends in its place, with the occurrence at which it
// matches that text, so the edit removes what the refused one would have. The table is each shape
// of a find that spells a link with another URL or title - the whole link, untitled or titled;
// part of it; the whole link with text before or after it; part of it with text before or after
// it; a bare link, alone and in a longer find - by a delete and a replace (with `NEW`); then each
// shape of an edit that removes more than its find matched, or could: a delete whose find's list
// marker selects an item, which removes the whole item; a replace whose find's marker selects an
// item, which writes over what the find matched alone; a delete of a paragraph's whole text, which
// removes the paragraph and no text besides; and a replace whose with is blocks, which drops the
// whitespace beside its find; then each shape of a replace's find across a span's edge: a link's,
// a code span's, a struck span's, and a bold or an italic span's end and start, cut inside a word
// or at a space, the find spelling the span's markup over the part it covers or none. A link the
// edit removes whole must be spelled as it is wherever the find spells it, since the edit would
// remove a link another writer may have changed, so a find that spells such a link otherwise is
// refused, even one over part of the link whose edit removes the rest of it. An edit that removes
// only part of a link changes only the text its find named: its row reads the document it leaves,
// where exactly that text is deleted or replaced and the rest of the link keeps the document's URL
// and title. Each refused row reads the refusal's whole text, then sends the operation again with
// the find the refusal names in the find's place (ErrFindCrossesSpan.Own), with the occurrence it
// names, and reads the document that leaves: exactly what the refused edit would have removed is
// deleted or replaced - for a delete whose marker selected an item, that whole item - and the rest
// is as it was. A find is named only where one of its matches is that text, selected as the
// refused find selected it, and it is taken there. Where none is, the refusal names none: to move
// the space out of a bold or italic span where the part the find leaves would meet the written
// text at a space, which markdown cannot write the span's delimiters beside; for a struck span, to
// replace inside it and outside it in two operations
// (TestARefusalNamingNoFindGivesTheAdviceOfTheSpanThatStopsIt holds the spans that stop the find
// written from the document's text). The rows with a document of their own show where the find
// named matches at another occurrence than the refused find did: the text stands inside another
// word before the link, or bare before it, or in a second link, or in a link before it with no bold
// run; the link holds a bold run; or the selected item's link stands in a paragraph before the list
// too, or in the item before it, where the find named counts only the items it selects. One more
// is a link whose text starts on a space, which a with of blocks drops before the find.
func TestARefusedFindNamesTheFindThatKeepsItsRange(t *testing.T) {
	t.Parallel()
	const (
		plain   = "I wrote [one two](https://a.example/1) today.\n"
		titled  = "I wrote [one two](https://a.example/1 \"Spec\") today.\n"
		bare    = "Read https://a.example/1 today.\n"
		list    = "- [one two](https://a.example/1) and more\n- second\n"
		whole   = "[one two](https://a.example/1) and more\n\nAfter.\n"
		spaced  = "I wrote [one ](https://a.example/1) today.\n"
		another = "spells the link \"one two\" with another URL or title than the one it has; "
		crosses = "crosses the edge of the link \"one two\": it covers text inside the link and text outside it, and " +
			"it spells the link with another URL or title than the one it has; "
		replaces = ", which then replaces the markup too, so with decides that formatting"
		inside   = ", which writes with inside the link"
	)
	occurrence := func(index int) *int { return &index }
	for _, test := range []struct {
		shape, name, markdown, op, find string
		occurrence                      *int
		// with is what a replace writes, NEW where the row names none.
		with string
		// takes is the document a find that is taken leaves; refusal is the refusal's whole text
		// after the operation's index, and leaves the document the find it names leaves, "" where
		// it names none.
		takes, refusal, leaves string
	}{
		{
			shape: "the whole link", name: "spelled with another URL", markdown: plain, op: "delete", find: "[one two](https://b.example/2)",
			refusal: `find "[one two](https://b.example/2)" ` + another + `write the link as it is in find ("[one two](https://a.example/1)")`,
			leaves:  "I wrote  today.\n",
		},
		{
			shape: "the whole link", name: "spelled with another URL", markdown: plain, op: "replace", find: "[one two](https://b.example/2)",
			refusal: `find "[one two](https://b.example/2)" ` + another + `write the link as it is in find ("[one two](https://a.example/1)")` + replaces,
			leaves:  "I wrote NEW today.\n",
		},
		{
			shape: "the whole link", name: "spelled with another URL under its own title", markdown: titled, op: "delete", find: `[one two](https://b.example/2 "Spec")`,
			refusal: `find "[one two](https://b.example/2 \"Spec\")" ` + another + `write the link as it is in find ("[one two](https://a.example/1 \"Spec\")")`,
			leaves:  "I wrote  today.\n",
		},
		{
			shape: "the whole link", name: "spelled with another URL under its own title", markdown: titled, op: "replace", find: `[one two](https://b.example/2 "Spec")`,
			refusal: `find "[one two](https://b.example/2 \"Spec\")" ` + another + `write the link as it is in find ("[one two](https://a.example/1 \"Spec\")")` + replaces,
			leaves:  "I wrote NEW today.\n",
		},
		{
			shape: "the whole link", name: "spelled with another title", markdown: titled, op: "delete", find: `[one two](https://a.example/1 "Draft")`,
			refusal: `find "[one two](https://a.example/1 \"Draft\")" ` + another + `write the link as it is in find ("[one two](https://a.example/1 \"Spec\")")`,
			leaves:  "I wrote  today.\n",
		},
		{
			shape: "the whole link", name: "spelled with another title", markdown: titled, op: "replace", find: `[one two](https://a.example/1 "Draft")`,
			refusal: `find "[one two](https://a.example/1 \"Draft\")" ` + another + `write the link as it is in find ("[one two](https://a.example/1 \"Spec\")")` + replaces,
			leaves:  "I wrote NEW today.\n",
		},
		{
			// The find's occurrence counts each place `one two` stands under a link, the first
			// link and then this one; the link as it is, spelling its bold run, matches only this
			// one, so it is named with occurrence 0.
			shape: "the whole link", name: "holding a bold run, after a link with none", markdown: "I wrote [one two](https://a.example/1) and [one **two**](https://a.example/1) today.\n",
			op: "delete", find: "[one two](https://b.example/2)", occurrence: occurrence(1),
			refusal: `find "[one two](https://b.example/2)" ` + another + `write the link as it is in find ("[one **two**](https://a.example/1)") with occurrence 0`,
			leaves:  "I wrote [one two](https://a.example/1) and  today.\n",
		},
		{
			shape: "the whole link", name: "holding a bold run, after a link with none", markdown: "I wrote [one two](https://a.example/1) and [one **two**](https://a.example/1) today.\n",
			op: "replace", find: "[one two](https://b.example/2)", occurrence: occurrence(1),
			refusal: `find "[one two](https://b.example/2)" ` + another +
				`write the link as it is in find ("[one **two**](https://a.example/1)") with occurrence 0` + replaces,
			leaves: "I wrote [one two](https://a.example/1) and NEW today.\n",
		},
		{shape: "part of the link", name: "spelled with another URL", markdown: plain, op: "delete", find: "[one](https://b.example/2)", takes: "I wrote [ two](https://a.example/1) today.\n"},
		{shape: "part of the link", name: "spelled with another URL", markdown: plain, op: "replace", find: "[one](https://b.example/2)", takes: "I wrote NEW[ two](https://a.example/1) today.\n"},
		{
			shape: "part of the link", name: "spelled with another title", markdown: titled, op: "delete", find: `[two](https://a.example/1 "Draft")`,
			takes: "I wrote [one ](https://a.example/1 \"Spec\") today.\n",
		},
		{
			shape: "part of the link", name: "spelled with another title", markdown: titled, op: "replace", find: `[two](https://a.example/1 "Draft")`,
			takes: "I wrote [one ](https://a.example/1 \"Spec\")NEW today.\n",
		},
		{
			shape: "part of the link", name: "whose text stands inside a word before the link", markdown: "Someone wrote [one two](https://a.example/1) today.\n",
			op: "delete", find: "[one](https://b.example/2)", occurrence: occurrence(0), takes: "Someone wrote [ two](https://a.example/1) today.\n",
		},
		{
			shape: "part of the link", name: "whose text stands inside a word before the link", markdown: "Someone wrote [one two](https://a.example/1) today.\n",
			op: "replace", find: "[one](https://b.example/2)", occurrence: occurrence(0), takes: "Someone wrote NEW[ two](https://a.example/1) today.\n",
		},
		{
			shape: "part of the link", name: "in the second link holding its text", markdown: "I wrote [one](https://a.example/1) and [one two](https://a.example/1) today.\n",
			op: "delete", find: "[one](https://b.example/2)", occurrence: occurrence(1),
			takes: "I wrote [one](https://a.example/1) and [ two](https://a.example/1) today.\n",
		},
		{
			shape: "part of the link", name: "in the second link holding its text", markdown: "I wrote [one](https://a.example/1) and [one two](https://a.example/1) today.\n",
			op: "replace", find: "[one](https://b.example/2)", occurrence: occurrence(1),
			takes: "I wrote [one](https://a.example/1) and NEW[ two](https://a.example/1) today.\n",
		},
		{
			shape: "part of the link", name: "holding a bold run", markdown: "I wrote [one **two** three](https://a.example/1) today.\n",
			op: "delete", find: "[one **two**](https://b.example/2)", takes: "I wrote [ three](https://a.example/1) today.\n",
		},
		{
			shape: "part of the link", name: "holding a bold run", markdown: "I wrote [one **two** three](https://a.example/1) today.\n",
			op: "replace", find: "[one **two**](https://b.example/2)", takes: "I wrote NEW[ three](https://a.example/1) today.\n",
		},
		{
			shape: "part of the link", name: "whose text stands before it under a link with no bold run",
			markdown: "I wrote [one two](https://a.example/1) and [one **two** three](https://a.example/1) today.\n",
			op:       "delete", find: "[one two](https://b.example/2)", occurrence: occurrence(1),
			takes: "I wrote [one two](https://a.example/1) and [ three](https://a.example/1) today.\n",
		},
		{
			shape: "the whole link and text", name: "before and after it, spelled with another URL", markdown: titled, op: "delete", find: `wrote [one two](https://b.example/2 "Spec") today`,
			refusal: `find "wrote [one two](https://b.example/2 \"Spec\") today" ` + crosses +
				`write the whole link as it is in find ("wrote [one two](https://a.example/1 \"Spec\") today")`,
			leaves: "I .\n",
		},
		{
			shape: "the whole link and text", name: "before and after it, spelled with another URL", markdown: titled, op: "replace", find: `wrote [one two](https://b.example/2 "Spec") today`,
			refusal: `find "wrote [one two](https://b.example/2 \"Spec\") today" ` + crosses +
				`write the whole link as it is in find ("wrote [one two](https://a.example/1 \"Spec\") today")` + replaces,
			leaves: "I NEW.\n",
		},
		{
			shape: "the whole link and text", name: "before and after it, spelled with another title", markdown: titled, op: "delete", find: `wrote [one two](https://a.example/1 "Draft") today`,
			refusal: `find "wrote [one two](https://a.example/1 \"Draft\") today" ` + crosses +
				`write the whole link as it is in find ("wrote [one two](https://a.example/1 \"Spec\") today")`,
			leaves: "I .\n",
		},
		{
			shape: "the whole link and text", name: "before and after it, spelled with another title", markdown: titled, op: "replace", find: `wrote [one two](https://a.example/1 "Draft") today`,
			refusal: `find "wrote [one two](https://a.example/1 \"Draft\") today" ` + crosses +
				`write the whole link as it is in find ("wrote [one two](https://a.example/1 \"Spec\") today")` + replaces,
			leaves: "I NEW.\n",
		},
		{
			shape: "the whole link and text", name: "after it", markdown: plain, op: "delete", find: "[one two](https://b.example/2) today",
			refusal: `find "[one two](https://b.example/2) today" ` + crosses + `write the whole link as it is in find ("[one two](https://a.example/1) today")`,
			leaves:  "I wrote .\n",
		},
		{
			shape: "the whole link and text", name: "after it", markdown: plain, op: "replace", find: "[one two](https://b.example/2) today",
			refusal: `find "[one two](https://b.example/2) today" ` + crosses + `write the whole link as it is in find ("[one two](https://a.example/1) today")` + replaces,
			leaves:  "I wrote NEW.\n",
		},
		{shape: "part of the link and text", name: "before it", markdown: plain, op: "delete", find: "wrote [one](https://b.example/2)", takes: "I [ two](https://a.example/1) today.\n"},
		{shape: "part of the link and text", name: "before it", markdown: plain, op: "replace", find: "wrote [one](https://b.example/2)", takes: "I NEW[ two](https://a.example/1) today.\n"},
		{
			shape: "part of the link and text", name: "after it", markdown: titled, op: "delete", find: `[two](https://b.example/2 "Spec") today`,
			takes: "I wrote [one ](https://a.example/1 \"Spec\").\n",
		},
		{
			shape: "part of the link and text", name: "after it", markdown: titled, op: "replace", find: `[two](https://b.example/2 "Spec") today`,
			takes: "I wrote [one ](https://a.example/1 \"Spec\")NEW.\n",
		},
		{
			shape: "part of the link and text", name: "before it, which stands bare before the link too", markdown: "I wrote one, then I wrote [one two](https://a.example/1) today.\n",
			op: "delete", find: "wrote [one](https://b.example/2)", takes: "I wrote one, then I [ two](https://a.example/1) today.\n",
		},
		{
			shape: "a bare link", name: "alone", markdown: bare, op: "delete", find: "[https://a.example/1](https://b.example/2)",
			refusal: `find "[https://a.example/1](https://b.example/2)" spells the link "https://a.example/1" with another URL or title than the one it has; ` +
				`write the link as it is in find ("https://a.example/1")`,
			leaves: "Read  today.\n",
		},
		{
			shape: "a bare link", name: "alone", markdown: bare, op: "replace", find: "[https://a.example/1](https://b.example/2)",
			refusal: `find "[https://a.example/1](https://b.example/2)" spells the link "https://a.example/1" with another URL or title than the one it has; ` +
				`write the link as it is in find ("https://a.example/1")` + inside,
			leaves: "Read [NEW](https://a.example/1) today.\n",
		},
		{
			shape: "a bare link", name: "in a longer find", markdown: bare, op: "delete", find: "Read [https://a.example/1](https://b.example/2) today",
			refusal: `find "Read [https://a.example/1](https://b.example/2) today" crosses the edge of the link "https://a.example/1": ` +
				`it covers text inside the link and text outside it, and it spells the link with another URL or title than the one it has; ` +
				`write the whole link as it is in find ("Read https://a.example/1 today")`,
			leaves: ".\n",
		},
		{
			shape: "a bare link", name: "in a longer find", markdown: bare, op: "replace", find: "Read [https://a.example/1](https://b.example/2) today",
			refusal: `find "Read [https://a.example/1](https://b.example/2) today" crosses the edge of the link "https://a.example/1": ` +
				`it covers text inside the link and text outside it, and it spells the link with another URL or title than the one it has; ` +
				`write the whole link as it is in find ("Read https://a.example/1 today")` + replaces,
			leaves: "NEW.\n",
		},
		{
			// The delete removes the whole item its marker selects, so the link the find spells
			// with another URL over part of its text goes whole: the find named keeps the
			// marker, selects that item and removes it.
			shape: "a list item its marker selects", name: "part of its link spelled with another URL", markdown: list, op: "delete", find: "- [one](https://b.example/2)",
			refusal: `find "- [one](https://b.example/2)" selects a list item, which the delete removes whole with the link "one two", ` +
				`and find spells that link with another URL or title than the one it has; write the link as it is in find ("- [one](https://a.example/1)")`,
			leaves: "- second\n",
		},
		{
			shape: "a list item its marker selects", name: "its whole link spelled with another URL", markdown: list, op: "delete", find: "- [one two](https://b.example/2)",
			refusal: `find "- [one two](https://b.example/2)" ` + another + `write the link as it is in find ("- [one two](https://a.example/1)")`,
			leaves:  "- second\n",
		},
		{
			// A paragraph before the list holds the link too, which a find with no marker
			// matches; the find named, with its marker, counts only the items it selects.
			shape: "a list item its marker selects", name: "its whole link spelled with another URL, after a paragraph holding that link",
			markdown: "I wrote [one two](https://a.example/1).\n\n" + list, op: "delete", find: "- [one two](https://b.example/2)",
			refusal: `find "- [one two](https://b.example/2)" ` + another + `write the link as it is in find ("- [one two](https://a.example/1)")`,
			leaves:  "I wrote [one two](https://a.example/1).\n\n- second\n",
		},
		{
			// Both items hold the link, and the find's occurrence selects the second; the find
			// named, with its marker, counts the items it selects, so it selects that item too.
			shape: "a list item its marker selects", name: "its whole link spelled with another URL, in the second of two items holding that link",
			markdown: "- [one two](https://a.example/1) and more\n- [one two](https://a.example/1) and less\n",
			op:       "delete", find: "- [one two](https://b.example/2)", occurrence: occurrence(1),
			refusal: `find "- [one two](https://b.example/2)" ` + another + `write the link as it is in find ("- [one two](https://a.example/1)") with occurrence 1`,
			leaves:  "- [one two](https://a.example/1) and more\n",
		},
		{
			// A replace writes over what its find matched alone, whatever selected it, so part of
			// the link spelled otherwise is taken as it is with no marker.
			shape: "a list item its marker selects", name: "part of its link spelled with another URL", markdown: list, op: "replace", find: "- [one](https://b.example/2)",
			takes: "- NEW[ two](https://a.example/1) and more\n- second\n",
		},
		{
			shape: "a list item its marker selects", name: "its whole link spelled with another URL", markdown: list, op: "replace", find: "- [one two](https://b.example/2)",
			refusal: `find "- [one two](https://b.example/2)" ` + another + `write the link as it is in find ("- [one two](https://a.example/1)")` + replaces,
			leaves:  "- NEW and more\n- second\n",
		},
		{
			// The delete of a paragraph's whole text removes the paragraph, and no text but what
			// the find matched, so every link of it is one the find covers whole.
			shape: "a paragraph's whole text", name: "part of its link spelled with another URL", markdown: whole, op: "delete", find: "[one](https://b.example/2) two and more",
			refusal: `find "[one](https://b.example/2) two and more" ` + crosses + `write the whole link as it is in find ("[one two](https://a.example/1) and more")`,
			leaves:  "After.\n",
		},
		{
			shape: "a paragraph's whole text", name: "its whole link spelled with another URL", markdown: whole, op: "delete", find: "[one two](https://b.example/2) and more",
			refusal: `find "[one two](https://b.example/2) and more" ` + crosses + `write the whole link as it is in find ("[one two](https://a.example/1) and more")`,
			leaves:  "After.\n",
		},
		{
			// Blocks written over the find split the paragraph and drop the space after it, all
			// the link holds beyond the find, so the replace removes the link whole.
			shape: "a link whose text ends on a space", name: "part of it spelled with another URL, with blocks", markdown: spaced,
			op: "replace", find: "[one](https://b.example/2)", with: "- x\n- y",
			refusal: `find "[one](https://b.example/2)" covers part of the link "one ", whose rest is whitespace beside find that a with of blocks drops, ` +
				`so the replace removes the link whole, and find spells that link with another URL or title than the one it has; ` +
				`write the link as it is in find ("[one](https://a.example/1)")` + replaces,
			leaves: "I wrote\n\n- x\n- y\n\n&#32;today.\n",
		},
		{
			// The finder matches the find's text without the space it ends on.
			shape: "a link whose text ends on a space", name: "the whole of it spelled with another URL, with blocks", markdown: spaced,
			op: "replace", find: "[one ](https://b.example/2)", with: "- x\n- y",
			refusal: `find "[one ](https://b.example/2)" covers part of the link "one ", whose rest is whitespace beside find that a with of blocks drops, ` +
				`so the replace removes the link whole, and find spells that link with another URL or title than the one it has; ` +
				`write the link as it is in find ("[one](https://a.example/1)")` + replaces,
			leaves: "I wrote\n\n- x\n- y\n\n&#32;today.\n",
		},
		{
			// A with of one paragraph is written over the find alone, which leaves the link its
			// space.
			shape: "a link whose text ends on a space", name: "part of it spelled with another URL", markdown: spaced, op: "replace", find: "[one](https://b.example/2)",
			takes: "I wrote NEW[ ](https://a.example/1) today.\n",
		},
		{
			// The link's text opens on a space, the end of the run before the find, which blocks
			// written over the find drop too, so the replace removes the link whole.
			shape: "a link whose text starts on a space", name: "part of it spelled with another URL, with blocks", markdown: "I wrote[ one](https://a.example/1) today.\n",
			op: "replace", find: "[one](https://b.example/2)", with: "- x\n- y",
			refusal: `find "[one](https://b.example/2)" covers part of the link " one", whose rest is whitespace beside find that a with of blocks drops, ` +
				`so the replace removes the link whole, and find spells that link with another URL or title than the one it has; ` +
				`write the link as it is in find ("[one](https://a.example/1)")` + replaces,
			leaves: "I wrote\n\n- x\n- y\n\ntoday.\n",
		},
		{
			shape: "a link's edge", name: "the whole link and text before it", markdown: "Sami wrote [this](https://example.com/Sami) today.\n",
			op: "replace", find: "wrote this",
			refusal: `find "wrote this" crosses the edge of the link "this": it covers text inside the link and text outside it, and ` +
				`text written over both would not keep the link whole; to keep the text find names, ` +
				`spell the link's markup over the part of it find covers ("wrote [this](https://example.com/Sami)")` + replaces,
			leaves: "Sami NEW today.\n",
		},
		{
			shape: "a link's edge", name: "part of the link and text before it", markdown: plain, op: "replace", find: "wrote one",
			refusal: `find "wrote one" crosses the edge of the link "one two": it covers text inside the link and text outside it, and ` +
				`text written over both would not keep the link whole; to keep the text find names, ` +
				`spell the link's markup over the part of it find covers ("wrote [one](https://a.example/1)")` + replaces,
			leaves: "I NEW[ two](https://a.example/1) today.\n",
		},
		{
			shape: "a link's edge", name: "a second link, past one spelled", markdown: "I wrote [this](https://a.example/1) and [that](https://b.example/2) today.\n",
			op: "replace", find: "wrote [this](https://a.example/1) and that",
			refusal: `find "wrote [this](https://a.example/1) and that" crosses the edge of the link "that": it covers text inside the link and text outside it, and ` +
				`text written over both would not keep the link whole; to keep the text find names, ` +
				`spell the link's markup over the part of it find covers ("wrote [this](https://a.example/1) and [that](https://b.example/2)")` + replaces,
			leaves: "I NEW today.\n",
		},
		{
			shape: "a code span's end", name: "with text after it", markdown: "Run `make said all` now.\n", op: "replace", find: "all now",
			refusal: `find "all now" crosses the edge of the code span "make said all": it covers text inside the code span and text outside it, and ` +
				"text written over both would not keep the code span whole; to keep the text find names, " +
				"spell the code span's markup over the part of it find covers (\"`all` now\")" + replaces,
			leaves: "Run `make said `NEW.\n",
		},
		{
			// No find spells strikethrough's markup, so two operations keep the text: one inside
			// the struck span, one outside it.
			shape: "a struck span's end", name: "with text after it", markdown: "~~Sami said it~~ now.\n", op: "replace", find: "it now",
			refusal: `find "it now" crosses the edge of the strikethrough span "Sami said it": it covers text inside the strikethrough span and text outside it, and ` +
				`text written over both would not keep the strikethrough span whole; narrow find to the text inside the strikethrough span, ` +
				`and replace the text outside it in an operation of its own, since a find cannot spell strikethrough's markup`,
		},
		{
			shape: "a bold span's end", name: "cut after a space", markdown: "**Sami said bold** now.\n", op: "replace", find: "bold now",
			refusal: `find "bold now" crosses the edge of the bold span "Sami said bold": it covers text inside the bold span and text outside it, and ` +
				`text written over both would not keep the bold span whole; no find keeps the text find names, since the part of the bold span it leaves, ` +
				`"Sami said ", would meet the text written over it at a space, where markdown cannot write the bold span's delimiters; move that space out of the bold span`,
		},
		{
			shape: "a bold span's end", name: "cut inside a word", markdown: "**Sami said bold** now.\n", op: "replace", find: "old now",
			refusal: `find "old now" crosses the edge of the bold span "Sami said bold": it covers text inside the bold span and text outside it, and ` +
				`text written over both would not keep the bold span whole; to keep the text find names, ` +
				`spell the bold span's markup over the part of it find covers ("**old** now")` + replaces,
			leaves: "**Sami said b**NEW.\n",
		},
		{
			shape: "a bold span's start", name: "cut before a space", markdown: "Then **Sami said bold**.\n", op: "replace", find: "Then Sami",
			refusal: `find "Then Sami" crosses the edge of the bold span "Sami said bold": it covers text inside the bold span and text outside it, and ` +
				`text written over both would not keep the bold span whole; no find keeps the text find names, since the part of the bold span it leaves, ` +
				`" said bold", would meet the text written over it at a space, where markdown cannot write the bold span's delimiters; move that space out of the bold span`,
		},
		{
			shape: "a bold span's start", name: "cut inside a word", markdown: "Then **Sami said bold**.\n", op: "replace", find: "Then Sa",
			refusal: `find "Then Sa" crosses the edge of the bold span "Sami said bold": it covers text inside the bold span and text outside it, and ` +
				`text written over both would not keep the bold span whole; to keep the text find names, ` +
				`spell the bold span's markup over the part of it find covers ("Then **Sa**")` + replaces,
			leaves: "NEW**mi said bold**.\n",
		},
		{
			shape: "an italic span's end", name: "cut after a space", markdown: "*Sami said it* now.\n", op: "replace", find: "it now",
			refusal: `find "it now" crosses the edge of the italic span "Sami said it": it covers text inside the italic span and text outside it, and ` +
				`text written over both would not keep the italic span whole; no find keeps the text find names, since the part of the italic span it leaves, ` +
				`"Sami said ", would meet the text written over it at a space, where markdown cannot write the italic span's delimiters; move that space out of the italic span`,
		},
		{
			shape: "an italic span's end", name: "cut inside a word", markdown: "*Sami said it* now.\n", op: "replace", find: "t now",
			refusal: `find "t now" crosses the edge of the italic span "Sami said it": it covers text inside the italic span and text outside it, and ` +
				`text written over both would not keep the italic span whole; to keep the text find names, ` +
				`spell the italic span's markup over the part of it find covers ("*t* now")` + replaces,
			leaves: "*Sami said i*NEW.\n",
		},
		{
			shape: "an italic span's start", name: "cut before a space", markdown: "Then *Sami said it*.\n", op: "replace", find: "Then Sami",
			refusal: `find "Then Sami" crosses the edge of the italic span "Sami said it": it covers text inside the italic span and text outside it, and ` +
				`text written over both would not keep the italic span whole; no find keeps the text find names, since the part of the italic span it leaves, ` +
				`" said it", would meet the text written over it at a space, where markdown cannot write the italic span's delimiters; move that space out of the italic span`,
		},
		{
			shape: "an italic span's start", name: "cut inside a word", markdown: "Then *Sami said it*.\n", op: "replace", find: "Then Sa",
			refusal: `find "Then Sa" crosses the edge of the italic span "Sami said it": it covers text inside the italic span and text outside it, and ` +
				`text written over both would not keep the italic span whole; to keep the text find names, ` +
				`spell the italic span's markup over the part of it find covers ("Then *Sa*")` + replaces,
			leaves: "NEW*mi said it*.\n",
		},
		{
			// The find spells the bold span's markup over the part of it it covers, which the one
			// rule takes, but the part it leaves would end on a space, which markdown cannot close
			// bold after, and that same spelling is the only find over this text.
			shape: "a bold span's end, spelled", name: "cut after a space", markdown: "**Sami said bold** now.\n", op: "replace", find: "**bold** now",
			refusal: `find "**bold** now" crosses the edge of the bold span "Sami said bold": it covers text inside the bold span and text outside it, and ` +
				`the part of the bold span it leaves, "Sami said ", would meet the text written over it at a space, ` +
				`where markdown cannot write the bold span's delimiters; move that space out of the bold span`,
		},
		{
			shape: "a bold span's start, spelled", name: "cut before a space", markdown: "Then **Sami said bold**.\n", op: "replace", find: "Then **Sami**",
			refusal: `find "Then **Sami**" crosses the edge of the bold span "Sami said bold": it covers text inside the bold span and text outside it, and ` +
				`the part of the bold span it leaves, " said bold", would meet the text written over it at a space, ` +
				`where markdown cannot write the bold span's delimiters; move that space out of the bold span`,
		},
		{
			shape: "an italic span's end, spelled", name: "cut after a space", markdown: "*Sami said it* now.\n", op: "replace", find: "*it* now",
			refusal: `find "*it* now" crosses the edge of the italic span "Sami said it": it covers text inside the italic span and text outside it, and ` +
				`the part of the italic span it leaves, "Sami said ", would meet the text written over it at a space, ` +
				`where markdown cannot write the italic span's delimiters; move that space out of the italic span`,
		},
		{
			shape: "an italic span's start, spelled", name: "cut before a space", markdown: "Then *Sami said it*.\n", op: "replace", find: "Then *Sami*",
			refusal: `find "Then *Sami*" crosses the edge of the italic span "Sami said it": it covers text inside the italic span and text outside it, and ` +
				`the part of the italic span it leaves, " said it", would meet the text written over it at a space, ` +
				`where markdown cannot write the italic span's delimiters; move that space out of the italic span`,
		},
	} {
		t.Run(test.op+" of "+test.shape+", "+test.name, func(t *testing.T) {
			with := test.with
			if with == "" {
				with = "NEW"
			}
			edit := func(find string, occurrence *int) []model.EditOp {
				if test.op == "delete" {
					return []model.EditOp{{Op: "delete", Find: find, Occurrence: occurrence}}
				}
				return []model.EditOp{{Op: "replace", Find: find, With: with, Occurrence: occurrence}}
			}
			actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, test.markdown)
			before, _ := latestVersionOf(t, service.store, artifactID)
			_, err := joinedApplyOps(service, artifactID, edit(test.find, test.occurrence), actor, nil)
			if test.takes != "" {
				if err != nil {
					t.Fatalf("%s %q in %q: %v, want it taken", test.op, test.find, test.markdown, err)
				}
				requireDocument(t, service, artifactID, test.takes, fmt.Sprintf("%s %q in %q", test.op, test.find, test.markdown))
				return
			}
			var crossed *ErrFindCrossesSpan
			if !errors.As(err, &crossed) {
				t.Fatalf("%s %q in %q = %v, want it refused with the refusal\n%s", test.op, test.find, test.markdown, err, test.refusal)
			}
			requireUnchanged(t, service, artifactID, before, test.markdown, fmt.Sprintf("the refused %s %q", test.op, test.find))
			if refusal := strings.TrimPrefix(err.Error(), "operation 0: "); refusal != test.refusal {
				t.Errorf("%s %q in %q: refusal\n%s\nwant\n%s", test.op, test.find, test.markdown, refusal, test.refusal)
			}
			// The find the refusal names (Own) is sent in the find's place, with the occurrence it
			// names, so a find named that does not do what the refused edit would have fails here
			// whatever text the row wants.
			if (crossed.Own.Quote == "") != (test.leaves == "") {
				t.Fatalf("%s %q in %q: the refusal names the find %q, want one exactly where the row says what it leaves (%q)",
					test.op, test.find, test.markdown, crossed.Own.Quote, test.leaves)
			}
			if test.leaves == "" {
				return
			}
			t.Run(crossed.Own.Quote, func(t *testing.T) {
				service, artifactID := newTestService(t)
				seedServiceText(t, service, artifactID, test.markdown)
				if _, err := joinedApplyOps(service, artifactID, edit(crossed.Own.Quote, crossed.Own.Occurrence), actor, nil); err != nil {
					t.Fatalf("%s %q, which the refusal of %q names, in %q: %v, want it taken", test.op, crossed.Own.Quote, test.find, test.markdown, err)
				}
				requireDocument(t, service, artifactID, test.leaves, fmt.Sprintf("%s %q, which the refusal of %q names, in %q", test.op, crossed.Own.Quote, test.find, test.markdown))
			})
		})
	}
}

// Where the find written from the document's text over the text a refused find named would be
// refused itself, for a span that find crosses too, the refusal names no find and gives that
// span's advice, naming it, and following the advice leaves what the refused find meant. A
// replace's find from a link it spells with another URL into a struck span: the link as it is
// crosses the struck span too, whose markup no find spells, so the refusal says to replace the
// text inside the struck span and the text outside it in two operations, which are taken. A
// replace's find from a bold span into an italic span (`b c d` over `**ab** c *d e*`): the find
// spelling both spans leaves the italic span's ` e`, which would meet the written text at a space,
// so the refusal says to move that space out of the italic span; in the document with that space
// moved out (`**ab** c *d* *e*`), the same find is refused naming the find that spells both spans,
// which is taken there.
func TestARefusalNamingNoFindGivesTheAdviceOfTheSpanThatStopsIt(t *testing.T) {
	t.Parallel()
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	// refused is the refusal of a replace of find with NEW in a document seeded with markdown,
	// which it leaves as it was.
	refused := func(t *testing.T, markdown, find string) *ErrFindCrossesSpan {
		t.Helper()
		service, artifactID := newTestService(t)
		seedServiceText(t, service, artifactID, markdown)
		before, _ := latestVersionOf(t, service.store, artifactID)
		_, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: find, With: "NEW"}}, actor, nil)
		var crossed *ErrFindCrossesSpan
		if !errors.As(err, &crossed) {
			t.Fatalf("replace %q in %q = %v, want it refused for crossing a span", find, markdown, err)
		}
		requireUnchanged(t, service, artifactID, before, markdown, fmt.Sprintf("the refused replace %q", find))
		return crossed
	}
	// taken requires ops, sent in one batch on a document seeded with markdown, to leave want.
	taken := func(t *testing.T, markdown string, ops []model.EditOp, want string) {
		t.Helper()
		service, artifactID := newTestService(t)
		seedServiceText(t, service, artifactID, markdown)
		if _, err := joinedApplyOps(service, artifactID, ops, actor, nil); err != nil {
			t.Fatalf("%+v in %q: %v, want it taken", ops, markdown, err)
		}
		requireDocument(t, service, artifactID, want, fmt.Sprintf("%+v in %q", ops, markdown))
	}
	t.Run("a find from a link it spells with another URL into a struck span", func(t *testing.T) {
		const markdown = "I wrote [one two](https://a.example/1) ~~three four~~ today.\n"
		crossed := refused(t, markdown, "[one two](https://b.example/2) three")
		want := `operation 0: find "[one two](https://b.example/2) three" crosses the edge of the link "one two": ` +
			`it covers text inside the link and text outside it, and it spells the link with another URL or title than the one it has; ` +
			`no find keeps the text find names, since it crosses the edge of the strikethrough span "three four" too: ` +
			`narrow find to the text inside the strikethrough span, and replace the text outside it in an operation of its own, ` +
			`since a find cannot spell strikethrough's markup`
		if crossed.Error() != want || crossed.Own.Quote != "" {
			t.Fatalf("refusal =\n%s\nnaming %q, want\n%s\nnaming none", crossed, crossed.Own.Quote, want)
		}
		taken(t, markdown, []model.EditOp{
			{Op: "replace", Find: "three", With: "3"},
			{Op: "replace", Find: "[one two](https://a.example/1)", With: "NEW"},
		}, "I wrote NEW ~~3 four~~ today.\n")
	})
	t.Run("a find from a bold span into an italic span it leaves on a space", func(t *testing.T) {
		crossed := refused(t, "**ab** c *d e*\n", "b c d")
		want := `operation 0: find "b c d" crosses the edge of the bold span "ab": it covers text inside the bold span and text outside it, and ` +
			`text written over both would not keep the bold span whole; no find keeps the text find names, ` +
			`since it crosses the edge of the italic span "d e" too, and the part of the italic span it leaves, " e", ` +
			`would meet the text written over it at a space, where markdown cannot write the italic span's delimiters; move that space out of the italic span`
		if crossed.Error() != want || crossed.Own.Quote != "" {
			t.Fatalf("refusal =\n%s\nnaming %q, want\n%s\nnaming none", crossed, crossed.Own.Quote, want)
		}
		const moved = "**ab** c *d* *e*\n"
		if named := refused(t, moved, "b c d").Own; named.Quote != "**b** c *d*" || named.Occurrence != nil {
			t.Fatalf("with the space moved out of the italic span, the refusal of %q names %+v, want %q", "b c d", named, "**b** c *d*")
		}
		taken(t, moved, []model.EditOp{{Op: "replace", Find: "**b** c *d*", With: "NEW"}}, "**a**NEW *e*\n")
	})
}
