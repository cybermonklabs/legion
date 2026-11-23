package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestEachIssuersKeySetIsReadThroughOneCache: discoverVerifiers builds every reader of an issuer's
// key set on one cache of it. A pod token the pod verifier checks and the token exchange's read of
// the same key from the same cluster issuer cost that issuer one key-set read between them, and a
// listed caller's Google token the Google verifier checks and the exchange's read of the same key
// from Google's key set cost Google one.
func TestEachIssuersKeySetIsReadThroughOneCache(t *testing.T) {
	cluster, google := oidctest.New(t), oidctest.New(t)
	clusterKey, googleKey := cluster.PublishKey(t, "cluster-key"), google.PublishKey(t, "google-key")
	cfg := vmConfig(t, cluster.URL(), "example-vm-audience")
	cfg.GoogleOIDCIssuer = google.URL()
	cfg.ExchangeK8sAudience = "example-exchange-audience"
	cfg.ExchangeK8sIssuers = cfg.K8sOIDCIssuers
	found, err := discoverVerifiers(t.Context(), cfg, issuerKeys)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.ExchangeK8s) != 1 {
		t.Fatalf("the exchange takes the tokens of %d cluster issuers, want the one", len(found.ExchangeK8s))
	}

	pod := cluster.Claims("system:serviceaccount:example-ns:example-worker", cfg.K8sOIDCAudience)
	pod["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": "example-pod-uid"}}
	if _, err := found.Pod.Verify(t.Context(), cluster.Mint(t, clusterKey, pod)); err != nil {
		t.Fatalf("the pod verifier's check of a pod token = %v, want it verified", err)
	}
	if _, err := found.ExchangeK8s[0].Keys.Key(t.Context(), clusterKey.KeyID(), "RS256"); err != nil {
		t.Fatalf("the exchange's read of the cluster issuer's key = %v", err)
	}
	if got := cluster.JWKSFetches(); got != 1 {
		t.Fatalf("the pod verifier and the token exchange read the cluster issuer's key set %d times, want once: they read it through one cache", got)
	}

	caller := google.Claims(exampleVMAccount, "example-vm-audience")
	if _, err := found.GoogleIdentities.Verify(t.Context(), google.Mint(t, googleKey, caller)); err != nil {
		t.Fatalf("the Google verifier's check of a listed caller's token = %v, want it verified", err)
	}
	if _, err := found.GoogleKeys.Key(t.Context(), googleKey.KeyID(), "RS256"); err != nil {
		t.Fatalf("the exchange's read of Google's key = %v", err)
	}
	if got := google.JWKSFetches(); got != 1 {
		t.Fatalf("the Google verifier and the token exchange read Google's key set %d times, want once: they read it through one cache", got)
	}
}
