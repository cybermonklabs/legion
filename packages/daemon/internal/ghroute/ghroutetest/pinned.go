package ghroutetest

import "strings"

// PinnedCheckPortSHA256 is the sha256 of the public_leak_check.py PinnedCheckReads was ported
// from, leak-check-v33's. ghroute's TestGhReadsAreProvenAgainstThePinnedCheck fails while it is
// not ghroute.ReadsProvenCheckSHA256, so a bump of the pin fails until this port moves with it.
const PinnedCheckPortSHA256 = "2e9049bfe5969e039878ae88d72dbc844a843f2d7a4819124456f3dd6c2dd1c3"

// PinnedCheckReads says whether the pinned check, the public_leak_check.py whose sha256 is
// PinnedCheckPortSHA256, reads argv, a gh call's arguments, as a read in its gh_guard: whether it
// returns for the call before it looks up any target. It reads the call as that check does with
// no GH_HOST, which no call of the tests sets.
//
// The check first asks gh's aliases (gh_alias), for every call but exactly `alias list` and
// `alias delete -- KEY`, which it lets go at once, and which this reads as is_read reads them.
// After that the read is the check's, part for part (its gh_guard, is_read, collect and
// api_read): `--help`, `-h` and `--version` first; then a call that begins with an option, whose
// words after -R/--repo values and other options are read by is_read, and which the check
// refuses, so no read, where they are not; then `gh api` (pinnedAPIRead); and any other call by
// is_read over its first two words (pinnedIsRead).
//
// Five parts differ from the check. Two are stricter here: a GraphQL call is never a read here,
// where the check reads a query with no mutation as one, and `gh secret`, which the check lets go
// once it has asked gh's aliases and before it reads anything else, is read here as is_read reads
// it. Three are looser, and in each the check refuses the call before it looks up any target, so
// none makes a call the port reads as a read cost the check a lookup. The check refuses a call
// that an alias gh lists matches (gh_alias): a key of one word that is one of the words gh may
// read as the call's command, or a key of several words whose later words come, in that order,
// among the arguments after the first such word, as `gh alias list` lists them for GH_CONFIG_DIR
// and for each directory under LEGION_GH_OWNERS_DIR that holds a config.yml. This reads every
// call as if gh lists no alias. ghroute's TargetOf refuses such a call itself, before any guard,
// only where a word of the command path it walks is an alias that its parent's help lists
// (`pr pv2` for the key `pr pv2`), so a call that the check matches more widely, such as
// `pr view pv2` for that key, reaches the check. The check exits 2 where it cannot read gh's
// aliases: no gh to run, a working directory that is gone, a LEGION_GH_OWNERS_DIR it cannot list,
// or a `gh alias list` that fails or does not answer within its bound (read_gh_aliases). And the
// check exits 2 on a missing or unreadable file an argument names (file_ok), which this never
// looks for, so `api --input f --input= repos/acme/widgets/pulls` with no file f is a read here.
// No call the tests count as a read meets any of the five.
//
// The guard calls the check as `gh-guard -- <argv>`, and `gh-guard` alone for a call of no words
// (leakguard's modeArgv). The check's main() takes out the one `--` the guard writes after the mode
// word, so gh_guard gets argv unchanged, a leading `--` of the call's own among it; a call of no
// words returns 0 in mode_gh before gh_guard, and this reads it as is_read reads an empty first
// word.
func PinnedCheckReads(argv []string) bool {
	first := ""
	if len(argv) > 0 {
		first = argv[0]
	}
	switch {
	case first == "--help" || first == "-h" || first == "--version":
		return true
	case strings.HasPrefix(first, "-"):
		var words []string
		skip := false
		for _, word := range argv {
			if !skip && (word == "-R" || word == "--repo") {
				skip = true
				continue
			}
			if skip || strings.HasPrefix(word, "-") {
				skip = false
				continue
			}
			words = append(words, word)
		}
		return pinnedIsRead(words)
	case first == "api":
		return pinnedAPIRead(argv[1:])
	}
	return pinnedIsRead(argv)
}

// pinnedReadAll and pinnedReadTwo are the pinned check's READ_ALL and READ_TWO, copied whole: the
// first words of a call it reads as a read, and the first and second words, joined by `:`, of the
// others.
var (
	pinnedReadAll = wordSet("auth browse completion config alias extension help licenses search status version --help -h --version")
	pinnedReadTwo = wordSet("pr:view pr:list issue:view issue:list run:view run:list repo:view repo:list " +
		"release:view release:list workflow:view workflow:list project:view project:list ruleset:view ruleset:list " +
		"release:download pr:status issue:status pr:checks pr:diff pr:checkout run:watch run:download repo:clone " +
		"repo:set-default ruleset:check variable:get cache:list label:list org:list gpg-key:list ssh-key:list " +
		"codespace:list variable:list attestation:verify attestation:download")
)

// pinnedIsRead is the pinned check's is_read over words: an empty first word, a first word of
// READ_ALL, or a first and second word of READ_TWO.
func pinnedIsRead(words []string) bool {
	first, second := "", ""
	if len(words) > 0 {
		first = words[0]
	}
	if len(words) > 1 {
		second = words[1]
	}
	return first == "" || pinnedReadAll[first] || pinnedReadTwo[first+":"+second]
}

// pinnedAPIValueOptions are the pinned check's API_VALUE_OPTS, the long gh api options its collect
// takes a value for in the next word, and pinnedFieldOptions are its FIELD_ARGS.
var (
	pinnedAPIValueOptions = wordSet("--method --field --raw-field --header --jq --template --preview --cache --input --hostname")
	pinnedFieldOptions    = wordSet("-f -F --field --raw-field")
)

// pinnedAPIRead is the pinned check's api_read over its collect's parse of args, the words of a
// gh api call after `api`.
//
// collect reads each word in turn. The word after an option still waiting for its value is that
// value; `--name=value` is the option and its value; a long option of API_VALUE_OPTS waits for its
// value; and a single-dash word is scanned for the first of `XfFHpqt`, the short options that take
// a value, and the rest of the word after that letter, past one `=`, is that option's value. An
// option left with an empty value waits for the next word again, unless its word ends in `=`, and
// a field option sets the field mark either way. The first word that begins with no `-` and is no
// option and no value is the endpoint. The last method is kept, uppercased, and the last host and
// the last input as given.
//
// api_read then reads a read where the host is github.com, the endpoint holds no `://` and is not
// GitHub's GraphQL one, and the call has no input, and the method, or the field mark where no
// method is given, is one of "0", "GET" and "HEAD": the check's `(st.M or str(st.F)) in ("0",
// "GET", "HEAD")`, with the field mark 0 or 1. The endpoint is GraphQL's where it is `graphql` once
// its leading slashes are gone and it is cut at its first `?` or `#` (the check's
// graphql_endpoint, which also strips a leading scheme and host: an endpoint holding `://` is no
// read before that matters).
func pinnedAPIRead(args []string) bool {
	var endpoint, method, host, input, pending string
	field := false
	for _, word := range args {
		option, value := "", ""
		switch {
		case pending != "":
			option, value, pending = pending, word, ""
		case strings.HasPrefix(word, "--") && strings.Contains(word, "="):
			option, value, _ = strings.Cut(word, "=")
		case pinnedAPIValueOptions[word]:
			option = word
		case len(word) > 1 && word[0] == '-' && word[1] != '-':
			if at := strings.IndexAny(word[1:], "XfFHpqt"); at >= 0 {
				option, value = "-"+word[1+at:2+at], strings.TrimPrefix(word[2+at:], "=")
			}
		}
		if option != "" && value == "" && !strings.HasSuffix(word, "=") {
			pending, option = option, ""
			if pinnedFieldOptions[pending] {
				field = true
			}
		}
		if !strings.HasPrefix(word, "-") && option == "" && pending == "" && endpoint == "" {
			endpoint = word
		}
		switch {
		case option == "-X" || option == "--method":
			method = strings.ToUpper(value)
		case option == "--hostname":
			host = value
		case option == "--input":
			input = value
		case pinnedFieldOptions[option]:
			field = true
		}
	}
	if host == "" {
		host = "github.com"
	}
	// The check lowercases the host and compares it with github.com. Only an ASCII host of the
	// same length can match, and EqualFold over the same number of bytes is that test.
	if len(host) != len("github.com") || !strings.EqualFold(host, "github.com") || strings.Contains(endpoint, "://") {
		return false
	}
	path := strings.TrimLeft(endpoint, "/")
	if end := strings.IndexAny(path, "?#"); end >= 0 {
		path = path[:end]
	}
	if path == "graphql" || input != "" {
		return false
	}
	if method != "" {
		return method == "0" || method == "GET" || method == "HEAD"
	}
	return !field
}

// wordSet is the set of the words in words, separated by spaces.
func wordSet(words string) map[string]bool {
	set := map[string]bool{}
	for _, word := range strings.Fields(words) {
		set[word] = true
	}
	return set
}
