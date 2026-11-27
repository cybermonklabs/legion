package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
)

// In the environment `agent-secrets NAME -- git push` gives a command whose NAME the secrets
// broker's airlock holds back, both HTTPS_PROXY and https_proxy name the airlock's proxy, whose
// URL's user is cipheraddr.AirlockProxyUser, and SSL_CERT_FILE names the airlock's authority. The
// gh `legion gh` runs with its directory's token keeps neither proxy variable and no ALL_PROXY or
// all_proxy, so it connects to GitHub directly; gh under the caller's own GH_TOKEN or GITHUB_TOKEN
// keeps the airlock's proxy, which alone swaps that placeholder, and the caller's ALL_PROXY. Each
// keeps the caller's NO_PROXY and no_proxy as they are, and none is given the trust store.
func TestGhKeepsTheAirlocksProxyForTheCallersTokenAlone(t *testing.T) {
	t.Parallel()
	airlock := "http://" + cipheraddr.AirlockProxyUser + ":placeholder@broker.internal.example:13381"
	caller := []string{
		"PATH=/usr/bin:/bin", "HOME=/home/example",
		"HTTPS_PROXY=" + airlock, "https_proxy=" + airlock, "ALL_PROXY=http://127.0.0.1:1", "all_proxy=http://127.0.0.1:2",
		"NO_PROXY=localhost,.internal", "no_proxy=localhost,.svc", "SSL_CERT_FILE=/tmp/airlock-bundle.pem",
	}
	lists := map[string]string{"NO_PROXY": "localhost,.internal", "no_proxy": "localhost,.svc", "SSL_CERT_FILE": "unset"}
	callers := map[string]string{"HTTPS_PROXY": airlock, "https_proxy": airlock, "ALL_PROXY": "http://127.0.0.1:1", "all_proxy": "http://127.0.0.1:2"}
	for _, tc := range []struct {
		name  string
		extra []string
		want  map[string]string
	}{
		{name: "a gh directory's token", want: map[string]string{"HTTPS_PROXY": "unset", "https_proxy": "unset", "ALL_PROXY": "unset", "all_proxy": "unset"}},
		{name: "the caller's GH_TOKEN", extra: []string{"GH_TOKEN=callers-placeholder-not-a-secret"}, want: callers},
		{name: "the caller's GITHUB_TOKEN", extra: []string{"GITHUB_TOKEN=callers-other-not-a-secret"}, want: callers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := map[string]string{}
			for _, entry := range ghChildEnviron(slices.Concat(caller, tc.extra), nil) {
				if name, value, ok := strings.Cut(entry, "="); ok {
					kept[name] = value
				}
			}
			for name, want := range lists {
				if got := valueOr(kept, name); got != want {
					t.Errorf("gh was handed %s=%q, want %q", name, got, want)
				}
			}
			for name, want := range tc.want {
				if got := valueOr(kept, name); got != want {
					t.Errorf("gh was handed %s=%q, want %q", name, got, want)
				}
			}
		})
	}
}

// valueOr is env's value of name, or `unset` where env holds no entry of that name.
func valueOr(env map[string]string, name string) string {
	if value, ok := env[name]; ok {
		return value
	}
	return "unset"
}
