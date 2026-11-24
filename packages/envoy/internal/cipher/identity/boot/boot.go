// Package boot is the broker's registry of caller identity providers: one file per provider,
// behind the build tag !cipher_no_<kind>, registers how that provider is built from the broker's
// configuration, and Open builds the identity.Registry cmd/cipherd verifies every caller token
// through. A provider package never registers itself: this directory lists the providers of a
// build, and a build tag removes one.
package boot

import (
	"context"
	"log/slog"
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/oidc"
)

// KeysFunc reads an issuer's published key set: oidc.NewKeys in production, a seam for a test
// that must not reach a real issuer.
type KeysFunc func(ctx context.Context, issuer string) (*oidc.Keys, error)

// Boot is what Open built: the registry, and the pod verifiers enrollment and the pod-session
// registration route read.
type Boot struct {
	Registry *identity.Registry
	// Pods verifies a pod's enrollment token and a service's login token at
	// BROKER_K8S_OIDC_AUDIENCE; nil on a broker told about no cluster.
	Pods identity.PodVerifier
	// PodRegistration verifies a registration's pod token at BROKER_POD_REGISTRATION_AUDIENCE, on
	// the cluster's key set; nil on a broker with no such audience.
	PodRegistration identity.PodVerifier
}

// opened is what one provider's registry file built from the configuration: the provider, and
// its caller verifier for each use the configuration gives it an audience for.
type opened struct {
	provider identity.Provider
	callers  map[identity.Use]identity.CallerVerifier
}

// entry is one provider's registration: its kind, its rank (the order the registry asks the
// providers in), and how it is built. keys reads an issuer's key set once per Open, however many
// providers or verifiers read that issuer.
type entry struct {
	kind identity.Kind
	rank int
	open func(cfg config.Config, keys func(issuer string) (*oidc.Keys, error), b *Boot) (opened, error)
}

// entries are the providers of this build, in rank order.
var entries []entry

// register adds a provider's entry; each per-kind file calls it once from its init.
func register(e entry) {
	entries = append(entries, e)
	slices.SortStableFunc(entries, func(a, b entry) int { return a.rank - b.rank })
}

// Kinds is the kinds this build holds, in rank order.
func Kinds() []identity.Kind {
	kinds := make([]identity.Kind, len(entries))
	for i, e := range entries {
		kinds[i] = e.kind
	}
	return kinds
}

// Open builds every provider of this build from cfg, reading each issuer's keys once through
// keysFor, and the registry of them with the caller verifiers each use is authenticated with, in
// rank order.
func Open(ctx context.Context, cfg config.Config, keysFor KeysFunc) (Boot, error) {
	read := map[string]*oidc.Keys{}
	keys := func(issuer string) (*oidc.Keys, error) {
		if k, ok := read[issuer]; ok {
			return k, nil
		}
		k, err := keysFor(ctx, issuer)
		if err != nil {
			return nil, err
		}
		read[issuer] = k
		return k, nil
	}
	var b Boot
	providers := make([]identity.Provider, 0, len(entries))
	callers := map[identity.Use][]identity.CallerVerifier{}
	for _, e := range entries {
		built, err := e.open(cfg, keys, &b)
		if err != nil {
			return Boot{}, err
		}
		providers = append(providers, built.provider)
		for _, use := range []identity.Use{identity.UseAPI, identity.UseNATS} {
			if verifier, ok := built.callers[use]; ok {
				callers[use] = append(callers[use], verifier)
			}
		}
	}
	registry, err := identity.NewRegistry(providers, callers)
	if err != nil {
		return Boot{}, err
	}
	b.Registry = registry
	logExchange(cfg)
	return b, nil
}

// logExchange says once at boot which subject token issuers the exchange accepts, rather than
// leaving an operator to read it out of a refusal.
func logExchange(cfg config.Config) {
	if cfg.ExchangeK8sAudience == "" && cfg.ExchangeGitHubIssuer == "" && cfg.GoogleOIDCAudience == "" {
		slog.Info("token exchange", "state", "unconfigured", "detail", "no subject token issuer is set, so no pod, workflow run or machine exchanges")
		return
	}
	slog.Info("token exchange", "state", "on", "kubernetes", cfg.ExchangeK8sAudience != "", "github", cfg.ExchangeGitHubIssuer != "",
		"accounts", len(cfg.ExchangeAccounts), "read_accounts", len(cfg.ExchangeReadAccounts), "workflows", len(cfg.ExchangeWorkflows), "machines", len(cfg.ExchangeGoogleAccounts))
}
