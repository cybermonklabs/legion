package ghroutetest

import (
	"maps"
	"slices"
)

// ReadLeafCalls is one call of each leaf ghroute's read rule lists (its readLeaves), keyed by that
// leaf, each naming acme/widgets, or its owner alone for `repo list` and `search repos`, the way
// the leaf's help takes it. ghroute's TestGhReadLeafCallsCoverEveryReadLeaf fails while a leaf of
// the list has no call here, so a leaf added to the list is run as a read by cmd/legion's
// TestLegionGhChecksAReadAndReadsNoVisibility.
var ReadLeafCalls = map[string][]string{
	"pr view":        {"pr", "view", "1", "-R", "acme/widgets"},
	"pr list":        {"pr", "list", "-R", "acme/widgets"},
	"pr diff":        {"pr", "diff", "1", "-R", "acme/widgets"},
	"pr checks":      {"pr", "checks", "1", "-R", "acme/widgets"},
	"pr status":      {"pr", "status", "-R", "acme/widgets"},
	"run view":       {"run", "view", "5", "-R", "acme/widgets"},
	"run list":       {"run", "list", "-R", "acme/widgets"},
	"run watch":      {"run", "watch", "5", "-R", "acme/widgets"},
	"issue view":     {"issue", "view", "1", "-R", "acme/widgets"},
	"issue list":     {"issue", "list", "-R", "acme/widgets"},
	"issue status":   {"issue", "status", "-R", "acme/widgets"},
	"repo view":      {"repo", "view", "acme/widgets"},
	"repo list":      {"repo", "list", "acme"},
	"release view":   {"release", "view", "v1", "-R", "acme/widgets"},
	"release list":   {"release", "list", "-R", "acme/widgets"},
	"workflow view":  {"workflow", "view", "ci.yaml", "-R", "acme/widgets"},
	"workflow list":  {"workflow", "list", "-R", "acme/widgets"},
	"search code":    {"search", "code", "widget", "--repo", "acme/widgets"},
	"search commits": {"search", "commits", "widget", "--repo", "acme/widgets"},
	"search issues":  {"search", "issues", "repo:acme/widgets", "is:open"},
	"search prs":     {"search", "prs", "widget", "--repo", "acme/widgets"},
	"search repos":   {"search", "repos", "widget", "--owner", "acme"},
	"label list":     {"label", "list", "-R", "acme/widgets"},
	"cache list":     {"cache", "list", "-R", "acme/widgets"},
}

// ReadLeafSpellings are reads ReadLeafCalls does not spell, each in its leaf's grammar (ghroute's
// readLeaves): a -w that is no --web (`run list`'s is --workflow), the JSON options with a
// filter, a leaf's switches among its options that take a value, and a search's filters.
var ReadLeafSpellings = [][]string{
	{"run", "list", "-R", "acme/widgets", "-w", "ci.yaml"},
	{"pr", "view", "1", "--repo", "acme/widgets", "--json", "title,body", "-q", ".title"},
	{"pr", "checks", "1", "-R", "acme/widgets", "--watch", "-i", "5", "--fail-fast"},
	{"issue", "list", "-R", "acme/widgets", "-s", "open", "-l", "bug", "-L", "5", "--json", "number"},
	{"search", "issues", "--repo", "acme/widgets", "--state", "open", "--include-prs", "--json", "number"},
}

// GuardedLeafSpellings are calls of listed leaves that write nothing, typed outside their leaf's
// grammar and so guarded whole: a value after `=` or joined to its short option, an empty value
// and one beginning with `-`, short options run together, `--` before an argument, a boolean
// given a value, --web=false among them, which opens no browser, and --web, which no grammar
// names, in each spelling: --web and -w alone, -w last in a short cluster (`-cw`), each given a
// value, a value that is no boolean among them, and --web after a --web=false.
var GuardedLeafSpellings = [][]string{
	{"pr", "view", "1", "--repo=acme/widgets"},
	{"pr", "view", "1", "-Racme/widgets"},
	{"issue", "list", "-R", "acme/widgets", "--search", ""},
	{"issue", "list", "-R", "acme/widgets", "--search", "-label:bug"},
	{"pr", "view", "1", "-R", "acme/widgets", "-cq", ".title"},
	{"search", "issues", "--repo", "acme/widgets", "--", "is:open"},
	{"pr", "checks", "1", "-R", "acme/widgets", "--watch=true"},
	{"pr", "view", "1", "-R", "acme/widgets", "--web=false"},
	{"issue", "list", "-R", "acme/widgets", "-w", "--web=0"},
	{"search", "code", "widget", "--web"},
	{"pr", "view", "1", "-R", "acme/widgets", "--web"},
	{"repo", "view", "-w"},
	{"pr", "view", "1", "-R", "acme/widgets", "-cw"},
	{"issue", "list", "-R", "acme/widgets", "--web=true"},
	{"label", "list", "-R", "acme/widgets", "-w=1"},
	{"release", "view", "v1", "-R", "acme/widgets", "--web=false", "--web"},
	{"workflow", "view", "ci.yaml", "-R", "acme/widgets", "--web=maybe"},
}

// APIReadSpellings are gh api options in the grammar of a read (ghroute's apiReadOptions), which
// leave a GET of a relative REST path a read: none; each option alone, by each of its spellings,
// its value in the word after it, the method exactly GET; and several together.
var APIReadSpellings = [][]string{
	nil,
	{"--paginate"}, {"--paginate", "--slurp"}, {"--jq", ".[].number"}, {"-q", ".x"},
	{"--template", "{{.}}"}, {"-t", "{{.}}"}, {"--cache", "1h"},
	{"-X", "GET"}, {"--method", "GET"}, {"-H", "Accept: application/vnd.github+json"}, {"--header", "Accept: application/json"},
	{"--paginate", "-q", ".[].number", "--cache", "60m"},
	{"-X", "GET", "-H", "Accept: application/json", "--jq", ".[].number"},
}

// APIReadEndpoints are relative REST paths of acme/widgets: plain, with a leading slash, with a
// query, and with a percent-escape.
var APIReadEndpoints = []string{
	"repos/acme/widgets/pulls", "/repos/acme/widgets/pulls", "repos/acme/widgets/issues?state=open", "repos/acme/widgets/pull%73",
}

// APIGuardedSpellings are gh api options outside the grammar of a read, which make a call with
// any endpoint no read. First the shapes the shared check's collect was found to read otherwise
// than pflag (review 5911414081 and the two before it): `-i=` with each value ParseBool reads as
// false, and any other single-dash word holding `=`; `--`; and an empty value, after which
// collect takes the next word as the value again (`-q "" -q -XPOST` is a POST to it). Then a
// value joined to its option, after `=` or to a short option, and one empty or beginning with
// `-`; a method GET in any spelling but `-X GET` and `--method GET`; options that leave the
// request a GET but no read grammar names (-i, --silent, --hostname and -p); a method other than
// GET in each spelling, and a GET given a second method; a field or a raw field in each spelling,
// the last of a short cluster among them; a GET with fields; a body on --input; and a header
// naming each method override.
var APIGuardedSpellings = [][]string{
	{"-i=false"}, {"-i=f"}, {"-i=F"}, {"-i=FALSE"}, {"-i=False"}, {"-X=GET"}, {"--"},
	{"-q", "", "-q", "-XPOST"}, {"--jq", "", "--jq", "graphql"}, {"-q", ""}, {"--jq", ""}, {"-t", ""}, {"--cache", ""},
	{"--jq=.x"}, {"-q.x"}, {"-iq", ".x"}, {"--jq", "-.x"},
	{"-H", ""}, {"-H", "-x"}, {"--header", ""}, {"-X", ""}, {"--header=Accept: application/json"},
	{"-X", "get"}, {"--method=GET"}, {"--method=get"}, {"-XGET"},
	{"-i"}, {"--silent"}, {"--hostname", "github.com"}, {"--hostname=GITHUB.COM"}, {"-p", "corsair"},
	{"-X", "POST"}, {"-XPOST"}, {"--method=PATCH"}, {"--method", "HEAD"}, {"-X", "GET", "-X", "DELETE"},
	{"-f", "x=y"}, {"-fk=v"}, {"--raw-field=k=v"}, {"-F", "k=v"}, {"--field=k=v"}, {"-iF", "k=v"},
	{"--method", "GET", "-f", "state=open"}, {"--input", "-"}, {"--input=-"},
	{"-H", "X-HTTP-Method-Override: POST"}, {"-H", "X-HTTP-Method: POST"}, {"-H", "X-Method-Override: POST"},
}

// APIGuardedEndpoints are endpoints that make a call with any options no read: a whole URL,
// GitHub's GraphQL API by each of its names, and paths whose `.` or `..` segments a server may
// resolve to another.
var APIGuardedEndpoints = []string{
	"https://api.github.com/repos/acme/widgets/pulls", "graphql", "/graphql", "GRAPHQL", "%67raphql",
	"https://api.github.com/graphql", "repos/acme/widgets/./pulls", "repos/acme/widgets/pulls/../../../../graphql",
}

// APICalls is the cross product of options and endpoints as `gh api` calls: each endpoint with
// each option, the option before the endpoint and, where there is one, after it.
func APICalls(options [][]string, endpoints []string) [][]string {
	var calls [][]string
	for _, endpoint := range endpoints {
		for _, option := range options {
			calls = append(calls, slices.Concat([]string{"api"}, option, []string{endpoint}))
			if len(option) > 0 {
				calls = append(calls, slices.Concat([]string{"api", endpoint}, option))
			}
		}
	}
	return calls
}

// ReadCalls are the calls that write nothing which the tests run: ReadLeafCalls in the order of
// their leaves, ReadLeafSpellings, and every gh api read of APIReadSpellings and
// APIReadEndpoints.
func ReadCalls() [][]string {
	var calls [][]string
	for _, leaf := range slices.Sorted(maps.Keys(ReadLeafCalls)) {
		calls = append(calls, ReadLeafCalls[leaf])
	}
	calls = append(calls, ReadLeafSpellings...)
	return append(calls, APICalls(APIReadSpellings, APIReadEndpoints)...)
}

// GuardedAPICalls are the gh api calls the tests of `legion gh` run whole: every option of
// APIGuardedSpellings before one read endpoint, every endpoint of APIGuardedEndpoints with no
// option, and `--` before a word collect reads as a method. ghroute's
// TestGhWritesNothingReadsTheLeafAndTheMethod reads the whole cross product, each option before
// and after each endpoint.
func GuardedAPICalls() [][]string {
	calls := [][]string{{"api", "--", "-XPOST"}}
	for _, option := range APIGuardedSpellings {
		calls = append(calls, slices.Concat([]string{"api"}, option, []string{APIReadEndpoints[0]}))
	}
	for _, endpoint := range APIGuardedEndpoints {
		calls = append(calls, []string{"api", endpoint})
	}
	return calls
}

// GuardedCalls are calls of commands other than gh api that the guard still reads whole: the
// writes a role makes (`pr create`, `pr comment`, `pr edit`, `issue comment`, `release create`); a
// write of every family of commands ghroute's readLeaves reads from but search, which has none
// (`label create`, `issue create`, `issue close`, `pr merge`, `pr review`, `release delete`,
// `repo edit`, `cache delete`, `run rerun`, `workflow run`), so a write added to the list beside
// its family's reads fails ghroute's TestGhReadLeafCallsCoverEveryReadLeaf; a leaf readLeaves
// does not list, although it writes nothing to GitHub (`browse`); and a listed read typed
// otherwise than by its canonical path, by an alias (`pr ls`) or after an option (`-R o/r pr
// view`). The gh api calls are APICalls' cross products. (`--hostname` naming another host than
// github.com is refused before the guard, exit 2, as on main.)
var GuardedCalls = [][]string{
	{"pr", "create", "-R", "acme/widgets", "--title", "a title", "--body", "a body"},
	{"pr", "comment", "1", "-R", "acme/widgets", "--body", "a comment"},
	{"pr", "edit", "1", "-R", "acme/widgets", "--title", "a title"},
	{"issue", "comment", "1", "-R", "acme/widgets", "--body", "a comment"},
	{"release", "create", "v1", "-R", "acme/widgets", "--notes", "the notes"},
	{"label", "create", "bug", "-R", "acme/widgets"},
	{"issue", "create", "-R", "acme/widgets", "--title", "a title", "--body", "a body"},
	{"issue", "close", "1", "-R", "acme/widgets"},
	{"pr", "merge", "1", "-R", "acme/widgets", "--squash"},
	{"pr", "review", "1", "-R", "acme/widgets", "--approve"},
	{"release", "delete", "v1", "-R", "acme/widgets", "--yes"},
	{"repo", "edit", "acme/widgets", "--description", "a description"},
	{"cache", "delete", "123", "-R", "acme/widgets"},
	{"run", "rerun", "5", "-R", "acme/widgets"},
	{"workflow", "run", "ci.yaml", "-R", "acme/widgets"},
	{"browse", "-R", "acme/widgets", "--no-browser"},
	{"pr", "ls", "-R", "acme/widgets"},
	{"-R", "acme/widgets", "pr", "view", "1"},
}
