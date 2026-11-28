package policy_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// TestAKindsAllowIsInTheFingerprintOfEachNameBehindIt: a request records the fingerprint of each
// name it was decided under (Secret.ProfileDigest), and a release ends a grant whose name's
// fingerprint moved. A kind's allow bounds every name behind the kind, so each edit of it - a row
// added, which widens it, a row taken out, the allow taken away, which allows every path - moves
// the fingerprint of every name of the kind, and the version: a stored secret behind it by its tag
// alone, whose only allow is the kind's; a profile of a narrowed source with an allow of its own;
// and a profile behind the kind by its proxy string. A name of another kind keeps its fingerprint.
// The control holds the other half of the rule: the same file, the same rows in another order,
// and a kind that only gains a host or a header move nothing, since the release ends a grant whose
// kind lost a host or a header by what the request recorded of them, and a gained one takes
// nothing from a live grant. A present empty allow, which allows nothing, and no allow, which
// allows every path, are two fingerprints.
func TestAKindsAllowIsInTheFingerprintOfEachNameBehindIt(t *testing.T) {
	kinds := func(hosts, headers, allow string) string {
		return `[{"name": "api", "hosts": [` + hosts + `], "headers": [` + headers + `], "ways": ["swap"]` + allow + `}, ` +
			`{"name": "other", "hosts": ["other.example.com"], "ways": ["swap"], "allow": [{"method": "GET", "pathPrefix": "/"}]}]`
	}
	const (
		hosts   = `"api.example.com"`
		headers = `"Authorization"`
		rows    = `, "allow": [{"method": "GET", "pathPrefix": "/v1/a/"}, {"method": "POST", "pathPrefix": "/v1/a/"}]`
	)
	// served is the live set under kinds, failing t unless it serves each of want and OTHER_KEY.
	served := func(t *testing.T, kinds string, want ...string) *policy.Set {
		t.Helper()
		store := memory.New(
			policytest.Proxied(policytest.Secret("TAG_ONLY", appOwner, policy.TierAgent, "tag-v1"), "api"),
			policytest.Narrowed(policytest.Proxied(policytest.Secret("NARROWED_KEY", appOwner, policy.TierAgent, "narrowed-v1"), "api")),
			policytest.Secret("STRING_KEY", appOwner, policy.TierAgent, "string-v1"),
			policytest.Proxied(policytest.Secret("OTHER_KEY", appOwner, policy.TierAgent, "other-v1"), "other"),
		)
		set := policytest.ProfiledCurrent(t, store, policytest.KindsFileOf(kinds,
			`{"name": "NARROWED_READ", "source": "NARROWED_KEY", "allow": [{"method": "GET", "pathPrefix": "/v1/a/"}]}`,
			`{"name": "STRING_READ", "source": "STRING_KEY", "proxy": "api"}`,
		), daemonService).Get()
		for _, name := range append(want, "OTHER_KEY") {
			if _, ok := set.Secrets[name]; !ok {
				t.Fatalf("%s is not served under %s: %+v", name, kinds, set.Refused)
			}
		}
		return set
	}
	names := []string{"TAG_ONLY", "NARROWED_READ", "STRING_READ"}
	base := served(t, kinds(hosts, headers, rows), names...)

	for name, edit := range map[string]struct {
		kinds string
		names []string
	}{
		"a row added, which widens it": {kinds(hosts, headers, `, "allow": [{"method": "GET", "pathPrefix": "/v1/a/"}, {"method": "POST", "pathPrefix": "/v1/a/"}, {"method": "DELETE", "pathPrefix": "/v1/a/"}]`), names},
		"a row's prefix widened":       {kinds(hosts, headers, `, "allow": [{"method": "GET", "pathPrefix": "/v1/"}, {"method": "POST", "pathPrefix": "/v1/a/"}]`), names},
		"a row taken out":              {kinds(hosts, headers, `, "allow": [{"method": "GET", "pathPrefix": "/v1/a/"}]`), names},
		"the allow taken away":         {kinds(hosts, headers, ""), names},
		// NARROWED_READ's row is outside an allow of no row, so the load refuses that profile.
		"an allow of no row": {kinds(hosts, headers, `, "allow": []`), []string{"TAG_ONLY", "STRING_READ"}},
	} {
		t.Run(name, func(t *testing.T) {
			set := served(t, edit.kinds, edit.names...)
			for _, n := range edit.names {
				if was, is := base.Secrets[n].ProfileDigest, set.Secrets[n].ProfileDigest; was == is {
					t.Errorf("%s left %s's fingerprint at %s, so the grants decided under the old allow go on", name, n, is)
				}
			}
			if set.Secrets["OTHER_KEY"].ProfileDigest != base.Secrets["OTHER_KEY"].ProfileDigest {
				t.Errorf("%s moved OTHER_KEY's fingerprint, a name of another kind", name)
			}
			if set.Version == base.Version {
				t.Errorf("%s left the version at %s", name, set.Version)
			}
		})
	}
	none, empty := served(t, kinds(hosts, headers, ""), "TAG_ONLY"), served(t, kinds(hosts, headers, `, "allow": []`), "TAG_ONLY")
	if none.Secrets["TAG_ONLY"].ProfileDigest == empty.Secrets["TAG_ONLY"].ProfileDigest {
		t.Error("no allow, which allows every path, and an allow of no row, which allows none, give TAG_ONLY one fingerprint")
	}

	for name, same := range map[string]string{
		"the same file":             kinds(hosts, headers, rows),
		"the rows reordered":        kinds(hosts, headers, `, "allow": [{"method": "POST", "pathPrefix": "/v1/a/"}, {"method": "GET", "pathPrefix": "/v1/a/"}]`),
		"a host added":              kinds(hosts+`, "www.example.com"`, headers, rows),
		"a header added":            kinds(hosts, headers+`, "X-Api-Key"`, rows),
		"a host and a header added": kinds(hosts+`, "*.example.org"`, headers+`, "X-Api-Key"`, rows),
	} {
		t.Run("the control: "+name, func(t *testing.T) {
			set := served(t, same, names...)
			for _, n := range append(names, "OTHER_KEY") {
				if was, is := base.Secrets[n].ProfileDigest, set.Secrets[n].ProfileDigest; was != is {
					t.Errorf("%s moved %s's fingerprint from %s to %s, which would end its grants", name, n, was, is)
				}
			}
			if set.Version != base.Version {
				t.Errorf("%s moved the version from %s to %s", strings.ToLower(name), base.Version, set.Version)
			}
		})
	}
}
