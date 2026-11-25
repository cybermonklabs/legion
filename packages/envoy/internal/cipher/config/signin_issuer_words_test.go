package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerSignInIssuerThatLoadRefusesPrintsNoneOfIt: a BROKER_SIGNIN_ISSUER with redacttest's
// credentials in its user information and its query, refused as plain http to a host that is not
// a loopback one (plain-http), as another scheme (other-scheme) or as a URL with no host
// (no-host), is refused naming the setting, and the refusal holds no run of a credential. The
// refusal quoted the issuer as written. The twin shows each issuer as written holds every
// credential and is still refused naming BROKER_SIGNIN_ISSUER, so the row can see one printed.
// Captures config.signin-issuer/<case>.
func TestABrokerSignInIssuerThatLoadRefusesPrintsNoneOfIt(t *testing.T) {
	const credentials = redacttest.User + ":" + redacttest.Password + "@"
	const query = "?access" + "_token=" + redacttest.Token
	cases := []struct{ name, issuer string }{
		{"plain-http", "http://" + credentials + "accounts.example.com/" + query},
		{"other-scheme", "ftp://" + credentials + "accounts.example.com/" + query},
		{"no-host", "https://" + credentials + "/" + query},
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if held := redacttest.Held(tc.issuer); len(held) != len(redacttest.Secrets) {
				t.Errorf("%s: the issuer %q holds %v; want every credential, so the row can see one printed", tc.name, tc.issuer, held)
			}
			e := validEnv()
			e["BROKER_SIGNIN_ISSUER"] = tc.issuer
			if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SIGNIN_ISSUER") {
				t.Errorf("%s: Load(BROKER_SIGNIN_ISSUER=%q) = %v; want a refusal naming the setting", tc.name, tc.issuer, err)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_SIGNIN_ISSUER"] = tc.issuer
			_, err := Load(env(e))
			if err == nil {
				t.Fatalf("Load took BROKER_SIGNIN_ISSUER=%q; want it refused", tc.issuer)
			}
			if !strings.Contains(err.Error(), "BROKER_SIGNIN_ISSUER") {
				t.Errorf("Load(BROKER_SIGNIN_ISSUER=%q) = %v; want a refusal naming the setting", tc.issuer, err)
			}
			redacttest.RequireNoPiece(t, "config.signin-issuer/"+tc.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		})
	}
}
