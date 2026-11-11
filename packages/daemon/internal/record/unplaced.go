package record

import (
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// WaitKind is what holds an issue the daemon has not started where it stands, where what holds
// it is not a Dispatch dependency at all but one of the waiting line's own decisions. A row that
// names only an issue cannot tell a closed tree from an unapproved design gate, a person's
// unanswered question from a hand-over nobody has made, and those are four different moves by
// four different people; the kind says which one is owed.
//
// The two waits that ARE Dispatch dependencies carry no kind, because the issues they name say
// it already: an unfinished issue a blocked_by link names (blockers.Holds, blockers.WaitsOn) and
// an open child that goes before its parent (ChildWaitRows). BlockedIssue.BlockedBy is those
// issues and only those, which is what lets a reader keep reading it as the contract has always
// described it — unfinished issues this one waits on — rather than as a bag of related keys.
type WaitKind string

const (
	// WaitTreeClosed: the issue's tree is no longer live. Its root has finished, lingers after
	// its close, has left the workflow, or has no record here at all, so nothing of that tree
	// starts again. What places this issue is the hand-over its root's close owes it — the
	// expiry of the tree's linger hands over every member that never started (workflow's
	// unfinishedHandOvers) — or a person's re-admission of the tree. The issue named beside it
	// is the tree's root, which is the issue that must move, and naming it under BlockedBy
	// would report a finished issue as an unfinished dependency.
	WaitTreeClosed WaitKind = "tree_closed"
	// WaitDesignGate: a person's approval does not stand on the current version of every step
	// the deployment gates (TreeGate.Open), so the tree's work waits on that approval. The
	// issue named is the tree's root, which holds the gate.
	WaitDesignGate WaitKind = "design_gate"
	// WaitAwaitingPerson: the tree's root has handed a question to a person and nothing of the
	// tree starts until it is answered (admit's promotion of the answered root). The issue
	// named is the root that waits, which is the issue itself for the root's own row.
	WaitAwaitingPerson WaitKind = "awaiting_person"
	// WaitAcceptedAncestor: the daemon has already accepted an ancestor's reviewed head, and
	// nothing enters the workflow beneath it until it closes (HeldUnderAccepted). The issue
	// named is that ancestor.
	WaitAcceptedAncestor WaitKind = "accepted_ancestor"
	// WaitUnreleased: this root's recorded Dispatch chain reaches a live root, so it is that
	// tree's unreleased work and that tree's architect releases it (the ancestry hold in
	// Waiting). The issue named is the live root its chain reaches.
	WaitUnreleased WaitKind = "unreleased_work"
	// WaitUnlabelled: a person has taken the legion label off this root. Legion runs nothing
	// for it and starts nothing for it, which is what taking the label off asks for, and its
	// todo with the label back queues it again. No issue is named: the move is a person's, and
	// there is no issue to name.
	WaitUnlabelled WaitKind = "unlabelled"
	// WaitUnexplained: the line carries this issue nowhere and no rule here says why. It is
	// reported rather than dropped, because an issue in none of the three lists is one no
	// reader can see and no later pass looks at again, which is the state dispatch://INFRA-2964
	// reported. A row of this kind is a defect in this file, not in the records: the kinds
	// above are read in the order Waiting drops a candidate, so a drop this does not name is a
	// drop Waiting makes that nothing here mirrors.
	WaitUnexplained WaitKind = "unexplained"
)

// Wait is one waiting condition of the daemon's own making and the issue it turns on: the
// condition says what must happen, the issue says where. The issue is empty for a hold that
// turns on no issue (WaitUnlabelled).
type Wait struct {
	Kind  WaitKind `json:"kind"`
	Issue string   `json:"issue,omitempty"`
}

// Unplaced is every issue the pass's records call admitted that the pass has placed nowhere — it
// holds none of the cap's slots, the waiting line does not carry it, and no other decider has
// named it — with what holds it (Wait). It is the half of the daemon's account of its own
// admitted issues that was missing: `active`, `waiting` and `blocked` are read as a partition of
// them, and without this they are not one.
//
// The line decides this already, and decides it silently. Waiting drops a candidate at admitted
// where its tree is not live, where its tree's design gate is not open, where its tree's root
// waits on a person, where its Dispatch chain reaches another live root (the ancestry hold) and
// where an ancestor the daemon has accepted holds it; of those five, only the last is named
// anywhere, and only when a Dispatch link or an open child names it too (blockers.Blocked, which
// reads the mirror's links and the children-first wait and nothing else). So an issue any of them
// stops stands in the daemon's own records at admitted, in no list a reader can see, and no
// later pass looks at it again — the state dispatch://INFRA-2964 reported, of nineteen issues at
// once. This is read over the same records, slots, line and facts the line itself was read over,
// in the same transaction, so what it names and what the line carries cannot disagree.
//
// It is exhaustive by construction: every admitted in-workflow issue the three skips below do
// not account for gets a row, and waitOf names a kind for every one of them. A record it cannot
// explain is reported as WaitUnexplained rather than dropped, because dropping it is the defect
// this exists to close.
//
// An issue another decider has already named is left to it: one a Dispatch blocker holds where
// it stands (LineFacts.Held, rendered by blockers.Holds) and one whose own children go first
// (LineFacts.ChildWaits, rendered by ChildWaitRows). This answers only what nothing else does.
func Unplaced(issues []Issue, slots []Slot, line []LineEntry, facts LineFacts) []BlockedIssue {
	placed := make(map[string]struct{}, len(slots)+len(line))
	for _, slot := range slots {
		placed[slot.Issue] = struct{}{}
	}
	for _, entry := range line {
		placed[entry.Issue.Key] = struct{}{}
	}
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	// The same two reads the line makes of these records: the parent chain walked over the
	// records above the pass's own, so a parent already done is a gap in the chain rather than
	// the end of it (chainOf), and the set of live trees the ancestry hold is read against.
	//
	// The chain is also how this answers for a member of a tree whose root the pass's own read
	// leaves out. LiveIssues holds no finished issue, so a child stranded under a closed root
	// has no root in `issues` at all; the controller's notice reads exactly that list
	// (admit's Admission.blocked), and a lookup in `byKey` alone found no root and reported the
	// child nowhere, while the projection's test fixture, which carries the closed root
	// directly, hid the difference. ReadParents supplies that root (LineFacts.Parents), so the
	// holding root is resolved from the complete recorded ancestry and both callers answer
	// alike.
	chain := chainOf(byKey, facts.Parents)
	live := LiveRoots(issues, facts)

	// Nothing is reserved: a pass that places every issue it reads answers nothing at all, which
	// is the ordinary case, and a reservation the size of the project's records would be made on
	// every read of the state.
	var rows []BlockedIssue
	for _, issue := range issues {
		if issue.Phase != phase.Admitted || !InWorkflow(issue) {
			continue
		}
		if _, held := placed[issue.Key]; held {
			continue
		}
		// An issue a Dispatch blocker holds where it stands is named already, by the decider
		// that holds it (blockers.Holds, the judgement LineFacts.Held carries), and so is a
		// parent whose own children go first (ChildWaitRows). Naming a hold of our own beside
		// either would put a second holder on an issue whose wait the state answers.
		if _, blocked := facts.Held[issue.Key]; blocked {
			continue
		}
		if _, waits := facts.ChildWaits[issue.Key]; waits {
			continue
		}
		wait := waitOf(issue, chain, live, facts)
		rows = append(rows, BlockedIssue{Issue: issue.Key, Wait: &wait})
	}
	slices.SortFunc(rows, func(a, b BlockedIssue) int { return strings.Compare(a.Issue, b.Issue) })
	return rows
}

// waitOf is what holds issue where it stands, read in the order Waiting itself drops a
// candidate, so the hold reported is the decision the line actually made rather than another
// true sentence about the same record. chain is the pass's parent chain (chainOf), live its set
// of live trees (LiveRoots) and facts its own.
//
// A root a person has taken the legion label off is read first and not as a closed tree, which
// is what the live reading alone would call it: its tree is not live precisely because the label
// is off, and reporting a person's pause as a tree that closed would send a reader looking for a
// hand-over nobody owes.
func waitOf(issue Issue, chain map[string]Issue, live map[string]struct{}, facts LineFacts) Wait {
	root := claim.IsTreeRoot(issue.Key, issue.Tree)
	if root && !issue.HandedOver {
		return Wait{Kind: WaitUnlabelled}
	}
	if issue.AwaitingPerson() {
		return Wait{Kind: WaitAwaitingPerson, Issue: issue.Key}
	}
	// The ancestry hold is the queued-root loop's, and only a root records a chain to read it
	// over (Issue.Ancestry), so it is asked exactly where Waiting asks it.
	if QueuedRoot(issue, facts.Gates[issue.Key]) {
		if by := liveAncestor(issue, live); by != "" {
			return Wait{Kind: WaitUnreleased, Issue: by}
		}
	}
	treeRoot, recorded := chain[issue.Tree]
	if !recorded {
		return Wait{Kind: WaitTreeClosed, Issue: issue.Tree}
	}
	if treeRoot.AwaitingPerson() {
		return Wait{Kind: WaitAwaitingPerson, Issue: issue.Tree}
	}
	if _, running := live[issue.Tree]; !running || treeRoot.Lingers() {
		return Wait{Kind: WaitTreeClosed, Issue: issue.Tree}
	}
	if !facts.Gates[issue.Tree].Open() {
		return Wait{Kind: WaitDesignGate, Issue: issue.Tree}
	}
	if by := AcceptedAncestor(issue, chain); by != "" {
		return Wait{Kind: WaitAcceptedAncestor, Issue: by}
	}
	return Wait{Kind: WaitUnexplained, Issue: issue.Tree}
}

// liveAncestor is the live root issue's recorded Dispatch chain reaches, "" for a chain that
// reaches none: the one the ancestry hold keeps a root out of the line for (Waiting), read here
// over the same set of live trees so the hold and its name are one judgement. A root records a
// chain only while it has never run, so this answers nothing for any other record.
func liveAncestor(issue Issue, live map[string]struct{}) string {
	for _, key := range issue.Ancestry {
		if key == issue.Key {
			continue
		}
		if _, under := live[key]; under {
			return key
		}
	}
	return ""
}
