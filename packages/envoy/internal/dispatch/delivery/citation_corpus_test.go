package delivery

import (
	"slices"
	"testing"
)

// corpusRow is one body of the differential corpus with the citations the default branch credits
// for it. main is the answer citedIssues gave at the merge base f164e6a27756, read by restoring
// that commit's attribution.go into a workspace and running every body of this list through the
// same call:
//
//	jj file show -r f164e6a27756 packages/envoy/internal/dispatch/delivery/attribution.go > attribution.go
//	go test ./internal/dispatch/delivery/ -run TestZZCorpusProbe      # a scratch probe over this list
//
// hostIsNotGitHub marks a body whose authority before the issue path is not github.com, which is
// the one class this change refuses and the default branch credits: a name that merely ends in
// github.com, a subdomain, a name beginning with it, and a percent-encoded run that reads as
// another name. Those are what the CodeQL alert this change answers is about.
//
// What a failure of this test means, which a reader needs before editing anything here:
//
//   - a body the default branch credits and this code refuses is a DEFECT, unless its row carries
//     hostIsNotGitHub. Four review rounds of this change each found one, and each was a citation
//     somebody writes: a second citation joined by punctuation, one after a bare reference, one
//     inside brackets. Fix the code, not the row.
//   - a body this code credits and the default branch does not is an ADDITION, and this change
//     makes three of them deliberately: a host spelled in any case, a host carrying a port, and
//     userinfo before the host. An addition needs a row of its own in attribution_test.go saying
//     why it is wanted; it does not belong here.
//   - the golden list is another branch's behaviour rather than a contract, so it is dated: it is
//     f164e6a27756's answer and nothing else. Re-measure it with the command above when the base
//     moves, and never edit a golden entry by hand to make a red test green.
type corpusRow struct {
	body            string
	main            []string
	hostIsNotGitHub bool
}

// TestCitedIssuesAgreesWithTheDefaultBranch is the rule rather than a list of shapes: this code
// credits every citation the default branch credits, and refuses it only where the authority
// before the issue path is not github.com. The corpus is every body both citation test files
// hold, every example of this change's five review rounds, and the shapes each round measured by
// hand; it exists so the next change to this file is answerable without a sixth round finding a
// shape nobody thought of.
func TestCitedIssuesAgreesWithTheDefaultBranch(t *testing.T) {
	for _, row := range citationCorpus {
		t.Run(row.body, func(t *testing.T) {
			got := citedIssues("acme/widgets", row.body)
			for _, want := range row.main {
				credited := slices.Contains(got, want)
				switch {
				case row.hostIsNotGitHub && credited:
					t.Errorf("credits %s, whose authority is not github.com; the default branch credits it and this change exists to refuse it", want)
				case !row.hostIsNotGitHub && !credited:
					t.Errorf("drops %s, which the default branch credits: %v, got %v", want, row.main, got)
				}
			}
		})
	}
}

var citationCorpus = []corpusRow{
	{body: "github.com/sjawhar/envoy/internal/dispatch/events", main: []string{}},
	{body: "github.com/sjawhar/envoy/internal/dispatch/store", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/101", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/1030", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/103", main: []string{}},
	{body: "Follows https://github.com/acme/widgets/pull/1030.", main: []string{}},
	{body: "https://github.com/acme/widgets/issues/77", main: []string{"https://github.com/acme/widgets/issues/77"}},
	{body: "https://github.com/acme/other/issues/5", main: []string{"https://github.com/acme/other/issues/5"}},
	{body: "https://github.com/acme/widgets/issues/88", main: []string{"https://github.com/acme/widgets/issues/88"}},
	{body: "See https://github.com/acme/other/issues/5 for context.", main: []string{"https://github.com/acme/other/issues/5"}},
	{body: "},\n\t\t// A citation is the whole path of a URL whose host is github.com: a URL at the very start\n\t\t// of the body is one, scheme or none, and a host whose name merely ends in github.com is\n\t\t// not.\n\t\t{", main: []string{}},
	{body: "https://github.com/acme/other/issues/5 is the context.", main: []string{"https://github.com/acme/other/issues/5"}},
	{body: "github.com/acme/other/issues/5 is the context.", main: []string{"https://github.com/acme/other/issues/5"}},
	{body: "a lookalike host is not github.com", main: []string{}},
	{body: "See https://evilgithub.com/acme/widgets/issues/77 for context.", main: []string{"https://github.com/acme/widgets/issues/77"}, hostIsNotGitHub: true},
	{body: "a subdomain of github.com is not github.com", main: []string{}},
	{body: "See https://pages.github.com/acme/widgets/issues/77 for context.", main: []string{"https://github.com/acme/widgets/issues/77"}, hostIsNotGitHub: true},
	{body: "},\n\t\t// www.github.com serves the same pages as github.com, so a reader writing a citation by\n\t\t// hand is likelier to type it than any other lookalike. What the parse does with it is\n\t\t// pinned here rather than left to be rediscovered: it is a subdomain, and another host.\n\t\t{", main: []string{}},
	{body: "See https://www.github.com/acme/widgets/issues/77 for context.", main: []string{"https://github.com/acme/widgets/issues/77"}, hostIsNotGitHub: true},
	{body: "github.com in another host's path", main: []string{}},
	{body: "See https://example.test/github.com/acme/widgets/issues/77 for context.", main: []string{"https://github.com/acme/widgets/issues/77"}},
	{body: "github.com in another host's query", main: []string{}},
	{body: "See https://example.test/r?u=github.com/acme/widgets/issues/77 for context.", main: []string{"https://github.com/acme/widgets/issues/77"}},
	{body: "github.com in another host's fragment", main: []string{}},
	{body: "See https://example.test/r#github.com/acme/widgets/issues/77 for context.", main: []string{"https://github.com/acme/widgets/issues/77"}},
	{body: "},\n\t\t// Userinfo before the host is a citation, because the host after it is github.com and\n\t\t// that is where the reader goes; the host of `github.com@evil.test` is evil.test, which\n\t\t// citation_boundary_test.go holds.\n\t\t{", main: []string{}},
	{body: "See https://user@github.com/acme/widgets/issues/77 for context.", main: []string{"https://github.com/acme/widgets/issues/77"}},
	{body: "Unlinked #12, then #88, then https://github.com/acme/other/issues/5.", main: []string{"https://github.com/acme/widgets/issues/12", "https://github.com/acme/widgets/issues/88", "https://github.com/acme/other/issues/5"}},
	{body: "Reverts https://github.com/acme/widgets/pull/77.", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/110", main: []string{}},
	{body: "https://github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "https://github.com/a/b/issues/7", main: []string{"https://github.com/a/b/issues/7"}},
	{body: "https://github.com/c/d/issues/9", main: []string{"https://github.com/c/d/issues/9"}},
	{body: "https://github.com/acme/widgets/issues/12", main: []string{"https://github.com/acme/widgets/issues/12"}},
	{body: "See https://github.com/acme/widgets/blob/main/docs/issues/12 here.", main: []string{}},
	{body: "See https://github.com/acme/widgets/tree/fix/issues/123 here.", main: []string{}},
	{body: "See https://github.com/acme/widgets/wiki/issues/3 here.", main: []string{}},
	{body: "See https://github.com/acme/widgets/issues/7/x/y/issues/8 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See (github.com/acme/widgets/issues/7) here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See `github.com/acme/widgets/issues/7` here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See [the issue](github.com/acme/widgets/issues/7) here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See **github.com/acme/widgets/issues/7** here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "\u53c2\u7167github.com/acme/widgets/issues/7\u3067\u3059", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "Refs:github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "github.com/a/b/issues/7,github.com/c/d/issues/9", main: []string{"https://github.com/a/b/issues/7", "https://github.com/c/d/issues/9"}},
	{body: "github.com/a/b/issues/7;github.com/c/d/issues/9", main: []string{"https://github.com/a/b/issues/7", "https://github.com/c/d/issues/9"}},
	{body: "github.com/a/b/issues/7&github.com/c/d/issues/9", main: []string{"https://github.com/a/b/issues/7", "https://github.com/c/d/issues/9"}},
	{body: "github.com/a/b/issues/7+github.com/c/d/issues/9", main: []string{"https://github.com/a/b/issues/7", "https://github.com/c/d/issues/9"}},
	{body: "https://github.com/a/b/issues/7,github.com/c/d/issues/9", main: []string{"https://github.com/a/b/issues/7", "https://github.com/c/d/issues/9"}},
	{body: "(github.com/a/b/issues/7)(github.com/c/d/issues/9)", main: []string{"https://github.com/a/b/issues/7", "https://github.com/c/d/issues/9"}},
	{body: "#12,github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/12", "https://github.com/acme/widgets/issues/7"}},
	{body: "#12:github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/12", "https://github.com/acme/widgets/issues/7"}},
	{body: "Fixes (#12)github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/12", "https://github.com/acme/widgets/issues/7"}},
	{body: "See [acme/widgets#7](github.com/acme/widgets/issues/7)", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See [#7](github.com/acme/widgets/issues/7)", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "**#7**:github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "Related issues/PRs:github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "(//github.com/acme/widgets/issues/7)", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "[//github.com/acme/widgets/issues/7]", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://example.test/r?u=github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://example.test/r#github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://example.test/~github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://example.test/(github.com/acme/widgets/issues/7) here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://example.test/x@github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://github.com:99999/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://github.com:443443443/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://github.com:", main: []string{}},
	{body: "github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: ",github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "https://github.com/acme/widgets/pull/120", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/130", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/140", main: []string{}},
	{body: "https://github.com/acme/widgets/issues/143", main: []string{"https://github.com/acme/widgets/issues/143"}},
	{body: "https://github.com/acme/widgets/pull/149", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/150", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/160", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/180", main: []string{}},
	{body: "}, nil)\n\tif _, err := pool.Exec(ctx, `delete from issue_external_links where url like 'https://github.com/acme/widgets/pull/19_'`); err != nil {\n\t\tt.Fatalf(", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/194", main: []string{}},
	{body: "}, nil)\n\t// deliveryTestPool clears only the delivery tables: drop the link an earlier run added.\n\tif _, err := pool.Exec(ctx, `delete from issue_external_links where url = 'https://github.com/acme/widgets/pull/170'`); err != nil {\n\t\tt.Fatalf(", main: []string{}},
	{body: "https://github.com/acme/widgets/pull/170", main: []string{}},
	{body: "See /acme/widgets/issues/77 for context.", main: []string{}},
	{body: "/acme/widgets/issues/7 here.", main: []string{}},
	{body: "/a/b/issues/1", main: []string{}},
	{body: "See https://github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See HTTPS://github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See Https://github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See HTTPS://GITHUB.COM/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://GitHub.Com/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See GITHUB.COM/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See http://github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See //github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "Context (https://github.com/acme/widgets/issues/7).", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "Context [the issue](https://github.com/acme/widgets/issues/7).", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "Context <https://github.com/acme/widgets/issues/7>.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "Context \\\"https://github.com/acme/widgets/issues/7\\\".", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "[1]:https://github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "Refs:https://github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "<a href=https://github.com/acme/widgets/issues/7>the issue</a>", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "issue=https://github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://user@github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See HTTPS://USER@GITHUB.COM/acme/widgets/issues/7 here.", main: []string{}},
	{body: "a host whose name ends in github.com", main: []string{}},
	{body: "See https://evilgithub.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}, hostIsNotGitHub: true},
	{body: "See HTTPS://EVILGITHUB.COM/acme/widgets/issues/7 here.", main: []string{}},
	{body: "a subdomain of github.com", main: []string{}},
	{body: "See https://pages.github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}, hostIsNotGitHub: true},
	{body: "www.github.com", main: []string{}},
	{body: "See https://www.github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}, hostIsNotGitHub: true},
	{body: "www.github.com in upper case", main: []string{}},
	{body: "See HTTPS://WWW.GITHUB.COM/acme/widgets/issues/7 here.", main: []string{}},
	{body: "github.com as the head of another host's name", main: []string{}},
	{body: "See https://github.com.evil.test/acme/widgets/issues/7 here.", main: []string{}},
	{body: "a host name of its own ending in github.com", main: []string{}},
	{body: "See my-github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}, hostIsNotGitHub: true},
	{body: "github.com as the userinfo of another host", main: []string{}},
	{body: "See https://github.com@evil.test/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See HTTPS://GITHUB.COM@EVIL.TEST/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://example.test/github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See HTTPS://EXAMPLE.TEST/GITHUB.COM/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See HTTPS://EXAMPLE.TEST/~GITHUB.COM/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://example.test/(GitHub.com/acme/widgets/issues/7) here.", main: []string{}},
	{body: "See HTTPS://EXAMPLE.TEST/R?U=GITHUB.COM/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://example.test/a%2Fgithub.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}, hostIsNotGitHub: true},
	{body: "See HTTPS://EXAMPLE.TEST/A%2FGITHUB.COM/acme/widgets/issues/7 here.", main: []string{}},
	{body: "a github.com URL written inside another host's query is still written", main: []string{}},
	{body: "See https://evil.test/r?u=https://github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://github.com./acme/widgets/issues/7 here.", main: []string{}},
	{body: "a host with a port is github.com, the port being no part of the host", main: []string{}},
	{body: "See https://github.com:443/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://github.com/acme/widgets/issues/7/ here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See\\thttps://github.com/acme/widgets/issues/7\\there.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "a punycode host that reads like github.com is another host", main: []string{}},
	{body: "a percent-encoded host is no host net/url reads as github.com", main: []string{}},
	{body: "github.com percent-encoded into another host's userinfo", main: []string{}},
	{body: "See https://github.com%40evil.test/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://github.com\\\\@evil.test/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://github.com/acme/widgets/pull/7 here.", main: []string{}},
	{body: "See https://github.com/acme/widgets/tree/main/docs/issues/12 here.", main: []string{}},
	{body: "See https://github.com/acme/widgets/wiki/docs/issues/12 here.", main: []string{}},
	{body: "See `github.com/acme/widgets/issues/7` in backticks.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See **https://github.com/acme/widgets/issues/7** in bold.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "\u53c2\u7167https://github.com/acme/widgets/issues/7\u3067\u3059\u3002", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "https://github.com/acme/widgets/issues/9", main: []string{"https://github.com/acme/widgets/issues/9"}},
	{body: "github.com/acme/widgets/issues/7,github.com/acme/widgets/issues/9", main: []string{"https://github.com/acme/widgets/issues/7", "https://github.com/acme/widgets/issues/9"}},
	{body: "github.com/acme/widgets/issues/7;github.com/acme/widgets/issues/9", main: []string{"https://github.com/acme/widgets/issues/7", "https://github.com/acme/widgets/issues/9"}},
	{body: "github.com/acme/widgets/issues/7&github.com/acme/widgets/issues/9", main: []string{"https://github.com/acme/widgets/issues/7", "https://github.com/acme/widgets/issues/9"}},
	{body: "github.com/acme/widgets/issues/7+github.com/acme/widgets/issues/9", main: []string{"https://github.com/acme/widgets/issues/7", "https://github.com/acme/widgets/issues/9"}},
	{body: "https://github.com/acme/widgets/issues/7,github.com/acme/widgets/issues/9", main: []string{"https://github.com/acme/widgets/issues/7", "https://github.com/acme/widgets/issues/9"}},
	{body: "(github.com/acme/widgets/issues/7)(github.com/acme/widgets/issues/9)", main: []string{"https://github.com/acme/widgets/issues/7", "https://github.com/acme/widgets/issues/9"}},
	{body: " https://github.com/acme/widgets/issues/7", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://xn--github-8m9b.com/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://%67ithub.com/acme/widgets/issues/7 here.", main: []string{}},
	{body: "Fixes #12(github.com/acme/widgets/issues/7)", main: []string{"https://github.com/acme/widgets/issues/12", "https://github.com/acme/widgets/issues/7"}},
	{body: "See acme/widgets#7(github.com/acme/widgets/issues/7)", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "github.com/a/b/issues/7(github.com/c/d/issues/9)", main: []string{"https://github.com/a/b/issues/7", "https://github.com/c/d/issues/9"}},
	{body: "and/or(github.com/acme/widgets/issues/7)", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "Fixes #12<github.com/acme/widgets/issues/7>", main: []string{"https://github.com/acme/widgets/issues/12", "https://github.com/acme/widgets/issues/7"}},
	{body: "x/{github.com/acme/widgets/issues/7}", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://example.test/r?a=1&u=github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://example.test/a:github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://example.test/a,github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://example.test/r?q=see+github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://xn--guthub-9db.com/acme/widgets/issues/7 here.", main: []string{}},
	{body: "See https://example.test/%2Fgithub.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}, hostIsNotGitHub: true},
	{body: "(github.com/acme/widgets/issues/7)", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "`github.com/acme/widgets/issues/7`", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "**github.com/acme/widgets/issues/7**", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See\u00a0github.com/acme/widgets/issues/7 here.", main: []string{"https://github.com/acme/widgets/issues/7"}},
	{body: "See https://github.com/acme/widgets/compare/v1/issues/12 here.", main: []string{}},
}
