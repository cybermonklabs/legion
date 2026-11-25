package prompts

import (
	"os"
	"strings"
	"testing"
)

// Main's provider-error note (supervise/providererror.go:providerErrorTask at d46ec4e0) told every
// phase worker and every architect: "Your previous turn on this task stopped because of a provider
// error (HTTP %d). Your work did not cause it. The daemon sends you the task again. Read what that
// turn already did in your workspace and on the issue's branch. Then continue the task." A provider
// error ends the turn's model call and stops no process (supervise's providerErrored: "The prompt's
// turn ran and is over"), so the work to read is what that turn did, not work that stopped.
// INFRA-3686's R2: "a step that a text gives today goes into the reader's prompt where the prompt
// does not give it". The step rows of the provider-error note (texts_steps_test.go) point to "Before
// you do any of that work again, check what it already did in your workspace and on the issue's
// branch.", whose "that work" is the sentence before it: "Your tool calls in flight, and the
// subagents and background jobs that stopped". So each reader's prompt must give the step of
// reading what the turn the provider error stopped already did.
func TestTheProviderErrorNoteTellsTheReaderToReadWhatThatTurnDid(t *testing.T) {
	for _, path := range []string{"go/architect-common.md", "go/worker-common.md"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var steps []string
		given := false
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "`provider-error-note`") {
				continue
			}
			for _, sentence := range strings.SplitAfter(line, ". ") {
				if !strings.Contains(sentence, "already did") {
					continue
				}
				steps = append(steps, sentence)
				if strings.Contains(sentence, "turn") || strings.Contains(sentence, "provider") {
					given = true
				}
			}
		}
		if !given {
			t.Errorf("%s gives the provider-error note's reader no step of reading what that turn already did, as main's note did "+
				"(\"Read what that turn already did in your workspace and on the issue's branch.\"); its only read step is about the work that stopped: %q",
				path, steps)
		}
	}
}
