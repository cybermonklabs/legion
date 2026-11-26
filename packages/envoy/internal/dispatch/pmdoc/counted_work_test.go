package pmdoc

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
)

// THE COUNTED WORK OF A SHAPE (INFRA-3149). The rows that hold a read, a render or a write to work
// linear in its input count that work with Go's coverage counters, which the compiler puts in every
// basic block of each package it instruments, so no loop of those packages escapes them. They once
// counted steps this package kept of itself (ParseWork, RenderWork): a loop that called no counter
// passed them, as goldmark's link parser and imageNesting.Parse's walks over a run of 15,000
// brackets did, counted as 17 steps for 30,000 characters (dispatch://INFRA-3149, the
// parsework-brackets-finding artifact).
//
// countedBinary builds this package's test binary once for the whole run, with -cover
// -covermode=count and -coverpkg naming every package pmdoc imports that is not the standard
// library's: pmdoc, the packages of this module it imports, goldmark, ygo, golang.org/x/text and
// the rest (go list -deps). A shape runs in that binary as a child process
// (TestCountedWorkOfOneShape), selected by the environment countedRun sets, so nothing a row does
// in its own process is counted. The child runs twice at each size: once to prepare the shape's
// input alone, and once to prepare it and run the shape's op. The work of the op is the statements
// of each block times the times it ran, summed over the profile the second run writes, less the
// same sum over the first's: what the binary's start and the shape's preparing cost, ygo's document
// a write replaces among it, is never the op's. The child is bounded only by the shared wait
// (testwait.Context): a run that never ends fails the row at the binary's deadline, naming the run.
//
// The one gap: superlinear work inside the standard library, which the counters do not instrument,
// called from a linear loop of a counted package. A call to strings.Index, bytes.Equal, a regexp,
// unicode's tables or the runtime's copy of a string or a slice counts as the one statement that
// makes the call, whatever it reads. Two counts narrow it, each judged beside the counted work and
// never in its place: the bytes the op allocates (allocatedBy), which see a copy that allocates;
// and the bytes the standard library's searches read where this package keeps a count of them
// (searchedBytes: the inline raw HTML walks' bytes.Index and regexps, the render's
// strings.IndexByte). A search or a scan of the standard library's that allocates nothing and that
// no such count sits on, called once for each piece of the input over the rest of it, is seen by
// none of the three.
//
// A block's counter is 32 bits wide, so a block run 2^32 times or more in one run reads only what
// is left of its count past the last wrap, and the work summed from it is no count of the work at
// all. The counts are read from the counters the run itself writes (-test.gocoverdir), through go
// tool covdata textfmt, one line a block, and two rules fail a run that comes near a wrap, each
// for its reason (countedVerdict):
//
//   - a block counted 2^31 times or more (countedBlockLimit) fails its run, naming the block: a
//     shape's larger size holds growthStep times the smaller's units (alternatingBlocks four
//     times), so a count past half the counter's range at one size can pass 2^32 at the next,
//     and the sum is then no longer a count of the work;
//   - a larger size whose work is below the smaller's fails: work that shrinks as the input grows
//     is a counter that wrapped, not a reading.
//
// What neither sees is a block that wrapped at both sizes and reads below 2^31 at each: its
// readings are then what is left of each count, and only the per-unit bounds can still catch it.
// Each readings line names the busiest block's count at each size, and no fixed shape comes near
// 2^31.

// The environment TestCountedWorkOfOneShape reads: the row and the shape to run, the size to run it
// at, and whether to run the op ("1") or only prepare its input ("0").
const (
	countedRowEnv   = "PMDOC_COUNTED_ROW"
	countedShapeEnv = "PMDOC_COUNTED_SHAPE"
	countedSizeEnv  = "PMDOC_COUNTED_SIZE"
	countedOpEnv    = "PMDOC_COUNTED_OP"
)

// countedMark begins the line a counted run writes to its output: the units its input holds, the
// bytes its op allocated and what its stage counted.
const countedMark = "pmdoc counted run: "

// countedShape is one input a row holds to work linear in its size, counted at two sizes.
type countedShape struct {
	name string
	// sizes are the smaller and the larger size the shape is counted at, as prepare reads them.
	sizes [2]int
	// unit names what prepare's units count, in the failure text: "character", "block".
	unit string
	// prepare builds the shape's input at size and returns how many units it holds and op, the
	// call whose work is counted. Everything prepare does runs in the prepare-only run too, so none
	// of it is counted as op's.
	prepare func(t *testing.T, size int) (units int, op func())
	// reading is what the shape's op counted a unit when its bounds were set (countedMargin).
	reading countedReading
	// stage, where it is set, is this package's own count of a named stage of the op's work, read
	// before and after the op (counted's stage). For a parse or a render it is searchedBytes, which
	// countedVerdict judges beside the counted work; alternatingShape's is UpdateWork, which its row
	// judges itself.
	stage func() uint64
}

// searchedBytes is the bytes the standard library's searches read for a parse (ParseWork, the
// inline raw HTML walks' bytes.Index and regexps) and a render (RenderWork, the forward scans'
// strings.IndexByte): the one part of their work the coverage counters cannot see, since they do
// not instrument the standard library. Every parse and render shape counts it as its stage.
func searchedBytes() uint64 { return ParseWork() + RenderWork() }

// countedReading is a reading of a shape's counted work, from which its bounds are set: past, the
// statements each unit past the smaller size cost, (work(larger) - work(smaller)) / (units(larger) -
// units(smaller)); and most, the statements a unit at whichever size cost more a unit.
type countedReading struct{ past, most float64 }

// countedMargin is how far from its reading a shape's work a unit may move before it fails: its
// floor is a quarter of its reading past the smaller size, so a count that cannot see the shape's
// work fails, and its ceiling four times its reading a unit, which a pass that read its input again
// for each piece of it passes at either size. A change that moves a shape's work a unit fourfold
// either way asks for its readings to be taken again, from the readings line its row logs.
const countedMargin = 4

// countedBlockLimit is the count at which a block's counter, 32 bits wide, is half way to wrapping:
// a block counted this often at one size can wrap at the next, so its run fails (countedVerdict).
const countedBlockLimit = 1 << 31

// countedWorkIsLinear counts shape's op at each of its two sizes (countedRun) and fails t at each
// bound the counts break (countedVerdict), logging the readings either way.
func countedWorkIsLinear(t *testing.T, row string, shape countedShape) [2]counted {
	t.Helper()
	var runs [2]counted
	for i, size := range shape.sizes {
		runs[i] = countedRun(t, row, shape, size)
	}
	failures, readings := countedVerdict(shape, runs)
	if len(failures) > 0 {
		t.Errorf("%s: %s (%s)", shape.name, strings.Join(failures, "; "), readings)
		return runs
	}
	t.Logf("%s: %s", shape.name, readings)
	return runs
}

// countedVerdict judges the counted runs of shape at its two sizes, without a *testing.T to report
// through, and returns one line for each bound they break and one line of their readings:
//
//   - the work grows no faster than linearSlack times the units: a pass linear in its input grows
//     about as fast as the units, less while a fixed cost counts, and a pass that reads its input
//     again for each piece of it about as fast as their square;
//   - each unit past the smaller size costs at least the shape's floor, a quarter of its reading,
//     so the counters saw the op read every unit;
//   - each unit costs at most the shape's ceiling at either size, four times its reading;
//   - the bytes the standard library's searches read (searchedBytes), where the op's stage counts
//     any, grow no faster than linearSlack times the units: a search asked again from each piece of
//     the input over the rest of it, which the counters see as one statement a search, is seen
//     here;
//   - no block is counted countedBlockLimit times or more at either size, since past that the
//     next size can wrap its 32-bit counter and the work summed from it is no longer a count;
//   - the work at the larger size is not below the work at the smaller, since work that shrinks
//     as the input grows is a counter that wrapped, not a reading.
func countedVerdict(shape countedShape, runs [2]counted) (failures []string, readings string) {
	small, large := runs[0], runs[1]
	growth := float64(large.units) / float64(small.units)
	ratio := float64(large.work) / float64(small.work)
	past := float64(large.work-small.work) / float64(large.units-small.units)
	perUnit := [2]float64{float64(small.work) / float64(small.units), float64(large.work) / float64(large.units)}
	byteRatio := float64(large.bytes) / float64(small.bytes)
	limit := linearSlack * growth
	floor, ceiling := shape.reading.past/countedMargin, shape.reading.most*countedMargin
	readings = fmt.Sprintf("work(%d %ss) = %d (%s), %.1f a %s; work(%d %ss) = %d (%s), %.1f a %s; ratio %.2f for %.2f times the %ss "+
		"(at most %.2f); %.1f a %s past %d (at least %.1f; at most %.1f a %s); bytes %d then %d, ratio %.2f (at most %.2f); "+
		"searched bytes %d then %d; busiest block %d then %d",
		small.units, shape.unit, small.work, byModuleText(small.byModule), perUnit[0], shape.unit,
		large.units, shape.unit, large.work, byModuleText(large.byModule), perUnit[1], shape.unit,
		ratio, growth, shape.unit, limit, past, shape.unit, small.units, floor, ceiling, shape.unit,
		small.bytes, large.bytes, byteRatio, limit, small.stage, large.stage, small.busiest, large.busiest)
	for _, run := range runs {
		if run.busiest >= countedBlockLimit {
			failures = append(failures, fmt.Sprintf("the block %s ran %d times at %d %ss, at or past 2^31: its counter is 32 bits "+
				"wide, so the next size can wrap it, and the work summed from it is no longer a count", run.busiestBlock, run.busiest,
				run.units, shape.unit))
		}
	}
	if large.work < small.work {
		failures = append(failures, fmt.Sprintf("the work falls from %d at %d %ss to %d at %d: work that shrinks as the input "+
			"grows is a coverage counter that wrapped, not a reading", small.work, small.units, shape.unit, large.work, large.units))
	}
	if shape.reading.past <= 0 || shape.reading.most <= 0 {
		failures = append(failures, fmt.Sprintf("no reading is recorded for this shape: set its reading from past and most "+
			"in this line, {past: %.1f, most: %.1f}", past, max(perUnit[0], perUnit[1])))
	}
	if small.work <= 0 || ratio > limit {
		failures = append(failures, fmt.Sprintf("the work grows %.2f times over %.2f times the %ss, more than %.2f: it is not "+
			"linear in its %ss", ratio, growth, shape.unit, limit, shape.unit))
	}
	if past < floor {
		failures = append(failures, fmt.Sprintf("each %s past %d costs %.1f counted statements, fewer than %.1f: the counters did "+
			"not see the op read every %s", shape.unit, small.units, past, floor, shape.unit))
	}
	for i, run := range runs {
		if perUnit[i] > ceiling {
			failures = append(failures, fmt.Sprintf("%d %ss count %.1f statements a %s, more than %.1f",
				run.units, shape.unit, perUnit[i], shape.unit, ceiling))
		}
	}
	if small.bytes == 0 || byteRatio > limit {
		failures = append(failures, fmt.Sprintf("the bytes allocated grow from %d to %d, %.2f times over %.2f times the %ss, "+
			"more than %.2f", small.bytes, large.bytes, byteRatio, growth, shape.unit, limit))
	}
	if large.stage > 0 && (small.stage == 0 || float64(large.stage)/float64(small.stage) > limit) {
		failures = append(failures, fmt.Sprintf("the bytes the standard library's searches read grow from %d to %d, more than "+
			"%.2f times over %.2f times the %ss", small.stage, large.stage, limit, growth, shape.unit))
	}
	return failures, readings
}

// byModuleText is work by module, the largest first, each module by its path's last element.
func byModuleText(byModule map[string]int64) string {
	modules := slices.Collect(maps.Keys(byModule))
	slices.SortFunc(modules, func(a, b string) int {
		if byModule[a] != byModule[b] {
			if byModule[a] > byModule[b] {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	var parts []string
	for _, module := range modules {
		if byModule[module] != 0 {
			parts = append(parts, fmt.Sprintf("%s %d", path.Base(module), byModule[module]))
		}
	}
	return strings.Join(parts, ", ")
}

// countedRows are the rows whose shapes TestCountedWorkOfOneShape runs, each by its test's name.
var countedRows = map[string]func() []countedShape{
	"TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText": delimiterAndOpenerHeavyShapes,
	"TestRenderIsLinearOnRunsFootnoteLabelsAndLineFeeds":       runFootnoteAndLineFeedShapes,
	"TestParseIsLinearInATableOfEscapedPipes":                  escapedPipeTableShapes,
	"TestBareURLShapesParseAndRenderLinearly":                  bareURLShapes,
	"TestUnclosedRawHTMLShapesParseAndRenderLinearly":          unclosedRawHTMLShapes,
	"TestUpdateIsLinearInTheBlockCount": func() []countedShape {
		return append(updateShapes(), alternatingShape())
	},
	"TestHeavyBodiesAtTheBlockCapAreStoredOrRefusedWithinParseStepsPerCharacter": heavyBodyParseShapes,
	"TestHeavyBodiesAtTheBodyCapParseAndEncodeInWorkProportionalToTheirLength":   heavyBodyWriteShapes,
	"TestParseRefusesAMebibyteOfQuotesInBoundedTime":                             quoteMarkerShapes,
	"TestParseWrittenRefusesEmphasisNested4000DeepInBoundedWork":                 nestedEmphasisShapes,
	"TestParseForWriteRefusesDeeplyNestedBareDirectivesQuickly":                  nestedDirectiveShapes,
	"TestParseResolvesUnmatchedReferencesWithoutComparingEveryDefinition":        unmatchedReferenceShapes,
}

// TestCountedWorkOfOneShape runs the one shape countedRowEnv and countedShapeEnv name, at the size
// countedSizeEnv names, and writes the units its input holds, the bytes its op allocated and what
// its stage counted. It is the child countedRun starts in the counted test binary; with no shape
// named it skips.
func TestCountedWorkOfOneShape(t *testing.T) {
	row := os.Getenv(countedRowEnv)
	if row == "" {
		t.Skipf("%s is unset: the rows that count their work run this row in the counted test binary", countedRowEnv)
	}
	shapes, ok := countedRows[row]
	if !ok {
		t.Fatalf("%s = %q names no row of countedRows", countedRowEnv, row)
	}
	name := os.Getenv(countedShapeEnv)
	index := slices.IndexFunc(shapes(), func(shape countedShape) bool { return shape.name == name })
	if index < 0 {
		t.Fatalf("%s = %q names no shape of %s", countedShapeEnv, name, row)
	}
	size, err := strconv.Atoi(os.Getenv(countedSizeEnv))
	if err != nil || size < 1 {
		t.Fatalf("%s = %q, want a size", countedSizeEnv, os.Getenv(countedSizeEnv))
	}
	shape := shapes()[index]
	units, op := shape.prepare(t, size)
	var allocated, staged uint64
	if os.Getenv(countedOpEnv) == "1" {
		var before uint64
		if shape.stage != nil {
			before = shape.stage()
		}
		allocated = allocatedBy(op)
		if shape.stage != nil {
			staged = shape.stage() - before
		}
	}
	fmt.Printf("%s%d %d %d\n", countedMark, units, allocated, staged)
}

// countedBuild is the counted test binary, built once for the whole run (countedBinary) and removed
// by TestMain once every row has run, and the go command that built it, which reads the counters
// each run of it writes (countedChild).
var countedBuild struct {
	once  sync.Once
	dir   string
	bin   string
	goCmd string
	// modules is the module of each package the binary counts, by its import path.
	modules map[string]string
	err     string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if countedBuild.dir != "" {
		if err := os.RemoveAll(countedBuild.dir); err != nil {
			fmt.Fprintf(os.Stderr, "remove the counted test binary's directory: %v\n", err)
		}
	}
	os.Exit(code)
}

// countedBinary is the path of this package's test binary built with Go's coverage counters in
// every package pmdoc imports outside the standard library, the go command that built it, and the
// module of each of those packages. The first row to ask builds it; a build that failed fails
// every row that asks.
func countedBinary(t *testing.T) (bin, goCmd string, modules map[string]string) {
	t.Helper()
	countedBuild.once.Do(func() { buildCounted(t) })
	if countedBuild.err != "" {
		t.Fatal(countedBuild.err)
	}
	return countedBuild.bin, countedBuild.goCmd, countedBuild.modules
}

// buildCounted lists the packages pmdoc imports and builds the counted test binary into a
// directory of its own. Both go commands pass -race=false: the go command reads GOFLAGS from the
// test's environment, and a developer's GOFLAGS holding -race would make it refuse them,
// -covermode=count where cgo is on (`-covermode must be "atomic"`, since the race detector takes
// only atomic counters) and -race itself, the list included, where it is off (`-race requires
// cgo`). The counts read here need no detector, and a flag on the command line overrides GOFLAGS's.
func buildCounted(t *testing.T) {
	goCmd, err := exec.LookPath("go")
	if err != nil {
		countedBuild.err = "go is not on PATH. The rows that count their work build this package's test binary with " +
			"Go's coverage counters, so they need the go command: go test puts its own GOROOT/bin first on a " +
			"test's PATH, and a test binary run by hand needs Go's bin directory on PATH. They fail rather than skip."
		return
	}
	const mark = "counted "
	listed, err := countedCommand(t, "go list of the packages pmdoc imports", nil, goCmd, "list", "-race=false", "-deps",
		"-f", "{{if .Module}}"+mark+"{{.Module.Path}} {{.ImportPath}}{{end}}", ".")
	if err != nil {
		countedBuild.err = err.Error()
		return
	}
	modules := map[string]string{}
	for _, line := range strings.Split(listed, "\n") {
		if counted, ok := strings.CutPrefix(line, mark); ok {
			module, pkg, _ := strings.Cut(counted, " ")
			modules[pkg] = module
		}
	}
	if len(modules) == 0 {
		countedBuild.err = "go list names no package outside the standard library among pmdoc's"
		return
	}
	dir, err := os.MkdirTemp("", "pmdoc-counted-")
	if err != nil {
		countedBuild.err = err.Error()
		return
	}
	countedBuild.dir = dir
	bin := filepath.Join(dir, "pmdoc.test")
	if _, err := countedCommand(t, "the build of the counted test binary", nil, goCmd, "test", "-c", "-race=false", "-cover",
		"-covermode=count", "-coverpkg="+strings.Join(slices.Sorted(maps.Keys(modules)), ","), "-o", bin, "."); err != nil {
		countedBuild.err = err.Error()
		return
	}
	countedBuild.bin, countedBuild.goCmd, countedBuild.modules = bin, goCmd, modules
}

// countedCommand runs name with args, and env added to the test's own, its output to one file read
// once it exits, and returns that output. A run that does not exit before the shared wait's end
// (testwait.Context), or that exits with an error, returns an error carrying its output.
func countedCommand(t *testing.T, what string, env []string, name string, args ...string) (string, error) {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		return "", err
	}
	defer out.Close()
	ctx := testwait.Context(t)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = out, out
	runErr := cmd.Run()
	text, err := os.ReadFile(out.Name())
	if err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("testwait: %s: gave up waiting for %s at the shared wait's margin before the test "+
			"binary's deadline; its output:\n%s", t.Name(), what, text)
	}
	if runErr != nil {
		return "", fmt.Errorf("%s: %v; its output:\n%s", what, runErr, text)
	}
	return string(text), nil
}

// counted is what one counted run of a shape read at one size.
type counted struct {
	// units is how many units (characters, blocks) the shape's input holds.
	units int
	// work is the statements the op ran in the counted packages, and byModule the same by module.
	work     int64
	byModule map[string]int64
	// busiest is the count of the block the op's run ran most often, preparing included, and
	// busiestBlock that block, as the profile names it: a count is 32 bits wide, so one near 2^32
	// would read less than it ran (countedBlockLimit).
	busiest      int64
	busiestBlock string
	// bytes is what the op allocated on the heap (allocatedBy), and stage what the shape's stage
	// counted of it.
	bytes, stage uint64
}

// countedRun counts shape's op at size: the run that prepares its input and runs op, less the run
// that only prepares it.
func countedRun(t *testing.T, row string, shape countedShape, size int) counted {
	t.Helper()
	prepared := countedChild(t, row, shape, size, false)
	whole := countedChild(t, row, shape, size, true)
	if prepared.units != whole.units {
		t.Fatalf("%s at %d: the prepare-only run holds %d units and the op's run %d: prepare must build the same input each time",
			shape.name, size, prepared.units, whole.units)
	}
	whole.work -= prepared.work
	for module, work := range prepared.byModule {
		whole.byModule[module] -= work
	}
	return whole
}

// countedChild runs shape at size in the counted test binary, with its op or without it, and reads
// the statements the run counted, the units it says its input holds and the bytes its op allocated.
// The counts are the ones the run writes to its coverage directory, read by go tool covdata
// textfmt. covdata reads -i as a comma-separated list of directories, so that directory is one
// under the counted binary's own (countedBuild.dir), whose name holds no comma: t.TempDir's is the
// test's name, which a shape's name ("2,000 unclosed openers …") carries a comma into.
func countedChild(t *testing.T, row string, shape countedShape, size int, op bool) counted {
	t.Helper()
	bin, goCmd, modules := countedBinary(t)
	coverDir, err := os.MkdirTemp(countedBuild.dir, "cover-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(coverDir); err != nil {
			t.Errorf("remove a counted run's coverage directory: %v", err)
		}
	})
	profile := filepath.Join(t.TempDir(), "profile.txt")
	what := fmt.Sprintf("the counted run of %s at %d", shape.name, size)
	opValue := "0"
	if op {
		opValue = "1"
	} else {
		what += ", preparing its input alone"
	}
	out, err := countedCommand(t, what, []string{countedRowEnv + "=" + row, countedShapeEnv + "=" + shape.name,
		countedSizeEnv + "=" + strconv.Itoa(size), countedOpEnv + "=" + opValue}, bin,
		"-test.run=^TestCountedWorkOfOneShape$", "-test.gocoverdir="+coverDir)
	if err != nil {
		t.Fatal(err)
	}
	var run counted
	found := false
	for _, line := range strings.Split(out, "\n") {
		if values, ok := strings.CutPrefix(line, countedMark); ok {
			if _, err := fmt.Sscanf(values, "%d %d %d", &run.units, &run.bytes, &run.stage); err != nil {
				t.Fatalf("%s wrote %q, not its units, bytes and stage: %v", what, line, err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("%s wrote no %q line; its output:\n%s", what, countedMark, out)
	}
	if _, err := countedCommand(t, "go tool covdata textfmt of "+what, nil, goCmd, "tool", "covdata", "textfmt",
		"-i="+coverDir, "-o="+profile); err != nil {
		t.Fatal(err)
	}
	run.work, run.byModule, run.busiest, run.busiestBlock = countedWork(t, profile, modules)
	return run
}

// countedWork is the work a coverage profile in Go's text format counts: the statements of each
// block times the times it ran, summed, in all and by the module of the block's package, and the
// count of the block that ran most often, with that block.
func countedWork(t *testing.T, profile string, modules map[string]string) (work int64, byModule map[string]int64, busiest int64, busiestBlock string) {
	t.Helper()
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if lines[0] != "mode: count" {
		t.Fatalf("the profile %s begins %q, not mode: count", profile, lines[0])
	}
	byModule = map[string]int64{}
	for _, line := range lines[1:] {
		block, counts, _ := strings.Cut(line, " ")
		file, _, _ := strings.Cut(block, ":")
		f := strings.Fields(counts)
		module, known := modules[path.Dir(file)]
		if len(f) != 2 || !known {
			t.Fatalf("the profile line %q is not a block of a counted package, its statements and its count", line)
		}
		stmts, err1 := strconv.ParseInt(f[0], 10, 64)
		count, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 != nil || err2 != nil {
			t.Fatalf("the profile line %q is not a block, its statements and its count", line)
		}
		work += stmts * count
		byModule[module] += stmts * count
		if count > busiest {
			busiest, busiestBlock = count, block
		}
	}
	return work, byModule, busiest, busiestBlock
}
