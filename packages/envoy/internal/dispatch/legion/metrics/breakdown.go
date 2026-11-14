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
// an admission slot, and the first and last moment of its measured work. Beside them are the
// per-issue parts every Legion measure folds (dispatch://INFRA-2916, R17): the work time with what
// it took out (workedOf), the issue's part of each bar (barPartsOf), its rework (reworkOf), the
// children metric 4 counts for a root, and the two rules of D3 (slotWaitHours and idleByRole).
// One issue with Legion phase work, open or closed, has one of these, whatever its hours are.
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

	// ParkedHours and QueuedHours are what the work time took out of the issue's life, and
	// StartedAt and MergedAt are its first worker event and the earliest merge of the pull
	// requests Dispatch holds for it: the rest of metric 1's row. Both are nil for an issue with no
	// work time, and MergedAt for one whose merge Dispatch holds none of.
	ParkedHours float64    `json:"parked_hours"`
	QueuedHours float64    `json:"queued_hours"`
	StartedAt   *time.Time `json:"started_at"`
	MergedAt    *time.Time `json:"merged_at"`

	// Bars is the issue's part of each of the seven bars of metric 2, keyed by role, every bar
	// present (barPartsOf).
	Bars map[string]BarPart `json:"bars"`

	// SentBack and the two hours after the first review are the rest of metric 3's row
	// (reworkOf), whose review rounds are ReviewRounds above.
	SentBack                          bool    `json:"sent_back"`
	ImplementingHoursAfterFirstReview float64 `json:"implementing_hours_after_first_review"`
	TestingHoursAfterFirstReview      float64 `json:"testing_hours_after_first_review"`

	// Children is how many issues of the project have this issue as their tree root, which is
	// what metric 4 counts for a root that finished (Issue.Children).
	Children int `json:"children"`

	// SlotWaitHours is the hours of the issue's observed life it stood admitted with no slot of
	// its own, parked hours left out (slotWaitHours), and IdleByRole the hours of its worked spans
	// it held its own slot in a role's bar with that role out of a turn, for each of the six roles
	// (idleByRole).
	SlotWaitHours float64            `json:"slot_wait_hours"`
	IdleByRole    map[string]float64 `json:"idle_by_role"`
}

// ProjectFacts is every issue of the population with Legion phase work, open or closed, and its
// facts, counted as the metrics count them: an open issue's hours run to the population's end.
// It is the one computation of the per-issue facts — every reader of the same numbers calls this
// and keeps no rule of its own, so a number can never differ between two views of it. A part with
// no hours is left out of PhaseHours, which is `{}` for an issue whose work took no measured
// time; the entry itself is there, so "has an entry" is the same question as "Legion worked it"
// (Population.hasPhaseWork).
func ProjectFacts(population *Population) map[string]IssueFacts {
	c := newComputation(population)
	out := map[string]IssueFacts{}
	for _, key := range population.Keys {
		issue := population.Issues[key]
		if issue == nil || !population.hasPhaseWork(issue) {
			continue
		}
		out[key] = c.factsOf(issue)
	}
	return out
}

// factsOf is one issue's facts: ProjectFacts answers them for every issue with Legion phase work,
// which legionfacts stores and the Legion evals' day builders fold.
func (c *computation) factsOf(issue *Issue) IssueFacts {
	population, key := c.population, issue.Key
	spent, _ := c.issueParts(issue)
	parts := map[string]float64{}
	for id, here := range spent {
		if hours(here) > 0 {
			parts[id] = hours(here)
		}
	}
	work := c.workedOf(issue)
	rework := c.reworkOf(issue)
	sampled := population.sampledPhases(issue)
	facts := IssueFacts{
		OpenedAt:                          issue.CreatedAt,
		ClosedAt:                          issue.ClosedAt,
		PhaseHours:                        parts,
		WorkHours:                         work.hours,
		ReviewRounds:                      rework.reviewRounds,
		Root:                              c.isRoot(key),
		Tree:                              c.treeRoot(key),
		TreeDepth:                         c.treeDepth(key),
		SlotHours:                         hours(totalOf(population.slotSpans(issue, population.To))),
		ParkedHours:                       work.parked,
		QueuedHours:                       work.queued,
		Bars:                              c.barPartsOf(issue),
		SentBack:                          rework.sentBack,
		ImplementingHoursAfterFirstReview: rework.implementingAfterFirst,
		TestingHoursAfterFirstReview:      rework.testingAfterFirst,
		Children:                          len(issue.Children),
		SlotWaitHours:                     c.slotWaitHours(issue, sampled),
		IdleByRole:                        c.idleByRole(issue, sampled),
	}
	if work.ok {
		started := work.started
		facts.StartedAt, facts.MergedAt = &started, issue.MergedAt
	}
	if bars := normalize(c.barSpansOf(issue)); len(bars) > 0 {
		first, last := bars[0].From, bars[len(bars)-1].To
		facts.WorkFirstAt, facts.WorkLastAt = &first, &last
	}
	return facts
}

// IssueFiling is one issue a Legion session of the population's project filed, as the stored
// filings hold it (dispatch://INFRA-2916, R17): the filing as metric 4 classes it (classify) and
// the filed issue's tree root as it stands now, which is what the roots and children cuts read a
// filing by (InCut). A filed issue the project does not hold is the root of a tree of its own
// here, as metric 4 counts it.
type IssueFiling struct {
	FiledIssue
	Root string `json:"root"`
}

// ProjectFilings is every filing the population read (Population.Filed), each classified by the
// rule metric 4 classifies it by, one per filed issue: issue_filings holds one row per filed
// issue, so a filed issue that somehow carries two `issue.created` events by Legion sessions keeps
// the first, in the order the population read them, and a second row of one key in one write
// cannot fail the whole write. The class and the filer's phase read the filer's own issue, so the
// population must hold that issue's timeline: ReadProject and ReadIssues both read the filers of
// the filings they read.
func ProjectFilings(population *Population) []IssueFiling {
	c := newComputation(population)
	filings := make([]IssueFiling, 0, len(population.Filed))
	seen := make(map[string]bool, len(population.Filed))
	for _, filed := range population.Filed {
		if seen[filed.Key] {
			continue
		}
		seen[filed.Key] = true
		filings = append(filings, IssueFiling{FiledIssue: c.classify(filed), Root: c.treeRoot(filed.Key)})
	}
	return filings
}
