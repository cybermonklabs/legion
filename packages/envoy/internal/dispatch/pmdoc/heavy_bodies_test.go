package pmdoc

import (
	"errors"
	"math"
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
// brackets. Two more froze no tab: footnote references and then their definitions, which pmdoc
// resolves from maps of the definitions where goldmark walked them for each reference, and a line
// of bangs each before a letter and a caret, which goldmark's reference parser read as carets and
// scanned the line from (heavybodies.Shapes has why). Each is stored or refused here as it is
// written to be, and six of the eleven are the very same bytes the browser suite draws in a page
// (e2e/heavy-bodies.ts, held to these by heavybodies/testdata/shared-bodies.json), so the two read
// one body, not two.
//
// The shapes themselves are heavybodies.Shapes (pmdoc/heavybodies), not this file: the API's own
// test of the verdict each write gets reads the same bodies from there, and a _test.go file of
// this package cannot be imported (INFRA-1404).
//
// Each shape is asserted at an eighth of what it grows to and at the whole of it: the verdict a
// write of it gets, and the parse's work, counted by Go's coverage counters in pmdoc and every
// package it imports outside the standard library (counted_work_test.go), held linear in the
// body's length; and, for every stored shape, the work of the parse and of the write of the
// document a write makes of the body together, held the same way. Each shape is also counted at
// the whole of what it grows to and at pastCapLengthOf times it: its read as a rendering, which
// every write's parse shares and which no element limit stops, and, for every stored shape, its
// whole write on a budget that counts no elements (wholeWriteBudget). There a quadratic part of
// the work that is small beside the rest at the cap grows past what linear work may. Every count
// is also held under a bound for each character the same for every shape (statementsPerCharacter,
// bytesPerCharacter), beside each shape's own floor and ceiling.
//
// The Playwright spec that wrote eight of these shapes through the API held each write to a
// second and to nothing about its growth: a ratio between two wall times a shared CI runner took,
// a doubling of the body apart, cannot separate a regression from the runner's own noise, which
// is what made that bound fail at random (INFRA-985). The growth claim is made here instead, in
// counted work at an eightfold step, and the verdict each write gets is gated in
// internal/dispatch/api (TestHeavyDocumentWriteVerdicts).
//
// A second is the browser's own bound - the longest a card's draw may hold the page's main thread
// (longestHoldMs, packages/dispatch/e2e/main-thread-hold.ts) - and six of the eleven bodies here
// are the very bytes those rows draw, so those six keep their second there. This file held every
// shape to a second of this process's CPU time too, until a loaded two-processor run spent that
// second on the runner's load and its collections rather than the parse (INFRA-3149). It then held
// the steps the parse counted of itself (ParseWork), which counted 17 steps for the 30,000
// characters of the run of brackets, whose parse is goldmark's link parser and imageNesting.Parse:
// a quadratic walk planted there passed (dispatch://INFRA-3149, the parsework-brackets-finding
// artifact). It holds the work the coverage counters count now, which no load moves and no loop of
// a counted package escapes, beside the bytes the parse allocates, which a regression that built a
// string or a list again for each line or block in the standard library would make grow with the
// square of the body. The read and the whole write past the cap were held to CPU time growing no
// faster than linear work's (INFRA-3904); they are counted instead, and what CPU time saw there
// and no count sees is the one gap counted_work_test.go's header names: superlinear work inside a
// standard library call that allocates nothing and that no searchedBytes count sits on.

// Every shape at the block bound is stored or refused as it is, at both sizes, and the parse of it
// counts work linear in its characters (countedWorkIsLinear): its growth from an eighth of the
// body to the whole, a floor a character past the eighth and a ceiling a character at each size,
// each bound set from the shape's reading in heavyBodyParseReadings. Its read as a rendering is
// counted the same way at the whole and at pastCapLengthOf times it, with its readings in
// heavyBodyReadReadings. Every count is held under statementsPerCharacter and bytesPerCharacter.
func TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedWithinParseStepsPerCharacter(t *testing.T) {
	// The shapes spell their refusals with heavybodies.MaxNesting, which is this package's own
	// maxNesting copied: heavybodies may not import pmdoc, since this test is in-package, so the
	// two are held to each other here rather than by the compiler.
	if heavybodies.MaxNesting != maxNesting {
		t.Fatalf("heavybodies.MaxNesting = %d, want pmdoc's maxNesting %d", heavybodies.MaxNesting, maxNesting)
	}
	row := t.Name()
	for _, shape := range heavyBodyParseShapes() {
		t.Run(shape.name, func(t *testing.T) {
			holdsCountsPerCharacter(t, shape, countedWorkIsLinear(t, row, shape))
		})
	}
}

// heavyBodyParseShapes are the heavy bodies' parses, each at an eighth of what it grows to and at
// the whole of it, then their reads as a rendering, each at the whole and at pastCapLengthOf times
// it, each run checking the verdict the write gets. A shape's units are the characters it repeats
// (heavybodies.Shape's Grown), so the fixed opening a nested shape indents before its blank lines
// is no unit of its growth.
func heavyBodyParseShapes() []countedShape {
	var shapes []countedShape
	for _, shape := range heavybodies.Shapes() {
		shapes = append(shapes, countedShape{
			name:    shape.Name,
			sizes:   [2]int{shape.Grown / growthStep, shape.Grown},
			unit:    "repeated character",
			reading: heavyBodyParseReadings[shape.ID],
			stage:   searchedBytes,
			prepare: func(t *testing.T, size int) (int, func()) {
				body := heavyBody(t, shape, size)
				return size, func() {
					_, err := ParseForWrite(body, nil)
					requireHeavyVerdict(t, shape, len(body), err)
				}
			},
		})
	}
	for _, shape := range heavybodies.Shapes() {
		shapes = append(shapes, countedShape{
			name:    shape.Name + ", read as a rendering",
			sizes:   [2]int{shape.Grown, pastCapLengthOf(shape) * shape.Grown},
			unit:    "repeated character",
			reading: heavyBodyReadReadings[shape.ID],
			stage:   searchedBytes,
			prepare: func(t *testing.T, size int) (int, func()) {
				body := shape.Body(size)
				return size, func() {
					_, err := ParseRendering(body)
					requireHeavyVerdict(t, shape, len(body), err)
				}
			},
		})
	}
	return shapes
}

// heavyBodyParseReadings are the readings (countedReading) of each heavy body's parse, by its ID,
// taken on 2026-11-20 in INFRA-3149's pod (Go 1.27.3) at an eighth of each shape's Grown and at
// all of it, counting every package pmdoc imports outside the standard library: past is the
// statements each repeated character past the smaller size cost, most the statements a repeated
// character cost at whichever size cost more. Each shape's floor is a quarter of past and its
// ceiling four times most (countedMargin). The run of brackets counted 7,679,344 statements over
// 3,750 characters and 74,564,344 over 30,000, 9.71 times over 8 times the characters, linear
// with a fixed term below zero: each character past 3,750 cost 2,548.0, as each past 7,500 and
// past 15,000 did, and 70.8 million of the larger count were goldmark's. goldmark refuses a
// closer outright while more than 998 labels stand open, so of a run of n brackets only the last
// 998 closers copy their label (text/reader.go's Value) before refusing it for its length, which
// is 998 copies of up to n characters less the 998² the shorter ones save. ParseWork counted 17
// steps for that parse. The typed fence is refused at its line before any inline parse, at 1.0
// statements a character. The footnote references and the bangs, main's two shapes, were read on
// 2026-11-23 in the same pod, on main's reference parser (footnotes.go).
var heavyBodyParseReadings = map[string]countedReading{
	"plain-list":                           {past: 1532.3, most: 1533.3},
	"lazy-quote":                           {past: 572.0, most: 573.8},
	"lazy-list-item":                       {past: 610.5, most: 612.9},
	"bracket-run":                          {past: 2548.0, most: 2485.5},
	"nested-49-items-then-blank-lines":     {past: 9533.0, most: 10240.6},
	"markers-49-then-blank-lines":          {past: 9533.0, most: 9618.5},
	"nested-100-items-then-blank-lines":    {past: 8173.0, most: 10385.5},
	"marker-chain":                         {past: 673.0, most: 679.0},
	"callout-bracket-run":                  {past: 1.0, most: 1.7},
	"footnote-references-then-definitions": {past: 614.3, most: 614.3},
	"bangs-before-carets":                  {past: 304.0, most: 306.5},
}

// heavyBodyReadReadings are the readings (countedReading) of each heavy body's read as a
// rendering, by its ID, at all of its Grown and at pastCapLengthOf times it, taken on 2026-11-23
// in INFRA-3149's pod as heavyBodyParseReadings were. The footnote shape counted 7,743,232
// statements over 31,998 characters and 254,468,752 over 1,023,936, 32.86 times over 32 times the
// characters; the run of brackets 1,205.8 a character at 30,000 and 1,233.1 at 240,000, its fixed
// term below zero again (heavyBodyParseReadings).
var heavyBodyReadReadings = map[string]countedReading{
	"plain-list":                           {past: 630.0, most: 630.1},
	"lazy-quote":                           {past: 412.0, most: 412.1},
	"lazy-list-item":                       {past: 450.0, most: 450.2},
	"bracket-run":                          {past: 1237.0, most: 1233.1},
	"nested-49-items-then-blank-lines":     {past: 9484.0, most: 9527.1},
	"markers-49-then-blank-lines":          {past: 9484.0, most: 9488.7},
	"nested-100-items-then-blank-lines":    {past: 8123.0, most: 8399.2},
	"marker-chain":                         {past: 665.0, most: 665.7},
	"callout-bracket-run":                  {past: 1.0, most: 1.1},
	"footnote-references-then-definitions": {past: 248.7, most: 248.5},
	"bangs-before-carets":                  {past: 136.3, most: 136.5},
}

// pastCapLength is how many times its length each shape is also read, and each stored shape
// written whole, at: work quadratic in a body's length costs pastCapLength times more a character
// there than at its length, where linear work costs no more.
const pastCapLength = 8

// pastCapLengthOf is how many times its length shape is read and written whole at
// (pastCapLength), and footnotePastCapLength for the footnote references and their definitions.
func pastCapLengthOf(shape heavybodies.Shape) int {
	if shape.ID == "footnote-references-then-definitions" {
		return footnotePastCapLength
	}
	return pastCapLength
}

// footnotePastCapLength is how many times its length the footnote shape is read and written whole
// at. The resolution that walked every definition for each reference was a small part of the
// shape's work at its length, about a tenth of the whole write's CPU time, so at pastCapLength
// times it its read's CPU time a character grew 2.0 to 4.2 times and its whole write's 1.0 to 2.7,
// short of what quadratic work grows; at 32 times, with that resolution put back (footnotes.go of
// 33a29d1967), its read grew 21.9 to 27.6 times and its whole write 10.2 to 17.0 in each of four
// runs, and with this one 1.4 and 1.2 (INFRA-3904, measured as CPU time). A part that is a tenth
// of the work at the cap and grows with the square of the length makes the work a character 4.1
// times at 32 times the length, and the work 131 times, past the linearSlack times 32 that linear
// work may grow.
const footnotePastCapLength = 32

// statementsPerCharacter is how many statements (countedWorkIsLinear) a parse, a read or a write
// may execute for each repeated character of a heavy body, at every size it is counted at. A parse
// that reads each piece of its input a few times executes some tens of statements for it for each
// of the containers open around it, which maxNesting bounds: at the cap the shapes that leave 49
// lists open over thousands of blank lines counted most 10,240.6 and 9,618.5 statements a
// character in a parse (heavyBodyParseReadings), the 100 items nested the same way 10,385.5, and
// the others 2,548.0 or fewer. 200 for each container maxNesting allows is about twice the most.
// It bounds every shape alike, where each shape's ceiling is four times its own reading.
const statementsPerCharacter = 200 * maxNesting

// bytesPerCharacter is how many bytes a parse, a read or a write may allocate on the heap for each
// repeated character of a heavy body (allocatedBy), at every size it is counted at. At the cap the
// shapes that leave 49 lists open over thousands of blank lines allocated 13,469 and 13,458 bytes a
// character in a parse and 13,485 and 13,472 in a write, the 100 items nested the same way 8,444
// before their refusal, the run of brackets 2,652 and 2,657, the plain list 2,389 and 4,217, and
// the others 1,247 or fewer (INFRA-3904). A race build allocates more where the parse matches a
// regexp, the plain list's parse 16,328 bytes a character in one; the counted build is never a race
// build (buildCounted). Twice that, rounded up to a power of two, is 64 KiB. A parse that copied a
// list's events for each item it opened allocated 196,364 bytes a character on the plain list.
const bytesPerCharacter = 64 << 10

// holdsCountsPerCharacter fails t where a counted run of shape executed more than
// statementsPerCharacter statements or allocated more than bytesPerCharacter bytes for each of the
// units it read.
func holdsCountsPerCharacter(t *testing.T, shape countedShape, runs [2]counted) {
	t.Helper()
	for _, run := range runs {
		units := int64(run.units)
		if run.work > statementsPerCharacter*units {
			t.Errorf("%s: executed %d statements for %d %ss, past the %d allowed (%d a %s)",
				shape.name, run.work, run.units, shape.unit, statementsPerCharacter*units, statementsPerCharacter, shape.unit)
		}
		if run.bytes > uint64(bytesPerCharacter*units) {
			t.Errorf("%s: allocated %d bytes for %d %ss, past the %d allowed (%d a %s)",
				shape.name, run.bytes, run.units, shape.unit, bytesPerCharacter*units, bytesPerCharacter, shape.unit)
		}
	}
}

// heavyBody is shape at size, which may be no longer than the block cap.
func heavyBody(t *testing.T, shape heavybodies.Shape, size int) string {
	t.Helper()
	body := shape.Body(size)
	if len(body) > heavybodies.BlockCap {
		t.Fatalf("%s is %d characters, past the %d one block may hold", shape.Name, len(body), heavybodies.BlockCap)
	}
	return body
}

// requireHeavyVerdict fails t unless err is the verdict a write of shape gets: stored, or refused
// as it is written to be.
func requireHeavyVerdict(t *testing.T, shape heavybodies.Shape, characters int, err error) {
	t.Helper()
	if shape.Refusal == "" {
		if err != nil {
			t.Fatalf("%s at %d characters: %v, want it stored", shape.Name, characters, err)
		}
	} else if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), shape.Refusal) {
		t.Fatalf("%s at %d characters: %v, want ErrSchema saying %q", shape.Name, characters, err, shape.Refusal)
	}
}

// Every shape a write stores is parsed and written into a document in work linear in its length,
// at the sizes the parse alone is counted at above: this is the whole of what a document write
// does with a body before it reaches Postgres - the parse of the markdown a caller sent, with the
// read-back of its rendering that refuses a document stored otherwise than it was written
// (ParseWritten), and the write of that tree into a fresh document, encoded as the document's
// first durable update (parseReplacing and encodeDocumentTree, internal/dispatch/docs/tree.go).
// The parse is the part that has grown quadratically before (linear_test.go holds the shapes that
// did), and the write and the encode are counted beside it because they are the rest of what an
// API write does with a body, which the end-to-end bound this test replaced used to cover
// (INFRA-985). The write and the encode are ygo's as much as this package's, and the counters
// count ygo's packages too, so an encode or an insert that walked the document again for each item
// it wrote grows the count with the square of the document; an encode that copied its buffer again
// for each item, through the standard library's copy, grows the bytes the write allocates instead,
// which the row judges beside the count. A write that encodes no update fails. The same write on a
// budget that counts no elements (wholeWriteBudget) is counted at the whole of what the shape grows
// to and at pastCapLengthOf times it, where a write that long passes the elements one write may
// make, with its readings in heavyBodyWholeWriteReadings. Every count is held under
// statementsPerCharacter and bytesPerCharacter.
//
// Each size gets a document of its own, made before the count starts, since a write changes the
// document it writes into.
func TestHeavyBodiesAtTheBodyCapParseAndEncodeInWorkProportionalToTheirLength(t *testing.T) {
	row := t.Name()
	for _, shape := range heavyBodyWriteShapes() {
		t.Run(shape.name, func(t *testing.T) {
			holdsCountsPerCharacter(t, shape, countedWorkIsLinear(t, row, shape))
		})
	}
}

// heavyBodyWriteShapes are the stored heavy bodies' parses, each followed by the write of its tree
// into a fresh document and the encode of that document's update, in the units heavyBodyParseShapes
// counts; then their whole writes, at the whole and at pastCapLengthOf times it.
func heavyBodyWriteShapes() []countedShape {
	var shapes []countedShape
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal != "" {
			continue
		}
		shapes = append(shapes, countedShape{
			name:    shape.Name,
			sizes:   [2]int{shape.Grown / growthStep, shape.Grown},
			unit:    "repeated character",
			reading: heavyBodyWriteReadings[shape.ID],
			stage:   searchedBytes,
			prepare: func(t *testing.T, size int) (int, func()) {
				body := heavyBody(t, shape, size)
				return size, heavyWrite(t, shape, body, func() (*Node, error) { return ParseForWrite(body, nil) })
			},
		})
	}
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal != "" {
			continue
		}
		shapes = append(shapes, countedShape{
			name:    shape.Name + ", written whole",
			sizes:   [2]int{shape.Grown, pastCapLengthOf(shape) * shape.Grown},
			unit:    "repeated character",
			reading: heavyBodyWholeWriteReadings[shape.ID],
			stage:   searchedBytes,
			prepare: func(t *testing.T, size int) (int, func()) {
				body := shape.Body(size)
				// ParseWritten's steps, on a budget that counts no elements.
				return size, heavyWrite(t, shape, body, func() (*Node, error) {
					tree, err := parseForWrite(body, nil, true, wholeWriteBudget())
					if err != nil {
						return nil, err
					}
					_, misread, refused, err := documentMisread(tree)
					if err := refuseMisread(misread, refused, err); err != nil {
						return nil, err
					}
					return tree, nil
				})
			},
		})
	}
	return shapes
}

// heavyWrite is the op of a write of body: parse's tree written into a fresh document of
// heavyClientID, made here and so before the count starts, and that document's update encoded. It
// fails t where the parse or the write fails or the encode encodes nothing.
func heavyWrite(t *testing.T, shape heavybodies.Shape, body string, parse func() (*Node, error)) func() {
	t.Helper()
	// "prosemirror" is the fragment the document service writes a document's blocks into
	// (fragmentName, internal/dispatch/docs/tree.go).
	doc := crdt.New(crdt.WithClientID(heavyClientID))
	fragment := doc.GetXmlFragment("prosemirror")
	return func() {
		tree, err := parse()
		if err != nil {
			t.Fatalf("parsing %d characters of %s: %v", len(body), shape.Name, err)
		}
		if err := doc.TransactE(func(txn *crdt.Transaction) error { return Update(txn, fragment, tree) }); err != nil {
			t.Fatalf("writing %d characters of %s: %v", len(body), shape.Name, err)
		}
		if update := crdt.EncodeStateAsUpdateV1(doc, nil); len(update) == 0 {
			t.Fatalf("writing %d characters of %s encoded no update", len(body), shape.Name)
		}
	}
}

// heavyClientID is the client id a write's document takes: ygo gives each document a random 32-bit
// one, whose varints are one to five bytes long, so a write's statements moved by up to 0.22%
// from run to run. This is the largest of them, whose varints are the longest a random id makes.
const heavyClientID = crdt.ClientID(math.MaxUint32)

// wholeWriteBudget is a caller write's budget (NewWriteBudget) counting no elements, as a
// read-back's counts none: the whole write on it does every step a write does, at a length the
// elements a write may make would refuse.
func wholeWriteBudget() *WriteBudget {
	budget := NewWriteBudget()
	budget.elements = newElementCount(0)
	return budget
}

// heavyBodyWriteReadings are the readings (countedReading) of each stored heavy body's parse,
// write and encode, by its ID, taken as heavyBodyParseReadings were, main's two shapes on
// 2026-11-23.
var heavyBodyWriteReadings = map[string]countedReading{
	"plain-list":                           {past: 2045.5, most: 2045.1},
	"lazy-quote":                           {past: 574.0, most: 576.2},
	"lazy-list-item":                       {past: 612.5, most: 615.7},
	"bracket-run":                          {past: 2550.0, most: 2487.5},
	"nested-49-items-then-blank-lines":     {past: 9533.0, most: 10276.6},
	"markers-49-then-blank-lines":          {past: 9533.0, most: 9645.4},
	"footnote-references-then-definitions": {past: 721.8, most: 721.7},
	"bangs-before-carets":                  {past: 306.0, most: 309.2},
}

// heavyBodyWholeWriteReadings are the readings (countedReading) of each stored heavy body's whole
// write, by its ID, at all of its Grown and at pastCapLengthOf times it, taken on 2026-11-23 in
// INFRA-3149's pod as heavyBodyParseReadings were. The footnote shape counted 22,783,461
// statements over 31,998 characters and 746,355,966 over 1,023,936, 712.0 and 728.9 a character.
var heavyBodyWholeWriteReadings = map[string]countedReading{
	"plain-list":                           {past: 2019.5, most: 2019.4},
	"lazy-quote":                           {past: 541.5, most: 541.8},
	"lazy-list-item":                       {past: 579.5, most: 579.9},
	"bracket-run":                          {past: 2507.0, most: 2499.2},
	"nested-49-items-then-blank-lines":     {past: 9484.0, most: 9576.7},
	"markers-49-then-blank-lines":          {past: 9484.0, most: 9498.0},
	"footnote-references-then-definitions": {past: 729.5, most: 728.9},
	"bangs-before-carets":                  {past: 299.7, most: 300.0},
}
