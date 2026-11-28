package specform

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
)

// The work table (work_test.go) counts the work of a judgment with Go's coverage counters, as
// pmdoc's heavy-bodies rows count a parse's (pmdoc/heavy_bodies_test.go): countedBinary builds
// this package's tests once for the whole run with a counter in each basic block of every package
// this one imports outside the standard library, and in this one (countedPackages), and a row runs
// its judgment in that binary as a child process (TestCountedWorkOfOneJudgment), selected by the
// environment countedRun sets. The child runs twice at each size: once to build the judgment's
// input alone, and once to build it and judge it. The work of the judgment is the statements of
// each block times the times it ran, summed over the coverage profile the second run writes, less
// the same sum over the first's, so the start of the binary and the building of the input are
// never the judgment's work. A run that judged answers what it found (findingsAnswer), and the row
// fails unless that is what this build's own judgment of the same text found: a counter that
// changed what a package reads would count another path.

// The environment TestCountedWorkOfOneJudgment reads: the row of the work table, the n of its text,
// and whether to judge the text ("1") or only build it ("0").
const (
	countedRowEnv   = "SPECFORM_COUNTED_ROW"
	countedSizeEnv  = "SPECFORM_COUNTED_SIZE"
	countedJudgeEnv = "SPECFORM_COUNTED_JUDGE"
)

// countedMark begins the line a counted run writes once it has run: the row, the n, whether it
// judged, and what its judgment answered (findingsAnswer), or "built" for a run that judged
// nothing. A run that writes no such line ran nothing, and the row is refused rather than read as
// a run that executed nothing.
const countedMark = "specform counted run: "

// countedWrap is the count at which a block's counter wraps: the coverage counters are 32 bits wide
// (cmd/cover), and no build widens them, so a block run countedWrap times or more reads as the
// remainder.
const countedWrap = 1 << 32

// countedJudgment is the count of one judgment at one n: the statements it ran in countedPackages,
// less those of the run that only built its input, and the block that ran most often in the run
// that judged, with its count.
type countedJudgment struct {
	statements   uint64
	busiest      uint64
	busiestBlock string
}

// countedBuild is the counted test binary, built once for the whole run (countedBinary) and removed
// by TestMain once every test has run.
var countedBuild struct {
	once        sync.Once
	dir, binary string
	err         error
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

// countedBinary is the path of this package's test binary built with coverage counters in
// countedPackages. The first test to ask builds it; a build that failed fails every test that
// asks.
func countedBinary(t *testing.T) string {
	t.Helper()
	countedBuild.once.Do(func() { countedBuild.binary, countedBuild.err = buildCounted(t) })
	if countedBuild.err != nil {
		t.Fatal(countedBuild.err)
	}
	return countedBuild.binary
}

// buildCounted builds the counted test binary into a directory of its own: `go test -c` with
// -covermode=atomic, the one mode a GOFLAGS of -race, which reaches this build, allows. The build
// reads the environment and none of the flags this build was given (-race, -tags), and it reads
// the packages it counts from their files on disk, through no -overlay (pmdoc's
// heavy_bodies_test.go found that a coverage build applies none to the packages it puts counters
// in), so a change of the code under test, such as a planted walk, is made in the tree.
func buildCounted(t *testing.T) (string, error) {
	packages, err := countedPackages(t)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "specform-counted-")
	if err != nil {
		return "", err
	}
	countedBuild.dir = dir
	binary := filepath.Join(dir, "specform.test")
	if _, err := countedCommand(t, "the build of the counted test binary", nil, "go", "test", "-c", "-o", binary,
		"-covermode=atomic", "-coverpkg="+strings.Join(packages, ","), "."); err != nil {
		return "", err
	}
	return binary, nil
}

// countedPackages are the packages whose statements the work table counts: this one and every
// package outside the standard library that it imports, directly or through another (go list
// -deps): ste, the reader (pmdoc), goldmark with its extensions, the server's reference grammar
// (text), golang.org/x/text and the rest.
func countedPackages(t *testing.T) ([]string, error) {
	listed, err := countedCommand(t, "go list of the packages this one imports", nil, "go", "list", "-deps",
		"-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".")
	if err != nil {
		return nil, err
	}
	var packages []string
	for _, line := range strings.Split(listed, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			packages = append(packages, line)
		}
	}
	if !slices.Contains(packages, "github.com/sjawhar/envoy/internal/dispatch/specform/ste") {
		return nil, fmt.Errorf("go list -deps named %q, without ste, whose walks the work table counts", packages)
	}
	return packages, nil
}

// countedCommand runs name with args, and env added to the test's own, and returns what it wrote.
// A run that does not end before the shared wait's end (testwait.Context), or that ends with an
// error, returns an error carrying what it wrote. At the shared wait's end the context kills the
// run, and CombinedOutput returns once its output closes: the go command's own steps write to
// pipes of the go command, and the counted build starts no process, so no other process holds that
// output open, and nothing bounds the wait for it but the run's end.
func countedCommand(t *testing.T, what string, env []string, name string, args ...string) (string, error) {
	t.Helper()
	ctx := testwait.Context(t)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("testwait: %s: gave up waiting for %s at the shared wait's margin before the test binary's deadline; its output:\n%s",
			t.Name(), what, out)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %v; its output:\n%s", what, err, out)
	}
	return string(out), nil
}

// countedWork counts row's judgment at n: the run that builds its input and judges it, less the run
// that only builds it. want is what this build's own judgment of that text answered
// (findingsAnswer), which the counted build's must be.
func countedWork(t *testing.T, row string, n int, want string) countedJudgment {
	t.Helper()
	built := countedRun(t, row, n, false, "built")
	judged := countedRun(t, row, n, true, want)
	if judged.statements < built.statements {
		t.Fatalf("the counted run that judged %d units executed %d statements, fewer than the %d of the run that only built them",
			n, judged.statements, built.statements)
	}
	judged.statements -= built.statements
	return judged
}

// countedRun runs row's judgment at n in the counted test binary, judging its text when judge is
// set and only building it otherwise, and reads the statements the run counted and its busiest
// block from the coverage profile the run writes. It fails t unless the run wrote countedMark with
// want, what its judgment answered.
func countedRun(t *testing.T, row string, n int, judge bool, want string) countedJudgment {
	t.Helper()
	binary := countedBinary(t)
	profile := filepath.Join(t.TempDir(), "profile.out")
	what := fmt.Sprintf("the counted run of %d units", n)
	judging := "0"
	if judge {
		judging = "1"
	} else {
		what += ", building them alone"
	}
	out, err := countedCommand(t, what, []string{countedRowEnv + "=" + row, countedSizeEnv + "=" + strconv.Itoa(n),
		countedJudgeEnv + "=" + judging}, binary, "-test.run=^TestCountedWorkOfOneJudgment$", "-test.count=1",
		"-test.coverprofile="+profile)
	if err != nil {
		t.Fatal(err)
	}
	if line := countedMark + countedLine(row, n, judging, want); !strings.Contains(out, line) {
		t.Fatalf("%s did not write %q: it ran nothing, or a counter changed what it reads, so its count is of another path:\n%s",
			what, line, out)
	}
	var run countedJudgment
	if run.statements, run.busiest, run.busiestBlock, err = profileCounts(profile); err != nil {
		t.Fatal(err)
	}
	return run
}

// countedLine is the line TestCountedWorkOfOneJudgment writes after countedMark.
func countedLine(row string, n int, judging, answer string) string {
	return fmt.Sprintf("%s|%d|%s: %s", row, n, judging, answer)
}

// findingsAnswer is what a judgment answered, as a counted run writes it and as this build compares
// it: how many findings, and a digest of every field of each, in their order.
func findingsAnswer(found []Finding) string {
	digest := fnv.New64a()
	for _, finding := range found {
		fmt.Fprintf(digest, "%q %q %q %q %q %q\n", finding.Node, finding.Kind, finding.Where, finding.Rule, finding.Detail, finding.Excerpt)
	}
	return fmt.Sprintf("%d findings, digest %016x", len(found), digest.Sum64())
}

// profileCounts is what a coverage profile counts: each block's statements times the times it ran,
// summed, and the block that ran most often, with its count.
func profileCounts(path string) (statements, busiest uint64, busiestBlock string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, "", err
	}
	defer file.Close()
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		line := lines.Text()
		if strings.HasPrefix(line, "mode:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return 0, 0, "", fmt.Errorf("%s: a line of a block, its statements and its count wanted, got %q", path, line)
		}
		stmts, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, 0, "", fmt.Errorf("%s: %q: %w", path, line, err)
		}
		count, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return 0, 0, "", fmt.Errorf("%s: %q: %w", path, line, err)
		}
		statements += stmts * count
		if count > busiest {
			busiest, busiestBlock = count, fields[0]
		}
	}
	return statements, busiest, busiestBlock, lines.Err()
}

// TestCountedWorkOfOneJudgment is the run of the counted build that the work table counts: it
// builds the text of the row countedRowEnv names at the n countedSizeEnv names, judges it when
// countedJudgeEnv is "1", and writes countedMark with what it answered. With no row named it
// skips.
func TestCountedWorkOfOneJudgment(t *testing.T) {
	name := os.Getenv(countedRowEnv)
	if name == "" {
		t.Skipf("%s is unset: the work table runs this test in its counted test binary", countedRowEnv)
	}
	rows := workTable()
	index := slices.IndexFunc(rows, func(row workRow) bool { return row.name == name })
	if index < 0 {
		t.Fatalf("%s = %q names no row of the work table", countedRowEnv, name)
	}
	n, err := strconv.Atoi(os.Getenv(countedSizeEnv))
	if err != nil || n < 1 {
		t.Fatalf("%s = %q, want an n", countedSizeEnv, os.Getenv(countedSizeEnv))
	}
	judging := os.Getenv(countedJudgeEnv)
	judgment := rows[index].judge(t, n)
	answer := "built"
	if judging == "1" {
		answer = findingsAnswer(judgment())
	}
	fmt.Println(countedMark + countedLine(name, n, judging, answer))
}
