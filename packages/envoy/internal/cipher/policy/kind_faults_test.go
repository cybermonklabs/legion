package policy_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// faultKinds are the kinds of the fault tests: tight, which mints alone, caps a grant at an hour
// and allows GET under /a/ on api.example.com; and loose, which swaps on the same host, with
// neither.
const faultKinds = `[` +
	`{"name": "tight", "hosts": ["api.example.com"], "ways": ["mint"], "max_grant": "1h", "allow": [{"method": "GET", "pathPrefix": "/a/"}]}, ` +
	`{"name": "loose", "hosts": ["api.example.com"], "ways": ["swap"]}]`

// faultyProfile is a profile of the stored value PLAIN_KEY behind tight by its proxy string, which
// fails every rule a kind holds a profile to that its file decides and its source's tags decide:
// a stored value mints by no way of tight's, its own maxGrant is above tight's, it has an allow
// and its source no proxy-narrowed tag, one row names a host tight does not bind, and two rows are
// outside tight's allow.
const faultyProfile = `{"name": "PLAIN_READ", "source": "PLAIN_KEY", "proxy": "tight", "maxGrant": "2h", "allow": [` +
	`{"method": "GET", "pathPrefix": "/a/b"}, {"method": "GET", "pathPrefix": "/b/", "host": "other.example.com"}, {"method": "POST", "pathPrefix": "/a/"}]}`

// TestProxyFaultsAreEveryFaultInTheLoadsOrder: ProxyFaults answers every fault of a profile at the
// proxy, each in the load's words, in the order the load weighs them, where the load refuses for
// the first alone: so the load's refusal of the profile is the first fault, reason and detail, and
// `profiles check` can print the rest. Its source's tags not read (nil), only the faults the file
// decides remain: its own maxGrant over the kind's, the row's host and the rows outside the kind's
// allow. A proxy string that names another kind than the source's tag is a fault before the rest,
// which are weighed under the tag's kind. A profile with no proxy string and tags not read is held
// to no kind and has no fault; with them read, a stored value's profile behind no kind has one.
func TestProxyFaultsAreEveryFaultInTheLoadsOrder(t *testing.T) {
	profiles := policytest.Profiles(t, policytest.KindsFileOf(faultKinds, faultyProfile,
		`{"name": "BARE_READ", "source": "PLAIN_KEY"}`), daemonService)
	reasons := func(faults []policy.ProfileFault) []string {
		var out []string
		for _, f := range faults {
			if f.Detail == "" {
				t.Errorf("the fault %s has no detail", f.Reason)
			}
			out = append(out, f.Reason)
		}
		return out
	}
	plain := policy.Secret{Name: "PLAIN_KEY", Source: "arn:plain-key"}
	faults := profiles.ProxyFaults(profiles.Lookup("PLAIN_READ"), &plain)
	want := []string{policy.ReasonProxyWay, policy.ReasonProxyMaxGrant, policy.ReasonProfileAllowNotNarrowed, policy.ReasonProfileAllowHost, policy.ReasonProfileAllowOutsideKind}
	if got := reasons(faults); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProxyFaults(PLAIN_READ) = %v, want %v", got, want)
	}
	if want := "allow rows GET /b/ on other.example.com and POST /a/ are outside the kind tight's allow, and a profile narrows its kind and never widens it"; faults[4].Detail != want {
		t.Errorf("the outside fault says %q, want %q", faults[4].Detail, want)
	}

	set := policytest.ProfiledCurrent(t, secrets.NewLocal(policytest.Secret("PLAIN_KEY", appOwner, policy.TierAgent, "plain-v1")),
		policytest.KindsFileOf(faultKinds, faultyProfile), daemonService).Get()
	if refusal := refusedProfile(t, set, "PLAIN_READ", faults[0].Reason); refusal.Detail != faults[0].Detail {
		t.Errorf("the load refuses PLAIN_READ for %q, want the first fault's words, %q", refusal.Detail, faults[0].Detail)
	}

	if got, want := reasons(profiles.ProxyFaults(profiles.Lookup("PLAIN_READ"), nil)), []string{policy.ReasonProxyMaxGrant, policy.ReasonProfileAllowHost, policy.ReasonProfileAllowOutsideKind}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProxyFaults(PLAIN_READ) with the tags not read = %v, want the file's own %v", got, want)
	}

	tagged := plain
	tagged.Proxy, tagged.ProxyNarrowed = "loose", true
	if got, want := reasons(profiles.ProxyFaults(profiles.Lookup("PLAIN_READ"), &tagged)), []string{policy.ReasonProxyKindConflict, policy.ReasonProfileAllowHost}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProxyFaults(PLAIN_READ) from a source tagged loose = %v, want %v: the conflict, then loose's own rules, of which its row on other.example.com fails one", got, want)
	}

	if faults := profiles.ProxyFaults(profiles.Lookup("BARE_READ"), nil); faults != nil {
		t.Errorf("ProxyFaults(BARE_READ) with the tags not read = %v, want none: its kind is its source's tag", faults)
	}
	if got, want := reasons(profiles.ProxyFaults(profiles.Lookup("BARE_READ"), &plain)), []string{policy.ReasonProfileSourceNotProxied}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProxyFaults(BARE_READ) of an untagged stored value = %v, want %v", got, want)
	}
	if got, want := policy.ProfileKind(profiles.Lookup("PLAIN_READ"), "loose"), "loose"; got != want {
		t.Errorf("ProfileKind(PLAIN_READ, loose) = %q, want the tag's %q", got, want)
	}
	if got, want := policy.ProfileKind(profiles.Lookup("PLAIN_READ"), ""), "tight"; got != want {
		t.Errorf("ProfileKind(PLAIN_READ, no tag) = %q, want its string's %q", got, want)
	}
	if got := policy.RowWords(proxyrules.Allow{Method: "GET", PathPrefix: "/a/", Host: "api.example.com"}); got != "GET /a/ on api.example.com" {
		t.Errorf("RowWords = %q", got)
	}
}

// TestSecretOfTagsIsTheLoadsReading: SecretOfTags reads a secret's store tags as the load does:
// for a secret the load serves, the same kind, way, proxy-narrowed tag, max-grant, type and
// fingerprint; for one it refuses for a tag SecretOfTags reads, the same reason and detail.
func TestSecretOfTagsIsTheLoadsReading(t *testing.T) {
	file := policytest.KindsFile()
	profiles := policytest.Profiles(t, file, daemonService)
	tag := func(s secrets.LocalSecret, tags map[string]string) secrets.LocalSecret {
		for k, v := range tags {
			s.Tags[k] = v
		}
		return s
	}
	plain := func(name string) secrets.LocalSecret {
		return policytest.Secret(name, appOwner, policy.TierAgent, "v1")
	}
	rows := []secrets.LocalSecret{
		tag(plain("SWAP_KEY"), map[string]string{policy.TagProxy: "slack"}),
		tag(plain("SIGN_KEY"), map[string]string{policy.TagProxy: "aws", policy.TagMaxGrant: "2h"}),
		tag(plain("NARROW_KEY"), map[string]string{policy.TagProxy: "github", policy.TagProxyNarrowed: policy.ProxyNarrowedTrue}),
		tag(policytest.TypeSecret("FED_KEY", appOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation)), map[string]string{policy.TagProxy: "anthropic"}),
		tag(policytest.TypeSecret("LOGIN_KEY", appOwner, policy.TierAgent, policy.TypeGoogleLogin, policytest.TypeValue(policy.TypeGoogleLogin)), map[string]string{policy.TagProxy: "gcp"}),
		plain("BARE_KEY"),
		tag(plain("UNKNOWN_KEY"), map[string]string{policy.TagProxy: "gitlab"}),
		tag(plain("WAY_KEY"), map[string]string{policy.TagProxy: "anthropic", policy.TagMaxGrant: "1h"}),
		tag(policytest.TypeSecret("CAP_KEY", appOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation)), map[string]string{policy.TagProxy: "anthropic", policy.TagMaxGrant: "1h"}),
		tag(plain("NARROWED_BAD_KEY"), map[string]string{policy.TagProxyNarrowed: "yes"}),
		tag(plain("GRANT_BAD_KEY"), map[string]string{policy.TagProxy: "slack", policy.TagMaxGrant: "a day"}),
		tag(plain("TYPE_BAD_KEY"), map[string]string{policy.TagProxy: "slack", policy.TagType: "no-such-type"}),
	}
	// CAP_KEY's cap within the kind's is served; the kind with a cap a stored value under it lacks
	// is the max_grant row of TestEachProxyTagRefusalNamesWhatIsWrong.
	loader := policytest.Loader(secrets.NewLocal(rows...), daemonService)
	loader.Profiles = profiles
	set, err := loader.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		name := policy.SlugToName(row.Name[len(policytest.Prefix):])
		got, reason, detail := profiles.SecretOfTags(secrets.LocalARN(row.Name), row.Tags)
		if refusal, refused := set.Refused[row.Name]; refused {
			if reason != refusal.Reason || detail != refusal.Detail {
				t.Errorf("SecretOfTags(%s) = %q, %q; the load refuses it %q, %q", name, reason, detail, refusal.Reason, refusal.Detail)
			}
			continue
		}
		served, ok := set.Secrets[name]
		if !ok || reason != "" {
			t.Errorf("SecretOfTags(%s) = %q, %q, the load served %t", name, reason, detail, ok)
			continue
		}
		if got.Proxy != served.Proxy || got.Way != served.Way || got.ProxyNarrowed != served.ProxyNarrowed || got.MaxGrant != served.MaxGrant || got.Type() != served.Type() || got.ProfileDigest != served.ProfileDigest || got.Source != served.Source {
			t.Errorf("SecretOfTags(%s) = %+v; the load serves %+v", name, got, served)
		}
	}
	for _, refused := range []string{"UNKNOWN_KEY", "WAY_KEY", "NARROWED_BAD_KEY", "GRANT_BAD_KEY", "TYPE_BAD_KEY"} {
		if _, ok := set.Refused[policytest.ID(refused)]; !ok {
			t.Errorf("the load serves %s, so this test checks no refusal of it", refused)
		}
	}
	if got := set.Secrets["SIGN_KEY"]; got.Way != proxyrules.WaySign || got.MaxGrant != 2*time.Hour {
		t.Errorf("SIGN_KEY = %+v; want it signed with a 2h cap, so the row checks a way and a cap", got)
	}
}

// TestNarrowedFaultsIsTheLoadsRuleForANarrowedSecret: NarrowedFaults words the narrowed rule for one
// narrowed secret as the load refuses it: no profile names it, its one profile has no allow, and
// two profiles name it; and its one profile with an allow serves it, with no fault.
func TestNarrowedFaultsIsTheLoadsRuleForANarrowedSecret(t *testing.T) {
	profiles := policytest.Profiles(t, policytest.KindsFile(
		`{"name": "ONE_READ", "source": "ONE_KEY", "allow": [{"method": "GET", "pathPrefix": "/"}]}`,
		`{"name": "BARE_READ", "source": "BARE_KEY"}`,
		`{"name": "TWO_KEY", "allow": []}`, `{"name": "TWO_READ", "source": "TWO_KEY", "allow": []}`,
	), daemonService)
	fault := func(detail string) policy.ProfileFault {
		return policy.ProfileFault{Reason: policy.ReasonProxyNarrowedProfiles, Detail: detail}
	}
	for _, c := range []struct {
		name     string
		own      policy.ProfileFault
		profiles map[string]policy.ProfileFault
	}{
		{"ONE_KEY", policy.ProfileFault{}, nil},
		{"LONE_KEY", fault("it is tagged proxy-narrowed=true, and no profile names it"), nil},
		{"BARE_KEY", fault("it is tagged proxy-narrowed=true, and its one profile, BARE_READ, has no allow"),
			map[string]policy.ProfileFault{"BARE_READ": fault("its source BARE_KEY is tagged proxy-narrowed=true, and the profile has no allow")}},
		{"TWO_KEY", fault("TWO_KEY is tagged proxy-narrowed=true, and 2 profiles name it: TWO_KEY, TWO_READ"),
			map[string]policy.ProfileFault{"TWO_KEY": fault("TWO_KEY is tagged proxy-narrowed=true, and 2 profiles name it: TWO_KEY, TWO_READ"), "TWO_READ": fault("TWO_KEY is tagged proxy-narrowed=true, and 2 profiles name it: TWO_KEY, TWO_READ")}},
	} {
		own, of := profiles.NarrowedFaults(c.name)
		if own != c.own || !reflect.DeepEqual(of, c.profiles) {
			t.Errorf("NarrowedFaults(%s) = %+v, %+v; want %+v, %+v", c.name, own, of, c.own, c.profiles)
		}
	}
}
