package enroll

import (
	"maps"
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/github"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
)

// registryOf is the identity registry of the three providers, each trusting no issuer, the
// Google one listing machines: the Subjects and, through its Policy, the listed identities an
// enroll.Service reads, as cmd/cipherd composes them.
func registryOf(machines []exchange.GoogleEntry) *identity.Registry {
	pods, err := kubernetes.New(kubernetes.Config{})
	if err != nil {
		panic(err)
	}
	fleets, err := google.New(google.Config{Machines: machines})
	if err != nil {
		panic(err)
	}
	runs, err := github.New(github.Config{})
	if err != nil {
		panic(err)
	}
	registry, err := identity.NewRegistry([]identity.Provider{pods, fleets, runs}, nil)
	if err != nil {
		panic(err)
	}
	return registry
}

// fleets is the listed identities of a Google provider listing each account of services for its
// service, as BROKER_VM_SERVICES lists it: the Policy a machine's or a sign-in's credential is
// held to (AuthenticateLauncher).
func fleets(services map[string]string) *exchange.Set {
	var machines []exchange.GoogleEntry
	for _, account := range slices.Sorted(maps.Keys(services)) {
		machines = append(machines, exchange.GoogleEntry{Account: account, Service: services[account]})
	}
	return registryOf(machines).Policy()
}

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
