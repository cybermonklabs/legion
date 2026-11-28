package api

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// heldKeys is the keys memo keeps, the most recently used first: each stored version by its
// number, and the document as it stands as "live". It fails t unless what the memo charges is
// exactly what its entries were charged, its map and its list hold the same entries, and the
// charge is within the memo's bound.
func heldKeys(t *testing.T, memo *documentCutMemo) []string {
	t.Helper()
	var keys []string
	var charged int64
	for element := memo.order.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*documentCutEntry)
		name := strconv.Itoa(entry.key.number)
		if entry.key.live {
			name = "live"
		}
		if memo.items[entry.key] != element {
			t.Fatalf("key %s is in the memo's list but its map names another element", name)
		}
		keys = append(keys, name)
		charged += entry.size
	}
	if len(memo.items) != len(keys) {
		t.Fatalf("the memo's map holds %d entries and its list %d", len(memo.items), len(keys))
	}
	if memo.held != charged {
		t.Fatalf("the memo charges %d bytes, and its entries were charged %d", memo.held, charged)
	}
	if memo.held > memo.limit {
		t.Fatalf("the memo charges %d bytes, past its bound of %d", memo.held, memo.limit)
	}
	return keys
}

// The memo of a document's cuts (documentCuts) is held to its bound in bytes, under a bound of
// three entries here rather than 32 MiB. Each store keeps what is charged within the bound,
// dropping the least recently used entries first, as many as the new entry needs; a read marks
// its entry the most recently used; an entry charged more than the whole bound is not kept, and
// the entries already kept stay; a version whose markdown the parser refuses is parsed once and
// then served as refused; an entry is served only for the markdown it was cut from; and the
// document as it stands is kept under a key of its own, which no stored version's key is.
func TestTheDocumentCutMemoKeepsTheMostRecentlyUsedCutsWithinItsBound(t *testing.T) {
	// Not parallel: it writes the package variable decisionCutsObserver, the hook every decision cut
	// calls.
	const markdownBytes = 100
	const artifactID = "2e5d211a-b79d-48a7-9e08-3eece00fffed"
	entry := int64(markdownBytes + documentCutOverheadBytes)
	memo := newDocumentCutMemo(3 * entry)
	key := func(number int) documentCutKey {
		return documentCutKey{artifactID: artifactID, number: number}
	}
	// markdown is version number's text, size bytes long; cutOf is a cut of it with no decisions,
	// charged its bytes and one overhead.
	markdown := func(number, size int) string {
		prefix := fmt.Sprintf("version %d ", number)
		return prefix + strings.Repeat("x", size-len(prefix))
	}
	cutOf := func(number, size int) documentCut {
		return documentCut{markdown: markdown(number, size), cuts: map[sectionKey]decisionCut{}, read: true}
	}
	holds := func(want string) {
		t.Helper()
		if got := fmt.Sprint(heldKeys(t, memo)); got != want {
			t.Fatalf("the memo holds %s, most recently used first, want %s", got, want)
		}
	}

	for number := 1; number <= 3; number++ {
		memo.put(key(number), cutOf(number, markdownBytes))
		heldKeys(t, memo)
	}
	holds("[3 2 1]")
	if memo.held != memo.limit {
		t.Fatalf("three entries are charged %d bytes, want the whole bound %d", memo.held, memo.limit)
	}

	// A read marks version 1 the most recently used, so the next store drops version 2.
	if cut, held := memo.get(key(1), markdown(1, markdownBytes)); !held || !cut.read {
		t.Fatalf("the read of version 1 = %#v, %v; want its cut", cut, held)
	}
	holds("[1 3 2]")
	memo.put(key(4), cutOf(4, markdownBytes))
	holds("[4 1 3]")
	memo.put(key(5), cutOf(5, markdownBytes))
	holds("[5 4 1]")

	// An entry charged two entries' bytes drops the two least recently used.
	memo.put(key(6), cutOf(6, int(2*entry)-documentCutOverheadBytes))
	holds("[6 5]")

	// An entry charged more than the whole bound is not kept, and drops nothing.
	memo.put(key(7), cutOf(7, int(memo.limit)))
	holds("[6 5]")
	if _, held := memo.get(key(7), markdown(7, int(memo.limit))); held {
		t.Fatalf("the memo serves version 7, charged past its bound of %d", memo.limit)
	}

	// A refused version is parsed once and then served as refused.
	refused := "[spec]: https://example.com/spec\n\n## A decision\n\nIts text.\n"
	if _, read := decisionCuts(refused); read {
		t.Fatalf("the parser reads %q; the test needs markdown it refuses", refused)
	}
	parsed := 0
	decisionCutsObserver = func(string) { parsed++ }
	t.Cleanup(func() { decisionCutsObserver = nil })
	for range 2 {
		if cut := memo.cut(key(8), refused); cut.read || len(cut.cuts) != 0 || cut.markdown != refused {
			t.Fatalf("the cut of the refused version 8 = %#v, want it refused, with no cuts", cut)
		}
	}
	if parsed != 1 {
		t.Fatalf("two cuts of the refused version 8 parsed it %d times, want once", parsed)
	}
	holds("[8 6]")

	// An entry is served only for the markdown it was cut from: another markdown of version 8 is
	// parsed, and its cut replaces the one kept.
	if _, held := memo.get(key(8), refused+"Rewritten.\n"); held {
		t.Fatal("the memo serves version 8's cut for another markdown")
	}
	if cut := memo.cut(key(8), refused+"Rewritten.\n"); parsed != 2 || cut.markdown != refused+"Rewritten.\n" {
		t.Fatalf("the cut of version 8's other markdown = %#v after %d parses, want it parsed again", cut, parsed)
	}
	holds("[8 6]")

	// The document as it stands is kept under its live key, apart from every stored version's: its
	// cut is no cut of version 0, the number that names the time before the document's first
	// version, though the live key leaves its own number at 0. Kept, it drops version 6, the least
	// recently used, and a second cut of it parses nothing.
	live := documentCutKey{artifactID: artifactID, live: true}
	standing := markdown(9, markdownBytes)
	if cut := memo.cut(live, standing); parsed != 3 || cut.markdown != standing {
		t.Fatalf("the cut of the document as it stands = %#v after %d parses, want it parsed once", cut, parsed)
	}
	if _, held := memo.get(key(0), standing); held {
		t.Fatal("the memo serves the document as it stands under the key of version 0")
	}
	memo.cut(live, standing)
	if parsed != 3 {
		t.Fatalf("a second cut of the document as it stands parsed it again: %d parses, want 3", parsed)
	}
	holds("[live 8]")
}
