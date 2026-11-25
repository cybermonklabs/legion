package oauth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestASubjectTokenIssuerConfiguredInPartPrintsNoneOfIt: a subject token issuer configured in
// part, whose issuer (issuer) or audience (audience) carries redacttest's credentials in a URL's
// user information and query, is refused naming the issuer by where it comes from and which parts
// are set, and the refusal holds no run of a credential. The refusal quoted the issuer and the
// audience as written. The twin shows each value holds every credential and New still refuses it
// naming the issuer, so the row can see one printed. Captures oauth.issuer-in-part/<case>.
func TestASubjectTokenIssuerConfiguredInPartPrintsNoneOfIt(t *testing.T) {
	carrying := "https://" + redacttest.User + ":" + redacttest.Password + "@issuer.example/?access" + "_token=" + redacttest.Token
	sound := oauth.Config{
		Secret: make([]byte, 32), DeviceLifetime: time.Minute, AccessLifetime: time.Minute, RefreshLifetime: time.Hour,
		VerificationURL: "https://dispatch.example/credentials/machine", ProofSkew: time.Minute,
		Exchange: oauth.ExchangeConfig{Policy: newPolicy(t, nil, nil)},
	}
	cases := []struct {
		name, where string
		spoil       func(*oauth.Config)
	}{
		{"issuer", "Kubernetes", func(c *oauth.Config) { c.Exchange.Kubernetes = oauth.IssuerConfig{Issuer: carrying} }},
		{"audience", "GitHub", func(c *oauth.Config) { c.Exchange.GitHub = oauth.IssuerConfig{Audience: carrying} }},
	}
	t.Run("twin", func(t *testing.T) {
		if held := redacttest.Held(carrying); len(held) != len(redacttest.Secrets) {
			t.Errorf("the value %q holds %v; want every credential, so the row can see one printed", carrying, held)
		}
		for _, tc := range cases {
			c := sound
			tc.spoil(&c)
			if _, err := oauth.New(c, nil); err == nil || !strings.Contains(err.Error(), "the "+tc.where+" subject token issuer is configured in part") {
				t.Errorf("%s: oauth.New = %v; want the %s issuer refused as configured in part", tc.name, err, tc.where)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := sound
			tc.spoil(&c)
			_, err := oauth.New(c, nil)
			if err == nil {
				t.Fatalf("oauth.New took a %s issuer configured in part; want it refused", tc.where)
			}
			if !strings.Contains(err.Error(), "the "+tc.where+" subject token issuer is configured in part") {
				t.Errorf("oauth.New = %v; want the %s issuer named as configured in part", err, tc.where)
			}
			redacttest.RequireNoPiece(t, "oauth.issuer-in-part/"+tc.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		})
	}
}
