package claim

import "testing"

// A name is defined for an issue where Legion or a source defines the role it is, or, for a
// numbered name, the role it numbers (NumberedAgentName). So `implementer-2` is a second agent of
// Legion's implementer, and `checker-2` of the checker while a source defines the checker. The
// fault: a numbered name read as a role of its own, which no source defines. The controls: a role
// no source defines is not defined, nor a numbered name of it, nor a name that numbers nothing, and
// a second source counts as the first does.
func TestANumberedNameIsDefinedWhereTheRoleItNumbersIs(t *testing.T) {
	deployment := RoleList{"checker"}
	for _, tc := range []struct {
		name    string
		sources []RoleSource
		role    Role
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
