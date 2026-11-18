// Package clitest checks a command line's built binary against the rules each command line with
// commands keeps once it is on cobra: help on every command, a usage error that exits 2 and asks
// no server anything, flags read after a positional, and completion scripts each shell reads. It
// drives the binary only through its command line, so it checks any build, of any framework, in
// any module: a conversion runs it against the build before the conversion, where it must fail,
// and against its own, where it must pass.
//
// It imports the standard library alone, so a module that imports it gains no module through it.
// It is public, rather than under internal/, so another module of this repository (the daemon's,
// through its replace of this one) can import it.
//
// # The runs
//
// Conformance runs the binary, which bin names by a path (made absolute before the first run) or
// by a name on PATH, many times, one run after another and never two at once. Each run:
//
//   - has an environment of PATH (the test process's), HOME and TMPDIR (two directories of the
//     test's own), then Case.Env, a later entry winning over an earlier one of the same name.
//     Nothing else of the test process's environment reaches the binary, so a setting in the
//     shell that runs the tests cannot point the binary at a server the recorder does not see;
//   - runs in HOME and reads an empty standard input;
//   - is, on a Unix system, a process group of its own, which ends when every process of the
//     group has exited, the processes the binary leaves behind included. Elsewhere it ends when
//     the binary's own process exits, and a process it leaves behind is not waited for;
//   - is killed and reported when it has not ended within a minute, with every process of its
//     group;
//   - counts its output as standard output and standard error together, except where a check
//     below names standard output alone;
//   - is the only run a contact with the recorder is counted against when the contact came while
//     it ran. A contact is a request the server read, or a connection made to the recorder, which
//     is counted when the server takes it, so a TLS client counts though it sends no request the
//     server reads. Once the run has ended, the harness connects to the recorder itself and waits
//     until the server takes that connection: the server takes connections in the order they
//     came, so every one made during the run has been counted by then. A request the server reads
//     only after that, on a connection the run made, is listed against a later run.
//
// Where a check names a command, it is its words after the binary's name, as Case lists it:
// "config check" is the words {"config", "check"}.
//
// # The checks
//
// Each kind of check is a subtest of the test Conformance is given, named as below, and each
// failure names the run (the binary's name and its arguments) and what it printed.
//
// A usage error, as the checks below use the words, refuses one word: it exits 2; its output
// names that word; its output holds no line that starts "panic: " or "fatal error: ", with which
// the Go runtime starts its report when it ends a program, also with exit 2; and it makes no
// contact with Case.Server.
//
// help: `bin -h`, `bin --help` and `bin help` exit 0 and print something. For each command of
// Case.Commands, `bin <command> -h`, `bin <command> --help` and `bin help <command>` exit 0 and
// print the command's words, as a usage line does ("Usage: bin config check [flags]"), and print
// other than `bin help` does: a command's help that is the root's help, which names every
// command, fails. For each command of Case.Hidden, `bin help <command>` does the same. A hidden
// command gets no -h or --help run, since a command that hands its words to another program, such
// as a wrapper of git, hands those flags on too.
//
// flag-after-positional: for each command of Case.Commands, `bin <command> clitest-positional
// --help` exits 0 and prints exactly what `bin <command> --help` prints, standard output and
// standard error alike. A command that stops reading flags at its first positional, as Go's flag
// package does, runs with --help as an argument instead.
//
// unknown-flag: `bin --clitest-unknown-flag`, and for each command of Case.Commands `bin <command>
// --clitest-unknown-flag`, is a usage error that refuses the flag.
//
// usage-error: `bin completion clitest-unknown-shell` and `bin help clitest-unknown-topic` are
// each a usage error that refuses the shell or the topic. cobra's own commands answer both with
// help and exit 0: completion has no Run, so Command.execute returns flag.ErrHelp, and help prints
// the root's help when Find resolves no topic, which a root with an Args of its own makes it do
// without an error.
//
// typo: each command of either list, with one letter of its last word changed, is a usage error
// that refuses the typed word. For a command of Case.Commands, a line of the output is also that
// last word alone or the command's words alone, white space around it aside: cobra's suggestion
// ("Did you mean this?" and then one name a line). A hidden command is not checked for that line,
// since cobra suggests no hidden command. The changed letter is the last ASCII letter of the word
// that can become x, q or z, tried in that order, without naming another listed command of the
// same group, or at the top cobra's own help, completion, __complete and __completeNoDesc: "alpha"
// is typed "alphx", and "config check" is typed "config checx". A command whose last word holds
// no such letter fails.
//
// completion: one subtest for each of bash, zsh, fish and powershell, named for the shell.
// `bin completion <shell>` exits 0 and prints more than nothing on standard output, and the shell
// reads that output without running it: `bash -n`, `zsh -n` and `fish --no-execute` on a file
// holding it. bash must be on PATH, and its absence fails the check. zsh and fish are read only
// where they are on PATH, and their absence is logged; no reader is run for powershell. Each check
// is needed: `bash -n` reads an empty script as a valid one.
//
// complete: `bin __complete` with one empty argument lists on standard output, one name a line
// before its tab, every top-level command of Case.Commands and none of Case.Hidden, and `bin
// __complete <group>` with an empty argument after the group does the same for each group of
// Case.Commands. For each command of Case.Commands, `bin __complete <its group> <the first three
// letters of its last word>` lists that word (the whole word where it is shorter). Each exits 0.
// A command hidden under a hidden group is not read here, since nothing lists its group.
//
// # The Case
//
// Conformance refuses a Case it cannot check whole before it runs anything, and says why: one
// that lists no command (there is nothing to check, which is UNKNOWN and no pass), one with no
// Server or with no value of Env that holds Server's host and port (no contact could be counted,
// also UNKNOWN), a command of Case.Commands whose group is not in Case.Commands, a hidden command
// whose group is in neither list, a command listed twice, a command with no words, and a word
// that is empty, starts with "-" or holds white space.
//
// # What it does not check
//
// Whether the program imports cobra, what its commands do with their arguments and their exit
// codes, the shape of its command tree, and every call of it that its callers make: each
// conversion's own tests hold those.
//
// # A cobra root that passes
//
// The root sets SilenceUsage and SilenceErrors, prints the error Execute returns, and exits 2 on
// a usage error and on a flag error (SetFlagErrorFunc wraps it in the program's own usage error).
// cobra answers an unknown word under a command with no Run of its own with that command's help
// and exit 0 (Command.execute returns flag.ErrHelp for a command that is not Runnable), and its
// root suggests a command only when the root has no Args of its own. So the root and each group
// take an Args that refuses a word naming none of their commands with a usage error naming the
// word and adding cmd.SuggestionsFor's names in cobra's format, and a Run that prints their help.
// They also set SuggestionsMinimumDistance to 2: cobra's default of 2 is applied only by its own
// unexported findSuggestions, so SuggestionsFor on a command that leaves the field 0 suggests only
// the names that begin with the typed word. A command only a program runs sets Hidden. After
// root.InitDefaultCompletionCmd(), cobra's completion command takes the same Args and Run as a
// group, so a shell it does not know is a usage error; and help takes an Args that refuses a topic
// Find does not resolve to a command (the help command root.InitDefaultHelpCmd() makes, or one
// root.SetHelpCommand sets). fakeRoot in this package's tests is such a root.
package clitest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Case is a command line's commands and how its binary reaches a server, as Conformance checks
// them.
type Case struct {
	// Commands is every command a person runs, by its words after the binary's name, its groups
	// included: {"version"}, {"config"}, {"config", "check"}. None of them sets Hidden.
	Commands [][]string
	// Hidden is every command only a program runs, which sets Hidden, by its words, as Commands
	// lists them: {"worker-shim"}, {"workspace-init", "fetch"}.
	Hidden [][]string
	// Env is added to each run's environment after PATH, HOME and TMPDIR. It points every address
	// the binary calls at Server.URL(), and one of its values holds Server's host and port.
	Env []string
	// Server is the recorder the binary's Env points it at. It serves one Conformance call at a
	// time.
	Server *Recorder
}

// runTimeout bounds one run of the binary or of a shell: a run still going after it is killed and
// reported, so a binary that waits on a server or on its input fails rather than holding the test.
var runTimeout = time.Minute

const (
	// positional is the argument flag-after-positional puts before --help.
	positional = "clitest-positional"
	// unknownFlag is the flag unknown-flag gives each command.
	unknownFlag = "--clitest-unknown-flag"
	// unknownShell is the shell usage-error asks completion for.
	unknownShell = "clitest-unknown-shell"
	// unknownTopic is the topic usage-error asks help for.
	unknownTopic = "clitest-unknown-topic"
)

// syntaxCheck is the flag each shell reads a script with without running it.
var syntaxCheck = map[string]string{"bash": "-n", "zsh": "-n", "fish": "--no-execute"}

// Conformance checks the binary bin, a path or a name on PATH, against c, as the package
// documentation says.
func Conformance(t *testing.T, bin string, c Case) {
	t.Helper()
	if err := c.validate(); err != nil {
		t.Fatalf("clitest: %v", err)
	}
	// Each run starts in HOME, and os/exec reads a relative path against the run's directory.
	path, err := exec.LookPath(bin)
	if err == nil {
		path, err = filepath.Abs(path)
	}
	if err != nil {
		t.Fatalf("clitest: %v", err)
	}
	home := t.TempDir()
	h := &harness{
		bin:      path,
		name:     filepath.Base(bin),
		dir:      home,
		env:      append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + t.TempDir()}, c.Env...),
		server:   c.Server,
		commands: c.Commands,
		hidden:   c.Hidden,
	}
	t.Run("help", h.help)
	t.Run("flag-after-positional", h.flagAfterPositional)
	t.Run("unknown-flag", h.unknownFlag)
	t.Run("usage-error", h.builtinUsageErrors)
	t.Run("typo", h.typo)
	t.Run("completion", h.completion)
	t.Run("complete", h.complete)
}

// validate refuses a Case Conformance cannot check whole.
func (c Case) validate() error {
	if len(c.Commands) == 0 {
		return errors.New("Case.Commands lists no command, so there is nothing to check: UNKNOWN")
	}
	if c.Server == nil {
		return errors.New("Case.Server is nil, so no request a typo makes can be counted: UNKNOWN")
	}
	if addr := c.Server.address(); !slices.ContainsFunc(c.Env, func(entry string) bool {
		_, value, _ := strings.Cut(entry, "=")
		return strings.Contains(value, addr)
	}) {
		return fmt.Errorf("Case.Env holds no value that names Case.Server's address %s, so no contact a run makes can be counted: UNKNOWN", addr)
	}
	visible := map[string]bool{}
	listed := map[string]bool{}
	for _, list := range [][][]string{c.Commands, c.Hidden} {
		for _, command := range list {
			if len(command) == 0 {
				return errors.New("Case lists a command with no words")
			}
			for _, word := range command {
				if word == "" || strings.HasPrefix(word, "-") || strings.ContainsAny(word, " \t\n") {
					return fmt.Errorf("Case lists %q, whose word %q is no command's name", words(command), word)
				}
			}
			if listed[words(command)] {
				return fmt.Errorf("%q is listed twice", words(command))
			}
			listed[words(command)] = true
		}
	}
	for _, command := range c.Commands {
		visible[words(command)] = true
	}
	for _, command := range c.Commands {
		if group := command[:len(command)-1]; len(group) > 0 && !visible[words(group)] {
			return fmt.Errorf("Case.Commands lists %q but not %q, the group it is in", words(command), words(group))
		}
	}
	for _, command := range c.Hidden {
		if group := command[:len(command)-1]; len(group) > 0 && !listed[words(group)] {
			return fmt.Errorf("Case.Hidden lists %q, but neither list holds %q, the group it is in", words(command), words(group))
		}
	}
	return nil
}

// harness is one Conformance call's binary, the environment and directory it runs in, and the
// Case's commands.
type harness struct {
	bin      string
	name     string
	dir      string
	env      []string
	server   *Recorder
	commands [][]string
	hidden   [][]string
}

func (h *harness) help(t *testing.T) {
	var root result
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		root = h.run(args...)
		if root.wantExit(t, 0) && strings.TrimSpace(root.stdout+root.stderr) == "" {
			t.Errorf("%s: printed nothing", root.argv)
		}
	}
	// root is the run of `bin help`, which a command's own help must not repeat.
	for _, command := range h.commands {
		for _, args := range [][]string{with(command, "-h"), with(command, "--help"), slices.Concat([]string{"help"}, command)} {
			h.run(args...).wantHelpOf(t, command, root)
		}
	}
	for _, command := range h.hidden {
		h.run(slices.Concat([]string{"help"}, command)...).wantHelpOf(t, command, root)
	}
}

func (h *harness) flagAfterPositional(t *testing.T) {
	for _, command := range h.commands {
		want := h.run(with(command, "--help")...)
		if !want.wantExit(t, 0) {
			continue
		}
		got := h.run(with(command, positional, "--help")...)
		if got.wantExit(t, 0) && (got.stdout != want.stdout || got.stderr != want.stderr) {
			t.Errorf("%s: printed other than %s does, so the --help after %q was not read as a flag%s",
				got.argv, want.argv, positional, got.output())
		}
	}
}

func (h *harness) unknownFlag(t *testing.T) {
	for _, command := range slices.Concat([][]string{nil}, h.commands) {
		h.wantUsageError(t, unknownFlag, with(command, unknownFlag)...)
	}
}

// builtinUsageErrors asks cobra's own completion and help commands for a word neither knows.
func (h *harness) builtinUsageErrors(t *testing.T) {
	h.wantUsageError(t, unknownShell, "completion", unknownShell)
	h.wantUsageError(t, unknownTopic, "help", unknownTopic)
}

func (h *harness) typo(t *testing.T) {
	for _, command := range h.commands {
		h.typoOf(t, command, true)
	}
	for _, command := range h.hidden {
		h.typoOf(t, command, false)
	}
}

// typoOf runs command with one letter of its last word changed; suggested says whether the output
// must name the command, as cobra's suggestion of a command that is not hidden does.
func (h *harness) typoOf(t *testing.T, command []string, suggested bool) {
	t.Helper()
	group, word := command[:len(command)-1], command[len(command)-1]
	typed, ok := misspell(word, h.siblings(group))
	if !ok {
		t.Errorf("clitest: %q has no ASCII letter that can become x, q or z without naming another command of its group", words(command))
		return
	}
	r := h.wantUsageError(t, typed, with(group, typed)...)
	if suggested && !r.hasLine(word, words(command)) {
		t.Errorf("%s: no line of its output is %q alone, as cobra's suggestion of it is%s", r.argv, word, r.output())
	}
}

func (h *harness) completion(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			r := h.run("completion", shell)
			r.wantExit(t, 0)
			if r.stdout == "" {
				t.Errorf("%s: printed nothing on standard output%s", r.argv, r.output())
				return
			}
			h.readScript(t, shell, r)
		})
	}
}

// readScript has shell read the script r printed without running it, where the package
// documentation says it must or may.
func (h *harness) readScript(t *testing.T, shell string, r result) {
	t.Helper()
	flag, ok := syntaxCheck[shell]
	if !ok {
		return
	}
	reader, err := exec.LookPath(shell)
	if err != nil {
		if shell == "bash" {
			t.Errorf("bash is not on PATH, so the output of %s cannot be read with bash -n", r.argv)
		} else {
			t.Logf("%s is not on PATH, so the output of %s is not read with %s %s", shell, r.argv, shell, flag)
		}
		return
	}
	script := filepath.Join(t.TempDir(), "completion."+shell)
	if err := os.WriteFile(script, []byte(r.stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := h.command(ctx, reader, flag, script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("%s %s refuses the output of %s: %v\n%s", shell, flag, r.argv, err, out)
	}
}

func (h *harness) complete(t *testing.T) {
	groups := [][]string{nil}
	for _, command := range h.commands {
		if len(children(h.commands, command))+len(children(h.hidden, command)) > 0 {
			groups = append(groups, command)
		}
	}
	for _, group := range groups {
		r := h.run(slices.Concat([]string{"__complete"}, group, []string{""})...)
		if !r.wantExit(t, 0) {
			continue
		}
		listed := completions(r.stdout)
		for _, name := range children(h.commands, group) {
			if !listed[name] {
				t.Errorf("%s: does not list %q%s", r.argv, name, r.output())
			}
		}
		for _, name := range children(h.hidden, group) {
			if listed[name] {
				t.Errorf("%s: lists the hidden command %q%s", r.argv, name, r.output())
			}
		}
	}
	for _, command := range h.commands {
		group, word := command[:len(command)-1], command[len(command)-1]
		r := h.run(slices.Concat([]string{"__complete"}, group, []string{word[:min(3, len(word))]})...)
		if r.wantExit(t, 0) && !completions(r.stdout)[word] {
			t.Errorf("%s: does not list %q%s", r.argv, word, r.output())
		}
	}
}

// children is the last word of each command of list directly under group.
func children(list [][]string, group []string) []string {
	var names []string
	for _, command := range list {
		if len(command) == len(group)+1 && slices.Equal(command[:len(group)], group) {
			names = append(names, command[len(group)])
		}
	}
	return names
}

// siblings is every name a word under group may not be typed as: each listed command's name
// there, and at the top the commands cobra adds itself.
func (h *harness) siblings(group []string) map[string]bool {
	names := map[string]bool{}
	if len(group) == 0 {
		for _, name := range []string{"help", "completion", "__complete", "__completeNoDesc"} {
			names[name] = true
		}
	}
	for _, list := range [][][]string{h.commands, h.hidden} {
		for _, name := range children(list, group) {
			names[name] = true
		}
	}
	return names
}

// wantUsageError runs args, which must be a usage error that refuses word: exit 2, an output that
// names word and holds no line the Go runtime starts a crash with, and no contact with the
// recorder.
func (h *harness) wantUsageError(t *testing.T, word string, args ...string) result {
	t.Helper()
	r := h.run(args...)
	if r.wantExit(t, 2) {
		if r.crashed() {
			t.Errorf("%s: the Go runtime ended it, with a panic or a fatal error, which exits 2 as a usage error does%s", r.argv, r.output())
		}
		if !strings.Contains(r.stdout+r.stderr, word) {
			t.Errorf("%s: its output does not name %q, the word it refuses%s", r.argv, word, r.output())
		}
	}
	switch {
	case len(r.requests) > 0:
		t.Errorf("%s: made %d request(s), want none: %s%s", r.argv, len(r.requests), strings.Join(r.requests, ", "), r.output())
	case r.connections > 0:
		t.Errorf("%s: opened %d connection(s) to the recorder, want none%s", r.argv, r.connections, r.output())
	}
	return r
}

// result is one run of the binary.
type result struct {
	// argv is how a report names the run: the binary's name and its arguments.
	argv           string
	code           int
	stdout, stderr string
	// failed is why the run has no exit code of its own: it did not start, it or a process of its
	// group did not exit within runTimeout, it was ended by a signal, or the recorder's count
	// after it could not be read.
	failed string
	// requests and connections are what the recorder took during the run: the requests it read,
	// and how many connections were made to it.
	requests    []string
	connections int
}

// crashLine is the line the Go runtime starts its report with when it ends a program, with exit
// 2, on an unrecovered panic or a fatal error.
var crashLine = regexp.MustCompile(`(?m)^(panic|fatal error): `)

// crashed answers whether the Go runtime ended the run.
func (r result) crashed() bool { return crashLine.MatchString(r.stdout + "\n" + r.stderr) }

func (h *harness) run(args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	requests, connections := h.server.marks()
	cmd := h.command(ctx, h.bin, args...)
	inOwnGroup(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exited := ctx.Err() == nil
	groupEnded := cmd.Process == nil || waitGroup(ctx, cmd)
	r := result{argv: h.display(args), stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case !exited:
		r.failed = fmt.Sprintf("did not exit within %s", runTimeout)
	case !groupEnded:
		r.failed = fmt.Sprintf("exited, but a process it started was still running %s after the run began", runTimeout)
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		r.code = exit.ExitCode()
	default:
		r.failed = err.Error()
	}
	settle, cancelSettle := context.WithTimeout(context.Background(), runTimeout)
	defer cancelSettle()
	r.requests, r.connections, err = h.server.contactsSince(settle, requests, connections)
	if err != nil && r.failed == "" {
		r.failed = fmt.Sprintf("the recorder's count after the run could not be read: %v: UNKNOWN", err)
	}
	return r
}

// command is a run of path with args in the environment and the directory every run gets, killed
// when ctx ends.
func (h *harness) command(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env, cmd.Dir, cmd.WaitDelay = h.env, h.dir, time.Second
	return cmd
}

// display is how a report names a run of args.
func (h *harness) display(args []string) string {
	quoted := []string{h.name}
	for _, arg := range args {
		switch {
		case arg == "":
			arg = "''"
		case strings.ContainsAny(arg, " \t\n'\"\\$`"):
			arg = strconv.Quote(arg)
		}
		quoted = append(quoted, arg)
	}
	return strings.Join(quoted, " ")
}

// wantExit fails the test unless the run exited with code, and answers whether it did.
func (r result) wantExit(t *testing.T, code int) bool {
	t.Helper()
	switch {
	case r.failed != "":
		t.Errorf("%s: %s%s", r.argv, r.failed, r.output())
	case r.code != code:
		t.Errorf("%s: exit %d, want %d%s", r.argv, r.code, code, r.output())
	default:
		return true
	}
	return false
}

// wantHelpOf fails the test unless the run exited 0 and printed command's own help: its words,
// and other than root, the run of `bin help`, when that run exited 0 and printed something.
func (r result) wantHelpOf(t *testing.T, command []string, root result) {
	t.Helper()
	if !r.wantExit(t, 0) {
		return
	}
	rootPrinted := root.failed == "" && root.code == 0 && strings.TrimSpace(root.stdout+root.stderr) != ""
	switch {
	case rootPrinted && r.stdout == root.stdout && r.stderr == root.stderr:
		t.Errorf("%s: printed what %s prints, the root's help%s", r.argv, root.argv, r.output())
	case !strings.Contains(r.stdout+r.stderr, words(command)):
		t.Errorf("%s: its output does not name %q%s", r.argv, words(command), r.output())
	}
}

// hasLine answers whether a line of the run's output is one of names, white space around it aside.
func (r result) hasLine(names ...string) bool {
	for line := range strings.Lines(r.stdout + "\n" + r.stderr) {
		if slices.Contains(names, strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// output is the run's output as a report quotes it, each stream cut at outputLimit bytes.
func (r result) output() string {
	return fmt.Sprintf("\n  stdout: %q\n  stderr: %q", cut(r.stdout), cut(r.stderr))
}

const outputLimit = 2048

func cut(s string) string {
	if len(s) <= outputLimit {
		return s
	}
	return s[:outputLimit] + "…"
}

// completions is each name a __complete run listed: every line of its standard output but cobra's
// directive (":4"), up to its tab.
func completions(stdout string) map[string]bool {
	names := map[string]bool{}
	for line := range strings.Lines(stdout) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		name, _, _ := strings.Cut(line, "\t")
		names[name] = true
	}
	return names
}

// misspell is word with one letter changed: its last ASCII letter that can become x, q or z, in
// that order, without naming one of taken. It answers false when no letter can change so.
func misspell(word string, taken map[string]bool) (string, bool) {
	for i := len(word) - 1; i >= 0; i-- {
		c := word[i] | 0x20
		if c < 'a' || c > 'z' {
			continue
		}
		for _, letter := range "xqz" {
			typed := word[:i] + string(letter) + word[i+1:]
			if byte(letter) != c && !taken[typed] {
				return typed, true
			}
		}
	}
	return "", false
}

func words(command []string) string { return strings.Join(command, " ") }

// with is command followed by more, in a slice of its own.
func with(command []string, more ...string) []string {
	return slices.Concat(command, more)
}

// Recorder is an HTTP server that answers every request 404 and records it, and counts every
// connection made to it, for a binary's Case.Env to point each address it calls at, so
// Conformance counts what a run asked.
type Recorder struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []string
	// connections is the client address of each connection the server took, in the order it took
	// them, but for the ones contactsSince opened itself.
	connections []string
	// taken is closed, and a new one made, each time the server takes a connection.
	taken chan struct{}
	// beforeTake, when set, runs each time the server takes a connection, before it is counted: a
	// test's hold on the count.
	beforeTake func()
}

// NewRecorder starts a Recorder on a loopback port, closed when the test ends.
func NewRecorder(t testing.TB) *Recorder {
	t.Helper()
	r := &Recorder{taken: make(chan struct{})}
	r.server = httptest.NewUnstartedServer(http.HandlerFunc(r.record))
	r.server.Config.ConnState = r.connState
	r.server.Start()
	t.Cleanup(r.server.Close)
	return r
}

func (r *Recorder) record(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.Method+" "+req.URL.RequestURI())
	r.mu.Unlock()
	http.Error(w, "clitest: the recorder serves nothing", http.StatusNotFound)
}

// connState counts each connection the server takes, whether or not it then reads a request on
// it. The server takes its connections one at a time, in the order its listener queued them, and
// calls this before it takes the next.
func (r *Recorder) connState(c net.Conn, state http.ConnState) {
	if state != http.StateNew {
		return
	}
	r.mu.Lock()
	hold := r.beforeTake
	r.mu.Unlock()
	if hold != nil {
		hold()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connections = append(r.connections, c.RemoteAddr().String())
	close(r.taken)
	r.taken = make(chan struct{})
}

// marks is how many requests and connections the recorder has counted.
func (r *Recorder) marks() (requests, connections int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests), len(r.connections)
}

// contactsSince answers the requests and the number of connections counted after the marks given,
// once the server has taken every connection made to it before the call. It connects to the server
// itself and waits until the server takes that connection: the server takes connections in the
// order they came, so every earlier one has been counted by then. Its own connection is not
// counted.
func (r *Recorder) contactsSince(ctx context.Context, requests, connections int) ([]string, int, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", r.address())
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = conn.Close() }()
	own := conn.LocalAddr().String()
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		if i := slices.Index(r.connections[connections:], own); i >= 0 {
			r.connections = slices.Delete(r.connections, connections+i, connections+i+1)
			return slices.Clone(r.requests[requests:]), len(r.connections) - connections, nil
		}
		taken := r.taken
		r.mu.Unlock()
		select {
		case <-taken:
			r.mu.Lock()
		case <-ctx.Done():
			r.mu.Lock()
			return nil, 0, ctx.Err()
		}
	}
}

// address is the recorder's host and port, 127.0.0.1:<port>.
func (r *Recorder) address() string { return r.server.Listener.Addr().String() }

// URL is the recorder's base URL, http://127.0.0.1:<port>.
func (r *Recorder) URL() string { return r.server.URL }

// Requests is each request the recorder has answered, in the order they came, as their method and
// path with its query: "GET /v1/settings".
func (r *Recorder) Requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}
