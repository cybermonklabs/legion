package main

import "strings"

// This file reads git commit's options as git 2.47.3 reads them: builtin_commit_options and
// parse_and_validate_options of builtin/commit.c, read with parse_options of parse-options.c.

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
