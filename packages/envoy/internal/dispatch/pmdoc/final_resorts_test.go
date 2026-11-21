package pmdoc

import "testing"

// Each of these trees has a spelling that reads back, and no spelling the writer tried before
// finalResorts reads back, so the writer kept its first writing, which reads back otherwise, and
// an edit that left it stored a text an upload of it is refused for. Each markdown reads as its
// tree and is the tree's rendering:
//   - a delimiter run that closes one mark and opens another between punctuation and a letter,
//     which does both only with the letter written as a reference: the paragraph of a real
//     document an edit stored reading back otherwise, reduced, and the two other shapes a long run
//     of that document reduced to;
//   - marks kept open where that fuses a run, which no earlier spelling kept;
//   - a reference that makes the run at the other end of its text want one too;
//   - a code span's backtick inside a run beside a letter, which no earlier need read back;
//   - a code span of a space beside one under another mark, whose padding the writer sized over
//     both, so that the space read back as three.
func TestTheWriterSpellsTreesNoEarlierSpellingReadsBack(t *testing.T) {
	for _, markdown := range []string{
		"*(***&#120;*y***",
		"*&#120;***)**",
		"*&#32;***&#120;**",
		"***.**&#32;***.**",
		"&#120;*&#120;***(**",
		"*`x`*&#120;",
		"` `*`` ` ``*",
	} {
		doc, err := Parse(markdown + "\n")
		if err != nil {
			t.Fatalf("Parse(%q) = %v", markdown, err)
		}
		if deepest := deepestWholeSpelling(asWritten(doc).Children[0].Children, false); deepest != 0 {
			t.Errorf("a spelling before finalResorts writes %q so that it reads back under %d readers, want none: the tree proves nothing", markdown, deepest)
			continue
		}
		rendered, err := Render(doc)
		if err != nil {
			t.Errorf("Render(Parse(%q)) = %v", markdown, err)
			continue
		}
		if _, misread, refused, err := documentMisread(doc); misread.reason != "" || refused != nil || err != nil {
			t.Errorf("Render(Parse(%q)) = %q, which reads back otherwise: %q, %v, %v", markdown, rendered, misread.reason, refused, err)
			continue
		}
		if _, err := ParseForWrite(rendered, nil); err != nil {
			t.Errorf("an upload of Render(Parse(%q)) = %q is refused: %v", markdown, rendered, err)
		}
		if rendered != markdown+"\n" {
			t.Errorf("Render(Parse(%q)) = %q, want %q", markdown, rendered, markdown+"\n")
		}
	}
}
