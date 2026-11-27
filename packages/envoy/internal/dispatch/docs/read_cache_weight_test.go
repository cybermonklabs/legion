//go:build !race

// This file measures the heap a document read holds, which the race build must not compile: the
// detector's shadow memory and instrumented allocations change what the Go heap holds, so the
// measurement says nothing about the read (upload_memory_test.go's rule). The envoy-go job runs
// it without the detector. Measured: the ask-block shape's read holds 3,780,400 B in the plain
// build, and two race runs in CI of trees whose plain builds hold that same figure read
// 3,900,384 B and 3,990,560 B, which turned a ratio of 1.00 into 0.97 and 0.95.

package docs

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A cached read's weight is what the cache's budget counts it as, so for every shape of document
// it is within a stated factor of the heap the read holds: a table whose header cells are long,
// which every cell's position points into; a wide table, whose row's cells every cell's position
// shares; a table of many rows, whose block lists every block it holds; a deep document, whose
// every block's path repeats the blocks above it; prose, a block a line; and a document of ask
// blocks, whose read keeps the tree it was rendered from (documentRead.tree). The weight may fall
// short of the heap only by the allocator's rounding, which it does not model: an object past 32
// KiB takes whole 8 KiB pages (minWeightToHeap). It counts a string at each part of the read that
// holds it, a part several share once, and a map's slots at the load the map holds just after it
// grows, so it overcounts by at most a quarter (maxWeightToHeap); a part counted for each sharer,
// such as a row's cells counted for each cell, passes that.
func TestADocumentReadWeighsAboutTheHeapItHolds(t *testing.T) {
	// Not parallel: it reads the whole process's heap (runtime.ReadMemStats HeapAlloc around
	// runtime.GC, in heldByDocumentRead) as what one document read holds, which the heap of the
	// tests beside it would add to.
	const minWeightToHeap, maxWeightToHeap = 0.97, 1.25
	for _, shape := range []struct{ name, markdown string }{
		{"a table whose header cells are long", longHeaderTable()},
		{"a wide table", wideTable()},
		{"a table of many rows", manyRowTable()},
		{"a deep document", strings.Repeat("> ", 90) + "deep\n"},
		{"prose", prose()},
		{"ask blocks", askBlocks()},
	} {
		t.Run(shape.name, func(t *testing.T) {
			read, held := heldByDocumentRead(t, shape.markdown)
			ratio := float64(read.weight) / float64(held)
			t.Logf("weight %d B, heap held %d B (weight %.2fx the heap)", read.weight, held, ratio)
			if ratio < minWeightToHeap || ratio > maxWeightToHeap {
				t.Errorf("weight %d B is %.2fx the %d B the read holds, want %.2fx to %.2fx", read.weight, ratio, held, minWeightToHeap, maxWeightToHeap)
			}
		})
	}
}

// heldByDocumentRead builds markdown's read and returns it with the heap it holds: what stays
// allocated once collections have freed everything else its parse and its build made. A first
// build of the same markdown, discarded, keeps the parser's one-time setup out of the count; each
// reading follows two collections, since a sync.Pool's contents survive the first; and the least
// of three builds is taken, since what anything else allocates meanwhile only adds to a reading.
func heldByDocumentRead(t *testing.T, markdown string) (*documentRead, int64) {
	t.Helper()
	buildTestDocumentRead(t, markdown)
	var read *documentRead
	held := int64(math.MaxInt64)
	for range 3 {
		read = nil
		var before, after runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&before)
		read = buildTestDocumentRead(t, markdown)
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&after)
		held = min(held, int64(after.HeapAlloc)-int64(before.HeapAlloc))
	}
	return read, held
}

// buildTestDocumentRead builds markdown's read as a cold read does: from the tree of a document
// decoded from its stored state, which holds strings of its own rather than the markdown's.
func buildTestDocumentRead(t *testing.T, markdown string) *documentRead {
	t.Helper()
	state := newProofHistory(t, 1).write(markdown)
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatalf("decode the stored state: %v", err)
	}
	fold, err := foldLoaded("artifact", LoadedDocument{Doc: doc, Stamp: DocumentStamp{Version: 1, Row: "1"}})
	if fold.treeErr != nil {
		t.Fatalf("read the tree: %v", fold.treeErr)
	}
	if err != nil {
		t.Fatalf("build the read: %v", err)
	}
	return fold.read
}

// longHeaderTable is a table of eight columns whose header cells hold about 28 KiB of text each.
func longHeaderTable() string {
	var b strings.Builder
	b.WriteString("|")
	for range 8 {
		b.WriteString(" " + strings.Repeat("header ", 4<<10) + "|")
	}
	b.WriteString("\n|" + strings.Repeat(" - |", 8) + "\n|" + strings.Repeat(" cell |", 8) + "\n")
	return b.String()
}

// manyRowTable is a table of four columns and 2,000 body rows.
func manyRowTable() string {
	return "| a | b | c | d |\n| - | - | - | - |\n" + strings.Repeat("| w | x | y | z |\n", 2_000)
}

// prose is a thousand headings, each over one paragraph.
func prose() string {
	var b strings.Builder
	for index := range 1_000 {
		fmt.Fprintf(&b, "## Heading %d\n\nA paragraph of prose under heading %d.\n\n", index, index)
	}
	return b.String()
}

// askBlocks is five hundred ask blocks, each a question and two options.
func askBlocks() string {
	var b strings.Builder
	for index := range 500 {
		fmt.Fprintf(&b, ":::ask{#ask-%d urgency=\"med\" multiple=\"false\"}\nWhich way does part %d go?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n\n", index, index)
	}
	return b.String()
}

// wideTable is a table of one header row and one body row of 1,000 cells each.
func wideTable() string {
	var b strings.Builder
	b.WriteString("|")
	for index := range 1_000 {
		fmt.Fprintf(&b, " h%d |", index)
	}
	b.WriteString("\n|" + strings.Repeat(" - |", 1_000) + "\n|")
	for index := range 1_000 {
		fmt.Fprintf(&b, " cell %d |", index)
	}
	b.WriteString("\n")
	return b.String()
}
