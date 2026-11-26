package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The operator's controller, which `legion controller start` launches, is told the project's two
// refusal patterns as every role's host is (runtime.OwnRefusalVariable,
// runtime.ProviderRefusalVariable), from its own file's keys of the same names, since its host
// classes the refusals of its own session too; its load probe is told them as well, being the
// controller's environment before the mint. A pattern the file leaves unset sends no variable, and
// a pattern the start inherits from the shell it runs in, a Legion pane's among them, reaches
// neither: the patterns are the file's. The panes' and pods' environments are the daemon's and the
// sandbox runtime's TestSignalReporterLaunchConfig.
func TestSignalReporterLaunchConfig(t *testing.T) {
	const own, provider = `project (token|request) limit`, `[Oo]verloaded|Too many requests`
	for _, tc := range []struct {
		name  string
		extra []string
		want  []string
	}{
		{"both patterns set", []string{"signal_own_refusal_pattern: '" + own + "'", "signal_provider_refusal_pattern: '" + provider + "'"},
			[]string{runtime.OwnRefusalVariable + "=" + own, runtime.ProviderRefusalVariable + "=" + provider}},
		{"the provider's alone", []string{"signal_provider_refusal_pattern: '" + provider + "'"},
			[]string{runtime.ProviderRefusalVariable + "=" + provider}},
		{"none set", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newControllerDaemon(t)
			c := newControllerStart(t, d, controllerOptions{extra: tc.extra})
			t.Setenv(runtime.OwnRefusalVariable, "a pattern the operator's shell inherited")
			t.Setenv(runtime.ProviderRefusalVariable, "another pattern the operator's shell inherited")
			if code, _, errb := c.run(); code != 0 {
				t.Fatalf("legion controller start = %d, stderr %q", code, errb)
			}
			want := slices.Sorted(slices.Values(tc.want))
			for kind, env := range map[string]map[string]string{"load probe": c.probeEnv(), "controller": c.env()} {
				var told []string
				for name, value := range env {
					if strings.HasPrefix(name, "LEGION_SIGNAL_") {
						told = append(told, name+"="+value)
					}
				}
				slices.Sort(told)
				if !slices.Equal(told, want) {
					t.Errorf("the %s is told %q, want %q", kind, told, want)
				}
			}
		})
	}
}
