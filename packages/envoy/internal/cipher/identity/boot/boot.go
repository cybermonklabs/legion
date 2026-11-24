// Package boot is the broker's registry of caller identity providers: one file per provider,
// behind the build tag !cipher_no_<kind>, registers how that provider is built from the broker's
// configuration, and Open builds the identity.Registry cmd/cipherd verifies every caller token
// through. A provider package never registers itself: this directory lists the providers of a
// build, and a build tag removes one.
package boot

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/settingsfields"
	"github.com/sjawhar/envoy/internal/oidc"
)

// KeysFunc reads an issuer's published key set: oidc.NewKeys in production, a seam for a test
// that must not reach a real issuer.
type KeysFunc func(ctx context.Context, issuer string) (*oidc.Keys, error)

// Boot is what Open built: the registry, the pod verifiers enrollment and the pod-session
// registration route read, and the providers' fields of GET /v1/settings.
type Boot struct {
	Registry *identity.Registry
	// Pods verifies a pod's enrollment token and a service's login token at
	// BROKER_K8S_OIDC_AUDIENCE; nil on a broker told about no cluster.
	Pods identity.PodVerifier
	// PodRegistration verifies a registration's pod token at BROKER_POD_REGISTRATION_AUDIENCE, on
	// the cluster's key set; nil on a broker with no such audience.
	PodRegistration identity.PodVerifier
	// SettingsFields are the providers' own fields of GET /v1/settings, by JSON name, which the
	// broker merges with its store's (settingsfields.Merge); never nil.
	SettingsFields map[string]string
}

// opened is what one provider's registry file built from the configuration: the provider, its
// caller verifier for each use the configuration gives it an audience for, and the fields it adds
// to GET /v1/settings (settingsfields.Of of its own SettingsFields struct; nil for none).
type opened struct {
	provider identity.Provider
	callers  map[identity.Use]identity.CallerVerifier
	settings map[string]string
}

// entry is one provider's registration: its kind, its rank (the order the registry asks the
// providers in), and how it is built. keys reads an issuer's key set once per Open, however many
// providers or verifiers read that issuer. accountNoun and natsAudience are how a refusal names
// an account of the kind ("a Kubernetes account") and the setting that gives its tokens a NATS
// audience, for a kind whose services connect to the hub.
type entry struct {
	kind                      identity.Kind
	rank                      int
	open                      func(cfg config.Config, keys func(issuer string) (*oidc.Keys, error), b *Boot) (opened, error)
	accountNoun, natsAudience string
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
	if err := servedKinds(cfg); err != nil {
		return Boot{}, err
	}
	settings := map[string]string{}
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
		if err := settingsfields.Merge(settings, built.settings); err != nil {
			return Boot{}, fmt.Errorf("identity: the %s provider's settings fields: %w", e.kind, err)
		}
	}
	if err := natsAudiences(cfg, callers[identity.UseNATS]); err != nil {
		return Boot{}, err
	}
	registry, err := identity.NewRegistry(providers, callers)
	if err != nil {
		return Boot{}, err
	}
	b.Registry = registry
	b.SettingsFields = settings
	logExchange(cfg)
	return b, nil
}

// servedKinds refuses an account BROKER_API_ACCOUNTS or BROKER_NATS_SERVICES names, an edge
// listener's among them, whose kind no provider of this build verifies: such a caller could never
// authenticate, and a start that took it would refuse every one of its calls with nothing naming
// the cause (a build that left the provider out with -tags cipher_no_<kind>).
func servedKinds(cfg config.Config) error {
	built := Kinds()
	refuse := func(setting string, kind identity.Kind) error {
		return fmt.Errorf("%s names a %s account, and this build of cipherd holds no %s identity provider", setting, kind, kind)
	}
	for _, account := range slices.SortedFunc(maps.Keys(cfg.APIAccounts), func(a, b identity.Account) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Address, b.Address))
	}) {
		if !slices.Contains(built, account.Kind) {
			return refuse("BROKER_API_ACCOUNTS", account.Kind)
		}
	}
	for _, service := range cfg.NATS.Services {
		if !slices.Contains(built, service.Account.Kind) {
			return refuse(config.NATSServicesVariable, service.Account.Kind)
		}
	}
	return nil
}

// natsAudiences refuses, on a broker that runs the NATS auth service, a BROKER_NATS_SERVICES
// entry, an edge listener's among them, whose kind of platform token has no NATS verifier (verifiers):
// such a service could never connect, since the auth service answers its connection only once its
// token verifies, so the start refuses it naming the entry and the audience its kind needs. A
// broker that runs no auth service verifies no NATS token and holds no entry to an audience.
func natsAudiences(cfg config.Config, verifiers []identity.CallerVerifier) error {
	if !cfg.NATS.Enabled() {
		return nil
	}
	for _, service := range cfg.NATS.Services {
		if slices.ContainsFunc(verifiers, func(v identity.CallerVerifier) bool { return v.Kind() == service.Account.Kind }) {
			continue
		}
		e := entryOf(service.Account.Kind)
		return fmt.Errorf("%s lists %s, %s, and %s is unset, so its token could not be verified", config.NATSServicesVariable, service.Name(), e.accountNoun, e.natsAudience)
	}
	return nil
}

// entryOf is the registered entry of kind, which servedKinds has already found.
func entryOf(kind identity.Kind) entry {
	for _, e := range entries {
		if e.kind == kind {
			return e
		}
	}
	return entry{kind: kind}
}

// logExchange says once at boot which subject token issuers the exchange accepts, and how many
// entries of each setting it lists, job entries apart, rather than leaving an operator to read it
// out of a refusal.
func logExchange(cfg config.Config) {
	if cfg.ExchangeK8sAudience == "" && cfg.ExchangeGitHubIssuer == "" && cfg.GoogleOIDCAudience == "" {
		slog.Info("token exchange", "state", "unconfigured", "detail", "no subject token issuer is set, so no pod, workflow run or machine exchanges")
		return
	}
	slog.Info("token exchange", "state", "on", "kubernetes", cfg.ExchangeK8sAudience != "", "github", cfg.ExchangeGitHubIssuer != "",
		"accounts", len(cfg.ExchangeAccounts), "read_accounts", len(cfg.ExchangeReadAccounts), "job_accounts", len(cfg.ExchangeJobAccounts),
		"workflows", len(cfg.ExchangeWorkflows), "job_workflows", len(cfg.ExchangeJobWorkflows),
		"machines", len(cfg.ExchangeGoogleAccounts), "job_machines", jobMachines(cfg.ExchangeGoogleAccounts))
}

// jobMachines counts the BROKER_VM_EXCHANGE_JOB_ACCOUNTS entries among the Google entries: the
// fleets whose machines write as a job, naming no person.
func jobMachines(entries []exchange.GoogleEntry) int {
	n := 0
	for _, e := range entries {
		if e.Job.Listed() {
			n++
		}
	}
	return n
}
