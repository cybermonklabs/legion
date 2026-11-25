package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// verify is -verify, the check's normal verification of the rows: check-redact-remote-text.sh
// runs it after stage 3, and stages 4 and 5 only when it passed. Unlike the stages it works in the
// repository's working tree, not a pinned snapshot, from the working tree's inventory
// (verifyInventory). For each Go package the inventory names, in packages/envoy, it runs the
// package's start-up run (startUp) and then `go test -json -count=1 -timeout <bound> -run
// '^(<names>)$'`, where <names> are the top-level names of that package's inventory tests, each
// escaped, so each runs whole with every subtest, and the bound is runBound's for them; the go
// command's stderr is kept apart. A start-up run that does not exit 0 is its package's run, and
// the package's rows do not run. The whole packages are CI's. For each Bun file it runs `bun test`
// in the file's package with Bun's JUnit reporter. Then it prints the verdicts and answers
// judgeVerify's exit status, or 2 where it refused the inventory, a command did not start, or its
// scratch or -keep directory, or a run's record in it, could not be made or written.
//
// With keep, each run's record stays in that directory: for the nth package go-<n>.json (the
// events judged) and go-<n>.err, with go-steps.tsv (the package, its exit status and its stderr's
// file, one line each), and for the nth Bun file bun-<n>.xml and bun-<n>.txt (its JUnit report and
// bun's output), with bun-steps.tsv (the file and its exit status, one line each).
func verify(repo, inventoryPath, keep string, out io.Writer) int {
	inv, rowTimeout, err := verifyInventory(inventoryPath)
	if err != nil {
		fmt.Fprintf(out, "verify: refused: %v\n", err)
		return 2
	}
	dir := keep
	if dir == "" {
		if dir, err = os.MkdirTemp(os.Getenv("TMPDIR"), "redact-verify."); err != nil {
			fmt.Fprintf(out, "verify: refused: scratch: %v\n", err)
			return 2
		}
		defer os.RemoveAll(dir)
	} else if err = os.MkdirAll(dir, 0o755); err == nil {
		dir, err = filepath.Abs(dir)
	}
	if err != nil {
		fmt.Fprintf(out, "verify: refused: -keep %s: %v\n", keep, err)
		return 2
	}
	gos, err := runPackages(filepath.Join(repo, "packages/envoy"), inv.Go, rowTimeout, dir, out)
	if err != nil {
		fmt.Fprintf(out, "verify: refused: %v\n", err)
		return 2
	}
	buns, err := runBunFiles(repo, inv.Bun, dir)
	if err != nil {
		fmt.Fprintf(out, "verify: refused: %v\n", err)
		return 2
	}
	return judgeVerify(inv, gos, buns, out)
}

// wholeSeconds is gate.row_timeout as -verify reads it: a whole count of seconds, such as "48s".
var wholeSeconds = regexp.MustCompile(`^[1-9][0-9]*s$`)

// verifyInventory reads the inventory at path for -verify, with its gate.row_timeout. It refuses
// an inventory it cannot parse; one with no Go row or no Bun row; one with a row that lacks its id,
// requirement, package (a Bun row's file) or test, or holds a line break in one, which would split
// the row's verdict line; one that repeats an id; and one whose gate.row_timeout is not a whole
// count of seconds. The gate's load reads the same file for its stages and refuses only some of
// these: an inventory it cannot parse, one that holds no row at all, and a gate.row_timeout that
// is no duration.
func verifyInventory(path string) (inventory, time.Duration, error) {
	var inv inventory
	data, err := os.ReadFile(path)
	if err == nil {
		inv, err = decodeInventory(data)
	}
	if err != nil {
		return inv, 0, fmt.Errorf("the inventory %s cannot be parsed: %v", path, err)
	}
	if len(inv.Go) == 0 || len(inv.Bun) == 0 {
		return inv, 0, fmt.Errorf("the inventory %s names no Go row or no Bun row", path)
	}
	rowTimeout, err := time.ParseDuration(inv.Gate.RowTimeout)
	if err != nil || !wholeSeconds.MatchString(inv.Gate.RowTimeout) {
		return inv, 0, fmt.Errorf(`the inventory %s names no gate.row_timeout in whole seconds (such as "48s"), the time the test of one row may take`, path)
	}
	seen := map[string]bool{}
	for _, r := range slices.Concat(inv.Go, inv.Bun) {
		place, where := "package", r.Package
		if r.bun {
			place, where = "file", r.File
		}
		for _, field := range [][2]string{{"id", r.ID}, {"requirement", r.Requirement}, {place, where}, {"test", r.Test}} {
			if field[1] == "" || strings.Contains(field[1], "\n") {
				return inv, 0, fmt.Errorf("an inventory row (%q) has no %s", r.ID, field[0])
			}
		}
		if seen[r.ID] {
			return inv, 0, fmt.Errorf("the inventory repeats the id %s", r.ID)
		}
		seen[r.ID] = true
	}
	return inv, rowTimeout, nil
}

// goRun is one Go package's run: the package as the inventory names it, the go command's exit
// status as a shell gives it, the events -verify judges, and the go command's stderr.
type goRun struct {
	pkg            string
	status         int
	events, stderr []byte
}

// bunRun is one Bun file's run: the file as the inventory names it, bun test's exit status, its
// output, where the print test's REDACT-PRINT lines are, and its JUnit report, where it wrote one.
type bunRun struct {
	file           string
	status         int
	output, report []byte
	reported       bool
}

// runPackages runs each Go package of rows in dir, packages/envoy, in the order of their names,
// and prints how long each run took under which bound, and keeps each run's record in keep.
func runPackages(dir string, rows []row, rowTimeout time.Duration, keep string, out io.Writer) ([]goRun, error) {
	tops := map[string][]string{} // each package's top-level test names, escaped
	for _, r := range rows {
		top, _, _ := strings.Cut(r.Test, "/")
		if top = regexp.QuoteMeta(top); !slices.Contains(tops[r.Package], top) {
			tops[r.Package] = append(tops[r.Package], top)
		}
	}
	var steps strings.Builder
	var runs []goRun
	for i, pkg := range slices.Sorted(maps.Keys(tops)) {
		var events, stderr bytes.Buffer
		startup, err := startUp(dir, pkg, &events, &stderr)
		status, err := exitStatus(err)
		if err != nil {
			return nil, fmt.Errorf("go test %s did not start: %v", pkg, err)
		}
		if status != 0 {
			fmt.Fprintf(out, "go test %s: its start-up run (-run '^$') exited %d after %ds, so its rows did not run\n", pkg, status, int(startup.Seconds()))
		} else {
			names := tops[pkg]
			bound := runBound(rowTimeout, len(names), startup)
			events.Reset()
			started := time.Now()
			cmd := exec.Command("go", "test", "-json", "-count=1", "-timeout", bound.String(), "-run", "^("+strings.Join(names, "|")+")$", pkg)
			cmd.Dir, cmd.Stdout, cmd.Stderr = dir, &events, &stderr
			if status, err = exitStatus(cmd.Run()); err != nil {
				return nil, fmt.Errorf("go test %s did not start: %v", pkg, err)
			}
			fmt.Fprintf(out, "go test %s: %ds under a bound of %ds (%d top-level tests at %ds, and four times its start-up run's %ds)\n",
				pkg, int(time.Since(started).Seconds()), int(bound.Seconds()), len(names), int(rowTimeout.Seconds()), int(startup.Seconds()))
		}
		run := goRun{pkg: pkg, status: status, events: events.Bytes(), stderr: stderr.Bytes()}
		errFile := filepath.Join(keep, fmt.Sprintf("go-%d.err", i))
		if err := os.WriteFile(filepath.Join(keep, fmt.Sprintf("go-%d.json", i)), run.events, 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(errFile, run.stderr, 0o644); err != nil {
			return nil, err
		}
		fmt.Fprintf(&steps, "%s\t%d\t%s\n", pkg, status, errFile)
		runs = append(runs, run)
	}
	return runs, os.WriteFile(filepath.Join(keep, "go-steps.tsv"), []byte(steps.String()), 0o644)
}

// bunPackage is the package directory a Bun row's file is run from: its first two path elements
// (packages/<name>) where it has more than two, and the file itself otherwise, which then names no
// package directory and fails as a run, never as an index out of range.
func bunPackage(file string) string {
	if parts := strings.SplitN(file, "/", 3); len(parts) == 3 {
		return parts[0] + "/" + parts[1]
	}
	return file
}

// runBunFiles runs bun test on each Bun file of rows, in the order of their names, in the file's
// package (bunPackage), with Bun's JUnit reporter, and keeps each run's record in keep. The
// verdicts read the report rather than Bun's console, which prints no line for a passing test
// where AGENT or CLAUDECODE is set, as it is in an agent's shell.
func runBunFiles(repo string, rows []row, keep string) ([]bunRun, error) {
	files := make([]string, 0, len(rows))
	for _, r := range rows {
		files = append(files, r.File)
	}
	slices.Sort(files)
	files = slices.Compact(files)
	var steps strings.Builder
	runs := make([]bunRun, 0, len(files))
	for i, file := range files {
		pkg := bunPackage(file)
		report := filepath.Join(keep, fmt.Sprintf("bun-%d.xml", i))
		if err := os.Remove(report); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		cmd := exec.Command("bun", "test", "--reporter=junit", "--reporter-outfile="+report, strings.TrimPrefix(file, pkg+"/"))
		cmd.Dir = filepath.Join(repo, pkg)
		output, err := cmd.CombinedOutput()
		run := bunRun{file: file, output: output}
		if run.status, err = exitStatus(err); err != nil {
			return nil, fmt.Errorf("bun test %s did not start: %v", file, err)
		}
		switch run.report, err = os.ReadFile(report); {
		case err == nil:
			run.reported = true
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(keep, fmt.Sprintf("bun-%d.txt", i)), output, 0o644); err != nil {
			return nil, err
		}
		fmt.Fprintf(&steps, "%s\t%d\n", file, run.status)
		runs = append(runs, run)
	}
	return runs, os.WriteFile(filepath.Join(keep, "bun-steps.tsv"), []byte(steps.String()), 0o644)
}

// exitStatus is the exit status of a command that ran, as a shell gives it (128 and the signal's
// number for one a signal killed), or the error of one that did not start.
func exitStatus(err error) (int, error) {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case !errors.As(err, &exit):
		return 0, err
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return exit.ExitCode(), nil
}

// envoyModule is the module a Go row's package named relative to packages/envoy (./...) is in.
const envoyModule = "github.com/sjawhar/envoy"

// goPackageName is the import path of a Go row's package: a package of another module go.work
// uses, the daemon's, is named by it already.
func goPackageName(pkg string) string {
	if rest, ok := strings.CutPrefix(pkg, "./"); ok {
		return strings.TrimSuffix(envoyModule+"/"+rest, "/")
	}
	return pkg
}

// failureLine is a line of a test's output that reports a failure: a file and line of a _test.go
// file (a t.Log line reads as a t.Error line does), a panic, go test's FAIL, a failed build, a
// timeout, or a compiler's "cannot" or "undefined:". A failed test is named by its first and last.
var failureLine = regexp.MustCompile(`_test\.go:\d+:|^panic:|\bpanic: |^FAIL\b|build failed|timed out|cannot|undefined:`)

// goRead is what -verify reads of one package's go test events: the run is of that package alone,
// so each event is the package's, and its tests are named by test alone ("" the package itself).
type goRead struct {
	verdicts    map[string]string // each test's last pass, fail or skip
	order       []string          // the tests of verdicts, in the order of their first
	said, last  map[string]string // each test's first and last reported line
	output      string            // what the test binary printed, in order
	built       map[string]string // each build target's output (ImportPath)
	broken      string            // the build target whose failure failed the package
	holders     []string          // the print test's REDACT-PRINT lines
	unread      int               // event lines that do not parse
	firstUnread string
}

func readEvents(events []byte) goRead {
	read := goRead{verdicts: map[string]string{}, said: map[string]string{}, last: map[string]string{}, built: map[string]string{}}
	var output strings.Builder
	for line := range bytes.SplitSeq(events, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		var e struct{ Action, Test, Output, ImportPath, FailedBuild string }
		if json.Unmarshal(line, &e) != nil {
			if read.unread == 0 {
				read.firstUnread = string(line)
			}
			read.unread++
			continue
		}
		if at := strings.Index(e.Output, "REDACT-PRINT "); at >= 0 {
			read.holders = append(read.holders, strings.TrimSpace(e.Output[at:]))
		}
		switch e.Action {
		case "build-output":
			read.built[e.ImportPath] += e.Output
		case "output":
			output.WriteString(e.Output)
			if text := strings.TrimSpace(e.Output); failureLine.MatchString(text) {
				if _, ok := read.said[e.Test]; !ok {
					read.said[e.Test] = text
				}
				read.last[e.Test] = text
			}
		case "fail", "pass", "skip":
			if e.Action == "fail" && e.Test == "" && e.FailedBuild != "" {
				read.broken = e.FailedBuild
			}
			if _, ok := read.verdicts[e.Test]; !ok {
				read.order = append(read.order, e.Test)
			}
			read.verdicts[e.Test] = e.Action
		}
	}
	read.output = output.String()
	return read
}

// verdict is the test's last pass, fail or skip, or what stands in for none.
func (read goRead) verdict(test string) string {
	if v, ok := read.verdicts[test]; ok {
		return v
	}
	if test == "" {
		return "did not end"
	}
	return "did not run"
}

// failedLeaves are the failed tests no failed subtest of which failed, in the order they ended.
func (read goRead) failedLeaves() []string {
	var failed, leaves []string
	for _, test := range read.order {
		if test != "" && read.verdicts[test] == "fail" {
			failed = append(failed, test)
		}
	}
	for _, test := range failed {
		if !slices.ContainsFunc(failed, func(other string) bool { return strings.HasPrefix(other, test+"/") }) {
			leaves = append(leaves, test)
		}
	}
	return leaves
}

// lines is a failed test's first reported line, and its last where that differs.
func (read goRead) lines(test string) string {
	first, ok := read.said[test]
	if !ok {
		first = "(no failure line)"
	}
	if final, ok := read.last[test]; ok && final != first {
		return first + "\n      last: " + final
	}
	return first
}

// tail is text's last n lines, after a line that counts those before them, or "(nothing)".
func tail(text string, n int) []string {
	if strings.TrimSpace(text) == "" {
		return []string{"(nothing)"}
	}
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(all) > n {
		return append([]string{fmt.Sprintf("... (%d lines before these)", len(all)-n)}, all[len(all)-n:]...)
	}
	return all
}

// verdicts counts the verdict lines -verify prints by their state: PASS (the print test's PRINT
// line among them), FAIL or UNKNOWN.
type verdicts struct {
	out                     io.Writer
	passed, failed, unknown int
}

// say prints a verdict line, its state (PASS, PRINT, FAIL or UNKNOWN) and then text, and counts it.
func (v *verdicts) say(state, text string) {
	switch state {
	case "PASS", "PRINT":
		v.passed++
	case "FAIL":
		v.failed++
	default:
		v.unknown++
	}
	fmt.Fprintln(v.out, state+" "+text)
}

// printCount is a count a REDACT-PRINT line gives.
var printCount = regexp.MustCompile(`(rows|same|reshaped|relocated|blind)=(\d+)`)

// judgeVerify prints -verify's verdicts of the runs of inv's rows, and answers its exit status.
// It prints a verdict line for each Go package, then for each Go row, then why each package that
// did not pass did not; a verdict line for each Bun file, then for each Bun row; the print test's;
// and last its count of them.
//
//   - A package passed when its go command exited 0, and its events, each of which parses, end in
//     its pass. One that did not pass is UNKNOWN where it ran past its bound (ranPast) and no test
//     of it failed, and failed otherwise. Why one did not pass: the compiler's errors of one that
//     does not build (compilerError); each failed test with its first and last reported lines; the
//     timeout's panic line, the tests it was running and the goroutine running each of one that ran
//     past its bound (stall); the tail of the output of one that failed with no failing test; the
//     first event line that does not parse; and the tail of its stderr.
//   - A Go row passed when its test and its package passed; it is UNKNOWN where its package is and
//     its test did not fail, and failed otherwise (its test failed, was skipped or did not run).
//   - A Bun file passed when bun test exited 0; a Bun row, when its file's JUnit report holds its
//     testcase (by full name, whole, so a longer name that begins with it is not the row's) with no
//     failure, error or skipped child (junitStates).
//   - The print test passed when a holder printed a REDACT-PRINT line, in a Go package's events or
//     a Bun file's output; a run that printed none tested no holder. Its line sums their counts
//     and lists them.
//
// It answers 0 when every verdict passed, 1 when one failed, 3 when none failed and one was
// UNKNOWN, and 2, with no count, where it printed other than one verdict for each package, row,
// Bun file and the print test: a verdict it did not print would pass the row it owes.
func judgeVerify(inv inventory, gos []goRun, buns []bunRun, out io.Writer) int {
	v := &verdicts{out: out}
	reads := map[string]goRead{}
	late := map[string]bool{}
	var why, holders []string
	for _, run := range gos {
		read := readEvents(run.events)
		reads[run.pkg] = read
		holders = append(holders, read.holders...)
		whole := read.verdict("")
		if run.status == 0 && whole == "pass" && read.unread == 0 {
			v.say("PASS", "step go test "+run.pkg)
			continue
		}
		leaves := read.failedLeaves()
		ranPastBound := ranPast(read.output)
		state, what := "FAIL", "no test failed, and it ended in "+whole
		switch {
		case read.broken != "":
			what = fmt.Sprintf("it does not build (%s)", read.broken)
		case len(leaves) > 0:
			what = fmt.Sprintf("%d tests failed", len(leaves))
			if ranPastBound {
				what += ", and it ran past its bound"
			}
		case ranPastBound:
			state, what = "UNKNOWN", "it ran past its bound and no test failed, so its rows neither passed nor failed: run it again"
			late[run.pkg] = true
		case read.unread > 0:
			what = fmt.Sprintf("unreadable event lines: %d (a go command killed mid-line cuts its last line short), so no reading of its run is whole", read.unread)
		}
		v.say(state, fmt.Sprintf("step go test %s (exit %d): %s", run.pkg, run.status, what))
		why = append(why, fmt.Sprintf("  why %s did not pass:", goPackageName(run.pkg)))
		if read.broken != "" {
			why = append(why, "    "+compilerError(read.built[read.broken]))
		}
		for _, test := range leaves {
			why = append(why, fmt.Sprintf("    %s: %s", test, read.lines(test)))
		}
		if ranPastBound {
			for _, line := range strings.Split(stall(read.output), "\n") {
				why = append(why, "    "+line)
			}
		}
		if read.broken == "" && len(leaves) == 0 && !ranPastBound {
			for _, line := range tail(read.output, 20) {
				why = append(why, "    "+line)
			}
		}
		if read.unread > 0 {
			why = append(why, "    its first event line that does not parse: "+read.firstUnread)
		}
		if strings.TrimSpace(string(run.stderr)) != "" {
			why = append(why, "    its stderr, last lines:")
			for _, line := range tail(string(run.stderr), 20) {
				why = append(why, "      "+line)
			}
		}
	}
	for _, r := range inv.Go {
		read := reads[r.Package]
		ran, whole := read.verdict(r.Test), read.verdict("")
		row := fmt.Sprintf("%s (%s) %s %s", r.ID, r.Requirement, r.Package, r.Test)
		switch {
		case ran == "pass" && whole == "pass":
			v.say("PASS", row)
		case late[r.Package] && ran != "fail":
			v.say("UNKNOWN", fmt.Sprintf("%s: test %s, package ran past its bound", row, ran))
		default:
			v.say("FAIL", fmt.Sprintf("%s: test %s, package %s", row, ran, whole))
		}
	}
	for _, line := range why {
		fmt.Fprintln(out, line)
	}

	states := map[string]map[string]string{}
	for _, run := range buns {
		if run.status == 0 {
			v.say("PASS", "step bun test "+run.file)
		} else {
			v.say("FAIL", fmt.Sprintf("step bun test %s (exit %d)", run.file, run.status))
		}
		if run.reported {
			states[run.file] = junitStates(string(run.report))
		}
		for _, line := range strings.Split(string(run.output), "\n") {
			if strings.HasPrefix(line, "REDACT-PRINT ") {
				holders = append(holders, strings.TrimSpace(line))
			}
		}
	}
	for _, r := range inv.Bun {
		row := fmt.Sprintf("%s (%s) %s %s", r.ID, r.Requirement, r.File, r.Test)
		found, hasReport := states[r.File]
		state := "has no JUnit report"
		if hasReport {
			switch state = found[r.Test]; state {
			case "":
				state = "is not in the report"
			case "skipped":
				state = "was skipped"
			}
		}
		if state == "passed" {
			v.say("PASS", row)
		} else {
			v.say("FAIL", fmt.Sprintf("%s: the test %s", row, state))
		}
	}

	if len(holders) == 0 {
		v.say("FAIL", "print-test: no holder printed a REDACT-PRINT line, so no print row ran")
	} else {
		total := map[string]int{}
		for _, line := range holders {
			for _, m := range printCount.FindAllStringSubmatch(line, -1) {
				n, _ := strconv.Atoi(m[2])
				total[m[1]] += n
			}
		}
		v.say("PRINT", fmt.Sprintf("%d holders: %d printer and placement rows, %d with a twin at their own placement and shape, %d reshaped and %d relocated (each shows only that the printer output is captured there), %d blind",
			len(holders), total["rows"], total["same"], total["reshaped"], total["relocated"], total["blind"]))
		for _, line := range holders {
			fmt.Fprintln(out, "  "+line)
		}
	}

	packages, files := map[string]bool{}, map[string]bool{}
	for _, r := range inv.Go {
		packages[r.Package] = true
	}
	for _, r := range inv.Bun {
		files[r.File] = true
	}
	owed := len(packages) + len(inv.Go) + len(files) + len(inv.Bun) + 1
	owes := fmt.Sprintf("one for each of %d Go packages, %d Go rows, %d Bun files and %d Bun rows, and the print test's", len(packages), len(inv.Go), len(files), len(inv.Bun))
	if got := v.passed + v.failed + v.unknown; got != owed {
		fmt.Fprintf(out, "verify: refused: it printed %d verdict lines where it owes %d (%s), so the rows have no verdict\n", got, owed, owes)
		return 2
	}
	fmt.Fprintf(out, "verify: %d rows, %d failed, %d UNKNOWN (%d verdicts: %s)\n", len(inv.Go)+len(inv.Bun), v.failed, v.unknown, owed, owes)
	switch {
	case v.failed > 0:
		return 1
	case v.unknown > 0:
		return 3
	}
	return 0
}
