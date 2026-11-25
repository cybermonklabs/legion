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
// deployment defines, a "-" in it included, and reads nothing out of a token that is no issue
// role's: the controller's, a claim on the controller's name, a role outside the grammar, or an
// issue part that is no issue key's.
func TestParseTokenReadsTheIssueAndTheRole(t *testing.T) {
	for token, want := range map[string][2]string{
		"legion-infra-infra-448-tester":                  {"INFRA-448", "tester"},
		"legion-sjawharlegion-legion-7-architect":        {"LEGION-7", "architect"},
		"legion-acme-acme7-3-checker":                    {"ACME7-3", "checker"},
		"legion-acme-acme-12-code-check":                 {"ACME-12", "code-check"},
		"legion-acme-acme-12-" + strings.Repeat("r", 32): {"ACME-12", strings.Repeat("r", 32)},
	} {
		issue, role, ok := ParseToken(token)
		if !ok || issue != want[0] || role != want[1] {
			t.Errorf("ParseToken(%q) = %q, %q, %t; want %q, %q, true", token, issue, role, ok, want[0], want[1])
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
	}
}
