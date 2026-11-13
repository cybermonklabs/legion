package textcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A root waiting on a person gets no design-approved notice, so the architect's instructions must
// take the resume task that says the gate is open as that wake: the skill's release rule, the
// design gate reference, the wake routing table and the root architect's prompt each name the
// resume's phrase. The claude-envoy plugin's copies under packages/claude-envoy/legion/ are
// generated from these three skill files, and nothing commits that tree, so a pull request has no
// copies to read here. (Round 2, adversarial P2-1 and outside-whole P2-1.)
//
// The phrase is read from internal/workflow's design_approved_wake_extract.txt, which that
// package's TestTheDesignApprovedWakeExtractIsTheConstant holds to DesignApprovedWake, rather than
// imported: an import of internal/workflow compiles the daemon's closure, the store included, into
// this package, and this job is built with no database and reads no product code.
func TestTheArchitectsInstructionsTakeTheResumeAsTheDesignApprovedWake(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	const extract = "../workflow/design_approved_wake_extract.txt"
	raw, err := os.ReadFile(extract)
	if err != nil {
		t.Fatalf("read %s: %v", extract, err)
	}
	_, phrase, ok := strings.Cut(string(raw), "\n\n")
	phrase = strings.TrimSuffix(phrase, "\n")
	if !ok || phrase == "" {
		t.Fatalf("%s holds no phrase under its `#` header", extract)
	}
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
		if !strings.Contains(string(body), phrase) {
			t.Errorf("%s does not name the resume's design-approved wake (%q)", file, phrase)
		}
	}
}
