package access

import (
	"strings"
	"testing"
)

// TestM10TheRoleRuleReadsARolesNameThroughOneTerm: the half of a role's rule that reads the role's
// name - a role of a private project the viewer does not see reaches it as no role - has one
// spelling, RoleProjectClause. RoleClause splices it verbatim over the project the role's name
// gives (roleProjectSQL), whatever expression names the role, and the api's reference and resolve
// paths splice it over the project key they hold (api's
// TestM10TheAPISplicesTheRoleRulesNameHalf). A copy written beside it - the same rule spelled
// again, or this one spelled another way - is not this text, and fails here. The term names itself
// after Marker, as every fragment of this package does.
//
// Control: RoleClause splicing the term over the project wrapped in one more pair of parentheses -
// the same rule, another spelling; every role expression below then fails.
func TestM10TheRoleRuleReadsARolesNameThroughOneTerm(t *testing.T) {
	for _, role := range []string{"r.id", "$2", "substr(n.address, 6)", "null::text"} {
		term := RoleProjectClause(roleProjectSQL(role), "$1")
		if clause := RoleClause(role, "$1"); !strings.Contains(clause, term) {
			t.Errorf("RoleClause(%q) does not splice RoleProjectClause(roleProjectSQL(%q)) as it is spelled:\nRoleClause: %s\nterm: %s", role, role, clause, term)
		}
	}
	if term := RoleProjectClause("p.key", "$1"); !strings.HasPrefix(term, Marker+"RoleProjectClause */") {
		t.Errorf("RoleProjectClause = %q, want it to start with %q", term[:min(len(term), 60)], Marker+"RoleProjectClause */")
	}
}
