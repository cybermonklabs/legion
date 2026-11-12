package metrics

import (
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// The per-issue Legion facts (dispatch://INFRA-2915): one issue's work hours shared out by phase,
// the work time they come out of, its review rounds, where it stands in its tree, how long it
// held an admission slot, and when it opened, closed and was worked. ProjectFacts is the one
// computation of them. Every reader of these numbers calls it and keeps no rule of its own, so no
// two views of one issue can answer different numbers.
//
// The hours are shared out part by part, each hour counted once (dispatch://INFRA-979). They are
// the bars' hours (the work time less the queued time). An hour an ask held is waiting on a
// person. An hour a root waited while a child worked is the child's work, which the child's own
// facts count, so it is left out of the root's parts. Every other hour is its phase's. It reads
// the observed timeline or the status-history fallback alike, so it needs no observation.

// The three parts issueParts names outside the table below. breakdownPerson is the board's
// awaiting-a-person column, which holds the hours an ask held as well as the hours of that
// phase. breakdownChildren and breakdownOther are no column of the board: the hours a root
// waited while a child worked, and the hours no other part took.
const (
	breakdownPerson   = legion.ColumnAwaiting
	breakdownChildren = "waiting_on_children"
	breakdownOther    = "other"
)

// breakdownParts are the parts in the order a page draws them, each with the phases whose hours
// it holds. Their ids are the board's columns, which the legion package declares
// (legion.ColumnDesign and the rest), with waiting on children and other beside them.
var breakdownParts = []struct {
	id     string
	phases []string
}{
	{legion.ColumnDesign, []string{phaseDesign}},
	{legion.ColumnPlanning, []string{legion.PhasePlanning}},
	{legion.ColumnImplementing, []string{legion.PhaseImplementing}},
	{legion.ColumnTesting, []string{legion.PhaseTesting}},
	{legion.ColumnReviewing, []string{legion.PhaseReviewing}},
	{legion.ColumnAccepting, []string{legion.PhaseAccepting}},
	{legion.ColumnMerging, legion.MergerPhases},
	{breakdownPerson, []string{legion.PhaseAwaitingPerson}},
	{legion.ColumnHeld, []string{legion.PhaseHeld}},
}

// personClaim and childrenClaim are the two places in the precedence issueParts takes its hours
// from, which claims is keyed by. The precedence is a package value, so each is one lookup at
// start rather than a scan of it per issue.
var (
	personClaim   = precedenceIndex(SegmentWaitingOnPerson)
	childrenClaim = precedenceIndex(SegmentWaitingOnChildren)
)

// precedenceIndex is the segment's place in the precedence, which claims is keyed by.
func precedenceIndex(segment Segment) int {
	return slices.IndexFunc(precedence, func(rule segmentRule) bool { return rule.Segment == segment })
}

// issueParts shares the issue's bar hours out over the breakdown's parts, each hour once: an
// ask's hours to waiting on a person, then each phase's own. The hours it waited while a child
// worked are the second answer, outside the parts.
func (c *computation) issueParts(issue *Issue) (map[string]time.Duration, time.Duration) {
	view := c.viewOf(issue)
	rest := normalize(c.barSpansOf(issue))
	spent := map[string]time.Duration{}
	take := func(id string, claim []Span) {
		taken := intersect(rest, claim)
		spent[id] += totalOf(taken)
		rest = subtract(rest, taken)
	}
	take(breakdownPerson, view.claims(personClaim))
	take(breakdownChildren, view.claims(childrenClaim))
	children := spent[breakdownChildren]
	delete(spent, breakdownChildren)
	for _, part := range breakdownParts {
		take(part.id, phaseSpansOf(view.timeline, part.phases...))
	}
	spent[breakdownOther] += totalOf(rest)
	return spent, children
}

// IssueFacts is one issue's Legion facts: when it opened and closed, the hours by breakdown part
// (the board's column ids), the work time those hours are shared out of, the review rounds,
// whether the issue is its tree's root, which tree it is in and how deep in it, how long it held
// an admission slot, and the first and last moment of its measured work. One issue with Legion
// phase work, open or closed, has one of these, whatever its hours are.
type IssueFacts struct {
	// OpenedAt and ClosedAt are the issue's own row: when it was created, and when it closed or
	// nil while it is open.
	OpenedAt time.Time  `json:"opened_at"`
	ClosedAt *time.Time `json:"closed_at"`

	PhaseHours   map[string]float64 `json:"phase_hours"`
	WorkHours    float64            `json:"work_hours"`
	ReviewRounds int                `json:"review_rounds"`
	Root         bool               `json:"root"`
	Tree         string             `json:"tree"`

	// TreeDepth is how many child_of links lie between the issue and the root Tree names, zero
	// for the root itself (Population.treeDepth).
	TreeDepth int `json:"tree_depth"`

	// SlotHours is how long the issue held an admission slot of its own, from its own `slot`
	// observations read as a history (Population.slotSpans): never from the daemon's
	// `admission.active`, which names every issue holding one, children included.
	SlotHours float64 `json:"slot_hours"`

	// WorkFirstAt and WorkLastAt are the first and the last moment of the issue's measured work,
	// the ends of the same bar spans PhaseHours is shared out of. Both nil for an issue whose
	// work took no measured time. They name the days an issue's work falls in.
	WorkFirstAt *time.Time `json:"work_first_at"`
	WorkLastAt  *time.Time `json:"work_last_at"`
}

// ProjectFacts is every issue of the population with Legion phase work, open or closed, and its
// facts, counted as the metrics count them: an open issue's hours run to the population's end.
// It is the one computation of the per-issue facts — every reader of the same numbers calls this
// and keeps no rule of its own, so a number can never differ between two views of it. A part with
// no hours is left out of PhaseHours, which is `{}` for an issue whose work took no measured
// time; the entry itself is there, so "has an entry" is the same question as "Legion worked it"
// (Population.hasPhaseWork).
func ProjectFacts(population *Population) map[string]IssueFacts {
	c := &computation{
		population: population,
		scope:      ScopeAll,
		phases:     map[string]phaseTimeline{},
		views:      map[string]*issueView{},
		roots:      map[string]string{},
		workTimes:  map[string]workTimeRow{},
		barSpans:   map[string][]Span{},
	}
	out := map[string]IssueFacts{}
	for _, key := range population.Keys {
		issue := population.Issues[key]
		if issue == nil || !population.hasPhaseWork(issue) {
			continue
		}
		spent, _ := c.issueParts(issue)
		parts := map[string]float64{}
		for id, here := range spent {
			if hours(here) > 0 {
				parts[id] = hours(here)
			}
		}
		facts := IssueFacts{
			OpenedAt:     issue.CreatedAt,
			ClosedAt:     issue.ClosedAt,
			PhaseHours:   parts,
			WorkHours:    c.workedOf(issue).row.Hours,
			ReviewRounds: reviewRounds(issue, c.timelineOf(issue)),
			Root:         c.isRoot(key),
			Tree:         c.treeRoot(key),
			TreeDepth:    c.treeDepth(key),
			SlotHours:    hours(totalOf(population.slotSpans(issue, population.To))),
		}
		if bars := normalize(c.barSpansOf(issue)); len(bars) > 0 {
			first, last := bars[0].From, bars[len(bars)-1].To
			facts.WorkFirstAt, facts.WorkLastAt = &first, &last
		}
		out[key] = facts
	}
	return out
}
