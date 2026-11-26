package policy_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestAProfileOfAnAnthropicFederationSourceIsHeldToItsTypesRules: an anthropic-federation secret
// may be a profile's source, and R2 holds the profile to it as to any minted type. A profile that
// narrows its maxGrant is served with no proxy block, since R2 asks for one only of a stored value,
// a google-login and an azure-login source, and the broker mints this type's credential at each
// release; it keeps its source's type, so a person approves every release of it and the model
// judge decides none (policy.Secret.PersonApprovesEveryRelease). Each profile the type's own tag
// rules refuse is refused, the profile's tags over its source's: agent tier (type-human-tier), a
// maxGrant over an hour (max-grant-required) and requesters that leave out its owner
// (requesters-not-owner); and so are a github and a google block, neither of which fits the type.
func TestAProfileOfAnAnthropicFederationSourceIsHeldToItsTypesRules(t *testing.T) {
	source := policytest.TypeSecret("FED_ADMIN", appOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation))
	stored := []secrets.LocalSecret{appSource(), source}

	cur, _ := profiled(t, stored, readProfile, `{"name": "FED_ADMIN_SHORT", "source": "FED_ADMIN", "maxGrant": "30m"}`)
	set := cur.Get()
	served := servedProfile(t, set, "FED_ADMIN_SHORT")
	if served.Type() != policy.TypeAnthropicFederation || served.Tier != policy.TierHuman || served.MaxGrant != 30*time.Minute || !served.PersonApprovesEveryRelease() {
		t.Fatalf("FED_ADMIN_SHORT = type %s, tier %s, max-grant %s, a person approves every release %t; want %s, %s, 30m, true",
			served.Type(), served.Tier, served.MaxGrant, served.PersonApprovesEveryRelease(), policy.TypeAnthropicFederation, policy.TierHuman)
	}
	if d, err := set.Evaluate("FED_ADMIN_SHORT", policy.Requester{Operator: appOwner}); err != nil || d.Outcome != policy.Approval || d.Approver != appOwner {
		t.Fatalf("Evaluate(FED_ADMIN_SHORT) for its owner = %+v, %v; want an approval of %s", d, err, appOwner)
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
