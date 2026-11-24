package claim

import "slices"

// RoleSource is one source of the roles defined for an issue beside Legion's own (Roles): the
// deployment's settings (plan.Deployment), or any other that defines roles, each one more argument
// of DefinedRole.
type RoleSource interface {
	// DefinedRoles are the roles the source defines.
	DefinedRoles() []Role
}

// RoleList is a RoleSource of the roles it holds, for a caller that holds the roles defined for an
// issue as a list, as the sandbox runtime holds the roles of an issue pod.
type RoleList []Role

// DefinedRoles are the roles of l.
func (l RoleList) DefinedRoles() []Role { return l }

// DefinedRole is the role name names for an issue, and whether that role is defined for it: by
// Legion (Roles) or by one of sources. name is a role, or the name of an agent or of the slot it
// runs in (Agent, Token.Slot), which is the role of the agent's first step or, for a second or later
// agent of that role, a name that numbers it (NumberedAgentName, `implementer-2`). So a name that is
// a defined role is that role, and a numbered name is the role it numbers (NumberedAgentRole) where
// that role is defined: a numbered name of a role no source defines is defined no more than that
// role. It is the one rule of whether a name is defined for an issue: the workflow's lost role, lost
// agent and lost slot, and the sandbox runtime's reading of a volume named for an agent's slot, each
// read it.
func DefinedRole(name string, sources ...RoleSource) (Role, bool) {
	defined := slices.Clone(Roles)
	for _, source := range sources {
		defined = append(defined, source.DefinedRoles()...)
	}
	if slices.Contains(defined, Role(name)) {
		return Role(name), true
	}
	return NumberedAgentRole(name, defined)
}
