package runtime

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The refusal of a role Claude Code does not run lists what it runs: the five phase workers and
// each role of the deployment, which it runs too. Oh My Pi, which runs every role, names no role
// of the deployment apart from the others.
func TestTheRefusalOfARoleClaudeCodeDoesNotRunNamesTheRolesOfTheDeployment(t *testing.T) {
	claude := Backends[BackendClaude]
	if got, want := claude.RunsWhat(), "planner, implementer, tester, reviewer, merger and each role of the deployment"; got != want {
		t.Errorf("Claude Code RunsWhat() = %q, want %q", got, want)
	}
	if got := Backends[BackendOmp].RunsWhat(); strings.Contains(got, "deployment") {
		t.Errorf("Oh My Pi RunsWhat() = %q, which names the roles of the deployment apart", got)
	}
	for role, want := range map[claim.Role]bool{
		claim.RoleImplementer: true, "checker": true, "code-checker": true,
		claim.RoleArchitect: false, claim.RoleController: false, "Checker": false,
	} {
		if got := claude.Runs(role); got != want {
			t.Errorf("Claude Code Runs(%q) = %v, want %v", role, got, want)
		}
	}
}
