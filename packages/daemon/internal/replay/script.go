package replay

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// inFlightStep is how many more issues in flight multiply a work segment's length by the slowdown
// factor once (spec R3): a segment that starts with n issues in flight in a replay and m in the
// record takes its recorded length times f^((n − m)/inFlightStep).
const inFlightStep = 42

// slowed is a work segment's length in a replay (spec R3, assumption 1): its recorded length times
// f to the power (n − m)/42, n the issues in flight in the replay at the segment's start and m in
// the record at the record's start of it. At n = m, or at f = 1, it keeps its recorded length.
func slowed(recorded time.Duration, f float64, n, m int) time.Duration {
	if n == m || f == 1 {
		return recorded
	}
	return time.Duration(math.Round(float64(recorded) * math.Pow(f, float64(n-m)/inFlightStep)))
}

// pieceOpen is what opens a piece of an issue's work in a replay.
type pieceOpen int

const (
	// opensOnStart is a piece the start row of its phase's role opens: the stub takes a start
	// whose entry is above the one its claim was given.
	opensOnStart pieceOpen = iota
	// opensOnRetry is the rest of a visit after a hold: the start the hold's release writes
	// (workflow's restart, a resend of the entry) opens it.
	opensOnRetry
	// opensOnPhase is a piece no start row opens: accepting's, its phase's role being the
	// architect that owns the issue (design 10, "the accepting segment"), and that of a workerless
	// visit (Visit.Workerless), where the record shows no start of its role. The move into its
	// phase opens it.
	opensOnPhase
	// opensAtSeed is a piece under way at the window's start, which the start state's claims
	// stand in: it runs at its recorded times.
	opensAtSeed
)

// openingOf is how the first piece of the work of a visit to ph opens: on the move into ph where no
// start row of its role opens it (opensOnPhase), as accepting's and a workerless visit's do, and
// otherwise on its role's start (opensOnStart). visit is nil for a piece that no visit stands
// behind. It is the one rule that tells the two apart (issuePieces, clonePieces).
func openingOf(ph phase.Phase, visit *Visit) pieceOpen {
	if ph == phase.Accepting || visit != nil && visit.Workerless {
		return opensOnPhase
	}
	return opensOnStart
}

// pieceEnd is how a piece of work ends.
type pieceEnd int

const (
	// endMove is a piece the recorded move of its visit ends, which the replay enters at its end
	// (MoveSources): the move that ended the visit, or the merge at which the segment of a visit
	// to merging ended where the issue's close waited for the issues beneath it
	// (SegmentEndHeldMerge, piece.move).
	endMove pieceEnd = iota
	// endHeldSignOff is an accepting piece whose sign-off the engine held behind a member beneath
	// (SegmentEndHeldSignOff): the sign-off is entered at its end, and again at each leave of a
	// member beneath while it is refused.
	endHeldSignOff
	// endFailure is a piece its role's failed claim ends: the replay enters the claim's failure
	// (intake.ClaimFailed) at its end, which holds the issue.
	endFailure
	// endPass is a piece whose visit the admission pass ended (gate_opened): its worker's work
	// ended with its registration or its last turn, and the replay enters no move.
	endPass
	// endLeft is a piece the issue's leaving the workflow cut, and endOpen one the record shows
	// unended at the window's end; neither has an end the replay enters.
	endLeft
	endOpen
	// endMergeForward is the merge-forward segment a conflict adds where the daemon keeps the
	// issue's phase and asks a role to merge the base forward (record.ConflictMergeForward and
	// record.ConflictImplementerMergesForward, or under main's path a conflict past the review that
	// waited for merging; assumptions 23 and 37): at its end the merge forward lands, and the work
	// of the stretch that drew goes on (driver.mergedForward).
	endMergeForward
)

// pieceState is where a piece stands in a replay.
type pieceState int

const (
	pieceWaiting pieceState = iota
	pieceRunning
	piecePaused
	pieceEnded
	pieceCancelled
	pieceSkipped
)

// piece is one stretch of an issue's work as the record shows it and as a replay runs it: a visit's
// work segment (Visit.Segment), or the part of one before or after a hold, with every act of the
// issue's workers that falls in it at its offset from its start.
type piece struct {
	issue string
	index int
	visit *Visit
	phase phase.Phase
	role  claim.Role
	opens pieceOpen
	// start and end are the record's, end zero for a piece the record shows unended; ends is how
	// it ends, and hold the hold whose claim failure ends it (endFailure) or whose release opens
	// it (opensOnRetry).
	start, end time.Time
	ends       pieceEnd
	hold       *Wait
	// m is the issues in flight in the record at its start.
	m int
	// cut is how much of its recorded length the branch takes off its work: under AfterSendBack
	// less-slot-wait, the recorded wait for a slot before the first piece of a visit a send-back
	// began (assumption 13).
	cut time.Duration
	// drawn says the piece is another recorded piece's work given to this issue by a draw: the
	// never-started scenario's work, or a conflict loop drawn from the window's loops. Its moves
	// are entered at the generation the issue stands at, its registration names the issue's own
	// document, and the Dispatch events of the work it copies stay the copied issue's.
	drawn bool
	// leftUnrun says the piece is work of a generation its issue left in the replay before the
	// piece opened (driver.cancelWork): the replay never ran that generation's piece in it, so the
	// piece runs at the generation the issue stands at when it opens, and its move is entered at
	// that generation, as a drawn piece's is (moveGeneration).
	leftUnrun bool
	// drew is, for a merge-forward piece, the piece whose stretch drew the conflict, and resumes
	// says that piece stands paused by a conflict a boot's read found while it ran, which the merge
	// forward's end resumes (mergedForward). Whether a stretch drew is its live stretch's
	// (liveStretch), not a piece's.
	drew    *piece
	resumes bool
	// acts is every act of the piece, by its offset from the start.
	acts []act

	// The replay's own: where the piece stands, when it started, its length, when it was paused,
	// and the items it scheduled that have not run.
	state    pieceState
	at       time.Time
	n        int
	length   time.Duration
	pausedAt time.Time
	pending  []*item
	// turns is each role's turn under way in the piece, which a pause ends and a resume begins
	// again.
	turns map[claim.Role]bool
}

// recorded is the piece's recorded length, zero for one the record shows unended.
func (p *piece) recorded() time.Duration {
	if p.end.IsZero() {
		return 0
	}
	return p.end.Sub(p.start)
}

// move is the recorded move p's end enters where it ends by one (endMove): its visit's work end
// (Visit.workEnd), the merge where its segment ended at it, and otherwise the move that ended its
// visit; nil for none.
func (p *piece) move() *Move {
	if p.visit == nil {
		return nil
	}
	return p.visit.workEnd()
}

// worked is the length of the piece's work in a replay before the slowdown: its recorded length
// less what the branch takes off it (cut, never below zero), and zero for one the record shows
// unended, whose recorded length is zero.
func (p *piece) worked() time.Duration {
	return max(0, p.recorded()-p.cut)
}

// actKind is one kind of act inside a piece.
type actKind int

const (
	actTurnStart actKind = iota
	actTurnEnd
	// actEvent is a Dispatch event on the issue, or the filing of a child under it, that a worker
	// of the issue made (its session the actor) inside a piece of its role's work (script.workPiece),
	// which moves with that work: a child filed, a document's version written, an ask opened, a link
	// of the issue's own set. An event of the worker's on another issue is none (placeEvents).
	actEvent
	// actRegistration is the architect's registration of a design document
	// (intake.GateRegistered, from its `workflow: design gate registered` line).
	actRegistration
	// actClaimFailed is the failure of a claim of the issue whose role does not work the phase
	// it stands in, which holds nothing and tells its architect.
	actClaimFailed
	// actAdoption is the architect's release of queued roots into its tree (intake.ChildAdoption).
	actAdoption
	// actCloseRoot is the architect's close of its root before the first phase
	// (intake.CloseRoot).
	actCloseRoot
	// actReady is the merger's ready mark of the release at 9542c6fb inside merging
	// (ReadyMark, assumption 20).
	actReady
)

// act is one act inside a piece: its kind, its offset from the piece's start, the record's moment
// of it, its order among the acts of one moment (1 for an act a line holds, so it runs after a
// turn or an event of the same moment, and 0 for every other), and what it is of.
type act struct {
	kind   actKind
	offset time.Duration
	rec    time.Time
	order  int
	role   claim.Role
	event  int
	gate   GateFact
	failed ClaimFailed
	joined []RootJoined
}

// script is a replay's reading of the record's work: each issue's pieces in order, every
// recorded Dispatch event with the anchor that places it, and the record's issues in flight at any
// moment. reach is each issue's pieces that ended before the window's start, in order, with their
// turns: no replay runs them, and the conflict loops among them stand in the pool a drawn
// conflict's work is drawn from (recordedLoops).
type script struct {
	pieces map[string][]*piece
	reach  map[string][]*piece
	events []scriptEvent
	// flight is each issue's stretches in flight in the record (IssueTrace.inFlight), and inFlight
	// the count of them at any moment, each issue's own joined first (joinedStretches), so an
	// issue counts once.
	flight   map[string][][2]time.Time
	inFlight stretchCount
}

// anchorKind is what places an event of the record in a replay where no piece of work does
// (scriptEvent.piece).
type anchorKind int

const (
	// anchorTime places the event at its recorded time.
	anchorTime anchorKind = iota
	// anchorEvent places it at its offset from another event the replay places first: a
	// person's answer from the question it answers (Branch.Answer).
	anchorEvent
	// anchorWait places it at its offset from the replay's start of the wait it ended in the
	// record (Branch.Timing).
	anchorWait
	// anchorFollow places an event at its offset from another event the replay applies first
	// (scriptEvent.on), once that is applied, or at its recorded time where that comes later
	// (driver.placeFollowers). An architect's act on a child it filed follows that filing, so the
	// child's course follows its filing wherever the replay places it (spec R2). A write of the
	// daemon's own session follows the event before it on its issue (scriptEvent.prev), where that
	// event is the issue's filing as a child, an act of a piece of its parent's work that moves with
	// that piece, an event an issue's start places (anchorStart), or itself follows one: an
	// architect's own write on a child and its API's write on the child that Dispatch records under
	// the daemon's session, a release or a park, keep the order the record shows them in.
	anchorFollow
	// anchorStart places an event that depends on an issue's first start in its run, where no piece
	// of work places it, at its offset from the record's first start of that issue (runStartAt), from
	// the replay's first start of the issue in its run, or at its recorded time where that is later
	// (driver.placeStarted): the filing of a child under the issue, whoever files it (filedUnder),
	// and an act of the issue's own architect on it, each made while the record shows the issue in
	// flight in that run (spec R2: a child's filing enters at its recorded offset from the start of
	// the issue it is filed under, and a fact that depends on a start moves with that start). A
	// child's open status holds the issue's first start (the record's children wait), and so does a
	// link the architect sets on the issue, so either applied before the replay starts the issue
	// would hold a start the record made before it.
	anchorStart
)

// scriptEvent is one recorded Dispatch event, read once (ParseDispatchEvent, and its envelope as
// the replay rewrites it), and how the replay places it.
type scriptEvent struct {
	parsed DispatchEvent
	env    envelope
	at     time.Time
	order  int
	// piece is the piece of work the event is an act of (actEvent), nil for none: the replay places
	// it where that piece runs it, and by its anchor where the piece does not run it (placeUnrun).
	piece  *piece
	anchor anchorKind
	// on is the event an anchorEvent or an anchorFollow is placed from, and offset the event's own
	// distance from it; wait is the wait an anchorWait ended in the record; startOf is the issue
	// whose first start an anchorStart is placed from, offset its distance from that start.
	on      int
	offset  time.Duration
	wait    waitAnchor
	startOf string
	// dependents is every person's answer placed from this one, and followers every event placed
	// from it (anchorFollow).
	dependents []int
	followers  []int
	// filing is the filing of the event's issue (its issue.created) where the window holds it from
	// its start on and the record shows it before this event, -1 for none and for the filing itself:
	// the replay applies no event of an issue before the issue's filing (driver.awaitsFiling).
	// awaiting is, for a filing, each event of its issue the replay held back for it, which it
	// places once it is applied (driver.placeFiled).
	filing   int
	awaiting []int
	// prev is the issue event before this one on its issue, in the record's order, -1 for none or
	// for an event that is no issue event: what a write of the daemon's own session is judged
	// against (driver.keepsDaemonEvent), and what an issue event's change is read against
	// (shadow.take).
	prev int
	// dispatched is whether the replay has applied the event to its copy of Dispatch; where the
	// replay placed it is driver.placed's.
	dispatched bool
	// releasesHold says the event is the daemon session's todo on a held root that the record's
	// release of the hold names as its cause (markHeldRootTodos).
	releasesHold bool
}

// waitAnchor is the wait an event ended in the record: its waiting issue, the kind, and the
// record's start of it; the replay's start of the same wait places the event (Timing offset).
type waitAnchor struct {
	issue string
	kind  WaitKind
	start time.Time
	// added is the event that added the link a link wait stands on, and blocker the issue it
	// names.
	added   int
	blocker string
}

// inFlightAt is how many issues the record holds in flight at at.
func (s *script) inFlightAt(at time.Time) int {
	return s.inFlight.at(at)
}

// joinedStretches is the moments stretches hold, as withinStretches reads them, as stretches that
// neither meet nor overlap, in order: a stretch that holds no moment is left out, and stretches
// that meet or overlap are joined into one.
func joinedStretches(stretches [][2]time.Time) [][2]time.Time {
	var held [][2]time.Time
	for _, stretch := range stretches {
		if stretch[1].IsZero() || stretch[0].Before(stretch[1]) {
			held = append(held, stretch)
		}
	}
	slices.SortFunc(held, func(x, y [2]time.Time) int { return x[0].Compare(y[0]) })
	var joined [][2]time.Time
	for _, stretch := range held {
		n := len(joined)
		if n == 0 || !joined[n-1][1].IsZero() && stretch[0].After(joined[n-1][1]) {
			joined = append(joined, stretch)
			continue
		}
		if end := joined[n-1][1]; !end.IsZero() && (stretch[1].IsZero() || stretch[1].After(end)) {
			joined[n-1][1] = stretch[1]
		}
	}
	return joined
}

// buildScript reads t into the pieces of its issues' work and the placement of its events, the
// window's Dispatch events in the order the trace read them (readTrace), from the window's start
// on, and the pieces that ended in the reach before it, with their turns alone (script.reach). A
// piece's acts run by offset, then by the record's moment, then by order (act.order): of one
// instant, an act a line holds runs after a turn or an event, and acts of the same order keep the
// order they were placed in.
func buildScript(t Trace, events []readEvent, lines recordLines) (script, error) {
	s := script{pieces: map[string][]*piece{}, reach: map[string][]*piece{}, flight: map[string][][2]time.Time{}}
	var flying [][2]time.Time
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		s.flight[key] = iss.inFlight()
		flying = append(flying, joinedStretches(s.flight[key])...)
		if iss.Project != t.Project {
			continue
		}
		s.reach[key], s.pieces[key] = issuePieces(t, iss)
	}
	s.inFlight = countStretches(flying)
	for _, all := range []map[string][]*piece{s.reach, s.pieces} {
		for _, pieces := range all {
			for _, p := range pieces {
				p.m = s.inFlightAt(p.start)
			}
		}
	}
	s.placeTurns(t)
	s.placeLines(t, lines)
	issueEvents, err := s.placeEvents(t, events)
	if err != nil {
		return script{}, err
	}
	s.markHeldRootTodos(lines, issueEvents)
	for _, all := range []map[string][]*piece{s.reach, s.pieces} {
		for _, pieces := range all {
			for _, p := range pieces {
				slices.SortStableFunc(p.acts, func(x, y act) int {
					return cmp.Or(cmp.Compare(x.offset, y.offset), x.rec.Compare(y.rec), cmp.Compare(x.order, y.order))
				})
			}
		}
	}
	return s, nil
}

// issuePieces is iss's pieces of work in order, those that ended before the window's start (the
// reach's) apart from those that end at or after it: each visit's segment, cut at each hold of its
// role's claim inside it. The rest of a segment after a hold the record shows released
// (Wait.released) is a piece of its own, opened by the retry that releases it; a hold the record
// shows going on past the segment's end leaves that piece none of the segment's length. Each list
// numbers its pieces from 0.
func issuePieces(t Trace, iss *IssueTrace) (reach, kept []*piece) {
	var pieces []*piece
	for i := range iss.Visits {
		visit := &iss.Visits[i]
		segment := visit.Segment
		if segment == nil {
			continue
		}
		role := phaseRole(visit.Phase)
		opens := openingOf(visit.Phase, visit)
		until := segment.End
		if until.IsZero() {
			until = visit.Exit
		}
		start := segment.Start
		var holds []*Wait
		for j := range iss.Waits {
			hold := &iss.Waits[j]
			if hold.Kind == WaitHeld && !hold.Start.Before(start) && (until.IsZero() || hold.Start.Before(until)) {
				holds = append(holds, hold)
			}
		}
		released := true
		for _, hold := range holds {
			pieces = append(pieces, &piece{issue: iss.Key, visit: visit, phase: visit.Phase, role: role, opens: opens, start: start,
				end: hold.Start, ends: endFailure, hold: hold})
			if !hold.released() {
				released = false
				break
			}
			start, opens = hold.End, opensOnRetry
			if !until.IsZero() && start.After(until) {
				start = until
			}
		}
		if !released {
			continue
		}
		last := &piece{issue: iss.Key, visit: visit, phase: visit.Phase, role: role, opens: opens, start: start, end: segment.End}
		if len(holds) > 0 {
			last.hold = holds[len(holds)-1]
		}
		switch segment.EndedBy {
		case SegmentEndMove, SegmentEndHeldMerge:
			last.ends = endMove
		case SegmentEndHeldSignOff:
			last.ends = endHeldSignOff
		case SegmentEndRegistration, SegmentEndTurn:
			last.ends = endPass
		case SegmentEndLeft:
			last.ends, last.end = endLeft, time.Time{}
		default:
			last.ends, last.end = endOpen, time.Time{}
		}
		pieces = append(pieces, last)
	}
	for _, p := range pieces {
		if !p.end.IsZero() && p.end.Before(t.From) {
			p.index = len(reach)
			reach = append(reach, p)
			continue
		}
		if p.start.Before(t.From) {
			p.opens = opensAtSeed
		}
		p.index = len(kept)
		kept = append(kept, p)
	}
	return reach, kept
}

// pieceAt is the piece of key's work an act at at belongs to: the latest whose start is before
// it, or, where none is, the first that starts at it; with starting set, the latest that starts
// at or before it, as a turn that begins as a piece begins is that piece's. Nil for none.
func (s *script) pieceAt(key string, at time.Time, starting bool) *piece {
	return pieceIn(s.pieces[key], at, starting)
}

// pieceIn is the piece of pieces, one issue's work in order, an act at at belongs to (pieceAt).
func pieceIn(pieces []*piece, at time.Time, starting bool) *piece {
	var found *piece
	for _, p := range pieces {
		if p.start.Before(at) || starting && p.start.Equal(at) {
			found = p
		}
	}
	if found == nil {
		for _, p := range pieces {
			if p.start.Equal(at) {
				return p
			}
		}
	}
	return found
}

// workPiece is the piece of the work of ran, a claim of an issue, that an act of its worker at at
// falls in: of the pieces of ran's role, the latest whose start is before at, or, where none is,
// the first that starts at it, where the record shows that piece under way at at, at or before its
// end or, for a piece the record shows unended, its visit's exit. Nil for none: the worker acted
// outside every piece of its role's work, as a root's architect acts between its root's phases,
// where no start of its own opens a piece.
func (s *script) workPiece(ran SessionClaim, at time.Time) *piece {
	var found *piece
	for _, p := range s.pieces[ran.Issue] {
		if p.role == ran.Role && (p.start.Before(at) || found == nil && p.start.Equal(at)) {
			found = p
		}
	}
	if found == nil {
		return nil
	}
	end := found.end
	if end.IsZero() && found.visit != nil {
		end = found.visit.Exit
	}
	if !end.IsZero() && at.After(end) {
		return nil
	}
	return found
}

// placeTurns puts each turn of each issue's workers into the piece it falls in, its start and its
// end at their offsets from the piece's start: a turn that ended before the window's start into a
// piece of the reach (script.reach), and every other into a piece of the window's work.
func (s *script) placeTurns(t Trace) {
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		for i := range iss.Turns {
			turn := &iss.Turns[i]
			pieces := s.pieces[key]
			if !turn.End.IsZero() && turn.End.Before(t.From) {
				pieces = s.reach[key]
			}
			p := pieceIn(pieces, turn.Start, true)
			if p == nil {
				continue
			}
			p.acts = append(p.acts, act{kind: actTurnStart, offset: turn.Start.Sub(p.start), rec: turn.Start, role: turn.Claim.Role})
			if !turn.End.IsZero() {
				p.acts = append(p.acts, act{kind: actTurnEnd, offset: turn.End.Sub(p.start), rec: turn.End, role: turn.Claim.Role})
			}
		}
	}
}

// placeLines puts each recorded act of an architect or a worker that a line of the record holds
// into the piece of its issue it falls in (placeLineAct): each registration of a design document,
// each failure of a claim that holds nothing, each release of queued roots into a tree, and each
// close of a root by its architect.
func (s *script) placeLines(t Trace, lines recordLines) {
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		for _, gate := range t.Issues[key].Gates {
			if gate.Kind != GateRegisteredFact || gate.At.Before(t.From) {
				continue
			}
			s.placeLineAct(key, gate.At, act{kind: actRegistration, gate: gate})
		}
	}
	for _, failed := range lines.failed {
		key := failed.Claim.Issue
		if failed.Time.Before(t.From) || key == "" || s.endsAPiece(key, failed) {
			continue
		}
		s.placeLineAct(key, failed.Time, act{kind: actClaimFailed, failed: failed})
	}
	joined := map[string]map[time.Time][]RootJoined{}
	for _, line := range lines.joined {
		if line.Time.Before(t.From) {
			continue
		}
		if joined[line.Tree] == nil {
			joined[line.Tree] = map[time.Time][]RootJoined{}
		}
		joined[line.Tree][line.Time] = append(joined[line.Tree][line.Time], line)
	}
	for _, tree := range slices.Sorted(maps.Keys(joined)) {
		for _, at := range slices.SortedFunc(maps.Keys(joined[tree]), time.Time.Compare) {
			s.placeLineAct(tree, at, act{kind: actAdoption, joined: joined[tree][at]})
		}
	}
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		for _, at := range t.Issues[key].Ready {
			if at.Before(t.From) {
				continue
			}
			s.placeLineAct(key, at, act{kind: actReady})
		}
	}
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		for _, leave := range iss.Leaves {
			if leave.At.Before(t.From) || leave.Status != record.DoneStatus || !closedByArchitect(iss, leave) {
				continue
			}
			s.placeLineAct(key, leave.At, act{kind: actCloseRoot})
		}
	}
}

// placeLineAct puts a, an act a line of the record holds at at, into the piece of key's work it
// falls in (pieceAt), at its offset from the piece's start and of order 1, so it runs after a turn
// or an event of the same moment; nowhere where key's work holds no such piece.
func (s *script) placeLineAct(key string, at time.Time, a act) {
	if p := s.pieceAt(key, at, false); p != nil {
		a.offset, a.rec, a.order = at.Sub(p.start), at, 1
		p.acts = append(p.acts, a)
	}
}

// endsAPiece says whether failed, a claim failure of key, is the one that ends a piece of key's
// work (endFailure), which the piece's end enters rather than an act of it.
func (s *script) endsAPiece(key string, failed ClaimFailed) bool {
	return slices.ContainsFunc(s.pieces[key], func(p *piece) bool {
		return p.ends == endFailure && p.end.Equal(failed.Time) && p.role == failed.Claim.Role
	})
}

// closedByArchitect says whether leave, iss's leaving the workflow at done, is its architect's
// close of the root before its first phase (close_root): a root that leaves from admitted or from
// a wait on a person with no move to done, and with no event of a person or a session of its
// own making it done at that moment, which the daemon's own close writes instead.
func closedByArchitect(iss *IssueTrace, leave Leave) bool {
	entry := iss.entryAt(leave.At)
	if entry == nil || entry.Tree != iss.Key {
		return false
	}
	for _, visit := range iss.Visits {
		if visit.Exit.Equal(leave.At) && (!visit.Left || visit.Phase != phase.Admitted) {
			return false
		}
	}
	for _, event := range iss.Dispatch {
		if event.At.Equal(leave.At) && event.Issue != nil && event.Issue.Status == record.DoneStatus && !daemonWrote(event.Actor, iss.Key) {
			return false
		}
	}
	return true
}

// placeEvents reads every recorded event of the window at or after its start, from events, the
// window's events in the trace's order (readTrace): by time, then by id. It says how a replay
// places each, and answers each issue's issue events, by their index, in that order, each issue
// event naming the one before it on its issue (scriptEvent.prev), and each event of an issue the
// window files naming that filing (scriptEvent.filing), before which the replay applies no event
// of the issue (driver.awaitsFiling). A worker's event — one whose actor is a session the record
// shows running a claim of an issue, by the session's title or by the claim token its id is
// (actorClaimOf) — on that claim's issue, or the filing of a child under it, that falls in a piece
// of that claim's role's work (workPiece) is an act of that piece, placed at its offset in it where
// the replay runs the piece (spec R2, assumption 17: a child's filing moves with the start it is
// filed after). An event no piece places that depends on an issue's first start in its run, made
// while the record shows that issue in flight in it, is placed from the replay's start of the issue
// (anchorStart): the filing of a child under any issue, whoever files it (spec R2: a child's filing
// enters at its recorded offset from the start of the issue it is filed under), and an act of an
// issue's own architect on its issue that answers no question and ended no wait, which keep those
// anchors. An architect's act on a child it filed is placed from that filing (anchorFollow), so the
// child's course follows its filing. A worker's other event on another issue — an edit of it, a
// link it adds or takes off, a child it moves out — is no act of its piece and keeps its recorded
// time, as a version of a root's design does where the replay opens the root's work early
// (keepDesignTimes): the replay moves a piece of work, not the issues the worker touched, whose own
// course the record's times hold. Every other event is placed by its anchor, and so is a worker's
// event whose piece the replay does not run (placeUnrun, cancelWork): a person's answer from its
// question (Branch.Answer); an event that ended a wait in the record from the replay's start of that
// wait (Branch.Timing); a write of the daemon's own session, the window's project's, that follows
// the filing of its issue as a child, or follows such a write, from the event before it
// (anchorFollow); every other at its recorded time. Another project's daemon is another daemon,
// whose writes on its own issues are placed as any other event is.
func (s *script) placeEvents(t Trace, events []readEvent) (map[string][]int, error) {
	s.events = make([]scriptEvent, 0, len(events))
	for _, read := range events {
		env, err := readEnvelope(read.raw, read.event)
		if err != nil {
			return nil, err
		}
		s.events = append(s.events, scriptEvent{parsed: read.event, env: env, at: read.event.At, order: int(read.event.ID), on: -1, prev: -1,
			filing: -1})
	}
	issueEvents := map[string][]int{}
	filings := map[string]int{}
	for i := range s.events {
		event := &s.events[i]
		if event.parsed.Issue != nil {
			key := event.env.key
			if own := issueEvents[key]; len(own) > 0 {
				event.prev = own[len(own)-1]
			}
			issueEvents[key] = append(issueEvents[key], i)
		}
		if event.at.Before(t.From) {
			continue
		}
		if filing, filed := filings[event.env.key]; filed {
			event.filing = filing
		} else if event.env.kind == "issue.created" {
			filings[event.env.key] = i
		}
	}
	ended := s.waitEnders(t, issueEvents)
	questions := s.questions()
	for i := range s.events {
		event := &s.events[i]
		if event.at.Before(t.From) {
			continue
		}
		ran, worked := actorClaimOf(event.parsed)
		if worked && movesWithWork(event, ran) {
			if p := s.workPiece(ran, event.at); p != nil {
				event.piece = p
				p.acts = append(p.acts, act{kind: actEvent, offset: event.at.Sub(p.start), rec: event.at, event: i})
			}
		}
		question, answers := questions[i]
		answers = answers && event.piece == nil && !s.events[question].at.Before(t.From)
		wait, endsWait := ended[i]
		if key := startDependedOn(event, ran, worked); event.piece == nil && key != "" && !answers && !endsWait {
			if start, started := s.runStartAt(t, key, event.at); started {
				event.anchor, event.startOf, event.offset = anchorStart, key, event.at.Sub(start)
				continue
			}
		}
		if answers {
			event.anchor, event.on, event.offset = anchorEvent, question, event.at.Sub(s.events[question].at)
			s.events[question].dependents = append(s.events[question].dependents, i)
			continue
		}
		if endsWait && !wait.start.Before(t.From) {
			event.anchor, event.wait, event.offset = anchorWait, wait, event.at.Sub(wait.start)
		}
	}
	for i := range s.events {
		event := &s.events[i]
		if event.at.Before(t.From) || event.anchor != anchorTime || event.piece != nil {
			continue
		}
		if s.actsOnChildItFiled(event) {
			s.follow(i, event.filing)
			continue
		}
		if event.prev < 0 || event.env.project != t.Project || !daemonWrote(event.env.actor, event.env.key) {
			continue
		}
		if before := &s.events[event.prev]; before.piece != nil && before.piece.issue != event.env.key || before.anchor == anchorFollow ||
			before.anchor == anchorStart {
			s.follow(i, event.prev)
		}
	}
	return issueEvents, nil
}

// follow places events[i] from events[on] (anchorFollow): at its recorded offset from it, once the
// replay applies it.
func (s *script) follow(i, on int) {
	event := &s.events[i]
	event.anchor, event.on, event.offset = anchorFollow, on, event.at.Sub(s.events[on].at)
	s.events[on].followers = append(s.events[on].followers, i)
}

// actsOnChildItFiled says whether event is an architect's act on a child it filed: its actor's
// session runs the claim of an architect of another issue (actorClaimOf), the claim whose session
// filed event's issue in the window (scriptEvent.filing).
func (s *script) actsOnChildItFiled(event *scriptEvent) bool {
	if event.filing < 0 {
		return false
	}
	ran, worked := actorClaimOf(event.parsed)
	if !worked || ran.Role != claim.RoleArchitect || ran.Issue == event.env.key {
		return false
	}
	filer, filed := actorClaimOf(s.events[event.filing].parsed)
	return filed && filer == ran
}

// movesWithWork says whether event, made by the worker of ran, moves with the work it falls in
// (placeEvents): an event on ran's own issue, or the filing of a child under it (issue.created
// naming it the parent).
func movesWithWork(event *scriptEvent, ran SessionClaim) bool {
	if event.env.key == ran.Issue {
		return true
	}
	return event.parsed.Type == "issue.created" && event.parsed.Issue != nil && event.parsed.Issue.Parent == ran.Issue
}

// startDependedOn is the issue whose first start event depends on where no piece places it
// (anchorStart): the issue it files a child under, whoever files it (filedUnder), or the issue of
// the architect whose act on that issue it is (ran, where worked says its actor runs a claim); ""
// for every other event.
func startDependedOn(event *scriptEvent, ran SessionClaim, worked bool) string {
	if parent := filedUnder(event); parent != "" {
		return parent
	}
	if worked && ran.Role == claim.RoleArchitect && event.env.key == ran.Issue {
		return ran.Issue
	}
	return ""
}

// filedUnder is the issue event files a child under: the parent the child's issue.created names, or
// the issue a child.added is on; "" for an event that files no child.
func filedUnder(event *scriptEvent) string {
	switch event.env.kind {
	case "issue.created":
		if event.parsed.Issue != nil {
			return event.parsed.Issue.Parent
		}
	case "child.added":
		return event.env.key
	}
	return ""
}

// runStartAt is when key took its first slot in the run it stands in at at, the run of its entry at
// at, where the record shows key in flight at at (IssueTrace.inFlight), its run started and not yet
// ended: the first slot that begins at or after that entry or had not ended by it. False where the
// record shows key out of flight at at, unstarted in its run or out of the workflow since the run's
// end, where no fact at at depends on that run's start.
func (s *script) runStartAt(t Trace, key string, at time.Time) (time.Time, bool) {
	iss := t.Issues[key]
	if iss == nil || !withinStretches(s.flight[key], at) {
		return time.Time{}, false
	}
	entry := iss.entryAt(at)
	for _, span := range iss.Slots {
		if span.Start.After(at) {
			break
		}
		if entry == nil || !span.Start.Before(entry.At) || span.End.IsZero() || span.End.After(entry.At) {
			return span.Start, true
		}
	}
	return time.Time{}, false
}

// markHeldRootTodos marks each event in which the daemon's own session set a held root todo
// (releasesHold): the issue's last issue event (issueEvents, placeEvents) at or before a
// `workflow: a held phase is released` of heldRootReleaseReason, where that event is the daemon
// session's todo. Only a todo on a held root writes that release (workflow's releaseHeldRoot),
// from the todo's event or, for the controller's set_status, from the daemon API's own write,
// which Dispatch records as that event.
func (s *script) markHeldRootTodos(lines recordLines, issueEvents map[string][]int) {
	for _, line := range lines.released {
		if line.Reason != heldRootReleaseReason {
			continue
		}
		for _, i := range slices.Backward(issueEvents[line.Issue]) {
			event := &s.events[i]
			if event.at.After(line.Time) {
				continue
			}
			event.releasesHold = event.releasesHold || daemonWrote(event.env.actor, event.env.key) && event.parsed.Issue.Status == "todo"
			break
		}
	}
}

// questions is, by the index of each person's answer among the events, the event it answers: the
// opening of the ask an ask's close, hand-back or comment is of, or the newest version event of
// the document a person's verdict is on, at or before the verdict — the event that stands for the
// registration of the version the person reviews, since the registration itself is a line of the
// record. It reads the events once, in order, keeping for each issue the newest opening of each
// ask and the newest version event of each version of each document.
func (s *script) questions() map[int]int {
	type askOf struct{ issue, ask string }
	type versionOf struct {
		issue, artifact string
		number          int
	}
	opened := map[askOf]int{}
	written := map[versionOf]int{}
	found := map[int]int{}
	for i := range s.events {
		event := &s.events[i]
		key := event.parsed.Key
		if ask, artifact, version := answerOf(event); ask != "" {
			if j, ok := opened[askOf{key, ask}]; ok {
				found[i] = j
			}
		} else if artifact != "" {
			if j, ok := written[versionOf{key, artifact, version}]; ok {
				found[i] = j
			}
		}
		switch event.parsed.Type {
		case "ask.opened":
			opened[askOf{key, event.parsed.Ask.ID}] = i
		case "artifact.version":
			if artifact, number := versionWritten(event); artifact != "" {
				written[versionOf{key, artifact, number}] = i
			}
		}
	}
	return found
}

// answerOf is what event answers where it is a person's: the ask an ask's close or hand-back or a
// comment is of, or the document and version a verdict is on; all empty for any other event.
func answerOf(event *scriptEvent) (ask, artifact string, version int) {
	parsed := event.parsed
	if !parsed.Actor.Person() {
		return "", "", 0
	}
	switch {
	case parsed.Ask != nil && parsed.Type != "ask.opened":
		return parsed.Ask.ID, "", 0
	case parsed.Verdict != nil:
		return "", parsed.Verdict.Artifact, parsed.Verdict.Version
	case parsed.Type == "comment.created" || parsed.Type == "comment.answered":
		var payload struct {
			AskID string `json:"ask_id"`
		}
		if decodePayload(event.env, &payload) == nil {
			return payload.AskID, "", 0
		}
	}
	return "", "", 0
}

// versionWritten is the document event, an artifact.version event, writes a version of and the
// version's number; "" for a payload that names no document.
func versionWritten(event *scriptEvent) (string, int) {
	var payload versionPayload
	if decodePayload(event.env, &payload) != nil {
		return "", 0
	}
	return payload.ArtifactID, payload.Version.Number
}

// waitEnders is, by the index of the event, each event that ended a wait in the record: the last
// unfinished link taken off (link_removed, the waiting issue's own event), an outside blocker's
// close (blocker_done of an issue Legion does not work), and the last open child moved out from
// under its parent (child_moved_out, the child's event), each with the wait it ended. own is each
// issue's issue events, by their index, in order.
func (s *script) waitEnders(t Trace, own map[string][]int) map[int]waitAnchor {
	ended := map[int]waitAnchor{}
	find := func(key string, at time.Time) int {
		events := own[key]
		j, found := slices.BinarySearchFunc(events, at, func(i int, at time.Time) int { return s.events[i].at.Compare(at) })
		if !found {
			return -1
		}
		return events[j]
	}
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		for _, wait := range iss.Waits {
			if wait.End.IsZero() {
				continue
			}
			switch {
			case wait.Kind == WaitLink && wait.EndedBy == WaitLinkRemoved:
				if i := find(key, wait.End); i >= 0 {
					ended[i] = s.linkAnchor(own[key], iss, wait)
				}
			case wait.Kind == WaitLink && wait.EndedBy == WaitBlockerDone:
				for _, blocker := range wait.On {
					outside, known := t.Issues[blocker]
					if known && len(outside.Visits) > 0 {
						continue
					}
					if i := find(blocker, wait.End); i >= 0 {
						anchor := s.linkAnchor(own[key], iss, wait)
						anchor.blocker = blocker
						ended[i] = anchor
					}
				}
			case wait.Kind == WaitChildren && wait.EndedBy == WaitChildMovedOut:
				for _, child := range wait.On {
					if i := find(child, wait.End); i >= 0 {
						ended[i] = waitAnchor{issue: key, kind: WaitChildren, start: wait.Start, added: -1}
					}
				}
			}
		}
	}
	return ended
}

// linkAnchor is the anchor of an event that ended wait, a link wait of iss: the wait's start, and
// the event of iss that added the link it began on: the first of the issue events of iss, up to
// the wait's start, that each name a blocker the wait is on, with no event between them that
// names none. own is the issue events of iss, by their index, in order.
func (s *script) linkAnchor(own []int, iss *IssueTrace, wait Wait) waitAnchor {
	anchor := waitAnchor{issue: iss.Key, kind: WaitLink, start: wait.Start, added: -1}
	for _, i := range own {
		event := &s.events[i]
		if event.at.After(wait.Start) {
			break
		}
		blockedBy := event.parsed.Issue.BlockedBy
		names := slices.ContainsFunc(wait.On, func(key string) bool { return slices.Contains(blockedBy, key) })
		switch {
		case !names:
			anchor.added = -1
		case anchor.added < 0:
			anchor.added = i
		}
	}
	return anchor
}

// recordLines is what a replay reads of the window's log beside its trace: the lines of acts it
// applies again that the trace keeps no fact of — each claim's failure, each release of a hold
// with its reason, each escalation of a hold, and each root an architect's release took into its
// tree — each of a kind the window holds (Window.Lines), in order.
type recordLines struct {
	failed    []ClaimFailed
	released  []HoldReleased
	escalated []PhaseHeld
	joined    []RootJoined
}

// recordLinesOf is the recordLines of lines, the window's lines of the kinds it holds in the
// order the trace read them (readTrace): in time order, the window's own among lines of one
// moment.
func recordLinesOf(lines []LogLine) recordLines {
	var read recordLines
	for _, line := range lines {
		switch l := line.(type) {
		case ClaimFailed:
			read.failed = append(read.failed, l)
		case HoldReleased:
			read.released = append(read.released, l)
		case PhaseHeld:
			if l.Reason == string(record.HoldEscalated) {
				read.escalated = append(read.escalated, l)
			}
		case RootJoined:
			read.joined = append(read.joined, l)
		}
	}
	return read
}
