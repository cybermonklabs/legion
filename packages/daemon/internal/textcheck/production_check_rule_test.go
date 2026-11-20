package textcheck

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The production check fails a requirement that only the deploy can show when a look observes it
// as not met. It also fails one that is still unobserved at the last look, whether or not
// production carries the merge, and the last look's pending is only for an issue with no such
// requirement. Four pages and the two proof scripts state that rule, or the part of it they are
// about, outside a prompt: the concepts page, the walkthrough, the using-Legion page, the e2e
// proof's account of the bound's look (scripts/e2e/README.md) and the tester instructions of the
// two proofs. This is the page half of that check. It stays apart from the prompt half for the
// reason every test of this package does: a test that reads a page puts the page in its job's
// filter, so an edit of a page alone would start the whole daemon Go lane. The prompt half is
// internal/prompts's
// TestTheProductionCheckFailsAnUnobservedRequirementWhetherOrNotProductionCarriesTheMerge, which
// reads the composed tester's and implementer's prompts, and
// TestTheProductionCheckRuleHoldsInEveryPromptPart, which reads every prompt part. Neither half
// alone is the guarantee.
//
// The unit is the clause, cut at `; ` as well as at full stops, never the sentence (ruleClauses):
// matched sentence by sentence, a test of this rule passed with the concepts page's limit taken
// out, since that page's one sentence of the three verdicts reads whether production carries the
// merge in its opening clause. No clause states a limit the rule no longer has (bannedCheckLimits).
// A fail clause, one that holds "unobserved", "last" and "fail", holds no "production carries the
// merge" outside the words "whether or not production carries the merge". Each page holds one
// fail clause with those words at least, so a page that does not state the rule fails here rather
// than passing on nothing. The concepts page lists the three verdicts, so it also holds a fail
// clause for a requirement observed as not met. Each page is held in both its readings
// (failRuleOfThePage), so a rule or a limit that only an HTML comment holds, which the rendered
// page does not show, fails rather than passing. A proof script states that its issues have no
// requirement that only the deploy can show (proofSentence), the one case where its last look's
// pending is true. It is read once, each line without its leading white space and one `#`
// (scriptProse), so that its comments and its heredocs read as prose.
func TestThePagesFailAnUnobservedRequirementWhetherOrNotProductionCarriesTheMerge(t *testing.T) {
	for _, text := range []struct {
		path string
		// script marks a shell script, read as prose (failRuleOfTheScript).
		script bool
		// listsVerdicts marks the page that lists the three verdicts of a look.
		listsVerdicts bool
	}{
		{path: "docs/site/src/content/docs/legion/concepts.md", listsVerdicts: true},
		{path: "docs/site/src/content/docs/legion/walkthrough.md"},
		{path: "docs/site/src/content/docs/legion/using-legion.md"},
		{path: "scripts/e2e/README.md"},
		{path: "scripts/e2e/stage3-devbox-workflow.sh", script: true},
		{path: "scripts/e2e/stage4b-sandbox-tree.sh", script: true},
	} {
		t.Run(text.path, func(t *testing.T) {
			body := shippedText(t, text.path)
			var err error
			if text.script {
				err = failRuleOfTheScript(body)
			} else {
				err = failRuleOfThePage(body, text.listsVerdicts)
			}
			if err != nil {
				t.Errorf("%s %v", text.path, err)
			}
		})
	}
}

// A rule a reader of the rendered page cannot see is none: the page shows nothing of a clause, or
// of the words "whether or not production carries the merge", inside an HTML comment. An old limit
// inside a comment is still a clause of the page as an editor reads it. So the rule is held in both
// readings of the page. A comment around the rule's clause, around "whether or not", around an old
// limit, or one that splits the words "whether or not production carries the merge", fails for
// the comment. A comment that holds nothing of the rule, or a "<!--" inside a fenced block before
// it, hides nothing of it. Each old wording is joined from halves, as bannedCheckLimits is.
func TestTheFailRuleIsHeldWhereAReaderSeesIt(t *testing.T) {
	const (
		rule         = "One still unobserved at the last look is a `fail` whether or not production carries the merge.\n"
		noProduction = "One still unobserved at the last look is a `fail`.\n"
		before       = "The check reports a verdict.\n\n"
	)
	limited := "One still unobserved at the last look is a `fail` when production" + " carries the merge.\n"
	banned := "That look's `pending` is for production not carrying" + " the merge alone.\n"
	for _, page := range []struct {
		shape, text string
		err         error
	}{
		{"the rule a reader sees", before + rule, nil},
		{"the rule beside a fail clause that names no production", before + rule + noProduction, nil},
		{"the old limit of the fail", before + limited, errLimitedFail},
		{"an old limit of the pending beside the rule", before + rule + banned, errBannedLimit},
		{"a fail clause that names no production", before + noProduction, errNoStatement},
		{"no rule", before, errNoStatement},
		{"the rule inside a comment", before + "<!--\n" + rule + "-->\n", errCommented},
		{"\"whether or not\" inside a comment",
			before + "One still unobserved at the last look is a `fail` <!-- whether or not -->production carries the merge.\n",
			errCommented},
		{"an old limit of the fail inside a comment", before + rule + "\n<!--\n" + limited + "-->\n", errCommented},
		{"a comment splitting \"whether or not production carries the merge\"",
			before + "One still unobserved at the last look is a `fail` whether or not production <!-- whole -->carries the merge.\n",
			errCommented},
		{"a comment holding nothing of the rule", before + "<!-- a note -->\n" + rule, nil},
		{"a comment opened inside a fenced block before the rule",
			before + "```html\n<!--\n```\n\n" + rule, nil},
	} {
		err := failRuleOfThePage(page.text, false)
		if !errors.Is(err, page.err) {
			t.Errorf("%s: holding the rule answers the error %v, want %v: %q", page.shape, err, page.err, page.text)
		}
	}
}

// bannedCheckLimits are limits of the last look that the production check's rule no longer has:
// each keeps the last look's pending, or its close as not run, for a merge that production does
// not carry. Each is joined from halves, as internal/prompts's retiredCheckSentences are, so a
// search of the repository for one of them finds the texts that state it and never this test.
// internal/prompts and internal/workflow keep their own copies, since a test package cannot
// import another package's test helpers.
var bannedCheckLimits = []string{
	"not carrying" + " the merge alone",
	"did not carry the merge" + " within the bound",
	"never carried" + " the merge",
}

const (
	// whetherOrNotCarried are the words that keep a fail clause's mention of production from
	// limiting the fail.
	whetherOrNotCarried = "whether or not production carries the merge"
	// proofSentence is what a proof script says of its issues: no requirement that only the deploy
	// can show, so its last look's pending is true.
	proofSentence = "The issues of this proof have no requirement that only the deploy can show"
)

var (
	// errBannedLimit is why a check refuses a text: a clause of it states a limit the rule no
	// longer has (bannedCheckLimits).
	errBannedLimit = errors.New("states a limit the production check's rule no longer has: a requirement still " +
		"unobserved at the last look is a fail " + whetherOrNotCarried + ", and that look's pending is only for an issue " +
		"with no requirement that only the deploy can show")
	// errLimitedFail is why a check refuses a text: a fail clause of it holds "production carries
	// the merge" outside the words whetherOrNotCarried.
	errLimitedFail = errors.New("limits the fail of a requirement still unobserved at the last look by whether " +
		"production carries the merge, but that requirement is a fail " + whetherOrNotCarried)
	// errNoStatement is why a check refuses a page: no fail clause of it holds whetherOrNotCarried.
	errNoStatement = errors.New("holds no clause that makes a requirement still unobserved at the last look a fail " +
		whetherOrNotCarried)
	// errNoObservedNotMet is why a check refuses the page that lists the three verdicts: no fail
	// clause of it names a requirement observed as not met.
	errNoObservedNotMet = errors.New("lists the three verdicts of a look but holds no fail clause for a requirement " +
		"observed as not met")
	// errNoProofSentence is why a check refuses a proof script: it does not say proofSentence.
	errNoProofSentence = errors.New("does not say \"" + proofSentence + "\", the one case where the proof's last " +
		"look's pending is true")
)

// failRuleOfThePage holds the rule to page, a Markdown page, in both its readings (bothReadings):
// as written, and as it renders (visibleMarkdown). listsVerdicts marks the page that lists the
// three verdicts of a look.
func failRuleOfThePage(page string, listsVerdicts bool) error {
	return bothReadings(failRuleIn(page, listsVerdicts), failRuleIn(visibleMarkdown(page), listsVerdicts))
}

// failRuleIn holds the rule to text, one reading of a page (ruleClauses). The text states no limit
// the rule no longer has (limitsIn). One fail clause at least holds whetherOrNotCarried
// (errNoStatement). Where the page lists the three verdicts, one fail clause at least names a
// requirement observed as not met (errNoObservedNotMet). The error joins every refusal.
func failRuleIn(text string, listsVerdicts bool) error {
	clauses := ruleClauses(text)
	errs := []error{limitsIn(clauses)}
	statements, observedNotMet := 0, 0
	for _, clause := range clauses {
		if isCheckFailClause(clause) && strings.Contains(clause, whetherOrNotCarried) {
			statements++
		}
		if isCheckFailClause(clause) && strings.Contains(clause, "observed as not met") {
			observedNotMet++
		}
	}
	if statements == 0 {
		errs = append(errs, errNoStatement)
	}
	if listsVerdicts && observedNotMet == 0 {
		errs = append(errs, errNoObservedNotMet)
	}
	return errors.Join(errs...)
}

// failRuleOfTheScript holds the rule to script, a proof's shell script, read once as prose
// (scriptProse): it states no limit the rule no longer has (limitsIn), and it says proofSentence
// (errNoProofSentence). The error joins every refusal.
func failRuleOfTheScript(script string) error {
	clauses := ruleClauses(scriptProse(script))
	errs := []error{limitsIn(clauses)}
	said := false
	for _, clause := range clauses {
		said = said || strings.Contains(clause, proofSentence)
	}
	if !said {
		errs = append(errs, errNoProofSentence)
	}
	return errors.Join(errs...)
}

// limitsIn holds clauses to the two limits of the rule: no clause states a limit the rule no
// longer has (errBannedLimit, naming each such clause), and no fail clause (isCheckFailClause)
// holds "production carries the merge" when every whetherOrNotCarried is taken out of it
// (errLimitedFail, naming each such clause).
func limitsIn(clauses []string) error {
	var banned, limited []string
	for _, clause := range clauses {
		for _, phrase := range bannedCheckLimits {
			if strings.Contains(clause, phrase) {
				banned = append(banned, clause)
				break
			}
		}
		if isCheckFailClause(clause) && strings.Contains(strings.ReplaceAll(clause, whetherOrNotCarried, ""), "production carries the merge") {
			limited = append(limited, clause)
		}
	}
	var errs []error
	if len(banned) > 0 {
		errs = append(errs, fmt.Errorf("%w: %q", errBannedLimit, banned))
	}
	if len(limited) > 0 {
		errs = append(errs, fmt.Errorf("%w: %q", errLimitedFail, limited))
	}
	return errors.Join(errs...)
}

// isCheckFailClause reports whether clause makes a requirement still unobserved at the last look a
// fail.
func isCheckFailClause(clause string) bool {
	return strings.Contains(clause, "unobserved") && strings.Contains(clause, "last") && strings.Contains(clause, "fail")
}

// scriptProse is a shell script as its prose reads: each line without its leading white space and
// one `#`, so that the words of a comment, and of a heredoc, join across their line breaks.
func scriptProse(script string) string {
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(strings.TrimLeft(line, " \t"), "#")
	}
	return strings.Join(lines, "\n")
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
