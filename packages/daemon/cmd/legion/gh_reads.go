package main

import (
	"net/url"
	"slices"
	"strings"
)

// ghCheckedOnlyVisibility is the call log's visibility for a call the shared check alone read
// (leakguard.GhAnswer.CheckedOnly): a call that writes nothing, under the check the rule was
// proven against, for which nothing read its repository's visibility. It is neither `github` nor
// `kept`, and not the empty value of a call the guard read no visibility for because it named no
// repository.
const ghCheckedOnlyVisibility = "checked-only"

// ghReadsProvenCheck is the shared check ghWritesNothing's rule was proven against: its tag,
// which is the tag the worker image pins (`ARG LEAK_CHECK_TAG` in
// packages/daemon/docker/worker.Dockerfile, which TestGhReadsAreProvenAgainstThePinnedCheck holds
// to this one), and the sha256 of that tag's leak-check/public_leak_check.py. The guard lets the
// shared check alone read a call that writes nothing only where the public_leak_check.py beside
// the check it resolved hashes to this (leakguard.GhRequest.ProvenCheck), so a pod under another
// pin, a tmux pane's check of another version, and a check edited under the same tag get the
// whole guard. A bump of the pin fails that test until the tag, the sha256 and the tests' port of
// the check all move: read the new check's gh_guard, is_read, READ_ALL, READ_TWO, collect and
// api_read, and every definition they reach (graphql_endpoint, API_VALUE_OPTS and FIELD_ARGS among
// them), against the old tag's; port each change to what they read as a read into
// ghPinnedCheckReads and the new sha256 into ghPinnedCheckPortSHA256 (both in
// gh_pinned_check_test.go); run INFRA-3562's xcheck4069 harness against it with 0 mismatching
// rows; then write its tag here and the sha256 of its public_leak_check.py, `git show
// <tag>:leak-check/public_leak_check.py | sha256sum` in the shared check's own repository.
var ghReadsProvenCheck = struct{ tag, sha256 string }{
	tag:    "leak-check-v18",
	sha256: "9c8aa217e9bade9e1f82ae59120f0a86b97be208375f1dde592b6c9da9fd61d7",
}

// ghRateLimitRead is the exact `gh api rate_limit`, a GitHub API call with no repository content,
// which reaches gh before its command is read: it reads no help and no git remote, so a working
// directory whose remotes name two owners does not refuse it. Any option changes the request's
// method or shape and is read as every other call is.
func ghRateLimitRead(argv []string) bool {
	return slices.Equal(argv, []string{"api", "rate_limit"})
}

// ghReadLeaves are the leaves that send GitHub no request that writes, by the canonical path the
// help of gh 2.98.0, the gh the worker image pins, names on their USAGE lines, each with the
// options a call of it may be given and still count as a read (ghReadGrammar): every option its
// help lists, by each of its spellings, but --help and --web. A call is read with the help of
// the gh LEGION_GH_PATH names and matched to this list by that path, so under another gh a leaf
// of the same name counts; the gate is the shared check alone (ghReadsProvenCheck), not gh's
// version, whose read would cost a gh process on every call. The path is also the spelling a
// call must be typed with to count as a read (ghWritesNothing: `pr ls` resolves to `pr list` and
// is guarded whole). Each reads and prints, or polls until a run or the checks end (`run watch`,
// `pr checks --watch`), and no option named here sends GitHub a write. One writes a file of gh's
// own: `run view --log` keeps the run's log zip in gh's cache directory, which sends GitHub
// nothing. Those that can open the page in a browser do it only under --web, which no leaf names
// here, so a call giving it in any spelling is guarded whole. A test
// (TestGhReadGrammarNamesEachOptionAsItsHelpDoes) holds every option named here to its help, and
// fails for one whose help names a browser. A leaf not here is guarded whole, every leaf a later
// gh adds among them, and so is a leaf that reads GitHub to write somewhere else of the role's:
// `pr checkout` and `repo clone` write the working directory, `run download` and `release
// download` the files they name, `browse` hands a URL to whatever browser runs.
var ghReadLeaves = map[string]map[string]bool{
	"pr view": ghReadOptions(
		ghReadRepo+ghReadJSON,
		"-c --comments"),
	"pr list": ghReadOptions(
		ghReadRepo+ghReadJSON+"--app -a --assignee -A --author -B --base -H --head -l --label -L --limit -S --search -s --state",
		"-d --draft"),
	"pr diff": ghReadOptions(
		ghReadRepo+"--color -e --exclude",
		"--allow-escape-sequences --name-only --patch"),
	"pr checks": ghReadOptions(
		ghReadRepo+ghReadJSON+"-i --interval",
		"--fail-fast --required --watch"),
	"pr status": ghReadOptions(
		ghReadRepo+ghReadJSON,
		"-c --conflict-status"),
	"run view": ghReadOptions(
		ghReadRepo+ghReadJSON+"-a --attempt -j --job",
		"--exit-status --log --log-failed -v --verbose"),
	"run list": ghReadOptions(
		ghReadRepo+ghReadJSON+"-b --branch -c --commit --created -e --event -L --limit -s --status -u --user -w --workflow",
		"-a --all"),
	"run watch": ghReadOptions(
		ghReadRepo+"-i --interval",
		"--compact --exit-status"),
	"issue view": ghReadOptions(
		ghReadRepo+ghReadJSON,
		"-c --comments"),
	"issue list": ghReadOptions(
		ghReadRepo+ghReadJSON+"--app -a --assignee -A --author -l --label -L --limit --mention -m --milestone -S --search -s --state --type",
		""),
	"issue status": ghReadOptions(
		ghReadRepo+ghReadJSON,
		""),
	"repo view": ghReadOptions(
		ghReadJSON+"-b --branch",
		""),
	"repo list": ghReadOptions(
		ghReadJSON+"-l --language -L --limit --topic --visibility",
		"--archived --fork --no-archived --source"),
	"release view": ghReadOptions(
		ghReadRepo+ghReadJSON,
		""),
	"release list": ghReadOptions(
		ghReadRepo+ghReadJSON+"-L --limit -O --order",
		"--exclude-drafts --exclude-pre-releases"),
	"workflow view": ghReadOptions(
		ghReadRepo+"-r --ref",
		"-y --yaml"),
	"workflow list": ghReadOptions(
		ghReadRepo+ghReadJSON+"-L --limit",
		"-a --all"),
	"search code": ghReadOptions(
		ghReadRepo+ghReadJSON+"--extension --filename --language -L --limit --match --owner --size",
		""),
	"search commits": ghReadOptions(
		ghReadRepo+ghReadJSON+"--author --author-date --author-email --author-name --committer --committer-date "+
			"--committer-email --committer-name --hash -L --limit --order --owner --parent --sort --tree --visibility",
		"--merge"),
	"search issues": ghReadOptions(
		ghReadRepo+ghReadJSON+"--app --assignee --author --closed --commenter --comments --created --interactions "+
			"--involves --label --language -L --limit --match --mentions --milestone --order --owner --project --reactions "+
			"--search-type --sort --state --team-mentions --updated --visibility",
		"--archived --include-prs --locked --no-assignee --no-label --no-milestone --no-project"),
	"search prs": ghReadOptions(
		ghReadRepo+ghReadJSON+"--app --assignee --author -B --base --checks --closed --commenter --comments --created "+
			"-H --head --interactions --involves --label --language -L --limit --match --mentions --merged-at --milestone "+
			"--order --owner --project --reactions --review --review-requested --reviewed-by --sort --state --team-mentions "+
			"--updated --visibility",
		"--archived --draft --locked --merged --no-assignee --no-label --no-milestone --no-project"),
	"search repos": ghReadOptions(
		ghReadJSON+"--created --followers --forks --good-first-issues --help-wanted-issues --include-forks --language "+
			"--license -L --limit --match --number-topics --order --owner --size --sort --stars --topic --updated --visibility",
		"--archived"),
	"label list": ghReadOptions(
		ghReadRepo+ghReadJSON+"-L --limit --order -S --search --sort",
		""),
	"cache list": ghReadOptions(
		ghReadRepo+ghReadJSON+"-k --key -L --limit -O --order -r --ref -S --sort",
		""),
}

// ghReadRepo and ghReadJSON are spellings of options many read leaves take, each a value's: the
// repository (-R/--repo, inherited by most, a filter of the search leaves), and the JSON output
// and its filters.
const (
	ghReadRepo = "-R --repo "
	ghReadJSON = "-q --jq -t --template --json "
)

// ghAPIReadOptions are the options a `gh api` read may be given (ghReadGrammar), none of which
// makes gh send anything but a GET of its endpoint: --jq/-q, --template/-t, --cache,
// -X/--method and -H/--header, each taking a value, and --paginate and --slurp. A method is
// admitted only as exactly GET, and a header only where it names no method override
// (ghAPIReadValue). INFRA-3562's spec v20 admits -X/--method GET and -H/--header only where the
// pinned check reads every spelling of them as a read too, and the generated spellings of
// TestLegionGhReadsOnlyWhatTheSharedCheckReadsAsARead hold that, against ghPinnedCheckReads.
var ghAPIReadOptions = ghReadOptions("-q --jq -t --template --cache -X --method -H --header", "--paginate --slurp")

// ghAPIReadValue says whether a `gh api` read admits value for option, beyond what
// ghReadGrammar asks of every value: a method only where it is exactly GET, and a header only
// where its name is no method override (ghMethodOverrideHeaders).
func ghAPIReadValue(option, value string) bool {
	switch option {
	case "-X", "--method":
		return value == "GET"
	case "-H", "--header":
		return !ghMethodOverrideHeaders[ghHeaderName(value)]
	}
	return true
}

// ghMethodOverrideHeaders are the request headers, lowercased, that some servers read as the
// request's method in place of the one it was sent with. This cannot know that GitHub reads none
// of them, so a call that sets one is guarded whole.
var ghMethodOverrideHeaders = map[string]bool{"x-http-method-override": true, "x-http-method": true, "x-method-override": true}

// ghHeaderName is the name of the request header value sets, as gh's -H/--header takes one,
// `<name>:<value>`: the text before the first colon, trimmed and lowercased, as
// ghMethodOverrideHeaders names a header.
func ghHeaderName(value string) string {
	name, _, _ := strings.Cut(value, ":")
	return strings.ToLower(strings.TrimSpace(name))
}

// ghReadOptions are a read grammar's options from two lists of spellings, each separated by
// spaces: values, the options that take a value, which are true, and switches, the options that
// stand alone, which are false.
func ghReadOptions(values, switches string) map[string]bool {
	options := map[string]bool{}
	for _, spelling := range strings.Fields(values) {
		options[spelling] = true
	}
	for _, spelling := range strings.Fields(switches) {
		options[spelling] = false
	}
	return options
}

// ghWritesNothing is the rule for a call that writes nothing, held here once and pointed to
// from everywhere else: whether the call, argv as typed and path the leaf its words resolved to,
// sends GitHub nothing that writes, runs no browser, and is read as a read by the shared check
// too, so that the check spends no visibility read on it either and refuses a credential in it.
//
// The shared check counts a call as a read by the words it is typed with (ghReadsProvenCheck's
// gh_guard): its first two, which must be a pair of its READ_TWO or a first word of its READ_ALL
// (is_read), every leaf of ghReadLeaves among them when spelled by its canonical path; or, for
// `gh api`, its own parse of the call's options (collect) and api_read over it. It refuses a
// credential in every call this counts as a read, and returns for such a call before it looks
// up any target. A call it does not count as a read goes on, after a match of a rule other than a
// credential's, to look up its target's visibility, a REST request in an hour that may have none
// to spare.
//
// So this admits a read by a grammar of the words as typed, which gh's parse and the check's
// read alike, and never by refusing the shapes the two were found to read otherwise
// (INFRA-3562's spec v20): argv begins with the leaf's canonical path, with no cobra
// alias (`pr ls`) and no option before the command (`-R o/r pr view`, which the check also reads
// as a read, and this does not, to keep one rule); the leaf is on ghReadLeaves or is `gh api`;
// and the words after the path are in that leaf's grammar (ghReadGrammar). Every other call
// writes, and is guarded whole: a read typed in another shape costs the visibility read and
// nothing more, which a role avoids by typing the shape admitted here. A command whose parse
// failed never reaches this. The rule holds for the check it was proven against, and the guard
// trusts it under that check alone (ghReadsProvenCheck).
func ghWritesNothing(argv, path []string) bool {
	if len(argv) < len(path) || !slices.Equal(argv[:len(path)], path) {
		return false
	}
	if slices.Equal(path, []string{"api"}) {
		return ghReadGrammar(argv[1:], ghAPIReadOptions, true)
	}
	options, listed := ghReadLeaves[strings.Join(path, " ")]
	return listed && ghReadGrammar(argv[len(path):], options, false)
}

// ghGraphQLReadOptions are the options a `gh api graphql` query may be given (ghGraphQLQueryRead),
// each spelling keyed as ghReadOptions keys one: -f/--raw-field, -F/--field, -H/--header, -q/--jq
// and -t/--template, each taking a value, and the switches --paginate and --slurp.
var ghGraphQLReadOptions = ghReadOptions("-f --raw-field -F --field -H --header -q --jq -t --template", "--paginate --slurp")

// ghGraphQLQueryRead says whether argv, a call as typed, is a query of GitHub's GraphQL API that
// writes nothing, which R4 of dispatch://INFRA-3399 lets run with GH_CONFIG_DIR's directory where
// it names only owners the bundle holds no directory for, as it lets a call ghWritesNothing admits
// (selectors.dir). It is no rule of the guard's: ghWritesNothing admits no `gh api graphql`, so the
// guard reads such a call whole.
//
// gh sends a call to its GraphQL API where the endpoint is exactly the word `graphql` (cli/cli
// v2.98.0 pkg/cmd/api/api.go, `isGraphQL := opts.RequestPath == "graphql"`), as a POST of its
// fields, the query among them. So argv begins with `api graphql`, no word before them, and every
// word after them is one of the options api.go's NewCmdApi defines that leave the request that
// POST, its value in the next word: -f/--raw-field and -F/--field, -H/--header naming no method
// override (ghMethodOverrideHeaders) and no Content-Length, which gh sets as the length of the
// body it sends (pkg/cmd/api/http.go), -q/--jq and -t/--template; or one of the switches
// --paginate and --slurp. No --input, whose file gh sends as the body, no -X/--method, no
// --hostname, no value joined to its option, no `--` and no other option.
//
// Every field is key=value, its key the text before the first `=`. gh's parseFields
// (pkg/cmd/api/fields.go) reads a `[` or a `]` in a key as nesting, and sets the request's query
// from any key whose first named part it reads as `query`: `query[]`, `query[x]`, `query]x` and
// `[query]` among them. Each part it reads is text of the key, so exactly one field's key is
// exactly `query`, and no other key holds the word. That field is raw (-f/--raw-field), since gh
// rewrites a typed field's value after this read it (magicFieldValue): it fills `{owner}`,
// `{repo}` and `{branch}`, and `:owner`, `:repo` and `:branch`, from the working directory's
// repository (fillPlaceholders), and reads a value beginning with `@` from a file or standard
// input, which no typed field's value here does. The query names no operation but a query
// (graphQLQueryOnly).
func ghGraphQLQueryRead(argv []string) bool {
	if len(argv) < 2 || argv[0] != "api" || argv[1] != "graphql" {
		return false
	}
	query, queries := "", 0
	for i := 2; i < len(argv); i++ {
		option := argv[i]
		takesValue, named := ghGraphQLReadOptions[option]
		if !named {
			return false
		}
		if !takesValue {
			continue
		}
		if i++; i == len(argv) {
			return false
		}
		value := argv[i]
		switch option {
		case "-f", "--raw-field", "-F", "--field":
			key, text, valued := strings.Cut(value, "=")
			typed := option == "-F" || option == "--field"
			switch {
			case !valued, typed && strings.HasPrefix(text, "@"):
				return false
			case key == "query" && !typed:
				query, queries = text, queries+1
			case strings.Contains(key, "query"):
				return false
			}
		case "-H", "--header":
			if name := ghHeaderName(value); ghMethodOverrideHeaders[name] || name == "content-length" {
				return false
			}
		}
	}
	return queries == 1 && graphQLQueryOnly(query)
}

// graphQLQueryOnly says whether text, a GraphQL document, names no operation but a query: read in
// one pass over GraphQL's lexical grammar (the GraphQL spec, "Lexical Tokens"), none of its name
// tokens is `mutation` or `subscription`, wherever it stands, so a field or an argument of that
// name refuses the document too, and it holds no string left open. A comment and a string are
// passed over as the pass meets them, never in a pass of their own, so a `#` inside a string
// starts no comment and a quote inside a comment starts no string. A comment runs from `#` to the
// next line terminator (\n, \r or \r\n); a string, from `"` to its closing quote past its escapes,
// is refused where a line terminator or the text's end comes first; a block string, from `"""` to
// the first `"""` not escaped as `\"""`, is refused where the text ends first. A name is
// `[_A-Za-z][_0-9A-Za-z]*`, and a number is read whole, its fraction and exponent with it, so a
// name right after a number, which the spec's lexer refuses and a looser one reads as a name,
// is read as that name (`1e5mutation` names `mutation`). Anything else, the punctuators, white
// space, commas and a byte order mark among them, is passed over.
func graphQLQueryOnly(text string) bool {
	digit := func(c byte) bool { return '0' <= c && c <= '9' }
	nameStart := func(c byte) bool { return c == '_' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' }
	nameRest := func(c byte) bool { return nameStart(c) || digit(c) }
	inLine := func(c byte) bool { return c != '\n' && c != '\r' }
	// at says whether text holds, at i, a byte that admits; past is the first index from i on
	// whose byte it does not admit.
	at := func(i int, admits func(byte) bool) bool { return i < len(text) && admits(text[i]) }
	past := func(i int, admits func(byte) bool) int {
		for at(i, admits) {
			i++
		}
		return i
	}
	for i := 0; i < len(text); {
		switch c := text[i]; {
		case c == '#':
			i = past(i, inLine)
		case strings.HasPrefix(text[i:], `"""`):
			end := i + 3
			for !strings.HasPrefix(text[end:], `"""`) {
				if end == len(text) {
					return false
				}
				if strings.HasPrefix(text[end:], `\"""`) {
					end += 3
				}
				end++
			}
			i = end + 3
		case c == '"':
			end := i + 1
			for end == len(text) || text[end] != '"' {
				if !at(end, inLine) {
					return false
				}
				if text[end] == '\\' && at(end+1, inLine) {
					end++
				}
				end++
			}
			i = end + 1
		case nameStart(c):
			end := past(i+1, nameRest)
			if name := text[i:end]; name == "mutation" || name == "subscription" {
				return false
			}
			i = end
		case c == '-' || digit(c):
			i = past(i+1, digit)
			if i+1 < len(text) && text[i] == '.' && digit(text[i+1]) {
				i = past(i+2, digit)
			}
			if i < len(text) && (text[i] == 'e' || text[i] == 'E') {
				exponent := i + 1
				if exponent < len(text) && (text[exponent] == '+' || text[exponent] == '-') {
					exponent++
				}
				if at(exponent, digit) {
					i = past(exponent, digit)
				}
			}
		default:
			i++
		}
	}
	return true
}

// ghReadGrammar says whether args, the words of a call after its leaf's path, are in the read
// grammar of a leaf that takes options (ghReadLeaves, ghAPIReadOptions). A word that begins with
// `-` is one of options' spellings, whole: so no `--name=value`, no value joined to a short option
// (`-q.x`, `-XGET`), no short options run together (`-cw`), no `--` and no option options does
// not name. An option that takes a value has it in the next word, which is not empty and does not
// begin with `-`. Every other word is an argument. For `gh api` (api) there is exactly one, its
// endpoint, a relative REST path (ghAPIRESTEndpoint), and each value passes ghAPIReadValue.
//
// gh's parse (pflag) and the shared check's (collect) read a call in this grammar alike: each
// option is a word both read as that option, each value is the word after it, which both take as
// its value, and so for `gh api` the one word left is the endpoint to both, with no field or
// input and no method but GET. The shapes left out are where they were found to differ: collect
// reads `--` as no end of the options, scans a single-dash word past its `=` for an option that
// takes a value (`-i=false` is `-f alse`), and, given an empty value, waits for the next word as
// the value again (`-q "" -q -XPOST` is a POST).
func ghReadGrammar(args []string, options map[string]bool, api bool) bool {
	arguments := 0
	for i := 0; i < len(args); i++ {
		word := args[i]
		if !strings.HasPrefix(word, "-") {
			arguments++
			if api && (arguments > 1 || word == "" || !ghAPIRESTEndpoint(word)) {
				return false
			}
			continue
		}
		takesValue, named := options[word]
		if !named {
			return false
		}
		if takesValue {
			i++
			if i == len(args) || args[i] == "" || strings.HasPrefix(args[i], "-") {
				return false
			}
			if api && !ghAPIReadValue(word, args[i]) {
				return false
			}
		}
	}
	return !api || arguments == 1
}

// ghAPIRESTEndpoint says whether gh api's endpoint is a relative REST path: no whole URL (any
// `://`, which the shared check reads as no read), and a path that, read up to its query or
// fragment and with its percent-escapes decoded, has no segment `graphql`, in any case, and no
// segment `.` or `..`, which a server may resolve into another path. `gh api graphql` is GitHub's
// GraphQL API, and `/graphql`, `GRAPHQL`, `%67raphql` and `graphql#x` are the same endpoint by
// other names. A REST path holding the word as a name (`repos/graphql/graphql-js`) is read the
// same way, which costs that call the guard's visibility read and nothing more, and so is a path
// whose escapes do not decode.
func ghAPIRESTEndpoint(endpoint string) bool {
	if strings.Contains(endpoint, "://") {
		return false
	}
	path := endpoint
	if end := strings.IndexAny(path, "?#"); end >= 0 {
		path = path[:end]
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == "." || segment == ".." || strings.EqualFold(segment, "graphql") {
			return false
		}
	}
	return true
}
