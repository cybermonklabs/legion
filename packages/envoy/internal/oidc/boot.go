package oidc

import (
	"fmt"
	"strings"
	"time"
)

// DiscoveryTimeout bounds an issuer's discovery read at boot, for a binary that reads its issuer
// before it binds its port (the broker's issuerKeys): an issuer that accepts the connection and
// never answers has to refuse the boot rather than hang it, since a hung boot serves no health
// endpoint and reports nothing. Dispatch and the Envoy listener read nothing at boot and discover
// in the background instead (LazyVerifier, discovery.go).
const DiscoveryTimeout = 30 * time.Second

// ConfigFromEnv reads the both-or-neither issuer/audience pair named by
// issuerVar and audienceVar. Half a pair is a refusal naming the variable that
// is missing; neither set returns two empty strings and no error, which is
// "run no verifier". It reads getenv and returns errors and nothing else, so a
// binary can call it from wherever it validates the rest of its configuration
// — the discovery read is the verifier's own job, never this one's.
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
