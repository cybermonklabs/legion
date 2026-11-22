package policy_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestTheFederationTypeTakesTheServiceAccountTypesPolicyFields: an anthropic-federation secret is
// human tier only, bounds its grants to an hour, names its requesters by RequesterRule (its person
// owner once, or its group owner once, beside services at human tier), may be an admin secret, and
// carries no scopes; a valid one loads and is served.
func TestTheFederationTypeTakesTheServiceAccountTypesPolicyFields(t *testing.T) {
	federation := func(change func(*policy.TagSet)) policy.TagSet {
		set := policy.TagSet{Type: policy.TypeAnthropicFederation, Owner: "ada@example.com", Tier: policy.TierHuman, Requesters: "ada@example.com", MaxGrant: time.Hour}
		if change != nil {
			change(&set)
		}
		return set
	}
	for _, c := range []struct {
		name string
		set  policy.TagSet
		want string
	}{
		{"its person owner once", federation(nil), ""},
		{"an admin secret", federation(func(s *policy.TagSet) { s.Admin = true }), ""},
		{"its group owner once", federation(func(s *policy.TagSet) { s.Owner, s.Requesters = approversOwner, approversOwner }), ""},
		{"its owner and a service", federation(func(s *policy.TagSet) { s.Requesters = "ada@example.com legion-daemon" }), ""},
		{"agent tier", federation(func(s *policy.TagSet) { s.Tier = policy.TierAgent }), policy.ReasonTypeHumanTier},
		{"a grant over an hour", federation(func(s *policy.TagSet) { s.MaxGrant = 2 * time.Hour }), policy.ReasonMaxGrantRequired},
		{"no max-grant", federation(func(s *policy.TagSet) { s.MaxGrant = 0 }), policy.ReasonMaxGrantRequired},
		{"a requester not its owner", federation(func(s *policy.TagSet) { s.Requesters = "bob@example.com" }), policy.ReasonRequestersNotOwner},
		{"no requesters", federation(func(s *policy.TagSet) { s.Requesters = "" }), policy.ReasonRequestersNotOwner},
		{"a shared owner", federation(func(s *policy.TagSet) { s.Owner = policy.OwnerShared }), policy.ReasonRequestersNotOwner},
		{"a group owner listing a person", federation(func(s *policy.TagSet) { s.Owner, s.Requesters = approversOwner, "ada@example.com" }), policy.ReasonRequestersNotOwner},
		{"scopes", federation(func(s *policy.TagSet) { s.Scopes = "cloud-platform" }), policy.ReasonScopesType},
	} {
		if got := policy.TagRules(c.set); got != c.want {
			t.Errorf("%s: TagRules = %q, want %q", c.name, got, c.want)
		}
	}
	if !policy.ValidType(policy.TypeAnthropicFederation) {
		t.Fatalf("ValidType(%q) = false", policy.TypeAnthropicFederation)
	}
	set := loadOnly(t, secrets.NewLocal(policytest.TypeSecret("FED_ADMIN", approversOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation))))
	if secret, served := set.Secrets["FED_ADMIN"]; !served || secret.Type() != policy.TypeAnthropicFederation {
		t.Fatalf("FED_ADMIN = %+v, served %v, refused %v; want it served as %s", secret, served, set.Refused, policy.TypeAnthropicFederation)
	}
	if err := policy.DecodeValue(policy.TypeAnthropicFederation, `{"organization_id":"x"}`); err == nil {
		t.Fatal("DecodeValue took a federation value naming no rule, target or account")
	}
}

// TestAPersonApprovesEveryReleaseOfAFederationSecret: a federation secret with no admin tag is one
// a person approves every release of, as an admin secret is, since its token can be an
// organization administrator's; a google-service-account secret with no admin tag is not.
func TestAPersonApprovesEveryReleaseOfAFederationSecret(t *testing.T) {
	admin := policytest.Secret("ADMIN_KEY", "ada@example.com", policy.TierHuman, "v")
	admin.Tags[policy.TagAdmin] = policy.AdminTrue
	admin.Tags[policy.TagRequesters] = "ada@example.com"
	admin.Tags[policy.TagMaxGrant] = "1h"
	set := loadOnly(t, secrets.NewLocal(
		policytest.TypeSecret("FED_ADMIN", "ada@example.com", policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation)),
		admin,
		policytest.TypeSecret("GCP_ADMIN", "ada@example.com", policy.TierHuman, policy.TypeGoogleServiceAccount, policytest.TypeValue(policy.TypeGoogleServiceAccount)),
	))
	for name, want := range map[string]bool{"FED_ADMIN": true, "ADMIN_KEY": true, "GCP_ADMIN": false} {
		secret, served := set.Secrets[name]
		if !served {
			t.Fatalf("%s is refused: %v", name, set.Refused)
		}
		if got := secret.PersonApprovesEveryRelease(); got != want {
			t.Errorf("%s.PersonApprovesEveryRelease() = %v, want %v", name, got, want)
		}
	}
}
