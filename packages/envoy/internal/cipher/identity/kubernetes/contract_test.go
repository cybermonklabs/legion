package kubernetes_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/identitytest"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
	"github.com/sjawhar/envoy/internal/oidc"
)

const contractAccount = "system:serviceaccount:example-ns:example-worker"

func TestKubernetesRunsTheIdentityContract(t *testing.T) {
	identitytest.Contract(t, identitytest.Fixture{
		New: func(t *testing.T, issuer string, keys *oidc.Keys, audience string) identity.Provider {
			p, err := kubernetes.New(kubernetes.Config{
				Issuer: issuer, Keys: keys, ExchangeAudience: audience,
				Accounts: []exchange.AccountEntry{{Account: contractAccount, Project: "ACME", Person: "ada@example.com"}},
			})
			if err != nil {
				t.Fatalf("kubernetes.New: %v", err)
			}
			return p
		},
		Audience: "https://broker.example",
		Subject:  contractAccount,
		Claims:   map[string]any{"kubernetes.io": map[string]any{"pod": map[string]any{"uid": "pod-uid-1"}}},
		Want:     identity.Account{Kind: kubernetes.Kind, Address: contractAccount},
		Trades: []identitytest.TradeFixture{{
			TokenType:     kubernetes.SubjectTokenType,
			Claims:        map[string]any{"sub": contractAccount, "kubernetes.io": map[string]any{"pod": map[string]any{"uid": "pod-uid-1"}}},
			WantPrincipal: "account:" + contractAccount,
		}},
	})
}

func TestPrincipalSpellsTodaysAccountPrincipal(t *testing.T) {
	if got := kubernetes.Principal("system:serviceaccount:a:b"); got != "account:system:serviceaccount:a:b" {
		t.Fatalf("Principal = %q, want account:system:serviceaccount:a:b", got)
	}
}
