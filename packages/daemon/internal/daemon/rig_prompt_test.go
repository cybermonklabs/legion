package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan" // the package's own `plan` type holds the bare name
)

// The skill-scenario rig's `prompt` mode (packages/pi-legion/scripts/skill-scenarios/daemon-pane.go)
// prints the prompt a launch of a role starts with, composed as the daemon composes it. For the
// architect of the rig's tree root, and for the architect of a child issue under that root, its
// text is byte for byte the prompt this daemon composes (SpawnSpec, then runtime.ComposePrompt) for
// the same claim under the rig's deployment: the project `rig`, the repository skills-rig/project
// on its default branch trunk, the one owner skills-rig its App reaches, the roles checker (testing)
// and critic (review) of the rig's deployment, no listed model, no `gates.design`, no
// `production_check` (so the tester), no instructions file, no experiment, and every role on Oh My
// Pi. So both architects are told the deployment's roles and models after their addressing, the
// root's architect its design gate steps after those, and both the production check last. Built
// with daemon-pane-architect.go, the rig composes that part with the daemon's own
// ArchitectAddressing; its own restatements of an older daemon (deploymentRolesLine,
// productionCheckLine) tell no models sentence, so a rig that took them here would differ from the
// daemon's prompt at that sentence.
func TestTheRigsArchitectPromptIsTheDaemons(t *testing.T) {
	t.Parallel()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	s := specs{
		stateDir: t.TempDir(), project: "rig", prompts: composer, designSteps: config.Gates{}.Steps(),
		roles: rigDeploymentRoles, productionCheck: claim.RoleTester,
		repository: func(context.Context, string) (ghrepo.Repository, string, error) {
			return ghrepo.MustParse("skills-rig/project"), "trunk", nil
		},
		appOwners: func(context.Context, appauth.AppRole, claim.Role) []string { return []string{"skills-rig"} },
	}
	for _, tc := range []struct{ name, issue, mode string }{
		{"the root's architect", "RIG-1", "root"},
		{"a child's architect", "RIG-2", "child"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := claim.NewToken("rig", tc.issue, claim.RoleArchitect)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := s.SpawnSpec(context.Background(), supervise.Claim{
				Token: token, Project: "rig", Tree: "RIG-1", Issue: tc.issue, Role: claim.RoleArchitect, Generation: 1,
			})
			if err != nil {
				t.Fatalf("SpawnSpec: %v", err)
			}
			want, err := runtime.ComposePrompt(spec.Prompt)
			if err != nil {
				t.Fatalf("ComposePrompt: %v", err)
			}
			got, stderr, err := runRig(t, true, "prompt", "architect", tc.mode)
			if err != nil {
				t.Fatalf("the rig's prompt of %s: %v: %s", tc.name, err, stderr)
			}
			if got != want {
				at := firstDifference(got, want)
				t.Errorf("the rig's prompt of %s differs from the daemon's at byte %d (the rig's is %d bytes, the daemon's %d):\nrig:    %q\ndaemon: %q",
					tc.name, at, len(got), len(want), around(got, at), around(want, at))
			}
		})
	}
}

// rigDeploymentRoles is the rig's deployment (daemon-pane.go's rigRoles) as the settings define it:
// checker, of the type testing, and critic, of the type review, neither with a model.
var rigDeploymentRoles = []config.Role{{Name: "checker", Type: stepplan.TypeTesting}, {Name: "critic", Type: stepplan.TypeReview}}

// Built without daemon-pane-architect.go, as daemon-pane.ts builds the rig inside a checkout of
// the daemon module that declares no ArchitectAddressing (main's, or an older branch's), the rig
// still composes an architect's prompt, with its own restatements of such a daemon's sentences
// (deploymentRolesLine, productionCheckLine): after the addressing, the roles of the rig's
// deployment, the root's design gate steps, then the production check, each the sentence this
// daemon's own function gives, and no models sentence, which such a daemon tells none of. It still
// prints another role's prompt.
func TestTheRigComposesAnArchitectsPromptByItsRestatementsWithoutTheDaemonsComposition(t *testing.T) {
	t.Parallel()
	got, stderr, err := runRig(t, false, "prompt", "architect", "root")
	if err != nil {
		t.Fatalf("the rig's prompt of the root's architect with no ArchitectAddressing to compose it: %v: %s", err, stderr)
	}
	tail := " " + DeploymentRolesFragment(rigDeploymentRoles) + " " + DesignGateFragment(config.Gates{}.Steps()) + " " + ProductionCheckFragment(claim.RoleTester)
	if !strings.HasSuffix(got, tail) {
		t.Errorf("the rig's prompt of the root's architect ends %q, want it to end with %q", around(got, len(got)-len(tail)), tail)
	}
	if models := ModelsFragment(nil); strings.Contains(got, models) {
		t.Errorf("the rig's prompt of the root's architect holds %q, which only ArchitectAddressing tells", models)
	}
	reviewer, stderr, err := runRig(t, false, "prompt", "reviewer", "child")
	if err != nil {
		t.Fatalf("the rig's prompt of a reviewer: %v: %s", err, stderr)
	}
	if topic := "notifications.role.legion-rig-rig-2-reviewer"; !strings.Contains(reviewer, topic) {
		t.Errorf("the rig's prompt of a reviewer ends %q, want its addressing, which names its role topic %s", around(reviewer, len(reviewer)), topic)
	}
}

// runRig runs the skill-scenario rig's program with args as daemon-pane.ts runs it: `go run
// -overlay` builds daemon-pane.go as the main of cmd/daemon-pane inside this module, the one place
// the daemon's internal packages can be imported from, with daemon-pane-architect.go beside it when
// architect is set, as daemon-pane.ts builds it inside a checkout whose daemon declares
// ArchitectAddressing, which this one does. It answers the program's stdout, its stderr, and its
// error when it exits non-zero.
func runRig(t *testing.T, architect bool, args ...string) (string, string, error) {
	t.Helper()
	module, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	scenarios := filepath.Join(module, "..", "pi-legion", "scripts", "skill-scenarios")
	replace := map[string]string{
		filepath.Join(module, "cmd", "daemon-pane", "main.go"): filepath.Join(scenarios, "daemon-pane.go"),
	}
	if architect {
		replace[filepath.Join(module, "cmd", "daemon-pane", "architect.go")] = filepath.Join(scenarios, "daemon-pane-architect.go")
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": replace})
	if err != nil {
		t.Fatal(err)
	}
	overlayFile := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlayFile, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", append([]string{"run", "-overlay", overlayFile, "./cmd/daemon-pane"}, args...)...)
	cmd.Dir = module
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("run the rig: %v", err)
	}
	return stdout.String(), stderr.String(), err
}

// firstDifference is the first byte at which a and b differ, the shorter one's length when one is
// the other's prefix.
func firstDifference(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}

// around is s from 120 bytes before at to 400 after it, enough of a long prompt to read where it
// differs.
func around(s string, at int) string {
	return s[max(0, at-120):min(len(s), at+400)]
}
