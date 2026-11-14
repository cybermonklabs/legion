package workflow

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// News is everything one delivery's news of the approved design is rendered from (RenderNews):
// the record as it stands when the delivery is sent, read in the one transaction that sends it,
// never as it stood when the delivery was composed. Nothing here reads the record: the daemon's
// send hands it all.
//
// Audience says whom the text speaks to, and so which of its two forms it takes:
// record.AudienceHolder is the session holding Issue's phase (a worker, or the architect that owns
// an issue standing in accepting), told what changed for that one issue; record.AudienceArchitect
// is the tree root's architect, told of the whole tree in one summary (Members).
//
// Tree is the tree root, whose documents the gate holds and on which every pointer reads them.
// Steps are the deployment's gated steps and Gates the tree root's gate rows by step, as the
// transaction reads them (record.Store.GatesOf): the gate sentence is said of them, so a news task
// composed while the gate stood open and sent after a later version closed it says closed. Each
// row carries its own Step, as the store writes it.
//
// OpeningOwed is the audience's told row's own flag (record.DesignTold.OpeningOwed): the gate
// opened while the audience was owed news, so the news leads with the gate sentence in place of
// the opening's role message.
//
// Items is what the audience is told, each with its recorded change set: the holder's items for
// Issue, or the architect row's own items for the tree. The send has already dropped every item
// that no longer stands (classify.DesignApprovalStands) and every pair whose change set is not
// recorded yet, so each one here is said as given.
type News struct {
	Audience    record.DesignAudience
	Tree        string
	Issue       string
	Phase       phase.Phase
	Steps       []record.DesignStep
	Gates       map[record.DesignStep]record.DesignGate
	OpeningOwed bool
	Items       []NewsChange
	// Members is every member of the tree, the root among them, in the order the architect's
	// summary names them. The holder's form reads none.
	Members []NewsMember
}

// NewsChange is one step's approved version as news names it: the step, and the recorded change
// set of the pair (record.DesignChanges), whose VersionPair is the version the audience took
// (From, 0 for none approved before To) and the version approved now (To).
type NewsChange struct {
	Step    record.DesignStep
	Changes record.DesignChanges
}

// NewsMember is one member of a tree as the root architect's summary names it: its key, its phase,
// what its news is (State), and, for a member owed news, the items its holder is owed.
type NewsMember struct {
	Issue string
	Phase phase.Phase
	State NewsState
	Items []NewsChange
}

// NewsState is what one member of a tree is told of an approved version, as its root architect's
// summary says it: owed news and sent now, owed news and waiting for the next start of its phase,
// owed news its root architect answers itself, or told nothing, for one reason. The daemon picks it
// from the same facts record.OwedNews reads, the delivery it queued, and the change set.
type NewsState string

const (
	// NewsSentNow is a member owed news whose holder gets a task carrying it now: a news-only
	// start to the claim given its phase's current entry.
	NewsSentNow NewsState = "sent-now"
	// NewsAtNextStart is a member owed news that the next start of its phase's holder carries: a
	// claim owed its phase start, or a phase whose holder has not started yet.
	NewsAtNextStart NewsState = "next-start"
	// NewsYouAnswer is a member in accepting that the root architect owns: the architect holds
	// its phase, so it answers the news itself, by sign_off or by a move back.
	NewsYouAnswer NewsState = "you-answer"
	// NewsDecisionOnly is a member told nothing because what changed since the version it took is
	// decision blocks alone, which tell nobody anything of the work.
	NewsDecisionOnly NewsState = "decision-only"
	// NewsNotConcerned is a member told nothing because the only step that changed is criteria,
	// whose checks are the tree root's alone to prove.
	NewsNotConcerned NewsState = "not-concerned"
	// NewsAlreadyTaken is a member told nothing because its phase already took the approved
	// version: one released after the approval, whose first entry took it as its baseline, or
	// one whose news was confirmed.
	NewsAlreadyTaken NewsState = "already-taken"
	// NewsAdmitted is a member told nothing because it has not started: its first phase starts
	// from the approved version.
	NewsAdmitted NewsState = "admitted"
	// NewsAwaitingPerson is a member told nothing while it waits on a person, since no phase
	// holder works it.
	NewsAwaitingPerson NewsState = "awaiting-person"
	// NewsStopped is a member told nothing while it stands in held: the restart that moves it
	// back into its phase carries what it is owed then.
	NewsStopped NewsState = "stopped"
	// NewsDone is a member told nothing because its work is done.
	NewsDone NewsState = "done"
	// NewsOutOfWorkflow is a member told nothing because its Dispatch status takes it out of the
	// workflow (record.InWorkflow).
	NewsOutOfWorkflow NewsState = "out-of-workflow"
	// NewsMerged is a member told nothing because its pull request has merged, and merged work
	// never moves back. The summary gives the architect the duty that follows.
	NewsMerged NewsState = "merged"
	// NewsGateClosed is a member told nothing yet because the standing gate closed again before
	// the summary was sent: record.OwedNews owes nothing while it is closed, so no task carries
	// the member's news and no start will until a person approves the version now with its
	// reviewer. It is said in place of the states that name a task or a start.
	NewsGateClosed NewsState = "gate-closed"
)

// RenderNews is the one place the text of news of an approved design is written, in the form its
// Audience takes. It is pure: the daemon's send reads the record in one transaction, drops what
// no longer stands, and hands it the rest (News).
//
// Both forms lead with the gate sentence when the opening is owed to the audience, said of the
// gate as it stands at send: the opened sentence while the standing gate is open, naming the step
// approved last, and the closed sentence, naming the version that closed it, once a later version
// has (gateOpenedSentence, gateClosedSentence, which tellGateMoved also writes). So a merger's
// release reads the words its prompt quotes, and news sent after the gate closed again says so.
//
// To a holder it then names, for each step, the version pair as the version its phase last took,
// the `##` sections and the typed blocks that changed (requirements and the other non-check
// blocks for spec and decisions, checks for criteria), or the reason the change list could not
// be read, and the `dispatch doc-read` command that prints the new version. One closing sentence
// names the rule the role's prompt carries for answering it. A holder whose every item changed
// decision blocks alone is told only that nothing its issue holds changed, with no list and no
// rule, since there is nothing to answer: that is the one news task of a decision-only version,
// sent where the opening is owed to it.
//
// To the root architect it names each pair of versions this news tells of with its change list,
// then every member of the tree with its phase and what it is told: owed news sent now, owed news
// its next start carries, owed news the architect answers itself in accepting, or told nothing
// with the reason. A merged member carries the architect's duty: its pull request can take no new
// work, so the work goes to an issue of the tree whose pull request is still open, or to a person.
//
// It names no notice kind: a phase worker's session holds no notice, and the end-to-end proof
// fails a run where one reads as if it did (scripts/e2e/lib/workflow.sh, worker_notices).
func RenderNews(news News) string {
	var paragraphs []string
	if news.OpeningOwed {
		if sentence := gateAtSend(news.Tree, news.Steps, news.Gates); sentence != "" {
			paragraphs = append(paragraphs, sentence)
		}
	}
	if news.Audience == record.AudienceArchitect {
		paragraphs = append(paragraphs, architectNews(news)...)
	} else {
		paragraphs = append(paragraphs, holderNews(news)...)
	}
	return strings.Join(paragraphs, "\n\n")
}

// gateAtSend is the gate sentence of tree as gates stand now. The standing gate is closed when a
// step the tree is gated on holds a row that is not open (classify.StandingGate, the predicate the
// merger's hold reads too), and the sentence names that row's latest version, the one with its
// reviewer. Otherwise it is open, and the sentence names the approval that opened it: the row
// whose approval Dispatch recorded last (record.EventMark.After), the later step in the tree's
// order where the marks cannot tell. It is "" when no row holds an approval, which says nothing.
func gateAtSend(tree string, steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate) string {
	if held, closed := classify.StandingGate(steps, gates); closed {
		return gateClosedSentence(tree, held.Step.OrSpec(), held.ArtifactID, held.LatestVersion)
	}
	var opened record.DesignGate
	var openedStep record.DesignStep
	for _, step := range classify.RequiredSteps(classify.TreeSteps(steps, gates)) {
		row, ok := gates[step]
		if !ok || row.ApprovedVersion == nil {
			continue
		}
		if openedStep == "" || row.ApprovedMark.After(opened.ApprovedMark) {
			opened, openedStep = row, step
		}
	}
	if openedStep == "" {
		return ""
	}
	return gateOpenedSentence(tree, openedStep, opened.ArtifactID, *opened.ApprovedVersion)
}

// holderNews is the holder's form after its gate sentence: what changed for its issue, then the
// rule its prompt carries. An issue whose every item is decision blocks alone is told that
// nothing it holds changed, and nothing more.
func holderNews(news News) []string {
	if len(news.Items) == 0 {
		return nil
	}
	if decisionsOnly(news.Items) {
		sentences := make([]string, len(news.Items))
		for i, item := range news.Items {
			sentences[i] = nothingChanged(item)
		}
		return []string{strings.Join(sentences, " ")}
	}
	list := []string{fmt.Sprintf("The approved design of %s changed for %s, which you hold in %s:", news.Tree, news.Issue, news.Phase)}
	for _, item := range news.Items {
		if item.Changes.DecisionOnly {
			list = append(list, "- "+nothingChanged(item))
			continue
		}
		list = append(list, fmt.Sprintf("- The %s document (%s): %s.", item.Step, item.Changes.ArtifactID, pairWords(item.Changes, "your phase")))
		list = append(list, indented(changeLines(news.Tree, item))...)
	}
	return []string{strings.Join(list, "\n"), holderRule(news.Issue, news.Phase)}
}

// nothingChanged is the words that tell a holder an item's new version changed nothing its issue
// holds: the two versions differ in decision blocks alone.
func nothingChanged(item NewsChange) string {
	return fmt.Sprintf("%s Version %d of its %s document (%s) differs from it only in decision blocks.",
		nothingChangedSince(item.Changes.From), item.Changes.To, item.Step, item.Changes.ArtifactID)
}

// holderRule is the closing paragraph of a holder's news: the rule its role's prompt carries for
// answering it. The architect holding an issue in accepting answers by its sign-off or a move
// back (acceptingAnswer). The planner revises its plan when it is stale, since planning has no
// earlier phase to move back to (prompts/go/planner.md), and comments when it is current. Every
// other phase worker moves back when its work is stale, and comments when it is current.
func holderRule(issue string, p phase.Phase) string {
	switch p {
	case phase.Accepting:
		return "As your prompt says, read each named section and check at the new version, then " + acceptingAnswer(issue) +
			", with a reason that names the version and the change."
	case phase.Planning:
		return fmt.Sprintf("As your role's prompt says, read each named section and check at the new version. Planning has no earlier phase, "+
			"so a stale plan does not move back. If your plan is stale, revise `plan.md` in place to match the new version of the design, "+
			"then run the plan review again. If your plan is current, write a `dispatch comment` on %s that names the version and why.", issue)
	}
	return fmt.Sprintf("As your role's prompt says, read each named section and check at the new version: if your work is stale, "+
		"call `request_backward_move` with a reason that names the version and the change; if it is current, write a `dispatch comment` "+
		"on %s that names the version and why; an issue whose pull request has merged never moves back.", issue)
}

// acceptingAnswer is the answer an architect gives the news of an issue it holds in accepting,
// named as subject: its sign-off, or a move back. It is written once for its two readers: the
// closing rule of the holder form (holderRule) and the root architect's line for a member it
// answers itself (memberWords).
func acceptingAnswer(subject string) string {
	return fmt.Sprintf("sign %s off with `sign_off`, or call `request_backward_move` to move it back to planning, implementing or testing", subject)
}

// architectNews is the root architect's summary after its gate sentence: each pair of versions
// the news tells of with its change list, once each, then every member of the tree with its phase
// and what it is told.
func architectNews(news News) []string {
	var paragraphs []string
	own := make(map[record.VersionPair]bool, len(news.Items))
	for _, item := range news.Items {
		own[item.Changes.VersionPair] = true
	}
	if pairs := distinctPairs(news); len(pairs) > 0 {
		list := []string{fmt.Sprintf("A person approved a new version of the design of %s. What changed, for each pair of versions this news names:", news.Tree)}
		for _, item := range pairs {
			taker := ""
			if own[item.Changes.VersionPair] {
				taker = "you"
			}
			line := fmt.Sprintf("- The %s document (%s): %s.", item.Step, item.Changes.ArtifactID, pairWords(item.Changes, taker))
			if item.Changes.DecisionOnly {
				list = append(list, line+" Only its decision blocks changed, so nothing any issue holds changed.")
				continue
			}
			list = append(list, line)
			list = append(list, indented(changeLines(news.Tree, item))...)
		}
		paragraphs = append(paragraphs, strings.Join(list, "\n"))
	}
	if len(news.Members) > 0 {
		list := []string{fmt.Sprintf("Each issue of the tree of %s, with its phase and what it is told:", news.Tree)}
		for _, member := range news.Members {
			list = append(list, fmt.Sprintf("- %s, in %s: %s", member.Issue, member.Phase, memberWords(member)))
		}
		paragraphs = append(paragraphs, strings.Join(list, "\n"))
	}
	return paragraphs
}

// distinctPairs is each step's pair of versions the architect's summary tells of, once each: the
// architect's own items first, then each member's in the order the members are named.
func distinctPairs(news News) []NewsChange {
	var pairs []NewsChange
	seen := map[record.VersionPair]bool{}
	add := func(items []NewsChange) {
		for _, item := range items {
			if !seen[item.Changes.VersionPair] {
				seen[item.Changes.VersionPair] = true
				pairs = append(pairs, item)
			}
		}
	}
	add(news.Items)
	for _, member := range news.Members {
		add(member.Items)
	}
	return pairs
}

// memberWords is what the architect's summary says of one member: what it is owed, and who tells
// it, or that it is told nothing and why.
func memberWords(member NewsMember) string {
	owes := "owed news of the approved design"
	if len(member.Items) > 0 {
		owed := make([]string, len(member.Items))
		for i, item := range member.Items {
			owed[i] = fmt.Sprintf("version %d of the %s document, %s", item.Changes.To, item.Step, sinceWords(item.Changes, "its phase"))
		}
		owes = "owed " + strings.Join(owed, ", and ")
	}
	holder := "the holder of its phase"
	if role := phase.RoleFor(member.Phase); role != "" {
		holder = "its " + string(role)
	}
	switch member.State {
	case NewsSentNow:
		return fmt.Sprintf("%s; a task tells %s now.", owes, holder)
	case NewsAtNextStart:
		return fmt.Sprintf("%s; the next start of %s carries it.", owes, holder)
	case NewsYouAnswer:
		return fmt.Sprintf("%s. You hold it in accepting, so you answer it yourself: read what changed, then %s.", owes, acceptingAnswer("it"))
	case NewsDecisionOnly:
		return "told nothing: the approved version differs from the one it took only in decision blocks."
	case NewsNotConcerned:
		return "told nothing: only the root's checks changed, and they are the root's alone to prove."
	case NewsAlreadyTaken:
		return "told nothing: its phase already took the approved version."
	case NewsAdmitted:
		return "told nothing: it has not started, and its first phase starts from the approved version."
	case NewsAwaitingPerson:
		return "told nothing while it waits on a person: no phase holder works it."
	case NewsStopped:
		return "told nothing while it is stopped: the restart that moves it back into its phase carries what it is owed then."
	case NewsDone:
		return "told nothing: its work is done."
	case NewsOutOfWorkflow:
		return "told nothing: its status takes it out of the workflow."
	case NewsMerged:
		return "told nothing: its pull request has merged, and merged work never moves back. Give the new work to an issue of this tree " +
			"whose pull request is still open, or `dispatch ask` the person."
	case NewsGateClosed:
		return "told nothing yet: the gate closed again before this was sent; what it is owed goes once a person approves the version " +
			"now with its reviewer."
	}
	return "told nothing."
}

// pairWords is a change set's pair of versions as news says it: the new version, then what taker
// took before it (sinceWords).
func pairWords(changes record.DesignChanges, taker string) string {
	return fmt.Sprintf("version %d, %s", changes.To, sinceWords(changes, taker))
}

// sinceWords is the version taker last took, the change set's From, as news says it, or that no
// version was approved before the new one (From 0). A pair from 0 recorded unavailable is not a
// first approval: it is the pair the reconcile records when Dispatch's review history could not
// say which version an issue whose record knew none took (the daemon's designChanges), so it is
// said as a version the daemon could not read, and its change lines give the reason. An empty
// taker names nobody's taking.
func sinceWords(changes record.DesignChanges, taker string) string {
	from := changes.From
	switch {
	case from == 0 && changes.State == record.DesignChangesUnavailable:
		return "since a version the daemon could not read"
	case from == 0 && taker == "":
		return "the first version a person approved"
	case from == 0:
		return fmt.Sprintf("the first version a person approved; %s took none before it", taker)
	case taker == "":
		return fmt.Sprintf("since version %d", from)
	}
	return fmt.Sprintf("since version %d, which %s last took", from, taker)
}

// changeLines is what one item's change set says, line by line: the sections and the blocks that
// changed, or why the list could not be read, then the command that prints the new version.
func changeLines(tree string, item NewsChange) []string {
	changes := item.Changes
	var lines []string
	if changes.State == record.DesignChangesUnavailable {
		reason := strings.TrimRight(changes.Reason, ". ")
		if reason == "" {
			lines = append(lines, "The change list could not be read.")
		} else {
			lines = append(lines, fmt.Sprintf("The change list could not be read: %s.", reason))
		}
	} else {
		if len(changes.Sections) > 0 {
			lines = append(lines, sectionsLine(changes.Sections))
		}
		lines = append(lines, blockLines(item.Step, changes.Blocks)...)
		switch {
		case len(lines) > 0:
		case changes.From == 0:
			lines = append(lines, "No approved version came before it, so read it whole.")
		default:
			lines = append(lines, "Dispatch named no changed section or block of it.")
		}
	}
	return append(lines, fmt.Sprintf("Read it: `dispatch doc-read --issue %s --artifact %s --version %d`", tree, changes.ArtifactID, changes.To))
}

// sectionsLine names each `##` section that changed, by its heading, and how.
func sectionsLine(sections []record.SectionChange) string {
	named := make([]string, len(sections))
	for i, section := range sections {
		named[i] = sectionName(section) + " " + section.Change
	}
	return "Sections: " + strings.Join(named, "; ") + "."
}

// sectionName is how a section is named: its heading quoted, with its place among the sections of
// that heading when an earlier one carries it too, or, for the heading "", the text outside every
// `##` section.
func sectionName(section record.SectionChange) string {
	switch {
	case section.Heading == "":
		return "the text before the first section"
	case section.Occurrence > 0:
		return fmt.Sprintf("the %s section headed %q", ordinal(section.Occurrence+1), section.Heading)
	}
	return fmt.Sprintf("%q", section.Heading)
}

// ordinal is n as an ordinal number: 1st, 2nd, 3rd, 4th, 11th, 21st.
func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// blockLines names the typed blocks of step's document that changed, one line for each type in
// the order the types first appear: the checks of the Verification document (criteria), and
// every other block of the spec and the decisions document.
func blockLines(step record.DesignStep, blocks []record.BlockChange) []string {
	var types []string
	byType := map[string][]string{}
	for _, block := range blocks {
		if (block.Type == "check") != (step == record.DesignStepCriteria) {
			continue
		}
		if _, seen := byType[block.Type]; !seen {
			types = append(types, block.Type)
		}
		byType[block.Type] = append(byType[block.Type], block.ID+" "+block.Change)
	}
	lines := make([]string, len(types))
	for i, blockType := range types {
		lines[i] = blockKind(blockType) + ": " + strings.Join(byType[blockType], "; ") + "."
	}
	return lines
}

// blockKind is the words a line of changed blocks of one type opens with.
func blockKind(blockType string) string {
	switch blockType {
	case "requirement":
		return "Requirements"
	case "check":
		return "Checks"
	case "plan_task":
		return "Plan tasks"
	case "callout":
		return "Callouts"
	}
	return fmt.Sprintf("Blocks of type %s", blockType)
}

// decisionsOnly says whether every item changed decision blocks alone.
func decisionsOnly(items []NewsChange) bool {
	for _, item := range items {
		if !item.Changes.DecisionOnly {
			return false
		}
	}
	return true
}

// indented is lines under a list item.
func indented(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = "  " + line
	}
	return out
}
