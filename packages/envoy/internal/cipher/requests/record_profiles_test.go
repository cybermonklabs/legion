package requests

import (
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestARecordsProfilesCarryTheirKindsScopeFromTheSetTheyWereReadIn: what the approval page prints
// of each name behind a kind comes from the one set recordProfiles reads, with the kind's name and
// the fingerprint: a stored secret behind a kind by its own tag carries the kind's hosts and its
// allow rows; a profile whose own allow names a host in every row carries those hosts alone, and
// one whose own allow holds no row carries no host; a profile behind a kind with no allow carries
// that nil allow; and a name behind no kind is left out. Read from another set, in which the
// tag-only secret's tag names another kind, each name carries that set's kind, so nothing of it is
// read from a policy other than the one given.
func TestARecordsProfilesCarryTheirKindsScopeFromTheSetTheyWereReadIn(t *testing.T) {
	const kinds = `[` +
		`{"name": "deel", "hosts": ["api.deel.example"], "ways": ["swap"], "allow": [{"method": "GET", "pathPrefix": "/v1/payroll/"}]}, ` +
		`{"name": "open", "hosts": ["api.open.example", "uploads.open.example"], "ways": ["swap"]}]`
	file := policytest.KindsFileOf(kinds,
		`{"name": "NARROW_READ", "allow": [{"method": "GET", "pathPrefix": "/v1/", "host": "uploads.open.example"}]}`,
		`{"name": "SEALED_READ", "allow": []}`,
		`{"name": "WIDE_READ", "source": "WIDE_TOKEN", "proxy": "open"}`,
	)
	stored := func(deelKind string) *secrets.Local {
		return secrets.NewLocal(
			policytest.Proxied(policytest.Secret("DEEL_TOKEN", fixtureOperator, policy.TierHuman, "deel-v1"), deelKind),
			policytest.Narrowed(policytest.Proxied(policytest.Secret("NARROW_READ", fixtureOperator, policy.TierHuman, "narrow-v1"), "open")),
			policytest.Narrowed(policytest.Proxied(policytest.Secret("SEALED_READ", fixtureOperator, policy.TierHuman, "sealed-v1"), "open")),
			policytest.Secret("WIDE_TOKEN", fixtureOperator, policy.TierHuman, "wide-v1"),
			policytest.Secret("PLAIN_KEY", fixtureOperator, policy.TierHuman, "plain-v1"),
		)
	}
	details := func(names ...string) []record.AuthorizationDetail {
		out := make([]record.AuthorizationDetail, len(names))
		for i, name := range names {
			out[i] = record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"}}
		}
		return out
	}
	deelAllow := []proxyrules.Allow{{Method: "GET", PathPrefix: "/v1/payroll/"}}
	openHosts := []string{"api.open.example", "uploads.open.example"}
	for _, c := range []struct {
		deelKind string
		want     map[string]RecordProfile // by name: Kind, Hosts and Allow; the rest is compared apart
	}{
		{"deel", map[string]RecordProfile{
			"DEEL_TOKEN":  {Kind: "deel", Hosts: []string{"api.deel.example"}, Allow: deelAllow},
			"NARROW_READ": {Kind: "open", Hosts: []string{"uploads.open.example"}},
			"SEALED_READ": {Kind: "open", Hosts: []string{}},
			"WIDE_READ":   {Kind: "open", Hosts: openHosts},
		}},
		{"open", map[string]RecordProfile{
			"DEEL_TOKEN":  {Kind: "open", Hosts: openHosts},
			"NARROW_READ": {Kind: "open", Hosts: []string{"uploads.open.example"}},
			"SEALED_READ": {Kind: "open", Hosts: []string{}},
			"WIDE_READ":   {Kind: "open", Hosts: openHosts},
		}},
	} {
		t.Run("DEEL_TOKEN behind "+c.deelKind, func(t *testing.T) {
			set := policytest.ProfiledCurrent(t, stored(c.deelKind), file).Get()
			got := recordProfiles(set, details("DEEL_TOKEN", "NARROW_READ", "PLAIN_KEY", "SEALED_READ", "WIDE_READ"))
			var names []string
			for _, p := range got {
				names = append(names, p.Name)
				want, listed := c.want[p.Name]
				if !listed {
					t.Errorf("recordProfiles carries %s, which is behind no kind", p.Name)
					continue
				}
				served := set.Secrets[p.Name]
				if p.Kind != want.Kind || !reflect.DeepEqual(p.Hosts, want.Hosts) || !reflect.DeepEqual(p.Allow, want.Allow) {
					t.Errorf("%s = kind %q, hosts %#v, allow %#v; want kind %q, hosts %#v, allow %#v", p.Name, p.Kind, p.Hosts, p.Allow, want.Kind, want.Hosts, want.Allow)
				}
				if p.Digest != served.ProfileDigest || p.Profile != served.Profile {
					t.Errorf("%s carries the fingerprint %q and profile %p; want the set's %q and %p", p.Name, p.Digest, p.Profile, served.ProfileDigest, served.Profile)
				}
			}
			if want := []string{"DEEL_TOKEN", "NARROW_READ", "SEALED_READ", "WIDE_READ"}; !reflect.DeepEqual(names, want) {
				t.Fatalf("recordProfiles carries %v; want %v, in the record's order and without PLAIN_KEY", names, want)
			}
		})
	}
}
