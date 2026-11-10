package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

const ghUsage = "usage: legion gh [gh args…]  (the gh first on a Legion pane's or pod's PATH: runs LEGION_GH_PATH with GH_CONFIG_DIR set to the gh directory of the repository the command names, or of its owner where the bundle holds the repository none, under LEGION_GH_OWNERS_DIR)"

// runGh is `legion gh`, the gh a pane or pod runs. It reads the repository the command acts on,
// and its owner, as gh resolves them (ghTargetOf) and execs the real gh (LEGION_GH_PATH) as `gh`
// with its arguments untouched and GH_CONFIG_DIR set to a gh directory under
// LEGION_GH_OWNERS_DIR: the repository's own, `<owner>/<repo>`, whose token reaches that one
// repository, when every selector of the command named that one repository and the bundle holds
// its directory, and the owner's otherwise (repositoryGHConfigDir) — a command naming no
// repository, one naming two repositories of the one owner, one where a selector named the owner
// alone beside a repository, and one whose repository the bundle holds no directory for, which
// is every repository of an owner the role writes to one of. A command naming no owner at all,
// in a working directory whose git remotes name none, keeps GH_CONFIG_DIR, the issue's owner's.
// A command it cannot read for certain, one naming two owners, and one naming an owner the
// bundle holds no directory for are refused, exit 2, naming why, and the real gh never runs: no
// token but the target repository's or its owner's is ever sent.
//
// Before gh is handed anything, the leak guard reads the call (leakguard.GhCall): a call it
// refuses, and one whose checks could not be made to run, are refused here, exit 2, with the
// guard's own message, and gh never runs. Two calls pass unchecked: `legion gh auth
// git-credential`, which answers above before any of this, and the shared check's own read of a
// repository's visibility (ghVisibilityRead).
//
// The git gh runs itself (a clone, a fetch of a fork's parent, `pr checkout`) authenticates
// through the credential helper gh names after its own executable, GH_PATH: runGh sets it to the
// `gh` wrapper first on PATH, so that helper is `legion gh auth git-credential`, which answers as
// `legion git-credential` does, from the directory of the repository in git's path (useHttpPath set
// for github.com through GIT_CONFIG_COUNT), never with GH_CONFIG_DIR's token; when PATH's first gh
// is no such wrapper it refuses. The child also runs with gh's update check and prompts off, which a TTY
// would otherwise reach. Each of the three variables is runtime-set and required; an unset one is
// refused, exit 2, naming it. With no LEGION_GH_PATH, --help prints this command's usage, so
// `legion gh --help` outside a pane answers as every legion command does.
func runGh(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	env := processEnvironment()
	if env["LEGION_GH_PATH"] == "" && len(args) == 1 && helpRequested(args[0]) {
		fmt.Fprintln(stderr, ghUsage)
		return 0
	}
	for _, name := range []string{"LEGION_GH_PATH", "GH_CONFIG_DIR", "LEGION_GH_OWNERS_DIR"} {
		if env[name] == "" {
			fmt.Fprintf(stderr, "legion gh: %s is unset: a Legion pane or pod sets it, and nothing else runs this gh\n%s\n", name, ghUsage)
			return 2
		}
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "git-credential" {
		return runGitCredential(ctx, args[2:], stdout, stderr)
	}
	gh := env["LEGION_GH_PATH"]
	wrapper, err := ghWrapperPath()
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	target, err := ghTargetOf(args, env, func(path []string) (string, error) { return readGhHelp(gh, path) }, readGitRemotes)
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	// The guard, between the owner this command resolved and the gh it is about to run. The whole
	// argv is what it reads, rather than the options a parser calls text: the shared check's own
	// gh-guard reads full argv, and a private name is refusable wherever it stands. The call's
	// standard input goes with it, and whatever the guard read of it comes back to hand gh in
	// this process's place (runGhChild).
	//
	// Two calls reach gh unchecked, each for a reason of its own. `legion gh auth git-credential`
	// is one: it returned above, before any of this, because it is git's credential protocol on
	// standard input and standard output and reading that input would break it. The shared
	// check's own visibility read is the other (ghVisibilityRead).
	//
	// A *leakguard.Refusal and a *leakguard.SetupFailure each refuse the call: a check that did
	// not run has cleared nothing. Both exit 2, this command's status for a refusal made before
	// gh runs, which is what an unreadable command and an unset variable already exit with, and
	// both print the guard's own message, which names the record and the class and never the
	// text it refused.
	var buffered []byte
	if !ghVisibilityRead(args, env) {
		repo := ""
		if target.owner != "" && target.repo != "" {
			repo = target.owner + "/" + target.repo
		}
		buffered, err = leakguard.New(leakguard.Options{}).GhCall(ctx, leakguard.GhRequest{Args: args, Stdin: os.Stdin, Repo: repo})
		if err != nil {
			fmt.Fprintf(stderr, "legion gh: %v\n", err)
			return 2
		}
	}
	dir := env["GH_CONFIG_DIR"]
	if target.owner != "" {
		if dir, err = repositoryGHConfigDir(env["LEGION_GH_OWNERS_DIR"], target.owner, target.repo); err != nil {
			fmt.Fprintf(stderr, "legion gh: %v\n", err)
			return 2
		}
	}
	set := map[string]string{"GH_CONFIG_DIR": dir, "GH_PATH": wrapper, "GH_NO_UPDATE_NOTIFIER": "1", "GH_PROMPT_DISABLED": "1"}
	childEnv := make([]string, 0, len(env)+len(set)+3)
	for _, pair := range os.Environ() {
		if name, _, _ := strings.Cut(pair, "="); set[name] == "" {
			childEnv = append(childEnv, pair)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		childEnv = append(childEnv, name+"="+set[name])
	}
	if childEnv, err = withGitHubUseHTTPPath(childEnv); err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	return runGhChild(ctx, gh, args, childEnv, buffered, stderr)
}

// ghVisibilityRead is whether argv is the shared public leak check's own read of a repository's
// visibility, the one gh call the guard above never sees. A pod's gh is `legion gh`, so the
// check's read comes back through this command: checking it would run the check, whose own
// visibility read would come back here again, and the first write to a repository of another
// owner would recurse without end.
//
// The match is the check's own, as its gh guard makes it: exactly the argv
//
//	gh api --hostname github.com repos/<slug> --jq .visibility
//
// for exactly the slug the check's variable names, which must itself be a repository GitHub
// could have (ghrepo.Parse), so that a variable someone else set lets through one read of one
// repository and nothing else. Both halves come from leakguard, whose own visibility reader
// (Guard.ghVisibility) is built from the same two, so the exemption and the read it exempts
// cannot part.
func ghVisibilityRead(argv []string, env map[string]string) bool {
	slug := env[leakguard.VisibilityReadVar]
	if slug == "" {
		return false
	}
	if _, err := ghrepo.Parse(leakguard.VisibilityReadVar, slug); err != nil {
		return false
	}
	return slices.Equal(argv, leakguard.VisibilityReadArgs(slug))
}

// ghForwardedSignals are the signals runGhChild passes on to the gh it started: what a terminal
// sends a foreground job (an interrupt, a quit, a hangup, a window that changed size) and what a
// supervisor stops one with. gh runs in a process group of its own, so none of them reaches it
// otherwise. SIGKILL and SIGSTOP are not here because no process can catch them, and every other
// signal keeps this process's own default handling, which is what an exec'd gh had.
var ghForwardedSignals = []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGWINCH}

// runGhChild runs the real gh as a child of this process and is the status to exit with: gh's
// own exit status, and 128+<signal> for a gh a signal killed, which is the status a shell reads
// such an end as.
//
// gh is a child rather than this process exec'd into, as it was before the guard: the guard may
// have read this process's standard input to check it, and exec cannot put those bytes back,
// while a child can be handed them (buffered, nil where the guard read none and gh gets this
// process's own standard input whole). Standard output and standard error are this process's
// own descriptors, so gh's reading of its terminal — its colour, its spinner, its prompts — is
// what an exec'd gh would have read.
//
// The child runs in a process group of its own (procgroup.Configure, called before any other
// SysProcAttr field, as its doc requires), and this process forwards it the signals above, to
// the whole group, since that is what a terminal would have signalled had gh been exec'd here.
// Its context is this process's with the cancellation taken off (context.WithoutCancel): the
// Cancel procgroup gives the command is one exec refuses on a command built with no context at
// all, and the cancellation itself is not wanted — a signal that ends this process's context
// reaches gh as the signal it was, through the forwarding below, and gh ends on it and answers
// with its own status, where exec's cancellation would kill gh's group outright, mid-clone, and
// answer with a status nobody sent.
func runGhChild(ctx context.Context, gh string, args, env []string, buffered []byte, stderr io.Writer) int {
	child := exec.CommandContext(context.WithoutCancel(ctx), gh, args...)
	child.Env = env
	child.Stdin = os.Stdin
	if buffered != nil {
		child.Stdin = bytes.NewReader(buffered)
	}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	procgroup.Configure(child)
	// Notified before the child starts, so a signal sent while gh is starting is held in the
	// channel rather than lost, and stopped once it has ended.
	signals := make(chan os.Signal, len(ghForwardedSignals))
	signal.Notify(signals, ghForwardedSignals...)
	defer signal.Stop(signals)
	if err := child.Start(); err != nil {
		fmt.Fprintf(stderr, "legion gh: run %s: %v\n", gh, err)
		return 1
	}
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		for {
			select {
			case received := <-signals:
				if sig, ok := received.(syscall.Signal); ok {
					_ = syscall.Kill(-child.Process.Pid, sig)
				}
			case <-ended:
				return
			}
		}
	}()
	err := child.Wait()
	if err == nil {
		return 0
	}
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		fmt.Fprintf(stderr, "legion gh: run %s: %v\n", gh, err)
		return 1
	}
	if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exited.ExitCode()
}

// ghWrapperScript is the `gh` wrapper both runtimes install first on PATH: a script whose one
// command execs `legion gh` with its arguments (packages/daemon/docker/gh, workerbin.Install),
// the legion path absolute, so the shell's exec reads it as no option, and either a bare word of
// the characters a shell reads literally (shellprefix's safe set) or quoted, in single quotes
// (shellprefix.Literal) or in double quotes with no expansion.
var ghWrapperScript = regexp.MustCompile(`^#!/bin/sh\nexec ('/(?:[^']|'\\'')*'|"/[^"$` + "`" + `\\]*"|/[A-Za-z0-9_./:-]*) gh "\$@"\n?$`)

// ghWrapperMaxBytes bounds the wrapper read: a script longer is refused whole rather than matched
// by its start. A wrapper whose quoted legion executable path takes it past the bound (a path near
// PATH_MAX, or one whose single quotes, four bytes each once quoted, grow it that far) is refused
// too, which refuses every command `legion gh` would hand the real gh rather than letting one
// through; `legion gh auth git-credential`, answered before the check, answers by git's path alone.
const ghWrapperMaxBytes = 4096

// ghWrapperPath is the `gh` first on PATH, the wrapper that runs `legion gh`, as an absolute
// path: gh names its git credential helper after it (GH_PATH). It is refused when PATH's first gh
// is no such wrapper (isGhWrapper), or there is none: the real gh itself would answer every
// github.com request with GH_CONFIG_DIR's token, and a machine's own wrapper with whatever it
// chooses.
func ghWrapperPath() (string, error) {
	wrapper, err := exec.LookPath("gh")
	if err == nil {
		wrapper, err = filepath.Abs(wrapper)
	}
	if err != nil {
		return "", fmt.Errorf("no gh on PATH runs `legion gh` (%v), so the git gh runs would not authenticate through it; run it as a Legion pane's gh", err)
	}
	file, err := os.Open(wrapper)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", wrapper, err)
	}
	defer file.Close()
	ok, err := isGhWrapper(file)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", wrapper, err)
	}
	if !ok {
		return "", fmt.Errorf("the first gh on PATH, %s, is not the wrapper that runs `legion gh`, so the git gh runs would not authenticate through it; run it as a Legion pane's gh", wrapper)
	}
	return wrapper, nil
}

// isGhWrapper says whether r holds, whole, a script ghWrapperScript accepts: one longer than
// ghWrapperMaxBytes is none.
func isGhWrapper(r io.Reader) (bool, error) {
	script, err := io.ReadAll(io.LimitReader(r, ghWrapperMaxBytes+1))
	if err != nil {
		return false, err
	}
	return len(script) <= ghWrapperMaxBytes && ghWrapperScript.Match(script), nil
}

// withGitHubUseHTTPPath adds credential.https://github.com.useHttpPath=true to env's GIT_CONFIG_COUNT
// entries, which git reads as it reads `-c`, so the git gh runs tells its credential helper the
// repository's path, whose owner `legion git-credential` answers for. A GIT_CONFIG_COUNT that is no
// count is refused.
func withGitHubUseHTTPPath(env []string) ([]string, error) {
	count := 0
	for _, pair := range env {
		if value, ok := strings.CutPrefix(pair, "GIT_CONFIG_COUNT="); ok {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("GIT_CONFIG_COUNT is %q, which is no count, so git's configuration cannot be extended; unset it", value)
			}
			count = n
		}
	}
	key, value := fmt.Sprintf("GIT_CONFIG_KEY_%d", count), fmt.Sprintf("GIT_CONFIG_VALUE_%d", count)
	out := make([]string, 0, len(env)+3)
	for _, pair := range env {
		if name, _, _ := strings.Cut(pair, "="); name != "GIT_CONFIG_COUNT" && name != key && name != value {
			out = append(out, pair)
		}
	}
	return append(out, key+"=credential.https://github.com.useHttpPath", value+"=true", fmt.Sprintf("GIT_CONFIG_COUNT=%d", count+1)), nil
}

// ghTarget is what a gh command names: owner, the lowercased owner of the repository it acts on,
// and repo, that repository's lowercased name where every selector of the command named that one
// repository of that owner and "" where they named more than one, where one named the owner
// alone, and where none named a repository at all. A target with no owner is a command naming
// none, whose token stays the one GH_CONFIG_DIR already names.
type ghTarget struct{ owner, repo string }

// ghTargetOf is the ghTarget of the gh command argv (gh's arguments, without `gh`), the zero
// target when the command names no owner. It fails closed: whenever it cannot be certain which
// owner the command targets it is an error, which names the argument it could not place, and
// nothing is sent. `gh --version` alone, and a command whose last --help is true, name none,
// since gh only prints.
//
// It reads the command as gh does, from gh's own help of each command on the way (help): the
// path of subcommands down to the leaf the arguments run, every option's arity, and the arguments
// the leaf's USAGE line names. A first word gh's own help does not list, a command that runs
// another program (`gh copilot`, `gh extension exec`), and a word its parent's help lists as a gh
// alias, whose expansion may name a repository of its own, or as an extension, which reads gh's
// token itself, are errors before its help is read, since reading it could run them (ghWalk); so
// is such a word as the target of `gh help`, which for an extension gh runs (ghHelpTarget), and a
// word whose help is another command's that lists no such alias of its own. Before the leaf only
// the inherited -R/--repo and --help/-h may come, read with the leaf's own options; anything else
// there, a subcommand gh does not know, help that lists an option incompletely or not at all, an
// option the leaf's help does not list or names a selector of in a way this does not know, and an
// argument its USAGE has no place for or names in a way this does not know (ghSlotKinds) are
// errors. The leaf's options are read with pflag's rules, and after `--` the rest are its
// arguments, unless its USAGE passes them on (`gh repo clone <repository> [<directory>] [--
// <gitflags>...]`).
//
// Every selector the leaf's help names counts: a `<repository>` (or another repository) argument,
// before `--` or, for a leaf passing the rest on, the first after it; an `<owner>`; a URL argument
// that is a github.com URL (through gh's pr finder, which reads any other as a branch of the
// command's own repository, only a pull request's names its repository); OWNER/REPO#N where the
// leaf reads it (ghLeafQuirks); a search `<query>`'s repo:/owner:/user:/org: qualifiers; `gh api`'s
// endpoint (repos/<owner>/<repo>/…, repos/<owner>/, orgs/<owner>/, users/<owner>/, a search
// endpoint's q) and a GraphQL query's owner variable (`-F owner=<login>`); and the options naming
// a repository, an owner, a team or an issue by URL (ghOptionKinds). So does the command's own
// repository when gh resolves it, because no argument names it and the leaf reads one, or
// whatever names it for a leaf that reads every remote (fromOwnRepository): GH_REPO for a leaf
// that reads it, gh api's :owner and {repo} placeholders among them, and otherwise every
// github.com repository the working directory's git remotes name (remotes), read as gh reads
// them; a command naming no owner at all counts those too. They must all name one owner; two are
// an error. The target's repository is the one repository they all name, which is none where a
// selector names an owner alone — `orgs/<owner>/`, an owner:/org:/user: qualifier, an `<owner>`
// argument, a team, a project, an owner option — since such a command reaches whatever else of
// that owner's gh resolves. A GraphQL query naming its repository otherwise, by a node id or in
// the query text, names none gh could resolve. GH_HOST naming another host than github.com, where
// gh would send no token of the bundle's, and a GH_TOKEN or GITHUB_TOKEN, which gh sends in place
// of any directory's token, are errors.
func ghTargetOf(argv []string, env map[string]string, help func(path []string) (string, error), remotes func() ([]ghRemote, error)) (ghTarget, error) {
	if host := env["GH_HOST"]; host != "" && !strings.EqualFold(host, "github.com") {
		return ghTarget{}, fmt.Errorf("GH_HOST is %q, another host than github.com, so it sends no token; unset GH_HOST", host)
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if env[name] != "" {
			return ghTarget{}, fmt.Errorf("%s is set, and gh sends it in place of the token of the owner the command targets, so it sends no token; unset %s", name, name)
		}
	}
	walk := ghWalk{help: help}
	var rest []string
	var cmd *ghCommand
	for i := 0; i < len(argv) && cmd == nil; i++ {
		arg := argv[i]
		switch {
		case len(walk.path) == 0 && arg == "--version":
			if len(argv) == 1 {
				return ghTarget{}, nil
			}
			// gh reads a word after it as a command, an alias's or an extension's among them.
			return ghTarget{}, cannotPlace(arg, "comes before the command's name, and only -R/--repo and --help may (gh's version is `gh --version` alone)", false)
		case len(walk.path) == 0 && arg == "help":
			return ghTarget{}, ghHelpTarget(argv[i+1:], help)
		case arg == "-R" || arg == "--repo":
			if i+1 == len(argv) {
				return ghTarget{}, cannotPlace(arg, "has no value", false)
			}
			rest = append(rest, arg, argv[i+1])
			i++
		case strings.HasPrefix(arg, "--repo=") || (strings.HasPrefix(arg, "-R") && len(arg) > 2) ||
			arg == "--help" || strings.HasPrefix(arg, "--help=") || arg == "-h":
			// The leaf reads these as its own, in order with the rest of its arguments.
			rest = append(rest, arg)
		case strings.HasPrefix(arg, "-"):
			return ghTarget{}, cannotPlace(arg, "comes before the command's name, and only -R/--repo and --help may", false)
		default:
			if err := walk.check(arg); err != nil {
				return ghTarget{}, err
			}
			read, err := walk.read(arg)
			if err != nil {
				return ghTarget{}, err
			}
			if !read.group {
				cmd = &read
				rest = append(rest, argv[i+1:]...)
			}
		}
	}
	if cmd == nil {
		// No leaf: `gh`, or a group alone, which prints its help.
		return ghTarget{}, nil
	}
	parsed, err := cmd.parse(rest)
	if err != nil || parsed.help {
		return ghTarget{}, err
	}
	sel := selectors{api: cmd.isAPI(), owners: map[string]string{}, repos: map[string]bool{}}
	if err := sel.fromCommand(cmd, parsed); err != nil {
		return ghTarget{}, err
	}
	if err := sel.fromOwnRepository(cmd, parsed, env, remotes); err != nil {
		return ghTarget{}, err
	}
	switch len(sel.owners) {
	case 0:
		return ghTarget{}, nil
	case 1:
		for owner := range sel.owners {
			return ghTarget{owner: owner, repo: sel.onlyRepository()}, nil
		}
	}
	var listed []string
	for owner, what := range sel.owners {
		listed = append(listed, fmt.Sprintf("%s (%s)", owner, what))
	}
	slices.Sort(listed)
	return ghTarget{}, fmt.Errorf("the command names more than one owner, %s, so it sends no token; name one repository", strings.Join(listed, " and "))
}

// cannotPlace is ghTargetOf's refusal of an argument it cannot place: what it is and why, and how
// to name the repository instead, which for gh api is its endpoint (it has no -R).
func cannotPlace(arg, why string, api bool) error {
	advice := "pass -R <owner>/<repo>"
	if api {
		advice = "name it in the endpoint, repos/<owner>/<repo>/…"
	}
	return fmt.Errorf("cannot tell which repository the command targets: %q %s, so it sends no token; %s", arg, why, advice)
}

// ghArgs is a leaf's arguments as pflag reads them: its positional arguments in order, every
// value given each option, by long name, and whether gh only prints the leaf's help.
type ghArgs struct {
	positionals []string
	values      map[string][]string
	help        bool
}

// parse reads args, the arguments of the leaf with its path taken out, as pflag does, refusing an
// option the leaf's help does not list or does not say the arity of. --help is a boolean whose
// last value decides; a -h no option of the leaf takes is pflag's request for help, which ends
// the reading there. After `--` every argument is positional, but for a leaf whose usage passes
// them on (`gh repo clone <repository> [<directory>] [-- <gitflags>...]`), where gh, as cobra
// hands it every positional, takes the first one as the repository even after the dash and passes
// the rest on: so the first argument after `--` is positional when none came before it.
func (c *ghCommand) parse(args []string) (ghArgs, error) {
	api, leaf := c.isAPI(), c.quoted()
	parsed := ghArgs{values: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			rest := args[i+1:]
			if c.passthrough && len(parsed.positionals) > 0 {
				rest = nil
			} else if c.passthrough && len(rest) > 1 {
				rest = rest[:1]
			}
			parsed.positionals = append(parsed.positionals, rest...)
			return parsed, nil
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			flag, ok := c.flags[name]
			if !ok {
				return ghArgs{}, cannotPlace(arg, "is no option "+leaf+"'s help lists", api)
			}
			if flag.uncertain {
				return ghArgs{}, cannotPlace(arg, "is an option whose help does not say whether it takes a value", api)
			}
			if !hasValue {
				switch {
				case !flag.value:
					value, hasValue = "true", true
				case flag.optional:
					// Its default, which names nothing.
				case i+1 == len(args):
					return ghArgs{}, cannotPlace(arg, "has no value", api)
				default:
					i++
					value, hasValue = args[i], true
				}
			}
			if hasValue {
				if err := parsed.set(name, flag, value, arg, api); err != nil {
					return ghArgs{}, err
				}
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			// pflag's shorthands, each in turn: `-f=value` gives any its value; a boolean or an
			// optional value takes its default and leaves the rest of the word to the next
			// shorthand; any other takes the rest of the word, or the next argument.
			for shorts := arg[1:]; shorts != ""; {
				short, rest := shorts[0], shorts[1:]
				name, ok := c.shorts[short]
				if !ok {
					if short == 'h' {
						parsed.help = true
						return parsed, nil
					}
					return ghArgs{}, cannotPlace(arg, fmt.Sprintf("names -%c, which %s's help does not list", short, leaf), api)
				}
				flag := c.flags[name]
				if flag.uncertain {
					return ghArgs{}, cannotPlace(arg, "is an option whose help does not say whether it takes a value", api)
				}
				value, hasValue := "", true
				switch {
				case len(rest) > 1 && rest[0] == '=':
					value, rest = rest[1:], ""
				case !flag.value:
					value = "true"
				case flag.optional:
					hasValue = false
				case rest != "":
					value, rest = rest, ""
				case i+1 < len(args):
					i++
					value = args[i]
				default:
					return ghArgs{}, cannotPlace(arg, "has no value", api)
				}
				if hasValue {
					if err := parsed.set(name, flag, value, arg, api); err != nil {
						return ghArgs{}, err
					}
				}
				shorts = rest
			}
		default:
			parsed.positionals = append(parsed.positionals, arg)
		}
	}
	return parsed, nil
}

// set records value given the option name: --help's as a boolean, any other's taking a value in
// the order given.
func (p *ghArgs) set(name string, flag ghFlag, value, arg string, api bool) error {
	switch {
	case name == "help":
		help, err := strconv.ParseBool(value)
		if err != nil {
			return cannotPlace(arg, "gives --help a value that is no boolean", api)
		}
		p.help = help
	case flag.value:
		p.values[name] = append(p.values[name], value)
	}
	return nil
}

// readGhHelp is the real gh's help of the command path, `gh <path> --help` (`gh --help` for
// gh's own), refused when gh answers it with an error.
func readGhHelp(gh string, path []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, append(slices.Clone(path), "--help")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh answered its help with %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return "", errors.New("gh printed no help")
	}
	return stdout.String(), nil
}
