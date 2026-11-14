package record

import (
	"slices"
	"sort"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// HeldReason is the one word for why the daemon runs nothing of an issue its own record holds in
// the workflow and queues it nowhere: an issue that stands in neither the active set nor the
// waiting line stands in the held list with this word beside it (C1 of dispatch://INFRA-3107).
// The words are a closed list, read in one order by one ladder (HeldReasonOf), so that the
// projected state and the controller's tick give one issue one word rather than each naming
// whichever hold it noticed first.
type HeldReason string

// The eleven words of C1. The order they are read in is the order of the ladder's own body and
// is written there alone; this block is the vocabulary, one constant per word.
const (
	// HeldTreeClosed is an issue of a tree whose root has finished or lingers after its close:
	// the daemon starts no issue of such a tree.
	HeldTreeClosed HeldReason = "tree_closed"
	// HeldTreeParked is an issue of a tree that was parked, its root in backlog or icebox: the
	// two statuses of OutOfWorkflowStatuses that stop work without finishing it.
	HeldTreeParked HeldReason = "tree_parked"
	// HeldLabelOff is a root of its own tree whose record says the Legion label is off
	// (Issue.HandedOver): nothing hands it to the daemon.
	HeldLabelOff HeldReason = "label_off"
	// HeldAncestry is a root whose Dispatch chain names a live root that is not itself: it
	// stands as that tree's unreleased work (the ancestry hold of Waiting).
	HeldAncestry HeldReason = "ancestry"
	// HeldRootAwaitingPerson is an issue of a tree whose root has handed work to a person
	// (Issue.AwaitingPerson, answered or not): the daemon starts no issue of that tree before
	// that root resumes.
	HeldRootAwaitingPerson HeldReason = "root_awaiting_person"
	// HeldDesignGate is an issue of a tree whose design gate stands shut at a step the
	// deployment gates (TreeGate.Open).
	HeldDesignGate HeldReason = "design_gate"
	// HeldDispatchBlocker is an issue an unfinished Dispatch issue holds where it stands
	// (blockers.Holds, which fills LineFacts.Held).
	HeldDispatchBlocker HeldReason = "dispatch_blocker"
	// HeldChildrenFirst is a parent whose own Dispatch children are still open: none of its own
	// work starts while they stand open (LineFacts.ChildWaits).
	HeldChildrenFirst HeldReason = "children_first"
	// HeldAcceptedAncestor is an issue beneath an ancestor the daemon has already accepted:
	// nothing of it starts before that ancestor closes (HeldUnderAccepted).
	HeldAcceptedAncestor HeldReason = "accepted_ancestor"
	// HeldAtWork is the ladder's `!KeepsSlot(issue)` for an issue not standing in held: an issue
	// that keeps none of the cap's slots where it stands, whose workers run as they did and which
	// no other issue holds. KeepsSlot is InWorkflow and a phase up to the review
	// (PhaseKeepsSlot, over StandsAt), and HeldList keeps only InWorkflow candidates, so the
	// issues that read this word are the ones past their review — in accepting, retro or merging
	// — whose sign-off, retro and merge run on no slot at all. An issue held from one of those
	// phases keeps no slot either, but its hold stopped its work, so it does not read this word
	// and falls to unclassified (HeldReasonOf states why at the clause). Of the issues that read
	// it, one whose tree runs is served no `why` (projection's atWorkPastReview), so `why`
	// serves this word's sentence only to a member past its review of a tree that runs nothing.
	// This is the one statement of that; every other reader points here.
	HeldAtWork HeldReason = "at_work"
	// HeldUnclassified is an issue none of the ten words above is true of (C2). The record holds
	// it in the workflow and no rule here says what it waits for, which is a gap in the words
	// and not in the list: the issue has a place, and a reader can see that it needs one.
	HeldUnclassified HeldReason = "unclassified"
)

// HeldReasons is the eleven words in the order HeldReasonOf reads them, named once for a reader
// that must print or walk them in that order rather than keep an order of its own. Two read it:
// the operator's `admission.held` line (`legion state`), which counts each word and prints the
// counts in this order, so two readings of one state print one line; and HeldBefore, which the
// test of `why`'s sentence table (the projection's heldSentences) holds that table to. The table
// states which words `why` serves under which state. The order is decided in HeldReasonOf's own
// body, and this list must follow that body's cases exactly, since a reorder here moves the
// words that test requires of the table; a word added there is added here and to
// `LEGION_HELD_REASONS` in `packages/contracts/src/legion-api.ts`.
var HeldReasons = []HeldReason{
	HeldTreeClosed,
	HeldTreeParked,
	HeldLabelOff,
	HeldAncestry,
	HeldRootAwaitingPerson,
	HeldDesignGate,
	HeldDispatchBlocker,
	HeldChildrenFirst,
	HeldAcceptedAncestor,
	HeldAtWork,
	HeldUnclassified,
}

// HeldBefore reports whether the ladder reads word before other (HeldReasons, HeldReasonOf). It
// is false where either is not one of the eleven words, so the zero row's empty word stands
// before nothing. The test of `why`'s sentence table (the projection's heldSentences) requires
// the table to serve under `blocked` exactly the words HeldBefore reads before dispatch_blocker;
// the table states the rule that serves them.
func HeldBefore(word, other HeldReason) bool {
	rank, otherRank := slices.Index(HeldReasons, word), slices.Index(HeldReasons, other)
	return rank >= 0 && rank < otherRank
}

// HeldIssue is one issue the daemon's record holds in the workflow that stands in neither the
// active set nor the waiting line: its key, the one word for why (HeldReason) and the issues that
// hold it. It is declared here, below every reader, as BlockedIssue is declared in outbox.go, so
// the state's admission.held and the controller's tick notice carry one shape rather than two
// that could drift.
//
// heldBy is absent where no issue holds the one that waits — a root whose label is off, or an
// issue none of the other words is true of — so the field carries omitempty and this row gets no
// MarshalJSON writing an empty array for it. That is the one way the row differs from
// BlockedIssue, whose MarshalJSON writes [] for blockedBy because every row of that list names at
// least one blocker and a reader of it would have to answer for a null.
type HeldIssue struct {
	Issue  string     `json:"issue"`
	Reason HeldReason `json:"reason"`
	HeldBy []string   `json:"heldBy,omitempty"`
}

// HeldFacts is everything the ladder reads beside the issue itself, built once by whoever
// classifies: the pass that builds the line builds it at the top (Waiting) and the assembly
// builds the same thing from the same calls (HeldList), so a drop site and the backstop that
// answers for a candidate no drop site saw read one set of facts and give one answer.
type HeldFacts struct {
	// ByKey is the pass's own records by key, built over the whole issue list the pass was
	// given rather than over the candidates, so that a parked or closed root among those
	// records is found here and its tree's members read tree_parked or tree_closed.
	ByKey map[string]Issue
	// Live is the live roots (liveRoots), which the ancestry word reads: LineFacts carries no
	// such set, and the word must be read against the same trees the line's own ancestry hold
	// is read against.
	Live map[string]struct{}
	// Chain is the records the accepted-ancestor walk reads, chainOf(ByKey, LineFacts.Parents):
	// the same map Waiting walks, so the word and the hold that caused it answer alike about
	// one chain.
	Chain map[string]Issue
	// Facts is the pass's own facts, for the gates, the Dispatch blockers, the children-first
	// rows and the moment the pass read the record (LineFacts.Now), which the tree_closed word
	// reads a root's linger against.
	Facts LineFacts
}

// treeRoot is the record of an issue's tree root as the ladder reads it: the pass's own records
// first, then the roots its read leaves out (LineFacts.Roots, the roots a live read holds no
// record of), so one lookup answers for both and no word has to know which of the two holds a
// given root. A root neither map holds reads as not recorded, and the words that need the
// root's own record pass over it.
func (f HeldFacts) treeRoot(tree string) (Issue, bool) {
	if root, recorded := f.ByKey[tree]; recorded {
		return root, true
	}
	root, recorded := f.Facts.Roots[tree]
	return root, recorded
}

// liveAncestry is the live roots an issue's Dispatch chain names that are not the issue itself
// (Issue.Ancestry): the issues holding a root the ancestry word names. It is written over the
// same clause as the ancestry skip of Waiting's queued-root loop, which is what puts such a root
// out of the line, so a change to that hold moves this word with it.
func liveAncestry(issue Issue, live map[string]struct{}) []string {
	var under []string
	for _, key := range issue.Ancestry {
		if _, above := live[key]; above && key != issue.Key {
			under = append(under, key)
		}
	}
	return under
}

// openChildren is the keys of the open Dispatch children of one children-first wait, in the order
// that judgement lists them (ChildWait.Open, Dispatch key order).
func openChildren(wait ChildWait) []string {
	children := make([]string, len(wait.Open))
	for i, child := range wait.Open {
		children[i] = child.Key
	}
	return children
}

// HeldReasonOf is the ladder of C1: the eleven words read in one order, the first that is true of
// the issue being its word, with the issues that clause names beside it and nothing where the
// clause names none. The order is the whole of the rule — a member of a closed tree is
// tree_closed and not design_gate, though its gate is shut too — and this body is the one
// statement of it, so that the drop sites of the waiting line (Waiting) and the backstop that
// answers for a candidate none of them saw (HeldList) cannot give one issue two words. A reader
// that must walk the words in this order, or ask whether one word is read before another, reads
// HeldReasons or HeldBefore, which is this body's order written out (HeldReasons says who reads
// it and why the list must follow this body).
//
// The state and the controller's tick both read it, over the same candidates and the same facts,
// so one issue reads one word wherever it is read.
//
// Status is the daemon's own status field throughout, never DispatchStatus: it is what
// OutOfWorkflow reads, and what the linger's hand-over of a closed tree's members reads, so a
// word that says a tree is closed and a release that acts on it agree. The linger the first word
// reads is the one still running at the moment the pass read the record (Lingering,
// LineFacts.Now), never Issue.Lingers, which stays true for ever once a deadline was armed: every
// park arms one (workflow's beginLinger) and nothing but a re-admission clears it, so the broader
// reading would give every parked tree's member tree_closed and leave tree_parked unreachable.
func HeldReasonOf(issue Issue, facts HeldFacts) (HeldReason, []string) {
	root, recorded := facts.treeRoot(issue.Tree)
	// Each of these is read where its own word is, and not before it: the ancestry scan, the
	// chain walk and the two map reads cost nothing for an issue an earlier word answers for,
	// which on a project of hundreds of records is most of them.
	switch {
	case recorded && (root.Status == DoneStatus || Lingering(root, facts.Facts.Now)):
		return HeldTreeClosed, []string{issue.Tree}
	case recorded && (root.Status == "backlog" || root.Status == "icebox"):
		return HeldTreeParked, []string{issue.Tree}
	case claim.IsTreeRoot(issue.Key, issue.Tree) && !issue.HandedOver:
		return HeldLabelOff, nil
	}
	if ancestry := liveAncestry(issue, facts.Live); len(ancestry) > 0 {
		return HeldAncestry, ancestry
	}
	// Both words speak of the issue's tree root's own record, so both ask that the record is
	// held: a root the pass's issues leave out is read by ReadTreeRoots into LineFacts.Roots,
	// and one neither map holds is a root nothing records at all. Unguarded, the gate word
	// answered for such an issue whatever its tree says — the pass reads a gate for the roots
	// among its own issues and for the roots it read beside them (ReadLineFacts), so a root
	// nothing records has none, and TreeGate.Open() over no rows is false — so the issue read
	// design_gate, held by a key no record holds, where C2 puts it under unclassified. Only
	// these two are guarded: tree_closed and tree_parked ask the same of the record above, and
	// every word below reads facts of the issue itself, so an issue with no recorded root that
	// a Dispatch link holds still reads dispatch_blocker, which is C1's order (the architect's
	// ruling of 2026-11-12T07:28Z on dispatch://INFRA-3107).
	switch {
	case recorded && root.AwaitingPerson():
		return HeldRootAwaitingPerson, []string{issue.Tree}
	case recorded && !facts.Facts.Gates[issue.Tree].Open():
		return HeldDesignGate, []string{issue.Tree}
	}
	if blockedBy := facts.Facts.Held[issue.Key]; len(blockedBy) > 0 {
		return HeldDispatchBlocker, blockedBy
	}
	if children := facts.Facts.ChildWaits[issue.Key]; len(children.Open) > 0 {
		return HeldChildrenFirst, openChildren(children)
	}
	// The ancestor is read through the rule that holds such an issue where it stands
	// (acceptedAncestorHolding, which HeldUnderAccepted answers yes or no with), so the word
	// and the hold that caused it name one ancestor and the chain is walked once.
	if accepted := acceptedAncestorHolding(issue, facts.Chain); accepted != "" {
		return HeldAcceptedAncestor, []string{accepted}
	}
	// The issue must not stand in held. Since dispatch://INFRA-2560 a hold from a phase past the
	// review keeps no slot (KeepsSlot reads StandsAt), which makes this word reachable for a held
	// issue, and its sentence — that the issue's workers run as they did — is false for an issue
	// whose hold stopped its work. Such an issue falls to unclassified instead. The stalled-tree
	// measure is unmoved: neither word silences a tree (admit's silencedTree).
	if !KeepsSlot(issue) && issue.Phase != phase.Held {
		return HeldAtWork, nil
	}
	return HeldUnclassified, nil
}

// HeldList is the held list of one pass: every candidate that stands in neither the active set
// nor the waiting line, with the word for why. It is the one assembly, and the state and the
// tick both call it, so the two lists are built alike from the two readers' own records.
//
// issues is the same list the caller gave Waiting, and the candidates are the records of it the
// daemon holds in the workflow (InWorkflow, its own name for that): the facts are built over
// the whole list, so a closed or parked root among the records is found, and only a candidate
// takes a row. Both callers ask the same of their candidates, so the filter is read here and
// neither has to remember it. dropped is what the pass left behind (Waiting's second answer),
// and line is the line it built.
//
// A key a slot row names and a key the line names take no row: the active set and the waiting
// line already hold them, and a deferral the other loop went on to place is dropped here. The
// first row dropped holds for a key is the one kept, so an issue two loops passed over keeps the
// word of the first site that left it behind. A candidate dropped names nowhere reads the ladder
// here (HeldReasonOf) over the same facts the drop sites read — one chainOf, one queuedRoots and
// one liveRoots, as Waiting makes them — which is what gives an issue the pass never saw as a
// candidate its place, the unclassified row of C2 among them.
//
// The rows are sorted by issue key, which gives the state's body and the tick's notice a stable
// order.
func HeldList(issues []Issue, slots []Slot, line []LineEntry, dropped []HeldIssue, facts LineFacts) []HeldIssue {
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	queued := queuedRoots(issues, facts)
	held := HeldFacts{
		ByKey: byKey,
		Live:  liveRoots(issues, facts, queued),
		Chain: chainOf(byKey, facts.Parents),
		Facts: facts,
	}
	placed := make(map[string]struct{}, len(slots)+len(line))
	for _, slot := range slots {
		placed[slot.Issue] = struct{}{}
	}
	for _, entry := range line {
		placed[entry.Issue.Key] = struct{}{}
	}
	reported := make(map[string]HeldIssue, len(dropped))
	for _, row := range dropped {
		if _, seen := reported[row.Issue]; !seen {
			reported[row.Issue] = row
		}
	}
	rows := make([]HeldIssue, 0, len(issues))
	for _, issue := range issues {
		if !InWorkflow(issue) {
			continue
		}
		if _, stands := placed[issue.Key]; stands {
			continue
		}
		row, named := reported[issue.Key]
		if !named {
			reason, heldBy := HeldReasonOf(issue, held)
			row = HeldIssue{Issue: issue.Key, Reason: reason, HeldBy: heldBy}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Issue < rows[j].Issue })
	return rows
}
