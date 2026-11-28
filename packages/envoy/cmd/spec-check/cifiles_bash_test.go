package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// The forms of the claim, from which the test builds its scripts: the command's name, the white
// space between it and the subcommand, the subcommand (each quote form around it or inside it, a
// $ quote, an escaped letter, and a backslash and a line break inside it), the text before the
// command (a comment and a here-document whose bodies hold a quote and a backslash, a quote that an
// earlier line opens), and a nested shell around the script.
var (
	bashNames    = []string{"spec-check", "'spec-check'", `spec"-"check`, "./bin/spec-check", "spec-\\\ncheck"}
	bashBetween  = []string{" ", " \\\n", " \\\n  ", "\\\n  "}
	bashSubs     = []string{"compare", "'compare'", `"compare"`, "co'mp'are", "''compare", "$'compare'", `$"compare"`, `\compare`, "com\\\npare", "\"com\\\npare\"", "tree", "'tree'", "tr\\\nee"}
	bashPrefixes = []string{"", "env -u GITHUB_ACTIONS ", "# \" \\\n", ": <<'END\\'\n\" \\\nEND\\\n", ": \"it's\"; ", "echo 'start\nend' && "}
	bashNestedBy = []string{"", "# \" \\\n"}
	bashSingle   = func(s string) string { return "sh -c '" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	bashDouble   = func(s string) string {
		return `bash -c "` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(s) + `"`
	}
	bashHere      = func(s string) string { return "bash <<'EOF'\n" + s + "\nEOF" }
	bashWrappings = []func(string) string{bashSingle, bashDouble, bashHere,
		func(s string) string { return bashSingle(bashSingle(s)) }, func(s string) string { return bashSingle(bashDouble(s)) },
		func(s string) string { return bashDouble(bashSingle(s)) }, func(s string) string { return bashDouble(bashDouble(s)) }}
)

// bashCommands is each command of bashNames, bashBetween and bashSubs, which
// TestCIFilesThatCompareNamesEachComparisonThatBashRuns writes in each shape of bashShapeList. It
// leaves out a command where a backslash and a line break right after the command's name, with
// blanks after them, meet another backslash and line break in the name or in the subcommand: one
// reading of the rule keeps the blanks and the other removes them, and neither reads both
// (TestCIFilesThatCompareLeavesAFormOutsideTheClaim).
func bashCommands() []string {
	var commands []string
	for _, name := range bashNames {
		for _, between := range bashBetween {
			for _, sub := range bashSubs {
				if between == "\\\n  " && strings.Contains(name+sub, "\\\n") {
					continue
				}
				commands = append(commands, name+between+sub+" --dir a")
			}
		}
	}
	return commands
}

// bashShape is a shape that TestCIFilesThatCompareNamesEachComparisonThatBashRuns writes each
// command of bashCommands in: its name in plain words, and the script that it makes of a command.
type bashShape struct {
	name string
	of   func(command string) string
}

// bashShapeList is each bashShape, in its order: after each of bashPrefixes, then after
// each of bashNestedBy inside each of bashWrappings. The name of a wrapping shows it around an
// ellipsis.
func bashShapeList() []bashShape {
	var shapes []bashShape
	for _, prefix := range bashPrefixes {
		shapes = append(shapes, bashShape{fmt.Sprintf("after the prefix %q", prefix), func(command string) string { return prefix + command }})
	}
	for _, wrap := range bashWrappings {
		for _, nested := range bashNestedBy {
			shapes = append(shapes, bashShape{fmt.Sprintf("inside %q after the nested prefix %q", wrap("…"), nested), func(command string) string { return wrap(nested + command) }})
		}
	}
	return shapes
}

// The test asks Bash itself. Each command of bashCommands, in each shape of bashShapeList, is a
// script, which runs in bash three ways: as a script, as a workflow's run: | block and as a folded
// run: > value, the value that YAML reads from the file. spec-check is a shell function that
// records its first argument, and a spec-check on PATH and in ./bin does the same for a nested sh
// and for a path. Whenever Bash runs spec-check with compare as its first argument, directly or in
// a nested shell, the guard names the file; a miss fails the test. Only a file that the guard
// names no line of can be a miss, so Bash runs those (expectEachComparisonNamed), with the
// controls: spec-check compare --dir a, and spec-check tree --dir a, of the name spec-check and of
// ./bin/spec-check, in each shape of bashShapeList. The test makes the files and the controls of
// each shape from that shape, so a group holds the files of its shape alone, whatever the order
// of the shapes and the commands. Each shape runs as three parallel subtests, each with a third of
// the files of that shape and the four controls in that shape, so that a part of the slowest
// shapes, those inside two shells, is short too.
func TestCIFilesThatCompareNamesEachComparisonThatBashRuns(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		skipOutsideCI(t, fmt.Sprintf("bash is not on PATH, so no script can run in it: %v", err))
	}
	commands := bashCommands()
	var parts []bashGroup
	for _, shape := range bashShapeList() {
		g := bashGroup{name: shape.name}
		for _, command := range commands {
			script := shape.of(command)
			g.files = append(g.files, bashFile{"script", script, script})
			for _, style := range []string{"|", ">"} {
				raw := workflowSteps + "      - run: " + style + "\n          " + strings.ReplaceAll(script, "\n", "\n          ") + "\n"
				var workflow struct {
					Jobs map[string]struct{ Steps []struct{ Run string } }
				}
				if err := yaml.Unmarshal([]byte(raw), &workflow); err != nil {
					t.Fatalf("YAML cannot read %q: %v", raw, err)
				}
				g.files = append(g.files, bashFile{"run: " + style, raw, workflow.Jobs["spec"].Steps[0].Run})
			}
		}
		for _, sub := range []string{"compare", "tree"} {
			for _, command := range []string{"spec-check " + sub + " --dir a", "./bin/spec-check " + sub + " --dir a"} {
				g.controls = append(g.controls, bashControl{shape.of(command), sub == "compare"})
			}
		}
		parts = append(parts, g.inParts(3)...)
	}
	for _, g := range parts {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			expectEachComparisonNamed(t, bash, g.files, g.controls)
		})
	}
}

// bashEarlierParts, bashEarlierForms and bashEarlierWords are the pieces of the scripts that the
// Bash test of the round-6 reader (6823f4702b) built, whose comparisons each head of the text rule
// names too: a part is a text before the command, a form writes the command, with {w} the
// subcommand and {h} and {t} its two halves, and a word is a subcommand, in the order of the
// scripts. The forms that only that reader named are left out, since they are outside the text
// rule's claim (TestCIFilesThatCompareLeavesAFormOutsideTheClaim): a $ and an ANSI-C escape that a
// line continuation splits, and a redirection or a here-document between the two words.
var (
	bashEarlierParts = []string{
		": x; ", ": 'say \"x\"'; ", ": \"it's\"; ", ": $'it\\'s \"x\"'; ", ": a \\\nb; ", ": a\\\nb; ",
		": 'a\\\n\"b'; ", ": \"a\\\n'b\nc\"; ", ": $'a\\\n\"b'; ", ": 'a\"\nb\nc'; ", ": \"a'\nb\"; ",
		": $'a\"\nb\nc'; ", "# a\n", "# it's\n", "# a \\\n", "# \" \\\n",
		": <<END\nspec-check compare --dir \"it's\" \\\nx\nEND\n",
		": <<'END\\'\nspec-check compare --dir \" \\\nEND\\\n",
		": <<-\"END\"\n\tspec-check compare --dir 'it\\'s\n\tEND\n",
	}
	bashEarlierForms = []string{
		"spec-check {w} --dir a\n", "spec-check \\\n{w} --dir a\n", "spec-check $\\\n\"{w}\" --dir a\n",
		"spec-check \"{h}\\\n{t}\" --dir a\n", "spec-check '{h}\\\n{t}' --dir a\n", "spec-check $'{h}\\\n{t}' --dir a\n",
		"spec-check \\ {w} --dir a\n", "spec-check \\\t{w} --dir a\n", "spec-check \\\"{w}\\\" --dir a\n",
		": \\#'\n'; spec-check {w} --dir a\n", "spec-check {w} --dir a \\", ": '\nspec-check {w} --dir a\n'\n",
		": \"\nspec-check {w} --dir a\n\"\n", ": $'\nspec-check {w} --dir a\n'\n", "spec-check {w} --dir a # it's \\\n",
		"spec-check {w} --dir a <<'END'\nit's\nEND\n", "spec-check {h}\\\n{t} --dir a\n",
	}
	bashEarlierWords = []string{"compare", "tree"}
)

// bashEarlierScripts is each form of bashEarlierForms after no part and after each part of
// bashEarlierParts, and the last form, a line continuation inside the word, after each sequence of
// two parts and of three parts, each for compare and for tree; then the forms and the sequences of
// two parts again inside each of bashWrappings, as the round-6 test gave them to nested shells.
func bashEarlierScripts() []string {
	var scripts, wrapped []string
	for _, word := range bashEarlierWords {
		head, tail := "com", "pare"
		if word == "tree" {
			head, tail = "tr", "ee"
		}
		command := func(form string) string {
			return strings.NewReplacer("{w}", word, "{h}", head, "{t}", tail).Replace(form)
		}
		last := command(bashEarlierForms[len(bashEarlierForms)-1])
		for _, form := range bashEarlierForms {
			wrapped = append(wrapped, command(form))
			for _, p := range bashEarlierParts {
				wrapped = append(wrapped, p+command(form))
			}
		}
		for _, p := range bashEarlierParts {
			for _, q := range bashEarlierParts {
				wrapped = append(wrapped, p+q+last)
				for _, r := range bashEarlierParts {
					scripts = append(scripts, p+q+r+last)
				}
			}
		}
	}
	scripts = append(scripts, wrapped...)
	for _, wrap := range bashWrappings {
		for _, s := range wrapped {
			scripts = append(scripts, wrap(s))
		}
	}
	return scripts
}

// The scripts of the round-6 reader's Bash test, which the text rule names too, run in bash as
// scripts, as that test ran them: whenever Bash runs the comparison, directly or in a nested shell,
// the guard names the file. So no comparison that a head of the text rule named in that corpus is
// lost. The controls are the first form, spec-check compare --dir a and spec-check tree --dir a,
// after no part and after each part of bashEarlierParts, and inside each of bashWrappings. For each
// word, the three-part scripts after each first part, the forms with the two-part scripts, and those
// again inside each of bashWrappings are each a group of parallel subtests, which run its scripts
// with every control, so that each subtest's check of the controls is the whole corpus's. A group
// is one subtest, but for a tree group of the forms and the two-part scripts: the guard names few of
// those, so Bash runs most, and each shell of the wrapping adds to each one's time, so such a group
// runs in three parts and three more for each shell.
func TestCIFilesThatCompareNamesEachComparisonOfTheEarlierBashCorpus(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		skipOutsideCI(t, fmt.Sprintf("bash is not on PATH, so no script can run in it: %v", err))
	}
	var files []bashFile
	for _, script := range bashEarlierScripts() {
		files = append(files, bashFile{"script", script, script})
	}
	var controls []bashControl
	for _, word := range bashEarlierWords {
		command := strings.ReplaceAll(bashEarlierForms[0], "{w}", word)
		scripts := []string{command}
		for _, p := range bashEarlierParts {
			scripts = append(scripts, p+command)
		}
		for _, wrap := range bashWrappings {
			scripts = append(scripts, wrap(command))
		}
		for _, script := range scripts {
			controls = append(controls, bashControl{script, word == "compare"})
		}
	}
	// bashEarlierScripts writes the three-part scripts of compare and then of tree, each word's in
	// runs of one first part, then the forms and the two-part scripts of each word, and then those
	// again inside each of bashWrappings.
	threePart := len(bashEarlierWords) * len(bashEarlierParts) * len(bashEarlierParts) * len(bashEarlierParts)
	partsOf := func(w, shells int) int {
		if bashEarlierWords[w] != "tree" {
			return 1
		}
		return 3 * (1 + shells)
	}
	var groups []bashGroup
	for w, ofWord := range inRuns(t, files[:threePart], len(bashEarlierWords)) {
		for p, run := range inRuns(t, ofWord, len(bashEarlierParts)) {
			name := fmt.Sprintf("%s in the three-part scripts after the first part %q", bashEarlierWords[w], bashEarlierParts[p])
			for _, f := range run {
				if !strings.HasPrefix(f.script, bashEarlierParts[p]) {
					t.Fatalf("the group %s holds %q, so the subtests would not hold the groups that they name", name, f.script)
				}
			}
			groups = append(groups, bashGroup{name: name, files: run, controls: controls})
		}
	}
	sets := inRuns(t, files[threePart:], 1+len(bashWrappings))
	unwrapped := inRuns(t, sets[0], len(bashEarlierWords))
	for w, run := range unwrapped {
		groups = append(groups, bashGroup{name: bashEarlierWords[w] + " in the forms and the two-part scripts", files: run, controls: controls}.inParts(partsOf(w, 0))...)
	}
	for i, wrap := range bashWrappings {
		shells := strings.Count(wrap("…"), "sh ") // the shells that the wrapping starts, each a sh or a bash
		for w, run := range inRuns(t, sets[1+i], len(bashEarlierWords)) {
			name := fmt.Sprintf("%s in the forms and the two-part scripts inside %q", bashEarlierWords[w], wrap("…"))
			for j, f := range run {
				if want := wrap(unwrapped[w][j].script); f.script != want {
					t.Fatalf("the group %s holds %q where %q is due, so the subtests would not hold the groups that they name", name, f.script, want)
				}
			}
			groups = append(groups, bashGroup{name: name, files: run, controls: controls}.inParts(partsOf(w, shells))...)
		}
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			expectEachComparisonNamed(t, bash, g.files, g.controls)
		})
	}
}

// bashFile is one file of a Bash test: its kind, its text as the guard reads it, and the script
// that Bash runs from it.
type bashFile struct{ kind, raw, script string }

// bashControl is a script that Bash runs beside the files of a Bash test, and whether Bash runs
// the comparison in it: a control of the shape of the test's scripts, which runs spec-check
// compare, or another subcommand, and nothing else.
type bashControl struct {
	script   string
	compares bool
}

// bashGroup is files of a Bash test that run together, and the controls that Bash runs beside
// them: a subtest, or a group that the test cuts into subtests (inParts).
type bashGroup struct {
	name     string
	files    []bashFile
	controls []bashControl
}

// inParts is g cut into n parts of its files, in file order and of lengths that differ by one at
// most, each with all of g's controls and named g's name, part i of n. It is g alone when n is 1.
func (g bashGroup) inParts(n int) []bashGroup {
	if n == 1 {
		return []bashGroup{g}
	}
	parts := make([]bashGroup, n)
	for i := range parts {
		from, to := i*len(g.files)/n, (i+1)*len(g.files)/n
		parts[i] = bashGroup{name: fmt.Sprintf("%s, part %d of %d", g.name, i+1, n), files: g.files[from:to:to], controls: g.controls}
	}
	return parts
}

// inRuns is list cut into n runs of one length, in order. It fails the test unless the list holds
// a whole number of such runs.
func inRuns[T any](t *testing.T, list []T, n int) [][]T {
	t.Helper()
	if len(list)%n != 0 {
		t.Fatalf("%d items make no %d runs of one length", len(list), n)
	}
	return slices.Collect(slices.Chunk(list, len(list)/n))
}

// expectEachComparisonNamed fails the test for each file of files where Bash runs the comparison
// and the guard names no line. Only a file that the guard names no line of can be a miss, whatever
// Bash runs in a file that it names, so Bash runs the script of each file that the guard names no
// line of, and of each of controls; the guard reads every file. The test fails as UNKNOWN unless
// Bash, as firstArguments asks it, runs the comparison in each control that runs compare and in no
// other control, so that an answer of no comparison comes from an instrument that sees one in each
// shape of the files, and tells it from another subcommand. It logs how many files the guard names.
func expectEachComparisonNamed(t *testing.T, bash string, files []bashFile, controls []bashControl) {
	t.Helper()
	var unnamed []bashFile
	for _, f := range files {
		if textHits([]byte(f.raw)) == nil {
			unnamed = append(unnamed, f)
		}
	}
	scripts := make([]string, 0, len(controls)+len(unnamed))
	for _, c := range controls {
		scripts = append(scripts, c.script)
	}
	for _, f := range unnamed {
		scripts = append(scripts, f.script)
	}
	calls := firstArguments(t, bash, scripts)
	for n, c := range controls {
		if slices.Contains(calls[n], "compare") != c.compares {
			t.Fatalf("UNKNOWN: in the control %q bash makes the calls %q, and runs the comparison there: %t; want %t, so bash as the test asks it cannot show a miss", c.script, calls[n], !c.compares, c.compares)
		}
	}
	missed := 0
	for n, f := range unnamed {
		if c := calls[len(controls)+n]; slices.Contains(c, "compare") {
			if missed++; missed <= 20 {
				t.Errorf("%s %q: bash runs the comparison (first arguments %q), and the guard names no line", f.kind, f.raw, c)
			}
		}
	}
	t.Logf("%d files. The guard names %d; bash ran the other %d and %d controls, and runs the comparison in %d of those files, each a miss",
		len(files), len(files)-len(unnamed), len(unnamed), len(controls), missed)
}

// firstArguments runs each of scripts in bash and answers, for each, the first argument of each
// call of spec-check that it makes. A bash that defines spec-check as a function, and exports it to
// a nested bash, reads the scripts on its standard input and runs each one as (eval -- "$s") on its
// line 3, the line of its loop. The subshell keeps the exported function, the recorder on file
// descriptor 3 and the batch's PATH, and gives each script /dev/null as its standard input. A
// variable, exported or not, a function, an alias, an option, a trap or a working directory that a
// script sets ends with its subshell, and no later script of the batch sees it. Two things outlive
// the subshell: a file that a script writes stays for the scripts after it, and a process that a
// script starts in the background and leaves running can call spec-check after the subshell ends,
// and that call is recorded as one of the script of the batch that runs at that moment. A script
// does not run as . runs a file: $0 and BASH_SOURCE name the driver, LINENO counts from the
// driver's line 3, so that a script's line n reads n + 2, there are no positional parameters, and
// a top-level return is an error after which the script goes on. Two tests pin each of these facts,
// TestFirstArgumentsRunsEachScriptInItsOwnShell and
// TestFirstArgumentsLetsAFileAndABackgroundProcessOutliveTheirScript, and a third the standard
// input, TestFirstArgumentsGivesEachScriptEmptyStandardInput. A spec-check on PATH and in ./bin
// records the calls of a nested sh and of a path. The scripts run in batches, one bash for each, so
// that no script is a file to write and remove. Before each script's calls the driver writes a byte
// 1, the script's number and a byte 1, and each call's first argument with a NUL after it.
func firstArguments(t *testing.T, bash string, scripts []string) [][]string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "spec-check"), []byte("#!/bin/sh\nprintf '%s\\0' \"$1\" >&3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(dir, "driver.sh")
	writeFile(t, driver, []byte("spec-check() { printf '%s\\0' \"$1\" >&3; }\nexport -f spec-check\nwhile IFS= read -r -d '' n && IFS= read -r -d '' s; do printf '\\001%s\\001' \"$n\"; (eval -- \"$s\") 3>&1 </dev/null >/dev/null 2>&1; done\nexit 0\n"))
	batches := make([][]byte, min(runtime.NumCPU(), 16))
	for n, s := range scripts {
		if strings.Contains(s, "\x00") {
			t.Fatalf("the script %q holds a NUL, which ends a script on bash's standard input", s)
		}
		batches[n%len(batches)] = fmt.Appendf(batches[n%len(batches)], "%d\x00%s\x00", n, s)
	}
	outputs := make([][]byte, len(batches))
	failures := make([]error, len(batches))
	var wg sync.WaitGroup
	for b, batch := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run := exec.Command(bash, driver)
			run.Stdin = bytes.NewReader(batch)
			run.Dir = dir
			run.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "LC_ALL=C"}
			var stderr strings.Builder
			run.Stderr = &stderr
			if outputs[b], failures[b] = run.Output(); failures[b] != nil {
				failures[b] = fmt.Errorf("%v: %s", failures[b], stderr.String())
			}
		}()
	}
	wg.Wait()
	calls := make([][]string, len(scripts))
	ran := 0
	for b, out := range outputs {
		if failures[b] != nil {
			t.Fatal(failures[b])
		}
		records := strings.Split(string(out), "\x01")
		for k := 1; k+1 < len(records); k += 2 {
			var n int
			if _, err := fmt.Sscan(records[k], &n); err != nil || n >= len(scripts) {
				t.Fatalf("batch %d wrote a record of no script, %q", b, records[k])
			}
			ran++
			if records[k+1] != "" {
				calls[n] = strings.Split(strings.TrimSuffix(records[k+1], "\x00"), "\x00")
			}
		}
	}
	if ran != len(scripts) {
		t.Fatalf("bash wrote the calls of %d of %d scripts", ran, len(scripts))
	}
	return calls
}

// Each script of a batch runs in its own subshell. A script at the start of the batch sets a
// variable, an exported variable, a function, an alias, an option, a trap and the working
// directory, and each of the next sixteen scripts calls spec-check with the name of each of them
// that it sees: whatever the number of batches, one of them is in the first script's batch, after
// it. So a driver that runs the scripts in one shell fails here: the script that exits ends its
// batch, and with that script taken out, the later script names each state that it sees. The same
// batch keeps the other cases that the driver's doc names: $0 and BASH_SOURCE are the driver, a
// script's line 3 reads LINENO 5, there is no positional parameter, standard input is empty, an
// unclosed quote and an unfinished here-document make no call, a top-level return goes on, and an
// exit ends only its script.
func TestFirstArgumentsRunsEachScriptInItsOwnShell(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		skipOutsideCI(t, fmt.Sprintf("bash is not on PATH, so no script can run in it: %v", err))
	}
	scripts := []string{"batch_variable=set; export batch_exported=set; batch_function() { :; }; alias batch_alias=:; set -f; trap : USR1; cd bin"}
	want := [][]string{nil}
	for range 16 {
		scripts = append(scripts, `[[ -v batch_variable ]] && spec-check variable
[[ -v batch_exported ]] && spec-check exported
declare -F batch_function >/dev/null && spec-check function
alias batch_alias >/dev/null 2>&1 && spec-check alias
[[ -o noglob ]] && spec-check option
[[ -n $(trap -p USR1) ]] && spec-check trap
[[ $PWD == */bin ]] && spec-check directory
spec-check tree`)
		want = append(want, []string{"tree"})
	}
	for _, c := range []struct {
		script string
		want   []string
	}{
		{`spec-check "${0##*/}"`, []string{"driver.sh"}},
		{`spec-check "${BASH_SOURCE[0]##*/}"`, []string{"driver.sh"}},
		{":\n:\nspec-check \"$LINENO\"", []string{"5"}},
		{`spec-check "$#"`, []string{"0"}},
		{`if IFS= read -r line; then spec-check compare; else spec-check tree; fi`, []string{"tree"}},
		{`spec-check compare --dir "unterminated`, nil},
		{"cat <<'EOF'\nspec-check compare\n", nil},
		{"return\nspec-check tree", []string{"tree"}},
		{"exit 3", nil},
		{"spec-check tree", []string{"tree"}},
	} {
		scripts = append(scripts, c.script)
		want = append(want, c.want)
	}
	for i, got := range firstArguments(t, bash, scripts) {
		if !slices.Equal(got, want[i]) {
			t.Errorf("script %d %q calls %q, want %q", i, scripts[i], got, want[i])
		}
	}
}

// Two things outlive a script's subshell, as the driver's doc says: a file that the script writes,
// and a process that it starts in the background, whose call of spec-check after the subshell ends
// is recorded as one of the script that runs then. The first script writes a file and starts a
// process that waits until a later script of its batch has begun. The scripts of one batch share
// the batch's $$, so of the next sixteen scripts, the first of the first script's batch finds the
// file, calls spec-check file, lets the process call spec-check background, and waits for that
// call: whatever the number of batches, one of the sixteen is in that batch, after the first
// script. So that script records file, background and tree, each other one tree alone, and the first
// script nothing. A driver that took the file away, or waited for a script's background processes,
// fails here; the bound of each wait, 6,000 rounds of 10 ms, only turns such a driver's wait, which
// never ends, into a failure, where an honest one ends as soon as the next script of the batch starts.
func TestFirstArgumentsLetsAFileAndABackgroundProcessOutliveTheirScript(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		skipOutsideCI(t, fmt.Sprintf("bash is not on PATH, so no script can run in it: %v", err))
	}
	scripts := []string{`: >"kept-$$"
(for ((i = 0; i < 6000; i++)); do [[ -e begun-$$ ]] && break; sleep 0.01; done; spec-check background; : >"called-$$") &`}
	for range 16 {
		scripts = append(scripts, `if [[ -e kept-$$ && ! -e begun-$$ ]]; then
	spec-check file
	: >"begun-$$"
	for ((i = 0; i < 6000; i++)); do [[ -e called-$$ ]] && break; sleep 0.01; done
fi
spec-check tree`)
	}
	calls := firstArguments(t, bash, scripts)
	if calls[0] != nil {
		t.Errorf("script 0 %q calls %q, want none", scripts[0], calls[0])
	}
	outlived := 0
	for i, got := range calls[1:] {
		switch {
		case slices.Equal(got, []string{"file", "background", "tree"}):
			outlived++
		case !slices.Equal(got, []string{"tree"}):
			t.Errorf("script %d calls %q, want tree, or file, background and tree", i+1, got)
		}
	}
	if outlived != 1 {
		t.Errorf("%d scripts found the file and recorded the call of the background process, want 1", outlived)
	}
}

// A script must not read the records of later scripts from the batch's standard input. The first
// script reads one line and reports compare if it got one. For every possible batch count from one
// to sixteen, a later script in that batch starts with a newline, so a driver that drops </dev/null>
// gives the first script a line. The head gives it EOF and it reports tree.
func TestFirstArgumentsGivesEachScriptEmptyStandardInput(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		skipOutsideCI(t, fmt.Sprintf("bash is not on PATH, so no script can run in it: %v", err))
	}
	scripts := []string{`if IFS= read -r line; then spec-check compare; else spec-check tree; fi`}
	want := [][]string{{"tree"}}
	for range 16 {
		scripts = append(scripts, "\n:")
		want = append(want, nil)
	}
	for i, got := range firstArguments(t, bash, scripts) {
		if !slices.Equal(got, want[i]) {
			t.Errorf("script %d %q calls %q, want %q", i, scripts[i], got, want[i])
		}
	}
}
