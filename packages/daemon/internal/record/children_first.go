package record

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// ChildrenFirstCandidates is every issue of issues whose own first phase has not started yet and
// could: it stands at phase.Admitted, Dispatch still shows it in the workflow, its tree is not
// lingering, and its tree's design gate stands open. Those are the only issues whose first phase
// the children-first rule can hold, so they are the only parents whose Dispatch children this
// judgement reads (readChildWaits). A parent whose phases started is read by StartedParents, since
// an open child holds every later phase start of it too (dispatch://INFRA-3320 R9). The keys come
// out sorted, so one pass reads the same set in the same order whatever order the records arrived
// in.
//
// The open gate is what puts the start of the wait where the spec puts it: at the gate's
// opening, before the architect's design-approved turn releases the wave. Before that opening a
// root's architect is still writing the spec and its own first phase is nothing the line would
// offer a slot to, so a root there waits for nothing and is told nothing; from the opening the
// root and every released member of its tree are issues whose first phase the line does offer a
// slot to, and each of them waits for whatever Dispatch still shows hanging under it.
func ChildrenFirstCandidates(issues []Issue, gates map[string]TreeGate) []string {
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		// The root must be among the records and must not linger. A root whose tree has left the
		// workflow is no part of a live read at all — its phase is done and its status is not
		// todo (Store.LiveIssues) — so an unrecorded root is a tree that has gone, and reading
		// the zero record's linger would answer false for exactly the case this test is for.
		root, recorded := byKey[issue.Tree]
		if issue.Phase != phase.Admitted || !InWorkflow(issue) || !recorded || root.Lingers() {
			continue
		}
		if !gates[issue.Tree].Open() {
			continue
		}
		keys = append(keys, issue.Key)
	}
	slices.Sort(keys)
	return keys
}

// StartedParents is every issue of issues whose phases have started and whose next phase start an
// open Dispatch child of it holds (dispatch://INFRA-3320 R9): it is in the workflow (InWorkflow)
// and stands in a phase whose start an open child holds (OpenChildHolds) or is held from one
// (StandsAt), owes its worker a start (owed, LineFacts.Owed), or already waits in
// awaiting_children. Those are the only started parents whose Dispatch children are read
// (readChildWaits). The keys come out sorted, as ChildrenFirstCandidates' do.
//
// It is a judgement of its own beside ChildrenFirstCandidates rather than a widening of it, and it
// reads no design gate: the children-first rule waits for an open gate because before it a root's
// first phase is nothing the line would offer a slot to, while a started parent's next start is
// held by an open child whatever its tree's gate reads (R9). Kept apart, a started parent is read
// by no children-first notice (admit's tellChildrenFirst), no children_first held word and no wait
// of a started issue on another issue (LineFacts.Waits, blockers.waitsOf), since all of those read
// LineFacts.ChildWaits.
func StartedParents(issues []Issue, owed map[string]OwedStart) []string {
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		if !InWorkflow(issue) {
			continue
		}
		_, owes := owed[issue.Key]
		if at := StandsAt(issue); !OpenChildHolds(at) && at != phase.AwaitingChildren && !owes {
			continue
		}
		keys = append(keys, issue.Key)
	}
	slices.Sort(keys)
	return keys
}

// OpenChildHolds says whether p is a phase whose start an open Dispatch child of the issue holds
// once its phases have started (dispatch://INFRA-3320 R9): planning through accepting. The
// issue's first phase is not among the starts it reaches — that start is the admission line's
// (ChildrenFirstCandidates, workflow's StartFirstPhase) — nor retro and merging, which start once
// the daemon has accepted the issue (AcceptedPhase), after its children. The workflow's guard on
// every phase start (workflow's childrenHolding) and this judgement of the started parents
// (StartedParents) both read it, so the admission pass and the engine never disagree about which
// starts an open child holds.
func OpenChildHolds(p phase.Phase) bool {
	switch p {
	case phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Accepting:
		return true
	}
	return false
}

// readChildWaits is the three judgements of a parent's open Dispatch children that one pass makes
// (ReadLineFacts), from one read of the mirror's children of every parent any of them judges and
// one read of the daemon's own record of every child, whatever the number of parents: the
// children-first waits of the issues whose first phase has not started (ChildrenFirstCandidates,
// childWaits), the open children of every started parent (StartedParents), and the open children
// of every parent held before its retro, held (RetroHeldParents, LineFacts.RetroHolds), the
// last two with the one child withoutHeldUnderAccepted leaves out. childWaits judges each parent
// on its own, from that parent's rows and its children's records alone, so one read split by
// parent gives each judgement exactly what a read of its own parents would; an issue in more than
// one set — one at admitted that owes a start — is judged in each. A parent with none has no entry
// in any of the three maps. facts are the pass's own, which carry the gates each candidate is read
// against and the starts the project owes (LineFacts.Owed), and pass the pass's own records
// (passChain.issues) with the pass's one chain (passChain).
//
// It is two queries whatever the number of parents, and none where no issue can wait on its
// children (Store.DispatchChildren reads nothing for no parents, judgeChildren nothing for no
// child), because every judgement the daemon makes about a parent's children runs inside a
// transaction that performs no I/O of its own and under the one fact lock every fact takes
// (intake.ApplyFact): a read per parent would cost a round trip per parent on every fact of every
// consumer, and a read of its own for each judgement four more on every pass.
//
// Nothing here is stored, and nothing here moves an issue: the admission pass moves a started
// parent with an open child into awaiting_children and takes one whose last open child has gone
// back to the step ReturnStep gives its held start (admit's childrenWaits), and the held list
// names the children of a parent in awaiting_children and of one held before its retro
// (HeldReasonOf), each from this one read of the pass.
func readChildWaits(ctx context.Context, tx pgx.Tx, store Store, project string, held []string, facts LineFacts,
	pass *passChain,
) (childrenFirst, started, retroHeld map[string]ChildWait, err error) {
	candidates := ChildrenFirstCandidates(pass.issues, facts.Gates)
	parents := StartedParents(pass.issues, facts.Owed)
	all := slices.Concat(candidates, parents, held)
	slices.Sort(all)
	children, err := store.DispatchChildren(ctx, tx, project, slices.Compact(all))
	if err != nil {
		return nil, nil, nil, err
	}
	waits, records, err := judgeChildren(ctx, tx, store, project, pass, children)
	if err != nil {
		return nil, nil, nil, err
	}
	return waitsOf(waits, candidates), withoutHeldUnderAccepted(waitsOf(waits, parents), records, pass),
		withoutHeldUnderAccepted(waitsOf(waits, held), records, pass), nil
}

// RetroHeldParents is every issue of issues in the workflow held before its retro
// (HoldForChildren), in key order: the parents whose open Dispatch children and resting roots
// ReadLineFacts reads, and whose three holds it puts together (LineFacts.RetroHolds), which the
// held word children_before_retro names (HeldReasonOf). A pass with none reads nothing for them.
func RetroHeldParents(issues []Issue) []string {
	var keys []string
	for _, issue := range issues {
		if InWorkflow(issue) && HeldForChildren(issue) {
			keys = append(keys, issue.Key)
		}
	}
	slices.Sort(keys)
	return keys
}

// waitsOf is the entries of waits whose parent is one of parents, in a map of their own.
func waitsOf(waits map[string]ChildWait, parents []string) map[string]ChildWait {
	picked := make(map[string]ChildWait, len(parents))
	for _, parent := range parents {
		if wait, open := waits[parent]; open {
			picked[parent] = wait
		}
	}
	return picked
}

// OpenChildWaits is the open Dispatch children that hold the phase starts of each parent of
// children, the mirror's rows under each (Store.DispatchChildren), by parent key: each child
// judged by the daemon's own record of it (judgeChildren), and one child left out
// (withoutHeldUnderAccepted). It is the engine's judgement of one parent's open children
// (workflow's childrenOpen: the guard on every phase start, the sign-off's hold and the refusal of
// a parent in awaiting_children), the same judgement the admission pass makes of every started
// parent (readChildWaits). A parent with none has no entry. issues are the records the parents are
// read in — one parent's tree — and above the records above them that the cycle walk crosses
// (LineFacts.Parents), nil for none.
func OpenChildWaits(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue, children map[string][]DispatchIssueRow,
	above map[string]Issue,
) (map[string]ChildWait, error) {
	pass := &passChain{issues: issues, above: above}
	waits, records, err := judgeChildren(ctx, tx, store, project, pass, children)
	if err != nil {
		return nil, err
	}
	return withoutHeldUnderAccepted(waits, records, pass), nil
}

// withoutHeldUnderAccepted is waits, the open children of started parents, with one child left
// out of each, and a parent left with none dropped: a child of the parent's own tree that the
// daemon holds at admitted under an accepted ancestor (HeldUnderAccepted). That child starts
// nothing before the ancestor closes, and the ancestor's merge waits for the parent, so a parent
// held on it would wait for ever; the sign-off of a parent in accepting leaves it out for the same
// reason (OpenBeneath). records are the children's records the waits were judged by
// (judgeChildren), and pass the chain of the pass (passChain): the records the parents are read
// in and the records above them that the cycle walk crosses. It changes waits in place and
// returns it.
func withoutHeldUnderAccepted(waits map[string]ChildWait, records map[string]Issue, pass *passChain) map[string]ChildWait {
	if len(waits) == 0 {
		return waits
	}
	parents, chain := pass.own(), pass.chain()
	for key, wait := range waits {
		tree := parents[key].Tree
		open := make([]OpenChild, 0, len(wait.Open))
		for _, child := range wait.Open {
			if found, recorded := records[child.Key]; recorded && found.Tree == tree && HeldUnderAccepted(found, chain) {
				continue
			}
			open = append(open, child)
		}
		if len(open) == 0 {
			delete(waits, key)
			continue
		}
		waits[key] = ChildWait{Parent: key, Open: open}
	}
	return waits
}

// ChildOpen says whether one Dispatch child of a parent still holds that parent's phase starts:
// its own first phase (childWaits) and, once its phases started, every later one
// (LineFacts.StartedParentWaits). It is read over two sources and no others: the status Dispatch
// shows the child in, from the daemon's mirror of the project's issues, and the daemon's own
// record of the child, nil when it has never recorded one.
//
//	| Dispatch status of the child       | daemon record of the child                                   | open? |
//	| ---------------------------------- | ------------------------------------------------------------ | ----- |
//	| done                               | any                                                            | no    |
//	| icebox                             | any                                                            | no    |
//	| backlog                            | none                                                           | yes   |
//	| backlog                            | one whose phase is done or whose status left the workflow      | no    |
//	| triage                             | none                                                           | yes   |
//	| triage                             | one that left the workflow                                     | no    |
//	| todo, in_progress, testing, …      | none, or one that is no tree of its own                        | yes   |
//	| todo, in_progress, testing, …      | a tree of its own that has left the workflow                   | no    |
//
// A child the architect has filed and not released yet — backlog or triage, with no record of its
// own — is open, because it is work of the parent that has still to happen: the parent's wait
// therefore begins at its gate's opening, before the design-approved turn releases the wave. The
// same two statuses on a child some tree of the daemon records are the opposite case, a child a
// tree ran and parked or finished, which holds nothing. Which tree recorded it does not come into
// it: a parked child holds nothing wherever it was parked, and reading it as open because another
// tree parked it would hold its parent on work no tree will do.
//
// A descendant that has become a tree of its own — handed over, or recorded under its own key as
// its tree's root — is read by its own record whatever Dispatch shows: its run belongs to that
// tree now, and it is open exactly while it is in the workflow there.
//
// An unknown status is open. The safe reading of a status this daemon does not know is that the
// child still has work in it; a wait costs a pass, and starting a parent too early costs its
// children's merges.
func ChildOpen(child DispatchIssueRow, recorded *Issue) bool {
	switch child.Status {
	case "done", "icebox":
		return false
	}
	if recorded != nil && (recorded.HandedOverFrom != "" || claim.IsTreeRoot(recorded.Key, recorded.Tree)) {
		return InWorkflow(*recorded)
	}
	switch child.Status {
	case "backlog", "triage":
		return recorded == nil || InWorkflow(*recorded)
	default:
		return true
	}
}

// childWaits is the children-first judgement of one admission pass: for each parent of children,
// the open Dispatch children under it (ChildOpen), by parent key. A parent with none has no
// entry, so the map is exactly the set of issues whose own first phase waits. children is what
// the mirror says hangs under each parent (Store.DispatchChildren) and pass the records the pass
// already holds (passChain.own) with the chain it walks.
//
// Nothing here is stored. The wait ends when every open child is released and leaves the
// workflow, is closed, is moved to icebox, is moved to another parent, or is moved out from
// under its parent to no parent at all — Dispatch reports the last two the same way, as the
// issue's parent now, and the copy takes both from the first reading that shows them at a higher
// sequence than the parent it holds (Store.PutDispatchIssue). Each of those is a change to a row
// of the copy or to a record that the next pass reads. A flag would have to be cleared by
// whichever of them happened, and a missed clear would hold a parent for ever; a judgement made
// again each pass costs one pass when a row of the copy is stale, and the boot listing and the
// controller tick both read Dispatch again and write each value a newer reading shows.
//
// The wait is only as complete as the mirror is. A child in backlog or triage that the daemon has
// never recorded reaches the mirror only from a Dispatch listing of the whole project — the boot
// read and the controller tick's re-read — because the daemon consumes the events of its own
// project's issues and such a child has none it has applied. So a daemon that has not listed
// since the child was filed does not see it, and the children-first wait is complete only from
// the first boot listing after the child exists.
//
// The chain is the pass's one chain (passChain): its own records and the records of the issues
// over them that the pass leaves out (LineFacts.Parents), so that the cycle's own walk of the
// parent chain crosses an ancestor already done, read here exactly as the line reads it.
func childWaits(pass *passChain, children map[string][]DispatchIssueRow, records map[string]Issue) map[string]ChildWait {
	own, chain := pass.own(), pass.chain()
	waits := make(map[string]ChildWait, len(children))
	for key, rows := range children {
		parent, recorded := own[key]
		if !recorded {
			continue
		}
		var open []OpenChild
		for _, row := range rows {
			var child *Issue
			if found, ok := records[row.Key]; ok {
				child = &found
			} else if found, ok := own[row.Key]; ok {
				child = &found
			}
			if !ChildOpen(row, child) {
				continue
			}
			open = append(open, OpenChild{Key: row.Key, Status: row.Status, Released: child != nil && InWorkflow(*child),
				Cycle: cycleAncestor(row, parent, chain)})
		}
		if len(open) > 0 {
			waits[key] = ChildWait{Parent: key, Open: open}
		}
	}
	return waits
}

// cycleAncestor is the issue a Dispatch blocked_by link of child names that stands above child
// on its own parent chain — its parent itself, or one above that, up to the tree's own root —
// and is not finished, and "" when no link does. Such a link is a cycle: the child waits for an
// issue that is waiting for the child, and neither can ever start. The chain is the one every
// rule that reads "above" walks (Ancestors), over the records the pass already holds, so a gap
// in it is crossed here exactly as AcceptedAncestor crosses one.
//
// A link to an ancestor already done holds nothing — a blocker is a wait until that issue is
// done (blockers.Unfinished), and this one is — so it is no cycle and is not named: the child
// waits for nothing and its parent waits only for the child. The ancestor's own record is what
// says so, which this walk has in hand.
//
// The daemon names a cycle and does nothing else. Taking the link off is a decision about what
// the work depends on, which is a person's to make, and the alternative — the daemon dropping a
// Dispatch link of its own accord — would lose a fact nobody asked it to lose.
func cycleAncestor(child DispatchIssueRow, parent Issue, byKey map[string]Issue) string {
	above := map[string]bool{}
	if parent.Status != DoneStatus {
		above[parent.Key] = true
	}
	for _, ancestor := range Ancestors(parent, byKey) {
		if ancestor.Key != child.Key && ancestor.Status != DoneStatus {
			above[ancestor.Key] = true
		}
	}
	for _, blocker := range child.Blockers {
		if above[blocker] {
			return blocker
		}
	}
	return ""
}

// ChildWaitsOf is the children-first judgement of the pass facts were read for: the open
// Dispatch children of every issue whose first phase has not started (ChildrenFirstCandidates),
// judged (childWaits) from the one read of the children ReadLineFacts judged the started parents
// from (readChildWaits), so the judgement costs the pass no read of its own. It is the caller's
// to hand the line as LineFacts.ChildWaits (blockers.JudgeLine), which the line reads and
// ReadLineFacts leaves empty. Facts ReadLineFacts did not read carry none, which says that
// nothing waits for its children.
func ChildWaitsOf(facts LineFacts) map[string]ChildWait {
	return facts.childrenFirst
}

// judgeChildren is the second read both judgements of a parent's children make — the daemon's
// own record of each child the mirror shows under a parent of children (Store.IssuesOf), one
// query whatever the number of children — and the judgement over them (childWaits), with the
// records each child was judged by, by key, which withoutHeldUnderAccepted reads again for the
// one child it leaves out. No child is no read at all, and no walk of pass, the chain of the pass
// (passChain): the records the parents are read in and the records the cycle walk crosses.
func judgeChildren(ctx context.Context, tx pgx.Tx, store Store, project string, pass *passChain,
	children map[string][]DispatchIssueRow,
) (map[string]ChildWait, map[string]Issue, error) {
	keys := make([]string, 0, len(children))
	for _, rows := range children {
		for _, row := range rows {
			keys = append(keys, row.Key)
		}
	}
	if len(keys) == 0 {
		return map[string]ChildWait{}, nil, nil
	}
	slices.Sort(keys)
	// The daemon's own record of each child is read by key rather than taken from issues, the
	// project's live records: a child that has left the workflow is not among those, in this
	// parent's tree or in any other, and a child the daemon has never recorded is not either,
	// and the two are opposite answers — the first holds nothing, the second is work nobody has
	// released yet (ChildOpen). One query, whatever the number of children.
	rows, err := store.IssuesOf(ctx, tx, project, slices.Compact(keys))
	if err != nil {
		return nil, nil, err
	}
	records := byKey(rows)
	return childWaits(pass, children, records), records, nil
}

// ChildWaitRows is the children-first waits as the rows the state's `blocked` and a tick
// controller notice carry. Each waiting parent gives one row naming the open children under it,
// and each cycle among them gives a second row, naming the child and the ancestor its own
// blocked_by link holds it under: the link is a fact about the child, not about the parent, and
// a reader that saw only the parent's row would have no way to tell a child nobody has released
// from one that can never be released at all. The architect's notice says the same two things in
// words (ChildCycleReason). They are built from the judgement the line is held by, so what a
// reader sees is what the decider decided.
func ChildWaitRows(waits map[string]ChildWait) []BlockedIssue {
	rows := make([]BlockedIssue, 0, len(waits))
	for parent, wait := range waits {
		by := make([]string, 0, len(wait.Open))
		for _, child := range wait.Open {
			by = append(by, child.Key)
			if child.Cycle != "" {
				rows = append(rows, BlockedIssue{Issue: child.Key, BlockedBy: []string{child.Cycle}})
			}
		}
		rows = append(rows, BlockedIssue{Issue: parent, BlockedBy: by})
	}
	slices.SortFunc(rows, func(a, b BlockedIssue) int { return strings.Compare(a.Issue, b.Issue) })
	return rows
}
