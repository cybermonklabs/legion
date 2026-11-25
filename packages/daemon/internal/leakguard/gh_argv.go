package leakguard

import (
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
)

// ghArgv is a gh call's argv as readGhArgv reads it: every answer GhCall needs of the argv, from
// one walk of it.
type ghArgv struct {
	// endpoint is a `gh api` call's endpoint as readEndpoint read it, and the zero ghEndpoint,
	// a REST endpoint with no text, for every other call and for a `gh api` call that names none.
	endpoint ghEndpoint
	// fieldsAsBody is whether gh sends the call's fields as its request body, one JSON document:
	// the call names a field option (ghFieldOptions), its body option (ghLeaf.bodyOption) names
	// no file and no standard input, and its method (ghLeaf.methodOption) is not GET. A `gh api`
	// call whose --input names either, or whose method is GET in any case, puts its fields in the
	// URL's query string instead (cmd/legion/testdata/gh-help/api.txt: "To send the parameters as
	// a `GET` query string instead, use `--method GET`").
	fieldsAsBody bool
	// records are the records of the argv, in the order it writes them.
	records []Record
	// sent is what the argv's options name for gh to send, in the order the argv writes them
	// (GhSent): the files, the use of standard input, whether it is sent and the fields whose
	// value it is; and unread is why GhCall refuses the call for an upload this reader does not
	// read, "" for none: an option of ghUnreadUploads, or a release asset it cannot find
	// (releaseUnread). These are the fallback, for a caller that hands the guard a bare argv
	// (GhRequest.Sent nil).
	sent   GhSent
	unread string
}

// ghEndpoint is a `gh api` call's endpoint, classified once (ghAPIEndpoint): its kind, which says
// how the call's body is read (jsonBody, stdinRecords) and whether its `query` fields are a
// GraphQL query (readLeaf), and its text as the argv wrote it, which GhCall's setup failure for
// fields sent as a body GitHub stores as sent names.
type ghEndpoint struct {
	kind ghEndpointKind
	text string
}

// ghEndpointKind is what GitHub makes of the body of a `gh api` call by the endpoint the call
// sends it to (ghAPIEndpoint), which says how the guard reads that body (jsonBody, stdinRecords).
type ghEndpointKind int

const (
	// ghEndpointREST is a path of GitHub's REST API, which parses a JSON body and stores its
	// strings apart (jsonFields). It is the zero value, the kind of every command but `gh api`
	// and of a `gh api` call that names no endpoint, so the run's inputs of `gh workflow run
	// --json` are read as such a body.
	ghEndpointREST ghEndpointKind = iota
	// ghEndpointGraphQL is GitHub's GraphQL endpoint, which parses the query a field or a body
	// carries and decodes its literals (graphQLRecords). A call to it is what this package's
	// comments call `gh api graphql`.
	ghEndpointGraphQL
	// ghEndpointStoredAsSent is an endpoint whose body GitHub stores as the bytes gh sends, a
	// release asset's upload among them, where the REST API would store a JSON body's strings
	// apart: two fields of the body on one line stand on that one line in what GitHub keeps.
	ghEndpointStoredAsSent
)

// readGhArgv reads args, a gh call's argv, once, for everything GhCall asks of it: the kind of
// its endpoint and whether gh sends its fields as its body, the records the rules read, the files
// it sends, the use it makes of standard input and the fields that read that input. One walk
// answers all of them, so that no two answers read one argv two ways: a reader that takes an
// option's value for an option of its own reads a field where another reads a title.
//
// The command is read first (ghLeafOf). Two commands are read by the arity of each of their
// options, `gh api` and `gh workflow run` (readLeaf), which are the two whose help gives them
// gh's field options; every other command is read with no arity but that of its file options
// (readOther).
//
// Every argument is a record of its own, "the gh call's argument <i>", i its index in args, which
// holds the argument as it was written, and no record joins two: a rule that needs two words on
// one line matches only where one argument holds both, and the joined argv would put every
// argument on one line. A field of `gh api` and `gh workflow run` is read as two records, its
// name and its value, in place of the argument that holds it (readLeaf), and `gh api`'s endpoint
// is read decoded as well, beside its own record (readEndpoint). Arguments past a bare `--` that
// ends gh's options are read as arguments, with no field among them, and name no file: gh's
// parser takes no option after one, so a `-f` or a `--body-file` there is a word of some other
// program's, and reading the words costs nothing.
func readGhArgv(args []string) ghArgv {
	argv := ghArgv{records: make([]Record, 0, len(args))}
	leaf, start := ghLeafOf(args)
	if leaf == nil {
		argv.readOther(args)
		return argv
	}
	for i := range start {
		argv.argument(args, i)
	}
	argv.readLeaf(args, leaf, start)
	return argv
}

// ghLeaf is a command of gh's whose options readGhArgv reads by their arity, as gh's own parser
// does: its path, and the long options and the letters of the short ones that take a value
// written as the argument after them. Every other option of it takes none, and a long option
// written `--name=value` holds its own. The rest of it is what readLeaf reads in one of these
// commands and not in the other, so that readLeaf asks the leaf and never which leaf it is.
type ghLeaf struct {
	path  []string
	long  map[string]bool
	short string
	// endpoint is whether the command's first positional argument is an endpoint, which says
	// how the call's body is read (readEndpoint): `gh api`'s.
	endpoint bool
	// bodyOption is the option whose value names the request body, a file or, as `-`, standard
	// input: `gh api`'s --input (inputOption). "" is a command with none, and no option.
	bodyOption string
	// stdinBodyOption is the boolean option that, true, makes standard input the request body:
	// `gh workflow run`'s --json (jsonInputsOption). "" is a command with none, and no option.
	stdinBodyOption string
	// methodOption and methodShort are the long and the short spelling of the option that names
	// the request's HTTP method, whose last value pflag keeps: `gh api`'s --method and -X. A GET
	// puts the call's fields in the URL's query string, and makes no body of them
	// (ghArgv.fieldsAsBody). "" is a command with none, and no option.
	methodOption, methodShort string
}

// ghLeaves are the commands of ghLeaf: `gh api` and `gh workflow run`, the two whose help gives
// them gh's field options (ghFieldOptions).
var (
	ghAPILeaf = ghLeaf{
		path: []string{"api"}, long: ghAPIValueOptions, short: ghAPIShortValueOptions,
		endpoint: true, bodyOption: inputOption, methodOption: "--method", methodShort: "-X",
	}
	ghWorkflowRunLeaf = ghLeaf{
		path: []string{"workflow", "run"}, long: ghWorkflowRunValueOptions, short: ghWorkflowRunShortValueOptions,
		stdinBodyOption: jsonInputsOption,
	}
	ghLeaves = []*ghLeaf{&ghAPILeaf, &ghWorkflowRunLeaf}
)

// ghAPIValueOptions are `gh api`'s long options that take a value, and ghAPIShortValueOptions the
// letters of its short ones, in gh 2.98.0's help (cmd/legion/testdata/gh-help/api.txt, which
// TestGhLeafOptionTablesAreTheirHelps holds them to). Its other options (`--include`,
// `--paginate`, `--silent`, `--slurp`, `--verbose`, `--allow-escape-sequences`, `--help`) take
// none.
var ghAPIValueOptions = map[string]bool{
	"--cache":     true,
	"--field":     true,
	"--header":    true,
	"--hostname":  true,
	"--input":     true,
	"--jq":        true,
	"--method":    true,
	"--preview":   true,
	"--raw-field": true,
	"--template":  true,
}

const ghAPIShortValueOptions = "FHXfpqt"

// ghWorkflowRunValueOptions are `gh workflow run`'s long options that take a value, and
// ghWorkflowRunShortValueOptions the letters of its short ones, in gh 2.98.0's help
// (cmd/legion/testdata/gh-help/workflow_run.txt, which TestGhLeafOptionTablesAreTheirHelps holds
// them to), the inherited -R/--repo among them. Its other options take none: `--help`, and
// `--json`, a boolean that makes standard input the run's inputs (jsonInputsOption, readLeaf).
var ghWorkflowRunValueOptions = map[string]bool{
	"--field":     true,
	"--raw-field": true,
	"--ref":       true,
	"--repo":      true,
}

const ghWorkflowRunShortValueOptions = "FRfr"

// ghFieldOptions are gh's field options, which `gh api` and `gh workflow run` have and no other
// command does, each true where its value is typed: a typed field's `@<path>` reads its value
// from the file, and its `@-` from standard input, where a raw field sends its value literally,
// `@` and all (`gh api -f body=@x` posts the two characters `@x` and opens no file).
var ghFieldOptions = map[string]bool{
	"-f":          false,
	"--raw-field": false,
	"-F":          true,
	"--field":     true,
}

// ghLeafOf is the command of ghLeaves args runs and the index of its first argument past the
// command's own words, or nil where args runs any other. The command is the words of args that
// are not options, read until they are a path of ghLeaves or a word leaves every path. -R and
// --repo, which gh takes before and between those words and `legion gh` does too (`-R <v>`,
// `-R<v>`, `--repo <v>`, `--repo=<v>`), are passed over with their value, and so is any other
// option there, `--help` and `-h` among them, which takes none. A bare `--` there ends gh's
// options before any command is named.
func ghLeafOf(args []string) (*ghLeaf, int) {
	var words []string
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--":
			return nil, 0
		case arg == "-R" || arg == "--repo":
			i++
		case len(arg) > 1 && arg[0] == '-':
		default:
			words = append(words, arg)
			further := false
			for _, leaf := range ghLeaves {
				if len(leaf.path) < len(words) || !slices.Equal(leaf.path[:len(words)], words) {
					continue
				}
				if len(leaf.path) == len(words) {
					return leaf, i + 1
				}
				further = true
			}
			if !further {
				return nil, 0
			}
		}
	}
	return nil, 0
}

// readLeaf reads args from start, the arguments of leaf, a command of ghLeaves, as gh's parser
// reads them. An option of leaf's table takes the next argument as its value, and that value is
// never read as an option of its own: `gh api -t '-fix <name>=<word>'` is a Go template, read
// whole. A short cluster is read as pflag reads one: the letters before the first that takes a
// value take none (`-i` is `--include`), and that letter ends the cluster, taking the rest of the
// argument, an `=` before it dropped, or the next argument where it is the last letter. So
// `-iF query=<v>`, `-iFquery=<v>` and `-if query=<v>` are fields, and a cluster whose value letter
// is any other (`-iX POST`) is no field.
//
// A field, the `<name>=<value>` given to a field option (ghFieldOptions), is two records: "the gh
// call's field <name>", which holds the value after the first `=`, and "the gh call's field
// <name> (name)", which holds the name (field). A call's fields are values GitHub stores apart —
// the title and the body of `gh api …/issues -f title=… -f body=…`, the inputs of a workflow run,
// a gist's file name in `-f 'files[<name>][content]=…'` — and are read apart for the reason
// arguments are. The value of every other option is the one record of the argument that holds it.
//
// A field's value is the literal string it is: gh does the JSON escaping, so a `\u0074` written in
// an argument reaches GitHub's REST API as those six characters, and the rules read what GitHub
// would show. The one field that is not is a GraphQL query, `query` in a `gh api graphql` call,
// whose literals GitHub's own parser decodes: it is read one literal at a time (queryRecords),
// once the endpoint, which may come after it, says the call is one. A `-F query=@<path>` reads the
// query from the file, which is read so (GhFile.Field), and a `-F query=@-` from standard input,
// which is read so too (stdinRecords); the field's own value is then the path or the `@-`, read as
// written.
//
// `gh api`'s endpoint (ghLeaf.endpoint) is its first positional argument, the first past the bare
// `--` where none comes before it; a `-` alone is a positional to pflag. It is read once, for its
// kind and for its path and query decoded (readEndpoint). Its `--input <path>`
// (ghLeaf.bodyOption) names the request body's file, and its `--input -` standard input
// (GhSent.Names). A field option named where the last --input names neither makes the fields the
// body gh sends (ghArgv.fieldsAsBody), but where the last -X or --method (ghLeaf.methodOption) is
// GET, in any case: a GET puts the fields in the URL's query string.
//
// `gh workflow run`'s `--json` (ghLeaf.stdinBodyOption) is a boolean, `--json` or
// `--json=<bool>`, whose last value is the one pflag keeps; a value that is no boolean leaves it
// as it was, since gh refuses that call itself. Where it ends true, gh reads the run's inputs from
// standard input as one JSON object, so after the walk standard input is named as a request body
// (GhSent.UsesStdin), by the rule of every other option that names it. The argument itself is
// read as any argument is.
func (argv *ghArgv) readLeaf(args []string, leaf *ghLeaf, start int) {
	// endpointNext is whether the next positional argument is the endpoint, which in a command
	// that has one it is until the endpoint is read; and queries are the indices in records of the
	// query fields' values, read as GraphQL where the call is.
	endpointNext, queries := leaf.endpoint, []int(nil)
	// stdinBody is whether the leaf's stdinBodyOption is true; fields is whether a field option is
	// named; body is whether the last value of the leaf's bodyOption names a file or standard
	// input; and method is the last value of its methodOption, "" for none. pflag keeps the last.
	var stdinBody, fields, body bool
	var method string
read:
	for i := start; i < len(args); i++ {
		arg := args[i]
		var option, value string
		var attached bool
		switch {
		case arg == "--":
			for at := i; at < len(args); at++ {
				argv.argument(args, at)
				if endpointNext && at > i {
					endpointNext = false
					argv.readEndpoint(args, at)
				}
			}
			break read
		case strings.HasPrefix(arg, "--"):
			option, value, attached = strings.Cut(arg, "=")
			if !attached && !leaf.long[option] {
				argv.argument(args, i)
				if option == leaf.stdinBodyOption {
					stdinBody = true
				}
				continue
			}
		case len(arg) > 1 && arg[0] == '-':
			letter := 1 + strings.IndexAny(arg[1:], leaf.short)
			if letter == 0 {
				argv.argument(args, i)
				continue
			}
			option, value, attached = "-"+arg[letter:letter+1], strings.TrimPrefix(arg[letter+1:], "="), letter+1 < len(arg)
		default:
			argv.argument(args, i)
			if endpointNext {
				endpointNext = false
				argv.readEndpoint(args, i)
			}
			continue
		}
		at := i
		if !attached {
			argv.argument(args, i)
			if i+1 == len(args) {
				// An option with no value at all: gh refuses the call itself.
				break
			}
			i++
			at, value = i, args[i]
		}
		if typed, field := ghFieldOptions[option]; field {
			fields = true
			if query := argv.field(args, at, option, value, typed); query >= 0 {
				queries = append(queries, query)
			}
			continue
		}
		argv.argument(args, at)
		// An option here is never "", which is the leaf's word for an option it does not have.
		switch option {
		case leaf.bodyOption:
			argv.sent.Names(option, value, "", StdinRequestBody)
			body = value != ""
		case leaf.stdinBodyOption:
			if on, err := strconv.ParseBool(value); err == nil {
				stdinBody = on
			}
		case leaf.methodOption, leaf.methodShort:
			method = value
		}
	}
	if stdinBody {
		argv.sent.UsesStdin(StdinRequestBody)
	}
	argv.fieldsAsBody = fields && !body && !strings.EqualFold(method, "GET")
	if argv.endpoint.kind == ghEndpointGraphQL {
		argv.records = spliceQueries(argv.records, queries)
	}
}

// readEndpoint reads args[i], a `gh api` call's endpoint (readLeaf), once (ghAPIEndpoint): its
// kind, kept with its text (ghArgv.endpoint), and the two parts of it GitHub reads decoded, each
// a record beside the endpoint's own (argument) where decoding changes its text. GitHub decodes a
// REST path, and a contents API path is a file's name, so the path decoded is "the gh call's
// argument <i>, path"; and it stores a release asset's `name` and `label` query values decoded, as
// the asset's file name and label, so each value of the query decoded is "the gh call's argument
// <i>, query <key>" (decodedQuery). A path that does not decode adds nothing, nor does a pair of
// the query that does not, and the endpoint's own record is read as ever. Every value of every
// endpoint's query is read so, which errs toward reading more: a value GitHub stores nowhere costs
// a record.
func (argv *ghArgv) readEndpoint(args []string, i int) {
	kind, decodedPath, query := ghAPIEndpoint(args[i])
	argv.endpoint = ghEndpoint{kind: kind, text: args[i]}
	if decodedPath == "" && query == "" {
		return
	}
	if decodedPath != "" {
		argv.records = append(argv.records, Record{Label: argumentLabel(i) + ", path", Text: decodedPath})
	}
	argv.records = decodedQuery(argv.records, i, query)
}

// ghAPIEndpoint reads endpoint, a `gh api` call's endpoint (its first positional argument,
// readLeaf), once, and answers its kind; its path with its percent-escapes decoded, where they
// decode and decoding changes the path, and "" otherwise; and its query as written, "" for none.
// The path and the query are what readEndpoint reads decoded beside the endpoint's own record.
//
// An endpoint holding `://` is a whole URL to gh, which sends the request there, and is parsed as
// one, and its host decides its kind. GitHub stores the body of a call to any host but
// api.github.com as gh sends it (ghEndpointStoredAsSent), a release asset's upload to
// `https://uploads.github.com/repos/<owner>/<repo>/releases/<id>/assets?name=<file>` among them;
// and so is the body of a URL that does not parse, the strict side, which has no path and no
// query to read either. The host is api.github.com on any port and in any case strings.EqualFold
// folds, which folds onto ASCII only `ſ` (U+017F) and the Kelvin sign (U+212A), neither of which
// the name holds. So `apİ.github.com` is another host: strings.ToLower lowers `İ` (U+0130) to `i`
// and would read it as api.github.com, and EqualFold does not. Any other endpoint is a path gh
// appends to the REST API's, read up to its query or fragment.
//
// The path then says whether the call is GraphQL, the path /graphql of api.github.com under
// whatever spelling the call gives it, read as a server reads a path: its percent-escapes decoded
// and its `.` and `..` segments resolved, what is left, its slashes trimmed, is `graphql` in any
// case. A path whose escapes do not decode is none.
//
// The endpoint is read by the path it reaches, not by gh's own rule, the exact word `graphql`.
// That rule is how gh groups the fields: for that word alone it puts `query` and `operationName`
// at the top of the request body and every other field under `variables`. Any other spelling is a
// REST path to gh, which POSTs the fields as one flat JSON body to that path; where the path is
// /graphql, GitHub's GraphQL endpoint parses the body's `query` member as the query all the same
// and decodes its literals (TestGhJSONGraphQLEndpointSpellingsReadOneFieldAtATime records the
// measurement), so a name escaped inside one reaches GitHub as the name.
//
// It errs toward reading no query for anything else. A call it does not find is read as written,
// its query one field among the others, as every query was before literals were read; a call it
// found that was none would have a REST field named `query` read as a GraphQL document, which
// leaves out the text outside its literals.
//
// cmd/legion's ghAPIRESTEndpoint reads the same endpoint for the other question, whether a call
// is a REST read, and names the same spellings — `/graphql`, `GRAPHQL`, `%67raphql`,
// `graphql#x` — as GitHub's GraphQL API by other names.
func ghAPIEndpoint(endpoint string) (kind ghEndpointKind, decodedPath, query string) {
	escaped := endpoint
	if strings.Contains(endpoint, "://") {
		whole, err := url.Parse(endpoint)
		if err != nil {
			return ghEndpointStoredAsSent, "", ""
		}
		if !strings.EqualFold(whole.Hostname(), "api.github.com") {
			kind = ghEndpointStoredAsSent
		}
		escaped, query = whole.RawPath, whole.RawQuery
		if escaped == "" {
			escaped = whole.EscapedPath()
		}
	} else {
		escaped, _, _ = strings.Cut(escaped, "#")
		escaped, query, _ = strings.Cut(escaped, "?")
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return kind, "", query
	}
	if kind == ghEndpointREST && strings.EqualFold(strings.Trim(path.Clean("/"+decoded), "/"), "graphql") {
		kind = ghEndpointGraphQL
	}
	if decoded == escaped {
		decoded = ""
	}
	return kind, decoded, query
}

// decodedQuery is records with a record of each value of query, as written in the endpoint that
// is the call's argument i, decoded, labelled "the gh call's argument <i>, query <key>", where
// decoding changes the value's text, in the order query writes them. It decodes as url.ParseQuery
// does, pair by pair: the pairs parted at `&`, a key from its value at the first `=`, and each
// read by url.QueryUnescape, so a `+` is a space. A pair holding `;`, which url.ParseQuery
// refuses as a separator, and one whose key or value does not unescape, are skipped, as
// url.ParseQuery skips them, and the other pairs are still read. url.ParseQuery's own answer is a
// map, which keeps neither the order of the pairs nor the text each value had.
func decodedQuery(records []Record, i int, query string) []Record {
	for pair := range strings.SplitSeq(query, "&") {
		if strings.Contains(pair, ";") {
			continue
		}
		rawKey, raw, _ := strings.Cut(pair, "=")
		key, keyErr := url.QueryUnescape(rawKey)
		value, err := url.QueryUnescape(raw)
		if keyErr != nil || err != nil {
			continue
		}
		if value != raw {
			records = append(records, Record{Label: argumentLabel(i) + ", query " + key, Text: value})
		}
	}
	return records
}

// spliceQueries is records with the record at each index of queries, ascending, replaced by the
// records of the GraphQL query it holds (queryRecords): the values of a `gh api graphql` call's
// query fields (readLeaf), which are queries once the endpoint, which may come after them, says
// the call is one.
func spliceQueries(records []Record, queries []int) []Record {
	if len(queries) == 0 {
		return records
	}
	spliced := make([]Record, 0, len(records))
	for k, record := range records {
		if len(queries) > 0 && queries[0] == k {
			queries = queries[1:]
			spliced = append(spliced, queryRecords(record)...)
			continue
		}
		spliced = append(spliced, record)
	}
	return spliced
}

// field reads value, the value of the field option option held by args[at] (readLeaf), typed
// where option is -F or --field, and answers the index in records of the field's value where that
// value is a field named query, which is the query should the call be `gh api graphql`, and -1
// where it is not. A value with no `=` (`-f <name>`, gh's empty array `key[]`) is the argument,
// read whole. A typed field whose value is `@<path>` names a file gh reads the value from, and
// `@-` standard input (GhSent.Names); such a field's value is the `@<path>` as written, and no
// query.
func (argv *ghArgv) field(args []string, at int, option, value string, typed bool) int {
	name, text, found := strings.Cut(value, "=")
	if !found {
		argv.argument(args, at)
		return -1
	}
	label := "the gh call's field " + name
	argv.records = append(argv.records, Record{Label: label + " (name)", Text: name})
	valueAt := len(argv.records)
	argv.records = append(argv.records, Record{Label: label, Text: text})
	if path, fromFile := strings.CutPrefix(text, "@"); typed && fromFile {
		argv.sent.Names(option, path, name, StdinField)
		return -1
	}
	if name != graphQLQuery {
		return -1
	}
	return valueAt
}

// readOther reads args, the argv of a command none of ghLeaves is. Each argument is one record,
// read whole, whatever it looks like: -f and -F are no field options there and mean other things
// — `-F` is `--body-file` in `gh pr create`, `-f` is `--force` in `gh label create` — so a title
// written `-fix <name>=<word>` is a title, which a reader taking it for a field would part into
// two records that a rule needing two words reads apart.
//
// The files it names are the values of ghFileOptions, each in its separate and its attached
// spelling (`--body-file=<path>`), and of `-F <path>`, `-F<path>` and `-F=<path>`, the last as
// pflag reads a short option. gh gives -F to --body-file, --notes-file and --from-file, and outside
// `gh api` and `gh workflow run` to nothing else (`grep -ho -- '-F, --[a-z-]*'
// testdata/gh-help/*.txt | sort -u`), so a -F is a file option whichever of the three the command
// has. A `-F` anywhere but the front of a short argument is not read: it is the value of the
// option before it in `gh gist create` (`-dF` is `--desc F`), and reading it as a file would
// refuse a call over a file nobody named, while in `gh pr create` `-dF` is `--draft --body-file`,
// which only the leaf's own help can say, and `legion gh` reads it so (GhRequest.Sent).
//
// No other option's arity is known here but that of -R/--repo, the repository every command
// takes, so an option's value that itself begins with `-F` (a `--body` whose first word is
// `-Fixed`) is read as a -F of its own and refuses the call, naming a file nobody meant; and so is
// a value that is itself `--attach`, which is read as the upload (ghUnreadUploads). That is the
// direction this errs in: the other way round is `gh release create -F notes.md`, the documented
// short spelling, going to GitHub unread.
//
// A release asset is a positional argument, and with no arity but those this cannot tell one
// from another option's value (`--notes <text>`), so it does not look for one: a call that may
// upload one is refused (releaseUnread), and `legion gh`, whose parse knows the leaf's positional
// arguments, names each asset (GhSent.Asset). For that, the words of the argv are the arguments
// that are no option: the command's path and then its positional arguments, with the values of
// -R/--repo and of the file options passed over, and every argument past a bare `--`.
func (argv *ghArgv) readOther(args []string) {
	// words are the argv's words, as many of them as releaseUnread reads.
	var words []string
	word := func(arg string) {
		if len(words) < releaseWords {
			words = append(words, arg)
		}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			for at := i; at < len(args); at++ {
				argv.argument(args, at)
				if at > i {
					word(args[at])
				}
			}
			break
		}
		argv.argument(args, i)
		name, after, found := strings.Cut(arg, "=")
		if ghUnreadUploads[name] && argv.unread == "" {
			argv.unread = name + " uploads the file it names, which this guard does not read, so the call sends nothing; write the text in --body or --body-file, which it reads"
		}
		var option, value string
		var attached bool
		switch {
		case strings.HasPrefix(arg, "-F"):
			option, value, attached = "-F", strings.TrimPrefix(arg[2:], "="), len(arg) > 2
		case ghFileOptions[name]:
			option, value, attached = name, after, found
		case arg == "-R" || arg == "--repo":
			// The repository, which every command takes, and no positional argument.
			if i+1 < len(args) {
				i++
				argv.argument(args, i)
			}
			continue
		case len(arg) > 1 && arg[0] == '-':
			continue
		default:
			word(arg)
			continue
		}
		if !attached {
			if i+1 == len(args) {
				// An option with no value at all: gh refuses the call itself, and there is no
				// file here to read.
				break
			}
			i++
			argv.argument(args, i)
			value = args[i]
		}
		argv.sent.Names(option, value, "", StdinText)
	}
	if argv.unread == "" {
		argv.unread = releaseUnread(words)
	}
}

// releaseWords is how many words of an argv (readOther) releaseUnread reads: the command's path,
// `release create`, its tag, and whether a word comes after it.
const releaseWords = 4

// releaseUnread is why the fallback refuses a call that may upload a release asset, from words,
// the call's command path and positional arguments as readOther finds them: every `gh release
// upload <tag> <files>...`, and a `gh release create [<tag>] [<files>...]` (or `gh release new`,
// its alias) with a word past the tag, which may be an asset or another option's value; and "" for
// every other call. gh 2.98.0 takes the first positional argument of either as the tag and
// uploads each one after it as an asset (cli/cli v2.98.0 pkg/cmd/release/upload/upload.go and
// create/create.go: `opts.TagName = args[0]`, `shared.AssetsFromArgs(args[1:])`).
func releaseUnread(words []string) string {
	if len(words) < 2 || words[0] != "release" {
		return ""
	}
	if command := words[1]; command == "upload" || (command == "create" || command == "new") && len(words) > 3 {
		return "`gh release " + command + "` uploads each argument after the tag as a release asset, which this guard cannot tell from another option's value in a bare argv, so the call sends nothing; run it through legion gh, which reads each asset"
	}
	return ""
}

// GhSent is what a gh call's options name for gh to send to GitHub beside its argv: the files it
// sends as content, the use it makes of standard input, whether it sends that input at all, and
// the fields whose value it is. Its methods are the one rule for what a value an option names is
// (Names, UsesStdin): a path a file named once, `-` standard input with the use its option makes
// of it, and two uses that differ no known use; and for what a release asset's path names
// (Asset): a file, `-` among them, named once. Both readers of a gh call keep to it: this
// package's own, over a bare argv in the argv's order (readGhArgv), and cmd/legion's, over its
// parse of the command by gh's help, in the order the options' names sort (its ghSentFiles). So
// the two name what one option's value sends the same way. Only cmd/legion names release assets:
// the bare argv's reader cannot find them, and refuses a call that may upload one
// (releaseUnread).
//
// Finding the values is each reader's own work — which options a call names, and the `<path>` of
// a field's `<name>=@<path>` — and the two read some calls apart: `gh pr create -dF <path>` is
// `--draft --body-file <path>` to cmd/legion, which reads the leaf's help, and no file to this
// package's reader, which cannot tell it from `gh gist create`'s `--desc F` (readOther). What a
// call whose caller handed its parse sends is that parse's alone (GhRequest.Sent), so this
// package's reading decides it only where no caller parsed the call.
//
// The zero GhSent has named nothing, which is the parse of a call that sends nothing beside its
// argv.
type GhSent struct {
	// Files are the files named, each once, in the order they were named.
	Files []GhFile
	// stdinUse is the use the call makes of standard input: the first use named, and
	// StdinUnknown where none is or where a second that differs from it is (UsesStdin).
	stdinUse StdinUse
	// stdinNamed is whether any use of standard input is named, which tells a first use from a
	// second.
	stdinNamed bool
	// stdinFields are the names of the fields whose value is standard input, each `-F <name>=@-`
	// and `--field <name>=@-`, in the order they were named, by which the guard reads that input
	// (stdinRecords). A -f or --raw-field sends `@-` as those two characters and reads no input,
	// so it names none.
	stdinFields []string
}

// Names reads path, the value of option where it names a file gh sends (field, the name of the
// field the file's text is the value of, "" for any other option): standard input, used as use
// says, where path is `-`; nothing where it is empty; and a file to send for any other path,
// which is named once, since a second record of one file is a second open and a second line of
// the same report.
//
// Standard input is no file to open: GhCall reads it as GhRequest.Sent says. What is kept is that
// it is named (StdinSent), the use the option makes of it (UsesStdin), and the field's name.
func (sent *GhSent) Names(option, path, field string, use StdinUse) {
	switch path {
	case "":
	case "-":
		if field != "" {
			sent.stdinFields = append(sent.stdinFields, field)
		}
		sent.UsesStdin(use)
	default:
		sent.file(GhFile{Option: option, Path: path, Field: field})
	}
}

// Asset names path, a file gh uploads as a release asset (ReleaseAsset), with label, the asset's
// label where no argument holds it (GhFile.Label), once, as Names names a file; "" names nothing.
// A path of `-` is the file of that name and not standard input: gh opens each asset by its path
// (cli/cli v2.98.0 pkg/cmd/release/shared/upload.go, AssetsFromArgs).
func (sent *GhSent) Asset(path, label string) {
	if path != "" {
		sent.file(GhFile{Option: ReleaseAsset, Path: path, Label: label})
	}
}

// file names named, unless a file of its path is named already (Names, Asset).
func (sent *GhSent) file(named GhFile) {
	if !slices.ContainsFunc(sent.Files, func(file GhFile) bool { return file.Path == named.Path }) {
		sent.Files = append(sent.Files, named)
	}
}

// UsesStdin names standard input with use: for an option's `-` (Names), and on its own for an
// option that makes standard input an input with no path to name it by, `gh workflow run --json`,
// whose true makes it the run's inputs (StdinRequestBody). Two that name it with uses that differ
// leave its use StdinUnknown, in whichever order they come, since only one of them can have the
// input, and the guard then reads it both ways.
func (sent *GhSent) UsesStdin(use StdinUse) {
	switch {
	case !sent.stdinNamed:
		sent.stdinUse, sent.stdinNamed = use, true
	case sent.stdinUse != use:
		sent.stdinUse = StdinUnknown
	}
}

// StdinSent is whether any use of standard input is named (UsesStdin): whether the call sends its
// standard input to GitHub as content, which decides whether GhCall reads it (GhRequest.Sent).
func (sent *GhSent) StdinSent() bool {
	return sent.stdinNamed
}

// argument records args[i], as it was written, as "the gh call's argument <i>" (argumentLabel).
func (argv *ghArgv) argument(args []string, i int) {
	argv.records = append(argv.records, Record{Label: argumentLabel(i), Text: args[i]})
}

// argumentLabel is the label of the record of a gh call's argument i, "the gh call's argument
// <i>", and the start of the label of each record readEndpoint reads from that argument decoded.
func argumentLabel(i int) string {
	return "the gh call's argument " + strconv.Itoa(i)
}

// GhFile is one file a gh call would send as content: the option that named it, which a setup
// failure names, or ReleaseAsset for a file no option names; the path the caller named, as the
// argv wrote it or as a file a release asset's glob pattern matched, which is the record's label;
// and, for a file a field names (`--field <name>=@<path>`, `-F <name>=@<path>`), the field's name,
// "" for every other option. The name is what says that a file is a GraphQL query, `query` in a
// `gh api graphql` call, which is read one literal at a time (graphQLRecords).
type GhFile struct {
	Option, Path, Field string
	// Label is a release asset's label (ReleaseAsset) that no argument of the call holds: the
	// text after the `#` in the name of a file a glob pattern matched, which gh uploads as the
	// asset's label and GitHub shows (cmd/legion's ghReleaseAssets). It is "" for every other
	// file, and for an asset whose label, if any, is in the argument that named it.
	Label string
}

// ReleaseAsset is the Option of a GhFile gh uploads as a release asset: a positional argument of
// `gh release upload` or `gh release create` past the tag, which no option names (GhSent.Asset).
// GitHub publishes an asset as the bytes gh sends and names it by the file's own name, so GhCall
// reads it as it reads a body GitHub stores as sent (rawAndJSON), and its name beside it; a setup
// failure calls it the file gh uploads as a release asset (readGhFile). The asset's label, the
// text after a `#` (`<file>#<label>`), is text GitHub shows too: an argument's own record holds
// it, and the label of a file a glob pattern matched, which no argument holds, is its GhFile's
// Label, which GhCall reads too.
const ReleaseAsset = "a release asset"

// inputOption is `gh api`'s request body, the one option whose file is itself the body, read as
// the call's endpoint says (jsonBody), and one of the two whose standard input is (stdinRecords).
const inputOption = "--input"

// jsonInputsOption is the other: `gh workflow run --json`, a boolean that makes standard input
// the run's inputs as one JSON object (readLeaf). It names no file. In every other gh command
// `--json` takes the names of the fields to print, and makes no input of standard input.
const jsonInputsOption = "--json"

// ghFileOptions are the options of the commands readOther reads whose whole value is the path of
// a file gh sends to GitHub as content, in gh 2.98.0's spelling — the version the worker image
// pins, whose help is cmd/legion/testdata/gh-help, which is where each of these was read. A gh
// call carries its text in a file as readily as in an argument, so each file's text is a record of
// its own beside the records of the argv, which hold the file's name and never its text. `gh
// api`'s files are readLeaf's: its --input, and the `@<path>` of a typed field, which `gh
// workflow run` reads too — `gh api --method PATCH repos/<owner>/<repo>/pulls/<n> -F
// body=@body.md` is how a Legion role writes a pull request's body.
//
// These are the fallback, for a caller that hands the guard a bare argv (GhRequest.Sent nil).
// The caller that can do better does: `legion gh` parses the command with gh's own help of the
// leaf, so it knows every option's arity and reads a shape no table can — a short cluster whose
// last letter is the option (`-dF <path>`, which is `--draft --body-file <path>` in `gh pr
// create` and `--desc F` in `gh gist create`) — and hands the files it found.
//
// Four things are deliberately left out:
//
//   - `-f`/`--raw-field`, which sends its value literally (ghFieldOptions) and opens no file.
//   - `--env-file` (`gh secret set`, `gh variable set`), whose values are secrets GitHub stores
//     and never publishes. A secret is the shared check's to read, not the private-name rules',
//     which are names a deployment keeps out of public repositories.
//   - `--template`, which names a template of the repository (`gh pr create --template
//     "pull_request_template.md"`) or a Go template for gh's own output (`-t, --template
//     string`), neither of them a file the command sends.
//   - `gh gist create <filename>…`, whose positional arguments are files gh publishes, and
//     every other positional file but a release asset (ReleaseAsset, readOther): `gh alias
//     import <filename>`. Reading a positional would mean a copy of gh's grammar in here. An
//     option's name says what its value is in every command that carries it, while a
//     positional's meaning is the leaf's — a local file in `gist create`, a name inside the gist
//     in `gist edit <id> <filename>`, a number in `pr comment <number>` — and this reads argv
//     with no gh to ask, as cmd/legion's own reader asks gh's help. Half a copy would read as
//     cover where there is none: `gist edit --add <path>` publishes a file too, and its short
//     `-a` is `--all` in other commands, so it cannot be added to the table above. What these
//     files keep is the shared check, which reads every argument of the call. No Legion role
//     writes a gist: the roles post pull request and issue text, which the options above carry.
var ghFileOptions = map[string]bool{
	// The body of a pull request, an issue, a comment, a review or a revert. Twelve commands of
	// the pinned reference carry it: `awk '/^### /{cmd=$0} /--body-file/{print cmd}'
	// testdata/gh-help/reference.txt | sort -u`.
	"--body-file": true,
	// `gh release create` and `gh release edit`'s release notes.
	"--notes-file": true,
	// `gh agent-task create`'s task description.
	"--from-file": true,
}

// ghUnreadUploads are the options whose value names a file gh uploads to GitHub that this
// package does not read: gh 2.104.0's --attach, of `gh issue` and `gh pr` comment, create and
// edit and of `gh issue artifact create` and `edit`, which uploads an image (an SVG's text among
// them, up to 10 MiB) or a video (up to 100 MiB) as an asset GitHub serves. It has no short
// spelling, and `gh api` and `gh workflow run` have none of them. The fallback refuses a call
// naming one, in its separate or its attached spelling (`--attach=<path>`), before a bare `--`
// (ghArgv.unread), rather than read a file of that kind and size; `legion gh`, which hands the
// guard its parse (GhRequest.Sent), refuses it before the guard runs.
var ghUnreadUploads = map[string]bool{"--attach": true}
