// Package providers is the identity providers a client of this build holds, in rank order: the
// sources a machine proves itself with, the detectors that find one where no variable names it,
// and the service-token sources a process of the bus connects by. It is the client's half of
// internal/cipher/identity/boot, which is the broker's, and one file per provider behind a build
// tag registers each, so `-tags cipher_no_<kind>` takes a provider out of both halves at once.
//
// Nothing outside a provider's own package and these two registries names a provider: a client
// asks here which identities exist, and the names it prints are the ones registered.
package providers

import (
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/identity"
)

// entry is one provider's client half, with the rank that orders it against the others.
type entry struct {
	kind         identity.Kind
	rank         int
	sources      []identity.SourceEntry
	detectors    []identity.Detector
	serviceToken *identity.ServiceTokenSource
	actorType    func(subjectType string) (string, bool)
	// personType is the subject token type a person's own sign-in on this machine is presented
	// under, for the provider that verifies people's tokens; empty for one that does not.
	personType string
}

// PersonTokenType is the subject token type a person's own sign-in token is traded under, and
// false where this build holds no provider that verifies a person's tokens: the device flow then
// has no exchange to present a sign-in at.
func PersonTokenType() (string, bool) {
	for _, e := range entries {
		if e.personType != "" {
			return e.personType, true
		}
	}
	return "", false
}

// entries are this build's providers, in rank order.
var entries []entry

// register adds a provider's client half; each per-kind file calls it once from its init.
func register(e entry) {
	entries = append(entries, e)
	slices.SortStableFunc(entries, func(a, b entry) int { return a.rank - b.rank })
}

// Kinds are the providers of this build, in rank order.
func Kinds() []identity.Kind {
	kinds := make([]identity.Kind, 0, len(entries))
	for _, e := range entries {
		kinds = append(kinds, e.kind)
	}
	return kinds
}

// Sources are every identity AGENT_SECRETS_MACHINE_IDENTITY names, in rank order and, inside a
// provider, in the order that provider declares them: kubernetes, metadata, pool, github-actions
// today. A refusal lists them in this order, so the order is the one a person reads.
func Sources() []identity.SourceEntry {
	var sources []identity.SourceEntry
	for _, e := range entries {
		sources = append(sources, e.sources...)
	}
	return sources
}

// Source is the identity named name, and false for a name no provider of this build declares.
func Source(name string) (identity.SourceEntry, bool) {
	for _, source := range Sources() {
		if source.Name == name {
			return source, true
		}
	}
	return identity.SourceEntry{}, false
}

// Detectors are the detectors of this build, in rank order: the places a machine that names no
// identity is looked for, each with the settings field that carries the audience it needs.
func Detectors() []identity.Detector {
	var detectors []identity.Detector
	for _, e := range entries {
		detectors = append(detectors, e.detectors...)
	}
	return detectors
}

// SourceVariables are the variables every source reads, each once, in first-seen order: what a
// program that writes a machine's environment (the helper's install) carries over. A variable the
// platform sets for each run (identity.Variable.Platform) is left out, since no deployment sets it.
func SourceVariables() []identity.Variable {
	var variables []identity.Variable
	seen := map[string]bool{}
	for _, source := range Sources() {
		for _, variable := range source.Variables {
			if variable.Platform || seen[variable.Name] {
				continue
			}
			seen[variable.Name] = true
			variables = append(variables, variable)
		}
	}
	return variables
}

// ActorTokenType is the actor token type a trade of subjectType carries, and false where that
// subject takes no actor: today a person's sign-in on a machine, whose actor is that machine's
// own Google identity token.
func ActorTokenType(subjectType string) (string, bool) {
	for _, e := range entries {
		if e.actorType == nil {
			continue
		}
		if actor, ok := e.actorType(subjectType); ok {
			return actor, true
		}
	}
	return "", false
}

// ServiceTokenSources are the ways a service of the bus proves itself to the NATS auth service
// with a provider's own token, in rank order: the google provider's alone today.
func ServiceTokenSources() []identity.ServiceTokenSource {
	var sources []identity.ServiceTokenSource
	for _, e := range entries {
		if e.serviceToken != nil {
			sources = append(sources, *e.serviceToken)
		}
	}
	return sources
}

// SourceNames is every source's name, which a refusal lists.
func SourceNames() []string {
	sources := Sources()
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, source.Name)
	}
	return names
}

// NewHTTPClient is the client a source makes its own calls with when its caller names none.
func NewHTTPClient() *http.Client { return &http.Client{} }
