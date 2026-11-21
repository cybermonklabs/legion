package leakguard

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
	// The remote a real push has, which the pushes below must be able to resolve. jj 0.45 ended a
	// `jj git push` with no remote at "Nothing changed." because its default revset named no
	// bookmark; jj 0.46, whose git.push became a pattern over the remotes, resolves the remotes
	// first and fails "No git remotes to push to", so without one the row's verdict was the host's
	// jj version. With one, both versions stop at "Nothing changed." and run no git.
	remote := filepath.Join(dir, "remote.git")
	run.Run(dir, fixturegit.InitBare(remote, "")...)
	run.Run(repo, "jj", "git", "remote", "add", "origin", remote)

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
func TestTheJJWrapperChecksThePushJJWouldMake(t *testing.T) {
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("this test runs the real jj, which reads the configuration: %v", err)
	}
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
	)
	run := fixturegit.Isolated(t, filepath.Join(dir, "home")).With("PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"))
	// plain names no git of its own and has the remote a real push has, so a push from it runs to
	// jj's "Nothing changed." under jj 0.45 and 0.46 alike and runs no git.
	plain := filepath.Join(dir, "plain")
	run.Run(dir, fixturegit.JJInit(plain, false)...)
	remote := filepath.Join(dir, "remote.git")
	run.Run(dir, fixturegit.InitBare(remote, "")...)
	run.Run(plain, "jj", "git", "remote", "add", "origin", remote)
	// another names another git in its own configuration.
	another := filepath.Join(dir, "another")
	run.Run(dir, fixturegit.JJInit(another, false)...)
	run.Run(another, "jj", "config", "set", "--repo", "git.executable-path", "/usr/bin/git")
	configFile := filepath.Join(dir, "another.toml")
	if err := os.WriteFile(configFile, []byte("git.executable-path = \"/usr/bin/git\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// aliased names another git in its own configuration, beside the aliases jj reads from the
	// repository a command runs in. jj ignores the aliases that have a built-in command's name.
	aliased := filepath.Join(dir, "aliased")
	run.Run(dir, fixturegit.JJInit(aliased, false)...)
	// defaulted names another git and runs `git push` for a command line that names no command.
	// Its bookmark has a built-in command's name, log.
	defaulted := filepath.Join(dir, "defaulted")
	run.Run(dir, fixturegit.JJInit(defaulted, false)...)
	run.Run(defaulted, "jj", "bookmark", "create", "log", "-r", "@")
	// carrier names no git of its own; its alias pushes from another, which does.
	carrier := filepath.Join(dir, "carrier")
	run.Run(dir, fixturegit.JJInit(carrier, false)...)
	// twice names another git, and its default command names an alias of options only, which a
	// command line naming that alias has expanded already.
	twice := filepath.Join(dir, "twice")
	run.Run(dir, fixturegit.JJInit(twice, false)...)
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
		run.Run(setting.repository, "jj", "config", "set", "--repo", setting.key, setting.value)
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
// git ran the push.
func TestTheJJWrapperRunsEveryPushThroughTheGitWrapper(t *testing.T) {
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("this test runs the real jj, which pushes: %v", err)
	}
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
	}{
		{name: "the worker prompt's push, the git scoped to the git command", config: scoped, args: []string{"-R", "<repo>", "git", "push", "--bookmark", "b"}, wrapped: true},
		{name: "a push whose own --config scopes the git to the git command", args: []string{"git", "push", "-b", "b", "--config", `"--scope"=[{"--when" = {commands = ["git"]}, git = {executable-path = "/usr/bin/git"}}]`}, wrapped: true},
		{name: "an alias that a scope for config shows as version, pushing through another git", config: hidden, args: []string{"p"}},
		{name: "a push from a repository that names no git", args: []string{"git", "push", "-b", "b"}, wrapped: true},
		{name: "a push ended by --", args: []string{"git", "push", "-b", "b", "--"}, wrapped: true},
		{name: "an alias that puts an option jj does not know in front of log, where the default command is a push", config: unknown, args: []string{"p"}, bookmark: "log", refused: true},
		{name: "a push whose own --config, in front of it, sets git's whole table", args: []string{"--config", `git={private-commits="none()"}`, "git", "push", "-b", "b"}, wrapped: true},
		{name: "a push whose own --config, in front of it, makes its commit private, which jj then refuses to push", args: []string{"--config", `git.private-commits="b"`, "git", "push", "-b", "b"}},
		{name: "a push whose own --config, in front of it, makes its commit private, with a --config-file after the push", args: []string{"--config", `git.private-commits="b"`, "git", "push", "-b", "b", "--config-file=/dev/null"}},
		{name: "a push whose own --config, between git and push, makes its commit private", args: []string{"git", "--config", `git.private-commits="b"`, "push", "-b", "b"}},
		{name: "a --config-file in front of the push that makes its commit private, with a --config after the push", args: []string{"--config-file", private, "git", "push", "-b", "b", "--config", "ui.color=never"}},
		{name: "a --config naming another git in front of git, which a --config between git and push drops", args: []string{"--config", `git.executable-path="/usr/bin/git"`, "git", "--config", "ui.color=never", "push", "-b", "b"}, wrapped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			bookmark := tc.bookmark
			if bookmark == "" {
				bookmark = "b"
			}
			remote := repo + "-remote.git"
			run.Run(dir, fixturegit.JJInit(repo, false)...)
			run.Run(dir, fixturegit.InitBare(remote, "")...)
			if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			run.Run(repo, "jj", "commit", "-m", "c")
			run.Run(repo, "jj", "bookmark", "create", bookmark, "-r", "@-")
			run.Run(repo, "jj", "git", "remote", "add", "origin", remote)
			if tc.config != "" {
				path := strings.TrimSpace(run.Out(repo, "jj", "config", "path", "--repo"))
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
			out, _ := run.Try(repo, append([]string{script}, args...)...)
			if _, err := run.Try(dir, "git", "--git-dir", remote, "rev-parse", "-q", "--verify", "refs/heads/"+bookmark); err == nil {
				t.Fatalf("another git pushed the bookmark to the remote:\n%s", out)
			}
			if refused := strings.Contains(out, "LEAK CHECK SETUP ERROR"); refused != tc.refused {
				t.Fatalf("the wrapper refused=%v, want %v:\n%s", refused, tc.refused, out)
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
