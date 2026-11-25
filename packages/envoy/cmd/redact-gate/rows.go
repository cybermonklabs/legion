package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// baseline runs the row's test unmutated in a scratch copy of the snapshot (stage 4); it must
// pass. A test shared by several rows runs once. The longest correct run is kept. A run past its
// bound, or one that skipped the row's test or a subtest of it, is UNKNOWN, not a failure.
func (g *gate) baseline(r *row, baselines map[string]error) {
	key := r.Package + r.File + "\x00" + r.Test
	if _, done := baselines[key]; !done {
		copyDir, err := g.copy(r)
		if err != nil {
			baselines[key] = fmt.Errorf("the copy: %v", err)
		} else {
			res := g.runTest(copyDir, r)
			baselines[key] = res.baselineErr()
			if baselines[key] == nil && res.elapsed > g.longest {
				g.longest, g.longestRow = res.elapsed, r.ID
			}
		}
	}
	if err := baselines[key]; err != nil {
		var late lateRun
		var skipped skippedRun
		if errors.As(err, &late) || errors.As(err, &skipped) {
			g.unknown("stage 4 (baseline) row %s: the unmutated test %s %v", r.ID, r.Test, err)
		} else {
			g.fail("stage 4 (baseline) row %s: the unmutated test %s %v", r.ID, r.Test, err)
		}
	}
}

// gateRow copies the snapshot, applies the row's one mutation, checks the diff, runs the test and
// judges the evidence (stage 5). A run past its bound, or one that skipped the row's test or a
// subtest of it, leaves the mutant unproven: UNKNOWN, neither caught nor survived.
func (g *gate) gateRow(r *row, at [2]int) {
	copyDir, err := g.copy(r)
	if err != nil {
		g.fail("stage 5 (mutation) row %s: the copy: %v", r.ID, err)
		return
	}
	target := filepath.Join(copyDir, r.Mutation.File)
	before, err := os.ReadFile(target)
	if err != nil {
		g.fail("stage 5 (mutation) row %s: %v", r.ID, err)
		return
	}
	after := append(append(append([]byte{}, before[:at[0]]...), r.Mutation.Replacement...), before[at[1]:]...)
	if err := os.Remove(target); err != nil { // the copy may share the snapshot's blocks
		g.fail("stage 5 (mutation) row %s: %v", r.ID, err)
		return
	}
	if err := os.WriteFile(target, after, 0o644); err != nil {
		g.fail("stage 5 (mutation) row %s: %v", r.ID, err)
		return
	}
	if err := g.onlyChange(r, before, after); err != nil {
		g.fail("stage 5 (mutation) row %s: %v", r.ID, err)
		return
	}
	res := g.runTest(copyDir, r)
	if res.timedOut {
		g.unknown("stage 5 (mutation) row %s (%q in %s): the mutant's test %v", r.ID, r.Mutation.Preimage, r.Function, res.late())
		return
	}
	verdict := res.evidence(r)
	if len(res.skipped) > 0 && !res.compileError {
		g.unknown("stage 5 (mutation) row %s (%q in %s): %s", r.ID, r.Mutation.Preimage, r.Function, verdict)
		return
	}
	if verdict != "" {
		g.fail("stage 5 (mutation) row %s (%q in %s): %s", r.ID, r.Mutation.Preimage, r.Function, verdict)
		return
	}
	fmt.Fprintf(g.out, "PASS stage 5 (mutation) row %s: the mutant fails %s at REDACT-ASSERT %s, its captured output naming a marker (%s, under its bound of %s)\n", r.ID, r.Test, r.Capture, res.elapsed.Round(time.Second), res.bound)
}

// onlyChange checks that the copy's mutated file differs from the snapshot's by the one
// replacement. No other file of the copy differs: copy makes it from the snapshot afresh for each
// row, and gateRow writes no other file.
func (g *gate) onlyChange(r *row, before, after []byte) error {
	pristine, err := os.ReadFile(filepath.Join(g.snapshot, r.Mutation.File))
	if err != nil || !bytes.Equal(pristine, before) {
		return errors.New("the copy's file is not the snapshot's before the mutation")
	}
	prefix := commonPrefix(before, after)
	suffix := commonSuffix(before[prefix:], after[prefix:])
	removed, added := string(before[prefix:len(before)-suffix]), string(after[prefix:len(after)-suffix])
	if !strings.Contains(r.Mutation.Preimage, removed) || !strings.Contains(r.Mutation.Replacement, added) {
		return fmt.Errorf("the diff removes %q and adds %q, not the one replacement", removed, added)
	}
	return nil
}

func commonPrefix(a, b []byte) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}

func commonSuffix(a, b []byte) int {
	i := 0
	for i < len(a) && i < len(b) && a[len(a)-1-i] == b[len(b)-1-i] {
		i++
	}
	return i
}

// copy makes the row's scratch copy of the snapshot at one path, so the build cache serves every
// copy: packages/envoy for a Go row of the envoy module; for a Go row of another module go.work
// uses (packages/daemon), that module and go.work beside packages/envoy, since it builds against
// the envoy module through the workspace; and the whole tree (node_modules included) for a Bun
// row.
func (g *gate) copy(r *row) (string, error) {
	copyDir := filepath.Join(g.work, "copy")
	if err := os.RemoveAll(copyDir); err != nil {
		return "", err
	}
	paths := []string{"."}
	if !r.bun {
		mod, err := g.goRowModule(r)
		if err != nil {
			return "", err
		}
		paths = []string{"packages/envoy"}
		if mod != "packages/envoy" {
			paths = append(paths, mod, "go.work")
			if fileExists(filepath.Join(g.snapshot, "go.work.sum")) {
				paths = append(paths, "go.work.sum")
			}
		}
	}
	for _, path := range paths {
		src, dst := filepath.Join(g.snapshot, path), filepath.Join(copyDir, path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		cmd := exec.Command("cp", "-a", "--reflink=auto", src, dst)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("cp: %v: %s", err, out)
		}
	}
	return copyDir, nil
}

// goRowModule is the directory, relative to the snapshot, of the module a Go row's package is in:
// packages/envoy for a package named relative to it (./...), and otherwise the module go.work uses
// whose path the package's import path is, or is under.
func (g *gate) goRowModule(r *row) (string, error) {
	if strings.HasPrefix(r.Package, "./") || r.Package == "." {
		return "packages/envoy", nil
	}
	roots, err := workRoots(g.snapshot)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		data, err := os.ReadFile(filepath.Join(g.snapshot, root, "go.mod"))
		if err != nil {
			continue
		}
		if path := modfileModule(data); path != "" && (r.Package == path || strings.HasPrefix(r.Package, path+"/")) {
			return root, nil
		}
	}
	return "", fmt.Errorf("row %s: package %s is in no module go.work uses", r.ID, r.Package)
}

type result struct {
	ran, failed, timedOut, compileError bool
	asserted                            []string // REDACT-ASSERT <capture> lines
	captures                            map[string][]string
	blind                               []string // the print test's blind rows ("blind: ...")
	skipped                             []string // the row's test and subtests that were skipped, each with its skip's line
	elapsed                             time.Duration
	startup, bound                      time.Duration // the start-up run's time, and the run's bound
	stall                               string        // what a run past its bound printed of it
	note                                string
}

// lateRun is a run that went past its bound: UNKNOWN, neither a pass nor a failure.
type lateRun struct {
	bound, startup time.Duration
	stall          string
}

func (l lateRun) Error() string {
	how := ""
	if l.startup > 0 {
		how = fmt.Sprintf(" (row_timeout and four times its start-up run's %s)", l.startup)
	}
	return fmt.Sprintf("went past its bound of %s%s, so it neither passed nor failed: run it again\n%s", l.bound, how, l.stall)
}

func (res result) late() lateRun {
	return lateRun{bound: res.bound, startup: res.startup, stall: res.stall}
}

// skippedRun is a run that skipped the row's test or a subtest of it (go test's skip action, a
// Bun testcase's <skipped>): UNKNOWN, neither a pass nor a failure, since what was skipped
// checked nothing. A row's test that needs a test database skips without its URL.
type skippedRun struct{ tests []string }

func (s skippedRun) Error() string {
	return fmt.Sprintf("skipped %s, so it neither passed nor failed: run it again with what the skip asks for", strings.Join(s.tests, "; "))
}

var (
	captureLine = regexp.MustCompile(`REDACT-CAPTURE (\S+) ([A-Za-z0-9+/=]+)`)
	assertLine  = regexp.MustCompile(`REDACT-ASSERT ([^\s:]+)`)
	blindLine   = regexp.MustCompile(`blind: [^\n]*`)
	loggedLine  = regexp.MustCompile(`^\s+\S+\.go:\d+: `) // a line t.Log, t.Error or t.Skip printed
	testLogLine = regexp.MustCompile(`_test\.go:\d+: `)
)

// marked reports whether the output captured under k holds a marker.
func (res result) marked(k string) bool {
	for _, captured := range res.captures[k] {
		for _, m := range markers {
			if strings.Contains(captured, m) {
				return true
			}
		}
	}
	return false
}

// baselineErr is why the unmutated test did not pass, or nil. A test that failed names the first
// designated assertion it failed at with a marker in the output captured there, and how many of
// the print test's rows were blind, so a control reads which check caught it. A run that skipped
// the row's test or a subtest of it is UNKNOWN (skippedRun) even where another subtest failed:
// the row's test did not run whole, so the run is not the row's.
func (res result) baselineErr() error {
	switch {
	case res.compileError:
		return fmt.Errorf("does not compile: %s", res.note)
	case res.timedOut:
		return res.late()
	case !res.ran:
		return errors.New("is missing: it did not run")
	case len(res.skipped) > 0:
		return skippedRun{tests: res.skipped}
	case res.failed:
		why := res.note
		for _, k := range res.asserted {
			if res.marked(k) {
				why = fmt.Sprintf("at REDACT-ASSERT %s, with a marker in the output captured there (%d designated assertions failed)", k, len(res.asserted))
				break
			}
		}
		if len(res.blind) > 0 {
			why += fmt.Sprintf("; %d rows blind, the first %s", len(res.blind), res.blind[0])
		}
		return fmt.Errorf("fails: %s", why)
	}
	return nil
}

// evidence is "" when the mutant failed at the row's designated assertion - a REDACT-ASSERT line
// under the row's capture, or under capture/<form> - with a marker in the output captured under
// that same name, and the reason otherwise: a wrong assertion where the test failed elsewhere
// while its captured output holds a marker, no output evidence where it failed at the designated
// assertion but the output it captured there holds none. A run that skipped the row's test or a
// subtest of it leaves the mutant unproven, whatever else it shows: the row's test did not run
// whole.
func (res result) evidence(r *row) string {
	switch {
	case res.compileError:
		return "the mutant does not compile: " + res.note
	case res.timedOut:
		return "the mutant's test " + res.late().Error()
	case !res.ran:
		return "the test is missing"
	case len(res.skipped) > 0:
		return "the mutant is unproven, neither caught nor survived: its test " + skippedRun{tests: res.skipped}.Error()
	case !res.failed:
		return "a surviving mutant: the test passed with the mutation applied"
	}
	mine := func(k string) bool { return k == r.Capture || strings.HasPrefix(k, r.Capture+"/") }
	marked := res.marked
	asserted := false
	for _, k := range res.asserted {
		if mine(k) {
			asserted = true
			if marked(k) {
				return ""
			}
		}
	}
	if asserted {
		return fmt.Sprintf("no output evidence: the test failed at REDACT-ASSERT %s, but the output it captured there holds no marker", r.Capture)
	}
	for k := range res.captures {
		if mine(k) && marked(k) {
			return fmt.Sprintf("the wrong assertion: the test failed, but not at REDACT-ASSERT %s, while its captured output holds a marker (%s)", r.Capture, res.note)
		}
	}
	seen := map[string]bool{}
	var others []string
	for _, k := range res.asserted {
		if !seen[k] && len(others) < 3 {
			seen[k] = true
			others = append(others, k)
		}
	}
	return fmt.Sprintf("no evidence: the test failed, but not at REDACT-ASSERT %s (it failed at %s), and no output captured under that name holds a marker (%s)", r.Capture, orNoAssert(others), res.note)
}

func orNoAssert(names []string) string {
	if len(names) == 0 {
		return "no designated assertion"
	}
	return "REDACT-ASSERT " + strings.Join(names, ", ")
}

func (res *result) scan(text string) {
	for _, m := range captureLine.FindAllStringSubmatch(text, -1) {
		if decoded, err := base64.StdEncoding.DecodeString(m[2]); err == nil {
			res.captures[m[1]] = append(res.captures[m[1]], string(decoded))
		}
	}
	for _, m := range assertLine.FindAllStringSubmatch(text, -1) {
		res.asserted = append(res.asserted, m[1])
	}
	res.blind = append(res.blind, blindLine.FindAllString(text, -1)...)
}

// runTest runs the row's test in the copy, under a bound derived in the same run. Each test's
// output is read whole once the run ends, joined in the order its events came: test2json splits a
// line longer than its buffer across several output events, and a REDACT-CAPTURE line of a long
// page is one, so a capture read event by event would be cut short.
//
// A Go row's bound is runBound's for the one top-level test, after the start-up run of the row's
// package in the copy (startUp); a Bun row's is row_timeout alone (runBun). A start-up run that
// fails is not read: the run after it fails the same way, and that run is read.
func (g *gate) runTest(copyDir string, r *row) result {
	res := result{captures: map[string][]string{}}
	start := time.Now()
	if r.bun {
		return g.runBun(copyDir, r, res, start)
	}
	mod, err := g.goRowModule(r)
	if err != nil {
		res.compileError, res.note = true, err.Error()
		return res
	}
	res.startup, _ = startUp(filepath.Join(copyDir, mod), r.Package, nil, nil)
	res.bound = runBound(g.rowTimeout, 1, res.startup)
	start = time.Now()
	cmd := exec.Command("go", "test", "-json", "-count=1", "-v", "-timeout", res.bound.String(), "-run", "^"+regexp.QuoteMeta(r.Test)+"$", r.Package)
	cmd.Dir = filepath.Join(copyDir, mod)
	out, _ := cmd.CombinedOutput()
	res.elapsed = time.Since(start)
	output := map[string]*strings.Builder{}
	var printed strings.Builder // all the test binary printed, in order, for a run past its bound
	var built strings.Builder   // the go command's build output, which -json gives as build-output events
	var tests []string
	said := map[string]string{} // each test's last logged line that is no REDACT- line: a skip's reason
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "{") {
			if strings.Contains(line, "build failed") || strings.Contains(line, "[setup failed]") || compilerLine.MatchString(line) {
				res.compileError, res.note = true, strings.TrimSpace(line)
			}
			continue
		}
		var e struct{ Action, Test, Output string }
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Action == "output" {
			printed.WriteString(e.Output)
		}
		if e.Action == "build-output" {
			built.WriteString(e.Output)
		}
		if e.Action == "build-fail" || strings.Contains(e.Output, "[build failed]") {
			res.compileError = true
		}
		if e.Test != r.Test && !strings.HasPrefix(e.Test, r.Test+"/") {
			continue
		}
		switch e.Action {
		case "run":
			res.ran = true
		case "fail":
			res.failed = true
		case "output":
			if output[e.Test] == nil {
				output[e.Test] = &strings.Builder{}
				tests = append(tests, e.Test)
			}
			output[e.Test].WriteString(e.Output)
			if testLogLine.MatchString(e.Output) && !strings.Contains(e.Output, "REDACT-") && res.note == "" {
				res.note = strings.TrimSpace(e.Output)
			}
			if loggedLine.MatchString(e.Output) && !strings.Contains(e.Output, "REDACT-") {
				said[e.Test] = strings.TrimSpace(e.Output)
			}
		case "skip":
			skipped := e.Test
			if why := said[e.Test]; why != "" {
				skipped += " (" + why + ")"
			}
			res.skipped = append(res.skipped, skipped)
		}
	}
	for _, test := range tests {
		res.scan(output[test].String())
	}
	if res.compileError && res.note == "" {
		res.note = compilerError(built.String())
	}
	text := printed.String()
	if res.timedOut = ranPast(text); res.timedOut {
		res.stall = stall(text)
	}
	return res
}

// startUp is the start-up run of a package's tests in dir: `go test -json -count=1 -run '^$'`,
// which builds and links the package's test binary and runs it with no test (its TestMain's start
// and end) under the box's load of that moment, writing its events to stdout and the go command's
// stderr to stderr (nil discards either). It answers the run's time, in whole seconds rounded up,
// and the go command's error. Its own bound is go test's default, ten minutes.
func startUp(dir, pkg string, stdout, stderr io.Writer) (time.Duration, error) {
	start := time.Now()
	cmd := exec.Command("go", "test", "-json", "-count=1", "-run", "^$", pkg)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, stdout, stderr
	err := cmd.Run()
	return ceilSecond(time.Since(start)), err
}

// runBound is the bound of a go test run of tops top-level tests whose package's start-up run
// took startup: row_timeout, the time the test of one row may take, once for each test, plus four
// times the start-up run, so the tests' budget is its floor. go test's -timeout counts from the
// first test, but the go command kills the binary one minute past it counted from the binary's
// start, so the bound holds the binary's start and end as well as its tests: twice the start-up
// run holds that start and end (the start-up run made them once, with no test; a run's end also
// drops what its tests made, each database they cloned among it), and twice that again holds the
// load moving between the two runs. Each bound is derived in its own run, not set ahead, because
// the box's load moves from one run to the next: on 2026-11-22 the one row of
// daemon/internal/workflow took 46s at 14:06Z and 1.5s at 15:31Z. Stages 4 and 5 bound each Go
// row's run by it, and -verify each Go package's (verify.go).
func runBound(rowTimeout time.Duration, tops int, startup time.Duration) time.Duration {
	return time.Duration(tops)*rowTimeout + 4*startup
}

func ceilSecond(d time.Duration) time.Duration {
	if t := d.Truncate(time.Second); t < d {
		return t + time.Second
	}
	return d
}

// compilerLine is a line of the compiler's: a file, a line and a column, then the error.
var compilerLine = regexp.MustCompile(`\.go:\d+:\d+: `)

// compilerError is what a failed build printed, for the reason a row's run did not compile: its
// compiler lines (at most five), else its last three lines, else a phrase saying it printed none.
func compilerError(built string) string {
	var errs []string
	for _, line := range strings.Split(built, "\n") {
		if compilerLine.MatchString(line) && len(errs) < 5 {
			errs = append(errs, strings.TrimSpace(line))
		}
	}
	if len(errs) > 0 {
		return strings.Join(errs, "; ")
	}
	if text := strings.TrimSpace(built); text != "" {
		return lastLines(text, 3)
	}
	return "the go command printed no build output"
}

var (
	goroutineHead = regexp.MustCompile(`^goroutine \d+ `)
	// testFrame is a frame of a test function in a goroutine dump: pkg.TestX( or pkg.TestX.func1(.
	testFrame = regexp.MustCompile(`\.(Test(?:[A-Z0-9_]\w*)?)[.(]`)
)

// A run past its bound prints one of these at the start of a line: the test binary's own panic at
// its -timeout, or the go command's kill a minute after it (it kills the binary with SIGQUIT, and
// the binary prints every goroutine). go test -json carries the binary's stderr among its output
// events.
const (
	timeoutPanic = "panic: test timed out after "
	quitKill     = "*** Test killed with quit: ran too long"
)

// ranPast reports whether a run's output says it went past its bound: a line that starts with
// timeoutPanic or quitKill, where the testing package and the go command print them. A line that
// holds one past its start - a test's t.Log line quoting it, indented and led by the test's file -
// says nothing of the run. A panic printed after a test's own output that did not end its line is
// missed, so that run reads as failed, not UNKNOWN: either is no pass. stall finds the lines the
// same way, so a run read as past its bound always shows them.
func ranPast(text string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, timeoutPanic) || strings.HasPrefix(line, quitKill) {
			return true
		}
	}
	return false
}

// stall is what a run past its bound printed of it: the timeout's panic line with the tests it was
// running, the go command's kill line, and the goroutines of the dump that run a test - each with
// a frame of a test function other than TestMain - or, where none does, those in TestMain (a start
// or an end past the bound). -verify prints the same for a package (verify.go).
func stall(text string) string {
	lines := strings.Split(text, "\n")
	var shown []string
	var blocks [][]string
	var block []string
	panicked := false
	for i, line := range lines {
		switch {
		case !panicked && strings.HasPrefix(line, timeoutPanic):
			panicked = true
			shown = append(shown, line)
			for _, next := range lines[i+1:] {
				if strings.TrimSpace(next) == "" {
					break
				}
				shown = append(shown, next)
			}
		case strings.HasPrefix(line, quitKill):
			shown = append(shown, line)
		}
		switch {
		case goroutineHead.MatchString(line):
			if block != nil {
				blocks = append(blocks, block)
			}
			block = []string{line}
		case block != nil && strings.TrimSpace(line) == "":
			blocks = append(blocks, block)
			block = nil
		case block != nil:
			block = append(block, line)
		}
	}
	if block != nil {
		blocks = append(blocks, block)
	}
	runs := func(b []string, main bool) bool {
		for _, line := range b {
			if m := testFrame.FindStringSubmatch(line); m != nil && (m[1] == "TestMain") == main {
				return true
			}
		}
		return false
	}
	var running [][]string
	for _, main := range []bool{false, true} {
		for _, b := range blocks {
			if runs(b, main) {
				running = append(running, b)
			}
		}
		if len(running) > 0 {
			break
		}
	}
	for i, b := range running {
		if i == 4 {
			shown = append(shown, fmt.Sprintf("... (%d more goroutines running a test)", len(running)-4))
			break
		}
		if len(b) > 24 {
			b = append(b[:24:24], fmt.Sprintf("... (%d more lines)", len(b)-24))
		}
		shown = append(append(shown, ""), b...)
	}
	if len(running) == 0 {
		shown = append(shown, "(no goroutine of the dump runs a test)")
	}
	return strings.Join(shown, "\n")
}

// baselinePassed is stage 4's PASS line for gated, each of whose tests passed unmutated: how many,
// the longest correct run, and the bound of each kind of row the stage ran, each as its run takes
// it: a Go row's is row_timeout plus four times its start-up run (runBound), a Bun row's is
// row_timeout alone (runBun).
func baselinePassed(gated []*row, longest time.Duration, longestRow string, rowTimeout time.Duration) string {
	var goRows, bunRows int
	for _, r := range gated {
		if r.bun {
			bunRows++
		} else {
			goRows++
		}
	}
	var bounds []string
	if goRows > 0 {
		bounds = append(bounds, fmt.Sprintf("each Go row's run is bounded by row_timeout, %s, plus four times its start-up run", rowTimeout))
	}
	if bunRows > 0 {
		bounds = append(bounds, fmt.Sprintf("each Bun row's run is bounded by row_timeout, %s, alone", rowTimeout))
	}
	return fmt.Sprintf("PASS stage 4 (baseline): %d rows' tests pass unmutated (%d Go, %d Bun); the longest correct run is %s (row %s); %s",
		len(gated), goRows, bunRows, longest.Round(time.Second), longestRow, strings.Join(bounds, "; "))
}

func (g *gate) runBun(copyDir string, r *row, res result, start time.Time) result {
	pkg := bunPackage(r.File)
	report := filepath.Join(g.work, "junit.xml")
	_ = os.Remove(report)
	res.bound = g.rowTimeout
	cmd := exec.Command("timeout", strconv.Itoa(int(res.bound.Seconds())), "bun", "test", "--reporter=junit", "--reporter-outfile="+report, strings.TrimPrefix(r.File, pkg+"/"))
	cmd.Dir = filepath.Join(copyDir, pkg)
	out, err := cmd.CombinedOutput()
	res.elapsed = time.Since(start)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 124 {
		res.timedOut = true
	}
	text := string(out)
	res.scan(text)
	if strings.Contains(text, "error TS") || strings.Contains(text, "SyntaxError") || strings.Contains(text, "Could not resolve") {
		res.compileError, res.note = true, lastLines(text, 3)
	}
	xml, _ := os.ReadFile(report)
	state := junitStates(string(xml))[r.Test]
	res.ran = state != ""
	res.failed = state == "failed"
	if state == "skipped" {
		res.skipped = []string{r.Test}
	}
	if res.failed {
		res.note = lastLines(text, 3)
	}
	return res
}

var (
	junitTag  = regexp.MustCompile(`<(/?)(testsuite|testcase|failure|error|skipped)\b([^>]*?)(/?)>`)
	junitName = regexp.MustCompile(`\bname="([^"]*)"`)
)

// junitStates reads a Bun JUnit report: "passed", "failed" or "skipped" for each testcase, by its
// full name - the names of the describe blocks around it and its own, joined by " > ", as Bun
// prints them. A testcase with a failure or an error child failed, one with a skipped child was
// skipped, and of testcases that share a full name the first that did not pass decides.
func junitStates(xml string) map[string]string {
	states := map[string]string{}
	var suites []string
	open, state, inCase := "", "", false // the open testcase's full name and state
	closeCase := func() {
		if before, seen := states[open]; !seen || before == "passed" {
			states[open] = state
		}
		inCase = false
	}
	for _, m := range junitTag.FindAllStringSubmatch(xml, -1) {
		closing, kind, attrs, self := m[1] == "/", m[2], m[3], m[4] == "/"
		name := ""
		if a := junitName.FindStringSubmatch(attrs); a != nil {
			name = html.UnescapeString(a[1])
		}
		switch kind {
		case "testsuite":
			if closing {
				suites = suites[:max(0, len(suites)-1)]
			} else if !self {
				suites = append(suites, name)
			}
		case "testcase":
			if closing {
				if inCase {
					closeCase()
				}
				continue
			}
			open = strings.Join(append(append([]string{}, suites[min(1, len(suites)):]...), name), " > ")
			state, inCase = "passed", true
			if self {
				closeCase()
			}
		default:
			if inCase && !closing {
				if kind == "skipped" {
					state = "skipped"
				} else {
					state = "failed"
				}
			}
		}
	}
	return states
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
