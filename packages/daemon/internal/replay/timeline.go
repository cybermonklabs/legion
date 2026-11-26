package replay

import (
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Timeline is a window's course as the measures read it (Measure): each issue's states over the
// window, each holding from its moment to the next one's, with the slots it was given, its phase
// moves and its waits on other issues. A record's timeline is read from its trace (the line
// timeline, lineTimeline), and a replay builds two (design 10.1): one from the views its admission
// passes leave and the claims its worker stub holds (the snapshot timeline, snapshots), and one
// from its own log, read through the window's line kinds as the record is. So the record and a
// replay are measured by one code, and the gap between a replay's two timelines is the error of
// reading a course from its lines.
type Timeline struct {
	// From and To are the window's span, and End where the run ends: To, or the end of the
	// window's run-out past To (Window.End), to which the wait of urgent work counts a wait that
	// has not ended by then (urgentWaits). The zero time reads as To.
	From, To, End time.Time
	// Issues is each issue's course, by key.
	Issues map[string]*Course
	// MissingAncestors is each ancestor whose history the window lacks on which the priority
	// Dispatch resolves for an issue of it rests (resolvedPriorities.missingAncestors), set with
	// the issues' priorities (readUrgency): with any, the timeline gives no issue's priority the
	// measures can read, and Measure reads no wait of urgent work and no priority taken in today's
	// line's order (spec R10: no comparison that depends on the missing input).
	MissingAncestors []string
}

// Course is one issue's course over a timeline.
type Course struct {
	// States is the issue's states in time order, each holding from its At to the next one's At,
	// or to the timeline's end. Before the first the issue stands in no state a measure counts.
	States []Stint
	// Given is each moment a pass gave the issue a slot, in order.
	Given []time.Time
	// Moves is each move of the issue's phase, in order.
	Moves []CourseMove
	// Rounds is each round of a send-back loop that moved no phase, in order: a conflicting head
	// merged forward while the issue kept its phase (record.ConflictMergeForward, whose conflict
	// past the review the merger merges forward since cybermonklabs/legion#4979, and
	// record.ConflictImplementerMergesForward).
	Rounds []CourseRound
	// Waits is each wait of the issue on another issue, by the moment it began.
	Waits []CourseWait
	// Todo is the moment the issue's wait as urgent work begins (urgentWaits, urgencyOf): its first
	// todo in the span, or the span's start where it stood todo there from before; zero for an
	// issue that did neither. Priorities is its priority as Dispatch resolves it from Todo on
	// (resolvedPriorities): the priority at Todo, then each change of it after, in time order.
	Todo       time.Time
	Priorities []PriorityAt
}

// PriorityAt is an issue's priority from a moment on: the one Dispatch resolves for it
// (resolvedPriorities), as the measures read it.
type PriorityAt struct {
	At       time.Time
	Priority record.Priority
}

// CourseRound is one round of a send-back loop that moved no phase: from the merge forward's ask
// to its landing (End zero while it stands), and its cause, a head that conflicts with its base
// (the windows' README, "The measures": under main a drawn conflict that keeps its phase is one
// round of conflict, its hours the merge-forward segment).
type CourseRound struct {
	Start, End time.Time
	Cause      Cause
}

// Stint is one state of an issue and the moment it began.
type Stint struct {
	At time.Time
	IssueState
}

// IssueState is what a timeline records of one issue at a moment: whether it is in flight, whether
// it holds a slot and in which of the slots reading's states (record.HolderStateOf), whether a
// worker of it is in a turn, whether it stands past its review, its place in the waiting line, and,
// for a holder or an issue that waits for a slot, its tree and the Dispatch ranks that place it.
// The measures read every part but the holder's state, which the timeline keeps beside them. An
// issue that is not in flight, holds no slot and stands in no part of the line is in the zero
// state, which no measure counts.
type IssueState struct {
	// Tree is the key of the issue's tree root; Rank and RootRank are the Dispatch ranks of the
	// issue and of that root, which record.RankLess compares to place it (the true ranks, which a
	// replay's own copy of Dispatch does not show under FirstStarts recorded, design 10.7); and
	// Priority is the Dispatch priority that places it in today's line's band, its resolved one or
	// else its root's (record.PriorityOf over the priorities Dispatch resolves, which is the
	// resolved one: an issue that resolves to none has a root that does). They are set for a holder
	// and an issue that waits for a slot alone, the two a pair of priority taken is made of
	// (rankFunc.place).
	Tree, Rank, RootRank string
	Priority             record.Priority
	// InFlight says the daemon has started the issue's work and it has not reached done or left
	// the workflow (`replay.md`, "The measures").
	InFlight bool
	// Holds says it holds a slot of the cap, and Holder is the state of the slots reading it is in
	// while it does ("" while it does not).
	Holds  bool
	Holder record.HolderState
	// InTurn says a worker of the issue, of any role, is in a turn.
	InTurn bool
	// PastReview says it stands in accepting, retro or merging, or is held from one of them.
	PastReview bool
	// AwaitingChildren says it is in flight in awaiting_children: a started parent whose next
	// start its open children hold, its phase stopped by the admission pass under a rule whose
	// wait stops the phase (cybermonklabs/legion#4592). It stands in no part of the line, and
	// while it holds no slot it is started work without a slot in all (`replay.md`, "The
	// measures", startedWithoutSlot).
	AwaitingChildren bool
	// Part is the part of the waiting line it stands in, zero for none; Blocked says
	// admission.blocked names it; Waiting says it is an issue that waits for a slot: a member of
	// the line that holds no slot, that no wait holds back, and that neither admission.blocked
	// nor admission.held names.
	Part    record.LinePart
	Blocked bool
	Waiting bool
}

// CourseMove is one move of an issue's phase: its moment, its phases and, for a move back, its
// cause (CauseOf).
type CourseMove struct {
	At       time.Time
	From, To phase.Phase
	Cause    Cause
}

// CourseWait is one wait of an issue on other issues: from the moment it began to the moment
// nothing was left to wait on, or the issue moved past its review (zero while it stands), and what
// ended it (`replay.md`, "The measures": blocker_done, link_removed, child_done,
// child_moved_out, gate_closed, moved_on, left_workflow, or open).
type CourseWait struct {
	Start, End time.Time
	EndedBy    WaitEnd
}

// course is key's course, made where none is yet.
func (t *Timeline) course(key string) *Course {
	if t.Issues == nil {
		t.Issues = map[string]*Course{}
	}
	c, ok := t.Issues[key]
	if !ok {
		c = &Course{}
		t.Issues[key] = c
	}
	return c
}

// set records that the issue stands in state from at: a stint where the state differs from the one
// it stands in, which at the moment of the last stint takes that stint's place.
func (c *Course) set(at time.Time, state IssueState) {
	n := len(c.States)
	switch {
	case n == 0 && state == (IssueState{}):
	case n > 0 && c.States[n-1].IssueState == state:
	case n > 0 && c.States[n-1].At.Equal(at):
		c.States[n-1].IssueState = state
		if n > 1 && c.States[n-2].IssueState == state {
			c.States = c.States[:n-1]
		}
	default:
		c.States = append(c.States, Stint{At: at, IssueState: state})
	}
}

// moveOf is the course's reading of a move: its cause where it moves back (CauseOf, which reads
// the move's trigger), none for a move back CauseOf names no cause of. drop is the msg of the line
// of the issue's pull request leaving the merge queue before a move out of merging, "" for none
// (SendBack.Drop).
func moveOf(at time.Time, from, to phase.Phase, trigger workflow.TriggerKind, drop string) CourseMove {
	move := CourseMove{At: at, From: from, To: to}
	if phase.MovesBack(from, to) {
		if cause, named := CauseOf(SendBack{Trigger: trigger, From: from, To: to, Drop: drop}); named {
			move.Cause = cause
		}
	}
	return move
}

// pastReviewPhase says whether p is a phase past the review of the default steps every recorded
// window ran (record.InSlotlessPhase, the daemon's own rule's complement): accepting, retro or
// merging.
func pastReviewPhase(p phase.Phase) bool {
	return record.InSlotlessPhase(record.Issue{Phase: p})
}

// upTo is how many of ts, moments in time order, are at or before at: the index of the first one
// after it.
func upTo(ts []time.Time, at time.Time) int {
	return sort.Search(len(ts), func(i int) bool { return ts[i].After(at) })
}

// rankFunc is what places an issue on the board at a moment: its Dispatch rank and the priority
// Dispatch resolves for it.
type rankFunc func(key string, at time.Time) (string, record.Priority)

// ranksOf is the rank of each issue of t at each moment as its Dispatch events show it, that of its
// newest issue event at or before the moment, or of its first for a moment before every one, and
// "" for an issue the window holds no issue event of; with the priority priorities resolves for it
// then, under every rule, so every rule is measured on one banding of the line. A replay reads the
// ranks and the priorities of its record's trace, the true ones, whatever its own copy of Dispatch
// shows (design 10.7). Each issue's events are indexed once, and each is read by binary search.
func ranksOf(t Trace, priorities resolvedPriorities) rankFunc {
	shown := make(map[string]shownStates, len(t.Issues))
	for key, iss := range t.Issues {
		shown[key] = shownOf(iss)
	}
	return func(key string, at time.Time) (string, record.Priority) {
		s := shown[key]
		if len(s.states) == 0 {
			return "", record.NoPriority
		}
		state := s.stateAt(at)
		if state == nil {
			state = s.states[0]
		}
		return state.Rank, priorities.at(key, at)
	}
}

// place sets in state what places key, in tree, on the board at at: its tree, the ranks of the
// issue and of its tree root, and the priority that places it in today's line's band, as the line
// places an issue (record.PriorityOf) over the priorities Dispatch resolves, as ranks reads them.
// Both timelines place a holder and an issue that waits for a slot by it.
func (ranks rankFunc) place(state *IssueState, key, tree string, at time.Time) {
	rank, resolved := ranks(key, at)
	rootRank, rootResolved := ranks(tree, at)
	state.Tree, state.Rank, state.RootRank = tree, rank, rootRank
	state.Priority = record.PriorityOf(record.Issue{Priority: resolved}, record.Issue{Priority: rootResolved})
}

// shownStates is an issue's Dispatch state as each of its issue events shows it, in time order,
// read at a moment by binary search.
type shownStates struct {
	at     []time.Time
	states []*DispatchIssueState
}

// shownOf is the states iss's issue events show, in the order its log holds them, which is time
// order (BuildTrace).
func shownOf(iss *IssueTrace) shownStates {
	var s shownStates
	for _, event := range iss.Dispatch {
		if event.Issue != nil {
			s.at = append(s.at, event.At)
			s.states = append(s.states, event.Issue)
		}
	}
	return s
}

// stateAt is the state the newest issue event at or before at shows, nil before the first
// (dispatchStateBefore, read by binary search).
func (s shownStates) stateAt(at time.Time) *DispatchIssueState {
	i := upTo(s.at, at)
	if i == 0 {
		return nil
	}
	return s.states[i-1]
}

// stretchCount is how many of a set of stretches stand from each of its moments on, read at a
// moment by binary search: a stretch stands from its start up to, not at, its end, and on where
// its end is zero.
type stretchCount struct {
	from   []time.Time
	counts []int
}

// countStretches is the count of stretches.
func countStretches(stretches [][2]time.Time) stretchCount {
	type edge struct {
		at    time.Time
		delta int
	}
	edges := make([]edge, 0, 2*len(stretches))
	for _, stretch := range stretches {
		edges = append(edges, edge{at: stretch[0], delta: 1})
		if !stretch[1].IsZero() {
			edges = append(edges, edge{at: stretch[1], delta: -1})
		}
	}
	slices.SortStableFunc(edges, func(a, b edge) int { return a.at.Compare(b.at) })
	var count stretchCount
	standing := 0
	for i, e := range edges {
		standing += e.delta
		if i+1 < len(edges) && edges[i+1].at.Equal(e.at) {
			continue
		}
		count.from, count.counts = append(count.from, e.at), append(count.counts, standing)
	}
	return count
}

// at is how many stretches stand at at.
func (c stretchCount) at(at time.Time) int {
	i := upTo(c.from, at)
	if i == 0 {
		return 0
	}
	return c.counts[i-1]
}

// gateSteps is a design gate's openness after each fact of it, read at a moment by binary search.
type gateSteps struct {
	at   []time.Time
	open []bool
}

// gateStepsOf is root's design gate as its gate facts leave it, in time order (BuildTrace reads
// the lines and the verdicts in one time order), read as gateReader reads a gate, lines saying
// whether the window holds the daemon's lines of the gate's flips (Trace.GateLines). Only the facts
// that change the gate are kept.
func gateStepsOf(root *IssueTrace, lines bool) gateSteps {
	var steps gateSteps
	gate := gateReader{lines: lines}
	for _, fact := range root.Gates {
		open := gate.apply(fact)
		if n := len(steps.open); n > 0 && steps.open[n-1] == open {
			continue
		}
		steps.at, steps.open = append(steps.at, fact.At), append(steps.open, open)
	}
	return steps
}

// openAt says whether the gate stands open at at; closed before its first fact.
func (g gateSteps) openAt(at time.Time) bool {
	i := upTo(g.at, at)
	return i > 0 && g.open[i-1]
}

// issueIndex is what the line timeline reads of one issue of a trace, each part built once from
// the issue's trace and read at any moment by binary search, so reading an issue at each moment
// its state can change costs a logarithm per read rather than a walk of its record.
type issueIndex struct {
	iss *IssueTrace
	// shown is its Dispatch states, and done each moment its status turned done or turned from
	// done.
	shown shownStates
	done  []time.Time
	// rankChanges is each moment an issue event changed its rank or its priority, which place it
	// and the members of the tree it roots on the board.
	rankChanges []time.Time
	// flight is its stretches in flight (IssueTrace.inFlight).
	flight [][2]time.Time
	// inFlight, slots, turns, waits, person and held count its stretches in flight, its slots,
	// its workers' turns, its waits on other issues (link and children), its waits on a person and
	// its holds.
	inFlight, slots, turns, waits, person, held stretchCount
	// restarts is each moment a hold of it ended with its worker sent the held phase again
	// (Wait.released), in order: the held phase's retry writes that worker's start.
	restarts []time.Time
	// gate is the design gate of the tree it roots.
	gate gateSteps
}

// indexIssue indexes iss, the gate of the tree it roots read as gateStepsOf reads it.
func indexIssue(iss *IssueTrace, gateLines bool) *issueIndex {
	x := &issueIndex{iss: iss, shown: shownOf(iss), flight: iss.inFlight(), gate: gateStepsOf(iss, gateLines)}
	done, rank, priority := false, "", record.NoPriority
	for i, state := range x.shown.states {
		if isDone := state.Status == record.DoneStatus; i == 0 || isDone != done {
			done = isDone
			x.done = append(x.done, x.shown.at[i])
		}
		if i == 0 || state.Rank != rank || state.Priority != priority {
			rank, priority = state.Rank, state.Priority
			x.rankChanges = append(x.rankChanges, x.shown.at[i])
		}
	}
	x.inFlight = countStretches(x.flight)
	var slots, turns, waits, person, held [][2]time.Time
	for _, span := range iss.Slots {
		slots = append(slots, [2]time.Time{span.Start, span.End})
	}
	for _, turn := range iss.Turns {
		turns = append(turns, [2]time.Time{turn.Start, turn.End})
	}
	for _, wait := range iss.Waits {
		stretch := [2]time.Time{wait.Start, wait.End}
		switch wait.Kind {
		case WaitLink, WaitChildren:
			waits = append(waits, stretch)
		case WaitPerson:
			person = append(person, stretch)
		case WaitHeld:
			held = append(held, stretch)
			if wait.released() && !wait.End.IsZero() {
				x.restarts = append(x.restarts, wait.End)
			}
		}
	}
	slices.SortFunc(x.restarts, time.Time.Compare)
	x.slots, x.turns, x.waits, x.person, x.held = countStretches(slots), countStretches(turns), countStretches(waits),
		countStretches(person), countStretches(held)
	return x
}

// entryOf is iss's latest entry at or before at, nil before its first (IssueTrace.entryAt, read by
// binary search over its entries in time order).
func entryOf(iss *IssueTrace, at time.Time) *Entry {
	i := sort.Search(len(iss.Entries), func(i int) bool { return iss.Entries[i].At.After(at) })
	if i == 0 {
		return nil
	}
	return &iss.Entries[i-1]
}

// visitOf is the visit iss stands in at at, nil for none (IssueTrace.visitAt, read by binary
// search over its visits, which follow one another in time order).
func visitOf(iss *IssueTrace, at time.Time) *Visit {
	i := sort.Search(len(iss.Visits), func(i int) bool { return iss.Visits[i].Enter.After(at) })
	if i == 0 {
		return nil
	}
	if visit := &iss.Visits[i-1]; visit.Exit.IsZero() || visit.Exit.After(at) {
		return visit
	}
	return nil
}

// lineReader reads the issues of a trace at any moment for the line timeline: the trace, the ranks
// and the rule it reads by, and each issue's index, built on its first read.
type lineReader struct {
	t     Trace
	ranks rankFunc
	rule  func(time.Time) record.SlotRule
	index map[string]*issueIndex
}

// of is key's index, nil for an issue the trace does not hold.
func (r *lineReader) of(key string) *issueIndex {
	if x, ok := r.index[key]; ok {
		return x
	}
	iss, ok := r.t.Issues[key]
	if !ok {
		return nil
	}
	x := indexIssue(iss, r.t.GateLines)
	r.index[key] = x
	return x
}

// lineTimeline is the timeline read from t (the line timeline of design 10.1 (b)): the record's,
// or a replay's read from its own log through the window's line kinds. Each issue of t's project
// is read at each moment its state can change — a line or an event of its own, of its tree root,
// of an issue its links or its parent chain name, a rule's part beginning — with its ranks and its
// priority from ranks and the rule the daemon ran at that moment from rule. An issue is in flight
// from the first slot it took after an entry to its run's end; it stands in the line's returned
// part while in flight it holds no slot in a phase the rule keeps one in, unheld, where the
// daemon's line places it there (returnedAt: it owes the start of the move into its phase, or a
// pass gave its slot back for a wait), blocked while a link or children wait of it stands; and in
// the starting part while, not yet started, the daemon's line places it there (startingAt: a
// queued root handed to Legion in todo, or a todo child of a tree that stands in the workflow under
// an open design gate, its root waiting on no person), blocked while a blocker its links name is
// not done or its own open children hold it. Its waits are those of the measures (issueWaits), and
// each move back takes the cause the trace read it with (Move.Cause), a merger's move back for a
// conflicting head a conflict.
// It runs to t's end, the end of the window's run-out, and each issue's wait as urgent work is read
// from t's issue events with the priorities Dispatch resolves, priorities (readUrgency).
func lineTimeline(t Trace, ranks rankFunc, priorities resolvedPriorities, rule func(time.Time) record.SlotRule) Timeline {
	tl := Timeline{From: t.From, To: t.To, End: t.End, Issues: map[string]*Course{}}
	r := &lineReader{t: t, ranks: ranks, rule: rule, index: map[string]*issueIndex{}}
	for _, move := range t.Slots {
		if move.Given {
			if iss, ok := t.Issues[move.Issue]; ok && iss.Project == t.Project {
				c := tl.course(move.Issue)
				c.Given = append(c.Given, move.At)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		if iss.Project != t.Project {
			continue
		}
		x := r.of(key)
		c := tl.Issues[key]
		for _, at := range r.moments(x) {
			state := r.stateAt(x, at)
			if c == nil && state == (IssueState{}) {
				continue
			}
			if c == nil {
				c = tl.course(key)
			}
			c.set(at, state)
		}
		moves, waits := iss.Moves(), issueWaits(iss, x.flight)
		if c == nil && len(moves)+len(waits)+len(iss.MergeForwards) > 0 {
			c = tl.course(key)
		}
		for _, move := range moves {
			c.Moves = append(c.Moves, CourseMove{At: move.At, From: move.From, To: move.To, Cause: move.Cause})
		}
		for _, round := range iss.MergeForwards {
			c.Rounds = append(c.Rounds, CourseRound{Start: round.Start, End: round.End, Cause: CauseConflict})
		}
		if len(waits) > 0 {
			c.Waits = waits
		}
	}
	tl.readUrgency(t, priorities)
	return tl
}

// readUrgency sets, for each issue of t's project that Legion works, one t holds an entry into the
// workflow of (Entry: a root handed to Legion, or a child of a tree in the workflow), and whose wait
// as urgent work begins in t's span (urgencyOf), the moment it begins, as t's issue events show it,
// and its priority from then on as Dispatch resolves it (Course.Todo, Course.Priorities, from
// priorities), making a course for an issue tl holds none of; and the ancestors the window lacks
// on which a priority rests (Timeline.MissingAncestors). An issue no one handed to Legion, which a
// window holds for its events alone, is no work of Legion's and waits for no slot. The line
// timeline reads the todo from its own trace, and a replay's snapshot timeline from its record's,
// as it reads the ranks; both read the priorities of the record's trace.
func (tl *Timeline) readUrgency(t Trace, priorities resolvedPriorities) {
	tl.MissingAncestors = priorities.missingAncestors()
	for key, iss := range t.Issues {
		if iss.Project != t.Project || len(iss.Entries) == 0 {
			continue
		}
		if since := urgencyOf(iss, t.From, t.To); !since.IsZero() {
			c := tl.course(key)
			c.Todo, c.Priorities = since, priorities.from(key, since)
		}
	}
}

// urgencyOf is where iss's wait as urgent work begins (Course.Todo): at its first todo in [from,
// to), the first issue event in the span that shows it todo where the issue event before it, of any
// moment, did not, or at from where the newest issue event before from shows it todo. Zero for an
// issue that neither turned todo in the span nor stood todo at its start.
func urgencyOf(iss *IssueTrace, from, to time.Time) time.Time {
	var lastAt time.Time
	var last *DispatchIssueState
	for _, event := range iss.Dispatch {
		state := event.Issue
		if state == nil {
			continue
		}
		if !event.At.Before(from) {
			switch {
			case last != nil && last.Status == "todo":
				return from
			case state.Status == "todo" && event.At.Before(to):
				return event.At
			}
		}
		last, lastAt = state, event.At
	}
	if last != nil && last.Status == "todo" && lastAt.Before(from) {
		return from
	}
	return time.Time{}
}

// recordedWaitStarts is, for each of keys that t's record shows waiting for a slot at at, the
// moment it began that wait: the first of the run of moments up to at in which its line reading
// waits for one (waitsForSlot, as waitRuns joins them), read as lineTimeline reads each moment, by
// the ranks of t's record (ranksOf) and the rule its parts name (Trace.ruleAt). An issue the
// record shows waiting for none at at is left out. Each issue is read alone, at its own moments, so
// the cost is that of its own reading.
func recordedWaitStarts(t Trace, ranks rankFunc, at time.Time, keys []string) map[string]time.Time {
	r := &lineReader{t: t, ranks: ranks, rule: t.ruleAt, index: map[string]*issueIndex{}}
	starts := make(map[string]time.Time, len(keys))
	for _, key := range keys {
		x := r.of(key)
		if x == nil {
			continue
		}
		var since time.Time
		waiting := false
		for _, moment := range r.moments(x) {
			if moment.After(at) {
				break
			}
			switch {
			case !waitsForSlot(r.stateAt(x, moment)):
				waiting = false
			case !waiting:
				since, waiting = moment, true
			}
		}
		if waiting {
			starts[key] = since
		}
	}
	return starts
}

// issueWaits is iss's waits on other issues as the measures count them (`replay.md`, "The
// measures": a stretch in which an issue in flight, standing before its review or in
// awaiting_children, waits on another issue): each stretch of its link and children waits in which
// it stands in flight (flight) and in such a phase (measuredStretches), in the order they began,
// each run of them that overlap
// or touch read as one wait, from the first one's start to the last one's end, ended by what ended
// the last. A wait that begins where another ends is the same stretch of waiting, as the snapshot
// timeline reads it, whose wait goes on at a settle where something is left to wait on whatever
// ended and began there (snapshots.waitAt): a blocker done at the moment another is linked, or a
// children wait the trace ends where the daemon's next line of a children wait of the issue begins
// (IssueTrace.nextWaitStart). A wait cut by its stretch's end ends as that stretch does: its move
// past its review (moved_on), where the daemon judges no wait of it any longer (blockers' waitsOf)
// and the snapshot timeline ends it too (waitEnd), or its leave (left_workflow). So a children
// wait the daemon tells of a child that has not started, the line's hold on its first phase, is
// none of them, as the snapshot timeline, which reads the waits of the issues in flight alone,
// counts none.
func issueWaits(iss *IssueTrace, flight [][2]time.Time) []CourseWait {
	stretches := measuredStretches(iss, flight)
	var pieces []CourseWait
	for _, wait := range iss.Waits {
		if wait.Kind != WaitLink && wait.Kind != WaitChildren {
			continue
		}
		for _, stretch := range stretches {
			start, end, endedBy := maxTime(wait.Start, stretch.from), wait.End, wait.EndedBy
			if !stretch.to.IsZero() && (end.IsZero() || end.After(stretch.to)) {
				end, endedBy = stretch.to, stretch.cut
			}
			if !end.IsZero() && !end.After(start) {
				continue
			}
			pieces = append(pieces, CourseWait{Start: start, End: end, EndedBy: endedBy})
		}
	}
	slices.SortStableFunc(pieces, func(a, b CourseWait) int { return a.Start.Compare(b.Start) })
	var waits []CourseWait
	for _, wait := range pieces {
		if n := len(waits); n > 0 && (waits[n-1].End.IsZero() || !wait.Start.After(waits[n-1].End)) {
			last := &waits[n-1]
			if !last.End.IsZero() && (wait.End.IsZero() || wait.End.After(last.End)) {
				last.End, last.EndedBy = wait.End, wait.EndedBy
			}
			continue
		}
		waits = append(waits, wait)
	}
	return waits
}

// waitStretch is a stretch in which the measures count an issue's waits, from to to (zero while
// it stands at the window's end), and what ends a wait its end cuts.
type waitStretch struct {
	from, to time.Time
	cut      WaitEnd
}

// measuredStretches is each stretch in which iss stands in flight (flight) and in a visit to a
// phase whose waits are measured (measuredPhase: before its review, or awaiting_children), in
// order, the visits that follow one another joined: a stretch ends with the issue's move past its
// review (moved_on), or with its leave or its run's end (left_workflow).
func measuredStretches(iss *IssueTrace, flight [][2]time.Time) []waitStretch {
	var stretches []waitStretch
	for _, visit := range iss.Visits {
		if !measuredPhase(visit.Phase) {
			continue
		}
		for _, run := range flight {
			from, to, cut := maxTime(visit.Enter, run[0]), visit.Exit, WaitMovedOn
			if visit.Left || visit.Out != nil && visit.Out.To == phase.Done {
				cut = WaitLeft
			}
			if !run[1].IsZero() && (to.IsZero() || run[1].Before(to)) {
				to, cut = run[1], WaitLeft
			}
			if !to.IsZero() && !to.After(from) {
				continue
			}
			if n := len(stretches); n > 0 && stretches[n-1].to.Equal(from) {
				stretches[n-1].to, stretches[n-1].cut = to, cut
				continue
			}
			stretches = append(stretches, waitStretch{from: from, to: to, cut: cut})
		}
	}
	return stretches
}

// moments is every moment from the trace's start to its end at which x's issue's state can
// change, in order, and the window's start: each of its own entries, visits, slots, turns, waits,
// stretches in flight and issue events, each change of a rule's part, each change of the done-ness
// of an issue its links name, for each tree it entered as a child each change of the root's rank
// or priority, design gate, visits, lingers and waits on a person, and for each issue its Dispatch
// parent chain has named each of that issue's entries, visits, lingers and issue events.
func (r *lineReader) moments(x *issueIndex) []time.Time {
	t, iss := r.t, x.iss
	times := []time.Time{t.From}
	add := func(at ...time.Time) {
		for _, moment := range at {
			if !moment.IsZero() && !moment.Before(t.Start) && moment.Before(t.To) {
				times = append(times, moment)
			}
		}
	}
	var roots []string
	for _, entry := range iss.Entries {
		add(entry.At)
		if entry.Tree == "" || entry.Tree == iss.Key || slices.Contains(roots, entry.Tree) {
			continue
		}
		roots = append(roots, entry.Tree)
		if root := r.of(entry.Tree); root != nil {
			add(root.rankChanges...)
			add(root.gate.at...)
			add(root.person.from...)
			for _, visit := range root.iss.Visits {
				add(visit.Enter, visit.Exit)
			}
			for _, linger := range root.iss.Lingers {
				add(linger.At, linger.Closed)
			}
		}
	}
	for _, visit := range iss.Visits {
		add(visit.Enter, visit.Exit)
	}
	add(x.slots.from...)
	add(x.turns.from...)
	add(x.waits.from...)
	add(x.person.from...)
	add(x.held.from...)
	add(x.inFlight.from...)
	add(x.shown.at...)
	for _, part := range t.Parts {
		add(part.From, part.To)
	}
	var blockers []string
	for _, state := range x.shown.states {
		for _, key := range state.BlockedBy {
			if !slices.Contains(blockers, key) {
				blockers = append(blockers, key)
			}
		}
	}
	for _, key := range blockers {
		if blocker := r.of(key); blocker != nil {
			add(blocker.done...)
		}
	}
	chain := []string{iss.Key}
	for i := 0; i < len(chain); i++ {
		above := r.of(chain[i])
		if above == nil {
			continue
		}
		if i > 0 {
			for _, entry := range above.iss.Entries {
				add(entry.At)
			}
			for _, visit := range above.iss.Visits {
				add(visit.Enter, visit.Exit)
			}
			for _, linger := range above.iss.Lingers {
				add(linger.At, linger.Closed)
			}
			add(above.shown.at...)
		}
		for _, state := range above.shown.states {
			if state.Parent != "" && !slices.Contains(chain, state.Parent) {
				chain = append(chain, state.Parent)
			}
		}
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	return slices.CompactFunc(times, func(a, b time.Time) bool { return a.Equal(b) })
}

// stateAt is the state x's issue stands in from at on. An issue in flight that holds no slot and
// is not held stands in the line's returned part where the daemon's line places it there
// (returnedAt), and in no part otherwise.
func (r *lineReader) stateAt(x *issueIndex, at time.Time) IssueState {
	iss := x.iss
	entry := entryOf(iss, at)
	tree := iss.Key
	if entry != nil && entry.Tree != "" {
		tree = entry.Tree
	}
	state := IssueState{InFlight: x.inFlight.at(at) > 0, Holds: x.slots.at(at) > 0, InTurn: x.turns.at(at) > 0}
	visit := visitOf(iss, at)
	waiting, held := x.waits.at(at) > 0, x.held.at(at) > 0
	if visit != nil && (state.InFlight || state.Holds) {
		state.PastReview = pastReviewPhase(visit.Phase)
		state.AwaitingChildren = state.InFlight && visit.Phase == phase.AwaitingChildren
	}
	if state.Holds {
		state.Holder = r.holderState(x, at, tree, visit, waiting, held)
	}
	switch {
	case state.InFlight && !state.Holds && !held && visit != nil && r.returnedAt(x, tree, visit, at):
		state.Part, state.Blocked, state.Waiting = record.PartReturned, waiting, !waiting
	case !state.InFlight && !state.Holds && entry != nil:
		if member, blocked := r.startingAt(x, at, tree); member {
			state.Part, state.Blocked, state.Waiting = record.PartStarting, blocked, !blocked
		}
	}
	if !state.InFlight && !state.Holds && state.Part == 0 {
		return IssueState{}
	}
	if state.Holds || state.Waiting {
		r.ranks.place(&state, iss.Key, tree, at)
	}
	return state
}

// returnedAt says whether x's issue, in flight and holding no slot at at in visit, in tree,
// stands in the line's returned part there, as the daemon's line places such an issue:
// record.Waiting's first loop, the same at the head and at the commit of the rule
// legion-v20.42.5 (958173da) but for the rule it asks NeedsSlot of. Each part of the reading
// mirrors a part of that loop:
//
//   - holding no slot: the loop takes every slot holder out of the line, since the slot is what
//     releases the start it owes;
//   - NeedsSlot, asked of the rule the line is built under at the head
//     (record.LineFacts.NeedsSlot) and of the daemon's own at that commit (record.NeedsSlot):
//     its phase half, the rule's PhaseKeepsSlot of the phase the issue stands in, and its tree
//     half, a tree that is live with a root that does not linger (treeLiveAt);
//   - a start it owes: the loop places an issue that owes its next worker a start
//     (record.LineFacts.Owed) as LineReturning, or as LineWaitingOnIssue where it waits on an
//     issue. Two acts of a visit write that start marked to wait for a slot
//     (record.SuperviseRequest.NeedsSlot, workflow.StartOrOwe): the move that began it, and the
//     retry of its held phase that ends a hold (workflow's retryPhase; Wait.released, a hold
//     released or gone on). A slot the issue held at the act delivered the start at once, and a
//     slot given later delivers it then (LineReturning's promotion). So the issue owes it where
//     the visit's last such act came while it held no slot and it has taken none since. A visit
//     begun by an entry, or before the log, with no hold released in it, owes none, as the start
//     state seeds none for it (Seed's owedStarts);
//   - a release for a wait: the loop places an issue that carries the mark of a pass that took
//     its slot back for a wait on another issue (record.Issue.ReleasedForWaitAt, which
//     record.Store.ReleaseSlotForWait writes with the release) as LineWaitingOnIssue, whatever it
//     owes, until a pass gives it a slot and clears the mark. Its last slot's SlotSpan.ForWait
//     reads that release: the reason the pass's record line gives, or a link or children wait of
//     the issue that stands where the slot went back. A pass clears the mark too where the issue
//     needs no slot (admit's releaseSlots): a move into a phase the rule keeps no slot in, whose
//     move back owes its start, and a tree that stopped being live, so a mark stands only while
//     the tree has stood live since the release.
//
// The issue's place in that part is the caller's: blocked while a link or children wait of it
// stands, as the promotion passes over an entry while what it waits on is unfinished
// (record.LineEntry.Waits), and waiting for a slot otherwise. The loop holds any other such issue
// (its hold, which the replay's daemon logs as `admission: the record holds an issue no held word
// is true of`, unclassified, or as tree_closed or tree_parked where the tree half fails), and it
// stands in no part of the line.
func (r *lineReader) returnedAt(x *issueIndex, tree string, visit *Visit, at time.Time) bool {
	liveSince, live := r.treeLiveAt(tree, at)
	if !live || !r.rule(at).PhaseKeepsSlot(visit.Phase) {
		return false
	}
	slots := x.iss.Slots
	var last *SlotSpan
	if i := sort.Search(len(slots), func(i int) bool { return slots[i].Start.After(at) }); i > 0 {
		last = &slots[i-1]
	}
	if last != nil && last.ForWait && !liveSince.After(last.End) {
		return true
	}
	var written time.Time
	if visit.In != nil {
		written = visit.Enter
	}
	if i := upTo(x.restarts, at); i > 0 && !x.restarts[i-1].Before(visit.Enter) {
		written = x.restarts[i-1]
	}
	return !written.IsZero() && (last == nil || last.End.Before(written))
}

// treeLiveAt says whether tree is live at at and its root does not linger (NeedsSlot's tree half:
// record.LiveRoots' answer for the tree and record.Issue.Lingers of its root), and the moment its
// root last entered the workflow. The root stands in the workflow in a visit: its visits end where
// it leaves the workflow or moves to done, where its linger begins, and begin again only with its
// entry, which a re-admission that clears the linger writes; and no linger of it stands (Linger,
// to its close or to the root's next entry). A root the trace holds no record of leaves the tree
// not live, as a root the pass's records do not hold leaves the issue out of the loop.
func (r *lineReader) treeLiveAt(tree string, at time.Time) (time.Time, bool) {
	root := r.of(tree)
	if root == nil || visitOf(root.iss, at) == nil {
		return time.Time{}, false
	}
	for _, linger := range root.iss.Lingers {
		if linger.At.After(at) {
			continue
		}
		end := linger.Closed
		if entry := slices.IndexFunc(root.iss.Entries, func(entry Entry) bool { return entry.At.After(linger.At) }); entry >= 0 &&
			(end.IsZero() || root.iss.Entries[entry].At.Before(end)) {
			end = root.iss.Entries[entry].At
		}
		if end.IsZero() || at.Before(end) {
			return time.Time{}, false
		}
	}
	if entry := entryOf(root.iss, at); entry != nil {
		return entry.At, true
	}
	return time.Time{}, true
}

// startingAt says whether x's issue, not yet started in its entry, stands in the line's starting
// part at at, and whether it is blocked there, as the daemon's line places it (record.Waiting's
// queued-root and first-phase loops): a queued root handed to Legion in todo whose Dispatch chain
// names no live root (the ancestry hold, underLiveRoot), or a todo child of a tree whose root
// stands in the workflow, waits on no person and holds its design gate open, under no accepted
// ancestor (HeldUnderAccepted, underAccepted). It is blocked, as admission.blocked names it, while a
// blocker its links name is not done (record.LineFacts.Held) or its own open children hold its
// first phase (record.LineFacts.ChildWaits): the children wait the daemon tells of it, which is the
// line's hold and no wait of the measures' (issueWaits).
func (r *lineReader) startingAt(x *issueIndex, at time.Time, tree string) (bool, bool) {
	state := x.shown.stateAt(at)
	if state == nil || state.Status != "todo" {
		return false, false
	}
	blocked := r.unfinished(state.BlockedBy, at) || x.waits.at(at) > 0
	if tree == x.iss.Key {
		return dispatch.CarriesLegionLabel(state.Labels) && !r.underLiveRoot(x, at), blocked
	}
	root := r.of(tree)
	if root == nil || visitOf(root.iss, at) == nil || root.person.at(at) > 0 || !root.gate.openAt(at) || r.underAccepted(x, at, tree) {
		return false, false
	}
	return true, blocked
}

// parentAt is the Dispatch parent the newest issue event of x's issue at or before at shows, ""
// for none.
func parentAt(x *issueIndex, at time.Time) string {
	if state := x.shown.stateAt(at); state != nil {
		return state.Parent
	}
	return ""
}

// underAccepted says whether an ancestor of x's issue, in tree, stands accepted at at, in retro or
// in merging or held from one of them (record.AcceptedAncestor over record.Ancestors): each issue
// its Dispatch parent chain names that the daemon has recorded, nearest first, up to tree's root,
// and the root itself where the chain leaves the records or loops before reaching it. A parent
// that is done is no acceptance and the walk crosses it (record.LineFacts.Parents).
func (r *lineReader) underAccepted(x *issueIndex, at time.Time, tree string) bool {
	visited := []string{x.iss.Key}
	for key := parentAt(x, at); key != "" && !slices.Contains(visited, key); {
		visited = append(visited, key)
		above := r.of(key)
		if above == nil || entryOf(above.iss, at) == nil {
			break
		}
		if acceptedAt(above.iss, at) {
			return true
		}
		if key == tree {
			return false
		}
		key = parentAt(above, at)
	}
	root := r.of(tree)
	return root != nil && !slices.Contains(visited, tree) && acceptedAt(root.iss, at)
}

// acceptedAt says whether iss stands accepted at at (record.Accepted): in a visit to retro or to
// merging, a hold from one of them keeping its visit.
func acceptedAt(iss *IssueTrace, at time.Time) bool {
	visit := visitOf(iss, at)
	return visit != nil && record.Accepted(record.Issue{Phase: visit.Phase})
}

// underLiveRoot says whether x's issue, a queued root, stands under a live root at at: an issue its
// Dispatch parent chain names, from its parent upward and other than itself, that is a tree's own
// root in a tree that is live (treeLiveAt). That is the ancestry hold (record.Issue.Ancestry,
// liveAncestry), which holds the root as an unreleased child of that tree.
func (r *lineReader) underLiveRoot(x *issueIndex, at time.Time) bool {
	visited := []string{x.iss.Key}
	for key := parentAt(x, at); key != "" && !slices.Contains(visited, key); {
		visited = append(visited, key)
		above := r.of(key)
		if above == nil {
			return false
		}
		if entry := entryOf(above.iss, at); entry != nil && (entry.Tree == "" || entry.Tree == key) {
			if _, live := r.treeLiveAt(key, at); live {
				return true
			}
		}
		key = parentAt(above, at)
	}
	return false
}

// unfinished says whether a blocker of blockedBy is not done at at: its Dispatch events show it in
// another status, or the window holds none of it.
func (r *lineReader) unfinished(blockedBy []string, at time.Time) bool {
	for _, key := range blockedBy {
		blocker := r.of(key)
		if blocker == nil {
			return true
		}
		if state := blocker.shown.stateAt(at); state == nil || state.Status != record.DoneStatus {
			return true
		}
	}
	return false
}

// holderState is the state of the slots reading x's issue, holding a slot, stands in at at
// (record.HolderStateOf over holderFactsAt).
func (r *lineReader) holderState(x *issueIndex, at time.Time, tree string, visit *Visit, waiting, held bool) record.HolderState {
	state, _ := record.HolderStateOf(holderFactsAt(x, at, tree, visit, waiting, held))
	return state
}

// holderFactsAt is what x's issue's trace says of it as a slot holder at at, in tree and visit,
// waiting on another issue or held: its turns, its hold, its root's wait on a person and its wait
// on another issue (record.HolderFacts), each with no time but its turn's. Nothing on its way to a
// worker shows in a trace, so a holder is working only in a turn.
func holderFactsAt(x *issueIndex, at time.Time, tree string, visit *Visit, waiting, held bool) record.HolderFacts {
	facts := record.HolderFacts{Issue: record.Issue{Key: x.iss.Key, Tree: tree}, WaitsOnIssue: waiting}
	if visit != nil {
		facts.Issue.Phase = visit.Phase
	}
	if held {
		facts.Issue.Phase = phase.Held
	}
	if x.turns.at(at) > 0 {
		facts.InTurn = []time.Time{at}
	}
	facts.Person = tree == x.iss.Key && x.person.at(at) > 0
	return facts
}
