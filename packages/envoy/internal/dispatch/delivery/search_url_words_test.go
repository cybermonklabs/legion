package delivery

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestASearchResultURLReconcileCannotReadPrintsNoneOfIt: a merged pull request a search answered
// with an HTML URL that echoes redacttest's credentials, and that reconcile cannot read as
// https://github.com/<owner>/<repo>/pull/<n> (on another host, elsewhere; naming no owner,
// no-owner; with no /pull/ segment, no-pull), is refused naming the URL by what it is, and the
// refusal holds no run of a credential. The refusal quoted the URL. The twin shows each URL holds a
// credential and is still refused, so the row can see one printed. Captures
// delivery.search-url/<case>.
func TestASearchResultURLReconcileCannotReadPrintsNoneOfIt(t *testing.T) {
	cases := []struct{ name, url string }{
		{"elsewhere", "https://" + redacttest.User + ":" + redacttest.Password + "@github.example/acme/widgets/pull/7?access" + "_token=" + redacttest.Token},
		{"no-owner", "https://github.com//" + redacttest.Token},
		{"no-pull", "https://github.com/acme/" + redacttest.Token},
	}
	r := &Reconcile{}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if len(redacttest.Held(tc.url)) == 0 {
				t.Errorf("%s: the URL %q holds no credential, so the row could see none printed", tc.name, tc.url)
			}
			if _, err := r.searchResultRepo(FetchedPullRequest{Number: 7, URL: tc.url}); err == nil {
				t.Errorf("%s: searchResultRepo took %q; want it refused", tc.name, tc.url)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.searchResultRepo(FetchedPullRequest{Number: 7, URL: tc.url})
			if err == nil {
				t.Fatalf("searchResultRepo took %q; want it refused", tc.url)
			}
			redacttest.RequireNoPiece(t, "delivery.search-url/"+tc.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		})
	}
}
