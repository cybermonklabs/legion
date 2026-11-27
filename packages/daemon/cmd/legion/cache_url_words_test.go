package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestACacheURLTheProvisionRefusesPrintsNoneOfIt: a LEGION_REPOSITORY_CACHE_URL carrying a made-up
// user name and password that is no http:// URL with a host, written with no scheme (no-scheme,
// which has no "://" for the rule that passes the CLI's stderr to find) or with another scheme and
// a token in its query (other-scheme), is refused naming the setting, and the refusal holds no run
// of a credential. The refusal quoted the URL as written. The twin shows each URL holds the
// credentials and is still refused naming the setting, so the row can see one printed. Captures
// legion.cache-url/<case>.
func TestACacheURLTheProvisionRefusesPrintsNoneOfIt(t *testing.T) {
	const user, password = "not-a-secret-user", "not-a-secret-password"
	cases := []struct{ name, cacheURL string }{
		{"no-scheme", user + ":" + password + "@cache.example:8418"},
		{"other-scheme", "https://" + user + ":" + password + "@cache.example:8418/?access" + "_token=" + oauth2calltest.Token},
	}
	// inputs is provisionInputs with every input but the cache URL one it takes, so the refusal
	// is the cache URL's.
	inputs := func(cacheURL string) error {
		ws := workspaceProcess{environ: []string{cacheURLEnv + "=" + cacheURL}}
		_, err := provisionInputs(ws, "LEGION-9", winitRepo, "main", "/workspaces", winitHelper)
		return err
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if !strings.Contains(tc.cacheURL, user) || !strings.Contains(tc.cacheURL, password) {
				t.Errorf("%s: the URL %q holds no credential, so the row could see none printed", tc.name, tc.cacheURL)
			}
			if err := inputs(tc.cacheURL); err == nil || !strings.Contains(err.Error(), cacheURLEnv) {
				t.Errorf("%s: provisionInputs = %v; want a refusal naming %s", tc.name, err, cacheURLEnv)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := inputs(tc.cacheURL)
			if err == nil {
				t.Fatalf("provisionInputs took %s %q; want it refused", cacheURLEnv, tc.cacheURL)
			}
			if !strings.Contains(err.Error(), cacheURLEnv) {
				t.Errorf("provisionInputs = %v; want a refusal naming %s", err, cacheURLEnv)
			}
			oauth2calltest.RequireNoPiece(t, "legion.cache-url/"+tc.name, err.Error(), user, password, oauth2calltest.Token)
		})
	}
}
