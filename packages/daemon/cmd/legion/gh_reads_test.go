package main

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// ghReadLeafCalls is one call of each leaf ghReadLeaves lists, keyed by that leaf, each naming
// acme/widgets, or its owner alone for `repo list` and `search repos`, the way the leaf's help
// takes it. TestGhReadLeafCallsCoverEveryReadLeaf fails while a leaf of the list has no call here,
// so a leaf added to the list is run as a read by TestLegionGhChecksAReadAndReadsNoVisibility.
var ghReadLeafCalls = map[string][]string{
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

// ghReadLeafSpellings are reads ghReadLeafCalls does not spell, each in its leaf's grammar
// (ghReadLeaves): a -w that is no --web (`run list`'s is --workflow), the JSON options with a
// filter, a leaf's switches among its options that take a value, and a search's filters.
var ghReadLeafSpellings = [][]string{
	{"run", "list", "-R", "acme/widgets", "-w", "ci.yaml"},
	{"pr", "view", "1", "--repo", "acme/widgets", "--json", "title,body", "-q", ".title"},
	{"pr", "checks", "1", "-R", "acme/widgets", "--watch", "-i", "5", "--fail-fast"},
	{"issue", "list", "-R", "acme/widgets", "-s", "open", "-l", "bug", "-L", "5", "--json", "number"},
	{"search", "issues", "--repo", "acme/widgets", "--state", "open", "--include-prs", "--json", "number"},
}

// ghGuardedLeafSpellings are calls of listed leaves that write nothing, typed outside their
// leaf's grammar and so guarded whole: a value after `=` or joined to its short option, an empty
// value and one beginning with `-`, short options run together, `--` before an argument, a
// boolean given a value, --web=false among them, which opens no browser, and --web, which no
// grammar names, in each spelling: --web and -w alone, -w last in a short cluster (`-cw`), each
// given a value, a value that is no boolean among them, and --web after a --web=false.
var ghGuardedLeafSpellings = [][]string{
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

// ghAPIReadSpellings are gh api options in the grammar of a read (ghAPIReadOptions), which leave
// a GET of a relative REST path a read: none; each option alone, by each of its spellings, its
// value in the word after it, the method exactly GET; and several together.
var ghAPIReadSpellings = [][]string{
	nil,
	{"--paginate"}, {"--paginate", "--slurp"}, {"--jq", ".[].number"}, {"-q", ".x"},
	{"--template", "{{.}}"}, {"-t", "{{.}}"}, {"--cache", "1h"},
	{"-X", "GET"}, {"--method", "GET"}, {"-H", "Accept: application/vnd.github+json"}, {"--header", "Accept: application/json"},
	{"--paginate", "-q", ".[].number", "--cache", "60m"},
	{"-X", "GET", "-H", "Accept: application/json", "--jq", ".[].number"},
}

// ghAPIReadEndpoints are relative REST paths of acme/widgets: plain, with a leading slash, with a
// query, and with a percent-escape.
var ghAPIReadEndpoints = []string{
	"repos/acme/widgets/pulls", "/repos/acme/widgets/pulls", "repos/acme/widgets/issues?state=open", "repos/acme/widgets/pull%73",
}

// ghAPIGuardedSpellings are gh api options outside the grammar of a read, which make a call with
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
var ghAPIGuardedSpellings = [][]string{
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

// ghAPIGuardedEndpoints are endpoints that make a call with any options no read: a whole URL,
// GitHub's GraphQL API by each of its names, and paths whose `.` or `..` segments a server may
// resolve to another.
var ghAPIGuardedEndpoints = []string{
	"https://api.github.com/repos/acme/widgets/pulls", "graphql", "/graphql", "GRAPHQL", "%67raphql",
	"https://api.github.com/graphql", "repos/acme/widgets/./pulls", "repos/acme/widgets/pulls/../../../../graphql",
}

// ghAPICalls is the cross product of options and endpoints as `gh api` calls: each endpoint with
// each option, the option before the endpoint and, where there is one, after it.
func ghAPICalls(options [][]string, endpoints []string) [][]string {
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

// ghReadCalls are the calls that write nothing which these tests run: ghReadLeafCalls in the
// order of their leaves, ghReadLeafSpellings, and every gh api read of ghAPIReadSpellings and
// ghAPIReadEndpoints.
func ghReadCalls() [][]string {
	var calls [][]string
	for _, leaf := range slices.Sorted(maps.Keys(ghReadLeafCalls)) {
		calls = append(calls, ghReadLeafCalls[leaf])
	}
	calls = append(calls, ghReadLeafSpellings...)
	return append(calls, ghAPICalls(ghAPIReadSpellings, ghAPIReadEndpoints)...)
}

// ghGuardedAPICalls are the gh api calls these tests run whole: every option of
// ghAPIGuardedSpellings before one read endpoint, every endpoint of ghAPIGuardedEndpoints with no
// option, and `--` before a word collect reads as a method. TestGhWritesNothingReadsTheLeafAndTheMethod
// reads the whole cross product, each option before and after each endpoint.
func ghGuardedAPICalls() [][]string {
	calls := [][]string{{"api", "--", "-XPOST"}}
	for _, option := range ghAPIGuardedSpellings {
		calls = append(calls, slices.Concat([]string{"api"}, option, []string{ghAPIReadEndpoints[0]}))
	}
	for _, endpoint := range ghAPIGuardedEndpoints {
		calls = append(calls, []string{"api", endpoint})
	}
	return calls
}

// ghGuardedCalls are calls of commands other than gh api that the guard still reads whole: the
// writes a role makes (`pr create`, `pr comment`, `pr edit`, `issue comment`, `release create`); a
// write of every family of commands ghReadLeaves reads from but search, which has none (`label
// create`, `issue create`, `issue close`, `pr merge`, `pr review`, `release delete`, `repo edit`,
// `cache delete`, `run rerun`, `workflow run`), so a write added to the list beside its family's
// reads fails here; a leaf ghReadLeaves does not list, although it writes nothing to GitHub
// (`browse`); and a listed read typed otherwise than by its canonical path, by an alias (`pr ls`)
// or after an option (`-R o/r pr view`). The gh api calls are ghAPICalls' cross products.
// (`--hostname` naming another host than github.com is refused before the guard, exit 2, as on
// main.)
var ghGuardedCalls = [][]string{
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

// ghTargetOf says a call writes nothing only for a call in a read grammar (ghWritesNothing): a
// leaf on ghReadLeaves typed by its canonical path with only the options its grammar names, each
// value in the word after its option, or a `gh api` GET of a relative REST path given only the
// options ghAPIReadOptions names, each word read as typed. It says every other call writes: a
// listed leaf typed in another shape, one given --web, and every gh api call of the three cross
// products with a guarded option or a guarded endpoint, each option before and after each
// endpoint, among them.
func TestGhWritesNothingReadsTheLeafAndTheMethod(t *testing.T) {
	guardedAPI := slices.Concat(
		ghAPICalls(ghAPIGuardedSpellings, ghAPIReadEndpoints),
		ghAPICalls(ghAPIReadSpellings, ghAPIGuardedEndpoints),
		ghAPICalls(ghAPIGuardedSpellings, ghAPIGuardedEndpoints),
		ghGuardedAPICalls(),
	)
	for _, calls := range []struct {
		argvs [][]string
		want  bool
	}{{ghReadCalls(), true}, {ghGuardedCalls, false}, {ghGuardedLeafSpellings, false}, {guardedAPI, false}} {
		for _, argv := range calls.argvs {
			target, err := ghTargetOfKept(t, argv, nil, savedGhHelp, noGitRepository, ownersOwnDirectories)
			if err != nil {
				t.Errorf("ghTargetOf(%q) = %v", argv, err)
				continue
			}
			if target.writesNothing != calls.want {
				t.Errorf("ghTargetOf(%q) writes nothing = %v, want %v", argv, target.writesNothing, calls.want)
			}
		}
	}
}

// ghGraphQLQueryRead admits a query of GitHub's GraphQL API that writes nothing (R4 of
// dispatch://INFRA-3399), and nothing else: `api graphql` typed first and exactly, then only raw
// and typed fields, headers that name no method override and no Content-Length, the output
// filters and the pagination switches, in each of their spellings and each value in the next
// word; exactly one raw `query` field and no other field naming the query; no typed value gh
// reads from a file; and a query whose text holds neither `mutation` nor `subscription`, in any
// case and wherever it stands. A call it admits is one ghTargetOf reads, and still one that does
// not write nothing by ghWritesNothing's rule, which the guard's reading of the call rests on.
func TestGhGraphQLQueryRead(t *testing.T) {
	graphql := func(words ...string) []string { return append([]string{"api", "graphql"}, words...) }
	const viewer = "query={viewer{login}}"
	const paged = "query=query($endCursor:String){viewer{repositories(first:1,after:$endCursor){nodes{name}pageInfo{hasNextPage endCursor}}}}"
	for _, tc := range []struct {
		argv []string
		want bool
	}{
		// A fragment; the shorthand query; and three documents whose block string opens on a run
		// of quotes, its `"""` followed by a backslash and four quotes, by a backslash and five, or
		// by three more quotes, which lexers may close at different quotes: each holds neither
		// word, so it is a read whatever a lexer makes of its quotes.
		{graphql("-f", "query=query{...f} fragment f on Query{viewer{login}}"), true},
		{graphql("-f", viewer), true},
		{graphql("-f", `query=query Q{a(x:"""\"""" "){b}} query M{c}`), true},
		{graphql("-f", `query=query Q{a(x:"""\""""" "){b}} query M{c}`), true},
		{graphql("-f", `query=query Q{a(x:"""""" "){b}} query M{c}`), true},
		// The twins of two refused documents below, each holding neither word: a repository's
		// nameWithOwner read in place of its viewerSubscription, and the review's document with a
		// query in place of its mutation.
		{graphql("-f", "owner=octocat", "-f", "name=hello", "-f", "query=query($owner:String!,$name:String!){repository(owner:$owner,name:$name){nameWithOwner}}"), true},
		{graphql("-f", "owner=octocat", "-f", "operationName=M", "-f", `query=query Q{nodes(ids:["""\"""" "]){id}} query M{viewer{login}} # """`), true},
		// A variable typed beside the raw query, and each option and switch, by each spelling.
		{graphql("-F", "owner=octocat", "-f", "query=query($owner:String!){repositoryOwner(login:$owner){login}}"), true},
		{graphql("-F", "name=hello", "-f", viewer), true},
		{graphql("-H", "Accept: application/json", "-f", viewer), true},
		{graphql("-f", viewer, "-q", ".data"), true},
		{graphql("-f", viewer, "-t", "{{.data}}"), true},
		{graphql("--paginate", "-f", paged), true},
		{graphql("--paginate", "--slurp", "-f", paged), true},
		{graphql("--raw-field", viewer), true},
		{graphql("--field", "name=hello", "-f", viewer), true},
		{graphql("--header", "Accept: application/json", "-f", viewer), true},
		{graphql("-f", viewer, "--jq", ".data"), true},
		{graphql("-f", viewer, "--template", "{{.data}}"), true},
		// A document naming `mutation` or `subscription` as an operation, a field or an argument.
		{graphql("-f", `query=mutation{addStar(input:{starrableId:"x"}){clientMutationId}}`), false},
		{graphql("-f", "query=subscription{x}"), false},
		{graphql("-f", `query=query{viewer{login}} mutation{addStar(input:{starrableId:"x"}){clientMutationId}}`), false},
		{graphql("-f", "query=query{mutation}"), false},
		{graphql("-f", "query=query{a(subscription:1){b}}"), false},
		// The word anywhere refuses, where GraphQL reads no name too: in a comment, a string and a
		// block string, one holding an escaped `\"""` among them; inside a longer name; and in any
		// case.
		{graphql("-f", "query=query{viewer{login}} # mutation{addStar}"), false},
		{graphql("-f", "owner=octocat", "-f", "query=query{viewer{login}} # mutation"), false},
		{graphql("-f", "query=# mutation\r\nquery{viewer{login}}"), false},
		{graphql("-f", `query=query{a(x:"mutation{b} \" # \\")}`), false},
		{graphql("-f", "owner=octocat", "-f", `query=query{search(query:"subscription",type:REPOSITORY,first:1){repositoryCount}}`), false},
		{graphql("-f", `query=query{a(x:"""mutation{b}""")}`), false},
		{graphql("-f", "query=query{a(x:\"\"\"line\nmutation{b} \\\"\"\" still the string\"\"\")}"), false},
		{graphql("-f", "owner=octocat", "-f", "name=hello", "-f", "query=query($owner:String!,$name:String!){repository(owner:$owner,name:$name){viewerSubscription}}"), false},
		{graphql("-f", "query=query{a} MUTATION{b}"), false},
		{graphql("-f", "query=query{a} Subscription{b}"), false},
		// A mutation after a run of quotes: the review's document, whose block string GitHub's
		// lexer closes at the backslash and four quotes and a lexer written to the GraphQL
		// specification runs on to the comment's `"""`, and the three documents above with a
		// mutation in place of their second query.
		{graphql("-f", "owner=octocat", "-f", "operationName=M", "-f", "id=x", "-f", `query=query Q{nodes(ids:["""\"""" "]){id}} mutation M($id:ID!){addStar(input:{starrableId:$id}){clientMutationId}} # """`), false},
		{graphql("-f", `query=query Q{a(x:"""\"""" "){b}} mutation M{c}`), false},
		{graphql("-f", `query=query Q{a(x:"""\""""" "){b}} mutation M{c}`), false},
		{graphql("-f", `query=query Q{a(x:"""""" "){b}} mutation M{c}`), false},
		// A mutation a lexer could take for a comment's text where it reads comments and strings
		// otherwise than GraphQL: after a `#` in a string, after a comment a lone `\r` ends, and
		// after a comment holding a quote.
		{graphql("-f", `query=query{a(x:"#")} mutation{b}`), false},
		{graphql("-f", "query=query{a} #\rmutation{b}"), false},
		{graphql("-f", "query=query{a} # \"\nmutation{b}"), false},
		// Not exactly one raw `query` field, or another field gh reads as the query.
		{graphql("-f", viewer, "-f", viewer), false},
		{graphql("-f", "owner=octocat"), false},
		{graphql("-f", viewer, "-f", "query[]=mutation{b}"), false},
		{graphql("-f", viewer, "-f", "query[x]=mutation{b}"), false},
		{graphql("-f", viewer, "-f", "query]x=mutation{b}"), false},
		{graphql("-f", viewer, "-f", "[query]=mutation{b}"), false},
		// A query gh fills placeholders in after this read it, or reads from a file; and a
		// typed value read from a file or standard input.
		{graphql("-F", viewer), false},
		{graphql("-F", "query={branch}{viewer{login}}"), false},
		{graphql("-F", "query=:branch{viewer{login}}"), false},
		{graphql("-F", "query={repo}{viewer{login}}"), false},
		{graphql("--field", viewer), false},
		{graphql("-F", "query=@f"), false},
		{graphql("-f", viewer, "-F", "x=@f"), false},
		{graphql("-f", viewer, "-F", "x=@-"), false},
		// An option that sends another body, method or host, or names a method override or the
		// body's length; a value joined to its option; `--`; a value missing.
		{graphql("-f", viewer, "--input", "f"), false},
		{graphql("-f", viewer, "-X", "GET"), false},
		{graphql("-f", viewer, "--method", "GET"), false},
		{graphql("-f", viewer, "--hostname", "github.com"), false},
		{graphql("-f", viewer, "-H", "X-HTTP-Method-Override: DELETE"), false},
		{graphql("-f", viewer, "-H", " x-http-method : DELETE"), false},
		{graphql("-f", viewer, "-H", "Content-Length: 0"), false},
		{graphql("-fquery={viewer{login}}"), false},
		{graphql("--raw-field=query={viewer{login}}"), false},
		{graphql("--", "-f", viewer), false},
		{graphql("-f", viewer, "-q"), false},
		// Another endpoint, a word before `api graphql`, and a field with no `=`.
		{[]string{"api", "/graphql", "-f", viewer}, false},
		{[]string{"api", "graphql/", "-f", viewer}, false},
		{[]string{"api", "GraphQL", "-f", viewer}, false},
		{[]string{"-R", "acme/widgets", "api", "graphql", "-f", viewer}, false},
		{[]string{"x", "api", "graphql", "-f", viewer}, false},
		{graphql("-f", viewer, "-f", "owner"), false},
	} {
		if got := ghGraphQLQueryRead(tc.argv); got != tc.want {
			t.Errorf("ghGraphQLQueryRead(%q) = %v, want %v", tc.argv, got, tc.want)
		}
		if !tc.want {
			continue
		}
		if target, err := ghTargetOfKept(t, tc.argv, nil, savedGhHelp, noGitRepository, ownersOwnDirectories); err != nil || target.writesNothing {
			t.Errorf("ghTargetOf(%q) = %+v, %v; want a call it reads, which writes nothing by no rule of ghWritesNothing's", tc.argv, target, err)
		}
	}
}

// runGh decides each call it passes with no guard before a call that writes nothing, so none of
// them is ever taken for one (ghCallModeOf): the exact `gh api rate_limit`, which the gh api read
// grammar would admit, is the rate-limit call, and a guard's own visibility read is that read,
// whatever ghWritesNothing says of either. Nor is the shared check's own visibility read, which
// runGh answers before it reads the command (sharedCheckVisibilityRead), or either of the guard's
// own reads in a read grammar: each gives --hostname, and the GraphQL read is a POST to `graphql`
// with fields, so none would be taken for a read if the order changed.
func TestGhCallModeDecidesTheUncheckedCallsBeforeTheReadGrammar(t *testing.T) {
	for _, tc := range []struct {
		rateLimitRead, visibilityRead, writesNothing bool
		want                                         ghCallMode
	}{
		{true, false, true, ghRateLimitCall},
		{true, false, false, ghRateLimitCall},
		{false, true, true, ghVisibilityCall},
		{false, true, false, ghVisibilityCall},
		{false, false, true, ghReadCall},
		{false, false, false, ghGuardedCall},
	} {
		if got := ghCallModeOf(tc.rateLimitRead, tc.visibilityRead, tc.writesNothing); got != tc.want {
			t.Errorf("ghCallModeOf(rate limit %v, visibility read %v, writes nothing %v) = %d, want %d", tc.rateLimitRead, tc.visibilityRead, tc.writesNothing, got, tc.want)
		}
	}
	for _, argv := range [][]string{
		leakguard.VisibilityReadArgs("acme/widgets"),
		leakguard.VisibilityReadWithHeadersArgs("acme/widgets"),
		leakguard.VisibilityGraphQLArgs("acme/widgets"),
	} {
		if ghWritesNothing(argv, argv[:1]) {
			t.Errorf("ghWritesNothing(%q) = true, want a visibility read outside every read grammar", argv)
		}
	}
}

// The read rule is proven against the check the worker image pins: the tag ghReadsProvenCheck
// names is worker.Dockerfile's `ARG LEAK_CHECK_TAG`, its sha256 is one of a public_leak_check.py,
// and it is the sha256 of the public_leak_check.py the tests' port of the check was written
// against (ghPinnedCheckPortSHA256, gh_pinned_check_test.go). That sha256 is the gate `legion gh`
// hands the guard for every call, whatever checker a role is told. A bump of the pin
// fails here until the tag, the sha256 and that port all move.
func TestGhReadsAreProvenAgainstThePinnedCheck(t *testing.T) {
	const prove = "follow the steps in ghReadsProvenCheck's comment (gh_reads.go): read the definitions it lists and every definition they reach in the new check, port each change to what they read as a read into ghPinnedCheckReads and the new sha256 into ghPinnedCheckPortSHA256 (gh_pinned_check_test.go), run INFRA-3562's xcheck4069 harness (xcheck4069.tar.gz) against the check with 0 mismatching rows, then name its tag and the sha256 of its public_leak_check.py in ghReadsProvenCheck"
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "docker", "worker.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`(?m)^ARG LEAK_CHECK_TAG=(\S+)$`).FindSubmatch(dockerfile)
	if pinned == nil {
		t.Fatal("worker.Dockerfile holds no ARG LEAK_CHECK_TAG=<tag> line")
	}
	if string(pinned[1]) != ghReadsProvenCheck.tag {
		t.Errorf("the worker image pins %s, and the read rule was proven against %s: for %s, %s", pinned[1], ghReadsProvenCheck.tag, pinned[1], prove)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(ghReadsProvenCheck.sha256) {
		t.Errorf("ghReadsProvenCheck's sha256 is %q, want 64 lowercase hex digits", ghReadsProvenCheck.sha256)
	}
	if ghPinnedCheckPortSHA256 != ghReadsProvenCheck.sha256 {
		t.Errorf("ghReadsProvenCheck names the public_leak_check.py whose sha256 is %s, and ghPinnedCheckReads ports the one whose sha256 is %s: for %s, %s", ghReadsProvenCheck.sha256, ghPinnedCheckPortSHA256, ghReadsProvenCheck.tag, prove)
	}
}

// ghReadLeafCalls holds a call of every leaf ghReadLeaves lists and of no other, each call runs
// the leaf it is keyed by, and ghGuardedCalls holds a write of every family of commands the list
// reads from but search, which has none.
func TestGhReadLeafCallsCoverEveryReadLeaf(t *testing.T) {
	for leaf := range ghReadLeaves {
		if _, ok := ghReadLeafCalls[leaf]; !ok {
			t.Errorf("ghReadLeaves lists %q, and ghReadLeafCalls holds no call of it", leaf)
		}
	}
	families := map[string]bool{}
	for leaf, argv := range ghReadLeafCalls {
		if _, listed := ghReadLeaves[leaf]; !listed {
			t.Errorf("ghReadLeafCalls holds %q, which ghReadLeaves does not list", leaf)
		}
		if target, err := ghTargetOfKept(t, argv, nil, savedGhHelp, noGitRepository, ownersOwnDirectories); err != nil || target.command != leaf {
			t.Errorf("ghTargetOf(%q) = %+v, %v; want the leaf %q", argv, target, err, leaf)
		}
		families[strings.Fields(leaf)[0]] = true
	}
	delete(families, "search")
	for family := range families {
		if !slices.ContainsFunc(ghGuardedCalls, func(argv []string) bool { return argv[0] == family }) {
			t.Errorf("ghGuardedCalls holds no write of the %s commands, whose reads ghReadLeaves lists", family)
		}
	}
}

// Every option a read grammar names (ghReadLeaves, ghAPIReadOptions) is one its leaf's help
// lists by that spelling, takes a value exactly where the grammar says it does and never only
// after `=` (pflag's NoOptDefVal), and is neither --web, nor --help, nor an option whose help
// names the browser, nor one whose value can name a file gh sends (ghSentFileOptions): so gh
// reads each word of a call in the grammar as the grammar does, such a call opens no browser, and
// it sends no file, which ghTargetOf's writesNothing relies on. A gh whose help moves an option a
// grammar names fails this once its help is saved here.
func TestGhReadGrammarNamesEachOptionAsItsHelpDoes(t *testing.T) {
	grammars := maps.Clone(ghReadLeaves)
	grammars["api"] = ghAPIReadOptions
	for leaf, options := range grammars {
		cmd, err := readGhCommand(strings.Fields(leaf), savedGhHelp)
		if err != nil {
			t.Errorf("gh %s's saved help: %v", leaf, err)
			continue
		}
		for spelling, takesValue := range options {
			name, listed := "", false
			if long, ok := strings.CutPrefix(spelling, "--"); ok {
				name = long
				_, listed = cmd.flags[name]
			} else if len(spelling) == 2 && spelling[0] == '-' {
				name, listed = cmd.shorts[spelling[1]]
			}
			if !listed {
				t.Errorf("gh %s's read grammar names %s, which its help does not list", leaf, spelling)
				continue
			}
			flag := cmd.flags[name]
			if flag.uncertain || flag.optional || flag.value != takesValue {
				t.Errorf("gh %s's read grammar reads %s as taking a value: %v; its help says %+v", leaf, spelling, takesValue, flag)
			}
			if name == "web" || name == "help" || strings.Contains(strings.ToLower(flag.description), "browser") {
				t.Errorf("gh %s's read grammar names %s (%q), which opens a browser or prints help", leaf, spelling, flag.description)
			}
			if _, sends := ghSentFileOptions[name]; sends {
				t.Errorf("gh %s's read grammar names %s, whose value can name a file gh sends", leaf, spelling)
			}
		}
	}
}

// ghFakeToken is a credential of the shape the shared check refuses in every call the wrapper
// counts as a read, a GitHub token (its LEAK_SECRET's `gh[pousr]_[A-Za-z0-9]{36,}`, which its own
// self-test fills with 36 drawn characters), built here at run time so that this file holds no
// string of that shape.
var ghFakeToken = "ghp_" + strings.Repeat("0aZ9", 9)

// ghCheckRefusesACredential is a stand-in shared check that refuses, as the installed check does
// (its gh_guard refuses a SECRET before it returns for a read), a gh call any argument of which
// carries a GitHub token, and clears every other call. It records each run and reads its standard
// input, as leakguardtest's stand-ins do, and prints the check's own refusal, which names no part
// of the token.
var ghCheckRefusesACredential = leakguardtest.Recording("cat >/dev/null 2>&1\n" +
	"if printf '%s\\n' \"$@\" | grep -Eq '(^|[^A-Za-z0-9_])gh[pousr]_[A-Za-z0-9]{36,}'; then\n" +
	"  echo 'LEAK CHECK: refusing this gh call: it carries a credential, which is refused whatever repository the call goes to'\n" +
	"  exit 3\n" +
	"fi\n" +
	"exit 0\n")

// ghGuardRecord is what one `legion gh` run of ghGuardRig left: its status and standard error,
// what the stand-in gh recorded (`gh <args>`, `env <its pager and browser variables>` and `stdin
// <text>` for the call, `visibility <args>` for each read of the guard's), what the stand-in
// shared check recorded of each run, and the call log.
type ghGuardRecord struct {
	code                     int
	stderr, gh, checked, log string
}

// ghGuardRun is the line a recording stand-in check (leakguardtest.Recording) writes for the
// guard's run of the gh call args: the check's gh-guard mode, `--` and the call's words, the call
// form leak-check-v33 takes (leakguard's modeArgv).
func ghGuardRun(args []string) string {
	return "checked gh-guard -- " + strings.Join(args, " ") + "\n"
}

// ghRigEnv is what the stand-in gh records of the pager, browsers and editor the rig's caller
// names for a call guarded whole: none of them, since gh is run from an allowlist of its caller's
// environment that holds no program (ghChildEnviron), and as its editor a program that fails
// (ghNoEditor), so no editor gh starts can post text the guard never read; ghRigReadEnv is what it
// records for a call that writes nothing, which runs with cat as its pager.
const (
	ghRigEnv     = "env GH_PAGER=unset GH_BROWSER=unset BROWSER=unset GH_EDITOR=" + ghNoEditor + " EDITOR=unset VISUAL=unset\n"
	ghRigReadEnv = "env GH_PAGER=cat GH_BROWSER=unset BROWSER=unset GH_EDITOR=" + ghNoEditor + " EDITOR=unset VISUAL=unset\n"
)

// ghGuardRig answers a run of `legion gh args…` with stdin against the shared check at the path
// check (ghCheckAt), a stand-in gh that answers the visibility read `public` and every call with
// exit 7 after recording it, and a state directory of the run's own, so no visibility is kept
// from another run. The caller's environment names a pager, two browsers and three editors; the
// deployment's manifest digest as the runtime tells a role it (leakguard.CheckerVar), which is
// the stand-in check's own (ghOwnManifest); the binary's proven digest (testProvenCheckEnv),
// which is the stand-in program's (ghProvenProgramSHA256); and rules, where it is not "", as the
// deployment's rule file (LEGION_PRIVATE_NAME_RULES_FILE): a call guarded whole reads its
// destination's visibility and runs the private-name pass only where a rule file is named, so a
// rig of a test that reads either names the fixture's (leakguardtest.WriteRules).
func ghGuardRig(t *testing.T, rules string) func(t *testing.T, check, stdin string, args ...string) ghGuardRecord {
	t.Helper()
	return ghGuardRigWith(t, ghOwnManifest, rules)
}

// ghOwnManifest stands, as a rig's checker, for the manifest digest of the check each call names
// (ghManifestOf): a role of a deployment that runs that very check.
const ghOwnManifest = "the check's own manifest digest"

// ghManifestOf is the manifest digest (leakguard.Manifest) of the stand-in check at check.
func ghManifestOf(t *testing.T, check string) string {
	t.Helper()
	digest, err := leakguard.Manifest(func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(filepath.Dir(check), name))
	})
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// ghGuardRigWith is ghGuardRig with checker as the deployment's manifest digest the caller's
// environment names (ghOwnManifest for each call's check's own), and none at all where checker is
// empty: a role of a deployment that names no check.
func ghGuardRigWith(t *testing.T, checker, rules string) func(t *testing.T, check, stdin string, args ...string) ghGuardRecord {
	t.Helper()
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	script := ghStandInPrefix(help) +
		"printf 'gh %s\\n' \"$*\" >>\"$GH_STANDIN_RECORD\"\n" +
		"printf 'env GH_PAGER=%s GH_BROWSER=%s BROWSER=%s GH_EDITOR=%s EDITOR=%s VISUAL=%s\\n' \"${GH_PAGER-unset}\" \"${GH_BROWSER-unset}\" \"${BROWSER-unset}\" \"${GH_EDITOR-unset}\" \"${EDITOR-unset}\" \"${VISUAL-unset}\" >>\"$GH_STANDIN_RECORD\"\n" +
		"printf 'stdin %s\\n' \"$(cat)\" >>\"$GH_STANDIN_RECORD\"\n" +
		"exit 7\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return func(t *testing.T, check, stdin string, args ...string) ghGuardRecord {
		t.Helper()
		records := t.TempDir()
		ghRecord, checkRecord, state := filepath.Join(records, "gh"), filepath.Join(records, "check"), filepath.Join(records, "state")
		told := checker
		if told == ghOwnManifest {
			// A check that is not there has no manifest: its role is one of a deployment that
			// names some digest, which a stand-in stands for; the call is refused for the
			// missing check whatever the digest.
			told = strings.Repeat("5e", 32)
			if _, err := os.Stat(check); err == nil {
				told = ghManifestOf(t, check)
			}
		}
		env := map[string]string{
			"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
			"PUBLIC_LEAK_CHECK": check, leakguard.CheckerVar: told, testProvenCheckEnv: ghProvenProgramSHA256,
			"GH_STANDIN_RECORD": ghRecord, "LEAK_CHECK_RECORD": checkRecord, "XDG_STATE_HOME": state,
			"LEGION_ROLE": "merger", "LEGION_ISSUE": "issue-9", "LEAK_CHECK_STANDIN_TARGET": "acme/widgets",
			"GH_PAGER": "rig-pager", "GH_BROWSER": "rig-gh-browser", "BROWSER": "rig-browser",
			"GH_EDITOR": "rig-gh-editor", "EDITOR": "rig-editor", "VISUAL": "rig-visual",
		}
		if told == "" {
			delete(env, leakguard.CheckerVar)
		}
		if rules != "" {
			env[leakguard.RulesFileVar] = rules
		}
		code, _, stderr := legionGhIn(t, t.TempDir(), env, stdin, args...)
		return ghGuardRecord{
			code: code, stderr: stderr,
			gh: readRecord(t, ghRecord), checked: readRecord(t, checkRecord),
			log: readRecord(t, filepath.Join(state, "legion-gh", "calls.log")),
		}
	}
}

// testProvenCheckEnv names, to this package's test binary run as `legion` (TestMain), the sha256
// of the public_leak_check.py its tests count as proven, in place of ghReadsProvenCheck's.
const testProvenCheckEnv = "LEGION_TEST_PROVEN_CHECK_SHA256"

// ghProvenProgram is the public_leak_check.py of the stand-in checks these tests count as the one
// the read rule was proven against, and ghProvenProgramSHA256 is its sha256, which the rig names
// to `legion gh` as the binary's proven digest (testProvenCheckEnv). The deployment's manifest
// digest is another thing, over every file of the check (ghManifestOf).
const ghProvenProgram = "# stands for the shared check the read rule was proven against\n"

var ghProvenProgramSHA256 = func() string {
	sum := sha256.Sum256([]byte(ghProvenProgram))
	return hex.EncodeToString(sum[:])
}()

// ghCheckAt is the path of a stand-in shared check whose body is script, in a directory of the
// test's own, with ghProvenProgram beside it as its public_leak_check.py and the other files of
// its manifest.
func ghCheckAt(t *testing.T, script string) string {
	t.Helper()
	return ghCheckWithProgram(t, script, ghProvenProgram)
}

// ghCheckWithProgram is ghCheckAt with program as its public_leak_check.py, and none where
// program is empty.
func ghCheckWithProgram(t *testing.T, script, program string) string {
	t.Helper()
	check := leakguardtest.Write(t, t.TempDir(), script)
	leakguardtest.Companions(t, filepath.Dir(check), program)
	return check
}

// A call that writes nothing, a call of every leaf ghReadLeaves lists among them, runs the shared
// check and no visibility read: the stand-in check records its run of the call and clears it, the
// stand-in gh records no visibility read, only the call, which gets the run's standard input
// whole and whose status comes back. gh runs it with GH_PAGER=cat and with neither GH_BROWSER nor
// BROWSER, though the caller set all three. Its line in the call log says
// `visibility="checked-only"` and names the repository the call resolved.
func TestLegionGhChecksAReadAndReadsNoVisibility(t *testing.T) {
	run := ghGuardRig(t, "")
	const stdin = "a line on standard input\n"
	for _, args := range ghReadCalls() {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got := run(t, ghCheckAt(t, ghCheckRefusesACredential), stdin, args...)
			if got.code != 7 {
				t.Errorf("legion gh %q = exit %d, stderr %q; want gh's own 7", args, got.code, got.stderr)
			}
			if want := ghGuardRun(args); got.checked != want {
				t.Errorf("the shared check recorded %q, want its one run of the call, %q", got.checked, want)
			}
			if strings.Contains(got.gh, "visibility ") {
				t.Errorf("gh was asked the repository's visibility for a call that writes nothing: %q", got.gh)
			}
			if want := "gh " + strings.Join(args, " ") + "\n" + ghRigReadEnv + "stdin " + strings.TrimSuffix(stdin, "\n") + "\n"; got.gh != want {
				t.Errorf("gh recorded %q, want %q", got.gh, want)
			}
			target, err := ghTargetOfKept(t, args, nil, savedGhHelp, noGitRepository, ownersOwnDirectories)
			if err != nil {
				t.Fatalf("ghTargetOf(%q) = %v", args, err)
			}
			repository := ""
			if target.repo != "" {
				repository = target.owner + "/" + target.repo
			}
			if wants := []string{`visibility="checked-only"`, "repository=" + ghCallLogValue(repository), "status=7"}; strings.Count(got.log, "\n") != 1 || slices.ContainsFunc(wants, func(want string) bool { return !strings.Contains(got.log, want) }) {
				t.Errorf("the call log reads %q, want one line naming %q", got.log, wants)
			}
		})
	}
}

// A read whose arguments carry a credential is refused by the shared check, exit 2 with the
// check's refusal, and never reaches gh: the check runs for a call that writes nothing, and it
// refuses a credential in every call the wrapper counts as a read (ghWritesNothing). Each of
// these is in a read grammar as typed, and the refusal comes before any visibility read.
func TestLegionGhRefusesAReadThatCarriesACredential(t *testing.T) {
	run := ghGuardRig(t, "")
	for _, args := range [][]string{
		{"pr", "list", "-R", "acme/widgets", "--search", ghFakeToken},
		{"api", "repos/acme/widgets/issues?q=" + ghFakeToken},
		{"search", "code", ghFakeToken},
		{"api", "-H", "Authorization: token " + ghFakeToken, "repos/acme/widgets/pulls"},
	} {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			if target, err := ghTargetOfKept(t, args, nil, savedGhHelp, noGitRepository, ownersOwnDirectories); err != nil || !target.writesNothing {
				t.Fatalf("ghTargetOf(%q) = %+v, %v; want a call that writes nothing", args, target, err)
			}
			got := run(t, ghCheckAt(t, ghCheckRefusesACredential), "", args...)
			if got.code != 2 || !strings.Contains(got.stderr, "LEAK CHECK: refusing") || !strings.Contains(got.stderr, "it carries a credential") {
				t.Errorf("legion gh %q = exit %d, stderr %q; want exit 2 and the check's refusal of a credential", args, got.code, got.stderr)
			}
			if want := ghGuardRun(args); got.checked != want {
				t.Errorf("the shared check recorded %q, want its run of the call", got.checked)
			}
			if got.gh != "" {
				t.Errorf("gh ran for a call the check refused: %q", got.gh)
			}
		})
	}
}

// A read whose shared check cannot run is refused as every guarded call is, exit 2 with the
// guard's setup error, and never reaches gh: a check that is not there, and one that exits with
// its own setup error (exit 2), have cleared nothing.
func TestLegionGhRefusesAReadWhoseGuardCannotRun(t *testing.T) {
	run := ghGuardRig(t, "")
	for _, tc := range []struct {
		name  string
		check func(t *testing.T) string
	}{
		{"no shared check", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone", leakguardtest.CheckName) }},
		{"a shared check that cannot finish", func(t *testing.T) string { return ghCheckAt(t, leakguardtest.CannotFinish) }},
	} {
		for _, args := range [][]string{
			{"pr", "view", "1", "-R", "acme/widgets"},
			{"api", "repos/acme/widgets/pulls"},
		} {
			t.Run(tc.name+": "+strings.Join(args, " "), func(t *testing.T) {
				got := run(t, tc.check(t), "", args...)
				if got.code != 2 || !strings.Contains(got.stderr, "LEAK CHECK SETUP ERROR") {
					t.Errorf("legion gh %q = exit %d, stderr %q; want exit 2 and the guard's setup error", args, got.code, got.stderr)
				}
				if got.gh != "" {
					t.Errorf("gh ran for a call whose check could not run: %q", got.gh)
				}
			})
		}
	}
}

// A read whose shared check is stopped before it answers never reaches gh, and `legion gh` ends
// with the status of the signal that stopped it, 143 for a SIGTERM, as for any signal that comes
// before gh starts (runGh), rather than with the setup error the stopped check would otherwise be
// refused with. `legion gh` gives the check no bound of its own, as it gives a write's none; what
// ends a check that hangs is this process's own end, a SIGTERM to its pid, which cancels the
// context the check runs under (main's signal.NotifyContext), and the guard kills the check's
// process group (procgroup). The bound the guard reads a check's stop against is leakguard's own
// test of a read past its bound (TestAGhCallThatWritesNothingRunsTheSharedCheckAlone).
func TestLegionGhRefusesAReadWhoseGuardIsStopped(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"printf 'gh %s\\n' \"$*\" >>\"$GH_STANDIN_RECORD\"\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	records := t.TempDir()
	ghRecord, checkRecord := filepath.Join(records, "gh"), filepath.Join(records, "check")
	// The check records its run and then waits in ticks for thirty seconds before it clears the
	// call, so a stop that never reaches it is a gh run this test reads, not a test that hangs.
	hangs := leakguardtest.Recording("cat >/dev/null 2>&1\nticks=0\nwhile [ \"$ticks\" -lt 600 ]; do sleep 0.05; ticks=$((ticks+1)); done\nexit 0\n")
	check := ghCheckAt(t, hangs)
	command := legionGhCommand(t, t.TempDir(), map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PUBLIC_LEAK_CHECK": check, leakguard.CheckerVar: ghManifestOf(t, check), testProvenCheckEnv: ghProvenProgramSHA256,
		"GH_STANDIN_RECORD": ghRecord, "LEAK_CHECK_RECORD": checkRecord,
	}, "pr", "view", "1", "-R", "acme/widgets")
	printed, err := os.Create(filepath.Join(records, "printed"))
	if err != nil {
		t.Fatal(err)
	}
	defer printed.Close()
	command.Stdout, command.Stderr = printed, printed
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	testwait.Eventually(t, "the shared check to be running", func() bool {
		_, err := os.Stat(checkRecord)
		return err == nil
	})
	// To this one pid, never to the group, which is this test binary's own.
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waited := command.Wait()
	exit, ok := waited.(*exec.ExitError)
	if !ok || exit.ExitCode() != 128+int(syscall.SIGTERM) {
		t.Fatalf("legion gh under a stopped check = %v, want exit %d: %s", waited, 128+int(syscall.SIGTERM), readRecord(t, printed.Name()))
	}
	if out := readRecord(t, printed.Name()); out != "" {
		t.Errorf("legion gh printed %q, want nothing: a stopped check is no refusal", out)
	}
	if ran := readRecord(t, ghRecord); ran != "" {
		t.Errorf("gh ran for a call whose check was stopped: %q", ran)
	}
}

// A listed read under a shared check that is not the one the read rule was proven against — its
// public_leak_check.py another file, or no such file, as a tmux pane's check of another version
// may be — gets the whole guard, the visibility read included where the deployment names its
// rule file, and its log line is no checked-only one. The role is told no checker, as a pane of a
// daemon with no leak check setting is: told one, a check whose program is not that checker is
// refused before the read (TestGhReadsUnderTheDeploymentsChecker).
func TestLegionGhGuardsAReadWholeUnderAnUnprovenCheck(t *testing.T) {
	run := ghGuardRigWith(t, "", leakguardtest.WriteRules(t, t.TempDir()))
	for _, program := range []struct{ name, text string }{
		{"another program", ghProvenProgram + "# an edit under the same tag\n"},
		{"no program", ""},
	} {
		for _, args := range [][]string{
			{"pr", "view", "1", "-R", "acme/widgets"},
			{"api", "repos/acme/widgets/pulls"},
		} {
			t.Run(program.name+": "+strings.Join(args, " "), func(t *testing.T) {
				guardedWhole(t, run(t, ghCheckWithProgram(t, ghCheckPasses, program.text), "", args...), args)
			})
		}
	}
}

// The manifest digest a role is told (leakguard.CheckerVar) widens no read: whether `legion gh`
// reads a call by the check alone is the check's program's, compared with the digest `legion gh`
// holds for every call, as main does. Under the proven stand-in check, a read is read by the check
// alone where the role is told that check's own manifest digest or none at all, as a pane of a
// tmux daemon that names no setting is. A role told another digest has a check that is not its
// deployment's, so the guard refuses the call before gh runs, naming the digest it was told and
// the check's own.
func TestGhReadsUnderTheDeploymentsChecker(t *testing.T) {
	args := []string{"pr", "view", "1", "-R", "acme/widgets"}
	for _, tc := range []struct{ name, checker string }{
		{name: "the check's own manifest digest", checker: ghOwnManifest},
		{name: "no digest", checker: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ghGuardRigWith(t, tc.checker, "")(t, ghCheckAt(t, ghCheckPasses), "", args...)
			if got.code != 7 || strings.Contains(got.gh, "visibility ") || !strings.Contains(got.log, `visibility="checked-only"`) {
				t.Errorf("legion gh %q = exit %d, gh %q, log %q; want gh's own 7, no visibility read and a checked-only line", args, got.code, got.gh, got.log)
			}
		})
	}
	t.Run("another digest", func(t *testing.T) {
		another := strings.Repeat("0", 64)
		check := ghCheckAt(t, ghCheckPasses)
		got := ghGuardRigWith(t, another, "")(t, check, "", args...)
		const refused = "LEAK CHECK SETUP ERROR: the gh call: the shared public leak check found is not the one the deployment names: LEGION_LEAK_CHECKER_MANIFEST_SHA256=" + "0000"
		if got.code != 2 || got.gh != "" || !strings.Contains(got.stderr, refused) || !strings.Contains(got.stderr, "has the manifest digest "+ghManifestOf(t, check)) {
			t.Errorf("legion gh %q = exit %d, gh %q, stderr %q; want exit 2, gh never run, and the setup error naming the digest told and the check's own", args, got.code, got.gh, got.stderr)
		}
	})
}

// A deployment whose own check's program is not the one the read rule was proven against gets the
// whole guard for a read, though that check is its deployment's (the role is told its own manifest
// digest): the read rule was never compared with that check, whose program is not the one the
// digest `legion gh` holds names, so the guard reads the call's visibility and runs the
// private-name pass as for a write, under the rule file the deployment names (the fixture's).
func TestARoleCheckerNamingAnotherDigestGetsTheWholeGuard(t *testing.T) {
	const another = "# another shared check, which the read rule was never compared with\n"
	sum := sha256.Sum256([]byte(another))
	if hex.EncodeToString(sum[:]) == ghProvenProgramSHA256 {
		t.Fatal("the other check's program hashes to the proven one's")
	}
	for _, args := range [][]string{
		{"pr", "view", "1", "-R", "acme/widgets"},
		{"api", "repos/acme/widgets/pulls"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			guardedWhole(t, ghGuardRigWith(t, ghOwnManifest, leakguardtest.WriteRules(t, t.TempDir()))(t, ghCheckWithProgram(t, ghCheckPasses, another), "", args...), args)
		})
	}
}

// guardedWhole fails t unless got is a call the guard read whole under a check that clears it:
// the check recorded its run of the call, the guard asked gh the visibility of the repository the
// call names (none for a call naming none, such as a GraphQL call, which the guard scans for that
// reason), and only then did gh run, with none of the pager and the browsers the caller set, and
// the log line is neither a checked-only one nor a no-rules one.
func guardedWhole(t *testing.T, got ghGuardRecord, args []string) {
	t.Helper()
	if got.code != 7 {
		t.Errorf("legion gh %q = exit %d, stderr %q; want the guard to clear it and gh's own 7", args, got.code, got.stderr)
	}
	if want := ghGuardRun(args); !strings.Contains(got.checked, want) {
		t.Errorf("the shared check recorded %q, want its run of the call, %q", got.checked, want)
	}
	wantReads := 0
	if strings.Contains(strings.Join(args, " "), "acme/widgets") {
		wantReads = 1
	}
	if reads := strings.Count(got.gh, "visibility "); reads != wantReads {
		t.Errorf("the guard read the visibility %d times, want %d: %q", reads, wantReads, got.gh)
	}
	if want := "gh " + strings.Join(args, " ") + "\n" + ghRigEnv; !strings.Contains(got.gh, want) {
		t.Errorf("gh recorded %q, want the call %q after the guard, with none of the caller's pager and browsers", got.gh, want)
	}
	for _, partial := range []string{ghCheckedOnlyVisibility, ghNoRulesVisibility} {
		if strings.Contains(got.log, "visibility="+ghCallLogValue(partial)) {
			t.Errorf("the call log reads a call the guard read whole as %s: %q", partial, got.log)
		}
	}
}

// Every other call runs the whole guard as before, one visibility read included, where the
// deployment names its rule file: the calls of commands other than gh api, calls of listed leaves
// typed outside their grammar, and gh api calls with a guarded option, the spellings review
// 5911414081 named among them, or a guarded endpoint.
func TestLegionGhGuardsEveryWrite(t *testing.T) {
	run := ghGuardRig(t, leakguardtest.WriteRules(t, t.TempDir()))
	for _, args := range slices.Concat(ghGuardedCalls, ghGuardedLeafSpellings, ghGuardedAPICalls()) {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			guardedWhole(t, run(t, ghCheckAt(t, ghCheckPasses), "", args...), args)
		})
	}

	// The guard still refuses what its check refuses, so gh never runs a write the check
	// refused.
	args := []string{"pr", "comment", "1", "-R", "acme/widgets", "--body", "a comment"}
	got := run(t, ghCheckAt(t, ghCheckRefuses), "", args...)
	if got.code != 2 || !strings.Contains(got.stderr, "LEAK CHECK: refusing") || strings.Contains(got.gh, "gh ") {
		t.Errorf("legion gh %q under a check that refuses = exit %d, stderr %q, gh %q; want exit 2, the guard's refusal and no gh run", args, got.code, got.stderr, got.gh)
	}
}

// With no rule file named, a write `legion gh` guards gets the shared check alone: the check
// runs, no visibility is read, gh runs the call with the environment of a call guarded whole
// (ghRigEnv: none of the pager, browsers and editors the caller set), the call's log line says
// no-rules, and standard error holds nothing, no guard line among it, though the body file names
// the fixture's host, which the fixture's rules refuse: a script that reads gh's output reads gh's
// alone. A read the shared check alone clears skips no private-name step, and its log line stays
// checked-only.
func TestLegionGhRunsAWriteWithNoRuleFile(t *testing.T) {
	run := ghGuardRig(t, "")
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("the runner answers at "+leakguardtest.RulesHost+" today\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		args       []string
		env        string
		visibility string
	}{
		{"a write", []string{"pr", "create", "-R", "acme/widgets", "--title", "a title", "--body-file", body}, ghRigEnv, ghNoRulesVisibility},
		{"a read", []string{"pr", "view", "1", "-R", "acme/widgets"}, ghRigReadEnv, ghCheckedOnlyVisibility},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, ghCheckAt(t, ghCheckPasses), "", tc.args...)
			if got.code != 7 || got.stderr != "" {
				t.Errorf("legion gh %q with no rule file = exit %d, stderr %q; want gh's own 7 and nothing printed", tc.args, got.code, got.stderr)
			}
			if want := ghGuardRun(tc.args); got.checked != want {
				t.Errorf("the shared check recorded %q, want its one run of the call, %q", got.checked, want)
			}
			// The call alone, and so no visibility read before it.
			if want := "gh " + strings.Join(tc.args, " ") + "\n" + tc.env + "stdin \n"; got.gh != want {
				t.Errorf("gh recorded %q, want %q", got.gh, want)
			}
			if wants := []string{"visibility=" + ghCallLogValue(tc.visibility), "repository=" + ghCallLogValue("acme/widgets"), "status=7"}; strings.Count(got.log, "\n") != 1 || slices.ContainsFunc(wants, func(want string) bool { return !strings.Contains(got.log, want) }) {
				t.Errorf("the call log reads %q, want one line naming %q", got.log, wants)
			}
		})
	}
}
