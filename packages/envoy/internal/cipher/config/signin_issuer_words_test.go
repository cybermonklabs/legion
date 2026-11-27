package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerSignInIssuerThatLoadRefusesPrintsNoneOfIt: a BROKER_SIGNIN_ISSUER with redacttest's
// credentials in its path, refused as plain http to a host that is not a loopback one
// (plain-http), as another scheme (other-scheme) or as a URL with no host (no-host), is refused
// naming the setting and the form it must have, and the refusal holds no run of a credential. The
// refusal quoted the issuer as written. The credentials are in the path because the issuer rule
// (oidc.IssuerRefusal) refuses a user name, a password, an "@" past the host, a query and a
// fragment first, in fixed words that name the setting alone: a path is where a credential that
// reaches this refusal can stand, and each case checks the refusal is this one. The twin shows
// each issuer as written holds every credential and is still refused naming BROKER_SIGNIN_ISSUER,
// so the row can see one printed. Captures config.signin-issuer/<case>.
func TestABrokerSignInIssuerThatLoadRefusesPrintsNoneOfIt(t *testing.T) {
	const (
		path = "/" + redacttest.User + "/" + redacttest.Password + "/" + redacttest.Token
		form = "BROKER_SIGNIN_ISSUER must be an absolute https URL, or http to a loopback host"
	)
	cases := []struct{ name, issuer string }{
		{"plain-http", "http://accounts.example.com" + path},
		{"other-scheme", "ftp://accounts.example.com" + path},
		{"no-host", "https://" + path},
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
			redacttest.RequireNoPiece(t, "config.signin-issuer/"+tc.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
			if !strings.HasPrefix(err.Error(), form) {
				t.Errorf("Load(BROKER_SIGNIN_ISSUER=%q) = %v; want the refusal of the form it must have, %q", tc.issuer, err, form)
			}
		})
	}
}
