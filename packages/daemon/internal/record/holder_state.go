package record

import (
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

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
//
// Idle is timed from the later of the newest lastTurnEnd and the slot's admittedAt: a turn that
// ended before the holder took the slot it holds now, one of a run the holder had before a
// re-admission, is no idle time of this slot's.
func HolderStateOf(f HolderFacts) (HolderState, *time.Time) {
	switch {
	case len(f.InTurn) > 0:
		return HolderWorking, oldestTime(f.InTurn)
	case len(f.Starting) > 0:
		return HolderWorking, oldestTime(f.Starting)
	case len(f.PendingTasks) > 0:
		return HolderWorking, oldestTime(f.PendingTasks)
	case f.Issue.Phase == phase.Held:
		return HolderStopped, f.HeldSince
	case f.Stranded:
		return HolderStopped, f.StoppedAt
	case f.Person:
		return HolderPerson, f.PersonSince
	case f.WaitsOnIssue:
		return HolderIssue, f.WaitSince
	}
	since := KnownTime(f.AdmittedAt)
	if f.LastTurnEnd != nil && (since == nil || f.LastTurnEnd.After(*since)) {
		since = KnownTime(*f.LastTurnEnd)
	}
	return HolderIdle, since
}

// oldestTime is the oldest of times, nil where none of them is known (the zero time is a time
// the record does not hold).
func oldestTime(times []time.Time) *time.Time {
	var oldest *time.Time
	for _, at := range times {
		if at.IsZero() {
			continue
		}
		if oldest == nil || at.Before(*oldest) {
			oldest = KnownTime(at)
		}
	}
	return oldest
}

// KnownTime is at in UTC, as every time of the slots reading is written, or nil for the zero
// time, which is a time the record does not hold.
func KnownTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	utc := at.UTC()
	return &utc
}

// HolderClaim is one role claim as a slot holder's state reads it: whose claim it is, the state
// it stands in and the times that state is dated by. The admission pass reads these from the
// store's own narrow read (Store.HolderClaims) for the tick notice, and the state's projection
// builds them from the claims it already holds (HolderClaimsOf) unless its caller read them
// through that same statement, so the two read a holder from one shape. A time the record does
// not hold is the zero time.
type HolderClaim struct {
	Token claim.Token
	Issue string
	Role  claim.Role
	Tree  string
	State supervise.ClaimState
	// SuspendedBy is who suspended the claim while it is suspended, which the stranded judgement
	// reads (workflow.Stranded).
	SuspendedBy supervise.SuspendSource
	// LastTurnStart and LastTurnEnd are when its agent last began and last ended a turn
	// (supervise.Claim's own two).
	LastTurnStart time.Time
	LastTurnEnd   time.Time
	// QueuedAt is when the task the claim was last queued was queued to it: its pending task's
	// queued time where it holds one, and otherwise its handshake's, the first queue of the last
	// entry it was queued a task of (supervise.Claim.Handshake). It is what dates a start of a
	// claim not yet ready and a task no turn of it has taken.
	QueuedAt time.Time
	// Pending says the claim holds a task no turn has confirmed (supervise.Claim.Pending).
	Pending bool
	// StoppedAt is when the claim stopped: when it failed (supervise.Claim.FailedAt) for a
	// failed claim, and the moment the store recorded its move into suspended or retired
	// (claims.stopped_at) for the other two. The claims a caller holds whole carry no record of
	// the second, so HolderClaimsOf gives it for a failed claim alone.
	StoppedAt time.Time
}

// HolderClaimsOf is claims as a slot holder's state reads them, for a caller that already holds
// whole claims: the state's projection, given every claim of its snapshot (projection.Project).
// A claim on no issue, the project's controller's, is read too and matches no holder.
func HolderClaimsOf(claims []supervise.Claim) []HolderClaim {
	read := make([]HolderClaim, 0, len(claims))
	for _, c := range claims {
		held := HolderClaim{
			Token: c.Token, Issue: c.Issue, Role: c.Role, Tree: c.Tree, State: c.State, SuspendedBy: c.SuspendedBy,
			LastTurnStart: c.LastTurnStart, LastTurnEnd: c.LastTurnEnd, QueuedAt: c.Handshake.QueuedAt,
			Pending: c.Pending != nil,
		}
		if c.Pending != nil {
			held.QueuedAt = c.Pending.QueuedAt
		}
		if c.State == supervise.StateFailed {
			held.StoppedAt = c.FailedAt
		}
		read = append(read, held)
	}
	return read
}

// startingStates are the claim states of a start under way that has not reached its agent's
// ready: what HolderFacts.Starting dates a claim by.
var startingStates = []supervise.ClaimState{
	supervise.StateQueued, supervise.StateLaunching, supervise.StateLaunchUncertain,
	supervise.StateShimConnected, supervise.StateRegistered,
}

// readyStates are the two states of a claim whose agent is up and in no turn, where a task it
// holds is one no turn has taken yet (HolderFacts.PendingTasks). A suspended, failed or retired
// claim holding a task is no work under way: what stops it is its own stop.
var readyStates = []supervise.ClaimState{supervise.StateReady, supervise.StateIdle}

// HolderClaimFacts sets on facts what the claims of its holder say: the claims in a turn, the
// starts under way, the tasks no turn has taken, the newest turn's end, and, where stranded names
// a claim of the holder, that it is stranded and the oldest time one of those claims stopped.
// claims are claims the caller read, of any issue (the slots reading hands in the holder's own
// alone); the holder's own are those on its issue.
func HolderClaimFacts(facts *HolderFacts, claims []HolderClaim, stranded map[claim.Token]bool) {
	var stops []time.Time
	for _, c := range claims {
		if c.Issue != facts.Issue.Key {
			continue
		}
		switch {
		case c.State == supervise.StateWorking:
			facts.InTurn = append(facts.InTurn, c.LastTurnStart)
		case slices.Contains(startingStates, c.State):
			facts.Starting = append(facts.Starting, c.QueuedAt)
		case c.Pending && slices.Contains(readyStates, c.State):
			facts.PendingTasks = append(facts.PendingTasks, c.QueuedAt)
		}
		if !c.LastTurnEnd.IsZero() && (facts.LastTurnEnd == nil || c.LastTurnEnd.After(*facts.LastTurnEnd)) {
			facts.LastTurnEnd = KnownTime(c.LastTurnEnd)
		}
		if stranded[c.Token] {
			facts.Stranded = true
			stops = append(stops, c.StoppedAt)
		}
	}
	if facts.Stranded {
		facts.StoppedAt = oldestTime(stops)
	}
}
