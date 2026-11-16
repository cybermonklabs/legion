package tmux

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// bigPromptBytes is the size of the role prompt the tests here launch: well over the 131072 bytes
// Linux allows one argv string, NUL included (MAX_ARG_STRLEN), so a launch that carried the prompt
// as an argument could not exec its agent at all.
const bigPromptBytes = 200_000

// longestArgument is the most bytes the tests here allow any one argument the agent is started
// with: a path is a few dozen bytes, and a prompt is far more.
const longestArgument = 4096

// stubAgent writes an executable that stands for the pane's agent only as far as the exec: it
// records the arguments it was started with, each ended by a NUL, into the file STUB_ARGV names,
// and exits 0.
func stubAgent(t *testing.T) string {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "agent-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"+`printf '%s\0' "$@" > "$STUB_ARGV"`+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return stub
}

// bigRolePrompt writes a role prompt file of exactly bigPromptBytes bytes, ending in no newline, so
// a spec that names it alone, with no addressing and no instructions, composes exactly its bytes.
func bigRolePrompt(t *testing.T) (path, text string) {
	t.Helper()
	var b strings.Builder
	for b.Len() < bigPromptBytes {
		b.WriteString("You are the stand-in tester; this line pads your role prompt past one argument's bound.\n")
	}
	text = b.String()[:bigPromptBytes-1] + "."
	path = filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, text
}

// execAsThePane runs words, a command line of the pane's, as the pane's shell runs it — `sh -c`,
// which expands and unquotes the words and execs the first — and returns the arguments the stub
// agent recorded. A shell that could not exec the stub, as one handed a single argument over
// Linux's bound cannot ("Argument list too long"), fails the test with its exit status and what it
// wrote on stderr.
func execAsThePane(t *testing.T, words string) []string {
	t.Helper()
	recorded := filepath.Join(t.TempDir(), "argv")
	cmd := exec.Command(shell, "-c", words)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=/usr/bin:/bin", "STUB_ARGV=" + recorded}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		status := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			status = exitErr.ExitCode()
		}
		t.Fatalf("sh -c on the pane's command exited %d (%v); its stderr:\n%s\nthe command: %s", status, err, stderr.String(), words)
	}
	raw, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatalf("the stub recorded no arguments: %v; sh's stderr:\n%s", err, stderr.String())
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

// checkPromptFileArgument fails the test unless args, the arguments an agent was started with,
// hold no argument longer than longestArgument and name, as flag's value, an absolute path whose
// file holds text byte for byte.
func checkPromptFileArgument(t *testing.T, args []string, flag, text string) {
	t.Helper()
	for i, arg := range args {
		if len(arg) > longestArgument {
			t.Errorf("the agent's argument %d is %d bytes, want none over %d: the prompt rode its argv", i, len(arg), longestArgument)
		}
	}
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("the agent's %d arguments name no %s value", len(args), flag)
	}
	path := args[i+1]
	if len(path) > longestArgument || !filepath.IsAbs(path) {
		t.Fatalf("the agent's %s value is %d bytes and no absolute path, want the path of the prompt's file", flag, len(path))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the agent's %s file: %v", flag, err)
	}
	if string(got) != text {
		t.Errorf("the agent's %s file %s holds %d bytes, want the whole %d-byte prompt byte for byte", flag, path, len(got), len(text))
	}
}

// A role whose system prompt is 200,000 bytes launches in an Oh My Pi pane: the words the shim
// execs, run the way the pane's shell runs them, start the agent with no argument longer than a
// path, and its --append-system-prompt value is the absolute path of a file that holds the whole
// prompt. Were the prompt carried as one argument, by the text itself or by a command substitution
// the pane's shell expands into one, the shell's exec of the agent would fail with E2BIG ("Argument
// list too long"), which the failure prints.
func TestAPromptOver131072BytesLaunchesInAPaneAsAPath(t *testing.T) {
	stub := stubAgent(t)
	f := newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) { o.Launchers = ompOnly(shellprefix.Word(stub)) })
	spec := f.spec(t, claim.RoleTester)
	role, text := bigRolePrompt(t)
	spec.Prompt = runtime.PromptParts{RolePromptPaths: []string{role}}

	argv, _ := newWindow(t, f, spec)
	// The pane's shell command, its last argv word, runs the shim, which execs the words after its
	// own `--` separator: the inner command.
	_, inner, ok := strings.Cut(argv[len(argv)-1], " -- ")
	if !ok {
		t.Fatalf("the pane's command runs no shim:\n%s", argv[len(argv)-1])
	}
	checkPromptFileArgument(t, execAsThePane(t, inner), "--append-system-prompt", text)
}

// A Claude Code role whose system prompt is 200,000 bytes launches in its pane the same way: the
// words the bridge runs Claude Code with, run the way the pane's shell runs them, start it with no
// argument longer than a path, and its --append-system-prompt-file value is the absolute path of a
// file that holds the whole prompt.
func TestAPromptOver131072BytesLaunchesInAClaudeCodePaneAsAPath(t *testing.T) {
	stub := stubAgent(t)
	f := newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) {
		o.Launchers = map[runtime.Backend]Launcher{
			runtime.BackendClaude: NewClaudeLauncher(ClaudeLaunch{Binary: stub, PluginDir: t.TempDir(), Bun: "/bin/true"}),
		}
	})
	spec := f.spec(t, claim.RoleImplementer)
	spec.Backend = runtime.BackendClaude
	role, text := bigRolePrompt(t)
	spec.Prompt = runtime.PromptParts{RolePromptPaths: []string{role}}

	argv, _ := newWindow(t, f, spec)
	// The bridge starts Claude Code on the words after its own `--` separator.
	_, claude, ok := strings.Cut(argv[len(argv)-1], " claude-bridge -- ")
	if !ok {
		t.Fatalf("the pane's command runs no Claude Code bridge:\n%s", argv[len(argv)-1])
	}
	checkPromptFileArgument(t, execAsThePane(t, claude), "--append-system-prompt-file", text)
}
