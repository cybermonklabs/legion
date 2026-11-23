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
// cfg.NATS.Services (which config.Load parsed, refusing there, naming the entry, every issuer
// name config.AccountIssuers refuses) names a service, an edge listener among them, whose kind of
// token has no audience. A Kubernetes token is checked against the issuer of
// BROKER_K8S_OIDC_ISSUER its iss names, through the clusters' issuer set the pod routes read in
// the background (loggedIssuers.audience), so the auth service reads no issuer of its own and an
// issuer that cannot be read stops no boot. A deployment that runs no auth service needs no NATS
// audience and no key set, and reads none.
func natsCalloutBoot(ctx context.Context, cfg config.Config, discovered verifiers, keysFor keysFunc) (natscallout.PlatformTokens, error) {
	if !cfg.NATS.Enabled() {
		return natscallout.PlatformTokens{}, nil
	}
	if err := natscallout.RequireAudiences(cfg.NATS.Services, cfg.NATS.K8sAudience, cfg.NATS.GoogleAudience); err != nil {
		return natscallout.PlatformTokens{}, err
	}
	tokens := natscallout.PlatformTokens{K8sIssuers: cfg.K8sOIDCIssuers, GoogleIssuer: cfg.GoogleOIDCIssuer}
	if cfg.NATS.K8sAudience != "" {
		tokens.K8s = enroll.K8sPodVerifier{Issuers: discovered.Clusters.audience(cfg.NATS.K8sAudience)}
	}
	if cfg.NATS.GoogleAudience != "" {
		keys := discovered.GoogleKeys
		if keys == nil {
			// A broker that takes no machine's Google token at its exchange read no Google key
			// set at discovery; the auth service's audience needs one, which logs its changes
			// as discovery's does (logKeySet).
			read, err := keysFor(ctx, cfg.GoogleOIDCIssuer)
			if err != nil {
				return natscallout.PlatformTokens{}, fmt.Errorf("read the Google issuer's keys for BROKER_NATS_GOOGLE_AUDIENCE: %w", err)
			}
			keys = logKeySet(read, cfg.GoogleOIDCIssuer)
		}
		tokens.Google = api.GoogleTokens{Verifier: oidc.NewWithKeys(cfg.GoogleOIDCIssuer, cfg.NATS.GoogleAudience, keys)}
	}
	return tokens, nil
}
