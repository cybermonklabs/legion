package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEnvoyURLTheBootRefusesPrintsNoneOfIt: an ENVOY_URL carrying redacttest's credentials in
// its user information and query, refused as another scheme (other-scheme) or as a URL with no
// host (no-host), or refused by the dev sign-in as a listener on another machine (dev-remote), is
// refused naming ENVOY_URL, and the refusal holds no run of a credential. Each refusal quoted the
// URL as written. The twin shows each URL holds every credential and is still refused naming
// ENVOY_URL, so the row can see one printed. Captures dispatch.envoy-url/<case>.
func TestAnEnvoyURLTheBootRefusesPrintsNoneOfIt(t *testing.T) {
	const credentials = redacttest.User + ":" + redacttest.Password + "@"
	const query = "?access" + "_token=" + redacttest.Token
	production := func(listenerURL string) settingValues {
		return envGetter(map[string]string{
			"DATABASE_URL":                     "postgres://dispatch",
			"DISPATCH_AGENT_TOKEN":             "agent-token",
			"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
			"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
			"ENVOY_URL":                        listenerURL,
		})
	}
	devSignIn := func(listenerURL string) settingValues {
		return devSignInEnvironment(map[string]string{"ENVOY_URL": listenerURL})
	}
	cases := []struct {
		name, listenerURL string
		env               func(string) settingValues
	}{
		{"other-scheme", "ftp://" + credentials + "listener.example/" + query, production},
		{"no-host", "http://" + credentials + "/" + query, production},
		{"dev-remote", "https://" + credentials + "listener.example/" + query, devSignIn},
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if held := redacttest.Held(tc.listenerURL); len(held) != len(redacttest.Secrets) {
				t.Errorf("%s: the URL %q holds %v; want every credential, so the row can see one printed", tc.name, tc.listenerURL, held)
			}
			if _, err := resolveBootConfig(tc.env(tc.listenerURL)); err == nil || !strings.Contains(err.Error(), "ENVOY_URL") {
				t.Errorf("%s: resolveBootConfig = %v; want a refusal naming ENVOY_URL", tc.name, err)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveBootConfig(tc.env(tc.listenerURL))
			if err == nil {
				t.Fatalf("resolveBootConfig took ENVOY_URL=%q; want it refused", tc.listenerURL)
			}
			if !strings.Contains(err.Error(), "ENVOY_URL") {
				t.Errorf("resolveBootConfig = %v; want a refusal naming ENVOY_URL", err)
			}
			redacttest.RequireNoPiece(t, "dispatch.envoy-url/"+tc.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		})
	}
}
