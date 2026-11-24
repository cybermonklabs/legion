//go:build !cipher_no_google

package boot

import (
	"log/slog"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/settingsfields"
	"github.com/sjawhar/envoy/internal/oidc"
)

func init() {
	register(entry{
		kind: google.Kind, rank: 20,
		zero: func() any { return google.Config{} },
		load: func(getenv func(string) string) (any, error) { return google.LoadConfig(getenv) },
		withShared: func(cfg any, shared Shared) any {
			c := cfg.(google.Config)
			c.SignInDomain = shared.SignInDomain
			return c
		},
		open:         openGoogle,
		parseAccount: google.ParseAccount,
		accountForm:  google.AccountForm,
		serves: func(cfg any, use identity.Use, site identity.Site) error {
			return cfg.(google.Config).Serves(use, site)
		},
		check: func(cfg any, shared Shared) error {
			return cfg.(google.Config).Check(shared.Callers[google.Kind])
		},
		mentions: func(cfg any, account identity.Account) []identity.Mention {
			return cfg.(google.Config).Mentions(account)
		},
		services:    func(cfg any) []string { return cfg.(google.Config).Services() },
		logSettings: logGoogle,
		exchanges:   func(cfg any) bool { return cfg.(google.Config).Audience != "" },
		exchangeFields: func(cfg any) []any {
			c := cfg.(google.Config)
			// Every machine counts, a job's among them, and the job ones again on their own, as
			// the line always counted BROKER_VM_EXCHANGE_JOB_ACCOUNTS.
			jobs := 0
			for _, entry := range c.Machines {
				if entry.Job.Listed() {
					jobs++
				}
			}
			return []any{"machines", len(c.Machines), "job_machines", jobs}
		},
	})
}

// openGoogle builds the Google provider from its own settings: the issuer
// BROKER_GOOGLE_OIDC_ISSUER names, read only where BROKER_GOOGLE_OIDC_AUDIENCE or
// BROKER_NATS_GOOGLE_AUDIENCE gives it an audience, the machine and person audiences, the
// sign-in domain config hands over, the exchange's Google entries, and the API's and the NATS
// auth service's callers at their audiences.
func openGoogle(cfg any, shared Shared, keys func(string) (*oidc.Keys, error), _ *Boot) (opened, error) {
	c := cfg.(google.Config)
	if c.Audience != "" || c.NATSAudience != "" {
		var err error
		if c.Keys, err = keys(c.Issuer); err != nil {
			return opened{}, err
		}
	}
	p, err := google.New(c)
	if err != nil {
		return opened{}, err
	}
	// The provider's own field of GET /v1/settings, google_exchange_audience, documented on its
	// SettingsFields struct, which cipher-refgen reads.
	built := opened{provider: p, callers: map[identity.Use]identity.CallerVerifier{}, settings: settingsfields.Of(p.Settings())}
	if c.Audience != "" {
		// A caller on a VM, the Envoy listener, authenticates on the routes BROKER_API_ACCOUNTS
		// names it for with the token its metadata server already hands it, for the audience
		// every machine of the deployment holds a token for.
		built.callers[identity.UseAPI] = p.CallerVerifier(c.Audience)
	}
	if c.NATSAudience != "" {
		built.callers[identity.UseNATS] = p.CallerVerifier(c.NATSAudience)
	}
	return built, nil
}

// logGoogle says what this provider's settings turn on, as the boot said before the variables
// moved: one line for the machine identity where an audience is set, and one for a person's
// sign-in where the device audience is.
func logGoogle(cfg any) {
	c := cfg.(google.Config)
	if c.Audience != "" {
		slog.Info("google machine identity", "issuer", c.Issuer, "audience", c.Audience,
			"services", strings.Join(c.Services(), ","), "fleets", len(c.Machines))
	}
	if c.PersonAudience != "" {
		slog.Info("google person sign-in", "issuer", c.Issuer, "audience", c.PersonAudience, "domain", c.SignInDomain)
	}
}
