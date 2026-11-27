package prompts

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The prompt of a role an owner's plan defines is composed as a role of the deployment's is
// (ComposeStep): the headless mechanics, the role's own prompt, then Legion's worker part, which
// carries the SEPARATE-POD line, so a role a plan defines can ask for a pod of its own as each
// phase worker can.
func TestARoleOfAPlansComposedPromptHoldsTheSeparatePodLine(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prober := filepath.Join(t.TempDir(), "prober.md")
	if err := os.WriteFile(prober, []byte("# Prober\n\nProbe the change."), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := composer.ComposeStep(prober).RolePromptPaths
	want := []string{filepath.Join(composer.sharedDir, "mechanics", "headless.md"), prober, filepath.Join(composer.goDir, "worker-common.md")}
	if !slices.Equal(paths, want) {
		t.Fatalf("ComposeStep(prober.md) = %q, want %q", paths, want)
	}
	var text strings.Builder
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text.Write(body)
		text.WriteString("\n")
	}
	composed := text.String()
	own, line := strings.Index(composed, "# Prober"), strings.Index(composed, "SEPARATE-POD <ISSUE> <task> <reason>")
	if own < 0 || line < own || !strings.Contains(composed, "Legion starts no pod for this line") {
		t.Fatalf("the prober's composed prompt holds its own text at %d and the SEPARATE-POD line at %d, want the text first and the line after it, with %q",
			own, line, "Legion starts no pod for this line")
	}
}
