package pmdoc

import (
	"errors"
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
// Each shape is asserted four ways: the verdict a write of it gets; the steps the parse takes on
// it (ParseWork), no more than parseStepsPerCharacter for each of its characters; and, for every
// stored shape, growth in proportion to its length, counted at an eighth of what it grows to and
// then at the whole of it (growsLinearly, linear_test.go) - of the parse alone, and of the parse
// and the write of the document a write makes of the body together.
//
// The Playwright spec that wrote eight of these nine shapes through the API held each write to a
// second and to nothing about its growth: a ratio between two wall times a shared CI runner took,
// a doubling of the body apart, cannot separate a regression from the runner's own noise, which
// is what made that bound fail at random (INFRA-985). The growth claim is made here instead, in
// the parse's own count of its steps and at an eightfold step, and the verdict each write gets is
// gated in internal/dispatch/api (TestHeavyDocumentWriteVerdicts).
//
// A second is the browser's own bound - the longest a card's draw may hold the page's main thread
// (longestHoldMs, packages/dispatch/e2e/main-thread-hold.ts) - and six of the nine bodies here are
// the very bytes those rows draw, so those six keep their second there. This file held every
// shape to a second of this process's CPU time too, until a loaded two-processor run spent that
// second on the runner's load and its collections rather than the parse (INFRA-3149); it holds the
// parse's work instead, which no load moves. It is the guard over the parse's own costs as well -
// the block ids a document's blocks are minted from, and the tab-indentation check each open
// block runs on each line - which a regression would make superlinear again.

// Every shape at the block bound is stored or refused as it is, and the parse of it takes no more
// than parseStepsPerCharacter steps for each of its characters: a count linear in the body, which
// a parse whose steps grew with the square of a body's length passes by thousands of times at
// these lengths.
func TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedWithinParseStepsPerCharacter(t *testing.T) {
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
			before := ParseWork()
			_, err = ParseForWrite(body, nil)
			steps := ParseWork() - before
			if shape.Refusal == "" {
				if err != nil {
					t.Fatalf("%s: %v, want it stored", shape.Name, err)
				}
			} else if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), shape.Refusal) {
				t.Fatalf("%s: %v, want ErrSchema saying %q", shape.Name, err, shape.Refusal)
			}
			allowed := parseStepsPerCharacter * uint64(len(body))
			t.Logf("%s: %d characters in %d steps of the parse (at most %d)", shape.Name, len(body), steps, allowed)
			if steps > allowed {
				t.Fatalf("%s: the parse took %d steps for a body of %d characters, past the %d allowed (%d a character) to a parse linear in its body",
					shape.Name, steps, len(body), allowed, parseStepsPerCharacter)
			}
		})
	}
}

// parseStepsPerCharacter is how many steps (ParseWork) the parse may take for each character of a
// heavy body. Every step is a line, a character or a run the parse reads, or a node or an event
// it walks, and a parse that reads each once takes a few for each character for each of the
// containers open around it, which maxNesting bounds: a list's spread is read over the events of
// every list it holds as well as its own (prepareList). The shapes that leave 49 lists open over
// thirty thousand blank lines took 92 and 100 steps a character, the others 1 to 7. A parse that
// read a body's lines again for each line it read would take thousands a character at these
// lengths.
const parseStepsPerCharacter = 2 * maxNesting

// Every shape a write stores parses in work proportional to its length, counted at an eighth of
// what the shape grows and then at the whole of it: a shape whose cost grew with the square of
// its length would grow growthStep² times where a linear one may grow linearSlack*growthStep
// (growthStep, linear_test.go).
func TestHeavyBodiesAtTheBlockCapParseInWorkProportionalToTheirLength(t *testing.T) {
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal != "" {
			continue
		}
		t.Run(shape.Name, func(t *testing.T) {
			sizes := []int{shape.Grown / growthStep, shape.Grown}
			growsLinearly(t, "parsing "+shape.Name, sizes, linearSlack, func(size int) uint64 {
				body := shape.Body(size)
				var err error
				work := workOf(func() { _, err = ParseForWrite(body, nil) })
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
// that has grown quadratically before (linear_test.go holds the shapes that did), and the write is
// counted beside it (UpdateWork) because it is the rest of what an API write does with a body,
// which the end-to-end bound this test replaced used to cover (INFRA-985). The encode is ygo's,
// which counts nothing of its own; it is run, so a write that encodes no update still fails.
//
// Each size gets a document of its own, since a write changes the document it writes into, and
// the body it writes is built before the count starts.
func TestHeavyBodiesAtTheBodyCapParseAndEncodeInWorkProportionalToTheirLength(t *testing.T) {
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal != "" {
			continue
		}
		t.Run(shape.Name, func(t *testing.T) {
			sizes := []int{shape.Grown / growthStep, shape.Grown}
			growsLinearly(t, "writing "+shape.Name, sizes, linearSlack, func(size int) uint64 {
				body := shape.Body(size)
				// "prosemirror" is the fragment the document service writes a document's blocks
				// into (fragmentName, internal/dispatch/docs/tree.go).
				doc := crdt.New()
				fragment := doc.GetXmlFragment("prosemirror")
				var err error
				var update []byte
				work := workOf(func() {
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
