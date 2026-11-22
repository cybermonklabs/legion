package pmdoc

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/heavybodies"
)

// The bodies a reader's browser tab froze on (sjawhar/legion#2519) are read here through the
// parser a caller's write takes, each at the length heavybodies grows it to - the longest one
// block of a body may be, but for the marker chain at 31,999 characters and the two runs of
// brackets at 30,000: a plain list of 8,000 items; a quote and a list item each followed by
// lines that carry no marker of their own; a run of brackets; 49 items nested by indentation and
// then blank lines, and 100 items nested the same way; 49 markers on one line and then blank
// lines; a chain of list markers on one line; and a typed fence whose label opens a run of
// brackets. Each is stored or refused here as it is written to be, and six
// of the nine are the very same bytes the browser suite draws in a page (e2e/heavy-bodies.ts,
// held to these by heavybodies/testdata/shared-bodies.json), so the two read one body, not two.
//
// The shapes themselves are heavybodies.Shapes (pmdoc/heavybodies), not this file: the API's own
// test of the verdict each write gets reads the same bodies from there, and a _test.go file of
// this package cannot be imported (INFRA-1404).
//
// Each shape is asserted four ways: the verdict a write of it gets; the work the parse does on it,
// no more than a bound for each of its characters (parseStepsPerCharacter, parseBytesPerCharacter);
// and, for every stored shape, growth of that work in proportion to its length, counted at an
// eighth of what it grows to and then at the whole of it (workGrowsLinearly, below) - of the parse
// alone, and of the parse, the write of the document a write makes of the body and its encode
// together.
//
// The work is two counts, which no load on the machine, no race detector and no collection moves:
// the steps the parse and the write take (ParseWork and UpdateWork, work.go), each a line, a
// character, a run, a node, an event or a fragment item they read, and the bytes the call
// allocates on the heap (workOf). The parse costs that grew with the square of a body's length
// before grew in the steps (linear_test.go holds the shapes that did), and a call that allocates
// without bound grows the bytes wherever in the call it does so, ygo's encode and goldmark's own
// code included, which count no steps.
//
// The Playwright spec that wrote eight of these nine shapes through the API held each write to a
// second and to nothing about its growth: a ratio between two wall times a shared CI runner took,
// a doubling of the body apart, cannot separate a regression from the runner's own noise, which
// is what made that bound fail at random (INFRA-985). This file then held each shape to a second
// of this process's CPU time and each growth to a ratio of two CPU times with the collector
// stopped, and those failed at random too (INFRA-3904): a race-detector run of every envoy
// package at a load of about 200 spent 1.179 s of CPU time on the plain list, and in a pod whose
// parse ran faster than another's a timed window held more runs, whose allocations reached the
// window's memory limit and ran a collection inside it, which the window refuses. The verdict each
// write gets is gated in internal/dispatch/api (TestHeavyDocumentWriteVerdicts), and six of the
// nine bodies keep the browser's own second in the browser suite - the longest a card's draw may
// hold the page's main thread (longestHoldMs, packages/dispatch/e2e/main-thread-hold.ts).
//
// What the counts do not hold:
//
//   - CPU time that grows with no step counted and no byte allocated. The tab-indentation check
//     each open block runs on each line (tabIndented) is such a cost: asked of the whole source
//     each time rather than read from the parse's context (tabFree), it took the 49-deep shapes
//     1.7 s of CPU time each at the cap against 0.3 to 0.4 s, which the one-second budget this
//     file held failed and these counts pass. So would a read of the kernel's randomness for each
//     block id (blockIDEntropy), or any scan by code that counts no step.
//   - The run of brackets in its steps: goldmark's link parser reads it, and counts none (17 for
//     the whole run). Its bytes hold it here, and the walk of it that once grew with the square of
//     the run is held by its nodes (TestARunOfBracketsTheirClosersTurnBackIntoTextIsOneText).
//   - Bytes that grow with the square of a body while they are still few beside the parse's own:
//     the parse allocates hundreds to thousands of bytes a character, so a growth past linearSlack
//     shows only once such bytes at an eighth of the cap pass about two fifths of the parse's own
//     there (countGrowthVerdict's bound), and past parseBytesPerCharacter only once they pass it.

// Every shape at the block bound is stored or refused as it is, and the parse of it takes no more
// than parseStepsPerCharacter steps and allocates no more than parseBytesPerCharacter bytes for
// each of its characters: counts linear in the body. A parse that walked a list's events back to
// its start for each item it opened took the plain list 192 million steps, 30 times its bound, and
// one that copied them for each item allocated 6.3 GB, 3 times its bound.
func TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedWithinTheirStepsAndBytesPerCharacter(t *testing.T) {
	// The shapes spell their refusals with heavybodies.MaxNesting, which is this package's own
	// maxNesting copied: heavybodies may not import pmdoc, since this test is in-package, so the
	// two are held to each other here rather than by the compiler.
	if heavybodies.MaxNesting != maxNesting {
		t.Fatalf("heavybodies.MaxNesting = %d, want pmdoc's maxNesting %d", heavybodies.MaxNesting, maxNesting)
	}
	for _, shape := range heavybodies.Shapes() {
		t.Run(shape.Name, func(t *testing.T) {
			body := shape.Grow()
			if len(body) > heavybodies.BlockCap {
				t.Fatalf("%s is %d characters, past the %d one block may hold", shape.Name, len(body), heavybodies.BlockCap)
			}
			var err error
			work := workOf(t, "parsing "+shape.Name, func() { _, err = ParseForWrite(body, nil) })
			if shape.Refusal == "" {
				if err != nil {
					t.Fatalf("%s: %v, want it stored", shape.Name, err)
				}
			} else if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), shape.Refusal) {
				t.Fatalf("%s: %v, want ErrSchema saying %q", shape.Name, err, shape.Refusal)
			}
			characters := uint64(len(body))
			steps, bytes := parseStepsPerCharacter*characters, parseBytesPerCharacter*characters
			t.Logf("%s: %d characters in %d steps of the parse (at most %d) and %d bytes allocated (at most %d)",
				shape.Name, len(body), work.steps, steps, work.bytes, bytes)
			if work.steps > steps {
				t.Errorf("%s: the parse took %d steps for a body of %d characters, past the %d allowed (%d a character) to a parse linear in its body",
					shape.Name, work.steps, len(body), steps, parseStepsPerCharacter)
			}
			if work.bytes > bytes {
				t.Errorf("%s: the parse allocated %d bytes for a body of %d characters, past the %d allowed (%d a character) to a parse linear in its body",
					shape.Name, work.bytes, len(body), bytes, parseBytesPerCharacter)
			}
		})
	}
}

// parseStepsPerCharacter is how many steps (ParseWork) the parse may take for each character of a
// heavy body. Every step is a line, a character or a run the parse reads, or a node or an event
// it walks, and a parse that reads each once takes a few for each character for each of the
// containers open around it, which maxNesting bounds: a list's spread is read over the events of
// every list it holds as well as its own (prepareList). At the cap the shapes that leave 49 lists
// open over thousands of blank lines took 93 and 100 steps a character, the plain list 7, the
// quote and the list item followed by unmarked lines 4.5 and 5.5, and the run of brackets and the
// three refused shapes 2 or fewer. A parse that read a body's lines again for each line it read
// would take thousands a character at these lengths.
const parseStepsPerCharacter = 2 * maxNesting

// parseBytesPerCharacter is how many bytes the parse may allocate on the heap for each character
// of a heavy body (workOf). At the cap the shapes that leave 49 lists open over thousands of blank
// lines allocated 13,468 and 13,458 bytes a character, the 100 items nested the same way 8,447
// before their refusal, the run of brackets 2,652, the plain list 2,389, and the others 802 or
// fewer. A race build allocates more where the parse matches a regexp: the race detector makes
// sync.Pool drop a quarter of what is put back, and regexp keeps its backtracker's state there, so
// the plain list allocated 16,328 bytes a character in one. Twice that, rounded up to a power of
// two, is 64 KiB. A parse that copied a list's events for each item it opened allocated 196,364
// bytes a character on the plain list.
const parseBytesPerCharacter = 64 << 10

// Every shape a write stores parses in work proportional to its length, counted at an eighth of
// what the shape grows and then at the whole of it: a shape whose steps or allocations grew with
// the square of its length would grow growthStep² times where a linear one may grow
// linearSlack*growthStep (growthStep, linear_test.go).
func TestHeavyBodiesAtTheBlockCapParseInWorkProportionalToTheirLength(t *testing.T) {
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal != "" {
			continue
		}
		t.Run(shape.Name, func(t *testing.T) {
			sizes := []int{shape.Grown / growthStep, shape.Grown}
			workGrowsLinearly(t, "parsing "+shape.Name, sizes, linearSlack, func(size int) callWork {
				body := shape.Body(size)
				var err error
				work := workOf(t, "parsing "+shape.Name, func() { _, err = ParseForWrite(body, nil) })
				if err != nil {
					t.Fatalf("parsing %d characters of %s: %v", size, shape.Name, err)
				}
				return work
			})
		})
	}
}

// Every shape a write stores is parsed and written into a document in work proportional to its
// length, at the sizes the parse alone is counted at above: this is the whole of what a document
// write does with a body before it reaches Postgres - the parse of the markdown a caller sent and
// the write of that tree into a fresh document, encoded as the document's first durable update
// (parseReplacing and encodeDocumentTree, internal/dispatch/docs/tree.go). The parse is the part
// that has grown quadratically before (linear_test.go holds the shapes that did), and the write and
// the encode are counted beside it because they are the rest of what an API write does with a
// body, which the end-to-end bound this test replaced used to cover (INFRA-985): the write in its
// steps (UpdateWork) and its bytes, the encode, which is ygo's and counts no steps, in its bytes.
//
// Each size gets a document of its own, since a write changes the document it writes into, and
// the body it writes and the document are built before the count starts.
func TestHeavyBodiesAtTheBodyCapParseAndEncodeInWorkProportionalToTheirLength(t *testing.T) {
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal != "" {
			continue
		}
		t.Run(shape.Name, func(t *testing.T) {
			sizes := []int{shape.Grown / growthStep, shape.Grown}
			workGrowsLinearly(t, "writing "+shape.Name, sizes, linearSlack, func(size int) callWork {
				body := shape.Body(size)
				// "prosemirror" is the fragment the document service writes a document's blocks
				// into (fragmentName, internal/dispatch/docs/tree.go).
				doc := crdt.New()
				fragment := doc.GetXmlFragment("prosemirror")
				var err error
				var update []byte
				work := workOf(t, "writing "+shape.Name, func() {
					tree, parseErr := ParseForWrite(body, nil)
					if parseErr != nil {
						err = parseErr
						return
					}
					if err = doc.TransactE(func(txn *crdt.Transaction) error {
						return Update(txn, fragment, tree)
					}); err != nil {
						return
					}
					update = crdt.EncodeStateAsUpdateV1(doc, nil)
				})
				if err != nil {
					t.Fatalf("writing %d characters of %s: %v", size, shape.Name, err)
				}
				if len(update) == 0 {
					t.Fatalf("writing %d characters of %s encoded no update", size, shape.Name)
				}
				return work
			})
		})
	}
}

// callWork is what one call costs in the two counts this file holds a parse and a write to: the
// steps the package counts of its own work (ParseWork and UpdateWork, work.go) and the bytes the
// call allocates on the heap.
type callWork struct {
	steps, bytes uint64
}

// workOf is the work op does, labelled what: the steps ParseWork and UpdateWork count and the bytes
// runtime.MemStats.TotalAlloc counts between a reading before op and one after it. ReadMemStats
// stops the world and flushes every processor's cache of spans before it reads, so the bytes are
// every allocation made between the two readings, whether or not a collection ran between them.
// The steps are the same in every run of a body. The bytes are too but for what sync.Pool hands
// back: a collection empties it, and a race build drops a quarter of what is put back, so a pooled
// object is made again (parseBytesPerCharacter has the measure).
//
// Both counts are the process's, so nothing else may run beside op: no test in this package calls
// t.Parallel, and a timed call of linear_test.go still running past its deadline (leakedCallGuard)
// fails t before op runs.
func workOf(t *testing.T, what string, op func()) callWork {
	t.Helper()
	if leaking := leakedCallGuard(what); leaking != "" {
		t.Fatal(leaking)
	}
	var before, after runtime.MemStats
	steps := ParseWork() + UpdateWork()
	runtime.ReadMemStats(&before)
	op()
	runtime.ReadMemStats(&after)
	return callWork{steps: ParseWork() + UpdateWork() - steps, bytes: after.TotalAlloc - before.TotalAlloc}
}

// workGrowsLinearly counts the work measure does at each of sizes in turn, once each, since a
// count is the work itself and needs no sample, and fails t at the first size whose steps or
// whose bytes grew more than allowance times faster than the size did (countGrowthVerdict).
func workGrowsLinearly(t *testing.T, what string, sizes []int, allowance int, measure func(size int) callWork) {
	t.Helper()
	works := make([]callWork, len(sizes))
	for i, size := range sizes {
		works[i] = measure(size)
	}
	for _, count := range []struct {
		unit string
		of   func(callWork) uint64
	}{
		{"steps", func(w callWork) uint64 { return w.steps }},
		{"bytes allocated", func(w callWork) uint64 { return w.bytes }},
	} {
		counts := make([]uint64, len(works))
		for i, w := range works {
			counts[i] = count.of(w)
		}
		failedStep, lines := countGrowthVerdict(sizes, counts, allowance, count.unit)
		for step, line := range lines {
			if step == failedStep {
				t.Errorf("%s %s", what, line)
				break
			}
			t.Logf("%s: %s", what, line)
		}
	}
}

// countGrowthVerdict is workGrowsLinearly's verdict on one count without a *testing.T to report
// through: which step (0-based, among sizes[1:]) first grew past its bound, or -1 if none did,
// and one line for every step up to and including a failure. counts[i] is the count at sizes[i],
// and a step's bound is allowance times its growth times the count at the size before it, the
// bound growthVerdict (linear_test.go) puts on two times. A count of none at the first size is a
// failure at step 0, since a growth from nothing says nothing: none of the work is counted.
// TestCountGrowthVerdictCatchesQuadraticWorkAndRefusesACountOfNone calls this directly.
func countGrowthVerdict(sizes []int, counts []uint64, allowance int, unit string) (failedStep int, lines []string) {
	before := counts[0]
	if before == 0 {
		return 0, []string{fmt.Sprintf("counted no %s at %s, so its growth says nothing: none of the work it does is counted", unit, sizeText(sizes[0]))}
	}
	for i := 1; i < len(sizes); i++ {
		smaller, size := sizes[i-1], sizes[i]
		growth := size / smaller
		count := counts[i]
		allowed := uint64(allowance*growth) * before
		ratio := float64(count) / float64(before)
		if count > allowed {
			lines = append(lines, fmt.Sprintf("took %d %s at %s, %.1f times the %d it took at %s: the input grew %d times, and work linear in it may take %d at most",
				count, unit, sizeText(size), ratio, before, sizeText(smaller), growth, allowed))
			return i - 1, lines
		}
		lines = append(lines, fmt.Sprintf("%d %s at %s, %d at %s, %.1f times for an input %d times larger (at most %d)",
			before, unit, sizeText(smaller), count, sizeText(size), ratio, growth, allowed))
		before = count
	}
	return -1, lines
}

// countGrowthVerdict passes work that grows with its input at this file's growth step and slack,
// fails work that grows with its square there, and fails a count of none, which would pass any
// growth at all: the same arithmetic TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack
// proves of the timed verdict, on counts.
func TestCountGrowthVerdictCatchesQuadraticWorkAndRefusesACountOfNone(t *testing.T) {
	sizes := []int{4_000, 4_000 * growthStep}
	linear := func(size int) uint64 { return uint64(3*size + 500) }
	quadratic := func(size int) uint64 { return uint64(size) * uint64(size) / 64 }
	for _, test := range []struct {
		name   string
		counts []uint64
		failAt int
	}{
		{"linear work", []uint64{linear(sizes[0]), linear(sizes[1])}, -1},
		{"quadratic work", []uint64{quadratic(sizes[0]), quadratic(sizes[1])}, 0},
		{"no work counted", []uint64{0, linear(sizes[1])}, 0},
	} {
		failedStep, lines := countGrowthVerdict(sizes, test.counts, linearSlack, "steps")
		if failedStep != test.failAt {
			t.Errorf("%s: failed at step %d, want %d (%s)", test.name, failedStep, test.failAt, strings.Join(lines, "; "))
		}
		t.Logf("%s: %s", test.name, strings.Join(lines, "; "))
	}
}
