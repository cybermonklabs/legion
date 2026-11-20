package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestSecretTypeAndMaxGrantOnAWS retags one secret through the types. Each retag to a type reads
// the value and decodes it as that type, so the value that type decodes is stored first, as a
// writer other than this client stores it (a retag between two types cannot be preceded by a
// set, which decodes the value under the type the secret holds).
func TestSecretTypeAndMaxGrantOnAWS(t *testing.T) {
	local := secrets.NewLocal()
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	useStdin(t, policytest.TypeValue(policy.TypeGitHubApp))

	stdout, stderr, code := runSecret("create", "APP_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGitHubApp, "--github-owners", "Acme-Org", "--requesters", "ada@example.com", "--max-grant", "1h")
	if code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags)
	if tags[policy.TagType] != policy.TypeGitHubApp || tags[policy.TagMaxGrant] != "1h" || tags[policy.TagGitHubOwners] != "acme-org" {
		t.Fatalf("create tags = %v, want type, github-owners as the broker reads them, and max-grant", tags)
	}
	if tags[policy.TagRequesters] != "ada@example.com" {
		t.Fatalf("create tags = %v, want the requesters list", tags)
	}
	if tags[policy.TagRollbackGuard] != policy.RollbackGuardValue {
		t.Fatalf("create tags = %v, want the rollback guard beside the type", tags)
	}
	if !strings.Contains(stdout, "broker: serving APP_KEY") {
		t.Fatalf("create stdout %q does not name the broker result", stdout)
	}

	stdout, stderr, code = runSecret("show", "APP_KEY")
	if code != 0 || !strings.Contains(stdout, "type: github-app") || !strings.Contains(stdout, "github_owners: acme-org") || !strings.Contains(stdout, "max_grant: 1h") {
		t.Fatalf("show: exit %d stdout %q stderr %q; want type, github owners and max-grant", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "requesters: ada@example.com") {
		t.Fatalf("show: stdout %q does not name the requesters", stdout)
	}
	stdout, stderr, code = runSecret("list")
	if code != 0 || !strings.Contains(stdout, "TYPE") || !strings.Contains(stdout, "MAX_GRANT") || !strings.Contains(stdout, "github-app") || !strings.Contains(stdout, "1h") {
		t.Fatalf("list: exit %d stdout %q stderr %q; want type and max-grant", code, stdout, stderr)
	}
	stdout, stderr, code = runSecret("list", "--json")
	var listed []map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &listed) != nil || len(listed) != 1 {
		t.Fatalf("list --json: exit %d stdout %q stderr %q; want one secret", code, stdout, stderr)
	}
	if got := listed[0]; got["type"] != policy.TypeGitHubApp || got["github_owners"] != "acme-org" || got["max_grant"] != "1h" {
		t.Fatalf("list --json = %v; want type github-app, github_owners acme-org and max_grant 1h", got)
	}

	for _, secretType := range []string{policy.TypeGoogleLogin, policy.TypeGitHubApp, policy.TypeAzureLogin} {
		if _, err := local.PutSecretValue(context.Background(), &secretsmanager.PutSecretValueInput{SecretId: aws.String(policytest.ID("APP_KEY")), SecretString: aws.String(policytest.TypeValue(secretType))}); err != nil {
			t.Fatal(err)
		}
		_, stderr, code = runSecret("retag", "APP_KEY", "--type", secretType)
		if code != 0 {
			t.Fatalf("retag %s: exit %d, stderr %q", secretType, code, stderr)
		}
		tags = secrets.TagMap(describe(t, local, "APP_KEY").Tags)
		if tags[policy.TagType] != secretType || tags[policy.TagMaxGrant] != "1h" || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent || tags[policy.TagRollbackGuard] != policy.RollbackGuardValue {
			t.Fatalf("retag %s tags = %v, want new type, the rollback guard and retained tags", secretType, tags)
		}
	}
	_, stderr, code = runSecret("retag", "APP_KEY", "--type", "none")
	if code != 0 {
		t.Fatalf("retag none: exit %d, stderr %q", code, stderr)
	}
	tags = secrets.TagMap(describe(t, local, "APP_KEY").Tags)
	if _, ok := tags[policy.TagType]; ok || tags[policy.TagMaxGrant] != "1h" {
		t.Fatalf("retag none tags = %v, want removed type and retained max-grant", tags)
	}
	if _, ok := tags[policy.TagRollbackGuard]; ok {
		t.Fatalf("retag none tags = %v, want the rollback guard removed with the type", tags)
	}
}

// TestSecretTypeAndMaxGrantOnGCP retags one secret through every type, each value stored before
// the retag that decodes it, as TestSecretTypeAndMaxGrantOnAWS does.
func TestSecretTypeAndMaxGrantOnGCP(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	settings := gcpTestSettings()
	startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
	useStdin(t, policytest.TypeValue(policy.TypeGoogleLogin))

	_, stderr, code := runSecret("create", "LOGIN_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleLogin, "--max-grant", "30m")
	if code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	id := testGCPPrefix + "login-key"
	fake.mu.Lock()
	annotations := fake.secrets[id].secret.GetAnnotations()
	fake.mu.Unlock()
	if annotations[policy.TagType] != policy.TypeGoogleLogin || annotations[policy.TagMaxGrant] != "30m" || annotations[policy.TagRollbackGuard] != policy.RollbackGuardValue {
		t.Fatalf("create annotations = %v, want type, max-grant and the rollback guard", annotations)
	}
	if _, ok := annotations[policy.TagGitHubOwners]; ok {
		t.Fatalf("create annotations = %v, want no github-owners on a google-login secret", annotations)
	}

	stdout, stderr, code := runSecret("show", "LOGIN_KEY")
	if code != 0 || !strings.Contains(stdout, "type: google-login") || !strings.Contains(stdout, "max_grant: 30m") {
		t.Fatalf("show: exit %d stdout %q stderr %q; want type and max-grant", code, stdout, stderr)
	}
	stdout, stderr, code = runSecret("list")
	if code != 0 || !strings.Contains(stdout, "TYPE") || !strings.Contains(stdout, "MAX_GRANT") || !strings.Contains(stdout, "google-login") || !strings.Contains(stdout, "30m") {
		t.Fatalf("list: exit %d stdout %q stderr %q; want type and max-grant", code, stdout, stderr)
	}
	for _, secretType := range policy.Types {
		// The type's value as a new version, added as a writer other than this client adds one.
		if _, err := fake.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{Parent: id, Payload: &secretmanagerpb.SecretPayload{Data: []byte(policytest.TypeValue(secretType))}}); err != nil {
			t.Fatal(err)
		}
		args := []string{"retag", "LOGIN_KEY", "--type", secretType}
		if policy.TypeNeedsGitHubOwners(secretType) {
			args = append(args, "--github-owners", "Ada  acme-org")
		}
		if secretType == policy.TypeGoogleServiceAccount {
			// The type is served to the one person who owns the secret, and such a secret bounds
			// its grants to an hour (policy.TagRules), so the retag names both, the max grant as
			// the secret already holds it.
			args = append(args, "--requesters", "ada@example.com", "--max-grant", "30m")
		}
		if _, stderr, code = runSecret(args...); code != 0 {
			t.Fatalf("retag %s: exit %d, stderr %q", secretType, code, stderr)
		}
		fake.mu.Lock()
		annotations = fake.secrets[id].secret.GetAnnotations()
		fake.mu.Unlock()
		if annotations[policy.TagType] != secretType || annotations[policy.TagMaxGrant] != "30m" || annotations[policy.TagOwner] != "ada@example.com" || annotations[policy.TagTier] != policy.TierAgent {
			t.Fatalf("retag %s annotations = %v, want new type and retained annotations", secretType, annotations)
		}
	}
	fake.mu.Lock()
	if got := fake.secrets[id].secret.GetAnnotations()[policy.TagGitHubOwners]; got != "acme-org ada" {
		t.Errorf("github-owners after the round trip = %q, want the canonical space-separated list: acme-org ada", got)
	}
	fake.mu.Unlock()
	if _, stderr, code = runSecret("retag", "LOGIN_KEY", "--type", "none"); code != 0 {
		t.Fatalf("retag none: exit %d, stderr %q", code, stderr)
	}
	fake.mu.Lock()
	annotations = fake.secrets[id].secret.GetAnnotations()
	fake.mu.Unlock()
	if _, ok := annotations[policy.TagType]; ok || annotations[policy.TagMaxGrant] != "30m" {
		t.Fatalf("retag none annotations = %v, want removed type and retained max-grant", annotations)
	}
	if _, ok := annotations[policy.TagRollbackGuard]; ok {
		t.Fatalf("retag none annotations = %v, want the rollback guard removed with the type", annotations)
	}
}

func TestSecretTypeFlagsRejectInvalidValues(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "value"))
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	useStdin(t, "stored-value")
	for _, args := range [][]string{
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", "other"},
		{"retag", "HELD_KEY", "--type", "other"},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--max-grant", "soon"},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--max-grant", "500ms"},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGitHubApp},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGitHubApp, "--github-owners", "  "},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleLogin, "--github-owners", "ada"},
		{"retag", "HELD_KEY", "--github-owners", "  "},
		{"retag", "HELD_KEY", "--github-owners", "none", "--tier", "human"},
	} {
		_, stderr, code := runSecret(args...)
		if code != exitUsageError {
			t.Fatalf("%v: exit %d stderr %q; want usage error", args, code, stderr)
		}
	}
	// The requesters list and the rules it brings with it, each refused before anything is
	// written: the broker's own reading of the list (policy.ParseRequesters), and its cross-tag
	// rules (policy.TagRules), named as the broker names them. The rows below the first three
	// pin that the client's refusal names the rule that refused the write and not another that
	// happens to be true beside it: the check reads the tags the write leaves, so --type none
	// leaves a stored secret, and its arms follow policy.RequesterRule's fault, which weighs
	// the requesters conditions most fundamental first, so a secret owned by shared is answered
	// for its owner however its list and tier read, a list naming a service at agent tier is
	// answered for the tier, and the same list at human tier for the person it leaves out.
	const other = "jason@example.com"
	for _, tc := range []struct {
		want, absent string
		args         []string
	}{
		{want: "spaces, not commas", args: []string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--requesters", "ada@example.com,builder", "--max-grant", "1h"}},
		{want: "at least one person or service", args: []string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--requesters", "  ", "--max-grant", "1h"}},
		{want: "lowercase sign-in email", args: []string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--requesters", "Ada@Example.com", "--max-grant", "1h"}},
		{want: policy.ReasonRequestersNotOwner, args: []string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleServiceAccount, "--max-grant", "1h", "--requesters", "builder"}},
		{want: policy.ReasonRequestersNotOwner, args: []string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleServiceAccount, "--max-grant", "1h"}},
		// none removes the type tag, so what the write leaves is a stored secret, which is what
		// the refusal says.
		{want: "a stored secret holds none", absent: "a " + tagNone + " secret", args: []string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", tagNone, "--scopes", "bigquery"}},
		// One list, two tiers. At agent tier the service is what TagRules refuses, so the client
		// names the tier and not the person; at human tier the service is allowed and the person
		// who is not the owner is what refuses, so the client names them and not the tier.
		{want: "only at " + policy.TierHuman + " tier", absent: "once, not", args: []string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleServiceAccount, "--max-grant", "1h", "--requesters", other + " builder"}},
		{want: "name ada@example.com once, not", absent: "only at " + policy.TierHuman + " tier", args: []string{"create", "NEW_KEY", "--owner", "me", "--tier", policy.TierHuman, "--type", policy.TypeGoogleServiceAccount, "--max-grant", "1h", "--requesters", other + " builder"}},
		// The same list, pinned on the advice: a human-tier secret of this type may keep the
		// registered services its list names, so the write offered puts the owner beside
		// builder rather than dropping it.
		{want: `--requesters "ada@example.com builder"`, args: []string{"create", "NEW_KEY", "--owner", "me", "--tier", policy.TierHuman, "--type", policy.TypeGoogleServiceAccount, "--max-grant", "1h", "--requesters", other + " builder"}},
		// A secret owned by shared can carry this type under no list and no tier, so that is
		// what the refusal names, and not the service beside it.
		{want: "is no person", absent: "only at " + policy.TierHuman + " tier", args: []string{"create", "NEW_KEY", "--owner", "shared", "--tier", "agent", "--type", policy.TypeGoogleServiceAccount, "--max-grant", "1h", "--requesters", "builder"}},
		// A human-tier list of services alone: the owner goes beside them, never in their place.
		{want: `--requesters "ada@example.com builder"`, args: []string{"create", "NEW_KEY", "--owner", "me", "--tier", policy.TierHuman, "--type", policy.TypeGoogleServiceAccount, "--max-grant", "1h", "--requesters", "builder"}},
	} {
		_, stderr, code := runSecret(tc.args...)
		if code != exitUsageError || !strings.Contains(stderr, tc.want) {
			t.Fatalf("%v: exit %d stderr %q; want a usage error naming %q", tc.args, code, stderr, tc.want)
		}
		if tc.absent != "" && strings.Contains(stderr, tc.absent) {
			t.Fatalf("%v: stderr %q names %q, which is not the rule that refused it", tc.args, stderr, tc.absent)
		}
	}
	if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
		t.Fatal("a refused create made NEW_KEY")
	}
}

// TestSecretCreateRefusesBadAdminAndScopes: --admin takes the one word the broker reads as an
// administrator's secret, and none removes the tag on retag alone, so neither another value nor
// none is written on create; --scopes takes the broker's own scope names (policy.ParseScopes),
// so a comma, a whole https://www.googleapis.com/auth/ URL and an upper-case name are each
// refused as that flag. Every one of them is refused before the sign-in is read, so a create
// that names one writes nothing at all.
func TestSecretCreateRefusesBadAdminAndScopes(t *testing.T) {
	local := secrets.NewLocal()
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	base := []string{"create", "NEW_KEY", "--owner", "me", "--tier", "human", "--max-grant", "1h", "--requesters", "ada@example.com"}
	for _, tc := range []struct {
		flag, value, want string
	}{
		// The refused value is not printed (valueNotPrinted): a flag's value is where a pasted
		// credential lands.
		{"--admin", "yes", "--admin is true (the value is not printed)"},
		{"--admin", "none", "--admin is true (the value is not printed)"},
		{"--scopes", "cloud-platform,bigquery", "--scopes: scopes separates its names with spaces, not commas"},
		{"--scopes", "https://www.googleapis.com/auth/cloud-platform", "--scopes: scopes names the part of a Google scope"},
		{"--scopes", "Cloud-Platform", "--scopes: scopes names the part of a Google scope"},
	} {
		useStdin(t, "stored-value")
		args := append(slices.Clone(base), tc.flag, tc.value)
		_, stderr, code := runSecret(args...)
		if code != exitUsageError || !strings.Contains(stderr, tc.want) || strings.Contains(stderr, strconv.Quote(tc.value)) {
			t.Errorf("%s %q: exit %d stderr %q; want a usage error naming %q", tc.flag, tc.value, code, stderr, tc.want)
		}
	}
	if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
		t.Fatal("a refused create made NEW_KEY")
	}
}

// TestANoneBesideALoginIsRefusedOnCreateAndRetag: none removes github-owners only alone on retag,
// so a list that names it beside a login, in any case, is refused before anything is read or
// written, on create and on retag, rather than written as a login named none.
func TestANoneBesideALoginIsRefusedOnCreateAndRetag(t *testing.T) {
	local := secrets.NewLocal(typeTaggedSecret(t, "APP_KEY"))
	startSecretBroker(t, servedBy(local))
	writes := &countingTagWrites{Local: local}
	useAWS(t, writes, testAccount, adaSignIn)
	for _, owners := range []string{"none ada", "ada NONE", "None  acme-org"} {
		useStdin(t, "stored-value")
		_, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGitHubApp, "--github-owners", owners)
		if code != exitUsageError || !strings.Contains(stderr, "none names no GitHub account") {
			t.Fatalf("create --github-owners %q: exit %d stderr %q; want a usage error naming none", owners, code, stderr)
		}
		_, stderr, code = runSecret("retag", "APP_KEY", "--github-owners", owners)
		if code != exitUsageError || !strings.Contains(stderr, "given alone") {
			t.Fatalf("retag --github-owners %q: exit %d stderr %q; want a usage error naming none alone", owners, code, stderr)
		}
	}
	if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
		t.Fatal("a refused create made NEW_KEY")
	}
	if writes.tags != 0 || writes.untags != 0 || writes.describes != 0 {
		t.Fatalf("refused commands made %d TagResource, %d UntagResource and %d DescribeSecret calls; want none", writes.tags, writes.untags, writes.describes)
	}
}

// TestARetagOfASecretWhoseStoredOwnersAreMalformedNamesTheMalformedTag: a github-app secret whose
// github-owners tag was written with a comma outside the CLI is refused by the broker as
// github-owners-malformed, and a retag that leaves the tag as it is says so, not that owners are
// missing, and writes nothing; naming the owners fixes it.
func TestARetagOfASecretWhoseStoredOwnersAreMalformedNamesTheMalformedTag(t *testing.T) {
	commas := typeTaggedSecret(t, "APP_KEY")
	commas.Tags[policy.TagGitHubOwners] = "acme-org,ada"
	local := secrets.NewLocal(commas)
	startSecretBroker(t, servedBy(local))
	writes := &countingTagWrites{Local: local}
	useAWS(t, writes, testAccount, adaSignIn)
	_, stderr, code := runSecret("retag", "APP_KEY", "--tier", "human")
	if code != exitUsageError || !strings.Contains(stderr, policy.ReasonGitHubOwnersMalformed) || strings.Contains(stderr, policy.ReasonGitHubOwnersMissing) {
		t.Fatalf("retag --tier human: exit %d stderr %q; want a usage error naming %s, not %s", code, stderr, policy.ReasonGitHubOwnersMalformed, policy.ReasonGitHubOwnersMissing)
	}
	if writes.tags != 0 {
		t.Fatalf("the refused retag made %d TagResource calls, want none", writes.tags)
	}
	if _, stderr, code := runSecret("retag", "APP_KEY", "--github-owners", "acme-org ada"); code != 0 {
		t.Fatalf("retag naming the owners: exit %d stderr %q", code, stderr)
	}
	if tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags); tags[policy.TagGitHubOwners] != "acme-org ada" {
		t.Fatalf("tags = %v, want github-owners %q", tags, "acme-org ada")
	}
}

// TestARetagThatLeavesAGitHubAppSecretWithoutOwnersWritesNothing: the github-owners rule
// (policy.GitHubOwnersFor) holds on retag as on create, against the type and owners the secret
// will hold once the write lands, on both stores, so a retag never writes a github-app secret the
// broker then refuses as github-owners-missing.
func TestARetagThatLeavesAGitHubAppSecretWithoutOwnersWritesNothing(t *testing.T) {
	refusals := [][]string{
		{"retag", "PLAIN_KEY", "--type", policy.TypeGitHubApp},
		{"retag", "APP_KEY", "--github-owners", "none"},
		{"retag", "PLAIN_KEY", "--github-owners", "ada"},
	}
	t.Run("aws", func(t *testing.T) {
		// PLAIN_KEY holds an App's value, which the retag to github-app below decodes.
		local := secrets.NewLocal(policytest.Secret("PLAIN_KEY", "ada@example.com", policy.TierAgent, policytest.TypeValue(policy.TypeGitHubApp)), typeTaggedSecret(t, "APP_KEY"))
		startSecretBroker(t, servedBy(local))
		writes := &countingTagWrites{Local: local}
		useAWS(t, writes, testAccount, adaSignIn)
		for _, args := range refusals {
			_, stderr, code := runSecret(args...)
			if code != exitUsageError || !strings.Contains(stderr, "github") {
				t.Fatalf("%v: exit %d stderr %q; want a usage error naming the github-owners rule", args, code, stderr)
			}
		}
		if writes.tags != 0 || writes.untags != 0 {
			t.Fatalf("refused retags made %d TagResource and %d UntagResource calls, want none", writes.tags, writes.untags)
		}
		if _, stderr, code := runSecret("retag", "PLAIN_KEY", "--type", policy.TypeGitHubApp, "--github-owners", "ada"); code != 0 {
			t.Fatalf("retag to github-app with owners: exit %d stderr %q", code, stderr)
		}
		if tags := secrets.TagMap(describe(t, local, "PLAIN_KEY").Tags); tags[policy.TagType] != policy.TypeGitHubApp || tags[policy.TagGitHubOwners] != "ada" {
			t.Fatalf("tags = %v, want github-app with owner ada", tags)
		}
		if _, stderr, code := runSecret("retag", "APP_KEY", "--type", "none", "--github-owners", "none"); code != 0 {
			t.Fatalf("removing the type and its owners together: exit %d stderr %q", code, stderr)
		}
	})
	t.Run("gcp", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		for _, create := range []struct {
			args  []string
			value string
		}{
			{[]string{"create", "PLAIN_KEY", "--owner", "me", "--tier", "agent"}, "stored-value"},
			{[]string{"create", "APP_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGitHubApp, "--github-owners", "acme-org ada"}, policytest.TypeValue(policy.TypeGitHubApp)},
		} {
			useStdin(t, create.value)
			if _, stderr, code := runSecret(create.args...); code != 0 {
				t.Fatalf("%v: exit %d stderr %q", create.args, code, stderr)
			}
		}
		annotations := func() map[string]map[string]string {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			held := map[string]map[string]string{}
			for id, secret := range fake.secrets {
				held[id] = maps.Clone(secret.secret.GetAnnotations())
			}
			return held
		}
		before := annotations()
		for _, args := range refusals {
			_, stderr, code := runSecret(args...)
			if code != exitUsageError || !strings.Contains(stderr, "github") {
				t.Fatalf("%v: exit %d stderr %q; want a usage error naming the github-owners rule", args, code, stderr)
			}
		}
		after := annotations()
		for id := range before {
			if !maps.Equal(after[id], before[id]) {
				t.Fatalf("annotations of %s after refused retags = %v, want unchanged %v", id, after[id], before[id])
			}
		}
	})
}

// TestARetagThatLeavesASecretUnservableToItsRequestersWritesNothing: the cross-tag rules
// (policy.TagRules) hold on retag as on create, against the tags the secret will hold once the
// write lands, so no retag leaves a secret the broker then refuses for who may ask for it: making
// a secret a google-service-account one whose requesters are not its own person owner is refused,
// whether the list names a service or the owner is no person at all, as is dropping the list from
// a secret that already holds that type. The list is written as the broker reads it, and
// --requesters none removes it.
func TestARetagThatLeavesASecretUnservableToItsRequestersWritesNothing(t *testing.T) {
	// LISTED_KEY holds a service account, which the retag to that type below decodes.
	listed := policytest.Secret("LISTED_KEY", "ada@example.com", policy.TierAgent, policytest.TypeValue(policy.TypeGoogleServiceAccount))
	listed.Tags[policy.TagRequesters] = "ada@example.com"
	listed.Tags[policy.TagMaxGrant] = "1h"
	local := secrets.NewLocal(listed)
	startSecretBroker(t, servedBy(local))
	writes := &countingTagWrites{Local: local}
	useAWS(t, writes, testAccount, adaSignIn)
	for _, tc := range []struct {
		want string
		args []string
	}{
		{policy.ReasonRequestersNotOwner, []string{"retag", "LISTED_KEY", "--type", policy.TypeGoogleServiceAccount, "--requesters", "builder"}},
		{policy.ReasonRequestersNotOwner, []string{"retag", "LISTED_KEY", "--type", policy.TypeGoogleServiceAccount, "--owner", policy.OwnerShared}},
	} {
		_, stderr, code := runSecret(tc.args...)
		if code != exitUsageError || !strings.Contains(stderr, tc.want) {
			t.Fatalf("%v: exit %d stderr %q; want a usage error naming %q", tc.args, code, stderr, tc.want)
		}
	}
	if writes.tags != 0 || writes.untags != 0 {
		t.Fatalf("refused retags made %d TagResource and %d UntagResource calls, want none", writes.tags, writes.untags)
	}
	if _, stderr, code := runSecret("retag", "LISTED_KEY", "--type", policy.TypeGoogleServiceAccount, "--requesters", "Ada@example.com"); code != exitUsageError || !strings.Contains(stderr, "lowercase sign-in email") {
		t.Fatalf("retag with an uppercase email: exit %d stderr %q; want a usage error naming the lowercase form", code, stderr)
	}
	if _, stderr, code := runSecret("retag", "LISTED_KEY", "--type", policy.TypeGoogleServiceAccount, "--requesters", "ada@example.com"); code != 0 {
		t.Fatalf("retag to a service-account secret its owner may ask for: exit %d stderr %q", code, stderr)
	}
	tags := secrets.TagMap(describe(t, local, "LISTED_KEY").Tags)
	if tags[policy.TagType] != policy.TypeGoogleServiceAccount || tags[policy.TagRequesters] != "ada@example.com" || tags[policy.TagMaxGrant] != "1h" {
		t.Fatalf("tags = %v, want the service-account type beside its owner's list and the hour", tags)
	}
	if _, stderr, code := runSecret("retag", "LISTED_KEY", "--requesters", "none"); code != exitUsageError || !strings.Contains(stderr, policy.ReasonRequestersNotOwner) {
		t.Fatalf("removing the list of a service-account secret: exit %d stderr %q; want a usage error naming %s", code, stderr, policy.ReasonRequestersNotOwner)
	}
	if _, stderr, code := runSecret("retag", "LISTED_KEY", "--type", "none"); code != 0 {
		t.Fatalf("retag --type none: exit %d stderr %q", code, stderr)
	}
	if _, stderr, code := runSecret("retag", "LISTED_KEY", "--requesters", "none"); code != 0 {
		t.Fatalf("retag --requesters none: exit %d stderr %q", code, stderr)
	}
	tags = secrets.TagMap(describe(t, local, "LISTED_KEY").Tags)
	if _, listed := tags[policy.TagRequesters]; listed {
		t.Fatalf("tags = %v, want no requesters", tags)
	}
	if tags[policy.TagMaxGrant] != "1h" || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent {
		t.Fatalf("tags = %v, want the max grant, owner and tier kept", tags)
	}
	if _, stderr, code := runSecret("retag", "LISTED_KEY", "--max-grant", "2h"); code != 0 {
		t.Fatalf("retag --max-grant 2h once no one is listed: exit %d stderr %q", code, stderr)
	}
}

// TestAGitHubOwnersListIsWrittenSpaceSeparatedAndACommaIsRefused: on AWS, whose tag values take a
// space and no comma, a list of several accounts is written as the broker reads it, lowercased,
// sorted and single-spaced; a comma anywhere is refused before create or retag writes anything,
// naming the space-separated form.
func TestAGitHubOwnersListIsWrittenSpaceSeparatedAndACommaIsRefused(t *testing.T) {
	local := secrets.NewLocal(typeTaggedSecret(t, "APP_KEY"))
	startSecretBroker(t, servedBy(local))
	writes := &countingTagWrites{Local: local}
	useAWS(t, writes, testAccount, adaSignIn)
	useStdin(t, "stored-value")
	for _, args := range [][]string{
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGitHubApp, "--github-owners", "acme-org,ada"},
		{"retag", "APP_KEY", "--github-owners", "acme-org, ada"},
	} {
		_, stderr, code := runSecret(args...)
		if code != exitUsageError || !strings.Contains(stderr, "spaces, not commas") || !strings.Contains(stderr, `"<login> <login>"`) {
			t.Fatalf("%v: exit %d stderr %q; want a usage error naming the space-separated form", args, code, stderr)
		}
	}
	if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
		t.Fatal("the refused create made NEW_KEY")
	}
	if writes.tags != 0 {
		t.Fatalf("the refused retag made %d TagResource calls, want none", writes.tags)
	}
	if _, stderr, code := runSecret("retag", "APP_KEY", "--github-owners", "Widgets-Org  ada acme-org"); code != 0 {
		t.Fatalf("retag to three owners: exit %d stderr %q", code, stderr)
	}
	if tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags); tags[policy.TagGitHubOwners] != "acme-org ada widgets-org" {
		t.Fatalf("tags = %v, want github-owners %q", tags, "acme-org ada widgets-org")
	}
	useStdin(t, policytest.TypeValue(policy.TypeGitHubApp))
	if _, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGitHubApp, "--github-owners", "ada acme-org"); code != 0 {
		t.Fatalf("create with two owners: exit %d stderr %q", code, stderr)
	}
	if tags := secrets.TagMap(describe(t, local, "NEW_KEY").Tags); tags[policy.TagGitHubOwners] != "acme-org ada" {
		t.Fatalf("tags = %v, want github-owners %q", tags, "acme-org ada")
	}
}

// TestACreateThatNamesNoneAsItsGitHubOwnersWritesNothing: on create, `--github-owners none` (in
// any case) names no GitHub account. It is not the removal it is on retag, and the github-app
// secret it would write has no github-owners tag. Such a create is refused with a usage error
// before anything is written, on both stores, as a create without --github-owners is.
func TestACreateThatNamesNoneAsItsGitHubOwnersWritesNothing(t *testing.T) {
	owners := []string{"none", "None", "NONE"}
	t.Run("aws", func(t *testing.T) {
		local := secrets.NewLocal()
		startSecretBroker(t, servedBy(local))
		useAWS(t, local, testAccount, adaSignIn)
		for _, owner := range owners {
			useStdin(t, "stored-value")
			_, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGitHubApp, "--github-owners", owner)
			if code != exitUsageError {
				t.Fatalf("create --github-owners %q: exit %d stderr %q; want a usage error", owner, code, stderr)
			}
			if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
				t.Fatalf("create --github-owners %q wrote NEW_KEY", owner)
			}
		}
	})
	t.Run("gcp", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		for _, owner := range owners {
			useStdin(t, "stored-value")
			_, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGitHubApp, "--github-owners", owner)
			if code != exitUsageError {
				t.Fatalf("create --github-owners %q: exit %d stderr %q; want a usage error", owner, code, stderr)
			}
			fake.mu.Lock()
			_, written := fake.secrets[testGCPPrefix+"new-key"]
			fake.mu.Unlock()
			if written {
				t.Fatalf("create --github-owners %q wrote NEW_KEY", owner)
			}
		}
	})
}

// countingTagWrites is a secrets.Local that counts DescribeSecret, TagResource and UntagResource
// calls and can refuse UntagResource as IAM refuses it.
type countingTagWrites struct {
	*secrets.Local
	describes, tags, untags int
	refuseUntag             bool
}

func (c *countingTagWrites) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	c.describes++
	return c.Local.DescribeSecret(ctx, in, o...)
}

func (c *countingTagWrites) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	c.tags++
	return c.Local.TagResource(ctx, in, o...)
}

func (c *countingTagWrites) UntagResource(ctx context.Context, in *secretsmanager.UntagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error) {
	c.untags++
	if c.refuseUntag {
		return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: secretsmanager:UntagResource"}
	}
	return c.Local.UntagResource(ctx, in, o...)
}

// typeTaggedSecret is a github-app secret name holding an App's value, with a max grant of an hour.
func typeTaggedSecret(t *testing.T, name string) secrets.LocalSecret {
	t.Helper()
	secret := policytest.TypeSecret(name, "ada@example.com", policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	secret.Tags[policy.TagMaxGrant] = "1h"
	return secret
}

// TestSecretRetagRemovalIsOneCallThatChangesNothingElse: on AWS a tag removal is its own
// UntagResource and nothing else, so IAM refusing it leaves every tag as it was, and a retag that
// both removes and sets tags is refused before any read or write, since it would take two calls.
// Each retag reads the secret's tags once, to build the tags it will leave and check them, and a
// write that lands reads them once more, to find a concurrent writer's (awsBackend.raceCheck).
func TestSecretRetagRemovalIsOneCallThatChangesNothingElse(t *testing.T) {
	local := secrets.NewLocal(typeTaggedSecret(t, "APP_KEY"))
	startSecretBroker(t, servedBy(local))
	writes := &countingTagWrites{Local: local}
	useAWS(t, writes, testAccount, adaSignIn)

	for _, args := range [][]string{
		{"retag", "APP_KEY", "--type", "none", "--tier", "human"},
		{"retag", "APP_KEY", "--type", "none", "--owner", "shared"},
		{"retag", "APP_KEY", "--max-grant", "none", "--type", policy.TypeGoogleLogin},
	} {
		_, stderr, code := runSecret(args...)
		if code != exitUsageError || !strings.Contains(stderr, "two retags") {
			t.Fatalf("%v: exit %d stderr %q; want a usage error naming two retags", args, code, stderr)
		}
	}
	if writes.tags != 0 || writes.untags != 0 || writes.describes != 0 {
		t.Fatalf("refused mixes made %d TagResource, %d UntagResource and %d DescribeSecret calls, want none", writes.tags, writes.untags, writes.describes)
	}

	writes.refuseUntag = true
	_, stderr, code := runSecret("retag", "APP_KEY", "--type", "none")
	if code != 1 || !strings.Contains(stderr, "UntagResource") {
		t.Fatalf("refused removal: exit %d stderr %q; want 1 naming the refusal", code, stderr)
	}
	if writes.tags != 0 || writes.untags != 1 {
		t.Fatalf("refused removal made %d TagResource and %d UntagResource calls, want 0 and 1", writes.tags, writes.untags)
	}
	tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags)
	if tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent || tags[policy.TagType] != policy.TypeGitHubApp || tags[policy.TagMaxGrant] != "1h" {
		t.Fatalf("tags after a refused removal = %v, want every tag unchanged", tags)
	}

	writes.refuseUntag = false
	stdout, stderr, code := runSecret("retag", "APP_KEY", "--type", "none", "--max-grant", "none")
	if code != 0 || !strings.Contains(stdout, "removed type, kind, mint-service-account, max-grant") {
		t.Fatalf("removal: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if writes.tags != 0 || writes.untags != 2 || writes.describes != 3 {
		t.Fatalf("removals made %d TagResource, %d UntagResource and %d DescribeSecret calls in all, want 0, 2 and 3: one read of the tags and one write each, and one read back after the write that landed", writes.tags, writes.untags, writes.describes)
	}
	tags = secrets.TagMap(describe(t, local, "APP_KEY").Tags)
	if _, ok := tags[policy.TagType]; ok {
		t.Fatalf("tags after removal = %v, want no type", tags)
	}
	if _, ok := tags[policy.TagMaxGrant]; ok {
		t.Fatalf("tags after removal = %v, want no max-grant", tags)
	}
	if tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent {
		t.Fatalf("tags after removal = %v, want owner and tier unchanged", tags)
	}
}

// TestSecretRefusedRemovalOnASharedSecretSaysOnlyWhatTheCloudSaid: a removal sends no owner or
// tier, so the cloud refusing one on a shared secret does not print the owner-and-tier hint.
func TestSecretRefusedRemovalOnASharedSecretSaysOnlyWhatTheCloudSaid(t *testing.T) {
	shared := typeTaggedSecret(t, "SHARED_APP_KEY")
	shared.Tags[policy.TagOwner] = policy.OwnerShared
	local := secrets.NewLocal(shared)
	startSecretBroker(t, servedBy(local))
	useAWS(t, &countingTagWrites{Local: local, refuseUntag: true}, testAccount, adaSignIn)
	_, stderr, code := runSecret("retag", "SHARED_APP_KEY", "--type", "none")
	if code != 1 || !strings.Contains(stderr, "UntagResource") || strings.Contains(stderr, "administrator") {
		t.Fatalf("refused removal on a shared secret: exit %d stderr %q; want 1 with the cloud's refusal alone", code, stderr)
	}
}

// TestSecretRetagSetsAndRemovesMaxGrant: retag --max-grant sets the tag on both backends and keeps
// type, and --max-grant none removes it and keeps type.
func TestSecretRetagSetsAndRemovesMaxGrant(t *testing.T) {
	t.Run("aws", func(t *testing.T) {
		local := secrets.NewLocal(typeTaggedSecret(t, "APP_KEY"))
		startSecretBroker(t, servedBy(local))
		useAWS(t, local, testAccount, adaSignIn)
		if _, stderr, code := runSecret("retag", "APP_KEY", "--max-grant", "2h"); code != 0 {
			t.Fatalf("retag --max-grant 2h: exit %d, stderr %q", code, stderr)
		}
		tags := secrets.TagMap(describe(t, local, "APP_KEY").Tags)
		if tags[policy.TagMaxGrant] != "2h" || tags[policy.TagType] != policy.TypeGitHubApp || tags[policy.TagOwner] != "ada@example.com" {
			t.Fatalf("tags = %v, want max-grant 2h beside the kept type and owner", tags)
		}
		if _, stderr, code := runSecret("retag", "APP_KEY", "--max-grant", "none"); code != 0 {
			t.Fatalf("retag --max-grant none: exit %d, stderr %q", code, stderr)
		}
		tags = secrets.TagMap(describe(t, local, "APP_KEY").Tags)
		if _, ok := tags[policy.TagMaxGrant]; ok || tags[policy.TagType] != policy.TypeGitHubApp {
			t.Fatalf("tags = %v, want no max-grant and the type kept", tags)
		}
		for _, value := range []string{"soon", "500ms"} {
			if _, stderr, code := runSecret("retag", "APP_KEY", "--max-grant", value); code != exitUsageError {
				t.Fatalf("retag --max-grant %s: exit %d stderr %q; want usage error", value, code, stderr)
			}
		}
	})
	t.Run("gcp", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		useStdin(t, policytest.TypeValue(policy.TypeGoogleLogin))
		if _, stderr, code := runSecret("create", "LOGIN_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleLogin); code != 0 {
			t.Fatalf("create: exit %d, stderr %q", code, stderr)
		}
		id := testGCPPrefix + "login-key"
		annotations := func() map[string]string {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return fake.secrets[id].secret.GetAnnotations()
		}
		if _, stderr, code := runSecret("retag", "LOGIN_KEY", "--max-grant", "45m"); code != 0 {
			t.Fatalf("retag --max-grant 45m: exit %d, stderr %q", code, stderr)
		}
		if got := annotations(); got[policy.TagMaxGrant] != "45m" || got[policy.TagType] != policy.TypeGoogleLogin || got[policy.TagOwner] != "ada@example.com" {
			t.Fatalf("annotations = %v, want max-grant 45m beside the kept type and owner", got)
		}
		before := annotations()
		if _, stderr, code := runSecret("retag", "LOGIN_KEY", "--type", "none", "--tier", "human"); code != exitUsageError || !strings.Contains(stderr, "two retags") {
			t.Fatalf("mixed retag: exit %d stderr %q; want a usage error naming two retags", code, stderr)
		}
		if got := annotations(); !maps.Equal(got, before) {
			t.Fatalf("annotations after a refused mix = %v, want unchanged %v", got, before)
		}
		if _, stderr, code := runSecret("retag", "LOGIN_KEY", "--max-grant", "none"); code != 0 {
			t.Fatalf("retag --max-grant none: exit %d, stderr %q", code, stderr)
		}
		if got := annotations(); got[policy.TagType] != policy.TypeGoogleLogin || got[policy.TagTier] != policy.TierAgent {
			t.Fatalf("annotations = %v, want type and tier kept", got)
		} else if _, ok := got[policy.TagMaxGrant]; ok {
			t.Fatalf("annotations = %v, want no max-grant", got)
		}
	})
}

// TestTagRefusalRefusesAReasonNoArmNames pins the client's answer on both ways into tagRefusal's
// one fallback: a usage error quoting the reason that refused the secret, never nil. Every other
// arm turns one of policy.TagRules' reasons, or one of policy.RequesterRule's faults, into the
// flag that fixes it; the fallback carries the rest, and refusing there holds the client to the
// broker's rules where answering nil would let a write through that the broker refuses on the
// reread.
//
// The two ways in are a reason no case names, and policy.ReasonRequestersNotOwner under a fault
// no case of the inner switch names. The second set is the one that reaches it: no type, so the
// google-service-account conditions are all skipped, a BoundAccount the deployment binds to one
// person (dev-x@p.iam.gserviceaccount.com) and a requesters list naming the service builder, for
// which policy.RequesterRule answers RequesterFaultServiceOnDevAccount — checked here, so the
// case cannot quietly start passing through an arm that does name its fault.
func TestTagRefusalRefusesAReasonNoArmNames(t *testing.T) {
	onDevAccount := policy.TagSet{
		Owner:        "ada@example.com",
		Tier:         policy.TierAgent,
		Requesters:   "builder",
		BoundAccount: "dev-x@p.iam.gserviceaccount.com",
	}
	if fault := policy.RequesterRule(onDevAccount); fault != policy.RequesterFaultServiceOnDevAccount {
		t.Fatalf("policy.RequesterRule(%+v) = %v, want RequesterFaultServiceOnDevAccount, the fault tagRefusal names no arm for", onDevAccount, fault)
	}
	for _, c := range []struct {
		name   string
		reason string
		set    policy.TagSet
	}{
		{"a reason no arm names", "a-rule-this-client-does-not-know", policy.TagSet{Owner: "ada@example.com", Tier: policy.TierAgent}},
		{"a requesters fault no arm names", policy.ReasonRequestersNotOwner, onDevAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := tagRefusal(c.reason, c.set, map[string]string{policy.TagOwner: c.set.Owner, policy.TagTier: c.set.Tier})
			var usage usageErr
			if !errors.As(err, &usage) {
				t.Fatalf("tagRefusal(%q) = %v, want a usage error", c.reason, err)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("tagRefusal error %q does not name the reason %q that refused the secret", err, c.reason)
			}
		})
	}
	if tagRefusal("", onDevAccount, nil) != nil {
		t.Fatalf("tagRefusal of no reason = %v, want nil", tagRefusal("", onDevAccount, nil))
	}
}

// appKeyPEM is the private key policytest.TypeValue's github-app value holds, in PEM: the one
// GitHub App key these tests store.
func appKeyPEM(t *testing.T) string {
	t.Helper()
	var fields map[string]string
	if err := json.Unmarshal([]byte(policytest.TypeValue(policy.TypeGitHubApp)), &fields); err != nil {
		t.Fatal(err)
	}
	return fields["private_key"]
}

// appValue is a github-app value holding fields and nothing else.
func appValue(t *testing.T, fields map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// unparsableAppKey is a PEM block of the private key's type whose bytes are no key at all.
func unparsableAppKey() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("no key")}))
}

// leak is a word each refused value below carries where it can, beside the key material: no
// refusal may print it.
const leak = "s3cr3t"

// refusedTypeValue is a value its type's minter refuses, and name says what is wrong with it.
type refusedTypeValue struct{ secretType, name, value string }

// refusedTypeValues are, for each of the four types, values its minter refuses: for github-app a
// value that is not a JSON object, no app_id, an app_id that is not digits, no private_key, a key
// that does not parse and one that cannot sign.
func refusedTypeValues(t *testing.T) []refusedTypeValue {
	t.Helper()
	key := appKeyPEM(t)
	return []refusedTypeValue{
		{policy.TypeGitHubApp, "not a JSON object", leak + " is no JSON"},
		{policy.TypeGitHubApp, "no app_id", appValue(t, map[string]string{"private_key": key, "note": leak})},
		{policy.TypeGitHubApp, "an app_id that is not digits", appValue(t, map[string]string{"app_id": "12" + leak, "private_key": key})},
		{policy.TypeGitHubApp, "no private_key", appValue(t, map[string]string{"app_id": "123456", "note": leak})},
		{policy.TypeGitHubApp, "a key that does not parse", appValue(t, map[string]string{"app_id": "123456", "private_key": unparsableAppKey()})},
		{policy.TypeGitHubApp, "a key that cannot sign", appValue(t, map[string]string{"app_id": "123456", "private_key": policytest.UnsignableAppKey(t)})},
		{policy.TypeGoogleLogin, "not a JSON object", leak},
		{policy.TypeGoogleLogin, "not an authorized_user credential", `{"type":"service_account","client_id":"client-id","client_secret":"` + leak + `","refresh_token":"refresh-not-a-secret"}`},
		{policy.TypeGoogleLogin, "no refresh_token", `{"type":"authorized_user","client_id":"client-id","client_secret":"` + leak + `"}`},
		{policy.TypeGoogleServiceAccount, "not a JSON object", leak},
		{policy.TypeGoogleServiceAccount, "a person's address", `{"service_account":"` + leak + `@example.com"}`},
		{policy.TypeAzureLogin, "not a JSON object", leak},
		{policy.TypeAzureLogin, "no client_id", `{"tenant_id":"` + leak + `"}`},
		{policy.TypeAzureLogin, "a tenant_id of white space", `{"tenant_id":" ","client_id":"` + leak + `"}`},
	}
}

// mintRefusal is type's minter's answer to value, run as the broker runs it, with the stored
// value the first thing it reads (the Azure minter on its own configured human-tier secret): the
// words a client's refusal of the same value must carry.
func mintRefusal(t *testing.T, secretType, value string) string {
	t.Helper()
	req := secrets.MintRequest{Name: "TYPE_KEY", HumanTier: true, Value: value, GitHubOwners: []string{"acme-org"}}
	var err error
	switch secretType {
	case policy.TypeGitHubApp:
		_, err = secrets.GitHubApp{}.Mint(t.Context(), req)
	case policy.TypeGoogleLogin:
		_, err = secrets.GoogleLogin{}.Mint(t.Context(), req)
	case policy.TypeGoogleServiceAccount:
		_, err = (&secrets.GoogleServiceAccount{}).Mint(t.Context(), req)
	case policy.TypeAzureLogin:
		_, err = secrets.AzureLogin{SecretName: req.Name}.Mint(t.Context(), req)
	default:
		t.Fatalf("no minter of type %q", secretType)
	}
	if secrets.MintErrorClass(err) != "stored-value" {
		t.Fatalf("the %s minter's answer to the value = %v; want a stored-value refusal", secretType, err)
	}
	return err.Error()
}

// typeArgs are the flags beside --type type that a create or a retag names so the tags it leaves
// are ones the broker serves: a github-app secret's GitHub owners, and a google-service-account
// secret's requesters and hour-long cap.
func typeArgs(secretType string) []string {
	switch secretType {
	case policy.TypeGitHubApp:
		return []string{"--github-owners", "acme-org"}
	case policy.TypeGoogleServiceAccount:
		return []string{"--requesters", "ada@example.com", "--max-grant", "1h"}
	}
	return nil
}

// typeStore is a store the secret forms write, as these tests set it up and read it back: Secrets
// Manager (a secrets.Local behind useAWS) or Secret Manager (a fakeSecretManager).
type typeStore interface {
	// put holds the secret name with tags and value, as a writer other than this client stores it.
	put(t *testing.T, name string, tags map[string]string, value string)
	// has, value and tags read the secret name back: whether the store holds it, its current
	// value, and its tags.
	has(t *testing.T, name string) bool
	value(t *testing.T, name string) string
	tags(t *testing.T, name string) map[string]string
	// refuseValueReads refuses every value read from now on: as access denied when denied, and as
	// another failure of the read otherwise. valueReads counts the value reads, refused or not.
	refuseValueReads(denied bool)
	valueReads() int
	// readPermissions are the permissions the client names for this store's value read.
	readPermissions() []string
	// calls is how many calls the client has made of the store so far, refused ones included.
	calls() int
	// refuseTagReads refuses every read of a secret's tags from now on, as access denied: IAM
	// grants the writes and not the read. Its refusal never writes the permission the client
	// must name, so a client that names none fails tagReadPermission's test.
	refuseTagReads()
	// tagReadPermission is the permission the client names for this store's tag read.
	tagReadPermission() string
}

// onEachStore runs test on each store the secret forms write, signed in as ada@example.com, with
// the fake broker answering each reread from what that store holds.
func onEachStore(t *testing.T, test func(t *testing.T, store typeStore, broker *secretBroker)) {
	t.Run("aws", func(t *testing.T) {
		reads := &valueReads{Local: secrets.NewLocal()}
		broker := startSecretBroker(t, servedBy(reads.Local))
		useAWS(t, reads, testAccount, adaSignIn)
		test(t, awsTypeStore{reads}, broker)
	})
	t.Run("gcp", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		broker := startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		test(t, gcpTypeStore{fake}, broker)
	})
}

// valueReads is a secrets.Local that counts every call made of it and, of them, the
// GetSecretValue calls, and answers each of those with refuse when it is set, as IAM refuses the
// call or KMS the decryption, and answers DescribeSecret with refuseDescribe when that is set, as
// IAM refuses the tag read.
type valueReads struct {
	*secrets.Local
	reads          int
	refuse         error
	refuseDescribe error
	all            atomic.Int32
}

func (v *valueReads) GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	v.all.Add(1)
	v.reads++
	if v.refuse != nil {
		return nil, v.refuse
	}
	return v.Local.GetSecretValue(ctx, in, o...)
}

func (v *valueReads) ListSecrets(ctx context.Context, in *secretsmanager.ListSecretsInput, o ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	v.all.Add(1)
	return v.Local.ListSecrets(ctx, in, o...)
}

func (v *valueReads) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	v.all.Add(1)
	if v.refuseDescribe != nil {
		return nil, v.refuseDescribe
	}
	return v.Local.DescribeSecret(ctx, in, o...)
}

func (v *valueReads) CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	v.all.Add(1)
	return v.Local.CreateSecret(ctx, in, o...)
}

func (v *valueReads) PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	v.all.Add(1)
	return v.Local.PutSecretValue(ctx, in, o...)
}

func (v *valueReads) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	v.all.Add(1)
	return v.Local.TagResource(ctx, in, o...)
}

func (v *valueReads) UntagResource(ctx context.Context, in *secretsmanager.UntagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error) {
	v.all.Add(1)
	return v.Local.UntagResource(ctx, in, o...)
}

func (v *valueReads) DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	v.all.Add(1)
	return v.Local.DeleteSecret(ctx, in, o...)
}

func (v *valueReads) RestoreSecret(ctx context.Context, in *secretsmanager.RestoreSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.RestoreSecretOutput, error) {
	v.all.Add(1)
	return v.Local.RestoreSecret(ctx, in, o...)
}

// awsTypeStore is Secrets Manager as typeStore: a secrets.Local behind valueReads.
type awsTypeStore struct{ sm *valueReads }

func (s awsTypeStore) put(_ *testing.T, name string, tags map[string]string, value string) {
	s.sm.Put(secrets.LocalSecret{Name: policytest.ID(name), KmsKeyID: policytest.KeyARN, Tags: maps.Clone(tags), Value: value})
}

func (s awsTypeStore) has(_ *testing.T, name string) bool {
	_, err := s.sm.Local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID(name))})
	return err == nil
}

func (s awsTypeStore) value(t *testing.T, name string) string { return valueOf(t, s.sm.Local, name) }

func (s awsTypeStore) tags(t *testing.T, name string) map[string]string {
	return secrets.TagMap(describe(t, s.sm.Local, name).Tags)
}

func (s awsTypeStore) refuseValueReads(denied bool) {
	if denied {
		s.sm.refuse = &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "User: " + adaSignIn + " is not authorized to perform: secretsmanager:GetSecretValue"}
		return
	}
	s.sm.refuse = &smtypes.DecryptionFailure{Message: aws.String("Secrets Manager can't decrypt the protected secret text using the provided KMS key.")}
}

func (s awsTypeStore) valueReads() int { return s.sm.reads }

func (s awsTypeStore) readPermissions() []string {
	return []string{"secretsmanager:GetSecretValue", "kms:Decrypt"}
}

func (s awsTypeStore) calls() int { return int(s.sm.all.Load()) }

func (s awsTypeStore) refuseTagReads() {
	// The refusal says only that the call was refused: the permission the client must name is
	// the client's to write, so a client that names none has nothing to copy it from.
	s.sm.refuseDescribe = &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "User: " + adaSignIn + " is not authorized to perform this call"}
}

func (s awsTypeStore) tagReadPermission() string { return "secretsmanager:DescribeSecret" }

// gcpTypeStore is Secret Manager as typeStore: a fakeSecretManager.
type gcpTypeStore struct{ fake *fakeSecretManager }

// id is the resource name of the secret a session asks for as name.
func (s gcpTypeStore) id(t *testing.T, name string) string {
	t.Helper()
	slug, err := policy.NameToSlug(name)
	if err != nil {
		t.Fatal(err)
	}
	return testGCPPrefix + slug
}

func (s gcpTypeStore) put(t *testing.T, name string, tags map[string]string, value string) {
	id := s.id(t, name)
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	s.fake.secrets[id] = &fakeSecret{
		secret:      &secretmanagerpb.Secret{Name: id, Annotations: maps.Clone(tags), Etag: "1"},
		versions:    []*secretmanagerpb.SecretVersion{{Name: id + "/versions/1", State: secretmanagerpb.SecretVersion_ENABLED}},
		payloads:    [][]byte{[]byte(value)},
		etagCounter: 1,
	}
}

func (s gcpTypeStore) has(t *testing.T, name string) bool {
	id := s.id(t, name)
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	_, ok := s.fake.secrets[id]
	return ok
}

func (s gcpTypeStore) value(t *testing.T, name string) string {
	id := s.id(t, name)
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	entry, ok := s.fake.secrets[id]
	if !ok || len(entry.payloads) == 0 {
		t.Fatalf("Secret Manager holds no value of %s", id)
	}
	return string(entry.payloads[len(entry.payloads)-1])
}

func (s gcpTypeStore) tags(t *testing.T, name string) map[string]string {
	id := s.id(t, name)
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	entry, ok := s.fake.secrets[id]
	if !ok {
		t.Fatalf("Secret Manager holds no %s", id)
	}
	return maps.Clone(entry.secret.GetAnnotations())
}

func (s gcpTypeStore) refuseValueReads(denied bool) {
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	s.fake.refuseAccess = codes.FailedPrecondition
	if denied {
		s.fake.refuseAccess = codes.PermissionDenied
	}
}

func (s gcpTypeStore) valueReads() int {
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	return s.fake.accesses
}

func (s gcpTypeStore) calls() int {
	_, calls := s.fake.calledUnder()
	return calls
}

func (s gcpTypeStore) readPermissions() []string { return []string{"secretmanager.versions.access"} }

func (s gcpTypeStore) refuseTagReads() {
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	s.fake.refuseMethods = map[string]codes.Code{"GetSecret": codes.PermissionDenied}
}

func (s gcpTypeStore) tagReadPermission() string { return "secretmanager.secrets.get" }

// TestSecretCreateSetAndRetagRefuseEveryValueTheTypesMinterRefuses: on both stores, for each of
// the four types, create, set and retag refuse each value that type's minter refuses, and leave
// the store as they found it: create holds no secret, set no new version, retag no new tag. Each
// exits 2 naming what is wrong in the minter's own words, read back from the minter driven with
// the same value, so the one decoder decides both, names the type after the article it takes ("an
// azure-login secret's"), and none prints the value. Set checks the value under the type tag the
// secret holds, since it names none. The broker is asked to reread nothing, but for a create on
// Secret Manager: that makes the secret before it reads the value and deletes it on the refusal,
// then asks the broker to reread the name once, which the broker answers absent.
func TestSecretCreateSetAndRetagRefuseEveryValueTheTypesMinterRefuses(t *testing.T) {
	onEachStore(t, func(t *testing.T, store typeStore, broker *secretBroker) {
		for _, row := range refusedTypeValues(t) {
			t.Run(row.secretType+"/"+row.name, func(t *testing.T) {
				says := mintRefusal(t, row.secretType, row.value)
				store.put(t, "HELD_KEY", policytest.TypeSecret("HELD_KEY", "ada@example.com", policy.TierAgent, row.secretType, "").Tags, policytest.TypeValue(row.secretType))
				plain := policytest.Secret("PLAIN_KEY", "ada@example.com", policy.TierAgent, "").Tags
				store.put(t, "PLAIN_KEY", plain, row.value)
				// The refusal names the type after the article it takes, written out here rather
				// than derived by the rule that writes it: "an azure-login", never "a azure-login".
				named := "the broker refuses this value as a " + row.secretType + " secret's"
				if row.secretType == policy.TypeAzureLogin {
					named = "the broker refuses this value as an " + row.secretType + " secret's"
				}
				_, madeFirst := store.(gcpTypeStore)
				for _, form := range []struct {
					args []string
					// wrote reports whether the form left anything written: a secret created, a
					// version set, a tag retagged.
					wrote func() bool
					// rereads is the names the form asks the broker to reread.
					rereads []string
				}{
					{append([]string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--type", row.secretType}, typeArgs(row.secretType)...), func() bool { return store.has(t, "NEW_KEY") }, nil},
					{[]string{"set", "HELD_KEY"}, func() bool { return store.value(t, "HELD_KEY") != policytest.TypeValue(row.secretType) }, nil},
					{append([]string{"retag", "PLAIN_KEY", "--type", row.secretType}, typeArgs(row.secretType)...), func() bool { return !maps.Equal(store.tags(t, "PLAIN_KEY"), plain) }, nil},
				} {
					if madeFirst && form.args[0] == "create" {
						form.rereads = []string{"NEW_KEY"}
					}
					asked, _ := broker.recorded()
					useStdin(t, row.value)
					stdout, stderr, code := runSecret(form.args...)
					if code != exitUsageError || !strings.Contains(stderr, says) {
						t.Errorf("%s of a %s value with %s: exit %d stderr %q; want %d naming the minter's own refusal %q", form.args[0], row.secretType, row.name, code, stderr, exitUsageError, says)
					}
					if !strings.Contains(stderr, named) || strings.Contains(stderr, " a "+policy.TypeAzureLogin) {
						t.Errorf("%s of a %s value with %s: stderr %q; want it to say %q", form.args[0], row.secretType, row.name, stderr, named)
					}
					if strings.Contains(stdout+stderr, leak) || strings.Contains(stdout+stderr, "PRIVATE KEY") {
						t.Errorf("%s of a %s value with %s printed the value: stdout %q stderr %q", form.args[0], row.secretType, row.name, stdout, stderr)
					}
					if form.wrote() {
						t.Errorf("a refused %s of a %s value with %s wrote it", form.args[0], row.secretType, row.name)
					}
					rereads, answers := broker.recorded()
					if got := rereads[len(asked):]; !slices.Equal(got, form.rereads) || slices.ContainsFunc(answers[len(asked):], func(r Reread) bool { return r.Served || r.Reason != policy.ReasonAbsent }) {
						t.Errorf("a refused %s of a %s value with %s asked the broker to reread %v, answered %v; want %v, answered absent", form.args[0], row.secretType, row.name, got, answers[len(asked):], form.rereads)
					}
				}
			})
		}
	})
}

// TestSecretRetagReadsTheValueOnlyWhenItChangesTheType: on both stores, a retag reads the
// secret's value only when the tags it writes change the type, since people commonly may retag a
// secret and not read it; any other retag reads none, and writes over a value the decoder
// refuses. The read has three outcomes, each its own: a value the decoder refuses is refused,
// naming why, and nothing is written; any read error but access denied is refused, and nothing
// is written; and access denied, and only that, lets the retag write its tags, with one line on
// stderr saying the value was not checked, naming the permissions the read needs and saying the
// broker reports such a secret unservable.
func TestSecretRetagReadsTheValueOnlyWhenItChangesTheType(t *testing.T) {
	noAppID := appValue(t, map[string]string{"private_key": appKeyPEM(t)})
	says := mintRefusal(t, policy.TypeGitHubApp, noAppID)
	onEachStore(t, func(t *testing.T, store typeStore, broker *secretBroker) {
		store.put(t, "APP_KEY", policytest.TypeSecret("APP_KEY", "ada@example.com", policy.TierAgent, policy.TypeGitHubApp, "").Tags, noAppID)
		store.put(t, "PLAIN_KEY", policytest.Secret("PLAIN_KEY", "ada@example.com", policy.TierAgent, "").Tags, noAppID)
		toApp := []string{"retag", "PLAIN_KEY", "--type", policy.TypeGitHubApp, "--github-owners", "acme-org"}
		rereads := func() int {
			asked, _ := broker.recorded()
			return len(asked)
		}

		for _, args := range [][]string{
			{"retag", "APP_KEY", "--tier", policy.TierHuman},
			{"retag", "APP_KEY", "--max-grant", "2h"},
			{"retag", "APP_KEY", "--github-owners", "acme-org"},
			{"retag", "APP_KEY", "--type", policy.TypeGitHubApp},
			{"retag", "PLAIN_KEY", "--tier", policy.TierHuman},
		} {
			if _, stderr, code := runSecret(args...); code != 0 {
				t.Fatalf("%v: exit %d stderr %q; want 0", args, code, stderr)
			}
		}
		if n := store.valueReads(); n != 0 {
			t.Fatalf("retags that change no type read %d values; want none", n)
		}

		// The value reads, and the decoder refuses it.
		before, asked := store.tags(t, "PLAIN_KEY"), rereads()
		_, stderr, code := runSecret(toApp...)
		if code != exitUsageError || !strings.Contains(stderr, says) {
			t.Fatalf("a retag to github-app over a value with no app_id: exit %d stderr %q; want %d naming %q", code, stderr, exitUsageError, says)
		}
		if got := store.tags(t, "PLAIN_KEY"); !maps.Equal(got, before) || rereads() != asked {
			t.Fatalf("the refused retag left tags %v (were %v) and asked for %d rereads", got, before, rereads()-asked)
		}
		if n := store.valueReads(); n != 1 {
			t.Fatalf("the retag that changes the type read %d values; want 1", n)
		}

		// Any other read error.
		store.refuseValueReads(false)
		before, asked = store.tags(t, "PLAIN_KEY"), rereads()
		_, stderr, code = runSecret(toApp...)
		if code != 1 || !strings.Contains(stderr, "read PLAIN_KEY's value to check it as a github-app secret's, so nothing is written") || strings.Contains(stderr, "not checked") {
			t.Fatalf("a retag whose value read fails: exit %d stderr %q; want 1 refusing it", code, stderr)
		}
		if got := store.tags(t, "PLAIN_KEY"); !maps.Equal(got, before) || rereads() != asked {
			t.Fatalf("a retag whose value read fails left tags %v (were %v) and asked for %d rereads", got, before, rereads()-asked)
		}

		// Access denied, and only that.
		store.refuseValueReads(true)
		stdout, stderr, code := runSecret(toApp...)
		if code != 0 || !strings.Contains(stdout, "broker: serving PLAIN_KEY") {
			t.Fatalf("a retag whose value read is denied: exit %d stdout %q stderr %q; want 0 with the tags served", code, stdout, stderr)
		}
		if tags := store.tags(t, "PLAIN_KEY"); tags[policy.TagType] != policy.TypeGitHubApp || tags[policy.TagGitHubOwners] != "acme-org" {
			t.Fatalf("tags after a retag whose value read is denied = %v; want github-app for acme-org", tags)
		}
		if strings.Count(afterUsing(t, stderr, "secret retag"), "\n") != 1 || !strings.Contains(stderr, "the tags are written, but PLAIN_KEY's value was not checked as a github-app secret's") ||
			!strings.Contains(stderr, "reports PLAIN_KEY unservable with the decoder's reason") {
			t.Fatalf("stderr %q; want one line saying the value was not checked and the broker reports it unservable", stderr)
		}
		for _, permission := range store.readPermissions() {
			if !strings.Contains(stderr, permission) {
				t.Fatalf("stderr %q does not name %s, which the value read needs", stderr, permission)
			}
		}
		if n := store.valueReads(); n != 3 {
			t.Fatalf("value reads = %d; want 3, one for each retag that changed the type", n)
		}
	})
}

// TestSecretSetRefusesASecretWhoseTypeTagNamesNoType: the broker refuses a secret whose type tag
// names no type it mints (type-unknown) and serves no value of it, so set refuses to store one,
// exiting 2 and naming that reason, before it reads the value.
func TestSecretSetRefusesASecretWhoseTypeTagNamesNoType(t *testing.T) {
	local := secrets.NewLocal(policytest.TypeSecret("ODD_KEY", "ada@example.com", policy.TierAgent, "other", "v1"))
	broker := startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	refuseValueReads(t)
	_, stderr, code := runSecret("set", "ODD_KEY")
	if code != exitUsageError || !strings.Contains(stderr, `type tag "other"`) || !strings.Contains(stderr, policy.ReasonTypeUnknown) {
		t.Fatalf("set of a secret whose type tag is other: exit %d stderr %q; want %d naming %s", code, stderr, exitUsageError, policy.ReasonTypeUnknown)
	}
	if got := valueOf(t, local, "ODD_KEY"); got != "v1" {
		t.Fatalf("the refused set stored %q", got)
	}
	if rereads, _ := broker.recorded(); len(rereads) != 0 {
		t.Fatalf("rereads = %v after a refused set; want none", rereads)
	}
}
