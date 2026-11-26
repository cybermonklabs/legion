package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The project's two refusal patterns reach every host that runs a session of the project, as the
// variables the hosts read (runtime.OwnRefusalVariable, runtime.ProviderRefusalVariable): the pane
// of a role on Oh My Pi and of one on Claude Code, and the daemon's own controller, each through
// the launch's spec (SpawnSpec, after its fork) and the pane's environment as tmux is told it
// (tmux.PaneVariables). A pattern the project leaves unset sends no variable, so a host told none
// classes no refusal by it. The pod's environment is the sandbox runtime's
// (TestSignalReporterLaunchConfig there) and the operator's controller `legion controller start`'s
// (cmd/legion's).
func TestSignalReporterLaunchConfig(t *testing.T) {
	t.Parallel()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	const own, provider = `project (token|request) limit`, `[Oo]verloaded|Too many requests`
	worker, err := claim.NewToken("s1", "S1-2", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	launches := []struct {
		name    string
		claim   supervise.Claim
		backend runtime.Backend
	}{
		{"a role on Oh My Pi", supervise.Claim{Token: worker, Project: "s1", Issue: "S1-2", Tree: "S1-1", Role: claim.RoleImplementer, Generation: 1}, runtime.BackendOmp},
		{"a role on Claude Code", supervise.Claim{Token: worker, Project: "s1", Issue: "S1-2", Tree: "S1-1", Role: claim.RoleImplementer, Generation: 1}, runtime.BackendClaude},
		{"the daemon's controller", supervise.Claim{Token: claim.ControllerToken("s1"), Project: "s1", Role: claim.RoleController, Generation: 1}, runtime.BackendOmp},
	}
	for _, tc := range []struct {
		name     string
		provider config.SignalProvider
		want     []string
	}{
		{"both patterns set", config.SignalProvider{OwnRefusal: own, ProviderRefusal: provider},
			[]string{runtime.OwnRefusalVariable + "=" + own, runtime.ProviderRefusalVariable + "=" + provider}},
		{"the provider's alone", config.SignalProvider{ProviderRefusal: provider},
			[]string{runtime.ProviderRefusalVariable + "=" + provider}},
		{"none set", config.SignalProvider{TokenLimit: 5500000, TokenModels: "^claude-"}, nil},
	} {
		for _, launch := range launches {
			t.Run(tc.name+"/"+launch.name, func(t *testing.T) {
				s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, appOwners: noAppOwners, provider: tc.provider,
					backend: func(claim.Role) runtime.Backend { return launch.backend }}
				spec, err := s.SpawnSpec(context.Background(), launch.claim)
				if err != nil {
					t.Fatalf("SpawnSpec: %v", err)
				}
				spec.Claim, spec.Project, spec.Role, spec.Generation, spec.Issue, spec.Tree =
					launch.claim.Token, launch.claim.Project, launch.claim.Role, launch.claim.Generation, launch.claim.Issue, launch.claim.Tree
				variables, err := tmux.PaneVariables(spec, tmux.PaneInputs{StateDir: t.TempDir(), Workspace: t.TempDir(),
					DaemonURL: "http://127.0.0.1:13370", EnvoyURL: "http://127.0.0.1:9020"}, nil)
				if err != nil {
					t.Fatalf("the pane's variables: %v", err)
				}
				var told []string
				for _, pair := range variables {
					if strings.HasPrefix(pair, "LEGION_SIGNAL_") {
						told = append(told, pair)
					}
				}
				slices.Sort(told)
				want := slices.Sorted(slices.Values(tc.want))
				if !slices.Equal(told, want) {
					t.Fatalf("the pane of %s is told %q, want %q", launch.name, told, want)
				}
			})
		}
	}
}
