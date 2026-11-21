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

// natsCalloutBoot is the verifiers of the platform tokens the NATS auth service's services present,
// for a deployment that runs it (cfg.NATS): one verifier per NATS audience set, on the key set the
// broker already reads for that issuer. First, RequireAudiences refuses the start when
// cfg.NATS.Services (which config.Load parsed) names a service whose kind of token has no
// audience; an edge listener's entry needs none. A deployment that runs no auth service needs no
// NATS audience and no key set, and reads none.
func natsCalloutBoot(ctx context.Context, cfg config.Config, discovered verifiers, keysFor keysFunc) (natscallout.PlatformTokens, error) {
	if !cfg.NATS.Enabled() {
		return natscallout.PlatformTokens{}, nil
	}
	if err := natscallout.RequireAudiences(cfg.NATS.Services, cfg.NATS.K8sAudience, cfg.NATS.GoogleAudience); err != nil {
		return natscallout.PlatformTokens{}, err
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
			var err error
			if keys, err = keysFor(ctx, cfg.GoogleOIDCIssuer); err != nil {
				return natscallout.PlatformTokens{}, fmt.Errorf("read the Google issuer's keys for BROKER_NATS_GOOGLE_AUDIENCE: %w", err)
			}
		}
		tokens.Google = api.GoogleTokens{Verifier: oidc.NewWithKeys(cfg.GoogleOIDCIssuer, cfg.NATS.GoogleAudience, keys)}
	}
	return tokens, nil
}
