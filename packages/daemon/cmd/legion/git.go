package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"syscall"
)

// gitUsage is the help of `legion git`. No line of it starts with `legion git` and a word: the docs
// site's CLI generator (docs/site/generators/legion-cli.sh) reads such a line as a subcommand.
const gitUsage = `usage: legion git --real <git> -- <git argument>…
       legion git --after <directory> -- <git argument>…

A git wrapper runs this command, never a person. It runs a git with the arguments after --.
For a git commit, it first adds two trailers: Omp-Session, with the session id that the
dispatch command acts as, and Legion-Role, with LEGION_ROLE. It adds each line only when its
value is valid, and only when git alone would accept the commit's message.

  --real <git>         run <git>, the real git (the pod's form: /usr/bin/git)
  --after <directory>  run the next git on PATH after <directory>, the wrapper's own directory
                       (the form of the git first on a tmux pane's PATH)
`

// wrappedGitAt is the loop guard's variable. It holds the path, `<PATH entry>/git`, of each script
// that `legion git --after` ran in this chain of runs, joined by `:`, which no PATH entry holds. A
// host's git wrapper that runs the git after its own PATH entry can run the tmux wrapper again,
// and a path of the variable that is still a git on PATH tells that run that it came back: it
// goes on past every script the chain ran and adds nothing. Paths are compared, not PATH indexes,
// since a host's wrapper may put an entry in front of PATH before it runs the next git. Its name
// starts with neither LEGION_ nor GIT_: fixtureenv.Isolated drops both prefixes, and git reserves
// GIT_.
const wrappedGitAt = "WRAPPED_GIT_AT"

// runGit is `legion git`. Both of Legion's git wrappers run it: the pod's /opt/legion/bin/git
// hands it a commit with --real /usr/bin/git, and the git first on a tmux pane's PATH hands it
// every command with --after and its own directory. It finds the subcommand as the pod's wrapper
// does (gitSubcommand), chooses the git to run (gitAfter for --after), and replaces itself with
// that git (execGit). It changes the arguments of a commit only (commitWithTrailers). For every
// other command it prints nothing and starts no other process.
func runGit(_ context.Context, args []string, _, stderr io.Writer) int {
	if len(args) > 0 && helpRequested(args[0]) {
		fmt.Fprint(stderr, gitUsage)
		return 0
	}
	if len(args) < 3 || (args[0] != "--real" && args[0] != "--after") || args[1] == "" || args[2] != "--" {
		fmt.Fprint(stderr, "legion git: give --real <git> or --after <directory>, then --, then the git arguments\n"+gitUsage)
		return 2
	}
	gitArgs := args[3:]
	choice := gitChoice{run: args[1], helper: args[1]}
	if args[0] == "--real" {
		choice.script = isGitScript(choice.run)
	} else {
		var ok bool
		if choice, ok = gitAfter(args[1], os.Getenv("PATH"), os.Getenv(wrappedGitAt)); !ok {
			fmt.Fprintf(stderr, "legion git: no git on PATH after %s\n", args[1])
			return 127
		}
	}
	env := choice.environment(os.Environ())
	sub := gitSubcommand(gitArgs)
	if sub < 0 || gitArgs[sub] != "commit" || choice.reentry || choice.helper == "" {
		return execGit(choice.run, gitArgs, env, nil, stderr)
	}
	commit, err := commitWithTrailers(choice.helper, gitArgs[:sub], gitArgs[sub+1:], processEnvironment())
	if err != nil {
		fmt.Fprintf(stderr, "legion git: %v\n", err)
		return 1
	}
	return execGit(choice.run, commit.args, env, commit.keep, stderr)
}

// gitSubcommand is the index of git's subcommand in args, or -1 when there is none: the first word
// that is no global option. The options whose value is the next word are the list of the pod's
// wrapper, packages/daemon/docker/git. Every other word that starts with `-` is an option of its
// own, the `--opt=value` spellings included. A word that ends git's own options before any
// subcommand (handle_options, git.c) ends the search with none: --help, -h, --version and -v,
// which git runs as its help and version commands with what follows as their arguments, and
// --exec-path without a value, --html-path, --man-path and --info-path, which print a path and
// end git. So `git --help commit` is no commit.
func gitSubcommand(args []string) int {
	value := false
	for i, arg := range args {
		switch {
		case value:
			value = false
		case slices.Contains([]string{"-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env", "--attr-source", "--shallow-file"}, arg):
			value = true
		case slices.Contains([]string{"--help", "-h", "--version", "-v", "--html-path", "--man-path", "--info-path"}, arg),
			strings.HasPrefix(arg, "--exec-path") && !strings.HasPrefix(arg, "--exec-path="):
			return -1
		case !strings.HasPrefix(arg, "-"):
			return i
		}
	}
	return -1
}

// gitChoice is the git that `legion git` runs, and the git that answers its reads.
type gitChoice struct {
	// run is the git it replaces itself with.
	run string
	// script is whether run is a script: its first two bytes are `#!`.
	script bool
	// helper is the git that answers the reads of a commit (commitWithTrailers). It is no script
	// under --after, so a host's wrapper never answers them. It is "" when there is none, and then
	// no trailer is added.
	helper string
	// after is whether the form is --after, and chain, under --after, the loop guard's paths that
	// run gets: the chain's scripts, run among them when it is a script.
	after bool
	chain []string
	// reentry is whether this run came back through a host's wrapper that an earlier run chose
	// (wrappedGitAt). Such a run adds nothing: the first run added the trailers.
	reentry bool
}

// environment is the environment run gets: environ as received, with the loop guard's variable
// set to the chain's paths when run is a script under --after, and taken out when run is no
// script. Under --real a script gets the variable as received, since --real reads no PATH.
func (c gitChoice) environment(environ []string) []string {
	if c.script && !c.after {
		return environ
	}
	env := slices.DeleteFunc(slices.Clone(environ), func(entry string) bool {
		return strings.HasPrefix(entry, wrappedGitAt+"=")
	})
	if c.script {
		env = append(env, wrappedGitAt+"="+strings.Join(c.chain, ":"))
	}
	return env
}

// gitAfter chooses the git for `legion git --after own`, from path (PATH) and marker (the loop
// guard's variable) and nothing else. A candidate is each non-empty entry of path whose `git` is an
// executable regular file, leaving out each entry that is own (os.SameFile). It runs the first
// candidate after the first entry that is own, or the first candidate when own is on no entry. A
// script is a candidate too, so a host's own git wrapper still runs. The helper is the first
// candidate after own that is no script.
//
// A marker that names a candidate is a re-entry: it runs the first candidate after own that the
// marker does not name. A marker that names no candidate is stale and changes nothing, and a
// script it runs starts a new chain. ok is false when there is no git to run.
func gitAfter(own, path, marker string) (choice gitChoice, ok bool) {
	ownInfo, ownErr := os.Stat(own)
	first := -1
	type candidate struct {
		index int
		path  string
	}
	var candidates []candidate
	for i, entry := range strings.Split(path, ":") {
		if entry == "" {
			continue
		}
		if ownErr == nil {
			if info, err := os.Stat(entry); err == nil && os.SameFile(info, ownInfo) {
				if first < 0 {
					first = i
				}
				continue
			}
		}
		git := entry + "/git"
		if info, err := os.Stat(git); err == nil && executableFile(info) {
			candidates = append(candidates, candidate{i, git})
		}
	}
	ran := slices.DeleteFunc(strings.Split(marker, ":"), func(p string) bool { return p == "" })
	choice.after = true
	choice.reentry = slices.ContainsFunc(candidates, func(c candidate) bool { return slices.Contains(ran, c.path) })
	for _, c := range candidates {
		if c.index > first && !(choice.reentry && slices.Contains(ran, c.path)) {
			choice.run = c.path
			break
		}
	}
	if choice.run == "" {
		return choice, false
	}
	if choice.script = isGitScript(choice.run); choice.script {
		if choice.reentry {
			choice.chain = ran
		}
		choice.chain = append(choice.chain, choice.run)
	}
	for _, c := range candidates {
		if c.index > first && !isGitScript(c.path) {
			choice.helper = c.path
			break
		}
	}
	return choice, true
}

// isGitScript is whether path is a script: its first two bytes are `#!`. A file this process
// cannot read counts as a script, since nothing shows that it is a real git.
func isGitScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	head := make([]byte, 2)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return n == 2 && string(head) == "#!"
}

// execGit replaces this process with git at path, argv[0] being path. keep is the file whose
// descriptor args name in place of a message git could no longer read (commitStash): it stays
// open across the exec, since its close-on-exec flag is clear. execGit returns only when the exec
// fails: 127 when path does not exist, as a shell answers, and 126 otherwise.
func execGit(path string, args, env []string, keep *os.File, stderr io.Writer) int {
	err := syscall.Exec(path, append([]string{path}, args...), env)
	runtime.KeepAlive(keep)
	fmt.Fprintf(stderr, "legion git: run %s: %v\n", path, err)
	if errors.Is(err, syscall.ENOENT) {
		return 127
	}
	return 126
}
