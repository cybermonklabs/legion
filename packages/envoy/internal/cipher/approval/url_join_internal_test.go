package approval

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/store"
)

// TestTheSignInCallbackIsJoinedOntoThePublicURL: BROKER_PUBLIC_URL is the deployment's, and it can
// carry a path, a query or a fragment. The sign-in's redirect URI is that URL's path with
// /signin/callback joined onto it, the query kept and the fragment dropped, which is what RFC 6749
// section 3.1.2 says of a redirection endpoint: https://host/broker?sig=... redirects to
// https://host/broker/signin/callback?sig=... (query), and so does https://host/broker/?sig=...
// (query-trailing-slash), where the path appended to the URL's text sat inside the query
// (https://host/broker?sig=.../signin/callback, a redirect URI naming the path /broker with the
// query's value changed); https://host/broker#pane redirects to https://host/broker/signin/callback
// with no fragment, which RFC 6749 forbids there (fragment). A URL with neither redirects where it
// did (no-query), and so does one with no path at all (origin).
func TestTheSignInCallbackIsJoinedOntoThePublicURL(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, public, want string }{
		{"query", "https://secrets.example/broker?sig=s1g-7Qz4k", "https://secrets.example/broker/signin/callback?sig=s1g-7Qz4k"},
		{"query-trailing-slash", "https://secrets.example/broker/?sig=s1g-7Qz4k", "https://secrets.example/broker/signin/callback?sig=s1g-7Qz4k"},
		{"fragment", "https://secrets.example/broker#pane", "https://secrets.example/broker/signin/callback"},
		{"no-query", "https://secrets.example/broker", "https://secrets.example/broker/signin/callback"},
		{"origin", "https://secrets.example", "https://secrets.example/signin/callback"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			page, err := New(Config{PublicURL: c.public, Store: &store.Store{}, Machine: &requests.Machine{}})
			if err != nil {
				t.Fatalf("New(%q) = %v", c.public, err)
			}
			if page.redirectURL != c.want {
				t.Errorf("the public URL %q gives the redirect URI %q; want %q", c.public, page.redirectURL, c.want)
			}
		})
	}
}
