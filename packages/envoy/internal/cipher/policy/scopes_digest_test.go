package policy_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestAGoogleServiceAccountsScopesAreInItsFingerprint: the scopes tag of a google-service-account
// secret is what its minted token may do, so a name that mints under that tag is served with a
// fingerprint (Secret.ProfileDigest) that moves when the tag does, and a release ends each grant
// decided under the old tag. That is the secret by its own name behind no proxy, by its own name
// behind a kind, and a profile of it with no google block of its own, which inherits the tag; each
// moves between two scopes, and between no scopes tag, which mints at cloud-platform, and one. The
// control is a profile with a google block, whose scopes are its entry's and in its fingerprint
// already: its source's tag changing around them moves nothing.
func TestAGoogleServiceAccountsScopesAreInItsFingerprint(t *testing.T) {
	source := func(scopes string, kind string) secrets.LocalSecret {
		s := policytest.TypeSecret("GCP_FLEET", appOwner, policy.TierAgent, policy.TypeGoogleServiceAccount, policytest.TypeValue(policy.TypeGoogleServiceAccount))
		if scopes != "" {
			s.Tags[policy.TagScopes] = scopes
		}
		if kind != "" {
			s = policytest.Proxied(s, kind)
		}
		return s
	}
	served := func(t *testing.T, name string, s secrets.LocalSecret, profiles ...string) string {
		t.Helper()
		set := policytest.ProfiledCurrent(t, secrets.NewLocal(s), policytest.KindsFile(profiles...), daemonService).Get()
		secret, ok := set.Secrets[name]
		if !ok {
			t.Fatalf("%s is not served: %+v", name, set.Refused)
		}
		return secret.ProfileDigest
	}
	for _, c := range []struct {
		name, served, kind, was, is string
		profiles                    []string
		moves                       bool
	}{
		{"by its own name behind no proxy", "GCP_FLEET", "", "devstorage.read_only", "bigquery cloud-platform", nil, true},
		{"by its own name behind no proxy, from no scopes tag", "GCP_FLEET", "", "", "devstorage.read_only", nil, true},
		{"by its own name behind no proxy, to no scopes tag", "GCP_FLEET", "", "devstorage.read_only", "", nil, true},
		{"by its own name behind a kind", "GCP_FLEET", "gcp", "devstorage.read_only", "bigquery cloud-platform", nil, true},
		{"a profile with no google block", "GCP_FLEET_READ", "", "devstorage.read_only", "bigquery cloud-platform", []string{`{"name": "GCP_FLEET_READ", "source": "GCP_FLEET"}`}, true},
		{"a profile with no google block behind its source's kind", "GCP_FLEET_READ", "gcp", "devstorage.read_only", "bigquery cloud-platform", []string{`{"name": "GCP_FLEET_READ", "source": "GCP_FLEET"}`}, true},
		{"the control: a profile with a google block", "GCP_FLEET_BIGQUERY", "", "bigquery devstorage.read_only", "bigquery cloud-platform", []string{`{"name": "GCP_FLEET_BIGQUERY", "source": "GCP_FLEET", "google": {"scopes": ["bigquery"]}}`}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			was, is := served(t, c.served, source(c.was, c.kind), c.profiles...), served(t, c.served, source(c.is, c.kind), c.profiles...)
			if moved := was != is; moved != c.moves {
				t.Errorf("%s's fingerprint is %q under scopes %q and %q under %q; want it moved = %t", c.served, was, c.was, is, c.is, c.moves)
			}
		})
	}
}
