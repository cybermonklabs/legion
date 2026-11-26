// packages/envoy/internal/cipher/requests/kindreach.go
// What a grant was decided under of the kind its name is behind: the kind's hosts and headers. A
// kind that loses a host or a header ends the grants decided under it, and one that only gains one
// ends none. No fingerprint of the profiles file alone tells the two apart: a value that stays the
// same across a gain stays the same across the loss that undoes it, and the broker reads the file
// only at start, so it never holds the file before. So each name a request names records its
// kind's hosts and headers (request_secrets.airlock_kind) when the request is decided and again
// when it is approved, and a release compares them with the kind the policy holds then.
package requests

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/fold"
)

// kindReach is the hosts a name's kind lets its credential go to and the headers the credential
// travels in (proxyrules.Kind.HeaderNames, so a kind that names none travels in Authorization), as
// a request records them, each as the profiles file writes it.
type kindReach struct {
	Hosts   []string `json:"hosts"`
	Headers []string `json:"headers"`
}

// reachOf is the kind name is behind in set's rules as a request records it, and nil for a name
// behind no kind. A name a dpop block binds is bound by an inline rule and no kind, and records
// none: the entry digest of its minted grants covers its hosts (config.MintedEntryDigest).
func reachOf(set *policy.Set, name string) *kindReach {
	kind := boundKind(set.ProxyRules(), name)
	if kind == nil {
		return nil
	}
	return &kindReach{Hosts: kind.Hosts, Headers: kind.HeaderNames()}
}

// boundKind is the kind name is behind in rules, nil for a name behind no kind or bound by an
// inline rule alone.
func boundKind(rules proxyrules.Rules, name string) *proxyrules.Kind {
	binding := rules.Lookup(name)
	if binding == nil || binding.Kind == "" {
		return nil
	}
	return rules.Kind(binding.Kind)
}

// column is r as request_secrets.airlock_kind holds it: its JSON, and NULL for nil.
func (r *kindReach) column() *string {
	if r == nil {
		return nil
	}
	// Marshal cannot fail for two lists of strings.
	encoded, _ := json.Marshal(r)
	text := string(encoded)
	return &text
}

// kindReachOf is request_secrets.airlock_kind as read, nil for NULL, which is a name behind no
// kind and every row written before the column.
func kindReachOf(raw []byte) (*kindReach, error) {
	if raw == nil {
		return nil, nil
	}
	var reach kindReach
	if err := json.Unmarshal(raw, &reach); err != nil {
		return nil, fmt.Errorf("decode request_secrets.airlock_kind: %w", err)
	}
	return &reach, nil
}

// kindLost is each host and header in recorded that the kind name is behind in rules does not
// hold now, compared after ASCII case folding, as a refusal words them, in the recorded order:
// none for a kind that holds every one, whatever it gained, and none for a name that recorded no
// kind. A name behind no kind now has lost them all, though its fingerprint, which holds the kind
// it is behind, has ended its grant before a release reads this.
func kindLost(rules proxyrules.Rules, name string, recorded *kindReach) []string {
	if recorded == nil {
		return nil
	}
	var hosts, headers []string
	if kind := boundKind(rules, name); kind != nil {
		hosts, headers = kind.Hosts, kind.HeaderNames()
	}
	var lost []string
	for _, host := range recorded.Hosts {
		if !holdsFolded(hosts, host) {
			lost = append(lost, "the host "+host)
		}
	}
	for _, header := range recorded.Headers {
		if !holdsFolded(headers, header) {
			lost = append(lost, "the header "+header)
		}
	}
	return lost
}

// holdsFolded reports whether list holds word, compared after ASCII case folding.
func holdsFolded(list []string, word string) bool {
	folded := fold.ASCII(word)
	return slices.ContainsFunc(list, func(held string) bool { return fold.ASCII(held) == folded })
}

// kindNarrowed is the refusal of a granted name whose kind lost hosts or headers the grant was
// decided under (kindLost), which ends the grant for good, as a moved fingerprint does: at its
// release (checkRelease) and at Create's reuse of it (reuseLiveGrant). The kind giving the host or
// header back does not bring the grant back; the session asks again.
func kindNarrowed(name string, lost []string) error {
	return fmt.Errorf("%w: the kind %s is behind no longer holds %s, which this grant was decided under", ErrGrantNotLive, name, strings.Join(lost, " or "))
}
