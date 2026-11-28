package pmdoc

import (
	"strings"
	"testing"
	"time"
)

// noOneSpelling is a paragraph no one spelling of the writer reads back. Its first half, a tilde
// and a backslash before a letter beside an emphasised tilde, reads back only with that letter
// written as a reference and the backslash before it as `\\` (the flanking spellings of
// lastResorts); its second half, an emphasised letter and a struck emphasised tilde, only with the
// emphasis closed and opened again inside the strikethrough (marksReopened), which no spelling
// writes with references. Each half reads back in a spelling of its own, so the run is written in
// one for each.
const noOneSpelling = "p &#126;\\\\&#97;*&#126;* a*a*~*~*~\n"

// deepestWholeSpelling is how many readers, from the first, the writing of nodes, a paragraph's
// inline nodes, that reads back under the most of them does: its first writing and each spelling
// inlineWithEscapes writes a run in, each written whole, finalResorts' only where final says so.
func deepestWholeSpelling(nodes []*Node, final bool) int {
	r := &renderer{}
	context := inlineContext{startsLine: true}
	fused := r.writeInlineRun(nodes, "", context, runSpelling{})
	needs := runNeedsOf(nodes, context, fused, r.misflanked(0))
	labels, signature := referencedLabels(nodes), inlineSignature(nodes)
	deepest := readsBackUnder(r.b.Bytes(), "", labels, signature)
	spellings := runSpellings(needs)
	every := append(spellings, lastResorts(needs, spellings)...)
	if final {
		every = append(every, finalResorts(needs, nestsMarks(nodes))...)
	}
	for _, spelling := range every {
		r.b.Reset()
		if r.writeInlineRun(nodes, "", context, spelling) && spelling.order != marksAsWritten && !spelling.flankingFused {
			continue
		}
		deepest = max(deepest, readsBackUnder(r.b.Bytes(), "", labels, signature))
	}
	return deepest
}

// A run no one spelling reads back is written in a spelling for each of its segments, which reads
// back as the tree, so an upload of the rendering is stored: the writer used to keep the run's first
// writing, which reads back otherwise.
func TestARunNoOneSpellingReadsBackIsWrittenInOneForEachSegment(t *testing.T) {
	doc, err := Parse(noOneSpelling)
	if err != nil {
		t.Fatalf("Parse(%q) = %v", noOneSpelling, err)
	}
	if deepest := deepestWholeSpelling(asWritten(doc).Children[0].Children, true); deepest != 0 {
		t.Fatalf("a whole spelling of %q reads back under %d readers, want none: the run proves nothing", noOneSpelling, deepest)
	}
	rendered, err := Render(doc)
	if err != nil {
		t.Fatalf("Render(Parse(%q)) = %v", noOneSpelling, err)
	}
	if _, misread, refused, err := documentMisread(doc); misread.reason != "" || refused != nil || err != nil {
		t.Fatalf("Render(Parse(%q)) = %q, which reads back otherwise: %q, %v, %v", noOneSpelling, rendered, misread.reason, refused, err)
	}
	if _, err := ParseForWrite(rendered, nil); err != nil {
		t.Fatalf("an upload of Render(Parse(%q)) = %q is refused: %v", noOneSpelling, rendered, err)
	}
	const want = "p &#126;\\\\&#97;*&#126;* a*a*~~*&#126;*~~\n"
	if rendered != want {
		t.Errorf("Render(Parse(%q)) = %q, want %q", noOneSpelling, rendered, want)
	}
}

// A run written in a spelling for each segment is written the same every time, and an edit of the
// document writes only the blocks it touches: Layout.Splice keeps the bytes of every other block,
// the run's among them, and the run reads back after an edit beside it and after one inside it.
func TestAnEditBesideARunWrittenBySegmentsKeepsEveryOtherBlocksBytes(t *testing.T) {
	const before, after = "Words before it.\n\n", "\nWords after it.\n"
	doc, err := Parse(before + noOneSpelling + after)
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	markdown, _, layout, err := RenderWithLayout(doc)
	if err != nil {
		t.Fatalf("RenderWithLayout = %v", err)
	}
	if again, err := Render(doc); err != nil || again != markdown {
		t.Fatalf("a second Render = %q, %v, want the first, %q", again, err, markdown)
	}
	if reading, refused, err := storedMisread(doc, markdown); reading.misread().reason != "" || refused != nil || err != nil {
		t.Fatalf("%q reads back otherwise: %q, %v, %v", markdown, reading.misread().reason, refused, err)
	}
	edits := []struct {
		name, markdown string
		block          int
	}{
		{"an edit of the block before the run", "Words before it, edited.\n\n" + noOneSpelling + after, 0},
		{"an edit inside the run", before + "p &#126;\\\\&#97;*&#126;* and a*a*~*~*~\n" + after, 1},
	}
	for _, edit := range edits {
		next, err := Parse(edit.markdown)
		if err != nil {
			t.Fatalf("%s: Parse = %v", edit.name, err)
		}
		spliced, _, ok := layout.Splice(markdown, []int{edit.block}, func(index int) (*Node, error) { return next.Children[index], nil })
		if !ok {
			t.Fatalf("%s: Splice did not answer", edit.name)
		}
		assertSpliceIsWhole(t, edit.name, markdown, layout, next, []int{edit.block})
		for index := range layout.Blocks() {
			if index == edit.block {
				continue
			}
			kept := markdown[layout.start(index):int(layout.ends[index])]
			if edit.block < index {
				if !strings.HasSuffix(spliced, markdown[layout.start(index):]) {
					t.Errorf("%s: block %d, %q, and the blocks after it changed bytes: %q", edit.name, index, kept, spliced)
				}
			} else if !strings.HasPrefix(spliced, markdown[:int(layout.ends[index])]) {
				t.Errorf("%s: block %d, %q, and the blocks before it changed bytes: %q", edit.name, index, kept, spliced)
			}
		}
		if reading, refused, err := storedMisread(next, spliced); reading.misread().reason != "" || refused != nil || err != nil {
			t.Errorf("%s: %q reads back otherwise: %q, %v, %v", edit.name, spliced, reading.misread().reason, refused, err)
		}
	}
}

// A large run no one spelling reads back is read back a few times its length in all, where the
// writer used to read it back whole once for each distinct writing, a dozen times and more: each
// writing is read a group of segments at a time, a group whose bytes no writing before wrote, and
// the run whole only where every group reads back. The run's address and www text give it more
// spellings, each of which changes the bytes of the one group that holds them.
func TestALargeRunNoOneSpellingReadsBackIsReadBackAFewTimesItsLength(t *testing.T) {
	markdown := "p &#126;\\\\&#97;*&#126;* mail a@b.cc or see www.example.com " + strings.Repeat("plain words ", 25000) + "a*a*~*~*~\n"
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	read := 0
	readBackObserver = func(markdown string, _ inlineReader) { read += len(markdown) }
	defer func() { readBackObserver = nil }()
	start := time.Now()
	rendered, err := Render(doc)
	took := time.Since(start)
	readBackObserver = nil
	if err != nil {
		t.Fatalf("Render = %v", err)
	}
	t.Logf("a run of %d bytes rendered in %v, reading back %d bytes", len(rendered), took, read)
	if limit := 6 * len(rendered); read > limit {
		t.Errorf("rendering a run of %d bytes read back %d bytes, want at most %d", len(rendered), read, limit)
	}
	if reading, refused, err := storedMisread(doc, rendered); reading.misread().reason != "" || refused != nil || err != nil {
		t.Errorf("the run's rendering reads back otherwise: %q, %v, %v", reading.misread().reason, refused, err)
	}
}
