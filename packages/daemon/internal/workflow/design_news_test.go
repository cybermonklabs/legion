package workflow

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// These tests read the news text as its readers do: the phase worker or architect a task carries
// it to. Every one is pure, built from the facts the daemon's send hands RenderNews, and opens no
// database.

// newsTree is the tree root every news fixture is of.
const newsTree = "LEGION-208"

// newsSteps is a deployment that gates spec alone; the root's architect also registered its
// Verification document, so criteria holds the tree beside it (record.TreeSteps).
var newsSteps = []record.DesignStep{record.DesignStepSpec}

// newsGates is the root's two gate rows, both open: the Verification document approved at
// version 3, then the spec at version 5, the approval Dispatch recorded last.
func newsGates() map[record.DesignStep]record.DesignGate {
	return map[record.DesignStep]record.DesignGate{
		record.DesignStepSpec: {Issue: newsTree, Step: record.DesignStepSpec, ArtifactID: "artifact-spec", LatestVersion: 5,
			ApprovedVersion: new(5), ApprovedMark: record.EventMark{Seq: 31}},
		record.DesignStepCriteria: {Issue: newsTree, Step: record.DesignStepCriteria, ArtifactID: "artifact-checks", LatestVersion: 3,
			ApprovedVersion: new(3), ApprovedMark: record.EventMark{Seq: 30}},
	}
}

// closedGates is newsGates after the writer saved version 6 of the spec, which is with its
// reviewer: the gate closed, and the approval of version 5 still recorded below it.
func closedGates() map[record.DesignStep]record.DesignGate {
	gates := newsGates()
	spec := gates[record.DesignStepSpec]
	spec.LatestVersion = 6
	gates[record.DesignStepSpec] = spec
	return gates
}

// specChanges is what changed in the spec from version from to version 5: four sections, among
// them the text before the first `##` and the second section headed "Notes", three requirements
// and a callout.
func specChanges(from int) NewsChange {
	return NewsChange{Step: record.DesignStepSpec, Changes: record.DesignChanges{
		VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: from, To: 5}, State: record.DesignChangesRead,
		Sections: []record.SectionChange{{Heading: "Design", Change: "changed"}, {Heading: "Rollout", Change: "added"},
			{Heading: "", Change: "changed"}, {Heading: "Notes", Occurrence: 1, Change: "removed"}},
		Blocks: []record.BlockChange{{ID: "r2", Type: "requirement", Change: "changed"}, {ID: "r7", Type: "requirement", Change: "added"},
			{ID: "r3", Type: "requirement", Change: "removed"}, {ID: "note-rollback", Type: "callout", Change: "added"}},
	}}
}

// checksChanges is what changed in the Verification document from version 2 to version 3: two
// checks, and a callout beside them that names no check.
func checksChanges() NewsChange {
	return NewsChange{Step: record.DesignStepCriteria, Changes: record.DesignChanges{
		VersionPair: record.VersionPair{ArtifactID: "artifact-checks", From: 2, To: 3}, State: record.DesignChangesRead,
		Sections: []record.SectionChange{{Heading: "Checks", Change: "changed"}},
		Blocks: []record.BlockChange{{ID: "c-live", Type: "check", Change: "changed"}, {ID: "c-upgrade", Type: "check", Change: "added"},
			{ID: "aside-ci", Type: "callout", Change: "changed"}},
	}}
}

// rootHolderNews is the news of the root's implementer, owed the opening: the spec since version
// 3 and the Verification document since version 2, the root's own checks.
func rootHolderNews() News {
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: newsTree, Phase: phase.Implementing,
		Steps: newsSteps, Gates: newsGates(), OpeningOwed: true, Items: []NewsChange{specChanges(3), checksChanges()}}
}

// closedHolderNews is rootHolderNews sent after a later version closed the gate again.
func closedHolderNews() News {
	news := rootHolderNews()
	news.Gates = closedGates()
	return news
}

// unreadHolderNews is the news of a child's reviewer, which took spec version 4, whose pair
// Dispatch could not read: a release without the changes route.
func unreadHolderNews() News {
	unread := NewsChange{Step: record.DesignStepSpec, Changes: record.DesignChanges{
		VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 4, To: 5}, State: record.DesignChangesUnavailable,
		Reason: "dispatch has no changes route: GET /api/v1/artifacts/artifact-spec/changes answered 404 Not Found"}}
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: "LEGION-209", Phase: phase.Reviewing,
		Steps: newsSteps, Gates: newsGates(), Items: []NewsChange{unread}}
}

// firstApprovalHolderNews is the news of a child's planner whose phase began before any version
// of the spec was approved.
func firstApprovalHolderNews() News {
	first := specChanges(0)
	first.Changes.Sections, first.Changes.Blocks = nil, nil
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: "LEGION-213", Phase: phase.Planning,
		Steps: newsSteps, Gates: newsGates(), Items: []NewsChange{first}}
}

// acceptingHolderNews is the news of a child standing in accepting, whose holder is the
// sub-architect that owns it.
func acceptingHolderNews() News {
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: "LEGION-210", Phase: phase.Accepting,
		Steps: newsSteps, Gates: newsGates(), Items: []NewsChange{specChanges(4)}}
}

// decisionOnlyHolderNews is the one news task of a version whose only difference is decision
// blocks: the gate closed on it, its holder was told so, and a person approved it later, so the
// opening is owed and nothing else is.
func decisionOnlyHolderNews() News {
	decisions := NewsChange{Step: record.DesignStepSpec, Changes: record.DesignChanges{
		VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 4, To: 5}, State: record.DesignChangesRead, DecisionOnly: true}}
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: "LEGION-211", Phase: phase.Testing,
		Steps: newsSteps, Gates: newsGates(), OpeningOwed: true, Items: []NewsChange{decisions}}
}

// treeSummaryNews is the root architect's summary of a spec change over the whole tree: the
// architect last took spec version 3 and checks version 2, and each member stands somewhere else.
func treeSummaryNews() News {
	return News{Audience: record.AudienceArchitect, Tree: newsTree, Steps: newsSteps, Gates: newsGates(),
		Items: []NewsChange{specChanges(3), checksChanges()},
		Members: []NewsMember{
			{Issue: newsTree, Phase: phase.AwaitingPerson, State: NewsAwaitingPerson},
			{Issue: "LEGION-209", Phase: phase.Implementing, State: NewsSentNow, Items: []NewsChange{specChanges(3)}},
			{Issue: "LEGION-210", Phase: phase.Testing, State: NewsAtNextStart, Items: []NewsChange{specChanges(4)}},
			{Issue: "LEGION-211", Phase: phase.Accepting, State: NewsYouAnswer, Items: []NewsChange{specChanges(4)}},
			{Issue: "LEGION-212", Phase: phase.Reviewing, State: NewsDecisionOnly},
			{Issue: "LEGION-213", Phase: phase.Planning, State: NewsAlreadyTaken},
			{Issue: "LEGION-214", Phase: phase.Admitted, State: NewsAdmitted},
			{Issue: "LEGION-215", Phase: phase.Held, State: NewsStopped},
			{Issue: "LEGION-216", Phase: phase.Done, State: NewsDone},
		}}
}

// mergedRootSummaryNews is the root architect's summary of a change to the checks alone on a root
// whose pull request has merged: the person approved the Verification document's version 3 after
// the spec, which opened the gate; no member is owed anything, and the architect is owed the
// opening and the duty the merge leaves it.
func mergedRootSummaryNews() News {
	gates := newsGates()
	checks := gates[record.DesignStepCriteria]
	checks.ApprovedMark = record.EventMark{Seq: 32}
	gates[record.DesignStepCriteria] = checks
	return News{Audience: record.AudienceArchitect, Tree: newsTree, Steps: newsSteps, Gates: gates, OpeningOwed: true,
		Items: []NewsChange{checksChanges()},
		Members: []NewsMember{
			{Issue: newsTree, Phase: phase.Merging, State: NewsMerged},
			{Issue: "LEGION-209", Phase: phase.Implementing, State: NewsNotConcerned},
			{Issue: "LEGION-210", Phase: phase.Admitted, State: NewsOutOfWorkflow},
		}}
}

// everyNewsForm is each news text the fixtures render, by name.
func everyNewsForm() map[string]string {
	return map[string]string{
		"the root holder owed the opening":           RenderNews(rootHolderNews()),
		"the holder sent news after the gate closed": RenderNews(closedHolderNews()),
		"the holder of an unread pair":               RenderNews(unreadHolderNews()),
		"the holder of the first approved version":   RenderNews(firstApprovalHolderNews()),
		"the sub-architect of an accepting child":    RenderNews(acceptingHolderNews()),
		"the holder of a decision-only version":      RenderNews(decisionOnlyHolderNews()),
		"the root architect's tree summary":          RenderNews(treeSummaryNews()),
		"the root architect of a merged root":        RenderNews(mergedRootSummaryNews()),
	}
}

// mergerQuote is the gate sentence prompts/roles/merger.md quotes as the message that moves a
// merge, opened or closed, with its placeholders filled in: the words a merger reads for.
func mergerQuote(t *testing.T, move, step, document string, version int) string {
	t.Helper()
	prompt, err := os.ReadFile(filepath.Join("..", "prompts", "roles", "merger.md"))
	if err != nil {
		t.Fatalf("read the merger's prompt: %v", err)
	}
	quoted := regexp.MustCompile("`(The design gate of <tree> " + move + ":[^`]*)`").FindStringSubmatch(string(prompt))
	if quoted == nil {
		t.Fatalf("the merger's prompt quotes no gate sentence that says %q", move)
	}
	return strings.NewReplacer("<tree>", newsTree, "<n>", strconv.Itoa(version), "<step>", step, "<id>", document).Replace(quoted[1])
}

// newsItem is the item of a news list whose first line opens with opening, that line and the
// lines indented under it.
func newsItem(t *testing.T, text, opening string) string {
	t.Helper()
	var item []string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "- "+opening):
			item = []string{line}
		case item != nil && strings.HasPrefix(line, "  "):
			item = append(item, line)
		case item != nil:
			return strings.Join(item, "\n")
		}
	}
	if item == nil {
		t.Fatalf("no item opens %q in the news:\n%s", opening, text)
	}
	return strings.Join(item, "\n")
}

// newsLine is the line of text that opens with opening.
func newsLine(t *testing.T, text, opening string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, opening) {
			return line
		}
	}
	t.Fatalf("no line opens %q in the news:\n%s", opening, text)
	return ""
}

// A holder owed the opening reads the gate sentence first, in the words the merger's prompt
// quotes as what releases a merge, so a merger released by its news task reads the release it
// waits for. A holder that was told the opening already reads no gate sentence.
func TestNewsOwedTheOpeningLeadsWithTheSentenceThatReleasesAMerge(t *testing.T) {
	text := RenderNews(rootHolderNews())
	t.Logf("the root holder's news:\n%s", text)
	opened := mergerQuote(t, "opened", "spec", "artifact-spec", 5)
	if !strings.HasPrefix(text, opened) {
		t.Fatalf("the news opens %q, want the merger prompt's quoted opening %q first", firstLine(text), opened)
	}
	told := rootHolderNews()
	told.OpeningOwed = false
	if got := RenderNews(told); strings.Contains(got, "The design gate of") {
		t.Fatalf("news to a holder already told the opening names the gate:\n%s", got)
	}
}

// News composed while the gate stood open and sent after a later version closed it says closed,
// naming the version with its reviewer, and never the opening that no longer stands: the gate
// sentence is said of the gate at send.
func TestNewsSentAfterALaterVersionClosedTheGateSaysClosed(t *testing.T) {
	text := RenderNews(closedHolderNews())
	t.Logf("the news sent after version 6 closed the gate:\n%s", text)
	closed := mergerQuote(t, "closed", "spec", "artifact-spec", 6)
	if !strings.HasPrefix(text, closed) {
		t.Fatalf("the news opens %q, want the merger prompt's quoted close at version 6 %q first", firstLine(text), closed)
	}
	if strings.Contains(text, "opened") {
		t.Fatalf("news sent while the gate is closed says it opened:\n%s", text)
	}
	if item := newsItem(t, text, "The spec document (artifact-spec)"); !strings.Contains(item, "version 5, since version 3") {
		t.Fatalf("the spec's item after the close = %q, want it to keep the approved pair 3 to 5", item)
	}
}

// Each step names its own pair as the version the holder's phase last took, and its own change
// list: the spec's sections, the text outside every section among them, and its requirements;
// the Verification document's checks, which are the root's to prove, and none of its other
// blocks. Each item ends with the command that prints its new version, and the news ends with the
// rule the holder's prompt carries.
func TestNewsNamesEachStepsSectionsAndBlocksWithItsVersionPair(t *testing.T) {
	text := RenderNews(rootHolderNews())
	spec := newsItem(t, text, "The spec document (artifact-spec)")
	for _, want := range []string{"version 5, since version 3, which your phase last took",
		`"Design" changed`, `"Rollout" added`, "the text before the first section changed", `the 2nd section headed "Notes" removed`,
		"Requirements: r2 changed; r7 added; r3 removed.", "Callouts: note-rollback added.",
		"`dispatch doc-read --issue LEGION-208 --artifact artifact-spec --version 5`"} {
		if !strings.Contains(spec, want) {
			t.Errorf("the spec's item does not say %q:\n%s", want, spec)
		}
	}
	checks := newsItem(t, text, "The criteria document (artifact-checks)")
	for _, want := range []string{"version 3, since version 2, which your phase last took", `"Checks" changed`,
		"Checks: c-live changed; c-upgrade added.", "`dispatch doc-read --issue LEGION-208 --artifact artifact-checks --version 3`"} {
		if !strings.Contains(checks, want) {
			t.Errorf("the Verification document's item does not say %q:\n%s", want, checks)
		}
	}
	if strings.Contains(checks, "aside-ci") || strings.Contains(spec, "c-live") {
		t.Errorf("a step's item names a block of the other kind:\n%s\n%s", spec, checks)
	}
	if !strings.Contains(text, "The approved design of LEGION-208 changed for LEGION-208, which you hold in implementing:") {
		t.Errorf("the news does not name the issue and the phase its holder holds:\n%s", text)
	}
	rule := lastParagraph(text)
	for _, want := range []string{"read each named section and check at the new version", "`request_backward_move`",
		"names the version and the change", "`dispatch comment` on LEGION-208", "never moves back"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the news's closing rule %q does not say %q", rule, want)
		}
	}
	first := RenderNews(firstApprovalHolderNews())
	t.Logf("the news of a first approved version:\n%s", first)
	if item := newsItem(t, first, "The spec document (artifact-spec)"); !strings.Contains(item, "the first version a person approved; your phase took none before it") ||
		!strings.Contains(item, "read it whole") {
		t.Errorf("the item of a version with no approved one before it = %q, want it to say so and send the reader to the whole version", item)
	}
}

// A pair Dispatch could not read names the versions, says the change list could not be read and
// why, and still names the command that prints the new version.
func TestNewsOfAnUnreadPairNamesTheReasonAndTheCommandThatPrintsIt(t *testing.T) {
	text := RenderNews(unreadHolderNews())
	t.Logf("the news of an unread pair:\n%s", text)
	item := newsItem(t, text, "The spec document (artifact-spec)")
	for _, want := range []string{"version 5, since version 4, which your phase last took",
		"The change list could not be read: dispatch has no changes route: GET /api/v1/artifacts/artifact-spec/changes answered 404 Not Found.",
		"`dispatch doc-read --issue LEGION-208 --artifact artifact-spec --version 5`"} {
		if !strings.Contains(item, want) {
			t.Errorf("the unread pair's item does not say %q:\n%s", want, item)
		}
	}
	if strings.Contains(item, "Sections:") {
		t.Errorf("the unread pair's item names sections it could not read:\n%s", item)
	}
}

// The architect holding a child in accepting answers its news as its prompt says it answers
// accepting: by its sign-off, or by a move back to planning, implementing or testing.
func TestNewsToTheArchitectHoldingAnAcceptingIssueEndsWithItsAnswer(t *testing.T) {
	text := RenderNews(acceptingHolderNews())
	t.Logf("the news of an accepting child:\n%s", text)
	rule := lastParagraph(text)
	for _, want := range []string{"sign LEGION-210 off with `sign_off`", "`request_backward_move`", "planning, implementing or testing"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the accepting holder's closing rule %q does not say %q", rule, want)
		}
	}
	if strings.Contains(rule, "dispatch comment") {
		t.Errorf("the accepting holder is told to answer as a phase worker does: %q", rule)
	}
}

// A version whose only difference is decision blocks, owed only for its opening, tells the holder
// the gate opened and that nothing its issue holds changed since the version its phase took: no
// change list, no command and no rule, since nothing asks for an answer.
func TestADecisionOnlyVersionOwedTheOpeningSaysNothingChanged(t *testing.T) {
	text := RenderNews(decisionOnlyHolderNews())
	t.Logf("the news of a decision-only version:\n%s", text)
	if opened := mergerQuote(t, "opened", "spec", "artifact-spec", 5); !strings.HasPrefix(text, opened) {
		t.Fatalf("the news opens %q, want the opening %q first", firstLine(text), opened)
	}
	if !strings.Contains(text, "Nothing your issue holds changed since version 4, which your phase took.") {
		t.Fatalf("the news does not say that nothing the issue holds changed since version 4:\n%s", text)
	}
	for _, unwanted := range []string{"Sections:", "Requirements:", "doc-read", "request_backward_move", "dispatch comment"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the news of a decision-only version says %q:\n%s", unwanted, text)
		}
	}
}

// The root architect's one summary names every member of the tree with its phase and what it is
// told: owed and sent now, owed and carried by its next start, or told nothing and why. Each pair
// of versions it names comes once with its change list, the architect's own marked as what it
// last took.
func TestTheArchitectsSummaryNamesEveryMemberWithItsStateAndReason(t *testing.T) {
	text := RenderNews(treeSummaryNews())
	t.Logf("the root architect's summary:\n%s", text)
	for opening, want := range map[string]string{
		"- LEGION-208, in awaiting_person: ": "told nothing while it waits on a person",
		"- LEGION-209, in implementing: ":    "owed version 5 of the spec document, since version 3, which its phase last took; a task tells its implementer now.",
		"- LEGION-210, in testing: ":         "owed version 5 of the spec document, since version 4, which its phase last took; the next start of its tester carries it.",
		"- LEGION-212, in reviewing: ":       "told nothing: the approved version differs from the one it took only in decision blocks.",
		"- LEGION-213, in planning: ":        "told nothing: its phase already took the approved version.",
		"- LEGION-214, in admitted: ":        "told nothing: it has not started, and its first phase starts from the approved version.",
		"- LEGION-215, in held: ":            "told nothing while it is stopped: the restart that moves it back into its phase carries what it is owed then.",
		"- LEGION-216, in done: ":            "told nothing: its work is done.",
	} {
		if line := newsLine(t, text, opening); !strings.Contains(line, want) {
			t.Errorf("the summary's line %q does not say %q", line, want)
		}
	}
	if own := newsItem(t, text, "The spec document (artifact-spec): version 5, since version 3"); !strings.Contains(own, "which you last took") ||
		!strings.Contains(own, "Requirements: r2 changed; r7 added; r3 removed.") || !strings.Contains(own, "--version 5`") {
		t.Errorf("the architect's own spec pair = %q, want it named as what it last took, with its change list and command", own)
	}
	if other := newsItem(t, text, "The spec document (artifact-spec): version 5, since version 4."); !strings.Contains(other, "Sections:") {
		t.Errorf("the pair of LEGION-210 and LEGION-211 = %q, want its own change list", other)
	}
	if checks := newsItem(t, text, "The criteria document (artifact-checks)"); !strings.Contains(checks, "version 3, since version 2, which you last took") ||
		!strings.Contains(checks, "Checks: c-live changed; c-upgrade added.") {
		t.Errorf("the architect's own checks pair = %q, want its pair and its checks", checks)
	}
	if count := strings.Count(text, "- The spec document (artifact-spec): version 5, since version 3"); count != 1 {
		t.Errorf("the summary names the pair 3 to 5 %d times, want once whoever is owed it", count)
	}
}

// A member whose pull request has merged takes no new work, so the summary hands the architect
// the duty that follows: the work goes to an issue of the tree whose pull request is still open,
// or to a person. A change to the checks alone still reaches the architect of a merged root, which
// owes no member anything, and names why each other member is told nothing. Its opening names the
// approval that opened the gate, the Verification document's, which Dispatch recorded last.
func TestTheSummaryGivesTheArchitectTheDutyOfAMergedMember(t *testing.T) {
	text := RenderNews(mergedRootSummaryNews())
	t.Logf("the summary of a merged root:\n%s", text)
	if opened := mergerQuote(t, "opened", "criteria", "artifact-checks", 3); !strings.HasPrefix(text, opened) {
		t.Fatalf("the summary owed the opening opens %q, want %q first", firstLine(text), opened)
	}
	merged := newsLine(t, text, "- LEGION-208, in merging: ")
	for _, want := range []string{"its pull request has merged", "never moves back",
		"Give the new work to an issue of this tree whose pull request is still open, or `dispatch ask` the person."} {
		if !strings.Contains(merged, want) {
			t.Errorf("the merged root's line %q does not say %q", merged, want)
		}
	}
	if line := newsLine(t, text, "- LEGION-209, in implementing: "); !strings.Contains(line, "only the root's checks changed") {
		t.Errorf("the child's line %q does not say the checks are the root's alone", line)
	}
	if line := newsLine(t, text, "- LEGION-210, in admitted: "); !strings.Contains(line, "out of the workflow") {
		t.Errorf("the parked child's line %q does not say it is out of the workflow", line)
	}
	if checks := newsItem(t, text, "The criteria document (artifact-checks)"); !strings.Contains(checks, "Checks: c-live changed; c-upgrade added.") {
		t.Errorf("the summary's checks pair = %q, want its checks", checks)
	}
}

// A member in accepting that the root architect owns is named as the architect's own to answer:
// what it is owed, then its sign-off or a move back to planning, implementing or testing.
func TestTheSummaryNamesAnAcceptingMemberTheArchitectAnswersItself(t *testing.T) {
	text := RenderNews(treeSummaryNews())
	line := newsLine(t, text, "- LEGION-211, in accepting: ")
	for _, want := range []string{"owed version 5 of the spec document, since version 4", "you answer it yourself", "`sign_off`",
		"`request_backward_move`", "planning, implementing or testing"} {
		if !strings.Contains(line, want) {
			t.Errorf("the accepting member's line %q does not say %q", line, want)
		}
	}
}

// No news text names a notice kind as a notice does. A phase worker's session holds no notice:
// every kind is an architect's or the controller's, and the end-to-end proof fails a run whose
// worker session holds a delivery reading `summary: <kind> on …` (scripts/e2e/lib/workflow.sh,
// worker_notices). So no kind of more than one word appears anywhere, and no kind appears in the
// form a notice's summary takes. A kind of one word is an English word as well — "held", which
// the gate's own opening sentence says of a merge and which names a phase — and may stand as one.
// The kinds are the record's own, asked of its validation one word at a time (noticeKind), so a
// kind added to the record is held here with no list to keep.
func TestNewsNamesNoNoticeKind(t *testing.T) {
	if !noticeKind("phase-finished") || !noticeKind("triage") || noticeKind("approved") {
		t.Fatal("the record's validation no longer tells a notice kind from another word, so this test cannot read the kinds")
	}
	words := regexp.MustCompile(`[a-z]+(?:-[a-z]+)*`)
	for name, text := range everyNewsForm() {
		lower := strings.ToLower(text)
		for _, word := range words.FindAllString(lower, -1) {
			if !noticeKind(word) {
				continue
			}
			if strings.Contains(word, "-") {
				t.Errorf("the news to %s names the notice kind %q:\n%s", name, word, text)
			}
			if strings.Contains(lower, word+" on ") {
				t.Errorf("the news to %s reads %q as a notice's summary does:\n%s", name, word+" on", text)
			}
		}
	}
}

// noticeKind says whether word is a kind of notice the daemon publishes, to an architect or to
// the controller: the record's validation of a controller notice takes the kinds of both and
// refuses every other kind as unknown.
func noticeKind(word string) bool {
	_, err := record.NewOutboxRow(newsTree, record.ControllerNotice{Kind: record.NoticeKind(word)}, time.Time{})
	return err == nil || !strings.Contains(err.Error(), "unknown controller notice kind")
}

// firstLine is the first line of text.
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// lastParagraph is the last paragraph of text.
func lastParagraph(text string) string {
	paragraphs := strings.Split(text, "\n\n")
	return paragraphs[len(paragraphs)-1]
}
