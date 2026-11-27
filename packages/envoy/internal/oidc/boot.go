package oidc

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/natsurl"
)

// DiscoveryTimeout bounds one discovery read of an issuer, for a binary that reads its issuer
// before it serves (the broker's issuerKeys). The broker takes its reads of the K8s and Google
// issuers before it binds its port, and its read of the GitHub exchange issuer after the bind but
// before it serves, so an issuer that accepts the connection and never answers has to refuse the
// broker's boot rather than hang it: a hung boot serves no health endpoint and reports nothing.
// Dispatch and the Envoy listener read nothing at boot and discover in the background instead
// (LazyVerifier, discovery.go).
const DiscoveryTimeout = 30 * time.Second

// ConfigFromEnv reads the both-or-neither issuer/audience pair named by
// issuerVar and audienceVar. Half a pair is a refusal naming the variable that
// is missing; neither set returns two empty strings and no error, which is
// "run no verifier". An issuer IssuerRefusal refuses is refused too. It reads
// getenv and returns errors and nothing else, so a binary can call it from
// wherever it validates the rest of its configuration — the discovery read is
// the verifier's own job, never this one's.
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
	if err := IssuerRefusal(issuerVar, issuer); err != nil {
		return "", "", err
	}
	return issuer, audience, nil
}

// IssuerRefusal is why issuer, the value of the setting named setting, is no issuer URL, or nil:
// one that does not parse, one with a user name or a password, one with an "@" after its host
// (natsurl.UserinfoPastHost: where a credential holding a "/", "?" or "#" written as the user
// lands, its head read as the host), and one with a query or a fragment, an empty one included,
// which OpenID Connect Discovery 1.0 (§3) says an issuer identifier has none of, and where a token
// would be pasted. The issuer is printed in boot lines, discovery errors and the broker's sign-in
// page, so a credential pasted there would print. The refusal names the setting alone, never the
// value. "" (unset) is no refusal.
func IssuerRefusal(setting, issuer string) error {
	if issuer == "" {
		return nil
	}
	parsed, err := url.Parse(issuer)
	switch {
	case err != nil:
		return fmt.Errorf("%s does not parse as a URL", setting)
	case parsed.User != nil:
		return fmt.Errorf("%s cannot be read: it holds a user name or a password, which no issuer URL has", setting)
	case natsurl.UserinfoPastHost(parsed):
		return fmt.Errorf("%s cannot be read: %s", setting, natsurl.UserinfoPastHostReason)
	case parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(issuer, "#"):
		return fmt.Errorf("%s cannot be read: it holds a query or a fragment, which no issuer identifier has", setting)
	}
	return nil
}
