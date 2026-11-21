// Package ghroute reads which repositories a gh command acts on, as gh itself resolves them, and
// routes them to the one gh directory of a Legion credential bundle that answers for them all
// (DirFor), so the caller runs gh with that directory and no other: `legion gh`, the gh every
// pane and pod runs, and the daemon's own gh tool for an agent that is not running. DirFor is
// also the pick `legion git-credential` answers git with, and ForkDir its pick for a fork whose
// owner the bundle holds no directory for. The package also says whether the command writes
// nothing, by a rule proven against the shared leak check the worker image pins
// (Target.WritesNothing, ReadsProvenCheckSHA256).
package ghroute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// Target is what TargetOf reads off a gh command: Dir, the gh directory every selector of the
// command is routed to (selectors.dir), and "" for a command naming no owner, whose token stays
// the one GH_CONFIG_DIR already names; Owner, the lowercased owner of the repository it acts on,
// "" where the selectors named no owner or two; Repo, that repository's name keyed as gh reads a
// name of it (ghconfig.RepositoryName) where every selector of the command named that one
// repository of that owner, and "" where they named more than one, where one named the owner
// alone, and where none named a repository at all; Command, the path of the leaf command it runs
// as gh's own help names it (`pr list` for `pr ls`), nil when it runs none (`gh`, a group alone,
// `gh help`, `gh --version`); Files, the files the command would send to GitHub as content
// (sentFiles), for the leak guard a caller runs before gh (leakguard.GhRequest.Files), and
// StdinSent, whether it sends its standard input as content as well
// (leakguard.GhRequest.StdinSent); and WritesNothing, whether the call is in a read grammar
// (writesNothing), false for a help call, so that under the shared check that rule was proven
// against (ReadsProvenCheckSHA256) a caller's guard runs that check alone for it
// (leakguard.GhRequest.WritesNothing). Such a call sends no file: no grammar names an option of
// sentFileOptions (TestGhReadGrammarNamesEachOptionAsItsHelpDoes).
type Target struct {
	Dir           string
	Owner         string
	Repo          string
	Command       []string
	Files         []leakguard.GhFile
	StdinSent     bool
	WritesNothing bool
}

// sentFileOptions are the options of gh 2.98.0, by long name, whose value names a file gh
// sends to GitHub as content: true where the whole value is the path, and false for `gh api`'s
// --field, whose file is the `@<path>` half of a `<name>=@<path>` value. They are the same four
// plus the field that leakguard's own table holds (its ghFileArgs), read here by their long
// names because the leaf's own help has already said which option each argument is: a `-F` is
// --body-file in `gh pr create`, --notes-file in `gh release create` and --field in `gh api`,
// and a short cluster (`-dF <path>`) and an attached value (`-F=<path>`) are read as pflag
// reads them.
var sentFileOptions = map[string]bool{
	"body-file":  true,
	"notes-file": true,
	"input":      true,
	"from-file":  true,
	"field":      false,
}

// sentFiles are the files the parsed command would send to GitHub as content, each named once
// and each named by the option it came from, for the leak guard to read the text of
// (leakguard.GhRequest.Files), and whether it sends its standard input as content as well. The
// files are never nil, so a command that sends no file is a command the guard reads no file of,
// rather than one it reads the argv of again with a table of its own.
//
// A value of `-`, and an `@-`, is standard input, which is no file to open: the guard reads it
// through the shared check's own handshake, and reads it itself where no shared check runs
// (leakguard.GhRequest.StdinSent). The options are read in the order their names sort, since a
// parse holds each option's values together and not the argv's order across options, and a
// report that named two files must name them the same way twice.
func sentFiles(parsed ghArgs) (files []leakguard.GhFile, stdin bool) {
	files = []leakguard.GhFile{}
	for _, name := range slices.Sorted(maps.Keys(sentFileOptions)) {
		whole := sentFileOptions[name]
		for _, value := range parsed.values[name] {
			path := value
			if !whole {
				_, after, found := strings.Cut(value, "=")
				if !found || !strings.HasPrefix(after, "@") {
					// A field given a literal value, and `gh project field-list --field <name>`,
					// name no file.
					continue
				}
				path = strings.TrimPrefix(after, "@")
			}
			if path == "-" {
				stdin = true
				continue
			}
			if path == "" {
				continue
			}
			if slices.ContainsFunc(files, func(file leakguard.GhFile) bool { return file.Path == path }) {
				continue
			}
			files = append(files, leakguard.GhFile{Option: "--" + name, Path: path})
		}
	}
	return files, stdin
}

// CallerToken is the first of GH_TOKEN and GITHUB_TOKEN that env sets, in gh's own order, and
// "" where env sets neither. gh sends that token in place of any gh directory's token, so
// TargetOf refuses a call under it, and `legion gh` runs only the guard's own visibility read
// under it (cmd/legion's runGh).
func CallerToken(env map[string]string) string {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if env[name] != "" {
			return name
		}
	}
	return ""
}

// TargetOf reads the Target of the gh command argv (gh's arguments, without `gh`): the gh
// directory it runs with, the repository it acts on, the leaf command it runs, and whether the
// call writes nothing (writesNothing, over argv as typed and the leaf's path). It fails closed:
// whenever it cannot be certain which gh directory the command needs it is an error, which names
// the argument it could not place, and nothing is sent. `gh --version` alone, and a command whose
// last --help is true, name none, since gh only prints.
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
// that is a github.com URL (through gh's pr finder, which reads any other URL, and OWNER/REPO#N,
// as a branch of the command's own repository: only a pull request's URL names its repository); a
// search `<query>`'s repo:/owner:/user:/org: qualifiers; `gh api`'s
// endpoint (repos/<owner>/<repo>/…, repos/<owner>/, orgs/<owner>/, users/<owner>/, a search
// endpoint's q) and a GraphQL query's owner variable (`-F owner=<login>`); and the options naming
// a repository, an owner, a team or an issue by URL (ghOptionKinds). So does the command's own
// repository when gh resolves it, because no argument names it and the leaf reads one, or
// whatever names it for a leaf that reads every remote (fromOwnRepository): GH_REPO for a leaf
// that reads it, gh api's :owner and {repo} placeholders among them, and otherwise the working
// directory's git remotes (remotes), read as gh reads them: origin's fetch URL alone where the
// leaf honours gh's default marker (honoursDefaultMarker) and `remote.origin.gh-resolved base`
// makes origin's repository gh's own, and every github.com repository they name otherwise
// (fromWorkingDirectory); a command naming no owner at all counts those too. Every repository
// they name, and every owner one names alone — `orgs/<owner>/`, an owner:/org:/user: qualifier,
// an `<owner>` argument, a team, a project, an owner option, since such a command reaches
// whatever else of that owner's gh resolves — is routed through dirFor, the bundle's own pick
// (DirFor), and they must all route to one directory; two are an error (selectors.dir). A call
// that writes nothing (writesNothing, or a GraphQL query graphQLQueryRead admits), whose every
// item is an owner the bundle holds no directory for, which names no repository a configured
// rename moves (dirFor answers one with the pair's own refusal, or routes it to the `to` owner's
// directory) and whose working directory's remotes named no github.com repository for it, runs
// with GH_CONFIG_DIR's directory, the one a command naming no owner runs with, rather than being
// refused (R4 of dispatch://INFRA-3399); every other call is routed and refused by the rule
// above. The target's WritesNothing stays writesNothing's alone, which a caller's guard reads the
// call by. The target's repository is the one repository they all name, which is none where a
// selector names an owner alone. A GraphQL query naming its repository otherwise, by a node id or
// in the query text, names none gh could resolve. GH_HOST naming another host than github.com,
// where gh would send no token of the bundle's, and a GH_TOKEN or GITHUB_TOKEN, which gh sends in
// place of any directory's token, are errors, and so is a command of refusedCommands and an
// option of refusedOptions on any leaf.
func TargetOf(argv []string, env map[string]string, help func(path []string) (string, error), remotes func() ([]Remote, error), dirFor func(owner, name string) (string, error)) (Target, error) {
	if host := env["GH_HOST"]; host != "" && !strings.EqualFold(host, "github.com") {
		return Target{}, fmt.Errorf("GH_HOST is %q, another host than github.com, so it sends no token; unset GH_HOST", host)
	}
	if name := CallerToken(env); name != "" {
		return Target{}, fmt.Errorf("%s is set, and gh sends it in place of the token of the owner the command targets, so it sends no token; unset %s", name, name)
	}
	walk := ghWalk{help: help}
	var rest []string
	var cmd *ghCommand
	for i := 0; i < len(argv) && cmd == nil; i++ {
		arg := argv[i]
		switch {
		case len(walk.path) == 0 && arg == "--version":
			if len(argv) == 1 {
				return Target{}, nil
			}
			// gh reads a word after it as a command, an alias's or an extension's among them.
			return Target{}, cannotPlace(arg, "comes before the command's name, and only -R/--repo and --help may (gh's version is `gh --version` alone)", false)
		case len(walk.path) == 0 && arg == "help":
			return Target{}, ghHelpTarget(argv[i+1:], help)
		case arg == "-R" || arg == "--repo":
			if i+1 == len(argv) {
				return Target{}, cannotPlace(arg, "has no value", false)
			}
			rest = append(rest, arg, argv[i+1])
			i++
		case strings.HasPrefix(arg, "--repo=") || (strings.HasPrefix(arg, "-R") && len(arg) > 2) ||
			arg == "--help" || strings.HasPrefix(arg, "--help=") || arg == "-h":
			// The leaf reads these as its own, in order with the rest of its arguments.
			rest = append(rest, arg)
		case strings.HasPrefix(arg, "-"):
			return Target{}, cannotPlace(arg, "comes before the command's name, and only -R/--repo and --help may", false)
		default:
			if err := walk.check(arg); err != nil {
				return Target{}, err
			}
			read, err := walk.read(arg)
			if err != nil {
				return Target{}, err
			}
			if !read.group {
				cmd = &read
				rest = append(rest, argv[i+1:]...)
			}
		}
	}
	if cmd == nil {
		// No leaf: `gh`, or a group alone, which prints its help.
		return Target{}, nil
	}
	parsed, err := cmd.parse(rest)
	if err != nil {
		return Target{}, err
	}
	if parsed.help {
		// No read grammar names --help or -h (readLeaves, apiReadOptions), so a help call is no
		// call that writes nothing (writesNothing), and a caller's guard reads it whole.
		return Target{Command: cmd.path}, nil
	}
	// A refused command only prints under --help, as any command does.
	if err := refusedCommand(cmd.path); err != nil {
		return Target{}, err
	}
	sel := selectors{api: cmd.isAPI()}
	if err := sel.fromCommand(cmd, parsed); err != nil {
		return Target{}, err
	}
	if err := sel.fromOwnRepository(cmd, parsed, env, remotes); err != nil {
		return Target{}, err
	}
	files, stdinSent := sentFiles(parsed)
	reads := writesNothing(argv, cmd.path)
	fallback := ""
	if reads || graphQLQueryRead(argv) {
		fallback = env["GH_CONFIG_DIR"]
	}
	dir, err := sel.dir(dirFor, fallback)
	if err != nil {
		return Target{}, err
	}
	return Target{Dir: dir, Owner: sel.onlyOwner(), Repo: sel.onlyRepository(), Command: cmd.path, Files: files, StdinSent: stdinSent, WritesNothing: reads}, nil
}

// cannotPlace is TargetOf's refusal of an argument it cannot place: what it is and why, and how
// to name the repository instead, which for gh api is its endpoint (it has no -R).
func cannotPlace(arg, why string, api bool) error {
	advice := "pass -R <owner>/<repo>"
	if api {
		advice = "name it in the endpoint, repos/<owner>/<repo>/…"
	}
	return fmt.Errorf("cannot tell which repository the command targets: %q %s, so it sends no token; %s", arg, why, advice)
}

// ghArgs is a leaf's arguments as pflag reads them: its positional arguments in order, every
// value given each option, by long name, the last value given each boolean option but --help, by
// long name, and whether gh only prints the leaf's help.
type ghArgs struct {
	positionals []string
	values      map[string][]string
	booleans    map[string]bool
	help        bool
}

// parse reads args, the arguments of the leaf with its path taken out, as pflag does, refusing an
// option the leaf's help does not list or does not say the arity of. --help is a boolean whose
// last value decides; a -h no option of the leaf takes is pflag's request for help, which ends
// the reading there. After `--` every argument is positional, but for a leaf whose usage passes
// them on (`gh repo clone <repository> [<directory>] [-- <gitflags>...]`), where gh, as cobra
// hands it every positional, takes the first one as the repository even after the dash and passes
// the rest on: so the first argument after `--` is positional when none came before it. What such
// a leaf passes on to git clone, every positional but its first, is judged whole
// (cloneGitFlags).
func (c *ghCommand) parse(args []string) (ghArgs, error) {
	api, leaf := c.isAPI(), c.quoted()
	parsed := ghArgs{values: map[string][]string{}, booleans: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			rest := args[i+1:]
			if c.passesGitFlags {
				if err := cloneGitFlags(leaf, passedOn(append(slices.Clone(parsed.positionals), rest...))); err != nil {
					return ghArgs{}, err
				}
			}
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
	if c.passesGitFlags {
		if err := cloneGitFlags(leaf, passedOn(parsed.positionals)); err != nil {
			return ghArgs{}, err
		}
	}
	return parsed, nil
}

// passedOn is what gh hands git clone of a leaf's positionals: every one but the first, the
// repository (cli/cli v2.104.0 pkg/cmd/repo/clone/clone.go:88, repo/fork/fork.go:114).
func passedOn(positionals []string) []string {
	if len(positionals) == 0 {
		return nil
	}
	return positionals[1:]
}

// cloneGitFlagTakes are the git clone flags a leaf passing `<gitflags>` on may hand it, each by
// its exact spelling, with whether it takes a value: what the clone fetches (its depth, its
// branches, its tags, a filter), how it is laid out (bare, a mirror, sparse, no checkout, the
// remote's name) and how much it prints. None sets the clone's configuration, its template, its
// submodules, or where its objects come from.
var cloneGitFlagTakes = map[string]bool{
	"--quiet": false, "-q": false, "--verbose": false, "-v": false, "--progress": false, "--no-progress": false,
	"--single-branch": false, "--no-single-branch": false, "--no-tags": false, "--sparse": false,
	"--no-checkout": false, "-n": false, "--bare": false, "--mirror": false,
	"--depth": true, "--shallow-since": true, "--shallow-exclude": true,
	"--branch": true, "-b": true, "--origin": true, "-o": true, "--filter": true,
}

// cloneGitFlags refuses what the leaf hands git clone (passed, passedOn) unless it is, after a
// directory gh takes as the clone's target when it comes first (cli/cli v2.104.0 git/client.go,
// parseCloneArgs), only flags of cloneGitFlagTakes, each spelled exactly, a value-taking one
// with its value as `--name=value` or as the next argument, which git takes whatever it says.
// It is an allowlist because git clone's other flags hand the token on, and git takes any
// unambiguous abbreviation of each: measured with git 2.43 under the environment `legion gh`
// gives gh's git, `--config http.https://github.com/.sslVerify=false`, and the same spelled
// `--conf`, sent a man-in-the-middle proxy the credential helper's token; `--recurse-submodules`
// and `--recursive` clone every submodule with its own configuration; `--template` installs hooks
// and configuration; `--reference` and `--separate-git-dir` take objects or the git directory from
// a path the call names. A joined short flag (`-qn`, `-bmain`) and a stray argument are refused
// too, so nothing reaches git that this has not read the way git reads it.
func cloneGitFlags(leaf string, passed []string) error {
	i := 0
	if len(passed) > 0 && !strings.HasPrefix(passed[0], "-") {
		i = 1
	}
	for ; i < len(passed); i++ {
		arg := passed[i]
		name, _, joined := strings.Cut(arg, "=")
		takes, known := cloneGitFlagTakes[name]
		switch {
		case !known, joined && (!takes || !strings.HasPrefix(name, "--")):
			return fmt.Errorf("%s hands %q to git clone, which takes only %s from a call: its other flags set the clone's configuration (--config), its hooks (--template), its submodules (--recurse-submodules) or where its objects come from (--reference), and git takes an abbreviation of any of them, so it sends nothing; clone without it, then run git in the clone", leaf, arg, cloneGitFlagList())
		case takes && !joined:
			if i+1 == len(passed) {
				return fmt.Errorf("%s hands %q to git clone with no value, so it sends nothing", leaf, arg)
			}
			i++
		}
	}
	return nil
}

// cloneGitFlagList is cloneGitFlagTakes' names, sorted, for a refusal to name.
func cloneGitFlagList() string {
	return strings.Join(slices.Sorted(maps.Keys(cloneGitFlagTakes)), ", ")
}

// set records value given the option name: --help's as a boolean; another boolean's as pflag
// reads it (strconv.ParseBool), the last given deciding, and false for a value that is no boolean,
// which gh refuses itself; any other's taking a value in the order given. An option of
// refusedOptions is refused first, naming the argument it came in.
func (p *ghArgs) set(name string, flag ghFlag, value, arg string, api bool) error {
	if why, refused := refusedOptions[name]; refused {
		return fmt.Errorf("%q %s, so it sends nothing", arg, why)
	}
	switch {
	case name == "help":
		help, err := strconv.ParseBool(value)
		if err != nil {
			return cannotPlace(arg, "gives --help a value that is no boolean", api)
		}
		p.help = help
	case flag.value:
		p.values[name] = append(p.values[name], value)
	default:
		p.booleans[name], _ = strconv.ParseBool(value)
	}
	return nil
}

// refusedOptions are the options, by long name, TargetOf refuses on any leaf, each with why:
//
//   - --editor (-e on `gh issue comment`, `gh issue create`, `gh pr comment` and `gh pr create`, at
//     gh 2.98.0 and 2.104.0 alike) runs an editor program after the leak guard has read the call
//     and posts whatever it wrote: gh picks the program from GH_EDITOR, gh's config `editor`, VISUAL
//     or EDITOR, or else its own default (cmdutil.DetermineEditor), and asks GitHub for the issue
//     before it runs it (pkg/cmd/pr/shared/commentable.go, CommentableRun), so no environment gh is
//     given keeps the text from GitHub.
//   - --attach, gh 2.104.0's, on eight leaves (`gh issue` and `gh pr` comment, create and edit,
//     and `gh issue artifact create` and `edit`), uploads the file it names as an asset GitHub
//     then serves: an image up to 10 MiB, an SVG's text among them, or a video up to 100 MiB
//     (cli/cli v2.104.0 internal/attachments). It has no short spelling. The leak guard reads no
//     such file.
//   - --recurse-submodules, `gh pr checkout`'s, runs `git submodule update --init --recursive`
//     after the checkout (cli/cli v2.104.0 pkg/cmd/pr/checkout/checkout.go:172-180), and that
//     update fetches each submodule with the submodule's own configuration
//     (.git/modules/<name>/config), which no check of `legion gh` reads: measured with git 2.43,
//     a URL-scoped http.sslVerify=false there and the caller's proxy handed a man-in-the-middle
//     proxy the token. No configuration turns an explicit update off, as `legion gh`'s forced git
//     configuration (cmd/legion's ghForcedGitConfig) turns off the recursion a fetch or a
//     checkout would make by itself.
var refusedOptions = map[string]string{
	"editor":             "runs an editor program after the leak guard has read the call and posts the text that program wrote, which the guard never read; write the text in --body or --body-file, which it reads",
	"attach":             "uploads the file it names to GitHub, which the leak guard does not read; write the text in --body or --body-file, which the guard reads",
	"recurse-submodules": "runs `git submodule update --init --recursive`, which fetches each submodule with the submodule's own git configuration, which `legion gh` does not read; check out without it, then run `git submodule update` yourself",
}

// helpTimeout bounds one ReadHelp.
var helpTimeout = 30 * time.Second

// ReadHelp is the real gh's help of the command path, `gh <path> --help` (`gh --help` for
// gh's own), run with env (`legion gh`'s allowlisted environment of the caller's GH_CONFIG_DIR,
// cmd/legion's ghChildEnviron) and refused when gh answers it with an error. `legion gh` reads it
// through its keep of the helps gh printed (cmd/legion's ghHelpKeep), which keeps what gh printed
// for the calls after, and never a read that ctx ended. gh runs in a process group of its own,
// which is stopped whole when the read outlives helpTimeout or ctx ends (procgroup.Configure): a
// stop that killed gh alone would leave whatever gh started running, reparented and unwaited for.
// That group is not the terminal's, so none of the terminal's signals reach it, and ctx is how
// they do: `legion gh`'s, which main cancels on a SIGINT or a SIGTERM and hangupCancels on a
// SIGHUP. It runs under leakguard.RunByRealGhVar, so a gh that leads back to `legion gh` is
// refused there.
func ReadHelp(ctx context.Context, gh string, env, path []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, helpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, append(slices.Clone(path), "--help")...)
	killed := procgroup.Configure(cmd)
	cmd.Env = append(slices.Clone(env), leakguard.RunByRealGhVar+"="+gh)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		switch {
		case killed() && errors.Is(ctx.Err(), context.DeadlineExceeded):
			return "", fmt.Errorf("gh did not answer its help within %s, and its process group was stopped", helpTimeout)
		case killed():
			return "", fmt.Errorf("gh's help read was stopped, with its process group: %v", context.Cause(ctx))
		}
		return "", fmt.Errorf("gh answered its help with %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return "", errors.New("gh printed no help")
	}
	return stdout.String(), nil
}

// refusedCommands are the gh commands TargetOf refuses for everybody, by the path their help
// names, each with why; a group's path refuses every command under it. None is a command a Legion
// role runs.
//
//   - `gh agent-task` sends the token, as a bearer, to the Copilot API host GitHub's own answer
//     names (cli/cli v2.104.0 pkg/cmd/agent-task/capi/client.go:74-79).
//   - `gh auth login`, `logout`, `refresh` and `switch` write or swap the token the gh directory
//     holds, which the daemon alone writes (ghconfig.WriteBundle): login takes one on standard input
//     or from a browser, for any host; refresh fetches a new one; switch makes another account's
//     the active one; logout removes it.
//   - `gh auth setup-git` writes a credential helper into git's global configuration, which the
//     git `legion gh` runs never reads (GIT_CONFIG_GLOBAL is /dev/null, cmd/legion's
//     ghChildForced), so git answers it "could not lock config file /dev/null"; a pane's own git
//     authenticates through `legion git-credential`, which the runtime names on its command line.
//   - `gh config set` writes the gh directory the call runs with, and gh 2.104.0 sends every API
//     request for github.com, with its token, to the host its `api_host` key names; its
//     `http_unix_socket` sends them through a socket; and gh writes a key it does not know all
//     the same, a token's `oauth_token` among them.
//   - `gh gist edit` opens an editor program by default and posts the text it wrote, which the
//     leak guard never read (cli/cli v2.104.0 pkg/cmd/gist/edit/edit.go:302-326): the editor
//     opens unless a file is named, and gh reads it whatever prompts allow.
var refusedCommands = map[string]string{
	"agent-task":     "sends the token to the Copilot API host GitHub names, not to GitHub's own API",
	"auth login":     "writes a token into the gh directory, which the daemon alone writes",
	"auth logout":    "removes the token from the gh directory, which the daemon alone writes",
	"auth refresh":   "writes a new token into the gh directory, which the daemon alone writes",
	"auth switch":    "makes another account's token the one the gh directory sends, which the daemon alone decides",
	"auth setup-git": "writes git's global configuration, which the git `legion gh` runs never reads; a pane's git authenticates through `legion git-credential`, which the runtime sets",
	"config set":     "writes the gh directory's configuration, where api_host or http_unix_socket would send the token's requests somewhere other than github.com and any other key is written all the same; the daemon writes that directory, and no Legion role runs this command",
	"gist edit":      "opens an editor program and posts the text it wrote, which the leak guard never read",
}

// refusedCommand is the refusal of path, a command's path as its help names it, when it is one
// of refusedCommands or under one, naming the refused command and why, and nil for any other.
func refusedCommand(path []string) error {
	for n := len(path); n > 0; n-- {
		refused := strings.Join(path[:n], " ")
		if why, ok := refusedCommands[refused]; ok {
			return fmt.Errorf("`gh %s` %s, so it sends nothing", refused, why)
		}
	}
	return nil
}
