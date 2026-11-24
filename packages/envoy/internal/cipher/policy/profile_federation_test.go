package policy_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// TestAProfileOfAnAnthropicFederationSourceIsHeldToItsTypesRules: an anthropic-federation secret
// may be a profile's source, and R2 holds the profile to it as to any minted type. A profile that
// narrows its maxGrant is served behind the anthropic kind its source's proxy tag names, with the
// way mint; it keeps its source's type, so a person approves every release of it and the model
// judge decides none (policy.Secret.PersonApprovesEveryRelease). Each profile the type's own tag
// rules refuse is refused, the profile's tags over its source's: agent tier (type-human-tier), a
// maxGrant over an hour (max-grant-required) and requesters that leave out its owner
// (requesters-not-owner); and so are a github and a google block, neither of which fits the type.
// The same profile of a source with no proxy tag is refused, profile-source-not-proxied: the
// broker mints the type's token at each release and cannot narrow it there, so only the proxy
// scopes it (spec v70 adds the type to the three of R2 that need it).
func TestAProfileOfAnAnthropicFederationSourceIsHeldToItsTypesRules(t *testing.T) {
	fedAdmin := func() memory.Secret {
		return policytest.TypeSecret("FED_ADMIN", appOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation))
	}
	stored := []memory.Secret{appSource(), policytest.Proxied(fedAdmin(), "anthropic")}

	cur, _ := profiled(t, stored, readProfile, `{"name": "FED_ADMIN_SHORT", "source": "FED_ADMIN", "maxGrant": "30m"}`)
	set := cur.Get()
	served := servedProfile(t, set, "FED_ADMIN_SHORT")
	if served.Type() != policy.TypeAnthropicFederation || served.Tier != policy.TierHuman || served.MaxGrant != 30*time.Minute || !served.PersonApprovesEveryRelease() {
		t.Fatalf("FED_ADMIN_SHORT = type %s, tier %s, max-grant %s, a person approves every release %t; want %s, %s, 30m, true",
			served.Type(), served.Tier, served.MaxGrant, served.PersonApprovesEveryRelease(), policy.TypeAnthropicFederation, policy.TierHuman)
	}
	if served.Proxy != "anthropic" || served.Way != "mint" {
		t.Errorf("FED_ADMIN_SHORT is served behind %q by way %q; want anthropic, mint", served.Proxy, served.Way)
	}
	if d, err := set.Evaluate("FED_ADMIN_SHORT", policy.Requester{Operator: appOwner}); err != nil || d.Outcome != policy.Approval || d.Approver != appOwner {
		t.Fatalf("Evaluate(FED_ADMIN_SHORT) for its owner = %+v, %v; want an approval of %s", d, err, appOwner)
	}
	untagged, _ := profiled(t, []memory.Secret{appSource(), fedAdmin()}, readProfile, `{"name": "FED_ADMIN_SHORT", "source": "FED_ADMIN", "maxGrant": "30m"}`)
	refusedProfile(t, untagged.Get(), "FED_ADMIN_SHORT", policy.ReasonProfileSourceNotProxied)
	if refusal := untagged.Get().Refused[policytest.ID("FED_ADMIN")]; refusal.Reason != policy.ReasonProfileSourceNotProxied {
		t.Errorf("the untagged source's own refusal = %+v; want %s", refusal, policy.ReasonProfileSourceNotProxied)
	}

	for _, row := range []struct {
		name, profile, reason string
	}{
		{"agent tier", `{"name": "FED_ADMIN_SHORT", "source": "FED_ADMIN", "tier": "agent"}`, policy.ReasonTypeHumanTier},
		{"a maxGrant over an hour", `{"name": "FED_ADMIN_SHORT", "source": "FED_ADMIN", "maxGrant": "2h"}`, policy.ReasonMaxGrantRequired},
		{"requesters that leave out its owner", `{"name": "FED_ADMIN_SHORT", "source": "FED_ADMIN", "requesters": ["bob@example.com"]}`, policy.ReasonRequestersNotOwner},
		{"a github block", `{"name": "FED_ADMIN_SHORT", "source": "FED_ADMIN", ` + acmeRead + `}`, policy.ReasonProfileBlockMismatch},
		{"a google block", `{"name": "FED_ADMIN_SHORT", "source": "FED_ADMIN", "google": {"scopes": ["cloud-platform"]}}`, policy.ReasonProfileBlockMismatch},
	} {
		t.Run(row.name, func(t *testing.T) {
			cur, _ := profiled(t, stored, readProfile, row.profile)
			refusedProfile(t, cur.Get(), "FED_ADMIN_SHORT", row.reason)
		})
	}
}
