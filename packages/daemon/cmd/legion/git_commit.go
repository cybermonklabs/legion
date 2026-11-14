package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
)

// This file is the commit half of `legion git`: the two trailer lines, git commit's options as git
// reads them, the message as git builds and cleans it, git's rules for a message it refuses, git's
// trailer step, and the argument list that adds the lines. The git it mirrors is 2.47.3, the
// worker image's: the functions each part names are in builtin/commit.c, sequencer.c,
// parse-options.c, parse-options-cb.c, trailer.c, strbuf.c, commit.c and pretty.c of that release.

// gitTrailer is one trailer line that `legion git` adds to a commit: `<key>: <value>`.
type gitTrailer struct{ key, value string }

// String is the line as git writes it, `<key>: <value>`, and the value of its --trailer option.
func (l gitTrailer) String() string { return l.key + ": " + l.value }

// sessionIDRule is the rule a session id must meet to reach a trailer. It is the jj overlay
// writer's rule, SESSION_ID_PATH_SEGMENT (packages/envoy-client/src/dispatch-session-directory.ts),
// so an id that holds a line break gives no line.
var sessionIDRule = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// gitTrailers is the lines a commit gets, in order, each only where its value is valid.
//
// Omp-Session holds the session id that the dispatch command acts as (runDispatchCli,
// packages/envoy-client/src/dispatch-cli.ts). An empty variable counts as unset. The host is
// DISPATCH_HOST, or claude when DISPATCH_HOST is unset and CLAUDE_CODE_CHILD_SESSION=1. Host omp
// takes DISPATCH_SESSION_ID. Host claude takes ENVOY_SESSION_ID, else CLAUDE_CODE_SESSION_ID. Any
// other host, or no host, gives no line.
//
// Legion-Role holds LEGION_ROLE when it is one of the roles a claim can be on, or the
// controller's role.
func gitTrailers(env map[string]string) []gitTrailer {
	var lines []gitTrailer
	host := env["DISPATCH_HOST"]
	if host == "" && env["CLAUDE_CODE_CHILD_SESSION"] == "1" {
		host = "claude"
	}
	id := ""
	switch host {
	case "omp":
		id = env["DISPATCH_SESSION_ID"]
	case "claude":
		if id = env["ENVOY_SESSION_ID"]; id == "" {
			id = env["CLAUDE_CODE_SESSION_ID"]
		}
	}
	if sessionIDRule.MatchString(id) {
		lines = append(lines, gitTrailer{"Omp-Session", id})
	}
	if role := legionclaim.Role(env["LEGION_ROLE"]); legionclaim.IsRole(role) || role == legionclaim.RoleController {
		lines = append(lines, gitTrailer{"Legion-Role", string(role)})
	}
	return lines
}

// gitCommit is the argument list that `legion git` hands git for a commit, keep, the file whose
// descriptor that list names in place of a message or template git could no longer read, and
// stdin, the file that is to be git's standard input in place of the one the wrapper read
// (commitStash). keep must stay open until the exec, and stdin goes onto descriptor 0 before it
// (asStandardInput).
type gitCommit struct {
	args  []string
	keep  *os.File
	stdin *os.File
}

// commitWithTrailers is the argument list for `git <globals> commit <args>`, read with git, the
// helper, whose globals are the caller's global options. script is whether the git that runs the
// commit is a script. It adds a `--trailer '<key>: <value>'` for each line of gitTrailers that the
// message lacks, and only when it has shown that git alone would accept the message and that git's
// trailer step would change nothing but add those lines (commitBuild.lines). It puts them after
// every option of the caller: right before the `--` or `--end-of-options` that ends the options,
// or at the end when no word ends them. git applies --trailer options in order, and its default
// (trailer.ifExists addIfDifferentNeighbor) compares each with the line before it, so a line put
// before the caller's own would change which of the caller's lines git keeps. git commit reads an
// option after a path (parse_options with no PARSE_OPT_STOP_AT_NON_OPTION), so the end is a
// place for an option. It sets no trailer.* configuration, so each trailer of the caller's keeps
// git's behaviour, a repeated one too.
//
// It adds none and leaves every argument as written when no line is valid, when -h or --help is
// among the options, when an option matches no option of git commit, when git would make no
// commit (a dry run), when the helper's version is below 2.32 (the first git with
// `commit --trailer`), and when it cannot show those two things. A message or template that it has
// read and that git can no longer read goes to git as a file of its own, trailers or not
// (commitStash). The error is a stash it could not write, and then it hands git nothing.
func commitWithTrailers(git gitReader, script bool, args []string, env map[string]string) (gitCommit, error) {
	written := gitCommit{args: slices.Concat(git.globals, []string{"commit"}, args)}
	want := gitTrailers(env)
	if len(want) == 0 {
		return written, nil
	}
	opts, ok := readCommitOptions(args)
	if !ok || opts.help || opts.dryRun || opts.statusFormat || opts.null {
		return written, nil
	}
	plan, ok := opts.plan()
	if !ok {
		return written, nil
	}
	if !git.atLeast(2, 32) {
		return written, nil
	}
	b := commitBuild{git: git, script: script, opts: opts, plan: plan}
	lines, err := b.lines(want)
	if err != nil {
		return gitCommit{}, err
	}
	commit := gitCommit{args: slices.Clone(args)}
	switch {
	case b.stash == nil:
	case b.stash.stdin:
		commit.stdin = b.stash.file
	default:
		word := commit.args[b.stash.arg]
		commit.args[b.stash.arg] = word[:b.stash.at] + "/dev/fd/" + strconv.Itoa(int(b.stash.file.Fd()))
		commit.keep = b.stash.file
	}
	added := make([]string, 0, 2*len(lines))
	for _, line := range lines {
		added = append(added, "--trailer", line.String())
	}
	commit.args = slices.Concat(git.globals, []string{"commit"}, commit.args[:opts.end], added, commit.args[opts.end:])
	return commit, nil
}

// commitArity is how an option of git commit takes its value.
type commitArity int

const (
	// commitSwitch takes no value (PARSE_OPT_NOARG).
	commitSwitch commitArity = iota
	// commitValue takes a value: attached (`-mx`, `--message=x`) or the next word.
	commitValue
	// commitOptionalValue takes a value only when it is attached (PARSE_OPT_OPTARG).
	commitOptionalValue
)

// commitOption is one entry of git commit's option table.
type commitOption struct {
	short byte
	long  string
	arity commitArity
	// noNeg is PARSE_OPT_NONEG: the option has no --no- form.
	noNeg bool
}

// commitOptions is builtin_commit_options of git 2.47.3 (cmd_commit), in its order, without the
// group headings.
var commitOptions = []commitOption{
	{'q', "quiet", commitSwitch, false},
	{'v', "verbose", commitSwitch, false},
	{'F', "file", commitValue, false},
	{0, "author", commitValue, false},
	{0, "date", commitValue, false},
	{'m', "message", commitValue, false},
	{'c', "reedit-message", commitValue, false},
	{'C', "reuse-message", commitValue, false},
	{0, "fixup", commitValue, false},
	{0, "squash", commitValue, false},
	{0, "reset-author", commitSwitch, false},
	{0, "trailer", commitValue, true},
	{'s', "signoff", commitSwitch, false},
	{'t', "template", commitValue, false},
	{'e', "edit", commitSwitch, false},
	{0, "cleanup", commitValue, false},
	{0, "status", commitSwitch, false},
	{'S', "gpg-sign", commitOptionalValue, false},
	{'a', "all", commitSwitch, false},
	{'i', "include", commitSwitch, false},
	{0, "interactive", commitSwitch, false},
	{'p', "patch", commitSwitch, false},
	{'o', "only", commitSwitch, false},
	{'n', "no-verify", commitSwitch, false},
	{0, "dry-run", commitSwitch, false},
	{0, "short", commitSwitch, false},
	{0, "branch", commitSwitch, false},
	{0, "ahead-behind", commitSwitch, false},
	{0, "porcelain", commitSwitch, false},
	{0, "long", commitSwitch, false},
	{'z', "null", commitSwitch, false},
	{0, "amend", commitSwitch, false},
	{0, "no-post-rewrite", commitSwitch, false},
	{'u', "untracked-files", commitOptionalValue, false},
	{0, "pathspec-from-file", commitValue, false},
	{0, "pathspec-file-nul", commitSwitch, false},
	{0, "allow-empty", commitSwitch, false},
	{0, "allow-empty-message", commitSwitch, false},
}

// commitFile is a file option of git commit (-F, -t, --pathspec-from-file) as git keeps it after
// fix_filename (parse-options.c). set is false where git's variable is NULL: no option, its --no-
// form, or an empty value. The value is args[arg][at:] in the commit's arguments, for the rewrite.
type commitFile struct {
	set   bool
	value string
	arg   int
	at    int
}

// commitString is a string option of git commit as git keeps it. set is false where git's
// variable is NULL. An empty value is set.
type commitString struct {
	set   bool
	value string
}

// commitRead is what git commit's options set that decides its message, or whether it makes a
// commit at all (readCommitOptions).
type commitRead struct {
	// help is -h or --help: git prints its usage.
	help bool
	// verbose is -v's count, -1 until -v, --verbose or --no-verbose.
	verbose int
	// file is -F: "-" is standard input.
	file commitFile
	// haveMessage and message are have_option_m and the -m values as opt_parse_m joins them.
	haveMessage bool
	message     string
	// reedit is -c, reuse is -C.
	reedit, reuse commitString
	fixup, squash commitString
	// trailers is the value of each of the caller's --trailer options, in order.
	trailers []string
	signoff  bool
	// template is -t. templateWritten is whether the command line set or cleared it, and then
	// git's commit.template is not read.
	template        commitFile
	templateWritten bool
	// edit is -1 until -e, --edit or --no-edit, then 1 or 0.
	edit int
	// cleanup is --cleanup. cleanupWritten is whether the command line set or cleared it, and
	// then git's commit.cleanup is not read.
	cleanup        commitString
	cleanupWritten bool
	amend          bool
	// interactive and patch are --interactive and -p: each reads standard input.
	interactive, patch bool
	pathspecFromFile   commitFile
	allowEmptyMessage  bool
	// dryRun, statusFormat and null each make a commit a dry run: --dry-run, one of --short,
	// --porcelain and --long, and -z.
	dryRun, statusFormat, null bool
	// includeStatus is --status or --no-status, and statusWritten whether one was given. When
	// none was, git's commit.status decides.
	includeStatus, statusWritten bool
	// end is the index of the word that ends the options, `--` or `--end-of-options`, or the
	// number of words when no word ends them.
	end int
}

// readCommitOptions reads a commit's arguments as git 2.47.3's parse_options reads them against
// builtin_commit_options (parse_options_step, parse_short_opt, parse_long_opt and do_get_value in
// parse-options.c). A long option may be any unique prefix of a long name, and takes its --no-
// form unless it is PARSE_OPT_NONEG. Short options may be bundled (`-am`), and a value may be
// attached (`-mthree`, `-F-`, `--message=four`). A word that is no option is a path, and the
// options go on after it, up to `--` or `--end-of-options`. ok is false where git refuses the
// options: an option that matches nothing or matches two, a value that is missing or not allowed,
// and a bundle that check_typos reads as a long option written with one dash.
func readCommitOptions(args []string) (r commitRead, ok bool) {
	r = commitRead{verbose: -1, edit: -1, end: len(args)}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--" || arg == "--end-of-options":
			r.end = i
			return r, true
		case len(arg) < 2 || arg[0] != '-':
			// A path.
		case len(args) == 1 && (arg == "--git-completion-helper" || arg == "--git-completion-helper-all"):
			return r, false
		case arg[1] != '-':
			if i, ok = r.readShort(args, i); !ok || r.help {
				return r, ok
			}
		case arg == "--help" || arg == "--help-all":
			r.help = true
			return r, true
		default:
			if i, ok = r.readLong(args, i); !ok {
				return r, false
			}
		}
	}
	return r, true
}

// readShort reads the bundle of short options args[i] (parse_short_opt). It answers the index of
// the last word it read: a value can be the next word.
func (r *commitRead) readShort(args []string, i int) (int, bool) {
	arg := args[i]
	rest := arg[1:]
	for first := true; rest != ""; first = false {
		o := commitShortOption(rest[0])
		if o == nil {
			// An unknown h asks for the usage, as -h does.
			r.help = rest[0] == 'h'
			return i, r.help
		}
		rest = rest[1:]
		switch {
		case o.arity == commitSwitch:
			r.set(o, false, "", 0, 0)
		case rest != "":
			r.set(o, false, rest, i, len(arg)-len(rest))
			rest = ""
		case o.arity == commitOptionalValue:
			r.set(o, false, "", 0, 0)
		case i+1 < len(args):
			i++
			r.set(o, false, args[i], i, 0)
		default:
			return i, false
		}
		if first && rest != "" && commitTypo(arg[1:]) {
			return i, false
		}
	}
	return i, true
}

// commitShortOption is the option whose short name is c, or nil.
func commitShortOption(c byte) *commitOption {
	for k := range commitOptions {
		if commitOptions[k].short == c {
			return &commitOptions[k]
		}
	}
	return nil
}

// commitTypo is check_typos: a bundle of three characters or more is refused when it starts with
// `no-` or starts some long name, as `-amend` does.
func commitTypo(bundle string) bool {
	if len(bundle) < 3 {
		return false
	}
	if strings.HasPrefix(bundle, "no-") {
		return true
	}
	for _, o := range commitOptions {
		if o.long != "" && strings.HasPrefix(o.long, bundle) {
			return true
		}
	}
	return false
}

// readLong reads the long option args[i] (parse_long_opt). It answers the index of the last word
// it read: a value can be the next word.
func (r *commitRead) readLong(args []string, i int) (int, bool) {
	body := args[i][2:]
	start := body
	unset, noNo := false, false
	if s, ok := strings.CutPrefix(start, "no-"); ok {
		if s2, ok := strings.CutPrefix(s, "no-"); ok {
			start, noNo = s2, true
		} else {
			start, unset = s, true
		}
	}
	nameLen := len(start)
	if eq := strings.IndexByte(start, '='); eq >= 0 {
		nameLen = eq
	}
	type match struct {
		option *commitOption
		unset  bool
	}
	var abbrev, ambiguous *match
	register := func(o *commitOption, unset bool) {
		// git commit has no alias options, so a second match is always ambiguous.
		if abbrev != nil {
			ambiguous = abbrev
		}
		abbrev = &match{o, unset}
	}
	for k := range commitOptions {
		o := &commitOptions[k]
		if o.long == "" {
			continue
		}
		long, optUnset := o.long, false
		if s, ok := strings.CutPrefix(long, "no-"); ok {
			long, optUnset = s, true
		} else if noNo {
			continue
		}
		if unset != optUnset && o.noNeg {
			continue
		}
		if rest, ok := strings.CutPrefix(start, long); ok {
			switch {
			case rest == "":
				return r.apply(args, i, o, unset != optUnset, "", false)
			case rest[0] == '=':
				return r.apply(args, i, o, unset != optUnset, rest[1:], true)
			}
			continue
		}
		if nameLen <= len(long) && long[:nameLen] == start[:nameLen] {
			register(o, unset != optUnset)
		}
		// `--n`, `--no` and `--no-` abbreviate the --no- form of every option that has one.
		if !o.noNeg && strings.HasPrefix("no-", body) {
			register(o, !optUnset)
		}
	}
	if ambiguous != nil || abbrev == nil {
		return i, false
	}
	value, hasValue := "", false
	if eq := strings.IndexByte(body, '='); eq >= 0 {
		value, hasValue = body[eq+1:], true
	}
	return r.apply(args, i, abbrev.option, abbrev.unset, value, hasValue)
}

// apply takes the value of the long option o, written at args[i], as do_get_value does.
func (r *commitRead) apply(args []string, i int, o *commitOption, unset bool, value string, hasValue bool) (int, bool) {
	switch {
	case hasValue && (unset || o.arity == commitSwitch):
		return i, false
	case unset:
		r.set(o, true, "", 0, 0)
	case hasValue || o.arity == commitOptionalValue:
		r.set(o, false, value, i, len(args[i])-len(value))
	case o.arity == commitSwitch:
		r.set(o, false, "", 0, 0)
	case i+1 < len(args):
		r.set(o, false, args[i+1], i+1, 0)
		return i + 1, true
	default:
		return i, false
	}
	return i, true
}

// set records what option o does to the variables git keeps, with value written at args[arg][at:].
// value is "" for an unset option.
func (r *commitRead) set(o *commitOption, unset bool, value string, arg, at int) {
	file := commitFile{}
	if !unset && value != "" {
		file = commitFile{set: true, value: value, arg: arg, at: at}
	}
	text := commitString{set: !unset, value: value}
	switch o.long {
	case "verbose":
		if unset {
			r.verbose = 0
		} else {
			r.verbose = max(r.verbose, 0) + 1
		}
	case "file":
		r.file = file
	case "message":
		// opt_parse_m: a blank line between values, and each value ends its line. --no-message
		// drops them all.
		if unset {
			r.haveMessage, r.message = false, ""
			break
		}
		r.haveMessage = true
		if r.message != "" {
			r.message += "\n"
		}
		r.message += value
		if r.message != "" && !strings.HasSuffix(r.message, "\n") {
			r.message += "\n"
		}
	case "reedit-message":
		r.reedit = text
	case "reuse-message":
		r.reuse = text
	case "fixup":
		r.fixup = text
	case "squash":
		r.squash = text
	case "trailer":
		r.trailers = append(r.trailers, value)
	case "signoff":
		r.signoff = !unset
	case "template":
		r.template, r.templateWritten = file, true
	case "edit":
		r.edit = 0
		if !unset {
			r.edit = 1
		}
	case "cleanup":
		r.cleanup, r.cleanupWritten = text, true
	case "interactive":
		r.interactive = !unset
	case "patch":
		r.patch = !unset
	case "dry-run":
		r.dryRun = !unset
	case "short", "porcelain", "long":
		r.statusFormat = !unset
	case "status":
		r.includeStatus, r.statusWritten = !unset, true
	case "null":
		r.null = !unset
	case "amend":
		r.amend = !unset
	case "pathspec-from-file":
		r.pathspecFromFile = file
	case "allow-empty-message":
		r.allowEmptyMessage = !unset
	}
}

// commitPlan is what parse_and_validate_options makes of the options for the message.
type commitPlan struct {
	useEditor bool
	// templateOn is whether template_file survives: no -m, -F, -C, -c or --fixup drops it.
	templateOn bool
	// use is use_message: -C, else -c, else HEAD for --amend with no other source.
	use commitString
	// fixupPrefix is "fixup" or "amend" when --fixup is set, and fixupCommit its commit.
	fixupPrefix, fixupCommit string
}

// plan is parse_and_validate_options for the message. ok is false where git dies: -m beside -F,
// -C or -c, two of -C, -c, -F and --fixup, --fixup beside --squash, --fixup with an unknown
// `<word>:`, and --fixup=amend: beside -m (prepare_to_commit).
func (r commitRead) plan() (p commitPlan, ok bool) {
	p.useEditor = !(r.file.set || r.haveMessage || r.reuse.set)
	count := func(flags ...bool) int {
		n := 0
		for _, f := range flags {
			if f {
				n++
			}
		}
		return n
	}
	if r.fixup.set && r.squash.set ||
		count(r.reuse.set, r.reedit.set, r.file.set, r.fixup.set) > 1 ||
		count(r.haveMessage, r.reedit.set, r.reuse.set, r.file.set) > 1 {
		return p, false
	}
	p.templateOn = !(r.reuse.set || r.reedit.set || r.file.set || r.fixup.set || r.haveMessage)
	p.use = r.reuse
	if r.reedit.set {
		p.use = r.reedit
	}
	if r.amend && !p.use.set && !r.fixup.set {
		p.use = commitString{true, "HEAD"}
	}
	if r.fixup.set {
		// A run of letters and then a colon is `<word>:<commit>`, and the word must be amend or
		// reword. Anything else is the commit.
		value := r.fixup.value
		letters := 0
		for letters < len(value) && ('a' <= value[letters] && value[letters] <= 'z' || 'A' <= value[letters] && value[letters] <= 'Z') {
			letters++
		}
		if letters > 0 && letters < len(value) && value[letters] == ':' {
			if word := value[:letters]; word != "amend" && word != "reword" {
				return p, false
			}
			if r.haveMessage {
				return p, false
			}
			p.fixupPrefix, p.fixupCommit = "amend", value[letters+1:]
		} else {
			p.fixupPrefix, p.fixupCommit = "fixup", value
			p.useEditor = false
		}
	}
	if r.edit >= 0 {
		p.useEditor = r.edit == 1
	}
	return p, true
}

// commitCleanupMode is git's commit_msg_cleanup_mode.
type commitCleanupMode int

const (
	cleanupSpace commitCleanupMode = iota
	cleanupNone
	cleanupScissors
	cleanupAll
)

// commitCleanup is get_cleanup_mode (sequencer.c). default is strip when the editor runs and
// whitespace when it does not, and scissors is whitespace without the editor. ok is false for a
// mode git dies on.
func commitCleanup(arg commitString, useEditor bool) (commitCleanupMode, bool) {
	switch {
	case !arg.set || arg.value == "default":
		if useEditor {
			return cleanupAll, true
		}
		return cleanupSpace, true
	case arg.value == "verbatim":
		return cleanupNone, true
	case arg.value == "whitespace":
		return cleanupSpace, true
	case arg.value == "strip":
		return cleanupAll, true
	case arg.value == "scissors":
		if useEditor {
			return cleanupScissors, true
		}
		return cleanupSpace, true
	}
	return 0, false
}

// cutLine is the scissors line without its comment string (wt-status.c). A message that holds it
// is cut there when the cleanup is scissors or the commit is verbose (cleanup_message).
const cutLine = "------------------------ >8 ------------------------"

// commitStash is a message or template that the wrapper read, that git reads once, and that git
// can no longer read: standard input for -F -, or a -F path, or a -t path that template_untouched
// alone reads, that is no regular file (a FIFO, /dev/stdin, a process substitution). It is in
// file, an unlinked regular file under TMPDIR.
//
// Standard input (stdin) becomes git's standard input again: the file goes onto descriptor 0
// before the exec (asStandardInput), and `-F -` stays as written in every spelling. Descriptors 0
// to 2 reach git through any host wrapper, and a wrapper that closes the ones above 2 (Python's
// subprocess, Node's spawn, sudo) leaves them open.
//
// Any other source keeps its descriptor open across the exec, and git gets /dev/fd/<that
// descriptor> in place of the value at args[arg][at:]. The descriptor is the one the open
// returned, the lowest free one, so it takes the place of none that the caller passed. Only a git
// that is no script gets such a stash: a host's wrapper may close every descriptor above 2, so for
// a script the wrapper leaves such a source unread and adds no line (commitBuild.readFile).
type commitStash struct {
	file  *os.File
	stdin bool
	arg   int
	at    int
}

// commitBuild builds a commit's message as git does, to learn whether git accepts it and what
// git's trailer step makes of it. script is whether the git that runs the commit is a script.
type commitBuild struct {
	git    gitReader
	script bool
	opts   commitRead
	plan   commitPlan
	stash  *commitStash
}

// lines is each line of want that the message lacks, when the wrapper may add those lines, and
// nil when it adds none. It may add them when two things hold.
//
// First, git alone accepts the message: the message built as prepare_to_commit builds it, with
// the caller's own trailers added as git's trailer step adds them, cleaned as cleanup_message
// cleans it, and checked against the three refusals of cmd_commit, which --allow-empty-message
// turns off. message_is_empty refuses a message whose every line is blank or starts with
// `Signed-off-by: `, and under verbatim accepts any byte. template_untouched refuses a message
// equal to the template once both are cleaned. The third refuses an `amend!` commit with an empty
// body. A message that holds every line of want needs no line, and then nothing more is read.
//
// Second, git's trailer step changes nothing but add the lines. Each --trailer has git run
// `git interpret-trailers --in-place --no-divider` over COMMIT_EDITMSG (amend_file_with_trailers,
// trailer.c), which adds each trailer that trailer.<key>.command configures and writes an existing
// trailer block again in git's own form. So the helper runs that step twice over the text git
// would hand it: once with the caller's own --trailer values, or not at all when the caller gives
// none, since git then runs no trailer step, and once with the caller's values and then the
// lacking lines. Both results are cleaned, and the lines are added only when the second is the
// first and then exactly those lines, right after it, or after one blank line when the first has
// no trailer block. A configured trailer command makes the wrapper add none without that run: the
// run would start the command, and a command that git alone does not run must not run here.
//
// The text git hands its trailer step is COMMIT_EDITMSG as prepare_to_commit writes it: the
// message from its sources, cleaned of spaces unless the cleanup is verbatim or the template is
// the source, the -s line, and, when the editor runs with commit.status on, a line feed and the
// status lines. The status lines are left out here, and the line feed git writes before them
// stands in for them. Each status line starts with the comment string, and the trailer step
// ignores the comment and blank lines at the end of its input (ignored_log_message_bytes,
// commit.c), so it finds the same trailer block, puts the lines at the same place, and leaves
// the lines after that place as they are. The cleanup takes the status lines away again, except
// under the whitespace and verbatim cleanups, where git's message keeps them, and git then
// accepts more than the message built here.
//
// The editor is taken to leave the text as it is, which is exact for Oh My Pi's GIT_EDITOR=true.
// For a person's editor the answer is about the text before the edit. A read that fails gives
// nil. Under core.commentChar or core.commentString auto, git picks the comment character from
// the message (adjust_comment_line_char), and its status lines and cleanup use a character the
// helper's own runs do not know, so the wrapper adds none when the editor runs or the cleanup is
// strip. It adds none when a message that git cuts (the cut line under scissors or a verbose
// commit) holds the cut line.
func (b *commitBuild) lines(want []gitTrailer) ([]gitTrailer, error) {
	o, p := b.opts, b.plan
	cfg, ok := b.git.commitConfig()
	if !ok || cfg.trailerCommand {
		return nil, nil
	}
	cleanup := o.cleanup
	if !o.cleanupWritten {
		cleanup = cfg.cleanup
	}
	mode, ok := commitCleanup(cleanup, p.useEditor)
	if !ok || cfg.autoComment && (p.useEditor || mode == cleanupAll) {
		return nil, nil
	}
	verbatim := mode == cleanupNone
	includeStatus := cfg.includeStatus
	if o.statusWritten {
		includeStatus = o.includeStatus
	}

	// The template: -t, else commit.template (git_commit_config), when no source dropped it.
	template, templateWritten := commitFile{}, true
	if p.templateOn {
		if o.templateWritten {
			template = o.template
		} else {
			value, found, err := b.git.config("commit.template", "--type=path")
			if err != nil {
				return nil, nil
			}
			template, templateWritten = commitFile{set: found, value: value}, false
		}
	}

	mergeOn := !o.haveMessage && !o.file.set && !p.use.set && !o.fixup.set
	var paths gitPaths
	if mergeOn || o.file.set && o.file.value != "-" && !filepath.IsAbs(o.file.value) || template.set && !filepath.IsAbs(template.value) {
		var err error
		if paths, err = b.git.paths(); err != nil {
			return nil, nil
		}
	}
	templatePath := paths.configPath(template.value)
	if templateWritten {
		templatePath = paths.cliPath(template.value)
	}

	// The commits git reads: use_message (parse_and_validate_options), then --squash's and
	// --fixup's (prepare_to_commit).
	squashIsUse := o.squash.set && p.use.set && p.use.value == o.squash.value
	var use, squash, fixup commitText
	for _, read := range []struct {
		on   bool
		name string
		text *commitText
	}{
		{p.use.set, p.use.value, &use},
		{o.squash.set && !squashIsUse, o.squash.value, &squash},
		{o.fixup.set, p.fixupCommit, &fixup},
	} {
		if read.on {
			var err error
			if *read.text, err = b.git.commitMessage(read.name); err != nil {
				return nil, nil
			}
		}
	}

	// prepare_to_commit: the sources, in its order.
	text := ""
	switch {
	case squashIsUse:
		text = "squash! "
	case o.squash.set:
		text = "squash! " + squash.subject + "\n\n"
	}
	fromTemplate := false
	templateText, templateRead := "", false
	switch {
	case o.haveMessage && !o.fixup.set:
		text += o.message
	case o.file.set:
		path := "-"
		if o.file.value != "-" {
			path = paths.cliPath(o.file.value)
		}
		read, status, err := b.readFile(o.file, path, o.file.value == "-", true)
		if status != readDone {
			return nil, err
		}
		text += read
	case p.use.set:
		text += skipBlankLines(use.message)
	case o.fixup.set:
		text += p.fixupPrefix + "! " + fixup.subject + "\n\n"
		switch {
		case p.fixupPrefix == "fixup":
			if o.haveMessage {
				text += o.message
			}
		case strings.HasPrefix(fixup.subject, "amend!"):
			// prepare_amend_commit: an amend! commit gives its body only, so its subject is not
			// written twice.
			text += fixup.body
		default:
			text += fixup.message
		}
	case fileExists(paths.merge):
		// SQUASH_MSG goes first when a `merge --squash` left it beside MERGE_MSG.
		names := []string{paths.merge}
		if fileExists(paths.squash) {
			names = []string{paths.squash, paths.merge}
		}
		for _, name := range names {
			read, err := os.ReadFile(name)
			if err != nil {
				return nil, nil
			}
			text += string(read)
		}
	case fileExists(paths.squash):
		read, err := os.ReadFile(paths.squash)
		if err != nil {
			return nil, nil
		}
		text += string(read)
	case template.set:
		// git reads the template twice here: as the message, and again in template_untouched. A
		// template that is no regular file gives git's second read nothing, and template_untouched
		// then refuses nothing. A stash would give that read the whole text again, and git would
		// refuse a commit it accepts alone. So such a template is left for git to read (written is
		// false), and no line is added.
		read, status, err := b.readFile(template, templatePath, false, false)
		if status != readDone {
			return nil, err
		}
		text += read
		fromTemplate, templateText, templateRead = true, read, true
	}
	if template.set && !templateRead && !verbatim {
		// template_untouched reads the template again, the one read git makes of it here. Under
		// verbatim the wrapper never uses that text, and it reads nothing: template_untouched
		// returns before it opens the template for a message that is not empty, and for an empty
		// one git reads the template itself, once. A template it cannot read refuses nothing.
		read, status, err := b.readFile(template, templatePath, false, templateWritten)
		switch status {
		case readDeclined:
			return nil, err
		case readDone:
			templateText, templateRead = read, true
		}
	}
	if !verbatim && !fromTemplate {
		text = stripSpace(text)
	}
	if o.signoff {
		if text, ok = b.withSignoff(text, cfg.comment); !ok {
			return nil, nil
		}
	}
	if p.useEditor && includeStatus {
		text += "\n"
	}

	// git's trailer step with the caller's own trailers, then cleanup_message: the cut, then
	// strbuf_stripspace, with the comment string under strip only.
	first := text
	if len(o.trailers) > 0 {
		var err error
		if first, err = b.git.interpretTrailers(text, o.trailers); err != nil {
			return nil, nil
		}
	}
	// verbose is -1 until a -v option, and -1 counts as verbose here: git's commit.verbose may turn
	// verbose on, and the wrapper does not read it.
	if (mode == cleanupScissors || o.verbose != 0) && strings.Contains(first, cutLine) {
		return nil, nil
	}
	clean := func(text string) (string, error) {
		switch mode {
		case cleanupNone:
			return text, nil
		case cleanupAll:
			return b.git.stripComments(text)
		}
		return stripSpace(text), nil
	}
	message, err := clean(first)
	if err != nil {
		return nil, nil
	}

	// cmd_commit's refusals.
	if !o.allowEmptyMessage {
		if messageIsEmpty(message, verbatim) {
			return nil, nil
		}
		// template_untouched accepts any message under verbatim here, since message_is_empty
		// refused an empty one above.
		if template.set && templateRead && templateText != "" && !verbatim {
			cleanTemplate, err := clean(templateText)
			if err != nil {
				return nil, nil
			}
			rest := message
			if strings.HasPrefix(message, cleanTemplate) {
				rest = message[len(cleanTemplate):]
			}
			if restIsEmpty(rest) {
				return nil, nil
			}
		}
		if o.fixup.set && strings.HasPrefix(message, "amend! ") && messageIsEmpty(message[commitSubjectLength(message):], verbatim) {
			return nil, nil
		}
	}

	// The lines the message lacks: a line with the same key, in any case, and the same value is
	// one it holds. git reads a message's trailers with `interpret-trailers --parse`.
	held, err := b.git.trailers(message)
	if err != nil {
		return nil, nil
	}
	var lacking []gitTrailer
	for _, line := range want {
		if !slices.ContainsFunc(held, line.sameAs) {
			lacking = append(lacking, line)
		}
	}
	if len(lacking) == 0 {
		return nil, nil
	}

	// git's trailer step with the caller's trailers and then the lacking lines.
	values := slices.Clone(o.trailers)
	block := ""
	for _, line := range lacking {
		values = append(values, line.String())
		block += line.String() + "\n"
	}
	second, err := b.git.interpretTrailers(text, values)
	if err != nil {
		return nil, nil
	}
	withLines, err := clean(second)
	if err != nil {
		return nil, nil
	}
	// A message that ends in the middle of a line gets no blank line from the step's line feed,
	// only the end of that line, so the lines would join its last paragraph.
	newBlock := len(held) == 0 && (message == "" || strings.HasSuffix(message, "\n")) && withLines == message+"\n"+block
	if withLines != message+block && !newBlock {
		return nil, nil
	}
	return lacking, nil
}

// commitReadStatus is how readFile ended.
type commitReadStatus int

const (
	// readDone is a read of the whole text.
	readDone commitReadStatus = iota
	// readFailed is a read that fails, as git's own read of the source fails.
	readFailed
	// readDeclined is a source left unread for git. No trailer is added then.
	readDeclined
)

// readFile reads what git reads for the file option f at path, standard input when stdin is true.
// It reads a regular file and leaves it for git to read again. git cannot read anything else
// twice, so readFile stashes it (commitStash) when written is true: the command line names it,
// and git reads it once. It declines it when written is false: commit.template names it, so no
// argument can name the stash, or git reads it twice (commitBuild.lines). It declines a file that
// is no regular file when the git to run is a script, which may close the stash's descriptor. It
// declines standard input and every file that is no regular file when --interactive, -p or
// --pathspec-from-file is given, since git reads standard input for each before the message. The
// error is a stash it could not write after it read the source.
func (b *commitBuild) readFile(f commitFile, path string, stdin, written bool) (string, commitReadStatus, error) {
	if !stdin {
		info, err := os.Stat(path)
		if err != nil {
			return "", readFailed, nil
		}
		if info.Mode().IsRegular() {
			read, err := os.ReadFile(path)
			if err != nil {
				return "", readFailed, nil
			}
			return string(read), readDone, nil
		}
	}
	if !written || !stdin && b.script || b.opts.interactive || b.opts.patch || b.opts.pathspecFromFile.set {
		return "", readDeclined, nil
	}
	file, err := os.CreateTemp("", "legion-git-message-*")
	if err != nil {
		return "", readDeclined, nil
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		return "", readDeclined, nil
	}
	var read []byte
	if stdin {
		read, err = io.ReadAll(os.Stdin)
	} else {
		read, err = os.ReadFile(path)
	}
	if err != nil {
		file.Close()
		return "", readFailed, nil
	}
	if _, err := file.Write(read); err != nil {
		file.Close()
		return "", readDeclined, fmt.Errorf("keep the text git can no longer read: %w", err)
	}
	if !stdin {
		if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, 0); err != nil {
			file.Close()
			return "", readDeclined, fmt.Errorf("keep the text git can no longer read open for git: %w", err)
		}
	}
	b.stash = &commitStash{file: file, stdin: stdin, arg: f.arg, at: f.at}
	return string(read), readDone, nil
}

// asStandardInput makes file, a stash of standard input (commitStash), descriptor 0 of this
// process, read from its start, so git reads it as the standard input it was. A dup onto 0 leaves
// the new descriptor open across the exec, and file's own descriptor closes there. A file that is
// already descriptor 0 (this process started with none) has its close-on-exec flag cleared.
func asStandardInput(file *os.File) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind the standard input kept for git: %w", err)
	}
	fd := int(file.Fd())
	if fd == 0 {
		_, err := unix.FcntlInt(0, unix.F_SETFD, 0)
		return err
	}
	if err := dupOnto(fd, 0); err != nil {
		return fmt.Errorf("hand git the standard input kept for it: %w", err)
	}
	return nil
}

// withSignoff is text with the line -s adds, as append_signoff (sequencer.c) adds it with no
// flag, which prepare_to_commit does. The line is `Signed-off-by: <committer>` and goes before
// the comment and blank lines at the end of text (ignored_log_message_bytes, commit.c). It goes
// nowhere when it is the whole message or the last line of the message's trailer block, right
// after a trailer block, and after a blank line otherwise. comment is the comment string git
// reads then. ok is false when the committer cannot be read and where the wrapper does not place
// the line as git does: a text that holds the cut line, a `---` line (has_conforming_footer reads
// the trailer block up to it), or a cherry-pick line, and a message whose last line continues the
// line before it.
func (b *commitBuild) withSignoff(text, comment string) (string, bool) {
	if strings.Contains(text, cutLine) {
		return "", false
	}
	committer, err := b.git.committer()
	if err != nil {
		return "", false
	}
	sob := "Signed-off-by: " + committer + "\n"
	ignore := ignoredLogMessageBytes(text, comment)
	if ignore == 0 && text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	at := len(text) - ignore
	body := text[:at]
	if body == sob {
		return text, true
	}
	for line := range strings.Lines(body) {
		if rest, ok := strings.CutPrefix(line, "---"); ok && rest != "" && gitIsSpace(rest[0]) {
			return "", false
		}
	}
	// has_conforming_footer: whether the message has a trailer block, and whether the line is its
	// last entry. Every block holds a line with a separator, which `--only-trailers` prints, but
	// one made of cherry-pick lines alone.
	block, err := b.git.output(body, "interpret-trailers", "--only-trailers", "--only-input")
	if err != nil || block == "" && strings.Contains(body, "(cherry picked from commit ") {
		return "", false
	}
	gap := ""
	if block != "" {
		last := body[strings.LastIndexByte(strings.TrimSuffix(body, "\n"), '\n')+1:]
		if gitIsSpace(last[0]) {
			return "", false
		}
		if last == sob {
			return text, true
		}
	} else {
		switch {
		case at == 0:
			gap = "\n\n"
		case at == 1 || body[at-2] != '\n':
			gap = "\n"
		}
	}
	return body + gap + sob + text[at:], true
}

// ignoredLogMessageBytes is ignored_log_message_bytes (commit.c) for a text that holds no cut
// line: the length of the run of comment lines, blank lines and an old `Conflicts:` block at its
// end. As in git, a run that starts on the first line counts from the line after it.
func ignoredLogMessageBytes(text, comment string) int {
	boc, bol := 0, 0
	conflicts := false
	for line := range strings.Lines(text) {
		switch {
		case strings.HasPrefix(line, comment) || line[0] == '\n':
			if boc == 0 {
				boc = bol
			}
		case strings.HasPrefix(line, "Conflicts:\n"):
			conflicts = true
			if boc == 0 {
				boc = bol
			}
		case conflicts && line[0] == '\t':
		case boc != 0:
			boc, conflicts = 0, false
		}
		bol += len(line)
	}
	if boc == 0 {
		return 0
	}
	return len(text) - boc
}

// stripSpace is strbuf_stripspace (strbuf.c) with no comment string, as the whitespace and
// scissors cleanups and prepare_to_commit run it: each line loses the spaces at its end, a run of
// blank lines becomes one blank line, the blank lines at the start and the end go, and the last
// line ends with a line feed.
func stripSpace(text string) string {
	var out strings.Builder
	out.Grow(len(text) + 1)
	empties := 0
	for line := range strings.Lines(text) {
		line = trimGitSpace(line)
		if line == "" {
			empties++
			continue
		}
		if empties > 0 && out.Len() > 0 {
			out.WriteByte('\n')
		}
		empties = 0
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// sameAs is whether held, a line `interpret-trailers --parse` printed, is this line: the same key
// in any case, and the same value.
func (l gitTrailer) sameAs(held string) bool {
	key, value, ok := strings.Cut(held, ": ")
	return ok && strings.EqualFold(key, l.key) && value == l.value
}

// fileExists is whether stat finds path, as prepare_to_commit tests MERGE_MSG and SQUASH_MSG.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return path != "" && err == nil
}

// gitIsSpace is git's own isspace (sane_ctype, ctype.c): space, tab, line feed and carriage
// return.
func gitIsSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// oneLine is the first line of s with its line feed, as get_one_line counts it.
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i+1]
	}
	return s
}

// trimGitSpace is line without the spaces at its end, as is_blank_line trims it.
func trimGitSpace(line string) string {
	for line != "" && gitIsSpace(line[len(line)-1]) {
		line = line[:len(line)-1]
	}
	return line
}

// skipBlankLines is s from its first line that is not blank (skip_blank_lines, commit.c).
func skipBlankLines(s string) string {
	for {
		line := oneLine(s)
		if line == "" || trimGitSpace(line) != "" {
			return s
		}
		s = s[len(line):]
	}
}

// commitSubjectLength is the length of the first paragraph of body, its line feeds included
// (commit_subject_length, commit.c).
func commitSubjectLength(body string) int {
	p := 0
	for p < len(body) && len(skipBlankLines(body[p:])) == len(body)-p {
		p += len(oneLine(body[p:]))
	}
	return p
}

// restIsEmpty is whether every line of s is blank or starts with `Signed-off-by: ` (rest_is_empty,
// sequencer.c).
func restIsEmpty(s string) bool {
	for s != "" {
		line := oneLine(s)
		s = s[len(line):]
		if strings.HasPrefix(line, "Signed-off-by: ") {
			continue
		}
		if trimGitSpace(line) != "" {
			return false
		}
	}
	return true
}

// messageIsEmpty is message_is_empty (sequencer.c): under verbatim any byte is a message.
func messageIsEmpty(message string, verbatim bool) bool {
	if verbatim && message != "" {
		return false
	}
	return restIsEmpty(message)
}

// gitReader runs the helper git for the reads of a commit: with the caller's global options, with
// env as its environment (the process's own when env is nil), and with standard input from
// /dev/null unless the read hands it text. Its standard error goes nowhere, since a read prints
// nothing of its own.
type gitReader struct {
	path    string
	globals []string
	env     []string
}

// command is `<helper> <args…>` under the reader's environment.
func (g gitReader) command(args ...string) *exec.Cmd {
	cmd := exec.Command(g.path, args...)
	cmd.Env = g.env
	return cmd
}

// output runs `<helper> <globals> <args…>` with stdin as its standard input and answers what it
// printed.
func (g gitReader) output(stdin string, args ...string) (string, error) {
	cmd := g.command(slices.Concat(g.globals, args)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	return string(out), err
}

// atLeast is whether `<helper> version` names major.minor or later.
func (g gitReader) atLeast(major, minor int) bool {
	out, err := g.command("version").Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return false
	}
	parts := strings.SplitN(fields[2], ".", 3)
	if len(parts) < 2 {
		return false
	}
	got, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	gotMinor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return got > major || got == major && gotMinor >= minor
}

// config is the value that `git config --get name` gives, with the options in typ (a --type)
// before --get. found is false when name is unset, which git answers with exit 1.
func (g gitReader) config(name string, typ ...string) (value string, found bool, err error) {
	out, err := g.output("", slices.Concat([]string{"config"}, typ, []string{"--get", name})...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSuffix(out, "\n"), true, nil
}

// commitText is a commit's message as git formats it: %s, its subject, the lines of the first
// paragraph joined by a space, %b, the body after that paragraph, and %B, the whole message.
type commitText struct {
	subject, body, message string
}

// commitMessage is %s, %b and %B of the commit name names, read in one `git log -1`, as
// prepare_to_commit and prepare_amend_commit format them: `squash! %s`, `fixup! %s`, the body of
// an amend! commit, and the whole message of -C, -c and --amend. NUL bytes part the three, which
// git keeps out of every commit message. `--end-of-options` and `--` keep a name that looks like
// an option or a path a revision, and a name git commit cannot look up (a range, a negative
// revision) prints no commit, which is a failure.
func (g gitReader) commitMessage(name string) (commitText, error) {
	out, err := g.output("", "log", "-1", "--no-show-signature", "--format=%s%x00%b%x00%B", "--end-of-options", name, "--")
	if err != nil {
		return commitText{}, err
	}
	parts := strings.Split(out, "\x00")
	message, ok := strings.CutSuffix(parts[len(parts)-1], "\n")
	if len(parts) != 3 || !ok {
		return commitText{}, fmt.Errorf("git log -1 %s printed no commit", name)
	}
	return commitText{subject: parts[0], body: parts[1], message: message}, nil
}

// stripComments is text cleaned by `git stripspace --strip-comments`, which runs
// strbuf_stripspace with the comment string as the strip cleanup does, so core.commentChar and
// core.commentString apply.
func (g gitReader) stripComments(text string) (string, error) {
	return g.output(text, "stripspace", "--strip-comments")
}

// interpretTrailers is text after git's trailer step with values: `git interpret-trailers
// --no-divider --trailer=<value>…`, as amend_file_with_trailers runs it over COMMIT_EDITMSG with
// the --trailer options git commit kept (parse_opt_passthru_argv writes each as
// `--trailer=<value>`).
func (g gitReader) interpretTrailers(text string, values []string) (string, error) {
	args := []string{"interpret-trailers", "--no-divider"}
	for _, value := range values {
		args = append(args, "--trailer="+value)
	}
	return g.output(text, args...)
}

// trailers is each trailer of message's trailer block, one `<key>: <value>` each, as
// `git interpret-trailers --parse --no-divider` prints them, and none when it has no block.
func (g gitReader) trailers(message string) ([]string, error) {
	out, err := g.output(message, "interpret-trailers", "--parse", "--no-divider")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n"), nil
}

// committer is the committer as -s names it (fmt_name): `git var GIT_COMMITTER_IDENT` without the
// date and the time zone at its end.
func (g gitReader) committer() (string, error) {
	out, err := g.output("", "var", "GIT_COMMITTER_IDENT")
	if err != nil {
		return "", err
	}
	ident := strings.TrimSuffix(out, "\n")
	for range 2 {
		end := strings.LastIndexByte(ident, ' ')
		if end < 0 {
			return "", fmt.Errorf("git var printed no committer: %q", out)
		}
		ident = ident[:end]
	}
	return ident, nil
}

// commitConfig is the configuration git commit reads that the wrapper needs, other than
// commit.template, which needs a path's expansion.
type commitConfig struct {
	// cleanup is commit.cleanup.
	cleanup commitString
	// includeStatus is commit.status: whether the editor's text gets the status lines.
	includeStatus bool
	// comment is the comment string, core.commentChar or core.commentString, the last one set,
	// and `#` when neither is. autoComment is whether that last one is auto, and then comment is
	// `#` until git picks a character from the message.
	comment     string
	autoComment bool
	// trailerCommand is whether some trailer.<key>.command or trailer.<key>.cmd is set.
	trailerCommand bool
}

// commitConfig reads commitConfig with one `git config -z --get-regexp`, whose output is each
// entry, in the order git reads them, as `<key>`, a line feed and the value when there is one, and
// a NUL. ok is false when the read fails, and when a value is one git dies on (git_commit_config,
// git_default_core_config): a commit.cleanup or comment string with no value, an empty comment
// string or one with a line feed, and a commit.status that is no boolean.
func (g gitReader) commitConfig() (cfg commitConfig, ok bool) {
	cfg = commitConfig{includeStatus: true, comment: "#"}
	out, err := g.output("", "config", "-z", "--get-regexp", `^(commit\.(cleanup|status)|core\.comment(char|string)|trailer\..+\.(command|cmd))$`)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return cfg, true
	}
	if err != nil {
		return cfg, false
	}
	for _, entry := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		key, value, hasValue := strings.Cut(entry, "\n")
		switch key {
		case "commit.cleanup":
			if !hasValue {
				return cfg, false
			}
			cfg.cleanup = commitString{true, value}
		case "commit.status":
			if cfg.includeStatus, ok = gitBool(value, hasValue); !ok {
				return cfg, false
			}
		case "core.commentchar", "core.commentstring":
			switch {
			case !hasValue || value == "" || strings.Contains(value, "\n"):
				return cfg, false
			case strings.EqualFold(value, "auto"):
				cfg.comment, cfg.autoComment = "#", true
			default:
				cfg.comment, cfg.autoComment = value, false
			}
		default:
			cfg.trailerCommand = true
		}
	}
	return cfg, true
}

// gitBool is git_config_bool for a value without a unit: no value is true, an empty one false,
// yes, on, true, no, off and false in any case, and else an integer that is true when it is not
// zero. ok is false for anything else, which git dies on.
func gitBool(value string, hasValue bool) (b, ok bool) {
	switch {
	case !hasValue:
		return true, true
	case value == "":
		return false, true
	case strings.EqualFold(value, "true") || strings.EqualFold(value, "yes") || strings.EqualFold(value, "on"):
		return true, true
	case strings.EqualFold(value, "false") || strings.EqualFold(value, "no") || strings.EqualFold(value, "off"):
		return false, true
	}
	n, err := strconv.Atoi(value)
	return n != 0, err == nil
}

// gitPaths is where git commit reads its files. top is the top of the work tree, the directory
// git commit runs in (setup_work_tree). prefix is the directory under top where git started, with
// its slash, or "" outside the work tree. merge and squash are MERGE_MSG and SQUASH_MSG.
type gitPaths struct {
	top, prefix, merge, squash string
}

// paths reads gitPaths with one `git rev-parse`. It fails outside a work tree, where git commit
// fails too.
func (g gitReader) paths() (gitPaths, error) {
	out, err := g.output("", "rev-parse", "--path-format=absolute", "--show-toplevel", "--show-prefix", "--git-path", "MERGE_MSG", "--git-path", "SQUASH_MSG")
	if err != nil {
		return gitPaths{}, err
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 {
		return gitPaths{}, fmt.Errorf("git rev-parse printed %d lines, not 4", len(lines))
	}
	return gitPaths{top: lines[0], prefix: lines[1], merge: lines[2], squash: lines[3]}, nil
}

// cliPath is where git commit reads a file that -F or -t names. fix_filename puts the prefix
// before a relative path, and git reads it from top. A lone `-` gets no prefix
// (prefix_filename_except_for_dash).
func (p gitPaths) cliPath(file string) string {
	switch {
	case filepath.IsAbs(file):
		return file
	case file == "-":
		return p.top + "/-"
	}
	return p.top + "/" + p.prefix + file
}

// configPath is where git commit reads the file commit.template names: a relative path is read
// from top.
func (p gitPaths) configPath(file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	return p.top + "/" + file
}
