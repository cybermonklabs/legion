package metrics

import (
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// closed.go is the facts as a stored row holds them (dispatch://INFRA-2916, R21): each issue's
// facts closed at the moment of the newest input they read, with how fast each grows after it,
// so a row that no input moved is the same row whatever the clock says, and a reader gives each
// fact at its own clock (Grown) with no second rule.
//
// What makes that possible is the shape of the computation. Every fact is span algebra over the
// issue's inputs (timeline.go's normalize, intersect and subtract, with no length that decides a
// rule), and every span still open ends at the population's To, through issueEnd or the slot
// hours' end. Past the newest input an issue's facts read, no span starts, ends or changes
// phase, so each hours fact grows by a constant whole number of hours an hour, the last moment
// of a bar still open is the clock itself, and every count and every moment else stands still.
// The stored row is therefore two numbers a fact, its value at that moment and its rate, and
// ProjectFactsClosed measures both from the one computation (factsOf) run at two moments after
// it. It is the one caller of that computation: a reader of the facts takes them closed and
// grows them to its own clock (Grown).

// growthSpan is how far apart the two moments are that each fact's growth is measured between.
// An hour makes each whole-number rate an exact number of the four decimals every hours value is
// rounded to (round), so a rate carries no noise of its own.
const growthSpan = time.Hour

// ClosedFacts is one issue's facts closed at AsOf, the newest input its facts read (newestInput),
// and how fast each grows after it (Growth). The facts at any moment after AsOf are the facts
// grown by the time between (IssueFacts.Grown), which is the value the computation (factsOf)
// answers of the same inputs at that moment.
//
// The facts are the ones the computation answers just after AsOf: a span an input opened at AsOf
// itself holds no time at AsOf and does after it, so a count that span starts - a review round, a
// bar that holds hours, the first moment of measured work - is answered as every later moment
// answers it.
type ClosedFacts struct {
	IssueFacts
	AsOf   time.Time
	Growth Growth
}

// FactsAt is the issue's facts at moment: the facts grown by the time from AsOf to it (Grown),
// which a moment before AsOf closes back. A reader that must not shrink a row read behind its
// writer's clock guards that itself (legionfacts' Row.At).
func (c ClosedFacts) FactsAt(moment time.Time) IssueFacts {
	return c.Grown(c.Growth, moment.Sub(c.AsOf))
}

// ProjectFactsClosed is the per-issue facts as a stored row holds them: every issue of the
// population with Legion phase work that wanted names (every one where wanted is nil, by the one
// selection of them, factsIssues), its facts closed at the newest input they read and their
// growth after it. Its facts are the one computation's (factsOf), so no second rule computes a
// fact (R20).
//
// Each issue is computed at two moments past its newest input, an hour apart, and its facts are
// closed back at that input from the first of them with the rate the two measured (Grown with a
// negative elapsed). The first moment is the UTC midnight after the input, so the issues whose
// newest input falls on one day are computed in one computation at each moment, and a whole
// project costs two computations a day of its history's newest inputs rather than two an issue.
// The moment is a function of the input alone, and the facts of an issue read the inputs of its
// own tree alone, so one issue's row is the same whichever population it was computed in: a
// pass over its tree and a whole read of its project write the same values, and a pass whose
// inputs did not move writes none.
func ProjectFactsClosed(population *Population, wanted map[string]bool) map[string]ClosedFacts {
	base := newComputation(population)
	asOf := map[string]time.Time{}
	byMoment := map[time.Time][]*Issue{}
	for issue := range population.factsIssues(wanted) {
		newest := base.newestInput(issue)
		asOf[issue.Key] = newest
		at := closingMoment(newest)
		byMoment[at] = append(byMoment[at], issue)
	}
	closed := make(map[string]ClosedFacts, len(asOf))
	for at, issues := range byMoment {
		first := newComputation(population.at(at))
		later := newComputation(population.at(at.Add(growthSpan)))
		first.roots, later.roots = base.roots, base.roots
		for _, issue := range issues {
			facts := first.factsOf(issue)
			growth := growthBetween(facts, later.factsOf(issue))
			closed[issue.Key] = ClosedFacts{
				IssueFacts: facts.Grown(growth, asOf[issue.Key].Sub(at)),
				AsOf:       asOf[issue.Key],
				Growth:     growth,
			}
		}
	}
	return closed
}

// closingMoment is the first of the two moments an issue whose newest input is asOf is computed
// at: the UTC midnight after it, which is after asOf and at most a day on.
func closingMoment(asOf time.Time) time.Time {
	return measures.Midnight(asOf).AddDate(0, 0, 1)
}

// growthBetween is how fast each fact grew from first to later, which were computed growthSpan
// apart: each hours fact's rate, and each moment of measured work that moved, by its name
// (Growth). A fact that did not move has no entry.
func growthBetween(first, later IssueFacts) Growth {
	growth := Growth{}
	before, after := first.hoursFacts(), later.hoursFacts()
	for name, value := range after {
		if rate := round((value - before[name]) / growthSpan.Hours()); rate != 0 {
			growth[name] = rate
		}
	}
	for name, value := range before {
		if _, kept := after[name]; !kept && value != 0 {
			growth[name] = round(-value / growthSpan.Hours())
		}
	}
	for _, name := range []string{growthWorkFirstAt, growthWorkLastAt} {
		from, to := *first.workMoment(name), *later.workMoment(name)
		if from == nil || to == nil {
			continue
		}
		if rate := to.Sub(*from).Hours() / growthSpan.Hours(); rate != 0 {
			growth[name] = rate
		}
	}
	return growth
}

// newestInput is the moment an issue's facts are closed at: the newest input any rule of its
// facts reads, and never later than the population's To. Past it, nothing the facts read moves
// but the clock.
//
// The issue's own inputs are all of them: its opening and close, its merge, its role events, its
// handoffs, its statuses, every observation of it and its ask turns. Two rules read another
// issue, and each takes only the inputs that rule reads, at or before the issue's own end, since
// every span of the issue ends there:
//
//   - A child's time waiting on a person is the stretches its root was recorded in
//     `awaiting_person` (waitingOnPersonSpans), so a child reads the root's phase marks that open
//     or close such a stretch, the root's close where the last of them leaves one open, and the
//     root's recorded answer that cuts one of the child's own ask turns short (answeredWithin).
//     A `null` phase mark opens and closes no stretch (phaseStretches), so it is skipped here as
//     the timeline skips it, and the mark after it that carries a phase, or the root's close, is
//     what ends the wait.
//   - A root's time waiting on its children is its children's working phases over their work
//     time (waitingOnChildrenSpans), so a root reads each child's role events, handoffs, statuses,
//     `phase` and `slot` observations and close, which are the whole of what those spans are made
//     of (Phases and workedOf).
//
// An input of another issue that no rule of this one reads moves nothing of its facts, and is
// left out so that it moves nothing of its row either: a row closed at a later moment holds other
// values of the same growing facts, and would be written for a change that changed nothing.
func (c *computation) newestInput(issue *Issue) time.Time {
	population := c.population
	end := population.issueEnd(issue)
	var newest time.Time
	take := func(at time.Time) {
		if at.After(newest) {
			newest = at
		}
	}
	read := func(at time.Time) {
		if !at.After(end) {
			take(at)
		}
	}
	take(issue.CreatedAt)
	for _, at := range []*time.Time{issue.ClosedAt, issue.MergedAt} {
		if at != nil {
			take(*at)
		}
	}
	takeWork(issue, take)
	for _, marks := range issue.Observed {
		for _, mark := range marks {
			take(mark.At)
		}
	}
	for _, turn := range issue.AskTurns {
		take(turn.From)
		take(turn.To)
	}
	root := c.rootOf(issue)
	// The answer that cuts one of the issue's own ask turns short, the first strictly inside it,
	// which is the answer the waiting-on-a-person rule reads (waitingOnPersonSpans); a later
	// answer inside the same turn ends nothing and moves no fact.
	answered := answeredAts(root)
	for _, turn := range issue.AskTurns {
		if turn.To.IsZero() {
			turn.To = end
		}
		if at, found := answeredWithin(answered, turn); found {
			read(at)
		}
	}
	if root.Key == issue.Key {
		for _, key := range issue.Children {
			child := population.Issues[key]
			if child == nil {
				continue
			}
			takeWork(child, read)
			for _, field := range []string{legion.FieldPhase, legion.FieldSlot} {
				for _, mark := range child.Observed[field] {
					read(mark.At)
				}
			}
			if child.ClosedAt != nil {
				read(*child.ClosedAt)
			}
		}
	} else {
		awaiting := false
		for _, mark := range root.Observed[legion.FieldPhase] {
			phase := markedPhase(mark)
			if phase == "" {
				continue
			}
			now := phase == legion.PhaseAwaitingPerson
			if now || awaiting {
				read(mark.At)
			}
			awaiting = now
		}
		if awaiting && root.ClosedAt != nil {
			read(*root.ClosedAt)
		}
	}
	if newest.After(population.To) {
		return population.To
	}
	return newest
}

// takeWork hands take every moment of the issue's work its phase timeline and its work time read
// besides the observations: its role events, its handoffs and its statuses.
func takeWork(issue *Issue, take func(time.Time)) {
	for _, at := range issue.FirstRoleEvent {
		take(at)
	}
	for _, handoff := range issue.Handoffs {
		take(handoff.At)
	}
	for _, mark := range issue.StatusMarks {
		take(mark.At)
	}
}
