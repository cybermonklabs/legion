package api_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
	"github.com/sjawhar/envoy/internal/oidc"
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

// identities is the identity registry of the three providers, each trusting no issuer and
// listing nothing, as cmd/cipherd composes them: the Subjects an enroll.Service and a
// requests.Machine read.
func identities(t testing.TB, machines ...exchange.GoogleEntry) *identity.Registry {
	t.Helper()
	pods, err := kubernetes.New(kubernetes.Config{})
	if err != nil {
		t.Fatalf("kubernetes.New: %v", err)
	}
	fleets, err := google.New(google.Config{Machines: machines})
	if err != nil {
		t.Fatalf("google.New: %v", err)
	}
	registry, err := identity.NewRegistry([]identity.Provider{pods, fleets}, nil)
	if err != nil {
		t.Fatalf("identity.NewRegistry: %v", err)
	}
	return registry
}

// fleetPolicy is the listed identities of a Google provider listing each account of services for
// its service, as BROKER_VM_SERVICES lists it: the Policy a machine's or a sign-in's credential is
// held to.
func fleetPolicy(t testing.TB, services map[string]string) *exchange.Set {
	t.Helper()
	var machines []exchange.GoogleEntry
	for _, account := range slices.Sorted(maps.Keys(services)) {
		machines = append(machines, exchange.GoogleEntry{Account: account, Service: services[account]})
	}
	return identities(t, machines...).Policy()
}

// kubernetesCaller is the kubernetes provider's API verifier of issuer's projected tokens at
// audience, over issuer's key set keys: what the identity registry hands the API for a cluster.
func kubernetesCaller(t testing.TB, issuer string, keys *oidc.Keys, audience string) identity.CallerVerifier {
	t.Helper()
	provider, err := kubernetes.New(kubernetes.Config{Issuer: issuer, Keys: keys})
	if err != nil {
		t.Fatalf("kubernetes.New: %v", err)
	}
	return provider.CallerVerifier(audience)
}

// googleCaller is the google provider's API verifier of issuer's identity tokens at audience.
func googleCaller(t testing.TB, issuer string, keys *oidc.Keys, audience string) identity.CallerVerifier {
	t.Helper()
	provider, err := google.New(google.Config{Issuer: issuer, Keys: keys, Audience: audience})
	if err != nil {
		t.Fatalf("google.New: %v", err)
	}
	return provider.CallerVerifier(audience)
}

// inCluster is the Kubernetes service account whose subject is address, as BROKER_API_ACCOUNTS
// lists one; ofGoogle is the Google service account address is.
func inCluster(address string) identity.Account {
	return identity.Account{Kind: kubernetes.Kind, Address: address}
}

func ofGoogle(address string) identity.Account {
	return identity.Account{Kind: google.Kind, Address: address}
}
