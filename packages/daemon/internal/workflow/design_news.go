package workflow

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
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
// Items is what the audience is told, each with its recorded change set, or with the review
// history that could not say which version the audience took: the holder's items for Issue, or the
// architect row's own items for the tree. The send has already dropped every item that no longer
// stands (the daemon's itemStands, which asks StepApprovalStands) and every item the record cannot
// tell yet, so each one here is said as given.
type News struct {
	Audience    record.DesignAudience
	Tree        string
	Issue       string
	Phase       phase.Phase
	Steps       []record.DesignStep
	Gates       map[record.DesignStep]record.DesignGate
	OpeningOwed bool
	Items       []NewsChange
	// Back is the phases a move back of Issue reaches where Issue stands in accepting
	// (MoveBackTargets), which its answer names (acceptingAnswer); nil elsewhere, where nothing
	// reads it.
	Back []phase.Phase
	// Members is every member of the tree, the root among them, in the order the architect's
	// summary names them. The holder's form reads none.
	Members []NewsMember
}

// NewsChange is one step's approved version as news names it: the step, and either the recorded
// change set of the pair (Changes, record.DesignChanges), whose VersionPair is the version the
// audience took (From, 0 for none approved before To) and the version approved now (To), or, where
// the audience's told row knows no version of the step and Dispatch's review history could not say
// which it took, that failed read (HistoryUnread, record.DesignHistoryUnread), with Changes unused:
// no earlier version is known, so no pair is named, and the item is said as a version the daemon
// could not read, with the reason, never as a first approval.
type NewsChange struct {
	Step          record.DesignStep
	Changes       record.DesignChanges
	HistoryUnread *record.DesignHistoryUnread
}

// newsKey is what one item of news names, once: the pair of versions of its change set, or, for
// an item whose review history could not be read, the question that history did not answer.
type newsKey struct {
	pair   record.VersionPair
	unread record.DesignHistoryQuestion
}

// key is what the item names (newsKey).
func (c NewsChange) key() newsKey {
	if c.HistoryUnread != nil {
		return newsKey{unread: c.HistoryUnread.DesignHistoryQuestion}
	}
	return newsKey{pair: c.Changes.VersionPair}
}

// artifact is the document the item names.
func (c NewsChange) artifact() string {
	if c.HistoryUnread != nil {
		return c.HistoryUnread.ArtifactID
	}
	return c.Changes.ArtifactID
}

// to is the approved version the item tells.
func (c NewsChange) to() int {
	if c.HistoryUnread != nil {
		return c.HistoryUnread.To
	}
	return c.Changes.To
}

// NewsMember is one member of a tree as the root architect's summary names it: its key, its phase,
// what its news is (State), and, for a member owed news, the items its holder is owed. Architect
// is the key of the issue whose architect holds the member in accepting, where that is an
// architect other than the tree root's: the member's own, or one between it and the root (the
// owner walk, record.OwningIssue). The summary names that architect as the one told. Every other
// member has none. Back is the phases a move back of the member reaches where the root architect
// answers it in accepting (NewsYouAnswer), as News.Back is the holder's.
type NewsMember struct {
	Issue     string
	Phase     phase.Phase
	State     NewsState
	Items     []NewsChange
	Architect string
	Back      []phase.Phase
}

// NewsState is what one member of a tree is told of an approved version, as its root architect's
// summary says it: owed news and sent now, owed news that waits until its holder takes the task it
// holds, owed news and waiting for the next start of its phase, owed news its root architect
// answers itself, or told nothing, for one reason. The daemon picks it from the same facts
// record.OwedNews reads, the delivery it queued, and the change set.
type NewsState string

const (
	// NewsSentNow is a member owed news whose holder gets a task carrying it now: a news-only
	// start to the claim given its phase's current entry, or, for a member in accepting, to the
	// child's architect that owns it (NewsMember.Architect).
	NewsSentNow NewsState = "sent-now"
	// NewsWaitsForTask is a member owed news whose holder, or the child's architect that owns it in
	// accepting, holds a task its worker has not taken yet. That claim takes no second task while
	// one is pending, so no news-only start goes to it now: the news waits until a turn takes that
	// task. The task can carry the news itself; otherwise the sweep's next reconcile queues the
	// start that tells it once the task is taken.
	NewsWaitsForTask NewsState = "waits-for-task"
	// NewsAtNextStart is a member owed news that the next start of its phase's holder carries: a
	// claim owed its phase start, or a phase whose holder has not started yet.
	NewsAtNextStart NewsState = "next-start"
	// NewsYouAnswer is a member in accepting that the root architect owns: the architect holds
	// its phase, so it answers the news itself, by sign_off or by a move back. A member in
	// accepting that another architect owns is told as that architect's claim is told, and the
	// summary names that architect (NewsMember.Architect).
	NewsYouAnswer NewsState = "you-answer"
	// NewsWorkUnchanged is a member told nothing because no version it is owed changes anything of
	// its work: each approved version differs from the version it took in decision blocks alone,
	// or in nothing at all (record.DesignChanges.WorkUnchanged). One state says both, because the
	// reconcile raises such a member by one rule (the daemon's settlementOf) whatever mix of the
	// two its steps hold, so its words must be true of each mix.
	NewsWorkUnchanged NewsState = "work-unchanged"
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
	// NewsStopped is a member told nothing while it stands in held (record.UntoldHeld), whatever
	// holds it (its stopped task, the issues beneath it or its failed worker): the release that
	// moves it back into a phase carries what it is owed then.
	NewsStopped NewsState = "stopped"
	// NewsAwaitingChildren is a parent told nothing while it stands in awaiting_children, a
	// Dispatch child of it open (record.UntoldAwaitingChildren): no restart moves it back, and the
	// admission pass's return once no child of it is open (ReturnFromChildren) carries what it is
	// owed then.
	NewsAwaitingChildren NewsState = "awaiting-children"
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
// gate as it stands at send (gateAtSend): the opened message while the standing gate is open,
// naming the step approved last, and, where a gated step stands open on an ancestor's approval,
// each such step with the approval it stands on (gateOpenedMessage); and the closed sentence,
// naming the version that closed it, once a later version has (gateClosedSentence). tellGateMoved
// writes both through the same two functions, so a merger's release reads the words its prompt
// quotes, and news sent after the gate closed again says so.
//
// To a holder it then names, for each step, the version pair as the version its phase last took,
// the sections and the typed blocks that changed (requirements and the other non-check blocks for
// spec and decisions, checks for criteria), or the reason the change list could not be read, and
// the `dispatch doc-read` command that prints the new version. A section is a `##` section, named
// by its heading, or a run of the text outside every section, named by where it stands
// (sectionName). A step whose earlier version Dispatch's review history could not give is named
// as the new version since a version the daemon could not read, with the reason that history
// could not be read, and the same command. One closing sentence names the rule the role's prompt
// carries for answering it. A holder whose every item changed nothing of the work
// (record.DesignChanges.WorkUnchanged: decision blocks alone, or nothing at all) is told only that
// nothing its issue holds changed, and why, with no list and no rule, since there is nothing to
// answer: that is the one news task of such a version, sent where the opening is owed to it.
//
// To the root architect it names each pair of versions this news tells of with its change list,
// or why a pair changed nothing any issue holds, then every member of the tree with its phase and
// what it is told: owed news sent now, owed news that waits until its holder takes the task it
// holds, owed news its next start carries, owed news the architect answers itself in accepting, or
// told nothing with the reason. A merged member carries the architect's duty: its pull request can
// take no new work, so the work goes to an issue of the tree whose pull request is still open, or
// to a person.
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
// reviewer. Otherwise it is open, and the sentence is the opened message of the approval that
// opened it: the row whose approval Dispatch recorded last (record.EventMark.After), the later
// step in the tree's order where the marks cannot tell, with each step that stands open on an
// ancestor's approval (basisStepsOf), as tellGateMoved says it (gateOpenedMessage). It is "" when
// no row holds an approval, which says nothing.
func gateAtSend(tree string, steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate) string {
	if held, closed := classify.StandingGate(steps, gates); closed {
		return gateClosedSentence(tree, held.Step.OrSpec(), held.ArtifactID, held.LatestVersion)
	}
	var opened record.DesignGate
	var openedStep record.DesignStep
	for _, step := range (record.TreeGate{Steps: steps, Rows: gates}).Required() {
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
	return gateOpenedMessage(tree, openedStep, opened.ArtifactID, *opened.ApprovedVersion, basisStepsOf(steps, gates))
}

// holderNews is the holder's form after its gate sentence: what changed for its issue, then the
// rule its prompt carries. An issue whose every item changed nothing of the work is told that
// nothing it holds changed, and why, and nothing more.
func holderNews(news News) []string {
	if len(news.Items) == 0 {
		return nil
	}
	if workUnchanged(news.Items) {
		sentences := make([]string, len(news.Items))
		for i, item := range news.Items {
			sentences[i] = nothingChanged(item)
		}
		return []string{strings.Join(sentences, " ")}
	}
	list := []string{fmt.Sprintf("The approved design of %s changed for %s, which you hold in %s:", news.Tree, news.Issue, news.Phase)}
	for _, item := range news.Items {
		if item.Changes.WorkUnchanged() {
			list = append(list, "- "+nothingChanged(item))
			continue
		}
		basis := basisWords(news.Gates, item)
		list = append(list, fmt.Sprintf("- The %s document (%s): %s.%s", item.Step, item.artifact(), pairWords(item, "your phase", basis != ""), basis))
		list = append(list, indented(changeLines(news.Tree, item, "your phase"))...)
	}
	return []string{strings.Join(list, "\n"), holderRule(news.Issue, news.Phase, news.Back)}
}

// nothingChanged is the words that tell a holder an item's new version changed nothing its issue
// holds (record.DesignChanges.WorkUnchanged), and why: the new version holds the same text as the
// version its phase took, or differs from it in decision blocks alone. It names no change list:
// such an item has none.
func nothingChanged(item NewsChange) string {
	changes := item.Changes
	why := "differs from it only in decision blocks"
	if changes.Unchanged {
		why = fmt.Sprintf("holds the same text as version %d", changes.From)
	}
	return fmt.Sprintf("%s Version %d of its %s document (%s) %s.", nothingChangedSince(changes.From), changes.To, item.Step, changes.ArtifactID, why)
}

// holderRule is the closing paragraph of a holder's news: the rule its role's prompt carries for
// answering it. The architect holding an issue in accepting answers by its sign-off or a move
// back to back, the phases of the steps of its task before the acceptance (acceptingAnswer). The
// planner revises its plan in place when it is stale, since the plan is its own work wherever its
// task places its step (prompts/go/planner.md), and comments when it is current. Every other phase
// worker moves back when its work is stale, and comments when it is current.
func holderRule(issue string, p phase.Phase, back []phase.Phase) string {
	switch p {
	case phase.Accepting:
		return "As your prompt says, read each named section and check at the new version, then " + acceptingAnswer(issue, back) +
			", with a reason that names the version and the change."
	case phase.Planning:
		return fmt.Sprintf("As your role's prompt says, read each named section and check at the new version. A stale plan is your own work "+
			"wherever your task places your step, so it does not move back. If your plan is stale, revise `plan.md` in place to match the new version of the design, "+
			"then run the plan review again. If your plan is current, write a `dispatch comment` on %s that names the version and why.", issue)
	}
	return fmt.Sprintf("As your role's prompt says, read each named section and check at the new version: if your work is stale, "+
		"call `request_backward_move` with a reason that names the version and the change; if it is current, write a `dispatch comment` "+
		"on %s that names the version and why; an issue whose pull request has merged never moves back.", issue)
}

// acceptingAnswer is the answer an architect gives the news of an issue it holds in accepting,
// named as subject: its sign-off, or a move back to back, the phases of the steps of its task
// before the acceptance (MoveBackTargets), which the daemon's send reads from the task and refuses
// to send without. It is written once for its two readers: the closing rule of the holder form
// (holderRule) and the root architect's line for a member it answers itself (memberWords).
func acceptingAnswer(subject string, back []phase.Phase) string {
	named := make([]string, len(back))
	for i, p := range back {
		named[i] = string(p)
	}
	return fmt.Sprintf("sign %s off with `sign_off`, or call `request_backward_move` to move it back to %s", subject, listed(named, "or"))
}

// MoveBackTargets is the phases a move back of an issue standing in accepting reaches, on line,
// the phases its task walks (Walk.Line, LineOf): each phase before accepting, the phases of the
// steps of the task before the acceptance, as the walk's backward rows reach them (backwardRows).
// So the answer of the news (acceptingAnswer) names no phase a move would be refused for, where
// the task has no step for it, and leaves out none a move reaches. A line that holds no accepting,
// or no step before it, is an error: an issue stands in accepting only on a task with an
// acceptance after its implementer step (plan.Supported), so such a line is a record the news
// cannot answer.
func MoveBackTargets(line []phase.Phase) ([]phase.Phase, error) {
	at := slices.Index(line, phase.Accepting)
	if at < 1 {
		return nil, fmt.Errorf("the line %v holds no step before an acceptance", line)
	}
	return slices.Clone(line[:at]), nil
}

// architectNews is the root architect's summary after its gate sentence: each pair of versions
// the news tells of with its change list, or with why it changed nothing any issue holds, once
// each, then every member of the tree with its phase and what it is told. An item whose review
// history could not be read is an entry of its own, named by the question that history did not
// answer (newsKey), never folded into a pair.
func architectNews(news News) []string {
	var paragraphs []string
	own := make(map[newsKey]bool, len(news.Items))
	for _, item := range news.Items {
		own[item.key()] = true
	}
	if pairs := distinctPairs(news); len(pairs) > 0 {
		opener := fmt.Sprintf("A person approved a new version of the design of %s.", news.Tree)
		if !slices.ContainsFunc(pairs, func(item NewsChange) bool { return basisWords(news.Gates, item) == "" }) {
			opener = fmt.Sprintf("A new version of the design of %s stands approved on an ancestor's approval.", news.Tree)
		}
		list := []string{opener + " What changed, for each pair of versions this news names:"}
		for _, item := range pairs {
			taker := ""
			if own[item.key()] {
				taker = "you"
			}
			basis := basisWords(news.Gates, item)
			line := fmt.Sprintf("- The %s document (%s): %s.%s", item.Step, item.artifact(), pairWords(item, taker, basis != ""), basis)
			if item.Changes.WorkUnchanged() {
				why := "Only its decision blocks changed"
				if item.Changes.Unchanged {
					why = fmt.Sprintf("It holds the same text as version %d", item.Changes.From)
				}
				list = append(list, line+" "+why+", so nothing any issue holds changed.")
				continue
			}
			list = append(list, line)
			list = append(list, indented(changeLines(news.Tree, item, taker))...)
		}
		paragraphs = append(paragraphs, strings.Join(list, "\n"))
	}
	if len(news.Members) > 0 {
		list := []string{fmt.Sprintf("Each issue of the tree of %s, with its phase and what it is told:", news.Tree)}
		for _, member := range news.Members {
			list = append(list, fmt.Sprintf("- %s, in %s: %s", member.Issue, member.Phase, memberWords(member, news.Gates)))
		}
		paragraphs = append(paragraphs, strings.Join(list, "\n"))
	}
	return paragraphs
}

// basisWords is what news adds of an item whose new version stands approved on an ancestor's
// approval rather than a person's: the step's row in gates holds that version open on a basis
// (record.DesignGate.OnBasis). It says so in the words of the opened message (basisSentence),
// after a space, so no news task tells a holder that a person approved that version. It is "" for
// every other item.
func basisWords(gates map[record.DesignStep]record.DesignGate, item NewsChange) string {
	gate, ok := gates[item.Step]
	if !ok || !gate.OnBasis() || gate.ApprovedVersion == nil || *gate.ApprovedVersion != item.to() {
		return ""
	}
	return " " + basisSentence(gate)
}

// distinctPairs is each step's pair of versions the architect's summary tells of, and each
// question of a review history it could not read, once each (newsKey): the architect's own items
// first, then each member's in the order the members are named.
func distinctPairs(news News) []NewsChange {
	var pairs []NewsChange
	seen := map[newsKey]bool{}
	add := func(items []NewsChange) {
		for _, item := range items {
			if key := item.key(); !seen[key] {
				seen[key] = true
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
// it, or that it is told nothing and why. The holder of a member in accepting that another
// architect than the tree root's owns is that architect (NewsMember.Architect): the member's own,
// or the architect of the issue above it that the summary names. gates is the tree root's gate
// rows, which say whether an item's new version stands on an ancestor's approval (basisWords).
func memberWords(member NewsMember, gates map[record.DesignStep]record.DesignGate) string {
	owes := "owed news of the approved design"
	if len(member.Items) > 0 {
		owed := make([]string, len(member.Items))
		for i, item := range member.Items {
			owed[i] = fmt.Sprintf("version %d of the %s document, %s", item.to(), item.Step, sinceWords(item, "its phase", basisWords(gates, item) != ""))
		}
		owes = "owed " + strings.Join(owed, ", and ")
	}
	holder := "the holder of its phase"
	switch role := phase.RoleFor(member.Phase); {
	case member.Architect == member.Issue:
		holder = "its own architect"
	case member.Architect != "":
		holder = "the architect of " + member.Architect
	case role != "":
		holder = "its " + string(role)
	}
	switch member.State {
	case NewsSentNow:
		return fmt.Sprintf("%s; a task tells %s now.", owes, holder)
	case NewsWaitsForTask:
		return fmt.Sprintf("%s; %s holds a task it has not taken yet, and the news waits until it takes that task.", owes, holder)
	case NewsAtNextStart:
		return fmt.Sprintf("%s; the next start of %s carries it.", owes, holder)
	case NewsYouAnswer:
		return fmt.Sprintf("%s. You hold it in accepting, so you answer it yourself: read what changed, then %s.", owes, acceptingAnswer("it", member.Back))
	case NewsWorkUnchanged:
		return "told nothing: the approved version holds the same text as the one it took, apart from decision blocks."
	case NewsNotConcerned:
		return "told nothing: only the root's checks changed, and they are the root's alone to prove."
	case NewsAlreadyTaken:
		return "told nothing: its phase already took the approved version."
	case NewsAdmitted:
		return "told nothing: it has not started, and its first phase starts from the approved version."
	case NewsAwaitingPerson:
		return "told nothing while it waits on a person: no phase holder works it."
	case NewsStopped:
		return "told nothing while it is held: the release that moves it back into a phase carries what it is owed then."
	case NewsAwaitingChildren:
		return "told nothing while it waits on its open children: the return that moves it back into its phase once no child " +
			"of it is open carries what it is owed then."
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

// pairWords is the version an item tells as news says it: the new version, then what taker took
// before it (sinceWords, with onBasis).
func pairWords(item NewsChange, taker string, onBasis bool) string {
	return fmt.Sprintf("version %d, %s", item.to(), sinceWords(item, taker, onBasis))
}

// sinceWords is the version taker last took as news says it: the change set's From, or that no
// version was approved before the new one (From 0). From 0 is only ever a version the told row
// held or the review history read and found none before: a history that could not be read names
// no pair (NewsChange.HistoryUnread), and is said as a version the daemon could not read, never as
// a first approval. An empty taker names nobody's taking. onBasis says the new version stands
// approved on an ancestor's approval (basisWords), which no person gave, so its first approval is
// said as the first approved version, never as the first version a person approved.
func sinceWords(item NewsChange, taker string, onBasis bool) string {
	if item.HistoryUnread != nil {
		return "since a version the daemon could not read"
	}
	first := "the first version a person approved"
	if onBasis {
		first = "the first approved version"
	}
	from := item.Changes.From
	switch {
	case from == 0 && taker == "":
		return first
	case from == 0:
		return fmt.Sprintf("%s; %s took none before it", first, taker)
	case taker == "":
		return fmt.Sprintf("since version %d", from)
	}
	return fmt.Sprintf("since version %d, which %s last took", from, taker)
}

// changeLines is what one item says, line by line: the sections and the blocks of its change set
// that changed, or why the list could not be read, or, for an item whose review history could not
// be read, why the version taker last took could not be read; then the command that prints the new
// version. An empty taker, in the architect's summary, names each issue the item is owed to. An
// item that changed nothing of the work (record.DesignChanges.WorkUnchanged) never reaches it: its
// callers say why nothing changed in place of a list (nothingChanged, architectNews). So a read set
// that names nothing here is a first approval (from version 0, which the reconcile records read with
// nothing named and never asks Dispatch about, handled by the From 0 case below), or one from a
// Dispatch that predates its unchanged answer, which says neither, and the line says only what that
// Dispatch said.
func changeLines(tree string, item NewsChange, taker string) []string {
	changes := item.Changes
	var lines []string
	switch {
	case item.HistoryUnread != nil:
		if taker == "" {
			taker = "each issue owed it"
		}
		lines = append(lines, couldNotRead(fmt.Sprintf("The version %s last took", taker), item.HistoryUnread.Reason))
	case changes.State == record.DesignChangesUnavailable:
		lines = append(lines, couldNotRead("The change list", changes.Reason))
	default:
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
	return append(lines, fmt.Sprintf("Read it: `dispatch doc-read --issue %s --artifact %s --version %d`", tree, item.artifact(), item.to()))
}

// couldNotRead is the sentence that says subject could not be read, and why where reason says. The
// reason is Dispatch's own error text, or the reconcile's record of it, so it is quoted as a Go
// string (%q), as a heading is (headingName): the quotes mark where text from outside the daemon
// begins and ends, and a newline or a quote inside it is escaped rather than read as words of the
// daemon's own. Its own closing stop and spaces are trimmed first, so the quoted reason ends where
// its words do.
func couldNotRead(subject, reason string) string {
	reason = strings.TrimRight(reason, ". ")
	if reason == "" {
		return subject + " could not be read."
	}
	return fmt.Sprintf("%s could not be read: %q.", subject, reason)
}

// sectionsLine names each top-level unit that changed (sectionName), and how, in Dispatch's own
// word for it: changed, added, removed, or moved for a unit that kept its text and changed its
// place (record.SectionChange).
func sectionsLine(sections []record.SectionChange) string {
	named := make([]string, len(sections))
	for i, section := range sections {
		named[i] = sectionName(section) + " " + section.Change
	}
	return "Sections: " + strings.Join(named, "; ") + "."
}

// sectionName is how a top-level unit is named. A `##` section is named by its heading
// (headingName). A run of the text outside every section, the heading "" with occurrence 0, is
// named by the place Dispatch names it at: before the first section, or after the `##` section
// directly before it. A run with no place, as a Dispatch from before the place names one, is the
// text outside every section, which places it nowhere: such a run can follow any `#` heading, and
// calling it the text before the first section would send the reader to the top of the document
// for a change that stands lower down.
func sectionName(section record.SectionChange) string {
	if section.Heading != "" || section.Occurrence != 0 {
		return headingName(section.Heading, section.Occurrence)
	}
	switch section.Place {
	case dispatch.PlaceBeforeFirstSection:
		return "the text before the first section"
	case dispatch.PlaceAfterSection:
		return "the text after " + headingName(section.AfterHeading, section.AfterOccurrence)
	}
	return "the text outside every section"
}

// headingName is how the `##` section of heading and occurrence is named: its heading quoted, with
// its place among the sections of that heading when an earlier one carries it too. A heading with
// no words, which Dispatch counts from occurrence 1, is named by its place among such headings.
func headingName(heading string, occurrence int) string {
	switch {
	case heading == "":
		return fmt.Sprintf("the %s section with an empty heading", ordinal(occurrence))
	case occurrence > 0:
		return fmt.Sprintf("the %s section headed %q", ordinal(occurrence+1), heading)
	}
	return fmt.Sprintf("%q", heading)
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
// every other block of the spec and the decisions document. A block's id is the document's own
// text, so it is quoted (%q), as a heading is (headingName).
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
		byType[block.Type] = append(byType[block.Type], fmt.Sprintf("%q %s", block.ID, block.Change))
	}
	lines := make([]string, len(types))
	for i, blockType := range types {
		lines[i] = blockKind(blockType) + ": " + strings.Join(byType[blockType], "; ") + "."
	}
	return lines
}

// blockKind is the words a line of changed blocks of one type opens with. A type the daemon does
// not name is the document's own text, so it is quoted (%q).
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
	return fmt.Sprintf("Blocks of type %q", blockType)
}

// workUnchanged says whether no item changed anything of the work
// (record.DesignChanges.WorkUnchanged): each changed decision blocks alone, or nothing at all.
func workUnchanged(items []NewsChange) bool {
	for _, item := range items {
		if !item.Changes.WorkUnchanged() {
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
