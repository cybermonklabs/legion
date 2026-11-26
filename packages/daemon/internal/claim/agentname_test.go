package claim

import (
	"slices"
	"strings"
	"testing"
)

// The name of a role's numbered agent (NumberedAgentName, from FirstAgentNumber, the role cut so
// that the name holds at most 32 bytes) is a name of the role grammar, which an agent's name takes,
// and reads back to its role
// (NumberedAgentRole), and only such a name does: a role's own name, a number below
// FirstAgentNumber or one NumberedAgentName never spells, a role cut otherwise, a name of no role
// and the controller's read back to none.
func TestANumberedAgentsNameReadsBackToItsRole(t *testing.T) {
	long := Role(strings.Repeat("r", 32))
	roles := append(slices.Clone(Roles), long, RoleController)
	for _, role := range []Role{RoleImplementer, RoleTester, long} {
		for _, n := range []int{FirstAgentNumber, 10, 123} {
			name := NumberedAgentName(role, n)
			if !roleName.MatchString(string(name)) {
				t.Errorf("NumberedAgentName(%s, %d) = %q, which is no name of the role grammar", role, n, name)
			}
			if got, ok := NumberedAgentRole(string(name), roles); !ok || got != role {
				t.Errorf("NumberedAgentRole(%q) = %q, %t; want %s", name, got, ok, role)
			}
		}
	}
	if got, want := NumberedAgentName(long, 2), Agent(strings.Repeat("r", 30)+"-2"); got != want {
		t.Errorf("NumberedAgentName of a role of 32 bytes = %q, want %q", got, want)
	}
	if got, want := RoleAgentName(long+"s"), Agent(long); got != want {
		t.Errorf("RoleAgentName of a name of 33 bytes = %q, want %q", got, want)
	}
	for _, name := range []string{
		"implementer", "implementer-1", "implementer-0", "implementer-02", "implementer-+2", "implementer-",
		"cache-2", "controller-2", strings.Repeat("r", 31) + "-2", "-2", "",
	} {
		if role, ok := NumberedAgentRole(name, roles); ok {
			t.Errorf("NumberedAgentRole(%q) = %s, want none: no agent of a plan takes that name", name, role)
		}
	}
}

// A role is no numbered agent of itself. A role of 32 bytes that ends in `-2`, 30 r's and -2, is
// the name NumberedAgentName gives its own second agent, and NumberedAgentRole answers none for it
// among roles that hold it. A role it is another role's numbered agent of, 30 r's, is still
// answered. The fault: the role was read back as its own numbered agent, and each caller took it
// out of the list first.
func TestARoleIsNoNumberedAgentOfItself(t *testing.T) {
	self := Role(strings.Repeat("r", 30) + "-2")
	other := Role(strings.Repeat("r", 30))
	if NumberedAgentName(self, FirstAgentNumber) != Agent(self) || NumberedAgentName(other, FirstAgentNumber) != Agent(self) {
		t.Fatalf("the second agents of %s and %s are named %q and %q, want both %q: the case's premise", self, other,
			NumberedAgentName(self, FirstAgentNumber), NumberedAgentName(other, FirstAgentNumber), self)
	}
	if role, ok := NumberedAgentRole(string(self), []Role{self}); ok {
		t.Errorf("NumberedAgentRole(%q) among itself = %s, want none: a role is no numbered agent of itself", self, role)
	}
	if role, ok := NumberedAgentRole(string(self), []Role{self, other}); !ok || role != other {
		t.Errorf("NumberedAgentRole(%q) among itself and %s = %q, %t; want %s", self, other, role, ok, other)
	}
}
