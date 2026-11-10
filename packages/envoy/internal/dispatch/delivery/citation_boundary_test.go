package delivery

import (
	"slices"
	"strings"
	"testing"
	"time"
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
		{name: "a host with a port is github.com, the port being no part of the host", body: "See https://github.com:443/acme/widgets/issues/7 here.", credit: true},
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

// TestCitedIssuesCreditsEveryCitationItsWordEnds holds the citations a body carries when one
// citation ends and another begins without a space between them. Running text separates items
// with punctuation — a comma, a semicolon, an ampersand, a plus, a closing bracket — and a bare
// `#N` reference or a Markdown link may stand immediately before a URL. `main` credits every row
// below, so a row that loses one is a citation this change took away from the default branch.
//
// The expected lists are what `main` answers, measured at origin/main, and they are written out
// rather than described: a reader comparing two implementations needs the answer, not a rule.
func TestCitedIssuesCreditsEveryCitationItsWordEnds(t *testing.T) {
	const repo = "acme/widgets"
	const seven = "https://github.com/acme/widgets/issues/7"
	const nine = "https://github.com/acme/widgets/issues/9"
	const twelve = "https://github.com/acme/widgets/issues/12"
	for _, row := range []struct {
		name string
		body string
		want []string
	}{
		{name: "two citations joined by a comma", body: "github.com/acme/widgets/issues/7,github.com/acme/widgets/issues/9", want: []string{seven, nine}},
		{name: "joined by a semicolon", body: "github.com/acme/widgets/issues/7;github.com/acme/widgets/issues/9", want: []string{seven, nine}},
		{name: "joined by an ampersand", body: "github.com/acme/widgets/issues/7&github.com/acme/widgets/issues/9", want: []string{seven, nine}},
		{name: "joined by a plus", body: "github.com/acme/widgets/issues/7+github.com/acme/widgets/issues/9", want: []string{seven, nine}},
		{name: "the first carrying a scheme", body: "https://github.com/acme/widgets/issues/7,github.com/acme/widgets/issues/9", want: []string{seven, nine}},
		{name: "each in brackets of its own", body: "(github.com/acme/widgets/issues/7)(github.com/acme/widgets/issues/9)", want: []string{seven, nine}},
		{name: "after a bare reference and a comma", body: "#12,github.com/acme/widgets/issues/7", want: []string{twelve, seven}},
		{name: "after a bracketed bare reference", body: "Fixes (#12)github.com/acme/widgets/issues/7", want: []string{twelve, seven}},
		{name: "as the target of a Markdown link", body: "See [acme/widgets#7](github.com/acme/widgets/issues/7)", want: []string{seven}},
		{name: "as the target of a link whose text is a bare reference", body: "See [#7](github.com/acme/widgets/issues/7)", want: []string{seven}},
		{name: "after bold markers and a colon", body: "**#7**:github.com/acme/widgets/issues/7", want: []string{seven}},
		{name: "after a word that ends in a slash and a colon", body: "Related issues/PRs:github.com/acme/widgets/issues/7", want: []string{seven}},
		{name: "scheme-relative inside brackets", body: "(//github.com/acme/widgets/issues/7)", want: []string{seven}},
		{name: "scheme-relative inside square brackets", body: "[//github.com/acme/widgets/issues/7]", want: []string{seven}},

		// The refusals that must survive whatever ends a word: a host inside another URL's path
		// is not a citation, however the prose brackets it.
		{name: "another host's path, bracketed", body: "See https://example.test/(GitHub.com/acme/widgets/issues/7) here.", want: nil},
		{name: "another host's path, plain", body: "See https://example.test/github.com/acme/widgets/issues/7 here.", want: nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			if urls := citedIssues(repo, row.body); !slices.Equal(urls, row.want) {
				t.Errorf("citedIssues(%q, %q) = %v, want %v", repo, row.body, urls, row.want)
			}
		})
	}
}

// TestCitedIssuesCostsNoMoreForACitationFarFromASpace is the cost test, written so that it fails
// on the shape it guards against rather than on a clock. Two bodies of the same length carry the
// same 200 citations and differ only in what separates them: spaces, which end a word at once,
// and commas, which do not. A reader that walks back to the nearest space pays the body's length
// for every match and the comma body costs many times the space one; a reader that reads a
// bounded run pays the same for both.
//
// Measured in this pod at this head, best of three each: 5.83 ms and 5.05 ms, a ratio under one.
// The same bodies against the walk this change replaced (ebb65528's reader): 5.74 ms and
// 134.77 ms, a ratio of 23. The bound below is 8, which no linear reader approaches and no
// walking one meets.
func TestCitedIssuesCostsNoMoreForACitationFarFromASpace(t *testing.T) {
	const repo = "acme/widgets"
	one := "github.com/acme/widgets/issues/7"
	prose := strings.Repeat("a", 65536)
	bySpace := prose + " " + strings.TrimSuffix(strings.Repeat(one+" ", 200), " ")
	byComma := prose + "," + strings.TrimSuffix(strings.Repeat(one+",", 200), ",")
	best := func(body string) time.Duration {
		var quickest time.Duration
		for attempt := 0; attempt < 3; attempt++ {
			started := time.Now()
			citedIssues(repo, body)
			if took := time.Since(started); quickest == 0 || took < quickest {
				quickest = took
			}
		}
		return quickest
	}
	space, comma := best(bySpace), best(byComma)
	if comma > 8*space {
		t.Errorf("citing %d bytes separated by commas took %v against %v separated by spaces, more than 8 times as long: a match is reading back to the nearest space rather than a bounded run", len(byComma), comma, space)
	}
}

// TestCitedIssuesReadsAHostNameOfAHostNamesLength holds the two ends of what a host may be. A
// citation after a word of 65,533 bytes is credited, because the word before it is no part of
// the host; a run of 65,533 host bytes ending in the citation is refused, because a host name
// that long is no host name. The bodies are built rather than written out, since a row of a
// table cannot hold 65,533 bytes and still be read by a person.
func TestCitedIssuesReadsAHostNameOfAHostNamesLength(t *testing.T) {
	const repo = "acme/widgets"
	const cited = "https://github.com/acme/widgets/issues/7"
	t.Run("a citation after a word of 65,533 bytes", func(t *testing.T) {
		body := strings.Repeat("a", 65533) + " https://github.com/acme/widgets/issues/7"
		if urls := citedIssues(repo, body); len(urls) != 1 || urls[0] != cited {
			t.Errorf("citedIssues over a 65,533-byte word = %v, want %s and nothing else", urls, cited)
		}
	})
	t.Run("a host name of 65,533 bytes is no host name", func(t *testing.T) {
		body := strings.Repeat("a", 65533) + "github.com/acme/widgets/issues/7"
		if urls := citedIssues(repo, body); len(urls) != 0 {
			t.Errorf("citedIssues over a 65,533-byte host name = %v, want none", urls)
		}
	})
}
