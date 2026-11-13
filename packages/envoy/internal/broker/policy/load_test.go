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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestLoadServesEveryWellTaggedSecretOnTheKey pins what the loader reads from Secrets Manager: a
// secret's request name from its name under the prefix, owner and tier from its tags, and its ARN
// as the source, for every form Secrets Manager reports the agent-secrets key in — its ARN, its
// key id, and an alias that points at it by name or by ARN.
func TestLoadServesEveryWellTaggedSecretOnTheKey(t *testing.T) {
	logged := policytest.CaptureLog(t)
	keyID := policytest.KeyARN[strings.LastIndex(policytest.KeyARN, "/")+1:]
	byForm := map[string]string{
		"BY_ARN":       policytest.KeyARN,
		"BY_KEY_ID":    keyID,
		"BY_ALIAS":     "alias/agent-secrets",
		"BY_ALIAS_ARN": "arn:aws:kms:us-east-1:111122223333:alias/agent-secrets",
	}
	store := secrets.NewLocal()
	store.Alias(policytest.KeyARN, "alias/agent-secrets")
	for name, keyForm := range byForm {
		s := policytest.Secret(name, owner, policy.TierAgent, "v")
		s.KmsKeyID = keyForm
		store.Put(s)
	}
	store.Put(policytest.Secret("DEEL_API_KEY", policy.OwnerShared, policy.TierHuman, "v"))

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != len(byForm)+1 {
		t.Fatalf("served %d secrets, want %d: %+v", len(set.Secrets), len(byForm)+1, set.Secrets)
	}
	deel := set.Secrets["DEEL_API_KEY"]
	if deel.Name != "DEEL_API_KEY" || deel.Owner != policy.OwnerShared || deel.Tier != policy.TierHuman || deel.Source != secrets.LocalARN(policytest.ID("DEEL_API_KEY")) {
		t.Fatalf("DEEL_API_KEY = %+v, want shared, human, read by its ARN", deel)
	}
	if len(set.Refused) != 0 || logged.String() != "" {
		t.Fatalf("a load refusing nothing refused %+v and logged:\n%s", set.Refused, logged)
	}
}

// failingAliases fails every ListAliases call, counting them.
type failingAliases struct{ calls int }

func (f *failingAliases) ListAliases(context.Context, *kms.ListAliasesInput, ...func(*kms.Options)) (*kms.ListAliasesOutput, error) {
	f.calls++
	return nil, errors.New("AccessDeniedException: not authorized to perform kms:ListAliases")
}

// TestARefusalForANameOrTagCostsNoAliasLookup pins that the policy asks whether a secret is on
// the agent-secrets key only after its name and tags pass: a secret that names its key by an alias
// but is refused for a malformed name or tag, or is scheduled for deletion, is refused (or absent)
// by itself without a kms:ListAliases call, so a KMS that refuses the lookup fails no load. A
// well-tagged secret on an alias still asks, and that failure fails the load.
func TestARefusalForANameOrTagCostsNoAliasLookup(t *testing.T) {
	onAlias := func(name string, edit func(*secrets.LocalSecret)) secrets.LocalSecret {
		s := policytest.Secret(name, owner, policy.TierAgent, "v")
		s.KmsKeyID = "alias/agent-secrets"
		edit(&s)
		return s
	}
	deleting := onAlias("DELETING", func(s *secrets.LocalSecret) { s.DeletedAt = new(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)) })
	store := secrets.NewLocal(
		onAlias("ADMIN_TIER", func(s *secrets.LocalSecret) { s.Tags[policy.TagTier] = "admin" }),
		onAlias("NO_OWNER", func(s *secrets.LocalSecret) { delete(s.Tags, policy.TagOwner) }),
		deleting,
	)
	aliases := &failingAliases{}
	loader := policy.Loader{Store: secrets.AWS{Client: store, KMS: aliases, KeyARN: policytest.KeyARN}, Prefix: policytest.Prefix}
	ctx := context.Background()
	if _, err := loader.Load(ctx); err != nil || aliases.calls != 0 {
		t.Fatalf("Load = %v after %d ListAliases calls; want the refusals alone, with no lookup", err, aliases.calls)
	}
	for name, reason := range map[string]string{"ADMIN_TIER": policy.ReasonTierTagMalformed, "DELETING": policy.ReasonAbsent} {
		if lk, err := loader.LoadOne(ctx, name); err != nil || lk.Reason != reason || aliases.calls != 0 {
			t.Fatalf("LoadOne(%s) = %+v, %v after %d ListAliases calls; want %s with no lookup", name, lk, err, aliases.calls, reason)
		}
	}
	store.Put(onAlias("SERVED", func(*secrets.LocalSecret) {}))
	if _, err := loader.Load(ctx); err == nil || !strings.Contains(err.Error(), "AccessDeniedException") || aliases.calls != 1 {
		t.Fatalf("Load with a well-tagged secret on an alias = %v after %d ListAliases calls; want the lookup's failure after one", err, aliases.calls)
	}
}

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
		secret secrets.LocalSecret
		want   policy.Refusal
	}{}
	refuse := func(name, reason, refusedOwner string, edit func(*secrets.LocalSecret)) {
		s := policytest.Secret(name, owner, policy.TierAgent, "v")
		edit(&s)
		refusals[name] = struct {
			secret secrets.LocalSecret
			want   policy.Refusal
		}{s, policy.Refusal{Reason: reason, Owner: refusedOwner}}
	}
	refuse("NO_OWNER", policy.ReasonOwnerTagMissing, "", func(s *secrets.LocalSecret) { delete(s.Tags, policy.TagOwner) })
	refuse("GITHUB_LOGIN_OWNER", policy.ReasonOwnerTagMalformed, "", func(s *secrets.LocalSecret) { s.Tags[policy.TagOwner] = "sjawhar" })
	refuse("CASED_OWNER", policy.ReasonOwnerTagMalformed, "", func(s *secrets.LocalSecret) { s.Tags[policy.TagOwner] = "Sami@Example.com" })
	refuse("EMPTY_OWNER", policy.ReasonOwnerTagMalformed, "", func(s *secrets.LocalSecret) { s.Tags[policy.TagOwner] = "" })
	refuse("UNREGISTERED_SERVICE", policy.ReasonOwnerTagMalformed, "", func(s *secrets.LocalSecret) { s.Tags[policy.TagOwner] = "dispatch" })
	refuse("NO_TIER", policy.ReasonTierTagMissing, owner, func(s *secrets.LocalSecret) { delete(s.Tags, policy.TagTier) })
	refuse("ADMIN_TIER", policy.ReasonTierTagMalformed, owner, func(s *secrets.LocalSecret) { s.Tags[policy.TagTier] = "admin" })
	refuse("CASED_TAG_KEY", policy.ReasonOwnerTagMissing, "", func(s *secrets.LocalSecret) {
		s.Tags["Owner"] = s.Tags[policy.TagOwner]
		delete(s.Tags, policy.TagOwner)
	})
	refuse("SERVICE_HUMAN", policy.ReasonServiceOwnerHumanTier, legion, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagOwner], s.Tags[policy.TagTier] = legion, policy.TierHuman
	})
	refuse("MANAGED_KEY", policy.ReasonNotOnAgentSecretsKey, owner, func(s *secrets.LocalSecret) { s.KmsKeyID = "" })
	refuse("ANOTHER_KEY", policy.ReasonNotOnAgentSecretsKey, owner, func(s *secrets.LocalSecret) {
		s.KmsKeyID = "arn:aws:kms:us-east-1:111122223333:key/another-key"
	})
	refuse("ANOTHER_KEYS_ALIAS", policy.ReasonNotOnAgentSecretsKey, owner, func(s *secrets.LocalSecret) { s.KmsKeyID = "alias/someone-elses" })
	refuse("NO_VALUE", policy.ReasonNoCurrentValue, owner, func(s *secrets.LocalSecret) { s.Value = "" })
	refuse("NO_TIER_NOR_VALUE", policy.ReasonTierTagMissing, owner, func(s *secrets.LocalSecret) {
		delete(s.Tags, policy.TagTier)
		s.Value = ""
	})
	refuse("MANAGED_KEY_NO_VALUE", policy.ReasonNotOnAgentSecretsKey, owner, func(s *secrets.LocalSecret) {
		s.KmsKeyID = ""
		s.Value = ""
	})
	refuse("BOGUS_SHAPE", policy.ReasonShapeMalformed, owner, func(s *secrets.LocalSecret) { s.Tags[policy.TagShape] = "bogus" })

	wellTagged := policytest.Secret("WELL_TAGGED", owner, policy.TierAgent, "v")
	wellTagged.Tags[policy.TagShape] = policy.ShapeDocument
	store := secrets.NewLocal(wellTagged)
	store.Alias("arn:aws:kms:us-east-1:111122223333:key/another-key", "alias/someone-elses")
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
	if got := set.Secrets["WELL_TAGGED"].Shape; got != policy.ShapeDocument {
		t.Errorf("WELL_TAGGED's Shape = %q, want the %s tag the store holds", got, policy.ShapeDocument)
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
	if logged.String() != lines.String() {
		t.Errorf("the first load logged:\n%s\nwant one line for each refused secret, in the order of their names:\n%s", logged, lines.String())
	}
}

// pagedSecrets answers ListSecrets one secret per page, the shape a namespace bigger than one page
// comes back in, and counts the filters it was asked for. Its other Secrets Manager calls are a
// Local's, which a Load never makes.
type pagedSecrets struct {
	*secrets.Local
	entries []smtypes.SecretListEntry
	filters [][]smtypes.Filter
}

// pagedLoader is the Loader over a pagedSecrets listing entries, with no alias to resolve.
func pagedLoader(t *testing.T, entries ...smtypes.SecretListEntry) (policy.Loader, *pagedSecrets) {
	sm := &pagedSecrets{Local: secrets.NewLocal(), entries: entries}
	return policy.Loader{Store: secrets.AWS{Client: sm, KMS: noAliases{t}, KeyARN: policytest.KeyARN}, Prefix: policytest.Prefix}, sm
}

func (p *pagedSecrets) ListSecrets(_ context.Context, in *secretsmanager.ListSecretsInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	p.filters = append(p.filters, in.Filters)
	i := 0
	if in.NextToken != nil {
		i = int((*in.NextToken)[0] - '0')
	}
	out := &secretsmanager.ListSecretsOutput{SecretList: p.entries[i : i+1]}
	if i+1 < len(p.entries) {
		out.NextToken = aws.String(string(rune('0' + i + 1)))
	}
	return out, nil
}

// noAliases fails a test that resolves an alias it never needed.
type noAliases struct{ t *testing.T }

func (n noAliases) ListAliases(context.Context, *kms.ListAliasesInput, ...func(*kms.Options)) (*kms.ListAliasesOutput, error) {
	n.t.Error("ListAliases called, but no secret names its key by an alias")
	return &kms.ListAliasesOutput{}, nil
}

// onlyCurrent is a listing's SecretVersionsToStages for a secret holding a value: one version,
// labelled AWSCURRENT.
var onlyCurrent = map[string][]string{"v1": {"AWSCURRENT"}}

// listEntry is the ListSecrets entry for the secret named name (its whole Secrets Manager name),
// shared, agent tier, on the agent-secrets key, with its ARN and its versions' stages.
func listEntry(name, arn string, versionsToStages map[string][]string) smtypes.SecretListEntry {
	return smtypes.SecretListEntry{
		Name: aws.String(name), ARN: aws.String(arn), KmsKeyId: aws.String(policytest.KeyARN),
		Tags: []smtypes.Tag{
			{Key: aws.String(policy.TagOwner), Value: aws.String(policy.OwnerShared)},
			{Key: aws.String(policy.TagTier), Value: aws.String(policy.TierAgent)},
		},
		SecretVersionsToStages: versionsToStages,
	}
}

// TestLoadReadsEveryPageOfThePrefixOnly pins that the loader asks Secrets Manager for the
// namespace prefix alone, reads every page, skips a name the lister answered outside the namespace
// (here the prefix in upper case: the namespace is the prefix as written) without refusing it (it
// is not in the namespace, so no line and no broker_unservable_secrets series is its), and never
// resolves aliases nobody used.
func TestLoadReadsEveryPageOfThePrefixOnly(t *testing.T) {
	entry := func(name string) smtypes.SecretListEntry { return listEntry(name, "arn:"+name, onlyCurrent) }
	loader, sm := pagedLoader(t,
		entry(policytest.Prefix+"first"), entry(policytest.Prefix+"second"),
		entry(strings.ToUpper(policytest.Prefix)+"third"), entry(policytest.Prefix+"fourth"),
	)
	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != 3 || set.Secrets["FIRST"].Source != "arn:"+policytest.Prefix+"first" || set.Secrets["FOURTH"].Name != "FOURTH" {
		t.Fatalf("served %+v, want FIRST, SECOND and FOURTH", set.Secrets)
	}
	if len(set.Refused) != 0 {
		t.Fatalf("a name outside the namespace was refused rather than skipped: %+v", set.Refused)
	}
	if len(sm.filters) != 4 {
		t.Fatalf("asked for %d pages, want 4", len(sm.filters))
	}
	for _, f := range sm.filters {
		if len(f) != 1 || f[0].Key != smtypes.FilterNameStringTypeName || len(f[0].Values) != 1 || f[0].Values[0] != policytest.Prefix {
			t.Fatalf("ListSecrets filters = %+v, want name=%s alone", f, policytest.Prefix)
		}
	}
}

// TestLoadServesASecretOnlyWhileAVersionIsCurrent pins the value check on what ListSecrets reports,
// with no call per secret: a secret is served when one of its versions carries AWSCURRENT, the
// label GetSecretValue reads, mid-rotation included, and refused with no-current-value when none
// does: created without a value, or holding only a pending or a previous version.
func TestLoadServesASecretOnlyWhileAVersionIsCurrent(t *testing.T) {
	entry := func(name string, versionsToStages map[string][]string) smtypes.SecretListEntry {
		return listEntry(policytest.ID(name), "arn:"+name, versionsToStages)
	}
	loader, _ := pagedLoader(t,
		entry("CURRENT", onlyCurrent),
		entry("ROTATING", map[string][]string{"v1": {"AWSPREVIOUS"}, "v2": {"AWSCURRENT"}, "v3": {"AWSPENDING"}}),
		entry("NEVER_GIVEN_A_VALUE", nil),
		entry("NO_STAGES", map[string][]string{}),
		entry("PENDING_ONLY", map[string][]string{"v1": {"AWSPENDING"}}),
		entry("PREVIOUS_ONLY", map[string][]string{"v1": {"AWSPREVIOUS"}}),
	)
	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, current := set.Secrets["CURRENT"]; !current || len(set.Secrets) != 2 || set.Secrets["ROTATING"].Name != "ROTATING" {
		t.Fatalf("served %+v, want CURRENT and ROTATING alone", set.Secrets)
	}
	noValue := policy.Refusal{Reason: policy.ReasonNoCurrentValue, Owner: policy.OwnerShared}
	want := map[string]policy.Refusal{
		policytest.ID("NEVER_GIVEN_A_VALUE"): noValue,
		policytest.ID("NO_STAGES"):           noValue,
		policytest.ID("PENDING_ONLY"):        noValue,
		policytest.ID("PREVIOUS_ONLY"):       noValue,
	}
	if !maps.Equal(set.Refused, want) {
		t.Fatalf("refused %+v, want %+v", set.Refused, want)
	}
}

// TestReloadServesASecretOnceItIsGivenAValue pins that a secret created and tagged without a value
// is left out of the live policy, and that the reload after its value is put serves it, with no
// restart. The first load writes its refusal once, marked first_load, the reloads that find it
// still without a value write nothing, and the reload that serves it writes the one line that
// says so (now=servable). Its synctest bubble waits for the reload goroutine to return.
func TestReloadServesASecretOnceItIsGivenAValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := secrets.NewLocal(policytest.Secret("SEEDED_LATER", owner, policy.TierHuman, ""))
		logged := policytest.CaptureLog(t)
		cur, err := policy.NewCurrent(t.Context(), policytest.Loader(store), 10*time.Millisecond)
		if err != nil {
			t.Fatalf("NewCurrent: %v", err)
		}
		if _, served := cur.Get().Secrets["SEEDED_LATER"]; served {
			t.Fatalf("served SEEDED_LATER before it had a value: %+v", cur.Get().Secrets)
		}
		refused := refusedLine(policytest.ID("SEEDED_LATER"), policy.ReasonNoCurrentValue, owner, true, "")
		time.Sleep(time.Second) // a hundred reloads that find it as the first load did
		synctest.Wait()
		if logged.String() != refused {
			t.Fatalf("logged:\n%s\nwant the first load's line alone:\n%s", logged, refused)
		}

		store.Put(policytest.Secret("SEEDED_LATER", owner, policy.TierHuman, "seeded-v1"))
		await(t, func() bool { return cur.Get().Secrets["SEEDED_LATER"].Name == "SEEDED_LATER" })
		if want := refused + servableLine(policytest.ID("SEEDED_LATER"), policy.ReasonNoCurrentValue, owner, "servable"); logged.String() != want {
			t.Fatalf("logged:\n%s\nwant:\n%s", logged, want)
		}
	})
}

// failingSecrets fails ListSecrets once failing is set.
type failingSecrets struct {
	*secrets.Local
	mu      sync.Mutex
	failing bool
}

func (f *failingSecrets) ListSecrets(ctx context.Context, in *secretsmanager.ListSecretsInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	f.mu.Lock()
	failing := f.failing
	f.mu.Unlock()
	if failing {
		return nil, errors.New("ThrottlingException: Rate exceeded")
	}
	return f.Local.ListSecrets(ctx, in, opts...)
}

// TestReloadKeepsThePolicyWhenSecretsManagerFails pins the live policy across reloads: a tag edit
// takes effect on the next tick, a reload that cannot reach Secrets Manager logs the exact
// LoadFailedMessage line and keeps the previous policy, and a first load that fails fails the
// broker's boot. Its synctest bubble waits for the reload goroutine to return, so no failed
// reload logs into a later test's capture.
func TestReloadKeepsThePolicyWhenSecretsManagerFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &failingSecrets{Local: secrets.NewLocal(policytest.Secret("DEEL_API_KEY", owner, policy.TierAgent, "v"))}
		loader := policy.Loader{Store: policytest.StoreThrough(store.Local, store), Prefix: policytest.Prefix}
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

// TestVersionNamesThePolicyNotItsListingOrder pins that the version is a function of the served
// policy alone: the same secrets listed in another order give the same version, and any owner,
// tier, name or ARN change gives another.
func TestVersionNamesThePolicyNotItsListingOrder(t *testing.T) {
	// Secrets Manager promises no listing order, so these listings come back in the order given.
	listed := func(entries ...smtypes.SecretListEntry) string {
		t.Helper()
		loader, _ := pagedLoader(t, entries...)
		set, err := loader.Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set.Version
	}
	first, second := listEntry(policytest.Prefix+"a-key", "arn:a-key-1", onlyCurrent), listEntry(policytest.Prefix+"b-key", "arn:b-key-1", onlyCurrent)
	if listed(first, second) != listed(second, first) {
		t.Fatal("the same secrets in another order gave another version")
	}
	if listed(first, listEntry(policytest.Prefix+"b-key", "arn:b-key-2", onlyCurrent)) == listed(first, second) {
		t.Error("a secret created again under its name, with a new ARN, left the version unchanged")
	}

	load := func(ss ...secrets.LocalSecret) string {
		t.Helper()
		set, err := policytest.Loader(secrets.NewLocal(ss...)).Load(context.Background())
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
	for name, changed := range map[string]secrets.LocalSecret{
		"owner": policytest.Secret("B_KEY", other, policy.TierHuman, "v"),
		"tier":  policytest.Secret("B_KEY", policy.OwnerShared, policy.TierAgent, "v"),
		"name":  policytest.Secret("C_KEY", policy.OwnerShared, policy.TierHuman, "v"),
	} {
		if load(a, changed) == base {
			t.Errorf("a changed %s left the version unchanged", name)
		}
	}
}

// TestTheAirlockDigestIsInTheVersion pins the three things a rules file may do to a deployment's
// policy version. A loader with no AirlockDigest — a deployment with no rules file — gives every
// secret an empty digest; a file naming no served secret leaves the version exactly where such a
// deployment has it, so this release does not end a running deployment's grants; and a change to
// one secret's entry moves it, which is what ends the grants that entry affects.
func TestTheAirlockDigestIsInTheVersion(t *testing.T) {
	store := secrets.NewLocal(
		policytest.Secret("A_KEY", owner, policy.TierAgent, "v"),
		policytest.Secret("B_KEY", policy.OwnerShared, policy.TierHuman, "v"),
	)
	load := func(digests map[string]string) *policy.Set {
		t.Helper()
		loader := policytest.Loader(store)
		if digests != nil {
			loader.AirlockDigest = func(name string) string { return digests[name] }
		}
		set, err := loader.Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set
	}
	none := load(nil)
	for _, name := range []string{"A_KEY", "B_KEY"} {
		if d := none.Secrets[name].AirlockDigest; d != "" {
			t.Errorf("a loader with no AirlockDigest gave %s the digest %q, want none", name, d)
		}
	}
	if unnamed := load(map[string]string{"OTHER_KEY": "d1"}); unnamed.Version != none.Version {
		t.Errorf("rules naming no served secret moved the version to %s, want %s", unnamed.Version, none.Version)
	}
	named := load(map[string]string{"B_KEY": "d1"})
	if got := named.Secrets["B_KEY"].AirlockDigest; got != "d1" {
		t.Errorf("B_KEY's digest = %q, want d1", got)
	}
	if named.Version == none.Version {
		t.Error("a secret the rules name left the version unchanged")
	}
	if changed := load(map[string]string{"B_KEY": "d2"}); changed.Version == named.Version {
		t.Error("a changed entry left the version unchanged")
	}
}

// goldenFixture is a fixed namespace whose Version a full Load produced at legion main 7544b767,
// before NewSet existed. A change to the digest's bytes or order fails TestGoldenDigest, and the
// production digests (requests.rules_version rows) would shift with it.
func goldenFixture() *secrets.Local {
	return secrets.NewLocal(
		policytest.Secret("A0", "ada@example.com", policy.TierAgent, "v1"),
		policytest.Secret("A_B", "ada@example.com", policy.TierHuman, "v1"),
		policytest.Secret("DEEL_API_KEY", policy.OwnerShared, policy.TierHuman, "v1"),
	)
}

func TestGoldenDigest(t *testing.T) {
	const want = "252a3be479774f1163b99a7fbeeec17d523eb5e7048d54f90b8cb5e702fc16ca"
	set, err := policytest.Loader(goldenFixture()).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if set.Version != want {
		t.Fatalf("Version = %s, want the pre-change golden %s: the digest bytes or order changed", set.Version, want)
	}
}

// TestGoldenDigestWithKindAndMaxGrant pins the lines of secrets that carry a max-grant or
// github-owners: a kind is in the Source (KindSource), and the two tags are a fifth field in
// time.Duration's own form (0s for none) and a sixth (empty for none). The expected value is the
// SHA-256 of the lines printed by hand, so it holds the bytes, not just their stability:
// printf 'A_B\tada@example.com\thuman\t<arn>/a-b\nA0\t…\tagent\t<arn>/a0\n
// DEEL_API_KEY\tshared\thuman\t<arn>/deel-api-key\n
// GOLDEN_APP\tada@example.com\tagent\tmint:github-app:<arn>/golden-app\t0s\tacme ada\n
// GOLDEN_KIND\tada@example.com\tagent\tmint:google-login:<arn>/golden-kind\t1h0m0s\t\n' | sha256sum,
// with <arn> arn:aws:secretsmanager:local:000000000000:secret:example/agent-secrets.
func TestGoldenDigestWithKindAndMaxGrant(t *testing.T) {
	store := goldenFixture()
	kind := policytest.KindSecret("GOLDEN_KIND", "ada@example.com", policy.TierAgent, policy.KindGoogleLogin, "v1")
	kind.Tags[policy.TagMaxGrant] = "1h"
	store.Put(kind)
	app := policytest.KindSecret("GOLDEN_APP", "ada@example.com", policy.TierAgent, policy.KindGitHubApp, "v1")
	app.Tags[policy.TagGitHubOwners] = "Ada  acme"
	store.Put(app)
	const want = "88c77e2ce85b8174fcfaa88ce25c7905ae902d12bc5584cf82cb3089aa7c364e"
	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if set.Version != want {
		t.Fatalf("Version = %s, want the kind golden %s", set.Version, want)
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
	version := func(edit func(*secrets.LocalSecret)) string {
		t.Helper()
		store := goldenFixture()
		account := policytest.KindSecret("GOLDEN_ACCOUNT", "ada@example.com", policy.TierHuman,
			policy.KindGoogleServiceAccount, `{"service_account": "releases@example-project.iam.gserviceaccount.com"}`)
		edit(&account)
		store.Put(account)
		set, err := policytest.Loader(store).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set.Version
	}
	// The scopes tag is written unsorted and double-spaced, so the pinned bytes are the one
	// canonical form ParseScopes answers and not the tag as a person typed it.
	for name, tc := range map[string]struct {
		want string
		edit func(*secrets.LocalSecret)
	}{
		"neither tag": {"e918b72687a566e455b7d59585df0e5ac6edd7529ac1d9822ff7b8a7115c20e8", func(*secrets.LocalSecret) {}},
		"scopes": {"04a5ed5dd44be3bfa0f6bf2b28d9035ad8976b3a48cac2899d2dce10e4922130", func(s *secrets.LocalSecret) {
			s.Tags[policy.TagScopes] = "apps.groups.settings  admin.directory.group"
		}},
		"admin": {"3327086f9c271078a77058b80d6ac88623de5ae6f0ced1bc36bad6a737518b24", func(s *secrets.LocalSecret) {
			s.Tags[policy.TagAdmin] = "true"
		}},
	} {
		if got := version(tc.edit); got != tc.want {
			t.Errorf("Version with %s = %s, want the golden %s", name, got, tc.want)
		}
	}
}

// TestTheShapeTagIsInThePolicyVersion pins what the shape tag does to the digest: a secret
// carrying the tag writes a version of its own, and each word writes its own. That is what
// carries a retag to the airlock proxy, which holds a grant's released values per pod and reads
// them again on no event but a name's own reread or the policy version moving
// (internal/broker/airlockproxy/bound.go), so a pod would otherwise go on spending the untrimmed
// value. The other half, that a namespace tagging no shape digests the bytes it digested before
// the tag existed, is TestGoldenDigest's, which pins this same fixture's version.
func TestTheShapeTagIsInThePolicyVersion(t *testing.T) {
	version := func(shape string) string {
		t.Helper()
		store := goldenFixture()
		if shape != "" {
			deel := policytest.Secret("DEEL_API_KEY", policy.OwnerShared, policy.TierHuman, "v1")
			deel.Tags[policy.TagShape] = shape
			store.Put(deel)
		}
		set, err := policytest.Loader(store).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := set.Secrets["DEEL_API_KEY"].Shape; got != shape {
			t.Fatalf("DEEL_API_KEY's Shape = %q, want %q", got, shape)
		}
		return set.Version
	}
	untagged, token, document := version(""), version(policy.ShapeToken), version(policy.ShapeDocument)
	if token == untagged {
		t.Error("tagging a secret shape=token left the version where an untagged namespace has it, so no retag would reach a pod already holding the values")
	}
	if token == document {
		t.Error("shape=token and shape=document gave one version, so the digest holds the tag's presence and not its word")
	}
	if document == untagged {
		t.Error("tagging a secret shape=document left the version where an untagged namespace has it, so no retag would reach a pod already holding the values")
	}
}

// TestMergedSetVersionEqualsAFullLoads pins the digest contract: building the same contents by a
// full Load and by single-name merges gives byte-equal Versions — including names whose slug order
// differs from their Name order ("A_B" vs "A0": '-' < '0' but '_' > '0').
func TestMergedSetVersionEqualsAFullLoads(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("A0", "ada@example.com", policy.TierAgent, "v1"))
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
	store := secrets.NewLocal(
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
	if s := lk.Secret; err != nil || !lk.Served || lk.Reason != "" || s.Name != "SERVED" || s.Owner != owner || s.Tier != policy.TierHuman || s.Source != secrets.LocalARN(policytest.ID("SERVED")) {
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
	store := secrets.NewLocal()
	count := &policytest.CountingDescriber{SecretsManagerAPI: store}
	loader := policytest.Loader(store)
	loader.Store = policytest.StoreThrough(store, count)
	ctx := context.Background()

	atLimit := "A" + strings.Repeat("B", 512-len(policytest.Prefix)-1)
	if lk, err := loader.LoadOne(ctx, atLimit); err != nil || lk.Served || lk.Reason != policy.ReasonAbsent || count.Calls.Load() != 1 {
		t.Fatalf("LoadOne(a name whose id is 512 characters) = %+v, %v after %d DescribeSecret calls; want absent after one", lk, err, count.Calls.Load())
	}
	if lk, err := loader.LoadOne(ctx, atLimit+"C"); !errors.Is(err, policy.ErrNameInvalid) || count.Calls.Load() != 1 {
		t.Fatalf("LoadOne(a name whose id is 513 characters) = %+v, %v after %d DescribeSecret calls; want ErrNameInvalid with no further call", lk, err, count.Calls.Load())
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
	store := &failingSecrets{Local: secrets.NewLocal(policytest.Secret("SERVED", owner, policy.TierAgent, "v"))}
	registry := metrics.New()
	loader := policy.Loader{Store: policytest.StoreThrough(store.Local, store), Prefix: policytest.Prefix, StoreReads: policy.NewStoreReads(registry)}
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
// max-grant-required, the cap its kind demands; and one whose requesters are not its own person
// owner is requesters-not-owner, a missing list included. The well-tagged secret beside them is
// served, so the rules refuse these secrets and not every secret. A secret that merely lists its
// requesters is held to no cap of its own: SERVED_KEY carries one, but dropping it would leave
// the secret served.
func TestRequestersLoad(t *testing.T) {
	const (
		sami   = "sami@theorem.dev"
		jason  = "jason@theorem.dev"
		daemon = "legion-daemon"
	)
	type refusal struct {
		secret secrets.LocalSecret
		reason string
	}
	refusals := map[string]refusal{}
	refuse := func(name, reason string, edit func(*secrets.LocalSecret)) {
		s := policytest.ListedSecret(name, sami, policy.TierAgent, sami, "v")
		edit(&s)
		refusals[name] = refusal{s, reason}
	}
	refuseKind := func(name, reason string, edit func(*secrets.LocalSecret)) {
		s := policytest.KindSecret(name, sami, policy.TierAgent, policy.KindGoogleServiceAccount,
			`{"service_account": "releases@example-project.iam.gserviceaccount.com"}`)
		edit(&s)
		refusals[name] = refusal{s, reason}
	}
	refuse("COMMA_LIST", policy.ReasonRequestersMalformed, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagRequesters] = sami + "," + daemon
	})
	refuse("EMPTY_LIST", policy.ReasonRequestersMalformed, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagRequesters] = "  "
	})
	refuse("CASED_EMAIL", policy.ReasonRequestersMalformed, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagRequesters] = "Sami@Theorem.dev"
	})
	refuse("UNKNOWN_SERVICE", policy.ReasonRequestersMalformed, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagRequesters] = "dispatch"
	})
	refuseKind("ACCOUNT_NO_MAX_GRANT", policy.ReasonMaxGrantRequired, func(s *secrets.LocalSecret) {
		delete(s.Tags, policy.TagMaxGrant)
	})
	refuseKind("ACCOUNT_LONG_MAX_GRANT", policy.ReasonMaxGrantRequired, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagMaxGrant] = "2h"
	})
	refuseKind("ACCOUNT_NO_LIST", policy.ReasonRequestersNotOwner, func(s *secrets.LocalSecret) {
		delete(s.Tags, policy.TagRequesters)
	})
	refuseKind("ACCOUNT_TWO_PEOPLE", policy.ReasonRequestersNotOwner, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagRequesters] = sami + " " + jason
	})
	refuseKind("ACCOUNT_ANOTHER_PERSON", policy.ReasonRequestersNotOwner, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagRequesters] = jason
	})

	served := policytest.ListedSecret("SERVED_KEY", sami, policy.TierAgent, sami, "v")
	store := secrets.NewLocal(served)
	for _, r := range refusals {
		store.Put(r.secret)
	}

	set, err := policytest.Loader(store, daemon).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != 1 || set.Secrets["SERVED_KEY"].Requesters != sami {
		t.Fatalf("served %+v, want only SERVED_KEY, listing %s", set.Secrets, sami)
	}
	want := map[string]policy.Refusal{}
	for name, r := range refusals {
		want[policytest.ID(name)] = policy.Refusal{Reason: r.reason, Owner: sami}
	}
	if !maps.Equal(set.Refused, want) {
		t.Errorf("refused\n%+v\nwant each refusal with its own reason\n%+v", set.Refused, want)
	}

	// The same tag read by a loader that registers no service at all, which is what the broker
	// has when BROKER_SERVICES is unset: an entry naming a service is refused
	// requesters-malformed, exactly as an owner tag naming an unregistered service is refused,
	// since no pod can prove a name the deployment does not register.
	unregistered := policytest.ListedSecret("SERVICE_LIST_KEY", sami, policy.TierAgent, daemon, "v")
	bare, err := policytest.Loader(secrets.NewLocal(unregistered)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load with no registered services: %v", err)
	}
	if len(bare.Secrets) != 0 {
		t.Errorf("a loader registering no service served %+v, want the list naming %s refused", bare.Secrets, daemon)
	}
	if want := (policy.Refusal{Reason: policy.ReasonRequestersMalformed, Owner: sami}); bare.Refused[policytest.ID("SERVICE_LIST_KEY")] != want || len(bare.Refused) != 1 {
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
// account that is not one person's own dev-<name> one. The scopes tag is read on that kind alone.
func TestAdminCap(t *testing.T) {
	const (
		sami         = "sami@theorem.dev"
		jason        = "jason@theorem.dev"
		daemon       = "legion-daemon"
		fleetAccount = "releases@example-project.iam.gserviceaccount.com"
		devAccount   = "dev-sami@theorem-agent-identities.iam.gserviceaccount.com"
		accountValue = `{"service_account": "releases@example-project.iam.gserviceaccount.com"}`
	)
	// An admin secret as the rules want it (ListedSecret's max-grant is an hour), a secret that
	// only lists its requesters, and a google-service-account one, each before a row's edit.
	adminSecret := func(name, tier string, edit func(*secrets.LocalSecret)) secrets.LocalSecret {
		s := policytest.ListedSecret(name, sami, tier, sami, "v")
		s.Tags[policy.TagAdmin] = "true"
		edit(&s)
		return s
	}
	loginSecret := func(name, tier string, edit func(*secrets.LocalSecret)) secrets.LocalSecret {
		s := policytest.KindSecret(name, sami, tier, policy.KindGoogleLogin, "v")
		s.Tags[policy.TagRequesters] = sami
		edit(&s)
		return s
	}
	accountSecret := func(name, tier string, edit func(*secrets.LocalSecret)) secrets.LocalSecret {
		s := policytest.KindSecret(name, sami, tier, policy.KindGoogleServiceAccount, accountValue)
		edit(&s)
		return s
	}
	unedited := func(*secrets.LocalSecret) {}
	// The name a session asks for the fixture by, which the refusals are keyed under.
	nameOf := func(s secrets.LocalSecret) string {
		return policy.SlugToName(strings.TrimPrefix(s.Name, policytest.Prefix))
	}

	served := []secrets.LocalSecret{
		adminSecret("ADMIN_KEY", policy.TierHuman, unedited),
		loginSecret("LISTED_HUMAN", policy.TierHuman, unedited),
		loginSecret("LISTED_AGENT", policy.TierAgent, unedited),
		loginSecret("LISTED_LONG_CAP", policy.TierHuman, func(s *secrets.LocalSecret) {
			s.Tags[policy.TagMaxGrant] = "12h"
		}),
		accountSecret("ACCOUNT_DAEMON", policy.TierHuman, func(s *secrets.LocalSecret) {
			s.Tags[policy.TagRequesters] = sami + " " + daemon
		}),
	}
	refusals := map[string]struct {
		secret secrets.LocalSecret
		reason string
	}{}
	refuse := func(secret secrets.LocalSecret, reason string) {
		refusals[nameOf(secret)] = struct {
			secret secrets.LocalSecret
			reason string
		}{secret, reason}
	}
	refuse(adminSecret("ADMIN_NO_CAP", policy.TierHuman, func(s *secrets.LocalSecret) {
		delete(s.Tags, policy.TagMaxGrant)
	}), policy.ReasonMaxGrantRequired)
	refuse(adminSecret("ADMIN_LONG_CAP", policy.TierHuman, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagMaxGrant] = "2h"
	}), policy.ReasonMaxGrantRequired)
	refuse(adminSecret("ADMIN_AGENT_TIER", policy.TierAgent, unedited), policy.ReasonAdminTier)
	refuse(adminSecret("ADMIN_NO_LIST", policy.TierHuman, func(s *secrets.LocalSecret) {
		delete(s.Tags, policy.TagRequesters)
	}), policy.ReasonAdminRequesters)
	refuse(adminSecret("ADMIN_YES", policy.TierHuman, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagAdmin] = "yes"
	}), policy.ReasonAdminMalformed)
	refuse(accountSecret("ACCOUNT_NO_CAP", policy.TierHuman, func(s *secrets.LocalSecret) {
		delete(s.Tags, policy.TagMaxGrant)
	}), policy.ReasonMaxGrantRequired)
	refuse(accountSecret("ACCOUNT_SERVICE_AGENT", policy.TierAgent, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagRequesters] = sami + " " + daemon
	}), policy.ReasonRequestersNotOwner)
	refuse(accountSecret("ACCOUNT_TWO_PEOPLE", policy.TierHuman, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagRequesters] = sami + " " + jason
	}), policy.ReasonRequestersNotOwner)
	// The same list ACCOUNT_DAEMON is served for, on a secret the deployment binds to one
	// person's own dev-<name> account: that token is theirs, so no service may ask for it.
	refuse(accountSecret("ACCOUNT_DEV_BOUND", policy.TierHuman, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagRequesters] = sami + " " + daemon
	}), policy.ReasonRequestersNotOwner)
	appScopes := policytest.KindSecret("APP_SCOPES", sami, policy.TierAgent, policy.KindGitHubApp, "v")
	appScopes.Tags[policy.TagScopes] = "cloud-platform"
	refuse(appScopes, policy.ReasonScopesKind)
	refuse(accountSecret("ACCOUNT_BAD_SCOPES", policy.TierHuman, func(s *secrets.LocalSecret) {
		s.Tags[policy.TagScopes] = "cloud-platform,bigquery"
	}), policy.ReasonScopesMalformed)

	store := secrets.NewLocal(served...)
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
		want[policytest.ID(name)] = policy.Refusal{Reason: r.reason, Owner: sami}
	}
	if !maps.Equal(set.Refused, want) {
		t.Errorf("refused\n%+v\nwant each refusal with its own reason\n%+v", set.Refused, want)
	}
}

// TestTagRulesRefusesAShapeThatIsNeitherWord is the shape tag's own rule, weighed by calling
// TagRules rather than by driving a load as the two tests above do: the tag says what the stored
// value is, so the broker takes the two words it knows and the absent tag, and refuses every
// other value — another casing of a word it knows among them — as ReasonShapeMalformed rather
// than reading it as one of them. The line a load writes for such a secret is
// TestLoadRefusesEachUnservableSecretByNameWithTheExactLine's BOGUS_SHAPE row; this is the rule
// that line comes of, and the one `agent-secrets secret` applies before it writes a tag.
func TestTagRulesRefusesAShapeThatIsNeitherWord(t *testing.T) {
	for _, c := range []struct{ shape, reason string }{
		{"", ""},
		{policy.ShapeToken, ""},
		{policy.ShapeDocument, ""},
		{"Token", policy.ReasonShapeMalformed},
		{"DOCUMENT", policy.ReasonShapeMalformed},
		{"documents", policy.ReasonShapeMalformed},
		{"one-line", policy.ReasonShapeMalformed},
		{" ", policy.ReasonShapeMalformed},
	} {
		// Every other tag of the set is one the rules allow, so the answer is the shape's alone.
		set := policy.TagSet{Owner: owner, Tier: policy.TierAgent, Shape: c.shape}
		if got := policy.TagRules(set); got != c.reason {
			t.Errorf("TagRules(shape=%q) = %q, want %q", c.shape, got, c.reason)
		}
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
		sami   = "sami@theorem.dev"
		daemon = "legion-daemon"
	)
	version := func(requesters string) string {
		t.Helper()
		s := policytest.ListedSecret("LISTED_KEY", sami, policy.TierAgent, requesters, "v1")
		set, err := policytest.Loader(secrets.NewLocal(s), daemon).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set.Version
	}
	if first, again := version(daemon+" "+sami), version("  "+sami+"   "+daemon+" "); first != again {
		t.Errorf("two spellings of one list gave %s and %s, want one version", first, again)
	}
	if listed, narrowed := version(daemon+" "+sami), version(sami); listed == narrowed {
		t.Error("dropping a requester from the list left the version unchanged")
	}

	const want = "4b1e0cfefd5df408721f6fa65119a8991fadc3f4e91b46bba981f7a22de6c9c3"
	limited := policytest.Secret("LIMITED_KEY", "ada@example.com", policy.TierHuman, "v1")
	limited.Tags[policy.TagMaxGrant] = "1h"
	plain := policytest.Secret("PLAIN_KEY", "ada@example.com", policy.TierAgent, "v1")
	set, err := policytest.Loader(secrets.NewLocal(limited, plain)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.Version != want {
		t.Fatalf("Version = %s, want %s: a namespace with no requesters digests the bytes it always did", set.Version, want)
	}
}
