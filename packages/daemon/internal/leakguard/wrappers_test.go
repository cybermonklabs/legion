package leakguard

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// The two wrappers a pod's PATH puts in front of git and jj (packages/daemon/docker/git and
// packages/daemon/docker/jj, installed by worker.Dockerfile) are what put this guard in front of
// a push at all: the git one points git at the root-owned pre-push hook that runs the guard, and
// the jj one refuses a `jj git push` that would shell out to some other git. Neither is Go, so
// these tests run them: a copy of each under the test's own directory, with the pod's absolute
// paths repointed, and a logging stand-in in the real tool's place. Reading them would prove
// nothing about what a shell does with them.
const wrapperDir = "../../docker"

// wrapperCopy is the wrapper named name, copied into dir with each of repointed's replacements
// made — an assignment's whole line, as the script writes it, for the same line naming a path of
// this test's — and made executable, as the image's copy is. A replacement that matches nothing
// fails the test rather than leaving a copy that still names a pod's path.
func wrapperCopy(t *testing.T, dir, name string, repointed ...[2]string) string {
	t.Helper()
	read, err := os.ReadFile(filepath.Join(wrapperDir, name))
	if err != nil {
		t.Fatal(err)
	}
	script := string(read)
	for _, pair := range repointed {
		if !strings.Contains(script, pair[0]) {
			t.Fatalf("docker/%s holds no %q to repoint", name, pair[0])
		}
		script = strings.Replace(script, pair[0], pair[1], 1)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// logging is a stand-in for the real tool at path: it appends each of its arguments to log, one
// to a line, and exits 0. What the wrapper handed the real tool is the whole of what it records,
// in order, which is what these tests read.
func logging(t *testing.T, path, log string) string {
	t.Helper()
	script := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\" >> '" + log + "'; done\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// ranAs is the argv the stand-in recorded in log.
func ranAs(t *testing.T, log string) []string {
	t.Helper()
	read, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the wrapper ran the real tool with nothing, or not at all: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(read), "\n"), "\n")
}

// The git wrapper's own `-c core.hooksPath` goes after the caller's own arguments and
// immediately before the subcommand, so that git, which takes the last `-c` of a name, reads the
// wrapper's. A caller's own `-c core.hooksPath=<anywhere>` used to win, which is a push with no
// pre-push hook and so no leak check at all.
func TestTheGitWrappersHooksPathOutranksTheCallersOwn(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	script := wrapperCopy(t, dir, "git", [2]string{
		"real=/usr/bin/git", "real=" + logging(t, filepath.Join(dir, "stand-in-git"), log),
	})
	run := fixturegit.Isolated(t, filepath.Join(dir, "home"))
	const callers = "/tmp/a-hooks-path-of-the-callers-own"
	if out, err := run.Try(dir, script, "-c", "core.hooksPath="+callers, "push", "origin", "main", "--no-verify"); err != nil {
		t.Fatalf("the wrapper failed: %v\n%s", err, out)
	}
	want := []string{
		"-c", "core.hooksPath=" + callers,
		"-c", "core.hooksPath=/opt/legion/githooks",
		"push", "origin", "main",
	}
	if got := ranAs(t, log); !slices.Equal(got, want) {
		t.Fatalf("the wrapper ran git with %q, want %q", got, want)
	}
	// And what git itself makes of those two, which is the whole reason for the order: the last
	// one of a name wins, so the hooks path git reads is the wrapper's.
	hooks := run.Run(dir, "git", "-c", "core.hooksPath="+callers, "-c", "core.hooksPath=/opt/legion/githooks",
		"config", "--get", "core.hooksPath")
	if hooks != "/opt/legion/githooks" {
		t.Fatalf("git read core.hooksPath as %q, want the wrapper's, which is written last", hooks)
	}
}

// Everything that is no push reaches the real git exactly as it was written: no hooks path of
// the wrapper's, and no `--no-verify` taken out. The pin that keeps hooks off for every other
// command is the runtime's, and this wrapper leaves it alone.
func TestTheGitWrapperPassesANonPushThroughByteForByte(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	script := wrapperCopy(t, dir, "git", [2]string{
		"real=/usr/bin/git", "real=" + logging(t, filepath.Join(dir, "stand-in-git"), log),
	})
	run := fixturegit.Isolated(t, filepath.Join(dir, "home"))
	want := []string{"-c", "core.hooksPath=/tmp/x", "--git-dir", "/tmp/repo/.git", "status", "--porcelain", "--no-verify"}
	if out, err := run.Try(dir, append([]string{script}, want...)...); err != nil {
		t.Fatalf("the wrapper failed: %v\n%s", err, out)
	}
	if got := ranAs(t, log); !slices.Equal(got, want) {
		t.Fatalf("the wrapper ran git with %q, want the arguments it was given, %q", got, want)
	}
}

// The jj wrapper reads git.executable-path as the push would read it, the caller's own
// configuration arguments included: a `jj git push --config git.executable-path=<another git>`
// names a git on the command line, which outranks every configuration file, and a read without
// those arguments answered for a push nobody was making and let that one through to an unwrapped
// git — a push with no pre-push hook and so no leak check.
func TestTheJJWrapperReadsTheConfigurationThePushWouldUse(t *testing.T) {
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("this test runs the real jj, which reads the configuration: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// The git wrapper, under the name and in the directory the jj wrapper looks for on PATH: the
	// jj wrapper's first check is that the git jj would shell out to is that wrapper.
	gitWrapper := wrapperCopy(t, bin, "git", [2]string{
		"real=/usr/bin/git", "real=" + logging(t, filepath.Join(dir, "stand-in-git"), filepath.Join(dir, "argv")),
	})
	script := wrapperCopy(t, dir, "jj",
		[2]string{"real=/usr/local/bin/jj", "real=" + jj},
		[2]string{"wrapper=/opt/legion/bin/git", "wrapper=" + gitWrapper},
	)
	// A configuration file naming another git, for the --config-file spelling.
	configFile := filepath.Join(dir, "another.toml")
	if err := os.WriteFile(configFile, []byte("git.executable-path = \"/usr/bin/git\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	run := fixturegit.Isolated(t, filepath.Join(dir, "home")).With("PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"))
	run.Run(dir, fixturegit.JJInit(repo, false)...)

	for _, tc := range []struct {
		name string
		args []string
		// refused is whether the wrapper must refuse the push rather than run it.
		refused bool
	}{
		{name: "a --config naming another git", args: []string{"git", "push", "--config", "git.executable-path=/usr/bin/git"}, refused: true},
		{name: "an attached --config naming another git", args: []string{"git", "push", "--config=git.executable-path=/usr/bin/git"}, refused: true},
		{name: "a --config-file naming another git", args: []string{"git", "push", "--config-file", configFile}, refused: true},
		{name: "no configuration argument at all", args: []string{"git", "push"}},
		{name: "a --config naming the wrapper itself", args: []string{"git", "push", "--config", "git.executable-path=" + gitWrapper}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run.Try(repo, append([]string{script}, tc.args...)...)
			refused := strings.Contains(out, "LEAK CHECK SETUP ERROR")
			if refused != tc.refused {
				t.Fatalf("the wrapper refused=%v, want %v: %v\n%s", refused, tc.refused, err, out)
			}
			if !tc.refused {
				if err != nil {
					t.Fatalf("the wrapper did not run the push: %v\n%s", err, out)
				}
				return
			}
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 2 {
				t.Fatalf("the refusal exited %v, want 2, the shared check's own setup-error status\n%s", err, out)
			}
			if !strings.Contains(out, "/usr/bin/git") {
				t.Fatalf("the refusal does not name the git jj would have run:\n%s", out)
			}
		})
	}
}
