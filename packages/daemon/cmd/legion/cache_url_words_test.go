package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestACacheURLTheFetchRefusesPrintsNoneOfIt: a --cache-url carrying a made-up user name and
// password that is no http:// URL with a host, written with no scheme (no-scheme, which has no
// "://" for the rule that passes the CLI's stderr to find) or with another scheme and a token in
// its query (other-scheme), is refused naming the flag, and the refusal holds no run of a
// credential. The refusal quoted the URL as written. The twin shows each URL holds the
// credentials and is still refused naming --cache-url, so the row can see one printed. Captures
// legion.cache-url/<case>.
func TestACacheURLTheFetchRefusesPrintsNoneOfIt(t *testing.T) {
	const user, password = "not-a-secret-user", "not-a-secret-password"
	cases := []struct{ name, cacheURL string }{
		{"no-scheme", user + ":" + password + "@cache.example:8418"},
		{"other-scheme", "https://" + user + ":" + password + "@cache.example:8418/?access" + "_token=" + oauth2calltest.Token},
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if !strings.Contains(tc.cacheURL, user) || !strings.Contains(tc.cacheURL, password) {
				t.Errorf("%s: the URL %q holds no credential, so the row could see none printed", tc.name, tc.cacheURL)
			}
			if _, err := fetchInputs("acme/widgets", "/feed", tc.cacheURL); err == nil || !strings.Contains(err.Error(), "--cache-url") {
				t.Errorf("%s: fetchInputs = %v; want a refusal naming --cache-url", tc.name, err)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetchInputs("acme/widgets", "/feed", tc.cacheURL)
			if err == nil {
				t.Fatalf("fetchInputs took --cache-url %q; want it refused", tc.cacheURL)
			}
			if !strings.Contains(err.Error(), "--cache-url") {
				t.Errorf("fetchInputs = %v; want a refusal naming --cache-url", err)
			}
			oauth2calltest.RequireNoPiece(t, "legion.cache-url/"+tc.name, err.Error(), user, password, oauth2calltest.Token)
		})
	}
}
