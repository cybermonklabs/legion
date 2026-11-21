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
// it (cfg.NATS): the services BROKER_NATS_SERVICES lists, each a Kubernetes account's with the
// issuers of BROKER_K8S_OIDC_ISSUER its token is taken from, and each refused at start when its
// kind of platform token has no NATS audience; and one verifier per audience set, on the key set
// the broker already reads for that issuer. A Kubernetes token is checked against the issuer of
// BROKER_K8S_OIDC_ISSUER its iss names, through the clusters' issuer set the pod routes read in
// the background (loggedIssuers.audience), so the auth service reads no issuer of its own and an
// issuer that cannot be read stops no boot. A deployment that runs no auth service gets nothing
// and reads nothing.
func natsCalloutBoot(ctx context.Context, cfg config.Config, discovered verifiers, keysFor keysFunc) ([]natscallout.ServiceEntry, natscallout.PlatformTokens, error) {
	if !cfg.NATS.Enabled() {
		return nil, natscallout.PlatformTokens{}, nil
	}
	services, err := natscallout.ParseServices(cfg.NATS.Services, cfg.K8sOIDCIssuers)
	if err != nil {
		return nil, natscallout.PlatformTokens{}, err
	}
	if err := natscallout.RequireAudiences(services, cfg.NATS.K8sAudience, cfg.NATS.GoogleAudience); err != nil {
		return nil, natscallout.PlatformTokens{}, err
	}
	tokens := natscallout.PlatformTokens{K8sIssuers: cfg.K8sOIDCIssuers, GoogleIssuer: cfg.GoogleOIDCIssuer}
	if cfg.NATS.K8sAudience != "" {
		tokens.K8s = enroll.K8sPodVerifier{Issuers: discovered.Clusters.audience(cfg.NATS.K8sAudience)}
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
