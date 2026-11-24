// Package boot is the broker's registry of caller identity providers: one file per provider,
// behind the build tag !cipher_no_<kind>, registers how that provider reads its own BROKER_*
// variables and how it is built from them, Load reads every provider's settings, and Open builds
// the identity.Registry cmd/cipherd verifies every caller token through. A provider package never
// registers itself: this directory lists the providers of a build, and a build tag removes one.
//
// It imports no configuration package: internal/cipher/config reads its own settings through
// Load and holds the answer whole (config.Config.Identity), so nothing here reads a variable a
// provider does not own.
package boot

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

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
	// Pods verifies a pod's enrollment token and a service's login token at the pod provider's
	// own audience; nil on a broker told about no cluster.
	Pods identity.PodVerifier
	// PodRegistration verifies a registration's pod token at BROKER_POD_REGISTRATION_AUDIENCE, on
	// the cluster's key set; nil on a broker with no such audience.
	PodRegistration identity.PodVerifier
	// SettingsFields are the providers' own fields of GET /v1/settings, by JSON name, which the
	// broker merges with its store's (settingsfields.Merge); never nil.
	SettingsFields map[string]string
}

// Settings is every registered provider's own parsed configuration, as Load read it from the
// broker's environment: one value per kind, in rank order. config holds it whole and reads no
// variable of a provider's.
type Settings struct {
	// configs is each kind's Config, as its LoadConfig answered (ConfigOf reads one).
	configs map[identity.Kind]any
	// shared is what a provider needs from the broker's own settings rather than from its own
	// variables; today the Workspace domain BROKER_SIGNIN_DOMAIN names, which the google
	// provider holds a signing-in person to.
	shared Shared
}

// Shared is what config knows and the providers do not: the kinds of account the settings config
// owns name as callers, and the sign-in domain the broker holds a person to.
type Shared struct {
	// Callers is the kinds BROKER_API_ACCOUNTS and BROKER_NATS_SERVICES name.
	Callers map[identity.Kind]bool
	// SignInDomain is BROKER_SIGNIN_DOMAIN.
	SignInDomain string
	// NATSServices are the services BROKER_NATS_SERVICES lists, each with its kind, on a broker
	// that runs the NATS auth service; none on one that does not, which verifies no NATS token.
	NATSServices []NATSService
}

// NATSService is one listed service of the NATS auth service: the kind of account its token
// names, and the entry's name in a refusal.
type NATSService struct {
	Kind identity.Kind
	Name string
}

// opened is what one provider's registry file built from its settings: the provider, its caller
// verifier for each use its settings give it an audience for, and the fields it adds to GET
// /v1/settings (settingsfields.Of of its own SettingsFields struct; nil for none).
type opened struct {
	provider identity.Provider
	callers  map[identity.Use]identity.CallerVerifier
	settings map[string]string
}

// entry is one provider's registration: its kind, its rank (the order the registry asks the
// providers in), how its variables are read (load), how it is built (open), and the questions
// config asks it about an account or a setting of its own (serves, podsServed, mentions,
// services, check). keys reads an issuer's key set once per Open, however many providers or
// verifiers read that issuer. parseAccount reads an account of this kind wherever the broker's
// settings name one, and accountForm is how such an account is written; parsePodAccount is the
// pod provider's own reading, for the settings outside its package that name a pod's account.
// exchanges says whether this provider accepts a subject token at all, and exchangeFields what
// the one boot line says of it, so the "token exchange" record reads as it always has.
type entry struct {
	kind identity.Kind
	rank int
	// zero is this provider's Config as the broker reads one it was told nothing about, which
	// every question of the provider's is answered from when the settings were never loaded.
	zero func() any
	// withShared is this provider's config carrying the fields config hands every provider
	// (Shared), so one read answers every question of it: the opener, the checks and the boot
	// lines all see the same configuration.
	withShared      func(cfg any, shared Shared) any
	load            func(getenv func(string) string) (any, error)
	open            func(cfg any, shared Shared, keys func(issuer string) (*oidc.Keys, error), b *Boot) (opened, error)
	parseAccount    func(setting, text, account string) (identity.Account, bool, error)
	parsePodAccount func(setting, text, account string) (string, error)
	accountForm     string
	serves          func(cfg any, use identity.Use, site identity.Site) error
	podsServed      func(cfg any, setting string) error
	check           func(cfg any, shared Shared) error
	mentions        func(cfg any, account identity.Account) []identity.Mention
	services        func(cfg any) []string
	logSettings     func(cfg any)
	exchanges       func(cfg any) bool
	exchangeFields  func(cfg any) []any
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

// Load reads every registered provider's own variables from getenv, in rank order, and answers
// them whole. Each refusal is the provider's own, in the words config.Load answered before the
// variables moved; the first provider that refuses stops the load, so a configuration with
// faults of two kinds is refused for the kubernetes one, then the google one, then github's.
func Load(getenv func(string) string) (Settings, error) {
	s := Settings{configs: make(map[identity.Kind]any, len(entries))}
	for _, e := range entries {
		cfg, err := e.load(getenv)
		if err != nil {
			return Settings{}, err
		}
		s.configs[e.kind] = cfg
	}
	return s, nil
}

// WithShared is s carrying what config knows: the kinds its own settings name as callers and the
// sign-in domain. config sets it once, after it has read those settings.
func (s Settings) WithShared(shared Shared) Settings {
	s.shared = shared
	return s
}

// ConfigOf is kind's own parsed configuration, as every question of that provider reads it
// (configOf: the shared fields merged in), and false for a kind this build does not hold or one
// whose settings were never loaded.
func ConfigOf[T any](s Settings, kind identity.Kind) (T, bool) {
	if _, held := s.configs[kind]; !held {
		var zero T
		return zero, false
	}
	typed, ok := s.configOf(kind).(T)
	return typed, ok
}

// configOf is kind's parsed configuration, carrying the fields config hands every provider
// (WithShared), and the provider's own zero value for a Settings that was never loaded: a zero
// Settings is a broker told nothing, which every provider reads as trusting no issuer and
// listing nothing, rather than a nil no question of a provider's could be asked of.
func (s Settings) configOf(kind identity.Kind) any {
	for _, e := range entries {
		if e.kind != kind {
			continue
		}
		cfg, held := s.configs[kind]
		if !held {
			cfg = e.zero()
		}
		if e.withShared != nil {
			cfg = e.withShared(cfg, s.shared)
		}
		return cfg
	}
	return nil
}

// Serves answers nil when a provider of kind verifies a caller of use, and that provider's own
// sentence, naming the variable its tokens need, when it does not. A kind no provider of this
// build holds is refused naming the setting and the kind: such a caller could never authenticate,
// and a start that took it would refuse every one of its calls with nothing naming the cause (a
// build that left the provider out with -tags cipher_no_<kind>).
func (s Settings) Serves(kind identity.Kind, use identity.Use, site identity.Site) error {
	for _, e := range entries {
		if e.kind == kind {
			return e.serves(s.configOf(kind), use, site)
		}
	}
	return fmt.Errorf("%s names a %s account, and this build of cipherd holds no %s identity provider", site.Setting, kind, kind)
}

// ServesAny reports whether any provider verifies a caller of use: the NATS auth service runs
// only for a broker that can verify at least one kind of service token.
func (s Settings) ServesAny(use identity.Use) bool {
	for _, e := range entries {
		if e.serves(s.configOf(e.kind), use, identity.Site{}) == nil {
			return true
		}
	}
	return false
}

// PodsServed answers nil when the pod provider trusts a cluster, and its sentence, naming setting
// and the variables a pod's token needs, when it does not. A build with no pod provider refuses
// every setting about pods.
func (s Settings) PodsServed(setting string) error {
	for _, e := range entries {
		if e.podsServed != nil {
			return e.podsServed(s.configOf(e.kind), setting)
		}
	}
	return fmt.Errorf("%s names a pod's account, and this build of cipherd holds no identity provider that verifies a pod's token", setting)
}

// Check is each provider's own rule against what config owns, in rank order.
func (s Settings) Check(shared Shared) error {
	for _, e := range entries {
		if e.check == nil {
			continue
		}
		if err := e.check(s.configOf(e.kind), shared); err != nil {
			return err
		}
	}
	return nil
}

// Mentions is every place a provider's settings name account, in rank order.
func (s Settings) Mentions(account identity.Account) []identity.Mention {
	var mentions []identity.Mention
	for _, e := range entries {
		mentions = append(mentions, e.mentions(s.configOf(e.kind), account)...)
	}
	return mentions
}

// Services is every service the providers' entries name, sorted and each once: the owners a
// secret may name beside BROKER_SERVICES'.
func (s Settings) Services() []string {
	var services []string
	for _, e := range entries {
		services = append(services, e.services(s.configOf(e.kind))...)
	}
	slices.Sort(services)
	return slices.Compact(services)
}

// ErrAccountForm is the sentinel every refusal of an account written in no registered provider's
// form wraps, which each setting matches to refuse in its own words (errors.Is).
var ErrAccountForm = identity.ErrAccountForm

// AccountFormError is that refusal, listing the forms of this build's providers, in rank order.
// It is a function rather than a variable because each provider registers in its own init, which
// runs after a package-level variable is set.
func AccountFormError() error {
	return fmt.Errorf("%w: an account is %s", ErrAccountForm, accountForms())
}

// AccountForms is every registered provider's account form, in rank order: how a setting names
// an account of each kind this build reads.
func AccountForms() []string {
	var forms []string
	for _, e := range entries {
		if e.accountForm != "" {
			forms = append(forms, e.accountForm)
		}
	}
	return forms
}

// PodAccountForm is how a setting names a pod's account, the pod provider's own form, and "" in
// a build that holds no pod provider.
func PodAccountForm() string {
	for _, e := range entries {
		if e.parsePodAccount != nil {
			return e.accountForm
		}
	}
	return ""
}

// accountForms is every registered provider's account form, in rank order, joined as a sentence
// lists them: "A or B", "A, B or C".
func accountForms() string {
	var forms []string
	for _, e := range entries {
		if e.accountForm != "" {
			forms = append(forms, e.accountForm)
		}
	}
	switch len(forms) {
	case 0:
		return "of no form this build of cipherd reads: it holds no identity provider that names one"
	case 1:
		return forms[0]
	}
	return strings.Join(forms[:len(forms)-1], ", ") + " or " + forms[len(forms)-1]
}

// ParseAccount reads account as the broker's settings write one, asking each provider in rank
// order: the first that claims the text answers it, with its own refusal for an account of its
// form that it does not take. An account no provider claims is ErrAccountForm. Every setting that
// names a caller's account reads it here, so no setting admits an account another refuses.
func ParseAccount(setting, text, account string) (identity.Account, error) {
	for _, e := range entries {
		if e.parseAccount == nil {
			continue
		}
		parsed, claimed, err := e.parseAccount(setting, text, account)
		if claimed {
			return parsed, err
		}
	}
	return identity.Account{}, AccountFormError()
}

// ParsePodAccount reads account as a pod's, for the settings that name one outside a provider
// package (BROKER_SERVICES, BROKER_SERVICE_LOGINS, BROKER_POD_OPERATORS,
// BROKER_SESSION_ACCOUNT_PAIRS, BROKER_POD_REGISTRARS): the pod provider's own reading, refused
// in its words.
func ParsePodAccount(setting, text, account string) (string, error) {
	for _, e := range entries {
		if e.parsePodAccount != nil {
			return e.parsePodAccount(setting, text, account)
		}
	}
	return "", fmt.Errorf("%s names a pod's account, and this build of cipherd holds no identity provider that reads one", setting)
}

// Open builds every provider of this build from its own settings, reading each issuer's keys once
// through keysFor, and the registry of them with the caller verifiers each use is authenticated
// with, in rank order.
func Open(ctx context.Context, s Settings, keysFor KeysFunc) (Boot, error) {
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
	settings := map[string]string{}
	for _, e := range entries {
		built, err := e.open(s.configOf(e.kind), s.shared, keys, &b)
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
	registry, err := identity.NewRegistry(providers, callers)
	if err != nil {
		return Boot{}, err
	}
	// Every listed service of the NATS auth service needs its own kind's NATS audience: without
	// it the auth service could not verify that service's token, so a broker that would refuse
	// every one of its connections refuses to start instead. It is checked here, where the
	// verifiers are built, rather than where the settings are read, because a broker that runs
	// no auth service lists no service here and holds none of them to an audience.
	for _, service := range s.shared.NATSServices {
		if !slices.ContainsFunc(callers[identity.UseNATS], func(v identity.CallerVerifier) bool { return v.Kind() == service.Kind }) {
			if err := s.Serves(service.Kind, identity.UseNATS, identity.Site{Setting: "BROKER_NATS_SERVICES", Entry: service.Name}); err != nil {
				return Boot{}, err
			}
		}
	}
	b.Registry = registry
	b.SettingsFields = settings
	s.logExchange()
	return b, nil
}

// logExchange says once at boot which subject token issuers the exchange accepts, rather than
// leaving an operator to read it out of a refusal: one line from each provider that accepts one,
// and one line saying so when none does.
func (s Settings) logExchange() {
	var logged bool
	for _, e := range entries {
		if e.logSettings == nil {
			continue
		}
		e.logSettings(s.configOf(e.kind))
	}
	for _, e := range entries {
		if e.exchanges != nil && e.exchanges(s.configOf(e.kind)) {
			logged = true
		}
	}
	if !logged {
		slog.Info("token exchange", "state", "unconfigured", "detail", "no subject token issuer is set, so no pod, workflow run or machine exchanges")
		return
	}
	fields := []any{"state", "on"}
	for _, e := range entries {
		if e.exchangeFields != nil {
			fields = append(fields, e.exchangeFields(s.configOf(e.kind))...)
		}
	}
	slog.Info("token exchange", fields...)
}
