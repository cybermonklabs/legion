package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestANatsURLsEntryTheConfigRefusesPrintsNoneOfIt: a nats_urls entry carrying a made-up user name
// and password, refused for a query that holds a token (query) or as no URL at all (unparsed, which
// has no "://" for a reader of the URL rule to find), is named by its place in the list, and the
// refusal holds no run of a credential. The refusal quoted the entry as written. The twin shows
// each entry holds the credentials and is still refused naming nats_urls, so the row can see one
// printed. Captures config.nats-urls-entry/<case>.
func TestANatsURLsEntryTheConfigRefusesPrintsNoneOfIt(t *testing.T) {
	const user, password = "not-a-secret-user", "not-a-secret-password"
	cases := []struct{ name, entry string }{
		{"query", "nats://" + user + ":" + password + "@127.0.0.1:4222?access" + "_token=" + oauth2calltest.Token},
		{"unparsed", user + ":" + password + "@nats-host"},
	}
	load := func(t *testing.T, entry string) error {
		t.Helper()
		body := strings.Replace(minimalFile, "nats_urls: [nats://127.0.0.1:4222]\n", `nats_urls: ["`+entry+`"]`+"\n", 1)
		_, err := LoadForValidation(writeConfigFile(t, body), noEnv)
		return err
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if !strings.Contains(tc.entry, user) || !strings.Contains(tc.entry, password) {
				t.Errorf("%s: the entry %q holds no credential, so the row could see none printed", tc.name, tc.entry)
			}
			if err := load(t, tc.entry); err == nil || !strings.HasPrefix(err.Error(), "nats_urls entry ") {
				t.Errorf("%s: LoadForValidation = %v; want the entry refused", tc.name, err)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := load(t, tc.entry)
			if err == nil {
				t.Fatalf("LoadForValidation took nats_urls entry %q; want it refused", tc.entry)
			}
			if !strings.HasPrefix(err.Error(), "nats_urls entry 1 ") {
				t.Errorf("LoadForValidation = %v; want the entry named by its place, 1", err)
			}
			oauth2calltest.RequireNoPiece(t, "config.nats-urls-entry/"+tc.name, err.Error(), user, password, oauth2calltest.Token)
		})
	}
}
