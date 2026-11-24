package requests

import (
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
)

// kubernetesPolicy is the listed identities the identity registry composes from a Kubernetes
// provider listing accounts, as cmd/cipherd composes them: the set the request machine reads an
// enrolled pod's mapped projects from.
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
