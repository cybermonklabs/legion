package textcheck

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The production check fails a requirement still unobserved at its last look only where
// production carries the merge, and one page states that rule outside a prompt: the concepts
// page. This is the page half of that check, kept apart from the prompt half for the reason every
// test of this package is: a test that reads a page puts the page in its job's filter, so an edit
// of a page alone would start the whole daemon Go lane. The prompt half is
// internal/prompts's TestTheProductionCheckFailsAnUnobservedRequirementOnlyWhereProductionCarriesTheMerge,
// which reads the composed tester's and implementer's prompts. Neither half alone is the
// guarantee.
//
// The unit is the clause, split at `; ` as well as at full stops, never the sentence: matched
// sentence by sentence, this check passed with the concepts page's limit taken out, since that
// page's one sentence of the three verdicts reads whether production carries the merge in its
// opening clause and limits nothing of its `fail` clause by it. Each page must hold one such
// clause at least, so a matcher that stopped finding the rule fails here rather than passing on
// nothing. Each page is held in both its readings (failRuleOfThePage), so that a rule or a limit
// only an HTML comment holds, which the rendered page does not show, fails rather than passing.
func TestThePagesFailAnUnobservedRequirementOnlyWhereProductionCarriesTheMerge(t *testing.T) {
	for _, page := range []string{"docs/site/src/content/docs/legion/concepts.md"} {
		t.Run(page, func(t *testing.T) {
			if err := failRuleOfThePage(shippedText(t, page)); err != nil {
				t.Errorf("%s %v", page, err)
			}
		})
	}
}

// A rule a reader of the rendered page cannot see is none: the page shows nothing of a clause or
// of its limit inside an HTML comment, and a clause without the limit inside a comment is still a
// clause of the page as an editor reads it. So the rule is held in both readings of the page, and
// a comment around the rule's clause, around its limit or around a clause without the limit, or one
// that splits the limit, fails for the comment. A comment that holds nothing of the rule, or a
// "<!--" inside a fenced block before it, hides nothing of it.
func TestTheFailRuleIsHeldWhereAReaderSeesIt(t *testing.T) {
	const (
		limited   = "One still unobserved at the last look is a `fail` when production carries the merge.\n"
		unlimited = "One still unobserved at the last look is a `fail`.\n"
		before    = "The check reports a verdict.\n\n"
	)
	for _, page := range []struct {
		shape, text string
		err         error
	}{
		{"the rule a reader sees", before + limited, nil},
		{"the rule without its limit", before + unlimited, errUnlimitedFail},
		{"no rule", before, errNoFailRule},
		{"the rule inside a comment", before + "<!--\n" + limited + "-->\n", errCommented},
		{"the limit inside a comment",
			before + "One still unobserved at the last look is a `fail` <!-- when production carries the merge -->.\n",
			errCommented},
		{"a rule without its limit inside a comment", before + limited + "\n<!--\n" + unlimited + "-->\n",
			errCommented},
		{"a comment splitting the limit",
			before + "One still unobserved at the last look is a `fail` when production <!-- whole -->carries the merge.\n",
			errCommented},
		{"a comment holding nothing of the rule", before + "<!-- a note -->\n" + limited, nil},
		{"a comment opened inside a fenced block before the rule",
			before + "```html\n<!--\n```\n\n" + limited, nil},
	} {
		err := failRuleOfThePage(page.text)
		if !errors.Is(err, page.err) {
			t.Errorf("%s: holding the rule answers the error %v, want %v: %q", page.shape, err, page.err, page.text)
		}
	}
}

var (
	// errNoFailRule is why failRuleIn refuses a text: no clause of it makes a requirement still
	// unobserved at the last look a fail.
	errNoFailRule = errors.New("holds no clause making a requirement still unobserved at the last look a fail")
	// errUnlimitedFail is why failRuleIn refuses a text: a clause of it makes such a requirement a
	// fail without the limit.
	errUnlimitedFail = errors.New("makes a requirement still unobserved at the last look a fail without " +
		"saying \"production carries the merge\", so it fails a merge production never carried")
)

// failRuleOfThePage holds the rule to page, a Markdown page, in both its readings (bothReadings):
// as written, and as it renders (visibleMarkdown).
func failRuleOfThePage(page string) error {
	return bothReadings(failRuleIn(page), failRuleIn(visibleMarkdown(page)))
}

// failRuleIn holds the rule to text, one reading of a page: one clause at least (ruleClauses)
// makes a requirement still unobserved at the last look a fail (errNoFailRule), and every such
// clause says production carries the merge (errUnlimitedFail, naming each clause that does not).
func failRuleIn(text string) error {
	const limit = "production carries the merge"
	fails := 0
	var unlimited []string
	for _, clause := range ruleClauses(text) {
		if strings.Contains(clause, "unobserved") && strings.Contains(clause, "last") && strings.Contains(clause, "fail") {
			fails++
			if !strings.Contains(clause, limit) {
				unlimited = append(unlimited, clause)
			}
		}
	}
	switch {
	case fails == 0:
		return errNoFailRule
	case len(unlimited) > 0:
		return fmt.Errorf("%w: %q", errUnlimitedFail, unlimited)
	}
	return nil
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
