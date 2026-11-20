package tests3

import (
	"fmt"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

// The rules of TestEveryWaitAndCheckOfTheScriptHasACase (waits_test.go) for the steps of
// storeSteps, in the shard job of .github/workflows/dispatch-browser.yaml, which start the
// browser suite's file store, wait for it and check it. stepFindings reads the shell each step
// runs, through the shell reader of shellread_test.go: its run: text, or the script that text
// names (the wait step runs .github/scripts/dispatch-browser-wait-for-store.sh), and fails on what
// these say:
//
//   - The steps run their own function s3 and the commands of stepCommands alone, so a request
//     or a wait by any other command (`wget`, `weed shell`, `tail`, `.`) is named, and no
//     function runs itself again. set runs only as `set -eu`, so no step turns off the exit on a
//     failed command or sets its arguments.
//   - A step runs in the job's shell, sh -e, the POSIX sh this program reads: no store step names
//     a shell: of its own, and neither the shard job nor the workflow a defaults.run.shell, since
//     another shell may read a word otherwise (bash makes {--max-time,0,http://...} three words,
//     and gives curl --max-time 0). A script a step's run: names runs as a program of its own:
//     the shell its first line names runs it, and the runner's -e does not reach it. So its first
//     line is #!/bin/sh and its first statement `set -eu` (scriptHeadFindings, which holds
//     one-collection.sh to the same), or a failed command, prepare among them, would not end it.
//   - They ask the store things only through the wait step's s3, whose one kind of curl is
//     `curl -q -g --proto =http -sf --max-time "$seconds" "$@"`: -q, first, reads no .curlrc,
//     which could make curl try again; -g makes a URL's [..] and {..} text, not a list of
//     requests; and --proto =http refuses every URL but http://, since curl does not hold a
//     file:// URL (file:///dev/zero) to --max-time, and librtmp ends an rtmp:// one by its own
//     clock. s3 takes its arguments as its first two statements, `seconds=$1 what=$2` and
//     `shift 2`, with no other shift or set, and sets neither seconds nor what again, so a call's
//     first word is its seconds and its words after the second are curl's, as the shell builds
//     them at that call. Every s3 call gives a whole number of seconds above 0 (curl's 0 is no
//     limit), and gives curl -o with one word the shell does not split, --data-binary with
//     literal text that names no file (curl reads @<file>, @- among them, before --max-time
//     holds it), -X with a method of literal text, none of which sets a time, and one URL alone,
//     literal text or the word after --: curl makes a request of each URL, each under its own
//     --max-time. s3 ends the step by name, with exit 1, on curl's timeout (exit 28).
//   - They start weed server only by `nohup weed server ... &`, in the background of the step
//     that starts the store.
//   - They wait only in an until at the top of a step, a statement of its own right after
//     `deadline=$(( $(date +%s) + <seconds> ))` alone, whose body ends the step by name, with
//     exit 1, once the deadline passes, and they sleep only as `sleep 1`. No while or for stands
//     anywhere, no sleep outside such a wait, and no wait inside another wait or in a function,
//     which a call from inside another wait would run there: either would set the deadline the
//     outer wait reads on every pass, so it might never end.
//   - The rules shellpolicy_test.go's ownFindings reads for the script hold here too: no own
//     function takes the name of a command these rules read or is defined twice, only a wait's
//     deadline statement sets deadline and holds arithmetic, and only s3's first statement sets
//     seconds and what, to its first and second arguments.

// repository is this repository's root, read from this package's directory; workflow is the
// workflow whose shard job starts the browser suite's file store; waitStep is that job's step
// that waits for the store, and storeSteps every step of it that starts the store, waits for it
// or checks it.
const (
	repository = "../../../../"
	workflow   = repository + ".github/workflows/dispatch-browser.yaml"
	waitStep   = "Wait for the S3-compatible server"
)

var storeSteps = []string{"Start an S3-compatible server for uploaded files", waitStep, "Check that no write grew the file store"}

// stepCommands are the commands the store steps may run besides their own function s3: each
// ends by itself, or under a bound a rule of stepFindings reads. curl runs only in s3, as
// `curl -q -g --proto =http -sf --max-time "$seconds" "$@"`; one-collection.sh's own waits are
// held by the script's rules; nohup only starts weed server in the background, in the step that
// starts the store; sleep runs only as a wait's sleep 1, and set only as `set -eu`. Any other
// command is named, so a request or a wait by any other command (wget, nc, tail, weed shell, .)
// fails the program.
var stepCommands = map[string]bool{
	"[": true, "cat": true, "curl": true, "date": true, "echo": true, "exit": true, "mkdir": true,
	"nohup": true, "one-collection.sh": true, "return": true, "set": true, "shift": true, "sleep": true,
}

// scriptStep matches a step's run: text that is the path of a script of this repository and
// nothing else, which the step runs in its place.
var scriptStep = regexp.MustCompile(`^\.github/scripts/[A-Za-z0-9._-]+\.sh$`)

// A storeStep is the shell one step of storeSteps runs, and the file it is read from: the step's
// run: text, from the workflow, or the script that text names (script). shell is the step's own
// shell: key, or else its job's or its workflow's defaults.run.shell, "" where none names one.
type storeStep struct {
	file, text string
	script     bool
	shell      string
}

// planted answers the step with old replaced by replacement in its text, once (replaceOnce).
func (s storeStep) planted(t *testing.T, old, replacement string) storeStep {
	t.Helper()
	s.text = replaceOnce(t, s.file, s.text, old, replacement)
	return s
}

// workflowText answers the text of workflow.
func workflowText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatalf("read %s: %v", workflow, err)
	}
	return string(raw)
}

// storeStepRuns answers the shell each step of storeSteps runs, read from text, workflow's shard
// job: its run: text, or, where that text is the path of a script of this repository and nothing
// else, that script; and the shell that runs that text. A run: text that names a script with
// anything around it is read as it stands, so the script's name is a command the rules name.
func storeStepRuns(t *testing.T, text string) map[string]storeStep {
	t.Helper()
	type defaults struct {
		Run struct {
			Shell string `yaml:"shell"`
		} `yaml:"run"`
	}
	var parsed struct {
		Defaults defaults `yaml:"defaults"`
		Jobs     map[string]struct {
			Defaults defaults `yaml:"defaults"`
			Steps    []struct {
				Name  string `yaml:"name"`
				Run   string `yaml:"run"`
				Shell string `yaml:"shell"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("parse %s: %v", workflow, err)
	}
	job := parsed.Jobs["shard"]
	runs := map[string]storeStep{}
	for _, step := range job.Steps {
		if !slices.Contains(storeSteps, step.Name) {
			continue
		}
		shell := step.Shell
		if shell == "" {
			shell = job.Defaults.Run.Shell
		}
		if shell == "" {
			shell = parsed.Defaults.Run.Shell
		}
		runs[step.Name] = storeStep{file: path.Base(workflow), text: step.Run, shell: shell}
	}
	for _, name := range storeSteps {
		step := runs[name]
		run := step.text
		if run == "" {
			t.Fatalf("%s: the shard job has no step %q that runs a script", workflow, name)
		}
		if script := strings.TrimSpace(run); scriptStep.MatchString(script) {
			raw, err := os.ReadFile(repository + script)
			if err != nil {
				t.Fatalf("%s: step %q runs %s: %v", workflow, name, script, err)
			}
			step.file, step.text, step.script = path.Base(script), string(raw), true
			runs[name] = step
		}
	}
	return runs
}

// s3Curl answers whether the words of a curl are s3's own:
// -q -g --proto =http -sf --max-time "$seconds" "$@". -q, which curl reads only as its first word,
// keeps it from reading a .curlrc, which could make it try again (retry = 100000); -g makes [..]
// and {..} in a URL text, not a list of requests; --proto =http refuses a URL of any other
// scheme, file:// among them, whose read curl does not end at --max-time.
func s3Curl(args []shellWord) bool {
	want := []string{"-q", "-g", "--proto", "=http", "-sf", "--max-time", `"$seconds"`, `"$@"`}
	return slices.EqualFunc(args, want, func(w shellWord, form string) bool { return w.form() == form })
}

// s3CurlFinding answers what is wrong with the words an s3 call (s3 <seconds> <what> <word>...)
// gives curl, or "" when nothing is. They must be -o <file> with one word the shell does not
// split, --data-binary <data> with literal text that names no file, -X <method> with a method of
// literal text, none of which sets a time, and one URL alone: literal text that does not start
// with -, or the one word after --, which curl reads as a URL whatever it holds. curl makes a
// request of each URL, each under its own --max-time, so two URLs are two bounds. A word before
// -- whose value this program cannot know (an expansion of any kind, ~ and $'...' among them) may
// be an option that turns the bound off (--max-time 0, -m 0, --retry) or a second URL, and so may
// a value of -o, --data-binary or -X that the shell splits into words. curl reads the file a
// --data-binary of @<file> names, @- its standard input among them, before --max-time holds it,
// so a file that never ends (a pipe held open, /dev/zero) holds the request past its bound.
func s3CurlFinding(args []shellWord) string {
	urls := 0
	for i := 2; i < len(args); i++ {
		w := args[i]
		switch {
		case w.is("-o") || w.is("--data-binary") || w.is("-X"):
			if i+1 == len(args) {
				return "an s3 call that gives curl " + w.text + " and no value after it"
			}
			value := args[i+1]
			if w.is("-X") && !value.literal {
				return "an s3 call that gives curl -X " + value.source + ", a method whose value this program cannot know, so it may be words of curl's that turn the bound off"
			}
			if !value.single {
				return "an s3 call that gives curl " + w.text + " " + value.source + ", a value the shell splits into words, so it may give curl an option that turns the bound off or a second URL"
			}
			if w.is("--data-binary") && (!value.literal || strings.HasPrefix(value.text, "@")) {
				return "an s3 call that gives curl --data-binary " + value.source + ", which may name a file curl reads before --max-time holds it (@- among them, a standard input that may never end)"
			}
			i++
		case w.is("--"):
			rest := args[i+1:]
			if len(rest) != 1 {
				return fmt.Sprintf("an s3 call that gives curl %d words after --, where one URL alone is one request", len(rest))
			}
			if !rest[0].single {
				return "an s3 call that gives curl " + rest[0].source + " after --, which the shell splits into any number of URLs"
			}
			urls++
			i = len(args)
		case w.literal && !strings.HasPrefix(w.text, "-"):
			urls++
		case !w.literal:
			return "an s3 call that gives curl " + w.source + ", a word whose value this program cannot know, before --, so it may be an option that turns the bound off or a second URL"
		default:
			return "an s3 call that gives curl " + w.text + ", which is none of -o, -X, --data-binary and a URL, so it may turn the bound off"
		}
	}
	if urls != 1 {
		return fmt.Sprintf("an s3 call that gives curl %d URLs, not one: curl makes a request of each, each under its own --max-time", urls)
	}
	return ""
}

// stepSeconds answers whether a word of a step's deadline statement gives its seconds: a whole
// number of literal text.
func stepSeconds(w *syntax.Word) bool { return digits.MatchString(w.Lit()) }

// isDeadline answers whether stmt is a step's deadline statement,
// deadline=$(( $(date +%s) + <seconds> )), alone.
func isDeadline(stmt *syntax.Stmt) bool {
	a, _ := deadlineStatement(stmt, stepSeconds)
	return a != nil
}

// failsByName answers whether stmts, a block's, print an ::error:: line first and end with
// exit 1.
func failsByName(stmts []*syntax.Stmt) bool {
	if len(stmts) < 2 || !callIs(stmts[len(stmts)-1], "exit", "1") {
		return false
	}
	echo, ok := stmts[0].Cmd.(*syntax.CallExpr)
	return ok && len(echo.Args) == 2 && readWord(echo.Args[0]).is("echo") && strings.HasPrefix(readWord(echo.Args[1]).text, "::error::")
}

// endsWait answers whether stmt is the statement that ends a wait of the steps by name once its
// deadline passes: [ "$(date +%s)" -lt "$deadline" ] || { echo "::error::..."; ...; exit 1; }.
func endsWait(stmt *syntax.Stmt) bool {
	or, ok := stmt.Cmd.(*syntax.BinaryCmd)
	if !ok || or.Op != syntax.OrStmt || !callIs(or.X, "[", `"$(date +%s)"`, "-lt", `"$deadline"`, "]") {
		return false
	}
	block, ok := or.Y.Cmd.(*syntax.Block)
	return ok && failsByName(block.Stmts)
}

// endsOnTimeout answers whether s3 (decl) ends the step by name, with exit 1, on curl's timeout:
// if [ "$status" = 28 ]; then echo "::error::..."; ...; exit 1; fi.
func endsOnTimeout(decl *syntax.FuncDecl) bool {
	found := false
	if decl != nil {
		syntax.Walk(decl, func(n syntax.Node) bool {
			if c, ok := n.(*syntax.IfClause); ok && len(c.Cond) == 1 && callIs(c.Cond[0], "[", `"$status"`, "=", "28", "]") && failsByName(c.Then) {
				found = true
			}
			return !found
		})
	}
	return found
}

// stepFindings answers what the shell of the shard job's step name (step: its run: text, or the
// script that text names, with the shell that runs it) breaks of the rules above for the
// workflow's store steps.
func stepFindings(name string, step storeStep) []string {
	var findings []string
	lines := strings.Split(step.text, "\n")
	finding := func(line int, what string) {
		shown := ""
		if line >= 1 && line <= len(lines) {
			shown = strings.TrimSpace(lines[line-1])
		}
		findings = append(findings, fmt.Sprintf("%s, step %q, line %d: %s: %s", step.file, name, line, what, shown))
	}
	if step.shell != "" {
		findings = append(findings, fmt.Sprintf("%s, step %q: a shell of its own (shell: %s, the step's or a defaults.run.shell), which may read a word otherwise than this program, which reads the step as the POSIX sh the job runs it in with neither, sh -e, so it may hold a wait past its bound", step.file, name, step.shell))
	}
	if step.script {
		for _, head := range scriptHeadFindings(step.text) {
			finding(head.line, head.what)
		}
	}
	read := readShell(step.text)
	for _, refused := range read.refused {
		finding(refused.line, refused.what)
	}
	selfRunning := read.selfRunning()
	for _, function := range slices.Sorted(maps.Keys(selfRunning)) {
		finding(int(read.functions[function].Pos().Line()), "a function that runs itself again ("+selfRunning[function]+"), so nothing but a bound ends it")
	}
	for _, loop := range read.loops {
		if loop.kind != "until" {
			finding(loop.line, "a loop that is not a wait with a deadline (an until right after its deadline)")
			continue
		}
		if loop.function != "" {
			finding(loop.line, "a wait in a function, which a call from inside another wait runs there, setting the deadline that wait reads again on every pass")
		}
		if !isDeadline(loop.before) {
			finding(loop.line, "a wait with no deadline=$(( $(date +%s) + <seconds> )) just before it, in the same list of statements")
		}
		if !slices.ContainsFunc(loop.body, endsWait) {
			finding(loop.line, "a wait that does not end the step by name, with exit 1, once its deadline passes")
		}
	}
	for _, own := range ownFindings(read, stepSeconds, "s3", "seconds", "what") {
		finding(own.line, own.what)
	}
	if s3 := read.functions["s3"]; s3 != nil && !takesArguments(read, "s3", "seconds", "what") {
		finding(int(s3.Pos().Line()), "s3 does not take off its seconds and what by `seconds=$1 what=$2` and then `shift 2`, its first two statements, with no other shift or set, so curl may be given words of a call that are not its own")
	}
	for _, c := range read.commands {
		if !c.name.literal {
			finding(c.line, "runs "+c.name.source+", which is not in stepCommands, so it may ask the store or wait with no bound")
			continue
		}
		switch command := c.name.text; {
		case command == "s3" && read.functions[command] != nil:
			if len(c.args) == 0 || !c.args[0].literal || !wholeSeconds.MatchString(c.args[0].text) {
				finding(c.line, "an s3 call whose seconds are not a whole number above 0, while curl's --max-time 0 is no limit")
			}
			if what := s3CurlFinding(c.args); what != "" {
				finding(c.line, what)
			}
		case read.functions[command] != nil:
		case !stepCommands[command]:
			finding(c.line, "runs "+command+", which is not in stepCommands, so it may ask the store or wait with no bound")
		case command == "curl" && (c.function != "s3" || !s3Curl(c.args)):
			finding(c.line, `a curl outside s3, or other than s3's own curl -q -g --proto =http -sf --max-time "$seconds" "$@", so with no bound, one turned off, a .curlrc, a URL pattern or a scheme other than http`)
		case command == "nohup" && (name != storeSteps[0] || !c.background || len(c.args) < 2 || !c.args[0].is("weed") || !c.args[1].is("server")):
			finding(c.line, "a nohup that is not `nohup weed server ... &` in the step that starts the store, so it may hold the step")
		case command == "sleep" && !c.waiting:
			finding(c.line, "a sleep outside a wait with a deadline")
		case command == "sleep" && (len(c.args) != 1 || !c.args[0].is("1")):
			finding(c.line, "a sleep in a wait other than its sleep 1, which holds the wait past its deadline")
		case command == "set" && (len(c.args) != 1 || !c.args[0].is("-eu")):
			finding(c.line, "a set other than `set -eu`, which may turn off the exit on a failed command or set the arguments")
		}
	}
	if name == waitStep && !endsOnTimeout(read.functions["s3"]) {
		findings = append(findings, fmt.Sprintf("%s, step %q: s3 does not end the step by name, with exit 1, on curl's timeout (exit 28)", step.file, name))
	}
	return findings
}
