package delivery

import (
	"strings"
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
		{name: "github.com as the userinfo of another host", body: "See https://github.com@evil.test/acme/widgets/issues/7 here.", credit: false},
		{name: "that confusion in upper case", body: "See HTTPS://GITHUB.COM@EVIL.TEST/acme/widgets/issues/7 here.", credit: false},

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

		// Two readings the architect settled rather than left to chance, each pinned so that a
		// later reader who changes one sees a row go red and knows to ask.
		{name: "a github.com URL written inside another host's query is still written", body: "See https://evil.test/r?u=https://github.com/acme/widgets/issues/7 here.", credit: true},
		{name: "the DNS trailing dot is another host, as it is on main", body: "See https://github.com./acme/widgets/issues/7 here.", credit: false},

		// Eight shapes measured in this pod against the parse, each row the answer the code gives
		// today rather than one anybody wishes it gave.
		{name: "a host with a port is github.com, a credit main refused and this change gives", body: "See https://github.com:443/acme/widgets/issues/7 here.", credit: true},
		{name: "a citation keeps its meaning with a trailing slash", body: "See https://github.com/acme/widgets/issues/7/ here.", credit: true},
		{name: "a tab delimits the token as a space does", body: "See\thttps://github.com/acme/widgets/issues/7\there.", credit: true},
		{name: "a punycode host that reads like github.com is another host", body: "See https://xn--github-8m9b.com/acme/widgets/issues/7 here.", credit: false},
		{name: "a percent-encoded host is no host net/url reads as github.com", body: "See https://%67ithub.com/acme/widgets/issues/7 here.", credit: false},
		{name: "github.com percent-encoded into another host's userinfo", body: "See https://github.com%40evil.test/acme/widgets/issues/7 here.", credit: false},
		{name: "a backslash before the userinfo at-sign leaves the host another's", body: "See https://github.com\\@evil.test/acme/widgets/issues/7 here.", credit: false},
		{name: "a pull-request URL is no issue citation, and this row records that rather than asking for it", body: "See https://github.com/acme/widgets/pull/7 here.", credit: false},

		// The path the expression found must be the URL's whole path, which is what tells a citation
		// from a repository file whose name ends the same way.
		{name: "a blob path that ends in an issues segment is a file, not a citation", body: "See https://github.com/acme/widgets/blob/main/docs/issues/12 here.", credit: false},
		{name: "a tree path the same", body: "See https://github.com/acme/widgets/tree/main/docs/issues/12 here.", credit: false},
		{name: "a wiki path the same", body: "See https://github.com/acme/widgets/wiki/docs/issues/12 here.", credit: false},
		{name: "a path carrying two issue segments cites the URL it is, once", body: "See https://github.com/acme/widgets/issues/7/x/y/issues/8 here.", credit: true},

		// Running text puts punctuation of its own around a URL, and none of it ends the citation.
		{name: "between backticks", body: "See `github.com/acme/widgets/issues/7` in backticks.", credit: true},
		{name: "between bold markers", body: "See **https://github.com/acme/widgets/issues/7** in bold.", credit: true},
		{name: "after Refs: with no scheme", body: "Refs:github.com/acme/widgets/issues/7", credit: true},
		{name: "after a non-breaking space", body: "See\u00a0https://github.com/acme/widgets/issues/7\u00a0here.", credit: true},
		{name: "after a line separator", body: "See\u2028https://github.com/acme/widgets/issues/7 here.", credit: true},
		{name: "inside CJK text with no space in it", body: "参照https://github.com/acme/widgets/issues/7です。", credit: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			urls := citedIssues(repo, row.body)
			if row.credit {
				if len(urls) != 1 || urls[0] != cited {
					t.Errorf("citedIssues(%q, %q) = %v, want %s and nothing else", repo, row.body, urls, cited)
				}
				return
			}
			if len(urls) != 0 {
				t.Errorf("citedIssues(%q, %q) = %v, want no citation at all", repo, row.body, urls)
			}
		})
	}
}

// TestCitedIssuesReadsOneMatchInBoundedWork holds the two ends of the bound the citation reader
// works under: a body may be any size and a match still costs one bounded read, and a URL longer
// than that bound is no citation at all. The bodies are built rather than written out, because a
// row of a table cannot hold 65,533 bytes and still be read by a person.
func TestCitedIssuesReadsOneMatchInBoundedWork(t *testing.T) {
	const repo = "acme/widgets"
	const cited = "https://github.com/acme/widgets/issues/7"
	t.Run("a citation after a word of 65,533 bytes", func(t *testing.T) {
		body := strings.Repeat("a", 65533) + " https://github.com/acme/widgets/issues/7"
		if urls := citedIssues(repo, body); len(urls) != 1 || urls[0] != cited {
			t.Errorf("citedIssues over a 65,533-byte word = %v, want %s and nothing else", urls, cited)
		}
	})
	t.Run("a body of 65,533 bytes with no space in it cites nothing", func(t *testing.T) {
		body := strings.Repeat("a", 65533) + "github.com/acme/widgets/issues/7"
		if urls := citedIssues(repo, body); len(urls) != 0 {
			t.Errorf("citedIssues over a 65,533-byte word with the citation inside it = %v, want none: a URL that long is no citation", urls)
		}
	})
}
