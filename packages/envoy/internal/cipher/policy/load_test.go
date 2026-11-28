package policy_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestLoadRefusesEachUnservableSecretByNameWithTheExactLine pins every reason a secret under the
// prefix is refused, each handed to the set under its whole name in the store with its reason
// and its owner tag, while every other secret is still served: one person's malformed tag never
// freezes anyone else's policy. The owner is the tag as the store holds it, and empty for the two
// reasons that tag is itself the fault. A secret with no value and a tag or key fault is refused
// for the fault, which a value alone would not fix. The pod's first load over the same store
// writes the exact RefusedMessage line for each, marked first_load and in the order of the
// secrets' names. The one secret it serves carries shape=document, so the table holds both
// halves of that tag: a word the loader knows is carried into the Secret, and a word it does not
// is refused.
func TestLoadRefusesEachUnservableSecretByNameWithTheExactLine(t *testing.T) {
	refusals := map[string]struct {
		secret memory.Secret
		want   policy.Refusal
	}{}
	refuse := func(name, reason, refusedOwner string, edit func(*memory.Secret)) {
		s := policytest.Secret(name, owner, policy.TierAgent, "v")
		edit(&s)
		refusals[name] = struct {
			secret memory.Secret
			want   policy.Refusal
		}{s, policy.Refusal{Reason: reason, Owner: refusedOwner}}
	}
	refuse("NO_OWNER", policy.ReasonOwnerTagMissing, "", func(s *memory.Secret) { delete(s.Tags, policy.TagOwner) })
	refuse("GITHUB_LOGIN_OWNER", policy.ReasonOwnerTagMalformed, "", func(s *memory.Secret) { s.Tags[policy.TagOwner] = "octocat" })
	refuse("CASED_OWNER", policy.ReasonOwnerTagMalformed, "", func(s *memory.Secret) { s.Tags[policy.TagOwner] = "Person@Example.com" })
	refuse("EMPTY_OWNER", policy.ReasonOwnerTagMalformed, "", func(s *memory.Secret) { s.Tags[policy.TagOwner] = "" })
	refuse("UNREGISTERED_SERVICE", policy.ReasonOwnerTagMalformed, "", func(s *memory.Secret) { s.Tags[policy.TagOwner] = "dispatch" })
	refuse("NO_TIER", policy.ReasonTierTagMissing, owner, func(s *memory.Secret) { delete(s.Tags, policy.TagTier) })
	refuse("ADMIN_TIER", policy.ReasonTierTagMalformed, owner, func(s *memory.Secret) { s.Tags[policy.TagTier] = "admin" })
	refuse("CASED_TAG_KEY", policy.ReasonOwnerTagMissing, "", func(s *memory.Secret) {
		s.Tags["Owner"] = s.Tags[policy.TagOwner]
		delete(s.Tags, policy.TagOwner)
	})
	refuse("SERVICE_HUMAN", policy.ReasonServiceOwnerHumanTier, legion, func(s *memory.Secret) {
		s.Tags[policy.TagOwner], s.Tags[policy.TagTier] = legion, policy.TierHuman
	})
	refuse("MANAGED_KEY", policy.ReasonNotOnAgentSecretsKey, owner, func(s *memory.Secret) { s.OffKey = true })
	refuse("ANOTHER_KEY", policy.ReasonNotOnAgentSecretsKey, owner, func(s *memory.Secret) { s.OffKey = true })
	refuse("ANOTHER_KEYS_ALIAS", policy.ReasonNotOnAgentSecretsKey, owner, func(s *memory.Secret) { s.OffKey = true })
	refuse("NO_VALUE", policy.ReasonNoCurrentValue, owner, func(s *memory.Secret) { s.Value = "" })
	refuse("NO_TIER_NOR_VALUE", policy.ReasonTierTagMissing, owner, func(s *memory.Secret) {
		delete(s.Tags, policy.TagTier)
		s.Value = ""
	})
	refuse("MANAGED_KEY_NO_VALUE", policy.ReasonNotOnAgentSecretsKey, owner, func(s *memory.Secret) {
		s.OffKey = true
		s.Value = ""
	})

	wellTagged := policytest.Secret("WELL_TAGGED", owner, policy.TierAgent, "v")
	store := memory.New(wellTagged)
	want := map[string]policy.Refusal{}
	for name, r := range refusals {
		store.Put(r.secret)
		want[policytest.ID(name)] = r.want
	}
	// A name under the prefix that is no slug maps to no name a session asks for: it is refused
	// under its whole name in the store, which is all it has, with the owner its tag names.
	badName := policytest.Secret("X", owner, policy.TierAgent, "v")
	badName.Name = policytest.Prefix + "Deel_Api_Key"
	store.Put(badName)
	want[badName.Name] = policy.Refusal{Reason: policy.ReasonNameMalformed, Owner: owner}

	logged := policytest.CaptureLog(t)
	loader := policytest.Loader(store, legion)
	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != 1 || set.Secrets["WELL_TAGGED"].Name != "WELL_TAGGED" {
		t.Fatalf("served %+v, want only WELL_TAGGED", set.Secrets)
	}
	if !maps.Equal(set.Refused, want) {
		t.Errorf("Load refused\n%+v\nwant\n%+v", set.Refused, want)
	}
	if logged.String() != "" {
		t.Fatalf("Load logged; the state-change lines are Current's:\n%s", logged)
	}

	if _, err := policy.NewCurrent(t.Context(), loader, time.Hour); err != nil {
		t.Fatalf("NewCurrent: %v", err)
	}
	var lines strings.Builder
	for _, id := range slices.Sorted(maps.Keys(want)) {
		lines.WriteString(refusedLine(id, want[id].Reason, want[id].Owner, true, ""))
	}
	lines.WriteString(noProfilesLine)
	if logged.String() != lines.String() {
		t.Errorf("the first load logged:\n%s\nwant one line for each refused secret, in the order of their names, then the profiles line:\n%s", logged, lines.String())
	}
}

// TestReloadServesASecretOnceItIsGivenAValue pins that a secret created and tagged without a value
// is left out of the live policy, and that the reload after its value is put serves it, with no
// restart. The first load writes its refusal once, marked first_load, the reloads that find it
// still without a value write nothing, and the reload that serves it writes the one line that
// says so (now=servable). Its synctest bubble waits for the reload goroutine to return.
func TestReloadServesASecretOnceItIsGivenAValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := memory.New(policytest.Secret("SEEDED_LATER", owner, policy.TierHuman, ""))
		logged := policytest.CaptureLog(t)
		cur, err := policy.NewCurrent(t.Context(), policytest.Loader(store), 10*time.Millisecond)
		if err != nil {
			t.Fatalf("NewCurrent: %v", err)
		}
		if _, served := cur.Get().Secrets["SEEDED_LATER"]; served {
			t.Fatalf("served SEEDED_LATER before it had a value: %+v", cur.Get().Secrets)
		}
		refused := refusedLine(policytest.ID("SEEDED_LATER"), policy.ReasonNoCurrentValue, owner, true, "") + noProfilesLine
		time.Sleep(time.Second) // a hundred reloads that find it as the first load did
		synctest.Wait()
		if logged.String() != refused {
			t.Fatalf("logged:\n%s\nwant the first load's lines alone:\n%s", logged, refused)
		}

		store.Put(policytest.Secret("SEEDED_LATER", owner, policy.TierHuman, "seeded-v1"))
		await(t, func() bool { return cur.Get().Secrets["SEEDED_LATER"].Name == "SEEDED_LATER" })
		if want := refused + servableLine(policytest.ID("SEEDED_LATER"), policy.ReasonNoCurrentValue, owner, "servable"); logged.String() != want {
			t.Fatalf("logged:\n%s\nwant:\n%s", logged, want)
		}
	})
}

// failingSecrets fails List once failing is set, with the error AWS Secrets Manager answers a
// caller over its rate, as the AWS store words a failed listing.
type failingSecrets struct {
	*memory.Store
	mu      sync.Mutex
	failing bool
}

func (f *failingSecrets) List(ctx context.Context, prefix string, opts secretstore.ReadOptions) ([]secretstore.Listing, error) {
	f.mu.Lock()
	failing := f.failing
	f.mu.Unlock()
	if failing {
		return nil, fmt.Errorf("list secrets under %s: %w", prefix, errors.New("ThrottlingException: Rate exceeded"))
	}
	return f.Store.List(ctx, prefix, opts)
}

// TestReloadKeepsThePolicyWhenSecretsManagerFails pins the live policy across reloads: a tag edit
// takes effect on the next tick, a reload that cannot reach Secrets Manager logs the exact
// LoadFailedMessage line and keeps the previous policy, and a first load that fails fails the
// broker's boot. Its synctest bubble waits for the reload goroutine to return, so no failed
// reload logs into a later test's capture.
func TestReloadKeepsThePolicyWhenSecretsManagerFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &failingSecrets{Store: memory.New(policytest.Secret("DEEL_API_KEY", owner, policy.TierAgent, "v"))}
		loader := policy.Loader{Store: store, Prefix: policytest.Prefix}
		logged := policytest.CaptureLog(t)
		cur, err := policy.NewCurrent(t.Context(), loader, 10*time.Millisecond)
		if err != nil {
			t.Fatalf("NewCurrent: %v", err)
		}
		first := cur.Get()

		store.Put(policytest.Secret("DEEL_API_KEY", owner, policy.TierHuman, "v"))
		await(t, func() bool { return cur.Get().Secrets["DEEL_API_KEY"].Tier == policy.TierHuman })
		if cur.Get().Version == first.Version {
			t.Fatalf("a tag edit left the version at %s", first.Version)
		}

		edited := cur.Get()
		store.mu.Lock()
		store.failing = true
		store.mu.Unlock()
		const failed = "ERROR agent secret policy load failed; previous policy kept error=\"list secrets under " + policytest.Prefix + ": ThrottlingException: Rate exceeded\"\n"
		await(t, func() bool { return strings.Contains(logged.String(), failed) })
		if cur.Get() != edited {
			t.Fatalf("a failed reload replaced the policy: %+v", cur.Get())
		}

		if _, err := policy.NewCurrent(t.Context(), loader, time.Hour); err == nil {
			t.Fatal("NewCurrent with Secrets Manager failing = nil error, want the first load's failure")
		}
	})
}

func await(t *testing.T, done func() bool) {
	t.Helper()
	if !testwait.Holds(5*time.Second, 5*time.Millisecond, done) {
		t.Fatal("condition not met within 5s")
	}
}

// TestVersionNamesThePolicyNotItsValues pins that the version is a function of the served policy
// alone: a value change gives the same version, and any owner, tier or name change gives another.
// That the listing's order gives none and a source's change gives another is the AWS store's
// half, over ListSecrets (secretstore/aws, TestVersionNamesThePolicyNotItsListingOrder).
func TestVersionNamesThePolicyNotItsValues(t *testing.T) {
	load := func(ss ...memory.Secret) string {
		t.Helper()
		set, err := policytest.Loader(memory.New(ss...)).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set.Version
	}
	a := policytest.Secret("A_KEY", owner, policy.TierAgent, "v")
	b := policytest.Secret("B_KEY", policy.OwnerShared, policy.TierHuman, "v")
	base := load(a, b)
	value := b
	value.Value = "another value"
	if load(a, value) != base {
		t.Fatal("a value change gave another version; the version names the policy, not the values")
	}
	for name, changed := range map[string]memory.Secret{
		"owner": policytest.Secret("B_KEY", other, policy.TierHuman, "v"),
		"tier":  policytest.Secret("B_KEY", policy.OwnerShared, policy.TierAgent, "v"),
		"name":  policytest.Secret("C_KEY", policy.OwnerShared, policy.TierHuman, "v"),
	} {
		if load(a, changed) == base {
			t.Errorf("a changed %s left the version unchanged", name)
		}
	}
}

// TestTheProfileDigestIsInTheVersion pins the three things a profiles file may do to a
// deployment's policy version. A deployment with no profiles file gives every secret an empty
// digest; a file whose one profile serves nothing (its source is absent) leaves the version
// exactly where such a deployment has it, so the file alone ends no running grant; and a profile
// of one secret's own name, behind the kind its proxy string names, moves it, as an edit of that
// entry moves it again, which is what ends the grants that entry affects.
func TestTheProfileDigestIsInTheVersion(t *testing.T) {
	store := memory.New(
		policytest.Secret("A_KEY", owner, policy.TierAgent, "v"),
		policytest.Secret("B_KEY", policy.OwnerShared, policy.TierHuman, "v"),
	)
	served := func(profiles ...string) *policy.Set {
		t.Helper()
		if profiles == nil {
			return policytest.Current(t, store).Get()
		}
		return policytest.ProfiledCurrent(t, store, policytest.KindsFile(profiles...)).Get()
	}
	none := served()
	for _, name := range []string{"A_KEY", "B_KEY"} {
		if d := none.Secrets[name].ProfileDigest; d != "" {
			t.Errorf("a deployment with no profiles file gave %s the digest %q, want none", name, d)
		}
	}
	if unnamed := served(`{"name": "OTHER_KEY", "proxy": "example"}`); unnamed.Version != none.Version {
		t.Errorf("a profile that serves nothing moved the version to %s, want %s", unnamed.Version, none.Version)
	}
	named := served(`{"name": "B_KEY", "proxy": "example"}`)
	if got := named.Secrets["B_KEY"].ProfileDigest; got == "" {
		t.Error("B_KEY, which a profile holds, has no digest")
	}
	if named.Version == none.Version {
		t.Error("a secret a profile holds left the version unchanged")
	}
	if changed := served(`{"name": "B_KEY", "proxy": "github"}`); changed.Version == named.Version {
		t.Error("a changed entry left the version unchanged")
	}
}

// goldenFixture is a fixed namespace whose Version a full Load produced at legion main 7544b767,
// before NewSet existed. A change to the digest's bytes or order fails TestGoldenDigest, and the
// production digests (requests.rules_version rows) would shift with it.
func goldenFixture() *memory.Store {
	return memory.New(
		policytest.Secret("A0", "ada@example.com", policy.TierAgent, "v1"),
		policytest.Secret("A_B", "ada@example.com", policy.TierHuman, "v1"),
		policytest.Secret("DEEL_API_KEY", policy.OwnerShared, policy.TierHuman, "v1"),
	)
}

func TestGoldenDigest(t *testing.T) {
	const want = "252a3be479774f1163b99a7fbeeec17d523eb5e7048d54f90b8cb5e702fc16ca"
	set, err := policytest.Loader(arnSourced{goldenFixture()}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if set.Version != want {
		t.Fatalf("Version = %s, want the pre-change golden %s: the digest bytes or order changed", set.Version, want)
	}
}

// TestGoldenDigestWithTypeAndMaxGrant pins the lines of secrets that carry a max-grant or
// github-owners: a type is in the Source (TypeSource), and the two tags are a fifth field in
// time.Duration's own form (0s for none) and a sixth (empty for none). The expected value is the
// SHA-256 of the lines printed by hand, so it holds the bytes, not just their stability:
// printf 'A_B\tada@example.com\thuman\t<arn>/a-b\nA0\t…\tagent\t<arn>/a0\n
// DEEL_API_KEY\tshared\thuman\t<arn>/deel-api-key\n
// GOLDEN_APP\tada@example.com\tagent\tmint:github-app:<arn>/golden-app\t0s\tacme ada\n
// GOLDEN_TYPE\tada@example.com\tagent\tmint:google-login:<arn>/golden-type\t1h0m0s\t\n' | sha256sum,
// with <arn> arn:aws:secretsmanager:local:000000000000:secret:example/agent-secrets.
func TestGoldenDigestWithTypeAndMaxGrant(t *testing.T) {
	store := goldenFixture()
	secretType := policytest.TypeSecret("GOLDEN_TYPE", "ada@example.com", policy.TierAgent, policy.TypeGoogleLogin, "v1")
	secretType.Tags[policy.TagMaxGrant] = "1h"
	store.Put(secretType)
	app := policytest.TypeSecret("GOLDEN_APP", "ada@example.com", policy.TierAgent, policy.TypeGitHubApp, "v1")
	app.Tags[policy.TagGitHubOwners] = "Ada  acme"
	store.Put(app)
	const want = "2baba37d254f41d900fc8e07b45a97c870e2049d9ee96ecaef7c20fab84af71d"
	set, err := policytest.Loader(arnSourced{store}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if set.Version != want {
		t.Fatalf("Version = %s, want the type golden %s", set.Version, want)
	}
}

// TestGoldenDigestWithScopesAndAdmin pins the lines of secrets that carry a scopes or an admin
// tag: NewSet ends such a line, after the requesters list, with "\tscopes=<list>" and then
// "\tadmin=true". A google-service-account secret carrying neither writes the bytes it wrote
// before the two tags existed, so no deployed secret's digest shifts when a release adds them,
// and each tag gives the secret a line, and a version, of its own. The expected values are the
// SHA-256 of the lines printed by hand, so they hold the bytes and not just their stability:
// printf 'A_B\tada@example.com\thuman\t<arn>/a-b\nA0\t…\tagent\t<arn>/a0\n
// DEEL_API_KEY\tshared\thuman\t<arn>/deel-api-key\n
// GOLDEN_ACCOUNT\tada@example.com\thuman\tmint:google-service-account:<arn>/golden-account
// \t1h0m0s\t\trequesters=ada@example.com<tail>\n' | sha256sum, with <arn>
// arn:aws:secretsmanager:local:000000000000:secret:example/agent-secrets and <tail> empty,
// "\tscopes=admin.directory.group apps.groups.settings" or "\tadmin=true".
func TestGoldenDigestWithScopesAndAdmin(t *testing.T) {
	version := func(edit func(*memory.Secret)) string {
		t.Helper()
		store := goldenFixture()
		account := policytest.TypeSecret("GOLDEN_ACCOUNT", "ada@example.com", policy.TierHuman,
			policy.TypeGoogleServiceAccount, `{"service_account": "releases@example-project.iam.gserviceaccount.com"}`)
		edit(&account)
		store.Put(account)
		set, err := policytest.Loader(arnSourced{store}).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set.Version
	}
	// The scopes tag is written unsorted and double-spaced, so the pinned bytes are the one
	// canonical form ParseScopes answers and not the tag as a person typed it.
	for name, tc := range map[string]struct {
		want string
		edit func(*memory.Secret)
	}{
		"neither tag": {"e918b72687a566e455b7d59585df0e5ac6edd7529ac1d9822ff7b8a7115c20e8", func(*memory.Secret) {}},
		"scopes": {"04a5ed5dd44be3bfa0f6bf2b28d9035ad8976b3a48cac2899d2dce10e4922130", func(s *memory.Secret) {
			s.Tags[policy.TagScopes] = "apps.groups.settings  admin.directory.group"
		}},
		"admin": {"3327086f9c271078a77058b80d6ac88623de5ae6f0ced1bc36bad6a737518b24", func(s *memory.Secret) {
			s.Tags[policy.TagAdmin] = "true"
		}},
	} {
		if got := version(tc.edit); got != tc.want {
			t.Errorf("Version with %s = %s, want the golden %s", name, got, tc.want)
		}
	}
}

// TestALeftoverShapeTagIsIgnored: a shape tag is no tag of this broker's. A secret a writer left
// one on loads exactly as it would without it, whatever word the tag holds — a word the tag once
// took and a word it never did alike — and the namespace's version is the version of the same
// namespace with the tags removed, so no leftover tag moves a deployment's policy version.
func TestALeftoverShapeTagIsIgnored(t *testing.T) {
	load := func(leftover map[string]string) *policy.Set {
		t.Helper()
		store := goldenFixture()
		for name, word := range leftover {
			s := policytest.Secret(name, policy.OwnerShared, policy.TierHuman, "v1")
			if word != "" {
				s.Tags["shape"] = word
			}
			store.Put(s)
		}
		set, err := policytest.Loader(store).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set
	}
	tagged := load(map[string]string{"DEEL_API_KEY": "token", "SECOND_KEY": "bogus"})
	for _, name := range []string{"DEEL_API_KEY", "SECOND_KEY"} {
		if _, served := tagged.Secrets[name]; !served {
			t.Errorf("%s carrying a leftover shape tag is not served; refused %+v", name, tagged.Refused)
		}
	}
	if plain := load(map[string]string{"DEEL_API_KEY": "", "SECOND_KEY": ""}); tagged.Version != plain.Version {
		t.Errorf("the version with leftover shape tags is %s, want the version without them, %s", tagged.Version, plain.Version)
	}
}

// TestMergedSetVersionEqualsAFullLoads pins the digest contract: building the same contents by a
// full Load and by single-name merges gives byte-equal Versions — including names whose slug order
// differs from their Name order ("A_B" vs "A0": '-' < '0' but '_' > '0').
func TestMergedSetVersionEqualsAFullLoads(t *testing.T) {
	store := memory.New(policytest.Secret("A0", "ada@example.com", policy.TierAgent, "v1"))
	cur := policytest.Current(t, store)
	store.Put(policytest.Secret("A_B", "ada@example.com", policy.TierAgent, "v1"))
	if lk, err := cur.RefreshOne(context.Background(), "A_B"); err != nil || !lk.Served {
		t.Fatalf("RefreshOne(A_B) = %+v, %v", lk, err)
	}
	merged := cur.Get().Version
	full, err := policytest.Loader(store).Load(context.Background())
	if err != nil || full.Version != merged {
		t.Fatalf("merged Version %s != full load's %s (%v)", merged, full.Version, err)
	}
}

// TestLoadOneAnswersOneNameByLoadsRules pins LoadOne against Load for the same secret: served, or
// refused with the reason and the owner Load hands to the set for it (the owner empty for a
// reason that is the owner tag's own), or absent - none exists, or it is scheduled for deletion -
// which is no refusal. Like Load it logs nothing: the lines are Current's. A name no secret can
// carry is ErrNameInvalid.
func TestLoadOneAnswersOneNameByLoadsRules(t *testing.T) {
	deleting := policytest.Secret("DELETING", owner, policy.TierAgent, "v")
	deleting.DeletedAt = new(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC))
	noOwner := policytest.Secret("NO_OWNER", owner, policy.TierAgent, "v")
	delete(noOwner.Tags, policy.TagOwner)
	store := memory.New(
		policytest.Secret("SERVED", owner, policy.TierHuman, "v"),
		policytest.Secret("ADMIN_TIER", owner, "admin", "v"),
		noOwner,
		deleting,
	)
	loader := policytest.Loader(store)
	logged := policytest.CaptureLog(t)
	ctx := context.Background()
	set, err := loader.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, listed := set.Secrets["DELETING"]; listed {
		t.Fatalf("Load served a secret scheduled for deletion: %+v", set.Secrets)
	}

	lk, err := loader.LoadOne(ctx, "SERVED")
	if s := lk.Secret; err != nil || !lk.Served || lk.Reason != "" || s.Name != "SERVED" || s.Owner != owner || s.Tier != policy.TierHuman || s.Source != policytest.Source("SERVED") {
		t.Fatalf("LoadOne(SERVED) = %+v, %v; want it served, %s's, human, read by its ARN", lk, err, owner)
	}
	for name, want := range map[string]policy.Refusal{
		"ADMIN_TIER": {Reason: policy.ReasonTierTagMalformed, Owner: owner},
		"NO_OWNER":   {Reason: policy.ReasonOwnerTagMissing},
		"MISSING":    {Reason: policy.ReasonAbsent},
		"DELETING":   {Reason: policy.ReasonAbsent},
	} {
		lk, err := loader.LoadOne(ctx, name)
		if err != nil || lk.Served || lk.Reason != want.Reason || lk.Owner != want.Owner {
			t.Errorf("LoadOne(%s) = %+v, %v; want unserved for %s, owner %q", name, lk, err, want.Reason, want.Owner)
		}
		if loaded, refused := set.Refused[policytest.ID(name)]; refused != (want.Reason != policy.ReasonAbsent) || refused && loaded != want {
			t.Errorf("Load refused %s as %+v (%t), want the refusal LoadOne answers, %+v", name, loaded, refused, want)
		}
	}
	if logged.String() != "" {
		t.Errorf("LoadOne and Load logged %q, want nothing: the lines are Current's", logged.String())
	}
	if _, err := loader.LoadOne(ctx, "serv-ed"); !errors.Is(err, policy.ErrNameInvalid) {
		t.Errorf("LoadOne(serv-ed) = %v, want ErrNameInvalid", err)
	}
}

// TestLoadOneMakesNoCallForAnIDPastSecretsManagersNameLimit pins the 512-character limit on a
// secret's name: a request name whose id under the prefix is exactly that long is described as any
// other, and one a character longer is ErrNameInvalid with no DescribeSecret made, since Secrets
// Manager refuses a longer SecretId as invalid input rather than answering it not found.
func TestLoadOneMakesNoCallForAnIDPastSecretsManagersNameLimit(t *testing.T) {
	store := memory.New()
	count := &secretstoretest.Counting{Store: store}
	loader := policytest.Loader(count)
	ctx := context.Background()

	atLimit := "A" + strings.Repeat("B", 512-len(policytest.Prefix)-1)
	if lk, err := loader.LoadOne(ctx, atLimit); err != nil || lk.Served || lk.Reason != policy.ReasonAbsent || count.Describes.Load() != 1 {
		t.Fatalf("LoadOne(a name whose id is 512 characters) = %+v, %v after %d DescribeSecret calls; want absent after one", lk, err, count.Describes.Load())
	}
	if lk, err := loader.LoadOne(ctx, atLimit+"C"); !errors.Is(err, policy.ErrNameInvalid) || count.Describes.Load() != 1 {
		t.Fatalf("LoadOne(a name whose id is 513 characters) = %+v, %v after %d DescribeSecret calls; want ErrNameInvalid with no further call", lk, err, count.Describes.Load())
	}
}

// samplesOf is every sample of metric the registry serves, one per line in the order the scrape
// writes them.
func samplesOf(t *testing.T, registry *metrics.Registry, metric string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	var samples strings.Builder
	for line := range strings.Lines(recorder.Body.String()) {
		if strings.HasPrefix(line, metric+"{") {
			samples.WriteString(line)
		}
	}
	return samples.String()
}

// TestLoadCountsEachStoreReadByWhatItRead pins broker_policy_store_reads_total, the observable
// for "this pod read the store": each List a Load makes is one read="list", each Describe a
// LoadOne makes is one read="describe" whatever it found (served or absent), a List the store
// fails is counted all the same, since it was asked of the store, and a name no secret can carry
// makes no call and counts nothing.
func TestLoadCountsEachStoreReadByWhatItRead(t *testing.T) {
	store := &failingSecrets{Store: memory.New(policytest.Secret("SERVED", owner, policy.TierAgent, "v"))}
	registry := metrics.New()
	loader := policy.Loader{Store: store, Prefix: policytest.Prefix, StoreReads: policy.NewStoreReads(registry)}
	ctx := context.Background()

	for _, step := range []struct {
		name string
		read func() error
		want string
	}{
		{"a whole-namespace load", func() error { _, err := loader.Load(ctx); return err },
			`broker_policy_store_reads_total{read="list"} 1` + "\n"},
		{"a reread of a secret the store serves", func() error { _, err := loader.LoadOne(ctx, "SERVED"); return err },
			`broker_policy_store_reads_total{read="describe"} 1` + "\n" + `broker_policy_store_reads_total{read="list"} 1` + "\n"},
		{"a reread of a name the store does not hold", func() error { _, err := loader.LoadOne(ctx, "MISSING"); return err },
			`broker_policy_store_reads_total{read="describe"} 2` + "\n" + `broker_policy_store_reads_total{read="list"} 1` + "\n"},
		{"a name no secret can carry", func() error {
			if _, err := loader.LoadOne(ctx, "serv-ed"); !errors.Is(err, policy.ErrNameInvalid) {
				return fmt.Errorf("LoadOne(serv-ed) = %v, want ErrNameInvalid", err)
			}
			return nil
		}, `broker_policy_store_reads_total{read="describe"} 2` + "\n" + `broker_policy_store_reads_total{read="list"} 1` + "\n"},
		{"a load the store fails", func() error {
			store.mu.Lock()
			store.failing = true
			store.mu.Unlock()
			if _, err := loader.Load(ctx); err == nil {
				return errors.New("Load with the store failing = nil error, want the List's failure")
			}
			return nil
		}, `broker_policy_store_reads_total{read="describe"} 2` + "\n" + `broker_policy_store_reads_total{read="list"} 2` + "\n"},
	} {
		if err := step.read(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := samplesOf(t, registry, "broker_policy_store_reads_total"); got != step.want {
			t.Fatalf("after %s the counter reads\n%s\nwant\n%s", step.name, got, step.want)
		}
	}
}

// TestRequestersLoad pins what the loader makes of a requesters tag, each refusal handed to the
// set with its own reason and its owner: a list it cannot read at all
// is requesters-malformed, an entry naming a service this deployment does not register included;
// a google-service-account secret whose grants are unbounded, or bounded past an hour, is
// max-grant-required, the cap its type demands; and one whose requesters are not its own person
// owner is requesters-not-owner, a missing list included. The well-tagged secret beside them is
// served, so the rules refuse these secrets and not every secret. A secret that merely lists its
// requesters is held to no cap of its own: SERVED_KEY carries one, but dropping it would leave
// the secret served.
func TestRequestersLoad(t *testing.T) {
	const (
		person = "person@example.com"
		other  = "other@example.com"
		daemon = "legion-daemon"
	)
	type refusal struct {
		secret memory.Secret
		reason string
	}
	refusals := map[string]refusal{}
	refuse := func(name, reason string, edit func(*memory.Secret)) {
		s := policytest.ListedSecret(name, person, policy.TierAgent, person, "v")
		edit(&s)
		refusals[name] = refusal{s, reason}
	}
	refuseType := func(name, reason string, edit func(*memory.Secret)) {
		s := policytest.TypeSecret(name, person, policy.TierAgent, policy.TypeGoogleServiceAccount,
			`{"service_account": "releases@example-project.iam.gserviceaccount.com"}`)
		edit(&s)
		refusals[name] = refusal{s, reason}
	}
	refuse("COMMA_LIST", policy.ReasonRequestersMalformed, func(s *memory.Secret) {
		s.Tags[policy.TagRequesters] = person + "," + daemon
	})
	refuse("EMPTY_LIST", policy.ReasonRequestersMalformed, func(s *memory.Secret) {
		s.Tags[policy.TagRequesters] = "  "
	})
	refuse("CASED_EMAIL", policy.ReasonRequestersMalformed, func(s *memory.Secret) {
		s.Tags[policy.TagRequesters] = "Person@Example.com"
	})
	refuse("UNKNOWN_SERVICE", policy.ReasonRequestersMalformed, func(s *memory.Secret) {
		s.Tags[policy.TagRequesters] = "dispatch"
	})
	refuseType("ACCOUNT_NO_MAX_GRANT", policy.ReasonMaxGrantRequired, func(s *memory.Secret) {
		delete(s.Tags, policy.TagMaxGrant)
	})
	refuseType("ACCOUNT_LONG_MAX_GRANT", policy.ReasonMaxGrantRequired, func(s *memory.Secret) {
		s.Tags[policy.TagMaxGrant] = "2h"
	})
	refuseType("ACCOUNT_NO_LIST", policy.ReasonRequestersNotOwner, func(s *memory.Secret) {
		delete(s.Tags, policy.TagRequesters)
	})
	refuseType("ACCOUNT_TWO_PEOPLE", policy.ReasonRequestersNotOwner, func(s *memory.Secret) {
		s.Tags[policy.TagRequesters] = person + " " + other
	})
	refuseType("ACCOUNT_ANOTHER_PERSON", policy.ReasonRequestersNotOwner, func(s *memory.Secret) {
		s.Tags[policy.TagRequesters] = other
	})

	served := policytest.ListedSecret("SERVED_KEY", person, policy.TierAgent, person, "v")
	store := memory.New(served)
	for _, r := range refusals {
		store.Put(r.secret)
	}

	set, err := policytest.Loader(store, daemon).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != 1 || set.Secrets["SERVED_KEY"].Requesters != person {
		t.Fatalf("served %+v, want only SERVED_KEY, listing %s", set.Secrets, person)
	}
	want := map[string]policy.Refusal{}
	for name, r := range refusals {
		want[policytest.ID(name)] = policy.Refusal{Reason: r.reason, Owner: person}
	}
	if !maps.Equal(set.Refused, want) {
		t.Errorf("refused\n%+v\nwant each refusal with its own reason\n%+v", set.Refused, want)
	}

	// The same tag read by a loader that registers no service at all, which is what the broker
	// has when BROKER_SERVICES is unset: an entry naming a service is refused
	// requesters-malformed, exactly as an owner tag naming an unregistered service is refused,
	// since no pod can prove a name the deployment does not register.
	unregistered := policytest.ListedSecret("SERVICE_LIST_KEY", person, policy.TierAgent, daemon, "v")
	bare, err := policytest.Loader(memory.New(unregistered)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load with no registered services: %v", err)
	}
	if len(bare.Secrets) != 0 {
		t.Errorf("a loader registering no service served %+v, want the list naming %s refused", bare.Secrets, daemon)
	}
	if want := (policy.Refusal{Reason: policy.ReasonRequestersMalformed, Owner: person}); bare.Refused[policytest.ID("SERVICE_LIST_KEY")] != want || len(bare.Refused) != 1 {
		t.Errorf("refused %+v, want SERVICE_LIST_KEY alone, as %+v", bare.Refused, want)
	}
}

// TestAdminCap pins every rule the admin tag and the scopes tag bring, both ways, driven through
// the loader as TestRequestersLoad is: the refusal a row reads is the one the load hands to the
// set, not TagRules' answer called directly. An admin secret is human tier, names its
// requesters and caps its grants at an hour, so every release of it is approved, asked for by
// someone it names and short enough to review while it is live; a secret that merely lists its
// requesters keeps its own cap, or none, whatever its tier. A google-service-account secret
// carries that same hour's cap, is asked for by its one person owner, and may name registered
// services beside that owner only at human tier and only where the deployment binds it to an
// account that is not one person's own dev-<name> one. The scopes tag is read on that type alone.
func TestAdminCap(t *testing.T) {
	const (
		person       = "person@example.com"
		other        = "other@example.com"
		daemon       = "legion-daemon"
		fleetAccount = "releases@example-project.iam.gserviceaccount.com"
		devAccount   = "dev-person@example-agent-identities.iam.gserviceaccount.com"
		accountValue = `{"service_account": "releases@example-project.iam.gserviceaccount.com"}`
	)
	// An admin secret as the rules want it (ListedSecret's max-grant is an hour), a secret that
	// only lists its requesters, and a google-service-account one, each before a row's edit.
	adminSecret := func(name, tier string, edit func(*memory.Secret)) memory.Secret {
		s := policytest.ListedSecret(name, person, tier, person, "v")
		s.Tags[policy.TagAdmin] = "true"
		edit(&s)
		return s
	}
	loginSecret := func(name, tier string, edit func(*memory.Secret)) memory.Secret {
		s := policytest.TypeSecret(name, person, tier, policy.TypeGoogleLogin, "v")
		s.Tags[policy.TagRequesters] = person
		edit(&s)
		return s
	}
	accountSecret := func(name, tier string, edit func(*memory.Secret)) memory.Secret {
		s := policytest.TypeSecret(name, person, tier, policy.TypeGoogleServiceAccount, accountValue)
		edit(&s)
		return s
	}
	unedited := func(*memory.Secret) {}
	// The name a session asks for the fixture by, which the refusals are keyed under.
	nameOf := func(s memory.Secret) string {
		return policy.SlugToName(strings.TrimPrefix(s.Name, policytest.Prefix))
	}

	served := []memory.Secret{
		adminSecret("ADMIN_KEY", policy.TierHuman, unedited),
		loginSecret("LISTED_HUMAN", policy.TierHuman, unedited),
		loginSecret("LISTED_AGENT", policy.TierAgent, unedited),
		loginSecret("LISTED_LONG_CAP", policy.TierHuman, func(s *memory.Secret) {
			s.Tags[policy.TagMaxGrant] = "12h"
		}),
		accountSecret("ACCOUNT_DAEMON", policy.TierHuman, func(s *memory.Secret) {
			s.Tags[policy.TagRequesters] = person + " " + daemon
		}),
	}
	refusals := map[string]struct {
		secret memory.Secret
		reason string
	}{}
	refuse := func(secret memory.Secret, reason string) {
		refusals[nameOf(secret)] = struct {
			secret memory.Secret
			reason string
		}{secret, reason}
	}
	refuse(adminSecret("ADMIN_NO_CAP", policy.TierHuman, func(s *memory.Secret) {
		delete(s.Tags, policy.TagMaxGrant)
	}), policy.ReasonMaxGrantRequired)
	refuse(adminSecret("ADMIN_LONG_CAP", policy.TierHuman, func(s *memory.Secret) {
		s.Tags[policy.TagMaxGrant] = "2h"
	}), policy.ReasonMaxGrantRequired)
	refuse(adminSecret("ADMIN_AGENT_TIER", policy.TierAgent, unedited), policy.ReasonAdminTier)
	refuse(adminSecret("ADMIN_NO_LIST", policy.TierHuman, func(s *memory.Secret) {
		delete(s.Tags, policy.TagRequesters)
	}), policy.ReasonAdminRequesters)
	refuse(adminSecret("ADMIN_YES", policy.TierHuman, func(s *memory.Secret) {
		s.Tags[policy.TagAdmin] = "yes"
	}), policy.ReasonAdminMalformed)
	refuse(accountSecret("ACCOUNT_NO_CAP", policy.TierHuman, func(s *memory.Secret) {
		delete(s.Tags, policy.TagMaxGrant)
	}), policy.ReasonMaxGrantRequired)
	refuse(accountSecret("ACCOUNT_SERVICE_AGENT", policy.TierAgent, func(s *memory.Secret) {
		s.Tags[policy.TagRequesters] = person + " " + daemon
	}), policy.ReasonRequestersNotOwner)
	refuse(accountSecret("ACCOUNT_TWO_PEOPLE", policy.TierHuman, func(s *memory.Secret) {
		s.Tags[policy.TagRequesters] = person + " " + other
	}), policy.ReasonRequestersNotOwner)
	// The same list ACCOUNT_DAEMON is served for, on a secret the deployment binds to one
	// person's own dev-<name> account: that token is theirs, so no service may ask for it.
	refuse(accountSecret("ACCOUNT_DEV_BOUND", policy.TierHuman, func(s *memory.Secret) {
		s.Tags[policy.TagRequesters] = person + " " + daemon
	}), policy.ReasonRequestersNotOwner)
	appScopes := policytest.TypeSecret("APP_SCOPES", person, policy.TierAgent, policy.TypeGitHubApp, "v")
	appScopes.Tags[policy.TagScopes] = "cloud-platform"
	refuse(appScopes, policy.ReasonScopesType)
	refuse(accountSecret("ACCOUNT_BAD_SCOPES", policy.TierHuman, func(s *memory.Secret) {
		s.Tags[policy.TagScopes] = "cloud-platform,bigquery"
	}), policy.ReasonScopesMalformed)

	store := memory.New(served...)
	for _, r := range refusals {
		store.Put(r.secret)
	}
	loader := policytest.Loader(store, daemon)
	loader.ServiceAccounts = map[string]string{"ACCOUNT_DAEMON": fleetAccount, "ACCOUNT_DEV_BOUND": devAccount}

	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range served {
		if _, ok := set.Secrets[nameOf(s)]; !ok {
			t.Errorf("%s was refused, want it served", nameOf(s))
		}
	}
	if len(set.Secrets) != len(served) {
		t.Errorf("served %+v, want only the %d rows the rules allow", set.Secrets, len(served))
	}
	if !set.Secrets["ADMIN_KEY"].Admin {
		t.Errorf("ADMIN_KEY = %+v, want it served as an administrator's secret", set.Secrets["ADMIN_KEY"])
	}
	want := map[string]policy.Refusal{}
	for name, r := range refusals {
		want[policytest.ID(name)] = policy.Refusal{Reason: r.reason, Owner: person}
	}
	if !maps.Equal(set.Refused, want) {
		t.Errorf("refused\n%+v\nwant each refusal with its own reason\n%+v", set.Refused, want)
	}
}

// TestNewSetDigest pins what a requesters tag does to the digest: two spellings of one list — the
// entries in another order, separated by more space — are one policy and one version, a changed
// list is another, and a namespace that carries no requesters digests to the bytes it digested
// before the tag existed, written out here, so a change that moved them fails. The expected value
// is the SHA-256 of the lines printed by hand:
// printf 'LIMITED_KEY\tada@example.com\thuman\t<arn>/limited-key\t1h0m0s\t\n
// PLAIN_KEY\tada@example.com\tagent\t<arn>/plain-key\n' | sha256sum, with <arn>
// arn:aws:secretsmanager:local:000000000000:secret:example/agent-secrets.
func TestNewSetDigest(t *testing.T) {
	const (
		person = "person@example.com"
		daemon = "legion-daemon"
	)
	version := func(requesters string) string {
		t.Helper()
		s := policytest.ListedSecret("LISTED_KEY", person, policy.TierAgent, requesters, "v1")
		set, err := policytest.Loader(arnSourced{memory.New(s)}, daemon).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set.Version
	}
	if first, again := version(daemon+" "+person), version("  "+person+"   "+daemon+" "); first != again {
		t.Errorf("two spellings of one list gave %s and %s, want one version", first, again)
	}
	if listed, narrowed := version(daemon+" "+person), version(person); listed == narrowed {
		t.Error("dropping a requester from the list left the version unchanged")
	}

	const want = "4b1e0cfefd5df408721f6fa65119a8991fadc3f4e91b46bba981f7a22de6c9c3"
	limited := policytest.Secret("LIMITED_KEY", "ada@example.com", policy.TierHuman, "v1")
	limited.Tags[policy.TagMaxGrant] = "1h"
	plain := policytest.Secret("PLAIN_KEY", "ada@example.com", policy.TierAgent, "v1")
	set, err := policytest.Loader(arnSourced{memory.New(limited, plain)}).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.Version != want {
		t.Fatalf("Version = %s, want %s: a namespace with no requesters digests the bytes it always did", set.Version, want)
	}
}
