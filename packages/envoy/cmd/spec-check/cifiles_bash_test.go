package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/testprereq"
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

// bashScripts is each command of bashNames, bashBetween and bashSubs after each of bashPrefixes,
// and the same commands after each of bashNestedBy inside each of bashWrappings. It leaves out a
// command where a backslash and a line break right after the command's name, with blanks after
// them, meet another backslash and line break in the name or in the subcommand: one reading of the
// rule keeps the blanks and the other removes them, and neither reads both
// (TestCIFilesThatCompareLeavesAFormOutsideTheClaim).
func bashScripts() []string {
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
	var scripts []string
	for _, prefix := range bashPrefixes {
		for _, command := range commands {
			scripts = append(scripts, prefix+command)
		}
	}
	for _, wrap := range bashWrappings {
		for _, prefix := range bashNestedBy {
			for _, command := range commands {
				scripts = append(scripts, wrap(prefix+command))
			}
		}
	}
	return scripts
}

// The test asks Bash itself. Each script of bashScripts runs in bash three ways: as a script, as a
// workflow's run: | block and as a folded run: > value, the value that YAML reads from the file.
// spec-check is a shell function that records its first argument, and a spec-check on PATH and in
// ./bin does the same for a nested sh and for a path. Whenever Bash runs spec-check with compare
// as its first argument, directly or in a nested shell, the guard names the file; a miss fails the
// test. The guard can name more, and the test logs how many files it names where no shell runs
// the comparison.
func TestCIFilesThatCompareNamesEachComparisonThatBashRuns(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	testprereq.Require(t, err == nil, "envoy-go-checks", "bash is not on PATH, so no script can run in it: %v", err)
	began := time.Now()
	var files []bashFile
	for _, script := range bashScripts() {
		files = append(files, bashFile{"script", script, script})
		for _, style := range []string{"|", ">"} {
			raw := workflowSteps + "      - run: " + style + "\n          " + strings.ReplaceAll(script, "\n", "\n          ") + "\n"
			var workflow struct {
				Jobs map[string]struct{ Steps []struct{ Run string } }
			}
			if err := yaml.Unmarshal([]byte(raw), &workflow); err != nil {
				t.Fatalf("YAML cannot read %q: %v", raw, err)
			}
			files = append(files, bashFile{"run: " + style, raw, workflow.Jobs["spec"].Steps[0].Run})
		}
	}
	expectEachComparisonNamed(t, bash, files, began)
}

// bashEarlierParts and bashEarlierForms are the pieces of the scripts that the Bash test of the
// round-6 reader (6823f4702b) built, whose comparisons each head of the text rule names too: a
// part is a text before the command, and a form writes the command, with {w} the subcommand and
// {h} and {t} its two halves. The forms that only that reader named are left out, since they are
// outside the text rule's claim (TestCIFilesThatCompareLeavesAFormOutsideTheClaim): a $ and an
// ANSI-C escape that a line continuation splits, and a redirection or a here-document between the
// two words.
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
)

// bashEarlierScripts is each form of bashEarlierForms after no part and after each part of
// bashEarlierParts, and the last form, a line continuation inside the word, after each sequence of
// two parts and of three parts, each for compare and for tree; then the forms and the sequences of
// two parts again inside each of bashWrappings, as the round-6 test gave them to nested shells.
func bashEarlierScripts() []string {
	var scripts, wrapped []string
	for _, word := range []string{"compare", "tree"} {
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
// lost.
func TestCIFilesThatCompareNamesEachComparisonOfTheEarlierBashCorpus(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	testprereq.Require(t, err == nil, "envoy-go-checks", "bash is not on PATH, so no script can run in it: %v", err)
	began := time.Now()
	var files []bashFile
	for _, script := range bashEarlierScripts() {
		files = append(files, bashFile{"script", script, script})
	}
	expectEachComparisonNamed(t, bash, files, began)
}

// bashFile is one file of a Bash test: its kind, its text as the guard reads it, and the script
// that Bash runs from it.
type bashFile struct{ kind, raw, script string }

// expectEachComparisonNamed runs the script of each of files in bash and fails the test for each
// file where Bash runs the comparison and the guard names no line. It logs how many files the guard
// names where no shell runs the comparison, and it does not pass when Bash ran the comparison in
// none of the files or in all of them.
func expectEachComparisonNamed(t *testing.T, bash string, files []bashFile, began time.Time) {
	t.Helper()
	scripts := make([]string, len(files))
	for n, f := range files {
		scripts[n] = f.script
	}
	calls := firstArguments(t, bash, scripts)
	ran, missed, over := 0, 0, 0
	for n, f := range files {
		named := textHits([]byte(f.raw)) != nil
		switch compares := slices.Contains(calls[n], "compare"); {
		case compares && !named:
			if missed++; missed <= 20 {
				t.Errorf("%s %q: bash runs the comparison (first arguments %q), and the guard names no line", f.kind, f.raw, calls[n])
			}
		case compares:
			ran++
		case named:
			over++
		}
	}
	if ran == 0 || ran == len(files) {
		t.Fatalf("UNKNOWN: bash ran the comparison in %d of %d files, so the files cannot tell a guard that names each file from one that names none", ran+missed, len(files))
	}
	t.Logf("%d files in bash, in %v. Bash ran the comparison in %d; the guard missed %d, and named %d files where no shell ran the comparison",
		len(files), time.Since(began).Round(time.Millisecond), ran+missed, missed, over)
}

// firstArguments runs each of scripts in bash and answers, for each, the first argument of each
// call of spec-check that it makes. A bash that defines spec-check as a function, and exports it to
// a nested bash, reads each script with ., as a CI step's bash reads its run: value, and a
// spec-check on PATH and in ./bin records the calls of a nested sh and of a path. The scripts run
// in batches, one bash for each, which writes before each script's calls a byte 1, the script's
// number and a byte 1, and each call's first argument with a NUL after it.
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
	writeFile(t, driver, []byte("spec-check() { printf '%s\\0' \"$1\" >&3; }\nexport -f spec-check\nfor f; do printf '\\001%s\\001' \"${f##*/}\"; . \"$f\" 3>&1 </dev/null >/dev/null 2>&1; done\nexit 0\n"))
	batches := make([][]string, min(runtime.NumCPU(), 16))
	for n, s := range scripts {
		path := filepath.Join(dir, fmt.Sprint(n))
		writeFile(t, path, []byte(s))
		batches[n%len(batches)] = append(batches[n%len(batches)], path)
	}
	outputs := make([][]byte, len(batches))
	failures := make([]error, len(batches))
	var wg sync.WaitGroup
	for b, batch := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run := exec.Command(bash, append([]string{driver}, batch...)...)
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
