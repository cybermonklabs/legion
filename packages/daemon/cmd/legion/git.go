package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// gitUsage is the help of `legion git`, which reads its own arguments (passThrough), --help among
// them.
const gitUsage = `usage: legion git --real <git> -- <git argument>…
       legion git --after <directory> -- <git argument>…

A git wrapper runs this command, never a person. It runs a git with the arguments after --.
For a git commit, it first adds two trailers: Omp-Session, with the session id that the
dispatch command acts as, and Legion-Role, with LEGION_ROLE. It adds each line only when its
value is valid, and only when git alone would accept the commit's message. It adds none when
the caller asks git for an isolated configuration: GIT_CONFIG_NOSYSTEM set to a value git reads
as true, or GIT_CONFIG_GLOBAL=/dev/null.

  --real <git>         run <git>, the real git (the pod's form: /usr/bin/git)
  --after <directory>  run the next git on PATH after <directory>, the wrapper's own directory
                       (the form of the git first on a tmux pane's PATH)
`

// wrappedGitAt is the loop guard's variable. Each run of `legion git --after` sets it in the
// environment of the git it runs: the number of runs in this chain that came back through a
// host's git wrapper, and then the path of each git the chain ran, a script or not, in order, all
// joined by `:`, which no PATH entry holds. A host's git wrapper that runs the next git on PATH
// can run the tmux wrapper again, and a listed path that is the same file as a git on PATH tells
// that run that it came back: it goes on past every git the chain ran and adds nothing. Files are
// compared (os.SameFile), not PATH indexes or path strings, since a host's wrapper may put an
// entry in front of PATH or reach a file through another spelling. The count ends a chain whose
// gits run one another without end. A git that git itself starts (an alias's `!git`, a hook)
// never reaches either wrapper, because git puts its own exec path first on its children's PATH,
// so the variable in a real git's environment does no harm. Its name starts with neither LEGION_
// nor GIT_: fixtureenv.Isolated drops both prefixes, and git reserves GIT_.
const wrappedGitAt = "WRAPPED_GIT_AT"

// runGit is `legion git`. Both of Legion's git wrappers run it: the pod's /opt/legion/bin/git
// hands it a commit with --real /usr/bin/git, and the git first on a tmux pane's PATH hands it
// every command with --after and its own directory. It reads its arguments and puts back the
// signals' actions (startWrapper), finds the subcommand as the pod's wrapper does
// (gitSubcommand), chooses the git to run (pickAfter for --after), and replaces itself with that
// git (execWrapped). It changes the arguments of a commit only (commitWithTrailers). For every
// other command it prints nothing and starts no other process. An interrupt or a terminate ends
// a read of the commit's that waits (`-F -` from a pipe that stays open, a FIFO with no writer),
// so a cancelled commit is never made.
func runGit(ctx context.Context, args []string, _, stderr io.Writer) int {
	if status, runs := startWrapper(ctx, "git", gitUsage, args, stderr); !runs {
		return status
	}
	gitArgs := args[3:]
	sub := gitSubcommand(gitArgs)
	commit := sub >= 0 && gitArgs[sub] == "commit"
	env := os.Environ()
	if args[0] == "--real" {
		if !commit {
			return execWrapped("legion git", args[1], gitArgs, env, nil, stderr)
		}
		route := stashThroughDescriptor
		if isScript(args[1]) {
			route = stashNone
		}
		return runCommit(args[1], route, gitReader{path: args[1], globals: gitArgs[:sub]}, gitArgs[sub+1:], env, stderr)
	}
	pick, err := pickAfter("git", args[1], os.Getenv("PATH"), os.Getenv(wrappedGitAt))
	if err != nil {
		fmt.Fprintf(stderr, "legion git: %v\n", err)
		return 127
	}
	env = withVariable(env, wrappedGitAt, pick.chain.String())
	if !commit || pick.reentry {
		return execWrapped("legion git", pick.run, gitArgs, env, nil, stderr)
	}
	helper := pick.helper()
	if helper == "" {
		return execWrapped("legion git", pick.run, gitArgs, env, nil, stderr)
	}
	// The helper answers the commit's reads with the chain as it would run it, so a helper that
	// is a host's compiled wrapper comes back as a re-entry and reaches the git after it.
	readChain := pick.chain
	if helper != pick.run {
		readChain.ran = append(slices.Clone(readChain.ran), helper)
	}
	reader := gitReader{path: helper, globals: gitArgs[:sub], env: withVariable(os.Environ(), wrappedGitAt, readChain.String())}
	return runCommit(pick.run, stashOnStandardInput, reader, gitArgs[sub+1:], env, stderr)
}

// runCommit runs git at path with the commit's arguments args as commitWithTrailers makes them,
// read with reader, whose globals are the caller's global options. route is the way a stash
// reaches that git (stashRoute). For --after it is stashOnStandardInput, whether path is a script
// or not. For --real it is stashThroughDescriptor, or stashNone when path is a script.
func runCommit(path string, route stashRoute, reader gitReader, args, env []string, stderr io.Writer) int {
	commit, err := commitWithTrailers(reader, route, args, processEnvironment())
	if err == nil && commit.stdin != nil {
		err = asStandardInput(commit.stdin)
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion git: %v\n", err)
		return 1
	}
	return execWrapped("legion git", path, commit.args, env, commit.keep, stderr)
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
