package independent

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"
)

// rules.go is the reading of each finished issue's values: its work time, its hours on each bar of
// phase time with their segments, its rework and, for a root, its children; its idle hours by role
// and its slot wait are observed.go's. The words each rule is read from are quoted above it:
// dispatch://INFRA-1077's "The four Legion metrics" and "Rule details", whose rules the four
// measures besides idle time and slot wait keep, and R6 of dispatch://INFRA-2916 for the bars.

// The phases the daemon reports, written out here rather than taken from another package, so
// this reading stands on the words and not on another reader's constants.
const (
	phaseAdmitted       = "admitted"
	phaseAwaitingPerson = "awaiting_person"
	phasePlanning       = "planning"
	phaseImplementing   = "implementing"
	phaseTesting        = "testing"
	phaseReviewing      = "reviewing"
	phaseAccepting      = "accepting"
	phaseRetro          = "retro"
	phaseMerging        = "merging"
	phaseAwaitingMerge  = "awaiting_merge"
	phaseHeld           = "held"
)

// The two phases this reading tells apart inside the daemon's `admitted`: "A root in `admitted`
// that holds a slot is in design, not in the queue", and "Queue time is every observed stretch in
// which the issue's phase is `admitted` and it holds no slot".
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
	fieldPhase       = "phase"
	fieldSlot        = "slot"
	fieldWaitingOn   = "waiting_on"
	fieldTree        = "tree"
	fieldClaimPrefix = "claim."
)

// parkedStatuses are the statuses an issue sits parked in: "A park runs from its `backlog` or
// `icebox` status write until the next status write, or to the end of the issue's time."
var parkedStatuses = []string{"backlog", "icebox"}

// noRole is the seventh bar of phase time, the one no role works.
const noRole = "no_role"

// bars are phase time's seven bars, each with the phases it holds: "The bars are: architect,
// `design` and `accepting`; planner, `planning`; implementer, `implementing` and `retro`; tester,
// `testing`; reviewer, `reviewing`; merger, `merging` and `awaiting_merge`. Retro is on the
// implementer's bar because the daemon gives retro to the implementer" (R6), and the seventh, "no
// role: `awaiting_person` and `held`" (legion-phase-time.md). Idle time by role reads the same bars
// for the six roles (idleByRole).
var bars = []struct {
	role   string
	phases []string
}{
	{"architect", []string{phaseDesign, phaseAccepting}},
	{"planner", []string{phasePlanning}},
	{"implementer", []string{phaseImplementing, phaseRetro}},
	{"tester", []string{phaseTesting}},
	{"reviewer", []string{phaseReviewing}},
	{"merger", []string{phaseMerging, phaseAwaitingMerge}},
	{noRole, []string{phaseAwaitingPerson, phaseHeld}},
}

// The five segments each hour of a bar goes to, the first that holds: "Waiting on a person ... In
// a turn ... CI wait: not measured ... Waiting on children ... Idle: none of these."
const (
	segmentPerson = iota
	segmentTurn
	segmentCI
	segmentChildren
	segmentIdle
	segmentCount
)

// segmentNames are the five segments by the names the eval routes state them under.
var segmentNames = [segmentCount]string{"waiting_on_person", "in_a_turn", "ci_wait", "waiting_on_children", "idle"}

// childWorkingPhases are the phases "from planning to awaiting merge": a root with a child in one
// of them is waiting on that child.
var childWorkingPhases = []string{
	phasePlanning, phaseImplementing, phaseTesting, phaseReviewing, phaseAccepting,
	phaseRetro, phaseMerging, phaseAwaitingMerge,
}

// sentBackFrom is where a move to implementing is a send-back: "a move from `testing`,
// `reviewing`, `accepting` or the merger's phase to `implementing`", words that held `retro` among
// the merger's phases, which R6 puts on the implementer's bar (bars). sentBackStatuses is the same
// before observations, where the statuses say it: "moves from `testing`, `needs_review` or `retro`
// to `in_progress`", which is also when an `in_progress` marks implementing.
var (
	sentBackFrom     = []string{phaseTesting, phaseReviewing, phaseAccepting, phaseRetro, phaseMerging, phaseAwaitingMerge}
	sentBackStatuses = []string{"testing", "needs_review", "retro"}
)

// The four classes of a filing: "In the tree, at design", "In the tree, later", "Out of the
// tree" and "By the controller", by the names legion-splitting's series carry.
const (
	classAtDesign   = "in_tree_at_design"
	classLater      = "in_tree_later"
	classOutOfTree  = "out_of_tree"
	classController = "by_the_controller"
)

var classes = []string{classAtDesign, classLater, classOutOfTree, classController}

// phaseSpan is one stretch of one phase.
type phaseSpan struct {
	phase   string
	stretch span
}

// timeline is an issue's phases over its life: the event log's up to the first phase the daemon's
// sampler recorded, and the sampler's from there on (timelineOf). The review rounds, the send-back
// and the first review read which part each stretch is of.
type timeline struct {
	spans []phaseSpan
	// firstObserved is the index of the first span the sampler recorded, and len(spans) for an
	// issue the sampler never saw: the spans before it are the event log's.
	firstObserved int
}

// bar is one issue's part of one bar of phase time: its hours in each segment, and whether any
// stretch of its work time falls on the bar at all, which is what a bar's count of issues counts.
type bar struct {
	holds    bool
	segments [segmentCount]float64
}

// hours is the bar's whole time.
func (b bar) hours() float64 {
	total := 0.0
	for _, hours := range b.segments {
		total += hours
	}
	return total
}

// values is one finished issue's values, which the measures fold: whether it is its tree's root,
// and each value a measure counts.
type values struct {
	root bool
	// started is whether the issue has a first worker event, which its work time is from.
	started          bool
	workHours        float64
	bars             map[string]bar
	idle             map[string]float64
	slotWait         float64
	reviewRounds     int
	sentBack         bool
	afterFirstReview float64
	children         int
	// parked is the hours of the issue's work time it sat in each parked status ("minus any time
	// in `backlog` or `icebox`"), by status: what the work time left out, which no measure states
	// and the reading reports beside them.
	parked map[string]float64
}

// reading is one run over one population: the trees it placed the issues in, and the timelines,
// work times and slot stretches it has measured so far. A root reads its children's, so each is
// measured once.
type reading struct {
	population *population
	roots      map[string]string
	children   map[string][]string
	sampled    map[string][]phaseSpan
	timelines  map[string]timeline
	worked     map[string][]span
	slots      map[string][]span
}

// newReading places every issue of the population in its tree and gives each issue the ask turns
// that count for it.
func newReading(held *population) *reading {
	r := &reading{
		population: held,
		roots:      map[string]string{},
		children:   map[string][]string{},
		sampled:    map[string][]phaseSpan{},
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

// valuesOf is one issue's values.
func (r *reading) valuesOf(key string) values {
	worked := r.workedOf(key)
	_, started := r.firstWork(key)
	result := values{
		root:      r.rootOf(key) == key,
		started:   started,
		workHours: rounded(hoursOf(worked)),
		bars:      r.barsOf(key, worked),
		idle:      r.idleByRole(key, worked),
		slotWait:  rounded(r.slotWait(key)),
		children:  len(r.children[key]),
		parked:    map[string]float64{},
	}
	if at, works := r.firstWork(key); works {
		life := []span{{from: at, to: r.population.end(key)}}
		for _, status := range parkedStatuses {
			sat := markStretches(r.population.issues[key].statusMarks, r.population.end(key), func(value string) bool { return value == status })
			result.parked[status] = hoursOf(intersect(life, sat))
		}
	}
	result.reviewRounds, result.sentBack, result.afterFirstReview = r.rework(key, worked)
	return result
}

// workedOf is the issue's work time: "Hours from the issue's first worker event to its close,
// minus any time in `backlog` or `icebox` and any time back in the queue after it", and since
// dispatch://INFRA-1077's 9f994275a7 minus any hour out of the workflow before the close too
// (outOfWorkflow); an issue still open runs to the end of the window. An issue with no first
// worker event has none.
func (r *reading) workedOf(key string) []span {
	if held, measured := r.worked[key]; measured {
		return held
	}
	// Stored before a root reads its children's, and a child reads none of its root's, so no
	// measurement waits on itself.
	var worked []span
	if at, works := r.firstWork(key); works {
		end := r.population.end(key)
		life := []span{{from: at, to: end}}
		queued := phaseStretches(r.timelineOf(key).spans, phaseQueued)
		worked = subtract(subtract(subtract(life, r.parked(key)), queued), r.outOfWorkflow(key))
	}
	r.worked[key] = worked
	return worked
}

// parked is when the issue sat parked: each `backlog` or `icebox` status write until the next
// status write, "an event by any role inside that stretch does not end the park".
func (r *reading) parked(key string) []span {
	return markStretches(r.population.issues[key].statusMarks, r.population.end(key), func(value string) bool {
		return slices.Contains(parkedStatuses, value)
	})
}

// outOfWorkflow is when the issue was out of the workflow before its close, which the work time
// leaves out as it leaves out a park, so no bar holds it (dispatch://INFRA-1077's spec version 50,
// "Out of the workflow before the close", 9f994275a7): "With observations, that is every stretch
// whose observed phase is `done` before the close ... Before observations, it is the stretch from
// an `issue.closed` that a reopen follows, or from a `triage` status write, to the next status
// write." The sampler's part of the timeline gives the first, and the statuses its event log's
// part covers give the second. The close is the issue's last move to `done`, so the `done` its
// status history ends with, and any snapshot of it after, is the close and no stretch. It is no
// park: the parked hours are `backlog` and `icebox` alone (parked).
func (r *reading) outOfWorkflow(key string) []span {
	end := r.population.end(key)
	drawn := r.timelineOf(key)
	sampledFrom := end
	if drawn.firstObserved < len(drawn.spans) {
		sampledFrom = drawn.spans[drawn.firstObserved].stretch.from
	}
	out := phaseStretches(drawn.spans[drawn.firstObserved:], phaseDone)
	statuses := r.population.issues[key].statusMarks
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

// firstWork is the issue's first worker event: the first event of a session titled for the issue's
// own key, and its first handoff only where the issue's Legion work is handoffs alone: a handoff a
// worker wrote is among that role's events already (the base's rule since 8beac41a07).
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

// barsOf shares the issue's work time out over the seven bars, and each bar's hours over the five
// segments. "An hour that work time to merge leaves out is on no bar of phase time", so the bars
// share the work time and nothing else, and "Time in the queue is on no bar", which the work time
// has already left out.
func (r *reading) barsOf(key string, worked []span) map[string]bar {
	claims := [segmentCount][]span{
		segmentPerson:   r.personStretches(key),
		segmentTurn:     r.turnStretches(key),
		segmentChildren: r.childrenStretches(key),
	}
	drawn := r.timelineOf(key).spans
	shared := make(map[string]bar, len(bars))
	for _, one := range bars {
		rest := intersect(phaseStretches(drawn, one.phases...), worked)
		part := bar{holds: len(rest) > 0}
		// "Each hour of a phase goes to the first of these that holds", and idle is what none
		// of the others took. CI wait holds through no hour: "this segment is empty on every
		// bar".
		for segment := range segmentIdle {
			taken := intersect(rest, claims[segment])
			part.segments[segment] = rounded(hoursOf(taken))
			rest = subtract(rest, taken)
		}
		part.segments[segmentIdle] = rounded(hoursOf(rest))
		shared[one.role] = part
	}
	return shared
}

// personStretches is when the issue was waiting on a person: "the daemon recorded the root in
// `awaiting_person`, or an ask a session of the tree opened was on the person's turn (`ask.opened`
// or `ask.handed_back`, until `ask.answered`, `ask.resolved`, or `waitingOn.answeredAt`)". The asks
// are the ones opened on this issue by a session of its tree (keepTreeAsks); the recorded phase and
// the recorded wait are its root's, the phase read through the marks that name one (phaseMarks), so
// a `null` phase mark ends no wait.
func (r *reading) personStretches(key string) []span {
	held := r.population.issues[key]
	rootKey := r.rootOf(key)
	root := r.population.issues[rootKey]
	if root == nil {
		root, rootKey = held, key
	}
	stretches := markStretches(phaseMarks(root.observed[fieldPhase]), r.population.end(rootKey),
		func(value string) bool { return jsonString(value) == phaseAwaitingPerson })
	answered := answeredAts(root)
	for _, turn := range held.askTurns {
		// An answer the daemon recorded inside the turn ends the turn there.
		for _, at := range answered {
			if at.After(turn.from) && at.Before(turn.to) {
				turn.to = at
				break
			}
		}
		stretches = append(stretches, turn)
	}
	return normalize(stretches)
}

// turnStretches is when the issue was in a turn: "a role of the issue had claim state `working`",
// each role's claim holding from its mark to its next.
func (r *reading) turnStretches(key string) []span {
	held := r.population.issues[key]
	var stretches []span
	for field, marks := range held.observed {
		if strings.HasPrefix(field, fieldClaimPrefix) {
			stretches = append(stretches, markStretches(marks, r.population.end(key), claimWorking)...)
		}
	}
	return normalize(stretches)
}

// claimWorking is whether a recorded claim state is `working`.
func claimWorking(value string) bool {
	return jsonString(value) == "working"
}

// childrenStretches is when the issue was waiting on its children: "the issue is a root with a
// child in a phase from planning to awaiting merge", a child counting "only during its own work
// time, as work time to merge defines it: not while it is parked, back in the queue, or before its
// first worker event". The child's work time leaves out its hours out of the workflow as well, so a
// root waits on a child through none of them (9f994275a7).
func (r *reading) childrenStretches(key string) []span {
	if r.rootOf(key) != key {
		return nil
	}
	var stretches []span
	for _, child := range r.children[key] {
		working := phaseStretches(r.timelineOf(child).spans, childWorkingPhases...)
		stretches = append(stretches, intersect(working, r.workedOf(child))...)
	}
	return normalize(stretches)
}

// rework is the issue's review rounds, whether it was sent back, and its hours of implementing and
// testing after its first review.
//
// "the median review rounds per issue (entries into `reviewing`, or into status `needs_review`
// before observations) ... and the share of issues sent back at least once (a move from `testing`,
// `reviewing`, `accepting` or the merger's phase to `implementing`)". Before observations the
// rounds and the send-backs are read off the Dispatch status history by the fallback rule:
// "entries into `needs_review`, and moves from `testing`, `needs_review` or `retro` to
// `in_progress`". The statuses read so are those written before the sampler's part of the
// timeline begins, and the sampler's part is read for reviewing entries and moves to
// implementing from there on, a reviewing stretch the sampler first saw being no new entry where
// the event log's part ends in reviewing too: "An issue in flight when its first observation is
// stored takes its phases from observations only from that observation on" (the base's rule
// since 8beac41a07). An issue whose observations are only its closing mark has no sampler's part,
// so all of it is read off its statuses.
//
// "The first review": the first entry into status `needs_review` before the sampler's part
// begins, and else the first `reviewing` stretch of that part. The hours after it are the
// issue's work time from that moment on in implementing and testing, "an issue that reached no
// review counts as zero hours".
func (r *reading) rework(key string, worked []span) (int, bool, float64) {
	drawn := r.timelineOf(key)
	before := r.population.issues[key].statusMarks
	if drawn.firstObserved < len(drawn.spans) {
		from := drawn.spans[drawn.firstObserved].stretch.from
		if cut := slices.IndexFunc(before, func(recorded mark) bool { return !recorded.at.Before(from) }); cut >= 0 {
			before = before[:cut]
		}
	}
	rounds, sentBack := 0, false
	var firstReview *time.Time
	statuses := changes(before)
	for index, one := range statuses {
		if one.value == "needs_review" {
			rounds++
			if firstReview == nil {
				at := one.at
				firstReview = &at
			}
		}
		if index > 0 && one.value == "in_progress" && slices.Contains(sentBackStatuses, statuses[index-1].value) {
			sentBack = true
		}
	}
	for index := drawn.firstObserved; index < len(drawn.spans); index++ {
		one := drawn.spans[index]
		previous := ""
		if index > 0 {
			previous = drawn.spans[index-1].phase
		}
		if one.phase == phaseReviewing && previous != phaseReviewing {
			rounds++
		}
		if firstReview == nil && one.phase == phaseReviewing {
			at := one.stretch.from
			firstReview = &at
		}
		if one.phase == phaseImplementing && slices.Contains(sentBackFrom, previous) {
			sentBack = true
		}
	}
	after := 0.0
	if firstReview != nil {
		since := intersect(worked, []span{{from: *firstReview, to: r.population.end(key)}})
		after = hoursOf(intersect(since, phaseStretches(drawn.spans, phaseImplementing, phaseTesting)))
	}
	return rounds, sentBack, rounded(after)
}

// changes is the status marks with each repeat of the status before it left out: the moves the
// status history made.
func changes(marks []mark) []mark {
	var moved []mark
	for _, one := range marks {
		if len(moved) > 0 && moved[len(moved)-1].value == one.value {
			continue
		}
		moved = append(moved, one)
	}
	return moved
}

// timelineOf is the issue's phases over its life: "An issue in flight when its first observation
// is stored takes its phases from observations only from that observation on. Its hours before it
// come from these status rules" (the spec's "Statuses before observations", the base's rule since
// 8beac41a07). So the event log draws it up to the first phase the sampler recorded, and the
// sampler's marks from there on; an issue the sampler never saw is the event log's whole. "The
// work hours in between" a first worker event and the first phase mark are put "on the bar of that
// first phase": the event log's first step where one falls before the first observation, and the
// first observed phase where none does.
func (r *reading) timelineOf(key string) timeline {
	if held, drawn := r.timelines[key]; drawn {
		return held
	}
	observed := slices.Clone(r.sampledOf(key))
	steps := phaseSteps(r.logMarks(key))
	until := r.population.end(key)
	if len(observed) > 0 {
		until = observed[0].stretch.from
		if cut := slices.IndexFunc(steps, func(step logMark) bool { return !step.at.Before(until) }); cut >= 0 {
			steps = steps[:cut]
		}
	}
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

// sampledOf is the timeline the sampler's `phase` marks draw and nothing else: each mark that
// names a phase holds until the next mark that names one, and the last until the issue's end
// (phaseMarks). An `admitted` stretch is split in two: the part the issue held a slot of its own
// through is design, and the part it held none is the queue.
func (r *reading) sampledOf(key string) []phaseSpan {
	if held, drawn := r.sampled[key]; drawn {
		return held
	}
	marks := phaseMarks(r.population.issues[key].observed[fieldPhase])
	end := r.population.end(key)
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
	r.sampled[key] = drawn
	return drawn
}

// phaseMarks is the `phase` marks of an issue that name a phase, oldest first. A `null` mark is a
// read that no longer carried the issue's phase, which the sampler writes when the daemon stops
// reporting the issue: it names no phase and ends none, so the phase before it runs on through it
// to the next mark that names one, or to the issue's end, and the issue's bars still add up to its
// work time (the method texts of legion-phase-time and legion-work-time: "The phase and the slot
// that the issue held run on through a drop until the next observation that names a phase"). Both
// readers of the marks take them from here, the timeline (sampledOf) and the recorded wait on a
// person (personStretches).
func phaseMarks(marks []mark) []mark {
	var named []mark
	for _, recorded := range marks {
		if jsonString(recorded.value) != "" {
			named = append(named, recorded)
		}
	}
	return named
}

// slotStretches is when the issue held a slot of its own. A `slot` mark naming a slot holds it,
// and a mark that names none releases it in a read that still carries the issue. A read that no
// longer carries the issue writes `null` for every field of it at one moment, its phase among
// them, so a `null` slot mark beside a `null` phase mark is the daemon dropping the issue, not the
// issue giving its slot up: the slot is held on through it, to the first read that carries the
// issue again, its next phase mark that names a phase, which releases the slot unless the same
// read names one (the method texts of legion-phase-time and legion-work-time: "When the daemon
// stops reporting an issue ... that is a drop, not a release").
//
// It is measured once per issue and kept, as sampledOf keeps its timeline: two rules read it, the
// timeline's split of `admitted` (sampledOf) and the design phase's start before observations
// (logMarks).
func (r *reading) slotStretches(key string) []span {
	if kept, measured := r.slots[key]; measured {
		return kept
	}
	held := r.population.issues[key]
	// The slot mark and the phase mark one read wrote, by the moment the read wrote them at.
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
			// Carried again after a drop, with no slot mark: the issue's newest slot mark is
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
		stretches = append(stretches, span{from: from, to: r.population.end(key)})
	}
	r.slots[key] = normalize(stretches)
	return r.slots[key]
}

// The three things the event log says about an issue's phase, and the order two of them that fall
// at one moment are read in: a role's first event, the handoff that starts the next phase, and the
// status the workflow wrote with it, which is the last word.
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

// rolePhase is the phase each role's first event starts: "each role's phase starts at the first
// event of a session titled `Legion <role> · <KEY>` for the issue", until the issue is first sent
// back (phaseSteps). The merger's first event starts merging: retro is the implementer's, and the
// `retro` status the workflow writes as retro starts marks it (statusPhase; the base's rule since
// 8beac41a07).
var rolePhase = map[string]string{
	"architect":   phaseDesign,
	"planner":     phasePlanning,
	"implementer": phaseImplementing,
	"tester":      phaseTesting,
	"reviewer":    phaseReviewing,
	"merger":      phaseMerging,
}

// handoffStarts is the phase each handoff starts: "Each phase ends, and the next one starts, at the
// earlier of the phase's own handoff and the next role's first event", so the planner's `plan`
// starts implementing, the implementer's `implement` testing, the tester's `test` reviewing, and
// "Accepting starts at the `review` handoff".
var handoffStarts = map[string]string{
	"plan":      phaseImplementing,
	"implement": phaseTesting,
	"test":      phaseReviewing,
	"review":    phaseAccepting,
}

// phaseOrder is the workflow's order of the phases the event log names, which "No re-entry by a
// first event" reads.
var phaseOrder = []string{
	phaseDesign, phasePlanning, phaseImplementing, phaseTesting, phaseReviewing, phaseAccepting, phaseRetro,
	phaseMerging,
}

// loggedPhases is the timeline the event log's steps draw up to until: each step holds until the
// next phase change and the last until until.
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
// first event, each handoff, and each status write that marks a phase.
func (r *reading) logMarks(key string) []logMark {
	held := r.population.issues[key]
	var marks []logMark
	for role, at := range held.firstRoleEvent {
		if phase, works := rolePhase[role]; works && role != "architect" {
			marks = append(marks, logMark{at: at, source: fromRole, phase: phase})
		}
	}
	// "An issue has a design phase when a session titled `Legion architect · <its own key>` wrote
	// an event on it ... The design phase runs from the issue's first slot, or else from that
	// architect's first event, to the first planner event."
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
		return slices.Index(phaseOrder, a.phase) - slices.Index(phaseOrder, b.phase)
	})
	return marks
}

// phaseSteps is the phase the issue is in as the marks move it. "A role's first event starts that
// role's phase only when that phase is later in the workflow than the furthest phase the issue
// has yet reached ... A handoff, a status write or an observed phase may set the phase to any
// phase", and two marks of one moment are one change, whose last word is the phase.
func phaseSteps(marks []logMark) []logMark {
	var steps []logMark
	reached := -1
	sentBack := false
	for _, one := range marks {
		rank := slices.Index(phaseOrder, one.phase)
		// "After a send-back, the next phases come from status writes and handoffs": once the
		// `in_progress` written over `testing`, `needs_review` or `retro` sent the issue back, no
		// role's first event starts a phase (the base's rule since 687c7048d5).
		if one.source == fromRole && (sentBack || rank <= reached) {
			continue
		}
		if one.source == fromStatus && one.phase == phaseImplementing {
			sentBack = true
		}
		reached = max(reached, rank)
		last := len(steps) - 1
		if last >= 0 && steps[last].phase == one.phase {
			continue
		}
		if last >= 0 && steps[last].at.Equal(one.at) {
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

// statusPhase is the phase a status write marks for an issue the sampler never saw: "`testing` is
// testing, `needs_review` is reviewing until that round's `review` handoff and then accepting, ...
// and an `in_progress` after `testing`, `needs_review` or `retro` (a send-back) is implementing.
// Every other `in_progress`, such as admission's, and every `todo` mark no phase." `retro` marks
// retro, on the implementer's bar (bars). A park is no phase either: the work time leaves it out.
func statusPhase(previous, status string) (string, bool) {
	switch status {
	case "testing":
		return phaseTesting, true
	case "needs_review":
		return phaseReviewing, true
	case "retro":
		return phaseRetro, true
	case "in_progress":
		if slices.Contains(sentBackStatuses, previous) {
			return phaseImplementing, true
		}
	}
	return "", false
}

// rootOf is the issue's tree root: "When the daemon's state names an issue's `tree`, that names
// the top issue", and otherwise "the highest Legion issue up the parent chain. The walk stops below
// the first ancestor that is not a Legion issue ... An issue that is not itself a Legion issue, but
// whose parent is, belongs to its parent's tree."
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
		if above == nil || !(above.reported || above.worked()) {
			return at
		}
		at = held.parent
	}
	return at
}

// topOf is the top of an issue's parent chain, "walk up from that parent through each issue's
// parent as Dispatch records it, to the top", which is how a filing's tree is told; a parent the
// project does not hold is the top of its chain.
func (r *reading) topOf(key string) string {
	climbed := map[string]bool{}
	at := key
	for !climbed[at] {
		climbed[at] = true
		held := r.population.issues[at]
		if held == nil || held.parent == "" {
			return at
		}
		at = held.parent
	}
	return at
}

// keepTreeAsks gives each issue the ask turns that count for it: "An ask counts for an issue when
// it is opened on that issue by any Legion session of the issue's tree ... An ask opened by a
// session outside the tree does not count."
func (r *reading) keepTreeAsks() {
	for _, turn := range r.population.asks {
		held := r.population.issues[turn.on]
		if held == nil || turn.by == "" || r.rootOf(turn.on) != r.rootOf(turn.by) {
			continue
		}
		held.askTurns = append(held.askTurns, turn.stretch)
	}
	for _, key := range r.population.keys {
		slices.SortFunc(r.population.issues[key].askTurns, func(a, b span) int { return a.from.Compare(b.from) })
	}
}

// classOf is a filing's class: "In the tree, at design: a child of the filer's tree, created
// before the filer's issue had its first implementer event. In the tree, later: a child of the
// filer's tree, created after that ... Out of the tree: any other issue an architect or phase
// worker filed. By the controller: issues the controller filed." The new issue is in the filer's
// tree "when both walks reach the same top issue".
func (r *reading) classOf(filed filing) string {
	if filed.filer.controller {
		return classController
	}
	filer := r.population.issues[filed.filer.issue]
	if filer == nil || filed.parent == "" || r.topOf(filed.parent) != r.topOf(filer.key) {
		return classOutOfTree
	}
	if implemented, started := filer.firstRoleEvent["implementer"]; started && !filed.at.Before(implemented) {
		return classLater
	}
	return classAtDesign
}

// answeredAts are the moments the waits the daemon recorded of the issue name as answered
// (`waitingOn.answeredAt`), in order.
func answeredAts(held *issue) []time.Time {
	var answered []time.Time
	for _, recorded := range held.observed[fieldWaitingOn] {
		var wait struct {
			AnsweredAt string `json:"answeredAt"`
		}
		if err := json.Unmarshal([]byte(recorded.value), &wait); err != nil || wait.AnsweredAt == "" {
			continue
		}
		if at, err := time.Parse(time.RFC3339, wait.AnsweredAt); err == nil {
			answered = append(answered, at)
		}
	}
	slices.SortFunc(answered, func(a, b time.Time) int { return a.Compare(b) })
	return answered
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

// rounded keeps four decimals of a figure, under a second of an hour: enough that a sum of
// minutes reads exactly, and few enough that no answer carries the noise of a float.
func rounded(value float64) float64 {
	return math.Round(value*1e4) / 1e4
}
