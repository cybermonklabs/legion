package aws_test

// This file holds the loader's reads of the AWS store that are the AWS store's own: the forms
// Secrets Manager reports a secret's key in, KMS aliases, ListSecrets pages and filters, and the
// version stages of a listing. They moved here from internal/cipher/policy's load_test.go, their
// assertions unchanged, when the policy's other tests moved onto the memory store.

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// owner is the person the secrets of these tests are owned by.
const owner = "sami@example.com"

// awsLoader is the policy.Loader over the AWS store over local, on the agent-secrets key.
func awsLoader(local *awstest.Local) policy.Loader {
	return policy.Loader{Store: aws.Store{Client: local, KMS: local, KeyARN: awstest.KeyARN}, Prefix: policytest.Prefix}
}

// TestLoadServesEveryWellTaggedSecretOnTheKey pins what the loader reads from Secrets Manager: a
// secret's request name from its name under the prefix, owner and tier from its tags, and its ARN
// as the source, for every form Secrets Manager reports the agent-secrets key in — its ARN, its
// key id, and an alias that points at it by name or by ARN.
func TestLoadServesEveryWellTaggedSecretOnTheKey(t *testing.T) {
	logged := policytest.CaptureLog(t)
	keyID := awstest.KeyARN[strings.LastIndex(awstest.KeyARN, "/")+1:]
	byForm := map[string]string{
		"BY_ARN":       awstest.KeyARN,
		"BY_KEY_ID":    keyID,
		"BY_ALIAS":     "alias/agent-secrets",
		"BY_ALIAS_ARN": "arn:aws:kms:us-east-1:111122223333:alias/agent-secrets",
	}
	store := awstest.NewLocal()
	store.Alias(awstest.KeyARN, "alias/agent-secrets")
	for name, keyForm := range byForm {
		s := awstest.Secret(name, owner, policy.TierAgent, "v")
		s.KmsKeyID = keyForm
		store.Put(s)
	}
	store.Put(awstest.Secret("DEEL_API_KEY", policy.OwnerShared, policy.TierHuman, "v"))

	set, err := awsLoader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != len(byForm)+1 {
		t.Fatalf("served %d secrets, want %d: %+v", len(set.Secrets), len(byForm)+1, set.Secrets)
	}
	deel := set.Secrets["DEEL_API_KEY"]
	if deel.Name != "DEEL_API_KEY" || deel.Owner != policy.OwnerShared || deel.Tier != policy.TierHuman || deel.Source != awstest.ARN(policytest.ID("DEEL_API_KEY")) {
		t.Fatalf("DEEL_API_KEY = %+v, want shared, human, read by its ARN", deel)
	}
	if len(set.Refused) != 0 || logged.String() != "" {
		t.Fatalf("a load refusing nothing refused %+v and logged:\n%s", set.Refused, logged)
	}
}

// TestTheLoaderRefusesASecretOffTheKeyInEachForm pins, for the forms of a key that is not the
// agent-secrets key - none (the AWS managed key), another key's ARN, another key's alias - the
// refusal not-on-agent-secrets-key with the owner tag, as the policy's own refusal table holds it
// for a store that marks a secret off the key.
func TestTheLoaderRefusesASecretOffTheKeyInEachForm(t *testing.T) {
	store := awstest.NewLocal()
	store.Alias("arn:aws:kms:us-east-1:111122223333:key/another-key", "alias/someone-elses")
	forms := map[string]string{
		"MANAGED_KEY":        "",
		"ANOTHER_KEY":        "arn:aws:kms:us-east-1:111122223333:key/another-key",
		"ANOTHER_KEYS_ALIAS": "alias/someone-elses",
	}
	want := map[string]policy.Refusal{}
	for name, key := range forms {
		s := awstest.Secret(name, owner, policy.TierAgent, "v")
		s.KmsKeyID = key
		store.Put(s)
		want[policytest.ID(name)] = policy.Refusal{Reason: policy.ReasonNotOnAgentSecretsKey, Owner: owner}
	}
	set, err := awsLoader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Secrets) != 0 || !maps.Equal(set.Refused, want) {
		t.Fatalf("served %+v and refused %+v; want each refused %+v", set.Secrets, set.Refused, want)
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
	onAlias := func(name string, edit func(*awstest.LocalSecret)) awstest.LocalSecret {
		s := awstest.Secret(name, owner, policy.TierAgent, "v")
		s.KmsKeyID = "alias/agent-secrets"
		edit(&s)
		return s
	}
	deleting := onAlias("DELETING", func(s *awstest.LocalSecret) { s.DeletedAt = new(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)) })
	store := awstest.NewLocal(
		onAlias("ADMIN_TIER", func(s *awstest.LocalSecret) { s.Tags[policy.TagTier] = "admin" }),
		onAlias("NO_OWNER", func(s *awstest.LocalSecret) { delete(s.Tags, policy.TagOwner) }),
		deleting,
	)
	aliases := &failingAliases{}
	loader := policy.Loader{Store: aws.Store{Client: store, KMS: aliases, KeyARN: awstest.KeyARN}, Prefix: policytest.Prefix}
	ctx := context.Background()
	if _, err := loader.Load(ctx); err != nil || aliases.calls != 0 {
		t.Fatalf("Load = %v after %d ListAliases calls; want the refusals alone, with no lookup", err, aliases.calls)
	}
	for name, reason := range map[string]string{"ADMIN_TIER": policy.ReasonTierTagMalformed, "DELETING": policy.ReasonAbsent} {
		if lk, err := loader.LoadOne(ctx, name); err != nil || lk.Reason != reason || aliases.calls != 0 {
			t.Fatalf("LoadOne(%s) = %+v, %v after %d ListAliases calls; want %s with no lookup", name, lk, err, aliases.calls, reason)
		}
	}
	store.Put(onAlias("SERVED", func(*awstest.LocalSecret) {}))
	if _, err := loader.Load(ctx); err == nil || !strings.Contains(err.Error(), "AccessDeniedException") || aliases.calls != 1 {
		t.Fatalf("Load with a well-tagged secret on an alias = %v after %d ListAliases calls; want the lookup's failure after one", err, aliases.calls)
	}
}

// pagedSecrets answers ListSecrets one secret per page, the shape a namespace bigger than one page
// comes back in, and counts the filters it was asked for. Its other Secrets Manager calls are a
// Local's, which a Load never makes.
type pagedSecrets struct {
	*awstest.Local
	entries []smtypes.SecretListEntry
	filters [][]smtypes.Filter
}

// pagedLoader is the Loader over a pagedSecrets listing entries, with no alias to resolve.
func pagedLoader(t *testing.T, entries ...smtypes.SecretListEntry) (policy.Loader, *pagedSecrets) {
	sm := &pagedSecrets{Local: awstest.NewLocal(), entries: entries}
	return policy.Loader{Store: aws.Store{Client: sm, KMS: noAliases{t}, KeyARN: awstest.KeyARN}, Prefix: policytest.Prefix}, sm
}

func (p *pagedSecrets) ListSecrets(_ context.Context, in *secretsmanager.ListSecretsInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	p.filters = append(p.filters, in.Filters)
	i := 0
	if in.NextToken != nil {
		i = int((*in.NextToken)[0] - '0')
	}
	out := &secretsmanager.ListSecretsOutput{SecretList: p.entries[i : i+1]}
	if i+1 < len(p.entries) {
		out.NextToken = awssdk.String(string(rune('0' + i + 1)))
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
		Name: awssdk.String(name), ARN: awssdk.String(arn), KmsKeyId: awssdk.String(awstest.KeyARN),
		Tags: []smtypes.Tag{
			{Key: awssdk.String(policy.TagOwner), Value: awssdk.String(policy.OwnerShared)},
			{Key: awssdk.String(policy.TagTier), Value: awssdk.String(policy.TierAgent)},
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

// TestVersionNamesThePolicyNotItsListingOrder pins that the version is a function of the served
// policy alone: the same secrets listed in another order give the same version, and an ARN change
// gives another. Its owner, tier, name and value half is the policy's own
// (internal/cipher/policy, TestVersionNamesThePolicyNotItsValues).
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
}
