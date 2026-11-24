package policy_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// The profiles of a file written before the kinds, one proxy block of each kind a block took then
// and one of a GitHub App's profile, each a profile of the name its tests serve.
const (
	// legacyPAT is a stored token's profile of its own name, behind a block of no kind.
	legacyPAT = `{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com", "uploads.github.com"], "headers": ["Authorization"]}}`
	// legacyRows is a stored token's profile of its own name, behind a block of no kind with an
	// allow and no headers, which is the default header.
	legacyRows = `{"name": "ADMIN_ROWS", "proxy": {"hosts": ["api.example.com"], "allow": [{"method": "GET", "pathPrefix": "/v1/"}]}}`
	// legacyAWS is a stored AWS key pair's profile of its own name, behind a block of kind aws.
	legacyAWS = `{"name": "AIRLOCK_AWS_PROBE", "proxy": {"hosts": ["sts.amazonaws.com"], "kind": "aws"}}`
	// legacyAzure is an azure-login secret's profile of its own name, behind a block of kind azure.
	legacyAzure = `{"name": "AZURE_ADMIN", "proxy": {"hosts": ["login.microsoftonline.com", "management.azure.com"], "headers": ["Authorization"], "kind": "azure"}}`
	// legacyApp is a GitHub App's bare-token profile, behind a block of no kind.
	legacyApp = `{"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP",
  "github": {"accounts": ["acme"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"},
  "proxy": {"hosts": ["api.github.com"], "headers": ["Authorization"]}}`
)

// legacySources is the store behind the legacy profiles: each profile's source, none with a proxy
// tag, as no secret carried one before the kinds, and UNPROXIED_TOKEN, a stored token no profile
// names.
func legacySources() []memory.Secret {
	return []memory.Secret{
		policytest.Secret("GITHUB_ADMIN_PAT", appOwner, policy.TierAgent, "pat-v1"),
		policytest.Secret("ADMIN_ROWS", appOwner, policy.TierAgent, "rows-v1"),
		policytest.Secret("AIRLOCK_AWS_PROBE", appOwner, policy.TierAgent, "aws-pair-v1"),
		policytest.TypeSecret("AZURE_ADMIN", appOwner, policy.TierAgent, policy.TypeAzureLogin, policytest.TypeValue(policy.TypeAzureLogin)),
		appSource(),
		policytest.Secret("UNPROXIED_TOKEN", appOwner, policy.TierAgent, "plain-v1"),
	}
}

// TestAFileOfNoKindsServesItsProxyBlocksAsBefore: a profiles file that declares no kinds, as every
// deployment's file did before the kinds, loads, and each proxy block of kind "", aws or azure is
// served as the broker before the kinds served it: its name is behind the proxy, bound to the
// block's hosts, headers and allow and nothing else, in the way its source's type takes (a stored
// value swaps, a GitHub App's token is minted and swapped, a stored key pair under aws signs, an
// azure-login exchanges), and a request is decided by the block's allow. The file still declares
// no kind (Kinds), so no proxy tag names the kind a block is served behind: a secret tagged with
// one is refused as naming no kind of the file, as under a file of no blocks. A stored token no
// block names is behind nothing.
func TestAFileOfNoKindsServesItsProxyBlocksAsBefore(t *testing.T) {
	file := policytest.ProfilesFile(legacyPAT, legacyRows, legacyAWS, legacyAzure, legacyApp)
	if kinds := policytest.Profiles(t, file, daemonService).Kinds(); len(kinds) != 0 {
		t.Errorf("Kinds() = %+v; a file that declares no kind declares none", kinds)
	}
	set := policytest.ProfiledCurrent(t, memory.New(legacySources()...), file, daemonService).Get()
	rules := set.ProxyRules()
	if err := proxyrules.Validate(rules); err != nil {
		t.Fatalf("the set's rules are not valid: %v", err)
	}
	for _, c := range []struct {
		name, way      string
		hosts, headers []string
		allow          []proxyrules.Allow
	}{
		{"GITHUB_ADMIN_PAT", proxyrules.WaySwap, []string{"api.github.com", "uploads.github.com"}, []string{"Authorization"}, nil},
		{"ADMIN_ROWS", proxyrules.WaySwap, []string{"api.example.com"}, []string{"authorization"}, []proxyrules.Allow{{Method: "GET", PathPrefix: "/v1/"}}},
		{"AIRLOCK_AWS_PROBE", proxyrules.WaySign, []string{"sts.amazonaws.com"}, []string{"authorization"}, nil},
		{"AZURE_ADMIN", proxyrules.WayExchange, []string{"login.microsoftonline.com", "management.azure.com"}, []string{"Authorization"}, nil},
		{"GITHUB_READ", proxyrules.WayMint, []string{"api.github.com"}, []string{"Authorization"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			secret := servedProfile(t, set, c.name)
			binding := rules.Lookup(c.name)
			kind := rules.KindOf(binding)
			if secret.Proxy == "" || binding == nil || kind == nil || binding.Kind != secret.Proxy || binding.Inline != nil {
				t.Fatalf("%s = %+v, bound %+v under %+v; want it behind a kind of the rules, its binding naming it", c.name, secret, binding, kind)
			}
			if secret.Way != c.way || binding.Way != c.way {
				t.Errorf("%s's way = %q, bound %q; want %q", c.name, secret.Way, binding.Way, c.way)
			}
			if !reflect.DeepEqual(kind.Hosts, c.hosts) || !reflect.DeepEqual(kind.HeaderNames(), c.headers) || !reflect.DeepEqual(kind.Allow, c.allow) || binding.Allow != nil {
				t.Errorf("%s is bound to hosts %q, headers %q, allow %+v and its own allow %+v; want its block's %q, %q, %+v and none of its own", c.name, kind.Hosts, kind.HeaderNames(), kind.Allow, binding.Allow, c.hosts, c.headers, c.allow)
			}
			if secret.ProfileDigest == "" {
				t.Errorf("%s has no fingerprint; a name behind the proxy is fingerprinted with its kind", c.name)
			}
		})
	}
	t.Run("the decision on ADMIN_ROWS is its block's allow", func(t *testing.T) {
		for _, c := range []struct {
			host, method, path string
			allowed            bool
		}{
			{"api.example.com", "GET", "/v1/things", true},
			{"api.example.com", "POST", "/v1/things", false},
			{"api.example.com", "GET", "/v2/things", false},
			{"api.github.com", "GET", "/v1/things", false},
		} {
			request := &proxyrules.Request{ConnectHost: c.host, ConnectPort: 443, SNI: c.host, HostHeader: c.host,
				Method: c.method, Path: c.path, RawPath: c.path, Headers: [][2]string{{"Authorization", "Bearer p"}}}
			if ok, why := proxyrules.Decide(rules, proxyrules.Grant{Secret: "ADMIN_ROWS", Placeholder: "p"}, "rows-v1", request); ok != c.allowed {
				t.Errorf("the decision on %s %s%s = %t (%q); want %t", c.method, c.host, c.path, ok, why, c.allowed)
			}
		}
	})
	t.Run("a stored token no block names", func(t *testing.T) {
		if secret := servedSecret(t, set, "UNPROXIED_TOKEN"); secret.Proxy != "" || rules.Lookup("UNPROXIED_TOKEN") != nil {
			t.Errorf("UNPROXIED_TOKEN = %+v, bound %+v; want it behind nothing", secret, rules.Lookup("UNPROXIED_TOKEN"))
		}
	})
	t.Run("a proxy tag naming the kind a block is served behind", func(t *testing.T) {
		kind := set.Secrets["GITHUB_ADMIN_PAT"].Proxy
		tagged := policytest.Proxied(policytest.Secret("OTHER_TOKEN", appOwner, policy.TierAgent, "other-v1"), kind)
		tagSet := policytest.ProfiledCurrent(t, memory.New(append(legacySources(), tagged)...), file, daemonService).Get()
		if refusal := refusedSecret(t, tagSet, "OTHER_TOKEN", policy.ReasonProxyKindUnknown); !strings.Contains(refusal.Detail, kind) {
			t.Errorf("the refusal's detail %q does not name the tag's value %q", refusal.Detail, kind)
		}
		servedProfile(t, tagSet, "GITHUB_ADMIN_PAT")
	})
}

// TestAProxyBlockOfAFileOfNoKindsIsFingerprintedWhole: a name behind a proxy block of a file that
// declares no kinds is served with a fingerprint (Secret.ProfileDigest) that holds every field of
// its block, as the broker before the kinds fingerprinted it, so each change of what a grant of it
// may do ends its live grants: its kind, which is the way its credential goes into a call (none
// swaps a stored value in, aws signs with it); a host added or taken away; a header added or taken
// away; a row of its allow added, or its allow taken away; and its source's judge tag. The control
// is the same block written in another order and spacing, which moves nothing.
func TestAProxyBlockOfAFileOfNoKindsIsFingerprintedWhole(t *testing.T) {
	const (
		hosts   = `"hosts": ["api.example.com", "uploads.example.com"]`
		headers = `"headers": ["Authorization"]`
		allow   = `"allow": [{"method": "GET", "pathPrefix": "/v1/"}]`
	)
	token := func() memory.Secret {
		return policytest.Secret("BLOCK_TOKEN", appOwner, policy.TierAgent, "block-v1")
	}
	served := func(t *testing.T, block string, source memory.Secret) string {
		t.Helper()
		file := policytest.ProfilesFile(`{"name": "BLOCK_TOKEN", "proxy": {` + block + `}}`)
		return servedProfile(t, policytest.ProfiledCurrent(t, memory.New(source), file, daemonService).Get(), "BLOCK_TOKEN").ProfileDigest
	}
	was := served(t, hosts+", "+headers+", "+allow, token())
	for _, c := range []struct {
		name, block string
		source      memory.Secret
	}{
		{"its kind, none to aws: the stored value is signed with, not swapped in", hosts + ", " + headers + ", " + allow + `, "kind": "aws"`, token()},
		{"a host added", `"hosts": ["api.example.com", "uploads.example.com", "files.example.com"], ` + headers + ", " + allow, token()},
		{"a host taken away", `"hosts": ["api.example.com"], ` + headers + ", " + allow, token()},
		{"a header added", hosts + `, "headers": ["Authorization", "X-Api-Key"], ` + allow, token()},
		{"its headers taken away", hosts + `, "headers": [], ` + allow, token()},
		{"a row added to its allow", hosts + ", " + headers + `, "allow": [{"method": "GET", "pathPrefix": "/v1/"}, {"method": "POST", "pathPrefix": "/v1/"}]`, token()},
		{"its allow taken away", hosts + ", " + headers, token()},
		{"its source's judge tag", hosts + ", " + headers + ", " + allow, policytest.Judged(token(), 300, "")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if is := served(t, c.block, c.source); is == was {
				t.Errorf("BLOCK_TOKEN's fingerprint is %s before and after %s; want it moved, so the change ends its live grants", was, c.name)
			}
		})
	}
	t.Run("the control: the block in another order and spacing", func(t *testing.T) {
		if is := served(t, allow+",\n  "+headers+",  "+hosts, token()); is != was {
			t.Errorf("BLOCK_TOKEN's fingerprint moved from %s to %s for the same block written another way", was, is)
		}
	})
}

// TestAProxyBlocksDigestIsTheOneItHadBeforeTheKinds pins the digest of four proxy-block profiles of
// a file that declares no kinds to the values the broker before the kinds (the parent of #5493's
// merge, 3f468fe8^) computed for the same entries, printed by its Profiles.Digest: a block of no
// kind with an allow, one of kind aws, one of kind azure, and a GitHub App's profile behind a block.
// Each request records the digest of the profile it was decided under, and a release ends a grant
// whose profile's digest moved, so a broker of this release reading the same file ends none of the
// grants the broker before it made; and since that digest holds the whole block, a change of any
// of its fields ends them (TestAProxyBlockOfAFileOfNoKindsIsFingerprintedWhole).
func TestAProxyBlocksDigestIsTheOneItHadBeforeTheKinds(t *testing.T) {
	profiles, err := policy.ParseProfiles([]byte(policytest.ProfilesFile(
		`{"name": "BLOCK_TOKEN", "proxy": {"hosts": ["api.example.com", "uploads.example.com"], "headers": ["Authorization"], "allow": [{"method": "GET", "pathPrefix": "/v1/"}]}}`,
		legacyAWS, legacyAzure, legacyApp)))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"BLOCK_TOKEN":       "8645dff0bf8bdc288fa660f730202fd0a35f4c1231f41910eac0d253b4ca855e",
		"AIRLOCK_AWS_PROBE": "19b2878e60c26ec0ac8ca9340fcf7c6fd94a33fec77f79f3de88af3aba2f4623",
		"AZURE_ADMIN":       "38dea9c1e35fbbc92114e98c146dfdbc783ff1a3a9e301dd7f8edf1b63076464",
		"GITHUB_READ":       "f65349449210f1011d31b6cafc703e6723bb7f0c5a5f81796520291359c7436f",
	} {
		if got := profiles.Digest(name); got != want {
			t.Errorf("Digest(%s) = %s, want %s, the digest the broker before the kinds gave the same entry", name, got, want)
		}
	}
}

// TestAFileOfNoKindsHoldsItsProxyBlocksToTheirRules: a proxy block of a file that declares no
// kinds is held to what the broker before the kinds held it to, each refusal naming the profile
// by its index and name and the field: at least one host, each in a rules host's grammar; a kind
// of "", aws or azure, or dpop; and an allow row that names no host, a field such a block never
// had. A judge or guidance in the block is refused, naming the source's tags: a key's judge
// seconds and guidance are its own tags now, and a block's would decide nothing. The control is
// the rows' profiles as their constants write them, one file, served.
func TestAFileOfNoKindsHoldsItsProxyBlocksToTheirRules(t *testing.T) {
	for _, c := range []struct {
		name, profile, field string
		want                 []string
	}{
		{"a block with no hosts", `{"name": "GITHUB_ADMIN_PAT", "proxy": {"headers": ["Authorization"]}}`, "proxy.hosts", []string{"required"}},
		{"a host with a scheme", `{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["https://api.github.com"]}}`, "proxy.hosts[0]", []string{"holds a scheme"}},
		{"a kind a block never took", `{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "kind": "gcp"}}`, "proxy.kind", []string{`"gcp"`, `"", aws, azure or dpop`}},
		{"an allow row that names a host", `{"name": "OTHER_ROWS", "proxy": {"hosts": ["api.example.com"], "allow": [{"method": "GET", "pathPrefix": "/", "host": "api.example.com"}]}}`, "proxy", []string{"names the host"}},
		{"a judge", `{"name": "GITHUB_ADMIN_READ", "source": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "judge": 300}}`, "proxy.judge", []string{"GITHUB_ADMIN_PAT", policy.TagJudge + " tag"}},
		{"guidance", `{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "guidance": "grant it for the release job"}}`, "proxy.guidance", []string{"GITHUB_ADMIN_PAT", policy.TagGuidance + " tag"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := policy.ParseProfiles([]byte(policytest.ProfilesFile(legacyRows, c.profile)))
			if err == nil {
				t.Fatalf("ParseProfiles served a file of no kinds with %s", c.name)
			}
			for _, want := range append([]string{"profiles[1]", c.field}, c.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
		})
	}
	t.Run("the control: each block as its constant writes it", func(t *testing.T) {
		if _, err := policy.ParseProfiles([]byte(policytest.ProfilesFile(legacyRows, legacyPAT, legacyAWS, legacyAzure, legacyApp))); err != nil {
			t.Fatalf("ParseProfiles of a file of valid blocks: %v", err)
		}
	})
}

// TestEachBlockOfAFileOfNoKindsIsBehindAKindOfItsOwn: the kind a block is served behind is named
// for its profile, one for each name, so two profiles whose names a reading that dropped
// underscores, digits or letters' cases would run together (AB and A_B, TOKEN_2 and TOKENQC, AX_B
// and A_XB, X1 and X_1) are each behind a kind of their own, holding their own block's host,
// never the other's. The rules are valid: each name is one lowercase word, held once.
func TestEachBlockOfAFileOfNoKindsIsBehindAKindOfItsOwn(t *testing.T) {
	names := []string{"AB", "A_B", "TOKEN_2", "TOKENQC", "AX_B", "A_XB", "X1", "X_1"}
	entries := make([]string, len(names))
	for i, name := range names {
		entries[i] = `{"name": "` + name + `", "proxy": {"hosts": ["h` + strconv.Itoa(i) + `.example.com"]}}`
	}
	profiles, err := policy.ParseProfiles([]byte(policytest.ProfilesFile(entries...)))
	if err != nil {
		t.Fatalf("ParseProfiles: %v", err)
	}
	rules := profiles.FileRules()
	if err := proxyrules.Validate(rules); err != nil {
		t.Fatalf("the rules of a file of blocks are not valid: %v", err)
	}
	held := map[string]string{}
	for i, name := range names {
		kind := policy.ProfileKind(profiles.Lookup(name), "")
		if other, taken := held[kind]; taken {
			t.Errorf("%s and %s are both behind the kind %q", other, name, kind)
		}
		held[kind] = name
		if k := rules.Kind(kind); k == nil || !reflect.DeepEqual(k.Hosts, []string{"h" + strconv.Itoa(i) + ".example.com"}) {
			t.Errorf("%s is behind %q, of hosts %+v; want its own block's", name, kind, k)
		}
	}
}

// TestAFileOfKindsServesItsProfilesByTheKinds: a file that declares kinds is read by its kinds
// alone. A profile whose proxy string names a kind is behind that kind of the file, in its
// source's way, and the rules hold the file's kinds and no other.
func TestAFileOfKindsServesItsProfilesByTheKinds(t *testing.T) {
	file := policytest.KindsFile(`{"name": "GITHUB_ADMIN_PAT", "proxy": "github"}`, readProfile)
	declared := policytest.Profiles(t, file, daemonService).Kinds()
	if len(declared) != 6 {
		t.Fatalf("Kinds() = %+v; want the file's 6", declared)
	}
	set := policytest.ProfiledCurrent(t, memory.New(policytest.Secret("GITHUB_ADMIN_PAT", appOwner, policy.TierAgent, "pat-v1"), appSource()), file, daemonService).Get()
	rules := set.ProxyRules()
	if !reflect.DeepEqual(rules.Kinds, declared) {
		t.Errorf("the rules' kinds = %+v; want the file's %+v", rules.Kinds, declared)
	}
	for name, way := range map[string]string{"GITHUB_ADMIN_PAT": proxyrules.WaySwap, "GITHUB_READ": proxyrules.WayMint} {
		secret := servedProfile(t, set, name)
		want := proxyrules.Binding{Name: name, Kind: "github", Way: way}
		if binding := rules.Lookup(name); secret.Proxy != "github" || binding == nil || !reflect.DeepEqual(*binding, want) {
			t.Errorf("%s = %+v, bound %+v; want it behind github, bound %+v", name, secret, binding, want)
		}
	}
}

// TestAFileOfKindsNeverReadsAProxyBlockTheOldWay: in a file that declares kinds, a proxy block of
// kind "", aws or azure is the form the kinds replaced, and the reading a file of no kinds gets is
// never taken for it. ParseProfiles refuses the file, naming kinds; DecodeProfiles, which `profiles
// check --against` reads the file in force with, answers the block by LegacyProxy alone and serves
// the profile behind no kind, its rules the file's kinds and no other. The control is each block in
// a file that declares no kinds, which both readers serve behind a kind of its own: a reading that
// took the old way for a file of kinds fails here, rather than passing because it never serves a
// block at all.
func TestAFileOfKindsNeverReadsAProxyBlockTheOldWay(t *testing.T) {
	for _, c := range []struct{ name, profile string }{
		{"GITHUB_ADMIN_PAT", legacyPAT},
		{"AIRLOCK_AWS_PROBE", legacyAWS},
		{"AZURE_ADMIN", legacyAzure},
	} {
		t.Run(c.name, func(t *testing.T) {
			withKinds := policytest.KindsFile(readProfile, c.profile)
			if _, err := policy.ParseProfiles([]byte(withKinds)); err == nil || !strings.Contains(err.Error(), "a proxy block is no longer read") || !strings.Contains(err.Error(), "declare the kind under kinds") {
				t.Errorf("ParseProfiles of a file of kinds with %s's proxy block = %v; want the refusal naming kinds", c.name, err)
			}
			decoded, err := policy.DecodeProfiles([]byte(withKinds))
			if err != nil {
				t.Fatalf("DecodeProfiles: %v", err)
			}
			rules := decoded.FileRules()
			if decoded.LegacyProxy(c.name) == nil || policy.ProfileKind(decoded.Lookup(c.name), "") != "" || rules.Lookup(c.name) != nil {
				t.Errorf("DecodeProfiles serves %s behind %q, bound %+v, its block %+v; want its block answered and the profile behind no kind", c.name, policy.ProfileKind(decoded.Lookup(c.name), ""), rules.Lookup(c.name), decoded.LegacyProxy(c.name))
			}
			if len(rules.Kinds) != 6 || !reflect.DeepEqual(rules.Kinds, decoded.Kinds()) {
				t.Errorf("DecodeProfiles' rules hold kinds %+v; want the file's 6 and no other", rules.Kinds)
			}
			t.Run("the control: the block in a file of no kinds", func(t *testing.T) {
				for reader, read := range map[string]func([]byte) (policy.Profiles, error){"ParseProfiles": policy.ParseProfiles, "DecodeProfiles": policy.DecodeProfiles} {
					profiles, err := read([]byte(policytest.ProfilesFile(c.profile)))
					if err != nil {
						t.Fatalf("%s of a file of no kinds: %v", reader, err)
					}
					if kind := policy.ProfileKind(profiles.Lookup(c.name), ""); kind == "" || profiles.FileRules().Kind(kind) == nil {
						t.Errorf("%s serves %s behind %q, of rules %+v; want a kind of its own", reader, c.name, kind, profiles.FileRules())
					}
				}
			})
		})
	}
}
