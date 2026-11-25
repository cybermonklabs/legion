package workflow

import (
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// Main's gate-moved message of an opening (workflow/gate.go:tellGateMoved at d46ec4e0 and
// c03d3afe) said who did what to which version of which document: "The design gate of %s opened: a
// person approved version %d of its %s document (%s), and every step the deployment gates now
// stands open, so the design you are working to is the approved one. A merge the closed gate held
// is released." By the architect's ruling on R8's last sentence the sentence may become fields
// "with correct prompt lines" (kind-audit-r13.md, "Settled by the ruling": "Each still needs the
// head's prompt line to describe the field correctly"). The branch's message (gateMovedOn) names
// the version in `version`, but each reader's prompt says what `version` is only for `gate:
// closed`; for `gate: open` it says only that every gated step stands open, so no reader is told
// that the opening's `version` is the version of `document` that a person approved.
// dispatch://INFRA-3686's R2: "The fields are those of the kind, the issue, the act, the actor,
// the time, the references (a `dispatch://` link for each Dispatch item), the versions and the
// state that the reader acts on", and "a step that a text gives today goes into the reader's
// prompt where the prompt does not give it".
func TestEveryReaderOfAGateOpeningIsToldThatAPersonApprovedItsVersion(t *testing.T) {
	opened := GateMovedMessage("LEGION-1", record.DesignStepSpec, "0a6c2a3e-6f3b-4d55-9b1c-2a7d1f0e9c11", 4, true)
	if opened.Gate != texts.GateOpened || opened.Version != 4 || opened.Merge != texts.MergeReleased {
		t.Fatalf("the opening's gate-moved message is %+v; want gate open at version 4 with the merge released", opened)
	}
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("prompts: %v", err)
	}
	readers := []struct {
		name string
		role claim.Role
		root bool
	}{
		{"planner", claim.RolePlanner, false}, {"implementer", claim.RoleImplementer, false}, {"tester", claim.RoleTester, false},
		{"reviewer", claim.RoleReviewer, false}, {"merger", claim.RoleMerger, false}, {"root architect", claim.RoleArchitect, true},
	}
	for _, reader := range readers {
		parts, err := composer.Compose(reader.role, reader.root)
		if err != nil {
			t.Fatalf("%s: compose: %v", reader.name, err)
		}
		sentences := gateOpenSentences(t, parts.RolePromptPaths)
		if len(sentences) == 0 {
			t.Errorf("%s: no prompt file explains a gate-moved message with `gate: open`", reader.name)
			continue
		}
		told := false
		for _, sentence := range sentences {
			if strings.Contains(sentence, "`version`") && strings.Contains(sentence, "person") {
				told = true
			}
		}
		if !told {
			t.Errorf("%s: no prompt sentence on `gate: open` says that `version` (%d in this message) is the version of `document` a person approved, "+
				"as main's message did (\"a person approved version 4 of its spec document\"); its sentences on `gate: open` are %q",
				reader.name, opened.Version, sentences)
		}
	}
}

// gateOpenSentences is each sentence of the prompt files at paths that names `gate: open`.
func gateOpenSentences(t *testing.T, paths []string) []string {
	t.Helper()
	var sentences []string
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			for _, sentence := range strings.SplitAfter(line, ". ") {
				if strings.Contains(sentence, "`gate: open`") {
					sentences = append(sentences, strings.TrimSpace(sentence))
				}
			}
		}
	}
	return sentences
}
