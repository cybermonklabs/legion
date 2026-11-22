package pmdoc

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
// no more than a bound for each of its characters (parseStatementsPerCharacter,
// parseBytesPerCharacter); and, for every stored shape, growth of that work in proportion to its
// length, counted at an eighth of what it grows to and then at the whole of it
// (workGrowsLinearly, below) - of the parse alone, and of the parse, the write of the document a
// write makes of the body and its encode together.
//
// The work is two counts, which no load on the machine, no race detector and no collection moves:
//
//   - The statements the call executes in this package, in ygo and in goldmark's parser, text,
//     util and ast packages (countedPackages): each counted by the coverage counters `go test
//     -cover` puts in those packages, in a build of this package's tests that runs one call
//     (countedRun). Every loop of those packages counts each of its turns, and every walk each of
//     its nodes, so work that grows with the square of a body's length grows here wherever in
//     them it is done: goldmark's walk of a link label for a link inside it as much as pmdoc's
//     own reading of list spreads or ygo's walk of a fragment to an index.
//   - The bytes the call allocates on the heap (workOf), which reach allocation anywhere in the
//     call, the packages no counter is built into among them.
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
//   - Work inside a call into a package no counter is built into, which counts as the one
//     statement that calls it: the standard library (a bytes.IndexByte over the whole source is
//     one statement), and goldmark's extension package, whose counters change what it reads (with
//     them, 22 more of the fixture texts are refused and one read panics in lazyTableRows). The
//     one scan of the whole source a parse makes, whether it holds a tab, is read in runs of 64
//     bytes for that reason (holdsTab, lines.go).
//   - Bytes that grow with the square of a body while they are still few beside the parse's own:
//     the parse allocates hundreds to thousands of bytes a character, so a growth past linearSlack
//     shows only once such bytes at an eighth of the cap pass about two fifths of the parse's own
//     there (countGrowthVerdict's bound), and past parseBytesPerCharacter only once they pass it.
//
// The counted build reads the packages it counts from their files on disk: a coverage build does
// not apply -overlay to the packages it puts counters in (go 1.27.3), so a change made through an
// overlay reaches the verdicts and the bytes here and not the statements. Make it in the tree.

// Every shape at the block bound is stored or refused as it is, and the parse of it executes no
// more than parseStatementsPerCharacter statements and allocates no more than
// parseBytesPerCharacter bytes for each of its characters: counts linear in the body.
func TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedWithinTheirStepsAndBytesPerCharacter(t *testing.T) {
	// The shapes spell their refusals with heavybodies.MaxNesting, which is this package's own
	// maxNesting copied: heavybodies may not import pmdoc, since this test is in-package, so the
	// two are held to each other here rather than by the compiler.
	if heavybodies.MaxNesting != maxNesting {
		t.Fatalf("heavybodies.MaxNesting = %d, want pmdoc's maxNesting %d", heavybodies.MaxNesting, maxNesting)
	}
	counted := newCountedRun(t)
	for _, shape := range heavybodies.Shapes() {
		t.Run(shape.Name, func(t *testing.T) {
			if len(shape.Grow()) > heavybodies.BlockCap {
				t.Fatalf("%s is %d characters, past the %d one block may hold", shape.Name, len(shape.Grow()), heavybodies.BlockCap)
			}
			op := heavyOp{mode: "parse", shape: shape.Name, size: shape.Grown}
			work, outcome := counted.work(t, op)
			if shape.Refusal == "" {
				if outcome.err != nil {
					t.Fatalf("%s: %v, want it stored", shape.Name, outcome.err)
				}
			} else if !errors.Is(outcome.err, ErrSchema) || !strings.Contains(outcome.err.Error(), shape.Refusal) {
				t.Fatalf("%s: %v, want ErrSchema saying %q", shape.Name, outcome.err, shape.Refusal)
			}
			characters := uint64(outcome.characters)
			statements, bytes := parseStatementsPerCharacter*characters, parseBytesPerCharacter*characters
			t.Logf("%s: %d characters in %d statements of the parse (at most %d) and %d bytes allocated (at most %d)",
				shape.Name, outcome.characters, work.steps, statements, work.bytes, bytes)
			if work.steps > statements {
				t.Errorf("%s: the parse executed %d statements for a body of %d characters, past the %d allowed (%d a character) to a parse linear in its body",
					shape.Name, work.steps, outcome.characters, statements, parseStatementsPerCharacter)
			}
			if work.bytes > bytes {
				t.Errorf("%s: the parse allocated %d bytes for a body of %d characters, past the %d allowed (%d a character) to a parse linear in its body",
					shape.Name, work.bytes, outcome.characters, bytes, parseBytesPerCharacter)
			}
		})
	}
}

// parseStatementsPerCharacter is how many statements (countedRun) the parse may execute for each
// character of a heavy body. A parse that reads each piece of its input a few times executes some
// tens of statements for it for each of the containers open around it, which maxNesting bounds:
// at the cap the shapes that leave 49 lists open over thousands of blank lines executed 8,855 and
// 9,514 statements a character, the 100 items nested the same way 5,730 before their refusal,
// the run of brackets 2,480, the plain list 1,510, and the others 673 or fewer. 200 for each
// container maxNesting allows is about twice the most.
const parseStatementsPerCharacter = 200 * maxNesting

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
// what the shape grows and then at the whole of it: a shape whose statements or allocations grew
// with the square of its length would grow growthStep² times where a linear one may grow
// linearSlack*growthStep (growthStep, linear_test.go).
func TestHeavyBodiesAtTheBlockCapParseInWorkProportionalToTheirLength(t *testing.T) {
	counted := newCountedRun(t)
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal != "" {
			continue
		}
		t.Run(shape.Name, func(t *testing.T) {
			sizes := []int{shape.Grown / growthStep, shape.Grown}
			workGrowsLinearly(t, "parsing "+shape.Name, sizes, linearSlack, func(size int) callWork {
				work, outcome := counted.work(t, heavyOp{mode: "parse", shape: shape.Name, size: size})
				if outcome.err != nil {
					t.Fatalf("parsing %d characters of %s: %v", size, shape.Name, outcome.err)
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
// body, which the end-to-end bound this test replaced used to cover (INFRA-985).
func TestHeavyBodiesAtTheBodyCapParseAndEncodeInWorkProportionalToTheirLength(t *testing.T) {
	counted := newCountedRun(t)
	for _, shape := range heavybodies.Shapes() {
		if shape.Refusal != "" {
			continue
		}
		t.Run(shape.Name, func(t *testing.T) {
			sizes := []int{shape.Grown / growthStep, shape.Grown}
			workGrowsLinearly(t, "writing "+shape.Name, sizes, linearSlack, func(size int) callWork {
				work, outcome := counted.work(t, heavyOp{mode: "write", shape: shape.Name, size: size})
				if outcome.err != nil {
					t.Fatalf("writing %d characters of %s: %v", size, shape.Name, outcome.err)
				}
				if outcome.encoded == 0 {
					t.Fatalf("writing %d characters of %s encoded no update", size, shape.Name)
				}
				return work
			})
		})
	}
}

// heavyOp is one call the rows count: the parse a write makes of a shape at a size ("parse"), that
// parse and the write of its tree into a fresh document, encoded ("write"), or nothing ("none"),
// the count of which is what a run of the counted build costs before any call.
type heavyOp struct {
	mode, shape string
	size        int
}

// String is how an op is handed to the counted build's helper (countedHelperEnv).
func (op heavyOp) String() string { return fmt.Sprintf("%s|%s|%d", op.mode, op.shape, op.size) }

func parseHeavyOp(text string) (heavyOp, error) {
	parts := strings.Split(text, "|")
	if len(parts) != 3 {
		return heavyOp{}, fmt.Errorf("%q is not mode|shape|size", text)
	}
	size, err := strconv.Atoi(parts[2])
	if err != nil {
		return heavyOp{}, fmt.Errorf("%q: %w", text, err)
	}
	return heavyOp{mode: parts[0], shape: parts[1], size: size}, nil
}

// heavyOutcome is what a call answered: the parse's error, the characters it read, and the length
// of the update a write encoded.
type heavyOutcome struct {
	err        error
	characters int
	encoded    int
}

// prepare builds op's input - the body, and for a write the document it writes into - and returns
// the call itself, so that a count taken around the call takes none of its input.
func (op heavyOp) prepare() (call func() heavyOutcome, err error) {
	if op.mode == "none" {
		return func() heavyOutcome { return heavyOutcome{} }, nil
	}
	var body string
	for _, shape := range heavybodies.Shapes() {
		if shape.Name == op.shape {
			body = shape.Body(op.size)
		}
	}
	if body == "" {
		return nil, fmt.Errorf("no shape %q", op.shape)
	}
	switch op.mode {
	case "parse":
		return func() heavyOutcome {
			_, err := ParseForWrite(body, nil)
			return heavyOutcome{err: err, characters: len(body)}
		}, nil
	case "write":
		// "prosemirror" is the fragment the document service writes a document's blocks into
		// (fragmentName, internal/dispatch/docs/tree.go).
		doc := crdt.New()
		fragment := doc.GetXmlFragment("prosemirror")
		return func() heavyOutcome {
			outcome := heavyOutcome{characters: len(body)}
			tree, err := ParseForWrite(body, nil)
			if err == nil {
				err = doc.TransactE(func(txn *crdt.Transaction) error { return Update(txn, fragment, tree) })
			}
			if outcome.err = err; err == nil {
				outcome.encoded = len(crdt.EncodeStateAsUpdateV1(doc, nil))
			}
			return outcome
		}, nil
	}
	return nil, fmt.Errorf("no mode %q", op.mode)
}

// countedPackages are the packages whose statements the rows count: this one, ygo, which writes
// and encodes the document, and the goldmark packages the parse runs in. goldmark's extension
// package is left out: built with counters, it reads differently (the header has the measure).
var countedPackages = []string{
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc",
	"github.com/reearth/ygo/...",
	"github.com/yuin/goldmark",
	"github.com/yuin/goldmark/ast",
	"github.com/yuin/goldmark/parser",
	"github.com/yuin/goldmark/text",
	"github.com/yuin/goldmark/util",
}

const (
	// countedHelperEnv hands the counted build's helper the op it runs (heavyOp.String).
	countedHelperEnv = "PMDOC_HEAVY_COUNTED_OP"
	// countedHelperRan is what the helper logs once it has run its op, so a run of the build that
	// ran no op is refused rather than read as one that executed nothing.
	countedHelperRan = "counted op ran: "
)

// countedRun is a build of this package's tests with coverage counters in countedPackages
// (`go test -c -cover`), made once for a test, and the statements a run of it executes before any
// op (baseline): the package's initialisation, which every run repeats.
type countedRun struct {
	binary, dir string
	baseline    uint64
}

// newCountedRun builds the counted binary into t's temporary directory and counts its baseline.
// The build reads the same packages this package's own test build does, with counters in some.
func newCountedRun(t *testing.T) *countedRun {
	t.Helper()
	dir := t.TempDir()
	run := &countedRun{binary: filepath.Join(dir, "counted.test"), dir: dir}
	build := exec.Command("go", "test", "-c", "-o", run.binary, "-covermode=count",
		"-coverpkg="+strings.Join(countedPackages, ","), ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building this package's tests with coverage counters: %v\n%s", err, out)
	}
	run.baseline = run.statements(t, heavyOp{mode: "none"})
	if run.baseline == 0 {
		t.Fatal("the counted build executed no statement before any op: its counters count nothing, so no count from it says anything")
	}
	return run
}

// statements is how many statements of countedPackages one run of op executes in the counted
// build, less the baseline: the run's coverage profile, each block's statements times its count.
func (r *countedRun) statements(t *testing.T, op heavyOp) uint64 {
	t.Helper()
	profile, err := os.CreateTemp(r.dir, "profile-*.out")
	if err != nil {
		t.Fatal(err)
	}
	_ = profile.Close()
	child := exec.Command(r.binary, "-test.run", "^TestHeavyBodiesCountedHelper$", "-test.count=1", "-test.v",
		"-test.coverprofile="+profile.Name())
	child.Env = append(os.Environ(), countedHelperEnv+"="+op.String())
	out, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(out), countedHelperRan+op.String()) {
		t.Fatalf("the counted build did not run %s (%v):\n%s", op, err, out)
	}
	total, err := profileStatements(profile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if op.mode == "none" {
		return total
	}
	if total < r.baseline {
		t.Fatalf("%s executed %d statements, fewer than the %d every run executes before its op", op, total, r.baseline)
	}
	return total - r.baseline
}

// profileStatements is the statements a coverage profile counts: each block's statements times
// the times it ran.
func profileStatements(path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var total uint64
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		line := lines.Text()
		if strings.HasPrefix(line, "mode:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return 0, fmt.Errorf("%s: a line of three fields wanted, got %q", path, line)
		}
		statements, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %q: %w", path, line, err)
		}
		count, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %q: %w", path, line, err)
		}
		total += statements * count
	}
	return total, lines.Err()
}

// work is op's two counts: its statements in the counted build, and the bytes it allocates here,
// run here once more for its outcome.
func (r *countedRun) work(t *testing.T, op heavyOp) (callWork, heavyOutcome) {
	t.Helper()
	call, err := op.prepare()
	if err != nil {
		t.Fatal(err)
	}
	var outcome heavyOutcome
	work := workOf(t, op.String(), func() { outcome = call() })
	work.steps = r.statements(t, op)
	return work, outcome
}

// TestHeavyBodiesCountedHelper is the run of the counted build the rows count: it prepares the op
// countedHelperEnv names, runs it once, and logs countedHelperRan with it.
func TestHeavyBodiesCountedHelper(t *testing.T) {
	text := os.Getenv(countedHelperEnv)
	if text == "" {
		t.Skip("the child process the heavy-bodies rows count statements in")
	}
	op, err := parseHeavyOp(text)
	if err != nil {
		t.Fatal(err)
	}
	call, err := op.prepare()
	if err != nil {
		t.Fatal(err)
	}
	call()
	t.Log(countedHelperRan + op.String())
}

// callWork is what one call costs in the two counts this file holds a parse and a write to: the
// statements it executes in countedPackages and the bytes it allocates on the heap.
type callWork struct {
	steps, bytes uint64
}

// workOf is the bytes op allocates, labelled what: runtime.MemStats.TotalAlloc between a reading
// before op and one after it. ReadMemStats stops the world and flushes every processor's cache of
// spans before it reads, so the bytes are every allocation made between the two readings, whether
// or not a collection ran between them. They are the same in every run of a call but for what
// sync.Pool hands back: a collection empties it, and a race build drops a quarter of what is put
// back, so a pooled object is made again (parseBytesPerCharacter has the measure).
//
// The bytes are the process's, so nothing else may run beside op: no test in this package calls
// t.Parallel, and a timed call of linear_test.go still running past its deadline (leakedCallGuard)
// fails t before op runs.
func workOf(t *testing.T, what string, op func()) callWork {
	t.Helper()
	if leaking := leakedCallGuard(what); leaking != "" {
		t.Fatal(leaking)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	op()
	runtime.ReadMemStats(&after)
	return callWork{bytes: after.TotalAlloc - before.TotalAlloc}
}

// workGrowsLinearly counts the work measure does at each of sizes in turn, once each, since a
// count is the work itself and needs no sample, and fails t at the first size whose statements or
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
		{"statements", func(w callWork) uint64 { return w.steps }},
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
		failedStep, lines := countGrowthVerdict(sizes, test.counts, linearSlack, "statements")
		if failedStep != test.failAt {
			t.Errorf("%s: failed at step %d, want %d (%s)", test.name, failedStep, test.failAt, strings.Join(lines, "; "))
		}
		t.Logf("%s: %s", test.name, strings.Join(lines, "; "))
	}
}
