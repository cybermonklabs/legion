package runtime_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The table of every role's identity reads back as it was written: one JSON object, keys sorted,
// each value the role's name and email. The reader passes over a field of an identity it does not
// know, since a role's image may run a `legion` of another release than the daemon's, and refuses
// every other value it cannot use with a reason that names none of the value's names or emails.
func TestRoleIdentitiesReadBackAndPassOverAnUnknownField(t *testing.T) {
	identities := map[claim.Role]runtime.GitIdentity{
		claim.RoleReviewer:    {Name: "rev", Email: "rev@x"},
		claim.RoleImplementer: {Name: "impl", Email: "impl@x"},
	}
	const written = `{"implementer":{"name":"impl","email":"impl@x"},"reviewer":{"name":"rev","email":"rev@x"}}`
	if got := runtime.RoleIdentities(identities); got != written {
		t.Fatalf("RoleIdentities = %s, want %s", got, written)
	}
	if read, why := runtime.ReadRoleIdentities(written); why != "" || !maps.Equal(read, identities) {
		t.Fatalf("ReadRoleIdentities(%s) = %v, %q; want %v", written, read, why, identities)
	}
	const later = `{"implementer":{"name":"a","email":"b","login":"c"}}`
	want := map[claim.Role]runtime.GitIdentity{claim.RoleImplementer: {Name: "a", Email: "b"}}
	if read, why := runtime.ReadRoleIdentities(later); why != "" || !maps.Equal(read, want) {
		t.Fatalf("ReadRoleIdentities(%s) = %v, %q; want %v: a field it does not know is passed over", later, read, why, want)
	}
	// A role the deployment defines is a role of the table, since a launch writes it beside Legion's
	// six (the daemon's specs.roleIdentities): the read takes every name of the role grammar.
	deployed := `{"checker":{"name":"a","email":"b"}}`
	if read, why := runtime.ReadRoleIdentities(deployed); why != "" || read["checker"] != (runtime.GitIdentity{Name: "a", Email: "b"}) {
		t.Fatalf("ReadRoleIdentities(%s) = %v, %q; want the checker's identity", deployed, read, why)
	}

	const notTable = "not a JSON object of each role's name and email"
	for _, tc := range []struct{ value, want string }{
		{"", "empty"},
		{"implementer=legion-implementer[bot]", notTable},
		{"null", notTable},
		{`["implementer"]`, notTable},
		{`{"implementer":{"name":"secret-name","email":"secret@x"}} {}`, notTable},
		{`{"implementer":"secret-name"}`, notTable},
		{`{"Wizard":{"name":"secret-name","email":"secret@x"}}`, `a table with "Wizard", which is no role`},
		{`{"controller":{"name":"secret-name","email":"secret@x"}}`, `a table with "controller", which is no role`},
		{`{"implementer":null}`, "a table with an empty name for implementer"},
		{`{"implementer":{"name":"","email":"secret@x"}}`, "a table with an empty name for implementer"},
		{`{"implementer":{"name":"secret-name","email":""}}`, "a table with an empty email for implementer"},
		{`{"implementer":{"name":"secret-name\nLegion-Role: reviewer","email":"secret@x"}}`, "a table with a line break in the name of implementer"},
		{`{"implementer":{"name":"secret-name","email":"secret@x\r"}}`, "a table with a line break in the email of implementer"},
	} {
		read, why := runtime.ReadRoleIdentities(tc.value)
		if why != tc.want || read != nil {
			t.Errorf("ReadRoleIdentities(%q) = %v, %q; want no table and %q", tc.value, read, why, tc.want)
		}
		if strings.Contains(why, "secret") {
			t.Errorf("ReadRoleIdentities(%q)'s reason %q names a value of the table", tc.value, why)
		}
	}
}
