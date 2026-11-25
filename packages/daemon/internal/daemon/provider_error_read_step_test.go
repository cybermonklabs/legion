package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// Main's provider-error note (supervise/providererror.go:providerErrorTask at d46ec4e0) told
// every reader, architects among them, "Read what that turn already did in your workspace and on
// the issue's branch. Then continue the task." INFRA-3686's R2: "a step that a text gives today
// goes into the reader's prompt where the prompt does not give it." The root and the child
// architect's prompt files must give that step's place, and the goldenSteps row that claims the
// step for the architect must name a passage that gives it.
func TestTheArchitectIsToldWhereToReadWhatAProviderErrorTurnDid(t *testing.T) {
	const prompts = "packages/daemon/internal/prompts/"
	readers := []struct {
		name  string
		files []string
	}{
		{"root architect", []string{"roles/architect-root.md", "go/architect-root.md", "go/architect-common.md"}},
		{"child architect", []string{"roles/architect.md", "go/architect.md", "go/architect-common.md"}},
	}
	const place = "in your workspace and on the issue's branch"
	for _, reader := range readers {
		held := false
		for _, file := range reader.files {
			body, err := os.ReadFile(filepath.Join(goldenRepositoryRoot, filepath.FromSlash(prompts+file)))
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			if strings.Contains(string(body), place) {
				held = true
			}
		}
		if !held {
			t.Errorf("the %s's prompt (%s) does not say where to read what a provider-error turn already did (%q), which main's provider-error sentence told it",
				reader.name, strings.Join(reader.files, ", "), place)
		}
	}
	found := false
	for _, step := range goldenSteps {
		if step.kind != texts.ProviderErrorNote || step.readers != "architect" ||
			!strings.HasPrefix(step.sentence, "Read what that turn already did") {
			continue
		}
		found = true
		if !strings.Contains(step.passage, "already did") {
			t.Errorf("goldenSteps claims %s's step %q for the architect is held by %s's passage %q, which gives no step of reading what that turn already did",
				step.kind, step.sentence, step.file, step.passage)
		}
	}
	if !found {
		t.Fatal("no goldenSteps row of the provider-error-note's read step for the architect")
	}
}
