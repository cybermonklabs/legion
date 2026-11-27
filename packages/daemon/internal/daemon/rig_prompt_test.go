package daemon

//ci:reads packages/pi-legion/scripts/skill-scenarios/daemon-pane.go runRig runs go run -overlay ./cmd/daemon-pane in packages/daemon, the skill-scenario rig's program, with it as cmd/daemon-pane/main.go
//ci:reads packages/pi-legion/scripts/skill-scenarios/daemon-pane-architect.go runRig builds it in beside the program as cmd/daemon-pane/architect.go when architect is set

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
	stepplan "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The skill-scenario rig's `prompt` mode (packages/pi-legion/scripts/skill-scenarios/daemon-pane.go)
// prints the prompt a launch of a role starts with, composed as the daemon composes it. For the
// architect of the rig's tree root, and for the architect of a child issue under that root, its
// text is byte for byte the prompt this daemon composes (SpawnSpec, then runtime.ComposePrompt) for
// the same claim under the rig's deployment: the project `rig`, the repository example/widgets on
// its default branch trunk, the one owner example its App reaches, the roles checker (of the
// type testing) and critic (of the type review) with no model, no listed model, no `gates.design`,
// the production check of a project that sets none (the tester), no instructions file, no
// experiment, and every role on Oh My Pi. So both architects are told the deployment's roles and
// models after their addressing, the root's architect its design gate steps after those, and both
// the production check last.
func TestTheRigsArchitectPromptIsTheDaemons(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	s := specs{
		stateDir: t.TempDir(), project: "rig", prompts: composer, designSteps: config.Gates{}.Steps(),
		roles: []config.Role{
			{Name: "checker", Type: stepplan.TypeTesting},
			{Name: "critic", Type: stepplan.TypeReview},
		},
		productionCheck: claim.RoleTester,
		repository: func(context.Context, string) (ghrepo.Repository, string, error) {
			return ghrepo.MustParse("example/widgets"), "trunk", nil
		},
		appOwners: func(context.Context, appauth.AppRole, claim.Role) []string { return []string{"example"} },
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

// Built without daemon-pane-architect.go, as daemon-pane.ts builds the rig inside a checkout of
// this module that declares no ArchitectAddressing, the rig composes no architect's prompt: it
// fails, naming the function the module lacks and that the rig must run from a checkout that
// carries it, rather than print a prompt without the sentences that function adds; and it still
// prints another role's prompt.
func TestTheRigRefusesAnArchitectsPromptWithoutTheDaemonsComposition(t *testing.T) {
	got, stderr, err := runRig(t, false, "prompt", "architect", "root")
	if err == nil {
		t.Fatalf("the rig printed an architect's prompt with no ArchitectAddressing to compose it:\n%s", around(got, len(got)))
	}
	if want := "this checkout has no daemon.ArchitectAddressing, so the rig cannot compose an architect's prompt; run the rig from a checkout that carries it"; !strings.Contains(stderr, want) || got != "" {
		t.Errorf("the rig failed with %v, stdout %q and stderr %q; want no prompt and stderr saying %q", err, got, stderr, want)
	}
	reviewer, stderr, err := runRig(t, false, "prompt", "reviewer", "child")
	if err != nil {
		t.Fatalf("the rig's prompt of a reviewer: %v: %s", err, stderr)
	}
	if !strings.Contains(reviewer, "\n\nLegion addressing: your role topic is `notifications.role.legion-rig-rig-2-reviewer`") {
		t.Errorf("the rig's prompt of a reviewer ends %q, want its addressing", around(reviewer, len(reviewer)))
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
