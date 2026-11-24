package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/approval"
)

// TestTheApprovalLinkIsJoinedOntoThePublicURL: the record's page every answer naming a pending
// request links is /approve/<record> joined onto the public URL's path, the configured query kept
// after it and the fragment dropped, where that path appended to the URL's text sat inside a query
// or a fragment the deployment set and named another page. A record id is escaped as a path
// element. A broker that serves no page (Deps.Approval nil) names none, and so does one whose
// public URL is no absolute URL with a host, which config.Load refuses.
func TestTheApprovalLinkIsJoinedOntoThePublicURL(t *testing.T) {
	t.Parallel()
	const record = "11111111-2222-4333-8444-555555555555"
	for _, c := range []struct{ name, public, want string }{
		{"query", "https://secrets.example/broker?sig=s1g-7Qz4k", "https://secrets.example/broker/approve/" + record + "?sig=s1g-7Qz4k"},
		{"query-trailing-slash", "https://secrets.example/broker/?sig=s1g-7Qz4k", "https://secrets.example/broker/approve/" + record + "?sig=s1g-7Qz4k"},
		{"fragment", "https://secrets.example/broker#pane", "https://secrets.example/broker/approve/" + record},
		{"no-query", "https://secrets.example/broker", "https://secrets.example/broker/approve/" + record},
		{"origin", "https://secrets.example", "https://secrets.example/approve/" + record},
		{"no-url", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := &server{deps: Deps{PublicURL: c.public, Approval: &approval.Page{}}}
			if got := s.approveURL(record); got != c.want {
				t.Errorf("the public URL %q names the record's page %q; want %q", c.public, got, c.want)
			}
		})
	}
	t.Run("no-page", func(t *testing.T) {
		t.Parallel()
		s := &server{deps: Deps{PublicURL: "https://secrets.example"}}
		if got := s.approveURL(record); got != "" {
			t.Errorf("a broker that serves no page names %q; want no page at all", got)
		}
	})
}
