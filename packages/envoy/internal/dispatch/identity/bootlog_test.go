package identity

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
)

// Each rule's boot line names the issuer through redact.URL: an issuer holding a made-up token in
// its query, its fragment or, percent-encoded, its path prints none of it. oidc.IssuerRefusal
// refuses the first two at boot; the line holds whatever it is given. A plain issuer is named as
// written.
func TestEachRulesBootLineNamesNoCredentialOfTheIssuer(t *testing.T) {
	const token = "madeUpIssuerToken0123456789"
	rules := map[string]SignInRule{"the domain rule": DomainRule{Domain: "example.com"}, "the group rule": GroupRule{Group: "dispatch-members"}}
	for ruleName, rule := range rules {
		for shape, issuer := range map[string]string{
			"a token in the query":         "https://issuer.example/?token=" + token,
			"a token in the fragment":      "https://issuer.example/#" + token,
			"an encoded token in the path": "https://issuer.example/%6D" + token,
		} {
			t.Run(ruleName+"/"+shape, func(t *testing.T) {
				var logged bytes.Buffer
				logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
				rule.BootLog(issuer, "dispatch-client")
				if strings.Contains(logged.String(), token[:10]) || !strings.Contains(logged.String(), "dispatch-client") {
					t.Fatalf("the boot line %q holds part of the issuer's token, or not the client", logged.String())
				}
			})
		}
		var logged bytes.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		rule.BootLog("https://issuer.example/pool", "dispatch-client")
		if !strings.Contains(logged.String(), "issuer=https://issuer.example/pool") {
			t.Errorf("%s: a plain issuer is not named as written: %q", ruleName, logged.String())
		}
	}
}
