package pmdoc

import (
	"bufio"
	"encoding/json"
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
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/fold"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The parent selects the input and operation through the child's environment.
const (
	countedModeEnv  = "PMDOC_COUNTED_MODE"
	countedRowEnv   = "PMDOC_COUNTED_ROW"
	countedShapeEnv = "PMDOC_COUNTED_SHAPE"
	countedOpEnv    = "PMDOC_COUNTED_OP"
	countedSizeEnv  = "PMDOC_COUNTED_SIZE"
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

// countedBuild keeps the covered binary and the startup baseline for the life of this test binary.
var countedBuild struct {
	buildOnce, noneOnce sync.Once
	dir, binary         string
	modules             map[string]string
	buildErr            error
	none                countedStatementRun
	noneErr             error
}

// TestMain removes the shared build directory after all tests that use it have ended.
func TestMain(m *testing.M) {
	code := m.Run()
	if countedBuild.dir != "" {
		if err := os.RemoveAll(countedBuild.dir); err != nil {
			fmt.Fprintf(os.Stderr, "remove the counted build: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// countedCommand waits on the test's deadline and keeps the command's output with a failure.
func countedCommand(t *testing.T, what string, env []string, binary string, args ...string) ([]byte, error) {
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
	if len(env) != 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
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

// buildCounted instruments every non-standard dependency in count mode. The command-line
// -race=false overrides GOFLAGS, including when cgo is disabled.
func buildCounted(t *testing.T) error {
	t.Helper()
	listed, err := countedCommand(t, "list the packages to count", nil, "go", "list", "-race=false", "-deps",
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
	dir, err := os.MkdirTemp(root, "pmdoc-counted-")
	if err != nil {
		return err
	}
	countedBuild.dir = dir
	countedBuild.binary = filepath.Join(dir, "pmdoc.test")
	if _, err := countedCommand(t, "build the counted pmdoc test binary", nil, "go", "test", "-c", "-race=false",
		"-cover", "-covermode=count", "-coverpkg="+strings.Join(packages, ","), "-o", countedBuild.binary, "."); err != nil {
		return err
	}
	countedBuild.modules = modules
	return nil
}

// readCountedProfile reads the text file that testing writes when the child finishes.
func readCountedProfile(profile, what string) (countedProfile, error) {
	file, err := os.Open(profile)
	if err != nil {
		return countedProfile{}, fmt.Errorf("read %s: %w", what, err)
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
func runCountedChild(t *testing.T, binary, what, profile string, env ...string) (countedMeasurement, error) {
	t.Helper()
	t.Logf("starting %s", what)
	args := []string{"-test.run=^TestHeavyBodiesCountedHelper$", "-test.v", "-test.count=1", "-test.timeout=0"}
	if profile != "" {
		args = append(args, "-test.coverprofile="+profile)
	}
	output, err := countedCommand(t, what, env, binary, args...)
	if err != nil {
		return countedMeasurement{}, err
	}
	out := string(output)
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
		return countedMeasurement{}, fmt.Errorf("%s wrote no %q line:\n%s", what, countedMark, out)
	}
	return result, nil
}

// runProfiledCountedChild reads one covered child's report and its profile.
func runProfiledCountedChild(t *testing.T, what string, env ...string) (countedMeasurement, countedProfile, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "profile.txt")
	measured, err := runCountedChild(t, countedBuild.binary, what, file, env...)
	if err != nil {
		return countedMeasurement{}, countedProfile{}, err
	}
	profile, err := readCountedProfile(file, what)
	return measured, profile, err
}

// readCountedNone measures startup once in a child that builds no input.
func readCountedNone(t *testing.T) (countedStatementRun, error) {
	t.Helper()
	measured, profile, err := runProfiledCountedChild(t, "the counted none baseline", countedModeEnv+"=none")
	return countedStatementRun{units: measured.Units, statements: profile.total}, err
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

// readCountedSample subtracts the prepare-only child's statement total from the whole child's
// total. It checks that the difference agrees with the sum of statements times the count change
// in each block. For heavy bodies it also subtracts the startup-only child from the whole child,
// so that reading includes the work of preparing the input.
func readCountedSample(t *testing.T, row string, leaf countedShape, size int, none *countedStatementRun) countedSample {
	t.Helper()
	what := fmt.Sprintf("%s at %d", leaf.name, size)
	env := func(mode string) []string {
		return []string{countedModeEnv + "=" + mode, countedRowEnv + "=" + row, countedShapeEnv + "=" + leaf.shape,
			countedOpEnv + "=" + leaf.operation, countedSizeEnv + "=" + strconv.Itoa(size)}
	}
	preparation, prepared, err := runProfiledCountedChild(t, "the prepare-only run of "+what, env("prepare")...)
	if err != nil {
		t.Fatal(err)
	}
	measured, whole, err := runProfiledCountedChild(t, "the whole run of "+what, env("whole")...)
	if err != nil {
		t.Fatal(err)
	}
	if preparation.Size != size || measured.Size != size {
		t.Fatalf("%s: the prepare-only and whole children built sizes %d and %d, want both to build %d", what, preparation.Size, measured.Size, size)
	}
	if preparation.Characters != measured.Characters {
		t.Fatalf("%s: the prepare-only and whole children built %d and %d characters", what, preparation.Characters, measured.Characters)
	}
	op, err := countedWorkBetween(prepared, whole)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	operation, withPreparation, err := countedStatementDifferences(leaf.name, size, none,
		countedStatementRun{units: preparation.Units, statements: prepared.total},
		countedStatementRun{units: measured.Units, statements: whole.total})
	if err != nil {
		t.Fatal(err)
	}
	if op.total != operation {
		t.Fatalf("%s: the blocks count %d operation-only statements, but the profile totals differ by %d", what, op.total, operation)
	}
	var invokingBytes uint64
	if none != nil {
		plain, err := runCountedChild(t, os.Args[0], "the invoking build's run of "+what, "", env("plain")...)
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
		updateItems: measured.Stages.Update, busiest: op.busiest, busiestBlock: op.busiestBlock, outcome: measured.Outcome,
	}
}

// TestHeavyBodiesCountedHelper runs the requested child mode. Until the old rows move to the new
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
	if mode == "none" {
		printCountedMeasurement(t, countedMeasurement{})
		return
	}
	if mode != "prepare" && mode != "whole" && mode != "plain" {
		t.Fatalf("%s = %q, want prepare, whole, plain, none, or census", countedModeEnv, mode)
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
		units, characters, run := leaf.prepare(t, size)
		measured := countedMeasurement{Size: size, Units: units, Characters: characters}
		if mode != "prepare" {
			measured = measureCountedOperation(measured, run)
		}
		printCountedMeasurement(t, measured)
		return
	}
	t.Fatalf("%s has no counted shape %q with operation %q", row, shape, operation)
}

// measureCountedOperation measures allocation and stage counters only around the operation.
func measureCountedOperation(measured countedMeasurement, run func() string) countedMeasurement {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := readCountedStages()
	measured.Outcome = run()
	end := readCountedStages()
	runtime.ReadMemStats(&after)
	measured.Bytes = after.TotalAlloc - before.TotalAlloc
	measured.Stages = countedStages{Parse: end.Parse - start.Parse, Render: end.Render - start.Render,
		Update: end.Update - start.Update, Scan: end.Scan - start.Scan}
	return measured
}

// printCountedMeasurement reports the result before testing writes the coverage profile.
func printCountedMeasurement(t *testing.T, result countedMeasurement) {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("%s%s\n", countedMark, encoded)
}

// countedProfile keeps the weighted statement total and the blocks in the producer's order.
type countedProfile struct {
	total  uint64
	blocks []countedProfileBlock
}

// countedProfileBlock identifies a block and records its statement and execution counts.
type countedProfileBlock struct {
	position, module  string
	statements, count uint64
}

// countedProfileDifference keeps the work after subtracting preparation, in all and by module.
// It also keeps the block whose operation count is nearest to wrapping.
type countedProfileDifference struct {
	total, busiest uint64
	byModule       map[string]uint64
	busiestBlock   string
}

// countedWorkBetween compares profiles from the same binary. The testing package emits every
// block in source order, including blocks that never ran, so the positions must match.
func countedWorkBetween(prepared, whole countedProfile) (countedProfileDifference, error) {
	if len(prepared.blocks) != len(whole.blocks) {
		return countedProfileDifference{}, fmt.Errorf("the prepare-only and whole profiles have %d and %d blocks", len(prepared.blocks), len(whole.blocks))
	}
	op := countedProfileDifference{byModule: make(map[string]uint64)}
	for i, after := range whole.blocks {
		before := prepared.blocks[i]
		if before.position != after.position || before.statements != after.statements {
			return countedProfileDifference{}, fmt.Errorf("the prepare-only profile has %s (%d statements), but the whole profile has %s (%d statements) at block %d",
				before.position, before.statements, after.position, after.statements, i+1)
		}
		if after.count < before.count {
			return countedProfileDifference{}, fmt.Errorf("the block %s has whole count %d below its prepare-only count %d", after.position, after.count, before.count)
		}
		count := after.count - before.count
		// Neither the product nor the sum can exceed the checked total of the whole profile.
		work := after.statements * count
		op.total += work
		op.byModule[after.module] += work
		if count > op.busiest {
			op.busiest, op.busiestBlock = count, after.position
		}
	}
	return op, nil
}

// countedPosition matches the start and end positions after a file name in a coverage profile.
var countedPosition = regexp.MustCompile(`^[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$`)

// parseCountedProfile reads a count-mode coverage profile and sums statements times executions.
// It refuses unknown packages and integers outside the range both old readers accept.
func parseCountedProfile(r io.Reader, modules map[string]string) (countedProfile, error) {
	lines := bufio.NewScanner(r)
	if !lines.Scan() && lines.Err() != nil {
		return countedProfile{}, lines.Err()
	}
	if lines.Text() != "mode: count" {
		return countedProfile{}, fmt.Errorf("coverage mode: got %q, want mode: count", lines.Text())
	}
	var result countedProfile
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
		result.blocks = append(result.blocks, countedProfileBlock{position: block, module: module, statements: statements, count: count})
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

// TestTheCountedProfileReaderRefusesWhatItCannotCount uses the whole run of the 2048-character
// bracket child. From the package directory, with the counted binary at "$d/pmdoc.test":
//
//	PMDOC_COUNTED_MODE=whole PMDOC_COUNTED_ROW=TestCountedBracketParagraph \
//	  PMDOC_COUNTED_SHAPE=brackets PMDOC_COUNTED_OP=read PMDOC_COUNTED_SIZE=2048 \
//	  "$d/pmdoc.test" -test.run='^TestHeavyBodiesCountedHelper$' -test.v \
//	  -test.count=1 -test.timeout=0 -test.coverprofile="$d/whole.txt"
//	grep -E '^(mode:|github\.com/sjawhar/envoy/internal/fold/)' "$d/whole.txt" > testdata/counted-profile.txt
//
// Testing writes all covered packages to the profile, so the filter keeps just the fold package.
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
	if got.total != 3077 {
		t.Fatalf("profile counted %d statements, want 3077", got.total)
	}

	mode, blocks, _ := strings.Cut(profile, "\n")
	line, _, _ := strings.Cut(blocks, "\n")
	fields := strings.Fields(line)
	if len(fields) != 3 {
		t.Fatalf("first fixture block has %d fields, want 3: %q", len(fields), line)
	}
	block, statements, count := fields[0], fields[1], fields[2]
	t.Run("operation-only counts", func(t *testing.T) {
		const prior = pkg + "/fold.go:25.5,25.35 1 "
		const busiest = pkg + "/fold.go:26.6,27.1"
		preparation := strings.NewReplacer(
			prior+"2047", prior+"2046",
			busiest+" 1 1024", busiest+" 1 0",
		).Replace(profile)
		prepared, err := parseCountedProfile(strings.NewReader(preparation), modules)
		if err != nil {
			t.Fatal(err)
		}
		op, err := countedWorkBetween(prepared, got)
		if err != nil {
			t.Fatal(err)
		}
		if op.total != 1025 || op.byModule[module] != 1025 || op.busiest != 1024 || op.busiestBlock != busiest {
			t.Fatalf("operation-only work = %+v, want 1025 statements, all in %s, and 1024 runs of %s", op, module, busiest)
		}
	})
	t.Run("a block count shrank", func(t *testing.T) {
		const shrank = pkg + "/fold.go:32.2,32.10"
		preparation := strings.Replace(profile, line, block+" "+statements+" 0", 1)
		preparation = strings.Replace(preparation, shrank+" 1 0", shrank+" 1 1", 1)
		prepared, err := parseCountedProfile(strings.NewReader(preparation), modules)
		if err != nil {
			t.Fatal(err)
		}
		_, err = countedWorkBetween(prepared, got)
		if want := "the block " + shrank + " has whole count 0 below its prepare-only count 1"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %q", err, want)
		}
	})

	for _, tc := range []struct {
		name, from, to, want string
	}{
		{"set mode", mode, "mode: set", "want mode: count"},
		{"atomic mode", mode, "mode: atomic", "want mode: count"},
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
