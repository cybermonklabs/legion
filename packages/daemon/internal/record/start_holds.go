package record

// StartHold says, for one issue of a project, why the daemon does not start it now: the one hold
// standing between that issue and a worker of its own, or that nothing holds it at all. It is a
// closed set, and the waiting line's own walk is the one producer of it (WaitingAndHolds):
// the pass that reads the project's issues, its slots and the facts under them to decide which
// issues the line offers a slot to is the pass that writes these, so a rule that must know why
// an issue stands still asks this set rather than reading the line and working the answer out
// again.
//
// One owner, because the same rule was read three ways inside one review. Why the daemon does
// not start an issue was answered from the start row an issue owes at one reading, from its
// place in the line at the next and from its need of a slot at the third, and each reading was
// right about the shape in front of it and missed a shape beside it that answers to the same
// cause (dispatch://INFRA-2560, R5). A second reader cannot make that mistake here: the walk
// that passes an issue over says why it passed it over, and every rule over it answers from the
// one word.
//
// Each value is named for the hold and never for the mechanism that carries it, because the
// mechanism is what drifted: an issue that needs a slot and holds none is held by the cap
// whether its start is written and waiting for the row, or not written at all until the
// promotion that grants the row writes it.
//
// What a hold means for a rule reading it is no part of this set. The measure of a stalled tree
// keeps a table of its own answers beside the rule that reads them (admit's silencedTree), with
// a test that fails where a hold of this set has no answer in that table, so a value added here
// is one that measure must decide about rather than one it can ignore.
//
// It is not Hold, which is a held issue's own record, nor HoldReason, which is why that issue was
// held: those name one phase of one issue, and this names why any issue of the project has no
// worker starting now.
//
// It is not the state's `why` either (api.WhyView, rendered by projection's whyStates), and the
// two are not one judgement read twice. This is the one statement of how they differ: the
// projection's call site and the arm that answers an issue past its review point here rather
// than arguing it over again. This set answers why the daemon has not started an issue; `why`
// answers why an issue is not working now, which is a question about the work and not about the
// start. The two part company at the phases past the review: since this change such an issue
// holds no admission slot, and while its tree runs nothing of the daemon's stands between it
// and a worker, so it is StartHoldNone here while that worker runs — and `why` gives it no
// answer at all, because it is working. They part the other way at a claim that died under a
// running phase: `why` names it, and this set still says StartHoldNone, because no start of the
// daemon's is being held. Where they do meet — the cap, a Dispatch link, an open child, a tree
// the daemon no longer runs — they say one thing in two registers, this word for the rules that
// read it and that sentence for the person reading the state, and neither is derived from the
// other: this comes out of the walk that builds the waiting line, and that out of the state the
// walk produced. A reader that must act on why no worker starts asks this; a reader rendering a
// sentence for a person asks that.
type StartHold string

const (
	// StartHoldNone is no hold at all: nothing the daemon knows of stands between this issue and
	// a worker of its own. An issue that already holds one of the cap's slots is in it, since
	// the row is what releases the start it owes; so is an issue whose phase keeps no slot
	// (KeepsSlot), one that needs one and owes its worker no start at all, a root whose tree the
	// daemon has already opened, and an issue the line holds a place for that holds its row
	// already, whose promotion takes no second one.
	//
	// It is the answer that says an issue standing still is standing still for no reason the
	// daemon has: a measure of stillness reading this must name the tree rather than excuse it,
	// which is the whole of what dispatch://INFRA-2560 was filed about.
	StartHoldNone StartHold = "none"
	// StartHoldPerson is an issue waiting on a person's answer: it stands in phase.AwaitingPerson,
	// where only a person's act moves it, and it keeps its slot while it waits (KeepsSlot, the
	// ruling on dispatch://INFRA-1909). Only a root ever stands there, answered or not: the
	// record stays in that phase until the promotion that resumes it moves the root on, so a root
	// whose answer has come back is still held by the wait it entered.
	StartHoldPerson StartHold = "person"
	// StartHoldRootPerson is an issue whose tree's root waits on a person: the daemon starts
	// nothing of a tree whose root has handed its work to somebody, and the member's own start
	// comes with that root's resume. The member stands in no line entry while it holds, so
	// nothing of the line names this hold.
	StartHoldRootPerson StartHold = "root_person"
	// StartHoldBlocker is an issue a Dispatch blocked-by link holds where it stands
	// (LineFacts.Held, the judgement blockers.Holds makes): it waits at admitted until the last
	// issue it names is done, and the line passes it over every pass until then.
	StartHoldBlocker StartHold = "blocker"
	// StartHoldChildren is an issue whose own first phase has not started while a Dispatch child
	// of it is still open (LineFacts.ChildWaits): its own head is meant to carry its children's
	// merges, so it waits for them. It is that judgement and never ChildrenFirstCandidates, which
	// names every issue whose first phase could start whether or not a child of it is open.
	StartHoldChildren StartHold = "children"
	// StartHoldDesign is an issue whose tree's design gate is not open (TreeGate.Open): the
	// tree's architect is writing or revising the design its work is meant to follow, and nothing
	// of the tree starts before a person approves it. The root of such a tree stands here beside
	// its members, because the gate holds its own first phase as well.
	StartHoldDesign StartHold = "design"
	// StartHoldCap is the cap's own hold: the issue stands in the waiting line and holds none of
	// the cap's slots (LineEntry.Holds), so the daemon starts it when a slot frees. Both shapes
	// of that wait are in it, and the start row tells them apart rather than the hold: an issue
	// whose work came back owes a start the outbox holds unclaimable while the slots table names
	// no row of it (SuperviseRequest.NeedsSlot), and an issue waiting for its first phase owes no
	// start at all, because the promotion that grants the slot is what writes it.
	//
	// What it never covers is an issue whose phase is already running. This hold is of a start —
	// one owed, one queued and one the daemon keeps out of the line — and every shape of it
	// stands before its phase. A member already in implementing has had its start, so nothing of
	// the daemon holds it and it needs no slot it does not hold: a member in a phase and not
	// moving is the stall this measure exists to report, not a wait by design
	// (dispatch://INFRA-2560, R5).
	StartHoldCap StartHold = "cap"
	// StartHoldAccepted is an issue beneath an ancestor the daemon has already accepted
	// (HeldUnderAccepted): it waits for that ancestor to close, because starting it would let the
	// ancestor merge work its own head does not carry.
	StartHoldAccepted StartHold = "accepted"
	// StartHoldUnreleased is a root recorded under a live tree that has not released it: the
	// ancestry hold. It is that tree's own unreleased work, which its architect's release, or its
	// parent's, brings into the tree.
	StartHoldUnreleased StartHold = "unreleased"
	// StartHoldTreeGone is an issue of a tree the daemon does not run: its root is in none of the
	// pass's records, its tree is not live (LiveRoots), or that root lingers after its close.
	// Nothing of such a tree is started again until its re-admission opens it.
	StartHoldTreeGone StartHold = "tree_gone"
	// StartHoldLeftWorkflow is an issue Dispatch no longer shows in the workflow (InWorkflow): a
	// person has parked, closed or iceboxed it, and the daemon starts nothing a person has taken
	// out of its hands.
	StartHoldLeftWorkflow StartHold = "left_workflow"
)

// StartHolds is every value of the set, in the order they are declared above. A reader that must
// cover all of them walks this rather than keeping a list of its own, and the test that holds
// the stalled-tree measure's table to the set reads the values this package's source declares
// and requires this list to be exactly those (admit's
// TestTheStartHoldListIsWhatTheSourceDeclares), so a value added above and left out here fails
// rather than passing quietly.
var StartHolds = []StartHold{
	StartHoldNone,
	StartHoldPerson,
	StartHoldRootPerson,
	StartHoldBlocker,
	StartHoldChildren,
	StartHoldDesign,
	StartHoldCap,
	StartHoldAccepted,
	StartHoldUnreleased,
	StartHoldTreeGone,
	StartHoldLeftWorkflow,
}

// whyNoSlot is the hold of an issue the line passes over because it needs none of the cap's slots
// where it stands (NeedsSlot answering false, or its tree root being in none of the records). It
// splits that one rule's own terms, in its order, and is read nowhere else, so the two move
// together: the tree must run — a recorded root, live and not lingering after its close — and the
// issue must be one Dispatch still shows in the workflow, standing in a phase that keeps a row.
// liveTree is whether the issue's tree root is recorded and live.
func whyNoSlot(issue, root Issue, liveTree bool) StartHold {
	switch {
	case !liveTree || root.Lingers():
		return StartHoldTreeGone
	case !InWorkflow(issue):
		return StartHoldLeftWorkflow
	default:
		return StartHoldNone
	}
}

// lineHold is the hold of an issue the line holds a place for: the cap's own where the issue
// holds none of its slots, and nothing at all where it already holds one, because such a
// candidate is promoted without taking a second row (LineEntry.Holds).
func lineHold(holdsSlot bool) StartHold {
	if holdsSlot {
		return StartHoldNone
	}
	return StartHoldCap
}
