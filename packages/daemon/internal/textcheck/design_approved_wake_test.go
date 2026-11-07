package textcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A root waiting on a person gets no design-approved notice, so the architect's instructions must
// take the resume task that says the gate is open as that wake: the skill's release rule, the
// design gate reference, the wake routing table and the root architect's prompt each name the
// resume's phrase. The claude-envoy plugin's copies under packages/claude-envoy/legion/ are
// generated from these three skill files, and nothing commits that tree, so a pull request has no
// copies to read here. (Round 2, adversarial P2-1 and outside-whole P2-1.)
func TestTheArchitectsInstructionsTakeTheResumeAsTheDesignApprovedWake(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	for _, file := range []string{
		"skills/legion-architect/SKILL.md",
		"skills/legion-architect/references/design-gate.md",
		"skills/legion-architect/references/wake-routing.md",
		"packages/daemon/internal/prompts/roles/architect-root.md",
	} {
		body, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if !strings.Contains(string(body), workflow.DesignApprovedWake) {
			t.Errorf("%s does not name the resume's design-approved wake (%q)", file, workflow.DesignApprovedWake)
		}
	}
}
