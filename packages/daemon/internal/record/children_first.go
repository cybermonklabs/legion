package record

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// ChildrenFirstCandidates is every issue of issues whose own first phase has not started yet and
// could: it stands at phase.Admitted, it is in Legion's work, and every design gate it waits for
// stands open (LineFacts.GatesOpen: the gate of each issue at or above it in Legion's work that
// holds its own gate, R19 of dispatch://INFRA-3320). Those are the only issues whose first phase
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
// root and every released issue under it are issues whose first phase the line does offer a slot
// to, and each of them waits for whatever Dispatch still shows hanging under it.
func ChildrenFirstCandidates(issues []Issue, facts LineFacts) []string {
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue.Phase != phase.Admitted || !InLegionsWork(issue) || !facts.GatesOpen(issue.Key) {
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
// held by an open child whatever its gates read (R9). Kept apart, a started parent is read by no
// children-first notice (admit's tellChildrenFirst), no children_first held word and no wait of a
// started issue on another issue (LineFacts.Waits, blockers.waitsOf), since all of those read
// ChildWaits.
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

// readChildWaits is both judgements of a parent's open Dispatch children that one pass makes
// (ReadLineFacts): the children-first waits of the issues whose first phase has not started
// (ChildrenFirstCandidates, ChildWaits), and the open children of every started parent
// (StartedParents), with the one child withoutHeldUnderAccepted leaves out. It reads nothing: the
// pass's chains hold the copy's children of every issue the pass was read for and the daemon's
// record of each of them (ReadChains), so the judgement costs no query whatever the number of
// parents, which matters because every judgement the daemon makes about a parent's children runs
// under the one fact lock every fact takes (intake.ApplyFact). ChildWaits judges each parent on
// its own, from that parent's children and their records alone, so one judgement split by parent
// gives each set exactly what a judgement of its own parents would; an issue in both sets — one at
// admitted that owes a start — is judged in both. A parent with none has no entry in either map.
//
// Nothing here is stored, and nothing here moves an issue: the admission pass moves a started
// parent with an open child into awaiting_children and takes one whose last open child has gone
// back to the start it held (admit's childrenWaits), and the held list names the children of a
// parent in awaiting_children (HeldReasonOf), each from this one judgement of the pass.
func readChildWaits(issues []Issue, facts LineFacts) (childrenFirst, started map[string]ChildWait) {
	candidates := ChildrenFirstCandidates(issues, facts)
	startedParents := StartedParents(issues, facts.Owed)
	parents := make(map[string]struct{}, len(candidates)+len(startedParents))
	for _, key := range slices.Concat(candidates, startedParents) {
		parents[key] = struct{}{}
	}
	judged := make([]Issue, 0, len(parents))
	for _, issue := range issues {
		if _, judges := parents[issue.Key]; judges {
			judged = append(judged, issue)
		}
	}
	waits := ChildWaits(facts.Chains, judged)
	return waitsOf(waits, candidates), withoutHeldUnderAccepted(waitsOf(waits, startedParents), facts.Chains)
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

// OpenChildWaits is the open Dispatch children that hold the phase starts of each of parents, by
// parent key: each child the copy holds under it (Chains.Children) judged by the daemon's own
// record of it (ChildWaits), and one child left out (withoutHeldUnderAccepted). It is the engine's
// judgement of one parent's open children (workflow's childrenOpen: the guard on every phase
// start, the sign-off's hold and the refusal of a parent in awaiting_children), the same judgement
// the admission pass makes of every started parent (readChildWaits). It reads the chains of
// project with each parent named (ReadChains), three queries whatever the number of parents and
// children. A parent with none has no entry.
func OpenChildWaits(ctx context.Context, tx pgx.Tx, store Store, project string, parents []Issue) (map[string]ChildWait, error) {
	keys := make([]string, len(parents))
	for i, parent := range parents {
		keys[i] = parent.Key
	}
	chains, err := ReadChains(ctx, tx, store, project, keys)
	if err != nil {
		return nil, err
	}
	return withoutHeldUnderAccepted(ChildWaits(chains, parents), chains), nil
}

// withoutHeldUnderAccepted is waits, the open children of started parents, with one child left
// out of each, and a parent left with none dropped: a child the daemon holds at admitted under an
// accepted issue above it (HeldUnderAccepted). That child starts nothing before the accepted issue
// closes, and that issue's merge waits for the parent, so a parent held on it would wait for ever;
// the sign-off of a parent in accepting leaves it out for the same reason (OpenBeneath). It
// changes waits in place and returns it.
func withoutHeldUnderAccepted(waits map[string]ChildWait, chains *Chains) map[string]ChildWait {
	for key, wait := range waits {
		open := make([]OpenChild, 0, len(wait.Open))
		for _, child := range wait.Open {
			if found, recorded := chains.Record(child.Key); recorded && HeldUnderAccepted(chains, found) {
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
// its own first phase (ChildWaits) and, once its phases started, every later one
// (LineFacts.StartedParentWaits). It is read over two sources and no others: the status Dispatch
// shows the child in, from the daemon's copy of the project's issues, and the daemon's own
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
//	| todo, in_progress, testing, …      | any                                                            | yes   |
//
// A child the architect has filed and not released yet — backlog or triage, with no record of its
// own — is open, because it is work of the parent that has still to happen: the parent's wait
// therefore begins at its gate's opening, before the design-approved turn releases the wave. The
// same two statuses on a child the daemon records are the opposite case, a child that ran and was
// parked or finished, which holds nothing. A child Dispatch shows in work is open whatever the
// daemon's record of it says: a parent is done only when each of its children is done (R24 of
// dispatch://INFRA-3320), and the children are Dispatch's, not a tree's (R15).
//
// An unknown status is open. The safe reading of a status this daemon does not know is that the
// child still has work in it; a wait costs a pass, and starting a parent too early costs its
// children's merges.
func ChildOpen(child DispatchIssueRow, recorded *Issue) bool {
	switch child.Status {
	case "done", "icebox":
		return false
	case "backlog", "triage":
		return recorded == nil || InWorkflow(*recorded)
	default:
		return true
	}
}

// ChildWaits is the children-first judgement of one admission pass: for each of parents, the open
// Dispatch children under it (ChildOpen), by parent key. A parent with none has no entry, so the
// map is exactly the set of issues whose own first phase waits. The children are what the copy
// shows hanging under each parent (Chains.Children), and each is judged by the daemon's record of
// it that the chains hold (Chains.Record).
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
// The wait is only as complete as the copy is. A child in backlog or triage that the daemon has
// never recorded reaches the copy only from a Dispatch listing of the whole project — the boot
// read and the controller tick's re-read — because the daemon consumes the events of its own
// project's issues and such a child has none it has applied. So a daemon that has not listed
// since the child was filed does not see it, and the children-first wait is complete only from
// the first boot listing after the child exists.
func ChildWaits(chains *Chains, parents []Issue) map[string]ChildWait {
	waits := make(map[string]ChildWait, len(parents))
	for _, parent := range parents {
		var open []OpenChild
		for _, row := range chains.Children(parent.Key) {
			var child *Issue
			if found, recorded := chains.Record(row.Key); recorded {
				child = &found
			}
			if !ChildOpen(row, child) {
				continue
			}
			open = append(open, OpenChild{Key: row.Key, Status: row.Status, Released: child != nil && InWorkflow(*child),
				Cycle: cycleAncestor(chains, row, parent)})
		}
		if len(open) > 0 {
			waits[parent.Key] = ChildWait{Parent: parent.Key, Open: open}
		}
	}
	return waits
}

// cycleAncestor is the issue a Dispatch blocked_by link of child names that stands above child
// on its own Dispatch chain — its parent itself, or one above that — and is not finished, and ""
// when no link does. Such a link is a cycle: the child waits for an issue that is waiting for the
// child, and neither can ever start. The chain is the copy's (Chains.Chain), read past issues of
// any status, so a done issue between does not end it.
//
// A link to an ancestor already done holds nothing — a blocker is a wait until that issue is
// done (blockers.Unfinished), and this one is — so it is no cycle and is not named: the child
// waits for nothing and its parent waits only for the child (Chains.finished).
//
// The daemon names a cycle and does nothing else. Taking the link off is a decision about what
// the work depends on, which is a person's to make, and the alternative — the daemon dropping a
// Dispatch link of its own accord — would lose a fact nobody asked it to lose.
func cycleAncestor(chains *Chains, child DispatchIssueRow, parent Issue) string {
	above := map[string]bool{}
	if parent.Status != DoneStatus {
		above[parent.Key] = true
	}
	for _, ancestor := range chains.Chain(parent.Key).Links {
		if ancestor != child.Key && !chains.finished(ancestor) {
			above[ancestor] = true
		}
	}
	for _, blocker := range child.Blockers {
		if above[blocker] {
			return blocker
		}
	}
	return ""
}

// finished says whether key is done: by the daemon's record of it where the read holds one
// (Issue.Status, which holds a status the daemon has written before Dispatch echoes it), else by
// the copy's row of it.
func (c *Chains) finished(key string) bool {
	if issue, recorded := c.records[key]; recorded {
		return issue.Status == DoneStatus
	}
	return c.rows[key].Status == DoneStatus
}

// ChildWaitsOf is the children-first judgement of the pass facts were read for: the open
// Dispatch children of every issue whose first phase has not started (ChildrenFirstCandidates),
// judged (ChildWaits) from the chains ReadLineFacts judged the started parents from
// (readChildWaits), so the judgement costs the pass no read of its own. It is the caller's to
// hand the line as LineFacts.ChildWaits (blockers.JudgeLine), which the line reads and
// ReadLineFacts leaves empty. Facts ReadLineFacts did not read carry none, which says that
// nothing waits for its children.
func ChildWaitsOf(facts LineFacts) map[string]ChildWait {
	return facts.childrenFirst
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
