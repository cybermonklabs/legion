package workflow

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
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
// transaction reads them (record.Store.GatesOf): the gate's record is said of them, so a news task
// composed while the gate stood open and sent after a later version closed it says closed. Each
// row carries its own Step, as the store writes it.
//
// Documents is the document each row of Gates is bound to, by step, by the rule the
// architect-start task and the catch-up name it by (GateDocuments): a dispatch:// link on the
// issue the row records, or that the store's rows on its artifact name, else on the issue Dispatch
// names where the daemon sends the task (NameNewsDocuments, outside the transaction), else the
// bare artifact reference. So a carried root's news names the old root's document, or the bare
// reference where nothing says whose it is, and never the carried root's own. It holds a document
// for every row of Gates.
//
// OpeningOwed is the audience's told row's own flag (record.DesignTold.OpeningOwed): the gate
// opened while the audience was owed news, so the news leads with the gate's record in place of
// the opening's role message.
//
// Items is what the audience is told, each with its recorded change set, or with the review
// history that could not say which version the audience took: the holder's items for Issue, or the
// architect row's own items for the tree. The send has already dropped every item that no longer
// stands (classify.DesignApprovalStands) and every item the record cannot tell yet, so each one
// here is said as given.
type News struct {
	Audience    record.DesignAudience
	Tree        string
	Issue       string
	Phase       phase.Phase
	Steps       []record.DesignStep
	Gates       map[record.DesignStep]record.DesignGate
	Documents   map[record.DesignStep]string
	OpeningOwed bool
	Items       []NewsChange
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
// what its news is (State), and, for a member owed news, the items its holder is owed whose change
// sets are recorded (Items), whether the task that tells it gives it the gate's opening (Opening:
// its holder row owes the opening, which record.OwedNews owes only while the standing gate is
// open), and how many items it is owed that the record cannot tell yet (Unnamed), which no item
// names. A member owed news is owed at least one of the three. Architect is the key of the issue
// whose architect holds the member in accepting, where that is an architect other than the tree
// root's: the member's own, or one between it and the root (the owner walk, record.OwningIssue).
// The summary names that architect as the one told. Every other member has none.
type NewsMember struct {
	Issue     string
	Phase     phase.Phase
	State     NewsState
	Items     []NewsChange
	Opening   bool
	Unnamed   int
	Architect string
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
	// NewsStopped is a member told nothing while it stands in held, whatever holds it (its stopped
	// task, the issues beneath it or its failed worker): the release that moves it back into a
	// phase carries what it is owed then.
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

// RenderNews is the one place the text of news of an approved design is written: the
// design-news record of its Audience (NewsRecord), rendered. It is pure: the daemon's send reads
// the record in one transaction, drops what no longer stands, and hands it the rest (News).
//
// It names no notice kind: a phase worker's session holds no notice, and the end-to-end proof
// fails a run where one reads as if it did (scripts/e2e/lib/workflow.sh, worker_notices). So a
// member's state is said in the record's own words (texts.NewsTold, texts.NewsWhy), never as a
// NewsState, one of which is spelled as a controller notice kind.
func RenderNews(news News) string {
	return taskText(NewsRecord(news))
}

// NewsRecord is the design-news record of news (texts.DesignNewsRecord), in the form its Audience
// takes. The steps the readers' prompts give with it are none of its fields: how to answer each
// form, reading a version whole, and the command that prints a version.
//
// Both forms carry the gate's fields when the opening is owed to the audience, said of the gate as
// it stands at send (gateAtSend): `gate: open` while the standing gate is open, naming the step
// approved last, and `gate: closed`, naming the version that closed it, once a later version has.
// They are the fields of the gate-moved message (gateMovedOn, which tellGateMoved also sends), so
// a merger's release reads the fields its prompt names, and news sent after the gate closed again
// says so.
//
// To a holder it names the issue, the phase the holder holds it in, and each of its items (Items),
// the version its phase took as the item's taken. To the root architect it names each pair of
// versions this news tells of once (distinctPairs), the architect's own marked as yours, then every
// member of the tree with its phase and how it is told, and each item a member is owed by its
// index. Each item says what changed (newsItems.add): the sections and the typed blocks of its
// change set as rows of their own, or why none are named.
func NewsRecord(news News) texts.DesignNewsRecord {
	rec := texts.DesignNewsRecord{TaskHead: texts.TaskHead{Kind: texts.DesignNews, Issue: news.Issue}, Tree: news.Tree,
		Audience: texts.NewsToHolder}
	if news.OpeningOwed {
		if gate, said := gateAtSend(news); said {
			rec.Gate, rec.GateStep, rec.GateDocument, rec.GateArtifact, rec.GateVersion, rec.Merge =
				gate.Gate, gate.Step, gate.Document, gate.Artifact, gate.Version, gate.Merge
		}
	}
	items := newsItems{news: news, rec: &rec}
	if news.Audience != record.AudienceArchitect {
		rec.Phase = string(news.Phase)
		for _, item := range news.Items {
			items.add(item, nil)
		}
		return rec
	}
	rec.Issue, rec.Audience = news.Tree, texts.NewsToRootArchitect
	own := make(map[newsKey]bool, len(news.Items))
	for _, item := range news.Items {
		own[item.key()] = true
	}
	index := map[newsKey]int{}
	for _, item := range distinctPairs(news) {
		yours := own[item.key()]
		index[item.key()] = len(rec.Items)
		items.add(item, &yours)
	}
	for _, member := range news.Members {
		told, why := memberTold(member.State)
		row := texts.DesignNewsMember{Issue: member.Issue, Phase: string(member.Phase), Told: told, Why: why, Opening: member.Opening,
			Unnamed: member.Unnamed, ArchitectOf: member.Architect}
		// The role that holds the member's phase is named where a task tells it, by its holder or by
		// the root architect, as the summary named the holder only beside news it is owed: a member
		// told nothing is told by nobody, and one at admitted, whose phase RoleFor names the
		// architect's, is never owed news (record.OwedNews), so no row names a holder for it.
		if told != texts.NewsToldNothing && told != texts.NewsToldNothingYet {
			row.Holder = string(phase.RoleFor(member.Phase))
		}
		rec.Members = append(rec.Members, row)
		for _, item := range member.Items {
			rec.Owed = append(rec.Owed, texts.DesignNewsOwed{Issue: member.Issue, Item: index[item.key()]})
		}
	}
	return rec
}

// gateAtSend is the gate-moved record of news's tree as its gates stand now. The standing gate is
// closed when a step the tree is gated on holds a row that is not open (classify.StandingGate, the
// predicate the merger's hold reads too), and the record names that row's latest version, the one
// with its reviewer. Otherwise it is open, and the record names the approval that opened it: the
// row whose approval Dispatch recorded last (record.EventMark.After), the later step in the tree's
// order where the marks cannot tell. Each names its row's document as the send names it
// (News.Documents): the old root's for a carried row, or the bare artifact reference where nothing
// says whose it is. It says nothing (false) when no row holds an approval.
func gateAtSend(news News) (texts.GateMovedRecord, bool) {
	tree, steps, gates := news.Tree, news.Steps, news.Gates
	if held, closed := classify.StandingGate(steps, gates); closed {
		step := held.Step.OrSpec()
		return gateMovedOn(tree, news.Documents[step], step, held.ArtifactID, held.LatestVersion, false), true
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
		return texts.GateMovedRecord{}, false
	}
	return gateMovedOn(tree, news.Documents[openedStep], openedStep, opened.ArtifactID, *opened.ApprovedVersion, true), true
}

// distinctPairs is each step's pair of versions the architect's summary tells of, and each
// question of a review history it could not read, once each (newsKey): the architect's own items
// first, then each member's in the order the members are named. An item whose review history could
// not be read is an entry of its own, never folded into a pair.
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

// memberTold is how the root architect's summary says a member of a state is told (a texts.NewsTold
// word), and, for a member told nothing or nothing yet, why (a texts.NewsWhy word).
func memberTold(state NewsState) (told, why string) {
	switch state {
	case NewsSentNow:
		return texts.NewsToldNow, ""
	case NewsWaitsForTask:
		return texts.NewsToldWhenTaskTaken, ""
	case NewsAtNextStart:
		return texts.NewsToldAtNextStart, ""
	case NewsYouAnswer:
		return texts.NewsToldByYou, ""
	case NewsGateClosed:
		return texts.NewsToldNothingYet, texts.NewsWhyGateClosed
	}
	return texts.NewsToldNothing, map[NewsState]string{
		NewsWorkUnchanged:  texts.NewsWhyWorkUnchanged,
		NewsNotConcerned:   texts.NewsWhyRootChecksOnly,
		NewsAlreadyTaken:   texts.NewsWhyAlreadyTaken,
		NewsAdmitted:       texts.NewsWhyNotStarted,
		NewsAwaitingPerson: texts.NewsWhyWaitsOnPerson,
		NewsStopped:        texts.NewsWhyStopped,
		NewsDone:           texts.NewsWhyDone,
		NewsOutOfWorkflow:  texts.NewsWhyOutOfWorkflow,
		NewsMerged:         texts.NewsWhyMerged,
	}[state]
}

// newsItems adds the items of news to its record, each with the rows of what changed in it.
type newsItems struct {
	news News
	rec  *texts.DesignNewsRecord
}

// add adds item to the record, yours being whether the version it names as taken is the root
// architect's own (nil in the holder's form): its step, its document, the version approved now, the
// version taken, and what changed, said once.
//
// An item whose review history could not be read names no version taken, since the history could
// not say which, and no pair (texts.NewsChangesNoPair), with the reason, never as a first approval.
// A pair Dispatch could not answer for names its reason (texts.NewsChangesUnavailable). A pair that
// changed nothing of the work (record.DesignChanges.WorkUnchanged) says which of the two it is: the
// same text, or decision blocks alone. Every other pair names each changed section and block as a
// row naming the item (texts.NewsChangesListed): every block of the spec and the decisions document
// but its checks, and only the checks of the Verification document (criteria), which are the
// root's to prove. A pair that names none is a first approval (from version 0, which the reconcile
// records read with nothing named and never asks Dispatch about), with no earlier version to
// compare with, or one Dispatch read and named nothing of (texts.NewsChangesNoneNamed).
func (n newsItems) add(item NewsChange, yours *bool) {
	row := texts.DesignNewsItem{Step: string(item.Step), Document: n.document(item), Artifact: item.artifact(), Version: item.to(),
		Yours: yours}
	row.Taken, row.Changes, row.Reason = n.changed(len(n.rec.Items), item)
	n.rec.Items = append(n.rec.Items, row)
}

// changed is what the item at index at says changed (add): the version taken, nil where its review
// history could not be read; the texts.NewsChanges word; and Dispatch's reason, nil for none. It
// adds the rows of the sections and the blocks it names.
func (n newsItems) changed(at int, item NewsChange) (*int, string, *texts.Words) {
	if unread := item.HistoryUnread; unread != nil {
		return nil, texts.NewsChangesNoPair, dispatchReason(unread.Reason, unread.ReadAt)
	}
	changes := item.Changes
	taken := new(changes.From)
	switch {
	case changes.State == record.DesignChangesUnavailable:
		return taken, texts.NewsChangesUnavailable, dispatchReason(changes.Reason, changes.ReadAt)
	case changes.Unchanged:
		return taken, texts.NewsChangesSameText, nil
	case changes.DecisionOnly:
		return taken, texts.NewsChangesDecisionBlocksOnly, nil
	}
	named := len(n.rec.Sections) + len(n.rec.Blocks)
	for _, section := range changes.Sections {
		n.rec.Sections = append(n.rec.Sections, sectionRow(at, section))
	}
	for _, block := range changes.Blocks {
		if (block.Type == "check") != (item.Step == record.DesignStepCriteria) {
			continue
		}
		n.rec.Blocks = append(n.rec.Blocks, blockRow(at, block))
	}
	switch {
	case len(n.rec.Sections)+len(n.rec.Blocks) > named:
		return taken, texts.NewsChangesListed, nil
	case changes.From == 0:
		return taken, texts.NewsChangesNoPair, nil
	}
	return taken, texts.NewsChangesNoneNamed, nil
}

// document is the document item names: the document of its step's gate row as the send names it
// (News.Documents: a dispatch:// link, the old root's for a carried row, or the bare artifact
// reference where nothing says whose it is), where that row holds the item's artifact, and
// otherwise the bare artifact reference (texts.ArtifactReference), since no record then says
// which issue holds it.
func (n newsItems) document(item NewsChange) string {
	if row, ok := n.news.Gates[item.Step]; ok && row.ArtifactID == item.artifact() {
		return n.news.Documents[item.Step]
	}
	return texts.ArtifactReference(item.artifact())
}

// dispatchReason is the reason Dispatch could not answer, as the reconcile recorded it read at at
// (the daemon's giveUpReason): the daemon's sentence of the read it gave up on, that Dispatch holds
// no such document or that Dispatch failed so many reads of it, with Dispatch's own error text
// after it, in a block of its own, bounded at texts.OneAct. The sentence is the daemon's, so the
// block names the daemon as its author (texts.ByDaemon), and Dispatch's words are those it quotes.
func dispatchReason(reason string, at time.Time) *texts.Words {
	if reason == "" {
		return nil
	}
	words := texts.Carry(reason, texts.OneAct, false)
	words.By, words.At = texts.ByDaemon, at
	return &words
}

// sectionRow is the row of one top-level unit that changed in the item at index item, as Dispatch
// names it (record.SectionChange). A `##` section is a section, its place among the sections of its
// heading counted from 1: Dispatch counts the sections of a heading with words from 0, and those
// of an empty heading from 1. A run of the text outside every section, the heading "" with
// occurrence 0, is named by the place Dispatch names it at: before the first section, or after the
// `##` section directly before it, whose heading and place it names too. A run with no place, as a
// Dispatch from before the place names one, is the text outside every section, which places it
// nowhere: such a run can follow any `#` heading, and calling it the text before the first section
// would send the reader to the top of the document for a change that stands lower down.
func sectionRow(item int, section record.SectionChange) texts.DesignNewsSection {
	row := texts.DesignNewsSection{Item: item, Change: section.Change}
	switch {
	case section.Heading != "" || section.Occurrence != 0:
		row.Unit, row.Nth, row.Heading = texts.NewsUnitSection, sectionNth(section.Heading, section.Occurrence), documentWords(section.Heading)
	case section.Place == dispatch.PlaceBeforeFirstSection:
		row.Unit = texts.NewsUnitBeforeFirstSection
	case section.Place == dispatch.PlaceAfterSection:
		row.Unit, row.AfterNth = texts.NewsUnitAfterSection, sectionNth(section.AfterHeading, section.AfterOccurrence)
		row.AfterHeading = documentWords(section.AfterHeading)
	default:
		row.Unit = texts.NewsUnitOutsideSections
	}
	return row
}

// sectionNth is the place of the `##` section of heading and occurrence among the sections of that
// heading, counted from 1: Dispatch counts a heading with words from 0, and an empty heading,
// which has no words to tell it, from 1.
func sectionNth(heading string, occurrence int) int {
	if heading == "" {
		return occurrence
	}
	return occurrence + 1
}

// blockRow is the row of one typed block that changed in the item at index item: its type, where
// the daemon names it (requirement, check, plan_task, callout), or texts.NewsBlockOther with the
// type as the document gives it, and Dispatch's word for how it changed. Its id is the document's
// own text, and so is a type the daemon does not name.
func blockRow(item int, block record.BlockChange) texts.DesignNewsBlock {
	row := texts.DesignNewsBlock{Item: item, Type: block.Type, Change: block.Change, ID: documentWords(block.ID)}
	switch block.Type {
	case "requirement", "check", "plan_task", "callout":
	default:
		row.Type, row.OtherType = texts.NewsBlockOther, documentWords(block.Type)
	}
	return row
}

// documentWords is text of a document, a heading, a block's id or a block's type, as the words of
// another author in a block of their own, bounded at texts.EachOfSeveral: nil for no text, which
// draws no block.
func documentWords(text string) *texts.Words {
	if text == "" {
		return nil
	}
	words := texts.Carry(text, texts.EachOfSeveral, false)
	return &words
}
