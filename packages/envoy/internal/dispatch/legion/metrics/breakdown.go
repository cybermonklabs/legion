package metrics

import (
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// The per-issue Legion facts (dispatch://INFRA-2915): one issue's work hours shared out by phase,
// the work time they come out of, its review rounds, whether it is its tree's root and which tree
// it is in. ProjectFacts is the one computation of them. Every reader of these numbers calls it
// and keeps no rule of its own, so no two views of one issue can answer different numbers.
//
// The hours are shared out part by part, each hour counted once (dispatch://INFRA-979). They are
// the bars' hours (the work time less the queued time). An hour an ask held is waiting on a
// person. An hour a root waited while a child worked is the child's work, which the child's own
// facts count, so it is left out of the root's parts. Every other hour is its phase's. It reads
// the observed timeline or the status-history fallback alike, so it needs no observation.

const (
	breakdownPerson   = "awaiting_person"
	breakdownChildren = "waiting_on_children"
	breakdownOther    = "other"
)

// breakdownParts are the parts in the order a page draws them. Their ids are the board's columns
// (api/legion_board.go), with waiting on children and other beside them.
var breakdownParts = []struct {
	id, label string
	phases    []string
}{
	{"design", "Design", []string{phaseDesign}},
	{"planning", "Planning", []string{legion.PhasePlanning}},
	{"implementing", "Implementing", []string{legion.PhaseImplementing}},
	{"testing", "Testing", []string{legion.PhaseTesting}},
	{"reviewing", "Reviewing", []string{legion.PhaseReviewing}},
	{"accepting", "Accepting", []string{legion.PhaseAccepting}},
	{"merging", "Merging", legion.MergerPhases},
	{breakdownPerson, "Waiting on a person", []string{legion.PhaseAwaitingPerson}},
	{"held", "Held", []string{legion.PhaseHeld}},
	{breakdownOther, "Other", nil},
}

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
	take(breakdownPerson, view.claims(precedenceIndex(SegmentWaitingOnPerson)))
	take(breakdownChildren, view.claims(precedenceIndex(SegmentWaitingOnChildren)))
	children := spent[breakdownChildren]
	delete(spent, breakdownChildren)
	for _, part := range breakdownParts {
		if len(part.phases) > 0 {
			take(part.id, phaseSpansOf(view.timeline, part.phases...))
		}
	}
	spent[breakdownOther] += totalOf(rest)
	return spent, children
}

// IssueFacts is one issue's Legion facts: the hours by breakdown part (the board's column ids),
// the work time those hours are shared out of, the review rounds, whether the issue is its tree's
// root and which tree it is in. One issue with Legion phase work, open or closed, has one of
// these, whatever its hours are.
type IssueFacts struct {
	PhaseHours   map[string]float64 `json:"phase_hours"`
	WorkHours    float64            `json:"work_hours"`
	ReviewRounds int                `json:"review_rounds"`
	Root         bool               `json:"root"`
	Tree         string             `json:"tree"`
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
		out[key] = IssueFacts{
			PhaseHours:   parts,
			WorkHours:    c.workedOf(issue).row.Hours,
			ReviewRounds: reviewRounds(issue, c.timelineOf(issue)),
			Root:         c.isRoot(key),
			Tree:         c.treeRoot(key),
		}
	}
	return out
}
