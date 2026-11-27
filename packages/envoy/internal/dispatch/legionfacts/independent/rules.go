package independent

import (
	"encoding/json"
	"math"
	"slices"
	"time"
)

// rules.go is the reading of R1 itself: every issue the project has Legion phase work for, open
// or closed, with its hours by phase, its work time, its review rounds, whether it is its tree's
// root and which tree it is in.
//
// The words each rule is read from are quoted above it. Where R1 names a thing the Legion
// requirements define elsewhere — what a phase is, when an hour counts as waiting on a person,
// what the queue is — the quoted words are those requirements', and the reading of them is this
// file's own.

// The phases the daemon reports, written out here rather than taken from another package, so
// this reading stands on the words and not on another reader's constants.
const (
	phaseAdmitted         = "admitted"
	phaseAwaitingPerson   = "awaiting_person"
	phaseAwaitingChildren = "awaiting_children"
	phasePlanning         = "planning"
	phaseImplementing     = "implementing"
	phaseTesting          = "testing"
	phaseReviewing        = "reviewing"
	phaseAccepting        = "accepting"
	phaseRetro            = "retro"
	phaseMerging          = "merging"
	phaseAwaitingMerge    = "awaiting_merge"
	phaseHeld             = "held"
)

// The two phases this reading tells apart inside the daemon's `admitted`, which the daemon does
// not split: "Design: roots that hold a slot while their phase is still `admitted`", and "Time
// in the queue is on no bar", the queue being an admitted issue that holds no slot.
const (
	phaseDesign = "design"
	phaseQueued = "queued"
)

// phaseDone is the phase the daemon reports for an issue it has finished, and for one it holds
// out of the workflow before Dispatch closes it: such an hour before the close is out of the
// workflow (outOfWorkflow).
const phaseDone = "done"

// The two statuses a stretch out of the workflow starts at before observations: `triage`, and a
// `done` some other status follows, which is a close a reopen undid (outOfWorkflow).
const (
	statusTriage = "triage"
	statusDone   = "done"
)

// The fields the sampler records that these rules read.
const (
	fieldPhase     = "phase"
	fieldSlot      = "slot"
	fieldWaitingOn = "waiting_on"
	fieldTree      = "tree"
)

// parkedStatuses are the statuses an issue sits parked in, whose time the work time leaves out.
var parkedStatuses = []string{"backlog", "icebox"}

// The parts an issue's hours are shared out over beside the phase table: the hours an ask held,
// and the hours no named part took.
const (
	partPerson = "awaiting_person"
	partOther  = "other"
)

// parts is "hours by phase": one part per column of the board, each holding the daemon's phases
// that fall in it, and `other` for an hour of the work time in a phase no column names. Retro has
// a column of its own, merging holds merging and awaiting merge alone (dispatch://INFRA-1077's
// b74df7a79b), and `awaiting_children` has a column of its own for a parent waiting on its open
// children (dispatch://INFRA-3320), giving the board twelve columns. Any hour of that phase while
// a child is working is already removed with the root's child wait, so this part holds the rest.
var parts = []struct {
	id     string
	phases []string
}{
	{"design", []string{phaseDesign}},
	{"planning", []string{phasePlanning}},
	{"implementing", []string{phaseImplementing}},
	{"testing", []string{phaseTesting}},
	{"reviewing", []string{phaseReviewing}},
	{"accepting", []string{phaseAccepting}},
	{"retro", []string{phaseRetro}},
	{"merging", []string{phaseMerging, phaseAwaitingMerge}},
	{"awaiting_children", []string{phaseAwaitingChildren}},
	{partPerson, []string{phaseAwaitingPerson}},
	{"held", []string{phaseHeld}},
}

// childWorkingPhases are the phases "from planning to awaiting merge": a root with a child in
// one of them is waiting on that child. `awaiting_children` is one of them too: a child enters it
// while waiting on children of its own that are still at work, so its root waits on it.
var childWorkingPhases = []string{
	phasePlanning, phaseImplementing, phaseTesting, phaseReviewing, phaseAccepting,
	phaseRetro, phaseMerging, phaseAwaitingMerge, phaseAwaitingChildren,
}

// phaseSpan is one stretch of one phase.
type phaseSpan struct {
	phase   string
	stretch span
}

// timeline is an issue's phases over its life: the event log's up to the first phase the daemon's
// sampler recorded, and the sampler's from there on (timelineOf). The review rounds read which
// part each stretch is of.
type timeline struct {
	spans []phaseSpan
	// firstObserved is the index of the first span the sampler recorded, and len(spans) for an
	// issue the sampler never saw: the spans before it are the event log's.
	firstObserved int
}

// reading is one run over one population: the trees it placed the issues in, and the timelines,
// work times and slot stretches it has measured so far. A root reads its children's, so each is
// measured once and kept.
type reading struct {
	population *population
	roots      map[string]string
	children   map[string][]string
	timelines  map[string]timeline
	worked     map[string][]span
	slots      map[string][]span
}

// newReading places every issue of the population in its tree and gives each issue the ask turns
// that count for it, which is what the facts below are measured over.
func newReading(held *population) *reading {
	r := &reading{
		population: held,
		roots:      map[string]string{},
		children:   map[string][]string{},
		timelines:  map[string]timeline{},
		worked:     map[string][]span{},
		slots:      map[string][]span{},
	}
	for _, key := range held.keys {
		if root := r.rootOf(key); root != key {
			r.children[root] = append(r.children[root], key)
		}
	}
	r.keepTreeAsks()
	return r
}

// factsOf is one issue's five facts.
func (r *reading) factsOf(key string) Facts {
	// "Work time to merge: hours from the issue's first worker event to its close, minus any
	// time in `backlog` or `icebox` and any time back in the queue after it", and since
	// dispatch://INFRA-1077's 9f994275a7 minus any hour out of the workflow before the close
	// too. The stretches it leaves are the hours shared out by phase: "an hour that work time
	// leaves out is on no bar of phase time", and the queue is on none either.
	bars := r.workedOf(key)
	return Facts{
		PhaseHours:   r.phaseHours(key, bars),
		WorkHours:    rounded(hoursOf(total(bars))),
		ReviewRounds: r.reviewRounds(key),
		Root:         r.rootOf(key) == key,
		Tree:         r.rootOf(key),
	}
}

// phaseHours shares the issue's hours out over the parts, each hour counted once.
//
// The order is the order the requirements state: an hour an ask held is waiting on a person,
// whatever phase the issue was in; an hour a root waited while a child worked is the child's own
// work, which the child's facts count, so it is left out of the root's parts rather than counted
// twice; and every other hour is its phase's.
func (r *reading) phaseHours(key string, bars []span) map[string]float64 {
	rest := bars
	spent := map[string]time.Duration{}
	take := func(id string, claim []span) {
		taken := intersect(rest, claim)
		spent[id] += total(taken)
		rest = subtract(rest, taken)
	}
	take(partPerson, r.personStretches(key))
	rest = subtract(rest, r.childrenStretches(key))
	for _, part := range parts {
		take(part.id, phaseStretches(r.timelineOf(key).spans, part.phases...))
	}
	spent[partOther] += total(rest)
	hours := map[string]float64{}
	for id, covered := range spent {
		if measured := rounded(hoursOf(covered)); measured > 0 {
			hours[id] = measured
		}
	}
	return hours
}

// personStretches is when the issue was waiting on a person: "the daemon recorded the root in
// `awaiting_person`, or an ask a session of the tree opened was on the person's turn (`ask.opened`
// or `ask.handed_back`, until `ask.answered`, `ask.resolved`, or `waitingOn.answeredAt`)".
//
// The asks are the issue's own, the ones opened on it by a session of its tree (keepTreeAsks).
// The recorded phase is the root's, which is the issue the daemon records a tree's wait on, read
// through the rows that name a phase (phaseMarks): a `null` phase row ends no wait.
func (r *reading) personStretches(key string) []span {
	held := r.population.issues[key]
	root := r.population.issues[r.rootOf(key)]
	if root == nil {
		root = held
	}
	stretches := markStretches(phaseMarks(root.observed[fieldPhase]), root.end(r.population.window),
		func(value string) bool { return jsonString(value) == phaseAwaitingPerson })
	answered := answeredAts(root)
	for _, turn := range held.askTurns {
		// A wait the daemon recorded as answered inside the turn ends the turn there: the
		// person answered, whatever later event took the ask back.
		if at, found := firstWithin(answered, turn); found {
			turn.to = at
		}
		stretches = append(stretches, turn)
	}
	return stretches
}

// childrenStretches is when the issue was waiting on its children: "the issue is a root with a
// child in a phase from planning to awaiting merge", each child counted "only during its own
// work time, as work time to merge defines it: not while it is parked, back in the queue, or
// before its first worker event". The child's work time leaves out its hours out of the workflow
// as well, so a root waits on a child through none of them (9f994275a7). An issue that is not its
// tree's root waits on no children.
func (r *reading) childrenStretches(key string) []span {
	if r.rootOf(key) != key {
		return nil
	}
	var stretches []span
	for _, child := range r.children[key] {
		working := phaseStretches(r.timelineOf(child).spans, childWorkingPhases...)
		stretches = append(stretches, intersect(working, r.workedOf(child))...)
	}
	return stretches
}

// reviewRounds is "entries into `reviewing`, or into status `needs_review` before observations":
// the entries into `needs_review` of the statuses written before the sampler's part of the
// timeline begins, and the entries into reviewing of that part, a reviewing stretch the sampler
// first saw being no new entry where the event log's part ends in reviewing too (dispatch://
// INFRA-1077's spec, "Statuses before observations", as the base settled it in 8beac41a07).
func (r *reading) reviewRounds(key string) int {
	drawn := r.timelineOf(key)
	statuses := r.population.issues[key].statusMarks
	if drawn.firstObserved < len(drawn.spans) {
		from := drawn.spans[drawn.firstObserved].stretch.from
		if cut := slices.IndexFunc(statuses, func(recorded mark) bool { return !recorded.at.Before(from) }); cut >= 0 {
			statuses = statuses[:cut]
		}
	}
	rounds := statusEntries(statuses, "needs_review")
	for index := drawn.firstObserved; index < len(drawn.spans); index++ {
		if drawn.spans[index].phase == phaseReviewing && (index == 0 || drawn.spans[index-1].phase != phaseReviewing) {
			rounds++
		}
	}
	return rounds
}

// workedOf is the issue's work time: its life from its first worker event to its close — or to
// the moment of the computation while it is open — less the time it sat parked, the time it sat
// in the queue and the time it was out of the workflow (outOfWorkflow). An issue no Legion role
// ever worked has no work time at all.
func (r *reading) workedOf(key string) []span {
	if held, measured := r.worked[key]; measured {
		return held
	}
	// Measured before the answer is stored, and stored once: a root reads each child's work
	// time, and a child reads none of the root's, so no measurement waits on itself.
	var worked []span
	held := r.population.issues[key]
	if at, works := r.firstWork(key); works {
		end := held.end(r.population.window)
		life := []span{{from: at, to: end}}
		parked := markStretches(held.statusMarks, end, func(value string) bool {
			return slices.Contains(parkedStatuses, value)
		})
		queued := phaseStretches(r.timelineOf(key).spans, phaseQueued)
		worked = subtract(subtract(subtract(life, parked), queued), r.outOfWorkflow(key))
	}
	r.worked[key] = worked
	return worked
}

// outOfWorkflow is when the issue was out of the workflow before its close, which the work time
// leaves out as it leaves out a park, so no part holds it (dispatch://INFRA-1077's spec version 50,
// "Out of the workflow before the close", 9f994275a7): "With observations, that is every stretch
// whose observed phase is `done` before the close ... Before observations, it is the stretch from
// an `issue.closed` that a reopen follows, or from a `triage` status write, to the next status
// write." The sampler's part of the timeline gives the first, and the statuses its event log's
// part covers give the second. The close is the issue's last move to `done`, so the `done` its
// status history ends with, and any snapshot of it after, is the close and no stretch.
func (r *reading) outOfWorkflow(key string) []span {
	held := r.population.issues[key]
	end := held.end(r.population.window)
	drawn := r.timelineOf(key)
	sampledFrom := end
	if drawn.firstObserved < len(drawn.spans) {
		sampledFrom = drawn.spans[drawn.firstObserved].stretch.from
	}
	out := phaseStretches(drawn.spans[drawn.firstObserved:], phaseDone)
	statuses := held.statusMarks
	closing := len(statuses)
	for closing > 0 && statuses[closing-1].value == statusDone {
		closing--
	}
	until := end
	if closing < len(statuses) {
		until = statuses[closing].at
	}
	logged := markStretches(statuses[:closing], until, func(value string) bool {
		return value == statusTriage || value == statusDone
	})
	return normalize(append(out, intersect(logged, []span{{to: sampledFrom}})...))
}

// firstWork is where the issue's work time starts: "the issue's first worker event", the first
// event of any Legion role on it, and its first handoff only for an issue whose Legion work is
// handoffs alone (the base's rule since 8beac41a07: a handoff a worker wrote is among that role's
// events already).
func (r *reading) firstWork(key string) (time.Time, bool) {
	held := r.population.issues[key]
	var first time.Time
	for _, at := range held.firstRoleEvent {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	if !first.IsZero() {
		return first, true
	}
	for _, handed := range held.handoffs {
		if first.IsZero() || handed.at.Before(first) {
			first = handed.at
		}
	}
	return first, !first.IsZero()
}

// timelineOf is the issue's phases over its life: "An issue in flight when its first observation
// is stored takes its phases from observations only from that observation on. Its hours before it
// come from these status rules" (the spec's "Statuses before observations", which the base
// follows since 8beac41a07). So the event log draws it up to the first phase the sampler
// recorded, and the sampler's rows from there on; an issue the sampler never saw is the event
// log's whole.
func (r *reading) timelineOf(key string) timeline {
	if held, drawn := r.timelines[key]; drawn {
		return held
	}
	end := r.population.issues[key].end(r.population.window)
	observed := r.observedPhases(key)
	steps := phaseSteps(r.logMarks(key))
	until := end
	if len(observed) > 0 {
		until = observed[0].stretch.from
		if cut := slices.IndexFunc(steps, func(step logMark) bool { return !step.at.Before(until) }); cut >= 0 {
			steps = steps[:cut]
		}
	}
	// "An hour of the work time before the first phase mark is on that phase's bar", so no hour
	// of the work time is off every part: the first phase is the event log's first step where
	// one falls before the first observation, and the first observed phase where none does.
	if at, works := r.firstWork(key); works {
		switch {
		case len(steps) > 0 && at.Before(steps[0].at):
			steps[0].at = at
		case len(steps) == 0 && len(observed) > 0 && at.Before(observed[0].stretch.from):
			observed[0].stretch.from = at
		}
	}
	logged := loggedPhases(steps, until)
	drawn := timeline{spans: append(logged, observed...), firstObserved: len(logged)}
	r.timelines[key] = drawn
	return drawn
}

// observedPhases is the timeline the sampler's `phase` rows draw: each row that names a phase
// holds until the next row that names one, and the last until the issue's end (phaseMarks). An
// `admitted` stretch is split in two, because the daemon does not: the part the issue held a slot
// through is its design, and the part it held none is the waiting line.
func (r *reading) observedPhases(key string) []phaseSpan {
	held := r.population.issues[key]
	marks := phaseMarks(held.observed[fieldPhase])
	if len(marks) == 0 {
		return nil
	}
	end := held.end(r.population.window)
	slots := r.slotStretches(key)
	var drawn []phaseSpan
	for index, recorded := range marks {
		phase := jsonString(recorded.value)
		stretch := span{from: recorded.at, to: end}
		if index+1 < len(marks) {
			stretch.to = marks[index+1].at
		}
		if stretch.length() == 0 {
			continue
		}
		if phase != phaseAdmitted {
			drawn = append(drawn, phaseSpan{phase: phase, stretch: stretch})
			continue
		}
		holding := intersect([]span{stretch}, slots)
		for _, designing := range holding {
			drawn = append(drawn, phaseSpan{phase: phaseDesign, stretch: designing})
		}
		for _, waiting := range subtract([]span{stretch}, holding) {
			drawn = append(drawn, phaseSpan{phase: phaseQueued, stretch: waiting})
		}
	}
	slices.SortFunc(drawn, func(a, b phaseSpan) int { return a.stretch.from.Compare(b.stretch.from) })
	return drawn
}

// phaseMarks is the `phase` rows of an issue that name a phase, oldest first. A `null` row is a
// read that no longer carried the issue's phase, which the sampler writes when the daemon stops
// reporting the issue: it names no phase and ends none, so the phase before it runs on through it
// to the next row that names one, or to the issue's end (the spec's "Parked and pre-work hours in
// phase time", which the base reads so since 22bd759ecb). Both readers of the rows take them from
// here, the timeline (observedPhases) and the recorded wait on a person (personStretches).
func phaseMarks(marks []mark) []mark {
	var named []mark
	for _, recorded := range marks {
		if jsonString(recorded.value) != "" {
			named = append(named, recorded)
		}
	}
	return named
}

// slotStretches is when the issue held an admission slot of its own, from the slot the sampler
// recorded of it. A row naming a slot holds it, and a row that names none releases it in a read
// that still carries the issue. A read that no longer carries the issue writes `null` for every
// field of it at one moment, its phase among them, so a `null` slot row beside a `null` phase row
// is the daemon dropping the issue, not the issue giving its slot up: the slot is held on through
// it, to the first read that carries the issue again, its next row that names a phase, which
// releases the slot unless the same read names one (dispatch://INFRA-1077's e2a409a4f5, "an issue
// the daemon drops keeps the slot it held").
//
// It is measured once per issue and kept, as the timeline and the work time are: two rules read
// it.
func (r *reading) slotStretches(key string) []span {
	if kept, measured := r.slots[key]; measured {
		return kept
	}
	held := r.population.issues[key]
	// The slot row and the phase row one read wrote, by the moment the read wrote them at.
	type read struct{ slot, phase *mark }
	reads := map[int64]*read{}
	var moments []time.Time
	readAt := func(at time.Time) *read {
		if found, seen := reads[at.UnixNano()]; seen {
			return found
		}
		fresh := &read{}
		reads[at.UnixNano()] = fresh
		moments = append(moments, at)
		return fresh
	}
	for index := range held.observed[fieldSlot] {
		readAt(held.observed[fieldSlot][index].at).slot = &held.observed[fieldSlot][index]
	}
	for index := range held.observed[fieldPhase] {
		readAt(held.observed[fieldPhase][index].at).phase = &held.observed[fieldPhase][index]
	}
	slices.SortFunc(moments, time.Time.Compare)
	var stretches []span
	var from time.Time
	holding, gone := false, false
	for _, at := range moments {
		one := reads[at.UnixNano()]
		drops := one.phase != nil && jsonString(one.phase.value) == ""
		holds := holding
		switch {
		case one.slot != nil && one.slot.value != "" && one.slot.value != "null":
			holds = true
		case one.slot != nil && !drops:
			holds = false
		case one.slot == nil && one.phase != nil && !drops && gone:
			// Carried again after a drop, with no slot row: the issue's newest slot row is
			// still the drop's `null`, so this read names no slot, and it holds none.
			holds = false
		}
		if one.phase != nil {
			gone = drops
		}
		if holds && !holding {
			from = at
		}
		if !holds && holding {
			stretches = append(stretches, span{from: from, to: at})
		}
		holding = holds
	}
	if holding {
		stretches = append(stretches, span{from: from, to: held.end(r.population.window)})
	}
	r.slots[key] = normalize(stretches)
	return r.slots[key]
}

// The three things the event log says about an issue's phase, and the order two of them that
// fall at one moment are read in: a role's first event is the earliest word on a change, the
// handoff that starts the next phase is the next, and the status the workflow wrote with it is
// the last word.
const (
	fromRole = iota
	fromHandoff
	fromStatus
)

// logMark is one moment the event log says the issue's phase changed, and what says so.
type logMark struct {
	at     time.Time
	source int
	phase  string
}

// rolePhase is the phase each role's first event on the issue starts, until the issue is first
// sent back (phaseSteps). The merger's first event starts merging: retro is the implementer's,
// and the event log tells it from merging by the `retro` status the workflow writes as retro
// starts (statusPhase; the base's rule since 8beac41a07). The architect is not here: its design
// phase may open at the issue's first slot instead (logMarks).
var rolePhase = map[string]string{
	"planner":     phasePlanning,
	"implementer": phaseImplementing,
	"tester":      phaseTesting,
	"reviewer":    phaseReviewing,
	"merger":      phaseMerging,
}

// handoffStarts is the phase each handoff word starts: the planner's `plan` starts implementing,
// the implementer's `implement` starts testing, the tester's `test` starts reviewing and the
// reviewer's `review` starts accepting. "A handoff starts the next phase", so no hour falls
// between two phases. A word Legion never writes starts none.
var handoffStarts = map[string]string{
	"plan":      phaseImplementing,
	"implement": phaseTesting,
	"test":      phaseReviewing,
	"review":    phaseAccepting,
}

// phaseOrder is the order the workflow moves an issue through the phases the event log names.
// One rule reads it: a role that speaks again after its phase has passed does not send the issue
// back to it.
var phaseOrder = []string{
	phaseDesign, phasePlanning, phaseImplementing, phaseTesting, phaseReviewing,
	phaseAccepting, phaseRetro, phaseMerging,
}

// loggedPhases is the timeline the event log's steps draw up to until: "Each phase ends, and the
// next one starts, at the earlier of the phase's own handoff and the next role's first event ...
// So a handoff starts the next phase, and no hour between phases is left over ... After a
// send-back, the Dispatch status sets the phase."
func loggedPhases(steps []logMark, until time.Time) []phaseSpan {
	drawn := make([]phaseSpan, 0, len(steps))
	for index, step := range steps {
		stretch := span{from: step.at, to: until}
		if index+1 < len(steps) && steps[index+1].at.Before(until) {
			stretch.to = steps[index+1].at
		}
		if stretch.length() <= 0 {
			continue
		}
		drawn = append(drawn, phaseSpan{phase: step.phase, stretch: stretch})
	}
	return drawn
}

// logMarks is every word the event log holds about an issue's phase, in time order: each role's
// first event, each handoff, and each change of the issue's Dispatch status.
func (r *reading) logMarks(key string) []logMark {
	held := r.population.issues[key]
	var marks []logMark
	for role, at := range held.firstRoleEvent {
		if phase, works := rolePhase[role]; works {
			marks = append(marks, logMark{at: at, source: fromRole, phase: phase})
		}
	}
	// "An issue has a design phase when a session titled `Legion architect · <its own key>`
	// wrote an event on it ... The design phase runs from the issue's first slot, or else from
	// that architect's first event." A slot recorded after the architect had spoken is a later
	// admission, so the earlier of the two opens the phase.
	if at, designed := held.firstRoleEvent["architect"]; designed {
		if slots := r.slotStretches(key); len(slots) > 0 && slots[0].from.Before(at) {
			at = slots[0].from
		}
		marks = append(marks, logMark{at: at, source: fromRole, phase: phaseDesign})
	}
	for _, handed := range held.handoffs {
		if phase, starts := handoffStarts[handed.phase]; starts {
			marks = append(marks, logMark{at: handed.at, source: fromHandoff, phase: phase})
		}
	}
	previous := ""
	for _, recorded := range held.statusMarks {
		if recorded.value == previous {
			continue
		}
		if phase, names := statusPhase(previous, recorded.value); names {
			marks = append(marks, logMark{at: recorded.at, source: fromStatus, phase: phase})
		}
		previous = recorded.value
	}
	slices.SortFunc(marks, func(a, b logMark) int {
		if at := a.at.Compare(b.at); at != 0 {
			return at
		}
		if source := a.source - b.source; source != 0 {
			return source
		}
		return phaseRank(a.phase) - phaseRank(b.phase)
	})
	return marks
}

// phaseSteps is the phase the issue is in as the marks move it, changing only where a mark
// changes it.
func phaseSteps(marks []logMark) []logMark {
	var steps []logMark
	reached := -1
	sentBack := false
	for _, one := range marks {
		rank := phaseRank(one.phase)
		// A role's first event starts that role's phase only where the issue has not reached
		// that phase already: a role that speaks again after its phase has passed does not send
		// the issue back, while a handoff and a status do. And once the issue has been sent
		// back no role's first event starts a phase at all: "After a send-back, the next phases
		// come from status writes and handoffs" (the base's rule since 687c7048d5).
		if one.source == fromRole && (sentBack || rank <= reached) {
			continue
		}
		// The send-back is the `in_progress` written over `testing`, `needs_review` or `retro`,
		// the one status change statusPhase names implementing for.
		if one.source == fromStatus && one.phase == phaseImplementing {
			sentBack = true
		}
		reached = max(reached, rank)
		last := len(steps) - 1
		if last >= 0 && steps[last].phase == one.phase {
			continue
		}
		if last >= 0 && steps[last].at.Equal(one.at) {
			// Two marks of one moment are one change, and the last word on it is the phase.
			steps[last].phase = one.phase
			if last > 0 && steps[last-1].phase == one.phase {
				steps = steps[:last]
			}
			continue
		}
		steps = append(steps, one)
	}
	return steps
}

// phaseRank is where a phase stands in the workflow's order, and -1 for a phase the event log
// never names.
func phaseRank(phase string) int {
	return slices.Index(phaseOrder, phase)
}

// statusPhase is the phase a status change sets for an issue the sampler never saw: "After a
// send-back, the Dispatch status sets the phase: `in_progress` after `testing` or `needs_review`
// is implementing, `testing` is testing, and `needs_review` is reviewing until the `review`
// handoff of that round, then accepting until the status changes."
//
// The statuses that name no phase — `todo`, `triage`, `backlog`, `icebox` and `done` — leave the
// phase as it was, and so does the `in_progress` the workflow writes when it admits an issue:
// "A status write such as the `in_progress` of admission starts no phase."
func statusPhase(previous, status string) (string, bool) {
	switch status {
	case "testing":
		return phaseTesting, true
	case "needs_review":
		return phaseReviewing, true
	case "retro":
		return phaseRetro, true
	case "in_progress":
		if slices.Contains([]string{"testing", "needs_review", "retro"}, previous) {
			return phaseImplementing, true
		}
		return "", false
	}
	return "", false
}

// statusEntries counts how many times the issue's status became status.
func statusEntries(marks []mark, status string) int {
	entries := 0
	for index, recorded := range marks {
		if recorded.value == status && (index == 0 || marks[index-1].value != status) {
			entries++
		}
	}
	return entries
}

// rootOf is the issue's tree root: the tree the daemon named for it, where a read carried one,
// and otherwise the topmost issue of its parent chain that Legion works — one the daemon reports
// or one that carries Legion phase work. An issue whose parent is no Legion issue is a root of
// its own, which is what keeps a tree from climbing into the tracking issues above it.
func (r *reading) rootOf(key string) string {
	if root, walked := r.roots[key]; walked {
		return root
	}
	root := r.walkToRoot(key)
	r.roots[key] = root
	return root
}

// walkToRoot is the walk itself. It keeps the issues it has climbed through, so a parent chain
// that loops, and a named tree that points back into the chain, stop rather than run for ever.
func (r *reading) walkToRoot(key string) string {
	climbed := map[string]bool{}
	at := key
	for !climbed[at] {
		climbed[at] = true
		held := r.population.issues[at]
		if held == nil {
			return at
		}
		if held.tree != "" {
			if held.tree == at || climbed[held.tree] {
				return held.tree
			}
			at = held.tree
			continue
		}
		if held.parent == "" {
			return at
		}
		above := r.population.issues[held.parent]
		if above == nil || !isLegionIssue(above) {
			return at
		}
		at = held.parent
	}
	return at
}

// isLegionIssue is whether a tree may climb through the issue: the daemon reports it, or it
// carries Legion phase work.
func isLegionIssue(held *issue) bool {
	return held.reported || held.worked()
}

// keepTreeAsks gives each issue the ask turns that count for it: "An ask counts for an issue when
// it is opened on that issue by any Legion session of the issue's tree ... An ask opened by a
// session outside the tree does not count." A controller's session names a project and no issue,
// so it is of no tree and its asks count for nothing.
func (r *reading) keepTreeAsks() {
	for _, turn := range r.population.asks {
		held := r.population.issues[turn.on]
		if held == nil || turn.by == "" || r.rootOf(turn.on) != r.rootOf(turn.by) {
			continue
		}
		held.askTurns = append(held.askTurns, turn.stretch)
	}
	for _, key := range r.population.keys {
		slices.SortFunc(r.population.issues[key].askTurns, func(a, b span) int {
			return a.from.Compare(b.from)
		})
	}
}

// answeredAts are the moments the wait the daemon recorded of the issue names as answered, in
// order.
func answeredAts(held *issue) []time.Time {
	var answered []time.Time
	for _, recorded := range held.observed[fieldWaitingOn] {
		at, named := answeredAt(recorded.value)
		if !named {
			continue
		}
		answered = append(answered, at)
	}
	slices.SortFunc(answered, func(a, b time.Time) int { return a.Compare(b) })
	return answered
}

// answeredAt is the moment one recorded wait names as answered, and false where it names none or
// names it in a shape no daemon sends.
func answeredAt(value string) (time.Time, bool) {
	var wait struct {
		AnsweredAt string `json:"answeredAt"`
	}
	if err := json.Unmarshal([]byte(value), &wait); err != nil || wait.AnsweredAt == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, wait.AnsweredAt)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// firstWithin is the first of the moments that falls inside the stretch, which is where that
// stretch ends.
func firstWithin(moments []time.Time, stretch span) (time.Time, bool) {
	for _, at := range moments {
		if at.After(stretch.from) && at.Before(stretch.to) {
			return at, true
		}
	}
	return time.Time{}, false
}

// phaseStretches is every stretch of the timeline whose phase is one of phases.
func phaseStretches(drawn []phaseSpan, phases ...string) []span {
	var stretches []span
	for _, one := range drawn {
		if slices.Contains(phases, one.phase) {
			stretches = append(stretches, one.stretch)
		}
	}
	return normalize(stretches)
}

// rounded keeps four decimals of an hour figure, which is under a second: enough that a sum of
// minutes reads exactly, and few enough that no answer carries the noise of a float.
func rounded(hours float64) float64 {
	return math.Round(hours*1e4) / 1e4
}
