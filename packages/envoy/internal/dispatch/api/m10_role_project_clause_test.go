package api

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
)

// TestM10TheAPISplicesTheRoleRulesNameHalf: the api's two reads of a role whose holder the Envoy
// listener names rather than a held_by tuple - the reference read that leaves a route's or a
// mention's role out (seenReferencesSQL's role_project arm) and a delivery's resolve
// (roleReachesReaderSQL) - judge the role's name by access.RoleProjectClause itself, the term
// access.RoleClause splices beside its own holder lookup (access's
// TestM10TheRoleRuleReadsARolesNameThroughOneTerm), and keep no copy of that rule of their own.
//
// Control: roleReachesReaderSQL splicing the term over `$3` rather than `$3::text`; its row then
// fails.
func TestM10TheAPISplicesTheRoleRulesNameHalf(t *testing.T) {
	t.Parallel()
	for name, read := range map[string]struct{ statement, term string }{
		"seenReferencesSQL":    {seenReferencesSQL, access.RoleProjectClause("named.id", "$6")},
		"roleReachesReaderSQL": {roleReachesReaderSQL, access.RoleProjectClause("$3::text", "$2")},
	} {
		if !strings.Contains(read.statement, read.term) {
			t.Errorf("%s does not splice access.RoleProjectClause as access spells it:\n%s\nterm: %s", name, read.statement, read.term)
		}
		if count := strings.Count(read.statement, "hidden.visibility = 'private'"); count != 1 {
			t.Errorf("%s reads a private project's role %d times, want once, through access.RoleProjectClause", name, count)
		}
	}
}
