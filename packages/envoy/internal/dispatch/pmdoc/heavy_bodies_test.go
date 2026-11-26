package pmdoc

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

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
// Each shape is asserted these ways: the verdict a write of it gets; the work its parse and, for a
// stored shape, its write do, no more than a bound for each of its characters
// (statementsPerCharacter, bytesPerCharacter) at every length they are counted at; for every
// stored shape, growth of that work in proportion to its length, counted at an eighth of what it
// grows to and then at the whole of it (workGrowsLinearly, below) - of the parse alone, and of the
// parse, the write of the document a write makes of the body and its encode together; and the
// CPU time of its read and, for a stored shape, of its whole write, at its length and at
// cpuCheckLength times it (footnoteCheckLength for the footnote shape, cpuCheckLengthOf), under a
// ceiling for each character and growing for each character no more than work linear in a body's
// length does (cpuGrowsLinearly).
//
// The work is two counts, which no load on the machine, no race detector and no collection moves:
//
//   - The statements the call executes in every package outside the standard library this one
//     imports, directly or through another, and in this one, but goldmark's extension package
//     (countedPackages): each counted by the coverage counters `go test -cover` puts in those
//     packages, in a build of this package's tests that runs one call (countedRun), and whose
//     answer must be the one this build gives, so a counter that changed what a package reads
//     fails the row rather than counting another path. Every loop of those packages counts each
//     of its turns, and every walk each of its nodes, so work that grows with the square of a
//     body's length grows here wherever in them it is done: goldmark's walk of a link label for a
//     link inside it as much as pmdoc's own reading of list spreads or ygo's walk of a fragment to
//     an index. A write's document takes one client id (heavyClientID) rather than ygo's random
//     one, whose varints would move the count by a fraction of a percent from run to run.
//   - The bytes the call allocates on the heap (workOf), which reach allocation anywhere in the
//     call, the packages no counter is built into among them.
//
// The counts do not see work inside a call into a package no counter is built into, which counts
// as the one statement that calls it: the standard library (a bytes.IndexByte over the whole
// source is one statement), and goldmark's extension package, whose counters change what it reads
// (with them, the census probe's 254 texts lose 22 more to refusals, and one read panics in
// lazyTableRows, table.go:457, at main too). Each counter is a uint32 the runtime gives (cmd/cover)
// and no build widens, so a block that runs 2^32 times reads as the remainder. That work is held
// by its CPU time instead. CPU time grows with the work the process does and not with the time it
// waits for a core, so the load on the machine moves it only through shared caches and clock
// frequency, and it is measured with one processor for the program (GOMAXPROCS 1) after a
// collection, so the collector's workers spread over idle processors add none of their own:
// work quadratic in a body's length costs cpuCheckLength times more for each character at
// cpuCheckLength times the length, and linear work no more, which no load reaches; and a ceiling
// for each character holds what is dear at both lengths. A count wrapped past 2^32 is caught
// there too: a block run 2^32 times where its work is linear has run more than
// statementsPerCharacter times a character at an eighth of the cap already, and where it is not,
// its CPU time grows past cpuGrowthPerCharacter. The CPU checks run in a build without the race
// detector only (race_disabled_test.go).
//
// The Playwright spec that wrote eight of these shapes through the API held each write to a second
// and to nothing about its growth: a ratio between two wall times a shared CI runner took, a
// doubling of the body apart, cannot separate a regression from the runner's own noise, which is
// what made that bound fail at random (INFRA-985). This file then held each shape to a second of
// this process's CPU time and each growth to a ratio of two CPU times with the collector stopped,
// and those failed at random too (INFRA-3904): a race-detector run of every envoy package at a
// load of about 200 spent 1.179 s of CPU time on the plain list, past its second, and in a pod
// whose parse ran faster than another's a timed window held more runs, whose allocations reached
// the window's memory limit and ran a collection inside it, which the window refuses. The verdict
// each write gets is gated in internal/dispatch/api (TestHeavyDocumentWriteVerdicts), and six of
// the bodies keep the browser's own second in the browser suite - the longest a card's draw may
// hold the page's main thread (longestHoldMs, packages/dispatch/e2e/main-thread-hold.ts).
//
// Bytes that grow with the square of a body while they are still few beside the parse's own pass
// the counts: the parse allocates hundreds to thousands of bytes a character, so a growth past
// linearSlack shows only once such bytes at an eighth of the cap pass about two fifths of the
// parse's own there (countGrowthVerdict's bound), and past bytesPerCharacter only once they pass
// it.
//
// The counted build is a `go test -c` of this directory: it reads the environment, GOFLAGS among
// it, and none of the flags the build running this test was given (-race, -tags). It reads the
// packages it counts from their files on disk: a coverage build does not apply -overlay to the
// packages it puts counters in (go 1.27.3), so a change made through an overlay reaches the
// verdicts, the bytes and the CPU time here and not the statements. Make it in the tree.

// Every shape at the block bound is stored or refused as it is, and the parse of it executes no
// more than statementsPerCharacter statements and allocates no more than bytesPerCharacter bytes
// for each of its characters: counts linear in the body. Its read, which every write's parse
// shares (readSource) and which no element limit stops, takes CPU time linear in its length
// (cpuGrowsLinearly): a refused shape's as much as a stored one's, since the reader reads the
// whole body before it refuses it.
func TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedWithinTheirStepsAndBytesPerCharacter(t *testing.T) {
	counted := newCountedRun(t)
	for _, shape := range heavybodies.Shapes() {
		t.Run(shape.Name, func(t *testing.T) {
			if len(shape.Grow()) > heavybodies.BlockCap {
				t.Fatalf("%s is %d characters, past the %d one block may hold", shape.Name, len(shape.Grow()), heavybodies.BlockCap)
			}
			work, outcome := counted.work(t, heavyOp{mode: "parse", shape: shape.Name, size: shape.Grown})
			expectVerdict(t, "parsing", shape, outcome)
			holdsCountsPerCharacter(t, "parsing "+shape.Name, work)
			cpuGrowsLinearly(t, "read", shape)
		})
	}
}

// statementsPerCharacter is how many statements (countedRun) a parse or a write may execute for
// each character of a heavy body. A parse that reads each piece of its input a few times executes
// some tens of statements for it for each of the containers open around it, which maxNesting
// bounds: at the cap the shapes that leave 49 lists open over thousands of blank lines executed
// 8,855 and 9,514 statements a character in a parse and 8,859 and 9,517 in a write, the 100 items
// nested the same way 5,730 before their refusal, the run of brackets 2,461 and 2,464, the plain
// list 1,530 and 2,042, and the others 714 or fewer. 200 for each container maxNesting allows is
// about twice the most.
const statementsPerCharacter = 200 * maxNesting

// bytesPerCharacter is how many bytes a parse or a write may allocate on the heap for each
// character of a heavy body (workOf). At the cap the shapes that leave 49 lists open over
// thousands of blank lines allocated 13,469 and 13,458 bytes a character in a parse and 13,485 and
// 13,472 in a write, the 100 items nested the same way 8,444 before their refusal, the run of
// brackets 2,652 and 2,657, the plain list 2,389 and 4,217, and the others 1,247 or fewer. A race
// build allocates more where the parse matches a regexp: the race detector makes sync.Pool drop a
// quarter of what is put back, and regexp keeps its backtracker's state there, so the plain list's
// parse allocated 16,328 bytes a character in one. Twice that, rounded up to a power of two, is 64
// KiB. A parse that copied a list's events for each item it opened allocated 196,364 bytes a
// character on the plain list.
const bytesPerCharacter = 64 << 10

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
// write does with a body before it reaches Postgres - the parse of the markdown a caller sent,
// with the read-back of its rendering that refuses a document stored otherwise than it was
// written (documentMisread), and the write of that tree into a fresh document, encoded as the
// document's first durable update (parseReplacing and encodeDocumentTree,
// internal/dispatch/docs/tree.go). The parse is the part that has grown quadratically before
// (linear_test.go holds the shapes that did), and the write and the encode are counted beside it
// because they are the rest of what an API write does with a body, which the end-to-end bound
// this test replaced used to cover (INFRA-985). The whole write takes CPU time linear in the
// body's length too (cpuGrowsLinearly), at the cap and at cpuCheckLengthOf times it, where a write
// that long passes the elements one write may make: it is the same write on a budget that counts
// none (wholeWriteBudget).
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
			cpuGrowsLinearly(t, "whole write", shape)
		})
	}
}

// expectVerdict fails t, naming what was done to shape, unless outcome is the verdict the shape is
// written to get: stored, or refused (ErrSchema) saying shape.Refusal.
func expectVerdict(t *testing.T, what string, shape heavybodies.Shape, outcome heavyOutcome) {
	t.Helper()
	if shape.Refusal == "" {
		if outcome.err != nil {
			t.Fatalf("%s %d characters of %s: %v, want it stored", what, outcome.characters, shape.Name, outcome.err)
		}
		return
	}
	if !errors.Is(outcome.err, ErrSchema) || !strings.Contains(outcome.err.Error(), shape.Refusal) {
		t.Fatalf("%s %d characters of %s: %v, want ErrSchema saying %q", what, outcome.characters, shape.Name, outcome.err, shape.Refusal)
	}
}

// holdsCountsPerCharacter fails t, naming what, where work executed more than
// statementsPerCharacter statements or allocated more than bytesPerCharacter bytes for each of the
// characters it read, and logs both otherwise.
func holdsCountsPerCharacter(t *testing.T, what string, work callWork) {
	t.Helper()
	characters := uint64(work.characters)
	statements, bytes := statementsPerCharacter*characters, bytesPerCharacter*characters
	t.Logf("%s: %d characters in %d statements (at most %d) and %d bytes allocated (at most %d)",
		what, work.characters, work.steps, statements, work.bytes, bytes)
	if work.steps > statements {
		t.Errorf("%s: executed %d statements for a body of %d characters, past the %d allowed (%d a character) to work linear in its body",
			what, work.steps, work.characters, statements, statementsPerCharacter)
	}
	if work.bytes > bytes {
		t.Errorf("%s: allocated %d bytes for a body of %d characters, past the %d allowed (%d a character) to work linear in its body",
			what, work.bytes, work.characters, bytes, bytesPerCharacter)
	}
}

// heavyOp is one call the rows measure: the parse a write makes of a shape at a size ("parse"),
// that parse and the write of its tree into a fresh document, encoded ("write"), the same write on
// a budget that counts no elements ("whole write", wholeWriteBudget), the read of the shape as a
// rendering is read back ("read"), or nothing ("none"), the count of which is what a run of the
// counted build costs before any call.
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

// String is the outcome as the counted build's helper logs it and as this build compares it.
func (o heavyOutcome) String() string {
	answer := "stored"
	if o.err != nil {
		answer = "refused: " + o.err.Error()
	}
	return fmt.Sprintf("%s; %d characters; %d bytes encoded", answer, o.characters, o.encoded)
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
	// "prosemirror" is the fragment the document service writes a document's blocks into
	// (fragmentName, internal/dispatch/docs/tree.go).
	write := func(parse func() (*Node, error)) func() heavyOutcome {
		doc := crdt.New(crdt.WithClientID(heavyClientID))
		fragment := doc.GetXmlFragment("prosemirror")
		return func() heavyOutcome {
			outcome := heavyOutcome{characters: len(body)}
			tree, err := parse()
			if err == nil {
				err = doc.TransactE(func(txn *crdt.Transaction) error { return Update(txn, fragment, tree) })
			}
			if outcome.err = err; err == nil {
				outcome.encoded = len(crdt.EncodeStateAsUpdateV1(doc, nil))
			}
			return outcome
		}
	}
	switch op.mode {
	case "read":
		return func() heavyOutcome {
			_, err := ParseRendering(body)
			return heavyOutcome{err: err, characters: len(body)}
		}, nil
	case "parse":
		return func() heavyOutcome {
			_, err := ParseForWrite(body, nil)
			return heavyOutcome{err: err, characters: len(body)}
		}, nil
	case "write":
		return write(func() (*Node, error) { return ParseForWrite(body, nil) }), nil
	case "whole write":
		// ParseWritten's steps, on a budget that counts no elements.
		return write(func() (*Node, error) {
			tree, err := parseForWrite(body, nil, true, wholeWriteBudget())
			if err != nil {
				return nil, err
			}
			_, misread, refused, err := documentMisread(tree)
			if err := refuseMisread(misread, refused, err); err != nil {
				return nil, err
			}
			return tree, nil
		}), nil
	}
	return nil, fmt.Errorf("no mode %q", op.mode)
}

// countedLeftOut is the one package outside the standard library the counted build puts no
// counter in: goldmark's extension package, which reads otherwise with them (the header has the
// measure).
const countedLeftOut = "github.com/yuin/goldmark/extension"

// countedPackages are the packages whose statements the rows count: this one and every package
// outside the standard library it imports, directly or through another (go list -deps), but
// countedLeftOut.
func countedPackages(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Fatalf("listing the packages this one imports: %v", err)
	}
	var packages []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" && line != countedLeftOut {
			packages = append(packages, line)
		}
	}
	if len(packages) < 2 {
		t.Fatalf("go list -deps named %d packages to count, %q: this one and the ones it imports, at least two, were wanted", len(packages), packages)
	}
	return packages
}

const (
	// countedHelperEnv hands the counted build's helper the op it runs (heavyOp.String).
	countedHelperEnv = "PMDOC_HEAVY_COUNTED_OP"
	// countedHelperRan is what the helper logs once it has run its op, with the op and its
	// outcome, so a run of the build that ran no op is refused rather than read as one that
	// executed nothing, and one that answered otherwise than this build is refused too.
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
// Its counters are atomic, the one mode a GOFLAGS of -race, which reaches this build, allows.
func newCountedRun(t *testing.T) *countedRun {
	t.Helper()
	dir := t.TempDir()
	run := &countedRun{binary: filepath.Join(dir, "counted.test"), dir: dir}
	build := exec.Command("go", "test", "-c", "-o", run.binary, "-covermode=atomic",
		"-coverpkg="+strings.Join(countedPackages(t), ","), ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building this package's tests with coverage counters: %v\n%s", err, out)
	}
	run.baseline = run.statements(t, heavyOp{mode: "none"}, heavyOutcome{})
	if run.baseline == 0 {
		t.Fatal("the counted build executed no statement before any op: its counters count nothing, so no count from it says anything")
	}
	return run
}

// statements is how many statements of countedPackages one run of op executes in the counted
// build, less the baseline: the run's coverage profile, each block's statements times its count.
// It fails t unless the run answered want, the outcome this build's own run of op gave.
func (r *countedRun) statements(t *testing.T, op heavyOp, want heavyOutcome) uint64 {
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
	if answered := countedHelperRan + op.String() + ": " + want.String(); !strings.Contains(string(out), answered) {
		t.Fatalf("the counted build answered %s otherwise than this build, %q: a counter changed what it reads, so its count is of another path:\n%s", op, want, out)
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

// work is op's counts: its statements in the counted build, and the bytes it allocates here,
// where it runs once more for its outcome, which the counted build's must match. Both are held to
// their bounds for each character (holdsCountsPerCharacter) wherever the rows count them.
func (r *countedRun) work(t *testing.T, op heavyOp) (callWork, heavyOutcome) {
	t.Helper()
	call, err := op.prepare()
	if err != nil {
		t.Fatal(err)
	}
	var outcome heavyOutcome
	work := workOf(t, op.String(), func() { outcome = call() })
	work.steps = r.statements(t, op, outcome)
	work.characters = outcome.characters
	return work, outcome
}

// TestHeavyBodiesCountedHelper is the run of the counted build the rows count: it prepares the op
// countedHelperEnv names, runs it once, and logs countedHelperRan with it and its outcome.
func TestHeavyBodiesCountedHelper(t *testing.T) {
	t.Parallel()
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
	t.Log(countedHelperRan + op.String() + ": " + call().String())
}

// cpuGrowsLinearly measures the CPU time of mode on shape at its length and at cpuCheckLengthOf
// times it, and fails t, naming them, where either spends more than cpuCeilingPerCharacter for
// each of its characters or the time for each character grew more than cpuGrowthPerCharacter
// times (cpuGrowthVerdict). Each answers the verdict the shape is written to get. In a race build
// it measures nothing (race_disabled_test.go).
func cpuGrowsLinearly(t *testing.T, mode string, shape heavybodies.Shape) {
	t.Helper()
	if raceEnabled {
		t.Logf("%s %s: no CPU time is measured in a race build (race_disabled_test.go)", mode, shape.Name)
		return
	}
	var perCharacter [2]float64
	var characters [2]int
	for i, size := range [2]int{shape.Grown, cpuCheckLengthOf(shape) * shape.Grown} {
		op := heavyOp{mode: mode, shape: shape.Name, size: size}
		spent, runs, outcome := cpuOf(t, op)
		expectVerdict(t, mode, shape, outcome)
		characters[i] = outcome.characters
		perCharacter[i] = float64(spent.Nanoseconds()) / float64(runs*outcome.characters)
		if limit := float64(cpuCeilingPerCharacter.Nanoseconds()); perCharacter[i] > limit {
			t.Errorf("%s %s: spent %s of CPU time in %d runs on %d characters, %.0f ns a character, past the %.0f allowed (cpuCeilingPerCharacter)",
				mode, shape.Name, spent.Round(time.Millisecond), runs, outcome.characters, perCharacter[i], limit)
		} else {
			t.Logf("%s %s: %s of CPU time in %d runs on %d characters, %.0f ns a character (at most %.0f)",
				mode, shape.Name, spent.Round(time.Millisecond), runs, outcome.characters, perCharacter[i], limit)
		}
	}
	if line, ok := cpuGrowthVerdict(characters, perCharacter); ok {
		t.Logf("%s %s: %s", mode, shape.Name, line)
	} else {
		t.Errorf("%s %s: %s", mode, shape.Name, line)
	}
}

// cpuOf is the CPU time op spends, the runs it spent it in, and the outcome of the last: op runs
// once, and again on fresh input until its runs have spent cpuMeasuredAtLeast, so the time of a
// call of a millisecond is read off many of them rather than off one reading of the clock. The
// time is this process's, user and system (processCPUTime, linear_test.go), around each call and
// not its input's making: op's own and the collector's while it runs. The runs start after a
// collection, so the collector works through their own allocations and no earlier call's, and with
// one processor for the program (GOMAXPROCS 1), so the collector's workers, which run on every
// processor the program has free, add the time of no idle one: with 288 processors, a write's
// time for each character was some five times its time on one.
//
// The time is the process's, so nothing else may run beside op: no test in this package calls
// t.Parallel, and a timed call of linear_test.go still running past its deadline (leakedCallGuard)
// fails t before op runs.
func cpuOf(t *testing.T, op heavyOp) (spent time.Duration, runs int, outcome heavyOutcome) {
	t.Helper()
	if leaking := leakedCallGuard(op.String()); leaking != "" {
		t.Fatal(leaking)
	}
	runtime.GC()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for runs == 0 || spent < cpuMeasuredAtLeast {
		call, err := op.prepare()
		if err != nil {
			t.Fatal(err)
		}
		started := processCPUTime()
		outcome = call()
		spent += processCPUTime() - started
		runs++
	}
	return spent, runs, outcome
}

// cpuMeasuredAtLeast is the CPU time cpuOf's runs of one call spend at least.
const cpuMeasuredAtLeast = 100 * time.Millisecond

// cpuGrowthVerdict is cpuGrowsLinearly's verdict on two CPU times without a *testing.T to report
// through: whether the time for each character at characters[1] is at most cpuGrowthPerCharacter
// times the time for each at characters[0], and the line that says so. A time of none at the
// first length is a failure, since a growth from nothing says nothing.
// TestCPUGrowthVerdictCatchesQuadraticWorkAndRefusesATimeOfNone calls this directly.
func cpuGrowthVerdict(characters [2]int, perCharacter [2]float64) (string, bool) {
	if perCharacter[0] <= 0 {
		return fmt.Sprintf("measured no CPU time at %d characters, so its growth says nothing", characters[0]), false
	}
	growth := perCharacter[1] / perCharacter[0]
	if growth > cpuGrowthPerCharacter {
		return fmt.Sprintf("CPU time grew from %.0f ns a character at %d characters to %.0f at %d, %.1f times, past the %.1f times work linear in a body's length may grow (cpuGrowthPerCharacter)",
			perCharacter[0], characters[0], perCharacter[1], characters[1], growth, cpuGrowthPerCharacter), false
	}
	return fmt.Sprintf("CPU time %.0f ns a character at %d characters and %.0f at %d, %.1f times (at most %.1f)",
		perCharacter[0], characters[0], perCharacter[1], characters[1], growth, cpuGrowthPerCharacter), true
}

// cpuGrowthVerdict passes work linear in its input at cpuCheckLength, as dear at the larger length
// as the dearest of these rows measured, fails work quadratic in it, and fails a time of none,
// which would pass any growth at all.
func TestCPUGrowthVerdictCatchesQuadraticWorkAndRefusesATimeOfNone(t *testing.T) {
	t.Parallel()
	characters := [2]int{32_000, cpuCheckLength * 32_000}
	for _, test := range []struct {
		name         string
		perCharacter [2]float64
		pass         bool
	}{
		{"linear work", [2]float64{2_000, 2_000}, true},
		{"linear work, dearer at the larger length", [2]float64{2_000, 2_000 * cpuMeasuredGrowth}, true},
		{"quadratic work", [2]float64{2_000, 2_000 * cpuCheckLength}, false},
		{"no time measured", [2]float64{0, 2_000}, false},
	} {
		line, pass := cpuGrowthVerdict(characters, test.perCharacter)
		if pass != test.pass {
			t.Errorf("%s: passed %t, want %t (%s)", test.name, pass, test.pass, line)
		}
		t.Logf("%s: %s", test.name, line)
	}
}

// cpuCheckLength is how many times its length each shape is also read, and each stored shape
// written whole, at, for its CPU time alone: work quadratic in a body's length costs
// cpuCheckLength times more a character there than at its length, where linear work costs no
// more.
const cpuCheckLength = 8

// cpuCheckLengthOf is how many times its length shape is read and written whole at
// (cpuCheckLength), and footnoteCheckLength for the footnote references and their definitions.
func cpuCheckLengthOf(shape heavybodies.Shape) int {
	if shape.ID == "footnote-references-then-definitions" {
		return footnoteCheckLength
	}
	return cpuCheckLength
}

// footnoteCheckLength is how many times its length the footnote shape is read and written whole
// at: the resolution that walked every definition for each reference was a small part of the
// shape's CPU time at its length, about a tenth of the whole write's, so at cpuCheckLength times
// it its read grew 2.0 to 4.2 times and its whole write 1.0 to 2.7, around the bound
// (cpuGrowthPerCharacter). At 32 times, with main's resolution put back (footnotes.go of
// 33a29d1967), its read grew 21.9 to 27.6 times and its whole write 10.2 to 17.0 in each of four
// runs, and with this one 1.4 and 1.2; the read of the 1,101,436 characters takes about 0.7 s of
// CPU time and the whole write about 3 s.
const footnoteCheckLength = 32

// cpuMeasuredGrowth is the most the CPU time for each character grew from a shape's length to
// cpuCheckLength times it, among every read and whole write of these rows, measured on 2026-11-22
// on one pair of CPUs of a 288-CPU machine at a load of 810 to 980, twice with the pair otherwise
// idle and twice beside 8 busy loops on it: 1.5 times, idle, and 1.4 beside the loops, both the
// read of the 100 items nested by indentation. The loops moved each call's time for each
// character by 0.74 to 1.50 times, 1.15 on average, and its growth not past those.
const cpuMeasuredGrowth = 1.5

// cpuGrowthPerCharacter is how many times the CPU time for each character may grow from a shape's
// length to cpuCheckLength times it: work linear in the length grew 1.5 times at most
// (cpuMeasuredGrowth), and work quadratic in it grows cpuCheckLength times. 2.5 is two thirds
// again the most linear work grew. A quadratic cost small beside the rest of a call at the
// shape's length grows the whole less: the footnote resolution that walked every definition for
// each reference grew the footnote body's read 2.0 to 4.2 times at cpuCheckLength times its
// length, and the footnote shape is read at footnoteCheckLength times it for that reason.
const cpuGrowthPerCharacter = 2.5

// cpuCeilingPerCharacter is the most CPU time a measured call may spend for each character it
// reads: ten times the most any read or whole write of these rows spent in the measurement
// cpuMeasuredGrowth names, 14,143 ns a character beside the loops (the whole write of the 49
// items nested by indentation at cpuCheckLength times its length) against 13,688 idle. It holds
// what is dear at both lengths, where the growth says nothing.
const cpuCeilingPerCharacter = 142 * time.Microsecond

// callWork is what one call costs in the counts this file holds a parse and a write to: the
// statements it executes in countedPackages and the bytes it allocates on the heap, for the
// characters it read.
type callWork struct {
	steps, bytes uint64
	characters   int
}

// workOf is the bytes op allocates, labelled what: runtime.MemStats.TotalAlloc between a reading
// before op and one after it. ReadMemStats stops the world and flushes every processor's cache of
// spans before it reads, so the bytes are every allocation made between the two readings, whether
// or not a collection ran between them. They are the same in every run of a call but for what
// sync.Pool hands back: a collection empties it, and a race build drops a quarter of what is put
// back, so a pooled object is made again (bytesPerCharacter has the measure).
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
// count is the work itself and needs no sample, holds each to its bounds for each character
// (holdsCountsPerCharacter), and fails t at the first size whose statements or whose bytes grew
// more than allowance times faster than the size did (countGrowthVerdict).
func workGrowsLinearly(t *testing.T, what string, sizes []int, allowance int, measure func(size int) callWork) {
	t.Helper()
	works := make([]callWork, len(sizes))
	for i, size := range sizes {
		works[i] = measure(size)
		holdsCountsPerCharacter(t, what, works[i])
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
	t.Parallel()
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
