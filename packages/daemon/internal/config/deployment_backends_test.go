package config

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A deployment whose one role on Claude Code is a role of its own boots a Claude Code launcher
// beside Oh My Pi's: Backends lists the backend of each role of `roles` as it lists Legion's, and
// Claude Code runs such a role, while it still runs neither the architect nor the controller.
func TestADeploymentWhoseOnlyClaudeRoleIsADeploymentRoleBootsAClaudeLauncher(t *testing.T) {
	cfg := Config{
		Roles:          []Role{{Name: "checker", Type: plan.TypeTesting, PromptFile: "/etc/legion/checker.md", GitHubApp: RoleAppReview}},
		WorkerBackends: map[claim.Role]runtime.Backend{"checker": runtime.BackendClaude},
	}
	if got, want := cfg.Backends(), []runtime.Backend{runtime.BackendOmp, runtime.BackendClaude}; !slices.Equal(got, want) {
		t.Errorf("Backends() = %q, want %q", got, want)
	}
	claude := runtime.Backends[runtime.BackendClaude]
	for role, want := range map[claim.Role]bool{"checker": true, claim.RoleArchitect: false, claim.RoleController: false} {
		if got := claude.Runs(role); got != want {
			t.Errorf("Claude Code Runs(%s) = %v, want %v", role, got, want)
		}
	}
}
