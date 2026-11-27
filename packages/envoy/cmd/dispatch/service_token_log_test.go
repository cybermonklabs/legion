package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
)

// The service-token boot line names DISPATCH_OIDC_ISSUER through redact.URL: an issuer holding a
// made-up token in its query, its fragment or, percent-encoded, its path prints none of it.
// oidc.IssuerRefusal refuses the first two at boot; the line holds whatever it is given. A plain
// issuer is named as written.
func TestTheServiceTokenBootLineNamesNoCredentialOfTheIssuer(t *testing.T) {
	const token = "madeUpIssuerToken0123456789"
	for shape, issuer := range map[string]string{
		"a token in the query":         "https://oidc.example/id/CLUSTER?token=" + token,
		"a token in the fragment":      "https://oidc.example/id/CLUSTER#" + token,
		"an encoded token in the path": "https://oidc.example/id/%6D" + token,
	} {
		t.Run(shape, func(t *testing.T) {
			var logged bytes.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			logServiceTokens(bootConfig{OIDCIssuer: issuer, OIDCAudience: "dispatch"})
			if strings.Contains(logged.String(), token[:10]) || !strings.Contains(logged.String(), "audience=dispatch") {
				t.Fatalf("the boot line %q holds part of the issuer's token, or not the audience", logged.String())
			}
		})
	}
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	logServiceTokens(bootConfig{OIDCIssuer: "https://oidc.example/id/CLUSTER", OIDCAudience: "dispatch"})
	if !strings.Contains(logged.String(), "issuer=https://oidc.example/id/CLUSTER") {
		t.Errorf("a plain issuer is not named as written: %q", logged.String())
	}
}
