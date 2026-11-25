package api

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// workGrowthStep and workLinearSlack are pmdoc's growthStep and linearSlack
// (pmdoc/linear_test.go): the larger of TestACutReadsAVersionsBlockIDsInWorkLinearInItsSize's two
// versions is workGrowthStep times the smaller, and its cut may read at most workLinearSlack times
// that growth more. A cut that reads the text once grows about eightfold, inside 3*8 = 24, and a
// cut that reads the text once for each block grows about 8*8 = 64 times, past it.
const (
	workGrowthStep  = 8
	workLinearSlack = 3
)

// requirementID is the id of the index-th requirement of requirementsVersion: its index's five
// digits written backwards, all of one width so no id begins another, and out of order so the
// cut's sort of what follows each `#` does all its work rather than find the text already sorted.
func requirementID(index int) string {
	digits := []byte(fmt.Sprintf("%05d", index))
	slices.Reverse(digits)
	return "r" + string(digits)
}

// requirementsVersion is a stored version of a spec holding blocks requirements under its title,
// as the renderer writes them: each holds word, but the last, which holds last.
func requirementsVersion(blocks int, word, last string) string {
	var text strings.Builder
	text.WriteString("# Spec\n")
	for index := range blocks {
		if index == blocks-1 {
			word = last
		}
		fmt.Fprintf(&text, "\n:::requirement{#%s}\n%s\n:::\n", requirementID(index), word)
	}
	return text.String()
}

// The change set leaves out every typed block whose id its stored version does not write
// (cutTypedBlocks), and one version may hold as many typed blocks as a mebibyte
// (pmdoc.MaxDocumentBytes) and 65,536 elements (pmdoc.MaxDocumentElements) allow. Any caller who
// may read a document may ask for its change set again and again, and the daemon asks for it at
// each reconcile, so the cut's work grows with the text's size and never with its size times its
// blocks.
//
// The work is counted, not timed (writtenIDReads): each step of the cut tells the bytes it reads,
// the scan of each stored text one search at a time, and this test bounds the total of those
// counted steps. The searches of one scan read its text exactly once, so the count of two versions
// is at least the length of the two texts, and the test fails below that: a cut whose steps tell
// writtenIDReads nothing would pass any bound on growth. A version at both caps, the requirements
// as many as its elements allow and each as long as fills the mebibyte, is compared with the same
// version with its last requirement reworded, and so is a version of an eighth of the
// requirements. The larger pair's counted reads may grow at most workLinearSlack times as much as
// its text does, the bound pmdoc's growsLinearly (pmdoc/linear_test.go) puts on a parse's time.
// So a cut whose counted steps read the text again for each block fails. The cut of the larger
// version keeps each of its requirements, and both change sets name the reworded requirement and
// no other block.
func TestACutReadsAVersionsBlockIDsInWorkLinearInItsSize(t *testing.T) {
	one := pmdoc.MeasureDocument(requirementsVersion(1, "a", "a"))
	two := pmdoc.MeasureDocument(requirementsVersion(2, "a", "a"))
	elements, bytes := two.Elements-one.Elements, two.Bytes-one.Bytes
	blocks := (pmdoc.MaxDocumentElements - (one.Elements - elements)) / elements
	word := strings.Repeat("a", 1+(pmdoc.MaxDocumentBytes-(one.Bytes-bytes))/blocks-bytes)
	reworded := strings.Repeat("b", len(word))
	if size := pmdoc.MeasureDocument(requirementsVersion(blocks, word, word)); size.TooLong() || size.TooHeavy() {
		t.Fatalf("the version of %d requirements measures %+v, past what one upload may send", blocks, size)
	}
	if cut, err := readCutVersion(requirementsVersion(blocks, word, word)); err != nil || len(cut.blocks) != blocks {
		t.Fatalf("the cut of a version of %d requirements kept %d of its typed blocks (%v), want every one", blocks, len(cut.blocks), err)
	}

	read := func(t *testing.T, blocks int) (text int, reads int64) {
		t.Helper()
		before, after := requirementsVersion(blocks, word, word), requirementsVersion(blocks, word, reworded)
		var total int64
		writtenIDReads = func(bytes int) { total += int64(bytes) }
		defer func() { writtenIDReads = nil }()
		changes, err := compareVersions(before, after)
		if err != nil {
			t.Fatalf("compare two versions of %d requirements: %v", blocks, err)
		}
		want := []artifactBlockChange{{ID: requirementID(blocks - 1), Type: "requirement", Change: changeChanged}}
		if !reflect.DeepEqual(changes.Blocks, want) {
			t.Fatalf("the change set of %d requirements names the blocks %+v, want %+v", blocks, changes.Blocks, want)
		}
		text = len(before) + len(after)
		if total < int64(text) {
			t.Fatalf("the cut of two versions of %d requirements counted %d bytes read, fewer than the %d bytes of the two texts its scans read: "+
				"a step of the cut does not tell writtenIDReads what it reads", blocks, total, text)
		}
		return text, total
	}
	smallText, smallReads := read(t, blocks/workGrowthStep)
	fullText, fullReads := read(t, blocks)
	growth := float64(fullText) / float64(smallText)
	t.Logf("%d requirements in %d bytes of two versions: %d bytes read; %d requirements in %d bytes: %d bytes read, %.1f times as many for %.1f times the text",
		blocks/workGrowthStep, smallText, smallReads, blocks, fullText, fullReads, float64(fullReads)/float64(smallReads), growth)
	if float64(fullReads) > workLinearSlack*growth*float64(smallReads) {
		t.Fatalf("the cut of two versions of %d requirements counted %d bytes read, %.1f times the %d bytes it counted for %d requirements, "+
			"past %d times the text's growth of %.1f: a counted step reads the text once for each block",
			blocks, fullReads, float64(fullReads)/float64(smallReads), smallReads, blocks/workGrowthStep, workLinearSlack, growth)
	}
}

// The cut keeps a typed block exactly when a search of its stored text for the block's id, written
// as the block's opening line writes it (pmdoc.TypedBlockIDAttribute), finds it: reading the text
// once changes the work of that answer and not the answer. Each case is a text and the ids looked
// up in it, among them an id that only begins the text after a `#`, an `id="` whose value ends at
// the `"` of the next `id="`, and ids written `id="…"` because they hold a space, a `"` or an `&`.
func TestAWrittenIDIsFoundWhereASearchOfTheTextFindsIt(t *testing.T) {
	t.Parallel()
	ids := []string{"r1", "r", "r12", "q:1", "décision", "a b", `x"y`, "a&b", "aid=", "b c", "c", "", "zz"}
	for _, stored := range []string{
		"",
		"# Spec\n\n## Scope\n\nNo block here.\n",
		":::requirement{#r1}\nThe read succeeds.\n:::\n",
		":::requirement{#r12}\nThe read succeeds.\n:::\n\nSee #r and #q:1.",
		":::requirement{#décision}\nOne.\n:::\n\n:::check{id=\"a b\"}\nTwo.\n:::\n",
		"::::callout{id=\"x&#x22;y\"}\nThree.\n::::\n\n%ask{id=\"a&amp;b\"}",
		"id=\"aid=\"b c\" and id=\"c",
		"##zz",
	} {
		written := writtenIDsIn(stored)
		for _, id := range ids {
			if got, want := written.names(id), strings.Contains(stored, pmdoc.TypedBlockIDAttribute(id)); got != want {
				t.Errorf("names(%q) in %q = %t, want %t, what a search of the text for %s answers",
					id, stored, got, want, pmdoc.TypedBlockIDAttribute(id))
			}
		}
	}
}
