package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
// bound is UNKNOWN, not a failure.
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
		if errors.As(err, &late) {
			g.unknown("stage 4 (baseline) row %s: the unmutated test %s %v", r.ID, r.Test, err)
		} else {
			g.fail("stage 4 (baseline) row %s: the unmutated test %s %v", r.ID, r.Test, err)
		}
	}
}

// gateRow copies the snapshot, applies the row's one mutation, checks the diff, runs the test and
// judges the evidence (stage 5).
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
	if verdict := res.evidence(r); verdict != "" {
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

var (
	captureLine = regexp.MustCompile(`REDACT-CAPTURE (\S+) ([A-Za-z0-9+/=]+)`)
	assertLine  = regexp.MustCompile(`REDACT-ASSERT ([^\s:]+)`)
	blindLine   = regexp.MustCompile(`blind: [^\n]*`)
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
// the print test's rows were blind, so a control reads which check caught it.
func (res result) baselineErr() error {
	switch {
	case res.compileError:
		return fmt.Errorf("does not compile: %s", res.note)
	case res.timedOut:
		return res.late()
	case !res.ran:
		return errors.New("is missing: it did not run")
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
// assertion but the output it captured there holds none.
func (res result) evidence(r *row) string {
	switch {
	case res.compileError:
		return "the mutant does not compile: " + res.note
	case res.timedOut:
		return "the mutant's test " + res.late().Error()
	case !res.ran:
		return "the test is missing"
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
// The bound is derived as check-redact-remote-text.sh derives each package's, whose comment says
// why: the start-up run, `go test -json -count=1 -run '^$'` of the row's package in the copy,
// timed, builds and links its test binary and runs it with no test under the box's load of that
// moment; the bound is row_timeout, the row's own budget, plus four times that time. A start-up
// run that fails is not read: the run after it fails the same way, and that run is read.
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
	startup := exec.Command("go", "test", "-json", "-count=1", "-run", "^$", r.Package)
	startup.Dir = filepath.Join(copyDir, mod)
	_ = startup.Run()
	res.startup = ceilSecond(time.Since(start))
	res.bound = g.rowTimeout + 4*res.startup
	start = time.Now()
	cmd := exec.Command("go", "test", "-json", "-count=1", "-v", "-timeout", res.bound.String(), "-run", "^"+regexp.QuoteMeta(r.Test)+"$", r.Package)
	cmd.Dir = filepath.Join(copyDir, mod)
	out, _ := cmd.CombinedOutput()
	res.elapsed = time.Since(start)
	output := map[string]*strings.Builder{}
	var printed strings.Builder // all the test binary printed, in order, for a run past its bound
	var built strings.Builder   // the go command's build output, which -json gives as build-output events
	var tests []string
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
		// The binary's own panic at its -timeout, or the go command's kill a minute after it.
		if strings.Contains(e.Output, "panic: test timed out") || strings.Contains(e.Output, "*** Test killed with quit: ran too long") {
			res.timedOut = true
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
			if regexp.MustCompile(`_test\.go:\d+: `).MatchString(e.Output) && !strings.Contains(e.Output, "REDACT-") && res.note == "" {
				res.note = strings.TrimSpace(e.Output)
			}
		}
	}
	for _, test := range tests {
		res.scan(output[test].String())
	}
	if res.compileError && res.note == "" {
		res.note = compilerError(built.String())
	}
	if res.timedOut {
		res.stall = stall(printed.String())
	}
	return res
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

// stall is what a run past its bound printed of it: the timeout's panic line with the tests it was
// running, the go command's kill line, and the goroutines of the dump that run a test - each with
// a frame of a test function other than TestMain - or, where none does, those in TestMain (a start
// or an end past the bound). check-redact-remote-text.sh prints the same for a package.
func stall(text string) string {
	lines := strings.Split(text, "\n")
	var shown []string
	var blocks [][]string
	var block []string
	panicked := false
	for i, line := range lines {
		switch {
		case !panicked && strings.HasPrefix(line, "panic: test timed out after "):
			panicked = true
			shown = append(shown, line)
			for _, next := range lines[i+1:] {
				if strings.TrimSpace(next) == "" {
					break
				}
				shown = append(shown, next)
			}
		case strings.HasPrefix(line, "*** Test killed with quit: ran too long"):
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

func (g *gate) runBun(copyDir string, r *row, res result, start time.Time) result {
	pkg := strings.Join(strings.SplitN(r.File, "/", 3)[:2], "/")
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
	state := junitState(string(xml), r.Test)
	res.ran = state != ""
	res.failed = state == "failed"
	if res.failed {
		res.note = lastLines(text, 3)
	}
	return res
}

// junitState is "passed", "failed", "skipped" or "" (not in the report) for the testcase whose
// full name is name.
func junitState(xml, name string) string {
	tag := regexp.MustCompile(`<(/?)(testsuite|testcase|failure|error|skipped)\b([^>]*?)(/?)>`)
	attr := regexp.MustCompile(`\bname="([^"]*)"`)
	var suites []string
	state, open := "", ""
	var current string
	for _, m := range tag.FindAllStringSubmatch(xml, -1) {
		closing, kind, attrs, self := m[1] == "/", m[2], m[3], m[4] == "/"
		n := ""
		if a := attr.FindStringSubmatch(attrs); a != nil {
			n = unescape(a[1])
		}
		switch kind {
		case "testsuite":
			if closing {
				suites = suites[:max(0, len(suites)-1)]
			} else if !self {
				suites = append(suites, n)
			}
		case "testcase":
			if closing {
				if current == name && (state == "" || state == "passed") {
					state = open
				}
				current = ""
				continue
			}
			full := strings.Join(append(append([]string{}, suites[min(1, len(suites)):]...), n), " > ")
			current, open = full, "passed"
			if self && full == name {
				state = "passed"
				current = ""
			}
		default:
			if current != "" && !closing {
				if kind == "skipped" {
					open = "skipped"
				} else {
					open = "failed"
				}
			}
		}
	}
	return state
}

func unescape(s string) string {
	return strings.NewReplacer("&apos;", "'", "&quot;", `"`, "&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
