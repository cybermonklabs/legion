package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerPublicURLThatLoadRefusesPrintsNoneOfIt: a BROKER_PUBLIC_URL with redacttest's
// credentials in its user information (user-info), the token in its path (path), in its query
// (query) or in its fragment (fragment) is refused naming the setting, and the refusal holds no
// run of a credential. The authorization server metadata publishes the URL as the issuer, and RFC
// 8414 section 2 holds an issuer to no query or fragment. The refusal quoted the URL as the rule
// reads it, which prints a plain path as written, and Load took the other three. The twin shows
// that the URL with the token in its path is still refused naming BROKER_PUBLIC_URL, and that the
// rule's reading of it holds the token, so the row can see one printed. Captures
// config.public-url/<case>.
func TestABrokerPublicURLThatLoadRefusesPrintsNoneOfIt(t *testing.T) {
	const host = "secrets.internal.example"
	inPath := "https://" + host + "/" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		e := validEnv()
		e["BROKER_PUBLIC_URL"] = inPath
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_PUBLIC_URL") {
			t.Errorf("Load(BROKER_PUBLIC_URL=%q) = %v; want a refusal naming the setting", inPath, err)
		}
		if shown := redact.URL(inPath); !strings.Contains(shown, redacttest.Token) {
			t.Errorf("redact.URL(%q) = %q; want the token in it, so the row can see one printed", inPath, shown)
		}
	})
	for _, tc := range []struct{ name, public string }{
		{"user-info", "https://" + redacttest.User + ":" + redacttest.Password + "@" + host},
		{"path", inPath},
		{"query", "https://" + host + "?access" + "_token=" + redacttest.Token},
		{"fragment", "https://" + host + "#access" + "_token=" + redacttest.Token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnv()
			e["BROKER_PUBLIC_URL"] = tc.public
			_, err := Load(env(e))
			if err == nil {
				t.Fatalf("Load took BROKER_PUBLIC_URL=%q; want it refused", tc.public)
			}
			if !strings.Contains(err.Error(), "BROKER_PUBLIC_URL") {
				t.Errorf("Load(BROKER_PUBLIC_URL=%q) = %v; want a refusal naming the setting", tc.public, err)
			}
			redacttest.RequireNoPiece(t, "config.public-url/"+tc.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		})
	}
}
