package policy_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
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

	set := policytest.ProfiledCurrent(t, memory.New(policytest.Secret("PLAIN_KEY", appOwner, policy.TierAgent, "plain-v1")),
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

// TestGrantAndTypeReadsTheTagsInTheLoadsOrder: the one reader of a secret's max-grant,
// default-grant and type tags refuses a secret with several of them malformed for the first, in
// the order max-grant, default-grant, type, which is the order the load has refused them in since
// before the kinds; reads the type only once both grant tags pass, so a secret refused for a grant
// tag writes no legacy-tag line; and, for a secret it serves, sets the grant cap, the session
// default and the type frozen into the source, read back by Secret.Type. The load and SecretOfTags
// both refuse in that order, since both read through it.
func TestGrantAndTypeReadsTheTagsInTheLoadsOrder(t *testing.T) {
	const source = "arn:row-key"
	for _, row := range []struct {
		name     string
		tags     map[string]string
		reason   string
		typeRead bool
		want     policy.Secret
	}{
		{"all three malformed", map[string]string{policy.TagMaxGrant: "a day", policy.TagDefaultGrant: "forever", policy.TagType: "no-such-type"}, policy.ReasonMaxGrantMalformed, false, policy.Secret{}},
		{"default-grant and type malformed", map[string]string{policy.TagMaxGrant: "1h", policy.TagDefaultGrant: "forever", policy.TagType: "no-such-type"}, policy.ReasonDefaultGrantMalformed, false, policy.Secret{}},
		{"type malformed", map[string]string{policy.TagMaxGrant: "1h", policy.TagDefaultGrant: policy.DefaultGrantSession, policy.TagType: "no-such-type"}, policy.ReasonTypeUnknown, true, policy.Secret{}},
		{"all three read", map[string]string{policy.TagMaxGrant: "1h", policy.TagDefaultGrant: policy.DefaultGrantSession, policy.TagType: policy.TypeAnthropicFederation}, "", true,
			policy.Secret{Source: policy.TypeSource(policy.TypeAnthropicFederation, source), MaxGrant: time.Hour, SessionDefault: true}},
		{"the old kind tag", map[string]string{policy.LegacyTypeTag: policy.TypeGoogleLogin}, "", true, policy.Secret{Source: policy.TypeSource(policy.TypeGoogleLogin, source)}},
		{"no tag", map[string]string{}, "", true, policy.Secret{Source: source}},
	} {
		t.Run(row.name, func(t *testing.T) {
			s, read := policy.Secret{Source: source}, false
			secretType, reason := policy.GrantAndType(&s, row.tags, func() string { read = true; return policy.TypeFromTags(row.tags) })
			if reason != row.reason || read != row.typeRead {
				t.Fatalf("GrantAndType = %q, read the type %t; want %q, %t", reason, read, row.reason, row.typeRead)
			}
			if reason != "" {
				return
			}
			if s != row.want || secretType != row.want.Type() {
				t.Errorf("GrantAndType read %+v and the type %q; want %+v and %q", s, secretType, row.want, row.want.Type())
			}
		})
	}

	// The load and SecretOfTags refuse in the helper's order: a secret whose default-grant and type
	// are both malformed is refused for its default-grant by both.
	bad := policytest.Secret("BAD_KEY", appOwner, policy.TierAgent, "v1")
	bad.Tags[policy.TagDefaultGrant], bad.Tags[policy.TagType] = "forever", "no-such-type"
	profiles := policytest.Profiles(t, policytest.KindsFile(), daemonService)
	set := policytest.ProfiledCurrent(t, memory.New(bad), policytest.KindsFile(), daemonService).Get()
	refusedSecret(t, set, "BAD_KEY", policy.ReasonDefaultGrantMalformed)
	if _, reason, _ := profiles.SecretOfTags(memory.Source(bad.Name), bad.Tags); reason != policy.ReasonDefaultGrantMalformed {
		t.Errorf("SecretOfTags refuses BAD_KEY for %q; the load refuses it for %q", reason, policy.ReasonDefaultGrantMalformed)
	}
}

// TestSecretOfTagsIsTheLoadsReading: SecretOfTags reads a secret's store tags as the load does,
// row by row over one store: for a secret the load serves, the same kind, way, proxy-narrowed tag,
// max-grant, type, fingerprint and source; for one it refuses for a tag SecretOfTags reads, the
// same reason and detail. The two share grantAndType and proxyTags today; this is the check that
// catches them drifting apart. The rows cover each way (swap, sign, mint), a narrowed secret, two
// typed secrets, a secret behind no kind, a cap within a kind's max_grant, and a refusal for each
// tag SecretOfTags reads: an unknown kind, a way the kind does not hold, a malformed
// proxy-narrowed tag, a malformed max-grant, a malformed default-grant and an unknown type. The
// test fails, rather than passing on nothing, if the load serves or refuses fewer rows than it
// names.
func TestSecretOfTagsIsTheLoadsReading(t *testing.T) {
	profiles := policytest.Profiles(t, policytest.KindsFile(), daemonService)
	tag := func(s memory.Secret, tags map[string]string) memory.Secret {
		for k, v := range tags {
			s.Tags[k] = v
		}
		return s
	}
	plain := func(name string) memory.Secret {
		return policytest.Secret(name, appOwner, policy.TierAgent, "v1")
	}
	federation := func(name string) memory.Secret {
		return policytest.TypeSecret(name, appOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation))
	}
	rows := []memory.Secret{
		tag(plain("SWAP_KEY"), map[string]string{policy.TagProxy: "slack"}),
		tag(plain("SIGN_KEY"), map[string]string{policy.TagProxy: "aws", policy.TagMaxGrant: "2h"}),
		tag(plain("NARROW_KEY"), map[string]string{policy.TagProxy: "github", policy.TagProxyNarrowed: policy.ProxyNarrowedTrue}),
		tag(federation("FED_KEY"), map[string]string{policy.TagProxy: "anthropic", policy.TagMaxGrant: "30m"}),
		tag(policytest.TypeSecret("LOGIN_KEY", appOwner, policy.TierAgent, policy.TypeGoogleLogin, policytest.TypeValue(policy.TypeGoogleLogin)), map[string]string{policy.TagProxy: "gcp"}),
		tag(plain("SESSION_KEY"), map[string]string{policy.TagProxy: "slack", policy.TagDefaultGrant: policy.DefaultGrantSession}),
		plain("BARE_KEY"),
		tag(federation("CAP_KEY"), map[string]string{policy.TagProxy: "anthropic", policy.TagMaxGrant: "1h"}),
		tag(plain("UNKNOWN_KEY"), map[string]string{policy.TagProxy: "gitlab"}),
		tag(plain("WAY_KEY"), map[string]string{policy.TagProxy: "anthropic", policy.TagMaxGrant: "1h"}),
		tag(plain("NARROWED_BAD_KEY"), map[string]string{policy.TagProxyNarrowed: "yes"}),
		tag(plain("GRANT_BAD_KEY"), map[string]string{policy.TagProxy: "slack", policy.TagMaxGrant: "a day"}),
		tag(plain("DEFAULT_BAD_KEY"), map[string]string{policy.TagProxy: "slack", policy.TagDefaultGrant: "forever"}),
		tag(plain("TYPE_BAD_KEY"), map[string]string{policy.TagProxy: "slack", policy.TagType: "no-such-type"}),
	}
	wantServed := []string{"SWAP_KEY", "SIGN_KEY", "NARROW_KEY", "FED_KEY", "LOGIN_KEY", "SESSION_KEY", "BARE_KEY", "CAP_KEY"}
	wantRefused := []string{"UNKNOWN_KEY", "WAY_KEY", "NARROWED_BAD_KEY", "GRANT_BAD_KEY", "DEFAULT_BAD_KEY", "TYPE_BAD_KEY"}
	loader := policytest.Loader(memory.New(rows...), daemonService)
	loader.Profiles = profiles
	set, err := loader.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range wantServed {
		if _, ok := set.Secrets[name]; !ok {
			t.Fatalf("the load refuses %s (%+v), so the row of a served secret compares nothing", name, set.Refused[policytest.ID(name)])
		}
	}
	for _, name := range wantRefused {
		if _, ok := set.Refused[policytest.ID(name)]; !ok {
			t.Fatalf("the load serves %s, so the row of a refusal compares nothing", name)
		}
	}
	for _, row := range rows {
		name := policy.SlugToName(row.Name[len(policytest.Prefix):])
		got, reason, detail := profiles.SecretOfTags(memory.Source(row.Name), row.Tags)
		if refusal, refused := set.Refused[row.Name]; refused {
			if reason != refusal.Reason || detail != refusal.Detail {
				t.Errorf("SecretOfTags(%s) = %q, %q; the load refuses it %q, %q", name, reason, detail, refusal.Reason, refusal.Detail)
			}
			continue
		}
		served := set.Secrets[name]
		if reason != "" {
			t.Errorf("SecretOfTags(%s) refuses it %q, %q; the load serves it", name, reason, detail)
			continue
		}
		if got.Proxy != served.Proxy || got.Way != served.Way || got.ProxyNarrowed != served.ProxyNarrowed || got.MaxGrant != served.MaxGrant ||
			got.SessionDefault != served.SessionDefault || got.Type() != served.Type() || got.ProfileDigest != served.ProfileDigest || got.Source != served.Source {
			t.Errorf("SecretOfTags(%s) = %+v; the load serves %+v", name, got, served)
		}
	}
	if got := set.Secrets["SIGN_KEY"]; got.Way != proxyrules.WaySign || got.MaxGrant != 2*time.Hour {
		t.Errorf("SIGN_KEY = %+v; want it signed with a 2h cap, so the row checks a way and a cap", got)
	}
	if got := set.Secrets["SESSION_KEY"]; !got.SessionDefault {
		t.Errorf("SESSION_KEY = %+v; want its default-grant read as a session's, so the row checks that field", got)
	}
}

// TestSourceMissingFaultWordsEachStateOfASource: a profile whose source the load does not serve is
// refused profile-source-missing, with the source's state as its detail, in the words of
// SourceMissingFault, which `profiles check` reports the same profile in: the source's refusal
// reason, with that refusal's detail where it has one; its wait for its value; its being a profile,
// which is never the source of another; and its absence. Each load refusal carries the source's
// owner where the store holds it. A stored secret refused or pending is worded so even when a
// profile holds its name, as the load words it.
func TestSourceMissingFaultWordsEachStateOfASource(t *testing.T) {
	unknownKind := policytest.Proxied(policytest.Secret("UNKNOWN_KIND_KEY", appOwner, policy.TierAgent, "v1"), "gitlab")
	store := memory.New(
		policytest.Secret("BAD_TIER_KEY", appOwner, "admin", "v1"),
		unknownKind,
		createdWithoutValue("WAITING_KEY", time.Now()),
		policytest.Proxied(policytest.Secret("PLAIN_KEY", appOwner, policy.TierAgent, "v1"), "example"),
	)
	file := policytest.KindsFile(
		`{"name": "TIER_READ", "source": "BAD_TIER_KEY", "proxy": "example"}`,
		`{"name": "KIND_READ", "source": "UNKNOWN_KIND_KEY", "proxy": "example"}`,
		`{"name": "WAITING_READ", "source": "WAITING_KEY", "proxy": "example"}`,
		`{"name": "FIRST_READ", "source": "PLAIN_KEY"}`,
		`{"name": "SECOND_READ", "source": "FIRST_READ"}`,
		`{"name": "GHOST_READ", "source": "NO_SUCH_KEY", "proxy": "example"}`,
	)
	profiles := policytest.Profiles(t, file, daemonService)
	set := policytest.ProfiledCurrent(t, store, file, daemonService).Get()
	kindDetail := set.Refused[policytest.ID("UNKNOWN_KIND_KEY")].Detail
	if kindDetail == "" {
		t.Fatalf("UNKNOWN_KIND_KEY's refusal has no detail, so the row of a refusal with a detail checks nothing: %+v", set.Refused)
	}
	for _, row := range []struct {
		profile, source, reason, detail, owner, want string
	}{
		{"TIER_READ", "BAD_TIER_KEY", policy.ReasonTierTagMalformed, "", appOwner, "source BAD_TIER_KEY: " + policy.ReasonTierTagMalformed},
		{"KIND_READ", "UNKNOWN_KIND_KEY", policy.ReasonProxyKindUnknown, kindDetail, appOwner, "source UNKNOWN_KIND_KEY: " + policy.ReasonProxyKindUnknown + " (" + kindDetail + ")"},
		{"WAITING_READ", "WAITING_KEY", policy.ReasonPendingValue, "", owner, "source WAITING_KEY: " + policy.ReasonPendingValue},
		{"SECOND_READ", "FIRST_READ", "", "", "", "source FIRST_READ: a profile, which is never the source of another"},
		{"GHOST_READ", "NO_SUCH_KEY", "", "", "", "source NO_SUCH_KEY: " + policy.ReasonAbsent},
	} {
		want := policy.ProfileFault{Reason: policy.ReasonProfileSourceMissing, Detail: row.want}
		if got := profiles.SourceMissingFault(row.source, row.reason, row.detail); got != want {
			t.Errorf("SourceMissingFault(%s, %q, %q) = %+v, want %+v", row.source, row.reason, row.detail, got, want)
		}
		if refusal := refusedProfile(t, set, row.profile, policy.ReasonProfileSourceMissing); refusal.Detail != row.want || refusal.Owner != row.owner {
			t.Errorf("the load refuses %s with detail %q and owner %q; want %q and %q", row.profile, refusal.Detail, refusal.Owner, row.want, row.owner)
		}
	}
	if got, want := profiles.SourceMissingFault("FIRST_READ", policy.ReasonTierTagMalformed, ""), "source FIRST_READ: "+policy.ReasonTierTagMalformed; got.Detail != want {
		t.Errorf("SourceMissingFault of a refused stored secret a profile's name is = %q, want %q: the store's state comes first", got.Detail, want)
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
