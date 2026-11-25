package record

import (
	"slices"
	"sort"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// HeldReason is the one word for why the daemon runs nothing of an issue its own record holds in
// the workflow and queues it nowhere: an issue that stands in neither the active set nor the
// waiting line stands in the held list with this word beside it (C1 of dispatch://INFRA-3107).
// The words are a closed list, read in one order by one ladder (HeldReasonOf), so that the
// projected state and the controller's tick give one issue one word rather than each naming
// whichever hold it noticed first.
type HeldReason string

// The words of C1, R16 and R20. The order they are read in is the order of the ladder's own body
// and is written there alone; this block is the vocabulary, one constant per word.
const (
	// HeldChainUnknown is an issue whose Dispatch chain the daemon's copy cannot show to its top
	// (Chain.Known: a row the copy lacks, a loop, more than 32 links): the daemon starts nothing
	// on it until the chain is read right (R16 of dispatch://INFRA-3320).
	HeldChainUnknown HeldReason = "chain_unknown"
	// HeldTreeClosed is an issue of a tree whose root had finished or lingered after its close.
	// The ladder no longer answers it (R20 of dispatch://INFRA-3320: the daemon acts on each
	// issue's own status, and no tree lingers); the word stays in the vocabulary because the
	// contracts before that release name it (api's stateNarrowings).
	HeldTreeClosed HeldReason = "tree_closed"
	// HeldTreeParked is an issue of a tree that was parked, its root in backlog or icebox. The
	// ladder no longer answers it, for the reason HeldTreeClosed gives.
	HeldTreeParked HeldReason = "tree_parked"
	// HeldLabelOff is a root (Chains.IsRoot) whose record says the Legion label is off
	// (Issue.HandedOver): nothing hands it to the daemon.
	HeldLabelOff HeldReason = "label_off"
	// HeldAncestry is an issue whose Dispatch parent is not in Legion's work while an issue above
	// that parent is (R19): it is that issue's unreleased work, held under its parent.
	HeldAncestry HeldReason = "ancestry"
	// HeldRootAwaitingPerson is an issue whose top issue (Chains.TopIssue) has handed work to a
	// person (Issue.AwaitingPerson, answered or not): the daemon starts nothing under it before
	// that issue resumes.
	HeldRootAwaitingPerson HeldReason = "root_awaiting_person"
	// HeldAwaitingChildren is a parent that holds a phase start because a Dispatch child of it is
	// still open (Issue.Phase awaiting_children): the daemon runs no phase of the parent's own
	// work and holds the start it had while a child of it is open.
	HeldAwaitingChildren HeldReason = "awaiting_children"
	// HeldAwaitingTodo is an issue whose run starts as a root's does (Chains.StartsAsRoot),
	// standing at admitted while Dispatch shows it in work at a status other than todo
	// (LineFacts.AwaitsTodo): a root a person moved from todo to in_progress before its first
	// promotion, or a new run that R20 of dispatch://INFRA-3320 opens at the status a person moved
	// the issue to, as when a person reopens an issue the merge of its pull request closed straight
	// to in_progress. Only a person's todo queues such a run, whose promotion starts its architect
	// (R19's handover). No issue holds it.
	HeldAwaitingTodo HeldReason = "awaiting_todo"
	// HeldDesignGate is an issue a design gate it waits for holds where it stands: the gate of an
	// issue at or above it in Legion's work that holds its own gate, or a gate step it holds
	// itself, stands shut at a step that holds it (LineFacts.ShutGates, whose TreeGate.Open reads
	// TreeSteps): a step the deployment gates, or a criteria step the gate holder registered beside
	// a deployment that gates spec and not criteria.
	HeldDesignGate HeldReason = "design_gate"
	// HeldDispatchBlocker is an issue an unfinished Dispatch issue holds where it stands
	// (blockers.Holds, which fills LineFacts.Held).
	HeldDispatchBlocker HeldReason = "dispatch_blocker"
	// HeldChildrenFirst is a parent whose own Dispatch children are still open: none of its own
	// work starts while they stand open (LineFacts.ChildWaits).
	HeldChildrenFirst HeldReason = "children_first"
	// HeldAcceptedAncestor is an issue beneath an issue the daemon has already accepted, one in
	// its retro or its merge: nothing of it starts while that issue stands in retro or merging
	// (HeldUnderAccepted).
	HeldAcceptedAncestor HeldReason = "accepted_ancestor"
	// HeldChildrenBeforeRetro is a parent whose task's last step is not the owner's acceptance,
	// held before its retro while a hold on its acceptance holds it (HoldForChildren,
	// HeldForChildren): issues beneath it on its Dispatch chain still in the workflow, roots that
	// rest on its approval still in the workflow, or open Dispatch children of it. heldBy names
	// each issue of the first of them that holds it, in R8's order (retroHeldBy), as the
	// retro-held notice and the refusal of a retry name them. Once none of them holds it in a tree
	// that runs, the hold ends and the daemon starts the retro itself (workflow's StartHeldRetro).
	// It stands below every word that speaks of the issue's chain, a Dispatch link or an issue
	// above it, because each of those holds the issue whatever its own hold is, and above at_work
	// and unclassified, which it read before it had a word of its own: at_work's sentence, that
	// the issue's workers run, is false of an issue its hold stopped, and unclassified says that no
	// rule names what the issue waits for, where this word names it.
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
	// (PhaseKeepsSlot, over StandsAt), and HeldList keeps only candidates in Legion's work, so the
	// issues that read this word are the ones in accepting, retro or merging (InSlotlessPhase), whose
	// sign-off, retro and merge run on no slot at all. An issue held from one of those
	// phases keeps no slot either, but its hold stopped its work, so it does not read this word:
	// a parent held there for its children reads children_before_retro, a stopped task reads
	// task_stopped, and any other hold falls to unclassified (HeldReasonOf states why at the
	// clause). Of the issues that read it, one whose gate root runs is served no `why`
	// (projection's atWorkInSlotlessPhase), so `why` serves this word's sentence only to an issue
	// in accepting, retro or merging whose gate root runs nothing. This is the one statement of
	// that; every other reader points here.
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
// `skills/legion-controller/references/admission.md`. tree_closed and tree_parked keep their
// place though the ladder answers neither any more, so the words an older contract knows keep
// their order.
var HeldReasons = []HeldReason{
	HeldChainUnknown,
	HeldTreeClosed,
	HeldTreeParked,
	HeldLabelOff,
	HeldAncestry,
	HeldRootAwaitingPerson,
	HeldAwaitingChildren,
	HeldAwaitingTodo,
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
// heldBy is absent where no issue holds the one that waits — a root whose label is off, a run that
// waits for a person's todo, an issue whose task was stopped, or an issue none of the other words
// is true of — so the field carries omitempty and this row gets no MarshalJSON writing an empty
// array for it. That is the one way the row differs from BlockedIssue, whose MarshalJSON writes []
// for blockedBy because every row of that list names at least one blocker and a reader of it would
// have to answer for a null.
type HeldIssue struct {
	Issue  string     `json:"issue"`
	Reason HeldReason `json:"reason"`
	HeldBy []string   `json:"heldBy,omitempty"`
}

// openChildren is the keys of the open Dispatch children of one wait on them — a children-first
// wait (childWaits), a started parent's (LineFacts.StartedParentWaits) or a parent's held before
// its retro (RetroHolds.Open, retroHoldsOf) — in the order that judgement lists them
// (ChildWait.Open, Dispatch key order).
func openChildren(wait ChildWait) []string {
	children := make([]string, len(wait.Open))
	for i, child := range wait.Open {
		children[i] = child.Key
	}
	return children
}

// RetroHolds is what a pass's facts show of the three holds on the acceptance of a parent held
// before its retro (HoldForChildren), each by key: the issues beneath it on its Dispatch chain
// still in the workflow (Beneath, OpenBeneath over the pass's chains), the roots still in the
// workflow that rest on its approval (Resting, RestingRootsOn), and its open Dispatch children
// (Open, readChildWaits), the same three the workflow reads to hold the parent and to release it
// (workflow's acceptanceHeld). ReadLineFacts puts them together once for each such parent
// (LineFacts.RetroHolds, retroHoldsOf).
type RetroHolds struct {
	Beneath, Resting, Open []string
}

// None says none of the three holds the parent.
func (h RetroHolds) None() bool {
	return len(h.Beneath) == 0 && len(h.Resting) == 0 && len(h.Open) == 0
}

// retroHoldsOf is the RetroHolds of each parent of held, the parents among the pass's own records
// held before their retro (RetroHeldParents), by key: the issues beneath each on its Dispatch
// chain, read over chains, the pass's one chain (LineFacts.Chains), the roots that rest on each
// (resting, RestingRootsOn) and its open Dispatch children (children, readChildWaits). It builds
// nothing where no parent is held so.
func retroHoldsOf(chains *Chains, held []string, children map[string]ChildWait, resting map[string][]RestingRoot) map[string]RetroHolds {
	if len(held) == 0 {
		return nil
	}
	holds := make(map[string]RetroHolds, len(held))
	for _, key := range held {
		roots := make([]string, len(resting[key]))
		for i, root := range resting[key] {
			roots[i] = root.Key
		}
		holds[key] = RetroHolds{Beneath: OpenBeneath(chains, key), Resting: roots, Open: openChildren(children[key])}
	}
	return holds
}

// retroHeldBy is what holds issue, a parent held before its retro (HoldForChildren), as heldBy
// names it: the first of the three holds on its acceptance that holds it (LineFacts.RetroHolds),
// in R8's order, as the workflow reads them (acceptanceHeld) — the issues beneath it on its
// Dispatch chain, then the roots that rest on its approval, then its open Dispatch children — each
// by key, and nothing where none holds it, as in a tree that runs nothing yet.
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

// HeldReasonOf is the ladder of C1: the words read in one order, the first that is true of the
// issue being its word, with the issues that clause names beside it and nothing where the clause
// names none. The order is the whole of the rule — an issue whose chain is unknown is
// chain_unknown and not design_gate, though a gate above it may be shut too — and this body is
// the one statement of it, so that the drop sites of the waiting line (Waiting) and the backstop
// that answers for a candidate none of them saw (HeldList) cannot give one issue two words. A
// reader that must walk the words in this order, or ask whether one word is read before another,
// reads HeldReasons or HeldBefore, which is this body's order written out (HeldReasons says who
// reads it and why the list must follow this body).
//
// The state and the controller's tick both read it, over the same candidates and the same facts,
// so one issue reads one word wherever it is read. Every word that speaks of where the issue
// stands reads the pass's chains (LineFacts.Chains) and nothing of the daemon's stored tree (R15
// of dispatch://INFRA-3320): chain_unknown first, since R16 lists an issue on an unknown chain
// held with that word whatever else holds it, and the chain is what every later word reads; then
// the three holds of an issue under a parent that R19 names, each with the issue that holds it.
// Where an issue above it stands in its retro or its merge, the issue reads accepted_ancestor
// even when its parent is out of work too: R21 reads an issue under a done parent under an issue
// in merging as held accepted_ancestor (INFRA-500 under INFRA-430) and one under a done parent
// under an issue at work as held ancestry (INFRA-1238 under INFRA-7), so the ancestry word stands
// aside for the stronger hold and the words keep their order.
//
// holds says whether the issue holds a slot row of the pass, which only awaiting_todo reads: a run
// whose promotion has started its architect holds its row, and waits for no todo.
func HeldReasonOf(issue Issue, holds bool, facts LineFacts) (HeldReason, []string) {
	chains := facts.Chains
	chain := chains.Chain(issue.Key)
	if !chain.Known() {
		return HeldChainUnknown, []string{chain.At}
	}
	// Each of these is read where its own word is, and not before it: the chain is walked once
	// and kept (Chains), and an issue an earlier word answers for, which on a project of hundreds
	// of records is most of them, reads nothing more of it. The accepted issue above is read once,
	// for the ancestry word and for its own (acceptedAncestorHolding).
	above := chains.Above(issue.Key)
	accepted := acceptedAncestorHolding(chains, issue)
	switch {
	case len(above) == 0 && !issue.HandedOver:
		return HeldLabelOff, nil
	case len(above) > 0 && above[0] != chain.Links[0] && accepted == "":
		// The issue that holds it is the nearest one above it in Legion's work, past the parent
		// out of work (R21 of dispatch://INFRA-3320: INFRA-1238 held ancestry under INFRA-7).
		return HeldAncestry, []string{above[0]}
	}
	top := chains.TopIssue(issue.Key)
	if record, recorded := chains.Record(top); recorded && record.AwaitingPerson() {
		return HeldRootAwaitingPerson, []string{top}
	}
	// A parent that held a phase start because a child of it is still open stands here on its own
	// phase (awaiting_children), not on a gate: it reads before design_gate, since its wait is its
	// open children and not a gate. The children that hold it are the started parents' judgement
	// (LineFacts.StartedParentWaits), the one the admission pass moves it and brings it back by; a
	// parent whose last open child has gone since stands with no issue beside it until the pass
	// takes it back to the step its return takes (ReturnStep).
	if issue.Phase == phase.AwaitingChildren {
		return HeldAwaitingChildren, openChildren(facts.StartedParentWaits[issue.Key])
	}
	// A run that starts as a root's does waits for a person's todo (LineFacts.AwaitsTodo) before
	// its gate: its own design gate stands shut, since no architect has designed it yet or a new run
	// cleared it, and the architect that designs it starts only once a todo queues the run, so the
	// todo is read before design_gate and before the links and children read after it. The words
	// read before it stay before it: an unknown chain, which every word after it reads; a root
	// whose label is off, which a todo does not hand over; and R19's holds of an issue under a
	// parent. An issue the chain holds where it stands, by those holds or by accepted_ancestor read
	// below, is no queued root at todo either (LineFacts.Queued), so it never reads this word: what
	// it waits for is its hold. It stands after awaiting_children, which no issue at admitted
	// reads.
	if facts.AwaitsTodo(issue, holds) {
		return HeldAwaitingTodo, nil
	}
	// The gates above an issue and its own hold its first start (R19 of dispatch://INFRA-3320), so
	// they hold an issue at admitted alone: an issue already at work started past them, and a gate
	// that shut above it later, as one above a root that got a parent at work, holds none of it.
	if issue.Phase == phase.Admitted {
		if shut := facts.ShutGates(issue.Key); len(shut) > 0 {
			return HeldDesignGate, shut
		}
	}
	if blockedBy := facts.Held[issue.Key]; len(blockedBy) > 0 {
		return HeldDispatchBlocker, blockedBy
	}
	if children := facts.ChildWaits[issue.Key]; len(children.Open) > 0 {
		return HeldChildrenFirst, openChildren(children)
	}
	// The ancestor is the one the rule that holds such an issue where it stands names
	// (acceptedAncestorHolding, which HeldUnderAccepted answers yes or no with), read above, so
	// the word and the hold that caused it name one ancestor.
	if accepted != "" {
		return HeldAcceptedAncestor, []string{accepted}
	}
	// The two holds an owner's plan makes stand here, below every word above, each of which holds
	// the issue whatever its own hold is, and above at_work and unclassified (each word's doc says
	// why). A stopped task that keeps its slot is not one of them: it stands in the active set, and
	// one that holds no slot row there is the gap unclassified reports.
	switch {
	case HeldForChildren(issue):
		return HeldChildrenBeforeRetro, retroHeldBy(issue, facts)
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
// issues is the same list the caller gave Waiting, and the candidates are the records of it in
// Legion's work (InLegionsWork, R21 of dispatch://INFRA-3320: every issue in Legion's work has one
// place). Both callers ask the same of their candidates, so the filter is read here and neither
// has to remember it. dropped is what the pass left behind (Waiting's second answer), and line
// is the line it built.
//
// A key a slot row names and a key the line names take no row: the active set and the waiting
// line already hold them, and a deferral the other loop went on to place is dropped here. The
// first row dropped holds for a key is the one kept, so an issue two loops passed over keeps the
// word of the first site that left it behind. A candidate dropped names nowhere reads the ladder
// here (HeldReasonOf) over the same facts the drop sites read, the pass's one chain
// (LineFacts.Chains) among them, holding no slot row since a slot holder takes no row, which is
// what gives an issue the pass never saw as a candidate its place, the unclassified row of C2
// among them.
//
// An issue the daemon records nothing of, which the copy shows todo on a chain that is unknown,
// takes a row too, with chain_unknown and the link the chain stopped at (R16 of
// dispatch://INFRA-3320): nothing records it, since no root and no issue under a parent can be
// told apart on such a chain, and the held list is where a reader sees that it waits for the
// daemon to read Dispatch again. A todo issue on a known chain takes no row here: the admission
// pass records it, as a root or under its parent, and the list names it once it does.
//
// The rows are sorted by issue key, which gives the state's body and the tick's notice a stable
// order.
func HeldList(issues []Issue, slots []Slot, line []LineEntry, dropped []HeldIssue, facts LineFacts) []HeldIssue {
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
		if !InLegionsWork(issue) {
			continue
		}
		if _, stands := placed[issue.Key]; stands {
			continue
		}
		row, named := reported[issue.Key]
		if !named {
			reason, heldBy := HeldReasonOf(issue, false, facts)
			row = HeldIssue{Issue: issue.Key, Reason: reason, HeldBy: heldBy}
		}
		rows = append(rows, row)
	}
	for _, key := range facts.Chains.unrecordedTodo() {
		if chain := facts.Chains.Chain(key); !chain.Known() {
			rows = append(rows, HeldIssue{Issue: key, Reason: HeldChainUnknown, HeldBy: []string{chain.At}})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Issue < rows[j].Issue })
	return rows
}
