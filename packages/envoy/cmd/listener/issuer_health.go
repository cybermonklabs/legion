package main

import (
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
)

// A listener starts and serves while an identity token issuer cannot be read, and answers each
// token of that issuer 503 until it has read the issuer's discovery document (oidc.LazyVerifier),
// so a roll completes while an issuer is unreadable: a wrong ENVOY_OIDC_ISSUER, an issuer that is
// down, or a pod with no egress to it. Nothing in the pod's readiness shows that state, since a
// listener in it serves everything else. The two reports below are what show it: /healthz's "oidc"
// and "google" fields (healthzHandler adds them to every answer), and the
// envoy_listener_oidc_issuer_discovered gauge, which an alert reads once the deployment scrapes the
// listener's /metrics. Neither decides /healthz's status, for the reason Dispatch's service_tokens
// field does not: a probe that failed on it would take every other service away for an outage the
// listener serves through.
//
// The trade they make: /healthz and /metrics need no credential, so any caller that reaches the
// listener's port learns from them which identity token pairs it takes (a pair that is not
// configured is null and writes no series), the fact the 401 body keeps from an unauthenticated
// caller (apiAuth). That leak is small: before an issuer is read, a JWT-shaped bearer's 503 shows
// as much, and which issuers a listener takes decides nothing about who it admits.

// The issuer label of envoy_listener_oidc_issuer_discovered: cluster is the ENVOY_OIDC_ISSUER
// pair's issuer, google the Google pair's.
const (
	issuerCluster = "cluster"
	issuerGoogle  = "google"
)

// registerIssuerGauge registers envoy_listener_oidc_issuer_discovered: for each identity token
// issuer the listener is configured for, 1 once it has read the issuer's discovery document
// (oidc.LazyVerifier.Discovered) and 0 until then, read at each scrape. A listener configured for
// neither writes no sample.
func registerIssuerGauge(met *metrics.Registry, auth listenerAPIAuth) {
	met.NewGaugeSetFunc("envoy_listener_oidc_issuer_discovered",
		"1 once the listener has read the identity token issuer's discovery document, 0 while it has not and answers that issuer's tokens 503, by issuer (cluster, google)",
		func() []metrics.GaugeSample {
			var samples []metrics.GaugeSample
			add := func(issuer string, verifier *oidc.LazyVerifier) {
				var value int64
				if verifier.Discovered() {
					value = 1
				}
				samples = append(samples, metrics.GaugeSample{Labels: [][2]string{{"issuer", issuer}}, Value: value})
			}
			if auth.verifier != nil {
				add(issuerCluster, auth.verifier)
			}
			if auth.google != nil {
				add(issuerGoogle, auth.google.verifier)
			}
			return samples
		})
}

// issuerHealthFields is the "oidc" and "google" fields of a /healthz answer: whether the listener
// has read the discovery document of the ENVOY_OIDC_ISSUER pair's issuer and of Google's, each
// null when that pair is not configured. healthzHandler adds them to every answer it writes
// (writeHealth).
func issuerHealthFields(auth listenerAPIAuth) map[string]any {
	fields := map[string]any{"oidc": nil, "google": nil}
	if auth.verifier != nil {
		fields["oidc"] = auth.verifier.Discovered()
	}
	if auth.google != nil {
		fields["google"] = auth.google.verifier.Discovered()
	}
	return fields
}
