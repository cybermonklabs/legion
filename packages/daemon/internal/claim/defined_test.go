package claim

import "testing"

// A numbered name is defined where the role it numbers is defined. The controls are an undefined
// role, a numbered name of that role, and names that number no agent.
func TestANumberedNameIsDefinedWhereTheRoleItNumbersIs(t *testing.T) {
	deployment := RoleList{"checker"}
	for _, tc := range []struct {
		name string
		sources []RoleSource
		role Role
		defined bool
	}{
		{name: "implementer", role: RoleImplementer, defined: true},
		{name: "implementer-2", role: RoleImplementer, defined: true},
		{name: "merger-3", role: RoleMerger, defined: true},
		{name: "checker", sources: []RoleSource{deployment}, role: "checker", defined: true},
		{name: "checker-2", sources: []RoleSource{deployment}, role: "checker", defined: true},
		{name: "checker"},
		{name: "checker-2"},
		{name: "implementer-1"},
		{name: "implementer-x"},
		{name: ""},
		{name: "critic-2", sources: []RoleSource{deployment, RoleList{"critic"}}, role: "critic", defined: true},
		{name: "implementer-2", sources: []RoleSource{RoleList{"implementer-2"}}, role: "implementer-2", defined: true},
		{name: "checker-2", sources: []RoleSource{deployment, RoleList{"checker-2"}}, role: "checker-2", defined: true},
	} {
		role, defined := DefinedRole(tc.name, tc.sources...)
		if role != tc.role || defined != tc.defined {
			t.Errorf("DefinedRole(%q) over %v = %q, %t, want %q, %t", tc.name, tc.sources, role, defined, tc.role, tc.defined)
		}
	}
}
