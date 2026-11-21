package leakguard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// The two wrappers a pod's PATH puts in front of git and jj (packages/daemon/docker/git and
// packages/daemon/docker/jj, installed by worker.Dockerfile) are what put this guard in front of
// a push at all: the git one points git at the root-owned pre-push hook that runs the guard, and
// the jj one refuses a `jj git push` or a `jj gerrit upload` that would shell out to some other
// git. Neither is Go, so these tests run them: a copy of each under the test's own directory, with
// the pod's absolute paths repointed, and a logging stand-in in the real tool's place. Reading
// them would prove nothing about what a shell does with them.
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

// logging is a stand-in for the real tool at path: it appends each of its arguments to log, each
// ended by a NUL byte, and exits 0. What the wrapper handed the real tool is the whole of what it
// records, in order, which is what these tests read; the NUL, which no argument can hold, is what
// lets an argument holding a newline be read back whole.
func logging(t *testing.T, path, log string) string {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\0' \"$@\" >> '" + log + "'\nexit 0\n"
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
	return strings.Split(strings.TrimSuffix(string(read), "\x00"), "\x00")
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

// A command that is neither a push nor a commit reaches the real git with its arguments as they
// came: no hooks path of the wrapper's, and no `--no-verify` taken out. A commit is the other
// command the wrapper changes: it goes to `legion git` (TestTheGitWrapperHandsACommitToLegionGit).
// The pin that keeps hooks off for every other command is the runtime's, and this wrapper leaves
// it alone.
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

// A commit goes to `legion git --real /usr/bin/git`, with the caller's arguments after `--` as
// they came, the global options in front of the subcommand included. legion git adds the trailers
// and then runs the real git itself. The logging stand-in cannot be that real git under legion
// git, whose reads it would answer with nothing, so what legion git adds is its own tests'
// (packages/daemon/cmd/legion/git_test.go). Here the stand-in is legion, and the real git is not
// run at all.
func TestTheGitWrapperHandsACommitToLegionGit(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	script := wrapperCopy(t, dir, "git", [2]string{
		"legion=/opt/legion/bin/legion", "legion=" + logging(t, filepath.Join(dir, "stand-in-legion"), log),
	})
	run := fixturegit.Isolated(t, filepath.Join(dir, "home"))
	if out, err := run.Try(dir, script, "-C", dir, "commit", "-m", "x"); err != nil {
		t.Fatalf("the wrapper failed: %v\n%s", err, out)
	}
	want := []string{"git", "--real", "/usr/bin/git", "--", "-C", dir, "commit", "-m", "x"}
	if got := ranAs(t, log); !slices.Equal(got, want) {
		t.Fatalf("the wrapper ran legion with %q, want %q", got, want)
	}
}

// The wrapper finds each command's subcommand where legion git's gitSubcommand finds it: the tests
// of both read one table (fixturegit.Subcommands). A word that ends git's own options before any
// subcommand makes git run its help or version command, or print a path and stop, whatever
// follows it: `git --help push` is no push, and `git --help commit` no commit. A global option's
// value is no subcommand, also when it is the word push or commit. So a commit reaches
// `legion git --real`, and the real git does not run. A push or a send-pack reaches the real git
// with the wrapper's hooks path in front of the subcommand and without the `--no-verify` after it.
// Every other command reaches the real git byte for byte. Neither of the last two runs legion.
func TestTheGitWrapperRoutesEachCommandByItsSubcommand(t *testing.T) {
	for _, tc := range fixturegit.Subcommands() {
		t.Run(strings.Join(tc.Args, " "), func(t *testing.T) {
			dir := t.TempDir()
			gitLog, legionLog := filepath.Join(dir, "argv"), filepath.Join(dir, "legion-argv")
			git := logging(t, filepath.Join(dir, "stand-in-git"), gitLog)
			script := wrapperCopy(t, dir, "git",
				[2]string{"real=/usr/bin/git", "real=" + git},
				[2]string{"legion=/opt/legion/bin/legion", "legion=" + logging(t, filepath.Join(dir, "stand-in-legion"), legionLog)},
			)
			run := fixturegit.Isolated(t, filepath.Join(dir, "home"))
			if out, err := run.Try(dir, append([]string{script}, tc.Args...)...); err != nil {
				t.Fatalf("the wrapper failed: %v\n%s", err, out)
			}
			word := ""
			if tc.Sub >= 0 {
				word = tc.Args[tc.Sub]
			}
			ran, ranLog, otherLog, want := "git", gitLog, legionLog, tc.Args
			switch word {
			case "commit":
				ran, ranLog, otherLog = "legion", legionLog, gitLog
				want = slices.Concat([]string{"git", "--real", git, "--"}, tc.Args)
			case "push", "send-pack":
				after := slices.DeleteFunc(slices.Clone(tc.Args[tc.Sub:]), func(arg string) bool { return arg == "--no-verify" })
				want = slices.Concat(tc.Args[:tc.Sub], []string{"-c", "core.hooksPath=/opt/legion/githooks"}, after)
			}
			if got := ranAs(t, ranLog); !slices.Equal(got, want) {
				t.Errorf("the wrapper ran %s with %q, want %q", ran, got, want)
			}
			if _, err := os.Stat(otherLog); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the wrapper ran %s, and the stand-in at %s too", ran, otherLog)
			}
		})
	}
}

// A command that is neither a push nor a commit reaches the real git with its arguments as they
// came, however many there are, because the wrapper does not rebuild the list of one: in dash a
// rebuild costs time that grows with the square of the argument count, and a leak check's `git
// rev-list` with one `^<sha>` for each commit a remote holds is about 4,970 arguments long for a
// clone of this repository. Among the 20,000 here are the ones a rebuild could get wrong: a space,
// a newline, a leading `-`, a `--no-verify`, which only a push loses, and a `push` that is no
// subcommand. The arguments alone would pass through a rebuild as well, so the run is also held to
// 10 s: without a rebuild it takes milliseconds, and the rebuild the wrapper made before took four
// minutes in a pod.
func TestTheGitWrapperPassesTwentyThousandArgumentsOfANonPushThroughWithoutARebuild(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	script := wrapperCopy(t, dir, "git", [2]string{
		"real=/usr/bin/git", "real=" + logging(t, filepath.Join(dir, "stand-in-git"), log),
	})
	run := fixturegit.Isolated(t, filepath.Join(dir, "home"))
	const count, bound = 20_000, 10 * time.Second
	want := []string{"rev-list", "an argument with a space", "an argument with\na newline", "-c", "--no-verify", "push"}
	for len(want) < 1+count {
		want = append(want, fmt.Sprintf("^%040x", len(want)))
	}
	started := time.Now()
	if out, err := run.Try(dir, append([]string{script}, want...)...); err != nil {
		t.Fatalf("the wrapper failed: %v\n%s", err, out)
	}
	if took := time.Since(started); took > bound {
		t.Fatalf("the wrapper took %s to pass %d arguments through, want under %s: it rebuilt the list of a command that is no push",
			took.Round(time.Millisecond), count, bound)
	}
	if got := ranAs(t, log); !slices.Equal(got, want) {
		same := 0
		for same < len(got) && same < len(want) && got[same] == want[same] {
			same++
		}
		t.Fatalf("the wrapper ran git with %d arguments, want the %d it was given; the first %d are as given, then %q, want %q",
			len(got), len(want), same, got[same:min(same+3, len(got))], want[same:min(same+3, len(want))])
	}
}

// A push and a send-pack still get the wrapper's `-c core.hooksPath` after the caller's own
// global options and their values and immediately before the subcommand, and lose the
// `--no-verify` that follows the subcommand, while a `--no-verify` that is a global option's value
// stays that value. `--shallow-file` takes the next word as its value, as git reads it: a wrapper
// that read that value as the subcommand would run the push with no hooks path of its own.
func TestTheGitWrapperPutsItsHooksPathAfterTheCallersGlobalOptions(t *testing.T) {
	const hooksPath = "core.hooksPath=/opt/legion/githooks"
	repo := filepath.Join(t.TempDir(), "repo")
	gitDir := filepath.Join(repo, ".git")
	shallow := filepath.Join(gitDir, "shallow")
	for _, tc := range []struct {
		name       string
		args, want []string
	}{
		{
			name: "a push",
			args: []string{"-C", repo, "-c", "a=b", "--git-dir", gitDir, "push", "--no-verify", "origin", "x"},
			want: []string{"-C", repo, "-c", "a=b", "--git-dir", gitDir, "-c", hooksPath, "push", "origin", "x"},
		},
		{
			name: "a send-pack",
			args: []string{"-C", repo, "-c", "a=b", "--git-dir", gitDir, "send-pack", "--no-verify", "origin", "x"},
			want: []string{"-C", repo, "-c", "a=b", "--git-dir", gitDir, "-c", hooksPath, "send-pack", "origin", "x"},
		},
		{
			name: "a push after --shallow-file",
			args: []string{"--shallow-file", shallow, "push"},
			want: []string{"--shallow-file", shallow, "-c", hooksPath, "push"},
		},
		{
			name: "a --no-verify that is a global option's value",
			args: []string{"--git-dir", "--no-verify", "push", "--no-verify"},
			want: []string{"--git-dir", "--no-verify", "-c", hooksPath, "push"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "argv")
			script := wrapperCopy(t, dir, "git", [2]string{
				"real=/usr/bin/git", "real=" + logging(t, filepath.Join(dir, "stand-in-git"), log),
			})
			run := fixturegit.Isolated(t, filepath.Join(dir, "home"))
			if out, err := run.Try(dir, append([]string{script}, tc.args...)...); err != nil {
				t.Fatalf("the wrapper failed: %v\n%s", err, out)
			}
			if got := ranAs(t, log); !slices.Equal(got, tc.want) {
				t.Fatalf("the wrapper ran git with %q, want %q", got, tc.want)
			}
		})
	}
}

// jjWrapper is the setup the jj wrapper's tests share, under a directory of the test's own: the
// git wrapper over a logging stand-in that records its argv in log, under the name and in the
// directory bin that the jj wrapper looks for on PATH, since the jj wrapper's first check is that
// the git jj would shell out to is that wrapper; script, a copy of the jj wrapper repointed at the
// real jj and at that git wrapper; and a bare remote, which each repository names as origin, so a
// push the wrapper lets through finds nothing to push and exits 0: jj 0.46 fails a push from a
// repository with no remote at all, where 0.45 printed "Nothing changed.". setup is the test's
// own git and jj, which make the remote and the repositories, not the stand-in; run is the same
// with bin first on PATH, which runs the wrapper.
type jjWrapper struct {
	dir, log, gitWrapper, script, remote string
	setup, run                           fixturegit.Runner
}

func newJJWrapper(t *testing.T) jjWrapper {
	t.Helper()
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("this test runs the real jj, which reads the configuration and pushes: %v", err)
	}
	f := jjWrapper{dir: t.TempDir()}
	bin := filepath.Join(f.dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	f.log = filepath.Join(f.dir, "argv")
	f.gitWrapper = wrapperCopy(t, bin, "git", [2]string{
		"real=/usr/bin/git", "real=" + logging(t, filepath.Join(f.dir, "stand-in-git"), f.log),
	})
	f.script = wrapperCopy(t, f.dir, "jj",
		[2]string{"real=/usr/local/bin/jj", "real=" + jj},
		[2]string{"wrapper=/opt/legion/bin/git", "wrapper=" + f.gitWrapper},
	)
	f.setup = fixturegit.Isolated(t, filepath.Join(f.dir, "home"))
	f.remote = filepath.Join(f.dir, "remote.git")
	f.setup.Run(f.dir, fixturegit.InitBare(f.remote, "main")...)
	f.run = f.setup.With("PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"))
	return f
}

// repo is a new jj repository named name under the fixture's directory, with the remote as origin.
func (f jjWrapper) repo(name string) string {
	repo := filepath.Join(f.dir, name)
	f.setup.Run(f.dir, fixturegit.JJInit(repo, false)...)
	f.setup.Run(repo, "jj", "git", "remote", "add", "origin", f.remote)
	return repo
}

// assertRefused fails the test unless the jj wrapper's run that printed out and returned err
// exited 2, the shared check's own setup-error status, saying says.
func assertRefused(t *testing.T, out string, err error, says string) {
	t.Helper()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 2 {
		t.Fatalf("the refusal exited %v, want 2, the shared check's own setup-error status\n%s", err, out)
	}
	if !strings.Contains(out, says) {
		t.Fatalf("the refusal does not say %q:\n%s", says, out)
	}
}

// The jj wrapper reads git.executable-path as the push would read it, the caller's own
// configuration arguments included: a `jj git push --config git.executable-path=<another git>`
// names a git on the command line, which outranks every configuration file, and a read without
// those arguments answered for a push nobody was making and let that one through to an unwrapped
// git — a push with no pre-push hook and so no leak check. jj prints `git` and exits 0 when
// nothing sets the value, so a read that fails answers for no git, and the wrapper refuses the
// push rather than take it for the bare `git` the read used to stand for.
func TestTheJJWrapperReadsTheConfigurationThePushWouldUse(t *testing.T) {
	f := newJJWrapper(t)
	// A configuration file naming another git, for the --config-file spelling.
	configFile := filepath.Join(f.dir, "another.toml")
	if err := os.WriteFile(configFile, []byte("git.executable-path = \"/usr/bin/git\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// f.repo names the fixture's remote as origin: the remote a real push has, which the pushes
	// below must be able to resolve. jj 0.45 ended a `jj git push` with no remote at "Nothing
	// changed." because its default revset named no bookmark; jj 0.46, whose git.push became a
	// pattern over the remotes, resolves the remotes first and fails "No git remotes to push to",
	// so without one the row's verdict was the host's jj version. With one, both versions stop at
	// "Nothing changed." and run no git.
	repo := f.repo("repo")

	for _, tc := range []struct {
		name string
		args []string
		// refused is whether the wrapper must refuse the push rather than run it.
		refused bool
		// says is what a refusal must say: the git jj would have run, or that there is none to name.
		says string
	}{
		{name: "a --config naming another git", args: []string{"git", "push", "--config", "git.executable-path=/usr/bin/git"}, refused: true, says: "git.executable-path is /usr/bin/git"},
		{name: "an attached --config naming another git", args: []string{"git", "push", "--config=git.executable-path=/usr/bin/git"}, refused: true, says: "git.executable-path is /usr/bin/git"},
		{name: "a --config-file naming another git", args: []string{"git", "push", "--config-file", configFile}, refused: true, says: "git.executable-path is /usr/bin/git"},
		{name: "a --config-file jj cannot read", args: []string{"git", "push", "--config-file", filepath.Join(f.dir, "missing.toml")}, refused: true, says: "jj could not read its git.executable-path"},
		{name: "no configuration argument at all", args: []string{"git", "push"}},
		{name: "a --config naming the wrapper itself", args: []string{"git", "push", "--config", "git.executable-path=" + f.gitWrapper}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := f.run.Try(repo, append([]string{f.script}, tc.args...)...)
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
			assertRefused(t, out, err, tc.says)
		})
	}
}

// The jj wrapper checks a `jj gerrit upload` as it checks a `jj git push`: jj uploads through the
// same git subprocess (`git::push_updates`, cli/src/commands/gerrit/upload.rs), so an upload run
// with `--config git.executable-path=<another git>` would reach a git with no pre-push hook and so
// no leak check. The refusal comes before the upload starts: the stand-in behind the git wrapper
// records nothing. The control is the same upload with git.executable-path at the git wrapper,
// which the jj wrapper lets through: jj runs its push through the git wrapper, and the stand-in
// records it with the git wrapper's hooks path in front of the subcommand. The stand-in answers
// the push with nothing, which jj then reports as a failed push; that failure is the stand-in's,
// and what this test reads is what reached it.
func TestTheJJWrapperChecksAGerritUploadAsAPush(t *testing.T) {
	f := newJJWrapper(t)
	// A change to upload, made by the test's own jj, so the stand-in records the upload's git alone.
	repo := f.repo("repo")
	if err := os.WriteFile(filepath.Join(repo, "file"), []byte("a change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setup.Run(repo, "jj", "describe", "-m", "a change")

	for _, tc := range []struct {
		name       string
		executable string
		refused    bool
	}{
		{name: "an upload naming another git", executable: "/usr/bin/git", refused: true},
		{name: "the same upload naming the git wrapper", executable: f.gitWrapper},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(f.log); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			out, err := f.run.Try(repo, f.script, "gerrit", "upload", "-r", "@", "--remote-branch", "main",
				"--config", "git.executable-path="+tc.executable)
			refused := strings.Contains(out, "LEAK CHECK SETUP ERROR")
			if refused != tc.refused {
				t.Fatalf("the wrapper refused=%v, want %v: %v\n%s", refused, tc.refused, err, out)
			}
			if !tc.refused {
				got := ranAs(t, f.log)
				at := slices.Index(got, "push")
				if at < 2 || !slices.Equal(got[at-2:at], []string{"-c", "core.hooksPath=/opt/legion/githooks"}) ||
					!strings.HasSuffix(got[len(got)-1], ":refs/for/main") {
					t.Fatalf("the upload reached the git wrapper's git as %q, want a push to refs/for/main with the wrapper's hooks path in front of it\n%s", got, out)
				}
				return
			}
			assertRefused(t, out, err, "git.executable-path is /usr/bin/git")
			if _, err := os.Stat(f.log); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a git ran for the refused upload, so the refusal came after the upload started: %v", err)
			}
		})
	}
}

// The jj wrapper reads the configuration of the repository the push opens, however the caller
// names it: a `-R` written after the subcommand names it as one written before it does, in each
// spelling, and of two the last written is the one jj opens. A wrapper that stopped reading at the
// subcommand read the configuration of the directory it ran in, and let `jj git push -R <a
// repository whose own configuration names another git>` through to that git.
func TestTheJJWrapperReadsTheRepositoryNamedAfterTheSubcommand(t *testing.T) {
	f := newJJWrapper(t)
	// named is the repository whose own configuration each case sets, and clean sets nothing. The
	// pushes run in the fixture's directory, which is no repository, so a wrapper that missed the
	// -R would read no repository's configuration at all.
	named := f.repo("named")
	clean := f.repo("clean")

	for _, tc := range []struct {
		name string
		// executable is the git.executable-path named's own configuration sets.
		executable string
		args       []string
		refused    bool
	}{
		{name: "a -R after the subcommand", executable: "/usr/bin/git", args: []string{"git", "push", "-R", named}, refused: true},
		{name: "an attached -R after the subcommand", executable: "/usr/bin/git", args: []string{"git", "push", "-R" + named}, refused: true},
		{name: "an attached --repository after the subcommand", executable: "/usr/bin/git", args: []string{"git", "push", "--repository=" + named}, refused: true},
		{name: "a later -R naming it", executable: "/usr/bin/git", args: []string{"-R", clean, "git", "push", "-R", named}, refused: true},
		{name: "a -R after the subcommand naming a repository set to the wrapper", executable: f.gitWrapper, args: []string{"git", "push", "-R", named}},
		{name: "an earlier -R naming it", executable: "/usr/bin/git", args: []string{"-R", named, "git", "push", "-R", clean}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.setup.Run(f.dir, "jj", "-R", named, "config", "set", "--repo", "git.executable-path", tc.executable)
			out, err := f.run.Try(f.dir, append([]string{f.script}, tc.args...)...)
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
			assertRefused(t, out, err, "git.executable-path is /usr/bin/git")
		})
	}
}

// A configuration file can set git.executable-path inside a `[[--scope]]` whose `--when.commands`
// names the push alone: jj applies such a scope to the command it names and to no other, so the
// jj wrapper's read of the value, a `jj config get`, answers the bare `git` while the push itself
// runs the other git, with `--no-verify` and no git wrapper in front of it, so no pre-push hook and
// no leak check. It is the one setting the wrapper refuses when it is written unscoped, here in the
// repository's own configuration as a role could write it. Whatever the wrapper does about it,
// refuse the push or run it through the git wrapper, the other git must never run.
func TestTheJJWrapperKeepsACommandScopedGitOffThePush(t *testing.T) {
	f := newJJWrapper(t)
	otherLog := filepath.Join(f.dir, "other-argv")
	other := logging(t, filepath.Join(f.dir, "other-git"), otherLog)
	// A change and a bookmark on it, so a push the wrapper lets through has something to send and
	// runs a git, made by the test's own jj.
	repo := f.repo("repo")
	if err := os.WriteFile(filepath.Join(repo, "file"), []byte("a change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setup.Run(repo, "jj", "describe", "-m", "a change")
	f.setup.Run(repo, "jj", "bookmark", "create", "probe", "-r", "@")
	repoConfig := f.setup.Run(repo, "jj", "config", "path", "--repo")

	for _, tc := range []struct {
		name, command string
		args          []string
	}{
		{name: "a jj git push", command: "git push", args: []string{"git", "push", "--bookmark", "probe"}},
		{name: "a jj gerrit upload", command: "gerrit upload", args: []string{"gerrit", "upload", "-r", "@", "--remote-branch", "main"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, log := range []string{f.log, otherLog} {
				if err := os.Remove(log); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			scoped := fmt.Sprintf("[[--scope]]\n--when.commands = [%q]\n[--scope.git]\nexecutable-path = %q\n", tc.command, other)
			if err := os.MkdirAll(filepath.Dir(repoConfig), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(repoConfig, []byte(scoped), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := f.run.Try(repo, append([]string{f.script}, tc.args...)...)
			if _, statErr := os.Stat(otherLog); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("the push ran the git that a scope on %q names, as %q, with no git wrapper and so no leak check: %v\n%s",
					tc.command, ranAs(t, otherLog), err, out)
			}
			if strings.Contains(out, "LEAK CHECK SETUP ERROR") {
				assertRefused(t, out, err, "git.executable-path")
				return
			}
			got := ranAs(t, f.log)
			at := slices.Index(got, "push")
			if at < 2 || !slices.Equal(got[at-2:at], []string{"-c", "core.hooksPath=/opt/legion/githooks"}) {
				t.Fatalf("the wrapper neither refused the push nor ran it through the git wrapper: it reached the git wrapper's git as %q: %v\n%s", got, err, out)
			}
		})
	}
}

// The caller's own configuration arguments reach the push whole, and the jj wrapper's own
// git.executable-path still outranks every one of them. The wrapper forces that value on the
// push's command line, and two rules of jj's, measured with jj 0.45.1 and 0.46.0-sami, decide
// where it may go. Consecutive `--config` arguments form one layer, in which a `--scope` value
// wins over a plain one whatever their order, so the value appended after a caller's `jj git push
// --config '"--scope"=[…]'` landed in that layer and lost to the scope: the push ran the scope's
// git, with no git wrapper and so no leak check. And a `--config` written after the subcommand
// makes jj drop every `--config` written before it, so the same appended value took a caller's
// `jj --config <value> git push` off the push but not off the wrapper's read, and the push ran
// with a configuration other than the one the read checked: to origin rather than the remote
// git.push names, and past jj's own refusal of a commit git.private-commits makes private. What
// jj says to those two is plain jj's, which each of their cases runs first: a case whose plain jj
// no longer says it fails as a stale expectation, not as the wrapper's fault.
func TestTheJJWrapperKeepsTheCallersConfigurationOnThePushUnderItsOwnGit(t *testing.T) {
	f := newJJWrapper(t)
	otherLog := filepath.Join(f.dir, "other-argv")
	other := logging(t, filepath.Join(f.dir, "other-git"), otherLog)
	// A change and a bookmark on it, made by the test's own jj, so a push has something to send,
	// and a second remote, which a case names in origin's place. A push through the stand-in
	// fails, since the stand-in answers it with nothing, so jj records no push and every case
	// finds the bookmark as new as the first did.
	repo := f.repo("repo")
	if err := os.WriteFile(filepath.Join(repo, "file"), []byte("a change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setup.Run(repo, "jj", "describe", "-m", "a change")
	f.setup.Run(repo, "jj", "bookmark", "create", "probe", "-r", "@")
	elsewhere := filepath.Join(f.dir, "elsewhere.git")
	f.setup.Run(f.dir, fixturegit.InitBare(elsewhere, "main")...)
	f.setup.Run(repo, "jj", "git", "remote", "add", "elsewhere", elsewhere)

	// A scope naming the other git for the push alone, passed on the push's own command line in
	// each spelling of --config. Whatever the wrapper does about it, refuse the push or run it
	// through the git wrapper, the other git must never run.
	for _, push := range []struct {
		command string
		args    []string
	}{
		{command: "git push", args: []string{"git", "push", "--bookmark", "probe"}},
		{command: "gerrit upload", args: []string{"gerrit", "upload", "-r", "@", "--remote-branch", "main"}},
	} {
		scope := fmt.Sprintf("[{ \"--when\" = { commands = [%q] }, git = { executable-path = %q } }]", push.command, other)
		for _, spelling := range []struct {
			name string
			args []string
		}{
			{name: "a --config", args: []string{"--config", "\"--scope\"=" + scope}},
			{name: "an attached --config=", args: []string{"--config=--scope=" + scope}},
		} {
			t.Run(fmt.Sprintf("a jj %s with a scope in %s", push.command, spelling.name), func(t *testing.T) {
				for _, log := range []string{f.log, otherLog} {
					if err := os.Remove(log); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
				}
				out, err := f.run.Try(repo, slices.Concat([]string{f.script}, push.args, spelling.args)...)
				if _, statErr := os.Stat(otherLog); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("the push ran the git that a scope on %q in %s on its command line names, as %q, with no git wrapper and so no leak check: %v\n%s",
						push.command, spelling.name, ranAs(t, otherLog), err, out)
				}
				if strings.Contains(out, "LEAK CHECK SETUP ERROR") {
					assertRefused(t, out, err, "git.executable-path")
					return
				}
				got := ranAs(t, f.log)
				at := slices.Index(got, "push")
				if at < 2 || !slices.Equal(got[at-2:at], []string{"-c", "core.hooksPath=/opt/legion/githooks"}) {
					t.Fatalf("the wrapper neither refused the push nor ran it through the git wrapper: it reached the git wrapper's git as %q: %v\n%s", got, err, out)
				}
			})
		}
	}

	// A --config written before the subcommand, on a dry run, which runs no git: says is what jj
	// prints for it (measured with jj 0.45.1 and 0.46.0-sami), and a push that dropped it says
	// "Changes to push to origin:" and goes ahead instead.
	for _, tc := range []struct {
		name, config, says string
	}{
		{name: "a git.private-commits before the subcommand", config: `git.private-commits="all()"`, says: "since it is private"},
		{name: "a git.push before the subcommand", config: `git.push="elsewhere"`, says: "Changes to push to elsewhere:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--config", tc.config, "git", "push", "--bookmark", "probe", "--dry-run"}
			if out, err := f.setup.Try(repo, append([]string{"jj"}, args...)...); !strings.Contains(out, tc.says) {
				t.Fatalf("plain jj %q does not say %q, so what this case expects of the wrapper is stale: %v\n%s", args, tc.says, err, out)
			}
			out, err := f.run.Try(repo, append([]string{f.script}, args...)...)
			if !strings.Contains(out, tc.says) {
				t.Fatalf("the wrapper's push of %q does not say %q as plain jj does, so it ran with a configuration other than the one its read checked: %v\n%s", args, tc.says, err, out)
			}
		})
	}
}
