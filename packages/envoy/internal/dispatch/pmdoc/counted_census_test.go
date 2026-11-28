package pmdoc

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/heavybodies"
	"github.com/sjawhar/envoy/internal/testwait"
)

// censusMark distinguishes outcomes from the child test runner's output.
const censusMark = "pmdoc census: "

// Census children receive the same shard coordinates in both builds.
const (
	censusShardIndexEnv = "PMDOC_CENSUS_SHARD_INDEX"
	censusShardCountEnv = "PMDOC_CENSUS_SHARD_COUNT"
	censusHeavyEnv      = "PMDOC_CENSUS_HEAVY"
)

// censusShard selects every count-th text starting at index, or the heavy bodies alone.
type censusShard struct {
	index, count int
	heavy        bool
}

// TestTheCountedBuildReadsEveryTextAsThePlainBuild compares the same reads and writes in both
// builds. Each shard has a process of its own because the block ID generator is process-wide.
func TestTheCountedBuildReadsEveryTextAsThePlainBuild(t *testing.T) {
	t.Parallel()
	n := min(runtime.GOMAXPROCS(0), 4)
	shards := make([]censusShard, n, n+1)
	for i := range shards {
		shards[i] = censusShard{index: i, count: n}
	}
	shards = append(shards, censusShard{count: 1, heavy: true})
	outcomes := make([][2]map[string]string, len(shards))
	var children sync.WaitGroup
	start := func(arm int, name, binary string) {
		for i, shard := range shards {
			shardName := fmt.Sprintf("texts-%d-of-%d", shard.index+1, shard.count)
			if shard.heavy {
				shardName = "heavy-bodies"
			}
			children.Go(func() {
				t.Run(name+"/"+shardName, func(t *testing.T) {
					outcomes[i][arm] = runCensusChild(t, binary, shard)
				})
			})
		}
	}
	// The plain shards can start before the counted binary is ready.
	start(0, "plain", os.Args[0])
	buildErr := countedBuildReady(t)
	if buildErr == nil {
		start(1, "counted", countedBuild.binary)
	}
	// Join the subtests before comparing their results or returning.
	children.Wait()
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	var plainTotal, countedTotal int
	for _, pair := range outcomes {
		plain, counted := pair[0], pair[1]
		compareCensusLines(t, plain, counted)
		plainTotal += len(plain)
		countedTotal += len(counted)
	}
	t.Logf("compared %d plain and %d counted outcomes", plainTotal, countedTotal)
}

// compareCensusLines matches inputs even when one build did not report an operation.
func compareCensusLines(t *testing.T, plain, counted map[string]string) {
	t.Helper()
	for _, key := range slices.Sorted(maps.Keys(plain)) {
		got, ok := counted[key]
		if !ok {
			t.Errorf("%s: counted build did not report (plain: %s)", key, plain[key])
		} else if got != plain[key] {
			t.Errorf("%s:\nplain:   %s\ncounted: %s", key, plain[key], got)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(counted)) {
		if _, ok := plain[key]; !ok {
			t.Errorf("%s: plain build did not report (counted: %s)", key, counted[key])
		}
	}
}

// runCensusChild reads one shard and rejects duplicate input, size and operation keys.
func runCensusChild(t *testing.T, binary string, shard censusShard) map[string]string {
	t.Helper()
	if shard.heavy {
		// Share the memory limit with the counted growth tests that also read large bodies.
		defer holdCountedChildSlot(t, "a heavy census child slot")()
	}
	out, stderr, err := testwait.RunChild(t, binary, 0, "^TestHeavyBodiesCountedHelper$",
		countedModeEnv+"=census",
		censusShardIndexEnv+"="+strconv.Itoa(shard.index),
		censusShardCountEnv+"="+strconv.Itoa(shard.count),
		censusHeavyEnv+"="+strconv.FormatBool(shard.heavy))
	if err != nil {
		t.Fatalf("census child: %v\n%s\n%s", err, out, stderr)
	}
	lines := make(map[string]string)
	for line := range strings.SplitSeq(out, "\n") {
		line, ok := strings.CutPrefix(line, censusMark)
		if !ok {
			continue
		}
		key, value, ok := strings.Cut(line, "\t")
		if !ok || key == "" || value == "" {
			t.Fatalf("malformed census outcome: %q", line)
		}
		if _, duplicate := lines[key]; duplicate {
			t.Fatalf("duplicate census outcome: %s", key)
		}
		lines[key] = value
	}
	if len(lines) == 0 {
		t.Fatalf("census child reported no outcomes:\n%s\n%s", out, stderr)
	}
	return lines
}

// writeCensus writes the requested shard from the same input order in both builds.
func writeCensus(t *testing.T) {
	t.Helper()
	defer SetBlockIDGenerator(nil)
	index, indexErr := strconv.Atoi(os.Getenv(censusShardIndexEnv))
	count, countErr := strconv.Atoi(os.Getenv(censusShardCountEnv))
	heavy, heavyErr := strconv.ParseBool(os.Getenv(censusHeavyEnv))
	if indexErr != nil || countErr != nil || heavyErr != nil || index < 0 || index >= count ||
		(heavy && count != 1) {
		t.Fatalf("invalid census shard: index %q, count %q, heavy %q",
			os.Getenv(censusShardIndexEnv), os.Getenv(censusShardCountEnv), os.Getenv(censusHeavyEnv))
	}
	out := bufio.NewWriter(os.Stdout)
	if heavy {
		writeCensusHeavyBodies(t, out)
	} else {
		writeCensusTexts(t, out, index, count)
	}
	if err := out.Flush(); err != nil {
		t.Fatal(err)
	}
}

// writeCensusTexts visits fixtures in file order and corpus paths in lexical order within each
// directory, so both builds give each shard the same texts.
func writeCensusTexts(t *testing.T, out *bufio.Writer, index, count int) {
	t.Helper()
	next := 0
	assigned := func() bool {
		match := next%count == index
		next++
		return match
	}
	text := func(name, markdown string) {
		censusText(t, out, name, "1x", markdown)
		censusText(t, out, name, fmt.Sprintf("%dx", checkLength), strings.Repeat(markdown, checkLength))
	}
	fixtures := loadFixtures(t)
	if len(fixtures) == 0 {
		t.Fatal("census: no fixtures")
	}
	for _, fixture := range fixtures {
		if assigned() {
			text("fixture "+fixture.Name, fixture.Markdown)
		}
	}
	for _, corpus := range []string{"corpus", "corpus-display", "corpus-errors"} {
		paths, err := filepath.Glob(filepath.Join("testdata", corpus, "*.md"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("census: %s: %v (%d documents)", corpus, err, len(paths))
		}
		for _, path := range paths {
			if !assigned() {
				continue
			}
			markdown, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text(path, string(markdown))
		}
	}
}

// writeCensusHeavyBodies compares each builder size that a counted leaf uses for the body.
func writeCensusHeavyBodies(t *testing.T, out *bufio.Writer) {
	t.Helper()
	sizesByShape := make(map[string]map[int]struct{})
	for _, leaves := range [][]countedShape{heavyBodyCapShapes(), heavyBodyParseShapes(), heavyBodyWriteShapes()} {
		for _, leaf := range leaves {
			sizes := sizesByShape[leaf.shape]
			if sizes == nil {
				sizes = make(map[int]struct{})
				sizesByShape[leaf.shape] = sizes
			}
			sizes[leaf.sizes[0]] = struct{}{}
			if !leaf.fixedSize {
				sizes[leaf.sizes[1]] = struct{}{}
			}
		}
	}
	for _, shape := range heavybodies.Shapes() {
		for _, size := range slices.Sorted(maps.Keys(sizesByShape[shape.Name])) {
			censusText(t, out, shape.ID, strconv.Itoa(size), shape.Body(size))
		}
	}
}

// censusText compares both parse results and both document states, including refusals at the
// step that returns them. A refused parse leaves its Update and encode without an input.
func censusText(t *testing.T, out *bufio.Writer, name, size string, markdown string) {
	t.Helper()
	label := fmt.Sprintf("%q at %s", name, size)
	report := func(op, result string) {
		if _, err := fmt.Fprintf(out, "%s%s, %s\t%d bytes: %s\n", censusMark, label, op, len(markdown), result); err != nil {
			t.Fatal(err)
		}
	}
	reportTree := func(op string, tree *Node, err error) {
		if err != nil {
			report(op, fmt.Sprintf("refused: %q", err.Error()))
			return
		}
		encoded, err := tree.JSON()
		if err != nil {
			t.Fatalf("%s, %s: encode tree as JSON: %v", label, op, err)
		}
		report(op, fmt.Sprintf("tree sha256 %x", sha256.Sum256(encoded)))
	}

	SetBlockIDGenerator(counterBlockIDs())
	rendering, err := ParseRendering(markdown)
	reportTree("ParseRendering", rendering, err)

	doc := crdt.New(crdt.WithClientID(heavyClientID))
	fragment := doc.GetXmlFragment("prosemirror")
	update := func(tree *Node) error {
		return doc.TransactE(func(txn *crdt.Transaction) error { return Update(txn, fragment, tree) })
	}
	reportWrite := func(which string, tree *Node, parseErr error) bool {
		updateOp, encodeOp := "Update "+which, "EncodeStateAsUpdateV1 "+which
		if parseErr != nil {
			report(updateOp, "not reached: parse refused")
			report(encodeOp, "not reached: parse refused")
			return false
		}
		if err := update(tree); err != nil {
			report(updateOp, fmt.Sprintf("refused: %q", err.Error()))
			report(encodeOp, "not reached: update refused")
			return false
		}
		report(updateOp, "accepted")
		report(encodeOp, fmt.Sprintf("update sha256 %x", sha256.Sum256(crdt.EncodeStateAsUpdateV1(doc, nil))))
		return true
	}

	SetBlockIDGenerator(counterBlockIDs())
	live, err := ParseForWrite(markdown, nil)
	reportTree("ParseForWrite fresh", live, err)
	if !reportWrite("fresh", live, err) {
		// Even a text refused by the fresh write must be tried against a live document.
		live = &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Attrs: Attrs{BlockIDAttr: "census-seed"}}}}
		if err := update(live); err != nil {
			t.Fatalf("%s: seed the live document: %v", label, err)
		}
	}

	SetBlockIDGenerator(counterBlockIDs())
	replacement, err := ParseForWrite(markdown, live)
	reportTree("ParseForWrite live", replacement, err)
	if err == nil {
		editCensusTree(replacement)
	}
	reportWrite("live", replacement, err)
}

// editCensusTree changes the first text run so the second Update computes a diff. A tree with
// no text gains a paragraph instead.
func editCensusTree(tree *Node) {
	edited := false
	Walk(tree, func(node *Node) bool {
		if node.Type != "text" {
			return true
		}
		node.Text += " census edit"
		edited = true
		return false
	})
	if !edited {
		tree.Children = append(tree.Children, paragraph("Census edit."))
	}
}
