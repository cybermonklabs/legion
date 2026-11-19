package gcp

import (
	"net/url"
	"strings"
)

// urlCarriesExtras is whether raw, which parsed as u, carries a user (a password with it or not),
// a query or a fragment, an empty query or fragment included (`https://host?`, `https://host#`),
// which url.Parse reads as none. It is the one rule for every URL the component takes whole and
// hands on as it was written: Dispatch's agentSecretsUrl, rendered as a plain environment
// variable, where any of the four could carry a credential, and an OIDC issuer, which a verifier
// discovers and compares each token's iss to, where any of them makes another URL than the one a
// token names. Each caller keeps its own rule for the scheme, the host and the path, and its own
// refusal: agentSecretsUrl's never quotes the value.
func urlCarriesExtras(raw string, u *url.URL) bool {
	return u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#")
}
