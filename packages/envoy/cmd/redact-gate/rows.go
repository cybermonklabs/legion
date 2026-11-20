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
// pass. A test shared by several rows runs once. The longest correct run is kept: the inventory's
// row_timeout is twice it, as measured.
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
		g.fail("stage 4 (baseline) row %s: the unmutated test %s %v", r.ID, r.Test, err)
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
	if err := g.onlyChange(copyDir, r, before, after); err != nil {
		g.fail("stage 5 (mutation) row %s: %v", r.ID, err)
		return
	}
	res := g.runTest(copyDir, r)
	if verdict := res.evidence(r); verdict != "" {
		g.fail("stage 5 (mutation) row %s (%q in %s): %s", r.ID, r.Mutation.Preimage, r.Function, verdict)
		return
	}
	fmt.Fprintf(g.out, "PASS stage 5 (mutation) row %s: the mutant fails %s at REDACT-ASSERT %s, its captured output naming a marker (%s)\n", r.ID, r.Test, r.Capture, res.elapsed.Round(time.Second))
}

// onlyChange checks that the copy's mutated file differs from the snapshot's by the one
// replacement, and that no other file of the copy differs.
func (g *gate) onlyChange(copyDir string, r *row, before, after []byte) error {
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
// copy: packages/envoy for a Go row, the whole tree (node_modules included) for a Bun row.
func (g *gate) copy(r *row) (string, error) {
	copyDir := filepath.Join(g.work, "copy")
	if err := os.RemoveAll(copyDir); err != nil {
		return "", err
	}
	src, dst := g.snapshot, copyDir
	if !r.bun {
		src, dst = filepath.Join(g.snapshot, "packages/envoy"), filepath.Join(copyDir, "packages/envoy")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("cp", "-a", "--reflink=auto", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cp: %v: %s", err, out)
	}
	return copyDir, nil
}

type result struct {
	ran, failed, timedOut, compileError bool
	asserted                            []string // REDACT-ASSERT <capture> lines
	captures                            map[string][]string
	blind                               []string // the print test's blind rows ("blind: ...")
	otherFailures                       []string
	elapsed                             time.Duration
	note                                string
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
		return errors.New("timed out")
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
		return "the mutant's test timed out"
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

// runTest runs the row's test in the copy, with the gate's timeout. Each test's output is read
// whole once the run ends, joined in the order its events came: test2json splits a line longer
// than its buffer across several output events, and a REDACT-CAPTURE line of a long page is one,
// so a capture read event by event would be cut short.
func (g *gate) runTest(copyDir string, r *row) result {
	res := result{captures: map[string][]string{}}
	start := time.Now()
	defer func() { res.elapsed = time.Since(start) }()
	if r.bun {
		return g.runBun(copyDir, r, res, start)
	}
	cmd := exec.Command("go", "test", "-json", "-count=1", "-v", "-timeout", g.rowTimeout.String(), "-run", "^"+regexp.QuoteMeta(r.Test)+"$", r.Package)
	cmd.Dir = filepath.Join(copyDir, "packages/envoy")
	out, _ := cmd.CombinedOutput()
	res.elapsed = time.Since(start)
	output := map[string]*strings.Builder{}
	var tests []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "{") {
			if strings.Contains(line, "build failed") || strings.Contains(line, "[setup failed]") || regexp.MustCompile(`\.go:\d+:\d+: `).MatchString(line) {
				res.compileError, res.note = true, strings.TrimSpace(line)
			}
			continue
		}
		var e struct{ Action, Test, Output string }
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if strings.Contains(e.Output, "panic: test timed out") {
			res.timedOut = true
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
	return res
}

func (g *gate) runBun(copyDir string, r *row, res result, start time.Time) result {
	pkg := strings.Join(strings.SplitN(r.File, "/", 3)[:2], "/")
	report := filepath.Join(g.work, "junit.xml")
	_ = os.Remove(report)
	cmd := exec.Command("timeout", strconv.Itoa(int(g.rowTimeout.Seconds())), "bun", "test", "--reporter=junit", "--reporter-outfile="+report, strings.TrimPrefix(r.File, pkg+"/"))
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
