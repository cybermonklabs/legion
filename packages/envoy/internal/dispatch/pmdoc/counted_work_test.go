package pmdoc

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/coverage"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/fold"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The parent names the operation and the directories in which the child takes its snapshots.
const (
	countedModeEnv  = "PMDOC_COUNTED_MODE"
	countedRowEnv   = "PMDOC_COUNTED_ROW"
	countedShapeEnv = "PMDOC_COUNTED_SHAPE"
	countedOpEnv    = "PMDOC_COUNTED_OP"
	countedSizeEnv  = "PMDOC_COUNTED_SIZE"
	countedPOEnv    = "PMDOC_COUNTED_PO"
	countedOEnv     = "PMDOC_COUNTED_O"
)

// countedMark begins the line that proves the requested child operation ran and reports its result.
const countedMark = "pmdoc counted run: "

// countedShape pairs a verdict with the operation that builds its readings. prepare reports the
// actual input units and body length. The operation returns its outcome, including any refusal,
// encoded length, or failed result check that the parent must judge.
type countedShape struct {
	countedLeaf
	shape, operation string
	sizes            [2]int
	limitChildren    bool
	prepare          func(t *testing.T, size int) (units, characters int, run func() string)
}

// countedRows lets the child find the same shape and operation that the parent selected.
var countedRows = map[string]func() []countedShape{
	"TestCountedBracketParagraph": countedBracketShapes,
}

// countedStages keeps the four independent readings around the operation, not its preparation.
type countedStages struct {
	Parse, Render, Update, Scan uint64
}

// readCountedStages reads the process counters in the isolated child.
func readCountedStages() countedStages {
	return countedStages{Parse: ParseWork(), Render: RenderWork(), Update: UpdateWork(), Scan: ScanWork()}
}

// countedMeasurement is what one child's operation reports apart from the coverage snapshots.
type countedMeasurement struct {
	Size, Units, Characters int
	Bytes                   uint64
	Stages                  countedStages
	Outcome                 string
}

// countedBuild keeps the coverage tools and the startup baseline for the life of this test binary.
var countedBuild struct {
	buildOnce, noneOnce  sync.Once
	dir, binary, covdata string
	modules              map[string]string
	buildErr             error
	none                 countedStatementRun
	noneErr              error
}

// TestMain removes the shared build directory after all tests that use it have ended.
func TestMain(m *testing.M) {
	switch os.Getenv(countedModeEnv) {
	case "count", "none":
		if code := initializeCountedSnapshots(m); code != 0 {
			os.Exit(code)
		}
	}
	code := m.Run()
	if countedBuild.dir != "" {
		if err := os.RemoveAll(countedBuild.dir); err != nil {
			fmt.Fprintf(os.Stderr, "remove the counted build: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// initializeCountedSnapshots runs no tests. A Go test binary initializes the coverage metadata
// hash and counter mode in m.Run's teardown, not on entry (internal/coverage/cfile.InitHook).
// Run that teardown once before the real helper starts so the public snapshot APIs can work.
func initializeCountedSnapshots(m *testing.M) int {
	flag.Parse()
	run := flag.Lookup("test.run").Value
	target := run.String()
	// The testing package defines test.run as a string flag, so both Set calls accept any string.
	_ = run.Set("^$")
	code := m.Run()
	_ = run.Set(target)
	return code
}

// countedCommand waits on the test's deadline and keeps the command's output with a failure.
func countedCommand(t *testing.T, what, binary string, args ...string) ([]byte, error) {
	t.Helper()
	ctx := testwait.Context(t)
	// A subprocess of go can inherit these descriptors. A file lets the wait end
	// when go exits, even if such a subprocess still holds them.
	log, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer log.Close()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout, cmd.Stderr = log, log
	runErr := cmd.Run()
	out, err := os.ReadFile(log.Name())
	if err != nil {
		return nil, fmt.Errorf("%s: read command output: %w", what, err)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("testwait: %s: gave up waiting for %s at the test's deadline: %w\n%s", t.Name(), what, ctx.Err(), out)
	}
	if runErr != nil {
		return nil, fmt.Errorf("%s: %w\n%s", what, runErr, out)
	}
	return out, nil
}

// buildCounted builds every non-standard dependency with atomic coverage so the child can clear
// its counters. The command-line -race=false overrides GOFLAGS, including when cgo is disabled.
// The build and its snapshots share a directory because covdata treats commas in -i as separators.
func buildCounted(t *testing.T) error {
	t.Helper()
	listed, err := countedCommand(t, "list the packages to count", "go", "list", "-race=false", "-deps",
		"-f", "{{if not .Standard}}{{.ImportPath}} {{with .Module}}{{.Path}}{{end}}{{end}}", ".")
	if err != nil {
		return err
	}
	var packages []string
	modules := make(map[string]string)
	for _, line := range strings.Split(string(listed), "\n") {
		if line == "" {
			continue
		}
		pkg, module, ok := strings.Cut(line, " ")
		if !ok || pkg == "" || module == "" || strings.ContainsAny(module, " \t") {
			return fmt.Errorf("go list did not name a package and its module in %q", line)
		}
		packages = append(packages, pkg)
		modules[pkg] = module
	}
	if len(packages) < 2 {
		return fmt.Errorf("go list named %d packages to count, want pmdoc and its non-standard dependencies", len(packages))
	}
	if _, ok := modules["github.com/yuin/goldmark/extension"]; !ok {
		return fmt.Errorf("go list did not name github.com/yuin/goldmark/extension among the counted packages")
	}
	root, err := filepath.Abs(os.TempDir())
	if err != nil {
		return err
	}
	if strings.Contains(root, ",") {
		return fmt.Errorf("temporary directory %q contains a comma, which covdata treats as an input separator", root)
	}
	dir, err := os.MkdirTemp(root, "pmdoc-counted-")
	if err != nil {
		return err
	}
	countedBuild.dir = dir
	countedBuild.binary = filepath.Join(dir, "pmdoc.test")
	countedBuild.covdata = filepath.Join(dir, "covdata")
	if _, err := countedCommand(t, "build the counted pmdoc test binary", "go", "test", "-c", "-race=false",
		"-cover", "-covermode=atomic", "-coverpkg="+strings.Join(packages, ","), "-o", countedBuild.binary, "."); err != nil {
		return err
	}
	// Build the tool once rather than letting go tool covdata rebuild it on each snapshot.
	if _, err := countedCommand(t, "build covdata", "go", "build", "-race=false", "-o", countedBuild.covdata, "cmd/covdata"); err != nil {
		return err
	}
	countedBuild.modules = modules
	return nil
}

// countedSnapshots reserves directories with no test name, which can also contain a comma.
func countedSnapshots(dir string, withPreparation bool) (operation, combined string, err error) {
	operation = filepath.Join(dir, "O")
	if err := os.Mkdir(operation, 0o700); err != nil {
		return "", "", err
	}
	if withPreparation {
		combined = filepath.Join(dir, "PO")
		if err := os.Mkdir(combined, 0o700); err != nil {
			return "", "", err
		}
	}
	return operation, combined, nil
}

// readCountedSnapshot converts the snapshot to the same text format the fixture exercises.
func readCountedSnapshot(t *testing.T, dir, what string) (countedProfile, error) {
	t.Helper()
	profile := filepath.Join(dir, "profile.txt")
	if _, err := countedCommand(t, "read "+what, countedBuild.covdata, "textfmt", "-i="+dir, "-o="+profile); err != nil {
		return countedProfile{}, err
	}
	file, err := os.Open(profile)
	if err != nil {
		return countedProfile{}, err
	}
	defer file.Close()
	result, err := parseCountedProfile(file, countedBuild.modules)
	if err != nil {
		return countedProfile{}, fmt.Errorf("read %s: %w", what, err)
	}
	return result, nil
}

// runCountedChild requires exactly one measurement, so a child that skipped does not look like a
// child that measured no work. The log identifies the operation if the shared wait has to kill it.
func runCountedChild(t *testing.T, binary, what string, env ...string) (countedMeasurement, error) {
	t.Helper()
	t.Logf("starting %s", what)
	out, stderr, err := testwait.RunChild(t, binary, 0, "^TestHeavyBodiesCountedHelper$", env...)
	if err != nil {
		return countedMeasurement{}, fmt.Errorf("%s: %w\n%s\n%s", what, err, out, stderr)
	}
	var result countedMeasurement
	found := false
	for _, line := range strings.Split(out, "\n") {
		if encoded, ok := strings.CutPrefix(line, countedMark); ok {
			if found {
				return countedMeasurement{}, fmt.Errorf("%s reported two measurements:\n%s", what, out)
			}
			if err := json.Unmarshal([]byte(encoded), &result); err != nil {
				return countedMeasurement{}, fmt.Errorf("%s reported an invalid measurement %q: %w", what, encoded, err)
			}
			found = true
		}
	}
	if !found {
		return countedMeasurement{}, fmt.Errorf("%s wrote no %q line:\n%s\n%s", what, countedMark, out, stderr)
	}
	return result, nil
}

// readCountedNone takes the startup snapshot once. It runs no builder and clears no counters.
func readCountedNone(t *testing.T) (countedStatementRun, error) {
	t.Helper()
	dir, err := os.MkdirTemp(countedBuild.dir, "none-")
	if err != nil {
		return countedStatementRun{}, err
	}
	if _, err := runCountedChild(t, countedBuild.binary, "the counted none baseline", countedModeEnv+"=none", countedOEnv+"="+dir); err != nil {
		return countedStatementRun{}, err
	}
	profile, err := readCountedSnapshot(t, dir, "the counted none baseline")
	return countedStatementRun{statements: profile.total}, err
}

// countedChildSlots limits the number of memory-hungry leaves that may use children at once.
var countedChildSlots = make(chan struct{}, 2)

// runCountedLeaf counts and judges one shape and operation at both sizes in isolation.
func runCountedLeaf(t *testing.T, row string, leaf countedShape) [2]countedSample {
	t.Helper()
	t.Parallel()
	countedBuild.buildOnce.Do(func() { countedBuild.buildErr = buildCounted(t) })
	if countedBuild.buildErr != nil {
		t.Fatal(countedBuild.buildErr)
	}
	var none *countedStatementRun
	if leaf.heavyBody {
		countedBuild.noneOnce.Do(func() { countedBuild.none, countedBuild.noneErr = readCountedNone(t) })
		if countedBuild.noneErr != nil {
			t.Fatal(countedBuild.noneErr)
		}
		none = &countedBuild.none
	}
	if leaf.limitChildren {
		ctx := testwait.Context(t)
		select {
		case countedChildSlots <- struct{}{}:
			defer func() { <-countedChildSlots }()
		case <-ctx.Done():
			t.Fatalf("wait for a counted child slot for %s: %v", leaf.name, ctx.Err())
		}
	}
	var samples [2]countedSample
	for i, size := range leaf.sizes {
		samples[i] = readCountedSample(t, row, leaf, size, none)
	}
	failures, readings := countedVerdict(leaf.countedLeaf, samples)
	t.Log(readings)
	for _, failure := range failures {
		t.Error(failure)
	}
	return samples
}

// readCountedSample reads O and, for a heavy body, PO. Those snapshots omit startup because the
// child clears its counters. On the original scale prepared = none + P and whole = none + P + O.
// Thus whole - prepared is O and whole - none is P + O. Restore the common baseline before
// passing the readings to countedStatementDifferences, which also checks the input units.
func readCountedSample(t *testing.T, row string, leaf countedShape, size int, none *countedStatementRun) countedSample {
	t.Helper()
	what := fmt.Sprintf("%s at %d", leaf.name, size)
	dir, err := os.MkdirTemp(countedBuild.dir, "sample-")
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove the snapshots for %s: %v", what, err)
		}
	}()
	opDir, bothDir, err := countedSnapshots(dir, none != nil)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	env := []string{countedRowEnv + "=" + row, countedShapeEnv + "=" + leaf.shape,
		countedOpEnv + "=" + leaf.operation, countedSizeEnv + "=" + strconv.Itoa(size)}
	measured, err := runCountedChild(t, countedBuild.binary, "the counted run of "+what,
		append(env, countedModeEnv+"=count", countedOEnv+"="+opDir, countedPOEnv+"="+bothDir)...)
	if err != nil {
		t.Fatal(err)
	}
	if measured.Size != size {
		t.Fatalf("%s: the counted child built size %d, want %d", what, measured.Size, size)
	}
	op, err := readCountedSnapshot(t, opDir, "the operation snapshot for "+what)
	if err != nil {
		t.Fatal(err)
	}
	prepared := countedStatementRun{units: measured.Size}
	whole := countedStatementRun{units: measured.Units, statements: op.total}
	busiest, busiestBlock := op.busiest, op.busiestBlock
	var invokingBytes uint64
	if none != nil {
		both, err := readCountedSnapshot(t, bothDir, "the combined snapshot for "+what)
		if err != nil {
			t.Fatal(err)
		}
		if both.total < op.total || both.total > math.MaxUint64-none.statements {
			t.Fatalf("%s: cannot combine %d statements with preparation, %d operation-only statements and a none baseline of %d", what, both.total, op.total, none.statements)
		}
		whole.statements = none.statements + both.total
		prepared.statements = whole.statements - op.total
		if both.busiest > busiest {
			busiest, busiestBlock = both.busiest, both.busiestBlock
		}
	}
	operation, withPreparation, err := countedStatementDifferences(leaf.name, size, none, prepared, whole)
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		plain, err := runCountedChild(t, os.Args[0], "the invoking build's run of "+what, append(env, countedModeEnv+"=plain")...)
		if err != nil {
			t.Fatal(err)
		}
		if plain.Outcome != measured.Outcome || plain.Units != measured.Units || plain.Characters != measured.Characters {
			t.Fatalf("%s: counted build answered %q (%d units, %d characters), invoking build answered %q (%d units, %d characters): a counter changed what it reads",
				what, measured.Outcome, measured.Units, measured.Characters, plain.Outcome, plain.Units, plain.Characters)
		}
		invokingBytes = plain.Bytes
	}
	return countedSample{
		units: measured.Units, characters: measured.Characters, operation: operation, withPreparation: withPreparation,
		operationByModule: op.byModule, countedBytes: measured.Bytes, invokingBytes: invokingBytes,
		searchedBytes: measured.Stages.Parse + measured.Stages.Render, scanBytes: measured.Stages.Scan,
		updateItems: measured.Stages.Update, busiest: busiest, busiestBlock: busiestBlock, outcome: measured.Outcome,
	}
}

// TestHeavyBodiesCountedHelper runs only the child operation. Until the old rows move to the new
// harness, it also accepts their original request and reports the same outcome in their format.
func TestHeavyBodiesCountedHelper(t *testing.T) {
	t.Parallel()
	mode := os.Getenv(countedModeEnv)
	if mode == "" {
		text := os.Getenv(countedHelperEnv)
		if text == "" {
			t.Skip("the child process the counted rows run to measure work")
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
		return
	}
	if mode == "census" {
		writeCensus(t)
		return
	}
	operationDir, combinedDir := os.Getenv(countedOEnv), os.Getenv(countedPOEnv)
	if mode == "none" {
		writeCountedSnapshot(t, operationDir)
		printCountedMeasurement(t, countedMeasurement{})
		return
	}
	if mode != "count" && mode != "plain" {
		t.Fatalf("%s = %q, want count, plain, none, or census", countedModeEnv, mode)
	}
	row, shape, operation := os.Getenv(countedRowEnv), os.Getenv(countedShapeEnv), os.Getenv(countedOpEnv)
	shapes, ok := countedRows[row]
	if !ok {
		t.Fatalf("%s = %q names no counted row", countedRowEnv, row)
	}
	size, err := strconv.Atoi(os.Getenv(countedSizeEnv))
	if err != nil || size <= 0 {
		t.Fatalf("%s = %q, want a positive size", countedSizeEnv, os.Getenv(countedSizeEnv))
	}
	for _, leaf := range shapes() {
		if leaf.shape != shape || leaf.operation != operation {
			continue
		}
		if mode == "count" && leaf.heavyBody != (combinedDir != "") {
			t.Fatalf("%s: combined snapshot directory %q does not match heavy body setting %t", leaf.name, combinedDir, leaf.heavyBody)
		}
		printCountedMeasurement(t, measureCountedOperation(t, leaf, size, mode == "count", operationDir, combinedDir))
		return
	}
	t.Fatalf("%s has no counted shape %q with operation %q", row, shape, operation)
}

// measureCountedOperation measures allocation and stage counters only around the operation. The
// two files in PO are summed by covdata, while O contains only the second counter snapshot.
func measureCountedOperation(t *testing.T, leaf countedShape, size int, counted bool, operationDir, combinedDir string) countedMeasurement {
	t.Helper()
	clear := func() {
		if err := coverage.ClearCounters(); err != nil {
			t.Fatalf("clear coverage counters: %v", err)
		}
	}
	if counted && combinedDir != "" {
		clear()
	}
	units, characters, run := leaf.prepare(t, size)
	if counted {
		if combinedDir != "" {
			writeCountedSnapshot(t, combinedDir)
		}
		clear()
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := readCountedStages()
	outcome := run()
	end := readCountedStages()
	runtime.ReadMemStats(&after)
	if counted {
		if combinedDir != "" {
			writeCountedSnapshot(t, combinedDir)
		}
		writeCountedSnapshot(t, operationDir)
	}
	return countedMeasurement{Size: size, Units: units, Characters: characters,
		Bytes: after.TotalAlloc - before.TotalAlloc, Outcome: outcome,
		Stages: countedStages{Parse: end.Parse - start.Parse, Render: end.Render - start.Render,
			Update: end.Update - start.Update, Scan: end.Scan - start.Scan}}
}

// writeCountedSnapshot writes metadata and a new counter file without resetting the counters.
func writeCountedSnapshot(t *testing.T, dir string) {
	t.Helper()
	if err := coverage.WriteMetaDir(dir); err != nil {
		t.Fatalf("write coverage metadata to %q: %v", dir, err)
	}
	if err := coverage.WriteCountersDir(dir); err != nil {
		t.Fatalf("write coverage counters to %q: %v", dir, err)
	}
}

// printCountedMeasurement reports the result after all requested snapshots have been written.
func printCountedMeasurement(t *testing.T, result countedMeasurement) {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("%s%s\n", countedMark, encoded)
}

// countedProfile holds the total and module totals, and the block whose counter is nearest to
// wrapping. The block keeps the source position from the profile for the failure message.
type countedProfile struct {
	total, busiest uint64
	byModule       map[string]uint64
	busiestBlock   string
}

// countedPosition matches the start and end positions after a file name in a covdata profile.
var countedPosition = regexp.MustCompile(`^[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$`)

// parseCountedProfile sums the statements times executions in an atomic coverage profile. It
// refuses unknown packages and numbers outside the nonnegative signed 64-bit range accepted by
// both old readers, and never returns a sum that wrapped.
func parseCountedProfile(r io.Reader, modules map[string]string) (countedProfile, error) {
	lines := bufio.NewScanner(r)
	if !lines.Scan() && lines.Err() != nil {
		return countedProfile{}, lines.Err()
	}
	if lines.Text() != "mode: atomic" {
		return countedProfile{}, fmt.Errorf("coverage mode: got %q, want mode: atomic", lines.Text())
	}
	result := countedProfile{byModule: make(map[string]uint64)}
	for lines.Scan() {
		line := lines.Text()
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return countedProfile{}, fmt.Errorf("profile line %q: want a block position, statements and count", line)
		}
		block := fields[0]
		file, pos, _ := strings.Cut(block, ":")
		if file == "" || !countedPosition.MatchString(pos) {
			return countedProfile{}, fmt.Errorf("profile line %q: invalid block position", line)
		}
		pkg := path.Dir(file)
		module, ok := modules[pkg]
		if !ok {
			return countedProfile{}, fmt.Errorf("profile line %q: package %q is not counted", line, pkg)
		}
		readNumber := func(field, raw string) (uint64, error) {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 || raw[0] == '-' || raw[0] == '+' {
				return 0, fmt.Errorf("profile line %q: %s %q is not a decimal integer from 0 to %d", line, field, raw, int64(math.MaxInt64))
			}
			return uint64(n), nil
		}
		statements, err := readNumber("statements", fields[1])
		if err != nil {
			return countedProfile{}, err
		}
		count, err := readNumber("count", fields[2])
		if err != nil {
			return countedProfile{}, err
		}
		hi, work := bits.Mul64(statements, count)
		if hi != 0 || work > math.MaxUint64-result.total {
			return countedProfile{}, fmt.Errorf("profile line %q: statement total overflows uint64", line)
		}
		result.total += work
		result.byModule[module] += work
		if count > result.busiest {
			result.busiest, result.busiestBlock = count, block
		}
	}
	if err := lines.Err(); err != nil {
		return countedProfile{}, err
	}
	return result, nil
}

// countedBracketShapes uses a small paragraph to exercise the shared build and both kinds of
// child. Its ASCII fold also gives the profile reader a fixture with only a few active blocks.
func countedBracketShapes() []countedShape {
	return []countedShape{{
		countedLeaf: countedLeaf{
			name: "bracket paragraph", unit: "character", heavyBody: true,
			reading: countedReading{past: 322, most: 323.677734375, scanMost: 1},
		},
		shape: "brackets", operation: "read", sizes: [2]int{2 << 10, 16 << 10},
		prepare: func(_ *testing.T, size int) (int, int, func() string) {
			body := strings.Repeat("[A", size/2)
			budget := readBackBudget()
			return len(body), len(body), func() string {
				if _, err := parseStamped(fold.ASCII(body), budget); err != nil {
					return err.Error()
				}
				return ""
			}
		},
	}}
}

// TestCountedBracketParagraph proves the real build, snapshots, invoking child and verdict agree
// on a small input. Preparation must add work, but must not enter the operation-only count.
func TestCountedBracketParagraph(t *testing.T) {
	for _, sample := range runCountedLeaf(t, t.Name(), countedBracketShapes()[0]) {
		if sample.outcome != "" {
			t.Errorf("at %d characters: read failed: %s", sample.characters, sample.outcome)
		}
		if sample.withPreparation <= sample.operation {
			t.Errorf("at %d characters: %d statements with preparation, want more than the %d operation-only statements", sample.characters, sample.withPreparation, sample.operation)
		}
	}
}

// TestTheCountedProfileReaderRefusesWhatItCannotCount uses the operation snapshot in "$d/O"
// from the 2048-character bracket child. From the package directory, the capture command was:
//
//	"$d/covdata" textfmt -i="$d/O" -pkg=github.com/sjawhar/envoy/internal/fold -o=testdata/counted-profile.txt
//
// In profile order the total is 1*1 + 1*2 + 2*1 + 1*2047 + 1*1024 + 1*1 + 1*0 + 1*0 = 3077.
func TestTheCountedProfileReaderRefusesWhatItCannotCount(t *testing.T) {
	t.Parallel()
	const module = "github.com/sjawhar/envoy"
	const pkg = module + "/internal/fold"
	data, err := os.ReadFile("testdata/counted-profile.txt")
	if err != nil {
		t.Fatal(err)
	}
	profile := string(data)
	modules := map[string]string{pkg: module}
	got, err := parseCountedProfile(strings.NewReader(profile), modules)
	if err != nil {
		t.Fatal(err)
	}
	const busiest = pkg + "/fold.go:25.5,25.35"
	if got.total != 3077 || got.byModule[module] != 3077 || got.busiest != 2047 || got.busiestBlock != busiest {
		t.Fatalf("profile read as %+v, want 3077 statements, all in %s, and 2047 runs of %s", got, module, busiest)
	}

	mode, blocks, _ := strings.Cut(profile, "\n")
	line, _, _ := strings.Cut(blocks, "\n")
	fields := strings.Fields(line)
	if len(fields) != 3 {
		t.Fatalf("first fixture block has %d fields, want 3: %q", len(fields), line)
	}
	block, statements, count := fields[0], fields[1], fields[2]
	for _, tc := range []struct {
		name, from, to, want string
	}{
		{"wrong mode", mode, "mode: set", "want mode: atomic"},
		{"unknown package", line, strings.Replace(line, pkg, "example.com/uncounted", 1), `package "example.com/uncounted" is not counted`},
		{"bad position", line, pkg + "/fold.go:bad " + statements + " " + count, "invalid block position"},
		{"missing field", line, block + " " + statements, "want a block position, statements and count"},
		{"non-integer statements", line, block + " many " + count, `statements "many" is not a decimal integer`},
		{"non-integer count", line, block + " " + statements + " many", `count "many" is not a decimal integer`},
		{"negative statements", line, block + " -1 " + count, `statements "-1" is not a decimal integer`},
		{"negative count", line, block + " " + statements + " -1", `count "-1" is not a decimal integer`},
		{"statements above the range", line, block + " 9223372036854775808 " + count, `statements "9223372036854775808" is not a decimal integer`},
		{"count above the range", line, block + " " + statements + " 9223372036854775808", `count "9223372036854775808" is not a decimal integer`},
		{"product overflows", line, block + " 9223372036854775807 3", "statement total overflows uint64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := strings.Replace(profile, tc.from, tc.to, 1)
			_, err := parseCountedProfile(strings.NewReader(changed), modules)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
