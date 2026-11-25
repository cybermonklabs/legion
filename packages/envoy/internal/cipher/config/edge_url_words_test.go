package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerEdgeURLThatLoadRefusesPrintsNoneOfIt: a BROKER_EDGE_URL with redacttest's credentials
// in its user information (user-info), the token in its path (path), in its query (query) or in
// its fragment (fragment) is refused naming the setting, and the refusal holds no run of a
// credential. The edge serves its routes under the URL as the broker's own serve theirs under
// BROKER_PUBLIC_URL, so it is held to that URL's form. The refusal quoted the URL as the rule reads
// it, which prints a plain path as written, and Load took the other three. The twin shows that the
// URL with the token in its path is still refused naming BROKER_EDGE_URL, and that the rule's
// reading of it holds the token, so the row can see one printed. Captures config.edge-url/<case>.
func TestABrokerEdgeURLThatLoadRefusesPrintsNoneOfIt(t *testing.T) {
	const host = "secrets.edge.example"
	inPath := "https://" + host + "/" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		e := edgeEnv()
		e["BROKER_EDGE_URL"] = inPath
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EDGE_URL") {
			t.Errorf("Load(BROKER_EDGE_URL=%q) = %v; want a refusal naming the setting", inPath, err)
		}
		if shown := redact.URL(inPath); !strings.Contains(shown, redacttest.Token) {
			t.Errorf("redact.URL(%q) = %q; want the token in it, so the row can see one printed", inPath, shown)
		}
	})
	for _, tc := range []struct{ name, edge string }{
		{"user-info", "https://" + redacttest.User + ":" + redacttest.Password + "@" + host},
		{"path", inPath},
		{"query", "https://" + host + "?access" + "_token=" + redacttest.Token},
		{"fragment", "https://" + host + "#access" + "_token=" + redacttest.Token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := edgeEnv()
			e["BROKER_EDGE_URL"] = tc.edge
			_, err := Load(env(e))
			if err == nil {
				t.Fatalf("Load took BROKER_EDGE_URL=%q; want it refused", tc.edge)
			}
			if !strings.Contains(err.Error(), "BROKER_EDGE_URL") {
				t.Errorf("Load(BROKER_EDGE_URL=%q) = %v; want a refusal naming the setting", tc.edge, err)
			}
			redacttest.RequireNoPiece(t, "config.edge-url/"+tc.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		})
	}
}
