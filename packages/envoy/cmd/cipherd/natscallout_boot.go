package main

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/oidc"
)

// natsCalloutBoot is what the NATS auth service needs from the boot, for a deployment that runs
// it (cfg.NATS): the services BROKER_NATS_SERVICES lists, each refused at start when its kind of
// platform token has no NATS audience, and one verifier per audience set, on the key set the
// broker already reads for that issuer. A deployment that runs no auth service gets the entries
// BROKER_NATS_SERVICES lists alone, which config.Load holds to edge listeners' entries, read for
// GET /v1/project-repositories and refused at start when one does not parse; it needs no NATS
// audience and no key set, and reads none.
func natsCalloutBoot(ctx context.Context, cfg config.Config, discovered verifiers, keysFor keysFunc) ([]natscallout.ServiceEntry, natscallout.PlatformTokens, error) {
	services, err := natscallout.ParseServices(cfg.NATS.Services)
	if err != nil {
		return nil, natscallout.PlatformTokens{}, err
	}
	if !cfg.NATS.Enabled() {
		return services, natscallout.PlatformTokens{}, nil
	}
	if err := natscallout.RequireAudiences(services, cfg.NATS.K8sAudience, cfg.NATS.GoogleAudience); err != nil {
		return nil, natscallout.PlatformTokens{}, err
	}
	tokens := natscallout.PlatformTokens{K8sIssuer: cfg.K8sOIDCIssuer, GoogleIssuer: cfg.GoogleOIDCIssuer}
	if cfg.NATS.K8sAudience != "" {
		tokens.K8s = enroll.K8sPodVerifier{Verifier: oidc.NewWithKeys(cfg.K8sOIDCIssuer, cfg.NATS.K8sAudience, discovered.K8sKeys)}
	}
	if cfg.NATS.GoogleAudience != "" {
		keys := discovered.GoogleKeys
		if keys == nil {
			// A broker that takes no machine's Google token at its exchange read no Google key
			// set at discovery; the auth service's audience needs one.
			if keys, err = keysFor(ctx, cfg.GoogleOIDCIssuer); err != nil {
				return nil, natscallout.PlatformTokens{}, fmt.Errorf("read the Google issuer's keys for BROKER_NATS_GOOGLE_AUDIENCE: %w", err)
			}
		}
		tokens.Google = api.GoogleTokens{Verifier: oidc.NewWithKeys(cfg.GoogleOIDCIssuer, cfg.NATS.GoogleAudience, keys)}
	}
	return services, tokens, nil
}
