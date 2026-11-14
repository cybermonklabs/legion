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
// reads them, the message as git builds and cleans it, git's rules for a message it refuses, and
// the argument list that adds the lines. The git it mirrors is 2.47.3, the worker image's: the
// functions each part names are in builtin/commit.c, sequencer.c, parse-options.c, commit.c and
// pretty.c of that release.

// gitTrailer is one trailer line that `legion git` adds to a commit: `<key>: <value>`.
type gitTrailer struct{ key, value string }

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

// gitCommit is the argument list that `legion git` hands git for a commit, and keep, the file
// whose descriptor that list names in place of a message or template git could no longer read
// (commitStash). keep must stay open until the exec.
type gitCommit struct {
	args []string
	keep *os.File
}

// commitWithTrailers is the argument list for `git <globals> commit <args>`, read with helper.
// It adds the lines of gitTrailers only when it has shown that git alone would accept the
// commit's message (commitBuild.accepted). For each line it puts
// `-c trailer.<key>.ifExists=addIfDifferent` right before the commit word, after the caller's
// own global options, so it outranks a caller's own -c of that name. It puts
// `--trailer '<key>: <value>'` right after the commit word, before every argument of the caller,
// since a --trailer after `--` is a path. Only the two keys get addIfDifferent, so an amend adds
// only the lines the message lacks. It sets no trailer.ifExists that applies to every key, so the
// caller's own trailers keep git's behaviour.
//
// It adds none and leaves every argument as written when no line is valid, when -h or --help is
// among the options, when an option matches no option of git commit, when git would make no
// commit (a dry run), when the helper's version is below 2.32 (the first git with
// `commit --trailer`), and when it cannot show that git would accept the message. One rewrite it
// always makes: a message or template it has read from standard input or from a file that is no
// regular file goes to git as /dev/fd/<n> (commitStash), trailers or not, since git can no longer
// read the original. The error is a stash it could not write, and then it hands git nothing.
func commitWithTrailers(helper string, globals, args []string, env map[string]string) (gitCommit, error) {
	written := gitCommit{args: slices.Concat(globals, []string{"commit"}, args)}
	lines := gitTrailers(env)
	if len(lines) == 0 {
		return written, nil
	}
	opts, ok := readCommitOptions(args)
	if !ok || opts.help || opts.dryRun || opts.status || opts.null {
		return written, nil
	}
	plan, ok := opts.plan()
	if !ok {
		return written, nil
	}
	git := gitReader{path: helper, globals: globals}
	if !git.atLeast(2, 32) {
		return written, nil
	}
	b := commitBuild{git: git, opts: opts, plan: plan}
	accepted, err := b.accepted()
	if err != nil {
		return gitCommit{}, err
	}
	commit := gitCommit{args: args}
	if b.stash != nil {
		commit.args = slices.Clone(args)
		word := commit.args[b.stash.arg]
		commit.args[b.stash.arg] = word[:b.stash.at] + "/dev/fd/" + strconv.Itoa(int(b.stash.file.Fd()))
		commit.keep = b.stash.file
	}
	if !accepted {
		commit.args = slices.Concat(globals, []string{"commit"}, commit.args)
		return commit, nil
	}
	out := slices.Clone(globals)
	for _, line := range lines {
		out = append(out, "-c", "trailer."+line.key+".ifExists=addIfDifferent")
	}
	out = append(out, "commit")
	for _, line := range lines {
		out = append(out, "--trailer", line.key+": "+line.value)
	}
	commit.args = append(out, commit.args...)
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
	// trailers counts the caller's --trailer options.
	trailers int
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
	// dryRun, status and null each make a commit a dry run: --dry-run, one of --short,
	// --porcelain and --long, and -z.
	dryRun, status, null bool
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
	r = commitRead{verbose: -1, edit: -1}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--" || arg == "--end-of-options":
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
		r.trailers++
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
		r.status = !unset
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

// commitStash is a message or template that the wrapper read and git can no longer read: standard
// input for -F -, or a -F or -t path that is no regular file (a FIFO, /dev/stdin, a process
// substitution). It is in file, an unlinked regular file under TMPDIR whose descriptor stays open
// across the exec, and git gets /dev/fd/<that descriptor> in place of the value at args[arg][at:].
// The descriptor is the one the open returned, the lowest free one, so it takes the place of none
// that the caller passed.
type commitStash struct {
	file *os.File
	arg  int
	at   int
}

// commitBuild builds a commit's message as git does, to learn whether git accepts it.
type commitBuild struct {
	git   gitReader
	opts  commitRead
	plan  commitPlan
	stash *commitStash
}

// accepted is whether git alone would accept the commit's message: the message built as
// prepare_to_commit builds it, cleaned as cleanup_message cleans it, and checked against the three
// refusals of cmd_commit, which --allow-empty-message turns off. message_is_empty refuses a
// message whose every line is blank or starts with `Signed-off-by: `, and under verbatim accepts
// any byte. template_untouched refuses a message equal to the template once both are cleaned. The
// third refuses an `amend!` commit with an empty body.
//
// The editor is taken to leave the text as it is, which is exact for Oh My Pi's GIT_EDITOR=true.
// For a person's editor the answer is about the text before the edit. A read that fails answers
// false.
//
// Where git's text holds more than the text built here, git accepts more: the status lines git
// adds for the editor, the comment character that core.commentChar=auto chooses, and the caller's
// own trailers. Where the two texts could differ the other way, the answer is false: a message
// that holds the cut line under scissors or a verbose commit, which git cuts, and a template check
// of a message that is not the template's own text beside -s, a caller's --trailer or
// core.commentChar=auto.
func (b *commitBuild) accepted() (bool, error) {
	o, p := b.opts, b.plan
	cleanup := o.cleanup
	if !o.cleanupWritten {
		value, found, err := b.git.config("commit.cleanup")
		if err != nil {
			return false, nil
		}
		cleanup = commitString{found, value}
	}
	mode, ok := commitCleanup(cleanup, p.useEditor)
	if !ok {
		return false, nil
	}
	verbatim := mode == cleanupNone

	// The template: -t, else commit.template (git_commit_config), when no source dropped it.
	template, templateWritten := commitFile{}, true
	if p.templateOn {
		if o.templateWritten {
			template = o.template
		} else {
			value, found, err := b.git.config("commit.template", "--type=path")
			if err != nil {
				return false, nil
			}
			template, templateWritten = commitFile{set: found, value: value}, false
		}
	}

	mergeOn := !o.haveMessage && !o.file.set && !p.use.set && !o.fixup.set
	var paths gitPaths
	if mergeOn || o.file.set && o.file.value != "-" && !filepath.IsAbs(o.file.value) || template.set && !filepath.IsAbs(template.value) {
		var err error
		if paths, err = b.git.paths(); err != nil {
			return false, nil
		}
	}
	templatePath := paths.configPath(template.value)
	if templateWritten {
		templatePath = paths.cliPath(template.value)
	}

	// The commits git reads: use_message (parse_and_validate_options), then --squash's and
	// --fixup's (prepare_to_commit).
	squashIsUse := o.squash.set && p.use.set && p.use.value == o.squash.value
	var useText, squashText, fixupText string
	for _, read := range []struct {
		on   bool
		name string
		text *string
	}{
		{p.use.set, p.use.value, &useText},
		{o.squash.set && !squashIsUse, o.squash.value, &squashText},
		{o.fixup.set, p.fixupCommit, &fixupText},
	} {
		if read.on {
			var err error
			if *read.text, err = b.git.commitMessage(read.name); err != nil {
				return false, nil
			}
		}
	}

	// prepare_to_commit: the sources, in its order.
	text := ""
	switch {
	case squashIsUse:
		text = "squash! "
	case o.squash.set:
		subject, _ := commitSubjectAndBody(squashText)
		text = "squash! " + subject + "\n\n"
	}
	templateOnly := false
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
			return false, err
		}
		text += read
	case p.use.set:
		text += skipBlankLines(useText)
	case o.fixup.set:
		subject, body := commitSubjectAndBody(fixupText)
		text += p.fixupPrefix + "! " + subject + "\n\n"
		switch {
		case p.fixupPrefix == "fixup":
			if o.haveMessage {
				text += o.message
			}
		case strings.HasPrefix(skipBlankLines(fixupText), "amend!"):
			// prepare_amend_commit: an amend! commit gives its body only, so its subject is not
			// written twice.
			text += body
		default:
			text += fixupText
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
				return false, nil
			}
			text += string(read)
		}
	case fileExists(paths.squash):
		read, err := os.ReadFile(paths.squash)
		if err != nil {
			return false, nil
		}
		text += string(read)
	case template.set:
		read, status, err := b.readFile(template, templatePath, false, templateWritten)
		if status != readDone {
			return false, err
		}
		text += read
		templateOnly, templateText, templateRead = !o.squash.set, read, true
	}
	if template.set && !templateRead {
		// template_untouched reads the template again. A template it cannot read refuses
		// nothing.
		read, status, err := b.readFile(template, templatePath, false, templateWritten)
		switch status {
		case readDeclined:
			return false, err
		case readDone:
			templateText, templateRead = read, true
		}
	}
	if o.signoff {
		text = withSignoff(text)
	}

	// cleanup_message: the cut, then strbuf_stripspace, with the comment string under strip only.
	if (mode == cleanupScissors || o.verbose != 0) && strings.Contains(text, cutLine) {
		return false, nil
	}
	message := text
	if !verbatim {
		var err error
		if message, err = b.git.stripspace(text, mode == cleanupAll); err != nil {
			return false, nil
		}
	}

	// cmd_commit's refusals.
	if o.allowEmptyMessage {
		return true, nil
	}
	if messageIsEmpty(message, verbatim) {
		return false, nil
	}
	// template_untouched accepts any message under verbatim here, since message_is_empty refused
	// an empty one above.
	if template.set && templateRead && templateText != "" && !verbatim {
		clean, err := b.git.stripspace(templateText, mode == cleanupAll)
		if err != nil {
			return false, nil
		}
		if !templateOnly && clean != "" && (o.signoff || o.trailers > 0 || mode == cleanupAll && b.autoCommentChar()) {
			return false, nil
		}
		rest := message
		if strings.HasPrefix(message, clean) {
			rest = message[len(clean):]
		}
		if restIsEmpty(rest) {
			return false, nil
		}
	}
	if o.fixup.set && strings.HasPrefix(message, "amend! ") && messageIsEmpty(message[commitSubjectLength(message):], verbatim) {
		return false, nil
	}
	return true, nil
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
// twice, so readFile stashes it (commitStash) when the command line names it (written). It
// declines a file that commit.template names and that is no regular file, since no argument can
// name the stash. It declines standard input and every file that is no regular file when
// --interactive, -p or --pathspec-from-file is given, since git reads standard input for each
// before the message. The error is a stash it could not write after it read the source.
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
	if !written || b.opts.interactive || b.opts.patch || b.opts.pathspecFromFile.set {
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
	if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, 0); err != nil {
		file.Close()
		return "", readDeclined, fmt.Errorf("keep the text git can no longer read open for git: %w", err)
	}
	b.stash = &commitStash{file: file, arg: f.arg, at: f.at}
	return string(read), readDone, nil
}

// autoCommentChar is whether core.commentChar is auto, read with the caller's global options. A
// read that fails counts as auto.
func (b *commitBuild) autoCommentChar() bool {
	value, found, err := b.git.config("core.commentChar")
	return err != nil || found && strings.EqualFold(value, "auto")
}

// withSignoff is text with the line -s adds (append_signoff, sequencer.c). Only whether a message
// holds such a line decides a refusal, so its name and its place do not matter here. accepted
// answers false for the one check where its place could matter.
func withSignoff(text string) string {
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + "\nSigned-off-by: legion\n"
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

// commitSubjectAndBody is %s and %b of a commit whose %B is message (pretty.c). The subject is
// the lines of the first paragraph, each without the spaces at its end, joined by a space
// (format_subject). The body is the rest after that paragraph and the blank lines after it.
func commitSubjectAndBody(message string) (subject, body string) {
	s := skipBlankLines(message)
	var lines []string
	for {
		line := oneLine(s)
		s = s[len(line):]
		trimmed := trimGitSpace(line)
		if trimmed == "" {
			break
		}
		lines = append(lines, trimmed)
	}
	return strings.Join(lines, " "), skipBlankLines(s)
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

// gitReader runs the helper git for the reads of a commit: with the caller's global options, and
// with standard input from /dev/null unless the read hands it text. Its standard error goes
// nowhere, since a read prints nothing of its own.
type gitReader struct {
	path    string
	globals []string
}

// output runs `<helper> <globals> <args…>` with stdin as its standard input and answers what it
// printed.
func (g gitReader) output(stdin string, args ...string) (string, error) {
	cmd := exec.Command(g.path, slices.Concat(g.globals, args)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	return string(out), err
}

// atLeast is whether `<helper> version` names major.minor or later.
func (g gitReader) atLeast(major, minor int) bool {
	out, err := exec.Command(g.path, "version").Output()
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

// commitMessage is %B of the commit name names: its whole message, as use_message and
// prepare_amend_commit read it. `--end-of-options` and `--` keep a name that looks like an option
// or a path a revision, and a name git commit cannot look up (a range, a negative revision)
// prints no commit, which is a failure.
func (g gitReader) commitMessage(name string) (string, error) {
	out, err := g.output("", "log", "-1", "--no-show-signature", "--format=%B", "--end-of-options", name, "--")
	if err != nil {
		return "", err
	}
	text, ok := strings.CutSuffix(out, "\n")
	if !ok {
		return "", fmt.Errorf("git log -1 %s printed no commit", name)
	}
	return text, nil
}

// stripspace is text cleaned by `git stripspace`, which runs strbuf_stripspace as git commit does,
// with --strip-comments when comments is true (so core.commentChar and core.commentString apply).
func (g gitReader) stripspace(text string, comments bool) (string, error) {
	args := []string{"stripspace"}
	if comments {
		args = append(args, "--strip-comments")
	}
	return g.output(text, args...)
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
