package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
)

// `legion gh stack …` runs github/gh-stack, the one gh extension `legion gh` runs: the worker
// image's copy (ghStackBinary), executed directly, so no extension gh would find in its data
// directory, and no gh alias, ever runs in its place. The rest of this file is how the call is
// read before it runs.
//
// What this reads is the call: its arguments, GH_REPO and the working directory's remotes, so
// that the call runs with the token of the one owner it acts on, and gh-stack's arguments reach
// git as gh-stack means them. It is a routing and correctness tool, not a boundary against the
// pod's own agent: a party that can write the pod's `.git` (its configuration, hooks, gh-stack's
// stack files) can already read the pod's tokens, so this reads none of that state, and the
// Cipher airlock is to replace `legion gh`.
//
// gh-stack v0.3.0 builds its one API client for the repository go-gh's repository.Current
// answers (internal/config's Repo and GitHubClient): GH_REPO where set, else one of the working
// directory's github.com remotes or the repository a gh-resolved marker names. None of its flags
// or arguments names another: a pull request URL argument is read for its number alone
// (cmd/utils.go, parsePRURL), and the rest are branches, pull request numbers and stack numbers
// of that repository. So the call routes by GH_REPO, where set, and by every remote line and
// marker the working directory holds (fromWorkingDirectory, every line counting, since gh-stack's
// git pushes to and fetches from them), and is refused where those route to two directories, as
// any command naming two owners is. A working directory that names no repository keeps
// GH_CONFIG_DIR, where gh-stack itself fails to find one.
//
// gh-stack runs git with what the call names: `git fetch` and `git push` take the remote that
// --remote names as their first argument, ahead of refspecs, and other flags' values and the
// branches it is given reach git too. git reads an argument that starts with `-` there as an
// option of its own, and `--upload-pack=<command>` or `--receive-pack=<command>` runs that
// command, with this call's token directory in its environment. So every flag is read against
// ghStackCommands, compiled from the pinned release's source, and an unknown one is refused; a
// flag's value that starts with `-` is refused; --remote must name a remote `git remote -v`
// lists; and `--`, after which gh-stack would take an argument that starts with `-` as a branch,
// is refused.
//
// The text gh-stack writes to GitHub is the titles and bodies of the pull requests `submit` and
// `link` create: the one commit's subject and body or the branch's name, and the working tree's
// pull request template (cmd/submit.go, defaultPRTitleBody, generatePRBody and humanize;
// cmd/link.go; internal/pr's FindTemplate). The pushes before them carried the commits and the
// branch names past the pre-push leak guard, which reads each pushed commit's message and the ref
// name, judged against the push's destination; so a call that creates pull requests is refused
// unless GH_REPO, where set, and every remote line and marker name one repository, the one the
// pushes went to, and the template it would post goes to the guard as a file the call sends
// (ghStackTemplate). The one other source is `submit`'s editor, which opens where its output is a
// terminal and --auto is absent (cmd/submit.go, `if cfg.IsInteractive() && !opts.auto`), and
// creates each pull request with the title and body typed there, which no guard reads; so
// `submit` is refused unless --auto is set. gh-stack's `alias`, which writes a script into
// ~/.local/bin, and `feedback`, which opens a browser on github/gh-stack's discussions with the
// title it is given, are refused. Its update check, which would read github/gh-stack's releases
// with the call's token, is off for every call (ghChildForced).

// ghStackVersion is the gh-stack release ghStackCommands is compiled from: the version
// packages/daemon/docker/worker.Dockerfile's ARG GH_STACK_TOOL pins, which
// TestGhStackCommandsAreThePinnedReleases holds equal, so a bump of the pin fails until this
// table is read again from the new release's cmd/*.go.
const ghStackVersion = "0.3.0"

// ghStackBinary is the image's gh-stack: podsafety.GhExtensionsDir's gh-stack, laid out as gh lays
// out an extension it installs.
var ghStackBinary = filepath.Join(podsafety.GhExtensionsDir, "gh-stack", "gh-stack")

// ghStackOwner is the uid that must own ghStackBinary and the two directories above it: root, as
// the image installs them, so no role replaces the program its siblings run with their token.
var ghStackOwner = 0

// ghStackCommand is one gh-stack command: each flag by its long name, true where it takes a
// value; its shorthands; and whether it creates pull requests whose text it writes.
type ghStackCommand struct {
	flags  map[string]bool
	short  map[byte]string
	writes bool
}

// ghStackCommands are gh-stack v0.3.0's commands this runs, each with the flags its cmd/<name>.go
// defines (every command also takes -h/--help), and `unstack`'s alias `delete`. `alias` and
// `feedback` are not among them (the file's comment says why), nor cobra's `completion`.
var ghStackCommands = func() map[string]ghStackCommand {
	printPath := ghStackCommand{flags: map[string]bool{"print-path": false}}
	unstack := ghStackCommand{flags: map[string]bool{"local": false}}
	return map[string]ghStackCommand{
		"init":     {flags: map[string]bool{"base": true, "adopt": false}, short: map[byte]string{'b': "base", 'a': "adopt"}},
		"add":      {flags: map[string]bool{"all": false, "update": false, "message": true}, short: map[byte]string{'A': "all", 'u': "update", 'm': "message"}},
		"view":     {flags: map[string]bool{"short": false, "json": false}, short: map[byte]string{'s': "short"}},
		"checkout": printPath,
		"modify":   {flags: map[string]bool{"abort": false, "continue": false}},
		"unstack":  unstack,
		"delete":   unstack,
		"submit":   {flags: map[string]bool{"auto": false, "open": false, "remote": true, "no-atomic": false}, writes: true},
		"sync":     {flags: map[string]bool{"remote": true, "prune": false, "accept-base-change": false, "no-atomic": false}},
		"rebase": {flags: map[string]bool{"downstack": false, "upstack": false, "no-trunk": false, "accept-base-change": false,
			"continue": false, "abort": false, "remote": true, "committer-date-is-author-date": false, "preserve-dates": false}},
		"push":   {flags: map[string]bool{"remote": true, "no-atomic": false}},
		"link":   {flags: map[string]bool{"base": true, "open": false, "remote": true, "no-atomic": false}, writes: true},
		"merge":  {flags: map[string]bool{"merge-method": true, "merge": false, "squash": false, "rebase": false, "yes": false}, short: map[byte]string{'y': "yes"}},
		"switch": {},
		"up":     printPath,
		"down":   printPath,
		"top":    printPath,
		"bottom": printPath,
		"trunk":  printPath,
		"help":   {},
	}
}()

// ghStackAlone are the arguments gh-stack's root takes, each as a call's only argument: its help
// and its version.
var ghStackAlone = []string{"-h", "--help", "-v", "--version"}

// ghStackTarget is the target of `gh stack args…` (the file's comment says how it is read).
func ghStackTarget(args []string, env map[string]string, remotes func() ([]ghRemote, error), dirFor func(owner, name string) (string, error)) (ghTarget, error) {
	call, err := readGhStackCall(args)
	if err != nil {
		return ghTarget{}, err
	}
	lines, err := remotes()
	if err != nil {
		return ghTarget{}, err
	}
	if err := ghStackRemoteIsListed(call, lines); err != nil {
		return ghTarget{}, err
	}
	// The routing counts GH_REPO, where set, and every remote line and marker alike: gh-stack's
	// API client takes GH_REPO, and its git pushes to and fetches from the remotes.
	var sel selectors
	if repo := env["GH_REPO"]; repo != "" {
		if err := sel.repository(repo, "GH_REPO "+repo); err != nil {
			return ghTarget{}, err
		}
	}
	if err := sel.fromWorkingDirectory(func() ([]ghRemote, error) { return lines, nil }, false); err != nil {
		return ghTarget{}, err
	}
	files := []leakguard.GhFile{}
	if call.command.writes {
		if sel.onlyRepository() == "" {
			return ghTarget{}, fmt.Errorf("`gh stack %s` creates pull requests with text the pre-push leak guard judged against the repository the branches were pushed to, and GH_REPO and the working directory's remotes do not name one repository, so it sends nothing; run it in a clone whose remotes name the one repository the stack is in, with no other GH_REPO", call.name)
		}
		template, err := ghStackTemplate()
		if err != nil {
			return ghTarget{}, err
		}
		if template != "" {
			files = append(files, leakguard.GhFile{Option: "gh-stack's pull request template", Path: template})
		}
	}
	dir, err := sel.dir(dirFor, "")
	if err != nil {
		return ghTarget{}, err
	}
	return ghTarget{dir: dir, owner: sel.onlyOwner(), repo: sel.onlyRepository(), command: "stack", files: files}, nil
}

// ghStackRemoteIsListed refuses a --remote value that names no remote `git remote -v` lists: a
// URL or a path there would push outside every repository the call is routed by.
func ghStackRemoteIsListed(call ghStackCall, lines []ghRemote) error {
	var names []string
	for _, line := range lines {
		if !line.marker {
			names = append(names, line.name)
		}
	}
	for _, remote := range call.remotes {
		if !slices.Contains(names, remote) {
			return fmt.Errorf("`gh stack` gives --remote %q, which is no remote `git remote -v` lists here (%s), so it sends nothing; name one of them", remote, strings.Join(slices.Compact(slices.Sorted(slices.Values(names))), ", "))
		}
	}
	return nil
}

// ghStackCall is what readGhStackCall read of a `gh stack` call: its command's name (empty for
// the root's own help or version) and table entry, and every value given to --remote.
type ghStackCall struct {
	name    string
	command ghStackCommand
	remotes []string
}

// readGhStackCall reads args, a `gh stack` call's arguments after `stack`, with pflag's rules
// against ghStackCommands: `--name`, `--name=value` and, for a flag that takes a value,
// `--name value`, whatever that value starts with; a cluster of shorthands, `-Au`, where one
// taking a value takes the rest of the cluster, after one `=`, or the next argument; and `-h` and
// `--help` after the command's name. The command's name comes first: cobra finds it before it
// adds the help and version flags, so it reads a flag before the name as taking the next word,
// and `-h push submit --help=false` runs submit; so the root takes its help or version only as
// the call's one argument (ghStackAlone), and a help given a value, which can turn it off, is
// refused. `--`, an unknown command or flag, a boolean given a value that is no boolean, a value
// that starts with `-`, and `submit` without --auto are refused, each naming the argument.
func readGhStackCall(args []string) (ghStackCall, error) {
	var call ghStackCall
	if len(args) == 1 && slices.Contains(ghStackAlone, args[0]) {
		return call, nil
	}
	var command ghStackCommand
	auto := false
	take := func(name, value, arg string) error {
		if strings.HasPrefix(value, "-") {
			return fmt.Errorf("`gh stack` gives --%s the value %q, which gh-stack hands to git, where a value that starts with `-` is an option of git's own, so it sends nothing", name, value)
		}
		if name == "remote" {
			call.remotes = append(call.remotes, value)
		}
		return nil
	}
	boolean := func(name, value, arg string) error {
		set, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("`gh stack` gives --%s, a flag of no value, the value %q in %q, so it sends nothing", name, value, arg)
		}
		if name == "auto" {
			auto = set
		}
		return nil
	}
	flagOf := func(name string) (takesValue, known bool) {
		if name == "help" {
			return false, true
		}
		takesValue, known = command.flags[name]
		return takesValue, known
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case call.name == "" && strings.HasPrefix(arg, "-") && arg != "-":
			return call, fmt.Errorf("`gh stack` is given %q before its command's name, where gh-stack's parser reads a flag as taking the next word, so it sends nothing; name the command first (`gh stack --help` and `gh stack --version` stand alone)", arg)
		case arg == "--":
			return call, errors.New("`gh stack` is given `--`, after which gh-stack takes an argument that starts with `-` as a branch it hands to git, where it is an option of git's own, so it sends nothing")
		case strings.HasPrefix(arg, "--"):
			name, value, valued := strings.Cut(arg[2:], "=")
			if name == "help" && valued {
				return call, fmt.Errorf("`gh stack` gives --help a value in %q, which can turn a help off and run the command, so it sends nothing", arg)
			}
			takesValue, known := flagOf(name)
			if !known {
				return call, fmt.Errorf("`gh stack%s` has no flag %q in gh-stack %s, so it sends nothing", commandSuffix(call.name), arg, ghStackVersion)
			}
			if !takesValue {
				if !valued {
					value = "true"
				}
				if err := boolean(name, value, arg); err != nil {
					return call, err
				}
				continue
			}
			if !valued {
				if i+1 == len(args) {
					return call, fmt.Errorf("`gh stack` gives %q no value, so it sends nothing", arg)
				}
				i++
				value = args[i]
			}
			if err := take(name, value, arg); err != nil {
				return call, err
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			cluster := arg[1:]
			for j := range len(cluster) {
				name, known := command.short[cluster[j]]
				if cluster[j] == 'h' {
					name, known = "help", true
				}
				if !known {
					return call, fmt.Errorf("`gh stack%s` has no flag -%c in gh-stack %s (in %q), so it sends nothing", commandSuffix(call.name), cluster[j], ghStackVersion, arg)
				}
				rest := cluster[j+1:]
				if name == "help" && strings.HasPrefix(rest, "=") {
					return call, fmt.Errorf("`gh stack` gives -h a value in %q, which can turn a help off and run the command, so it sends nothing", arg)
				}
				if takesValue, _ := flagOf(name); !takesValue {
					if value, ok := strings.CutPrefix(rest, "="); ok {
						if err := boolean(name, value, arg); err != nil {
							return call, err
						}
						break
					}
					if err := boolean(name, "true", arg); err != nil {
						return call, err
					}
					continue
				}
				value := strings.TrimPrefix(rest, "=")
				if rest == "" {
					if i+1 == len(args) {
						return call, fmt.Errorf("`gh stack` gives -%c no value, so it sends nothing", cluster[j])
					}
					i++
					value = args[i]
				}
				if err := take(name, value, arg); err != nil {
					return call, err
				}
				break
			}
		case call.name == "":
			entry, ok := ghStackCommands[arg]
			if !ok {
				return call, fmt.Errorf("`gh stack %s` is no command of gh-stack %s this runs, so it sends nothing", arg, ghStackVersion)
			}
			call.name, call.command, command = arg, entry, entry
		}
	}
	if call.name == "submit" && !auto {
		return call, errors.New("`gh stack submit` without --auto opens gh-stack's editor where its output is a terminal, and creates pull requests with the titles and bodies written there, which the leak guard never read, so it sends nothing; pass --auto, which takes them from the pushed commits the pre-push guard read")
	}
	return call, nil
}

// commandSuffix is " <name>" for a command's name, and nothing for the root.
func commandSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " " + name
}

// ghStackTemplatePattern is the name of a pull request template, as cli/cli v2.97.0's
// pkg/githubtemplate FindLegacy matches it for gh-stack (internal/pr's FindTemplate).
var ghStackTemplatePattern = regexp.MustCompile(`(?i)^pull[_-]request[_-]template(\.|$)`)

// ghStackTemplate is the pull request template gh-stack would post as the body of every pull
// request it creates, as FindLegacy finds it in the working tree's root (`git rev-parse
// --show-toplevel`): the first file of .github, the root and docs, in that order and each
// directory's own, whose name ghStackTemplatePattern matches and that is neither a directory nor
// a symbolic link. Empty where there is none, or no working tree.
func ghStackTemplate() (string, error) {
	root, err := gitLines("rev-parse", "--show-toplevel")
	if err != nil || len(root) != 1 {
		return "", err
	}
	for _, dir := range []string{filepath.Join(root[0], ".github"), root[0], filepath.Join(root[0], "docs")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if ghStackTemplatePattern.MatchString(entry.Name()) && !entry.IsDir() && entry.Type()&fs.ModeSymlink == 0 {
				return filepath.Join(dir, entry.Name()), nil
			}
		}
	}
	return "", nil
}

// ghStackBinaryIsTheImages refuses to run gh-stack unless ghStackBinary is a regular, executable
// file, and it and the two directories above it are ghStackOwner's, no symbolic link and
// writable by no one else: the image's own copy, which no role can replace.
func ghStackBinaryIsTheImages() error {
	for _, path := range []string{filepath.Dir(filepath.Dir(ghStackBinary)), filepath.Dir(ghStackBinary), ghStackBinary} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("`gh stack` runs the image's gh-stack, %s, and %v, so it sends nothing; run it in a Legion pod, whose image installs it", ghStackBinary, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("`gh stack` runs the image's gh-stack, and %s is a symbolic link, so it sends nothing", path)
		case !ok || int(stat.Uid) != ghStackOwner:
			return fmt.Errorf("`gh stack` runs the image's gh-stack, and %s is not root's, so it sends nothing", path)
		case info.Mode().Perm()&0o022 != 0:
			return fmt.Errorf("`gh stack` runs the image's gh-stack, and %s is writable by others than its owner, so it sends nothing", path)
		case path == ghStackBinary && (!info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0):
			return fmt.Errorf("`gh stack` runs the image's gh-stack, and %s is no executable file, so it sends nothing", path)
		}
	}
	return nil
}
