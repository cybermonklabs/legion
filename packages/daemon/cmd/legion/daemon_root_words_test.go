package main

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestADaemonTheControllerCannotReachIsNamedThroughTheRule: `legion controller start`'s three
// requests of the daemon (its repositories read, state-read; the capability mint, secret-mint; the
// gh credential's fetch, credential) to an API root carrying a made-up user name and password and
// a token in its query, nothing listening there, fail naming the root through the URL rule; and a
// root that does not parse as a URL with a host (unparsed), which has no "://" for the rule that
// passes the CLI's stderr to find, is named by what it is. The refusal named the root as written.
// The twin shows each root holds the credentials and each request still fails naming the daemon
// it could not reach, so the row can see one printed. Captures legion.daemon-root/<case>.
func TestADaemonTheControllerCannotReachIsNamedThroughTheRule(t *testing.T) {
	const user, password = "not-a-secret-user", "not-a-secret-password"
	parsed := "http://" + user + ":" + password + "@127.0.0.1:1/?access" + "_token=" + oauth2calltest.Token
	unparsed := user + ":" + password + "@127.0.0.1:1"
	ctx := context.Background()
	cases := []struct {
		name, root string
		call       func(root string) error
	}{
		{"state-read", parsed, func(root string) error { return checkControllerRepositories(ctx, root) }},
		{"state-read-unparsed", unparsed, func(root string) error { return checkControllerRepositories(ctx, root) }},
		{"secret-mint", parsed, func(root string) error {
			_, _, err := fetchControllerSecret(ctx, root, "operator-token", 1)
			return err
		}},
		{"credential", parsed, func(root string) error {
			_, _, err := fetchControllerCredential(ctx, root, "controller-secret", "")
			return err
		}},
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if !strings.Contains(tc.root, user) || !strings.Contains(tc.root, password) {
				t.Errorf("%s: the root %q holds no user name and password, so the row could see none printed", tc.name, tc.root)
			}
			if err := tc.call(tc.root); err == nil || !strings.Contains(err.Error(), "could not reach the Legion daemon at ") {
				t.Errorf("%s: the request = %v; want it refused naming the daemon it could not reach", tc.name, err)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(tc.root)
			if err == nil {
				t.Fatalf("the request to %q answered; want it refused", tc.root)
			}
			if !strings.Contains(err.Error(), "could not reach the Legion daemon at ") {
				t.Errorf("the request = %v; want it to name the daemon it could not reach", err)
			}
			oauth2calltest.RequireNoPiece(t, "legion.daemon-root/"+tc.name, err.Error(), user, password, oauth2calltest.Token)
		})
	}
}
