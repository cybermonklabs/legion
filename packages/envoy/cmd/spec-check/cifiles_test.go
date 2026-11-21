package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ciFilesThatCompare names each line of the CI files that names the command that compares the
// files with the approved spec (R2: "No CI job compares the files"). The clean fixture runs
// spec-check tree and print, and says in a comment that no step compares the files: no line is
// named. The planted fixture runs the comparison once from a job step, once from a composite
// action and once from a script: each of the three lines is named.
func TestCIFilesThatCompare(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		fixture string
		want    []string
	}{
		{"clean", nil},
		{"no-workflow", nil},
		{"planted", []string{
			".github/actions/compare/action.yml:9",
			".github/workflows/compare.yaml:13",
			"scripts/compare.sh:4",
		}},
	} {
		t.Run(row.fixture, func(t *testing.T) {
			t.Parallel()
			files := readCIFiles(t, filepath.Join("testdata", "ci-files", row.fixture))
			if got := ciFilesThatCompare(files); !reflect.DeepEqual(got, row.want) {
				t.Errorf("ciFilesThatCompare(%s) = %q, want %q", row.fixture, got, row.want)
			}
		})
	}
}

// The text of each file is read whole, not line by line: a backslash and the line break after it
// go, so the command goes on in the next line, and the line named is the one that holds
// spec-check. The word compare is named only as a whole word, a CRLF file is read as an LF file, a
// line that holds the command twice is named once, and the hits are in path and line order, the
// line as a number.
func TestCIFilesThatCompareReadsEachFileAsText(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"a backslash continues the command on the next line",
			map[string]string{"scripts/a.sh": "set -e\nspec-check \\\n  compare --dir .dispatch/spec --approved a.json\n"},
			[]string{"scripts/a.sh:2"}},
		{"the hit is the line that names spec-check",
			map[string]string{"scripts/a.sh": "env DIR=.dispatch/spec \\\n  spec-check compare --dir \"$DIR\" --approved a.json\n"},
			[]string{"scripts/a.sh:2"}},
		{"a backslash with no space before the next line joins two words",
			map[string]string{"scripts/a.sh": "spec-check\\\ncompare --dir .dispatch/spec\n"},
			nil},
		{"compare is a whole word",
			map[string]string{"scripts/a.sh": "spec-check comparer --dir .dispatch/spec\nspec-check tree --dir compare\nspec-check to compare the files\n"},
			nil},
		{"a CRLF file",
			map[string]string{".github/workflows/a.yaml": "on: pull_request\r\n  run: spec-check compare --dir .dispatch/spec\r\n"},
			[]string{".github/workflows/a.yaml:2"}},
		{"one hit for a line that runs it twice",
			map[string]string{"scripts/a.sh": "spec-check compare --dir a && spec-check compare --dir b\n"},
			[]string{"scripts/a.sh:1"}},
		{"the hits in path and line order",
			map[string]string{
				"scripts/b.sh": "\n" + "spec-check compare --dir a\n" + "\n\n\n\n\n\n\n" + "spec-check compare --dir b\n",
				"scripts/a.sh": "spec-check compare --dir a\n",
			},
			[]string{"scripts/a.sh:1", "scripts/b.sh:2", "scripts/b.sh:10"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			expectFiles(t, row.files, row.want)
		})
	}
}

// workflowSteps is the head of a workflow whose one job's steps follow it, from line 4 on.
const workflowSteps = "jobs:\n  spec:\n    steps:\n"

// A quote character and each backslash are ignored, and so is a line break between the two words,
// in any file: a quoted or an escaped subcommand, a plain, quoted or folded YAML value over two
// lines, a literal block, an anchor that an alias reads, a comment, a file that YAML cannot read,
// and a value under another key than run: are each named at the line that holds spec-check. A
// line that ends in two backslashes is named too, though Bash ends the command there.
func TestCIFilesThatCompareIgnoresQuotesBackslashesAndLineBreaks(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"a subcommand in double quotes",
			map[string]string{".github/workflows/a.yaml": workflowSteps + "      - run: spec-check \"compare\" --dir .dispatch/spec\n"},
			[]string{".github/workflows/a.yaml:4"}},
		{"a subcommand in single quotes",
			map[string]string{"scripts/a.sh": "spec-check 'compare' --dir .dispatch/spec\n"},
			[]string{"scripts/a.sh:1"}},
		{"quotes inside the word and an escaped letter",
			map[string]string{"scripts/a.sh": "spec-check com\"\"pare --dir a\nspec-check \\compare --dir b\n"},
			[]string{"scripts/a.sh:1", "scripts/a.sh:2"}},
		{"a line that ends in two backslashes is named too",
			map[string]string{"scripts/a.sh": "spec-check\\\\\n  compare --dir .dispatch/spec\n"},
			[]string{"scripts/a.sh:1"}},
		{"a plain scalar over two lines",
			map[string]string{".github/workflows/a.yaml": workflowSteps + "      - run: spec-check\n          compare --dir .dispatch/spec\n"},
			[]string{".github/workflows/a.yaml:4"}},
		{"a double-quoted scalar over two lines",
			map[string]string{".github/workflows/a.yaml": workflowSteps + "      - run: \"spec-check\n          compare --dir .dispatch/spec\"\n"},
			[]string{".github/workflows/a.yaml:4"}},
		{"a folded scalar over two lines, named where spec-check stands",
			map[string]string{".github/actions/a/action.yml": "runs:\n  using: composite\n  steps:\n    - shell: bash\n      run: >-\n        spec-check\n        compare --dir .dispatch/spec\n"},
			[]string{".github/actions/a/action.yml:6"}},
		{"a literal block keeps its lines",
			map[string]string{".github/workflows/a.yaml": workflowSteps + "      - run: |\n          echo start\n          spec-check \\\n            compare --dir .dispatch/spec\n"},
			[]string{".github/workflows/a.yaml:6"}},
		{"an anchor that an alias reads is named where spec-check stands",
			map[string]string{".github/workflows/a.yaml": "x-command: &compare >-\n  spec-check\n  compare --dir .dispatch/spec\n" + workflowSteps + "      - run: *compare\n"},
			[]string{".github/workflows/a.yaml:2"}},
		{"a comment that names the command is named",
			map[string]string{".github/workflows/a.yaml": workflowSteps + "      # spec-check compare is the work of Dispatch, so no step here runs it.\n      - run: spec-check tree --dir .dispatch/spec\n"},
			[]string{".github/workflows/a.yaml:4"}},
		{"a line is named once",
			map[string]string{".github/workflows/a.yaml": workflowSteps + "      - run: spec-check compare --dir .dispatch/spec\n"},
			[]string{".github/workflows/a.yaml:4"}},
		{"a YAML file that does not parse is read as text",
			map[string]string{".github/workflows/a.yaml": "on: [pull_request\n  run: spec-check compare --dir .dispatch/spec\n"},
			[]string{".github/workflows/a.yaml:2"}},
		{"a run value over two lines in a document before one that YAML cannot read",
			map[string]string{".github/workflows/a.yaml": workflowSteps + "      - run: spec-check\n          compare --dir .dispatch/spec\n---\non: [pull_request\n"},
			[]string{".github/workflows/a.yaml:4"}},
		{"a value under another key is named too",
			map[string]string{".github/workflows/a.yaml": workflowSteps + "      - name: >-\n          spec-check\n          compare\n        run: spec-check tree --dir .dispatch/spec\n"},
			[]string{".github/workflows/a.yaml:5"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			expectFiles(t, row.files, row.want)
		})
	}
}

// A $ right before a quote is ignored with the quote, so spec-check $'compare' and $"compare" are
// named, as the plain form is, behind env -u too. A subcommand that an ANSI-C escape builds is not
// found: the refusal of compare stops it in a step that keeps GITHUB_ACTIONS. A word that holds
// more than compare is not the word compare.
func TestCIFilesThatCompareIgnoresADollarBeforeAQuote(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		line string
		hit  bool
	}{
		{"the plain form", "spec-check compare --dir .dispatch/spec --approved a.json", true},
		{"an ANSI-C quoted subcommand", "spec-check $'compare' --dir .dispatch/spec --approved a.json", true},
		{"a locale-quoted subcommand", `spec-check $"compare" --dir .dispatch/spec`, true},
		{"an escaped dollar before a quote is named too", `spec-check \$'compare' --dir a`, true},
		{"a subcommand that a hex escape builds, behind env -u",
			`env -u GITHUB_ACTIONS /usr/local/bin/spec-check $'com\x70are' --dir .dispatch/spec --approved a.json`, false},
		{"a command and a subcommand that octal escapes build", `$'spec\055check' $'\143ompare' --dir a`, false},
		{"an ANSI-C quoted other subcommand", "spec-check $'tree' --dir .dispatch/spec", false},
		{"an ANSI-C quoted word with a tab in it", `spec-check $'com\tpare' --dir a`, false},
		{"an ANSI-C quoted word longer than compare", `spec-check $'compare\x64' --dir a`, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var want []string
			if row.hit {
				want = []string{".github/workflows/a.yaml:4", "scripts/a.sh:1"}
			}
			expectFiles(t, map[string]string{
				"scripts/a.sh":             row.line + "\n",
				".github/workflows/a.yaml": workflowSteps + "      - run: " + row.line + "\n",
			}, want)
		})
	}
}

// A backslash and the line break after it are ignored, with the indentation that starts the next
// line: so a word that a run: | block splits is one word again, and so is a word that a script
// splits. The text is also read with the indentation kept, which a shell reads as a break between
// two words. A $ and its quote that they split are named, and a quoted word that they split is
// named too, though Bash keeps the backslash and the line break inside '…' and $'…'. A subcommand
// that an ANSI-C escape builds is not found. Each row is checked as a script and as a literal
// block.
func TestCIFilesThatCompareJoinsALineThatABackslashEnds(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, text string
		named      bool
	}{
		{"between $ and a locale quote", "spec-check $\\\n\"compare\" --dir a", true},
		{"inside a word", "spec-check com\\\npare --dir a", true},
		{"two times inside a word", "spec-check c\\\no\\\nmpare --dir a", true},
		{"between two words", "spec-check \\\ncompare --dir a", true},
		{"right after the command's name, with indentation after it", "spec-check\\\n  compare --dir a", true},
		{"inside double quotes", "spec-check \"com\\\npare\" --dir a", true},
		{"after a single quote inside double quotes", "env X=\"it's\" spec-check com\\\npare --dir a", true},
		{"inside single quotes, which no shell runs as compare", "spec-check 'com\\\npare' --dir a", true},
		{"inside an ANSI-C quote, which no shell runs as compare", "spec-check $'com\\\npare' --dir a", true},
		{"between $ and an ANSI-C quote that a hex escape builds", "spec-check $\\\n'com\\x70are' --dir a", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var at []int
			if row.named {
				at = []int{1}
			}
			expectScriptAndBlock(t, row.text, at)
		})
	}
}

// The inputs that found a fault in the shell readers of earlier rounds, each with the line, from
// 1, of each comparison that Bash runs. The audit's F3 pair hides a split comparison behind a
// double quote in a comment or a here-document, F4 opens two subshells with ((, and the review's
// forms put a brace in ${…} and a space between the two closing parentheses. A quote that a comment,
// a here-document body or a quoted word leaves open, and one that an earlier line opens, changes
// nothing for a rule that reads text. The same comment or here-document before spec-check tree is
// not named.
func TestCIFilesThatCompareNamesTheInputsOfEarlierRounds(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, text string
		at         []int
	}{
		{"F3: a double quote in a comment that a backslash ends",
			"# \" \\\n: \"it's\"; env -u GITHUB_ACTIONS spec-check com\\\npare --dir /spec --approved /approved.json", []int{2}},
		{"F3: a double quote in a here-document whose quoted delimiter ends in a backslash",
			"cat <<'END\\'\n\" \\\nEND\\\n: \"it's\"; env -u GITHUB_ACTIONS spec-check com\\\npare --dir /spec --approved /approved.json", []int{4}},
		{"the plain comparison", "spec-check compare --dir /spec --approved /approved.json", []int{1}},
		{"the same comment, then spec-check tree", "# \" \\\n: \"it's\"; env -u GITHUB_ACTIONS spec-check tree --dir /spec", nil},
		{"the same here-document, then spec-check tree", "cat <<'END\\'\n\" \\\nEND\\\n: \"it's\"; env -u GITHUB_ACTIONS spec-check tree --dir /spec", nil},
		{"F4: (( that opens two subshells", "((env -u GITHUB_ACTIONS spec-check compare</dev/null --dir /spec --approved /approved.json); :)", []int{1}},
		{"two subshells with a space between their closing parentheses", "((spec-check compare --dir a) )", []int{1}},
		{"a brace in ${…}", ": ${x:-{}; spec-check compare --dir a", []int{1}},
		{"F8: a single quote that an earlier line opens and the split line closes",
			"echo 'start\nend' && spec-check comp\\\nare --dir .dispatch/spec --approved a.json", []int{2}},
		{"Q1: a single quote that an earlier line opens, then a continuation between the words",
			"X='\n' spec-check \\\ncompare --dir a", []int{2}},
		{"Q1: a nested shell with a continuation inside single quotes", "bash -c 'spec-check \\\ncompare --dir a'", []int{1}},
		{"Q1: a nested shell behind env -i", "env -i PATH=\"$PATH\" sh -c 'spec-check \\\ncompare --dir a'", []int{1}},
		{"a quote that a comment leaves open changes nothing after it", "# it's\nspec-check com\\\npare --dir a", []int{2}},
		{"a quote that a here-document body leaves open changes nothing after it",
			"cat <<'END'\nit's\nEND\nspec-check com\\\npare --dir a", []int{4}},
		{"a quote that a quoted word leaves open changes nothing after it", ": \"it's open\"; spec-check com\\\npare --dir a", []int{1}},
		{"a here-document that a quoted word leaves open changes nothing after it",
			": \"cat <<END\"\nspec-check com\\\npare --dir a\nEND", []int{2}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			expectScriptAndBlock(t, row.text, row.at)
		})
	}
}

// The rule names a comment or a quoted text that no shell runs, an escaped space before or after
// compare, and a backslash at the end of the line after compare: Bash keeps the space in its word
// and joins the next line onto compare, and the writer of such a line rewords it.
func TestCIFilesThatCompareNamesACommentOrAQuotedTextThatNoShellRuns(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, text string
		at         []int
	}{
		{"a quoted text", "echo 'spec-check compare --dir a'", []int{1}},
		{"a quoted text that tells the reader what to run", "echo 'run spec-check compare by hand'", []int{1}},
		{"a here-document body", "cat <<END\nspec-check compare --dir a\nEND", []int{2}},
		{"a comment", "# spec-check compare is the work of Dispatch", []int{1}},
		{"an escaped space before compare, which Bash keeps in the word", "spec-check \\ compare --dir a", []int{1}},
		{"an escaped space after compare, which Bash keeps in the word", "spec-check compare\\ --dir a", []int{1}},
		{"a backslash at the end of the line after compare, which joins the next line onto it", "spec-check compare\\\n--dir a", []int{1}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			expectScriptAndBlock(t, row.text, row.at)
		})
	}
}

// A script in another language runs the command with a list of its arguments, so a comma, ( or [
// stands between the two words once the quotes go. Node's execFileSync and spawnSync, and a Python
// list with no space after its comma, are named, as Bun's spawn, a Python list with spaces and Go's
// exec.Command are. A builder that adds the argument by a method call is named too: the word of the
// command's name runs on through ).arg, and the ( after it stands before compare. So is a Python
// or a JavaScript text that split() makes the list, also behind sudo -n: the quote after compare
// ends the word, and the . after the quote makes no other word of it.
func TestCIFilesThatCompareNamesAListOfArguments(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ name, path, text string }{
		{"Node's execFileSync", "scripts/a.ts", `execFileSync("spec-check", ["compare", "--dir", dir, "--approved", approved]);`},
		{"Node's spawnSync", "scripts/a.ts", `spawnSync('spec-check', ['compare', '--dir', dir]);`},
		{"a Python list with no space after its comma", ".github/scripts/a.py", `subprocess.run(["spec-check","compare","--dir",d])`},
		{"Bun's spawn", "scripts/a.ts", `Bun.spawn(["spec-check", "compare", "--dir", dir]);`},
		{"a Python list with spaces", ".github/scripts/a.py", `subprocess.run(["spec-check", "compare", "--dir", d], check=True)`},
		{"Go's exec.Command", "scripts/a.go", `exec.Command("spec-check", "compare", "--dir", d)`},
		{"a builder that adds the argument by a method call", "scripts/a.rs", `Command::new("spec-check").arg("compare").arg("--dir").status()`},
		{"a Python text that split() makes the list", ".github/scripts/a.py", `cmd = "spec-check compare".split() + ["--dir", d, "--approved", a]`},
		{"a Python text in single quotes that split() makes the list", ".github/scripts/a.py", `subprocess.run('spec-check compare'.split() + args)`},
		{"a Python text that split() makes the list, behind sudo -n", ".github/scripts/c.py", `subprocess.run(["sudo", "-n"] + "spec-check compare".split() + ["--dir", d], check=True)`},
		{"a JavaScript text that split(' ') makes the list", "scripts/a.ts", `cmd = 'spec-check compare'.split(' ').concat(['--dir', 'a'])`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			expectFiles(t, map[string]string{row.path: "\n" + row.text + "\n"}, []string{row.path + ":2"})
		})
	}
}

// wordsThatOnlyHoldTheName is each line whose word only holds the command's name, as a job, a key,
// a tool, a possessive with the ASCII apostrophe or a plural does, before compare: none names the
// command.
var wordsThatOnlyHoldTheName = []string{
	"needs: [envoy-spec-check, compare-baselines]",
	"myspec-check compare --dir a",
	"spec-check's compare step",
	"spec-checker compare",
	"key: spec-check-linux compare",
	"the spec-checks compare",
	"spec-check-compare compare",
}

// The command's name is a word of its own: no letter, digit, _ or - stands right before spec-check
// in the file, and a suffix right after it starts with another character, such as .exe, @v1 or
// ).arg(. So no line of wordsThatOnlyHoldTheName is named: the ASCII apostrophe of spec-check's is a
// quote character, which the rule ignores, so the s stands right after the name. A possessive with
// U+2019 is a suffix that keeps the name, so that line is named. A path keeps the name, also under a
// directory whose name holds it, where spec-checker is still another word, and so does a line that
// a backslash and a line break join onto a here-document's delimiter, since the file has a line
// break right before spec-check.
func TestCIFilesThatCompareReadsTheCommandsNameAsAWordOfItsOwn(t *testing.T) {
	t.Parallel()
	type row struct {
		text string
		at   []int
	}
	rows := []row{
		{"spec-check.exe compare --dir a", []int{1}},
		{"go run example.com/envoy/cmd/spec-check@v1 compare --dir a", []int{1}},
		{"$RUNNER_TEMP/envoy-spec-check/spec-check compare --dir a", []int{1}},
		{"go build -o x-spec-check/spec-check ./cmd/spec-check && x-spec-check/spec-check compare --dir a", []int{1}},
		{": <<'END\\'\n\" \\\nEND\\\nspec-check compare --dir a", []int{4}},
		{"$RUNNER_TEMP/envoy-spec-check/spec-checker compare --dir a", nil},
		{"the spec-check\u2019s compare step", []int{1}},
	}
	for _, text := range wordsThatOnlyHoldTheName {
		rows = append(rows, row{text, nil})
	}
	for _, row := range rows {
		t.Run(row.text, func(t *testing.T) {
			t.Parallel()
			expectScriptAndBlock(t, row.text, row.at)
		})
	}
}

// A double-quoted YAML value, and a string of JavaScript or Python, turn \n, \t and \r into a line
// break, a tab and a carriage return, and an escaped space or quote into that byte. The two bounds
// read the file's own bytes. After a backslash, n, t and r stand for white space, so the comparison
// right after \n, \t or \r is named, and so is compare right before one. After compare, the byte
// after a backslash takes its place, so an escaped space or quote ends the word too. Bash reads a
// line break and a tab as white space and a carriage return as a byte of its word, so for Bash a
// \r beside the two words names a line that runs no comparison (spec-check gets compare\r--dir, and
// set -e\rspec-check fails at set); PowerShell reads a lone carriage return as a line break (its
// language specification, 2.2.2 Line terminators), so there the line compares. Any other letter,
// digit, _ or - after a backslash is a byte of a word before the name, so a name right after \7,
// \- or \_, or in a Windows directory such as C:\bin\2spec-check, is no command that a shell runs. A
// backslash right before the name is no byte of a word, so a Windows path to the command is named.
// The escapes outside the claim, \0 before the name among them, are in
// TestCIFilesThatCompareLeavesAFormOutsideTheClaim.
func TestCIFilesThatCompareReadsAnEscapeAsTheEndOfAWord(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, path, text string
		named            bool
	}{
		{"a double-quoted run: value with \\n before the name", ".github/workflows/a.yaml", `      - run: "set -e\nspec-check compare --dir .dispatch/spec --approved a.json"`, true},
		{"a double-quoted run: value with \\t before the name", ".github/workflows/a.yaml", `      - run: "set -e;\tspec-check compare --dir a"`, true},
		{"a double-quoted run: value with \\r before the name, a line break for PowerShell", ".github/workflows/a.yaml", `      - run: "set -e\rspec-check compare --dir a"`, true},
		{"a double-quoted run: value with an escaped space before the name", ".github/workflows/a.yaml", `      - run: "set -e;\ spec-check compare --dir a"`, true},
		{"execSync with \\n before the name", "scripts/a.ts", `execSync("set -e\nspec-check compare --dir a");`, true},
		{"a double-quoted run: value with \\n after compare", ".github/workflows/a.yaml", `      - run: "spec-check compare\n--dir .dispatch/spec --approved a.json"`, true},
		{"execSync with \\n after compare", "scripts/a.ts", `execSync("spec-check compare\n");`, true},
		{"a double-quoted run: value with \\t after compare", ".github/workflows/a.yaml", `      - run: "spec-check compare\t--dir a"`, true},
		{"a double-quoted run: value with \\r after compare, a line break for PowerShell", ".github/workflows/a.yaml", `      - run: "spec-check compare\r--dir a"`, true},
		{"a double-quoted run: value with an escaped space after compare, behind env -u", ".github/workflows/a.yaml", `      - run: "env -u GITHUB_ACTIONS spec-check compare\ --dir .dispatch/spec --approved approved.json"`, true},
		{"a double-quoted run: value with an escaped quote after compare", ".github/workflows/a.yaml", `      - run: "bash -c \"spec-check compare\""`, true},
		{"no command: \\7 before the name", "scripts/a.sh", `bash -c $'true;\7spec-check compare --dir a'`, false},
		{"no command: \\- before the name", "scripts/a.sh", `\-spec-check compare --dir a`, false},
		{"no command: \\_ before the name", "scripts/a.ts", `execSync("\_spec-check compare");`, false},
		{"no command: a Windows directory 2spec-check", "scripts/a.cmd", `C:\bin\2spec-check compare --dir a`, false},
		{"no command: a Windows directory bspec-check", "scripts/a.cmd", `C:\a\bspec-check compare --dir a`, false},
		{"a Windows path to the command", "scripts/a.cmd", `C:\tools\spec-check.exe compare --dir a`, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			content, line := row.text+"\n", 1
			if strings.HasPrefix(row.path, ".github/workflows/") {
				content, line = workflowSteps+content, 4
			}
			var want []string
			if row.named {
				want = []string{fmt.Sprintf("%s:%d", row.path, line)}
			}
			expectFiles(t, map[string]string{row.path: content}, want)
		})
	}
}

// wordsThatOnlyHoldCompare is each line whose word after the command's name only holds compare, as
// a job, another subcommand, a script or a path does, or a word that goes on after a quote that
// ends compare, which Bash reads as compared or compare"d: none names the command.
var wordsThatOnlyHoldCompare = []string{
	"needs: [spec-check, compare-baselines]",
	"spec-check compare-all",
	"paths: ['cmd/spec-check', 'compare/**']",
	"spec-check compare.sh",
	`spec-check "compare.sh"`,
	`spec-check compare\.sh`,
	`spec-check "compare"d`,
	`spec-check 'compare'd`,
	`spec-check compare\"d`,
}

// The word compare is a word of its own too: a letter, a digit, _, -, . or / right after it makes
// it another word or a path, also right after a backslash, so no line of wordsThatOnlyHoldCompare
// is named. Each other byte ends the word, so the comparison at the end of a line or of the file,
// and before a space, a quote, ), ;, | or &, is named.
func TestCIFilesThatCompareReadsCompareAsAWordOfItsOwn(t *testing.T) {
	t.Parallel()
	type row struct {
		text string
		at   []int
	}
	rows := []row{
		{"spec-check compare", []int{1}},
		{"spec-check compare --dir a", []int{1}},
		{`bash -c "spec-check compare"`, []int{1}},
		{"(spec-check compare)", []int{1}},
		{"spec-check compare; echo done", []int{1}},
		{"spec-check compare| tee out", []int{1}},
		{"spec-check compare& wait", []int{1}},
	}
	for _, text := range wordsThatOnlyHoldCompare {
		rows = append(rows, row{text, nil})
	}
	for _, row := range rows {
		t.Run(row.text, func(t *testing.T) {
			t.Parallel()
			expectScriptAndBlock(t, row.text, row.at)
		})
	}
	t.Run("at the end of the file", func(t *testing.T) {
		t.Parallel()
		expectFiles(t, map[string]string{"scripts/a.sh": "spec-check compare"}, []string{"scripts/a.sh:1"})
	})
}

// A folded or a plain run: value turns a line break into a space before Bash reads it, so a
// backslash at the end of the line escapes that space: Bash runs spec-check " compare", which is
// no comparison. The rule ignores the backslash and the line break, and names the line that holds
// spec-check. A literal block keeps its lines, and there Bash runs the comparison.
func TestCIFilesThatCompareNamesAFoldedValueThatABackslashSplits(t *testing.T) {
	t.Parallel()
	steps := "on: pull_request\njobs:\n  spec:\n    runs-on: ubuntu-latest\n    steps:\n"
	for _, row := range []struct {
		name, workflow string
		want           []string
	}{
		{"a folded value", steps + "      - run: >\n          spec-check \\\n          compare --dir .dispatch/spec --approved a.json\n",
			[]string{".github/workflows/a.yaml:7"}},
		{"a plain value", steps + "      - run: spec-check \\\n          compare --dir .dispatch/spec --approved a.json\n",
			[]string{".github/workflows/a.yaml:6"}},
		{"a literal block, which is the comparison", steps + "      - run: |\n          spec-check \\\n            compare --dir .dispatch/spec --approved a.json\n",
			[]string{".github/workflows/a.yaml:7"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			expectFiles(t, map[string]string{".github/workflows/a.yaml": row.workflow}, row.want)
		})
	}
}

// The forms outside the claim, each a stated limit of the rule. The rule reads the file's bytes and
// runs no shell expansion, so it does not find a command that an expansion, an escape or an alias
// builds, one with another word between the two, such as a redirection, a here-document or an
// image (docker run --entrypoint spec-check <image> compare), or a run: | block whose two line
// continuations need the indentation after one kept and after the other removed. Before the name,
// only \n, \t and \r of the escapes stand for white space; each other escape or expansion of one
// language that builds the boundary is outside the claim (\x20, \x0a, \012, \u000a, PowerShell's
// `n, a batch file's %~dp0, $1), and so are \0 right before the name or right after compare, a NUL
// byte that Bash skips or stops at, \t between the two words, and a letter, a digit or _ right
// after the quote that ends compare, which Bash reads as more of the word and another language can
// read as its next token, such as Python's f or r prefix ("compare"f" --dir"). Each runs the
// comparison in Bash, or in Python where its row says so, with x set to spec-check and sub to
// compare, and the glob where a file named compare is.
// The refusal of compare stops each in a step that keeps GITHUB_ACTIONS; a container that docker
// run starts without -e GITHUB_ACTIONS, as the --entrypoint form and a block list of a command do,
// passes both. A command substitution that writes spec-check right before ) is named, since the
// text holds the two words. In a run: | block, a backslash and a line break right after the
// command's name, with more indentation after them, and a second pair inside the subcommand are not
// found: the first pair needs the blanks that start the next line, the second needs them gone, and
// each reading of the rule keeps them all or removes them all. The same text in a script is named,
// since there no indentation follows the second pair.
func TestCIFilesThatCompareLeavesAFormOutsideTheClaim(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		text, why string
		named     bool
	}{
		{"$x compare --dir a", "a variable holds the command's name", false},
		{"${SPEC:-spec-check} compare --dir a", "an expansion with a default builds the command's name, and the - before it makes it part of another word", false},
		{"spec-check $sub --dir a", "a variable holds the subcommand", false},
		{"spec-check $'com\\x70are' --dir a", "an escape builds the subcommand", false},
		{"$'spec\\055check' compare --dir a", "an escape builds the command's name", false},
		{"spec-check {compare,} --dir a", "a brace expansion builds the subcommand", false},
		{"spec-check >out compare --dir a", "a redirection stands between the two words", false},
		{"spec-check 2>/dev/null compare --dir a", "a redirection stands between the two words", false},
		{"spec-check {fd}>/dev/null compare --dir a", "a redirection to a named descriptor stands between the two words", false},
		{"spec-check <<END compare --dir a\nx\nEND", "a here-document stands between the two words", false},
		{"spec-check $(echo compare) --dir a", "a command substitution builds the subcommand", false},
		{"spec-check `echo compare` --dir a", "a command substitution in backticks builds the subcommand", false},
		{"spec-check compar[e] --dir a", "a glob builds the subcommand", false},
		{"spec-check compar{e..e} --dir a", "a brace sequence builds the subcommand", false},
		{"printf -v sub %s compare && spec-check \"$sub\" --dir a", "printf -v writes the subcommand into a variable", false},
		{"shopt -s expand_aliases\nalias sc=spec-check\nsc compare --dir a", "an alias stands for the command's name", false},
		{"docker run --rm --entrypoint spec-check debian:bookworm-slim compare --dir a", "the image stands between the command's name and compare", false},
		{"$(echo spec-check) compare --dir a", "a command substitution builds the command's name, and the text holds spec-check) compare", true},
	} {
		t.Run(row.why+": "+row.text, func(t *testing.T) {
			t.Parallel()
			var at []int
			if row.named {
				at = []int{1}
			}
			expectScriptAndBlock(t, row.text, at)
		})
	}
	// An escape or an expansion of one language that builds the boundary of a word or the
	// subcommand, a byte Bash skips or stops at, or a byte that one language reads as its next
	// token: the same bytes are another program's name in another language, so each is outside the
	// claim, as the earlier heads that named some of them read it.
	for _, row := range []struct{ name, path, text string }{
		{"\\x20 after compare", ".github/workflows/a.yaml", `      - run: "spec-check compare\x20--dir a"`},
		{"\\x20 before the name", ".github/workflows/a.yaml", `      - run: "set -e;\x20spec-check compare --dir a"`},
		{"\\x20 before the name in an ANSI-C quote", "scripts/a.sh", `bash -c $'true;\x20spec-check compare'`},
		{"\\012 before the name", "scripts/a.sh", `bash -c $'true;\012spec-check compare --dir a'`},
		{"\\x0a before the name", "scripts/a.sh", `bash -c $'true;\x0aspec-check compare --dir a'`},
		{"\\u000a before the name", "scripts/a.ts", `execSync("set -e\u000aspec-check compare --dir a");`},
		{"\\u000a before the name in an ANSI-C quote", "scripts/a.sh", `bash -c $'set -e\u000aspec-check compare'`},
		{"PowerShell's `n before the name", "scripts/a.ps1", "Invoke-Expression \"Set-Location a`nspec-check compare --dir a\""},
		{"PowerShell's `n after cd", "scripts/a.ps1", "Invoke-Expression \"cd .`nspec-check compare\""},
		{"a batch file's %~dp0 before the name", "scripts/a.cmd", `%~dp0spec-check compare --dir a`},
		{"$1 before the name", "scripts/a.sh", `$1spec-check compare --dir a`},
		{"\\0 before the name, whose NUL byte Bash skips", ".github/workflows/a.yaml", `      - run: "set -e\n\0spec-check compare --dir a"`},
		{"\\0 before the name in the text that printf writes for Bash", "scripts/a.sh", `printf 'set -e\n\0spec-check compare --dir a\n' | bash`},
		{"\\0 right after compare in the text that printf writes for Bash, whose NUL byte Bash skips", "scripts/a.sh", `printf 'spec-check compare\0 --dir a\n' | bash`},
		{"\\0 right after compare in an ANSI-C quote, where the text that bash -c runs stops", "scripts/a.sh", `bash -c $'spec-check compare\0 --dir a'`},
		{"\\t between the two words", ".github/workflows/a.yaml", `      - run: "spec-check\tcompare --dir a"`},
		{"\\t between the two words in execSync", "scripts/a.ts", `execSync("spec-check\tcompare --dir a");`},
		{"\\t between the two words in an ANSI-C quote", "scripts/a.sh", `bash -c $'spec-check\tcompare --dir a'`},
		{"Python's f prefix right after the quote that ends compare, which Python runs as spec-check compare --dir a", ".github/scripts/a.py", `subprocess.run("spec-check compare"f" --dir a", shell=True)`},
		{"Python's r prefix right after the quote that ends compare, which Python runs as spec-check compare --dir a", ".github/scripts/a.py", `subprocess.run('spec-check compare'r' --dir a', shell=True)`},
		{"Python's r prefix right after the quote that ends compare, in a list that split() makes, behind env -u", ".github/scripts/a.py", `subprocess.run("env -u GITHUB_ACTIONS spec-check compare"r" --dir a --approved b".split(), check=True)`},
		{"a YAML \\x63 that builds the subcommand", ".github/workflows/a.yaml", `      - run: "spec-check \x63ompare --dir .dispatch/spec --approved a.json"`},
		{"a YAML \\u0065 that builds the subcommand", ".github/workflows/a.yaml", `      - run: "spec-check compar\u0065 --dir .dispatch/spec --approved a.json"`},
	} {
		t.Run("a form of one language: "+row.name, func(t *testing.T) {
			t.Parallel()
			content := row.text + "\n"
			if strings.HasPrefix(row.path, ".github/workflows/") {
				content = workflowSteps + content
			}
			expectFiles(t, map[string]string{row.path: content}, nil)
		})
	}
	t.Run("two continuations in a run: | block that need the indentation kept and removed", func(t *testing.T) {
		t.Parallel()
		expectFiles(t, map[string]string{
			"scripts/a.sh":             "spec-check\\\n  com\\\npare --dir a\n",
			".github/workflows/a.yaml": workflowSteps + "      - run: |\n          spec-check\\\n            com\\\n          pare --dir a\n",
		}, []string{"scripts/a.sh:1"})
	})
	// A container action whose entrypoint is the command and whose args hold compare: a YAML key
	// stands between the two words.
	t.Run("a container action's entrypoint and args", func(t *testing.T) {
		t.Parallel()
		expectFiles(t, map[string]string{
			".github/actions/a/action.yml": "runs:\n  using: docker\n  image: Dockerfile\n  entrypoint: spec-check\n  args:\n    - compare\n    - --dir\n    - .dispatch/spec\n",
		}, nil)
	})
	// A block list of a command, as a compose file writes it: a line break and a - stand between the
	// two words.
	t.Run("a block list of a command", func(t *testing.T) {
		t.Parallel()
		expectFiles(t, map[string]string{
			"scripts/compose.yaml": "services:\n  spec:\n    image: debian:bookworm-slim\n    command:\n      - spec-check\n      - compare\n      - --dir\n      - .dispatch/spec\n",
		}, nil)
	})
	// A binary built under another name is not the command's name.
	t.Run("a binary built under another name", func(t *testing.T) {
		t.Parallel()
		expectScriptAndBlock(t, "go build -o /tmp/sc ./cmd/spec-check && /tmp/sc compare --dir a", nil)
	})
}

// limitPlaces is each text that states the limits of the guard's rule: the doc of
// ciFilesThatCompare, the package doc, the doc of TestCIFilesThatCompareLeavesAFormOutsideTheClaim,
// and the changelog fragment of #4183, which stays where it is: no release moves or deletes a
// fragment (.github/scripts/release-push.sh, the notes of fold-notes and of group_commits).
var limitPlaces = []string{
	"cifiles.go",
	"main.go",
	"cifiles_test.go",
	filepath.Join("..", "..", ".changes", "unreleased", "pr4183-Added-20261116-091606.yaml"),
}

// limitSentences is the start of each sentence that states the limits of the rule.
var limitSentences = []string{
	"The rule reads the file's bytes and runs no shell expansion,",
	"Before the name, only ",
}

// The sentences that state the limits of the rule are one text in each place that states them
// (limitPlaces): each place holds each sentence once, in the same words, read without the comment
// markers of Go, the code spans of Markdown and the doubled quote of YAML (limitText).
func TestTheLimitsOfTheRuleAreOneTextInEachPlace(t *testing.T) {
	t.Parallel()
	texts := make([]string, len(limitPlaces))
	for i, place := range limitPlaces {
		content, err := os.ReadFile(place)
		if err != nil {
			t.Fatal(err)
		}
		texts[i] = limitText(place, string(content))
	}
	for _, start := range limitSentences {
		var first, firstPlace string
		for i, place := range limitPlaces {
			found := sentencesFrom(texts[i], start)
			switch {
			case len(found) != 1:
				t.Errorf("%s holds %d sentences that start %q, want 1: %q", place, len(found), start, found)
			case firstPlace == "":
				first, firstPlace = found[0], place
			case found[0] != first:
				t.Errorf("the sentence that starts %q differs:\n%s: %s\n%s: %s", start, firstPlace, first, place, found[0])
			}
		}
	}
}

// limitText is the prose of a place as one line: in a Go file its comment lines alone, each without
// its //, and in another file each line, with the blanks that start it gone; then without the code
// spans of Markdown and, in a YAML file, with a doubled quote read as one.
func limitText(place, content string) string {
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(place, ".go") {
			comment, ok := strings.CutPrefix(line, "//")
			if !ok {
				continue
			}
			line = strings.TrimPrefix(comment, " ")
		}
		lines = append(lines, line)
	}
	text := strings.Join(lines, " ")
	if strings.HasSuffix(place, ".yaml") {
		text = strings.ReplaceAll(text, "''", "'")
	}
	return strings.NewReplacer("`` ", "", " ``", "", "`", "").Replace(text)
}

// sentencesFrom is each sentence of text that starts with start, up to its full stop, or up to the
// end of text for one that no full stop and blank end, such as a sentence at the end of a fragment's
// body, before its closing quote.
func sentencesFrom(text, start string) []string {
	var found []string
	for rest := text + " "; ; {
		i := strings.Index(rest, start)
		if i < 0 {
			return found
		}
		rest = rest[i:]
		end := strings.Index(rest, ". ") + 1
		if end == 0 {
			end = len(rest)
		}
		found, rest = append(found, rest[:end]), rest[end:]
	}
}

// sentencesFrom reads a sentence that no ". " follows: the first sentence of the limits, as #4183's
// changelog fragment holds it, moved to the end of a fragment's body, before the quote that closes
// the body. It reads the sentence up to the end of the text, so the test of the places names the
// place whose sentence differs, where it read no sentence there and looped for ever.
func TestSentencesFromReadsASentenceThatNoFullStopAndBlankEnd(t *testing.T) {
	t.Parallel()
	place := limitPlaces[len(limitPlaces)-1]
	content, err := os.ReadFile(place)
	if err != nil {
		t.Fatal(err)
	}
	held := sentencesFrom(limitText(place, string(content)), limitSentences[0])
	if len(held) != 1 {
		t.Fatalf("%s holds %d sentences that start %q, want 1", place, len(held), limitSentences[0])
	}
	moved := "kind: Added\nbody: 'A guard. " + strings.ReplaceAll(held[0], "'", "''") + "'\n"
	if got, want := sentencesFrom(limitText("a.yaml", moved), limitSentences[0]), []string{held[0] + "'  "}; !slices.Equal(got, want) {
		t.Errorf("the sentence at the end of the body: %q, want %q", got, want)
	}
}

// namedCalls is each calling line that a head of pull request #4183 named and that runs the
// comparison, as a test row, a review, a red team or a tester wrote it: on each line, the lines
// that the guard names, a tab, and the text as a Go string literal.
const namedCalls = "testdata/ci-files/named-calls.txt"

// Each calling line that a head of this pull request named, and that runs the comparison, is named
// at its lines (namedCalls), so a change of a bound that loses one fails here with the file's line.
// The scripts that the Bash tests generate are not in the file: those tests run them.
func TestCIFilesThatCompareNamesEachCallThatAHeadNamed(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(namedCalls)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for n, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		lines, quoted, _ := strings.Cut(line, "\t")
		text, err := strconv.Unquote(quoted)
		if err != nil {
			t.Fatalf("%s:%d: not the lines, a tab and a Go string literal: %v", namedCalls, n+1, err)
		}
		var want []int
		for _, field := range strings.Split(lines, ",") {
			at, err := strconv.Atoi(field)
			if err != nil {
				t.Fatalf("%s:%d: the lines are not numbers: %v", namedCalls, n+1, err)
			}
			want = append(want, at)
		}
		rows++
		if got := textHits([]byte(text)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s:%d: the guard names lines %v of %q, want %v", namedCalls, n+1, got, text, want)
		}
	}
	if rows == 0 {
		t.Fatalf("UNKNOWN: %s holds no row, so it pins no line", namedCalls)
	}
	t.Logf("%d calling lines of %s", rows, namedCalls)
}

// The rule reads a file one time, in memory linear in its size: a 1 MiB comment of # bytes, which
// made a reader of shell parse each suffix again, and a 1 MiB script of plain lines each allocate
// less than 16 bytes for each byte of the file. The test does not run in parallel, so no other
// test allocates while it measures.
func TestCIFilesThatCompareReadsAFileInLinearMemory(t *testing.T) {
	for _, row := range []struct{ name, content string }{
		{"a 1 MiB comment", strings.Repeat("#", 1<<20-1) + "\n"},
		{"a 1 MiB script of plain lines", strings.Repeat(": x\n", 1<<18)},
		{"a 1 MiB run of line continuations", strings.Repeat("\\\n", 1<<19)},
	} {
		content := []byte(row.content)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		hits := textHits(content)
		runtime.ReadMemStats(&after)
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16*uint64(len(content)) || hits != nil {
			t.Errorf("%s: textHits allocated %d bytes for %d bytes and named %v; want less than 16 bytes a byte and no line", row.name, allocated, len(content), hits)
		}
	}
}

// expectFiles checks that ciFilesThatCompare names want over files.
func expectFiles(t *testing.T, files map[string]string, want []string) {
	t.Helper()
	read := map[string][]byte{}
	for path, content := range files {
		read[path] = []byte(content)
	}
	if got := ciFilesThatCompare(read); !reflect.DeepEqual(got, want) {
		t.Errorf("ciFilesThatCompare(%q) = %q, want %q", files, got, want)
	}
}

// expectScriptAndBlock checks that ciFilesThatCompare names the lines at (from 1) of text, both as
// a script and as the literal block of a workflow step, whose first line is line 5.
func expectScriptAndBlock(t *testing.T, text string, at []int) {
	t.Helper()
	var want []string
	for _, n := range at {
		want = append(want, fmt.Sprintf(".github/workflows/a.yaml:%d", n+4))
	}
	for _, n := range at {
		want = append(want, fmt.Sprintf("scripts/a.sh:%d", n))
	}
	expectFiles(t, map[string]string{
		"scripts/a.sh":             text + "\n",
		".github/workflows/a.yaml": workflowSteps + "      - run: |\n          " + strings.ReplaceAll(text, "\n", "\n          ") + "\n",
	}, want)
}

// readCIFiles reads every regular file under dir, keyed by its path from dir with "/", as spec-check
// guard reads a repository.
func readCIFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := fs.WalkDir(os.DirFS(dir), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		content, err := os.ReadFile(filepath.Join(dir, path))
		files[path] = content
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
