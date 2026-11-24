package prompts

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A broken role sequence drops the shared core, uses a root prompt for a child, or makes the Go
// daemon's instructions win before the shared role instructions. The prompt paths are what every
// launch concatenates into the one prompt file its agent reads, so their order is the contract. The
// Go daemon's text is the role's own part, then the part every architect or every phase worker
// shares. Each shared part's snapshot is the embedded part byte for byte, and the Go daemon's
// parts meet the parts before them at one blank line.
func TestComposeOrdersSharedRolePartsBeforeTheGoDaemonParts(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tc := range []struct {
		name    string
		role    claim.Role
		isRoot  bool
		shared  []string
		goParts []string
	}{
		{"root architect", claim.RoleArchitect, true, []string{"architect-root.md"}, []string{"architect-root.md", "architect-common.md"}},
		{"sub-architect", claim.RoleArchitect, false, []string{"architect.md"}, []string{"architect.md", "architect-common.md"}},
		{"planner", claim.RolePlanner, false, []string{"core/common.md", "core/planner.md", "mechanics/headless.md", "planner.md"}, []string{"planner.md", "worker-common.md"}},
		{"implementer", claim.RoleImplementer, false, []string{"core/common.md", "core/implementer.md", "mechanics/headless.md", "implementer.md"}, []string{"implementer.md", "worker-common.md"}},
		{"tester", claim.RoleTester, false, []string{"core/common.md", "core/tester.md", "mechanics/headless.md", "tester.md"}, []string{"tester.md", "worker-common.md"}},
		{"reviewer", claim.RoleReviewer, false, []string{"core/common.md", "core/reviewer.md", "mechanics/headless.md", "reviewer.md"}, []string{"reviewer.md", "worker-common.md"}},
		{"merger", claim.RoleMerger, false, []string{"mechanics/headless.md", "merger.md"}, []string{"merger.md", "worker-common.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := composer.Compose(tc.role, tc.isRoot)
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}
			want := make([]string, 0, len(tc.shared)+len(tc.goParts))
			for _, part := range tc.shared {
				want = append(want, filepath.Join(stateDir, "prompts", "shared", part))
			}
			for _, part := range tc.goParts {
				want = append(want, filepath.Join(stateDir, "prompts", "go", part))
			}
			if !reflect.DeepEqual(parts.RolePromptPaths, want) {
				t.Fatalf("RolePromptPaths = %q, want %q", parts.RolePromptPaths, want)
			}
			// A pane reads each shared part from the snapshot by path, so the snapshot holds the
			// embedded part byte for byte.
			for _, part := range tc.shared {
				got, err := os.ReadFile(filepath.Join(stateDir, "prompts", "shared", part))
				if err != nil {
					t.Fatalf("read the snapshot's %s: %v", part, err)
				}
				if string(got) != rolePart(t, part) {
					t.Errorf("the snapshot's %s is not the embedded part", part)
				}
			}
			// Every launch joins the parts byte for byte (runtime.ComposePrompt), so the role's Go
			// daemon part and the shared one must meet at one blank line, as the paragraphs within a
			// part do.
			var text strings.Builder
			for i, path := range parts.RolePromptPaths {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read part: %v", err)
				}
				if joined := text.String(); i > len(tc.shared) {
					gap := len(joined) - len(strings.TrimRight(joined, "\n")) + len(body) - len(strings.TrimLeft(string(body), "\n"))
					if gap != 2 {
						t.Errorf("Go daemon parts %q join %s with %d newlines, want one blank line", tc.goParts, path, gap)
					}
				}
				text.Write(body)
			}
		})
	}
}

// composedText is the whole text a role's composed prompt gives its pane: the parts Compose names,
// in the order the tmux runtime appends them (Parts.RolePromptPaths), read back from the snapshot
// and joined. A test that asks whether a prompt still says something retired asks it of this join,
// since which part holds the words is no part of the claim, and a compose or a read that fails
// leaves the question unanswered rather than unsaid, so both end the test (t.Fatalf).
func composedText(t *testing.T, composer *Composer, role claim.Role, isRoot bool) string {
	t.Helper()
	parts, err := composer.Compose(role, isRoot)
	if err != nil {
		t.Fatalf("Compose %s (root %t): %v", role, isRoot, err)
	}
	return readJoined(t, parts.RolePromptPaths...)
}

// foldWhitespace is one line of text, so a phrase a hard-wrapped prompt breaks over two lines is
// still one phrase to match.
func foldWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// readJoined is the bytes of each of paths, read in order and joined; a read that fails ends the
// test (t.Fatalf), naming the path.
func readJoined(t *testing.T, paths ...string) string {
	t.Helper()
	var text strings.Builder
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text.Write(body)
	}
	return text.String()
}

// ruleParagraphs are the blank-line-separated paragraphs of a composed prompt that hold marker,
// each folded to one line: a paragraph is the span a reader takes the rule and its framing from,
// so it is the span a test asks what the rule calls its case. A composed prompt holds one per
// part that carries the rule (the role's own part and the core its phase shares).
func ruleParagraphs(text, marker string) []string {
	var found []string
	for _, paragraph := range strings.Split(text, "\n\n") {
		if folded := foldWhitespace(paragraph); strings.Contains(folded, marker) {
			found = append(found, folded)
		}
	}
	return found
}

// clauses are the clauses of text, each folded to one line: each blank-line paragraph cut after
// every full stop and every semicolon a space follows. A code span's own full stop (`<title>.`) is
// followed by its closing backtick, so it ends no clause, and a hard wrap inside one leaves it
// whole. A clause is the span a reader takes one verdict's rule from: a sentence that lists the
// three verdicts gives each its own clause, and the words of one clause limit no other.
func clauses(text string) []string {
	var found []string
	for _, paragraph := range strings.Split(text, "\n\n") {
		for _, sentence := range strings.SplitAfter(foldWhitespace(paragraph), ". ") {
			found = append(found, strings.SplitAfter(sentence, "; ")...)
		}
	}
	return found
}

// blocks are the blocks of text, each folded to one line: each blank-line paragraph cut before
// every line that starts an item of a list (startsListItem), so a paragraph and each item of a
// list in it are blocks of their own. A block is the span a reader takes one item's rule from:
// the words of one item limit no other.
func blocks(text string) []string {
	var found []string
	for _, paragraph := range strings.Split(text, "\n\n") {
		block := ""
		for _, line := range strings.Split(paragraph, "\n") {
			if block != "" && startsListItem(line) {
				found, block = append(found, foldWhitespace(block)), ""
			}
			block += line + "\n"
		}
		found = append(found, foldWhitespace(block))
	}
	return found
}

// startsListItem says whether line, past its indent, starts an item of a list: `- `, or a number
// of any length and `. `, so `10. ` starts an item as `1. ` does.
func startsListItem(line string) bool {
	item := strings.TrimLeft(line, " ")
	digits := len(item) - len(strings.TrimLeft(item, "0123456789"))
	return strings.HasPrefix(item, "- ") || digits > 0 && strings.HasPrefix(item[digits:], ". ")
}

// Each item of a list is a block of its own (blocks), a numbered item of two digits as much as
// one of one, so a check that one item names a step cannot pass on the words of its neighbour. A
// wrapped line that starts with a number but no `. ` after it stays in the item it continues.
func TestBlocksGiveEachItemOfAListABlockOfItsOwn(t *testing.T) {
	var text strings.Builder
	text.WriteString("Steps:\n")
	want := []string{"Steps:"}
	for n := 1; n <= 11; n++ {
		fmt.Fprintf(&text, "%d. Step %d,\n   10 files.\n", n, n)
		want = append(want, fmt.Sprintf("%d. Step %d, 10 files.", n, n))
	}
	text.WriteString("  - A nested item.\n\nA closing paragraph.")
	want = append(want, "- A nested item.", "A closing paragraph.")
	if got := blocks(text.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("blocks =\n%q\nwant\n%q", got, want)
	}
}

// clauseHolds says whether one clause of text (clauses) holds each of terms, in the order given:
// a rule pinned by its terms, so a rewording that keeps them passes and a change of one fails,
// while each term still has to stand in the one clause that states the rule.
func clauseHolds(text string, terms ...string) bool {
	for _, clause := range clauses(text) {
		rest, held := clause, true
		for _, term := range terms {
			_, after, found := strings.Cut(rest, term)
			if !found {
				held = false
				break
			}
			rest = after
		}
		if held {
			return true
		}
	}
	return false
}

// What a phase proved is a result recorded against a `check` block of the issue's Verification
// document (`dispatch result`), and the pull request body carries one line per check. No composed
// prompt — any phase worker's, and the architect's in both its shapes — still teaches the two
// lines the body carried before, the six-field `proof` array a handoff carried, or the
// `Production:` line the production check wrote, which is now an `after_merge` result a closed
// issue takes. The two line names are built from their role rather than written out, so a search
// of the repository for either finds the texts that teach it and not this test.
//
// The production check's own retired sentences are read here too (retiredCheckSentences): the
// check is no longer the implementer's, and no deployment's event wakes a look. This test reads
// what Compose joins, so it answers for the prompt a role is actually given;
// TestNoPartMakesTheProductionCheckTheImplementersOrWakesItFromADeploy reads the same sentences
// over every part of both trees, where one Compose never joins would otherwise sit unread.
func TestNoComposedPromptTeachesAnE2ELineOrAProofArray(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	retired := []string{"`proof` array", "`Production:` line",
		"pending deploy of", "the production-like proof", "the implementer's proof"}
	retired = append(retired, retiredCheckSentences...)
	for _, role := range []string{"implementer", "tester"} {
		retired = append(retired, fmt.Sprintf("E2E (%s)", role))
	}
	for _, tc := range []struct {
		role   claim.Role
		isRoot bool
	}{
		{claim.RolePlanner, false}, {claim.RoleImplementer, false}, {claim.RoleTester, false},
		{claim.RoleReviewer, false}, {claim.RoleMerger, false},
		{claim.RoleArchitect, true}, {claim.RoleArchitect, false},
	} {
		text := composedText(t, composer, tc.role, tc.isRoot)
		for _, phrase := range retired {
			if strings.Contains(text, phrase) {
				t.Errorf("the composed %s prompt (root %t) still says %q", tc.role, tc.isRoot, phrase)
			}
		}
	}
}

// A state directory outlives the daemon binary that wrote its prompt snapshot, so a boot of a newer
// daemon finds the older daemon's words there. A part whose content is not the running binary's is
// rewritten, shared or the daemon's own: a prompt that contradicts the daemon's own behaviour (who
// posts the merge) is the defect, and nothing treats these files as the operator's to edit (the
// operator's text is `instructions`). A part already holding the running binary's content is left
// as it is, so an ordinary restart writes nothing.
func TestNewRewritesAPartTheRunningDaemonDidNotWrite(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := New(stateDir); err != nil {
		t.Fatalf("first New: %v", err)
	}
	snapshot := func(name string) string { return filepath.Join(stateDir, "prompts", name) }
	embedded := map[string]string{
		"go/merger.md":          "go/merger.md",
		"go/tester.md":          "go/tester.md",
		"shared/merger.md":      "roles/merger.md",
		"shared/core/tester.md": "roles/core/tester.md",
	}
	stale := []string{"go/merger.md", "shared/merger.md"}
	current := []string{"go/tester.md", "shared/core/tester.md"}
	for _, name := range stale {
		if err := os.WriteFile(snapshot(name), []byte("an older daemon's "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write the older part: %v", err)
		}
	}
	written := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, name := range current {
		if err := os.Chtimes(snapshot(name), written, written); err != nil {
			t.Fatalf("date the current part: %v", err)
		}
	}
	if _, err := New(stateDir); err != nil {
		t.Fatalf("second New: %v", err)
	}
	for name, source := range embedded {
		parts := goParts
		if strings.HasPrefix(source, "roles/") {
			parts = roleParts
		}
		want, err := parts.ReadFile(source)
		if err != nil {
			t.Fatalf("read embedded %s: %v", source, err)
		}
		got, err := os.ReadFile(snapshot(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s after the second New = %q, want the running daemon's part", name, got)
		}
	}
	for _, name := range current {
		if info, err := os.Stat(snapshot(name)); err != nil || !info.ModTime().Equal(written) {
			t.Errorf("%s, already holding the running daemon's content, was written again: %v", name, err)
		}
	}
}

// bannedCheckLimits are limits of the last look that the production check's rule no longer has:
// each keeps the last look's pending, or its close as not run, for a merge that production does
// not carry. Each is joined from halves, as retiredCheckSentences are, so a search of the
// repository for one of them finds the texts that state it and never this test.
// internal/workflow and internal/textcheck keep their own copies, since a test package cannot
// import another package's test helpers.
var bannedCheckLimits = []string{
	"not carrying" + " the merge alone",
	"did not carry the merge" + " within the bound",
	"never carried" + " the merge",
}

// whetherOrNotCarried are the words that keep a fail clause's mention of production from limiting
// the fail.
const whetherOrNotCarried = "whether or not production carries the merge"

// isCheckFailClause reports whether clause, one of clauses, makes a requirement still unobserved
// at the last look a fail.
func isCheckFailClause(clause string) bool {
	return strings.Contains(clause, "unobserved") && strings.Contains(clause, "last") && strings.Contains(clause, "fail")
}

// checkRuleFaults are the ways clause breaks the production check's rule, one message each: it
// states a limit the rule no longer has (bannedCheckLimits), or it is a fail clause
// (isCheckFailClause) that still holds "production carries the merge" when every
// whetherOrNotCarried is taken out of it.
func checkRuleFaults(clause string) []string {
	var faults []string
	for _, phrase := range bannedCheckLimits {
		if strings.Contains(clause, phrase) {
			faults = append(faults, fmt.Sprintf("says %q, a limit the production check's rule no longer has: a requirement still unobserved at the last look is a fail %s, and that look's pending is only for an issue with no requirement that only the deploy can show: %q", phrase, whetherOrNotCarried, clause))
		}
	}
	if isCheckFailClause(clause) && strings.Contains(strings.ReplaceAll(clause, whetherOrNotCarried, ""), "production carries the merge") {
		faults = append(faults, fmt.Sprintf("limits the fail of a requirement still unobserved at the last look by whether production carries the merge, but that requirement is a fail %s: %q", whetherOrNotCarried, clause))
	}
	return faults
}

// A production check opens at the issue's close, which comes at the merge or, for a merge that
// waits on the issues beneath it, later (workflow.closeMerged), and its bound is counted from that
// close (workflow.checkBound). A requirement that only the deploy can show is a fail when a look
// observes it as not met. It is also a fail when it is still unobserved at the last look, whether
// or not production carries the merge, and that look's pending is only for an issue with no such
// requirement (workflow.checkTask). So the composed prompt of each role a look reaches, the
// tester's and the implementer's, holds that rule clause by clause. No clause states a limit the
// rule no longer has, and no fail clause, one that holds "unobserved", "last" and "fail", holds
// "production carries the merge" outside the words "whether or not production carries the merge"
// (checkRuleFaults). Each prompt holds one fail clause with those words at least, and one fail
// clause for a requirement observed as not met, so a matcher that stopped finding the rule fails
// here rather than passing on nothing. The unit is the clause, cut at `; ` as well as at full stops
// as workflow's check task test cuts its task, never the sentence: matched sentence by sentence, a
// test of this rule passed with the concepts page's limit taken out, since that page's one
// sentence of the three verdicts reads whether production carries the merge in its opening
// clause. A test that passes on the revert it exists to catch measures the wrong unit, so a check
// widened from here keeps the clause. TestTheProductionCheckRuleHoldsInEveryPromptPart holds
// every part of both trees to the same limits and names the part that breaks them. The pages and
// the proof scripts that state the rule outside a prompt are read by internal/textcheck's
// TestThePagesFailAnUnobservedRequirementWhetherOrNotProductionCarriesTheMerge, with the same
// clause rule, so an edit to one of them alone starts the text checks rather than this package's
// whole lane. Besides: a clause naming `production_check_hours` counts it from the close, never
// from the merge; a clause saying the session is handed the check task names the close before it;
// and no clause says the merge closes the issue, since in these two prompts the close is the
// moment the check task is handed and the role's work ends, and a merge that waits on the issues
// beneath it comes before that moment. The architect's, the merger's and the reviewer's "the merge
// closes" name only what causes the close, which is true, and no prompt this test reads holds
// them. This shows the text is loaded, never that a role follows it.
func TestTheProductionCheckFailsAnUnobservedRequirementWhetherOrNotProductionCarriesTheMerge(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	theClose := regexp.MustCompile(`\bclose\b`)
	boundFromTheMerge := regexp.MustCompile("production_check_hours`? (after|from|since) the merge")
	type text struct{ name, body string }
	var texts []text
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer} {
		texts = append(texts, text{"the composed " + string(role) + " prompt", composedText(t, composer, role, false)})
	}
	for _, tc := range texts {
		t.Run(tc.name, func(t *testing.T) {
			statements, observedNotMet := 0, 0
			for _, clause := range clauses(tc.body) {
				for _, fault := range checkRuleFaults(clause) {
					t.Errorf("%s %s", tc.name, fault)
				}
				if isCheckFailClause(clause) && strings.Contains(clause, whetherOrNotCarried) {
					statements++
				}
				if isCheckFailClause(clause) && strings.Contains(clause, "observed as not met") {
					observedNotMet++
				}
				if strings.Contains(clause, "`production_check_hours`") && (!theClose.MatchString(clause) || boundFromTheMerge.MatchString(clause)) {
					t.Errorf("%s counts the check's bound from something other than the issue's close: %q", tc.name, clause)
				}
				if before, _, found := strings.Cut(clause, "hands this session"); found && !theClose.MatchString(before) {
					t.Errorf("%s hands this session the check task at something other than the issue's close: %q", tc.name, clause)
				}
				if strings.Contains(strings.ToLower(clause), "merge closes") {
					t.Errorf("%s says the merge closes the issue, which puts the check's hand-off and the end of this role's work at the merge, before the close for a merge that waits on the issues beneath it: %q", tc.name, clause)
				}
			}
			if statements == 0 {
				t.Errorf("%s holds no clause that makes a requirement still unobserved at the last look a fail %s", tc.name, whetherOrNotCarried)
			}
			if observedNotMet == 0 {
				t.Errorf("%s holds no fail clause for a requirement observed as not met", tc.name)
			}
		})
	}
}

// An agent that wants a pod of its own for a task writes one line of a fixed form in its session,
// and Legion starts no pod for it: the line is a wish that a later read of the stored sessions
// counts. Legion's own part of every prompt gives the form, architect-common.md for an owner and
// worker-common.md for the agent of a step, so each composition an issue's agents start with holds
// the form, in the clause that says when to write it, and the clause that says Legion starts no
// pod for it, each held by its terms. This shows the text is loaded, never that an agent writes
// the line.
func TestEveryPromptGivesTheSeparatePodLine(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tc := range []struct {
		name   string
		role   claim.Role
		isRoot bool
	}{
		{"root architect", claim.RoleArchitect, true},
		{"sub-architect", claim.RoleArchitect, false},
		{"planner", claim.RolePlanner, false},
		{"implementer", claim.RoleImplementer, false},
		{"tester", claim.RoleTester, false},
		{"reviewer", claim.RoleReviewer, false},
		{"merger", claim.RoleMerger, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := composedText(t, composer, tc.role, tc.isRoot)
			for _, terms := range [][]string{
				{"pod of its own", "one line", "SEPARATE-POD", "SEPARATE-POD <ISSUE> <task> <reason>"},
				{"Legion", "no pod", "this line"},
			} {
				if !clauseHolds(text, terms...) {
					t.Errorf("no clause of the composed %s prompt holds %q, in that order", tc.name, terms)
				}
			}
		})
	}
}

// Every part of both trees holds the production check's rule clause by clause, not only the parts
// Compose joins for the tester and the implementer. A sentence left in a part that neither role
// composes still reaches whichever role does compose it, and a part Compose joins for nobody today
// is joined by the next role that needs it. So no clause of a part states a limit the rule no
// longer has, and no fail clause of a part limits the fail by whether production carries the merge
// (checkRuleFaults). Each failure names the part's file, so a revert of one sentence names the
// file it is in. A part need not state the rule, since most parts are about other work: the
// composed prompts hold the statement
// (TestTheProductionCheckFailsAnUnobservedRequirementWhetherOrNotProductionCarriesTheMerge).
func TestTheProductionCheckRuleHoldsInEveryPromptPart(t *testing.T) {
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			for _, clause := range clauses(string(body)) {
				for _, fault := range checkRuleFaults(clause) {
					t.Errorf("%s/%s %s", dir, name, fault)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
}
