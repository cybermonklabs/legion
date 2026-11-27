package policy_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// mintedBlock is a proxy block of kind dpop: the capability the broker mints and holds behind it
// (requests.MintedKind), bound to one host, carried in Authorization alone, with an allow list.
const mintedBlock = `"proxy": {"hosts": ["dispatch.example.com"], "headers": ["Authorization"], "kind": "dpop",
  "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}`

// TestADPoPProfileIsANameAndAProxyBlockAlone: a profile whose proxy block is of kind dpop names a
// capability the broker mints from no stored secret, so a field that scopes or limits a stored
// secret's grant - source, tier, requesters, maxGrant, a github or a google block, an allow of its
// own - is refused, naming that field, since it would read as a limit that holds nothing; a name
// and the dpop block alone parse, and so does a profile behind a kind beside such fields.
func TestADPoPProfileIsANameAndAProxyBlockAlone(t *testing.T) {
	for field, profile := range map[string]string{
		"source":     `{"name": "TEST_MINTED_TOKEN", "source": "LEGION_IMPLEMENT_APP", ` + mintedBlock + `}`,
		"tier":       `{"name": "TEST_MINTED_TOKEN", "tier": "agent", ` + mintedBlock + `}`,
		"requesters": `{"name": "TEST_MINTED_TOKEN", "requesters": ["ada@example.com"], ` + mintedBlock + `}`,
		"maxGrant":   `{"name": "TEST_MINTED_TOKEN", "maxGrant": "15m", ` + mintedBlock + `}`,
		"github":     `{"name": "TEST_MINTED_TOKEN", ` + acmeRead + `, ` + mintedBlock + `}`,
		"google":     `{"name": "TEST_MINTED_TOKEN", "google": {"scopes": ["cloud-platform"]}, ` + mintedBlock + `}`,
		"allow":      `{"name": "TEST_MINTED_TOKEN", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}], ` + mintedBlock + `}`,
	} {
		t.Run(field, func(t *testing.T) {
			_, err := policy.ParseProfiles([]byte(policytest.ProfilesFile(profile)))
			if err == nil || !strings.Contains(err.Error(), `"TEST_MINTED_TOKEN": `+field+" ") || !strings.Contains(err.Error(), "dpop") {
				t.Fatalf("ParseProfiles of a dpop profile with %s = %v; want a refusal naming the profile, the field and the kind", field, err)
			}
		})
	}
	if _, err := policy.ParseProfiles([]byte(policytest.ProfilesFile(`{"name": "TEST_MINTED_TOKEN", ` + mintedBlock + `}`))); err != nil {
		t.Fatalf("ParseProfiles of a name and a dpop block = %v; want it parsed", err)
	}
	if _, err := policy.ParseProfiles([]byte(policytest.KindsFile(readProfile))); err != nil {
		t.Fatalf("ParseProfiles of a profile behind a kind with a source and a github block = %v; want it parsed", err)
	}
}

// TestADPoPProfileIsNeverServedAsASecret: a dpop profile is proxy configuration for a capability
// the broker mints and holds, so the policy serves it as no secret whatever the store holds - with
// no stored secret of its name and with one - and it can be neither requested nor approved. It is
// no refusal either: a set whose profiles hold only a dpop block refuses nothing, so the block
// writes no refused line and counts in no broker_unservable_secrets series, which reads Refused.
// Beside another profile, that profile is still served.
func TestADPoPProfileIsNeverServedAsASecret(t *testing.T) {
	dpop := `{"name": "TEST_MINTED_TOKEN", ` + mintedBlock + `}`
	for name, stored := range map[string][]secrets.LocalSecret{
		"no stored secret of its name": nil,
		"a stored secret of its name":  {policytest.Secret("TEST_MINTED_TOKEN", appOwner, policy.TierAgent, "stored-v1")},
	} {
		t.Run(name, func(t *testing.T) {
			cur, _ := profiled(t, stored, dpop)
			set := cur.Get()
			if secret, served := set.Secrets["TEST_MINTED_TOKEN"]; served {
				t.Fatalf("TEST_MINTED_TOKEN is served (%+v); want it served as nothing", secret)
			}
			if len(set.Refused) != 0 {
				t.Fatalf("a set holding only a dpop block refuses %v; want nothing", set.Refused)
			}
		})
	}
	cur, _ := profiled(t, everySource(), readProfile, dpop)
	servedProfile(t, cur.Get(), "GITHUB_READ")
}
