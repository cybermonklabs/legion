package oidc

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DiscoveryTimeout bounds one discovery read of an issuer. The broker takes its
// reads of the K8s and Google issuers before it binds its port, and its read of
// the GitHub exchange issuer after the bind but before it serves, so an issuer
// that accepts the connection and never answers has to refuse the broker's boot
// rather than hang it: a hung boot serves no health endpoint and reports nothing.
// The listener reads in the background instead and reads again until a read succeeds
// (apiVerifier.discover in cmd/listener), so this bounds each of its reads and
// nothing bounds its wait. Dispatch discovers in the background too
// (LazyVerifier, discovery.go).
const DiscoveryTimeout = 30 * time.Second

// ConfigFromEnv reads the both-or-neither issuer/audience pair named by
// issuerVar and audienceVar. Half a pair is a refusal naming the variable that
// is missing; neither set returns two empty strings and no error, which is
// "run no verifier". It reads getenv and returns errors and nothing else, so a
// binary can call it from wherever it validates the rest of its configuration
// — the discovery read is Discover's job, never this one's.
func ConfigFromEnv(getenv func(string) string, issuerVar, audienceVar string) (issuer, audience string, err error) {
	issuer = strings.TrimSpace(getenv(issuerVar))
	audience = strings.TrimSpace(getenv(audienceVar))
	switch {
	case issuer == "" && audience == "":
		return "", "", nil
	case issuer == "":
		return "", "", fmt.Errorf("%s is required when %s is set", issuerVar, audienceVar)
	case audience == "":
		return "", "", fmt.Errorf("%s is required when %s is set", audienceVar, issuerVar)
	}
	return issuer, audience, nil
}

// Discover is New under a deadline, and (nil, nil) for an unconfigured pair.
// The deadline bounds the discovery read only: the key set refreshes on its own
// background context, so a boot deadline never disables key rotation.
func Discover(ctx context.Context, issuer, audience string, timeout time.Duration) (*Verifier, error) {
	if issuer == "" {
		return nil, nil
	}
	discovery, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return New(discovery, issuer, audience)
}
