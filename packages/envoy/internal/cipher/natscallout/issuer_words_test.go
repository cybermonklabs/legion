package natscallout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAServiceTokenOfAnotherIssuerPrintsNoneOfIt: a service token whose own iss claim, read
// before the token is verified, is no issuer this broker takes NATS tokens from and carries
// redacttest's credentials in a URL's user information and query (other-issuer) is refused naming
// the issuer by what it is, and the refusal holds no run of a credential. The refusal quoted the
// claim. The twin shows the claim holds every credential and is still refused, so the row can see
// one printed. Captures natscallout.issuer/other-issuer.
func TestAServiceTokenOfAnotherIssuerPrintsNoneOfIt(t *testing.T) {
	issuer := "https://" + redacttest.User + ":" + redacttest.Password + "@issuer.example/?access" + "_token=" + redacttest.Token
	claims, err := json.Marshal(map[string]string{"iss": issuer})
	if err != nil {
		t.Fatal(err)
	}
	raw := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".c2ln"
	tokens := PlatformTokens{K8sIssuer: "https://kubernetes.default.svc", GoogleIssuer: "https://accounts.google.com"}
	t.Run("twin", func(t *testing.T) {
		if held := redacttest.Held(issuer); len(held) != len(redacttest.Secrets) {
			t.Errorf("the claim %q holds %v; want every credential, so the row can see one printed", issuer, held)
		}
		if _, _, _, _, err := tokens.Verify(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "no NATS audience") {
			t.Errorf("Verify = %v; want the token refused as of no issuer this broker takes", err)
		}
	})
	t.Run("other-issuer", func(t *testing.T) {
		_, _, _, _, err := tokens.Verify(context.Background(), raw)
		if err == nil {
			t.Fatal("Verify took a token of another issuer; want it refused")
		}
		redacttest.RequireNoPiece(t, "natscallout.issuer/other-issuer", err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
	})
}
