package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
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

// newsDocuments is the document each row of newsGates and closedGates is bound to, as the daemon's
// send names it (News.Documents): each row records the root's own document.
func newsDocuments() map[record.DesignStep]string {
	return map[record.DesignStep]string{record.DesignStepSpec: specDocument, record.DesignStepCriteria: checksDocument}
}

// specChanges is what changed in the spec from version from to version 5, as Dispatch names it:
// four units, among them the run of text outside every section that stands after the "Rollout"
// section and the second section headed "Notes", three requirements and a callout.
func specChanges(from int) NewsChange {
	return NewsChange{Step: record.DesignStepSpec, Changes: record.DesignChanges{
		VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: from, To: 5}, State: record.DesignChangesRead,
		Sections: []record.SectionChange{{Heading: "Design", Change: "changed"}, {Heading: "Rollout", Change: "added"},
			{Heading: "", Change: "changed", Place: dispatch.PlaceAfterSection, AfterHeading: "Rollout"},
			{Heading: "Notes", Occurrence: 1, Change: "removed"}},
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
		Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(), OpeningOwed: true, Items: []NewsChange{specChanges(3), checksChanges()}}
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
		Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(), Items: []NewsChange{unread}}
}

// unreadBaselineChanges is the item of an issue whose told row knows no version of the spec and
// whose review history Dispatch could not read: it names no pair, only the failed read and its
// reason (the daemon's designChanges).
func unreadBaselineChanges() NewsChange {
	return NewsChange{Step: record.DesignStepSpec, HistoryUnread: &record.DesignHistoryUnread{
		DesignHistoryQuestion: record.DesignHistoryQuestion{ArtifactID: "artifact-spec", To: 5},
		Reason:                "Dispatch failed 3 reads of the document's review history, the last with: dispatch: 502 Bad Gateway"}}
}

// unreadBaselineHolderNews is the news of a child's tester whose taken version the review history
// could not give.
func unreadBaselineHolderNews() News {
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: "LEGION-217", Phase: phase.Testing,
		Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(), Items: []NewsChange{unreadBaselineChanges()}}
}

// firstApprovalHolderNews is the news of a child's planner whose phase began before any version
// of the spec was approved.
func firstApprovalHolderNews() News {
	first := specChanges(0)
	first.Changes.Sections, first.Changes.Blocks = nil, nil
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: "LEGION-213", Phase: phase.Planning,
		Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(), Items: []NewsChange{first}}
}

// acceptingHolderNews is the news of a child standing in accepting, whose holder is the
// sub-architect that owns it.
func acceptingHolderNews() News {
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: "LEGION-210", Phase: phase.Accepting,
		Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(), Items: []NewsChange{specChanges(4)}}
}

// plannerHolderNews is the news of a child's planner, which took spec version 4.
func plannerHolderNews() News {
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: "LEGION-218", Phase: phase.Planning,
		Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(), Items: []NewsChange{specChanges(4)}}
}

// decisionOnlyHolderNews is the one news task of a version whose only difference is decision
// blocks: the gate closed on it, its holder was told so, and a person approved it later, so the
// opening is owed and nothing else is.
func decisionOnlyHolderNews() News {
	decisions := NewsChange{Step: record.DesignStepSpec, Changes: record.DesignChanges{
		VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 4, To: 5}, State: record.DesignChangesRead, DecisionOnly: true}}
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: "LEGION-211", Phase: phase.Testing,
		Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(), OpeningOwed: true, Items: []NewsChange{decisions}}
}

// unchangedChanges is a pair Dispatch read as unchanged: version 5 of the spec holds the same text
// as version 4.
func unchangedChanges() NewsChange {
	return NewsChange{Step: record.DesignStepSpec, Changes: record.DesignChanges{
		VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 4, To: 5}, State: record.DesignChangesRead, Unchanged: true}}
}

// unchangedHolderNews is the one news task of a version that holds the same text as the one
// before it: the gate closed on it, its holder was told so, and a person approved it later, so the
// opening is owed and nothing else is.
func unchangedHolderNews() News {
	return News{Audience: record.AudienceHolder, Tree: newsTree, Issue: "LEGION-211", Phase: phase.Testing,
		Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(), OpeningOwed: true, Items: []NewsChange{unchangedChanges()}}
}

// treeSummaryNews is the root architect's summary of a spec change over the whole tree: the
// architect last took spec version 3 and checks version 2, and each member stands somewhere else.
func treeSummaryNews() News {
	return News{Audience: record.AudienceArchitect, Tree: newsTree, Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(),
		Items: []NewsChange{specChanges(3), checksChanges()},
		Members: []NewsMember{
			{Issue: newsTree, Phase: phase.AwaitingPerson, State: NewsAwaitingPerson},
			{Issue: "LEGION-209", Phase: phase.Implementing, State: NewsSentNow, Items: []NewsChange{specChanges(3)}},
			{Issue: "LEGION-210", Phase: phase.Testing, State: NewsAtNextStart, Items: []NewsChange{specChanges(4)}},
			{Issue: "LEGION-211", Phase: phase.Accepting, State: NewsYouAnswer, Items: []NewsChange{specChanges(4)}},
			{Issue: "LEGION-212", Phase: phase.Reviewing, State: NewsWorkUnchanged},
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
	return News{Audience: record.AudienceArchitect, Tree: newsTree, Steps: newsSteps, Gates: gates, Documents: newsDocuments(), OpeningOwed: true,
		Items: []NewsChange{checksChanges()},
		Members: []NewsMember{
			{Issue: newsTree, Phase: phase.Merging, State: NewsMerged},
			{Issue: "LEGION-209", Phase: phase.Implementing, State: NewsNotConcerned},
			{Issue: "LEGION-210", Phase: phase.Admitted, State: NewsOutOfWorkflow},
		}}
}

// closedSummaryNews is treeSummaryNews sent after the writer saved version 6 of the spec, which
// closed the gate again: the members its news was owed to are told nothing yet.
func closedSummaryNews() News {
	return News{Audience: record.AudienceArchitect, Tree: newsTree, Steps: newsSteps, Gates: closedGates(), Documents: newsDocuments(),
		Items: []NewsChange{specChanges(3)},
		Members: []NewsMember{
			{Issue: newsTree, Phase: phase.AwaitingPerson, State: NewsAwaitingPerson},
			{Issue: "LEGION-209", Phase: phase.Implementing, State: NewsGateClosed},
		}}
}

// waitingSummaryNews is the root architect's summary of a spec change while two members' news
// waits for a task not taken yet: a tester whose claim holds one, and a member in accepting owned
// by a sub-architect whose claim holds one.
func waitingSummaryNews() News {
	return News{Audience: record.AudienceArchitect, Tree: newsTree, Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(),
		Items: []NewsChange{specChanges(3)},
		Members: []NewsMember{
			{Issue: "LEGION-210", Phase: phase.Testing, State: NewsWaitsForTask, Items: []NewsChange{specChanges(4)}},
			{Issue: "LEGION-211", Phase: phase.Accepting, State: NewsWaitsForTask, Items: []NewsChange{specChanges(4)}},
		}}
}

// everyNewsForm is each news text the fixtures render, by name.
func everyNewsForm() map[string]string {
	return map[string]string{
		"the root holder owed the opening":                   RenderNews(rootHolderNews()),
		"the holder sent news after the gate closed":         RenderNews(closedHolderNews()),
		"the holder of an unread pair":                       RenderNews(unreadHolderNews()),
		"the holder of the first approved version":           RenderNews(firstApprovalHolderNews()),
		"the holder whose taken version could not be read":   RenderNews(unreadBaselineHolderNews()),
		"the sub-architect of an accepting child":            RenderNews(acceptingHolderNews()),
		"the planner of a child":                             RenderNews(plannerHolderNews()),
		"the holder of a decision-only version":              RenderNews(decisionOnlyHolderNews()),
		"the holder of an unchanged version":                 RenderNews(unchangedHolderNews()),
		"the root architect's tree summary":                  RenderNews(treeSummaryNews()),
		"the root architect of a merged root":                RenderNews(mergedRootSummaryNews()),
		"the root architect's summary after the gate closed": RenderNews(closedSummaryNews()),
		"the root architect's summary of waiting news":       RenderNews(waitingSummaryNews()),
	}
}

// mergerQuote is the gate and merge fields prompts/roles/merger.md names for the gate-moved
// message that moves a merge, move being open or closed: `gate: open` and `merge: released`, or
// `gate: closed` and `merge: held`, the fields a merger reads for.
func mergerQuote(t *testing.T, move string) (gate, merge string) {
	t.Helper()
	prompt, err := os.ReadFile(filepath.Join("..", "prompts", "roles", "merger.md"))
	if err != nil {
		t.Fatalf("read the merger's prompt: %v", err)
	}
	quoted := regexp.MustCompile("`gate: (" + move + ")` and `merge: ([a-z]+)`").FindStringSubmatch(string(prompt))
	if quoted == nil {
		t.Fatalf("the merger's prompt names no gate-moved message with `gate: %s` and its merge", move)
	}
	return quoted[1], quoted[2]
}

// wantGate fails the test unless news, rendered as text, carries the gate's fields as the merger's
// prompt names the move, with the step, the document, the artifact and the version, ahead of its
// items.
func wantGate(t *testing.T, news News, move string, step record.DesignStep, document, artifact string, version int) {
	t.Helper()
	rec, text := NewsRecord(news), RenderNews(news)
	t.Logf("the news:\n%s", text)
	gate, merge := mergerQuote(t, move)
	if rec.Gate != gate || rec.Merge != merge || rec.GateStep != string(step) || rec.GateDocument != document ||
		rec.GateArtifact != artifact || rec.GateVersion != version {
		t.Fatalf("the news's gate = %q %q %q %q %q %d, want %q %q %q %q %q %d", rec.Gate, rec.Merge, rec.GateStep, rec.GateDocument,
			rec.GateArtifact, rec.GateVersion, gate, merge, step, document, artifact, version)
	}
	for _, line := range []string{"gate: " + gate, "merge: " + merge} {
		if !hasLine(text, line) {
			t.Fatalf("the news has no line %q, the merger prompt's field:\n%s", line, text)
		}
	}
	if at, items := strings.Index(text, "\ngate: "), strings.Index(text, "\nitems["); items >= 0 && at > items {
		t.Fatalf("the news names its items before the gate:\n%s", text)
	}
}

// hasLine says whether text holds line as a line of its own.
func hasLine(text, line string) bool {
	return slices.Contains(strings.Split(text, "\n"), line)
}

// wordsText is the text of w, "" for none.
func wordsText(w *texts.Words) string {
	if w == nil {
		return ""
	}
	return w.Text
}

// itemLine is an item of a news record on one line, to compare and print.
func itemLine(item texts.DesignNewsItem) string {
	taken, yours := "null", "-"
	if item.Taken != nil {
		taken = strconv.Itoa(*item.Taken)
	}
	if item.Yours != nil {
		yours = strconv.FormatBool(*item.Yours)
	}
	return fmt.Sprintf("%s %s %s %d taken=%s yours=%s %s %q", item.Step, item.Document, item.Artifact, item.Version, taken, yours,
		item.Changes, wordsText(item.Reason))
}

// sectionLine is a section row of a news record on one line.
func sectionLine(s texts.DesignNewsSection) string {
	return fmt.Sprintf("%d %s %d %q %s after %d %q", s.Item, s.Unit, s.Nth, wordsText(s.Heading), s.Change, s.AfterNth, wordsText(s.AfterHeading))
}

// blockLine is a block row of a news record on one line.
func blockLine(b texts.DesignNewsBlock) string {
	return fmt.Sprintf("%d %s %q %q %s", b.Item, b.Type, wordsText(b.OtherType), wordsText(b.ID), b.Change)
}

// memberLine is a member row of a news record on one line.
func memberLine(m texts.DesignNewsMember) string {
	return fmt.Sprintf("%s %s told=%s why=%s holder=%s architect_of=%s", m.Issue, m.Phase, m.Told, m.Why, m.Holder, m.ArchitectOf)
}

// owedLine is an owed row of a news record on one line.
func owedLine(o texts.DesignNewsOwed) string { return fmt.Sprintf("%s %d", o.Issue, o.Item) }

// linesOf is each row of rows as line writes it.
func linesOf[T any](rows []T, line func(T) string) []string {
	lines := make([]string, len(rows))
	for i, row := range rows {
		lines[i] = line(row)
	}
	return lines
}

// wantLines fails the test unless got is want, row for row.
func wantLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("the news's %s =\n  %s\nwant\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// promptParagraph is the one line of the prompt part file (under prompts/) that holds marker, a
// paragraph of that part.
func promptParagraph(t *testing.T, file, marker string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "prompts", filepath.FromSlash(file)))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var found []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, marker) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s holds %d lines that say %q, want one", file, len(found), marker)
	}
	return found[0]
}

const (
	specDocument   = "dispatch://" + newsTree + "/spec"
	checksDocument = "dispatch://" + newsTree + "/artifact/" + dispatch.VerificationSlug
)

// A holder owed the opening reads the gate's fields first, the fields the merger's prompt names as
// what releases a merge, so a merger released by its news task reads the release it waits for. A
// holder that was told the opening already reads no gate.
func TestNewsOwedTheOpeningLeadsWithTheSentenceThatReleasesAMerge(t *testing.T) {
	wantGate(t, rootHolderNews(), "open", record.DesignStepSpec, specDocument, "artifact-spec", 5)
	told := rootHolderNews()
	told.OpeningOwed = false
	if rec, got := NewsRecord(told), RenderNews(told); rec.Gate != "" || rec.Merge != "" || strings.Contains(got, "gate") {
		t.Fatalf("news to a holder already told the opening names the gate:\n%s", got)
	}
}

// News composed while the gate stood open and sent after a later version closed it says closed,
// naming the version with its reviewer, and never the opening that no longer stands: the gate is
// said of the gate at send. The spec's item keeps the approved pair.
func TestNewsSentAfterALaterVersionClosedTheGateSaysClosed(t *testing.T) {
	wantGate(t, closedHolderNews(), "closed", record.DesignStepSpec, specDocument, "artifact-spec", 6)
	text := RenderNews(closedHolderNews())
	if hasLine(text, "gate: open") || hasLine(text, "merge: released") {
		t.Fatalf("news sent while the gate is closed says it opened:\n%s", text)
	}
	if item := itemLine(NewsRecord(closedHolderNews()).Items[0]); item != `spec `+specDocument+` artifact-spec 5 taken=3 yours=- listed ""` {
		t.Fatalf("the spec's item after the close = %s, want it to keep the approved pair 3 to 5", item)
	}
}

// Each step names its own pair as the version the holder's phase last took, and its own change
// rows: the spec's sections, among them the run of text outside every section named by the section
// it follows, and its requirements and callout; the Verification document's checks, which are the
// root's to prove, and none of its other blocks. The record names the issue, the phase its holder
// holds it in, and the tree. The rule for answering and the command that prints a version are the
// role's prompt's, so the record holds neither. A first approved version names 0 as the version
// taken and no pair.
func TestNewsNamesEachStepsSectionsAndBlocksWithItsVersionPair(t *testing.T) {
	text, rec := RenderNews(rootHolderNews()), NewsRecord(rootHolderNews())
	t.Logf("the root holder's news:\n%s", text)
	if rec.Kind != texts.DesignNews || rec.Issue != newsTree || rec.Tree != newsTree || rec.Audience != texts.NewsToHolder ||
		rec.Phase != string(phase.Implementing) {
		t.Errorf("the news names %s %s of tree %s to %s in %s, want design-news of %s to its holder in implementing",
			rec.Kind, rec.Issue, rec.Tree, rec.Audience, rec.Phase, newsTree)
	}
	wantLines(t, "items", linesOf(rec.Items, itemLine), []string{
		`spec ` + specDocument + ` artifact-spec 5 taken=3 yours=- listed ""`,
		`criteria ` + checksDocument + ` artifact-checks 3 taken=2 yours=- listed ""`,
	})
	wantLines(t, "sections", linesOf(rec.Sections, sectionLine), []string{
		`0 section 1 "Design" changed after 0 ""`,
		`0 section 1 "Rollout" added after 0 ""`,
		`0 after_section 0 "" changed after 1 "Rollout"`,
		`0 section 2 "Notes" removed after 0 ""`,
		`1 section 1 "Checks" changed after 0 ""`,
	})
	wantLines(t, "blocks", linesOf(rec.Blocks, blockLine), []string{
		`0 requirement "" "r2" changed`,
		`0 requirement "" "r7" added`,
		`0 requirement "" "r3" removed`,
		`0 callout "" "note-rollback" added`,
		`1 check "" "c-live" changed`,
		`1 check "" "c-upgrade" added`,
	})
	for _, never := range []string{"aside-ci", "request_backward_move", "dispatch comment", "doc-read"} {
		if strings.Contains(text, never) {
			t.Errorf("the news says %q:\n%s", never, text)
		}
	}
	for _, field := range []string{"kind: design-news", "issue: " + newsTree, "tree: " + newsTree, "audience: holder", "phase: implementing"} {
		if !hasLine(text, field) {
			t.Errorf("the news has no line %q:\n%s", field, text)
		}
	}
	first := NewsRecord(firstApprovalHolderNews())
	t.Logf("the news of a first approved version:\n%s", RenderNews(firstApprovalHolderNews()))
	wantLines(t, "items of a first approval", linesOf(first.Items, itemLine), []string{`spec ` + specDocument + ` artifact-spec 5 taken=0 yours=- no_pair ""`})
	if len(first.Sections)+len(first.Blocks) != 0 {
		t.Errorf("the news of a version with no approved one before it names changes: %+v %+v", first.Sections, first.Blocks)
	}
	if want := "so read the new version whole"; !strings.Contains(promptParagraph(t, "go/worker-common.md", "`no_pair`"), want) {
		t.Errorf("the phase workers' prompt does not say of `no_pair` %q", want)
	}
}

// A pair Dispatch could not read names the versions, says the change list could not be read, and
// carries the reason in a block by the daemon, whose sentence quotes Dispatch's error, with no
// change rows.
func TestNewsOfAnUnreadPairNamesTheReasonAndTheCommandThatPrintsIt(t *testing.T) {
	rec := NewsRecord(unreadHolderNews())
	t.Logf("the news of an unread pair:\n%s", RenderNews(unreadHolderNews()))
	wantLines(t, "items", linesOf(rec.Items, itemLine), []string{`spec ` + specDocument + ` artifact-spec 5 taken=4 yours=- unavailable ` +
		`"dispatch has no changes route: GET /api/v1/artifacts/artifact-spec/changes answered 404 Not Found"`})
	if by := rec.Items[0].Reason.By; by != texts.ByDaemon {
		t.Errorf("the reason's block is by %q, want %s, whose sentence quotes Dispatch's error", by, texts.ByDaemon)
	}
	if len(rec.Sections)+len(rec.Blocks) != 0 {
		t.Errorf("the unread pair's news names changes it could not read: %+v %+v", rec.Sections, rec.Blocks)
	}
	if want := "`dispatch doc-read --issue <tree> --artifact <artifact> --version <version>`"; !strings.Contains(
		promptParagraph(t, "go/worker-common.md", "`unavailable`"), want) {
		t.Errorf("the phase workers' prompt does not give the command that prints the version, %s", want)
	}
}

// An item whose review history could not be read is not a first approval: the history could not
// say which version the issue took, so the item names none (null) and no pair, with the reason the
// history could not be read, and never 0. The architect's summary names the member the same way,
// and lists the item as an entry of its own beside the first approval of the same version, which
// another member read from the history, so the one is never folded into the other.
func TestNewsOfAnItemWhoseReviewHistoryCouldNotBeReadSaysSoAndNotAFirstApproval(t *testing.T) {
	const reason = "Dispatch failed 3 reads of the document's review history, the last with: dispatch: 502 Bad Gateway"
	rec := NewsRecord(unreadBaselineHolderNews())
	t.Logf("the news of an item whose review history could not be read:\n%s", RenderNews(unreadBaselineHolderNews()))
	wantLines(t, "items", linesOf(rec.Items, itemLine), []string{`spec ` + specDocument + ` artifact-spec 5 taken=null yours=- no_pair "` + reason + `"`})
	if text := RenderNews(unreadBaselineHolderNews()); !hasLine(text, "items[1]{step,document,artifact,version,taken,changes}:") ||
		!strings.Contains(text, ",5,null,no_pair") {
		t.Errorf("the rendered item does not say null for the version taken:\n%s", text)
	}

	first := firstApprovalHolderNews().Items[0]
	summary := News{Audience: record.AudienceArchitect, Tree: newsTree, Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(),
		Members: []NewsMember{
			{Issue: "LEGION-217", Phase: phase.Testing, State: NewsSentNow, Items: []NewsChange{unreadBaselineChanges()}},
			{Issue: "LEGION-213", Phase: phase.Planning, State: NewsSentNow, Items: []NewsChange{first}},
		}}
	got := NewsRecord(summary)
	t.Logf("the summary:\n%s", RenderNews(summary))
	wantLines(t, "summary's items", linesOf(got.Items, itemLine), []string{
		`spec ` + specDocument + ` artifact-spec 5 taken=null yours=false no_pair "` + reason + `"`,
		`spec ` + specDocument + ` artifact-spec 5 taken=0 yours=false no_pair ""`,
	})
	wantLines(t, "summary's owed", linesOf(got.Owed, owedLine), []string{"LEGION-217 0", "LEGION-213 1"})
}

// The architect holding a child in accepting reads the phase it holds it in; its answer is the
// one its prompt gives for accepting, its sign-off or a move back to the phase of an earlier step
// of its task (main's any-order steps), and never a phase worker's comment, which the record names
// neither.
func TestNewsToTheArchitectHoldingAnAcceptingIssueEndsWithItsAnswer(t *testing.T) {
	text, rec := RenderNews(acceptingHolderNews()), NewsRecord(acceptingHolderNews())
	t.Logf("the news of an accepting child:\n%s", text)
	if rec.Issue != "LEGION-210" || rec.Audience != texts.NewsToHolder || rec.Phase != string(phase.Accepting) {
		t.Errorf("the news names %s to %s in %s, want LEGION-210 to its holder in accepting", rec.Issue, rec.Audience, rec.Phase)
	}
	rule := promptParagraph(t, "go/architect-common.md", "`audience: holder` and `phase: accepting`")
	for _, want := range []string{"sign the issue off with `sign_off`", "`request_backward_move`", "to the phase of an earlier step of its task"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the architect's prompt for an accepting issue's news does not say %q", want)
		}
	}
	if strings.Contains(rule, "`dispatch comment`") {
		t.Errorf("the architect's prompt answers an accepting issue's news as a phase worker does: %s", rule)
	}
	for _, never := range []string{"dispatch comment", "sign_off", "request_backward_move"} {
		if strings.Contains(text, never) {
			t.Errorf("the accepting holder's news says %q, the answer its prompt gives:\n%s", never, text)
		}
	}
}

// A stale plan is the planner's own work wherever its task places its step, so a planner's news
// names the phase its step holds, and the planner's own answer is its prompt's
// (prompts/go/planner.md): a stale plan is revised in place and its plan review run again, and a
// current one gets a comment. No move back is named, and the prompt no longer says that planning
// has no earlier phase, which is false for a planner whose step comes after the implementer's.
func TestNewsToAPlannerEndsWithThePlannersAnswer(t *testing.T) {
	text, rec := RenderNews(plannerHolderNews()), NewsRecord(plannerHolderNews())
	t.Logf("the news of a planner:\n%s", text)
	if rec.Issue != "LEGION-218" || rec.Phase != string(phase.Planning) {
		t.Errorf("the news names %s in %s, want LEGION-218 in planning", rec.Issue, rec.Phase)
	}
	rule := promptParagraph(t, "go/planner.md", "news task")
	if strings.Contains(rule, "Planning has no earlier phase") {
		t.Errorf("the planner's rule for its news still says planning has no earlier phase, false for a planner after the implementer: %q", rule)
	}
	for _, want := range []string{"A stale plan is your own work wherever your task places your step", "Revise `plan.md` in place to match the new version of the design",
		"run the plan review again", "`dispatch comment`"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the planner's prompt for its news does not say %q", want)
		}
	}
	if strings.Contains(text, "request_backward_move") || strings.Contains(rule, "request_backward_move") {
		t.Errorf("the planner is told to move back, which planning cannot do:\n%s\n%s", text, rule)
	}
}

// A version whose only difference is decision blocks, owed only for its opening, tells the holder
// the gate opened and that the version its phase took differs from the new one only in decision
// blocks, so nothing its issue holds changed: no change rows, no command and no rule.
func TestADecisionOnlyVersionOwedTheOpeningSaysNothingChanged(t *testing.T) {
	wantGate(t, decisionOnlyHolderNews(), "open", record.DesignStepSpec, specDocument, "artifact-spec", 5)
	text, rec := RenderNews(decisionOnlyHolderNews()), NewsRecord(decisionOnlyHolderNews())
	wantLines(t, "items", linesOf(rec.Items, itemLine), []string{`spec ` + specDocument + ` artifact-spec 5 taken=4 yours=- decision_blocks_only ""`})
	for _, unwanted := range []string{"sections[", "blocks[", "doc-read", "request_backward_move", "dispatch comment"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the news of a decision-only version says %q:\n%s", unwanted, text)
		}
	}
	if want := "nothing your issue holds changed since version `taken`"; !strings.Contains(promptParagraph(t, "go/worker-common.md", "`decision_blocks_only`, it differs"), want) {
		t.Errorf("the phase workers' prompt does not say of `decision_blocks_only` %q", want)
	}
}

// A version that holds the same text as the one before it, owed only for its opening, tells the
// holder the gate opened, the version its phase took, and why nothing its issue holds changed: the
// new version holds the same text. It names no change rows, empty or not, no decision blocks, and
// no command and no rule.
func TestAnUnchangedVersionOwedTheOpeningSaysItHoldsTheSameText(t *testing.T) {
	wantGate(t, unchangedHolderNews(), "open", record.DesignStepSpec, specDocument, "artifact-spec", 5)
	text, rec := RenderNews(unchangedHolderNews()), NewsRecord(unchangedHolderNews())
	wantLines(t, "items", linesOf(rec.Items, itemLine), []string{`spec ` + specDocument + ` artifact-spec 5 taken=4 yours=- same_text ""`})
	for _, unwanted := range []string{"sections[", "blocks[", "none_named", "decision", "doc-read", "request_backward_move", "dispatch comment"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the news of an unchanged version says %q:\n%s", unwanted, text)
		}
	}
}

// An unchanged pair is named as changing nothing, and why, with no change rows naming it, in each
// form: in the holder's items beside a pair that changed, and in the root architect's summary,
// which names the unchanged pair once and a member owed nothing else as told nothing, for the
// reason true of a version that holds the same text. The control is the changed pair, whose change
// rows each form still names.
func TestAnUnchangedPairIsNamedWithNoChangeListInEveryForm(t *testing.T) {
	unchangedChecks := NewsChange{Step: record.DesignStepCriteria, Changes: record.DesignChanges{
		VersionPair: record.VersionPair{ArtifactID: "artifact-checks", From: 2, To: 3}, State: record.DesignChangesRead, Unchanged: true}}
	holder := NewsRecord(News{Audience: record.AudienceHolder, Tree: newsTree, Issue: newsTree, Phase: phase.Implementing,
		Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(), Items: []NewsChange{specChanges(3), unchangedChecks}})
	wantLines(t, "holder's items", linesOf(holder.Items, itemLine), []string{
		`spec ` + specDocument + ` artifact-spec 5 taken=3 yours=- listed ""`,
		`criteria ` + checksDocument + ` artifact-checks 3 taken=2 yours=- same_text ""`,
	})
	for _, row := range append(linesOf(holder.Sections, sectionLine), linesOf(holder.Blocks, blockLine)...) {
		if !strings.HasPrefix(row, "0 ") {
			t.Errorf("the holder's news names a change of the unchanged checks: %s", row)
		}
	}
	if len(holder.Sections) == 0 {
		t.Error("the holder's news names no section of the changed spec, the control")
	}

	summary := NewsRecord(News{Audience: record.AudienceArchitect, Tree: newsTree, Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(),
		Items: []NewsChange{unchangedChanges()},
		Members: []NewsMember{
			{Issue: "LEGION-209", Phase: phase.Implementing, State: NewsSentNow, Items: []NewsChange{specChanges(3)}},
			{Issue: "LEGION-212", Phase: phase.Reviewing, State: NewsWorkUnchanged},
		}})
	wantLines(t, "summary's items", linesOf(summary.Items, itemLine), []string{
		`spec ` + specDocument + ` artifact-spec 5 taken=4 yours=true same_text ""`,
		`spec ` + specDocument + ` artifact-spec 5 taken=3 yours=false listed ""`,
	})
	for _, row := range append(linesOf(summary.Sections, sectionLine), linesOf(summary.Blocks, blockLine)...) {
		if !strings.HasPrefix(row, "1 ") {
			t.Errorf("the summary names a change of the unchanged pair: %s", row)
		}
	}
	wantLines(t, "summary's members", linesOf(summary.Members, memberLine), []string{
		"LEGION-209 implementing told=now why= holder=implementer architect_of=",
		"LEGION-212 reviewing told=nothing why=work_unchanged holder= architect_of=",
	})
	wantLines(t, "summary's owed", linesOf(summary.Owed, owedLine), []string{"LEGION-209 1"})
}

// Each run of the text outside every `##` section is named where Dispatch says it stands: the run
// above the first section before the first section, and any other run after the `##` section
// directly before it, a repeated heading by its place among the sections of that heading and a
// heading with no words by its place among such headings. A `##` section whose heading has no
// words is a section, never the text outside every section. Each unit keeps Dispatch's word for
// how it changed.
func TestNewsNamesEachRunOfTheTextOutsideTheSectionsWhereItStands(t *testing.T) {
	news := News{Audience: record.AudienceHolder, Tree: newsTree, Issue: newsTree, Phase: phase.Reviewing,
		Steps: newsSteps, Gates: newsGates(), Documents: newsDocuments(),
		Items: []NewsChange{{Step: record.DesignStepSpec, Changes: record.DesignChanges{
			VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 3, To: 5}, State: record.DesignChangesRead,
			Sections: []record.SectionChange{
				{Heading: "", Change: "moved", Place: dispatch.PlaceBeforeFirstSection},
				{Heading: "Design", Change: "changed"},
				{Heading: "", Change: "changed", Place: dispatch.PlaceAfterSection, AfterHeading: "Design"},
				{Heading: "Notes", Occurrence: 1, Change: "added"},
				{Heading: "", Change: "added", Place: dispatch.PlaceAfterSection, AfterHeading: "Notes", AfterOccurrence: 1},
				{Heading: "", Occurrence: 1, Change: "changed"},
				{Heading: "", Change: "removed", Place: dispatch.PlaceAfterSection, AfterHeading: "", AfterOccurrence: 1},
			}}}}}
	t.Logf("the news of runs at each place:\n%s", RenderNews(news))
	wantLines(t, "sections", linesOf(NewsRecord(news).Sections, sectionLine), []string{
		`0 before_first_section 0 "" moved after 0 ""`,
		`0 section 1 "Design" changed after 0 ""`,
		`0 after_section 0 "" changed after 1 "Design"`,
		`0 section 2 "Notes" added after 0 ""`,
		`0 after_section 0 "" added after 2 "Notes"`,
		`0 section 1 "" changed after 0 ""`,
		`0 after_section 0 "" removed after 1 ""`,
	})
}

// The root architect's one summary names every member of the tree with its phase, the role that
// holds it, and how it is told: owed and sent now, owed and carried by its next start, or told
// nothing and why. Each pair of versions it names comes once with its change rows, the architect's
// own marked as what it took, and each member owed news names the pair it is owed. A member that
// stands in held is told nothing whatever holds it (`why: stopped`), and the root architect's
// prompt says that the release that moves it back into a phase carries what it is owed then.
func TestTheArchitectsSummaryNamesEveryMemberWithItsStateAndReason(t *testing.T) {
	text, rec := RenderNews(treeSummaryNews()), NewsRecord(treeSummaryNews())
	t.Logf("the root architect's summary:\n%s", text)
	if rec.Issue != newsTree || rec.Audience != texts.NewsToRootArchitect || rec.Phase != "" {
		t.Errorf("the summary names %s to %s in %q, want the tree's root to its root architect in no phase", rec.Issue, rec.Audience, rec.Phase)
	}
	wantLines(t, "members", linesOf(rec.Members, memberLine), []string{
		"LEGION-208 awaiting_person told=nothing why=waits_on_person holder= architect_of=",
		"LEGION-209 implementing told=now why= holder=implementer architect_of=",
		"LEGION-210 testing told=at_next_start why= holder=tester architect_of=",
		"LEGION-211 accepting told=by_you why= holder=architect architect_of=",
		"LEGION-212 reviewing told=nothing why=work_unchanged holder= architect_of=",
		"LEGION-213 planning told=nothing why=already_taken holder= architect_of=",
		"LEGION-214 admitted told=nothing why=not_started holder= architect_of=",
		"LEGION-215 held told=nothing why=stopped holder= architect_of=",
		"LEGION-216 done told=nothing why=done holder= architect_of=",
	})
	wantLines(t, "items", linesOf(rec.Items, itemLine), []string{
		`spec ` + specDocument + ` artifact-spec 5 taken=3 yours=true listed ""`,
		`criteria ` + checksDocument + ` artifact-checks 3 taken=2 yours=true listed ""`,
		`spec ` + specDocument + ` artifact-spec 5 taken=4 yours=false listed ""`,
	})
	wantLines(t, "owed", linesOf(rec.Owed, owedLine), []string{"LEGION-209 0", "LEGION-210 2", "LEGION-211 2"})
	for i, want := range []int{4, 2, 4} {
		named := 0
		for _, block := range rec.Blocks {
			if block.Item == i {
				named++
			}
		}
		if named != want {
			t.Errorf("the summary names %d blocks of item %d, want %d", named, i, want)
		}
	}
	why := promptParagraph(t, "go/architect-common.md", "`audience: root_architect`")
	for _, want := range []string{"`stopped`, it stands in `held`, whatever holds it",
		"it is told nothing while it is held: the release that moves it back into a phase carries what it is owed then"} {
		if !strings.Contains(why, want) {
			t.Errorf("the root architect's prompt for its summary does not say %q", want)
		}
	}
}

// A member whose pull request has merged takes no new work, so the architect's prompt hands it the
// duty that follows: the work goes to an issue of the tree whose pull request is still open, or to
// a person. A change to the checks alone still reaches the architect of a merged root, which owes no
// member anything, and names why each other member is told nothing. Its gate names the approval
// that opened it, the Verification document's, which Dispatch recorded last.
func TestTheSummaryGivesTheArchitectTheDutyOfAMergedMember(t *testing.T) {
	wantGate(t, mergedRootSummaryNews(), "open", record.DesignStepCriteria, checksDocument, "artifact-checks", 3)
	rec := NewsRecord(mergedRootSummaryNews())
	wantLines(t, "members", linesOf(rec.Members, memberLine), []string{
		"LEGION-208 merging told=nothing why=merged holder= architect_of=",
		"LEGION-209 implementing told=nothing why=root_checks_only holder= architect_of=",
		"LEGION-210 admitted told=nothing why=out_of_workflow holder= architect_of=",
	})
	duty := promptParagraph(t, "go/architect-common.md", "`audience: root_architect`")
	for _, want := range []string{"`merged`, its pull request has merged", "a merged change never moves back",
		"give that work to an issue of this tree whose pull request is still open, or `dispatch ask` the person on the root"} {
		if !strings.Contains(duty, want) {
			t.Errorf("the root architect's prompt for its summary does not say %q", want)
		}
	}
	wantLines(t, "blocks", linesOf(rec.Blocks, blockLine), []string{`0 check "" "c-live" changed`, `0 check "" "c-upgrade" added`})
}

// A member in accepting that the root architect owns is named as the architect's own to answer:
// the pair it is owed, and the prompt's answer, its sign-off or a move back to the phase of an
// earlier step of its task.
func TestTheSummaryNamesAnAcceptingMemberTheArchitectAnswersItself(t *testing.T) {
	rec := NewsRecord(treeSummaryNews())
	if member := memberLine(rec.Members[3]); member != "LEGION-211 accepting told=by_you why= holder=architect architect_of=" {
		t.Errorf("the accepting member = %s, want it told by you", member)
	}
	if !slices.Contains(linesOf(rec.Owed, owedLine), "LEGION-211 2") || itemLine(rec.Items[2]) != `spec `+specDocument+` artifact-spec 5 taken=4 yours=false listed ""` {
		t.Errorf("the accepting member is owed %v of %v, want the spec since version 4", rec.Owed, linesOf(rec.Items, itemLine))
	}
	answer := promptParagraph(t, "go/architect-common.md", "`audience: root_architect`")
	for _, want := range []string{"`by_you`, you hold it in `accepting`, so you answer it yourself", "`sign_off`", "`request_backward_move`",
		"to the phase of an earlier step of its task"} {
		if !strings.Contains(answer, want) {
			t.Errorf("the root architect's prompt for its summary does not say %q", want)
		}
	}
}

// A summary sent after a later version closed the gate again tells the architect that a member
// its news was owed to is told nothing yet, and why: no task carries that news and no start will
// while the gate is closed, so no member is told now or at its next start.
func TestTheSummarySentAfterTheGateClosedAgainTellsAnOwedMemberNothingYet(t *testing.T) {
	rec := NewsRecord(closedSummaryNews())
	wantLines(t, "members", linesOf(rec.Members, memberLine), []string{
		"LEGION-208 awaiting_person told=nothing why=waits_on_person holder= architect_of=",
		"LEGION-209 implementing told=not_yet why=gate_closed holder= architect_of=",
	})
	if want := "what it is owed goes once a person approves the version now with its reviewer"; !strings.Contains(
		promptParagraph(t, "go/architect-common.md", "`not_yet`"), want) {
		t.Errorf("the root architect's prompt does not say of `not_yet` %q", want)
	}
}

// A member whose holder holds a task it has not taken yet, or whose accepting issue a sub-architect
// owns while it holds one, gets no news-only start until a turn takes that task. The summary says
// the news waits for that task, naming the role that holds it. It never says that a task tells the
// member now, that its next start carries the news, or that the root architect answers it.
func TestTheSummarySaysAMembersNewsWaitsWhileItsHolderHoldsATaskItHasNotTaken(t *testing.T) {
	rec := NewsRecord(waitingSummaryNews())
	t.Logf("the summary of waiting news:\n%s", RenderNews(waitingSummaryNews()))
	wantLines(t, "members", linesOf(rec.Members, memberLine), []string{
		"LEGION-210 testing told=when_task_taken why= holder=tester architect_of=",
		"LEGION-211 accepting told=when_task_taken why= holder=architect architect_of=",
	})
	wantLines(t, "owed", linesOf(rec.Owed, owedLine), []string{"LEGION-210 1", "LEGION-211 1"})
	if want := "its holder holds a task it has not taken yet, and the news waits until it takes that task"; !strings.Contains(
		promptParagraph(t, "go/architect-common.md", "`when_task_taken`"), want) {
		t.Errorf("the root architect's prompt does not say of `when_task_taken` %q", want)
	}
}

// No news text names a notice kind as a notice does. A phase worker's session holds no notice:
// every kind is an architect's or the controller's, and the end-to-end proof fails a run whose
// worker session holds a delivery reading `summary: <kind> on …` (scripts/e2e/lib/workflow.sh,
// worker_notices). So no kind of more than one word appears anywhere, and no kind appears in the
// form a notice's summary takes. A kind of one word is an English word as well — "held", which
// the gate's own merge field says and which names a phase — and may stand as one. The kinds are
// the one list's own, asked of it one word at a time (noticeKind), so a kind added to the list is
// held here with no list to keep. A member that waits on a person, whose state is spelled as the
// controller's notice kind, is among the forms.
func TestNewsNamesNoNoticeKind(t *testing.T) {
	if !noticeKind("phase-finished") || !noticeKind("triage") || noticeKind("approved") || noticeKind(string(texts.DesignNews)) {
		t.Fatal("the one list no longer tells a notice kind from another word, so this test cannot read the kinds")
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
// the controller, as the one list names them (texts.IsNotice).
func noticeKind(word string) bool {
	return texts.IsNotice(texts.Kind(word))
}
