package delivery

import "testing"

// TestGithubIssueURLReadsAURLInAnyCase holds githubIssueURL to what its comment says a citation
// is: `github.com/<owner>/<repo>/issues/<n>` as the host of its own URL, "with a scheme or
// without". A scheme and a host are both case-insensitive (RFC 3986) and github.com serves
// `HTTPS://GITHUB.COM/…`, so a body cites the same issue however it spells either; `main`
// credits every spelling of the scheme, because its expression holds no scheme at all, and
// neither `main` nor this head credits an upper-case host.
//
// The refusals are in the same table on purpose, each in both cases: a reading that credits
// `GITHUB.COM` must still refuse `EVILGITHUB.COM`, or it answers the finding this expression
// exists for.
//
// This test needs no database: it drives the expression, not the delivery store.
func TestGithubIssueURLReadsAURLInAnyCase(t *testing.T) {
	const citation = "github.com/acme/widgets/issues/7"
	for _, row := range []struct {
		name  string
		body  string
		cites bool
	}{
		{name: "a lower-case scheme", body: "See https://github.com/acme/widgets/issues/7 here.", cites: true},
		{name: "an upper-case scheme", body: "See HTTPS://github.com/acme/widgets/issues/7 here.", cites: true},
		{name: "a mixed-case scheme", body: "See Https://github.com/acme/widgets/issues/7 here.", cites: true},
		{name: "no scheme at all", body: "See github.com/acme/widgets/issues/7 here.", cites: true},
		{name: "a plain http scheme", body: "See http://github.com/acme/widgets/issues/7 here.", cites: true},
		{name: "a host whose name ends in github.com", body: "See https://evilgithub.com/acme/widgets/issues/7 here.", cites: false},
		{name: "a subdomain of github.com", body: "See https://pages.github.com/acme/widgets/issues/7 here.", cites: false},
		{name: "www.github.com", body: "See https://www.github.com/acme/widgets/issues/7 here.", cites: false},
		{name: "another host's path", body: "See https://example.test/github.com/acme/widgets/issues/7 here.", cites: false},
		{name: "another host's path under an upper-case scheme", body: "See HTTPS://example.test/github.com/acme/widgets/issues/7 here.", cites: false},
		{name: "another host's query", body: "See https://example.test/?u=github.com/acme/widgets/issues/7 here.", cites: false},
		{name: "another host's fragment", body: "See https://example.test/#github.com/acme/widgets/issues/7 here.", cites: false},
		{name: "userinfo before the host", body: "See https://user@github.com/acme/widgets/issues/7 here.", cites: false},
		{name: "a host name of its own ending in github.com", body: "See my-github.com/acme/widgets/issues/7 here.", cites: false},
		{name: "an upper-case host", body: "See HTTPS://GITHUB.COM/acme/widgets/issues/7 here.", cites: true},
		{name: "an upper-case host with no scheme", body: "See GITHUB.COM/acme/widgets/issues/7 here.", cites: true},
		{name: "a mixed-case host", body: "See https://GitHub.com/acme/widgets/issues/7 here.", cites: true},
		{name: "an upper-case host whose name ends in github.com", body: "See HTTPS://EVILGITHUB.COM/acme/widgets/issues/7 here.", cites: false},
		{name: "an upper-case subdomain", body: "See HTTPS://PAGES.GITHUB.COM/acme/widgets/issues/7 here.", cites: false},
		{name: "an upper-case www", body: "See HTTPS://WWW.GITHUB.COM/acme/widgets/issues/7 here.", cites: false},
		{name: "an upper-case host in another host's path", body: "See HTTPS://EXAMPLE.TEST/GITHUB.COM/acme/widgets/issues/7 here.", cites: false},
		{name: "an upper-case host in another host's query", body: "See HTTPS://EXAMPLE.TEST/?U=GITHUB.COM/acme/widgets/issues/7 here.", cites: false},
		{name: "an upper-case host in another host's fragment", body: "See HTTPS://EXAMPLE.TEST/#GITHUB.COM/acme/widgets/issues/7 here.", cites: false},
		{name: "an upper-case host after userinfo", body: "See HTTPS://USER@GITHUB.COM/acme/widgets/issues/7 here.", cites: false},
		{name: "a percent-encoded slash before the host", body: "See https://example.test/%2Fgithub.com/acme/widgets/issues/7 here.", cites: false},
		{name: "an upper-case percent-encoded slash before the host", body: "See HTTPS://EXAMPLE.TEST/%2FGITHUB.COM/acme/widgets/issues/7 here.", cites: false},
		{name: "github.com as the head of another host's name", body: "See https://github.com.evil.test/acme/widgets/issues/7 here.", cites: false},
		{name: "an upper-case head of another host's name", body: "See HTTPS://GITHUB.COM.EVIL.TEST/acme/widgets/issues/7 here.", cites: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			match := githubIssueURL.FindStringSubmatch(row.body)
			if !row.cites {
				if match != nil {
					t.Errorf("githubIssueURL cites %q in %q, want no citation", match[0], row.body)
				}
				return
			}
			if match == nil {
				t.Fatalf("githubIssueURL finds no citation in %q, want %s", row.body, citation)
			}
			cited := "github.com/" + match[1] + "/" + match[2] + "/issues/" + match[3]
			if cited != citation {
				t.Errorf("githubIssueURL cites %s in %q, want %s", cited, row.body, citation)
			}
		})
	}
}
