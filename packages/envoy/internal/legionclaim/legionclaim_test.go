package legionclaim_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/legionclaim"
)

// A role of an issue is one of Legion's six or a name of the role grammar a deployment defines, up
// to MaxRoleBytes, and never the controller's name.
func TestIsRoleTakesTheGrammarButTheController(t *testing.T) {
	for _, role := range append(slices.Clone(legionclaim.Roles), "checker", "code-checker", strings.Repeat("r", legionclaim.MaxRoleBytes)) {
		if !legionclaim.IsRole(role) {
			t.Fatalf("IsRole(%q) = false, want a role of an issue", role)
		}
	}
	for _, role := range []string{"controller", "Checker", "1checker", "", strings.Repeat("r", legionclaim.MaxRoleBytes+1), "tester.*"} {
		if legionclaim.IsRole(role) {
			t.Fatalf("IsRole(%q) = true, want no role of an issue", role)
		}
	}
}

// A claim token of an issue is split as the daemon's claim.Token.Cut splits it: its project, the
// issue it names and its agent's name, which may hold a "-", is up to MaxRoleBytes long and is the
// role of the agent's first step, whatever role the agent's step is on now. The controller's
// token, a token whose agent's name is controller or outside the role grammar, and a token of
// another shape are no claim of an issue.
func TestCutClaimTokenSplitsATokenAsTheDaemonDoes(t *testing.T) {
	for _, tc := range []struct{ token, project, issue, agent string }{
		{"legion-infra-infra-448-tester", "infra", "INFRA-448", "tester"},
		{"legion-sjawharlegion-legion-7-architect", "sjawharlegion", "LEGION-7", "architect"},
		{"legion-acme-acme7-3-implementer", "acme", "ACME7-3", "implementer"},
		{"legion-acme-acme-12-code-checker", "acme", "ACME-12", "code-checker"},
		{"legion-acme-acme-12-" + strings.Repeat("r", legionclaim.MaxRoleBytes), "acme", "ACME-12", strings.Repeat("r", legionclaim.MaxRoleBytes)},
	} {
		project, issue, agent, ok := legionclaim.CutClaimToken(tc.token)
		if !ok || project != tc.project || issue != tc.issue || agent != tc.agent {
			t.Fatalf("CutClaimToken(%q) = %q, %q, %q, %v; want %q, %q, %q", tc.token, project, issue, agent, ok, tc.project, tc.issue, tc.agent)
		}
	}
	for _, token := range []string{
		"legion-infra-controller", "legion-infra-infra-448-controller", "legion-infra-infra-448-Checker",
		"legion-infra-infra-448", "legion-infra-448-tester", "legion-infra-infra-448-tester.*",
		"legion-infra-infra-448-" + strings.Repeat("r", legionclaim.MaxRoleBytes+1), "",
	} {
		if project, issue, agent, ok := legionclaim.CutClaimToken(token); ok {
			t.Fatalf("CutClaimToken(%q) = %q, %q, %q; want no claim of an issue", token, project, issue, agent)
		}
	}
}
