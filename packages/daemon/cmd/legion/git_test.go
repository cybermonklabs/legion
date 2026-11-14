package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

// trailerGit is the tests' git: the first git on the test process's PATH that is no script. In a
// Legion pod /opt/legion/bin/git comes first and is a script, so this is /usr/bin/git. On CI it is
// the runner's own git.
func trailerGit(t *testing.T) string {
	t.Helper()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		git := dir + "/git"
		if info, err := os.Stat(git); err == nil && dir != "" && executableFile(info) && !isGitScript(git) {
			return git
		}
	}
	t.Fatal("no git on PATH that is no script: these tests run a real git")
	return ""
}

// trailerLegion is a `legion` for a test's commands: a two-line script that runs this test binary
// as legion (testMainEnv). So the environment of a command that runs it can hold no LEGION_*
// variable.
func trailerLegion(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion")
	writeTrailerScript(t, path, testMainEnv+"=1 exec '"+os.Args[0]+"' \"$@\"\n")
	return path
}

// writeTrailerScript writes an executable /bin/sh script with body at path.
func writeTrailerScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// trailerEnv is the whole environment of a command these tests run, alone or through the wrapper:
// only what the case sets, so no DISPATCH_*, ENVOY_SESSION_ID, CLAUDE_CODE_* or LEGION_* variable
// reaches it but the case's own. HOME is a directory of the test's own and GIT_CONFIG_NOSYSTEM is
// set, so no configuration of the host's applies, and the identity is the test's.
func trailerEnv(t *testing.T, path string, set ...string) []string {
	t.Helper()
	return append([]string{
		"PATH=" + path,
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid",
		"GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid",
		"GIT_EDITOR=true",
	}, set...)
}

// trailerPath is the PATH of a command that runs git by its full path: the tests' git's directory
// and the system's, for the editor and git's own helpers.
func trailerPath(git string) string {
	return filepath.Dir(git) + ":/usr/bin:/bin"
}

// trailerRun runs name with args in dir, with env and stdin, and answers its exit status and what
// it printed. A command that cannot start, or that runs for a minute, fails the test.
func trailerRun(t *testing.T, dir string, env []string, stdin, name string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s %q ran for a minute: %s", name, args, errb.String())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), out.String(), errb.String()
	}
	if err != nil {
		t.Fatalf("run %s %q: %v", name, args, err)
	}
	return 0, out.String(), errb.String()
}

// trailerRepo is a new repository under the test's temporary directory, made by git alone.
func trailerRepo(t *testing.T, git string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if code, _, stderr := trailerRun(t, t.TempDir(), trailerEnv(t, trailerPath(git)), "", git, "init", "-q", "-b", "main", dir); code != 0 {
		t.Fatalf("git init: %s", stderr)
	}
	return dir
}

// trailerMessage is the whole message of dir's HEAD, with the line feed it ends with, read with
// git alone.
func trailerMessage(t *testing.T, git, dir string) string {
	t.Helper()
	code, stdout, stderr := trailerRun(t, dir, trailerEnv(t, trailerPath(git)), "", git, "log", "-1", "--format=%B")
	if code != 0 {
		t.Fatalf("git log: %s", stderr)
	}
	return strings.TrimSuffix(stdout, "\n")
}

// trailerCalls is each run a logging script recorded in log: the arguments of each, as the
// script `printf '%s\037' "$@"` and a line feed write them.
func trailerCalls(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		calls = append(calls, strings.Split(strings.TrimSuffix(line, "\037"), "\037"))
	}
	return calls
}

// trailerLogging is a script body that records its arguments in log (trailerCalls).
func trailerLogging(log string) string {
	return "printf '%s\\037' \"$@\" >> '" + log + "'\nprintf '\\n' >> '" + log + "'\n"
}

// trailerWrapperDir is a directory holding a tmux pane's git: a script that runs legion as
// `legion git --after <its own directory>`.
func trailerWrapperDir(t *testing.T, legion string) string {
	t.Helper()
	bin := t.TempDir()
	writeTrailerScript(t, filepath.Join(bin, "git"), "exec '"+legion+"' git --after '"+bin+"' -- \"$@\"\n")
	return bin
}

// A tmux pane's git runs the next git on PATH after its own directory, a host's git script
// included: the script receives each argument of a commit, the trailers among them, and the
// commit carries both lines. With no LEGION_* variable in the environment, a `git status` and a
// `git commit` still reach the script, and the commit carries the session line alone.
func TestGitWrapperAfterRunsTheNextGitOnPathAScriptIncluded(t *testing.T) {
	git := trailerGit(t)
	legion := trailerLegion(t)
	bin := trailerWrapperDir(t, legion)
	host := t.TempDir()
	log := filepath.Join(host, "calls")
	writeTrailerScript(t, filepath.Join(host, "git"), trailerLogging(log)+"exec '"+git+"' \"$@\"\n")
	path := bin + ":" + host + ":" + trailerPath(git)
	repo := trailerRepo(t, git)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}

	env := trailerEnv(t, path, "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=after-session", "LEGION_ROLE=implementer")
	if code, _, stderr := trailerRun(t, repo, env, "", sh, "-c", "git commit --allow-empty -m one"); code != 0 {
		t.Fatalf("git commit through the pane's git = %d: %s", code, stderr)
	}
	calls := trailerCalls(t, log)
	want := []string{
		"-c", "trailer.Omp-Session.ifExists=addIfDifferent", "-c", "trailer.Legion-Role.ifExists=addIfDifferent",
		"commit", "--trailer", "Omp-Session: after-session", "--trailer", "Legion-Role: implementer",
		"--allow-empty", "-m", "one",
	}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("the host's git received %q, want one run with %q", calls, want)
	}
	if got, want := trailerMessage(t, git, repo), "one\n\nOmp-Session: after-session\nLegion-Role: implementer\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}

	env = trailerEnv(t, path, "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=after-session")
	for _, entry := range env {
		if strings.HasPrefix(entry, "LEGION_") {
			t.Fatalf("the environment holds %s", entry)
		}
	}
	if code, _, stderr := trailerRun(t, repo, env, "", sh, "-c", "git status --short"); code != 0 {
		t.Fatalf("git status through the pane's git = %d: %s", code, stderr)
	}
	if code, _, stderr := trailerRun(t, repo, env, "", sh, "-c", "git commit --allow-empty -m two"); code != 0 {
		t.Fatalf("git commit through the pane's git = %d: %s", code, stderr)
	}
	calls = trailerCalls(t, log)[1:]
	want = []string{"-c", "trailer.Omp-Session.ifExists=addIfDifferent", "commit", "--trailer", "Omp-Session: after-session", "--allow-empty", "-m", "two"}
	if len(calls) != 2 || !slices.Equal(calls[0], []string{"status", "--short"}) || !slices.Equal(calls[1], want) {
		t.Fatalf("the host's git received %q, want [status --short] and %q", calls, want)
	}
	if got, want := trailerMessage(t, git, repo), "two\n\nOmp-Session: after-session\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// realGit is real_git of sjawhar/dotfiles shims/git at fba0c8de6735, copied whole: the git after
// the shim's own PATH entry, leaving out every copy of the shim itself (`-ef "$0"`).
const realGit = `real_git() {
    local IFS=':'
    local dir candidate header seen_self=0 first_above=""
    for dir in $PATH; do
        candidate="$dir/git"
        [[ -f "$candidate" && -x "$candidate" ]] || continue
        if [[ "$candidate" -ef "$0" ]]; then
            seen_self=1
            continue
        fi
        header=""
        IFS= read -r -n 512 -d '' header < "$candidate" || true
        [[ "$header" == *git-worktree-shim* ]] && continue
        if [[ "$seen_self" == 1 ]]; then
            echo "$candidate"
            return
        fi
        [[ -n "$first_above" ]] || first_above="$candidate"
    done
    if [[ "$seen_self" == 0 && -n "$first_above" ]]; then
        echo "$first_above"
        return
    fi
    # A caller that builds its own PATH may leave the real binary off it.
    for candidate in /usr/bin/git /usr/local/bin/git /bin/git; do
        [[ -f "$candidate" && -x "$candidate" ]] || continue
        header=""
        IFS= read -r -n 512 -d '' header < "$candidate" || true
        [[ "$header" == *git-worktree-shim* ]] && continue
        echo "$candidate"
        return
    done
    return 1
}
`

// A host's git wrapper that runs the git after its own PATH entry, as dotfiles' shim does, ends
// when PATH names the pane's git twice around it, and when it names the host's wrapper twice
// around the pane's git: the host's wrapper runs, and the commit carries each line once. Each
// case runs `sh -c 'git commit …'` with the case's PATH in the command's environment, since
// exec.Command("git") would look git up on the test process's own PATH.
func TestGitWrapperAfterEndsAHostWrapperLoop(t *testing.T) {
	git := trailerGit(t)
	legion := trailerLegion(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Dir(git)
	for _, tc := range []struct {
		name string
		path func(bin, shims string) string
		runs int
	}{
		{"the pane's git around the host's", func(bin, shims string) string { return bin + ":" + shims + ":" + bin + ":" + real }, 1},
		{"the host's git around the pane's", func(bin, shims string) string { return shims + ":" + bin + ":" + shims + ":" + real }, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := trailerWrapperDir(t, legion)
			shims := t.TempDir()
			log := filepath.Join(t.TempDir(), "runs")
			wrapper := "#!" + bash + "\nset -euo pipefail\nprintf 'run\\n' >> '" + log + "'\n" + realGit +
				"_real=\"$(real_git)\"\nexec \"$_real\" \"$@\"\n"
			if err := os.WriteFile(filepath.Join(shims, "git"), []byte(wrapper), 0o700); err != nil {
				t.Fatal(err)
			}
			repo := trailerRepo(t, git)
			env := trailerEnv(t, tc.path(bin, shims), "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=loop-session", "LEGION_ROLE=implementer")
			if code, _, stderr := trailerRun(t, repo, env, "", sh, "-c", "git commit --allow-empty -m loop"); code != 0 {
				t.Fatalf("git commit = %d: %s", code, stderr)
			}
			runs, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(runs), "run\n"); got != tc.runs {
				t.Fatalf("the host's wrapper ran %d times, want %d", got, tc.runs)
			}
			if got, want := trailerMessage(t, git, repo), "loop\n\nOmp-Session: loop-session\nLegion-Role: implementer\n"; got != want {
				t.Fatalf("message = %q, want %q", got, want)
			}
		})
	}
}

// A WRAPPED_GIT_AT that names no entry of PATH is stale and no re-entry: the commit through the
// pane's git carries both lines. The next git, the tests' compiled git, is no script, so the
// pane's git takes the variable out, and an alias `!env` run through it prints none.
func TestGitWrapperAfterClearsAMarkerItDoesNotNeed(t *testing.T) {
	git := trailerGit(t)
	legion := trailerLegion(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"1:/nowhere/git", "7:/nowhere/git"} {
		t.Run(marker, func(t *testing.T) {
			bin := trailerWrapperDir(t, legion)
			repo := trailerRepo(t, git)
			env := trailerEnv(t, bin+":"+trailerPath(git), "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=marker-session", "LEGION_ROLE=implementer", wrappedGitAt+"="+marker)
			if code, _, stderr := trailerRun(t, repo, env, "", sh, "-c", "git commit --allow-empty -m marker"); code != 0 {
				t.Fatalf("git commit = %d: %s", code, stderr)
			}
			if got, want := trailerMessage(t, git, repo), "marker\n\nOmp-Session: marker-session\nLegion-Role: implementer\n"; got != want {
				t.Fatalf("message = %q, want %q", got, want)
			}
			code, stdout, stderr := trailerRun(t, repo, env, "", sh, "-c", "git -c 'alias.showenv=!env' showenv")
			if code != 0 {
				t.Fatalf("git showenv = %d: %s", code, stderr)
			}
			if !strings.Contains(stdout, "DISPATCH_SESSION_ID=marker-session") || strings.Contains(stdout, wrappedGitAt+"=") {
				t.Fatalf("the alias's environment = %q, want the session and no %s", stdout, wrappedGitAt)
			}
		})
	}
}

// The pane's git replaces itself with the next git: a script there sees the pid of the process
// that ran legion as its own, and that process's parent as its parent. So a host's wrapper that
// reads its parent (dotfiles' from_jj) sees jj when jj ran git.
func TestGitWrapperAfterExecsTheNextGit(t *testing.T) {
	legion := trailerLegion(t)
	bin, host := t.TempDir(), t.TempDir()
	writeTrailerScript(t, filepath.Join(host, "git"), "echo $$\nreadlink /proc/$PPID/exe\n")
	cmd := exec.Command(legion, "git", "--after", bin, "--", "status")
	cmd.Env = trailerEnv(t, bin+":"+host+":/usr/bin:/bin")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("legion git --after: %v: %s", err, errb.String())
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), strconv.Itoa(pid)+"\n"+self+"\n"; got != want {
		t.Fatalf("the next git printed %q, want its pid and its parent's executable %q", got, want)
	}
}

// workerbin.Install writes a tmux pane's git where the first git on the PATH it is given is not
// the image's wrapper. A commit through the launcher directory reaches the host's git script after
// it, with the trailers among its arguments, and the commit carries both lines. A command in an
// environment that holds no LEGION_* variable still runs through it.
func TestGitWrapperInstallsAPanesTrailers(t *testing.T) {
	git := trailerGit(t)
	legion := trailerLegion(t)
	host := t.TempDir()
	log := filepath.Join(host, "calls")
	writeTrailerScript(t, filepath.Join(host, "git"), trailerLogging(log)+"exec '"+git+"' \"$@\"\n")
	root := t.TempDir()
	if err := workerbin.Install(root, legion, host+":"+filepath.Dir(git)); err != nil {
		t.Fatalf("workerbin.Install: %v", err)
	}
	path := workerbin.LauncherDir(root) + ":" + host + ":" + trailerPath(git)
	repo := trailerRepo(t, git)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}

	env := trailerEnv(t, path, "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=pane-session", "LEGION_ROLE=tester")
	if code, _, stderr := trailerRun(t, repo, env, "", sh, "-c", "git commit --allow-empty -m one"); code != 0 {
		t.Fatalf("git commit through the installed git = %d: %s", code, stderr)
	}
	calls := trailerCalls(t, log)
	want := []string{
		"-c", "trailer.Omp-Session.ifExists=addIfDifferent", "-c", "trailer.Legion-Role.ifExists=addIfDifferent",
		"commit", "--trailer", "Omp-Session: pane-session", "--trailer", "Legion-Role: tester",
		"--allow-empty", "-m", "one",
	}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("the host's git received %q, want one run with %q", calls, want)
	}
	if got, want := trailerMessage(t, git, repo), "one\n\nOmp-Session: pane-session\nLegion-Role: tester\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}

	env = trailerEnv(t, path, "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=pane-session")
	for _, entry := range env {
		if strings.HasPrefix(entry, "LEGION_") {
			t.Fatalf("the environment holds %s", entry)
		}
	}
	if code, _, stderr := trailerRun(t, repo, env, "", sh, "-c", "git status --short"); code != 0 {
		t.Fatalf("git status through the installed git = %d: %s", code, stderr)
	}
	if calls = trailerCalls(t, log); len(calls) != 2 || !slices.Equal(calls[1], []string{"status", "--short"}) {
		t.Fatalf("the host's git received %q, want the commit and then [status --short]", calls)
	}
}

// legion git answers -h, -help and --help with its usage on stderr and exit 0. The usage starts
// `usage: legion git --`, and no line of it is one the docs site's CLI generator reads as a
// subcommand, checked with the generator's own pattern (docs/site/generators/legion-cli.sh).
func TestGitWrapperHelpPrintsUsage(t *testing.T) {
	subcommand := regexp.MustCompile(`^[[:space:]]*(usage:[[:space:]]+)?legion git [a-z]`)
	for _, help := range []string{"-h", "-help", "--help"} {
		var out, errb bytes.Buffer
		code := run(context.Background(), []string{"legion", "git", help}, &out, &errb)
		if code != 0 || out.Len() != 0 || !strings.HasPrefix(errb.String(), "usage: legion git --") {
			t.Fatalf("legion git %s = %d, stdout %q, stderr %q", help, code, out.String(), errb.String())
		}
		for line := range strings.SplitSeq(errb.String(), "\n") {
			if subcommand.MatchString(line) {
				t.Errorf("legion git %s prints %q, which the docs generator reads as a subcommand", help, line)
			}
		}
	}
}

// Any first argument but --real, --after or a help request is a usage error, exit 2, and so is a
// form without its value or its `--`. A help request after `--` is git's.
func TestGitWrapperRefusesAnyOtherForm(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"commit", "-m", "x"},
		{"--real", "/usr/bin/git", "commit"},
		{"--real", "", "--", "status"},
		{"--after"},
		{"--real=/usr/bin/git", "--", "status"},
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), append([]string{"legion", "git"}, args...), &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage: legion git --real") {
			t.Errorf("legion git %q = %d, stderr %q; want exit 2 and the usage", args, code, errb.String())
		}
	}
}
