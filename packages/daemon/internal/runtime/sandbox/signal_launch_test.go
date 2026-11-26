package sandbox

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The project's two refusal patterns reach the host of every pod that runs a session of the
// project as the variables the hosts read (runtime.OwnRefusalVariable,
// runtime.ProviderRefusalVariable), with the values the daemon's launch gives them
// (runtime.RefusalEnvironment, which SpawnSpec merges after its fork): the worker process of a role
// on Oh My Pi and of one on Claude Code, and of the controller. They are a spec's own variables,
// no name of this runtime's (runtimeOwned), so a spec that carries them is launched, and a pattern
// the project leaves unset sends no variable. The pane's environment is the daemon package's
// TestSignalReporterLaunchConfig, and the operator's controller cmd/legion's.
func TestSignalReporterLaunchConfig(t *testing.T) {
	t.Parallel()
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	const own, provider = `project (token|request) limit`, `[Oo]verloaded|Too many requests`
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"both patterns set", runtime.RefusalEnvironment(own, provider)},
		{"the own pattern alone", runtime.RefusalEnvironment(own, "")},
		{"none set", runtime.RefusalEnvironment("", "")},
	} {
		for kind, spec := range map[string]func(*testing.T) runtime.SpawnSpec{
			"a role on Oh My Pi": workerSpec, "a role on Claude Code": claudeWorkerSpec, "the controller": controllerSpec,
		} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				launched := spec(t)
				maps.Copy(launched.Env, tc.env)
				var told []string
				for name, value := range envOf(workerOf(t, r, launched)) {
					if strings.HasPrefix(name, "LEGION_SIGNAL_") {
						told = append(told, name+"="+value)
					}
				}
				var want []string
				for name, value := range tc.env {
					want = append(want, name+"="+value)
				}
				slices.Sort(told)
				slices.Sort(want)
				if !slices.Equal(told, want) {
					t.Fatalf("the worker process of %s is told %q, want %q", kind, told, want)
				}
			})
		}
	}
}
