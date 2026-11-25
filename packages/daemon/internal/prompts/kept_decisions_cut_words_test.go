package prompts

import (
	"os"
	"strings"
	"testing"
)

// Main's KeptDecisionsTask (workflow/parked_decision.go at d46ec4e0) listed each decision kept
// while nobody in the tree could hear it by its dispatch:// reference and ended "Read each before
// you continue." The branch carries each decision's words in an answer block, cut where they are
// long with rest= or more=, so the step of reading the reference is left only for cut words.
// INFRA-3686's R2: "a step that a text gives today goes into the reader's prompt where the prompt
// does not give it"; its R1 has a cut text carry "the count of the rest … and the item's
// reference". The root architect's prompt must then say where the rest of a cut kept decision is
// read, as roles/architect-root.md says it for the resume ("Where that block names a `rest` or
// `more`, the words were cut: read the rest at the ask's link.").
func TestTheKeptDecisionsPassageSaysWhereTheRestOfCutWordsIs(t *testing.T) {
	for _, path := range []string{"roles/architect-root.md", "go/architect-root.md", "go/architect-common.md"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "`kept-decisions`") && (strings.Contains(line, "`rest`") || strings.Contains(line, "`more`")) {
				return
			}
		}
	}
	t.Errorf("no line of the root architect's prompt that names the `kept-decisions` task says what a kept decision's " +
		"`rest` or `more` means or where its cut words are read; main's KeptDecisionsTask said \"Read each before you continue.\"")
}
