package ghroute

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
)

// TargetOf says a call writes nothing only for a call in a read grammar (writesNothing): a leaf
// on readLeaves typed by its canonical path with only the options its grammar names, each value
// in the word after its option, or a `gh api` GET of a relative REST path given only the options
// apiReadOptions names, each word read as typed. It says every other call writes: a listed leaf
// typed in another shape, one given --web, and every gh api call of the three cross products with
// a guarded option or a guarded endpoint, each option before and after each endpoint, among them.
func TestGhWritesNothingReadsTheLeafAndTheMethod(t *testing.T) {
	guardedAPI := slices.Concat(
		ghroutetest.APICalls(ghroutetest.APIGuardedSpellings, ghroutetest.APIReadEndpoints),
		ghroutetest.APICalls(ghroutetest.APIReadSpellings, ghroutetest.APIGuardedEndpoints),
		ghroutetest.APICalls(ghroutetest.APIGuardedSpellings, ghroutetest.APIGuardedEndpoints),
		ghroutetest.GuardedAPICalls(),
	)
	for _, calls := range []struct {
		argvs [][]string
		want  bool
	}{
		{ghroutetest.ReadCalls(), true},
		{ghroutetest.GuardedCalls, false},
		{ghroutetest.GuardedLeafSpellings, false},
		{guardedAPI, false},
	} {
		for _, argv := range calls.argvs {
			target, err := TargetOf(argv, nil, savedGhHelp, noGitRepository, ghroutetest.OwnersOwnDirectories)
			if err != nil {
				t.Errorf("TargetOf(%q) = %v", argv, err)
				continue
			}
			if target.WritesNothing != calls.want {
				t.Errorf("TargetOf(%q) writes nothing = %v, want %v", argv, target.WritesNothing, calls.want)
			}
		}
	}
}

// graphQLQueryRead admits a query of GitHub's GraphQL API that writes nothing (R4 of
// dispatch://INFRA-3399), and nothing else: `api graphql` typed first and exactly, then only raw
// and typed fields, headers that name no method override and no Content-Length, the output
// filters and the pagination switches, in each of their spellings and each value in the next
// word; exactly one raw `query` field and no other field naming the query; no typed value gh
// reads from a file; and a query whose text holds neither `mutation` nor `subscription`, in any
// case and wherever it stands. A call it admits is one TargetOf reads, and still one that does
// not write nothing by writesNothing's rule, which a caller's guard reads the call by.
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
		if got := graphQLQueryRead(tc.argv); got != tc.want {
			t.Errorf("graphQLQueryRead(%q) = %v, want %v", tc.argv, got, tc.want)
		}
		if !tc.want {
			continue
		}
		if target, err := TargetOf(tc.argv, nil, savedGhHelp, noGitRepository, ghroutetest.OwnersOwnDirectories); err != nil || target.WritesNothing {
			t.Errorf("TargetOf(%q) = %+v, %v; want a call it reads, which writes nothing by no rule of writesNothing's", tc.argv, target, err)
		}
	}
}

// The read rule is proven against the check the worker image pins: the tag ReadsProvenCheckTag
// names is worker.Dockerfile's `ARG LEAK_CHECK_TAG`, ReadsProvenCheckSHA256 is the sha256 of a
// public_leak_check.py, and it is the sha256 of the public_leak_check.py the tests' port of the
// check was written against (ghroutetest.PinnedCheckPortSHA256). That sha256 is the gate
// `legion gh` hands the guard for every call, whatever checker a role is told. A bump of the pin
// fails here until the tag, the sha256 and that port all move.
func TestGhReadsAreProvenAgainstThePinnedCheck(t *testing.T) {
	const prove = "follow the steps in ReadsProvenCheckTag's comment (reads.go): read the definitions it lists and every definition they reach in the new check, port each change to what they read as a read into ghroutetest.PinnedCheckReads and the new sha256 into ghroutetest.PinnedCheckPortSHA256, run INFRA-3562's xcheck4069 harness (xcheck4069.tar.gz) against the check with 0 mismatching rows, then name its tag and the sha256 of its public_leak_check.py in ReadsProvenCheckTag and ReadsProvenCheckSHA256"
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "docker", "worker.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`(?m)^ARG LEAK_CHECK_TAG=(\S+)$`).FindSubmatch(dockerfile)
	if pinned == nil {
		t.Fatal("worker.Dockerfile holds no ARG LEAK_CHECK_TAG=<tag> line")
	}
	if string(pinned[1]) != ReadsProvenCheckTag {
		t.Errorf("the worker image pins %s, and the read rule was proven against %s: for %s, %s", pinned[1], ReadsProvenCheckTag, pinned[1], prove)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(ReadsProvenCheckSHA256) {
		t.Errorf("ReadsProvenCheckSHA256 is %q, want 64 lowercase hex digits", ReadsProvenCheckSHA256)
	}
	if ghroutetest.PinnedCheckPortSHA256 != ReadsProvenCheckSHA256 {
		t.Errorf("ReadsProvenCheckSHA256 names the public_leak_check.py whose sha256 is %s, and ghroutetest.PinnedCheckReads ports the one whose sha256 is %s: for %s, %s", ReadsProvenCheckSHA256, ghroutetest.PinnedCheckPortSHA256, ReadsProvenCheckTag, prove)
	}
}

// ghroutetest.ReadLeafCalls holds a call of every leaf readLeaves lists and of no other, each call
// runs the leaf it is keyed by, and ghroutetest.GuardedCalls holds a write of every family of
// commands the list reads from but search, which has none.
func TestGhReadLeafCallsCoverEveryReadLeaf(t *testing.T) {
	for leaf := range readLeaves {
		if _, ok := ghroutetest.ReadLeafCalls[leaf]; !ok {
			t.Errorf("readLeaves lists %q, and ghroutetest.ReadLeafCalls holds no call of it", leaf)
		}
	}
	families := map[string]bool{}
	for leaf, argv := range ghroutetest.ReadLeafCalls {
		if _, listed := readLeaves[leaf]; !listed {
			t.Errorf("ghroutetest.ReadLeafCalls holds %q, which readLeaves does not list", leaf)
		}
		if target, err := TargetOf(argv, nil, savedGhHelp, noGitRepository, ghroutetest.OwnersOwnDirectories); err != nil || strings.Join(target.Command, " ") != leaf {
			t.Errorf("TargetOf(%q) = %+v, %v; want the leaf %q", argv, target, err, leaf)
		}
		families[strings.Fields(leaf)[0]] = true
	}
	delete(families, "search")
	for family := range families {
		if !slices.ContainsFunc(ghroutetest.GuardedCalls, func(argv []string) bool { return argv[0] == family }) {
			t.Errorf("ghroutetest.GuardedCalls holds no write of the %s commands, whose reads readLeaves lists", family)
		}
	}
}

// Every option a read grammar names (readLeaves, apiReadOptions) is one its leaf's help lists by
// that spelling, takes a value exactly where the grammar says it does and never only after `=`
// (pflag's NoOptDefVal), and is neither --web, nor --help, nor an option whose help names the
// browser, nor one whose value can name a file gh sends (sentFileOptions): so gh reads each word
// of a call in the grammar as the grammar does, such a call opens no browser, and it sends no
// file, which Target.WritesNothing relies on. A gh whose help moves an option a grammar names
// fails this once its help is saved here.
func TestGhReadGrammarNamesEachOptionAsItsHelpDoes(t *testing.T) {
	grammars := maps.Clone(readLeaves)
	grammars["api"] = apiReadOptions
	for leaf, options := range grammars {
		cmd, err := readGhCommand(strings.Fields(leaf), ghHelpWhat(strings.Fields(leaf)), savedGhHelp)
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
			if _, sends := sentFileOptions[name]; sends {
				t.Errorf("gh %s's read grammar names %s, whose value can name a file gh sends", leaf, spelling)
			}
		}
	}
}

// ghSpelling is a call as typed (argv) and the leaf path its words resolve to (path).
type ghSpelling struct{ argv, path []string }

// ghAPISpellingGroups are the groups of words ghGeneratedSpellings writes gh api calls of: each
// read option apiReadOptions names, its value in the next word; and the shapes found to divide
// gh's parse and the check's collect, and those next to them: an option with an empty value or
// with none, `--`, a single-dash word holding `=`, a value joined to its option or beginning with
// `-`, a method other than exactly GET, a field, an input, a second endpoint and a whole URL.
var ghAPISpellingGroups = [][]string{
	{"--paginate"}, {"--slurp"}, {"--jq", ".x"}, {"-q", ".[].number"}, {"--template", "{{.}}"}, {"-t", "{{.}}"}, {"--cache", "1h"},
	{"-X", "GET"}, {"--method", "GET"}, {"-H", "Accept: application/json"}, {"--header", "Accept: application/json"},
	{"-q", ""}, {"--jq", ""}, {"-H", ""}, {"-q"}, {"-H"}, {"-X"},
	{"--"}, {"-i=false"}, {"-X=GET"}, {"--jq=.x"}, {"-q.x"}, {"-XGET"}, {"-H", "-x"},
	{"-XPOST"}, {"-X", "POST"}, {"-X", "get"}, {"--method=GET"}, {"-f", "x=y"}, {"--input", "-"},
	{"graphql"}, {"https://api.github.com/repos/acme/widgets/pulls"},
}

// ghGeneratedSpellings are the gh api calls of up to three groups of ghAPISpellingGroups, in every
// order and with repetition, with the endpoint repos/acme/widgets/pulls before, between or after
// them; and each leaf of readLeaves, by its call in ghroutetest.ReadLeafCalls, given each option
// its grammar names, with a value where it takes one.
func ghGeneratedSpellings() []ghSpelling {
	var spellings []ghSpelling
	api := []string{"api"}
	var grow func(groups [][]string)
	grow = func(groups [][]string) {
		for at := range len(groups) + 1 {
			argv := slices.Concat(api, slices.Concat(groups[:at]...), []string{ghroutetest.APIReadEndpoints[0]}, slices.Concat(groups[at:]...))
			spellings = append(spellings, ghSpelling{argv: argv, path: api})
		}
		if len(groups) < 3 {
			for _, group := range ghAPISpellingGroups {
				grow(append(slices.Clone(groups), group))
			}
		}
	}
	grow(nil)
	for _, leaf := range slices.Sorted(maps.Keys(readLeaves)) {
		options := readLeaves[leaf]
		for _, option := range slices.Sorted(maps.Keys(options)) {
			argv := append(slices.Clone(ghroutetest.ReadLeafCalls[leaf]), option)
			if options[option] {
				argv = append(argv, "a-value")
			}
			spellings = append(spellings, ghSpelling{argv: argv, path: strings.Fields(leaf)})
		}
	}
	return spellings
}

// A call counts as a read only where the shared check counts it as one, so no read spends a
// visibility read in the check either.
//
// Over the spellings ghGeneratedSpellings writes, the cross products of gh api's read options and
// of the shapes found to divide gh's parse and collect's, and each listed leaf with each option
// its grammar names, every call writesNothing admits as a read, whether or not its parse would
// succeed, is one ghroutetest.PinnedCheckReads reads as a read too: 0 calls read otherwise. So is
// every call of ghroutetest.ReadCalls, which cmd/legion's TestLegionGhChecksAReadAndReadsNoVisibility
// runs end to end. This is the test that holds the gh api grammar, -X/--method GET and -H/--header
// among it, to the pinned check, and it starts no process. What a call outside the read grammar
// costs end to end, against a stand-in check that reads it as the pinned check does, is
// cmd/legion's TestLegionGhReadsOnlyWhatTheSharedCheckReadsAsARead.
func TestGhReadsOnlyWhatTheSharedCheckReadsAsARead(t *testing.T) {
	spellings := ghGeneratedSpellings()
	admitted, mismatches := 0, 0
	for _, spelling := range spellings {
		if !writesNothing(spelling.argv, spelling.path) {
			continue
		}
		admitted++
		if !ghroutetest.PinnedCheckReads(spelling.argv) {
			mismatches++
			t.Errorf("ghroute reads %q as a read, and the pinned check's gh_guard as no read", spelling.argv)
		}
	}
	if admitted == 0 {
		t.Fatalf("writesNothing admits none of %d generated spellings", len(spellings))
	}
	t.Logf("%d spellings generated, %d admitted as reads, %d of them read otherwise by the check", len(spellings), admitted, mismatches)
	for _, args := range ghroutetest.ReadCalls() {
		if !ghroutetest.PinnedCheckReads(args) {
			t.Errorf("ghroute's read %q is no read to the pinned check's gh_guard", args)
		}
	}
}
