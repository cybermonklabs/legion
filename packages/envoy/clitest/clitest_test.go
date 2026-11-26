package clitest

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/spf13/cobra"

	"github.com/sjawhar/envoy/internal/fixtureenv"
)

// Every fake is this test binary run again with fakeEnv set: TestMain then runs the command line
// fakeRoot builds instead of the tests, as the tests of internal/cmdtest and of the daemon's
// internal/claudebridge run their stand-ins. One binary serves every fake, so no test builds a
// program, and the cobra fake links the cobra the test binary links.
const (
	// fakeEnv names the fake a run of this binary is: noDefect, the command line Conformance
	// passes, or one of the defects below, each that same command line with one fault put in.
	fakeEnv = "CLITEST_FAKE"
	// fakeURLEnv is the recorder's URL, which the fake's commands call.
	fakeURLEnv = "CLITEST_FAKE_URL"
	// childEnv makes TestConformanceChild run Conformance on the fake or the Case it names, in a
	// process of its own, so a control reads a failing run's report and its exit status.
	childEnv = "CLITEST_CHILD"
	// leakEnv is a variable of no documented run: the run check's own control sets it, and the
	// fake that checks its run must refuse it.
	leakEnv = "CLITEST_LEAK"
)

// The fakes. Each defect is noDefect with one fault put in on purpose, so a control that fails
// on it fails for that fault alone.
const (
	noDefect = "cobra"
	// typoExits1: an unknown command is an error that exits 1, not a usage error, a shell
	// completion does not know among them, since completion is a group of the fake.
	typoExits1 = "typo-exits-1"
	// flagErrorExits1: no SetFlagErrorFunc, so an unknown flag exits 1.
	flagErrorExits1 = "flag-error-exits-1"
	// typoIsData: the root reads a word it does not know as a secret's name and asks the server
	// for it, instead of refusing the word.
	typoIsData = "typo-is-data"
	// emptyBashCompletion: `completion bash` exits 2 and prints nothing.
	emptyBashCompletion = "empty-bash-completion"
	// invalidBashCompletion: `completion bash` prints a script bash -n refuses.
	invalidBashCompletion = "invalid-bash-completion"
	// hiddenCompletes: `internal`, which only a program runs, does not set Hidden.
	hiddenCompletes = "hidden-completes"
	// visibleIsHidden: `alpha`, which a person runs, sets Hidden.
	visibleIsHidden = "visible-is-hidden"
	// prefixCompletesNothing: __complete of a command's first letters lists nothing.
	prefixCompletesNothing = "prefix-completes-nothing"
	// silentHelp: help prints nothing and exits 0.
	silentHelp = "silent-help"
	// helpIgnoresItsCommand: `help <command>` prints the root's help, which names every command,
	// whatever word follows help.
	helpIgnoresItsCommand = "help-ignores-its-command"
	// completionUnknownShellExits0: completion is cobra's own, which has no Run, so a shell it
	// does not know prints its help and exits 0.
	completionUnknownShellExits0 = "completion-unknown-shell-exits-0"
	// helpUnknownTopicExits0: help is cobra's own, which prints the root's help for a topic Find
	// does not resolve and exits 0.
	helpUnknownTopicExits0 = "help-unknown-topic-exits-0"
	// flagsBeforeArgumentsOnly: every command stops reading flags at its first positional, as Go's
	// flag package does, and alpha takes any words, so it runs with --help as one of them.
	flagsBeforeArgumentsOnly = "flags-before-arguments-only"
	// hangingHelp: `help pass` never exits.
	hangingHelp = "hanging-help"
	// checksItsRun: noDefect, refusing every run whose environment, directory or input is not the
	// one the package documentation gives each run.
	checksItsRun = "checks-its-run"
	// helpUnknownTopicExits1: help refuses a topic Find does not resolve with an error that exits
	// 1, not a usage error.
	helpUnknownTopicExits1 = "help-unknown-topic-exits-1"
	// panicsOnUsageError: every usage error dereferences nil, which the Go runtime ends with exit
	// 2 and a trace that names no refused word.
	panicsOnUsageError = "panics-on-usage-error"
	// panicsNamingTheWord: every usage error is panic(err), whose exit is 2 and whose trace starts
	// with the error, so it names the refused word.
	panicsNamingTheWord = "panics-naming-the-word"
	// rootFlagErrorExits1: an unknown flag exits 1 at the root and 2 under every command.
	rootFlagErrorExits1 = "root-flag-error-exits-1"
	// typoMakesATLSContact: each unknown word opens a TLS connection to the recorder first, which
	// sends no request the recorder reads.
	typoMakesATLSContact = "typo-makes-a-tls-contact"
	// completionLeavesAnAsker: completion starts a process that asks the recorder 300 ms later,
	// then refuses the unknown shell and exits.
	completionLeavesAnAsker = "completion-leaves-an-asker"
	// leftoverAsker is the process completionLeavesAnAsker leaves behind.
	leftoverAsker = "leftover-asker"
)

// TestMain runs this binary as the fake fakeEnv names, where it names one, and runs the tests
// otherwise. Only TestMain makes a fake of a copy a test starts: in a build of these tests without
// it, a copy started as a fake runs the tests, each of which starts copies that run them again.
// testBinary refuses to start a copy in such a process (fakeCopyRefusal), and
// TestACopyStartedAsAFakeRunsAsTheFake fails in such a build by name.
func TestMain(m *testing.M) {
	if defect, ok := os.LookupEnv(fakeEnv); ok {
		os.Exit(runFake(defect, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// A copy started as a fake runs as the fake: on -h it prints the fake's help. In a build without
// TestMain, the testing package reads -h before any test runs, prints its own flag usage and
// exits, so this test fails there by name and its copy starts no copy of its own.
func TestACopyStartedAsAFakeRunsAsTheFake(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(testBinary(t), "-h")
	cmd.Env = []string{fakeEnv + "=" + noDefect, oneP}
	out, err := cmd.CombinedOutput()
	if want := fakeRoot(noDefect).Short + "\n"; err != nil || !strings.HasPrefix(string(out), want) {
		t.Fatalf("a copy started as the fake %s with -h: %v, want exit 0 and the fake's help, which starts %q\n%s", noDefect, err, want, out)
	}
}

// fakeCopyRefusal, which testBinary asks before any copy starts, refuses a process that has fakeEnv
// set and no other. While TestMain is in the build no copy reaches it, so this test is what holds
// the refusal.
func TestAProcessStartedAsAFakeStartsNoCopy(t *testing.T) {
	fixtureenv.Unset(t, fakeEnv)
	if err := fakeCopyRefusal(); err != nil {
		t.Fatalf("with %s unset: %v, want no refusal", fakeEnv, err)
	}
	t.Setenv(fakeEnv, noDefect)
	if err := fakeCopyRefusal(); err == nil || !strings.Contains(err.Error(), fakeEnv+"="+noDefect) {
		t.Fatalf("with %s=%s: %v, want a refusal that names it", fakeEnv, noDefect, err)
	}
}

// A command line built the way a conversion builds its root passes every check. Each control
// below runs this command line with one fault put in, so this run is what makes each of them fail
// for its own fault.
func TestConformancePassesACobraFake(t *testing.T) {
	t.Parallel()
	rec := NewRecorder(t)
	Conformance(t, testBinary(t), fakeCase(rec, noDefect))
}

// A binary named by a path relative to the test's own directory runs, though each run starts in a
// directory of its own, against which os/exec would read that path.
func TestConformanceRunsABinaryGivenByARelativePath(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(wd, testBinary(t))
	if err != nil || filepath.IsAbs(relative) || !strings.ContainsRune(relative, filepath.Separator) {
		t.Fatalf("the test binary has no relative path with a separator from %s: %q, %v", wd, relative, err)
	}
	rec := NewRecorder(t)
	Conformance(t, relative, fakeCase(rec, noDefect))
}

// The recorder counts the requests the fake makes at the address Case.Env gives it, so the zero a
// typo must leave is read from a server this binary does call.
func TestRecorderCountsTheBinarysRequests(t *testing.T) {
	t.Parallel()
	rec := NewRecorder(t)
	cmd := exec.Command(testBinary(t), "alpha", "bob")
	cmd.Env = []string{fakeEnv + "=" + noDefect, fakeURLEnv + "=" + rec.URL(), oneP}
	out, err := cmd.CombinedOutput()
	// The recorder answers 404, so the command fails once it has asked.
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("fake alpha bob: %v, want exit 1 on the recorder's 404\n%s", err, out)
	}
	if got, want := rec.Requests(), []string{"GET /v1/alpha?name=bob"}; !slices.Equal(got, want) {
		t.Errorf("the recorder holds %q, want %q", got, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if made, opened, err := rec.contactsSince(ctx, 0, 0); err != nil || len(made) != 1 || opened != 1 {
		t.Errorf("the recorder counts %q and %d connection(s) (%v), want the one request on 1 connection", made, opened, err)
	}
}

// A connection the server has not taken yet when a run ends is counted against that run: the
// count waits for it. The test holds the server's taking of each connection for 300 ms, so a count
// read at once would read none.
func TestTheRecorderCountsAConnectionItHasNotTakenYet(t *testing.T) {
	t.Parallel()
	rec := NewRecorder(t)
	rec.mu.Lock()
	rec.beforeTake = func() { time.Sleep(300 * time.Millisecond) }
	rec.mu.Unlock()
	conn, err := net.Dial("tcp", rec.address())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if made, opened, err := rec.contactsSince(ctx, 0, 0); err != nil || len(made) != 0 || opened != 1 {
		t.Fatalf("the recorder counts %q and %d connection(s) (%v), want no request on 1 connection", made, opened, err)
	}
}

// An unknown command that exits 1 fails the typo check, visible and hidden commands alike, and
// the usage-error check on the shell completion does not know.
func TestConformanceFailsOnAnExit1Typo(t *testing.T) {
	t.Parallel()
	requireFails(t, typoExits1, "", []string{"typo", "usage-error"},
		"alphx: exit 1, want 2",
		"group chilx: exit 1, want 2",
		"internax: exit 1, want 2",
		"completion clitest-unknown-shell: exit 1, want 2",
	)
}

// A `completion bash` that exits 2 and prints nothing fails on both, though bash -n alone passes
// an empty script.
func TestConformanceFailsOnEmptyCompletion(t *testing.T) {
	t.Parallel()
	bash := exec.Command("bash", "-n")
	bash.Stdin = strings.NewReader("")
	if out, err := bash.CombinedOutput(); err != nil {
		t.Fatalf("bash -n on an empty script: %v, want exit 0, which is why the harness counts bytes too\n%s", err, out)
	}
	requireFails(t, emptyBashCompletion, "", []string{"completion/bash"},
		"completion bash: exit 2, want 0",
		"completion bash: printed nothing on standard output",
	)
}

// A root that reads an unknown word as data asks the server for it: the typo check fails on the
// request, on the exit and on the missing suggestion.
func TestConformanceFailsOnARequestAfterATypo(t *testing.T) {
	t.Parallel()
	requireFails(t, typoIsData, "", []string{"typo"},
		"alphx: made 1 request(s), want none: GET /v1/secrets/alphx",
		"alphx: exit 1, want 2",
		`alphx: no line of its output is "alpha" alone`,
	)
}

// A command only a program runs that does not set Hidden is listed by __complete.
func TestConformanceFailsWhenAHiddenCommandCompletes(t *testing.T) {
	t.Parallel()
	requireFails(t, hiddenCompletes, "", []string{"complete"},
		`__complete '': lists the hidden command "internal"`,
	)
}

// An unknown flag that exits 1 fails the unknown-flag check, at the root and under each command.
func TestConformanceFailsOnAFlagErrorThatExits1(t *testing.T) {
	t.Parallel()
	requireFails(t, flagErrorExits1, "", []string{"unknown-flag"},
		"clitest.test --clitest-unknown-flag: exit 1, want 2",
		"alpha --clitest-unknown-flag: exit 1, want 2",
		"group child --clitest-unknown-flag: exit 1, want 2",
	)
}

// A root whose own unknown flag exits 1, while every command's exits 2, fails the unknown-flag
// check on the root's run alone.
func TestConformanceFailsWhenOnlyTheRootsFlagErrorExits1(t *testing.T) {
	t.Parallel()
	out := requireFails(t, rootFlagErrorExits1, "", []string{"unknown-flag"},
		"clitest.test --clitest-unknown-flag: exit 1, want 2",
	)
	if strings.Contains(out, "alpha --clitest-unknown-flag: exit") {
		t.Errorf("the report on %s names alpha's run, whose flag error exits 2\n%s", rootFlagErrorExits1, out)
	}
}

// A binary that the Go runtime ends on each word it should refuse exits 2 as a usage error does:
// a nil dereference names no refused word, and every check that wants a usage error fails.
func TestConformanceFailsOnAPanicThatExits2(t *testing.T) {
	t.Parallel()
	requireFails(t, panicsOnUsageError, "", []string{"typo", "unknown-flag", "usage-error"},
		`clitest.test --clitest-unknown-flag: its output does not name "--clitest-unknown-flag"`,
		`completion clitest-unknown-shell: its output does not name "clitest-unknown-shell"`,
		`help clitest-unknown-topic: its output does not name "clitest-unknown-topic"`,
		`internax: its output does not name "internax"`,
		"alpha --clitest-unknown-flag: the Go runtime ended it",
	)
}

// A panic whose value is the usage error names the refused word and exits 2; the Go runtime's
// own first line is what fails it.
func TestConformanceFailsOnAPanicThatNamesTheWord(t *testing.T) {
	t.Parallel()
	out := requireFails(t, panicsNamingTheWord, "", []string{"typo", "unknown-flag", "usage-error"},
		"alpha --clitest-unknown-flag: the Go runtime ended it",
		"completion clitest-unknown-shell: the Go runtime ended it",
		"internax: the Go runtime ended it",
	)
	if strings.Contains(out, "its output does not name") {
		t.Errorf("the report on %s says a run did not name its word, which each panic's value does\n%s", panicsNamingTheWord, out)
	}
}

// A contact the recorder's handler never reads, a TLS connection, still fails each check that
// wants no contact.
func TestConformanceFailsOnATLSContactAfterATypo(t *testing.T) {
	t.Parallel()
	requireFails(t, typoMakesATLSContact, "", []string{"typo", "usage-error"},
		"alphx: opened 1 connection(s) to the recorder, want none",
		"internax: opened 1 connection(s) to the recorder, want none",
		"completion clitest-unknown-shell: opened 1 connection(s) to the recorder, want none",
	)
}

// A request that a process the binary left behind makes after the binary exited is counted
// against the run that started it.
func TestConformanceFailsOnARequestByAProcessLeftBehind(t *testing.T) {
	t.Parallel()
	requireFails(t, completionLeavesAnAsker, "", []string{"usage-error"},
		"completion clitest-unknown-shell: made 1 request(s), want none: GET /v1/late",
	)
}

// A help that exits 0 and prints nothing fails the help check.
func TestConformanceFailsOnAHelpThatPrintsNothing(t *testing.T) {
	t.Parallel()
	requireFails(t, silentHelp, "", []string{"help"},
		"-h: printed nothing",
		`group child --help: its output does not name "group child"`,
		`help pass: its output does not name "pass"`,
	)
}

// A `help <command>` that prints the root's help fails, though the root's help names alpha.
func TestConformanceFailsWhenHelpOfACommandIsTheRootsHelp(t *testing.T) {
	t.Parallel()
	requireFails(t, helpIgnoresItsCommand, "", []string{"help"},
		"help alpha: printed what clitest.test help prints, the root's help",
		"help pass: printed what clitest.test help prints, the root's help",
	)
}

// A shell completion does not know that prints completion's help and exits 0, as cobra's own
// completion command does, fails the usage-error check.
func TestConformanceFailsWhenAnUnknownShellExits0(t *testing.T) {
	t.Parallel()
	requireFails(t, completionUnknownShellExits0, "", []string{"usage-error"},
		"completion clitest-unknown-shell: exit 0, want 2",
	)
}

// A help topic that names no command and prints the root's help and exits 0, as cobra's own help
// command does, fails the usage-error check.
func TestConformanceFailsWhenAnUnknownHelpTopicExits0(t *testing.T) {
	t.Parallel()
	requireFails(t, helpUnknownTopicExits0, "", []string{"usage-error"},
		"help clitest-unknown-topic: exit 0, want 2",
	)
}

// A help topic that names no command and exits 1 fails the usage-error check.
func TestConformanceFailsWhenAnUnknownHelpTopicExits1(t *testing.T) {
	t.Parallel()
	requireFails(t, helpUnknownTopicExits1, "", []string{"usage-error"},
		"help clitest-unknown-topic: exit 1, want 2",
	)
}

// A command that stops reading flags at its first positional reads --help after one as an
// argument: alpha runs with it and prints something else, and group and group child refuse it.
func TestConformanceFailsWhenAFlagAfterAPositionalDoesNotParse(t *testing.T) {
	t.Parallel()
	requireFails(t, flagsBeforeArgumentsOnly, "", []string{"flag-after-positional"},
		"alpha clitest-positional --help: printed other than clitest.test alpha --help does",
		"group clitest-positional --help: exit 2, want 0",
		"group child clitest-positional --help: exit 1, want 0",
	)
}

// A command a person runs that sets Hidden is not listed, and no typo of it is answered with its
// name.
func TestConformanceFailsWhenAVisibleCommandDoesNotComplete(t *testing.T) {
	t.Parallel()
	requireFails(t, visibleIsHidden, "", []string{"complete", "typo"},
		`__complete '': does not list "alpha"`,
		`__complete alp: does not list "alpha"`,
		`alphx: no line of its output is "alpha" alone`,
	)
}

// A command's first letters that complete nothing fail the complete check.
func TestConformanceFailsWhenAPrefixCompletesNothing(t *testing.T) {
	t.Parallel()
	requireFails(t, prefixCompletesNothing, "", []string{"complete"},
		`__complete alp: does not list "alpha"`,
		`__complete group chi: does not list "child"`,
	)
}

// Each shell's own reader decides a script's syntax: a bash script bash -n refuses fails, bash
// missing from PATH fails, and zsh and fish refuse where they are on PATH. The zsh and fish here
// are stand-ins that refuse every script, so these rows run on a machine with neither installed.
func TestConformanceFailsOnACompletionAShellRefuses(t *testing.T) {
	t.Parallel()
	refusing := t.TempDir()
	for _, shell := range []string{"zsh", "fish"} {
		script := "#!/bin/sh\necho \"$0: refused $*\" >&2\nexit 1\n"
		if err := testexec.WriteFile(filepath.Join(refusing, shell), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		name, defect, path string
		failed             []string
		says               []string
	}{
		{name: "bash refuses", defect: invalidBashCompletion, failed: []string{"completion/bash"},
			says: []string{"bash -n refuses the output of"}},
		{name: "no bash", defect: noDefect, path: t.TempDir(), failed: []string{"completion/bash"},
			says: []string{"bash is not on PATH"}},
		{name: "zsh and fish refuse", defect: noDefect, path: refusing + string(os.PathListSeparator) + os.Getenv("PATH"),
			failed: []string{"completion/fish", "completion/zsh"},
			says:   []string{"zsh -n refuses the output of", "fish --no-execute refuses the output of"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			requireFails(t, row.defect, row.path, row.failed, row.says...)
		})
	}
}

// A run that does not exit is killed at the bound and reported, and the checks after it still run:
// complete, the last of them, passes.
func TestConformanceFailsOnAHangingCommand(t *testing.T) {
	t.Parallel()
	requireFails(t, hangingHelp, "", []string{"help"},
		"help pass: did not exit within 15s",
		"--- PASS: TestConformanceChild/complete (",
	)
}

// Each run gets the environment, the directory and the input the package documentation gives it,
// and nothing of the test process's environment: the fake refuses a run that differs, and the
// test process that runs Conformance here holds childEnv in its environment and a line on its
// standard input, neither of which the fake may see.
func TestConformanceGivesEachRunAnEnvironmentOfItsOwn(t *testing.T) {
	t.Parallel()
	out, err := runChild(t, checksItsRun)
	if err != nil || !strings.Contains(out, "--- PASS: TestConformanceChild ") {
		t.Fatalf("Conformance on %s: %v, want a pass\n%s", checksItsRun, err, out)
	}
}

// The fake that checks its run passes the run the package documentation gives each run, and
// refuses each difference from it, so the pass above is read by a detector that can fail.
func TestTheRunCheckRefusesEachDifferenceFromTheDocumentedRun(t *testing.T) {
	t.Parallel()
	home, tmp := t.TempDir(), t.TempDir()
	// The fake's own selector comes first, so each row that drops a variable keeps it.
	documented := []string{fakeEnv + "=" + checksItsRun, "PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + tmp, oneP}
	for _, row := range []struct {
		name, dir, stdin, says string
		env                    []string
	}{
		{name: "the documented run", dir: home, env: documented},
		{name: "a variable of the test process", dir: home, env: with(documented, leakEnv+"=1"), says: leakEnv + " reached the binary"},
		{name: "no TMPDIR", dir: home, env: slices.Concat(documented[:3], documented[4:]), says: "are not a PATH and two directories"},
		{name: "a run outside HOME", dir: tmp, env: documented, says: "not in HOME"},
		{name: "an input to read", dir: home, env: documented, stdin: "a line\n", says: "standard input is not empty"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(testBinary(t), "-h")
			cmd.Env, cmd.Dir = row.env, row.dir
			if row.stdin != "" {
				cmd.Stdin = strings.NewReader(row.stdin)
			}
			out, err := cmd.CombinedOutput()
			if row.says == "" {
				if err != nil {
					t.Fatalf("the documented run: %v, want exit 0\n%s", err, out)
				}
				return
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), row.says) {
				t.Fatalf("%s: %v, want exit 1 saying %q\n%s", row.name, err, row.says, out)
			}
		})
	}
}

// The typed word differs from the command's name in one letter, the last that can change, and
// names no other command of its group.
func TestMisspellChangesOneLetterAndNamesNoOtherCommand(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		word  string
		taken []string
		want  string
	}{
		{"alpha", nil, "alphx"},
		{"alpha", []string{"alphx"}, "alphq"},
		{"box", nil, "boq"},
		{"pre-push", nil, "pre-pusx"},
		{"Help", nil, "Helx"},
		{"v2", nil, "x2"},
		{"a", []string{"x", "q", "z"}, ""},
		{"42", nil, ""},
	} {
		taken := map[string]bool{}
		for _, name := range row.taken {
			taken[name] = true
		}
		got, ok := misspell(row.word, taken)
		if got != row.want || ok != (row.want != "") {
			t.Errorf("misspell(%q, %q) = %q, %v, want %q", row.word, row.taken, got, ok, row.want)
		}
	}
}

// A Case that cannot be checked whole is refused before any run: one that lists nothing, one with
// no recorder to count requests on, one whose Env points the binary at no address of the
// recorder, one whose command's group is missing, one whose hidden command's group is in neither
// list, one that lists a command twice, one with a command of no words, and one with a word that
// is no command's name.
func TestConformanceRefusesACaseItCannotCheck(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ child, says string }{
		{"case-without-commands", "Case.Commands lists no command, so there is nothing to check: UNKNOWN"},
		{"case-without-server", "Case.Server is nil, so no request a typo makes can be counted: UNKNOWN"},
		{"case-not-pointed-at-server", "Case.Env holds no value that names Case.Server's address 127.0.0.1:"},
		{"case-without-group", `Case.Commands lists "group child" but not "group", the group it is in`},
		{"case-hidden-under-nothing", `Case.Hidden lists "nowhere x", but neither list holds "nowhere", the group it is in`},
		{"case-listing-twice", `"alpha" is listed twice`},
		{"case-with-no-words", "Case lists a command with no words"},
		{"case-with-a-flag", `Case lists "-v", whose word "-v" is no command's name`},
	} {
		t.Run(row.child, func(t *testing.T) {
			t.Parallel()
			out := requireFails(t, row.child, "", nil, "clitest: "+row.says)
			if strings.Contains(out, "=== RUN   TestConformanceChild/") {
				t.Errorf("Conformance on %s started a check, want a refusal before any run\n%s", row.child, out)
			}
		})
	}
}

// TestConformanceChild is the process a control starts: it runs Conformance on the fake or the
// Case childEnv names, and is skipped in every other run.
func TestConformanceChild(t *testing.T) {
	child := os.Getenv(childEnv)
	if child == "" {
		t.Skip("the child of a harness control: it runs only with " + childEnv + " set")
	}
	rec := NewRecorder(t)
	c := fakeCase(rec, child)
	switch child {
	case "case-without-commands":
		c.Commands = nil
	case "case-without-server":
		c.Server = nil
	case "case-not-pointed-at-server":
		c.Env = []string{fakeEnv + "=" + noDefect}
	case "case-without-group":
		c.Commands = [][]string{{"alpha"}, {"group", "child"}}
	case "case-hidden-under-nothing":
		c.Hidden = append(c.Hidden, []string{"nowhere", "x"})
	case "case-listing-twice":
		c.Hidden = append(c.Hidden, []string{"alpha"})
	case "case-with-no-words":
		c.Commands = append(c.Commands, []string{})
	case "case-with-a-flag":
		c.Commands = append(c.Commands, []string{"-v"})
	case hangingHelp:
		// Long enough that every run but the hanging one ends well inside it on a loaded machine.
		before := runTimeout
		runTimeout = 15 * time.Second
		t.Cleanup(func() { runTimeout = before })
	}
	Conformance(t, testBinary(t), c)
}

// fakeCase is the Case of the command line fakeRoot builds, run as defect.
func fakeCase(rec *Recorder, defect string) Case {
	return Case{
		Commands: [][]string{{"alpha"}, {"group"}, {"group", "child"}},
		Hidden:   [][]string{{"internal"}, {"pass"}, {"group", "quiet"}},
		Env:      []string{fakeEnv + "=" + defect, fakeURLEnv + "=" + rec.URL(), oneP},
		Server:   rec,
	}
}

// oneP runs a copy of this test binary on one P. The parallel tests keep dozens of copies alive
// at once, controls and the fakes their Conformance runs, and a copy left to size GOMAXPROCS to
// the host starts a thread for each P its collector's idle mark workers take, about 80 on a
// 288-CPU host.
const oneP = "GOMAXPROCS=1"

// requireFails runs TestConformanceChild on child in a process of its own, with path as its PATH
// when path is not empty, fails unless that process failed, the checks that failed are exactly
// failed, and its report says each of says, and answers that report.
func requireFails(t *testing.T, child, path string, failed []string, says ...string) string {
	t.Helper()
	var env []string
	if path != "" {
		env = append(env, "PATH="+path)
	}
	out, err := runChild(t, child, env...)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("Conformance on %s: %v, want a failed test (exit 1)\n%s", child, err, out)
	}
	if got := failedChecks(out); !slices.Equal(got, failed) {
		t.Errorf("the checks that failed on %s are %q, want %q\n%s", child, got, failed, out)
	}
	for _, want := range says {
		if !strings.Contains(out, want) {
			t.Errorf("the report on %s does not say %q\n%s", child, want, out)
		}
	}
	return out
}

// runChild runs TestConformanceChild on child in a process of its own, on one P (oneP), with env
// added to this process's environment and a line on its standard input, which no run of the
// binary may read, and answers its output and how it ended.
func runChild(t *testing.T, child string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(testBinary(t), "-test.run=^TestConformanceChild$", "-test.v", "-test.count=1")
	cmd.Env = append(append(os.Environ(), oneP, childEnv+"="+child), env...)
	cmd.Stdin = strings.NewReader("standard input no run of the binary may read\n")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// failedLine is a failed subtest of TestConformanceChild in go test -v's output.
var failedLine = regexp.MustCompile(`(?m)^\s*--- FAIL: TestConformanceChild/(\S+) \(`)

// failedChecks is the subtests of TestConformanceChild that failed, sorted, leaving out a subtest
// whose failure is a subtest of its own that failed.
func failedChecks(output string) []string {
	var names []string
	for _, m := range failedLine.FindAllStringSubmatch(output, -1) {
		names = append(names, m[1])
	}
	var leaves []string
	for _, name := range names {
		if !slices.ContainsFunc(names, func(other string) bool { return strings.HasPrefix(other, name+"/") }) {
			leaves = append(leaves, name)
		}
	}
	slices.Sort(leaves)
	return leaves
}

// testBinary is the path of this test binary, from which every fake and every control's child
// runs. It fails t where fakeCopyRefusal refuses.
func testBinary(t *testing.T) string {
	t.Helper()
	if err := fakeCopyRefusal(); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// fakeCopyRefusal is why this process may start no copy of this test binary, nil when it may. A
// process that has fakeEnv set was started as a fake, and TestMain runs such a process as the fake,
// so one that runs the tests runs in a build without TestMain, and a copy started there would run
// the tests again.
func fakeCopyRefusal() error {
	if defect, ok := os.LookupEnv(fakeEnv); ok {
		return fmt.Errorf("this process has %s=%s, so a test started it as a fake, but it runs the tests: this build of the tests has no TestMain that runs a fake, and a copy started here would run the tests again", fakeEnv, defect)
	}
	return nil
}

// usageError is a usage error, on which the fake exits 2 as a conversion's main does.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// exitCode ends the fake with that code and prints nothing.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// runFake runs the fake command line as defect on args and answers its exit code.
func runFake(defect string, args []string) int {
	switch {
	case defect == checksItsRun:
		if problem := runProblem(); problem != "" {
			fmt.Fprintln(os.Stderr, "fake:", problem)
			return 1
		}
		defect = noDefect
	case defect == prefixCompletesNothing && len(args) > 1 && args[0] == cobra.ShellCompRequestCmd && args[len(args)-1] != "":
		fmt.Println(":4")
		return 0
	case defect == hangingHelp && slices.Equal(args, []string{"help", "pass"}):
		time.Sleep(time.Hour)
	case defect == leftoverAsker:
		time.Sleep(300 * time.Millisecond)
		_ = fetch("/v1/late")
		return 0
	}
	root := fakeRoot(defect)
	root.SetArgs(args)
	err := root.Execute()
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	}
	if errors.As(err, new(usageError)) {
		switch defect {
		case panicsOnUsageError:
			var none *usageError
			fmt.Fprintln(os.Stderr, none.error) // a nil dereference, as a crash on a word does
		case panicsNamingTheWord:
			panic(err)
		}
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	if errors.As(err, new(usageError)) {
		return 2
	}
	return 1
}

// runProblem is how this run differs from the one the package documentation gives each run: PATH
// set, HOME and TMPDIR two directories, the run in HOME, standard input empty, and no other
// variable than those and the fake's own Case.Env. It answers "" for a run that does not differ.
func runProblem() string {
	for _, entry := range os.Environ() {
		switch name, _, _ := strings.Cut(entry, "="); name {
		case "PATH", "HOME", "TMPDIR", fakeEnv, fakeURLEnv, "GOMAXPROCS":
		default:
			return name + " reached the binary, which only PATH, HOME, TMPDIR and Case.Env may"
		}
	}
	home, tmp := os.Getenv("HOME"), os.Getenv("TMPDIR")
	if os.Getenv("PATH") == "" || home == "" || tmp == "" || home == tmp {
		return fmt.Sprintf("PATH %q, HOME %q and TMPDIR %q are not a PATH and two directories", os.Getenv("PATH"), home, tmp)
	}
	for _, dir := range []string{home, tmp} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return fmt.Sprintf("%s is not a directory: %v", dir, err)
		}
	}
	wd, err := os.Getwd()
	resolvedHome, homeErr := filepath.EvalSymlinks(home)
	if err != nil || homeErr != nil || wd != resolvedHome {
		return fmt.Sprintf("the run is in %q (%v), not in HOME %q (%v)", wd, err, resolvedHome, homeErr)
	}
	if in, err := io.ReadAll(os.Stdin); err != nil || len(in) > 0 {
		return fmt.Sprintf("standard input is not empty: %q, %v", in, err)
	}
	return ""
}

// fakeRoot is the root the package documentation's "A cobra root that passes" describes, with
// defect's one fault put in.
func fakeRoot(defect string) *cobra.Command {
	root := &cobra.Command{
		Use:                        "fake",
		Short:                      "A command line for clitest's own tests",
		SilenceUsage:               true,
		SilenceErrors:              true,
		SuggestionsMinimumDistance: 2,
		Args:                       unknownCommand(defect),
		RunE:                       showHelp,
	}
	if defect != flagErrorExits1 {
		root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	}
	if defect == typoIsData {
		root.Args = cobra.ArbitraryArgs
		root.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return fetch("/v1/secrets/" + url.PathEscape(args[0]))
		}
	}
	alpha := &cobra.Command{
		Use:    "alpha [name]",
		Short:  "Ask the server about a name",
		Args:   cobra.MaximumNArgs(1),
		Hidden: defect == visibleIsHidden,
		RunE: func(_ *cobra.Command, args []string) error {
			name := "anyone"
			if len(args) == 1 {
				name = args[0]
			}
			return fetch("/v1/alpha?name=" + url.QueryEscape(name))
		},
	}
	alpha.Flags().Int("count", 1, "how many times to ask")
	group := &cobra.Command{
		Use:                        "group",
		Short:                      "Commands of a group",
		SuggestionsMinimumDistance: 2,
		Args:                       unknownCommand(defect),
		RunE:                       showHelp,
	}
	group.AddCommand(
		&cobra.Command{Use: "child", Short: "A command of the group", Args: cobra.NoArgs, Run: say("child")},
		&cobra.Command{Use: "quiet", Short: "A command of the group only a program runs", Hidden: true, Args: cobra.NoArgs, Run: say("quiet")},
	)
	root.AddCommand(alpha, group,
		&cobra.Command{Use: "internal", Short: "A command only a program runs", Hidden: defect != hiddenCompletes, Args: cobra.NoArgs, Run: say("internal")},
		&cobra.Command{
			Use:                "pass [word...]",
			Short:              "Hands its words on unread, as a wrapper of another program does",
			Hidden:             true,
			DisableFlagParsing: true,
			Run:                func(cmd *cobra.Command, args []string) { cmd.Println(strings.Join(args, " ")) },
		},
	)
	switch defect {
	case emptyBashCompletion, invalidBashCompletion:
		root.CompletionOptions.DisableDefaultCmd = true
		root.AddCommand(completionCommand(root, defect))
	case silentHelp:
		root.SetHelpFunc(func(*cobra.Command, []string) {})
	case helpIgnoresItsCommand:
		root.SetHelpCommand(&cobra.Command{
			Use:   "help [command]",
			Short: "Help about any command",
			Run:   func(*cobra.Command, []string) { _ = root.Help() },
		})
	case rootFlagErrorExits1:
		root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return err })
		for _, c := range root.Commands() {
			c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
		}
	case flagsBeforeArgumentsOnly:
		walk(root, func(c *cobra.Command) { c.Flags().SetInterspersed(false) })
		alpha.Args = cobra.ArbitraryArgs
		alpha.RunE = func(cmd *cobra.Command, args []string) error {
			cmd.Println("alpha for", strings.Join(args, " "))
			return nil
		}
	}
	// cobra's own completion and help commands answer a word they do not know with help and exit
	// 0: completion takes a group's Args and Run, and help an Args that refuses a topic.
	if !root.CompletionOptions.DisableDefaultCmd && defect != completionUnknownShellExits0 {
		root.InitDefaultCompletionCmd()
		completion := subcommand(root, "completion")
		completion.SuggestionsMinimumDistance = 2
		completion.Args, completion.RunE = unknownCommand(defect), showHelp
	}
	if defect != helpUnknownTopicExits0 {
		root.InitDefaultHelpCmd()
		subcommand(root, "help").Args = knownTopic(defect)
	}
	return root
}

// unknownCommand refuses a word that names no command of cmd, suggesting the commands it may have
// meant as cobra's own suggestion does ("Did you mean this?", one name a line), with a usage error.
func unknownCommand(defect string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		err := fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
		if names := cmd.SuggestionsFor(args[0]); len(names) > 0 {
			err = fmt.Errorf("%w\n\nDid you mean this?\n\t%s", err, strings.Join(names, "\n\t"))
		}
		switch defect {
		case typoExits1:
			return err
		case typoMakesATLSContact:
			dialer := &net.Dialer{Timeout: 300 * time.Millisecond}
			if conn, err := tls.DialWithDialer(dialer, "tcp", strings.TrimPrefix(os.Getenv(fakeURLEnv), "http://"), nil); err == nil {
				_ = conn.Close()
			}
		case completionLeavesAnAsker:
			if cmd.Name() == "completion" {
				leaveAnAsker()
			}
		}
		return usageError{err}
	}
}

// leaveAnAsker starts this binary as leftoverAsker, which asks the recorder once this process has
// exited, and does not wait for it.
func leaveAnAsker() {
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	asker := exec.Command(self)
	asker.Env = append(os.Environ(), fakeEnv+"="+leftoverAsker)
	if err := asker.Start(); err != nil {
		panic(err)
	}
}

// knownTopic refuses, with a usage error, a help topic Find does not resolve to a command, for
// which cobra's own help prints the root's help and exits 0.
func knownTopic(defect string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if _, rest, err := cmd.Root().Find(args); err != nil || len(rest) > 0 {
			err := fmt.Errorf("unknown help topic %q for %q", strings.Join(args, " "), cmd.Root().CommandPath())
			if defect == helpUnknownTopicExits1 {
				return err
			}
			return usageError{err}
		}
		return nil
	}
}

// subcommand is root's command named name, which the fake has.
func subcommand(root *cobra.Command, name string) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	panic("the fake has no command " + name)
}

// completionCommand is the fake's own completion command, whose bash script defect breaks.
func completionCommand(root *cobra.Command, defect string) *cobra.Command {
	return &cobra.Command{
		Use:       "completion bash|zsh|fish|powershell",
		Short:     "Print a shell's completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				if defect == emptyBashCompletion {
					return exitCode(2)
				}
				_, err := io.WriteString(out, "if then\n")
				return err
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			}
			return usageError{fmt.Errorf("no completion for the shell %q", args[0])}
		},
	}
}

// fetch asks the recorder for path.
func fetch(path string) error {
	base := os.Getenv(fakeURLEnv)
	if base == "" {
		return errors.New(fakeURLEnv + " is not set")
	}
	resp, err := http.Get(base + path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", path, resp.Status)
	}
	return nil
}

func showHelp(cmd *cobra.Command, _ []string) error { return cmd.Help() }

func say(word string) func(*cobra.Command, []string) {
	return func(cmd *cobra.Command, _ []string) { cmd.Println(word) }
}

func walk(c *cobra.Command, f func(*cobra.Command)) {
	f(c)
	for _, sub := range c.Commands() {
		walk(sub, f)
	}
}
