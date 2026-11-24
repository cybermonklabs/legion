package api_test

import (
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
)

// kubernetesRegistry is the identity registry of a Kubernetes provider listing accounts and
// trusting no cluster, as cmd/cipherd composes one.
func kubernetesRegistry(accounts []exchange.AccountEntry) (*identity.Registry, error) {
	pods, err := kubernetes.New(kubernetes.Config{Accounts: accounts})
	if err != nil {
		return nil, err
	}
	return identity.NewRegistry([]identity.Provider{pods}, nil)
}

// kubernetesPolicy is kubernetesRegistry's listed identities: the set the request machine reads
// an enrolled pod's mapped projects from.
func kubernetesPolicy(accounts []exchange.AccountEntry) (*exchange.Set, error) {
	registry, err := kubernetesRegistry(accounts)
	if err != nil {
		return nil, err
	}
	return registry.Policy(), nil
}
