package prompts

import (
	"path/filepath"
	"slices"
	"testing"
)

// The controller's prompt is its shared role part, which holds wherever it runs, then the daemon's
// part for how this controller was launched: in the operator's terminal by `legion controller
// start`, or headless in a pod the daemon launched (`controller: daemon`).
func TestTheControllersPromptIsItsRolePartThenItsLaunchsPart(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shared := filepath.Join(stateDir, "prompts", "shared", "controller-root.md")
	for _, tc := range []struct {
		name     string
		headless bool
		part     string
	}{
		{"started by the operator", false, "controller-interactive.md"},
		{"launched by the daemon", true, "controller-headless.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths, err := composer.ControllerPromptPaths(tc.headless)
			if err != nil {
				t.Fatalf("ControllerPromptPaths: %v", err)
			}
			if want := []string{shared, filepath.Join(stateDir, "prompts", "go", tc.part)}; !slices.Equal(paths, want) {
				t.Fatalf("paths = %q, want %q", paths, want)
			}
		})
	}
}
