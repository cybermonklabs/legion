package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The settings define a role whose name and type nothing else in Legion carries. The daemon
// composes the whole prompt of each launch, as the runtime starts it (runtime.ComposePrompt), and
// every owner's prompt, the root architect's and a child's architect's, holds that role's name
// with its type, while no prompt of Legion's other roles holds the name. The test compares no
// words of Legion's own texts: it reads only the planted marks and the `<name> (<type>)` form the
// composer writes each role in. Taking out the line of SpawnSpec that gives an owner's launch the
// deployment's roles fails it, since no owner's prompt then names the planted role.
func TestEveryOwnersComposedPromptHoldsThePlantedRoleOfTheDeployment(t *testing.T) {
	t.Parallel()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name, kind = "zqplantedrole", "zqplantedtype"
	promptFile := filepath.Join(t.TempDir(), name+".md")
	if err := os.WriteFile(promptFile, []byte("The planted role's own prompt.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer,
		roles:       []config.Role{{Name: claim.Role(name), Type: kind, PromptFile: promptFile}},
		designSteps: config.DesignGateRootIssues.Steps()}
	composed := func(issue string, role claim.Role) string {
		t.Helper()
		token, err := claim.NewToken("s1", issue, role)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: issue, Tree: "S1-1", Role: role})
		if err != nil {
			t.Fatalf("the %s's spawn spec on %s: %v", role, issue, err)
		}
		prompt, err := runtime.ComposePrompt(spec.Prompt)
		if err != nil {
			t.Fatalf("compose the %s's prompt on %s: %v", role, issue, err)
		}
		return prompt
	}

	planted := name + " (" + kind + ")"
	for _, owner := range []struct{ what, issue string }{{"the root architect", "S1-1"}, {"a child's architect", "S1-2"}} {
		if prompt := composed(owner.issue, claim.RoleArchitect); strings.Count(prompt, planted) != 1 {
			t.Errorf("%s's composed prompt names %q %d times, want once", owner.what, planted, strings.Count(prompt, planted))
		}
	}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
		if prompt := composed("S1-2", role); strings.Contains(prompt, name) {
			t.Errorf("the %s's composed prompt names the planted role %q; only an owner is told the deployment's roles", role, name)
		}
	}
}
