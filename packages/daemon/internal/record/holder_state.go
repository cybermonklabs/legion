package record

import "time"

// HolderState is what one holder of an admission slot is doing with it: at work, stopped,
// waiting on a person, waiting on another issue, or idle. The slots reading counts the holders by
// it (SlotsReading) and each holder's slot in the state carries it (api.SlotView.State), so a
// reader of `legion state` sees what the slots in use are being used for rather than only how
// many there are. A holder is in exactly one state, the first of HolderStates that holds for it
// (HolderStateOf).
type HolderState string

const (
	// HolderWorking is a holder whose work is under way: a claim of it in a turn, a start of one
	// of its roles queued (an outbox start row, or a claim not yet ready), or a task queued to a
	// claim of it that no turn has taken yet.
	HolderWorking HolderState = "working"
	// HolderStopped is a holder whose work has stopped: its phase is held, or a claim of it is one
	// nothing will wake (claim_stopped in its why).
	HolderStopped HolderState = "stopped"
	// HolderPerson is a tree root waiting on a person, on an ask of its architect or on its
	// design gate (awaiting_person).
	HolderPerson HolderState = "person"
	// HolderIssue is a holder that waits on another issue, an unfinished Dispatch blocker or an
	// open child, and keeps its slot through that wait under the slot rule it is read under.
	HolderIssue HolderState = "issue"
	// HolderIdle is a holder none of the four above is true of: its turns ended, or none began.
	HolderIdle HolderState = "idle"
)

// HolderStates is the precedence of the holder states: a holder is in the first state of this
// list that holds for it. A holder in a turn that also waits on an issue is working, and so is a
// held holder whose claim is in a turn, because the turn is the work the slot is spent on.
var HolderStates = []HolderState{HolderWorking, HolderStopped, HolderPerson, HolderIssue, HolderIdle}

// HolderFacts is everything one slot holder's state is read from: its record, its slot, and what
// its claims, its outbox rows, its hold, its wait on a person and its wait on another issue say,
// each with the time it began where the record holds one. The admission pass (for the tick
// notice), the state projection and a replay each gather these from their own reads and hand them
// to HolderStateOf, so the three name a holder's state by one rule.
type HolderFacts struct {
	// Issue is the holder's record.
	Issue Issue
	// AdmittedAt is when the holder took its slot (Slot.AdmittedAt).
	AdmittedAt time.Time
	// InTurn is the lastTurnStart of each claim of the holder in a turn (claim state working).
	InTurn []time.Time
	// Starting is the creation time of each start row the outbox holds for a role of the holder,
	// and the time each claim of it not yet ready (queued, launching, launch_uncertain,
	// shim_connected, registered) was queued its task.
	Starting []time.Time
	// PendingTasks is the queued time of each task a claim of the holder holds that no turn has
	// taken yet (the claim's pending task), for a claim in no turn.
	PendingTasks []time.Time
	// HeldSince is when the holder's hold began, for a holder in phase held; nil where the record
	// holds no such time.
	HeldSince *time.Time
	// Stranded says a claim of the holder is one nothing will wake (claim_stopped in its why,
	// workflow.Stranded), and StoppedAt when that claim stopped; nil where the record holds no
	// such time.
	Stranded  bool
	StoppedAt *time.Time
	// Person says the holder is a tree root waiting on a person, on an ask or on its design gate
	// (awaiting_person), and PersonSince where that wait began (Awaiting.Since).
	Person      bool
	PersonSince *time.Time
	// WaitsOnIssue says the holder waits on another issue and keeps its slot through the wait
	// under the slot rule it is read under, and WaitSince when that wait began; nil where the
	// record holds no such time.
	WaitsOnIssue bool
	WaitSince    *time.Time
	// LastTurnEnd is the newest lastTurnEnd of the holder's claims, nil where no turn of them
	// has ended.
	LastTurnEnd *time.Time
}

// HolderStateOf is the state of the holder f describes, the first of HolderStates that holds for
// it, and the time it entered that state: for working, the oldest lastTurnStart of its claims in
// a turn, or else the oldest start queued, or else the oldest task pending; for stopped, when its
// hold began or its claim stopped; for person, where the wait began; for issue, when the wait
// began; for idle, its newest lastTurnEnd, or its slot's admittedAt where no turn has ended. The
// time is nil where the record holds none for the state, which a state begun before the record
// kept its time is.
func HolderStateOf(f HolderFacts) (HolderState, *time.Time) {
	return "", nil
}
