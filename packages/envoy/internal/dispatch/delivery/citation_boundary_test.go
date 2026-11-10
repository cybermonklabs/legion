package delivery

import (
	"slices"
	"testing"
)

// TestCitedIssuesReadsAURLAsAURL holds the whole citation decision, not the expression that
// happens to carry it: `citedIssues` credits `github.com/<owner>/<repo>/issues/<n>` as the host
// of its own URL, in whatever case the writer spelled the scheme and the host, and whatever
// punctuation of running text surrounds it. A scheme and a host are both case-insensitive
// (RFC 3986) and github.com answers `HTTPS://GITHUB.COM/…`.
//
// The refusals sit in the same table as the credits on purpose, each in both cases: a reading
// that credits `GITHUB.COM` while admitting `EVILGITHUB.COM`, or one that credits a citation by
// admitting every host inside another URL's path, answers the finding this decision exists for.
// `www.github.com` and `pages.github.com` are refused, which review round one settled.
//
// This test needs no database: `citedIssues` is a pure function of the body.
func TestCitedIssuesReadsAURLAsAURL(t *testing.T) {
	const repo = "acme/widgets"
	const cited = "https://github.com/acme/widgets/issues/7"
	for _, row := range []struct {
		name   string
		body   string
		credit bool
	}{
		// A citation, however the writer spelled it.
		{name: "a lower-case scheme", body: "See https://github.com/acme/widgets/issues/7 here.", credit: true},
		{name: "an upper-case scheme", body: "See HTTPS://github.com/acme/widgets/issues/7 here.", credit: true},
		{name: "a mixed-case scheme", body: "See Https://github.com/acme/widgets/issues/7 here.", credit: true},
		{name: "an upper-case host", body: "See HTTPS://GITHUB.COM/acme/widgets/issues/7 here.", credit: true},
		{name: "a mixed-case host", body: "See https://GitHub.Com/acme/widgets/issues/7 here.", credit: true},
		{name: "no scheme at all", body: "See github.com/acme/widgets/issues/7 here.", credit: true},
		{name: "an upper-case host with no scheme", body: "See GITHUB.COM/acme/widgets/issues/7 here.", credit: true},
		{name: "a plain http scheme", body: "See http://github.com/acme/widgets/issues/7 here.", credit: true},
		{name: "a scheme-relative URL", body: "See //github.com/acme/widgets/issues/7 here.", credit: true},

		// A citation, whatever running text puts around it.
		{name: "in parentheses", body: "Context (https://github.com/acme/widgets/issues/7).", credit: true},
		{name: "in a Markdown link", body: "Context [the issue](https://github.com/acme/widgets/issues/7).", credit: true},
		{name: "in an autolink", body: "Context <https://github.com/acme/widgets/issues/7>.", credit: true},
		{name: "in quotes", body: "Context \"https://github.com/acme/widgets/issues/7\".", credit: true},
		{name: "a link reference definition", body: "[1]:https://github.com/acme/widgets/issues/7", credit: true},
		{name: "after Refs:", body: "Refs:https://github.com/acme/widgets/issues/7", credit: true},
		{name: "an unquoted href", body: "<a href=https://github.com/acme/widgets/issues/7>the issue</a>", credit: true},
		{name: "after a key and an equals sign", body: "issue=https://github.com/acme/widgets/issues/7", credit: true},
		{name: "userinfo before the host", body: "See https://user@github.com/acme/widgets/issues/7 here.", credit: true},
		{name: "upper-case userinfo and host", body: "See HTTPS://USER@GITHUB.COM/acme/widgets/issues/7 here.", credit: true},

		// Another host, however it carries those bytes.
		{name: "a host whose name ends in github.com", body: "See https://evilgithub.com/acme/widgets/issues/7 here.", credit: false},
		{name: "that host in upper case", body: "See HTTPS://EVILGITHUB.COM/acme/widgets/issues/7 here.", credit: false},
		{name: "a subdomain of github.com", body: "See https://pages.github.com/acme/widgets/issues/7 here.", credit: false},
		{name: "www.github.com", body: "See https://www.github.com/acme/widgets/issues/7 here.", credit: false},
		{name: "www.github.com in upper case", body: "See HTTPS://WWW.GITHUB.COM/acme/widgets/issues/7 here.", credit: false},
		{name: "github.com as the head of another host's name", body: "See https://github.com.evil.test/acme/widgets/issues/7 here.", credit: false},
		{name: "a host name of its own ending in github.com", body: "See my-github.com/acme/widgets/issues/7 here.", credit: false},

		// The same bytes inside another host's URL.
		{name: "another host's path", body: "See https://example.test/github.com/acme/widgets/issues/7 here.", credit: false},
		{name: "another host's path in upper case", body: "See HTTPS://EXAMPLE.TEST/GITHUB.COM/acme/widgets/issues/7 here.", credit: false},
		{name: "another host's path after a tilde", body: "See HTTPS://EXAMPLE.TEST/~GITHUB.COM/acme/widgets/issues/7 here.", credit: false},
		{name: "another host's path inside brackets", body: "See https://example.test/(GitHub.com/acme/widgets/issues/7) here.", credit: false},
		{name: "another host's query", body: "See https://example.test/r?u=github.com/acme/widgets/issues/7 here.", credit: false},
		{name: "another host's query in upper case", body: "See HTTPS://EXAMPLE.TEST/R?U=GITHUB.COM/acme/widgets/issues/7 here.", credit: false},
		{name: "another host's fragment", body: "See https://example.test/r#github.com/acme/widgets/issues/7 here.", credit: false},
		{name: "a percent-encoded slash before the host", body: "See https://example.test/a%2Fgithub.com/acme/widgets/issues/7 here.", credit: false},
		{name: "a percent-encoded slash in upper case", body: "See HTTPS://EXAMPLE.TEST/A%2FGITHUB.COM/acme/widgets/issues/7 here.", credit: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			urls := citedIssues(repo, row.body)
			if row.credit {
				if !slices.Contains(urls, cited) {
					t.Errorf("citedIssues(%q, %q) = %v, want it to carry %s", repo, row.body, urls, cited)
				}
				return
			}
			if slices.Contains(urls, cited) {
				t.Errorf("citedIssues(%q, %q) = %v, want no citation of %s", repo, row.body, urls, cited)
			}
		})
	}
}
