package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEnvironmentOverrideLoadRefusesPrintsNoneOfIt: a DISPATCH_SERVER_URL with redacttest's
// credentials in its user information and query (server-url), and a NATS_URLS whose URLs carry the
// user name and password, and the token as a NATS token, beside an empty element (nats-urls), are
// refused naming the variable, and the refusal holds no run of a credential. The refusal quoted the
// value as written. The twin shows each value holds every credential and is still refused naming
// its variable, so the row can see one printed. Captures dispatchconfig.override/<case>.
func TestAnEnvironmentOverrideLoadRefusesPrintsNoneOfIt(t *testing.T) {
	cases := []struct{ name, variable, value string }{
		{"server-url", "DISPATCH_SERVER_URL",
			"https://" + redacttest.User + ":" + redacttest.Password + "@dispatch.example/?access" + "_token=" + redacttest.Token},
		{"nats-urls", "NATS_URLS",
			"nats://" + redacttest.User + ":" + redacttest.Password + "@one.example:4222, nats://" + redacttest.Token + "@two.example:4222, "},
	}
	load := func(t *testing.T, variable, value string) error {
		t.Helper()
		_, err := Load(environment(map[string]string{variable: value}), LoadOptions{CWD: t.TempDir(), HomeDir: t.TempDir()})
		return err
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if held := redacttest.Held(tc.value); len(held) != len(redacttest.Secrets) {
				t.Errorf("%s: the value %q holds %v; want every credential, so the row can see one printed", tc.name, tc.value, held)
			}
			if err := load(t, tc.variable, tc.value); err == nil || !strings.Contains(err.Error(), tc.variable) {
				t.Errorf("%s: Load = %v; want a refusal naming %s", tc.name, err, tc.variable)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := load(t, tc.variable, tc.value)
			if err == nil {
				t.Fatalf("Load took %s=%q; want it refused", tc.variable, tc.value)
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Errorf("Load = %v; want a refusal naming %s", err, tc.variable)
			}
			redacttest.RequireNoPiece(t, "dispatchconfig.override/"+tc.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		})
	}
}
