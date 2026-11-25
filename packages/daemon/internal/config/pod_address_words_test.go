package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestAnAddressNoPodReachesIsNamedThroughTheRule: under runtime kubernetes, a nats_urls entry on a
// loopback host carrying a made-up user name and password (nats-loopback), and a daemon_url doing
// the same (daemon-loopback), are refused naming the URL through the URL rule, and the refusal
// holds no run of a credential. The refusal named the URL as written. The twin shows each URL
// holds the credentials and is still refused as an address no pod reaches, so the row can see one
// printed. Captures config.pod-address/<case>.
func TestAnAddressNoPodReachesIsNamedThroughTheRule(t *testing.T) {
	const user, password = "not-a-secret-user", "not-a-secret-password"
	cases := []struct{ name, line, replacement string }{
		{"nats-loopback", "nats_urls: [nats://nats.internal.example:4222]", "nats_urls: [nats://" + user + ":" + password + "@127.0.0.2:4222]"},
		{"daemon-loopback", "daemon_url: http://10.0.0.5:13370", "daemon_url: http://" + user + ":" + password + "@127.0.0.1:13370"},
	}
	load := func(t *testing.T, line, replacement string) error {
		t.Helper()
		_, err := LoadForValidation(writeConfigFile(t, kubernetesWith(line, replacement)), noEnv)
		return err
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if !strings.Contains(tc.replacement, user) || !strings.Contains(tc.replacement, password) {
				t.Errorf("%s: %q holds no credential, so the row could see none printed", tc.name, tc.replacement)
			}
			if err := load(t, tc.line, tc.replacement); err == nil || !strings.Contains(err.Error(), "names a loopback host") {
				t.Errorf("%s: LoadForValidation = %v; want the address refused as a loopback host", tc.name, err)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := load(t, tc.line, tc.replacement)
			if err == nil {
				t.Fatalf("LoadForValidation took %q; want it refused", tc.replacement)
			}
			if !strings.Contains(err.Error(), "names a loopback host") {
				t.Errorf("LoadForValidation = %v; want the address refused as a loopback host", err)
			}
			oauth2calltest.RequireNoPiece(t, "config.pod-address/"+tc.name, err.Error(), user, password)
		})
	}
}
