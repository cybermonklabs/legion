package enroll

import (
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
)

// kubernetesPolicy is the listed identities the identity registry composes from a Kubernetes
// provider listing accounts, as cmd/cipherd composes them.
func kubernetesPolicy(accounts []exchange.AccountEntry) (*exchange.Set, error) {
	pods, err := kubernetes.New(kubernetes.Config{Accounts: accounts})
	if err != nil {
		return nil, err
	}
	registry, err := identity.NewRegistry([]identity.Provider{pods}, nil)
	if err != nil {
		return nil, err
	}
	return registry.Policy(), nil
}

// podDecisions is the Kubernetes provider of a test's accounts, whose decision of a pod's trade a
// test compares an enrollment with, as the token exchange decides it.
type podDecisions struct{ provider *kubernetes.Provider }

// Pod is the exchange's decision of a pod's trade of token under session.
func (p podDecisions) Pod(token map[string]any, session string, projects []string) (exchange.Claims, error) {
	return p.provider.Decide(identity.Trade{TokenType: kubernetes.SubjectTokenType, Subject: token, Session: session, Projects: projects})
}

// kubernetesPods is the Kubernetes provider listing accounts.
func kubernetesPods(accounts []exchange.AccountEntry) (podDecisions, error) {
	provider, err := kubernetes.New(kubernetes.Config{Accounts: accounts})
	return podDecisions{provider: provider}, err
}
