package main

import (
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// testGhStackEnv names a stand-in gh-stack for this test binary run as `legion`, in place of the
// image's root-owned copy (TestMain).
const testGhStackEnv = "LEGION_TEST_GH_STACK"

// remotesAre is a working directory whose git remotes are lines, each a fetch and a push line of
// one remote named and URL'd as given, then markers, each a `git config --get-regexp` line of a
// gh-resolved setting.
func remotesAre(urls map[string]string, markers ...string) func() ([]ghRemote, error) {
	return func() ([]ghRemote, error) {
		var remotes []ghRemote
		for _, name := range slices.Sorted(maps.Keys(urls)) {
			remotes = append(remotes, ghRemote{name: name, url: urls[name]}, ghRemote{name: name, url: urls[name], push: true})
		}
		for _, marker := range markers {
			remotes = append(remotes, ghMarkerLine(marker))
		}
		return remotes, nil
	}
}

// inNoRepository runs the rest of the test in a directory that is no git repository, reading no
// global or system git file, so the git settings and the pull request template a `gh stack` call
// reads are none.
func inNoRepository(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Chdir(t.TempDir())
}

// inClone runs the rest of the test in a git repository whose origin is acme/widgets, after
// each of setup's git commands, reading no global or system git file. It answers the
// repository's root.
func inClone(t *testing.T, setup ...[]string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	repository := t.TempDir()
	t.Chdir(repository)
	fixture := fixturegit.Isolated(t, t.TempDir())
	for _, args := range append([][]string{fixturegit.InitWorkTree("", "")[1:], {"remote", "add", "origin", "https://github.com/acme/widgets.git"}}, setup...) {
		fixture.Run(repository, append([]string{"git"}, args...)...)
	}
	return repository
}

// `gh stack` runs the image's gh-stack, which acts on the one repository go-gh's
// repository.Current answers: GH_REPO where set, else the working directory's github.com remotes
// and gh-resolved markers, of which it may take any. So it runs with the directory of that
// repository's owner, whatever its arguments name (a pull request URL is read for its number),
// and is refused where they name two. Every flag is read against gh-stack v0.3.0's own
// (ghStackCommands), since gh-stack hands --remote, branches and other values to git, which reads
// one that starts with `-` as an option of its own: an unknown flag or command, `--`, a value
// that starts with `-`, a --remote naming no listed remote and a remote whose name starts with
// `-` are refused, and so are `submit` without --auto, whose editor writes pull request text no
// guard reads, `alias`, `feedback`, and a command that creates pull requests where the remotes
// and GH_REPO do not name one repository.
func TestGhStackRoutesByTheRepositoryItActsOn(t *testing.T) {
	help := savedGhHelpOf(t, "2.104.0")
	inNoRepository(t)
	acme := remotesAre(map[string]string{"origin": "https://github.com/acme/widgets.git"})
	twoRemotes := remotesAre(map[string]string{"origin": "https://github.com/acme/widgets.git", "upstream": "https://github.com/acme/widgets.git"})
	for _, tc := range []struct {
		name    string
		argv    []string
		env     map[string]string
		remotes func() ([]ghRemote, error)
		owner   string
		repo    string
		refuse  string
	}{
		{name: "a view", argv: []string{"stack", "view", "--json"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "a submit to a named remote", argv: []string{"stack", "submit", "--auto", "--remote", "upstream"}, remotes: twoRemotes, owner: "acme", repo: "widgets"},
		{name: "a submit to a named remote, joined", argv: []string{"stack", "submit", "--auto=true", "--remote=origin", "--open"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "a link naming another repository's pull request URL", argv: []string{"stack", "link", "https://github.com/other/thing/pull/1", "2"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "a checkout of a pull request URL", argv: []string{"stack", "checkout", "https://github.com/acme/widgets/pull/3"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "a merge", argv: []string{"stack", "merge", "42", "-y", "--squash"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "an add with a message in a cluster", argv: []string{"stack", "add", "-Am", "add the layer", "feature"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "an init with a joined shorthand value", argv: []string{"stack", "init", "-b=main", "feature"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "an unstack by its alias", argv: []string{"stack", "delete", "7", "--local"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "the extension's help and version", argv: []string{"stack", "--help"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "the extension's version", argv: []string{"stack", "-v"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "a command's help", argv: []string{"stack", "submit", "-h"}, remotes: acme, refuse: "without --auto"},
		{name: "GH_REPO and the remotes of two owners", argv: []string{"stack", "view", "--json"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, remotes: acme, refuse: "more than one owner"},
		{name: "GH_REPO of the remotes' owner for a read", argv: []string{"stack", "view", "--json"}, env: map[string]string{"GH_REPO": "acme/other"}, remotes: acme, owner: "acme"},
		{name: "GH_REPO equal to the remotes for a write", argv: []string{"stack", "submit", "--auto"}, env: map[string]string{"GH_REPO": "acme/widgets"}, remotes: acme, owner: "acme", repo: "widgets"},
		{name: "a working directory that names no repository", argv: []string{"stack", "view", "--json"}, remotes: noGitRepository},

		{name: "a submit without --auto", argv: []string{"stack", "submit"}, remotes: acme, refuse: "without --auto"},
		{name: "a submit with --auto false", argv: []string{"stack", "submit", "--auto", "--auto=false"}, remotes: acme, refuse: "without --auto"},
		{name: "an upload-pack in --remote", argv: []string{"stack", "push", "--remote=--upload-pack=touch /tmp/x"}, remotes: acme, refuse: "starts with `-` is an option of git's own"},
		{name: "a receive-pack as --remote's next argument", argv: []string{"stack", "push", "--remote", "--receive-pack=touch /tmp/x"}, remotes: acme, refuse: "starts with `-` is an option of git's own"},
		{name: "an option in --base", argv: []string{"stack", "init", "-b", "--upload-pack=x", "feature"}, remotes: acme, refuse: "starts with `-` is an option of git's own"},
		{name: "a remote no git remote lists", argv: []string{"stack", "push", "--remote", "fork"}, remotes: acme, refuse: "is no remote `git remote -v` lists here (origin)"},
		{name: "a double dash", argv: []string{"stack", "add", "--", "--upload-pack=x"}, remotes: acme, refuse: "is given `--`"},
		{name: "an unknown flag", argv: []string{"stack", "view", "--bogus"}, remotes: acme, refuse: `has no flag "--bogus" in gh-stack 0.3.0`},
		{name: "another command's flag", argv: []string{"stack", "view", "--remote", "origin"}, remotes: acme, refuse: `has no flag "--remote"`},
		{name: "an unknown shorthand", argv: []string{"stack", "view", "-x"}, remotes: acme, refuse: "has no flag -x"},
		{name: "a flag before the command", argv: []string{"stack", "--json", "view"}, remotes: acme, refuse: "before its command's name"},
		{name: "a help before the command takes the next word", argv: []string{"stack", "-h", "push", "submit", "--help=false"}, remotes: acme, refuse: "before its command's name"},
		{name: "a help before the command hides a write", argv: []string{"stack", "-h", "view", "link", "--help=false", "a", "b"}, env: map[string]string{"GH_REPO": "acme/other"}, remotes: acme, refuse: "before its command's name"},
		{name: "a version before the command", argv: []string{"stack", "--version", "alias"}, remotes: acme, refuse: "before its command's name"},
		{name: "a help turned off", argv: []string{"stack", "submit", "--help=false"}, remotes: acme, refuse: "gives --help a value"},
		{name: "a shorthand help turned off", argv: []string{"stack", "push", "-h=false"}, remotes: acme, refuse: "gives -h a value"},
		{name: "a boolean given a value that is no boolean", argv: []string{"stack", "view", "--json=maybe"}, remotes: acme, refuse: "a flag of no value"},
		{name: "a flag with no value", argv: []string{"stack", "push", "--remote"}, remotes: acme, refuse: "no value"},
		{name: "alias", argv: []string{"stack", "alias", "gs"}, remotes: acme, refuse: "`gh stack alias` is no command of gh-stack 0.3.0 this runs"},
		{name: "feedback", argv: []string{"stack", "feedback", "hi"}, remotes: acme, refuse: "`gh stack feedback` is no command"},
		{name: "completion", argv: []string{"stack", "completion", "bash"}, remotes: acme, refuse: "`gh stack completion` is no command"},
		{name: "a write where GH_REPO names another repository", argv: []string{"stack", "submit", "--auto"}, env: map[string]string{"GH_REPO": "acme/other"}, remotes: acme, refuse: "do not name one repository"},
		{name: "a write in a directory with no repository", argv: []string{"stack", "link", "a", "b"}, remotes: noGitRepository, refuse: "do not name one repository"},
		{name: "two owners' remotes", argv: []string{"stack", "view", "--json"},
			remotes: remotesAre(map[string]string{"origin": "https://github.com/acme/widgets.git", "upstream": "https://github.com/sjawhar/legion.git"}), refuse: "more than one owner"},
		{name: "a marker naming another owner's repository", argv: []string{"stack", "view", "--json"},
			remotes: remotesAre(map[string]string{"origin": "https://github.com/acme/widgets.git"}, "remote.origin.gh-resolved sjawhar/legion"), refuse: "more than one owner"},
		{name: "GH_REPO on another host", argv: []string{"stack", "view", "--json"}, env: map[string]string{"GH_REPO": "gitlab.com/acme/widgets"}, remotes: acme, refuse: "another host than github.com"},
		{name: "an ssh remote on another host", argv: []string{"stack", "view", "--json"},
			remotes: remotesAre(map[string]string{"origin": "git@forge.internal.example:acme/widgets.git"}), refuse: "ssh remote"},
		{name: "an option before stack", argv: []string{"-R", "acme/widgets", "stack", "view"}, remotes: acme, refuse: "comes before `stack`"},
		{name: "a caller's token", argv: []string{"stack", "view", "--json"}, env: map[string]string{"GH_TOKEN": "t"}, remotes: acme, refuse: "GH_TOKEN is set"},
		{name: "any other extension", argv: []string{"poi", "list"}, remotes: acme, refuse: "is no command gh's own help lists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ghTargetOf(tc.argv, tc.env, help, tc.remotes, ownersOwnDirectories)
			if tc.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refuse) || !strings.Contains(err.Error(), "sends no") {
					t.Errorf("ghTargetOf(%q) = %+v, %v; want a refusal containing %q", tc.argv, target, err, tc.refuse)
				}
				return
			}
			wantDir := ""
			if tc.owner != "" {
				wantDir = ghconfig.Dir("/owners", tc.owner)
			}
			if err != nil || target.dir != wantDir || target.owner != tc.owner || target.repo != tc.repo || target.command != "stack" || target.files == nil || len(target.files) != 0 {
				t.Errorf("ghTargetOf(%q) = %+v, %v; want %s/%s's directory %q, command stack and no file", tc.argv, target, err, tc.owner, tc.repo, wantDir)
			}
		})
	}
}

// ghStackCobraRun is what a command of ghStackCobraTree ran with: its name, --auto's value, the
// --remote it took, and every string flag's value.
type ghStackCobraRun struct {
	name, remote string
	auto         bool
	values       []string
}

// ghStackCobraTree is gh-stack's command tree as cobra v1.10.2 (gh-stack v0.3.0's, and this
// module's) builds it from ghStackCommands: each command with its flags and shorthands, the root
// with a version, under an outer `gh` command as gh-stack's cmd/root.go Execute wraps it. Each
// command records into ran what it ran with; a help or a version records nothing.
func ghStackCobraTree(ran *ghStackCobraRun) *cobra.Command {
	root := &cobra.Command{Use: "stack", Version: ghStackVersion, SilenceUsage: true, SilenceErrors: true}
	for name, entry := range ghStackCommands {
		if name == "help" || name == "delete" {
			continue
		}
		command := &cobra.Command{Use: name, RunE: func(cmd *cobra.Command, _ []string) error {
			ran.name = name
			ran.auto, _ = cmd.Flags().GetBool("auto")
			ran.remote, _ = cmd.Flags().GetString("remote")
			cmd.Flags().Visit(func(flag *pflag.Flag) {
				if flag.Value.Type() == "string" {
					ran.values = append(ran.values, flag.Value.String())
				}
			})
			return nil
		}}
		if name == "unstack" {
			command.Aliases = []string{"delete"}
		}
		short := map[string]string{}
		for letter, long := range entry.short {
			short[long] = string(letter)
		}
		for flag, takesValue := range entry.flags {
			if takesValue {
				command.Flags().StringP(flag, short[flag], "", "")
			} else {
				command.Flags().BoolP(flag, short[flag], false, "")
			}
		}
		root.AddCommand(command)
	}
	gh := &cobra.Command{Use: "gh", SilenceUsage: true, SilenceErrors: true}
	gh.SetHelpFunc(func(*cobra.Command, []string) {})
	gh.SetOut(io.Discard)
	gh.SetErr(io.Discard)
	gh.AddCommand(root)
	return gh
}

// Whatever argv `legion gh stack` admits, gh-stack runs the command the wrapper read, or none, with
// the values the wrapper read: a command cobra would run is one readGhStackCall named, so the
// checks that hang on the command (submit's --auto, the writes' one repository and template,
// alias and feedback) are the ones that run; a submit it runs has --auto on; the --remote it takes
// is one the wrapper checked; and no string flag it takes starts with `-`. Every argv of up to
// three words over a vocabulary of commands, flags and values is run through both, then 60,000
// longer ones drawn with a fixed seed. Control: the vocabulary holds the help-before-the-command
// shapes that made the two disagree before.
func TestGhStackRunsTheCommandTheWrapperRead(t *testing.T) {
	words := []string{"submit", "push", "view", "link", "alias", "feedback", "delete", "help", "completion",
		"-h", "--help", "--help=false", "-h=false", "-v", "--version", "--version=false", "--auto", "--auto=false",
		"--remote", "--remote=origin", "--remote=-x", "--json", "-m", "-m-x", "-b", "-b=-x", "-y", "-A", "-Am", "--", "-", "-x", "origin", "x"}
	compare := func(argv []string) {
		call, err := readGhStackCall(argv)
		if err != nil {
			return
		}
		var ran ghStackCobraRun
		tree := ghStackCobraTree(&ran)
		tree.SetArgs(append([]string{"stack"}, argv...))
		_ = tree.Execute()
		if ran.name == "" {
			return
		}
		want := call.name
		if want == "delete" {
			want = "unstack"
		}
		switch {
		case ran.name != want:
			t.Errorf("readGhStackCall(%q) read %q, and gh-stack's parser runs %q", argv, call.name, ran.name)
		case ran.name == "submit" && !ran.auto:
			t.Errorf("readGhStackCall(%q) admits submit, and gh-stack's parser runs it without --auto", argv)
		case ran.remote != "" && !slices.Contains(call.remotes, ran.remote):
			t.Errorf("readGhStackCall(%q) read --remote %q, and gh-stack's parser takes %q", argv, call.remotes, ran.remote)
		case slices.ContainsFunc(ran.values, func(value string) bool { return strings.HasPrefix(value, "-") }):
			t.Errorf("readGhStackCall(%q) admits a call whose string flags gh-stack's parser takes as %q", argv, ran.values)
		}
	}
	var walk func(prefix []string)
	walk = func(prefix []string) {
		compare(prefix)
		if len(prefix) == 3 {
			return
		}
		for _, word := range words {
			walk(append(slices.Clone(prefix), word))
		}
	}
	walk(nil)
	draw := rand.New(rand.NewPCG(5217, 4975))
	for range 60000 {
		argv := make([]string, 4+draw.IntN(3))
		for i := range argv {
			argv[i] = words[draw.IntN(len(words))]
		}
		compare(argv)
	}
}

// `submit` and `link` post the working tree's pull request template as the body of every pull
// request they create, found as gh-stack finds it (cli/cli's githubtemplate FindLegacy): the
// first match of .github, the root and docs, a symbolic link skipped. So the call hands the guard
// that file to read. Controls: a read hands it nothing, and a tree with no template hands nothing.
func TestGhStackHandsTheGuardThePullRequestTemplateItWouldPost(t *testing.T) {
	repository := inClone(t)
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := func(argv ...string) []string {
		t.Helper()
		target, err := ghTargetOf(argv, nil, savedGhHelp, readGitRemotes, ownersOwnDirectories)
		if err != nil {
			t.Fatalf("ghTargetOf(%q) = %v", argv, err)
		}
		var paths []string
		for _, file := range target.files {
			paths = append(paths, file.Path)
		}
		return paths
	}
	if got := files("stack", "submit", "--auto"); len(got) != 0 {
		t.Errorf("with no template, submit hands the guard %q, want nothing", got)
	}
	write(filepath.Join(repository, "docs", "PULL_REQUEST_TEMPLATE.md"), "docs template")
	if err := os.Symlink(filepath.Join(repository, "docs", "PULL_REQUEST_TEMPLATE.md"), filepath.Join(repository, "pull_request_template.md")); err != nil {
		t.Fatal(err)
	}
	if got, want := files("stack", "submit", "--auto"), filepath.Join(repository, "docs", "PULL_REQUEST_TEMPLATE.md"); !slices.Equal(got, []string{want}) {
		t.Errorf("with a template in docs and a link in the root, submit hands the guard %q, want %q", got, want)
	}
	write(filepath.Join(repository, ".github", "pull-request-template.md"), "github template")
	want := filepath.Join(repository, ".github", "pull-request-template.md")
	for _, argv := range [][]string{{"stack", "submit", "--auto"}, {"stack", "link", "a", "b"}} {
		if got := files(argv...); !slices.Equal(got, []string{want}) {
			t.Errorf("%q hands the guard %q, want .github's template %q", argv, got, want)
		}
	}
	if got := files("stack", "push"); len(got) != 0 {
		t.Errorf("push, which creates no pull request, hands the guard %q, want nothing", got)
	}
}

// ghStackAt lays out a stand-in gh-stack under dir the way the image does, its two directories
// 0755 and the program 0755, running script, and answers the program's path.
func ghStackAt(t *testing.T, dir, script string) string {
	t.Helper()
	program := filepath.Join(dir, "gh-extensions", "gh-stack", "gh-stack")
	for _, made := range []string{filepath.Dir(filepath.Dir(program)), filepath.Dir(program)} {
		if err := os.MkdirAll(made, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(made, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(program, 0o755); err != nil {
		t.Fatal(err)
	}
	return program
}

// `gh stack` runs the image's copy alone: a regular executable file that is its owner's, root's
// in the image, in two directories that are that owner's too, none a symbolic link and none
// writable by any other. Each other layout is refused, naming the path; the control is the
// image's own layout.
func TestGhStackRunsOnlyTheImagesOwnCopy(t *testing.T) {
	saved, savedOwner := ghStackBinary, ghStackOwner
	t.Cleanup(func() { ghStackBinary, ghStackOwner = saved, savedOwner })
	for _, tc := range []struct {
		name   string
		break_ func(program string)
		owner  int
		refuse string
	}{
		{name: "the image's layout", owner: os.Getuid()},
		{name: "another owner", owner: os.Getuid() + 1, refuse: "is not root's"},
		{name: "no program", break_: func(p string) { os.Remove(p) }, owner: os.Getuid(), refuse: "no such file"},
		{name: "a program writable by its group", break_: func(p string) { os.Chmod(p, 0o775) }, owner: os.Getuid(), refuse: "writable by others"},
		{name: "a directory writable by others", break_: func(p string) { os.Chmod(filepath.Dir(p), 0o757) }, owner: os.Getuid(), refuse: "writable by others"},
		{name: "the extensions directory writable by its group", break_: func(p string) { os.Chmod(filepath.Dir(filepath.Dir(p)), 0o775) }, owner: os.Getuid(), refuse: "writable by others"},
		{name: "the extensions directory a link", break_: func(p string) {
			extensions := filepath.Dir(filepath.Dir(p))
			os.Rename(extensions, extensions+".real")
			os.Symlink(extensions+".real", extensions)
		}, owner: os.Getuid(), refuse: "is a symbolic link"},
		{name: "a program that does not run", break_: func(p string) { os.Chmod(p, 0o644) }, owner: os.Getuid(), refuse: "no executable file"},
		{name: "a program that is a link", break_: func(p string) {
			other := p + ".real"
			os.Rename(p, other)
			os.Symlink(other, p)
		}, owner: os.Getuid(), refuse: "is a symbolic link"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ghStackBinary, ghStackOwner = ghStackAt(t, t.TempDir(), "#!/bin/sh\n"), tc.owner
			if tc.break_ != nil {
				tc.break_(ghStackBinary)
			}
			err := ghStackBinaryIsTheImages()
			switch {
			case tc.refuse == "" && err != nil:
				t.Errorf("the image's layout is refused: %v", err)
			case tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse) || !strings.Contains(err.Error(), "sends nothing")):
				t.Errorf("ghStackBinaryIsTheImages() = %v, want a refusal containing %q", err, tc.refuse)
			}
		})
	}
}

// ghStackCommands is compiled from the cmd/*.go of the gh-stack release ghStackVersion names, so
// it is the release the worker image pins (worker.Dockerfile's ARG GH_STACK_TOOL): a bump of the
// pin fails here until the table is read again from the new release's source and the version
// moved with it.
func TestGhStackCommandsAreThePinnedReleases(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "docker", "worker.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	pins := regexp.MustCompile(`(?m)^ARG GH_STACK_TOOL=github:github/gh-stack@(\S+)$`).FindAllSubmatch(dockerfile, -1)
	if len(pins) != 1 {
		t.Fatalf("worker.Dockerfile holds %d ARG GH_STACK_TOOL pins of github/gh-stack, want one", len(pins))
	}
	if pinned := string(pins[0][1]); pinned != ghStackVersion {
		t.Errorf("worker.Dockerfile pins gh-stack %s, and ghStackCommands is gh-stack %s's: read the new release's cmd/*.go into the table and move ghStackVersion", pinned, ghStackVersion)
	}
}

// `legion gh stack …` runs the image's gh-stack itself, not gh: with the arguments after `stack`,
// GH_EXTENSION=1 as gh would set, and GH_CONFIG_DIR at the directory of the owner of the
// repository the working directory's remotes name. A call it refuses runs nothing, and with no
// image copy at all every call is refused.
func TestLegionGhRunsTheImagesGhStackWithTheOwnersDirectory(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	wrappers := ghWrapperDir(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"echo gh ran \"$*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	stack := ghStackAt(t, t.TempDir(), "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$0\" \"$GH_CONFIG_DIR\" \"$GH_EXTENSION\" \"$*\"\n")
	clone := t.TempDir()
	fixture := fixturegit.Isolated(t, t.TempDir())
	fixture.Run(clone, append([]string{"git"}, fixturegit.InitWorkTree("", "")[1:]...)...)
	fixture.Run(clone, "git", "remote", "add", "origin", "https://github.com/sjawhar/legion-smoke.git")
	env := map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PATH": wrappers + ":" + os.Getenv("PATH"), "PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
		testGhStackEnv: stack,
	}
	code, stdout, stderr := legionGhIn(t, clone, env, "", "stack", "view", "--json")
	if want := stack + "|" + filepath.Join(root, "sjawhar") + "|1|view --json\n"; code != 0 || stdout != want {
		t.Errorf("legion gh stack view --json = exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, want)
	}
	code, stdout, stderr = legionGhIn(t, clone, env, "", "stack", "push", "--remote=--upload-pack=touch PWNED")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "an option of git's own") {
		t.Errorf("legion gh stack push --remote=--upload-pack=… = exit %d, stdout %q, stderr %q; want exit 2, nothing run", code, stdout, stderr)
	}
	delete(env, testGhStackEnv)
	code, stdout, stderr = legionGhIn(t, clone, env, "", "stack", "view", "--json")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "runs the image's gh-stack") {
		t.Errorf("with no image copy, legion gh stack view --json = exit %d, stdout %q, stderr %q; want exit 2, nothing run", code, stdout, stderr)
	}
}
