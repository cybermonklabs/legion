package record

import (
	"sort"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
	// nothing of such a tree runs again until a re-admission opens it.
	HeldTreeClosed HeldReason = "tree_closed"
	// HeldTreeParked is an issue of a tree a person stopped, its root in backlog or icebox: the
	// two statuses of OutOfWorkflowStatuses that stop work without finishing it.
	HeldTreeParked HeldReason = "tree_parked"
	// HeldLabelOff is a root of its own tree whose record says the Legion label is off
	// (Issue.HandedOver): nothing hands it to the daemon, so it waits for nothing.
	HeldLabelOff HeldReason = "label_off"
	// HeldAncestry is a root whose Dispatch chain names a live root that is not itself: it is
	// that tree's unreleased work, which its architect's release brings in (the ancestry hold of
	// Waiting).
	HeldAncestry HeldReason = "ancestry"
	// HeldRootAwaitingPerson is an issue of a tree whose root handed work to a person: its
	// members start with that root's resume.
	HeldRootAwaitingPerson HeldReason = "root_awaiting_person"
	// HeldDesignGate is an issue of a tree whose design gate stands shut at a step the
	// deployment gates (TreeGate.Open).
	HeldDesignGate HeldReason = "design_gate"
	// HeldDispatchBlocker is an issue an unfinished Dispatch issue holds where it stands
	// (blockers.Holds, which fills LineFacts.Held).
	HeldDispatchBlocker HeldReason = "dispatch_blocker"
	// HeldChildrenFirst is a parent whose own Dispatch children are still open, which does its
	// work after theirs (LineFacts.ChildWaits).
	HeldChildrenFirst HeldReason = "children_first"
	// HeldAcceptedAncestor is an issue beneath an ancestor the daemon has already accepted,
	// which waits for that ancestor to close (HeldUnderAccepted).
	HeldAcceptedAncestor HeldReason = "accepted_ancestor"
	// HeldAtWork is an issue that keeps none of the cap's slots where it stands (KeepsSlot),
	// which is an issue at awaiting_merge: its own work is finished and its merge is what is
	// left.
	HeldAtWork HeldReason = "at_work"
	// HeldUnclassified is an issue none of the ten words above is true of (C2). The record holds
	// it in the workflow and no rule here says what it waits for, which is a gap in the words
	// and not in the list: the issue has a place, and a reader can see that it needs one.
	HeldUnclassified HeldReason = "unclassified"
)

// HeldReasons is the eleven words in the order HeldReasonOf reads them, named once for a reader
// that must print or walk them in that order rather than keep an order of its own: the
// operator's `admission.held` line (`legion state`), which counts each word and prints the
// counts in this order, so two readings of one state print one line. It is the vocabulary in the
// ladder's order and nothing more — the order is decided in HeldReasonOf's own body, whose
// cases this list follows, and a word added there is added here.
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

// HeldIssue is one issue the daemon's record holds in the workflow that stands in neither the
// active set nor the waiting line: its key, the one word for why (HeldReason) and the issues that
// hold it. It is declared here, below every reader, as BlockedIssue is declared in outbox.go, so
// the state's admission.held and the controller's tick notice carry one shape rather than two
// that could drift.
//
// heldBy is absent where no issue holds the one that waits — a root whose label is off, an issue
// at awaiting_merge — so the field carries omitempty and this row gets no MarshalJSON writing an
// empty array for it. That is the one way the row differs from BlockedIssue, whose MarshalJSON
// writes [] for blockedBy because every row of that list names at least one blocker and a reader
// of it would have to answer for a null.
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
	// Roots is the record of each tree root the pass's own records do not hold
	// (LineFacts.Roots): the tick reads the live issues, which hold no root that is done or
	// parked, and without them it could not tell a closed tree from a parked one.
	Roots map[string]Issue
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
// first, then the roots its read leaves out, so one lookup answers for both and no word has to
// know which of the two holds a given root. A root neither map holds reads as not recorded, and
// the words that need the root's own record pass over it.
func (f HeldFacts) treeRoot(tree string) (Issue, bool) {
	if root, recorded := f.ByKey[tree]; recorded {
		return root, true
	}
	root, recorded := f.Roots[tree]
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
// that must walk the words in this order rather than ask about one issue reads HeldReasons,
// which is this body's order written out.
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
	ancestry := liveAncestry(issue, facts.Live)
	blockedBy := facts.Facts.Held[issue.Key]
	children := facts.Facts.ChildWaits[issue.Key]
	switch {
	case recorded && (root.Status == DoneStatus || Lingering(root, facts.Facts.Now)):
		return HeldTreeClosed, []string{issue.Tree}
	case recorded && (root.Status == "backlog" || root.Status == "icebox"):
		return HeldTreeParked, []string{issue.Tree}
	case claim.IsTreeRoot(issue.Key, issue.Tree) && !issue.HandedOver:
		return HeldLabelOff, nil
	case len(ancestry) > 0:
		return HeldAncestry, ancestry
	case root.AwaitingPerson():
		return HeldRootAwaitingPerson, []string{issue.Tree}
	case !facts.Facts.Gates[issue.Tree].Open():
		return HeldDesignGate, []string{issue.Tree}
	case len(blockedBy) > 0:
		return HeldDispatchBlocker, blockedBy
	case len(children.Open) > 0:
		return HeldChildrenFirst, openChildren(children)
	case HeldUnderAccepted(issue, facts.Chain):
		return HeldAcceptedAncestor, []string{AcceptedAncestor(issue, facts.Chain)}
	case !KeepsSlot(issue):
		return HeldAtWork, nil
	}
	return HeldUnclassified, nil
}

// HeldList is the held list of one pass: every candidate that stands in neither the active set
// nor the waiting line, with the word for why. It is the one assembly, and the state and the
// tick both call it, so the two lists are built alike from the two readers' own records.
//
// issues is the same list the caller gave Waiting and candidates is that list filtered by
// InWorkflow, which is the daemon's own name for "the record holds it in the workflow": the
// facts are built over the whole list, so a closed or parked root among the records is found,
// and only a candidate takes a row. dropped is what the pass left behind (Waiting's second
// answer), and line is the line it built.
//
// A key a slot row names and a key the line names take no row: the active set and the waiting
// line already hold them, and a deferral the other loop went on to place is dropped here. The
// first row dropped holds for a key is the one kept, so an issue two loops passed over keeps the
// word of the first site that left it behind. A candidate dropped names nowhere reads the ladder
// here (HeldReasonOf) over the same facts the drop sites read — one chainOf, one queuedRoots and
// one liveRoots, as Waiting makes them — which is what gives an issue the pass never saw as a
// candidate its place, the unclassified row of C2 among them.
//
// slots must be the project's own slot rows: the state reads every project's, and a row of
// another project would silently take an issue of this one out of the list. The rows are sorted
// by issue key, which gives the state's body and the tick's notice a stable order.
func HeldList(candidates, issues []Issue, slots []Slot, line []LineEntry, dropped []HeldIssue, facts LineFacts) []HeldIssue {
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	queued := queuedRoots(issues, facts)
	held := HeldFacts{
		ByKey: byKey,
		Roots: facts.Roots,
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
	rows := make([]HeldIssue, 0, len(candidates))
	for _, issue := range candidates {
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
