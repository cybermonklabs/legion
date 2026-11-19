package textcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The production check fails a requirement still unobserved at its last look only where
// production carries the merge, and two pages state that rule outside a prompt: the e2e proof's
// account of the bound's look (scripts/e2e/README.md) and the concepts page. This is the page half
// of that check, kept apart from the prompt half for the reason every test of this package is: a
// test that reads a page puts the page in its job's filter, so an edit of a page alone would start
// the whole daemon Go lane. The prompt half is
// internal/prompts's TestTheProductionCheckFailsAnUnobservedRequirementOnlyWhereProductionCarriesTheMerge,
// which reads the composed tester's and implementer's prompts. Neither half alone is the
// guarantee.
//
// The unit is the clause, split at `; ` as well as at full stops, never the sentence: matched
// sentence by sentence, this check passed with the concepts page's limit taken out, since that
// page's one sentence of the three verdicts reads whether production carries the merge in its
// opening clause and limits nothing of its `fail` clause by it. Each page must hold one such
// clause at least, so a matcher that stopped finding the rule fails here rather than passing on
// nothing.
func TestThePagesFailAnUnobservedRequirementOnlyWhereProductionCarriesTheMerge(t *testing.T) {
	const limit = "production carries the merge"
	for _, page := range []string{"scripts/e2e/README.md", "docs/site/src/content/docs/legion/concepts.md"} {
		t.Run(page, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", filepath.FromSlash(page)))
			if err != nil {
				t.Fatalf("read %s: %v", page, err)
			}
			fails := 0
			for _, clause := range ruleClauses(string(body)) {
				if strings.Contains(clause, "unobserved") && strings.Contains(clause, "last") && strings.Contains(clause, "fail") {
					fails++
					if !strings.Contains(clause, limit) {
						t.Errorf("%s makes a requirement still unobserved at the last look a fail without saying %q, so it fails a merge production never carried: %q", page, limit, clause)
					}
				}
			}
			if fails == 0 {
				t.Errorf("%s holds no clause making a requirement still unobserved at the last look a fail", page)
			}
		})
	}
}

// ruleClauses splits text as internal/prompts's clauses does: by blank-line paragraph, each folded
// to one line, then after every `. ` and every `; `.
func ruleClauses(text string) []string {
	var found []string
	for _, paragraph := range strings.Split(text, "\n\n") {
		for _, sentence := range strings.SplitAfter(strings.Join(strings.Fields(paragraph), " "), ". ") {
			found = append(found, strings.SplitAfter(sentence, "; ")...)
		}
	}
	return found
}
