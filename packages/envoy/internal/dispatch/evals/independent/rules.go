package independent

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"
)

// rules.go is the reading of each finished issue's values: its work time, its hours on each bar of
// phase time with their segments, its idle hours by role, its slot wait, its rework and, for a
// root, its children. The words each rule is read from are quoted above it: R6 and D3 of
// dispatch://INFRA-2916 for idle time and slot wait, and dispatch://INFRA-1077's "The four Legion
// metrics" and "Rule details" for the other four, which keep that change's rules.

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

// bars are phase time's seven bars, each with the phases it holds: "architect (design and
// accepting), planner (planning), implementer (implementing), tester (testing), reviewer
// (reviewing) and merger (retro, merging and awaiting merge). A seventh bar, 'no role', holds the
// phases no role works: `awaiting_person` and `held`." D3 gives idle time by role the same bars
// for the six roles.
var bars = []struct {
	role   string
	phases []string
}{
	{"architect", []string{phaseDesign, phaseAccepting}},
	{"planner", []string{phasePlanning}},
	{"implementer", []string{phaseImplementing}},
	{"tester", []string{phaseTesting}},
	{"reviewer", []string{phaseReviewing}},
	{"merger", []string{phaseRetro, phaseMerging, phaseAwaitingMerge}},
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
// `reviewing`, `accepting` or the merger's phase to `implementing`". sentBackStatuses is the same
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

// timeline is an issue's phases over its life, and whether the daemon's sampler drew it. Where
// the sampler recorded no phase of the issue, the event log draws the timeline instead, and the
// review rounds, the send-back and the first review read which of the two they got.
type timeline struct {
	spans    []phaseSpan
	observed bool
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
}

// reading is one run over one population: the trees it placed the issues in, and the timelines
// and work times it has measured so far. A root reads its children's, so each is measured once.
type reading struct {
	population *population
	roots      map[string]string
	children   map[string][]string
	sampled    map[string][]phaseSpan
	timelines  map[string]timeline
	worked     map[string][]span
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
	}
	result.reviewRounds, result.sentBack, result.afterFirstReview = r.rework(key, worked)
	return result
}

// workedOf is the issue's work time: "Hours from the issue's first worker event to its close,
// minus any time in `backlog` or `icebox` and any time back in the queue after it", and an
// issue still open runs to the end of the window. An issue with no first worker event has none.
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
		worked = subtract(subtract(life, r.parked(key)), phaseStretches(r.timelineOf(key).spans, phaseQueued))
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

// firstWork is the issue's first worker event: the first event of a session titled for the issue's
// own key, and its first handoff where that comes first or the issue's Legion work is a handoff
// alone.
func (r *reading) firstWork(key string) (time.Time, bool) {
	held := r.population.issues[key]
	var first time.Time
	for _, at := range held.firstRoleEvent {
		if first.IsZero() || at.Before(first) {
			first = at
		}
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
// the recorded wait are its root's.
func (r *reading) personStretches(key string) []span {
	held := r.population.issues[key]
	rootKey := r.rootOf(key)
	root := r.population.issues[rootKey]
	if root == nil {
		root, rootKey = held, key
	}
	stretches := markStretches(root.observed[fieldPhase], r.population.end(rootKey),
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
// first worker event".
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

// idleByRole is D3's idle time by role: "for role R, the hours of the issue's worked spans (life −
// parked − queued) in which the issue holds its own slot, its observed phase is in R's bar ..., and
// R's newest observed claim state is not `working` (no claim counts as not working)". The phase is
// the observed one alone, the sampler's own marks, and the slot is the issue's own.
func (r *reading) idleByRole(key string, worked []span) map[string]float64 {
	end := r.population.end(key)
	holding := intersect(worked, r.slotStretches(key))
	sampled := r.sampledOf(key)
	idle := make(map[string]float64, len(bars)-1)
	for _, one := range bars {
		if one.role == noRole {
			continue
		}
		inBar := intersect(holding, phaseStretches(sampled, one.phases...))
		working := markStretches(r.population.issues[key].observed[fieldClaimPrefix+one.role], end, claimWorking)
		idle[one.role] = rounded(hoursOf(subtract(inBar, working)))
	}
	return idle
}

// slotWait is D3's slot wait: "the hours of the issue's observed life in which its phase is
// `admitted` and it holds no slot, over the whole life, with parked hours left out". The life is
// the issue's, from its creation to its end; the phase is the one the sampler recorded.
func (r *reading) slotWait(key string) float64 {
	held := r.population.issues[key]
	life := []span{{from: held.created, to: r.population.end(key)}}
	waited := intersect(phaseStretches(r.sampledOf(key), phaseQueued), life)
	return hoursOf(subtract(waited, r.parked(key)))
}

// rework is the issue's review rounds, whether it was sent back, and its hours of implementing and
// testing after its first review.
//
// "the median review rounds per issue (entries into `reviewing`, or into status `needs_review`
// before observations) ... and the share of issues sent back at least once (a move from `testing`,
// `reviewing`, `accepting` or the merger's phase to `implementing`)"; "An issue whose observations
// are only its closing mark has no observed review round. For it, count review rounds and
// send-backs from its Dispatch status history, by the fallback rule: entries into `needs_review`,
// and moves from `testing`, `needs_review` or `retro` to `in_progress`."
//
// "The first review. Where an issue has no observed phase, the first review is its first entry into
// status `needs_review`. Where it has one, the first review is the first `reviewing` stretch of the
// observed timeline." The hours after it are the issue's work time from that moment on in
// implementing and testing, "an issue that reached no review counts as zero hours".
func (r *reading) rework(key string, worked []span) (int, bool, float64) {
	drawn := r.timelineOf(key)
	marks := r.population.issues[key].statusMarks
	rounds, sentBack := 0, false
	var firstReview *time.Time
	if drawn.observed {
		for index, one := range drawn.spans {
			previous := ""
			if index > 0 {
				previous = drawn.spans[index-1].phase
			}
			if one.phase == phaseReviewing && previous != phaseReviewing {
				rounds++
				if firstReview == nil {
					firstReview = &one.stretch.from
				}
			}
			if one.phase == phaseImplementing && slices.Contains(sentBackFrom, previous) {
				sentBack = true
			}
		}
	} else {
		statuses := changes(marks)
		for index, one := range statuses {
			if one.value == "needs_review" {
				rounds++
				if firstReview == nil {
					firstReview = &one.at
				}
			}
			if index > 0 && one.value == "in_progress" && slices.Contains(sentBackStatuses, statuses[index-1].value) {
				sentBack = true
			}
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

// timelineOf is the issue's phases over its life: the sampler's own where it recorded any phase of
// the issue, with "the work hours in between" a first worker event and the first phase mark put
// "on the bar of that first phase", and the event log's where it recorded none.
func (r *reading) timelineOf(key string) timeline {
	if held, drawn := r.timelines[key]; drawn {
		return held
	}
	drawn := timeline{spans: slices.Clone(r.sampledOf(key)), observed: true}
	if len(drawn.spans) == 0 {
		drawn = timeline{spans: r.loggedPhases(key)}
	} else if at, works := r.firstWork(key); works && at.Before(drawn.spans[0].stretch.from) {
		drawn.spans[0].stretch.from = at
	}
	r.timelines[key] = drawn
	return drawn
}

// sampledOf is the timeline the sampler's `phase` marks draw and nothing else: each mark holds
// until the next, and the last until the issue's end. An `admitted` stretch is split in two: the
// part the issue held a slot of its own through is design, and the part it held none is the queue.
func (r *reading) sampledOf(key string) []phaseSpan {
	if held, drawn := r.sampled[key]; drawn {
		return held
	}
	marks := r.population.issues[key].observed[fieldPhase]
	end := r.population.end(key)
	slots := r.slotStretches(key)
	var drawn []phaseSpan
	for index, recorded := range marks {
		phase := jsonString(recorded.value)
		if phase == "" {
			continue
		}
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

// slotStretches is when the issue held a slot of its own: a `slot` mark naming a slot holds until
// the mark that names none.
func (r *reading) slotStretches(key string) []span {
	return markStretches(r.population.issues[key].observed[fieldSlot], r.population.end(key), func(value string) bool {
		return value != "" && value != "null"
	})
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
// event of a session titled `Legion <role> · <KEY>` for the issue". The merger's phases cannot be
// told apart in the event log, so its first event starts the first of them.
var rolePhase = map[string]string{
	"architect":   phaseDesign,
	"planner":     phasePlanning,
	"implementer": phaseImplementing,
	"tester":      phaseTesting,
	"reviewer":    phaseReviewing,
	"merger":      phaseRetro,
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
}

// loggedPhases is the timeline the event log draws for an issue the sampler never saw. Each mark
// holds until the next phase change and the last until the issue's end; an hour of the work time
// before the first mark is on the phase the issue is first seen in.
func (r *reading) loggedPhases(key string) []phaseSpan {
	steps := phaseSteps(r.logMarks(key))
	if len(steps) == 0 {
		return nil
	}
	if at, works := r.firstWork(key); works && at.Before(steps[0].at) {
		steps[0].at = at
	}
	end := r.population.end(key)
	drawn := make([]phaseSpan, 0, len(steps))
	for index, step := range steps {
		stretch := span{from: step.at, to: end}
		if index+1 < len(steps) {
			stretch.to = steps[index+1].at
		}
		if stretch.length() == 0 {
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
	for _, one := range marks {
		rank := slices.Index(phaseOrder, one.phase)
		if one.source == fromRole && rank <= reached {
			continue
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
// testing, `needs_review` is reviewing until that round's `review` handoff and then accepting,
// `retro` is the merger's phase ... and an `in_progress` after `testing`, `needs_review` or `retro`
// (a send-back) is implementing. Every other `in_progress`, such as admission's, and every `todo`
// mark no phase." A park is no phase either: the work time leaves it out.
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
