// Package fixtureenv is the environment a test's real git and jj run in. The daemon's tests run
// inside Legion's own issue pods, whose environment names the pod's live workspace, its git
// configuration, its GitHub credential and the deployment's own session store, and whose PATH
// leads with Legion's own git and jj wrappers; a test that inherited them could read or push the
// pod's own work, read and write the conversation of every agent of the deployment, or have its
// own push refused by the pod's leak guard. Guard takes them out of a test process's own
// environment, so every command it starts and everything it runs in-process inherits none, and
// puts the git and jj a machine's own wrappers of them run first on the process's PATH, so a
// command under a home of the test's meets no wrapper that needs another; Isolated hands one
// command none of the variables and a home of the test's.
package fixtureenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

// reaches is whether a variable can lead git or jj to a workspace, repository, remote or
// credential other than the test's own (Isolated's doc says how each does), or can hand the leak
// guard a shared check of the machine's rather than the test's own stand-in (PUBLIC_LEAK_CHECK
// and the rest of the check's own prefix, which internal/leakguard reads), HOME, XDG_CONFIG_HOME
// and XDG_STATE_HOME aside.
func reaches(key string) bool {
	for _, prefix := range []string{"LEGION_", "GIT_", "JJ_", "GH_", "GITHUB_", "PUBLIC_LEAK_CHECK"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return key == "SSH_AUTH_SOCK" || key == "SSH_ASKPASS"
}

// placesSession is whether a variable places an Oh My Pi session. OMP_SESSION_SQL_DSN_FILE and
// OMP_SESSION_STORAGE are the two Oh My Pi reads to do that; in a Legion pod they name the
// deployment's live session database, which holds every agent's conversation, so a test that
// inherited them would read and write that store. OMP_SESSION_SQL_DSN carries the same URL by
// value, where a deployment exports the file's contents rather than its name. The three are named
// rather than an OMP_ or PI_ prefix taken because a test may set a variable of either prefix for
// the code under test to read: OMP_PROFILE (cmd/legion's probe-image, worker-shim and controller
// tests) and PI_CONFIG_DIR and PI_CODING_AGENT_DIR (cmd/legion's controller test).
func placesSession(key string) bool {
	return key == "OMP_SESSION_SQL_DSN_FILE" || key == "OMP_SESSION_STORAGE" ||
		key == "OMP_SESSION_SQL_DSN"
}

// guarded is every variable the guard takes out: the ones git and jj reach through, and the ones
// that place the session store.
func guarded(key string) bool {
	return reaches(key) || placesSession(key)
}

// imageGit is the image's git wrapper (workerbin.ImageGit), or the stand-in a test points it at.
var imageGit = workerbin.ImageGit

// withoutImageWrappers is path without each entry whose git is the image's git wrapper
// (imageGit), by the rule workerbin reads that wrapper by: the same file (os.SameFile), so an entry
// holding a link to it goes too. A pod's PATH leads with one such entry, /opt/legion/bin, whose
// git puts Legion's leak guard in front of every push, whose jj refuses a push whose git is any
// other, and whose gh runs `legion gh`. A test's own push to a local bare repository runs there
// under the test's empty home, where the guard finds no shared check, and is refused in a pod and
// not on CI, whose PATH holds no such entry. Every other entry stays, as written and in order, a
// test's own copy of a wrapper included, since a copy is not the same file.
func withoutImageWrappers(path string) string {
	image, err := os.Stat(imageGit)
	if err != nil {
		return path
	}
	entries := slices.DeleteFunc(filepath.SplitList(path), func(entry string) bool {
		if entry == "" {
			return false
		}
		git, err := os.Stat(filepath.Join(entry, "git"))
		return err == nil && os.SameFile(git, image)
	})
	return strings.Join(entries, string(filepath.ListSeparator))
}

// testInput is whether a variable is one a person or CI sets for the tests themselves, which Guard
// keeps: the LEGION_TEST_* and LEGION_E2E* services and binaries (LEGION_TEST_PG_DSN,
// LEGION_TEST_OMP), and GITHUB_ACTIONS, which turns a missing prerequisite's skip into a failure.
func testInput(key string, keep []string) bool {
	return strings.HasPrefix(key, "LEGION_TEST_") || strings.HasPrefix(key, "LEGION_E2E") ||
		key == "GITHUB_ACTIONS" || slices.Contains(keep, key)
}

// guardedKeep is what the last Guard kept beyond the test inputs; nil until a Guard ran.
var guardedKeep []string

// unguarded is the process's own KEY=value entry, before the first Guard, of every variable that
// Guard took out or set, for Live.
var unguarded []string

// Guard takes every variable that reaches (LEGION_*, GIT_*, JJ_*, GH_*, GITHUB_*, SSH_AUTH_SOCK,
// SSH_ASKPASS) or places a session (OMP_SESSION_SQL_DSN_FILE, OMP_SESSION_STORAGE,
// OMP_SESSION_SQL_DSN) out of the test process's own environment, but the test inputs and the
// names in keep, takes every directory of Legion's image wrappers out of its PATH
// (withoutImageWrappers), and sets GIT_ALLOW_PROTOCOL=file. The PATH is the process's own because
// exec.Command and exec.LookPath look a bare name up on it, whatever PATH a command's Env holds:
// a fixture's `git push` under Isolated, and a test's fixturegit.CompiledJJ for the real jj, read
// the process's PATH. Guard then puts first on that PATH a directory of links to the git and jj
// the PATH's own git and jj run (realTools), so neither a fixture under Isolated nor the code under
// test, which looks each up by name, meets a machine's wrapper of them. A package whose tests run
// git or jj — as child processes, with no Env of their own, or in-process through the code under
// test — calls it in TestMain before m.Run, after the branch where the test binary runs as a helper
// child: that child keeps the variables its test hands it. Code under test that names its own
// transport (workspace's GIT_ALLOW_PROTOCOL=https) still does; a git that names none can reach no
// network remote. Guard does the same in every build, the e2e one included, whose untagged tests
// are the same fixtures; a live proof of that build, which reaches a real cluster and GitHub on
// purpose, takes the process's own environment back for its own run with Live.
func Guard(keep ...string) error {
	first := guardedKeep == nil
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !guarded(key) || testInput(key, keep) {
			continue
		}
		if first {
			unguarded = append(unguarded, entry)
		}
		if key == "GIT_ALLOW_PROTOCOL" {
			continue
		}
		if err := os.Unsetenv(key); err != nil {
			return fmt.Errorf("fixtureenv: unset %s: %w", key, err)
		}
	}
	own := os.Getenv("PATH")
	path := withoutImageWrappers(own)
	tools, err := realTools(path)
	if err != nil {
		return err
	}
	if tools != "" {
		rest := slices.DeleteFunc(filepath.SplitList(path), func(entry string) bool { return entry == tools })
		path = strings.Join(append([]string{tools}, rest...), string(filepath.ListSeparator))
	}
	if path != own {
		if first {
			unguarded = append(unguarded, "PATH="+own)
		}
		if err := os.Setenv("PATH", path); err != nil {
			return fmt.Errorf("fixtureenv: set PATH: %w", err)
		}
	}
	if err := os.Setenv("GIT_ALLOW_PROTOCOL", "file"); err != nil {
		return fmt.Errorf("fixtureenv: set GIT_ALLOW_PROTOCOL: %w", err)
	}
	guardedKeep = append([]string{}, keep...)
	return nil
}

// MustGuard is Guard for a TestMain, which has no test to fail and no error to return: it guards
// the test process, and where Guard fails it prints the failure as `TestMain: <err>` on stderr and
// ends the process with status 1, so no package's tests run unguarded. Every guarding TestMain
// calls it in Guard's place.
func MustGuard(keep ...string) {
	if err := Guard(keep...); err != nil {
		guardFailed(err)
	}
}

// guardFailed ends the test process MustGuard could not guard. Guard can fail while changing its
// environment or while realTools cannot identify a git or jj from the PATH it was given; that
// second failure is observable outside this process (TestGuardNamesAToolItCannotRead). This
// function is separate because it is the half of MustGuard a test can run.
func guardFailed(err error) {
	fmt.Fprintln(os.Stderr, "TestMain:", err)
	os.Exit(1)
}

// parentExe is the shell command that prints the executable of the process it runs under, from
// Linux's /proc: the parent of the shell that runs it is that process.
const parentExe = `readlink "/proc/$PPID/exe"`

// realTools is a directory holding git and jj: links to the programs path's own git and jj run,
// for Guard to put first on the process's PATH. The first git or jj of a machine's PATH can be a
// wrapper that finds its tool by, or changes what that tool does by, the home of whoever runs it: a
// mise shim finds its version under the caller's HOME and none under a fixture's (`mise ERROR No
// version is set for shim: jj`), and a person's own git or jj shim can check every commit against
// a list it decrypts with that HOME's keys and refuse the commit where it cannot. A fixture runs
// git and jj under a home of the test's (Isolated), and so does the code under test, so a test's
// result depended on what the machine put first on PATH.
//
// Each program is read from itself while it runs: the executable /proc names for the parent of a
// shell it starts (a git shell alias; `jj util exec`), started as path's first git or jj, in a
// directory of its own, under the process's environment, where a wrapper finds its tool, and with
// no configuration of the machine's (GIT_CONFIG_GLOBAL, GIT_CONFIG_NOSYSTEM and an empty
// JJ_CONFIG). A tool path does not hold is left out, so a test that needs it skips or fails as it
// did; one path holds that cannot be read that way, a wrapper that runs no tool, is an error
// naming it. "" is a path with neither, and every system but Linux, which alone has that /proc.
// The directory is the user's cache's, named for the programs it links (linkDir), so every test
// binary that reads the same two programs shares it and none has one to remove.
func realTools(path string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", nil
	}
	work, err := os.MkdirTemp("", "fixtureenv-tools-")
	if err != nil {
		return "", fmt.Errorf("fixtureenv: make the directory the real git and jj are read in: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	emptyConfig := filepath.Join(work, "jj-config.toml")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		return "", fmt.Errorf("fixtureenv: write the empty jj configuration: %w", err)
	}
	targets := map[string]string{}
	for _, tool := range []struct {
		name string
		args []string
		env  []string
	}{
		{"git", []string{"-c", "alias.binary=!" + parentExe, "binary"}, []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}},
		{"jj", []string{"util", "exec", "--", "sh", "-c", parentExe}, []string{"JJ_CONFIG=" + emptyConfig}},
	} {
		onPath, found := lookpath.On(path, tool.name)
		if !found {
			continue
		}
		binary, err := runningBinary(work, path, onPath, tool.args, tool.env)
		if err != nil {
			return "", fmt.Errorf("fixtureenv: %w", err)
		}
		targets[tool.name] = binary
	}
	if len(targets) == 0 {
		return "", nil
	}
	return linkDir(targets)
}

// runningBinaryTimeout bounds each tool read Guard makes. Guard runs from TestMain, before go test
// arms the test binary's timeout alarm, so a tool that never answers would otherwise hold the
// binary with nothing to name it.
const runningBinaryTimeout = 30 * time.Second

// runningBinary runs onPath with args, which make it run parentExe in a shell under itself, in dir,
// under the process's environment with path as its PATH and env over it, and answers the absolute
// path that prints: the program onPath ran. It gives the tool runningBinaryTimeout. A run that
// prints no such path, or does not answer in time, is an error naming the command, its status,
// what it printed and its stderr.
func runningBinary(dir, path, onPath string, args, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runningBinaryTimeout)
	defer cancel()
	return runningBinaryContext(ctx, dir, path, onPath, args, env)
}

// runningBinaryContext is runningBinary under ctx, so the test of a blocking wrapper can give it a
// short deadline without changing Guard's production bound.
func runningBinaryContext(ctx context.Context, dir, path, onPath string, args, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, onPath, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "PATH="+path), env...)
	cmd.WaitDelay = 5 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("could not read the program %s runs: `%s %s` did not answer within %s: %w; stderr: %s",
			onPath, onPath, strings.Join(args, " "), runningBinaryTimeout, ctx.Err(), stderr.String())
	}
	binary := strings.TrimSpace(string(out))
	if err == nil && filepath.IsAbs(binary) {
		_, err = os.Stat(binary)
		if err == nil {
			return binary, nil
		}
	}
	return "", fmt.Errorf("could not read the program %s runs: `%s %s` answered %q (%v), and reading it needs Linux's /proc; stderr: %s",
		onPath, onPath, strings.Join(args, " "), binary, err, stderr.String())
}

// linkDir is the directory, in the user's cache, holding one link per name of targets to its
// program, named for those links' targets: a second test binary that reads the same programs finds
// it made, and two that make it at once each make a directory of their own and rename it into
// place, the one that loses taking the winner's.
func linkDir(targets map[string]string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("fixtureenv: find the cache that holds the real git and jj: %w", err)
	}
	parent := filepath.Join(cache, "legion-fixtureenv")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("fixtureenv: make %s: %w", parent, err)
	}
	hash := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(targets)) {
		fmt.Fprintf(hash, "%s=%s\n", name, targets[name])
	}
	dir := filepath.Join(parent, hex.EncodeToString(hash.Sum(nil))[:16])
	if holds(dir, targets) {
		return dir, nil
	}
	made, err := os.MkdirTemp(parent, filepath.Base(dir)+".")
	if err != nil {
		return "", fmt.Errorf("fixtureenv: make the links to the real git and jj: %w", err)
	}
	for name, target := range targets {
		if err := os.Symlink(target, filepath.Join(made, name)); err != nil {
			_ = os.RemoveAll(made)
			return "", fmt.Errorf("fixtureenv: link %s: %w", name, err)
		}
	}
	if err := os.Rename(made, dir); err != nil {
		_ = os.RemoveAll(made)
		if holds(dir, targets) {
			return dir, nil
		}
		return "", fmt.Errorf("fixtureenv: put the links to the real git and jj at %s: %w", dir, err)
	}
	return dir, nil
}

// holds is whether dir holds exactly one link per name of targets, each to its program.
func holds(dir string, targets map[string]string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != len(targets) {
		return false
	}
	for name, target := range targets {
		if got, err := os.Readlink(filepath.Join(dir, name)); err != nil || got != target {
			return false
		}
	}
	return true
}

// Live gives the test the process's own values back of every variable Guard took out or set, its
// PATH included, and leaves GIT_ALLOW_PROTOCOL unset when the process had none; the guarded
// environment returns when the test ends. Only a live proof's entry point calls it: an e2e-tagged
// test that reaches a real cluster and GitHub with the operator's credentials and configuration.
// Like t.Setenv, it cannot run in a parallel test. Without a Guard it changes nothing.
func Live(t testing.TB) {
	t.Helper()
	if guardedKeep == nil {
		return
	}
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	if err := os.Unsetenv("GIT_ALLOW_PROTOCOL"); err != nil {
		t.Fatalf("fixtureenv: unset GIT_ALLOW_PROTOCOL: %v", err)
	}
	for _, entry := range unguarded {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
}

// Kept is the names the last Guard kept beyond the test inputs, nil before any Guard. A TestMain
// that reads it immediately after its own Guard holds what that Guard was handed, which a Guard a
// test runs later cannot change: a package whose gated proofs read an operator's variables can
// then assert that its entry point kept them, where reading the live state would pass on whatever
// the test that ran first had set.
func Kept() []string {
	return slices.Clone(guardedKeep)
}

// RequireGuarded fails the test unless the package's TestMain ran Guard and the process holds
// none of the variables it takes out: the only GIT_ variable is GIT_ALLOW_PROTOCOL=file, the
// only JJ_ one is the JJ_CONFIG a TestMain may set after it, and PATH holds no directory of
// Legion's image wrappers.
func RequireGuarded(t testing.TB) {
	t.Helper()
	if guardedKeep == nil {
		t.Fatal("the package's TestMain never called fixtureenv.Guard")
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if guarded(key) && !testInput(key, guardedKeep) && key != "GIT_ALLOW_PROTOCOL" && key != "JJ_CONFIG" {
			t.Errorf("the test process holds %s, which Guard takes out", key)
		}
	}
	if path := os.Getenv("PATH"); withoutImageWrappers(path) != path {
		t.Errorf("PATH=%s holds a directory of Legion's image wrappers, which Guard takes out", path)
	}
	if got := os.Getenv("GIT_ALLOW_PROTOCOL"); got != "file" {
		t.Errorf("GIT_ALLOW_PROTOCOL=%q, want file", got)
	}
}

// Isolated is the process's environment without anything that could lead git or jj to a real
// workspace, repository, remote or credential, or Oh My Pi to a real session store, and with home
// as the home, an empty user configuration, an empty state home and an identity of the test's. It
// keeps no test input either, so a fixture that needs one appends it after. It drops:
//
//   - LEGION_*: LEGION_WORKSPACE names the workspace a shim adopts and reads
//     (shim.runAdoption, shim.readWorkspaceWork); a test that leaves it out to see a refusal would
//     otherwise run on the pod's own.
//   - GIT_*, JJ_*, HOME, XDG_CONFIG_HOME: GIT_DIR and GIT_WORK_TREE name another repository
//     whatever directory a command runs in, a pod hands every process a GIT_CONFIG_* block of its
//     own, and a home carries configuration and an identity.
//   - GH_*, GITHUB_*, SSH_AUTH_SOCK, SSH_ASKPASS: a credential git could push with.
//   - XDG_STATE_HOME: where `legion gh` keeps its call log and the leak guard its cache of the
//     repositories GitHub answered public (internal/leakguard); a fixture that inherited the
//     runner's would read the answers kept there and write its own among them.
//   - OMP_SESSION_SQL_DSN_FILE, OMP_SESSION_STORAGE, OMP_SESSION_SQL_DSN: the session store Oh My
//     Pi places a session in, which in a Legion pod is the deployment's own, holding every agent's
//     conversation; a fixture that carried them would have whatever it starts read and write it.
//
// GIT_ALLOW_PROTOCOL=file lets git use no transport but local files, whatever a repository's own
// configuration allows (git-config(1), protocol.allow): a push or fetch that still named a network
// remote is refused before it connects. A caller's own JJ_USER or GIT_AUTHOR_NAME appended after
// these wins, as exec.Cmd takes the last value of a key.
func Isolated(home string) []string {
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return guarded(key) || key == "HOME" || key == "XDG_CONFIG_HOME" || key == "XDG_STATE_HOME"
	})
	env = append(env, Home(home)...)
	return append(env,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=file",
		"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid",
		"GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid",
		"JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid",
	)
}

// Home is the home Isolated gives a command, as KEY=value entries: HOME, the config home under it
// (XDG_CONFIG_HOME, where jj keeps each repository's own configuration), the state home under it
// (XDG_STATE_HOME, where `legion gh` and the leak guard keep what they remember between calls),
// and a JJ_CONFIG naming a file that does not exist, so jj reads no user configuration. A test
// whose code under test reads the process's own environment sets these on the process, so that
// code and its fixtures under Isolated of the same home read the same jj repository configuration
// and the same state.
func Home(home string) []string {
	return []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"JJ_CONFIG=" + filepath.Join(home, "no-jj-config.toml"),
	}
}

// Unset takes every variable in names out of the test process's own environment: t.Setenv of an
// empty value, so Go puts back whatever the process held when the test ends, and then
// os.Unsetenv, so code that tells an empty value from an absent one (os.LookupEnv) finds none.
// It is variadic because a test that clears one of these usually clears a set of them together.
// Like t.Setenv, which it uses, it cannot be used in a parallel test. It is for a test and takes
// the test for that reason: a TestMain has no *testing.T, so nothing there would put the
// process's own values back, and a TestMain that wants a variable gone calls os.Unsetenv itself.
func Unset(t testing.TB, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}
