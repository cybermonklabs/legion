package pmdoc

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/heavybodies"
)

// TestParseForWriteRefusesDeeplyNestedBareDirectivesQuickly checks the two bodies that froze
// a browser tab (INFRA-581). The parser must refuse the bare directive at both sizes. The
// counted work must grow linearly with the number of openers and stay bounded per character.
// The API checks the refusal on the same full bodies (INFRA-1404).
func TestParseForWriteRefusesDeeplyNestedBareDirectivesQuickly(t *testing.T) {
	runLinearRow(t)
}

// nestedDirectiveShapes measures the refusal at two opener counts an eighth-to-whole step apart.
func nestedDirectiveShapes() []countedShape {
	var leaves []countedShape
	for _, directive := range heavybodies.NestedDirectives() {
		leaf := linearShape(directive.Name, "ParseForWrite", "opener")
		whole := directive.Openers
		// The infra6314-pf-pfd-* build-cluster Jobs at 5955cd4e chose 1/8 for the unclosed
		// openers and full size for the openers with closers (sizes.tsv).
		if directive.ID == "directive-openers-then-long-line" {
			whole /= 8
		}
		leaf.sizes = [2]int{whole / growthStep, whole}
		leaf.ceilingAtFullBody = true
		leaf.reading = nestedDirectiveReadings[directive.ID]
		name := leaf.name
		leaf.prepare = func(t *testing.T, openers int) (int, int, func() string) {
			body := directive.Body(openers)
			return openers, len(body), func() string {
				_, err := ParseForWrite(body, nil)
				return expectVerdict(t, name, malformedDirectiveReason, len(body), err)
			}
		}
		leaves = append(leaves, leaf)
	}
	return leaves
}

// nestedDirectiveReadings records each refusal's growth and its ceiling at the full body.
// The resized leaf was measured in the issue pod on 2026-11-28 with Go 1.27.3.
var nestedDirectiveReadings = map[string]countedReading{
	"directive-openers-then-long-line": {past: 917.5890410958904, most: 60.07096774193548, scanMost: 1},
	"directive-openers-then-closers":   {past: 1644, most: 182.97088888888888, scanMost: 1},
}

// A document write reads 32 typed blocks nested inside one another. The blocks are closed with
// fences that shorten inwards (nestedCallouts), since a closing fence closes the outermost block
// it satisfies.
func TestParseForWriteReads32NestedTypedBlocks(t *testing.T) {
	t.Parallel()
	doc, err := ParseForWrite(nestedCallouts(expectedMaxTypedNesting), nil)
	if err != nil {
		t.Fatalf("%d nested callouts: %v, want them read", expectedMaxTypedNesting, err)
	}
	if depth := typedDepth(doc); depth != expectedMaxTypedNesting {
		t.Fatalf("%d nested callouts read %d deep, want %d", expectedMaxTypedNesting, depth, expectedMaxTypedNesting)
	}
}

// A document write refuses a 33rd typed block nested inside 32, at its opening line, as the
// browser's default mode does; the read-only engine draws that opener as text.
func TestParseForWriteRefusesA33rdNestedTypedBlock(t *testing.T) {
	t.Parallel()
	_, err := ParseForWrite(nestedCallouts(expectedMaxTypedNesting+1), nil)
	refusal := fmt.Sprintf("line %d opens a typed block inside %d typed blocks; a document nests at most %d typed blocks",
		expectedMaxTypedNesting+1, expectedMaxTypedNesting, expectedMaxTypedNesting)
	if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("%d nested callouts: %v, want ErrSchema saying %q", expectedMaxTypedNesting+1, err, refusal)
	}
}

// The corpus's 33 nested typed blocks are refused by every reader of caller markdown at the 33rd
// opener's line.
func TestCorpusOf33NestedTypedBlocksIsRefused(t *testing.T) {
	t.Parallel()
	markdown, err := os.ReadFile("testdata/corpus-errors/typed-blocks-nested-33-deep.md")
	if err != nil {
		t.Fatalf("read 33 nested typed blocks corpus: %v", err)
	}
	refusal := fmt.Sprintf("line %d opens a typed block inside %d typed blocks; a document nests at most %d typed blocks",
		expectedMaxTypedNesting+1, expectedMaxTypedNesting, expectedMaxTypedNesting)
	for _, reader := range blockMarkdownReaders() {
		if _, err := reader.parse(string(markdown)); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), refusal) {
			t.Errorf("%s of 33 nested typed blocks: %v, want ErrSchema saying %q", reader.name, err, refusal)
		}
	}
}

// A tree whose typed blocks nest one past the bound, as a write can leave one by landing a typed
// block inside 32 others, is refused on read-back for how deeply its typed blocks nest, not for a
// line of a rendering nobody wrote.
func TestReadBackRefusalNamesTheTypedBlockNesting(t *testing.T) {
	t.Parallel()
	doc, err := ParseForWrite(nestedCallouts(expectedMaxTypedNesting), nil)
	if err != nil {
		t.Fatalf("%d nested callouts: %v", expectedMaxTypedNesting, err)
	}
	block := &Node{Type: "callout", Attrs: Attrs{"blockId": "outer", "kind": "note", "title": "T"}, Children: doc.Children}
	refusal := fmt.Sprintf("the result nests %d typed blocks inside one another; a document nests at most %d typed blocks",
		expectedMaxTypedNesting+1, expectedMaxTypedNesting)
	back := ReadBackBlock(block)
	for name, verdict := range map[string]error{"Read": back.Read, "Shape": back.Shape} {
		if !errors.Is(verdict, ErrSchema) || !strings.Contains(verdict.Error(), refusal) {
			t.Errorf("the read-back's %s of %d nested callouts: %v, want ErrSchema saying %q", name, expectedMaxTypedNesting+1, verdict, refusal)
		}
	}
}

// typedDepth is how many callouts stand inside one another on node's deepest path.
func typedDepth(node *Node) int {
	deepest := 0
	for _, child := range node.Children {
		deepest = max(deepest, typedDepth(child))
	}
	if node.Type == "callout" {
		return deepest + 1
	}
	return deepest
}
