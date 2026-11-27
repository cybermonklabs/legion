package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestSeedFinishRefusesAURLNamingNoneOfItsCredentials: seed-finish's -url with redacttest's
// credentials in its user information and its query, and no host (no-host), a path url.Parse
// refuses (unparsed) or no "//" after its scheme, which url.Parse reads as an opaque URL with the
// user name as its scheme (opaque), is refused naming the value as redactURL names an endpoint,
// where the refusal quoted it as given. The value as given holds the credentials (twin). The
// control: the refusal still says what -url must be. Captures loadtest.seedfinish-url/<case>.
func TestSeedFinishRefusesAURLNamingNoneOfItsCredentials(t *testing.T) {
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	query := "?access" + "_token=" + redacttest.Token
	cases := []struct{ name, value string }{
		{"no-host", "http://" + userinfo + "/dispatch" + query},
		{"unparsed", "http://" + userinfo + "127.0.0.1:1/%zz" + query},
		{"opaque", userinfo + "127.0.0.1:1/dispatch" + query},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			if len(redacttest.Held(c.value)) != len(redacttest.Secrets) {
				t.Fatalf("%s: the value %q holds fewer than every credential, so the row cannot see them printed", c.name, c.value)
			}
		}
	})
	pgFile := credentialFileAt(t, "pg-url", "postgres://dispatch@127.0.0.1:5432/dispatch?sslmode=disable\n")
	tokenFile := credentialFileAt(t, "dispatch-token", "dispatch-agent-token\n")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := seedFinishResolve(t, "-pg-file", pgFile, "-url", c.value, "-token-file", tokenFile)
			if err == nil {
				t.Fatalf("seed-finish took -url %q", c.value)
			}
			redacttest.RequireNoPiece(t, "loadtest.seedfinish-url/"+c.name, err.Error())
			if !strings.Contains(err.Error(), "is not an absolute URL with a host") {
				t.Errorf("seed-finish = %q; want the refusal to say what -url must be", err)
			}
		})
	}
}
