package tests3

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

// scriptBound is one bound of one-collection.sh, by the text that sets it, which occurs in the
// script once, and the case of TestOneCollectionFailsWhenTheRuleDoesNotHold that fails when it is
// cut: the case's wait or request never ends, so with the bound gone the case meets runScript's
// deadline, or fails later and by another message.
type scriptBound struct{ text, caseName string }

// scriptBounds lists every bound of one-collection.sh.
var scriptBounds = []scriptBound{
	{"timeout -s KILL 60 weed shell", "a shell that hangs: prepare fails by its bound"},
	{"timeout -s KILL 10 wget", "a master that never answers: prepare fails at the end of its wait for a volume server"},
	{"deadline=$(( $(date +%s) + seconds ))", "the grow refused: prepare fails at the end of its tries"},
	{"  registered 60\n", "a master that never answers: prepare fails at the end of its wait for a volume server"},
	{"  registered 0\n", "no volume server listed: check fails by name at once"},
	{`within 10 "the master grew no volumes`, "the grow refused: prepare fails at the end of its tries"},
	{`within 60 "the filer did not put the rule`, "the rule never in force: prepare fails at the end of its wait"},
}

// goBound is one function of seaweedfs.go that sets a deadline, and the case or test that fails
// when that deadline is cut.
type goBound struct{ function, caseName string }

// goBounds lists every function of seaweedfs.go that sets a deadline. testcontainers' own wait
// strategy (startServer's WithStartupTimeout) is not one of them: it ends at a default of its own
// when its bound is cut, so no case can tell the cut apart.
var goBounds = []goBound{
	{"runScriptWithin", "a step that never ends: runScript fails it by its deadline"},
	{"request", "TestARequestThatIsNeverAnsweredEndsAtItsDeadline"},
}

// Every wait and every check of one-collection.sh, and every deadline of seaweedfs.go, has a case
// that fails when it is cut, every command of the workflow steps around the browser suite's file
// store ends by itself or under a bound these rules read, and this is the program that says so,
// by reading those files. It reads the script, and the shell each store step runs (its run: text,
// or the script that text names), through the shell reader of shellread_test.go, which parses
// them as POSIX shell and answers every command the shell runs, wherever it runs it, with its
// words as the shell resolves them. What the reader cannot read fails the program by name: a file
// that does not parse, a kind of shell other than those the files use, a command whose name is
// not plain text (a variable, a substitution, a file-name pattern), and a trap whose command is
// not plain text. A word whose value this program cannot know (an expansion of any kind, a
// leading ~ and $'...' among them, or a file-name pattern) is not taken for a file or a URL where
// it could be an option that keeps a wait from ending: given to tail, or by an s3 call to curl,
// it fails the program unless it stands after a literal --, or is what -o takes, which curl reads
// as its file whatever it holds. It does not read what a listed command reads: a cat or a grep
// given a file that never ends, or no file at all, passes it; both places that run the script and
// the steps give them a standard input already at its end. The rules the script and the steps
// share are in shellpolicy_test.go, and the steps' own in storesteps_test.go.
//
//   - The script asks the store things (weed, wget, curl, nc) only in shell and master, each as
//     the command of `timeout -s KILL <seconds>` with seconds above 0; a timeout by any other
//     signal is refused, since weed shell ends 0 on TERM in the SeaweedFS image, and so is one of
//     0 seconds, which turns the bound off.
//   - The script waits (until, while, sleep, wait) only in within, which ends by its seconds,
//     and sleeps only as `sleep 1`. within's one wait is an until right after
//     `deadline=$(( $(date +%s) + seconds ))` alone. No wait stands inside another wait, in its
//     condition or its body, and no within's command runs within again, at once or through
//     others: each would set the deadline the outer wait reads on every pass, so it might never
//     end.
//   - The script runs its own functions and the commands of scriptCommands alone, and so do the
//     commands that within, timeout and a trap run, so a command that can wait with no bound
//     (`read`, `nc -l`, `.`) is named. A command named by a path is read by its base name. tail
//     runs only as `tail -n <lines> [--] <file>...`, each file before -- literal text that does
//     not start with -: any other word of it (`-f`, `-F`, `--follow`, or a word whose value this
//     program cannot know, which may be one) can follow a file for ever. No function runs itself
//     again, at once or through others.
//   - No own function of the script or of the steps takes the name of a command these rules read
//     (ruledCommand: date, sleep, tail, curl, timeout, kill, [ and the others), and none is
//     defined twice, since the shell would run its body in place of the one the rules read. Only
//     a wait's deadline statement, `deadline=$(( $(date +%s) + <seconds> ))` alone right before
//     its until, sets deadline, which that until's body reads, and only the first statement of
//     within in the script, of s3 in the steps, sets the words that function takes off its
//     arguments (seconds, and within's message and file or s3's what), each to its own argument;
//     a for's variable and ${x=...} assign too. No arithmetic stands anywhere but in a wait's
//     deadline statement, so no arithmetic assigns at all: busybox sh reads a variable's value in
//     arithmetic as an expression, and every shell reads the text of $x there, so after
//     `dl=deadline=9` both $(( dl )) and $(( $dl )) set deadline.
//   - within and s3 take their arguments as their first two statements, `seconds=$1 ...` and
//     `shift <n>`, and run no other shift and no set, so a call's first word is its seconds and
//     its words after those it takes off are what its last command is given, as the shell builds
//     them at that call. No set of the script sets the arguments (it needs `set -eu` alone; an
//     option o takes an option's name, as in `set -o noglob`), registered, whose within takes
//     registered's own "$1" as its seconds, shifts and sets none, and the script sets a trap on
//     EXIT alone, whose command runs once it ends: a trap on any other condition runs its
//     command in the middle of a wait, where it may set that wait's deadline again.
//   - The script runs as a program of its own (the check step, the wait script and tests3.Start
//     run it by its path), in the shell its first line names and out of reach of a runner's -e:
//     its first line is #!/bin/sh and its first statement `set -eu` (scriptHeadFindings), so the
//     shell that runs it is the POSIX sh this program reads, and a failed command ends it.
//   - No check ends the script with no message for a case to want: exit runs only in fail and
//     usage, return only as `return "$status"`, and no test (`[`, `test`, `grep`) runs where
//     nothing acts on its failure (an if, until or while, || or &&), which `set -e` turns into an
//     exit with no message.
//   - Every bound the script sets, `timeout ... <seconds>`, `within <seconds>` and
//     `registered <seconds>`, gives its seconds as a whole number and lies in a text of
//     scriptBounds, whose case exists; only registered's own within takes them as "$1".
//   - Every message the script fails by, fail's and within's, is the want of a case of
//     storeCases, each variable in it standing for any text, so a check or a wait with no case
//     fails here; a message that is a variable alone, but within's own, or that holds any other
//     expansion, is refused, and no `::error::` is printed but by fail and usage.
//   - In seaweedfs.go, every function that sets a deadline is in goBounds, whose case exists, and
//     every function that sends a request (Exec, Do) sets one.
//   - The steps of storeSteps, in the shard job of .github/workflows/dispatch-browser.yaml, which
//     start the browser suite's file store, wait for it and check it, and the script the wait step
//     runs (.github/scripts/dispatch-browser-wait-for-store.sh), are held to the rules that
//     storesteps_test.go's head says and stepFindings reads. No case in this package starts those
//     steps, so these rules are their check.
//
// The last subtest plants each kind of fault and wants this program to name it, so a rule that
// cannot see what it is for fails too, and plants shell that ends by itself, which it must pass,
// so a rule that names more than it is for fails as well.
func TestEveryWaitAndCheckOfTheScriptHasACase(t *testing.T) {
	cases := caseNames(t, storeCases)
	source, err := os.ReadFile("seaweedfs.go")
	if err != nil {
		t.Fatalf("read seaweedfs.go: %v", err)
	}
	workflowSource := workflowText(t)
	steps := storeStepRuns(t, workflowSource)
	t.Run("one-collection.sh", func(t *testing.T) {
		for _, finding := range scriptFindings(oneCollection, storeCases, scriptBounds, cases) {
			t.Error(finding)
		}
	})
	t.Run("seaweedfs.go", func(t *testing.T) {
		for _, finding := range goFindings(string(source), goBounds, cases) {
			t.Error(finding)
		}
	})
	t.Run("dispatch-browser.yaml", func(t *testing.T) {
		for _, name := range storeSteps {
			for _, finding := range stepFindings(name, steps[name]) {
				t.Error(finding)
			}
		}
	})
	t.Run("a planted request, wait, bound, check or deadline with no case fails it", func(t *testing.T) {
		script := func(old, replacement string) func() []string {
			return func() []string {
				return scriptFindings(replaceOnce(t, "one-collection.sh", oneCollection, old, replacement), storeCases, scriptBounds, cases)
			}
		}
		// inPrepare plants code in prepare, between two steps that no other plant touches.
		inPrepare := func(code string) func() []string {
			return script("  wait_in_force\n  alone_before_writes\n", "  wait_in_force\n"+code+"  alone_before_writes\n")
		}
		gosource := func(old, replacement string) func() []string {
			return func() []string {
				return goFindings(replaceOnce(t, "seaweedfs.go", string(source), old, replacement), goBounds, cases)
			}
		}
		wait := func(old, replacement string) func() []string {
			return func() []string {
				return stepFindings(waitStep, steps[waitStep].planted(t, old, replacement))
			}
		}
		// inWait plants code in the wait step's script, before the line that names the bucket.
		inWait := func(code string) func() []string {
			return wait("store=http://", code+"store=http://")
		}
		start := func(old, replacement string) func() []string {
			return func() []string {
				return stepFindings(storeSteps[0], steps[storeSteps[0]].planted(t, old, replacement))
			}
		}
		// workflowPlant plants a text in the workflow and answers the findings of its step name.
		workflowPlant := func(old, replacement, name string) func() []string {
			return func() []string {
				return stepFindings(name, storeStepRuns(t, replaceOnce(t, filepath.Base(workflow), workflowSource, old, replacement))[name])
			}
		}
		prepare := "one-collection.sh prepare dispatch-e2e-objects \"$RUNNER_TEMP/weed.log\"\n"
		// bashFirst answers text with its first line, whatever that line is, made bash's. Its plants
		// take no anchor in the line, so they name the shape whatever the real file's first line is.
		bashFirst := func(text string) string {
			_, rest, _ := strings.Cut(text, "\n")
			return "#!/usr/bin/env bash\n" + rest
		}
		const (
			tail     = "a tail that is not `tail -n <lines> [--] <file>...`"
			unknown  = "a word whose value this program cannot know"
			waits    = "waits outside within"
			s3Zero   = "an s3 call whose seconds are not a whole number above 0"
			s3Curl   = "an s3 call that gives curl "
			stepCurl = "a curl outside s3, or other than s3's own"
			ends     = `a wait that does not end the step by name`
			noDue    = "a wait with no deadline"
			itself   = "a function that runs itself again"
			ruled    = "which takes the name of a command these rules read"
			twice    = "defined more than once"
			deadline = "sets deadline other than right before its until"
			shifts   = "s3 does not take off its seconds and what"
			nested   = "a wait inside another wait"
			arith    = "arithmetic other than a wait's deadline statement"
			curlLine = `curl -q -g --proto =http -sf --max-time "$seconds" "$@"`
			noSetEU  = "a first statement other than `set -eu`"
			notSh    = "a first line other than #!/bin/sh"
			ownShell = "a shell of its own"
		)
		// One wait of the wait step, read by the program, after its deadline: shape is what
		// stands around it.
		readyWait := `until s3 10 "the root" -o /dev/null http://127.0.0.1:18333/; do [ "$(date +%s)" -lt "$deadline" ] || { echo "::error::no root"; exit 1; }; sleep 1; done`
		// The check of the S3 port's wait, which ends its body but for its sleep, and within's.
		portCheck := `[ "$(date +%s)" -lt "$deadline" ] || { echo "::error::the file store's S3 port did not answer within two minutes"; cat "$RUNNER_TEMP/weed.log"; exit 1; }` + "\n"
		withinCheck := `    [ "$(date +%s)" -lt "$deadline" ] || fail "$message" "$seen"` + "\n"
		// twoEdits plants two texts in the script, the second in the script the first made.
		twoEdits := func(old1, new1, old2, new2 string) func() []string {
			return func() []string {
				first := replaceOnce(t, "one-collection.sh", oneCollection, old1, new1)
				return scriptFindings(replaceOnce(t, "one-collection.sh", first, old2, new2), storeCases, scriptBounds, cases)
			}
		}
		planted := []struct {
			name     string
			findings func() []string
			want     string
		}{
			{"a request with no bound", script("prepare() {\n", "prepare() {\n  wget -q -O /dev/null http://127.0.0.1:9333/\n"), "asks the store outside shell and master"},
			{"a shell bounded by TERM", script("timeout -s KILL 60 weed", "timeout 60 weed"), "a timeout not by KILL"},
			{"a shell bounded by a TERM it names", script("timeout -s KILL 60 weed", "timeout -s TERM 60 weed"), "a timeout not by KILL"},
			{"a bound changed", script("timeout -s KILL 10 wget", "timeout -s KILL 5 wget"), `the bound "timeout -s KILL 5" is in no text`},
			{"a wait outside within", script("prepare() {\n", "prepare() {\n  until listed; do sleep 1; done\n"), waits},
			{"a wait with no sleep outside within", script("prepare() {\n", "prepare() {\n  while ! listed; do date > /dev/null; done\n"), waits},
			{"a bound with no case", script("\n  grow\n  configure\n", "\n  grow\n  within 30 \"the store took no rule\" \"$work/x\" true\n  configure\n"), `the bound "within 30" is in no text`},
			{"a bound whose seconds are not a number", inPrepare("  within \"$((60))\" \"the filer did not put the rule for $collection in force within a minute\" \"$work/rule.txt\" grep -q x \"$log\"\n"), "a bound whose seconds this program cannot read"},
			{"a check with no case", script("\n  grow\n  configure\n", "\n  grow\n  [ -d /data ] || fail \"the store has no data directory\"\n  configure\n"), `the message "the store has no data directory" is the want of no case`},
			{"a message that is a variable", script("  registered 0\n  list\n", "  [ -d /data ] || fail \"$collection\"\n  registered 0\n  list\n"), "a message this program cannot read"},
			{"a message that holds a substitution", script("  registered 0\n  list\n", "  [ -d /data ] || fail \"the store holds $(cat \"$work/x\")\"\n  registered 0\n  list\n"), "a message this program cannot read"},
			{"a check that exits with no message", script("  registered 0\n  list\n", "  [ -d /data ] || exit 1\n  registered 0\n  list\n"), "exits outside fail and usage"},
			{"a wait that follows the log", inPrepare("  tail -f \"$log\" | grep -q -m1 procesing\n"), tail},
			{"a tail that follows after its lines", inPrepare("  tail -n 200 -f \"$log\" > /dev/null\n"), tail},
			{"a tail whose follow is quoted", inPrepare("  tail -n 200 \"-f\" \"$log\" > /dev/null\n"), tail},
			{"a tail whose follow is on the next line", inPrepare("  tail -n 1 \\\n    -f \"$log\" > /dev/null\n"), tail},
			{"a tail whose follow a variable gives", inPrepare("  follow=-f\n  tail -n 200 $follow /tmp/weed.log > /dev/null\n"), "a tail given $follow, " + unknown},
			{"a tail whose words \"$@\" gives", inPrepare("  tail -n 200 \"$@\" > /dev/null\n"), "a tail given \"$@\", " + unknown},
			{"a tail given a file-name pattern", inPrepare("  tail -n 200 [-]f /tmp/weed.log > /dev/null\n"), "a tail given [-]f, " + unknown},
			{"a tail that follows by name", inPrepare("  tail -F \"$log\" > /dev/null\n"), tail},
			{"a tail named by its path", inPrepare("  /usr/bin/tail -f \"$log\" | grep -q -m1 procesing\n"), tail},
			{"a tail whose word is escaped", inPrepare("  \\tail -f \"$log\" > /dev/null\n"), tail},
			{"a tail whose word is partly quoted", inPrepare("  t'ai'l -f \"$log\" > /dev/null\n"), tail},
			{"a tail after a redirection", inPrepare("  > /dev/null tail -f \"$log\"\n"), tail},
			{"a command given by a pattern", inPrepare("  t?il -f \"$log\" > /dev/null\n"), "runs t?il, which is not in scriptCommands"},
			{"a tail in backticks", inPrepare("  seen=`tail -f \"$log\" | grep -m1 procesing`\n"), tail},
			{"a tail given to within", script(`"$work/rule.txt" grep -q "procesing.*$marker" "$log"`, `"$work/rule.txt" tail -n 1 -f "$log"`), tail},
			{"a tail given to a within whose seconds are unquoted", script(`within "$1" "the master lists no volume server, so the store cannot take a write" "$work/status.txt" listed`, `within $1 "the master lists no volume server, so the store cannot take a write" "$work/status.txt" tail -n 1 -f "$log"`), tail},
			{"a tail given to a within whose message is bare", inPrepare("  within \"$((60))\" rule \"$work/rule.txt\" tail -f \"$log\"\n"), tail},
			{"a tail given to timeout", inPrepare("  timeout -s KILL 10 tail -f \"$log\" > /dev/null\n"), tail},
			{"a sleep in a quoted substitution", inPrepare("  echo \"$(sleep 100000)\"\n"), waits},
			{"a sleep in arithmetic", inPrepare("  x=$(( $(sleep 100000) + 1 ))\n"), waits},
			{"a sleep in a substitution inside another", inPrepare("  x=$(echo $(sleep 100000))\n"), waits},
			{"a sleep in a redirection", inPrepare("  echo ready > \"$work/$(sleep 100000)\"\n"), waits},
			{"a sleep in a here-document", inPrepare("  cat <<EOF\n$(sleep 100000)\nEOF\n"), waits},
			{"a sleep in a parameter's default", inPrepare("  echo \"${collection:-$(sleep 100000)}\"\n"), waits},
			{"a sleep in a trap", script(`trap 'rm -rf "$work"' EXIT`, `trap 'sleep 100000; rm -rf "$work"' EXIT`), waits},
			{"a sleep in a trap past its --", script(`trap 'rm -rf "$work"' EXIT`, `trap -- 'sleep 100000; rm -rf "$work"' EXIT`), waits},
			{"a tail in a trap of one escaped word", script(`trap 'rm -rf "$work"' EXIT`, `trap tail\ -f\ /dev/null EXIT`), tail},
			{"a trap whose command holds a variable", script(`trap 'rm -rf "$work"' EXIT`, `trap "sleep 100000; rm -rf \"$work\"" EXIT`), "a trap whose command is not plain text"},
			{"a sleep that holds within past its seconds", script("    sleep 1\n  done\n", "    sleep 100000\n  done\n"), "a sleep other than `sleep 1`"},
			{"a command given by a variable", inPrepare("  follow=tail; $follow -f \"$log\" > /dev/null\n"), "runs $follow, which is not in scriptCommands"},
			{"a check that returns with no message", script("  registered 0\n  list\n", "  [ -d /data ] || return 1\n  registered 0\n  list\n"), `returns a status other than "$status"`},
			{"a test that nothing acts on", script("  registered 0\n  list\n", "  [ -d /data ]\n  registered 0\n  list\n"), "a test whose failure nothing acts on"},
			{"a shell bounded by 0 seconds", script("timeout -s KILL 60 weed", "timeout -s KILL 0 weed"), "or not of whole seconds above 0"},
			{"a command that can wait", inPrepare("  read answer\n"), "runs read, which is not in scriptCommands"},
			{"a file the script sources", inPrepare("  printf 'tail -f %s\\n' \"$log\" > \"$work/follow\"\n  . \"$work/follow\"\n"), "runs ., which is not in scriptCommands"},
			{"a script run by its path", inPrepare("  ./follow.sh > /dev/null\n"), "runs follow.sh, which is not in scriptCommands"},
			{"a wait given to within", script(`"$work/rule.txt" grep -q "procesing.*$marker" "$log"`, `"$work/rule.txt" inotifywait -q -e modify "$log"`), "runs inotifywait, which is not in scriptCommands"},
			{"a function that runs itself", script("\nprepare() {\n", "\npoll() {\n  listed || poll\n}\n\nprepare() {\n"), itself},
			{"a subshell", inPrepare("  ( sleep 1 )\n"), "runs a Subshell, a kind of shell this program does not read"},
			{"a script that does not parse", script("prepare() {\n  registered 60\n", "prepare() {\n  if registered 60\n"), "does not parse as POSIX shell"},
			{"an error printed outside fail", script("\n  grow\n  configure\n", "\n  grow\n  echo \"::error::the store is odd\"\n  configure\n"), "prints ::error:: outside fail"},
			{"a case taken away", func() []string {
				kept := slices.DeleteFunc(slices.Clone(storeCases), func(c storeCase) bool { return strings.HasPrefix(c.name, "the grow refused") })
				return scriptFindings(oneCollection, kept, scriptBounds, caseNames(t, kept))
			}, `the message "the master grew no volumes for $collection" is the want of no case`},
			{"a deadline with no case", gosource("\tt.Helper()\n\tcontainer, err := testcontainers.Run(", "\tt.Helper()\n\tctx, cancel := context.WithTimeout(ctx, time.Minute)\n\tdefer cancel()\n\tcontainer, err := testcontainers.Run("), "startServer sets a deadline"},
			{"a request with no deadline", gosource("\n// call sends", "\nfunc ping(c testcontainers.Container) {\n\t_, _, _ = c.Exec(context.Background(), []string{\"true\"})\n}\n\n// call sends"), "ping sends a request with no deadline"},
			{"a deadline cut", gosource("\tctx, cancel := context.WithTimeout(ctx, deadline)\n\tdefer cancel()\n\tsent, err", "\tsent, err"), "request sends a request with no deadline"},
			{"a curl with no bound in the wait step", inWait("curl -sf -o /dev/null http://127.0.0.1:18333/\n"), "a curl outside s3"},
			{"the wait step's s3 with its bound cut", wait(curlLine, `curl -q -g --proto =http -sf "$@"`), stepCurl},
			{"a second curl in s3 with no bound", wait(curlLine, curlLine+` || curl -sf "$@"`), stepCurl},
			{"s3's curl with its bound turned off after it", wait(curlLine, curlLine+" --max-time 0"), stepCurl},
			{"s3 that tries again a request that timed out", wait(`if [ "$status" = 28 ]; then`, `if false; then`), "on curl's timeout"},
			{"an s3 call whose seconds are a variable", wait(`s3 10 "the bucket"`, `s3 "$seconds" "the bucket"`), "an s3 call whose seconds are not a whole number"},
			{"an s3 call of 0 seconds", wait(`s3 10 "the bucket"`, `s3 0 "the bucket"`), s3Zero},
			{"an s3 call of 0 seconds after two spaces", wait(`s3 10 "the bucket"`, `s3  0 "the bucket"`), s3Zero},
			{"an s3 call whose 0 seconds are on the next line", wait(`s3 10 "the bucket"`, "s3 \\\n  0 \"the bucket\""), s3Zero},
			{"an escaped s3 call of 0 seconds", wait(`s3 10 "the readiness object's delete"`, `\s3 0 "the readiness object's delete"`), s3Zero},
			{"an s3 call that turns curl's bound off", wait(`s3 10 "the bucket" -X PUT`, `s3 10 "the bucket" --max-time 0 -X PUT`), s3Curl + "--max-time"},
			{"an s3 call that turns curl's bound off by -m", wait(`s3 10 "the bucket" -X PUT`, `s3 10 "the bucket" -m 0 -X PUT`), s3Curl + "-m"},
			{"an s3 call that has curl try again", wait(`s3 10 "the bucket" -X PUT`, `s3 10 "the bucket" --retry 100000 --retry-all-errors -X PUT`), s3Curl + "--retry"},
			{"s3 with seconds of its own", wait(`seconds=$1 what=$2`, `seconds=0 what=$2`), shifts},
			{"a wait with no deadline in the wait step", wait("deadline=$(( $(date +%s) + 60 ))\nuntil s3 15", "until s3 15"), noDue},
			{"a wait in braces after its deadline", inWait("deadline=$(( $(date +%s) + 60 ))\n{ " + readyWait + "; }\n"), noDue},
			{"a wait after && after its deadline", inWait("deadline=$(( $(date +%s) + 60 ))\n[ -d / ] && " + readyWait + "\n"), noDue},
			{"a deadline that ends nothing", wait(`[ "$(date +%s)" -lt "$deadline" ] || { echo "::error::the file store took no object"; cat "$RUNNER_TEMP/weed.log"; exit 1; }`, "true"), ends},
			{"a wait by tries in the wait step", inWait("for try in $(seq 1 60); do s3 10 \"the root\" -o /dev/null http://127.0.0.1:18333/ && break; done\n"), "a loop that is not a wait with a deadline"},
			{"a loop in a quoted substitution", inWait("x=\"$(while :; do :; done)\"\n"), "a loop that is not a wait with a deadline"},
			{"a sleep outside a wait", inWait("sleep 5\n"), "a sleep outside a wait"},
			{"a request by wget in the wait step", inWait("wget -q -O /dev/null http://127.0.0.1:18333/\n"), "runs wget, which is not in stepCommands"},
			{"a request by a command a variable gives", inWait("fetch=wget; $fetch -q -O /dev/null http://127.0.0.1:18333/\n"), "runs $fetch, which is not in stepCommands"},
			{"a weed shell in the wait step", inWait("printf 'collection.list\\n' | weed shell -master=127.0.0.1:9333 > \"$RUNNER_TEMP/collections.txt\"\n"), "runs weed, which is not in stepCommands"},
			{"a tail in the wait step", inWait("tail -f \"$RUNNER_TEMP/weed.log\" | grep -q -m1 \"Start Seaweed S3\"\n"), "runs tail, which is not in stepCommands"},
			{"a file the wait step sources", inWait(". \"$RUNNER_TEMP/more.sh\"\n"), "runs ., which is not in stepCommands"},
			{"a function of the wait step that runs itself", inWait("poll() {\n  s3 10 \"the root\" -o /dev/null http://127.0.0.1:18333/ || poll\n}\npoll\n"), itself},
			{"a wait on one line whose deadline ends nothing", inWait("deadline=$(( $(date +%s) + 60 ))\nuntil s3 10 \"the root\" -o /dev/null http://127.0.0.1:18333/; do sleep 1; done\n"), ends},
			{"a long sleep in a wait", wait("  sleep 1\ndone\npackages/envoy/internal/tests3/one-collection.sh prepare", "  sleep 100000\ndone\npackages/envoy/internal/tests3/one-collection.sh prepare"), "a sleep in a wait other than its sleep 1"},
			{"a wait whose done is never found", wait("one-collection.sh check", "one-collection.sh check dispatch-e2e-objects \"$RUNNER_TEMP/weed.log\"\ndeadline=$(( $(date +%s) + 60 ))\nuntil s3 10 \"the root\" -o /dev/null http://127.0.0.1:18333/; do\n  sleep 1\n#"), "does not parse as POSIX shell"},
			{"a store started in the foreground", start("2>&1 &", "2>&1"), "a nohup that is not `nohup weed server ... &`"},
			// A word whose value this program cannot know, before --.
			{"a tail whose follow a quoted variable gives", inPrepare("  follow=-f\n  tail -n 200 \"$follow\" /tmp/weed.log > /dev/null\n"), "a tail given \"$follow\", " + unknown},
			{"a tail whose follow ~ gives", inPrepare("  HOME=-f\n  tail -n 200 ~ /tmp/weed.log > /dev/null\n"), "a tail given ~, " + unknown},
			{"a tail whose follow $'...' gives", inPrepare("  tail -n 200 $'\\055f' /tmp/weed.log > /dev/null\n"), "a tail given $'\\055f', " + unknown},
			{"a tail whose follow $\"...\" gives", inPrepare("  tail -n 200 $\"-f\" /tmp/weed.log > /dev/null\n"), "a tail given $\"-f\", " + unknown},
			{"a tail with no file after --", inPrepare("  tail -n 200 -- > /dev/null\n"), tail},
			{"an s3 call whose option a quoted variable gives", inWait("limit=--max-time\ns3 10 \"the readiness object's delete\" \"$limit\" 0 -X DELETE -o /dev/null -- \"$store/readiness\"\n"), s3Curl + "\"$limit\", " + unknown},
			{"an s3 call whose option ~ gives", inWait("HOME=-m0\ns3 10 \"the root\" ~ -o /dev/null http://127.0.0.1:18333/\n"), s3Curl + "~, " + unknown},
			{"an s3 call whose option $'...' gives", inWait("s3 10 \"the root\" $'-m0' -o /dev/null http://127.0.0.1:18333/\n"), s3Curl + "$'-m0', " + unknown},
			// One URL alone, which curl asks for once, under the bound.
			{"an s3 call given a URL range", wait(`-X PUT -o /dev/null -- "$store"`, `-X PUT -o /dev/null "$store/[1-999999999]"`), s3Curl + "\"$store/[1-999999999]\", " + unknown},
			{"an s3 call given a URL list", wait(`-X PUT -o /dev/null -- "$store"`, `-X PUT -o /dev/null "$store/{a,b,c}"`), s3Curl + "\"$store/{a,b,c}\", " + unknown},
			{"an s3 call given three URLs", wait(`-X PUT -o /dev/null -- "$store"`, `-X PUT -o /dev/null "$store" "$store" "$store"`), s3Curl + "\"$store\", " + unknown},
			{"an s3 call given two URLs", inWait("s3 10 \"the root\" -o /dev/null http://127.0.0.1:18333/a http://127.0.0.1:18333/b\n"), "an s3 call that gives curl 2 URLs, not one"},
			{"an s3 call given two URLs after --", wait(`-X PUT -o /dev/null -- "$store"`, `-X PUT -o /dev/null -- "$store" "$store"`), "an s3 call that gives curl 2 words after --"},
			{"an s3 call given a URL the shell splits after --", wait(`-X PUT -o /dev/null -- "$store"`, `-X PUT -o /dev/null -- $store`), "after --, which the shell splits"},
			{"an s3 call given no URL", wait(`-X PUT -o /dev/null -- "$store"`, `-X PUT -o /dev/null`), "an s3 call that gives curl 0 URLs, not one"},
			{"an s3 call given \"$@\" after --", wait(`-X PUT -o /dev/null -- "$store"`, `-X PUT -o /dev/null -- "$@"`), "after --, which the shell splits"},
			{"s3's curl that reads URL patterns", wait(curlLine, `curl -q --proto =http -sf --max-time "$seconds" "$@"`), stepCurl},
			{"s3's curl that reads a .curlrc that tries again", func() []string {
				return stepFindings(waitStep, steps[waitStep].planted(t, "store=http://", "echo \"retry = 100000\" > \"$HOME/.curlrc\"\nstore=http://").
					planted(t, curlLine, `curl -g --proto =http -sf --max-time "$seconds" "$@"`))
			}, stepCurl},
			// A URL of a scheme curl does not hold to --max-time: file:///dev/zero is read for ever.
			{"s3's curl that reads a URL of any scheme", wait(curlLine, `curl -q -g -sf --max-time "$seconds" "$@"`), stepCurl},
			{"s3's curl that reads a file:// URL too", wait(curlLine, `curl -q -g --proto =http,file -sf --max-time "$seconds" "$@"`), stepCurl},
			// s3's arguments as the shell builds them at a call: a shift that does not run, or runs
			// elsewhere, leaves seconds and what to curl.
			{"s3 that gives curl its what", wait("  shift 2\n", "  shift\n"), shifts},
			{"s3 that takes off its seconds after its curl", wait("  shift 2\n  "+curlLine+" && return 0\n", "  "+curlLine+" && return 0\n  shift 2\n"), shifts},
			{"s3 whose shift a condition skips", wait("  shift 2\n", "  [ 1 = 0 ] && \\\n  shift 2\n"), shifts},
			{"s3 whose shift follows a test of its what", wait("  shift 2\n", "  [ -z \"$what\" ] && shift 2\n"), shifts},
			{"s3 whose shift runs in a substitution", wait("  shift 2\n", "  echo \"$(shift 2)\" > /dev/null\n"), shifts},
			{"s3 whose shift stands in an if", wait("  shift 2\n", "  if true; then shift 2; fi\n"), shifts},
			{"s3 that reads its seconds after its shift", wait("  seconds=$1 what=$2\n  shift 2\n", "  shift 2\n  seconds=$1 what=$2\n"), shifts},
			{"s3 that reads its what from another word", wait("  seconds=$1 what=$2\n", "  seconds=$1 what=$3\n"), shifts},
			{"s3 that shifts again", wait("  shift 2\n", "  shift 2\n  shift 1\n"), shifts},
			{"s3 that takes off one word", wait("  shift 2\n", "  shift 1\n"), shifts},
			{"s3 that takes off three words", wait("  shift 2\n", "  shift 3\n"), shifts},
			// -o, --data-binary and -X, each with a value curl takes as its own.
			{"an s3 call whose -o the shell splits", inWait("out='/dev/null -m0'\ns3 10 \"the root\" -o $out -- \"$store\"\n"), s3Curl + "-o $out, a value the shell splits"},
			{"an s3 call whose --data-binary the shell splits", inWait("d='ready -m0'\ns3 10 \"the root\" --data-binary $d -o /dev/null -- \"$store\"\n"), s3Curl + "--data-binary $d, a value the shell splits"},
			{"an s3 call whose -X a variable gives", inWait("m='PUT -m0'\ns3 10 \"the root\" -X $m -o /dev/null -- \"$store\"\n"), s3Curl + "-X $m, a method whose value"},
			{"an s3 call whose -X a quoted variable gives", inWait("m=PUT\ns3 10 \"the root\" -X \"$m\" -o /dev/null -- \"$store\"\n"), s3Curl + "-X \"$m\", a method whose value"},
			{"an s3 call that gives -o no value", inWait("s3 10 \"the root\" http://127.0.0.1:18333/ -o\n"), s3Curl + "-o and no value after it"},
			// --data-binary with literal text that names no file: curl reads @<file>, @- its
			// standard input among them, before --max-time holds the request.
			{"an s3 call whose --data-binary reads its standard input", wait("--data-binary ready", "--data-binary @-"), s3Curl + "--data-binary @-, which may name a file"},
			{"an s3 call whose --data-binary a quoted variable gives", wait("--data-binary ready", "--data-binary \"$d\""), s3Curl + "--data-binary \"$d\", which may name a file"},
			// The steps' set runs only as set -eu.
			{"a set in the wait step that turns off the exit on a failure", wait("set -eu\n", "set +e\n"), "a set other than `set -eu`"},
			{"a set in the wait step that sets its arguments", wait("set -eu\n", "set -eu\nset -- x\n"), "a set other than `set -eu`"},
			{"a set -eu that turns the exit off after it", wait("set -eu\n", "set -eu +e\n"), "a set other than `set -eu`"},
			// A script runs as a program of its own: the shell its first line names runs it, and no
			// runner's -e reaches it, so its first statement is set -eu, or a failed prepare goes on.
			{"a wait script with no set -eu", wait("set -eu\n", ""), noSetEU},
			{"a wait script whose set -eu comes after prepare", func() []string {
				return stepFindings(waitStep, steps[waitStep].planted(t, "set -eu\n", "").planted(t, prepare, prepare+"set -eu\n"))
			}, noSetEU},
			{"a wait script whose set -eu is in a function", wait("set -eu\n", "strict() {\n  set -eu\n}\n"), noSetEU},
			{"a wait script of no statement", func() []string {
				return stepFindings(waitStep, storeStep{file: steps[waitStep].file, text: "#!/bin/sh\n", script: true})
			}, noSetEU},
			{"one-collection.sh with no set -eu", script("set -eu\n", ""), noSetEU},
			{"a wait script that bash runs, with a word bash splits", func() []string {
				step := steps[waitStep].planted(t, "store=http://", "s3 10 \"the root\" -o /dev/null {--max-time,0,http://127.0.0.1:18333/}\nstore=http://")
				step.text = bashFirst(step.text)
				return stepFindings(waitStep, step)
			}, notSh},
			{"one-collection.sh that bash runs", func() []string {
				return scriptFindings(bashFirst(oneCollection), storeCases, scriptBounds, cases)
			}, notSh},
			{"a store step with a shell of its own", workflowPlant("        id: file-store\n", "        id: file-store\n        shell: bash -e {0}\n", waitStep), ownShell},
			{"a shard job whose steps a default shell runs", workflowPlant("  shard:\n    needs: binaries\n", "  shard:\n    needs: binaries\n    defaults:\n      run:\n        shell: bash -e {0}\n", storeSteps[0]), ownShell},
			{"a workflow whose steps a default shell runs", workflowPlant("\njobs:\n", "\ndefaults:\n  run:\n    shell: bash -e {0}\n\njobs:\n", storeSteps[2]), ownShell},
			// A command the rules read, or the deadline a wait reads, that the file makes its own.
			{"a date of the wait step's own", wait("s3() {", "date() {\n  echo 0\n}\ns3() {"), ruled},
			{"a date of the script's own", script("\nwithin() {\n", "\ndate() {\n  echo 0\n}\n\nwithin() {\n"), ruled},
			{"a kill of the script's own", script("\nwithin() {\n", "\nkill() {\n  echo 0\n}\n\nwithin() {\n"), ruled},
			{"a test of the script's own", script("\nwithin() {\n", "\ntest() {\n  echo 0\n}\n\nwithin() {\n"), ruled},
			{"a wait of the script's own", script("\nwithin() {\n", "\nwait() {\n  echo 0\n}\n\nwithin() {\n"), ruled},
			{"a within defined twice", script("\n# 1. A volume server", "\nwithin() {\n  shift 3\n  until \"$@\"; do\n    sleep 1\n  done\n}\n\n# 1. A volume server"), twice},
			{"a deadline set again in a wait's body", wait("http://127.0.0.1:18333/; do\n", "http://127.0.0.1:18333/; do\n  deadline=$(( $(date +%s) + 60 ))\n"), deadline},
			{"a deadline set again in s3", wait("  shift 2\n", "  shift 2\n  deadline=$(( $(date +%s) + 60 ))\n"), deadline},
			{"a deadline set in a wait's condition", wait(`until s3 10 "the S3 port's first request"`, `until deadline=$(( $(date +%s) + 60 )); s3 10 "the S3 port's first request"`), deadline},
			{"a deadline set again by arithmetic", wait("http://127.0.0.1:18333/; do\n", "http://127.0.0.1:18333/; do\n  echo $(( deadline = $(date +%s) + 60 )) > /dev/null\n"), arith},
			{"a deadline set again by a parameter", wait("http://127.0.0.1:18333/; do\n", "http://127.0.0.1:18333/; do\n  echo \"${deadline:=0}\" > /dev/null\n"), deadline},
			{"a deadline set again by ++", wait("http://127.0.0.1:18333/; do\n", "http://127.0.0.1:18333/; do\n  echo $(( deadline++ )) > /dev/null\n"), arith},
			{"a deadline set again in within's wait", script("    [ \"$(date +%s)\" -lt \"$deadline\" ]", "    deadline=$(( $(date +%s) + 60 ))\n    [ \"$(date +%s)\" -lt \"$deadline\" ]"), deadline},
			{"a deadline set again by a for", script("    [ \"$(date +%s)\" -lt \"$deadline\" ]", "    for deadline in 99999999999; do echo; done\n    [ \"$(date +%s)\" -lt \"$deadline\" ]"), deadline},
			{"within's seconds set again", script("  shift 3\n  deadline=", "  shift 3\n  seconds=100000\n  deadline="), "within takes its seconds from other than its first argument"},
			// Only within's first statement sets the words it takes off its arguments, and s3's
			// the same: a later one, even of the same text, takes a word that is not the call's.
			{"within's seconds set again by $1 after its shift", script("  shift 3\n  deadline=", "  shift 3\n  listed=99999999999 seconds=$1\n  deadline="), "within takes its seconds from other than its first argument"},
			{"within's message set again after its shift", script("  shift 3\n  deadline=", "  shift 3\n  message=$1\n  deadline="), "within takes its message from other than its second argument"},
			{"within's file set again after its shift", script("  shift 3\n  deadline=", "  shift 3\n  seen=/dev/null\n  deadline="), "within takes its seen from other than its third argument"},
			{"s3's seconds set again by $1 after its shift", wait("  shift 2\n", "  shift 2\n  seconds=$1\n"), "s3 takes its seconds from other than its first argument"},
			{"s3's what set again after its shift", wait("  shift 2\n", "  shift 2\n  what=$1\n"), "s3 takes its what from other than its second argument"},
			{"within that takes its message under another name", script("  seconds=$1 message=$2 seen=$3\n", "  seconds=$1 msg=$2 seen=$3\n"), "within does not take off its seconds, message and file"},
			{"within's seconds after its arguments are set", script("  seconds=$1 message=$2 seen=$3\n", "  first=$1; shift; set -- 100000 \"$@\"\n  seconds=$1 message=$2 seen=$3\n"), "within does not take off its seconds, message and file"},
			{"within whose arguments a set sets after its shift", script("  shift 3\n  deadline=", "  shift 3\n  set -- 100000 x y\n  deadline="), "within does not take off its seconds, message and file"},
			{"within's deadline set twice in one statement", script("  deadline=$(( $(date +%s) + seconds ))\n", "  deadline=$(( $(date +%s) + seconds )) deadline=99999999999\n"), deadline},
			{"a set that sets the script's arguments", script("set -eu\n", "set -eu\nset -- prepare tests3 /tmp/weed.log\n"), "a set that sets the arguments"},
			{"a set of plain words", script("set -eu\n", "set -eu\nset 100000 x y\n"), "a set that sets the arguments"},
			{"a set of a word this program cannot know", script("set -eu\n", "set -eu\nset -$opts\n"), "a set that sets the arguments"},
			{"a set whose option's name the shell gives", script("set -eu\n", "set -eu\nset -o $name\n"), "a set that sets the arguments"},
			{"a set of an option by its name and an argument", script("set -eu\n", "set -eu\nset -o noglob 99999999\n"), "a set that sets the arguments"},
			{"registered whose arguments a set -- sets", script("registered() {\n  within \"$1\"", "registered() {\n  set -- +99999999\n  within \"$1\""), "a set that sets the arguments"},
			{"registered whose arguments a set - sets", script("registered() {\n  within \"$1\"", "registered() {\n  set - +99999999\n  within \"$1\""), "a set that sets the arguments"},
			{"registered that shifts its seconds away", script("registered() {\n  within \"$1\"", "registered() {\n  shift\n  within \"$1\""), "a bound whose seconds this program cannot read"},
			{"registered whose seconds a set sets", script("registered() {\n  within \"$1\"", "registered() {\n  set -- 99999999\n  within \"$1\""), "a bound whose seconds this program cannot read"},
			{"within's wait with no deadline statement before it", script("  deadline=$(( $(date +%s) + seconds ))\n", "  # deadline=$(( $(date +%s) + seconds ))\n"), "a wait of within other than its until right after"},
			// within's deadline adds within's own seconds: a number of its own, with the bound's
			// text kept in a comment, so that scriptBounds' count of that text still reads 1.
			{"within's deadline by a number of its own", script("  deadline=$(( $(date +%s) + seconds ))\n", "  deadline=$(( $(date +%s) + 100000 )) # deadline=$(( $(date +%s) + seconds ))\n"), "a wait of within other than its until right after"},
			{"a deadline statement that assigns more", wait("deadline=$(( $(date +%s) + 120 ))\nuntil s3 10 \"the S3 port's", "deadline=$(( $(date +%s) + 120 )) n=0\nuntil s3 10 \"the S3 port's"), noDue},
			// Each condition of a step's deadline statement: a whole number of literal seconds,
			// added (+) to $(date +%s).
			{"a step's deadline whose seconds a variable gives", wait("deadline=$(( $(date +%s) + 60 ))\nuntil s3 15", "s=99999999999\ndeadline=$(( $(date +%s) + $s ))\nuntil s3 15"), noDue},
			{"a step's deadline that multiplies", wait("deadline=$(( $(date +%s) + 60 ))\nuntil s3 15", "deadline=$(( $(date +%s) * 60 ))\nuntil s3 15"), noDue},
			{"a step's deadline from a time other than the clock's", wait("deadline=$(( $(date +%s) + 60 ))\nuntil s3 15", "deadline=$(( $(echo 99999999999) + 60 ))\nuntil s3 15"), noDue},
			// A wait that sets the deadline another wait reads, on each of its passes.
			{"a wait inside another wait's body", wait(portCheck, portCheck+"  deadline=$(( $(date +%s) + 60 ))\n  "+readyWait+"\n"), nested},
			{"a wait inside another wait's condition", wait(`until s3 10 "the S3 port's first request"`, "until { deadline=$(( $(date +%s) + 60 )); "+readyWait+"; }; s3 10 \"the S3 port's first request\""), nested},
			{"a wait in a function of the wait step", inWait("poll() {\n  deadline=$(( $(date +%s) + 60 ))\n  " + readyWait + "\n}\n"), "a wait in a function"},
			{"a wait inside within's wait", script(withinCheck, withinCheck+"    deadline=$(( $(date +%s) + 60 ))\n    until [ -d / ]; do sleep 1; done\n"), nested},
			{"a within whose command runs within through others", twoEdits("prepare() {\n  registered 60\n", "gate() {\n  registered 60\n}\n\nprepare() {\n",
				` master "/vol/grow?collection=$collection&count=7" "$work/grow.txt"`, " gate"), "a within whose command runs within again (gate → registered → within)"},
			{"a within whose command is within", script(` master "/vol/grow?collection=$collection&count=7" "$work/grow.txt"`, ` within 10 x "$work/x" true`), "a within whose command runs within again (within)"},
			// Arithmetic that reads a variable's value, which can assign deadline.
			{"a deadline set through a variable's value in arithmetic", twoEdits("work=$(mktemp -d)\n", "work=$(mktemp -d)\ndl='deadline=99999999999'\n",
				withinCheck, withinCheck+"    echo $(( dl )) > /dev/null\n"), arith},
			{"a deadline set through $x in arithmetic", wait(portCheck, portCheck+"  echo $(( $dl )) > /dev/null\n"), arith},
			{"arithmetic in s3", wait("  shift 2\n", "  shift 2\n  echo $(( $dl )) > /dev/null\n"), arith},
			{"a deadline set by an operator the POSIX parse refuses", wait("http://127.0.0.1:18333/; do\n", "http://127.0.0.1:18333/; do\n  echo $(( deadline **= 2 )) > /dev/null\n"), "does not parse as POSIX shell"},
			// Each part of the reader, so a part made blind leaves its fault unnamed.
			{"a sleep in a case's word", inPrepare("  case $(sleep 100000) in *) echo x ;; esac\n"), waits},
			{"a sleep in a case's pattern", inPrepare("  case x in $(sleep 100000)) echo x ;; esac\n"), waits},
			{"a tail in a case's arm", inPrepare("  case x in x) tail -f \"$log\" ;; esac\n"), tail},
			{"a tail in an if's condition", inPrepare("  if tail -f \"$log\"; then echo x; fi\n"), tail},
			{"a tail in an if's then", inPrepare("  if [ -d / ]; then tail -f \"$log\"; fi\n"), tail},
			{"a tail in an if's else", inPrepare("  if [ -d / ]; then echo x; else tail -f \"$log\"; fi\n"), tail},
			{"a tail in an elif's condition", inPrepare("  if [ -d / ]; then echo x; elif tail -f \"$log\"; then echo y; fi\n"), tail},
			{"a sleep in a for's items", inPrepare("  for x in $(sleep 100000); do echo \"$x\"; done\n"), waits},
			{"a tail in a for's body", inPrepare("  for x in a; do tail -f \"$log\"; done\n"), tail},
			{"an s3 call of 0 seconds in a wait's condition", wait(`until s3 10 "the S3 port's first request"`, `until s3 0 "the S3 port's first request"`), s3Zero},
			{"a function that runs itself through another", script("\nprepare() {\n", "\npoll() {\n  again\n}\n\nagain() {\n  poll\n}\n\nprepare() {\n"), "poll → again → poll"},
			{"a trap whose command does not parse", script(`trap 'rm -rf "$work"' EXIT`, `trap 'if' EXIT`), "a trap whose command does not parse as POSIX shell"},
			// A trap on a condition other than EXIT runs its command in the middle of a wait.
			{"a trap on CHLD that runs a wait", script(`trap 'rm -rf "$work"' EXIT`, "trap 'rm -rf \"$work\"' EXIT\nagain() {\n  if [ -z \"${busy:-}\" ]; then busy=1; grow; busy=; fi\n}\ntrap again CHLD"), "a trap on a condition other than EXIT"},
			{"a trap on CHLD that shifts", script(`trap 'rm -rf "$work"' EXIT`, "trap 'rm -rf \"$work\"' EXIT\ntrap shift CHLD"), "a trap on a condition other than EXIT"},
			{"a trap on EXIT and CHLD that runs a wait", script(`trap 'rm -rf "$work"' EXIT`, "trap 'rm -rf \"$work\"' EXIT\nagain() {\n  if [ -z \"${busy:-}\" ]; then busy=1; grow; busy=; fi\n}\ntrap again EXIT CHLD"), "a trap on a condition other than EXIT"},
			{"a tail given to eval", inPrepare("  eval \"tail -f \\\"$log\\\"\"\n"), "runs eval, which is not in scriptCommands"},
			{"an alias that follows the log", inPrepare("  alias poll=\"tail -f \\\"$log\\\"\"\n  poll\n"), "runs alias, which is not in scriptCommands"},
			{"a tail given to a within whose seconds are unquoted arithmetic", inPrepare("  within $((60)) rule \"$work/rule.txt\" tail -f \"$log\"\n"), tail},
			// The lists of bounds, each held to the files and to the cases.
			{"a bound that occurs twice", script("  registered 60\n", "  registered 60\n  registered 60\n"), "occurs 2 times, not once"},
			{"a bound whose case does not exist", func() []string {
				bounds := slices.Clone(scriptBounds)
				bounds[0].caseName = "no such case"
				return scriptFindings(oneCollection, storeCases, bounds, cases)
			}, `names a case that does not exist: "no such case"`},
			{"seaweedfs.go that does not parse", gosource("\npackage tests3\n", "\npackage tests3\n{\n"), "seaweedfs.go: parse:"},
			{"a deadline listed that seaweedfs.go does not set", func() []string {
				return goFindings(string(source), append(slices.Clone(goBounds), goBound{"noSuchFunction", "TestARequestThatIsNeverAnsweredEndsAtItsDeadline"}), cases)
			}, "goBounds lists noSuchFunction, which sets no deadline"},
			{"a deadline whose case does not exist", func() []string {
				bounds := slices.Clone(goBounds)
				bounds[1].caseName = "no such case"
				return goFindings(string(source), bounds, cases)
			}, `the deadline of request names a case that does not exist: "no such case"`},
		}
		// Each compound operator of arithmetic that assigns to the variable on its left, which the
		// rule that refuses arithmetic outside a deadline statement names; = and ++ have cases of
		// their own above.
		for _, op := range []string{"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<=", ">>="} {
			planted = append(planted, struct {
				name     string
				findings func() []string
				want     string
			}{"a deadline set again by " + op, wait("http://127.0.0.1:18333/; do\n", "http://127.0.0.1:18333/; do\n  echo $(( deadline "+op+" 2 )) > /dev/null\n"), arith})
		}
		for _, c := range planted {
			findings := c.findings()
			if !slices.ContainsFunc(findings, func(f string) bool { return strings.Contains(f, c.want) }) {
				t.Errorf("%s: the program found %q, want a finding saying %q", c.name, findings, c.want)
			}
		}
		// Shell that ends by itself, which the program must pass: the shell acts on the failure of
		// a negated test, of a pipe's left side, of a wait's condition and of a substitution in an
		// assignment before a command; tail reads every word after -- as a file, a quoted literal
		// path is no option, and an s3 call of one URL is one request.
		for _, c := range []struct {
			name     string
			findings func() []string
		}{
			{"a negated test", inPrepare("  ! grep -q x \"$log\"\n")},
			{"a count on a pipe's left side", inPrepare("  grep -c x \"$log\" | tr -d \" \" > \"$work/n\"\n")},
			{"a tail of a file after --", inPrepare("  tail -n 200 -- \"$log\" > /dev/null\n")},
			{"a tail of a quoted literal path", inPrepare("  tail -n 200 \"/tmp/weed.log\" > /dev/null\n")},
			{"an s3 call of one URL after --", inWait("s3 30 \"the root\" -o /dev/null -- \"$url\"\n")},
			{"an s3 call of one literal URL", inWait("s3 10 \"the root\" -o /dev/null http://127.0.0.1:18333/\n")},
			{"a wait on one line whose deadline ends it", inWait("deadline=$(( $(date +%s) + 60 ))\n" + readyWait + "\n")},
			{"a test as a wait's condition", script("  until \"$@\"; do\n", "  until grep -q x \"$seen\"; do\n")},
			{"a test in an assignment before a command", inPrepare("  n=\"$(grep -c x \"$log\")\" echo > /dev/null\n")},
			{"a tail given an escaped $ before a quote", inPrepare("  tail -n 200 \\$'x' /tmp/weed.log > /dev/null\n")},
			{"a tail given an escaped ~ and a ~ inside a word", inPrepare("  tail -n 200 \\~ a~b /tmp/weed.log > /dev/null\n")},
			{"an s3 call whose what is an option", inWait("s3 2 -m0 -o /dev/null -- \"$store\"\n")},
			{"a set of options alone", script("set -eu\n", "set -eu\nset +x\n")},
			{"a set of an option by its name", script("set -eu\n", "set -eu\nset -o noglob\n")},
			{"a trap on EXIT past its --", script(`trap 'rm -rf "$work"' EXIT`, `trap -- 'rm -rf "$work"' EXIT`)},
		} {
			if findings := c.findings(); len(findings) > 0 {
				t.Errorf("%s: the program found %q, want no finding", c.name, findings)
			}
		}
	})
}

// A request to a server that takes the connection and never answers ends at its deadline, by
// name, rather than holding the test to go test's -timeout: the case of request's deadline.
func TestARequestThatIsNeverAnsweredEndsAtItsDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the server that never answers: %v", err)
	}
	var held []net.Conn
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-accepting
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	url := "http://" + listener.Addr().String() + "/bucket"
	_, _, err = request(context.Background(), http.MethodPut, url, "", time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "PUT "+url) {
		t.Fatalf("request to a server that never answers: %v, want an error naming PUT %s and its deadline", err, url)
	}
}

// caseNames answers the names of cases and of this package's Test functions, read from its test
// files, which a bound may name as its case.
func caseNames(t *testing.T, cases []storeCase) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, c := range cases {
		names[c.name] = true
	}
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list this package's test files: %v (%d found)", err, len(files))
	}
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				names[fn.Name.Name] = true
			}
		}
	}
	return names
}

// scriptCommands are the commands one-collection.sh may run besides its own functions: shell
// builtins, and programs that read what they are given and end, or that run under a bound the
// rules above hold (timeout, sleep). A command that can wait with no bound is left out, so the
// program names it wherever the script runs it; tail is held to `tail -n <lines> [--] <file>...`,
// sleep to `sleep 1`, exit to fail and usage and return to `return "$status"`, by rules of their
// own.
var scriptCommands = map[string]bool{
	"[": true, "basename": true, "cat": true, "date": true, "echo": true, "exit": true, "grep": true,
	"mktemp": true, "printf": true, "return": true, "rm": true, "set": true, "shift": true,
	"sleep": true, "tail": true, "timeout": true, "tr": true, "trap": true,
}

// storeRequests are the commands that can ask the store things, which the script runs only
// under a timeout in shell and master.
var storeRequests = map[string]bool{"weed": true, "wget": true, "curl": true, "nc": true}

// messageWord gives, for each command of the script that fails by a message, which of its words
// the message is: fail <message> [file...] and within <seconds> <message> <file> <command>...
var messageWord = map[string]int{"fail": 0, "within": 1}

// killTimeout answers whether the words of a timeout are -s KILL <seconds above 0> <command>...
func killTimeout(args []shellWord) bool {
	return len(args) > 3 && args[0].is("-s") && args[1].is("KILL") && args[2].literal && wholeSeconds.MatchString(args[2].text)
}

// tailFinding answers what is wrong with the words of a tail, or "" when nothing is. They must be
// -n <lines> [--] <file>...: any other option of tail (-f, -F, --follow) can follow a file for
// ever, and so can a word before -- whose value this program cannot know (an expansion of any
// kind, ~ and $'...' among them, or a file-name pattern), which may be one. tail reads every word
// after -- as a file.
func tailFinding(args []shellWord) string {
	const form = "a tail that is not `tail -n <lines> [--] <file>...`, which can follow a file for ever"
	if len(args) < 3 || !args[0].is("-n") || !args[1].literal || !digits.MatchString(args[1].text) {
		return form
	}
	if args[2].is("--") {
		if len(args) == 3 {
			return form
		}
		return ""
	}
	for _, file := range args[2:] {
		switch {
		case !file.literal:
			return "a tail given " + file.source + ", a word whose value this program cannot know, before --, so it may be an option that follows a file for ever"
		case strings.HasPrefix(file.text, "-"):
			return form
		}
	}
	return ""
}

// boundSeconds answers the word of a bound that gives its seconds, timeout's, within's or
// registered's, and the bound's text as a finding names it.
func boundSeconds(name string, args []shellWord) (shellWord, string) {
	i := 0
	if name == "timeout" && len(args) > 2 && args[0].is("-s") {
		i = 2
	}
	if i >= len(args) {
		return shellWord{}, name
	}
	words := []string{name}
	for _, w := range args[:i+1] {
		words = append(words, w.form())
	}
	return args[i], strings.Join(words, " ")
}

// messagePattern answers the pattern of every text a message of fail or within can be, each
// plain variable in it standing for any text, and whether the message is a variable alone; ok is
// false when it holds any other expansion, which no pattern says.
func messagePattern(message shellWord) (pattern string, only, ok bool) {
	var b strings.Builder
	var read func(part syntax.WordPart, quoted bool) bool
	read = func(part syntax.WordPart, quoted bool) bool {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(regexp.QuoteMeta(unescape(p.Value, quoted)))
		case *syntax.SglQuoted:
			b.WriteString(regexp.QuoteMeta(p.Value))
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				if !read(inner, true) {
					return false
				}
			}
		case *syntax.ParamExp:
			if !plainVariable(p) {
				return false
			}
			b.WriteString(".+")
		default:
			return false
		}
		return true
	}
	for _, part := range message.word.Parts {
		if !read(part, false) {
			return "", false, false
		}
	}
	return b.String(), b.String() == ".+", true
}

// scriptFindings answers what one-collection.sh (script) breaks of the rules above, against
// cases, bounds and the names a bound may give as its case.
func scriptFindings(script string, cases []storeCase, bounds []scriptBound, names map[string]bool) []string {
	var findings []string
	type span struct{ start, end int }
	var listed []span
	for _, b := range bounds {
		if n := strings.Count(script, b.text); n != 1 {
			findings = append(findings, fmt.Sprintf("one-collection.sh: the bound %q occurs %d times, not once", b.text, n))
			continue
		}
		if !names[b.caseName] {
			findings = append(findings, fmt.Sprintf("one-collection.sh: the bound %q names a case that does not exist: %q", b.text, b.caseName))
		}
		start := strings.Index(script, b.text)
		listed = append(listed, span{start, start + len(b.text)})
	}
	inListed := func(offset int) bool {
		return offset >= 0 && slices.ContainsFunc(listed, func(s span) bool { return s.start <= offset && offset < s.end })
	}
	var wants []string
	for _, c := range cases {
		wants = append(wants, c.want)
	}
	lines := strings.Split(script, "\n")
	finding := func(line int, what string) {
		shown := ""
		if line >= 1 && line <= len(lines) {
			shown = strings.TrimSpace(lines[line-1])
		}
		findings = append(findings, fmt.Sprintf("one-collection.sh:%d: %s: %s", line, what, shown))
	}
	read := readShell(script)
	for _, refused := range read.refused {
		finding(refused.line, refused.what)
	}
	for _, head := range scriptHeadFindings(script) {
		finding(head.line, head.what)
	}
	selfRunning := read.selfRunning()
	for _, name := range slices.Sorted(maps.Keys(selfRunning)) {
		finding(int(read.functions[name].Pos().Line()), "a function that runs itself again ("+selfRunning[name]+"), so nothing but a bound ends it")
	}
	for _, own := range ownFindings(read, withinSeconds, "within", "seconds", "message", "seen") {
		finding(own.line, own.what)
	}
	for _, loop := range read.loops {
		switch {
		case loop.kind == "for":
		case loop.function != "within":
			finding(loop.line, "waits outside within, which ends by its seconds")
		case loop.kind != "until" || !isWithinDeadline(loop.before):
			finding(loop.line, "a wait of within other than its until right after deadline=$(( $(date +%s) + seconds )) alone, so its seconds may not end it")
		}
	}
	if within := read.functions["within"]; within != nil && !takesArguments(read, "within", "seconds", "message", "seen") {
		finding(int(within.Pos().Line()), "within does not take off its seconds, message and file by `seconds=$1 message=$2 seen=$3` and then `shift 3`, its first two statements, with no other shift or set, so a call's first word may not be its seconds, nor its words after the third its command")
	}
	registeredShifts := slices.ContainsFunc(read.commands, func(c shellCommand) bool { return c.function == "registered" && changesArguments(c) })
	for _, c := range read.commands {
		if !c.name.literal {
			// within's "$@" is the command each call of within gives it, read at that call.
			if c.function != "within" || c.name.source != `"$@"` {
				finding(c.line, "runs "+c.name.source+", which is not in scriptCommands, so it may wait with no bound")
			}
			continue
		}
		name := c.name.text
		switch {
		case storeRequests[name]:
			if !c.timed || c.function != "shell" && c.function != "master" {
				finding(c.line, "asks the store outside shell and master, or with no `timeout -s KILL` of its own")
			}
		case name == "timeout" && !killTimeout(c.args):
			finding(c.line, "a timeout not by KILL, which weed shell answers with exit 0, or not of whole seconds above 0, since 0 turns the bound off")
		case name == "exit" && c.function != "fail" && c.function != "usage":
			finding(c.line, "exits outside fail and usage, so with no message for a case to want")
		case name == "return" && (len(c.args) != 1 || c.args[0].source != `"$status"`):
			finding(c.line, `returns a status other than "$status", which under set -e ends the script with no message for a case to want`)
		case name == "tail" && tailFinding(c.args) != "":
			finding(c.line, tailFinding(c.args))
		case name == "set" && setsArguments(c.args):
			finding(c.line, "a set that sets the arguments, so a $1 that a bound reads may not be its call's word; the script needs set -eu alone")
		case name == "trap" && !exitTrap(c.args):
			finding(c.line, "a trap on a condition other than EXIT, whose command the shell runs in the middle of what it runs then, a wait among them, so it may set that wait's deadline again or shift its arguments; the script needs a trap on EXIT alone")
		case !scriptCommands[name] && read.functions[name] == nil:
			finding(c.line, "runs "+name+", which is not in scriptCommands, so it may wait with no bound")
		}
		if name == "within" && read.functions["within"] != nil && len(c.args) > 3 && c.args[3].literal {
			way := "within"
			if command := c.args[3].text; command != "within" {
				way = read.way(command, "within")
			}
			if way != "" {
				finding(c.line, "a within whose command runs within again ("+way+"), which sets the deadline this within reads on every try, so it may never end")
			}
		}
		if (name == "sleep" || name == "wait") && c.function != "within" {
			finding(c.line, "waits outside within, which ends by its seconds")
		}
		if name == "sleep" && (len(c.args) != 1 || !c.args[0].is("1")) {
			finding(c.line, "a sleep other than `sleep 1`, which holds a wait past its seconds")
		}
		if slices.Contains([]string{"[", "test", "grep"}, name) && !c.acted {
			finding(c.line, "a test whose failure nothing acts on (an if, until or while, || or &&), which under set -e ends the script with no message for a case to want")
		}
		if c.function != "fail" && c.function != "usage" && slices.ContainsFunc(c.args, func(w shellWord) bool { return strings.Contains(w.text, "::error::") }) {
			finding(c.line, "prints ::error:: outside fail, so with no case")
		}
		if name == "timeout" || name == "within" || name == "registered" {
			switch seconds, text := boundSeconds(name, c.args); {
			case seconds.literal && digits.MatchString(seconds.text):
				if !inListed(c.offset) {
					finding(c.line, fmt.Sprintf("the bound %q is in no text of scriptBounds, so it has no case", text))
				}
			case name == "within" && c.function == "registered" && seconds.source == `"$1"` && !registeredShifts:
				// registered's own seconds, each call of which is a bound of its own; registered
				// shifts and sets none of its arguments, so its "$1" is each call's first word
			default:
				finding(c.line, fmt.Sprintf("a bound whose seconds this program cannot read (%s), so no case can be listed for it", text))
			}
		}
		if i, gives := messageWord[name]; gives && i < len(c.args) {
			message := c.args[i]
			switch pattern, only, ok := messagePattern(message); {
			case only && c.function == "within":
				// within's own fail "$message": the message each call of within gives it, read at
				// that call.
			case !ok || only:
				finding(c.line, "a message this program cannot read, so no case can want it")
			case !slices.ContainsFunc(wants, regexp.MustCompile("^::error::"+pattern+"$").MatchString):
				finding(c.line, fmt.Sprintf("the message %q is the want of no case, so nothing fails when its check or wait is cut", message.text))
			}
		}
	}
	return findings
}

// goFindings answers what seaweedfs.go (source) breaks of the rules above, against bounds and the
// names a bound may give as its case.
func goFindings(source string, bounds []goBound, names map[string]bool) []string {
	parsed, err := parser.ParseFile(token.NewFileSet(), "seaweedfs.go", source, 0)
	if err != nil {
		return []string{fmt.Sprintf("seaweedfs.go: parse: %v", err)}
	}
	deadlines := map[string]bool{}
	var findings []string
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		sets, sends := false, false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := selector.Sel.Name
			if pkg, ok := selector.X.(*ast.Ident); ok && (pkg.Name == "context" && strings.HasPrefix(name, "With") && (strings.Contains(name, "Timeout") || strings.Contains(name, "Deadline")) ||
				pkg.Name == "time" && slices.Contains([]string{"After", "AfterFunc", "NewTimer", "NewTicker", "Tick", "Sleep"}, name)) {
				sets = true
			} else if name == "Exec" || name == "Do" {
				sends = true
			}
			return true
		})
		listed := slices.ContainsFunc(bounds, func(b goBound) bool { return b.function == fn.Name.Name })
		switch {
		case sets && !listed:
			findings = append(findings, "seaweedfs.go: "+fn.Name.Name+" sets a deadline that no case is listed for in goBounds")
		case sends && !sets:
			findings = append(findings, "seaweedfs.go: "+fn.Name.Name+" sends a request with no deadline of its own")
		}
		if sets {
			deadlines[fn.Name.Name] = true
		}
	}
	for _, b := range bounds {
		if !deadlines[b.function] {
			findings = append(findings, "seaweedfs.go: goBounds lists "+b.function+", which sets no deadline")
		}
		if !names[b.caseName] {
			findings = append(findings, fmt.Sprintf("seaweedfs.go: the deadline of %s names a case that does not exist: %q", b.function, b.caseName))
		}
	}
	return findings
}

// withinSeconds answers whether a word of the script's deadline statement gives its seconds:
// within's own seconds, which its first statement takes from each call's first word.
func withinSeconds(w *syntax.Word) bool { return w.Lit() == "seconds" }

// isWithinDeadline answers whether stmt is within's deadline statement,
// deadline=$(( $(date +%s) + seconds )), alone.
func isWithinDeadline(stmt *syntax.Stmt) bool {
	a, _ := deadlineStatement(stmt, withinSeconds)
	return a != nil
}

// exitTrap answers whether the words of a trap set a command for EXIT alone:
// trap [--] <command> EXIT. A trap on any other condition (CHLD, INT, a signal's number) runs its
// command in the middle of whatever the shell runs then, inside a wait as well, where it may set
// that wait's deadline or shift its function's arguments; the command of a trap on EXIT runs once
// the script ends, inside no wait.
func exitTrap(args []shellWord) bool {
	if len(args) > 0 && args[0].is("--") {
		args = args[1:]
	}
	return len(args) == 2 && args[1].is("EXIT")
}
