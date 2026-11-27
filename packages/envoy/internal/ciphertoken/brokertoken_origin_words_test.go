package ciphertoken_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestNewRefusesAnOriginByItsPlaceAndPrintsNoneOfItsCredential: a configured origin carries a
// credential, in its user information or in a port url.Parse cannot read, and New refuses it. The
// refusal names the origin by its place among the origins and holds no run of the credential
// (captures ciphertoken.origin-words/<case>); the twin shows the origin as written, and
// url.Parse's own error, hold it.
func TestNewRefusesAnOriginByItsPlaceAndPrintsNoneOfItsCredential(t *testing.T) {
	database := storetest.Open(t)
	for _, c := range []struct{ name, origin string }{
		{"userinfo", "https://reader:" + redacttest.Password + "@dispatch.test/path"},
		{"unparsed", "https://dispatch.test:" + redacttest.Token + "/"},
	} {
		t.Run(c.name, func(t *testing.T) {
			secret := redacttest.Password
			if c.name == "unparsed" {
				secret = redacttest.Token
				if _, err := url.Parse(c.origin); err == nil || redacttest.Piece(err.Error(), secret) == "" {
					t.Fatalf("twin: url.Parse's error %v holds no run of the credential, so this row proves nothing", err)
				}
			}
			if redacttest.Piece(c.origin, secret) == "" {
				t.Fatalf("twin: the origin %q holds no run of the credential", c.origin)
			}
			_, err := ciphertoken.New(ciphertoken.Options{
				BrokerURL: "https://broker.test",
				Account:   ciphertoken.NewAccountToken("DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", accountTokenFile(t, accountToken)),
				Origins:   []string{dispatchOrigin, c.origin},
				Replay:    store.NewDPoPProofs(database.Pool),
				Address:   sourceAddress,
			})
			if err == nil {
				t.Fatalf("New accepted the origin %d of 2", 2)
			}
			redacttest.RequireNoPiece(t, "ciphertoken.origin-words/"+c.name, err.Error(), secret)
			if !strings.Contains(err.Error(), "origin 2 of 2") {
				t.Errorf("New's refusal %q names no place among the origins, want origin 2 of 2", err)
			}
		})
	}
}
