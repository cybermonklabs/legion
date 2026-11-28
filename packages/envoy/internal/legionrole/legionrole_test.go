package legionrole

import (
	"strings"
	"testing"
)

// IsRole takes each of Legion's six and a role the deployment defines, a "-" in it included, and
// refuses the controller and every name outside the grammar.
func TestIsRoleTakesARoleOfTheGrammarButTheController(t *testing.T) {
	for role, want := range map[string]bool{
		"implementer":           true,
		"checker":               true,
		"code-check":            true,
		strings.Repeat("r", 32): true,
		strings.Repeat("r", 33): false,
		"controller":            false,
		"Planner":               false,
		"1checker":              false,
		"":                      false,
	} {
		if got := IsRole(role); got != want {
			t.Errorf("IsRole(%q) = %t, want %t", role, got, want)
		}
	}
}

// ParseToken reads back the issue key and the role of a role token, the daemon's
// legion-<project>-<issue project>-<number>-<role>, for one of Legion's six and for a role the
// deployment defines, a "-" in it included, and TokenParts reads the project before them; both
// read nothing out of a token that is no issue role's: the controller's, a claim on the
// controller's name, a role outside the grammar, or an issue part that is no issue key's.
func TestParseTokenReadsTheIssueAndTheRole(t *testing.T) {
	for token, want := range map[string][3]string{
		"legion-infra-infra-448-tester":                  {"infra", "INFRA-448", "tester"},
		"legion-sjawharlegion-legion-7-architect":        {"sjawharlegion", "LEGION-7", "architect"},
		"legion-acme-acme7-3-checker":                    {"acme", "ACME7-3", "checker"},
		"legion-acme2-acme-12-code-check":                {"acme2", "ACME-12", "code-check"},
		"legion-acme-acme-12-" + strings.Repeat("r", 32): {"acme", "ACME-12", strings.Repeat("r", 32)},
	} {
		issue, role, ok := ParseToken(token)
		if !ok || issue != want[1] || role != want[2] {
			t.Errorf("ParseToken(%q) = %q, %q, %t; want %q, %q, true", token, issue, role, ok, want[1], want[2])
		}
		project, issue, role, ok := TokenParts(token)
		if !ok || [3]string{project, issue, role} != want {
			t.Errorf("TokenParts(%q) = %q, %q, %q, %t; want %q, true", token, project, issue, role, ok, want)
		}
	}
	for _, token := range []string{
		"legion-acme-controller",
		"legion-acme-acme-12-controller",
		"legion-acme-acme-12-Planner",
		"legion-acme-acme-12-" + strings.Repeat("r", 33),
		"legion-acme-acme-12",
		"legion-acme-12-planner",
		"",
	} {
		if issue, role, ok := ParseToken(token); ok {
			t.Errorf("ParseToken(%q) = %q, %q, true; want no issue role's token", token, issue, role)
		}
		if project, issue, role, ok := TokenParts(token); ok {
			t.Errorf("TokenParts(%q) = %q, %q, %q, true; want no issue role's token", token, project, issue, role)
		}
	}
}
