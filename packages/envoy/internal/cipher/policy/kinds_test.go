package policy_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// kindsProfiled is the live policy of a store holding stored, under the profiles file holding
// profiles and kinds, a JSON list of kinds (policytest.KindsFileOf), with daemonService registered.
func kindsProfiled(t *testing.T, kinds string, stored []secrets.LocalSecret, profiles ...string) *policy.Set {
	t.Helper()
	return policytest.ProfiledCurrent(t, secrets.NewLocal(stored...), policytest.KindsFileOf(kinds, profiles...), daemonService).Get()
}

// refusedSecret fails t unless set refuses the stored secret name by its own id for reason, and
// serves nothing by that name; it answers the refusal.
func refusedSecret(t *testing.T, set *policy.Set, name, reason string) policy.Refusal {
	t.Helper()
	if secret, served := set.Secrets[name]; served {
		t.Fatalf("%s is served (%+v); want it refused %s", name, secret, reason)
	}
	refusal, refused := set.Refused[policytest.ID(name)]
	if !refused || refusal.Reason != reason {
		t.Fatalf("Refused[%s] = %+v (present %t); want reason %s", policytest.ID(name), refusal, refused, reason)
	}
	return refusal
}

// servedSecret fails t unless set serves name by its own name, as no profile, and answers it.
func servedSecret(t *testing.T, set *policy.Set, name string) policy.Secret {
	t.Helper()
	secret, served := set.Secrets[name]
	if !served || secret.Profile != nil {
		t.Fatalf("%s served = %t as %+v, refused %+v; want it served by its own name", name, served, secret, set.Refused[policytest.ID(name)])
	}
	return secret
}

// TestAProxyTagPutsASecretBehindItsKind: a stored secret whose proxy tag names a kind of the file is
// served behind that kind, by the way its type takes (a stored value swaps), with a fingerprint of
// its tags, and the set's proxy rules bind its name to the kind, after the file's own rules, with
// no allow of its own: its kind's alone bounds it. The rules are valid, and a set with no profiles
// applied, a bare load, answers them too. The control is the same secret with no tag: behind no
// kind, no fingerprint, no binding, and the version of a deployment before the kinds.
func TestAProxyTagPutsASecretBehindItsKind(t *testing.T) {
	tagged := policytest.Proxied(policytest.Secret("SLACK_TOKEN", appOwner, policy.TierAgent, "xoxb-v1"), "slack")
	store := secrets.NewLocal(tagged)
	loader := policytest.Loader(store, daemonService)
	loader.Profiles = policytest.Profiles(t, policytest.KindsFile(), daemonService)
	for name, set := range map[string]*policy.Set{
		"the live set": policytest.ProfiledCurrent(t, store, policytest.KindsFile(), daemonService).Get(),
		"a bare load":  must(loader.Load(t.Context())),
	} {
		t.Run(name, func(t *testing.T) {
			secret := servedSecret(t, set, "SLACK_TOKEN")
			if secret.Proxy != "slack" || secret.Way != proxyrules.WaySwap || secret.ProxyNarrowed || secret.ProxyAllow() != nil || secret.ProfileDigest == "" {
				t.Errorf("SLACK_TOKEN = %+v; want it behind slack, way swap, with a fingerprint and no allow of its own", secret)
			}
			rules := set.ProxyRules()
			want := proxyrules.Rules{Kinds: loader.Profiles.Kinds(), Names: []proxyrules.Binding{{Name: "SLACK_TOKEN", Kind: "slack", Way: proxyrules.WaySwap}}}
			if !reflect.DeepEqual(rules, want) {
				t.Errorf("ProxyRules() = %+v, want %+v", rules, want)
			}
			if err := proxyrules.Validate(rules); err != nil {
				t.Errorf("the set's rules are not valid: %v", err)
			}
		})
	}
	t.Run("the control: no proxy tag", func(t *testing.T) {
		bare := policytest.Secret("SLACK_TOKEN", appOwner, policy.TierAgent, "xoxb-v1")
		set := policytest.ProfiledCurrent(t, secrets.NewLocal(bare), policytest.KindsFile(), daemonService).Get()
		secret := servedSecret(t, set, "SLACK_TOKEN")
		if secret.Proxy != "" || secret.Way != "" || secret.ProfileDigest != "" || set.ProxyRules().Lookup("SLACK_TOKEN") != nil {
			t.Errorf("an untagged SLACK_TOKEN = %+v, bound %+v; want it behind no kind", secret, set.ProxyRules().Lookup("SLACK_TOKEN"))
		}
		if before := policytest.Current(t, secrets.NewLocal(policytest.Secret("SLACK_TOKEN", appOwner, policy.TierAgent, "xoxb-v1"))).Get(); before.Version != set.Version {
			t.Errorf("an untagged secret under a kinds file moved the version from %s to %s", before.Version, set.Version)
		}
	})
}

// must is v, failing the test binary on err: for a load a test cannot go on without.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestEachProxyTagRefusalNamesWhatIsWrong holds one row per load refusal of a secret's own proxy
// tags, each with its detail: a proxy tag naming no kind of the file; a kind whose ways do not hold
// the way the secret's type takes (an azure-login secret under a kind of swap and mint;
// a stored value under the anthropic kind, which mints alone); a kind with a max_grant that the
// secret's max-grant tag is absent under or above; and a proxy-narrowed tag of any value but true.
// Each row's control is served: a kind that holds the way (a stored value under aws signs), a
// max-grant within the kind's, and proxy-narrowed=true. A reread (LoadOne) refuses each alike,
// with the same detail.
func TestEachProxyTagRefusalNamesWhatIsWrong(t *testing.T) {
	const capped = `[{"name": "capped", "hosts": ["api.example.com"], "ways": ["swap"], "max_grant": "12h"}]`
	withTags := func(s secrets.LocalSecret, tags map[string]string) secrets.LocalSecret {
		for k, v := range tags {
			s.Tags[k] = v
		}
		return s
	}
	plain := func() secrets.LocalSecret { return policytest.Secret("ROW_KEY", appOwner, policy.TierAgent, "row-v1") }
	azure := func() secrets.LocalSecret {
		return policytest.TypeSecret("ROW_KEY", appOwner, policy.TierAgent, policy.TypeAzureLogin, policytest.TypeValue(policy.TypeAzureLogin))
	}
	for _, row := range []struct {
		name    string
		kinds   string
		secret  secrets.LocalSecret
		reason  string
		details []string
	}{
		{"a kind the file does not declare", policytest.Kinds, withTags(plain(), map[string]string{policy.TagProxy: "gitlab"}), policy.ReasonProxyKindUnknown, []string{`"gitlab"`, "no kind"}},
		{"an empty proxy tag", policytest.Kinds, withTags(plain(), map[string]string{policy.TagProxy: ""}), policy.ReasonProxyKindUnknown, []string{`""`}},
		{"an azure-login secret under a kind that does not exchange", policytest.Kinds, withTags(azure(), map[string]string{policy.TagProxy: "github"}), policy.ReasonProxyWay, []string{"azure-login", "exchange", "github", "swap, mint)"}},
		{"a stored value under a kind that mints alone", policytest.Kinds, withTags(plain(), map[string]string{policy.TagProxy: "anthropic", policy.TagMaxGrant: "1h"}), policy.ReasonProxyWay, []string{"stored-value", "swap", "anthropic"}},
		{"no max-grant under a kind with a max_grant", capped, withTags(plain(), map[string]string{policy.TagProxy: "capped"}), policy.ReasonProxyMaxGrant, []string{"no max-grant", "capped", "12h"}},
		{"a max-grant above the kind's", capped, withTags(plain(), map[string]string{policy.TagProxy: "capped", policy.TagMaxGrant: "13h"}), policy.ReasonProxyMaxGrant, []string{"13h", "above", "12h"}},
		{"proxy-narrowed of another value", policytest.Kinds, withTags(plain(), map[string]string{policy.TagProxy: "example", policy.TagProxyNarrowed: "yes"}), policy.ReasonProxyNarrowedMalformed, []string{`"yes"`, "true"}},
		{"an empty proxy-narrowed", policytest.Kinds, withTags(plain(), map[string]string{policy.TagProxyNarrowed: ""}), policy.ReasonProxyNarrowedMalformed, []string{`""`}},
	} {
		t.Run(row.name, func(t *testing.T) {
			set := kindsProfiled(t, row.kinds, []secrets.LocalSecret{row.secret})
			refusal := refusedSecret(t, set, "ROW_KEY", row.reason)
			for _, want := range row.details {
				if !strings.Contains(refusal.Detail, want) {
					t.Errorf("the refusal's detail %q does not say %q", refusal.Detail, want)
				}
			}
			if refusal.Owner != appOwner {
				t.Errorf("the refusal names the owner %q, want %s", refusal.Owner, appOwner)
			}

			// A reread finds the same refusal, detail and all: the secret starts served, the
			// store is retagged, and RefreshOne merges what LoadOne reads.
			store := secrets.NewLocal(plain())
			cur := policytest.ProfiledCurrent(t, store, policytest.KindsFileOf(row.kinds), daemonService)
			servedSecret(t, cur.Get(), "ROW_KEY")
			store.Put(row.secret)
			if lk, err := cur.RefreshOne(context.Background(), "ROW_KEY"); err != nil || lk.Served || lk.Reason != row.reason {
				t.Fatalf("RefreshOne(ROW_KEY) = %+v, %v; want refused %s", lk, err, row.reason)
			}
			if reread := cur.Get().Refused[policytest.ID("ROW_KEY")]; reread != refusal {
				t.Errorf("the reread's refusal = %+v, want the load's %+v", reread, refusal)
			}
		})
	}
	t.Run("the controls", func(t *testing.T) {
		for name, c := range map[string]struct {
			kinds  string
			secret secrets.LocalSecret
			way    string
		}{
			"a stored value under aws signs":            {policytest.Kinds, withTags(plain(), map[string]string{policy.TagProxy: "aws"}), proxyrules.WaySign},
			"an azure-login secret under example":       {policytest.Kinds, withTags(azure(), map[string]string{policy.TagProxy: "example"}), proxyrules.WayExchange},
			"a max-grant at the kind's":                 {capped, withTags(plain(), map[string]string{policy.TagProxy: "capped", policy.TagMaxGrant: "12h"}), proxyrules.WaySwap},
			"proxy-narrowed=true under its one profile": {policytest.Kinds, withTags(plain(), map[string]string{policy.TagProxy: "example", policy.TagProxyNarrowed: policy.ProxyNarrowedTrue}), proxyrules.WaySwap},
		} {
			t.Run(name, func(t *testing.T) {
				profiles := []string{}
				if c.secret.Tags[policy.TagProxyNarrowed] != "" {
					profiles = append(profiles, `{"name": "ROW_KEY", "allow": [{"method": "GET", "pathPrefix": "/"}]}`)
				}
				set := kindsProfiled(t, c.kinds, []secrets.LocalSecret{c.secret}, profiles...)
				secret, served := set.Secrets["ROW_KEY"]
				if !served || secret.Way != c.way {
					t.Fatalf("ROW_KEY served %t as %+v, refused %+v; want it served by way %s", served, secret, set.Refused[policytest.ID("ROW_KEY")], c.way)
				}
			})
		}
	})
}

// TestWayOfTakesTheWayOfEachType: a stored value swaps, or signs under a kind that signs and does
// not swap; every minted type mints; azure-login exchanges; and the answer is false where the kind's
// ways do not hold that way, or the type is none the broker knows.
func TestWayOfTakesTheWayOfEachType(t *testing.T) {
	every := proxyrules.Kind{Ways: proxyrules.Ways}
	for _, row := range []struct {
		kind       proxyrules.Kind
		secretType string
		way        string
		ok         bool
	}{
		{every, "", proxyrules.WaySwap, true},
		{proxyrules.Kind{Ways: []string{proxyrules.WaySign}}, "", proxyrules.WaySign, true},
		{proxyrules.Kind{Ways: []string{proxyrules.WayMint, proxyrules.WaySign}}, "", proxyrules.WaySign, true},
		{proxyrules.Kind{Ways: []string{proxyrules.WayMint}}, "", proxyrules.WaySwap, false},
		{every, policy.TypeGitHubApp, proxyrules.WayMint, true},
		{every, policy.TypeGitHubInstallations, proxyrules.WayMint, true},
		{every, policy.TypeGoogleLogin, proxyrules.WayMint, true},
		{every, policy.TypeGoogleServiceAccount, proxyrules.WayMint, true},
		{every, policy.TypeAnthropicFederation, proxyrules.WayMint, true},
		{every, policy.TypeAzureLogin, proxyrules.WayExchange, true},
		{proxyrules.Kind{Ways: []string{proxyrules.WaySwap}}, policy.TypeAnthropicFederation, proxyrules.WayMint, false},
		{proxyrules.Kind{Ways: []string{proxyrules.WayMint}}, policy.TypeAzureLogin, proxyrules.WayExchange, false},
		{every, "no-such-type", "", false},
	} {
		if way, ok := policy.WayOf(row.kind, row.secretType); way != row.way || ok != row.ok {
			t.Errorf("WayOf(%v, %q) = %q, %t; want %q, %t", row.kind.Ways, row.secretType, way, ok, row.way, row.ok)
		}
	}
}

// TestAProxyStringServesAProfileUntilItsSourceIsTagged: a profile's proxy string serves it
// behind that kind while its source carries no proxy tag; a string that names its source's tag's
// kind is served, and each full load writes one ProxyKindRedundantMessage line for it, a reread
// none; a string that names another kind refuses the profile, naming both. A profile behind its
// string's kind is held to it as a tagged source is: a way its kind's ways hold, and a grant cap
// within its kind's max_grant, its own maxGrant over its source's.
func TestAProxyStringServesAProfileUntilItsSourceIsTagged(t *testing.T) {
	plain := func() secrets.LocalSecret {
		return policytest.Secret("PLAIN_KEY", appOwner, policy.TierAgent, "plain-v1")
	}
	t.Run("a string alone", func(t *testing.T) {
		set := kindsProfiled(t, policytest.Kinds, []secrets.LocalSecret{plain()}, `{"name": "PLAIN_READ", "source": "PLAIN_KEY", "proxy": "github"}`)
		if got := servedProfile(t, set, "PLAIN_READ"); got.Proxy != "github" || got.Way != proxyrules.WaySwap {
			t.Errorf("PLAIN_READ is served behind %q by way %q; want github, swap", got.Proxy, got.Way)
		}
		if b := set.ProxyRules().Lookup("PLAIN_READ"); b == nil || b.Kind != "github" {
			t.Errorf("PLAIN_READ's binding = %+v; want the github kind", b)
		}
	})
	t.Run("a string naming another kind than the source's tag", func(t *testing.T) {
		set := kindsProfiled(t, policytest.Kinds, []secrets.LocalSecret{policytest.Proxied(plain(), "slack")}, `{"name": "PLAIN_READ", "source": "PLAIN_KEY", "proxy": "github"}`)
		refusal := refusedProfile(t, set, "PLAIN_READ", policy.ReasonProxyKindConflict)
		if !strings.Contains(refusal.Detail, "github") || !strings.Contains(refusal.Detail, "slack") {
			t.Errorf("the conflict's detail %q names not both kinds", refusal.Detail)
		}
	})
	t.Run("a string naming the source's tag's kind", func(t *testing.T) {
		logged := policytest.CaptureLog(t)
		store := secrets.NewLocal(policytest.Proxied(plain(), "github"))
		cur := policytest.ProfiledCurrent(t, store, policytest.KindsFile(`{"name": "PLAIN_READ", "source": "PLAIN_KEY", "proxy": "github"}`), daemonService)
		if got := servedProfile(t, cur.Get(), "PLAIN_READ"); got.Proxy != "github" {
			t.Errorf("PLAIN_READ is served behind %q; want github", got.Proxy)
		}
		if err := cur.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := cur.RefreshOne(context.Background(), "PLAIN_KEY"); err != nil {
			t.Fatal(err)
		}
		lines := strings.Count(logged.String(), policy.ProxyKindRedundantMessage)
		if lines != 2 || !strings.Contains(logged.String(), "profile=PLAIN_READ") || !strings.Contains(logged.String(), "kind=github") {
			t.Errorf("two loads and a reread wrote %d %q lines, want 2, one per load, naming the profile and the kind:\n%s", lines, policy.ProxyKindRedundantMessage, logged)
		}
	})
	t.Run("the control: no line for a string the source's tag does not repeat", func(t *testing.T) {
		logged := policytest.CaptureLog(t)
		kindsProfiled(t, policytest.Kinds, []secrets.LocalSecret{plain()}, `{"name": "PLAIN_READ", "source": "PLAIN_KEY", "proxy": "github"}`)
		if strings.Contains(logged.String(), policy.ProxyKindRedundantMessage) {
			t.Errorf("a string its source's tag does not repeat wrote %q:\n%s", policy.ProxyKindRedundantMessage, logged)
		}
	})
	t.Run("a string naming a kind its source's type takes no way of", func(t *testing.T) {
		login := policytest.TypeSecret("AZURE_KEY", appOwner, policy.TierAgent, policy.TypeAzureLogin, policytest.TypeValue(policy.TypeAzureLogin))
		set := kindsProfiled(t, policytest.Kinds, []secrets.LocalSecret{login}, `{"name": "AZURE_READ", "source": "AZURE_KEY", "proxy": "slack"}`)
		refusedProfile(t, set, "AZURE_READ", policy.ReasonProxyWay)
	})
	t.Run("a string naming a kind whose max_grant its grant cap is not within", func(t *testing.T) {
		const capped = `[{"name": "capped", "hosts": ["api.example.com"], "ways": ["swap"], "max_grant": "1h"}]`
		set := kindsProfiled(t, capped, []secrets.LocalSecret{plain()},
			`{"name": "PLAIN_READ", "source": "PLAIN_KEY", "proxy": "capped"}`,
			`{"name": "PLAIN_SHORT", "source": "PLAIN_KEY", "proxy": "capped", "maxGrant": "1h"}`)
		refusedProfile(t, set, "PLAIN_READ", policy.ReasonProxyMaxGrant)
		servedProfile(t, set, "PLAIN_SHORT")
	})
}

// enterpriseRows is GITHUB_ENTERPRISE_ADMIN's allow in the deployment repository, moved to its profile:
// five rows, each on api.github.com alone.
const enterpriseRows = `"allow": [` +
	`{"method": "GET", "pathPrefix": "/enterprises/example-enterprise/actions/cache/storage-limit", "host": "api.github.com"}, ` +
	`{"method": "PUT", "pathPrefix": "/enterprises/example-enterprise/actions/cache/storage-limit", "host": "api.github.com"}, ` +
	`{"method": "GET", "pathPrefix": "/enterprises/example-enterprise/audit-log/stream-key", "host": "api.github.com"}, ` +
	`{"method": "GET", "pathPrefix": "/enterprises/example-enterprise/audit-log/streams", "host": "api.github.com"}, ` +
	`{"method": "POST", "pathPrefix": "/enterprises/example-enterprise/audit-log/streams", "host": "api.github.com"}]`

// enterpriseAdmin is GITHUB_ENTERPRISE_ADMIN as the design tags it: a stored classic token of the
// person appOwner, human tier, an administrator's, asked for by appOwner alone for an hour at
// most, behind the github kind and narrowed by its profile (narrowed); with narrowed false, the
// same secret with its proxy-narrowed tag removed.
func enterpriseAdmin(narrowed bool) secrets.LocalSecret {
	s := policytest.ListedSecret("GITHUB_ENTERPRISE_ADMIN", appOwner, policy.TierHuman, appOwner, "gea-v1")
	s.Tags[policy.TagAdmin] = policy.AdminTrue
	s = policytest.Proxied(s, "github")
	if narrowed {
		s = policytest.Narrowed(s)
	}
	return s
}

// TestANarrowedSecretIsServedThroughExactlyOneProfileWithAnAllow: a secret
// tagged proxy-narrowed=true is served through exactly one profile, and that profile has an allow.
// On GITHUB_ENTERPRISE_ADMIN with its profile holding its five rows, which is served: the profile
// deleted refuses the source by its own name; a second profile of the source with no allow refuses
// both profiles and the source; the one profile's allow removed refuses the secret and the profile;
// two profiles that each hold an allow refuse both and the secret; and one profile named otherwise
// than its source holding the five rows is served, its source served by its own name to nobody
// (ErrServedOnlyAsProfiles). Controls: the deleted-profile, second-profile and allow-removed files
// with the tag removed serve the token with the github kind's allow alone, so each of those
// refusals comes from the tag; each of the two profiles of the two-allow file alone is served, so
// that refusal comes from their count.
func TestANarrowedSecretIsServedThroughExactlyOneProfileWithAnAllow(t *testing.T) {
	const (
		own       = `{"name": "GITHUB_ENTERPRISE_ADMIN", ` + enterpriseRows + `}`
		bareOwn   = `{"name": "GITHUB_ENTERPRISE_ADMIN"}`
		second    = `{"name": "GEA_SECOND", "source": "GITHUB_ENTERPRISE_ADMIN"}`
		rowsOther = `{"name": "GEA_LIMITS", "source": "GITHUB_ENTERPRISE_ADMIN", ` + enterpriseRows + `}`
	)
	served := func(t *testing.T, narrowed bool, profiles ...string) *policy.Set {
		t.Helper()
		return kindsProfiled(t, policytest.Kinds, []secrets.LocalSecret{enterpriseAdmin(narrowed)}, profiles...)
	}
	t.Run("its one profile with the five rows is served", func(t *testing.T) {
		set := served(t, true, own)
		got := servedProfile(t, set, "GITHUB_ENTERPRISE_ADMIN")
		if got.Proxy != "github" || !got.ProxyNarrowed || len(got.ProxyAllow()) != 5 {
			t.Fatalf("GITHUB_ENTERPRISE_ADMIN = %+v with allow %v; want it behind github, narrowed, with its five rows", got, got.ProxyAllow())
		}
		if b := set.ProxyRules().Lookup("GITHUB_ENTERPRISE_ADMIN"); b == nil || !reflect.DeepEqual(b.Allow, got.ProxyAllow()) {
			t.Errorf("its binding = %+v; want its profile's five rows", b)
		}
	})
	t.Run("the profile deleted", func(t *testing.T) {
		refusal := refusedSecret(t, served(t, true), "GITHUB_ENTERPRISE_ADMIN", policy.ReasonProxyNarrowedProfiles)
		if !strings.Contains(refusal.Detail, "no profile names it") {
			t.Errorf("the refusal's detail %q does not say no profile names it", refusal.Detail)
		}
		_, err := served(t, true).Evaluate("GITHUB_ENTERPRISE_ADMIN", policy.Requester{Operator: appOwner})
		if !errors.Is(err, policy.ErrProfileRefused) || !strings.Contains(err.Error(), policy.ReasonProxyNarrowedProfiles) {
			t.Errorf("Evaluate(GITHUB_ENTERPRISE_ADMIN) = %v; want ErrProfileRefused naming %s", err, policy.ReasonProxyNarrowedProfiles)
		}
	})
	t.Run("the control: the profile deleted and the tag removed", func(t *testing.T) {
		set := served(t, false)
		if got := servedSecret(t, set, "GITHUB_ENTERPRISE_ADMIN"); got.Proxy != "github" || got.ProxyAllow() != nil {
			t.Errorf("GITHUB_ENTERPRISE_ADMIN = %+v; want it served behind github with the kind's allow alone", got)
		}
		if b := set.ProxyRules().Lookup("GITHUB_ENTERPRISE_ADMIN"); b == nil || b.Allow != nil {
			t.Errorf("its binding = %+v; want no allow of its own", b)
		}
	})
	t.Run("a second profile with no allow", func(t *testing.T) {
		set := served(t, true, own, second)
		refusedProfile(t, set, "GITHUB_ENTERPRISE_ADMIN", policy.ReasonProxyNarrowedProfiles)
		refusal := refusedProfile(t, set, "GEA_SECOND", policy.ReasonProxyNarrowedProfiles)
		if !strings.Contains(refusal.Detail, "GITHUB_ENTERPRISE_ADMIN, GEA_SECOND") {
			t.Errorf("the refusal's detail %q does not name both profiles", refusal.Detail)
		}
		refusedSecret(t, set, "GITHUB_ENTERPRISE_ADMIN", policy.ReasonProxyNarrowedProfiles)
	})
	t.Run("the control: a second profile with no allow and the tag removed", func(t *testing.T) {
		set := served(t, false, own, second)
		if got := servedProfile(t, set, "GEA_SECOND"); got.Proxy != "github" || got.ProxyAllow() != nil {
			t.Errorf("GEA_SECOND = %+v; want it served behind github with the kind's allow alone", got)
		}
	})
	t.Run("the one profile's allow removed", func(t *testing.T) {
		set := served(t, true, bareOwn)
		refusedProfile(t, set, "GITHUB_ENTERPRISE_ADMIN", policy.ReasonProxyNarrowedProfiles)
		refusedSecret(t, set, "GITHUB_ENTERPRISE_ADMIN", policy.ReasonProxyNarrowedProfiles)
	})
	t.Run("the control: the allow removed and the tag removed", func(t *testing.T) {
		if got := servedProfile(t, served(t, false, bareOwn), "GITHUB_ENTERPRISE_ADMIN"); got.ProxyAllow() != nil {
			t.Errorf("GITHUB_ENTERPRISE_ADMIN = %+v; want it served with the kind's allow alone", got)
		}
	})
	t.Run("two profiles each with an allow", func(t *testing.T) {
		set := served(t, true, own, rowsOther)
		refusedProfile(t, set, "GITHUB_ENTERPRISE_ADMIN", policy.ReasonProxyNarrowedProfiles)
		refusedProfile(t, set, "GEA_LIMITS", policy.ReasonProxyNarrowedProfiles)
		refusedSecret(t, set, "GITHUB_ENTERPRISE_ADMIN", policy.ReasonProxyNarrowedProfiles)
	})
	t.Run("the control: each of the two alone", func(t *testing.T) {
		servedProfile(t, served(t, true, own), "GITHUB_ENTERPRISE_ADMIN")
		servedProfile(t, served(t, true, rowsOther), "GEA_LIMITS")
	})
	t.Run("one profile named otherwise than its source holding the rows", func(t *testing.T) {
		set := served(t, true, rowsOther)
		if got := servedProfile(t, set, "GEA_LIMITS"); len(got.ProxyAllow()) != 5 {
			t.Errorf("GEA_LIMITS = %+v; want its five rows", got)
		}
		if _, err := set.Evaluate("GITHUB_ENTERPRISE_ADMIN", policy.Requester{Operator: appOwner}); !errors.Is(err, policy.ErrServedOnlyAsProfiles) {
			t.Errorf("Evaluate(GITHUB_ENTERPRISE_ADMIN) = %v; want ErrServedOnlyAsProfiles", err)
		}
		if refusal, refused := set.Refused[policytest.ID("GITHUB_ENTERPRISE_ADMIN")]; refused {
			t.Errorf("the source is refused %+v; it is served through its one profile", refusal)
		}
	})
}

// TestANarrowedProfilesAllowStaysInsideItsKind: a profile's allow narrows its kind
// at the proxy and never widens it. A profile with an allow whose source lacks proxy-narrowed=true
// is refused, naming the tag; control, the same file with the tag serves it. A row naming a host
// its kind does not bind is refused, naming the row; control, the same row on a host the kind
// binds is served. A row its kind's allow does not cover (proxyrules.Covers) is refused, naming
// that row and not the one it covers; control, the same file under a kind whose allow covers the
// row serves it; and with the containment check skipped the row would be served and still refused
// at the decision by the kind's allow, which is why the load names it rather than let it read as a
// narrowing in force.
func TestANarrowedProfilesAllowStaysInsideItsKind(t *testing.T) {
	const (
		kinds = `[{"name": "github", "hosts": ["api.github.com"], "ways": ["swap"], "allow": [` +
			`{"method": "GET", "pathPrefix": "/enterprises/t/actions/"}, {"method": "PUT", "pathPrefix": "/enterprises/t/actions/"}]}]`
		wider = `[{"name": "github", "hosts": ["api.github.com"], "ways": ["swap"], "allow": [{"method": "GET", "pathPrefix": "/enterprises/"}]}]`
	)
	source := func(narrowed bool) []secrets.LocalSecret {
		s := policytest.Proxied(policytest.Secret("GEA_TOKEN", appOwner, policy.TierHuman, "gea-v1"), "github")
		if narrowed {
			s = policytest.Narrowed(s)
		}
		return []secrets.LocalSecret{s}
	}
	profile := func(rows string) string {
		return `{"name": "GEA_TOKEN", "allow": [` + rows + `]}`
	}
	const inside = `{"method": "GET", "pathPrefix": "/enterprises/t/actions/cache/storage-limit"}`
	const billing = `{"method": "GET", "pathPrefix": "/enterprises/t/billing/"}`

	t.Run("an allow whose source lacks proxy-narrowed", func(t *testing.T) {
		refusal := refusedProfile(t, kindsProfiled(t, kinds, source(false), profile(inside)), "GEA_TOKEN", policy.ReasonProfileAllowNotNarrowed)
		if !strings.Contains(refusal.Detail, policy.TagProxyNarrowed) {
			t.Errorf("the refusal's detail %q does not name the tag", refusal.Detail)
		}
		servedProfile(t, kindsProfiled(t, kinds, source(true), profile(inside)), "GEA_TOKEN")
	})
	t.Run("a row on a host the kind does not bind", func(t *testing.T) {
		refusal := refusedProfile(t, kindsProfiled(t, kinds, source(true), profile(`{"method": "GET", "pathPrefix": "/enterprises/t/actions/", "host": "github.com"}`)), "GEA_TOKEN", policy.ReasonProfileAllowHost)
		if !strings.Contains(refusal.Detail, "github.com") || !strings.Contains(refusal.Detail, "api.github.com") {
			t.Errorf("the refusal's detail %q does not name the row's host and the kind's", refusal.Detail)
		}
		got := servedProfile(t, kindsProfiled(t, kinds, source(true), profile(`{"method": "GET", "pathPrefix": "/enterprises/t/actions/", "host": "api.github.com"}`)), "GEA_TOKEN")
		if got.ProxyAllow()[0].Host != "api.github.com" {
			t.Errorf("GEA_TOKEN's row = %+v; want its host kept", got.ProxyAllow()[0])
		}
	})
	t.Run("a row outside its kind's allow", func(t *testing.T) {
		refusal := refusedProfile(t, kindsProfiled(t, kinds, source(true), profile(inside+`, `+billing)), "GEA_TOKEN", policy.ReasonProfileAllowOutsideKind)
		if !strings.Contains(refusal.Detail, "GET /enterprises/t/billing/") || strings.Contains(refusal.Detail, "storage-limit") {
			t.Errorf("the refusal's detail %q does not name the billing row alone", refusal.Detail)
		}
		servedProfile(t, kindsProfiled(t, wider, source(true), profile(inside+`, `+billing)), "GEA_TOKEN")
		// The check skipped: the binding the load would have made, decided as the proxy decides.
		var kind []proxyrules.Kind
		kind = policytest.Profiles(t, policytest.KindsFileOf(kinds)).Kinds()
		rules := proxyrules.Rules{Kinds: kind, Names: []proxyrules.Binding{{Name: "GEA_TOKEN", Kind: "github", Way: proxyrules.WaySwap, Allow: []proxyrules.Allow{
			{Method: "GET", PathPrefix: "/enterprises/t/actions/cache/storage-limit"}, {Method: "GET", PathPrefix: "/enterprises/t/billing/"},
		}}}}
		request := &proxyrules.Request{ConnectHost: "api.github.com", ConnectPort: 443, SNI: "api.github.com", HostHeader: "api.github.com",
			Method: "GET", Path: "/enterprises/t/billing/x", RawPath: "/enterprises/t/billing/x", Headers: [][2]string{{"Authorization", "Bearer p"}}}
		if ok, why := proxyrules.Decide(rules, proxyrules.Grant{Secret: "GEA_TOKEN", Placeholder: "p"}, "v", request); ok || why != "not allowed by rule" {
			t.Errorf("the decision on GET /enterprises/t/billing/x = %t, %q; want it refused by the kind's allow", ok, why)
		}
	})
}

// TestAProfileOfASourceOnlyTheProxyScopesNeedsAKind: GITHUB_ENTERPRISE_ADMIN's
// source carrying no proxy tag, and its profile no proxy string, refuses the profile and the source
// by that name, profile-source-not-proxied; control, the same file with the tag serves it.
func TestAProfileOfASourceOnlyTheProxyScopesNeedsAKind(t *testing.T) {
	untagged := policytest.Narrowed(policytest.ListedSecret("GITHUB_ENTERPRISE_ADMIN", appOwner, policy.TierHuman, appOwner, "gea-v1"))
	untagged.Tags[policy.TagAdmin] = policy.AdminTrue
	profile := `{"name": "GITHUB_ENTERPRISE_ADMIN", ` + enterpriseRows + `}`
	set := kindsProfiled(t, policytest.Kinds, []secrets.LocalSecret{untagged}, profile)
	refusedProfile(t, set, "GITHUB_ENTERPRISE_ADMIN", policy.ReasonProfileSourceNotProxied)
	refusedSecret(t, set, "GITHUB_ENTERPRISE_ADMIN", policy.ReasonProfileSourceNotProxied)
	servedProfile(t, kindsProfiled(t, policytest.Kinds, []secrets.LocalSecret{enterpriseAdmin(true)}, profile), "GITHUB_ENTERPRISE_ADMIN")
}

// TestAProxyTagOrAnAllowEndsTheGrantsOfItsNameAlone: a request records the fingerprint of each name
// it was decided under (Secret.ProfileDigest, request_secrets.airlock_digest), and a release ends a
// grant whose name's fingerprint moved, across a broker restart too, since the fingerprint is read
// from the store's tags and the file at each load. So a change of a served name's proxy tag, of the
// proxy-narrowed tag beside it, or of its profile's allow, moves that name's fingerprint, and the
// policy version, and leaves every other name's where it was. A kind that only gains a host or a
// header moves no fingerprint and no version, so it ends no grant.
func TestAProxyTagOrAnAllowEndsTheGrantsOfItsNameAlone(t *testing.T) {
	other := policytest.Proxied(policytest.Secret("OTHER_KEY", appOwner, policy.TierAgent, "other-v1"), "example")
	served := func(t *testing.T, kinds string, secret secrets.LocalSecret, profiles ...string) *policy.Set {
		t.Helper()
		return kindsProfiled(t, kinds, []secrets.LocalSecret{secret, other}, profiles...)
	}
	key := func(tags map[string]string) secrets.LocalSecret {
		s := policytest.Secret("ROW_KEY", appOwner, policy.TierHuman, "row-v1")
		for k, v := range tags {
			s.Tags[k] = v
		}
		return s
	}
	const narrowedProfile = `{"name": "ROW_KEY", "allow": [{"method": "GET", "pathPrefix": "/a/"}]}`
	sets := map[string]*policy.Set{
		"no proxy tag":               served(t, policytest.Kinds, key(nil)),
		"proxy=github":               served(t, policytest.Kinds, key(map[string]string{policy.TagProxy: "github"})),
		"proxy=slack":                served(t, policytest.Kinds, key(map[string]string{policy.TagProxy: "slack"})),
		"a profile of its name":      served(t, policytest.Kinds, key(map[string]string{policy.TagProxy: "github"}), `{"name": "ROW_KEY"}`),
		"narrowed by one allow":      served(t, policytest.Kinds, key(map[string]string{policy.TagProxy: "github", policy.TagProxyNarrowed: "true"}), narrowedProfile),
		"narrowed by another allow":  served(t, policytest.Kinds, key(map[string]string{policy.TagProxy: "github", policy.TagProxyNarrowed: "true"}), `{"name": "ROW_KEY", "allow": [{"method": "GET", "pathPrefix": "/b/"}]}`),
		"narrowed by an empty allow": served(t, policytest.Kinds, key(map[string]string{policy.TagProxy: "github", policy.TagProxyNarrowed: "true"}), `{"name": "ROW_KEY", "allow": []}`),
	}
	digests, versions := map[string]string{}, map[string]string{}
	for name, set := range sets {
		row, ok := set.Secrets["ROW_KEY"]
		if !ok {
			t.Fatalf("%s: ROW_KEY is not served: %+v", name, set.Refused)
		}
		if was, taken := digests[row.ProfileDigest]; taken {
			t.Errorf("%s and %s serve ROW_KEY with one fingerprint %q", name, was, row.ProfileDigest)
		}
		digests[row.ProfileDigest] = name
		if was, taken := versions[set.Version]; taken {
			t.Errorf("%s and %s hold one version %s", name, was, set.Version)
		}
		versions[set.Version] = name
		if got, want := set.Secrets["OTHER_KEY"].ProfileDigest, sets["no proxy tag"].Secrets["OTHER_KEY"].ProfileDigest; got != want || got == "" {
			t.Errorf("%s moved OTHER_KEY's fingerprint from %q to %q", name, want, got)
		}
	}

	// A kind that gains a host or a header: every name's fingerprint and the version stay.
	const (
		before = `[{"name": "github", "hosts": ["api.github.com"], "headers": ["Authorization"], "ways": ["swap"]}, {"name": "example", "hosts": ["api.example.com"], "ways": ["swap"]}]`
		gained = `[{"name": "github", "hosts": ["api.github.com", "uploads.github.com"], "headers": ["Authorization", "X-Api-Key"], "ways": ["swap"]}, {"name": "example", "hosts": ["api.example.com", "www.example.com"], "ways": ["swap"]}]`
	)
	tagged := func() secrets.LocalSecret {
		return key(map[string]string{policy.TagProxy: "github", policy.TagProxyNarrowed: "true"})
	}
	was, now := served(t, before, tagged(), narrowedProfile), served(t, gained, tagged(), narrowedProfile)
	if was.Version != now.Version || was.Secrets["ROW_KEY"].ProfileDigest != now.Secrets["ROW_KEY"].ProfileDigest || was.Secrets["OTHER_KEY"].ProfileDigest != now.Secrets["OTHER_KEY"].ProfileDigest {
		t.Errorf("a kind that gained a host and a header moved the version from %s to %s, or a fingerprint", was.Version, now.Version)
	}
	if hosts := now.ProxyRules().HostsOf("ROW_KEY"); len(hosts) != 2 {
		t.Errorf("ROW_KEY is bound to %v under the kind that gained a host; want both hosts", hosts)
	}
}

// TestProxyRulesAreBuiltOnceWithTheSet: the proxy reads a set's rules on every request and the
// release on every use of a grant, so ProxyRules answers the rules the set was built with and
// allocates nothing. A set NewSet builds outside a load holds none, and a stored secret tagged a
// kind under the name a dpop profile holds is bound once, as that profile's inline rule.
func TestProxyRulesAreBuiltOnceWithTheSet(t *testing.T) {
	tagged := policytest.Proxied(policytest.Secret("TEST_MINTED_TOKEN", appOwner, policy.TierAgent, "stored-v1"), "example")
	set := policytest.ProfiledCurrent(t, secrets.NewLocal(tagged, policytest.Proxied(policytest.Secret("SLACK_TOKEN", appOwner, policy.TierAgent, "x"), "slack")),
		policytest.KindsFile(`{"name": "TEST_MINTED_TOKEN", `+mintedBlock+`}`), daemonService).Get()
	if allocs := testing.AllocsPerRun(100, func() { _ = set.ProxyRules() }); allocs != 0 {
		t.Errorf("ProxyRules allocates %v times a call, want 0", allocs)
	}
	rules := set.ProxyRules()
	if len(rules.Names) != 2 || rules.Names[0].Name != "TEST_MINTED_TOKEN" || rules.Names[0].Inline == nil || rules.Names[0].Way != proxyrules.WayDPoP || rules.Names[1].Name != "SLACK_TOKEN" {
		t.Errorf("ProxyRules().Names = %+v; want the dpop block's inline binding, then SLACK_TOKEN's", rules.Names)
	}
	if err := proxyrules.Validate(rules); err != nil {
		t.Errorf("the set's rules are not valid: %v", err)
	}
	if none := policy.NewSet(map[string]policy.Secret{}).ProxyRules(); len(none.Kinds) != 0 || len(none.Names) != 0 {
		t.Errorf("NewSet's rules = %+v; want none", none)
	}
}
