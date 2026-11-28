package pmdoc

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/heavybodies"
)

// These rows cover the eleven bodies in heavybodies.Shapes. Nine come from bodies that froze
// a browser tab (sjawhar/legion#2519). The other two cover footnote resolution that walked every
// definition for each reference, and a reference parser that searched the rest of the line after
// each bang. Six bodies share bytes with the browser suite, as shared-bodies.json records. The
// API also checks the write verdict on these same Go fixtures (INFRA-1404).
//
// INFRA-985 moved the API writes from Playwright, where a one-second wall-time limit failed on
// shared CI runners. The browser still limits the longest main-thread hold when it draws a card
// (longestHoldMs). CPU time failed here too (INFRA-3904): one loaded race run spent 1.179 s on
// the plain list, and faster pods fit more calls into a timed window and triggered a collection.
//
// The verdicts now count statements, allocated bytes and the bytes individual searches read.
// The shared coverage build counts statements in pmdoc and its non-standard dependencies,
// including goldmark's extensions. Each leaf counts the operation alone and with preparation.
// The invoking build checks the same outcome and also measures allocation, including under -race.
// The fixed budgets use both the builder's size and the body's length, which can differ because
// of a fixed prefix or rounding in the builder. Growth past the cap can expose quadratic work
// that remains small beside the rest of an operation at the cap.
//
// The counted build reads sources from disk. Go's coverage build does not apply -overlay to the
// packages it instruments (Go 1.27.3). Apply changes to those sources before counting them.

// TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedWithinTheirStepsAndBytesPerCharacter checks the
// write's parse at the cap, the growth of each refused parse, and each rendering read past the cap.
func TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedWithinTheirStepsAndBytesPerCharacter(t *testing.T) {
	if heavybodies.MaxNesting != maxNesting {
		t.Fatalf("heavybodies.MaxNesting = %d, want pmdoc's maxNesting %d", heavybodies.MaxNesting, maxNesting)
	}
	for _, shape := range heavybodies.Shapes() {
		if characters := len(shape.Grow()); characters > heavybodies.BlockCap {
			t.Errorf("%s is %d characters, past the %d one block may hold", shape.Name, characters, heavybodies.BlockCap)
		}
	}
	runLinearRow(t)
}

// TestHeavyBodiesAtTheBlockCapParseInWorkProportionalToTheirLength counts the parser's work on
// each stored body at an eighth of its builder's full size and at the full size.
func TestHeavyBodiesAtTheBlockCapParseInWorkProportionalToTheirLength(t *testing.T) {
	runLinearRow(t)
}

// TestHeavyBodiesAtTheBodyCapParseAndEncodeInWorkProportionalToTheirLength counts the parse,
// update and encode that precede a durable API write. Past the cap it also counts the read-back
// that refuses a document stored differently than it was written (documentMisread). That write
// uses an unlimited element budget, since the ordinary budget can refuse bodies at those sizes.
func TestHeavyBodiesAtTheBodyCapParseAndEncodeInWorkProportionalToTheirLength(t *testing.T) {
	runLinearRow(t)
}

// statementsPerCharacter is the fixed limit on statements in each heavy operation. At the cap
// the two shapes that leave 49 lists open over blank lines executed 8,855 and 9,514 statements a
// character in a parse. A write took 8,859 and 9,517. The 100 nested items took 5,730 before
// refusal, the brackets 2,461 to parse and 2,464 to write, the plain list 1,530 and 2,042, and
// the others at most 714. Allowing 200 for each container gives about twice the largest count.
const statementsPerCharacter = 200 * maxNesting

// bytesPerCharacter is the fixed limit on allocation in each heavy operation. At the cap the
// two shapes that leave 49 lists open allocated 13,469 and 13,458 bytes a character in a parse,
// and 13,485 and 13,472 in a write. The 100 nested items allocated 8,444 before refusal, the
// brackets 2,652 and 2,657, the plain list 2,389 and 4,217, and the others at most 1,247.
// In one race build the plain list allocated 16,328 bytes a character: sync.Pool drops some
// regexp backtrackers in that build. The limit is 64 KiB. A parser that copied a list's events
// for each item it opened allocated 196,364 bytes a character on the plain list.
const bytesPerCharacter = 64 << 10

// growthStep is the usual ratio of the full builder size to the small one.
const growthStep = 8

// linearSlack lets counts grow up to three times faster than the input. At an eightfold step a
// quadratic count grows 64 times, past the 24 times allowed for linear work.
const linearSlack = 3

// cpuCheckLength is the multiplier for a read or whole write past the cap. Quadratic work per
// character grows eightfold there, where linear work per character does not grow.
const cpuCheckLength = 8

// cpuCheckLengthOf uses a longer step for footnote references so their old resolution cost
// cannot hide beside the rest of the read or write.
func cpuCheckLengthOf(shape heavybodies.Shape) int {
	if shape.ID == "footnote-references-then-definitions" {
		return footnoteCheckLength
	}
	return cpuCheckLength
}

// footnoteCheckLength is the longer multiplier for footnote references and definitions. The old
// resolution walked every definition for each reference and cost about a tenth of a whole write
// at the cap. At eight times the length its read grew 2.0 to 4.2 times per character and its
// whole write 1.0 to 2.7, straddling the 2.5 bound. At 32 times, with that resolution restored
// (footnotes.go at 33a29d1967), those ratios were 21.9 to 27.6 and 10.2 to 17.0 over four runs.
// With the current resolution they were 1.4 and 1.2 (CPU samples from INFRA-3904).
const footnoteCheckLength = 32

// cpuGrowthPerCharacter limits how much counted work, allocation or searched bytes per body
// character may grow past the cap. It keeps the 2.5 ratio from the CPU verdict, where the
// largest observed per-character growth on the clean code was 1.5.
const cpuGrowthPerCharacter = 2.5

// heavyBodyCapShapes checks all the stored and refused bodies at the cap and in a rendering read.
// A separate growth leaf checks each refused parse at an eighth of the builder size and at full size.
func heavyBodyCapShapes() []countedShape {
	var leaves []countedShape
	for _, shape := range heavybodies.Shapes() {
		leaves = append(leaves,
			heavyBodyLeaf(shape, "parse at the cap", heavyCapReadings[shape.ID]),
			heavyBodyLeaf(shape, "read", heavyReadReadings[shape.ID]),
		)
		if shape.Refusal != "" {
			leaves = append(leaves, heavyBodyLeaf(shape, "parse growth", heavyParseReadings[shape.ID]))
		}
	}
	return leaves
}

// heavyBodyParseShapes checks the growth of the write's parse on each stored body.
func heavyBodyParseShapes() []countedShape {
	var leaves []countedShape
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal == "" {
			leaves = append(leaves, heavyBodyLeaf(shape, "parse", heavyParseReadings[shape.ID]))
		}
	}
	return leaves
}

// heavyBodyWriteShapes counts the write steps on each stored body at both pairs of sizes.
func heavyBodyWriteShapes() []countedShape {
	var leaves []countedShape
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal == "" {
			leaves = append(leaves,
				heavyBodyLeaf(shape, "write", heavyWriteReadings[shape.ID]),
				heavyBodyLeaf(shape, "whole write", heavyWholeWriteReadings[shape.ID]),
			)
		}
	}
	return leaves
}

// heavyBodyLeaf gives the parent and child the same sizes and operation. A body that is meant
// to stay within the cap is checked by its actual length, not by the builder's size.
func heavyBodyLeaf(shape heavybodies.Shape, operation string, reading countedReading) countedShape {
	leaf := linearShape(shape.Name, operation, "repeated character")
	leaf.reading = reading
	leaf.heavyBody, leaf.limitChildren = true, true
	leaf.fixedSize = operation == "parse at the cap"
	leaf.perCharacterGrowth = operation == "read" || operation == "whole write"
	switch {
	case leaf.fixedSize:
		leaf.sizes = [2]int{shape.Grown}
	case leaf.perCharacterGrowth:
		leaf.sizes = [2]int{shape.Grown, cpuCheckLengthOf(shape) * shape.Grown}
	default:
		leaf.sizes = [2]int{shape.Grown / growthStep, shape.Grown}
	}
	name, pastCap := leaf.name, leaf.perCharacterGrowth
	leaf.prepare = func(t *testing.T, size int) (int, int, func() string) {
		body := shape.Body(size)
		bounded := !pastCap || (operation == "read" && size == shape.Grown)
		if bounded && len(body) > heavybodies.BlockCap {
			t.Fatalf("%s at builder size %d is %d characters, past the %d one block may hold", name, size, len(body), heavybodies.BlockCap)
		}
		switch operation {
		case "read":
			return size, len(body), func() string {
				_, err := ParseRendering(body)
				return expectVerdict(t, name, shape.Refusal, len(body), err)
			}
		case "parse at the cap", "parse growth", "parse":
			return size, len(body), func() string {
				_, err := ParseForWrite(body, nil)
				return expectVerdict(t, name, shape.Refusal, len(body), err)
			}
		case "write", "whole write":
			return size, len(body), prepareHeavyWrite(t, name, body, operation == "whole write")
		default:
			t.Fatalf("unknown heavy body operation %q", operation)
			return 0, 0, nil
		}
	}
	return leaf
}

// expectVerdict checks the intended outcome before reporting it for the build comparison.
func expectVerdict(t *testing.T, what, refusal string, characters int, err error) string {
	t.Helper()
	if refusal == "" {
		if err != nil {
			t.Fatalf("%s at %d characters: %v, want it stored", what, characters, err)
		}
		return "stored"
	}
	if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("%s at %d characters: %v, want ErrSchema saying %q", what, characters, err, refusal)
	}
	return err.Error()
}

// heavyClientID fixes the encoded client ID's length. The random IDs ygo normally gives a
// document use one to five bytes, which varied the statement count by up to 0.22 percent.
const heavyClientID = crdt.ClientID(math.MaxUint32)

// wholeWriteBudget removes the element limit so even the write past the cap runs every step.
func wholeWriteBudget() *WriteBudget {
	budget := NewWriteBudget()
	budget.elements = newElementCount(0)
	return budget
}

// prepareHeavyWrite builds a fresh document before the operation-only count starts. The
// operation checks that the parse, update and encode succeed and reports the encoded length.
func prepareHeavyWrite(t *testing.T, what, body string, whole bool) func() string {
	// The document service writes to the same fragment (internal/dispatch/docs/tree.go).
	doc := crdt.New(crdt.WithClientID(heavyClientID))
	fragment := doc.GetXmlFragment("prosemirror")
	return func() string {
		var tree *Node
		var err error
		if whole {
			// Use ParseWritten's steps, but do not count elements against the write budget.
			tree, err = parseForWrite(body, nil, true, wholeWriteBudget())
			if err == nil {
				_, misread, refused, readErr := documentMisread(tree)
				err = refuseMisread(misread, refused, readErr)
			}
		} else {
			tree, err = ParseForWrite(body, nil)
		}
		expectVerdict(t, what, "", len(body), err)
		if err := doc.TransactE(func(txn *crdt.Transaction) error { return Update(txn, fragment, tree) }); err != nil {
			t.Fatalf("%s at %d characters: update: %v", what, len(body), err)
		}
		encoded := len(crdt.EncodeStateAsUpdateV1(doc, nil))
		if encoded == 0 {
			t.Fatalf("%s at %d characters encoded no update", what, len(body))
		}
		return fmt.Sprintf("stored; %d bytes encoded", encoded)
	}
}

// heavyCapReadings records the parse at the cap for each body by fixture ID (Go 1.27.3).
var heavyCapReadings = map[string]countedReading{
	"plain-list":                           {most: 1563.1380625, scanMost: 2},
	"lazy-quote":                           {most: 578.7509376172021, scanMost: 2.000250031253907},
	"lazy-list-item":                       {most: 617.3309476184523, scanMost: 2.000250031253907},
	"bracket-run":                          {most: 2462.482233333333, searchedMost: 0.5, scanMost: 2.0000333333333336},
	"nested-49-items-then-blank-lines":     {most: 9621.696658970528, scanMost: 1.1730272986554393},
	"markers-49-then-blank-lines":          {most: 9543.809780564263, scanMost: 1.006269592476489},
	"nested-100-items-then-blank-lines":    {most: 8449.694838709678, scanMost: 1.4746543778801844},
	"marker-chain":                         {most: 672.7853682927591, scanMost: 1},
	"callout-bracket-run":                  {most: 1.0928340391476874, scanMost: 1.0003668011604254},
	"footnote-references-then-definitions": {most: 622.9147134195887, searchedMost: 0.11091318207387962, scanMost: 2.0467216701043816},
	"bangs-before-carets":                  {most: 302.9734375, scanMost: 1.9999375},
}

// heavyParseReadings records parse growth for the stored and refused bodies by fixture ID.
var heavyParseReadings = map[string]countedReading{
	"plain-list":                           {past: 1563.0078571428571, most: 1564.0495, scanMost: 2},
	"lazy-quote":                           {past: 578.5206629281709, most: 580.3630907726932, scanMost: 2.0015003750937734},
	"lazy-list-item":                       {past: 617.0220380755081, most: 619.4936234058515, scanMost: 2.0015003750937734},
	"bracket-run":                          {past: 2525, most: 2462.482233333333, searchedMost: 0.5, scanMost: 2.0002666666666666},
	"nested-49-items-then-blank-lines":     {past: 9533, most: 10242.669654985059, scanMost: 2.384406411301277},
	"markers-49-then-blank-lines":          {past: 9533, most: 9619.489089541008, scanMost: 1.0501630298470028},
	"nested-100-items-then-blank-lines":    {past: 8173, most: 10386.966814159292, scanMost: 4.797935103244837},
	"marker-chain":                         {past: 672, most: 678.28432108027, scanMost: 1},
	"callout-bracket-run":                  {past: 1, most: 1.7427961579509073, scanMost: 1.0029348986125934},
	"footnote-references-then-definitions": {past: 622.9333904782313, most: 622.9147134195887, searchedMost: 0.11091318207387962, scanMost: 2.0467216701043816},
	"bangs-before-carets":                  {past: 302.65585714285714, most: 305.1965, scanMost: 2},
}

// heavyWriteReadings records write growth within the cap for each stored body by fixture ID.
var heavyWriteReadings = map[string]countedReading{
	"plain-list":                           {past: 2083.480285714286, most: 2083.13609375, scanMost: 2},
	"lazy-quote":                           {past: 580.5208058006215, most: 582.7894473618404, scanMost: 2.0015003750937734},
	"lazy-list-item":                       {past: 619.0221809479588, most: 622.235808952238, scanMost: 2.0015003750937734},
	"bracket-run":                          {past: 2527.000076190476, most: 2464.5228666666667, searchedMost: 0.5, scanMost: 2.0002666666666666},
	"nested-49-items-then-blank-lines":     {past: 9533, most: 10279.248302091823, scanMost: 2.384406411301277},
	"markers-49-then-blank-lines":          {past: 9533, most: 9646.79658891397, scanMost: 1.0501630298470028},
	"footnote-references-then-definitions": {past: 732.2235436979892, most: 732.1644790299393, searchedMost: 0.11091318207387962, scanMost: 2.0467216701043816},
	"bangs-before-carets":                  {past: 304.65628571428573, most: 307.91525, scanMost: 2},
}

// heavyReadReadings holds the readings from the build-cluster run of the rendering reads.
var heavyReadReadings = map[string]countedReading{}

// heavyWholeWriteReadings holds the readings from the build-cluster run of the whole writes.
var heavyWholeWriteReadings = map[string]countedReading{}
