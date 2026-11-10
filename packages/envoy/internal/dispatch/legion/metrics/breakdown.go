package metrics

import (
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// The phase breakdown (dispatch://INFRA-979): where the finished issues' work hours went, by
// phase, each issue's and all of them together. The hours are the bars' (work time less queued),
// and two of Dispatch's own records come first: an hour an ask held is waiting on a person, and an
// hour a child worked through is waiting on children; every other hour is its phase's. It reads
// the observed timeline or the status-history fallback alike, so it needs no observation.

// BreakdownPhase is one part of the breakdown: its id (a board column id where the phase has one,
// so a page colours it as the issues table does), its label, its hours and its share.
type BreakdownPhase struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	Hours float64 `json:"hours"`
	Share float64 `json:"share"`
}

// BreakdownIssue is one finished issue's hours, by part id.
type BreakdownIssue struct {
	Key    string             `json:"key"`
	Title  string             `json:"title"`
	Hours  float64            `json:"hours"`
	Phases map[string]float64 `json:"phases"`
}

// PhaseBreakdown is the finished issues' hours by part, in the parts' order, and each issue's.
type PhaseBreakdown struct {
	Hours  float64          `json:"hours"`
	Phases []BreakdownPhase `json:"phases"`
	Issues []BreakdownIssue `json:"issues"`
}

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
	{breakdownChildren, "Waiting on children", nil},
	{"held", "Held", []string{legion.PhaseHeld}},
	{breakdownOther, "Other", nil},
}

// precedenceIndex is the segment's place in the precedence, which claims is keyed by.
func precedenceIndex(segment Segment) int {
	return slices.IndexFunc(precedence, func(rule segmentRule) bool { return rule.Segment == segment })
}

func (c *computation) phaseBreakdown(finished []*Issue) PhaseBreakdown {
	out := PhaseBreakdown{Phases: []BreakdownPhase{}, Issues: make([]BreakdownIssue, 0, len(finished))}
	totals := map[string]time.Duration{}
	var whole time.Duration
	for _, issue := range finished {
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
		for _, part := range breakdownParts {
			if len(part.phases) > 0 {
				take(part.id, phaseSpansOf(view.timeline, part.phases...))
			}
		}
		spent[breakdownOther] += totalOf(rest)
		row := BreakdownIssue{Key: issue.Key, Title: issue.Title, Phases: map[string]float64{}}
		var issueTotal time.Duration
		for id, spentHere := range spent {
			if spentHere <= 0 {
				continue
			}
			row.Phases[id] = hours(spentHere)
			totals[id] += spentHere
			issueTotal += spentHere
		}
		row.Hours = hours(issueTotal)
		whole += issueTotal
		out.Issues = append(out.Issues, row)
	}
	out.Hours = hours(whole)
	for _, part := range breakdownParts {
		spent := totals[part.id]
		if spent <= 0 {
			continue
		}
		phase := BreakdownPhase{ID: part.id, Label: part.label, Hours: hours(spent)}
		if whole > 0 {
			phase.Share = round(float64(spent) / float64(whole))
		}
		out.Phases = append(out.Phases, phase)
	}
	slices.SortFunc(out.Issues, func(a, b BreakdownIssue) int {
		switch {
		case a.Hours > b.Hours:
			return -1
		case a.Hours < b.Hours:
			return 1
		}
		return 0
	})
	return out
}
