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

// The words of C1. The order they are read in is the order of the ladder's own body and
// is written there alone; this block is the vocabulary, one constant per word.
const (
	// HeldTreeClosed is an issue of a tree whose root has finished or lingers after its close, or a
	// member of a tree whose re-admitted root waits in the line, its design gate open, for the
	// promotion that opens its tree again (reopensAtPromotion): the daemon starts no issue of such a
	// tree, and in that last case the root's promotion is what ends the hold.
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
	// HeldAwaitingChildren is a parent that holds a phase start because a Dispatch child of it is
	// still open (Issue.Phase awaiting_children): the daemon runs no phase of the parent's own
	// work and holds the start it had while a child of it is open.
	HeldAwaitingChildren HeldReason = "awaiting_children"
	// HeldDesignGate is an issue of a tree whose design gate stands shut at a step that holds the
	// tree (TreeGate.Open over TreeSteps): a step the deployment gates, or a criteria step the root
	// registered beside a deployment that gates spec and not criteria.
	HeldDesignGate HeldReason = "design_gate"
	// HeldDispatchBlocker is an issue an unfinished Dispatch issue holds where it stands
	// (blockers.Holds, which fills LineFacts.Held).
	HeldDispatchBlocker HeldReason = "dispatch_blocker"
	// HeldChildrenFirst is a parent whose own Dispatch children are still open: none of its own
	// work starts while they stand open (LineFacts.ChildWaits).
	HeldChildrenFirst HeldReason = "children_first"
	// HeldAcceptedAncestor is an issue beneath an ancestor the daemon has already accepted:
	// nothing of it starts while that ancestor stands in retro or merging (HeldUnderAccepted).
	HeldAcceptedAncestor HeldReason = "accepted_ancestor"
	// HeldChildrenBeforeRetro is a parent whose task's last step is not the owner's acceptance,
	// held before its retro while a hold on its acceptance holds it (HoldForChildren,
	// HeldForChildren): issues beneath it still in the workflow, roots that rest on its approval
	// still in the workflow, or open Dispatch children of it. heldBy names each issue of the first
	// of them that holds it, in R8's order (retroHeldBy), as the retro-held notice and the refusal
	// of a retry name them. Once none of them holds it in a tree that runs, the hold ends and the
	// daemon starts the retro itself (workflow's StartHeldRetro). It stands below every
	// word that speaks of the issue's tree, a Dispatch link or an ancestor, because each of those
	// holds the issue whatever its own hold is, and above at_work and unclassified, which it read
	// before it had a word of its own: at_work's sentence, that the issue's workers run, is false
	// of an issue its hold stopped, and unclassified says that no rule names what the issue waits
	// for, where this word names it.
	HeldChildrenBeforeRetro HeldReason = "children_before_retro"
	// HeldTaskStopped is an issue whose task was stopped, by its owner or by a failed step with no
	// step to return to (HoldTaskStopped), and that keeps no slot where it stands: one held from
	// accepting, retro or merging (KeepsSlot). No issue holds it, so heldBy is empty. A later
	// version of the plan from its owner that changes the stopped task, or a person's todo on the
	// root, ends the hold (workflow's restartTask and waitForFirstStep). A task stopped at a step keeps
	// that step's slot and stands in the active set, so one that keeps its slot and holds none is
	// a gap and reads unclassified. It stands beside children_before_retro, for the same reasons;
	// the two are never true of one issue, since an issue holds one hold reason.
	HeldTaskStopped HeldReason = "task_stopped"
	// HeldAtWork is the ladder's `!KeepsSlot(issue)` for an issue not standing in held: an issue
	// that keeps none of the cap's slots where it stands, whose workers run as they did and which
	// no other issue holds. KeepsSlot is InWorkflow and a phase that keeps a slot
	// (PhaseKeepsSlot, over StandsAt), and HeldList keeps only InWorkflow candidates, so the
	// issues that read this word are the ones in accepting, retro or merging (InSlotlessPhase), whose
	// sign-off, retro and merge run on no slot at all. An issue held from one of those
	// phases keeps no slot either, but its hold stopped its work, so it does not read this word:
	// a parent held there for its children reads children_before_retro, a stopped task reads
	// task_stopped, and any other hold falls to unclassified (HeldReasonOf states why at the
	// clause). Of the issues that read it, one whose tree runs is served no `why` (projection's
	// atWorkInSlotlessPhase), so `why` serves this word's sentence only to a member in accepting, retro or merging of
	// a tree that runs nothing. This is the one statement of that; every other reader points here.
	HeldAtWork HeldReason = "at_work"
	// HeldUnclassified is an issue none of the words above is true of (C2). The record holds
	// it in the workflow and no rule here says what it waits for, which is a gap in the words
	// and not in the list: the issue has a place, and a reader can see that it needs one.
	HeldUnclassified HeldReason = "unclassified"
)

// HeldReasons is every word in the order HeldReasonOf reads them, named once for a reader
// that must print or walk them in that order rather than keep an order of its own. Two read it:
// the operator's `admission.held` line (`legion state`), which counts each word and prints the
// counts in this order, so two readings of one state print one line; and HeldBefore, which the
// test of `why`'s sentence table (the projection's heldSentences) holds that table to. The table
// states which words `why` serves under which state. The order is decided in HeldReasonOf's own
// body, and this list must follow that body's cases exactly, since a reorder here moves the
// words that test requires of the table; a word added there is added here, to
// `LEGION_HELD_REASONS` in `packages/contracts/src/legion-api.ts`, and as a row of the table in
// `skills/legion-controller/references/admission.md`.
var HeldReasons = []HeldReason{
	HeldTreeClosed,
	HeldTreeParked,
	HeldLabelOff,
	HeldAncestry,
	HeldRootAwaitingPerson,
	HeldAwaitingChildren,
	HeldDesignGate,
	HeldDispatchBlocker,
	HeldChildrenFirst,
	HeldAcceptedAncestor,
	HeldChildrenBeforeRetro,
	HeldTaskStopped,
	HeldAtWork,
	HeldUnclassified,
}

// HeldBefore reports whether the ladder reads word before other (HeldReasons, HeldReasonOf). It
// is false where either is not a word of HeldReasons, so the zero row's empty word stands
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
// heldBy is absent where no issue holds the one that waits — a root whose label is off, an issue
// whose task was stopped, or an issue none of the other words is true of — so the field carries
// omitempty and this row gets no MarshalJSON writing an empty array for it. That is the one way
// the row differs from BlockedIssue, whose MarshalJSON writes [] for blockedBy because every row
// of that list names at least one blocker and a reader of it would have to answer for a null.
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
	// the pass's one chain (passChain), the same map Waiting walks, so the word and the hold that
	// caused it answer alike about one chain.
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

// reopensAtPromotion says whether root waits in the line for the promotion that opens its tree
// while its design gate stands open: a re-admitted root whose tree's lifecycle is not open under
// the workflow, which the gate leaves queued (ran, dispatch://INFRA-5471), or a root another tree
// handed over carrying an open gate. Such a tree is closed until that promotion opens it, and its
// members wait for it: Waiting's hold of an unopened tree's members (its unopened set, over
// AwaitsPromotion) offers none of them a phase. It is written over QueuedRoot with no slot term,
// and the two name the same roots wherever the gate is open: a queued root whose gate is open
// holds no row the daemon granted it in this tree, since a promotion that granted one opened its
// tree's lifecycle, after which an open gate makes the tree run, and the re-admission frees every
// row of its tree from the generation before (admit's releaseTreeSlots); a root handed over
// awaits its promotion whatever row it carries. Without this the ladder's gate word would pass
// over them, the gate being open, and leave each at at_work or unclassified, which say that its
// workers run or that no rule names its wait. A queued root whose gate is shut answers false, and
// its members read design_gate as they always have.
func reopensAtPromotion(root Issue, facts LineFacts) bool {
	gate := facts.Gates[root.Key]
	return gate.Open() && QueuedRoot(root, facts.Lifecycles[root.Key], gate)
}

// openChildren is the keys of the open Dispatch children of one wait on them — a children-first
// wait (childWaits) or a started parent's (LineFacts.StartedParentWaits) — in the order that
// judgement lists them (ChildWait.Open, Dispatch key order).
func openChildren(wait ChildWait) []string {
	children := make([]string, len(wait.Open))
	for i, child := range wait.Open {
		children[i] = child.Key
	}
	return children
}

// openBeneathIn is the issues beneath a parent held before its retro that are still in the
// workflow (OpenBeneath), the issues the release of its hold waits for, and nothing where none
// is. They are read over the members of its tree among the records the pass walks its chains
// over (chainOf): the pass's own records and the done ones above them, so a grandchild under a
// child already done is beneath the parent here as it is in the workflow's read of the whole
// tree, and the word names what the hold waits for.
func openBeneathIn(issue Issue, chain map[string]Issue) []string {
	members := make([]Issue, 0, len(chain))
	for _, member := range chain {
		if member.Tree == issue.Tree {
			members = append(members, member)
		}
	}
	if open := OpenBeneath(issue, members); len(open) > 0 {
		return open
	}
	return nil
}

// RetroHolds is what a pass's facts show of the three holds on the acceptance of a parent held
// before its retro (HoldForChildren), each by key: the issues beneath it still in the workflow
// (Beneath, openBeneathIn), the roots still in the workflow that rest on its approval (Resting,
// RestingRootsOn), and its open Dispatch children (Open, readChildWaits), the same three the
// workflow reads to hold the parent and to release it (workflow's acceptanceHeld). ReadLineFacts
// puts them together once for each such parent (LineFacts.RetroHolds, retroHoldsOf).
type RetroHolds struct {
	Beneath, Resting, Open []string
}

// None says none of the three holds the parent.
func (h RetroHolds) None() bool {
	return len(h.Beneath) == 0 && len(h.Resting) == 0 && len(h.Open) == 0
}

// retroHoldsOf is the RetroHolds of each parent of held, the parents among the pass's own records
// held before their retro (RetroHeldParents), by key: the issues beneath each, read over the
// records the pass walks its chains over (pass, passChain: the pass's own records and the records
// above them its read leaves out, LineFacts.Parents), the roots that rest on each (resting,
// RestingRootsOn) and its open Dispatch children (children, readChildWaits). It builds nothing
// where no parent is held so.
func retroHoldsOf(pass *passChain, held []string, children map[string]ChildWait, resting map[string][]RestingRoot) map[string]RetroHolds {
	if len(held) == 0 {
		return nil
	}
	own, chain := pass.own(), pass.chain()
	holds := make(map[string]RetroHolds, len(held))
	for _, key := range held {
		roots := make([]string, len(resting[key]))
		for i, root := range resting[key] {
			roots[i] = root.Key
		}
		holds[key] = RetroHolds{Beneath: openBeneathIn(own[key], chain), Resting: roots, Open: openChildren(children[key])}
	}
	return holds
}

// retroHeldBy is what holds issue, a parent held before its retro (HoldForChildren), as heldBy
// names it: the first of the three holds on its acceptance that holds it (LineFacts.RetroHolds),
// in R8's order, as the workflow reads them (acceptanceHeld) — the issues beneath it, then the
// roots that rest on its approval, then its open Dispatch children — each by key, and nothing
// where none holds it, as in a tree that runs nothing yet.
func retroHeldBy(issue Issue, facts LineFacts) []string {
	switch holds := facts.RetroHolds[issue.Key]; {
	case len(holds.Beneath) > 0:
		return holds.Beneath
	case len(holds.Resting) > 0:
		return holds.Resting
	case len(holds.Open) > 0:
		return holds.Open
	}
	return nil
}

// HeldReasonOf is the ladder of C1: the words read in one order, the first that is true of
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
	case recorded && (root.Status == DoneStatus || Lingering(root, facts.Facts.Now) ||
		(!claim.IsTreeRoot(issue.Key, issue.Tree) && reopensAtPromotion(root, facts.Facts))):
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
	// A parent that held a phase start because a child of it is still open stands here on its own
	// phase (awaiting_children), not on its tree's: it reads before design_gate, since its wait is
	// its open children and not its gate. The children that hold it are the started parents'
	// judgement (LineFacts.StartedParentWaits), the one the admission pass moves it and brings it
	// back by; a parent whose last open child has gone since stands with no issue beside it until
	// the pass takes it back to the step its return takes (ReturnStep).
	case issue.Phase == phase.AwaitingChildren:
		return HeldAwaitingChildren, openChildren(facts.Facts.StartedParentWaits[issue.Key])
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
	// The two holds an owner's plan makes stand here, below every word above, each of which holds
	// the issue whatever its own hold is, and above at_work and unclassified (each word's doc says
	// why). A stopped task that keeps its slot is not one of them: it stands in the active set, and
	// one that holds no slot row there is the gap unclassified reports.
	switch {
	case HeldForChildren(issue):
		return HeldChildrenBeforeRetro, retroHeldBy(issue, facts.Facts)
	case HeldFor(issue, HoldTaskStopped) && !KeepsSlot(issue):
		return HeldTaskStopped, nil
	}
	// The issue must not stand in held. Since dispatch://INFRA-2560 a hold from accepting, retro or
	// merging keeps no slot (KeepsSlot reads StandsAt), which makes this word reachable for a held
	// issue, and its sentence — that the issue's workers run as they did — is false for an issue
	// whose hold stopped its work. Such an issue reads the word of its hold above where it has one,
	// and falls to unclassified where it has none. The stalled-tree measure is unmoved: none of
	// these four words silences a tree (admit's silencedTree says why).
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
// here (HeldReasonOf) over the same facts the drop sites read — the pass's one chain (passChain),
// one queuedRoots and one liveRoots, as Waiting makes them — which is what gives an issue the pass
// never saw as a candidate its place, the unclassified row of C2 among them.
//
// The rows are sorted by issue key, which gives the state's body and the tick's notice a stable
// order.
func HeldList(issues []Issue, slots []Slot, line []LineEntry, dropped []HeldIssue, facts LineFacts) []HeldIssue {
	pass := passOver(issues, facts)
	queued := queuedRoots(issues, facts)
	held := HeldFacts{
		ByKey: pass.own(),
		Live:  liveRoots(issues, facts, queued),
		Chain: pass.chain(),
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
