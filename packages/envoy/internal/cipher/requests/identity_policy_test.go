package requests

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/github"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
)

// registryOf is the identity registry of the three providers, the Kubernetes one listing
// accounts and the Google one listing machines, each trusting no issuer, as cmd/cipherd composes
// them: the reader of every launcher credential's subject (Machine.Subjects) and, through its
// Policy, of what every operator's entry lists (Machine.Exchange).
func registryOf(t testing.TB, accounts []kubernetes.AccountEntry, machines []google.GoogleEntry) *identity.Registry {
	t.Helper()
	pods, err := kubernetes.New(kubernetes.Config{Accounts: accounts})
	if err != nil {
		t.Fatalf("kubernetes.New: %v", err)
	}
	fleets, err := google.New(google.Config{Machines: machines})
	if err != nil {
		t.Fatalf("google.New: %v", err)
	}
	runs, err := github.New(github.Config{})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	registry, err := identity.NewRegistry([]identity.Provider{pods, fleets, runs}, nil)
	if err != nil {
		t.Fatalf("identity.NewRegistry: %v", err)
	}
	return registry
}

// identities is registryOf listing nothing.
func identities(t testing.TB) *identity.Registry { return registryOf(t, nil, nil) }

// kubernetesPolicy is the listed identities the identity registry composes from a Kubernetes
// provider listing accounts, as cmd/cipherd composes them: the set the request machine reads an
// enrolled pod's mapped projects from.
func kubernetesPolicy(accounts []kubernetes.AccountEntry) (*exchange.Set, error) {
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

// fleetPolicy is the listed identities of a Google provider listing machines: the services
// BROKER_VM_SERVICES lists their accounts for, and the person each BROKER_VM_EXCHANGE_ACCOUNTS
// entry names.
func fleetPolicy(t testing.TB, machines ...google.GoogleEntry) *exchange.Set {
	t.Helper()
	return registryOf(t, nil, machines).Policy()
}
