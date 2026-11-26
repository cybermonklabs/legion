package leakguard

//ci:reads packages/daemon/docker/git wrapperCopy copies the pod's git wrapper from ../../docker
//ci:reads packages/daemon/docker/jj wrapperCopy copies the pod's jj wrapper from ../../docker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
// real jj and at that git wrapper, whose legion is passingLegion, which runs the real jj with the
// line it is handed; and a bare remote, which each repository names as origin, so a push the
// wrapper lets through finds nothing to push and exits 0: jj 0.46 fails a push from a repository
// with no remote at all, where 0.45 printed "Nothing changed.". jj is the real jj
// (fixturegit.CompiledJJ), which script runs and which every jj command a test makes itself runs
// by its path, plain jj's included: a jj of the machine's own first on PATH, a dev box's shim,
// reads a push with the stand-in git and refuses it before jj runs. setup is the test's own git
// and jj, which make the remote and the repositories, not the stand-in; run is the same with bin
// first on PATH, which runs the wrapper.
type jjWrapper struct {
	dir, jj, log, gitWrapper, script, remote string
	setup, run                               fixturegit.Runner
}

func newJJWrapper(t *testing.T) jjWrapper {
	t.Helper()
	f := jjWrapper{dir: t.TempDir(), jj: fixturegit.CompiledJJ(t)}
	bin := filepath.Join(f.dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	f.log = filepath.Join(f.dir, "argv")
	f.gitWrapper = wrapperCopy(t, bin, "git", [2]string{
		"real=/usr/bin/git", "real=" + logging(t, filepath.Join(f.dir, "stand-in-git"), f.log),
	})
	f.script = wrapperCopy(t, f.dir, "jj",
		[2]string{"real=/usr/local/bin/jj", "real=" + f.jj},
		[2]string{"wrapper=/opt/legion/bin/git", "wrapper=" + f.gitWrapper},
		[2]string{"legion=/opt/legion/bin/legion", "legion=" + passingLegion(t, filepath.Join(f.dir, "stand-in-legion"))},
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
	f.setup.Run(f.dir, jjInit(f.jj, repo)...)
	f.setup.Run(repo, f.jj, "git", "remote", "add", "origin", f.remote)
	return repo
}

// jjInit is fixturegit.JJInit of dir, a repository jj does not colocate, made by the jj at jj
// rather than the first jj on PATH.
func jjInit(jj, dir string) []string {
	return append([]string{jj}, fixturegit.JJInit(dir, false)[1:]...)
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
// push rather than take it for the bare `git` the read used to stand for. A push it lets through
// goes to legion jj, here a stand-in that runs the real jj with the line the wrapper hands it, the
// git wrapper's layers included, and no step (passingLegion).
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
		{name: "a --config-file jj cannot read", args: []string{"git", "push", "--config-file", filepath.Join(f.dir, "missing.toml")}, refused: true, says: "jj could not read the git.executable-path this push would use"},
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

// A push the jj wrapper refuses because jj could not read its git.executable-path carries, on the
// same standard error and ahead of the refusal, what jj itself said: the read's standard error is
// the wrapper's own. When the read's standard error went to /dev/null, `jj git push -R <a directory
// with no repository>` and `jj git push --config foo` printed the wrapper's setup error alone, and
// the cause jj gave was lost. A configuration
// argument written after `--` is jj's to refuse as well: the push keeps it after its `--`, where
// jj refuses it as an unexpected argument, and the read, which took a `--config` wherever it was
// written, refused the push for a value no push reads. The read takes only the ones before the
// first `--`, as the push does, so the error that reaches the caller is jj's own. A `-R` written
// after that `--` is jj's to refuse too, and the read, which took one wherever it was written,
// opened its directory as the push's repository; the wrapper reads a `-R` only before the first
// `--`. What jj says to each case is plain jj's, which the case runs first (measured with jj 0.45.1
// and 0.46.0-sami): a case whose plain jj no longer says it fails as a stale expectation, not as
// the wrapper's fault.
func TestTheJJWrapperPrintsJJsOwnErrorForAPushItCannotRead(t *testing.T) {
	f := newJJWrapper(t)
	repo := f.repo("repo")
	empty := t.TempDir()
	const refusal = "LEAK CHECK SETUP ERROR: the push: jj could not read the git.executable-path this push would use"

	for _, tc := range []struct {
		name string
		args []string
		// refused is whether the wrapper must refuse the push, rather than run it for jj to refuse.
		refused bool
		// says is a stable part of what plain jj prints for the same command, which the wrapper's
		// standard error must carry, ahead of the refusal where there is one.
		says string
	}{
		{name: "a -R naming a directory with no repository", args: []string{"git", "push", "-R", empty}, refused: true, says: "There is no jj repo in"},
		{name: "a --config with no =", args: []string{"git", "push", "--config", "foo"}, refused: true, says: "--config must be specified as NAME=VALUE"},
		{name: "a --config after --", args: []string{"git", "push", "--", "--config", "git.executable-path=/usr/bin/git"}, says: "unexpected argument '--config' found"},
		{name: "a -R after --", args: []string{"git", "push", "--", "-R", empty}, says: "unexpected argument '-R' found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, err := f.setup.Try(repo, append([]string{f.jj}, tc.args...)...); err == nil || !strings.Contains(out, tc.says) {
				t.Fatalf("plain jj %q does not fail saying %q, so what this case expects of the wrapper is stale: %v\n%s", tc.args, tc.says, err, out)
			}
			wrapped := exec.Command(f.script, tc.args...)
			wrapped.Dir = repo
			wrapped.Env = f.run.Env
			var stdout, stderr strings.Builder
			wrapped.Stdout, wrapped.Stderr = &stdout, &stderr
			err := wrapped.Run()
			out := stderr.String()
			if stdout.Len() != 0 {
				t.Fatalf("the wrapper printed %q on its standard output, where a push that fails prints nothing", stdout.String())
			}
			if !tc.refused {
				if strings.Contains(out, "LEAK CHECK SETUP ERROR") || err == nil || !strings.Contains(out, tc.says) {
					t.Fatalf("the wrapper did not leave the push for jj to refuse saying %q: %v\n%s", tc.says, err, out)
				}
				return
			}
			assertRefused(t, out, err, refusal)
			if before, _, _ := strings.Cut(out, refusal); !strings.Contains(before, tc.says) {
				t.Fatalf("jj's own %q does not come ahead of the refusal, so the cause jj gave is lost:\n%s", tc.says, out)
			}
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
	f.setup.Run(repo, f.jj, "describe", "-m", "a change")

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
			f.setup.Run(f.dir, f.jj, "-R", named, "config", "set", "--repo", "git.executable-path", tc.executable)
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
	f.setup.Run(repo, f.jj, "describe", "-m", "a change")
	f.setup.Run(repo, f.jj, "bookmark", "create", "probe", "-r", "@")
	repoConfig := f.setup.Run(repo, f.jj, "config", "path", "--repo")

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
	f.setup.Run(repo, f.jj, "describe", "-m", "a change")
	f.setup.Run(repo, f.jj, "bookmark", "create", "probe", "-r", "@")
	elsewhere := filepath.Join(f.dir, "elsewhere.git")
	f.setup.Run(f.dir, fixturegit.InitBare(elsewhere, "main")...)
	f.setup.Run(repo, f.jj, "git", "remote", "add", "elsewhere", elsewhere)

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
			if out, err := f.setup.Try(repo, append([]string{f.jj}, args...)...); !strings.Contains(out, tc.says) {
				t.Fatalf("plain jj %q does not say %q, so what this case expects of the wrapper is stale: %v\n%s", args, tc.says, err, out)
			}
			out, err := f.run.Try(repo, append([]string{f.script}, args...)...)
			if !strings.Contains(out, tc.says) {
				t.Fatalf("the wrapper's push of %q does not say %q as plain jj does, so it ran with a configuration other than the one its read checked: %v\n%s", args, tc.says, err, out)
			}
		})
	}
}

// passingLegion is a stand-in for legion at path that takes `jj --real <real> -- <arguments>`, as
// the jj wrapper hands it a push, and runs the real jj with the arguments, as legion jj does when
// its step has nothing to do. Any other command line exits 97.
func passingLegion(t *testing.T, path string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"[ \"$1\" = jj ] && [ \"$2\" = --real ] && [ \"$4\" = -- ] || exit 97\n" +
		"real=$3\n" +
		"shift 4\n" +
		"exec \"$real\" \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// The jj wrapper hands a line it reads as a `jj git push` to legion jj, which runs the step that
// sets each commit's author, where its rules let it, and then the push: after its setup checks,
// it runs legion as `jj --real <the real jj> -- <the line>`, the line being the one it runs the
// real jj with for a `jj gerrit upload`. That line is the git wrapper's first layer
// (`--config=git.executable-path="<the git wrapper>"` and `--config-file=/dev/null`), then the
// command line as jj expands it, global options in front of the command included, with the git
// wrapper once more as its last layer, which goes right in front of the command where no
// `--config` or `--config-file` of the caller's is after it, as in each push here. An alias of the
// repository's own that expands to a push reaches legion as its expansion. Every other command
// reaches the real jj, the first layer in front of its arguments, and legion is not run; so does
// `jj gerrit upload`, which the wrapper checks as a push, and which therefore gets the last layer
// too. The two setup refusals still come first: a git on PATH that is not the git wrapper, or a
// git.executable-path naming another git, is refused in the shared check's words and with its
// status, and legion is never run.
func TestTheJJWrapperHandsAPushToLegionJJ(t *testing.T) {
	jj := fixturegit.CompiledJJ(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// The git wrapper, under the name and in the directory the jj wrapper looks for on PATH.
	gitWrapper := wrapperCopy(t, bin, "git", [2]string{
		"real=/usr/bin/git", "real=" + logging(t, filepath.Join(dir, "stand-in-git"), filepath.Join(dir, "git-argv")),
	})
	repo := filepath.Join(dir, "repo")
	run := fixturegit.Isolated(t, filepath.Join(dir, "home")).With("PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"))
	run.Run(dir, jjInit(jj, repo)...)
	// An alias of the repository's own that expands to a push, which jj reads from the repository
	// a command runs in.
	run.Run(repo, jj, "config", "set", "--repo", "aliases.up", `["git", "push", "-b", "b"]`)
	// copyWith is a copy of the jj wrapper in a directory of its own whose real jj is real and
	// whose legion is a logging stand-in, and the log of that legion.
	copyWith := func(t *testing.T, real string) (script, legionLog string) {
		t.Helper()
		at := t.TempDir()
		legionLog = filepath.Join(at, "legion-argv")
		script = wrapperCopy(t, at, "jj",
			[2]string{"real=/usr/local/bin/jj", "real=" + real},
			[2]string{"wrapper=/opt/legion/bin/git", "wrapper=" + gitWrapper},
			[2]string{"legion=/opt/legion/bin/legion", "legion=" + logging(t, filepath.Join(at, "stand-in-legion"), legionLog)},
		)
		return script, legionLog
	}
	// first is the git wrapper's layer in front of every line the wrapper runs; last is the layer
	// it writes once more on a push, the last one jj reads.
	force := `--config=git.executable-path="` + gitWrapper + `"`
	first := []string{force, "--config-file=/dev/null"}
	last := []string{"--config-file=/dev/null", force}

	for _, tc := range []struct {
		// options are the global options in front of the command; command is the rest of the line.
		options, command []string
		// expansion is what jj expands command to; command itself where nil.
		expansion []string
	}{
		{command: []string{"git", "push", "--bookmark", "legion/K-1"}},
		{options: []string{"-R", repo, "--config", "user.email=a@example.invalid"}, command: []string{"git", "push", "-c", "@", "--dry-run"}},
		{options: []string{"--at-op", "@"}, command: []string{"git", "--color", "never", "push", "--named", "x=@"}},
		{options: []string{"--repository=" + repo, "--config=ui.color=\"never\""}, command: []string{"git", "push", "--change=@"}},
		// aliases.up, of the repository's own configuration.
		{command: []string{"up"}, expansion: []string{"git", "push", "-b", "b"}},
	} {
		args := slices.Concat(tc.options, tc.command)
		t.Run("push "+strings.ReplaceAll(strings.Join(args, " "), repo, "<repo>"), func(t *testing.T) {
			script, legionLog := copyWith(t, jj)
			if out, err := run.Try(repo, append([]string{script}, args...)...); err != nil {
				t.Fatalf("the wrapper: %v\n%s", err, out)
			}
			expansion := tc.expansion
			if expansion == nil {
				expansion = tc.command
			}
			want := slices.Concat([]string{"jj", "--real", jj, "--"}, first, tc.options, last, expansion)
			if got := ranAs(t, legionLog); !slices.Equal(got, want) {
				t.Fatalf("the wrapper ran legion with %q, want %q", got, want)
			}
		})
	}

	for _, tc := range []struct {
		// options are the global options in front of the command; command is the rest of the line.
		options, command []string
		// checked is whether the wrapper checks the line as a push, and so writes its last layer.
		checked bool
	}{
		{command: []string{"log", "-r", "@", "--no-graph"}},
		{command: []string{"git", "fetch", "--remote", "push"}},
		{options: []string{"--config", "git.executable-path=/usr/bin/git"}, command: []string{"status"}},
		{options: []string{"-R", repo}, command: []string{"git", "remote", "list"}},
		{command: []string{"gerrit", "upload", "-r", "@", "--remote", "origin", "-b", "main"}, checked: true},
	} {
		args := slices.Concat(tc.options, tc.command)
		t.Run("not to legion: "+strings.ReplaceAll(strings.Join(args, " "), repo, "<repo>"), func(t *testing.T) {
			// The real jj is a stand-in that hands its arguments to logging, which logs them, but for
			// the read of the git a push would use, as the wrapper makes it for a line with no
			// repository or configuration option, which it hands to the real jj.
			at := t.TempDir()
			realLog := filepath.Join(at, "argv")
			logged := logging(t, filepath.Join(at, "logged-jj"), realLog)
			standIn := filepath.Join(at, "stand-in-jj")
			body := "#!/bin/sh\n" +
				"[ \"$*\" = '--ignore-working-copy --color=never config get git.executable-path' ] && exec '" + jj + "' \"$@\"\n" +
				"exec '" + logged + "' \"$@\"\n"
			if err := os.WriteFile(standIn, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			script, legionLog := copyWith(t, standIn)
			if out, err := run.Try(repo, append([]string{script}, args...)...); err != nil {
				t.Fatalf("the wrapper: %v\n%s", err, out)
			}
			want := slices.Concat(first, args)
			if tc.checked {
				want = slices.Concat(first, tc.options, last, tc.command)
			}
			if got := ranAs(t, realLog); !slices.Equal(got, want) {
				t.Fatalf("the wrapper ran the real jj with %q, want %q", got, want)
			}
			if _, err := os.Stat(legionLog); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the wrapper ran legion for a command legion jj does not take (%v)", err)
			}
		})
	}

	for _, tc := range []struct {
		name string
		path string
		args []string
		// names is what the refusal names.
		names string
	}{
		{name: "a git on PATH that is not the wrapper", path: os.Getenv("PATH"), args: []string{"git", "push"}, names: "the git on PATH is"},
		{name: "a --config naming another git", path: bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			args: []string{"git", "push", "--config", "git.executable-path=/usr/bin/git"}, names: "/usr/bin/git"},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			script, legionLog := copyWith(t, jj)
			out, err := run.With("PATH="+tc.path).Try(repo, append([]string{script}, tc.args...)...)
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 2 || !strings.Contains(out, "LEAK CHECK SETUP ERROR") || !strings.Contains(out, tc.names) {
				t.Fatalf("the wrapper exited %v and printed\n%s\nwant the shared check's setup error, naming %q, and status 2", err, out, tc.names)
			}
			if _, err := os.Stat(legionLog); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the wrapper ran legion after a setup refusal (%v)", err)
			}
		})
	}
}

// The jj wrapper checks the push jj would make, from the repository that push uses and through
// whatever jj expands the command line to. clap takes `-R` and `--repository` after `git push` as
// well as before it, the last of them names the repository, and `-R=<path>` names <path>; jj puts
// an alias's words in place of the command word, expands an alias those words name in turn, and
// puts ui.default-command in front of a command line that names no command, reading both from the
// repository the command runs in. The wrapper once took the repository only from in front of
// `git push`, kept a `-R=<path>` as the path `=<path>`, took a failed read for an unset value, and
// read only the words it was given: each of those let a push through another git, with no
// pre-push hook and so no leak check. jj answers `git` when nothing sets the value, so a read that
// fails is an error, and the wrapper refuses it, as it refuses an expansion it cannot read.
//
// A security review of that wrapper found more ways past it, each a push through another git:
// `jj gerrit upload`, which pushes to refs/for/<branch> and was let through as a built-in command;
// an alias named with a space (`git help`), which matched two built-in names in the wrapper's list;
// an alias named `enabled`, whose entry the wrapper's template took for a table's `enabled`; an
// empty command word and a lone `-`, which jj takes as command words and the wrapper did not; and a
// name jj expands twice, which the wrapper took for a loop jj refuses. And three commands base ran
// that the wrapper refused: a table alias with a `description`, shell completion, and `op`, clap's
// own name for `operation`, which jj never takes as an alias. A fourth, `jj --version` where the
// default command is a push, is here too.
//
// A second review found three more. An option jj does not know, in front of a built-in command's
// name, makes clap stop before the name, so jj reads no command word and puts ui.default-command
// in front of the line: `jj -b log`, where the default command is `git push`, pushes the bookmark
// log, and the wrapper ran it as `log`, with no check. `debug` is a built-in command that `jj
// help` does not list, so the wrapper ran an alias of that name, which jj ignores. And a
// `--config` that sets a whole table (`git={…}`) shared a layer with the wrapper's own `--config`
// in front of the line, and jj refused the line ("Would overwrite entire table").
//
// And a review of that wrapper found that it read aliases without jj's defaults, which hold `st`,
// `b`, `ci` and `desc`: it left those to jj with its last layer after the alias word, which jj
// then put after `status`, a deeper level than the caller's own `--config`, so jj dropped the
// caller's: `jj --config <a template> st` ran without it.
func TestTheJJWrapperChecksThePushJJWouldMake(t *testing.T) {
	jj := fixturegit.CompiledJJ(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// The git wrapper, under the name and in the directory the jj wrapper looks for on PATH.
	gitWrapper := wrapperCopy(t, bin, "git", [2]string{
		"real=/usr/bin/git", "real=" + logging(t, filepath.Join(dir, "stand-in-git"), filepath.Join(dir, "argv")),
	})
	script := wrapperCopy(t, dir, "jj",
		[2]string{"real=/usr/local/bin/jj", "real=" + jj},
		[2]string{"wrapper=/opt/legion/bin/git", "wrapper=" + gitWrapper},
		[2]string{"legion=/opt/legion/bin/legion", "legion=" + passingLegion(t, filepath.Join(dir, "stand-in-legion"))},
	)
	run := fixturegit.Isolated(t, filepath.Join(dir, "home")).With("PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"))
	// plain names no git of its own and has the remote a real push has, so a push from it runs to
	// jj's "Nothing changed." under jj 0.45 and 0.46 alike and runs no git.
	plain := filepath.Join(dir, "plain")
	run.Run(dir, jjInit(jj, plain)...)
	remote := filepath.Join(dir, "remote.git")
	run.Run(dir, fixturegit.InitBare(remote, "")...)
	run.Run(plain, jj, "git", "remote", "add", "origin", remote)
	// another names another git in its own configuration.
	another := filepath.Join(dir, "another")
	run.Run(dir, jjInit(jj, another)...)
	run.Run(another, jj, "config", "set", "--repo", "git.executable-path", "/usr/bin/git")
	configFile := filepath.Join(dir, "another.toml")
	if err := os.WriteFile(configFile, []byte("git.executable-path = \"/usr/bin/git\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// aliased names another git in its own configuration, beside the aliases jj reads from the
	// repository a command runs in. jj ignores the aliases that have a built-in command's name.
	aliased := filepath.Join(dir, "aliased")
	run.Run(dir, jjInit(jj, aliased)...)
	// defaulted names another git and runs `git push` for a command line that names no command.
	// Its bookmark has a built-in command's name, log.
	defaulted := filepath.Join(dir, "defaulted")
	run.Run(dir, jjInit(jj, defaulted)...)
	run.Run(defaulted, jj, "bookmark", "create", "log", "-r", "@")
	// carrier names no git of its own; its alias pushes from another, which does.
	carrier := filepath.Join(dir, "carrier")
	run.Run(dir, jjInit(jj, carrier)...)
	// twice names another git, and its default command names an alias of options only, which a
	// command line naming that alias has expanded already.
	twice := filepath.Join(dir, "twice")
	run.Run(dir, jjInit(jj, twice)...)
	for _, setting := range []struct{ repository, key, value string }{
		{aliased, "git.executable-path", "/usr/bin/git"},
		{aliased, "aliases.p", `["git", "push"]`},
		{aliased, "aliases.pp", `["p"]`},
		{aliased, "aliases.v", `["version"]`},
		{aliased, "aliases.log", `["git", "push"]`},
		{aliased, "aliases.op", `["git", "push"]`},
		{aliased, "aliases.nl", `["git", "push\nx"]`},
		{aliased, `aliases."git help"`, `["git", "push"]`},
		{aliased, "aliases.enabled", `["git", "push"]`},
		{aliased, `aliases.""`, `["git", "push"]`},
		{aliased, `aliases."-"`, `["git", "push"]`},
		{aliased, "aliases.q", `["--quiet"]`},
		{aliased, "aliases.debug", `["git", "push"]`},
		{defaulted, "git.executable-path", "/usr/bin/git"},
		{defaulted, "ui.default-command", `["git", "push"]`},
		{carrier, "aliases.pa", `["git", "push", "-R", ` + strconv.Quote(another) + `]`},
		{twice, "git.executable-path", "/usr/bin/git"},
		{twice, "aliases.q", `["--quiet"]`},
		{twice, "ui.default-command", `["q", "git", "push"]`},
		{plain, "aliases.tp.definition", `["git", "push"]`},
		{plain, "aliases.tp.description", `"a table alias, which jj 0.45 and 0.46 both run"`},
	} {
		run.Run(setting.repository, jj, "config", "set", "--repo", setting.key, setting.value)
	}

	for _, tc := range []struct {
		name string
		// in is the directory the command runs in; dir, which is no repository, where empty, as a
		// push of another directory's repository does.
		in   string
		args []string
		// env is added to the command's environment.
		env []string
		// names is what a refusal must name; none where the wrapper must run the command.
		names []string
		// prints is what the command's output must hold, where it runs.
		prints string
	}{
		{name: "a -R after git push naming a repository that names another git", args: []string{"git", "push", "-R", another}, names: []string{"/usr/bin/git"}},
		{name: "a --repository= after git push naming that repository", args: []string{"git", "push", "--repository=" + another}, names: []string{"/usr/bin/git"}},
		{name: "a -R= in front of git push naming that repository", args: []string{"-R=" + another, "git", "push"}, names: []string{"/usr/bin/git"}},
		{name: "a second -R, the last, naming that repository", args: []string{"-R", plain, "git", "push", "-R", another}, names: []string{"/usr/bin/git"}},
		{name: "a --config-file= after git push naming another git", args: []string{"git", "push", "-R", plain, "--config-file=" + configFile}, names: []string{"/usr/bin/git"}},
		{name: "a -R naming no repository", args: []string{"git", "push", "-R", filepath.Join(dir, "nowhere")}, names: []string{"could not read"}},
		{name: "an alias that expands to git push", in: aliased, args: []string{"p"}, names: []string{"/usr/bin/git", "through aliases.p"}},
		{name: "an alias that expands to an alias that expands to git push", in: aliased, args: []string{"pp"}, names: []string{"/usr/bin/git", "through aliases.pp, aliases.p"}},
		{name: "ui.default-command set to git push", in: defaulted, names: []string{"/usr/bin/git", "through ui.default-command"}},
		{name: "an alias whose -R names a repository that names another git", in: carrier, args: []string{"pa"}, names: []string{"/usr/bin/git", "through aliases.pa"}},
		{name: "an alias with a line break in a word", in: aliased, args: []string{"nl"}, names: []string{"line break"}},
		{name: "gerrit upload, a push to refs/for, from a repository that names another git", in: aliased, args: []string{"gerrit", "upload", "-r", "@", "--remote", "origin", "-b", "main"}, names: []string{"/usr/bin/git", "gerrit upload"}},
		{name: "an alias whose name holds a space and two built-in names", in: aliased, args: []string{"git help"}, names: []string{"holds characters"}},
		{name: "an alias named enabled", in: aliased, args: []string{"enabled"}, names: []string{"/usr/bin/git", "through aliases.enabled"}},
		{name: "an empty command word, an alias named with no letters", in: aliased, args: []string{""}, names: []string{"is empty"}},
		{name: "a lone - as the command word, an alias named -", in: aliased, args: []string{"-"}, names: []string{"/usr/bin/git", "through aliases.-"}},
		{name: "an alias of options only, named twice", in: aliased, args: []string{"q", "q", "git", "push"}, names: []string{"a second time"}},
		{name: "a default command naming the alias the command line expanded", in: twice, args: []string{"q"}, names: []string{"a second time"}},
		{name: "a second -R, the last, naming a repository that names no git", args: []string{"-R", another, "git", "push", "-R", plain}},
		{name: "a -R after git push naming a repository that names no git", args: []string{"git", "push", "-R", plain}},
		{name: "an alias that expands to no push", in: aliased, args: []string{"v"}},
		{name: "an alias with a built-in command's name, which jj ignores", in: aliased, args: []string{"log", "-r", "@", "--no-graph"}},
		{name: "op, clap's own name for operation, which jj ignores as an alias", in: aliased, args: []string{"op", "log", "-n", "1", "--no-graph"}},
		{name: "a table alias with a description, from a repository that names no git", in: plain, args: []string{"tp"}},
		{name: "shell completion, which runs no command", in: plain, env: []string{"COMPLETE=bash"}},
		{name: "--version where the default command is a push", in: twice, args: []string{"--version"}},
		{name: "an option jj does not know, in front of a built-in command's name, where the default command is a push", in: defaulted, args: []string{"-b", "log"}, names: []string{"/usr/bin/git", "through ui.default-command"}},
		{name: "debug, a built-in command jj's help does not list, which jj ignores as an alias", in: aliased, args: []string{"debug", "--help"}},
		{name: "a --config that sets a whole table, git's", in: plain, args: []string{"--config", `git={private-commits="none()"}`, "log", "-r", "@", "--no-graph"}},
		{name: "st, an alias in jj's defaults, keeps the caller's --config", in: plain, args: []string{"--config", `templates.commit_summary="\"MARKER\""`, "st"}, prints: "MARKER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if in == "" {
				in = dir
			}
			out, err := run.With(tc.env...).Try(in, append([]string{script}, tc.args...)...)
			refused := strings.Contains(out, "LEAK CHECK SETUP ERROR")
			if refused != (len(tc.names) > 0) {
				t.Fatalf("the wrapper refused=%v, want %v: %v\n%s", refused, len(tc.names) > 0, err, out)
			}
			if len(tc.names) == 0 {
				if err != nil {
					t.Fatalf("the wrapper did not run the command: %v\n%s", err, out)
				}
				if !strings.Contains(out, tc.prints) {
					t.Fatalf("the command's output does not hold %q:\n%s", tc.prints, out)
				}
				return
			}
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 2 {
				t.Fatalf("the refusal exited %v, want 2, the shared check's own setup-error status\n%s", err, out)
			}
			for _, name := range tc.names {
				if !strings.Contains(out, name) {
					t.Fatalf("the refusal does not name %q:\n%s", name, out)
				}
			}
		})
	}
}

// Every push the jj wrapper runs reaches the remote through the git wrapper, whatever git a
// configuration names for the push's own command. jj reads a `[[--scope]]` with `--when.commands`
// only for the command it names, and the wrapper's read of `git.executable-path` is a command of
// its own (`config get`), so it read `git` where the push used another git: the worker prompt's
// own push went through /usr/bin/git, with the git wrapper run 0 times, in jj 0.45.1, 0.46.0 and
// 0.47.1 alike. The wrapper now gives jj the git wrapper on the command line, the last layer, which
// outranks every file and every scope. And it runs the expansion it read, so an alias that a scope
// for `config` shows the wrapper as one command and jj as another runs as the one the wrapper read.
//
// A second review found two more. An alias whose words put an option jj does not know in front of
// a built-in command's name (`-b log`) makes jj put the default command, a push, in front of the
// line it expanded, and the wrapper ran that line as `log`, with only the git wrapper in front of
// it, where the alias's own `--config` outranked it: /usr/bin/git pushed the bookmark log. And a
// push whose own `--config`, written in front of `git push`, sets git's whole table shared a layer
// with the wrapper's `--config` in front of it, so jj refused the push.
//
// And clap keeps each of `--config` and `--config-file` only from the deepest command level that
// has any, so a last layer written deeper than the caller's own made jj drop them: a push whose own
// `--config` makes its commit private, in front of `git push`, between `git` and `push`, or in a
// `--config-file` in front of a `--config` after it, went through the git wrapper, where jj alone
// refuses it. The last layer now goes at the deepest level that has a caller's own, and the
// caller's shallower ones of the other option are written there once more. And the read of the
// git took every `--config`, so it refused a git named in front of `git` that a `--config` between
// `git` and `push` drops, where jj alone pushes through the git wrapper.
//
// Each row pushes a commit from a repository of its own to a bare remote. The git wrapper's real
// git is a logging stand-in that pushes nothing, so the remote holds the bookmark only when another
// git ran the push. A row that makes no push says why, in jj's own words where jj refuses it, and
// a row whose options the wrapper writes once more (the caller's `--config` or `--config-file` at
// a shallower level than its other option) shows such a line still pushes. The control runs jj
// itself, with the scoped git: the remote then holds the bookmark, so the test can see another git
// push.
func TestTheJJWrapperRunsEveryPushThroughTheGitWrapper(t *testing.T) {
	jj := fixturegit.CompiledJJ(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "argv")
	gitWrapper := wrapperCopy(t, bin, "git", [2]string{
		"real=/usr/bin/git", "real=" + logging(t, filepath.Join(dir, "stand-in-git"), log),
	})
	script := wrapperCopy(t, dir, "jj",
		[2]string{"real=/usr/local/bin/jj", "real=" + jj},
		[2]string{"wrapper=/opt/legion/bin/git", "wrapper=" + gitWrapper},
		[2]string{"legion=/opt/legion/bin/legion", "legion=" + passingLegion(t, filepath.Join(dir, "stand-in-legion"))},
	)
	run := fixturegit.Isolated(t, filepath.Join(dir, "home")).With(
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid",
	)
	// The scope that names another git for the command `git` alone, in TOML.
	const scoped = "[[--scope]]\n--when.commands = [\"git\"]\ngit.executable-path = \"/usr/bin/git\"\n"
	// An alias that pushes through another git, which a scope for `config` shows as `version`.
	hidden := "aliases.p = [\"--config\", \"git.executable-path=\\\"/usr/bin/git\\\"\", \"git\", \"push\", \"-b\", \"b\"]\n" +
		"[[--scope]]\n--when.commands = [\"config\"]\naliases.p = [\"version\"]\n"
	// A default command that pushes, and an alias that puts another git and an option jj does not
	// know in front of the bookmark log, a built-in command's name.
	unknown := "ui.default-command = [\"git\", \"push\"]\n" +
		"aliases.p = [\"--config=git.executable-path=\\\"/usr/bin/git\\\"\", \"-b\", \"log\"]\n"
	// A configuration file that makes the bookmark b's commit private.
	private := filepath.Join(dir, "private.toml")
	if err := os.WriteFile(private, []byte("git.private-commits = \"b\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		// config is added to the repository's own configuration file.
		config string
		// args is the command line, where "<repo>" stands for the repository's path.
		args []string
		// bookmark is the bookmark the repository has; b where empty.
		bookmark string
		// wrapped is whether the push must reach the git wrapper; otherwise the command must make
		// no push at all.
		wrapped bool
		// refused is whether the wrapper must refuse the command.
		refused bool
		// says is what the output must hold.
		says string
		// control runs jj itself, not the wrapper, and the remote must then hold the bookmark.
		control bool
	}{
		{name: "control: jj itself, the git scoped to the git command, pushes through that git", config: scoped, args: []string{"-R", "<repo>", "git", "push", "--bookmark", "b"}, control: true},
		{name: "the worker prompt's push, the git scoped to the git command", config: scoped, args: []string{"-R", "<repo>", "git", "push", "--bookmark", "b"}, wrapped: true},
		{name: "a push whose own --config scopes the git to the git command", args: []string{"git", "push", "-b", "b", "--config", `"--scope"=[{"--when" = {commands = ["git"]}, git = {executable-path = "/usr/bin/git"}}]`}, wrapped: true},
		{name: "an alias that a scope for config shows as version, pushing through another git", config: hidden, args: []string{"p"}},
		{name: "a push from a repository that names no git", args: []string{"git", "push", "-b", "b"}, wrapped: true},
		{name: "a push ended by --", args: []string{"git", "push", "-b", "b", "--"}, wrapped: true},
		{name: "an alias that puts an option jj does not know in front of log, where the default command is a push", config: unknown, args: []string{"p"}, bookmark: "log", refused: true},
		{name: "a push whose own --config, in front of it, sets git's whole table", args: []string{"--config", `git={private-commits="none()"}`, "git", "push", "-b", "b"}, wrapped: true},
		{name: "a push whose own --config, in front of it, makes its commit private, which jj then refuses to push", args: []string{"--config", `git.private-commits="b"`, "git", "push", "-b", "b"}, says: "since it is private"},
		{name: "a push whose own --config, in front of it, makes its commit private, with a --config-file after the push", args: []string{"--config", `git.private-commits="b"`, "git", "push", "-b", "b", "--config-file=/dev/null"}, says: "since it is private"},
		{name: "a push whose own --config, between git and push, makes its commit private", args: []string{"git", "--config", `git.private-commits="b"`, "push", "-b", "b"}, says: "since it is private"},
		{name: "a --config-file in front of the push that makes its commit private, with a --config after the push", args: []string{"--config-file", private, "git", "push", "-b", "b", "--config", "ui.color=never"}, says: "since it is private"},
		{name: "a push with a --config in front of it and a --config-file after it", args: []string{"--config", "ui.color=never", "git", "push", "-b", "b", "--config-file=/dev/null"}, wrapped: true},
		{name: "a push with a --config-file in front of it and a --config after it", args: []string{"--config-file=/dev/null", "git", "push", "-b", "b", "--config", "ui.color=never"}, wrapped: true},
		{name: "a --config naming another git in front of git, which a --config between git and push drops", args: []string{"--config", `git.executable-path="/usr/bin/git"`, "git", "--config", "ui.color=never", "push", "-b", "b"}, wrapped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			bookmark := tc.bookmark
			if bookmark == "" {
				bookmark = "b"
			}
			remote := repo + "-remote.git"
			run.Run(dir, jjInit(jj, repo)...)
			run.Run(dir, fixturegit.InitBare(remote, "")...)
			if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			run.Run(repo, jj, "commit", "-m", "c")
			run.Run(repo, jj, "bookmark", "create", bookmark, "-r", "@-")
			run.Run(repo, jj, "git", "remote", "add", "origin", remote)
			if tc.config != "" {
				path := strings.TrimSpace(run.Out(repo, jj, "config", "path", "--repo"))
				file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString("\n" + tc.config)
				if closed := file.Close(); err == nil {
					err = closed
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(log); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			args := slices.Clone(tc.args)
			for i, arg := range args {
				if arg == "<repo>" {
					args[i] = repo
				}
			}
			command := script
			if tc.control {
				command = jj
			}
			out, _ := run.Try(repo, append([]string{command}, args...)...)
			_, err := run.Try(dir, "git", "--git-dir", remote, "rev-parse", "-q", "--verify", "refs/heads/"+bookmark)
			if landed := err == nil; landed != tc.control {
				if tc.control {
					t.Fatalf("jj itself left the remote without the bookmark, so this test could not see another git push it:\n%s", out)
				}
				t.Fatalf("another git pushed the bookmark to the remote:\n%s", out)
			}
			if tc.control {
				return
			}
			if refused := strings.Contains(out, "LEAK CHECK SETUP ERROR"); refused != tc.refused {
				t.Fatalf("the wrapper refused=%v, want %v:\n%s", refused, tc.refused, out)
			}
			if !strings.Contains(out, tc.says) {
				t.Fatalf("the output does not hold %q:\n%s", tc.says, out)
			}
			read, err := os.ReadFile(log)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			wrapped := strings.Contains(string(read), "push") && strings.Contains(string(read), "core.hooksPath=/opt/legion/githooks")
			if wrapped != tc.wrapped {
				t.Fatalf("the push reached the git wrapper=%v, want %v; the git wrapper ran with %q\n%s", wrapped, tc.wrapped, read, out)
			}
		})
	}
}
