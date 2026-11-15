package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

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
// it printed (trailerRunInput).
func trailerRun(t *testing.T, dir string, env []string, stdin, name string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return trailerRunInput(t, dir, env, strings.NewReader(stdin), name, args...)
}

// trailerRunInput runs name with args in dir, with env, and with stdin as its standard input: a
// pipe for any reader but an *os.File, which the command gets as its descriptor 0. It answers the
// exit status and what the command printed. A command that cannot start, or that runs for a
// minute, fails the test. The command runs in a process group of its own, and the minute kills the
// whole group, so a chain of gits that run one another without end stops there too and holds no
// output pipe open.
func trailerRunInput(t *testing.T, dir string, env []string, stdin io.Reader, name string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
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
	if code, _, stderr := trailerRun(t, t.TempDir(), trailerEnv(t, trailerPath(git)), "", git, fixturegit.InitWorkTree(dir, "main")[1:]...); code != 0 {
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
	git := fixturegit.CompiledGit(t)
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
	want := []string{"commit", "--allow-empty", "-m", "one", "--trailer", "Omp-Session: after-session", "--trailer", "Legion-Role: implementer"}
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
	want = []string{"commit", "--allow-empty", "-m", "two", "--trailer", "Omp-Session: after-session"}
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
// around the pane's git: the host's wrapper runs, and the commit carries each line once. It ends
// too when the host's wrapper puts a new entry in front of PATH before it runs the next git, which
// moves every entry's index.
//
// A host's wrapper that runs the first git on PATH that is not itself ends too: a compiled one,
// whose runs the chain lists as it lists a script's (the commit is made once, with both lines),
// and a script reached through two PATH entries that name one directory, one a symbolic link to
// the other (the chain compares files, so the script runs once). Where PATH holds no real git,
// two such scripts, or one compiled wrapper, run one another and the pane's git without end, and
// the pane's git stops the chain with exit 127 and says so.
//
// Each case runs `sh -c 'git …'` with the case's PATH in the command's environment, since
// exec.Command("git") would look git up on the test process's own PATH.
func TestGitWrapperAfterEndsAHostWrapperLoop(t *testing.T) {
	git := fixturegit.CompiledGit(t)
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
	paneAround := func(bin, shims string) string { return bin + ":" + shims + ":" + bin + ":" + real }
	hostAround := func(bin, shims string) string { return shims + ":" + bin + ":" + shims + ":" + real }
	for _, tc := range []struct {
		name    string
		path    func(bin, shims string) string
		prepend bool
		runs    int
	}{
		{"the pane's git around the host's", paneAround, false, 1},
		{"the host's git around the pane's", hostAround, false, 2},
		{"the pane's git around a host's that puts an entry in front of PATH", paneAround, true, 1},
		{"a host's git that puts an entry in front of PATH around the pane's", hostAround, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := trailerWrapperDir(t, legion)
			shims := t.TempDir()
			log := filepath.Join(t.TempDir(), "runs")
			prepend := ""
			if tc.prepend {
				prepend = "PATH='" + t.TempDir() + "':\"$PATH\"\nexport PATH\n"
			}
			wrapper := "#!" + bash + "\nset -euo pipefail\nprintf 'run\\n' >> '" + log + "'\n" + prepend + realGit +
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

	compiled := goGitProgram(t, hostGitSource)
	firstNotSelf := func(log string) string {
		return "#!" + bash + "\nprintf 'run\\n' >> '" + log + "'\n" +
			"IFS=:\nfor dir in $PATH; do\n" +
			"  [[ -n $dir && -f $dir/git && -x $dir/git && ! $dir/git -ef $0 ]] && exec \"$dir/git\" \"$@\"\n" +
			"done\nexit 127\n"
	}
	for _, tc := range []struct {
		name string
		// path is the case's PATH, given the pane's git's directory.
		path func(t *testing.T, bin string) (path, log string)
		// runs is how often the host's wrapper that log records runs, -1 for any number.
		runs int
		// loop is whether the command ends with the pane's git's loop message, and else it commits.
		loop bool
	}{
		{"a compiled host's git that runs the first git that is not itself", func(t *testing.T, bin string) (string, string) {
			host := t.TempDir()
			copyExecutable(t, compiled, filepath.Join(host, "git"))
			return bin + ":" + host + ":" + real, filepath.Join(host, "runs")
		}, -1, false},
		{"a host's script on two PATH entries of one directory", func(t *testing.T, bin string) (string, string) {
			dir := t.TempDir()
			host, alias := filepath.Join(dir, "h"), filepath.Join(dir, "hdup")
			if err := os.Mkdir(host, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(host, alias); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "runs")
			if err := os.WriteFile(filepath.Join(host, "git"), []byte(firstNotSelf(log)), 0o700); err != nil {
				t.Fatal(err)
			}
			return bin + ":" + host + ":" + alias + ":" + real, log
		}, 1, false},
		{"two host scripts and no real git", func(t *testing.T, bin string) (string, string) {
			one, two := t.TempDir(), t.TempDir()
			log := filepath.Join(t.TempDir(), "runs")
			for _, host := range []string{one, two} {
				if err := os.WriteFile(filepath.Join(host, "git"), []byte(firstNotSelf(log)), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			return bin + ":" + one + ":" + two, log
		}, 2, true},
		// The compiled wrapper is also the helper here, so the commit's reads run it too.
		{"a compiled host's git and no real git", func(t *testing.T, bin string) (string, string) {
			host := t.TempDir()
			copyExecutable(t, compiled, filepath.Join(host, "git"))
			return bin + ":" + host, filepath.Join(host, "runs")
		}, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := trailerWrapperDir(t, legion)
			path, log := tc.path(t, bin)
			repo := trailerRepo(t, git)
			env := trailerEnv(t, path+":/usr/bin:/bin", "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=loop-session", "LEGION_ROLE=implementer")
			if tc.loop {
				// /usr/bin and /bin come after the hosts here only for the shell's own tools: the
				// case's gits are the hosts', so PATH ends at the last host for git.
				env = trailerEnv(t, path, "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=loop-session", "LEGION_ROLE=implementer")
			}
			code, _, stderr := trailerRun(t, repo, env, "", sh, "-c", "git commit --allow-empty -m loop")
			if tc.loop {
				if want := "legion git: the git programs on PATH after " + bin + " run one another in a loop"; code != 127 || !strings.Contains(stderr, want) {
					t.Fatalf("git commit = %d, stderr %q; want 127 and %q", code, stderr, want)
				}
			} else {
				if code != 0 {
					t.Fatalf("git commit = %d: %s", code, stderr)
				}
				if got, want := trailerMessage(t, git, repo), "loop\n\nOmp-Session: loop-session\nLegion-Role: implementer\n"; got != want {
					t.Fatalf("message = %q, want %q", got, want)
				}
				if _, count, _ := trailerRun(t, repo, trailerEnv(t, trailerPath(git)), "", git, "rev-list", "--count", "HEAD"); count != "1\n" {
					t.Fatalf("the repository holds %q commits, want 1", count)
				}
			}
			if tc.runs >= 0 {
				runs, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Count(string(runs), "run\n"); got != tc.runs {
					t.Fatalf("the host's wrapper ran %d times, want %d", got, tc.runs)
				}
			}
		})
	}
}

// goGitProgram is a compiled program named git, built from source, one main package, with the Go
// toolchain that runs the tests.
func goGitProgram(t *testing.T, source string) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module standin\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "git")
	build := exec.Command(goTool, "build", "-o", out, ".")
	build.Dir = src
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build a compiled git: %v: %s", err, output)
	}
	return out
}

// hostGitSource is a compiled host's git wrapper: it writes a line to `runs` beside itself on each
// run, and runs the first git on PATH that is not itself (os.SameFile), as a compiled multiplexer
// of a host does.
const hostGitSource = `package main

import (
	"os"
	"path/filepath"
	"syscall"
)

func main() {
	self, err := os.Executable()
	if err != nil {
		os.Exit(126)
	}
	if f, err := os.OpenFile(filepath.Join(filepath.Dir(self), "runs"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.WriteString("run\n")
		f.Close()
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		os.Exit(126)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		git := filepath.Join(dir, "git")
		info, err := os.Stat(git)
		if dir == "" || err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || os.SameFile(info, selfInfo) {
			continue
		}
		syscall.Exec(git, append([]string{git}, os.Args[1:]...), os.Environ())
		os.Exit(126)
	}
	os.Exit(127)
}
`

// copyExecutable copies the program at from to an executable file at to.
func copyExecutable(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o700); err != nil {
		t.Fatal(err)
	}
}

// A WRAPPED_GIT_AT that names no git on PATH is stale and no re-entry: the commit through the
// pane's git carries both lines. Among the values are some of another form. The pane's git starts
// a new chain for the git it runs, so an alias `!env` run through it sees the variable name that
// git alone: a git that git itself starts never reaches the pane's git, so the value in a real
// git's environment changes nothing.
func TestGitWrapperAfterClearsAMarkerItDoesNotNeed(t *testing.T) {
	git := fixturegit.CompiledGit(t)
	legion := trailerLegion(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"/nowhere/git", "/nowhere/git:/elsewhere/git", "1:/nowhere/git", "7:/nowhere/git"} {
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
			if want := wrappedGitAt + "=0:" + git; !strings.Contains(stdout, "DISPATCH_SESSION_ID=marker-session") || !slices.Contains(strings.Split(stdout, "\n"), want) {
				t.Fatalf("the alias's environment = %q, want the session and %s", stdout, want)
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
	git := fixturegit.CompiledGit(t)
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
	want := []string{"commit", "--allow-empty", "-m", "one", "--trailer", "Omp-Session: pane-session", "--trailer", "Legion-Role: tester"}
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

// gitSubcommand finds the subcommand of each argument list of the table that the pod's wrapper's
// test reads too (fixturegit.Subcommands), so legion git and packages/daemon/docker/git agree on
// which command is a commit.
func TestGitWrapperFindsTheSubcommandThePodsWrapperFinds(t *testing.T) {
	for _, tc := range fixturegit.Subcommands() {
		if got := gitSubcommand(tc.Args); got != tc.Sub {
			t.Errorf("gitSubcommand(%q) = %d, want %d", tc.Args, got, tc.Sub)
		}
	}
}

// A word that ends git's own options before any subcommand makes git run its help or version
// command, or print a path and end, whatever follows: `git --help commit` is no commit. legion git
// hands git each command of the shared table (fixturegit.Subcommands) that has no subcommand as it
// came, under either form. Under --real the git it runs also answers its reads, and that git runs
// once, so legion git read nothing first.
func TestGitWrapperLeavesGitsHelpAndPathWordsAlone(t *testing.T) {
	legion := trailerLegion(t)
	for _, tc := range fixturegit.Subcommands() {
		if tc.Sub >= 0 {
			continue
		}
		args := tc.Args
		for _, form := range []string{"--real", "--after"} {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			writeTrailerScript(t, filepath.Join(dir, "git"), trailerLogging(log))
			bin := t.TempDir()
			value := filepath.Join(dir, "git")
			if form == "--after" {
				value = bin
			}
			env := trailerEnv(t, bin+":"+dir+":/usr/bin:/bin", "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=help-session", "LEGION_ROLE=implementer")
			if code, _, stderr := trailerRun(t, dir, env, "", legion, append([]string{"git", form, value, "--"}, args...)...); code != 0 {
				t.Fatalf("legion git %s -- %q = %d: %s", form, args, code, stderr)
			}
			if calls := trailerCalls(t, log); len(calls) != 1 || !slices.Equal(calls[0], args) {
				t.Errorf("legion git %s -- %q ran git with %q, want one run with the arguments as they came", form, args, calls)
			}
		}
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
// form without its value or its `--`. A help request after `--` is git's
// (TestGitWrapperLeavesGitsHelpAndPathWordsAlone).
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

// A host's git wrapper that closes every descriptor above 2 before it runs the next git, as
// Python's subprocess, Node's spawn and sudo do, still gets each message or template that the
// pane's git kept for git: the pane's git hands it to git on descriptor 0, which every host keeps
// open. The rows run through a host's script and through a compiled host's git (closingGitSource)
// that close descriptors, and one row runs through a host's script that keeps them. In each row
// but one, the commit is git alone's, and the pane's git adds both lines where git alone commits:
//
//   - -F - through the host's script, -F /dev/stdin from a regular file and from a pipe, and -F
//     <a FIFO>: both commit `msg`.
//   - -m msg -t <a FIFO> that holds another text: both commit `msg`.
//   - -t /dev/stdin and -t <a FIFO> beside --amend, from a template that is HEAD's message: both
//     refuse the commit, and HEAD stays as it was.
//   - -F <(printf msg) through the host's script that keeps descriptors: both commit `msg`.
//
// The one row that differs from git alone on purpose is -F <(printf msg) through a host that
// closes descriptors. The substitution's path names a descriptor above 2, and the host closes it.
// So git alone cannot read the message (`could not read log file`) and makes no commit. The pane's
// git read the substitution before the host ran, so it commits `msg` with both lines: each git
// commit gets the two lines (R2).
func TestGitWrapperAfterHandsStandardInputThroughAHostThatClosesDescriptors(t *testing.T) {
	git := fixturegit.CompiledGit(t)
	legion := trailerLegion(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	closer, keeper, compiledCloser := t.TempDir(), t.TempDir(), t.TempDir()
	// The loop has no `2>/dev/null`. For that redirection bash keeps stderr on a descriptor above 2,
	// the loop closes that descriptor, and bash cannot put stderr back: git would print to /dev/null.
	closing := "#!" + bash + "\n" +
		"for fd in /proc/self/fd/*; do n=${fd##*/}; if [ \"$n\" -gt 2 ] && [ \"$n\" != 255 ]; then eval \"exec $n>&-\"; fi; done\n" +
		"exec '" + git + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(closer, "git"), []byte(closing), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTrailerScript(t, filepath.Join(keeper, "git"), "exec '"+git+"' \"$@\"\n")
	copyExecutable(t, goGitProgram(t, strings.Replace(closingGitSource, "REAL_GIT", strconv.Quote(git), 1)), filepath.Join(compiledCloser, "git"))
	lines := "\nOmp-Session: closer-session\nLegion-Role: implementer\n"
	// fifo is a command that makes the FIFO name in TMPDIR and writes text into it in the
	// background, with printf. The writer has a time limit and holds none of the command's output,
	// so a run where nothing opens the FIFO ends rather than waits.
	fifo := func(name, text string) string {
		return `mkfifo "$TMPDIR/` + name + `" && (timeout 10 sh -c 'printf "` + text + `" > "$TMPDIR/` + name + `"' >/dev/null 2>&1 &) && `
	}
	type row struct {
		name, host, command, stdin string
		// commits is whether git alone commits, and added what the pane's git adds then. refusal is
		// a line that git alone prints when it makes no commit. The pane's git makes the same
		// commit or refusal, except where wrapped is set.
		commits bool
		added   string
		refusal string
		// wrapped is HEAD's message after the pane's git, where the pane's git commits and git
		// alone does not. It is "" where both behave alike.
		wrapped string
	}
	rows := []row{
		{"-F - through a host that closes descriptors", closer, "git commit -q --allow-empty -F -", "msg", true, lines, "", ""},
		{"-F <(printf msg) through a host's script that keeps them", keeper, "git commit -q --allow-empty -F <(printf msg)", "", true, lines, "", ""},
	}
	for _, host := range []struct{ name, dir string }{{"a host's script", closer}, {"a compiled host's git", compiledCloser}} {
		rows = append(rows,
			row{"-F /dev/stdin from a regular file through " + host.name + " that closes descriptors", host.dir,
				`printf msg > "$TMPDIR/msg" && git commit -q --allow-empty -F /dev/stdin < "$TMPDIR/msg"`, "", true, lines, "", ""},
			row{"-F /dev/stdin from a pipe through " + host.name + " that closes descriptors", host.dir,
				"printf msg | git commit -q --allow-empty -F /dev/stdin", "", true, lines, "", ""},
			row{"-t /dev/stdin beside --amend, HEAD's message, through " + host.name + " that closes descriptors", host.dir,
				"'" + git + `' commit -q --allow-empty -m msg && printf 'msg\n' > "$TMPDIR/template" && ` +
					`git commit -q --allow-empty --amend -t /dev/stdin < "$TMPDIR/template"`,
				"", false, "", "Aborting commit; you did not edit the message.", ""},
			row{"-F <a FIFO> through " + host.name + " that closes descriptors", host.dir,
				fifo("message-fifo", "msg") + `git commit -q --allow-empty -F "$TMPDIR/message-fifo"`, "", true, lines, "", ""},
			row{"-m msg -t <a FIFO> with another text through " + host.name + " that closes descriptors", host.dir,
				fifo("template-fifo", `A template\n`) + `git commit -q --allow-empty -m msg -t "$TMPDIR/template-fifo"`, "", true, lines, "", ""},
			row{"-t <a FIFO> beside --amend, HEAD's message, through " + host.name + " that closes descriptors", host.dir,
				"'" + git + "' commit -q --allow-empty -m msg && " + fifo("amend-fifo", `msg\n`) +
					`git commit -q --allow-empty --amend -t "$TMPDIR/amend-fifo"`,
				"", false, "", "Aborting commit; you did not edit the message.", ""},
			row{"-F <(printf msg) through " + host.name + " that closes descriptors", host.dir,
				"git commit -q --allow-empty -F <(printf msg)", "", false, "", "could not read log file", "msg\n" + lines},
		)
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			// outcome runs the command and answers its exit status, its stderr and HEAD's message
			// after it, "" when there is no HEAD.
			outcome := func(path string) (code int, stderr, message string) {
				repo := trailerRepo(t, git)
				env := trailerEnv(t, path, "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=closer-session", "LEGION_ROLE=implementer")
				code, _, stderr = trailerRun(t, repo, env, tc.stdin, bash, "-c", tc.command)
				if head, _, _ := trailerRun(t, repo, trailerEnv(t, trailerPath(git)), "", git, "rev-parse", "-q", "--verify", "HEAD"); head == 0 {
					message = trailerMessage(t, git, repo)
				}
				return code, stderr, message
			}
			aloneCode, aloneErr, alone := outcome(tc.host + ":" + trailerPath(git))
			wrappedCode, wrappedErr, wrapped := outcome(trailerWrapperDir(t, legion) + ":" + tc.host + ":" + trailerPath(git))
			if (aloneCode == 0) != tc.commits {
				t.Fatalf("git alone exited %d (%q); want a commit: %v", aloneCode, aloneErr, tc.commits)
			}
			if tc.commits && alone != "msg\n" {
				t.Fatalf("git alone committed %q, want %q", alone, "msg\n")
			}
			if !tc.commits && !strings.Contains(aloneErr, tc.refusal) {
				t.Fatalf("git alone printed %q, want %q", aloneErr, tc.refusal)
			}
			if tc.wrapped != "" {
				if wrappedCode != 0 || wrapped != tc.wrapped {
					t.Fatalf("through the pane's git: exit %d (%q), HEAD %q; want exit 0 and %q", wrappedCode, wrappedErr, wrapped, tc.wrapped)
				}
				return
			}
			if wrappedCode != aloneCode {
				t.Fatalf("git alone exited %d (%q) and through the pane's git %d (%q); want both alike", aloneCode, aloneErr, wrappedCode, wrappedErr)
			}
			if tc.commits && wrapped != "msg\n"+tc.added {
				t.Fatalf("the pane's git committed %q, want %q", wrapped, "msg\n"+tc.added)
			}
			if !tc.commits && (wrapped != alone || !strings.Contains(wrappedErr, tc.refusal)) {
				t.Fatalf("git alone left HEAD %q and printed %q, the pane's git left %q and printed %q; want the same HEAD and %q from both", alone, aloneErr, wrapped, wrappedErr, tc.refusal)
			}
		})
	}
}

// A -F path that is a FIFO names no descriptor: git alone opens the path itself, so through a host's
// git that closes every descriptor above 2 git alone still reads the message and commits it. The
// pane's git must make the same commit through such a host, whether the host is a script or a
// compiled program: with both lines added, or with none, but never a refusal where git alone
// commits (R2). A copy that the pane's git hands git as /dev/fd/<n>, n above 2, is a descriptor such
// a host closes, and git then refuses with `could not read log file`.
func TestGitWrapperAfterGivesAFifoMessageThroughAHostThatClosesDescriptors(t *testing.T) {
	git := fixturegit.CompiledGit(t)
	legion := trailerLegion(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	closer, compiledCloser := t.TempDir(), t.TempDir()
	closing := "#!" + bash + "\n" +
		"for fd in /proc/self/fd/*; do n=${fd##*/}; if [ \"$n\" -gt 2 ] && [ \"$n\" != 255 ]; then eval \"exec $n>&-\"; fi; done\n" +
		"exec '" + git + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(closer, "git"), []byte(closing), 0o700); err != nil {
		t.Fatal(err)
	}
	copyExecutable(t, goGitProgram(t, strings.Replace(closingGitSource, "REAL_GIT", strconv.Quote(git), 1)), filepath.Join(compiledCloser, "git"))
	// The writer has a time limit and holds none of the command's output, so a run where nothing
	// opens the FIFO ends rather than waits.
	const command = `mkfifo "$TMPDIR/message" && (timeout 10 sh -c 'printf msg > "$TMPDIR/message"' >/dev/null 2>&1 &) && ` +
		`git commit -q --allow-empty -F "$TMPDIR/message"`
	for _, host := range []struct{ name, dir string }{{"a host's script", closer}, {"a compiled host's git", compiledCloser}} {
		t.Run("-F <a FIFO> through "+host.name+" that closes descriptors", func(t *testing.T) {
			outcome := func(path string) (code int, stderr, message string) {
				repo := trailerRepo(t, git)
				env := trailerEnv(t, path, "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=fifo-session", "LEGION_ROLE=implementer")
				code, _, stderr = trailerRun(t, repo, env, "", bash, "-c", command)
				if head, _, _ := trailerRun(t, repo, trailerEnv(t, trailerPath(git)), "", git, "rev-parse", "-q", "--verify", "HEAD"); head == 0 {
					message = trailerMessage(t, git, repo)
				}
				return code, stderr, message
			}
			aloneCode, aloneErr, alone := outcome(host.dir + ":" + trailerPath(git))
			if aloneCode != 0 || alone != "msg\n" {
				t.Fatalf("git alone exited %d (%q) and committed %q, want 0 and %q", aloneCode, aloneErr, alone, "msg\n")
			}
			wrappedCode, wrappedErr, wrapped := outcome(trailerWrapperDir(t, legion) + ":" + host.dir + ":" + trailerPath(git))
			lines := "msg\n\nOmp-Session: fifo-session\nLegion-Role: implementer\n"
			if wrappedCode != 0 || (wrapped != "msg\n" && wrapped != lines) {
				t.Fatalf("through the pane's git: exit %d (%q), HEAD %q; git alone commits %q, so want exit 0 and %q or %q", wrappedCode, wrappedErr, wrapped, alone, "msg\n", lines)
			}
		})
	}
}

// closingGitSource is a compiled host's git wrapper that closes every descriptor from 3 to 255, as
// Python's subprocess, Node's spawn and sudo do, and then runs the git that REAL_GIT names: the
// test puts a quoted path there.
const closingGitSource = `package main

import (
	"os"
	"syscall"
)

const git = REAL_GIT

func main() {
	for fd := 3; fd < 256; fd++ {
		syscall.Close(fd)
	}
	syscall.Exec(git, append([]string{git}, os.Args[1:]...), os.Environ())
	os.Exit(126)
}
`

// A host's git that is a script with no `#!` line, which the kernel refuses to run, runs as a
// shell and execvp run it, with /bin/sh. It counts as a script, not the real git: the helper is the
// real git after it, and a commit through it carries both lines.
func TestGitWrapperAfterRunsAHostScriptWithNoInterpreterLine(t *testing.T) {
	git := fixturegit.CompiledGit(t)
	legion := trailerLegion(t)
	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(host, "git"), []byte("exec '"+git+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	repo := trailerRepo(t, git)
	env := trailerEnv(t, trailerWrapperDir(t, legion)+":"+host+":"+trailerPath(git), "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=plain-session", "LEGION_ROLE=implementer")
	if code, _, stderr := trailerRun(t, repo, env, "", sh, "-c", "git status --short && git commit -q --allow-empty -m plain"); code != 0 {
		t.Fatalf("git status and git commit = %d: %s", code, stderr)
	}
	if got, want := trailerMessage(t, git, repo), "plain\n\nOmp-Session: plain-session\nLegion-Role: implementer\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// SIGTERM ends legion git while it reads a `-F -` message from a standard input that never ends,
// as it ends git alone: the command dies of the signal within seconds, and no commit is made.
func TestGitWrapperEndsOnSIGTERMWhileItReads(t *testing.T) {
	git := fixturegit.CompiledGit(t)
	legion := trailerLegion(t)
	repo := trailerRepo(t, git)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	cmd := exec.Command(legion, "git", "--real", git, "--", "commit", "--allow-empty", "-F", "-")
	cmd.Dir, cmd.Stdin = repo, reader
	cmd.Env = trailerEnv(t, trailerPath(git), "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=signal-session", "LEGION_ROLE=implementer")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// The reads before standard input take well under a second; the signal finds the read.
	time.Sleep(1500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal legion git: %v", err)
	}
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("legion git ended with %v, want death by SIGTERM: %s", err, errb.String())
		}
		status, _ := exit.Sys().(syscall.WaitStatus)
		if !(status.Signaled() && status.Signal() == syscall.SIGTERM) && exit.ExitCode() != 128+int(syscall.SIGTERM) {
			t.Fatalf("legion git ended with %v, want death by SIGTERM: %s", err, errb.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("legion git still runs 10 s after SIGTERM")
	}
	if code, _, _ := trailerRun(t, repo, trailerEnv(t, trailerPath(git)), "", git, "rev-parse", "--verify", "-q", "HEAD"); code == 0 {
		t.Fatal("a commit was made")
	}
}
