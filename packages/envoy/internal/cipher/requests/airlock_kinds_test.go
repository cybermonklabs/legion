package requests

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// TestAReleaseTakesEachNamesHostsAndPlaceholderPrefixFromItsKind: what a release hands the session
// for a name behind the proxy comes from the kind the name is behind. The hosts the command keeps
// off its NO_PROXY are that kind's, every one of them; and a kind with a placeholder_prefix opens
// the name's placeholder with it in place of the default prefix, for a client that checks a key's
// shape before it sends it, followed by the 32 characters every placeholder draws. A name of a
// kind with no prefix keeps the default, and the placeholder of either opens the grant's
// connection.
func TestAReleaseTakesEachNamesHostsAndPlaceholderPrefixFromItsKind(t *testing.T) {
	t.Parallel()
	const prefix = "tml-"
	rules := proxyrules.Rules{
		Kinds: []proxyrules.Kind{
			{Name: "prefixed", Hosts: []string{"api.prefixed.example", "*.uploads.prefixed.example"}, Ways: []string{proxyrules.WaySwap}, PlaceholderPrefix: prefix},
			{Name: "plain", Hosts: []string{"api.plain.example"}, Ways: []string{proxyrules.WaySwap}},
		},
		Names: []proxyrules.Binding{rule("AUTO_TOKEN", "prefixed"), rule("SECOND_TOKEN", "plain")},
	}
	m, enr, key, _ := newPlaceholderFixture(t, rules)
	retag(t, m, policytest.Secret("SECOND_TOKEN", fixtureOperator, policy.TierAgent, "second-v1"))
	_, grantID := grantOf(t, m, enr, key, "read both", "AUTO_TOKEN", "SECOND_TOKEN")
	release, err := m.Values(t.Context(), grantID, enr, ReleaseCall{})
	if err != nil || release.Airlock == nil {
		t.Fatalf("Values = %+v, %v; want both names behind the proxy", release, err)
	}
	prefixed, plain := release.Airlock.Names["AUTO_TOKEN"], release.Airlock.Names["SECOND_TOKEN"]
	rest, opens := strings.CutPrefix(prefixed.Value, prefix)
	if !opens || len(prefixed.Value) != len(prefix)+placeholder.Length || strings.ContainsFunc(rest, func(r rune) bool { return !strings.ContainsRune(placeholder.Alphabet, r) }) {
		t.Errorf("AUTO_TOKEN's placeholder = %q; want %s and %d characters of %s, %d in all", prefixed.Value, prefix, placeholder.Length, placeholder.Alphabet, len(prefix)+placeholder.Length)
	}
	if prefixed.Slots[SlotValue] != prefixed.Value {
		t.Errorf("AUTO_TOKEN's slots = %v; want the value slot holding %q", prefixed.Slots, prefixed.Value)
	}
	wellFormed(t, "SECOND_TOKEN's placeholder", plain.Value)
	if !slices.Equal(prefixed.Hosts, rules.Kinds[0].Hosts) || !slices.Equal(plain.Hosts, rules.Kinds[1].Hosts) {
		t.Errorf("hosts = %v and %v; want each name's kind's, %v and %v", prefixed.Hosts, plain.Hosts, rules.Kinds[0].Hosts, rules.Kinds[1].Hosts)
	}
	holdsNothingOf(t, release, "auto-v1")
	holdsNothingOf(t, release, "second-v1")
	// AUTO_TOKEN sorts first, so its prefixed placeholder is the password, and a connection opens
	// with it as with any other.
	if release.Airlock.ProxyPassword != prefixed.Value {
		t.Errorf("proxy password = %q; want AUTO_TOKEN's placeholder %q", release.Airlock.ProxyPassword, prefixed.Value)
	}
	for _, presented := range []string{prefixed.Value, plain.Value} {
		conn, err := m.AirlockConnection(t.Context(), presented)
		if err != nil || conn.GrantID != grantID || !slices.Equal(slices.Sorted(maps.Keys(conn.Names)), []string{"AUTO_TOKEN", "SECOND_TOKEN"}) {
			t.Errorf("AirlockConnection(%q) = %+v, %v; want grant %s carrying both names", presented, conn, err, grantID)
		}
	}
}

// TestAProxyTagIsReadAtEachLoad: whether a name is behind the proxy is its secret's proxy tag as
// the policy last read it, not as the broker read it at start. A secret whose tag is taken off is
// released as its value from the next load on, and one whose tag is put on is held back from
// then; the rules the proxy reads (Machine.AirlockRules) move with them. A grant of either made
// before the change ends at its next release, since the kind a name is behind is in its
// fingerprint, and a grant made after it releases the name as the policy now reads it.
func TestAProxyTagIsReadAtEachLoad(t *testing.T) {
	t.Parallel()
	m, enr, key := newAirlockFixture(t, policytest.KindsFile())
	m.AirlockProxyURL, m.ProxyRoots = testProxyURL, func() string { return testCACertificate }
	retag(t, m, policytest.Secret("SECOND_TOKEN", fixtureOperator, policy.TierAgent, "second-v1"))
	proxyTag(t, m, "AUTO_TOKEN", "example")
	behind := func(name string) bool { return m.AirlockRules().Lookup(name) != nil }
	if !behind("AUTO_TOKEN") || behind("SECOND_TOKEN") {
		t.Fatalf("the rules bind AUTO_TOKEN %v and SECOND_TOKEN %v; want AUTO_TOKEN alone", behind("AUTO_TOKEN"), behind("SECOND_TOKEN"))
	}
	_, heldBack := grantOf(t, m, enr, key, "read it behind the proxy", "AUTO_TOKEN")
	release, err := m.Values(t.Context(), heldBack, enr, ReleaseCall{})
	if err != nil || release.Airlock == nil || release.Values["AUTO_TOKEN"] != "" {
		t.Fatalf("Values(AUTO_TOKEN) while its tag names example = %+v, %v; want it held back", release, err)
	}
	_, handed := grantOf(t, m, enr, key, "read it", "SECOND_TOKEN")
	if release, err := m.Values(t.Context(), handed, enr, ReleaseCall{}); err != nil || release.Airlock != nil || release.Values["SECOND_TOKEN"] != "second-v1" {
		t.Fatalf("Values(SECOND_TOKEN) with no tag = %+v, %v; want its value", release, err)
	}

	proxyTag(t, m, "AUTO_TOKEN", "")
	proxyTag(t, m, "SECOND_TOKEN", "example")
	if behind("AUTO_TOKEN") || !behind("SECOND_TOKEN") {
		t.Fatalf("after the retags the rules bind AUTO_TOKEN %v and SECOND_TOKEN %v; want SECOND_TOKEN alone", behind("AUTO_TOKEN"), behind("SECOND_TOKEN"))
	}
	for name, grantID := range map[string]string{"AUTO_TOKEN": heldBack, "SECOND_TOKEN": handed} {
		if release, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || release.Values != nil || release.Airlock != nil {
			t.Errorf("Values of %s's grant made before its tag changed = %+v, %v; want ErrGrantNotLive and nothing released", name, release, err)
		}
	}
	_, now := grantOf(t, m, enr, key, "read it, untagged now", "AUTO_TOKEN")
	if release, err := m.Values(t.Context(), now, enr, ReleaseCall{}); err != nil || release.Airlock != nil || release.Values["AUTO_TOKEN"] != "auto-v1" {
		t.Errorf("Values(AUTO_TOKEN) once its tag is off = %+v, %v; want its value", release, err)
	}
	_, now = grantOf(t, m, enr, key, "read it, tagged now", "SECOND_TOKEN")
	release, err = m.Values(t.Context(), now, enr, ReleaseCall{})
	if err != nil || release.Airlock == nil || release.Values["SECOND_TOKEN"] != "" {
		t.Fatalf("Values(SECOND_TOKEN) once its tag names example = %+v, %v; want it held back", release, err)
	}
	holdsNothingOf(t, release, "second-v1")
}

// exampleKinds is a kinds list of the one kind example, on hosts with headers, each a JSON list's
// items as written; headers "" names none, so the kind travels in Authorization.
func exampleKinds(hosts, headers string) string {
	kind := `{"name": "example", "hosts": [` + hosts + `], `
	if headers != "" {
		kind += `"headers": [` + headers + `], `
	}
	return `[` + kind + `"ways": ["swap", "mint", "exchange"]}]`
}

// The example kinds the kind-loss rows roll between: base, on two hosts in Authorization; gained,
// base with one host and one header more; and three narrowed forms of base, each losing one host
// or the one header, or naming the header in another case, which loses nothing.
var (
	baseKind       = exampleKinds(`"api.example.com", "uploads.example.com"`, "")
	gainedKind     = exampleKinds(`"api.example.com", "uploads.example.com", "more.example.com"`, `"Authorization", "X-Api-Key"`)
	lostHostKind   = exampleKinds(`"api.example.com"`, "")
	lostHeaderKind = exampleKinds(`"api.example.com", "uploads.example.com"`, `"X-Api-Key"`)
	casedKind      = exampleKinds(`"api.example.com", "uploads.example.com"`, `"AUTHORIZATION"`)
)

// kindFile is the profiles file of kinds whose profiles put each of names behind example by its
// own name, as a proxy string.
func kindFile(kinds string, names ...string) string {
	profiles := make([]string, len(names))
	for i, name := range names {
		profiles[i] = `{"name": "` + name + `", "proxy": "example"}`
	}
	return policytest.KindsFileOf(kinds, profiles...)
}

// TestAKindThatLosesAHostOrAHeaderEndsTheGrantsDecidedUnderIt: a grant reaches the hosts its
// name's kind binds and travels in the kind's headers, so a kind that loses a host or a header
// ends, for good and at the next release, each grant decided while it held them, naming what it
// lost, and a kind that only gains one ends none, nor does a header named in another case. Each
// roll is a new boot reading the new file (rollProfiles), so what the grant was decided under is
// read from the database and holds across a restart, and it holds where no fingerprint moved: the
// profile's own fingerprint is the same under every one of these kinds. A loss that only undoes a
// gain ends the grants decided under the gain, and the kind getting the host back does not bring
// the grant back.
func TestAKindThatLosesAHostOrAHeaderEndsTheGrantsDecidedUnderIt(t *testing.T) {
	t.Parallel()
	type roll struct {
		kinds string
		lost  string // the end's reason names it; "" for a grant that goes on releasing
	}
	for _, c := range []struct {
		name    string
		decided string
		rolls   []roll
	}{
		{"a kind that gains a host and a header, then loses a host", baseKind, []roll{{gainedKind, ""}, {lostHostKind, "the host uploads.example.com"}, {baseKind, "ended"}}},
		{"a kind that loses its one header", baseKind, []roll{{lostHeaderKind, "the header authorization"}}},
		{"a kind that names its header in another case", baseKind, []roll{{casedKind, ""}, {baseKind, ""}}},
		{"a loss that undoes the gain a grant was decided under", gainedKind, []roll{{baseKind, "the host more.example.com or the header X-Api-Key"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key := newAirlockFixture(t, kindFile(c.decided, "AUTO_TOKEN"))
			m.AirlockProxyURL, m.ProxyRoots = testProxyURL, func() string { return testCACertificate }
			_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
			digest := m.Policy.Get().Secrets["AUTO_TOKEN"].ProfileDigest
			if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
				t.Fatalf("Values under the kind the grant was decided under: %v", err)
			}
			for i, r := range c.rolls {
				rollProfiles(t, m, kindFile(r.kinds, "AUTO_TOKEN"))
				if got := m.Policy.Get().Secrets["AUTO_TOKEN"].ProfileDigest; got != digest {
					t.Fatalf("roll %d moved AUTO_TOKEN's fingerprint from %s to %s; this row is about a kind no fingerprint holds", i, digest, got)
				}
				release, err := m.Values(t.Context(), grantID, enr, ReleaseCall{})
				if r.lost == "" {
					if err != nil || release.Airlock == nil {
						t.Fatalf("Values after roll %d, which loses nothing = %+v, %v; want AUTO_TOKEN released behind the proxy", i, release, err)
					}
					continue
				}
				if !errors.Is(err, ErrGrantNotLive) || release.Airlock != nil || release.Values != nil {
					t.Fatalf("Values after roll %d, which loses %s = %+v, %v; want ErrGrantNotLive and nothing released", i, r.lost, release, err)
				}
				if by, audits := endedForGood(t, m, grantID); by != GrantEndedActor || audits != 1 {
					t.Fatalf("grant revoked_by = %q with %d grant.revoked rows by it; want %s and 1", by, audits, GrantEndedActor)
				}
				if r.lost == "ended" {
					continue
				}
				var reason string
				if err := m.Store.Pool.QueryRow(t.Context(), `select detail->>'reason' from audit where kind='grant.revoked' and grant_id=$1`, grantID).Scan(&reason); err != nil {
					t.Fatal(err)
				}
				if want := ErrGrantNotLive.Error() + ": the kind AUTO_TOKEN is behind no longer holds " + r.lost + ", which this grant was decided under"; reason != want {
					t.Fatalf("grant.revoked reason = %q, want %q", reason, want)
				}
			}
		})
	}
}

// TestAnApprovalRecordsTheKindItWasGivenUnder: a request that waits for a person records its
// name's kind again when it is approved, as it records its fingerprint (decidedUnder), so a grant
// approved after its kind lost a host is decided under the narrower kind and releases, rather than
// ending at its first release for a loss the person approved past; and the kind it was approved
// under is what a later loss is measured from, so getting the host back ends nothing and losing it
// again ends nothing either. A request Create reuses the live grant of is held to the same rule:
// once the kind loses a host the grant was approved under, Create ends the grant rather than hand
// it back, and asks afresh.
func TestAnApprovalRecordsTheKindItWasGivenUnder(t *testing.T) {
	t.Parallel()
	m, enr, key := newAirlockFixture(t, kindFile(baseKind, "SHARED_KEY"))
	m.AirlockProxyURL, m.ProxyRoots = testProxyURL, func() string { return testCACertificate }
	// SHARED_KEY is shared and human tier, so its grant comes of a person's approval.
	pending, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the shared key", "SHARED_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want pending", pending, err)
	}
	rollProfiles(t, m, kindFile(lostHostKind, "SHARED_KEY"))
	decided, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
	if err != nil || decided.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", decided, err)
	}
	for _, kinds := range []string{lostHostKind, baseKind, lostHostKind} {
		rollProfiles(t, m, kindFile(kinds, "SHARED_KEY"))
		if release, err := m.Values(t.Context(), decided.GrantID, enr, ReleaseCall{}); err != nil || release.Airlock == nil {
			t.Fatalf("Values of the grant approved under the narrower kind, under %s = %+v, %v; want SHARED_KEY released", kinds, release, err)
		}
	}

	// Approved under base this time, then the kind loses uploads.example.com: Create reuses no
	// grant whose release would end, and ends it itself.
	rollProfiles(t, m, kindFile(baseKind, "SHARED_KEY"))
	if err := m.RevokeGrant(t.Context(), decided.GrantID, enr); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	again, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the shared key again", "SHARED_KEY"), "")
	if err != nil || again.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) again = %+v, %v; want pending", again, err)
	}
	wide, err := m.ApplyDecision(t.Context(), *again.RecordID, true, fixtureOperator)
	if err != nil || wide.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", wide, err)
	}
	rollProfiles(t, m, kindFile(lostHostKind, "SHARED_KEY"))
	asked, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the shared key once more", "SHARED_KEY"), "")
	if err != nil || asked.GrantID != nil {
		t.Fatalf("Create(SHARED_KEY) once the kind lost a host = %+v, %v; want no reused grant", asked, err)
	}
	if by, audits := endedForGood(t, m, wide.GrantID); by != GrantEndedActor || audits != 1 {
		t.Fatalf("the grant approved under base is revoked by %q with %d grant.revoked rows by it; want %s and 1, as its release would end it", by, audits, GrantEndedActor)
	}
}

// TestADPoPBlockRecordsNoKind: a name a dpop block binds is bound by an inline rule and no kind,
// so a request would record no kind for it and no kind loss ends anything of it: its minted
// grants are held to their entry digest instead. It is read in the set the loader builds before the
// profiles' apply, the one set where a dpop block binds a name the policy serves.
func TestADPoPBlockRecordsNoKind(t *testing.T) {
	t.Parallel()
	m, _, _ := newAirlockFixture(t, policytest.KindsFile())
	loader := policytest.Loader(fixtureStore(m), fixtureService)
	loader.Profiles = policytest.Profiles(t, policytest.KindsFile(`{"name": "AUTO_TOKEN", "proxy": {"hosts": ["dispatch.example.com"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}}`), fixtureService)
	unapplied, err := loader.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if binding := unapplied.ProxyRules().Lookup("AUTO_TOKEN"); binding == nil || binding.Way != proxyrules.WayDPoP {
		t.Fatalf("AUTO_TOKEN's binding = %+v; want the dpop block's", binding)
	}
	if reach := reachOf(unapplied, "AUTO_TOKEN"); reach != nil {
		t.Fatalf("reachOf(a name a dpop block binds) = %+v; want none", reach)
	}
	if lost := kindLost(unapplied.ProxyRules(), "AUTO_TOKEN", nil); lost != nil {
		t.Fatalf("kindLost of a name that recorded no kind = %v; want none", lost)
	}
}
