package runtime

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// What a pod lacks of a launch is one decision, which the outbox's start and the Sandbox runtime's
// launch both name: the role alone for a claim named for its role, each of two names with what it
// is to the claim, both where the pod lacks the two, and nothing where it holds both. A token that
// names no agent has no slot, so only its role is asked.
func TestWhatAPodLacksNamesTheSlotAndTheRoleOfALaunch(t *testing.T) {
	built := []claim.Role{claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester}
	lacks := func(name claim.Role) bool { return !slices.Contains(built, name) }
	for _, tc := range []struct {
		name string
		slot claim.Agent
		role claim.Role
		want string
	}{
		{"a claim named for a role the pod lacks", "checker", "checker", "checker"},
		{"a claim named for a role the pod holds", claim.Agent(claim.RoleTester), claim.RoleTester, ""},
		{"a keyed agent on a step of a role the pod lacks", claim.Agent(claim.RoleImplementer), "checker", "checker, the role of its step"},
		{"a keyed agent in a slot the pod lacks", "alpha", claim.RoleTester, "alpha, the slot of the claim's agent"},
		{"a keyed agent in a slot and on a role the pod lacks", "alpha", "checker", "alpha, the slot of the claim's agent, and checker, the role of its step"},
		{"a keyed agent whose slot and role the pod holds", claim.Agent(claim.RoleImplementer), claim.RoleTester, ""},
		{"a token that names no agent, on a role the pod lacks", "", "checker", "checker, the role of its step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PodLacks(lacks, tc.slot, tc.role); got != tc.want {
				t.Errorf("PodLacks(slot %q, role %q) = %q, want %q", tc.slot, tc.role, got, tc.want)
			}
		})
	}
}
