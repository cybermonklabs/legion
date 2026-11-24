//go:build !cipher_no_google

package boot

import (
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/settingsfields"
	"github.com/sjawhar/envoy/internal/oidc"
)

func init() {
	register(entry{kind: google.Kind, rank: 20, open: openGoogle, accountNoun: "a Google account", natsAudience: "BROKER_NATS_GOOGLE_AUDIENCE"})
}

// openGoogle builds the Google provider: the issuer BROKER_GOOGLE_OIDC_ISSUER names, read only
// where BROKER_GOOGLE_OIDC_AUDIENCE or BROKER_NATS_GOOGLE_AUDIENCE gives it an audience, the
// machine and person audiences, the sign-in domain, the exchange's Google entries, and the API's
// and the NATS auth service's callers at their audiences.
func openGoogle(cfg config.Config, keys func(string) (*oidc.Keys, error), _ *Boot) (opened, error) {
	c := google.Config{
		Issuer: cfg.GoogleOIDCIssuer, Audience: cfg.GoogleOIDCAudience, PersonAudience: cfg.SignInDeviceAudience,
		SignInDomain: cfg.SignIn.Domain, Machines: cfg.ExchangeGoogleAccounts,
	}
	if cfg.GoogleOIDCAudience != "" || cfg.NATS.GoogleAudience != "" {
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
	if cfg.GoogleOIDCAudience != "" {
		// A caller on a VM, the Envoy listener, authenticates on the routes BROKER_API_ACCOUNTS
		// names it for with the token its metadata server already hands it, for the audience
		// every machine of the deployment holds a token for.
		built.callers[identity.UseAPI] = p.CallerVerifier(cfg.GoogleOIDCAudience)
		services := slices.Sorted(maps.Values(cfg.VMServicesByAccount))
		slog.Info("google machine identity", "issuer", cfg.GoogleOIDCIssuer, "audience", cfg.GoogleOIDCAudience,
			"services", strings.Join(slices.Compact(services), ","), "fleets", len(cfg.ExchangeGoogleAccounts))
	}
	if cfg.NATS.GoogleAudience != "" {
		built.callers[identity.UseNATS] = p.CallerVerifier(cfg.NATS.GoogleAudience)
	}
	if cfg.SignInDeviceAudience != "" {
		slog.Info("google person sign-in", "issuer", cfg.GoogleOIDCIssuer, "audience", cfg.SignInDeviceAudience, "domain", cfg.SignIn.Domain)
	}
	return built, nil
}
