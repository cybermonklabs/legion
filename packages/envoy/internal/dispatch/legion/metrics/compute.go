package metrics

import (
	"fmt"
	"math"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// Scope is which of a project's Legion issues an answer counts: the Legion section's own toggle,
// and the cut every series of the Legion evals is stored in (CutSeries).
type Scope string

const (
	ScopeAll      Scope = "all"
	ScopeRoots    Scope = "roots"
	ScopeChildren Scope = "children"
)

// ParseScope reads the scope parameter, defaulting to all.
func ParseScope(raw string) (Scope, error) {
	switch Scope(raw) {
	case "", ScopeAll:
		return ScopeAll, nil
	case ScopeRoots:
		return ScopeRoots, nil
	case ScopeChildren:
		return ScopeChildren, nil
	}
	return "", fmt.Errorf("scope must be one of all, roots or children")
}

// The bars of metric 2: the six roles, each holding the phases that role works, and the seventh
// for the phases no role works. "A seventh bar, 'no role', holds the phases no role works:
// `awaiting_person` and `held`", and the spec's "Phase boundaries before observations" holds it
// to those two alone — a handoff starts the next phase, so no hour falls between two phases for
// it to hold. Time in the queue is on no bar, and neither is an hour the work time leaves out,
// so the seven bars of an issue add up to its work time (phaseTime).
const barNoRole = "no_role"

// bars are the seven stacked bars in the order the card draws them. The merger's three phases
// are the one declaration of that grouping (legion.MergerPhases).
var bars = []struct {
	Role   string
	Phases []string
}{
	{"architect", []string{phaseDesign, legion.PhaseAccepting}},
	{"planner", []string{legion.PhasePlanning}},
	{"implementer", []string{legion.PhaseImplementing}},
	{"tester", []string{legion.PhaseTesting}},
	{"reviewer", []string{legion.PhaseReviewing}},
	{"merger", legion.MergerPhases},
	{barNoRole, []string{legion.PhaseAwaitingPerson, legion.PhaseHeld}},
}

// The four classes of issue a Legion session filed (metric 4).
const (
	ClassInTreeAtDesign  = "in_tree_at_design"
	ClassInTreeLater     = "in_tree_later"
	ClassOutOfTree       = "out_of_tree"
	ClassByTheController = "by_the_controller"
)

// classes is every class, in the order the card lists them.
var classes = []string{ClassInTreeAtDesign, ClassInTreeLater, ClassOutOfTree, ClassByTheController}

// Report is one answer of the Legion section: the window, the issues that finished in it, and the
// four metrics. It is the body of GET /api/v1/legion/metrics, which the OpenAPI document declares
// as LegionMetricsResponse (api/openapi_tables.go's schemaNames).
type Report struct {
	Project    string    `json:"project"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Scope      Scope     `json:"scope" enum:"all,roots,children"`
	ComputedAt time.Time `json:"computed_at"`
	// Finished is how many issues finished in the window under this scope: every metric's
	// population.
	Finished  int       `json:"finished"`
	WorkTime  WorkTime  `json:"work_time"`
	PhaseTime PhaseTime `json:"phase_time"`
	Rework    Rework    `json:"rework"`
	Splitting Splitting `json:"splitting"`
}

// WorkTime is metric 1: hours from an issue's first worker event to its close, less its parked
// and queued time.
type WorkTime struct {
	MedianHours float64         `json:"median_hours"`
	P90Hours    float64         `json:"p90_hours"`
	Issues      []WorkTimeIssue `json:"issues"`
}

// WorkTimeIssue is one finished issue's work time with what was taken out of it.
type WorkTimeIssue struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Priority    *int       `json:"priority"`
	Root        bool       `json:"root"`
	StartedAt   time.Time  `json:"started_at"`
	ClosedAt    time.Time  `json:"closed_at"`
	MergedAt    *time.Time `json:"merged_at"`
	Hours       float64    `json:"hours"`
	ParkedHours float64    `json:"parked_hours"`
	QueuedHours float64    `json:"queued_hours"`
}

// PhaseTime is metric 2: the seven bars, each split into the five segments.
type PhaseTime struct {
	Bars []RoleBar `json:"bars"`
	// IdleShare is the idle hours of every bar over their whole time.
	IdleShare float64 `json:"idle_share"`
}

// RoleBar is one worker type's bar. Role is one of Legion's six worker types or `no_role`, the
// seventh bar (bars, barNoRole); the enum is what makes the generated type carry that set rather
// than a bare string.
type RoleBar struct {
	Role string `json:"role" enum:"architect,planner,implementer,tester,reviewer,merger,no_role"`
	// Hours is the bar's whole time across the finished issues.
	Hours float64 `json:"hours"`
	// Issues is how many finished issues hold hours on this bar. The median is taken over every
	// finished issue, one the role never worked counting as zero, so Issues can be far below the population.
	Issues              int      `json:"issues"`
	MedianHoursPerIssue float64  `json:"median_hours_per_issue"`
	Segments            Segments `json:"segments"`
}

// BarPart is one issue's part of one bar of metric 2: its hours on the bar and their five
// segments, and whether any of its bar spans fall on the bar at all (HoldsHours), which is what
// RoleBar.Issues counts. A bar's whole time is the sum of its parts' segments, and its median
// per issue is the median of its parts' segment totals, one per finished issue (barPartsOf,
// phaseTime). Hours is that total rounded as every number of a report is.
type BarPart struct {
	Hours      float64  `json:"hours"`
	HoldsHours bool     `json:"holds_hours"`
	Segments   Segments `json:"segments"`
}

// Rework is metric 3.
type Rework struct {
	MedianReviewRounds float64 `json:"median_review_rounds"`
	// SentBackShare is the share of finished issues moved from testing, reviewing, accepting or
	// the merger's phase back to implementing at least once.
	SentBackShare float64 `json:"sent_back_share"`
	// HoursAfterFirstReview is the implementing and testing hours after the first review, across
	// the finished issues, and its median per issue.
	HoursAfterFirstReview       float64       `json:"hours_after_first_review"`
	MedianHoursAfterFirstReview float64       `json:"median_hours_after_first_review"`
	Issues                      []ReworkIssue `json:"issues"`
}

// ReworkIssue is one finished issue's rework numbers.
type ReworkIssue struct {
	Key                         string  `json:"key"`
	ReviewRounds                int     `json:"review_rounds"`
	SentBack                    bool    `json:"sent_back"`
	ImplementingHoursAfterFirst float64 `json:"implementing_hours_after_first_review"`
	TestingHoursAfterFirst      float64 `json:"testing_hours_after_first_review"`
}

// Splitting is metric 4: what Legion sessions filed in the window, by class and week, against the
// issues that finished per week. The rows and the weeks' class counts are narrowed to the scope
// by the issue each filing created, as the finished population is narrowed by the issue that
// finished (inScope); the four root figures below are the project's whatever the toggle says.
type Splitting struct {
	Weeks []SplittingWeek `json:"weeks"`
	Filed []FiledIssue    `json:"filed"`
	// FinishedRoots is how many roots finished in the window, ChildrenPerRoot the mean number of
	// children of those roots, and the two medians are their work time with and without children.
	FinishedRoots                int     `json:"finished_roots"`
	ChildrenPerRoot              float64 `json:"children_per_root"`
	MedianHoursRootsWithChildren float64 `json:"median_hours_roots_with_children"`
	MedianHoursRootsNoChildren   float64 `json:"median_hours_roots_without_children"`
}

// SplittingWeek is one UTC week: each class's filings and how many issues finished.
type SplittingWeek struct {
	Week     time.Time      `json:"week"`
	Filed    map[string]int `json:"filed"`
	Finished int            `json:"finished"`
}

// FiledIssue is one issue a Legion session created in the window. Three of its fields carry a
// closed set the server decides — the four classes, the filer (one of Legion's six worker types
// or the controller, which works no issue) and the phase the filer's issue was in, named by the
// bar it falls on (tablePhase) — so each states its set as an enum and the generated type carries
// it rather than a bare string.
type FiledIssue struct {
	Key   string    `json:"key"`
	Title string    `json:"title"`
	At    time.Time `json:"at"`
	Class string    `json:"class" enum:"in_tree_at_design,in_tree_later,out_of_tree,by_the_controller"`
	// Filer is the role that filed it and the issue that role was working, or the controller.
	Filer      string `json:"filer" enum:"architect,planner,implementer,tester,reviewer,merger,controller"`
	FilerIssue string `json:"filer_issue"`
	// FilerPhase is the phase the filer's issue was in when it filed, named by the bar it falls
	// on (tablePhase): the seven bar words, the three a timeline holds that are on no bar
	// (`queued`, `awaiting_person` and `held`), `done` for an issue the daemon has finished and
	// Dispatch has not closed, and the empty string for a moment the issue's timeline does not
	// cover, which is what every filing by the controller carries.
	FilerPhase string `json:"filer_phase" enum:",queued,design,planning,implementing,testing,reviewing,accepting,merging,awaiting_person,held,done"`
}

// Compute is the metrics answer for a population. It reads nothing: the route and the live hub
// both call it over a population Read gathered, so a live push and a GET answer the same numbers.
// Every number of its four metrics is a Legion fold's (folds.go) over the parts of the window's
// finished issues and filings, built from each issue's facts (factsOf), the values the stored
// rows hold, so the section and the eval routes read one rule. The tables of issues beside the
// numbers are its own.
func Compute(population *Population, scope Scope, now time.Time) Report {
	computed := newComputation(population, scope)
	finished := computed.finished()
	return Report{
		Project:    population.Project,
		From:       population.From,
		To:         population.To,
		Scope:      scope,
		ComputedAt: now,
		Finished:   len(finished),
		WorkTime:   computed.workTime(finished),
		PhaseTime:  computed.phaseTime(finished),
		Rework:     computed.rework(finished),
		Splitting:  computed.splitting(),
	}
}

// computation is one answer being computed, with the timelines and views each metric shares.
type computation struct {
	population *Population
	scope      Scope
	phases     map[string]phaseTimeline
	views      map[string]*issueView
	// roots is each key's tree root, workTimes each issue's work-time row and barSpans the
	// spans its bars are shared out over, each computed once: TreeRoot walks a parent chain
	// allocating a set as it goes, workTimeOf runs the whole span algebra, barSpansOf runs a
	// subtraction over it, and the four metrics and the per-issue facts ask for each of them
	// two to four times per issue.
	roots     map[string]string
	workTimes map[string]workTimeRow
	barSpans  map[string][]Span
	// facts is each issue's facts (factsOf), which every metric of a report folds.
	facts map[string]IssueFacts
}

// newComputation is an empty computation over population under scope, with its memoised maps
// made: what Compute, ProjectFacts and ProjectFilings each start from.
func newComputation(population *Population, scope Scope) *computation {
	return &computation{
		population: population,
		scope:      scope,
		phases:     map[string]phaseTimeline{},
		views:      map[string]*issueView{},
		roots:      map[string]string{},
		workTimes:  map[string]workTimeRow{},
		barSpans:   map[string][]Span{},
		facts:      map[string]IssueFacts{},
	}
}

// phaseTimeline is one issue's phase timeline with where it came from: the daemon's own
// observations, or the event log's fallback (Population.Phases).
type phaseTimeline struct {
	spans    []PhaseSpan
	observed bool
}

// workTimeRow is one memoised answer of workTimeOf: the row, the spans its hours are the measure
// of, and whether the issue has a work time at all.
type workTimeRow struct {
	row    WorkTimeIssue
	worked []Span
	ok     bool
}

// finished is the population every metric counts: "issues of the picked project that finished in
// the window. An issue finished when it closed with Legion phase work: an event by a session
// titled `Legion <role> · <KEY>` for its own key, or a handoff", narrowed to the scope.
func (c *computation) finished() []*Issue {
	var finished []*Issue
	for _, key := range c.population.Keys {
		issue := c.population.Issues[key]
		if !c.finishedInWindow(issue) || !c.inScope(issue.Key) {
			continue
		}
		finished = append(finished, issue)
	}
	return finished
}

// finishedInWindow is the finished rule itself, before any scope. It reads Legion phase work and
// not the wider rule that places an issue in a tree (Population.isLegionIssue): an issue the
// daemon reports and no role ever worked is in its tree, and finished nothing.
func (c *computation) finishedInWindow(issue *Issue) bool {
	if issue.ClosedAt == nil || issue.ClosedAt.Before(c.population.From) || !issue.ClosedAt.Before(c.population.To) {
		return false
	}
	return c.population.hasPhaseWork(issue)
}

// treeRoot is the issue's tree root (Population.TreeRoot), computed once per key: the walk is a
// pure function of a population Compute never writes to, so one answer stands for the whole
// computation.
func (c *computation) treeRoot(key string) string {
	if root, held := c.roots[key]; held {
		return root
	}
	root := c.population.TreeRoot(key)
	c.roots[key] = root
	return root
}

// isRoot is whether the issue is the root of its own tree, by the walk every rule of this
// computation places an issue with (treeRoot).
func (c *computation) isRoot(key string) bool {
	return c.treeRoot(key) == key
}

// treeDepth is how many child_of links lie between the issue and its tree root (treeRoot), zero
// for the root itself. It climbs the parent Dispatch records, which is the link the depth counts;
// the root the walk stops at may instead be the `tree` the daemon named, and a chain that does
// not reach that root answers zero, as the root itself does. The keys it has climbed through keep
// a parent chain that loops from running for ever.
func (c *computation) treeDepth(key string) int {
	root := c.treeRoot(key)
	climbed := map[string]bool{}
	for depth, at := 0, key; at != "" && !climbed[at]; depth++ {
		if at == root {
			return depth
		}
		climbed[at] = true
		issue := c.population.Issues[at]
		if issue == nil {
			break
		}
		at = issue.Parent
	}
	return 0
}

// inScope is the scope toggle itself: the finished population (finished) and the filings of the
// splitting table (splitting) are narrowed by the rule that cuts every Legion series (inCut), so
// a reader of any of those numbers is reading the same rule. In `roots` an issue that is a root
// of its tree counts, in `children` one that is not, and in `all` every issue.
//
// It takes a key rather than an issue because the row of the splitting table is the issue that
// was filed: a filing is read by the tree the new issue is in, not by the filer's, so one filer's
// filings can fall on either side of the toggle.
func (c *computation) inScope(key string) bool {
	return inCut(c.scope, c.isRoot(key))
}

// timelineOf is the issue's phase timeline with where it came from, computed once.
func (c *computation) timelineOf(issue *Issue) phaseTimeline {
	if timeline, held := c.phases[issue.Key]; held {
		return timeline
	}
	spans, observed := c.population.Phases(issue)
	timeline := phaseTimeline{spans: spans, observed: observed}
	c.phases[issue.Key] = timeline
	return timeline
}

// phasesOf is the issue's phase timeline alone, for the rules that read the spans and not where
// they came from.
func (c *computation) phasesOf(issue *Issue) []PhaseSpan {
	return c.timelineOf(issue).spans
}

// viewOf is the issue with its tree and the timelines its segments read, computed once.
func (c *computation) viewOf(issue *Issue) *issueView {
	if view, held := c.views[issue.Key]; held {
		return view
	}
	root := c.population.Issues[c.treeRoot(issue.Key)]
	if root == nil {
		root = issue
	}
	view := &issueView{
		population:  c.population,
		issue:       issue,
		root:        root,
		timeline:    c.phasesOf(issue),
		childPhase:  map[string][]PhaseSpan{},
		childWorked: map[string][]Span{},
		end:         c.population.issueEnd(issue),
	}
	for _, key := range root.Children {
		child := c.population.Issues[key]
		if child == nil || child.Key == issue.Key {
			continue
		}
		view.children = append(view.children, child)
		if root.Key != issue.Key {
			// Only a root's view reads its children's timelines: the waiting-on-children rule
			// answers nothing for a child, and a child's view takes its tree's asks alone.
			continue
		}
		view.childPhase[child.Key] = c.phasesOf(child)
		view.childWorked[child.Key] = c.barSpansOf(child)
	}
	c.views[issue.Key] = view
	return view
}

// workTime is metric 1: "Hours from the issue's first worker event to its close, minus any time in
// `backlog` or `icebox` and any time back in the queue after it." Its median and p90 are
// FoldWorkTime's over the finished issues' part.
func (c *computation) workTime(finished []*Issue) WorkTime {
	metric := WorkTime{Issues: make([]WorkTimeIssue, 0, len(finished))}
	for _, issue := range finished {
		if row, ok := c.workTimeOf(issue); ok {
			metric.Issues = append(metric.Issues, row)
		}
	}
	fold := FoldWorkTime(WorkTimeDay(c.population.From, c.factsOfEach(finished)), ScopeAll)
	metric.MedianHours, metric.P90Hours = valueOrZero(fold.Median), valueOrZero(fold.P90)
	return metric
}

// factsOfEach is the facts of each of issues, in their order. A metric of a report folds the
// issues it is given as one part, whose day (the window's start) no fold of these reads, and the
// parts' sums run in this order, the order the metrics summed their issues in before the folds.
func (c *computation) factsOfEach(issues []*Issue) []IssueFacts {
	facts := make([]IssueFacts, len(issues))
	for index, issue := range issues {
		facts[index] = c.factsOf(issue)
	}
	return facts
}

// workTimeOf is one finished issue's work time, computed once: the work-time metric asks for
// every finished issue's row, and the per-issue facts ask again.
func (c *computation) workTimeOf(issue *Issue) (WorkTimeIssue, bool) {
	held := c.workedOf(issue)
	return held.row, held.ok
}

// workedSpansOf is the time the issue worked: the very spans computeWorkTime totalled for the
// hours of its row. Metric 2 shares a bar out over them and nothing else, so an hour the work
// time left out cannot reach a bar and no second subtraction can drift from this one.
func (c *computation) workedSpansOf(issue *Issue) []Span {
	return c.workedOf(issue).worked
}

// barSpansOf is the hours of an issue its seven bars share out, computed once: the work time
// metric 1 reports, less the time it sat in the queue, which is on no bar. Metric 2's bars and
// metric 3's hours after the first review are both the measure of these spans, so no table of a
// card counts an hour the card's own bars leave out.
//
// The answer is shared with every caller, and every caller reads it and nothing more: normalize,
// subtract and intersect each build a new slice and write to no argument, so no caller can move
// a span under another.
func (c *computation) barSpansOf(issue *Issue) []Span {
	if held, computed := c.barSpans[issue.Key]; computed {
		return held
	}
	held := subtract(c.workedSpansOf(issue), phaseSpansOf(c.phasesOf(issue), phaseQueued))
	c.barSpans[issue.Key] = held
	return held
}

// workedOf is the memoised answer of computeWorkTime.
func (c *computation) workedOf(issue *Issue) workTimeRow {
	if held, computed := c.workTimes[issue.Key]; computed {
		return held
	}
	held := c.computeWorkTime(issue)
	c.workTimes[issue.Key] = held
	return held
}

// computeWorkTime is the work time itself: the issue's life, less the time it sat parked and the
// time it sat back in the queue. The spans that are left are the answer's second half, because
// they are what the issue's bars of phase time are shared out over.
//
// Every issue the metric reports a row for has closed (finishedInWindow), and the row's ClosedAt
// is that close. A child still open has a work time too, which its root's waiting-on-children
// rule reads: its life runs to the window's end, as every other span of a live issue does
// (issueEnd).
func (c *computation) computeWorkTime(issue *Issue) workTimeRow {
	start, ok := firstWorkerEvent(issue)
	if !ok {
		return workTimeRow{}
	}
	end := c.population.issueEnd(issue)
	// A life of no length leaves no span to park, to queue or to put on a bar, which the span
	// algebra below answers on its own.
	life := []Span{{From: start, To: end}}
	parked := intersect(c.population.parkedSpans(issue), life)
	queued := intersect(c.population.queuedSpans(issue), life)
	worked := subtract(subtract(life, parked), queued)
	return workTimeRow{
		row: WorkTimeIssue{
			Key: issue.Key, Title: issue.Title, Priority: issue.Priority, Root: c.isRoot(issue.Key),
			StartedAt: start, ClosedAt: end, MergedAt: issue.MergedAt,
			Hours:       hours(totalOf(worked)),
			ParkedHours: hours(totalOf(parked)),
			QueuedHours: hours(totalOf(queued)),
		},
		worked: worked,
		ok:     true,
	}
}

// firstWorkerEvent is where an issue's work time starts: the first event of any session titled
// `Legion <role> · <its key>`, and its first handoff for an issue whose Legion work is a handoff
// alone.
func firstWorkerEvent(issue *Issue) (time.Time, bool) {
	var first time.Time
	for _, at := range issue.FirstRoleEvent {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	for _, handoff := range issue.Handoffs {
		if first.IsZero() || handoff.At.Before(first) {
			first = handoff.At
		}
	}
	return first, !first.IsZero()
}

// phaseTime is metric 2: one stacked bar per worker type, each hour of each phase going to the
// first segment of the precedence that holds.
//
// The hours shared out are the issue's work time and nothing else (the spec's "Rule details —
// Parked and pre-work hours in phase time": "An hour that work time to merge leaves out is on no
// bar of phase time ... So the bars of an issue add up to its work time, and the two cards
// agree"). Each bar takes the phases it holds and the queue is on none of them, so the seven are
// a partition of the work time: the timeline leaves no hour between two phases (the spec's
// "Phase boundaries before observations"), and every phase an issue can be in is on one of the
// seven bars.
//
// The bars are FoldPhaseTime's over the finished issues' part: every issue counts towards each
// bar's median, one the role never worked as a zero, so the seven medians describe one
// population and not seven (PhaseTimeDay).
func (c *computation) phaseTime(finished []*Issue) PhaseTime {
	fold := FoldPhaseTime(PhaseTimeDay(c.population.From, c.factsOfEach(finished)), ScopeAll)
	metric := PhaseTime{Bars: make([]RoleBar, len(bars)), IdleShare: valueOrZero(fold.IdleShare)}
	for index, bar := range bars {
		folded := fold.Bars[bar.Role]
		metric.Bars[index] = RoleBar{
			Role: bar.Role, Hours: folded.Hours, Issues: folded.Issues,
			MedianHoursPerIssue: valueOrZero(folded.MedianPerIssue), Segments: folded.Segments,
		}
	}
	return metric
}

// barPartsOf is one issue's part of metric 2, keyed by role: its time on each of the seven bars
// (bars), shared out over the five segments. The per-issue facts store these parts
// (ProjectFacts) and PhaseTimeDay adds them up across the finished issues, so the two read one
// rule.
//
// A bar's spans are the issue's bar spans (barSpansOf) in the phases the bar holds. HoldsHours
// is whether there are any, which is what RoleBar.Issues counts, kept apart from Hours because
// a span of a fraction of a second holds the bar while its hours round to nothing.
func (c *computation) barPartsOf(issue *Issue) map[string]BarPart {
	view := c.viewOf(issue)
	shared := c.barSpansOf(issue)
	parts := make(map[string]BarPart, len(bars))
	for _, bar := range bars {
		spans := intersect(phaseSpansOf(view.timeline, bar.Phases...), shared)
		segments := view.split(spans)
		parts[bar.Role] = BarPart{Hours: round(segments.total()), HoldsHours: len(spans) > 0, Segments: segments}
	}
	return parts
}

// rework is metric 3: review rounds, the share of issues sent back, and the implementing and
// testing hours after the first review, FoldRework's over the finished issues' part.
func (c *computation) rework(finished []*Issue) Rework {
	facts := c.factsOfEach(finished)
	metric := Rework{Issues: make([]ReworkIssue, 0, len(finished))}
	for index, issue := range finished {
		metric.Issues = append(metric.Issues, ReworkIssue{
			Key: issue.Key, ReviewRounds: facts[index].ReviewRounds, SentBack: facts[index].SentBack,
			ImplementingHoursAfterFirst: facts[index].ImplementingHoursAfterFirstReview,
			TestingHoursAfterFirst:      facts[index].TestingHoursAfterFirstReview,
		})
	}
	fold := FoldRework(ReworkDay(c.population.From, facts), ScopeAll)
	metric.MedianReviewRounds = valueOrZero(fold.ReviewRounds.Median)
	metric.SentBackShare = valueOrZero(fold.SentBack.Share)
	metric.HoursAfterFirstReview = fold.HoursAfterFirstReview.Sum
	metric.MedianHoursAfterFirstReview = valueOrZero(fold.HoursAfterFirstReview.Median)
	return metric
}

// reworkOf is one issue's part of metric 3: its review rounds, whether it was sent back, and its
// implementing and testing hours after the first review, which are the measure of its bar spans
// from that review on. The per-issue facts store these parts (ProjectFacts) and ReworkDay adds
// them up across the finished issues.
func (c *computation) reworkOf(issue *Issue) ReworkIssue {
	phases := c.timelineOf(issue)
	row := ReworkIssue{Key: issue.Key, ReviewRounds: reviewRounds(issue, phases), SentBack: sentBackOnce(issue, phases)}
	if at, reviewed := firstReview(issue, phases); reviewed {
		after := intersect(c.barSpansOf(issue), []Span{{From: at, To: c.population.issueEnd(issue)}})
		row.ImplementingHoursAfterFirst = hours(totalOf(intersect(phaseSpansOf(phases.spans, legion.PhaseImplementing), after)))
		row.TestingHoursAfterFirst = hours(totalOf(intersect(phaseSpansOf(phases.spans, legion.PhaseTesting), after)))
	}
	return row
}

// slotWaitHours is the issue's slot wait (dispatch://INFRA-2916, D3): "the hours of the issue's
// observed life in which its phase is `admitted` and it holds no slot, over the whole life, with
// parked hours left out".
//
// The phase is the observed one alone, the timeline the sampler's own `phase` observations draw
// (sampledPhases), whose `queued` stretches are the admitted hours in which the issue held no
// slot of its own: the one queue definition the work time subtracts too (queuedSpans). Work time
// counts that queue from the first worker event on; this counts it over the whole life, from the
// issue's creation to its end, because a wait for a slot before any worker started is a wait all
// the same. The parked hours are the ones the work time leaves out (parkedSpans). An issue the
// sampler never saw has no observed phase, and so no slot wait.
func (c *computation) slotWaitHours(issue *Issue, sampled []PhaseSpan) float64 {
	life := []Span{{From: issue.CreatedAt, To: c.population.issueEnd(issue)}}
	waited := intersect(phaseSpansOf(sampled, phaseQueued), life)
	return hours(totalOf(subtract(waited, c.population.parkedSpans(issue))))
}

// idleByRole is the issue's idle hours by role (dispatch://INFRA-2916, D3): for role R, "the hours
// of the issue's worked spans (life − parked − queued) in which the issue holds its own slot, its
// observed phase is in R's bar ..., and R's newest observed claim state is not `working` (no claim
// counts as not working)". Each of the six roles has an entry, zero where no hour qualifies.
//
// The worked spans are the ones the work time counts (workedSpansOf). The slot is the issue's own
// `slot` observations (slotSpans), never the daemon's admission block. R's bar is the bar phase
// time draws for R (bars): the architect's is `design` and `accepting`, where `design` is the
// daemon's `admitted` while the issue holds a slot of its own, which is how the timeline names
// that stretch (sampledPhases); the merger's is legion.MergerPhases, `retro`, `merging` and
// `awaiting_merge`. The phase is the observed one alone (sampledPhases), so an hour before the
// sampler first saw the issue is in no bar. R's claim is its `claim.<R>` observations: a stretch
// whose newest mark reads `working` is a turn, as the in-a-turn segment reads the same field
// (claimWorking), and every other stretch, the one before the first mark included, is not.
//
// It is not phase time's idle segment, which is the hours of a bar no other segment claimed: an
// hour of R's bar with R out of a turn is idle here whether or not CI, a person or a child held
// it.
func (c *computation) idleByRole(issue *Issue, sampled []PhaseSpan) map[string]float64 {
	end := c.population.issueEnd(issue)
	holding := intersect(c.workedSpansOf(issue), c.population.slotSpans(issue, end))
	idle := make(map[string]float64, len(bars)-1)
	for _, bar := range bars {
		if bar.Role == barNoRole {
			continue
		}
		inBar := intersect(holding, phaseSpansOf(sampled, bar.Phases...))
		working := markSpans(issue.Observed[legion.ClaimFieldPrefix+bar.Role], end, claimWorking)
		idle[bar.Role] = hours(totalOf(subtract(inBar, working)))
	}
	return idle
}

// reviewRounds is "entries into `reviewing`, or into status `needs_review` before observations".
// Before observations is where the sampler drew no timeline of the issue's own life: an issue
// whose only `phase` mark is the one the sampler's first pass left after it closed has no
// observed round of any hour it lived, so its rounds are its status entries.
func reviewRounds(issue *Issue, timeline phaseTimeline) int {
	if !timeline.observed {
		return statusEntries(issue, statusNeedsReview)
	}
	rounds := 0
	for index, span := range timeline.spans {
		if span.Phase == legion.PhaseReviewing && (index == 0 || timeline.spans[index-1].Phase != legion.PhaseReviewing) {
			rounds++
		}
	}
	return rounds
}

// sentBackOnce is "a move from `testing`, `reviewing`, `accepting` or the merger's phase to
// `implementing`": off the observed timeline where the sampler drew one, and off the status
// history before observations, as the review rounds beside it are, since `needs_review` is the
// status of both reviewing and accepting, `retro` the status of the merger's phase and
// `in_progress` the status of implementing. The merger's phase is three of the daemon's
// (legion.MergerPhases, the three the merger's bar holds), and a red check sends an issue back
// to implementing out of any of them, so the observed half counts all three where the fallback
// half counts the one status that names them.
func sentBackOnce(issue *Issue, timeline phaseTimeline) bool {
	if !timeline.observed {
		return statusMoved(issue, sentBackStatuses, statusImplementing)
	}
	for index := 1; index < len(timeline.spans); index++ {
		if timeline.spans[index].Phase != legion.PhaseImplementing {
			continue
		}
		switch previous := timeline.spans[index-1].Phase; {
		case previous == legion.PhaseTesting, previous == legion.PhaseReviewing,
			previous == legion.PhaseAccepting, legion.IsMergerPhase(previous):
			return true
		}
	}
	return false
}

// firstEntry is when the timeline first entered a phase.
func firstEntry(timeline []PhaseSpan, phase string) (time.Time, bool) {
	for _, span := range timeline {
		if span.Phase == phase {
			return span.Span.From, true
		}
	}
	return time.Time{}, false
}

// firstReview is when the issue was first reviewed: the first `reviewing` stretch of the
// timeline the sampler drew, and the first entry into status `needs_review` before observations,
// where the rounds and the send-back beside it are read off the same status history ("Rule
// details — Review rounds and send-backs before observations").
func firstReview(issue *Issue, timeline phaseTimeline) (time.Time, bool) {
	if !timeline.observed {
		return firstStatusEntry(issue, statusNeedsReview)
	}
	return firstEntry(timeline.spans, legion.PhaseReviewing)
}

// splitting is metric 4: each issue a Legion session created in the window, by the parent it had
// at its `issue.created` event, counted per week against the issues that finished per week. The
// rows are the filings the scope keeps, read by the issue the filing created (inScope). The
// numbers are FoldSplitting's over the day parts of every issue that finished in the window and
// every filing of it, by close day and by filing day: the weeks are the scope's cut, and the
// four root figures the all cut's, which counts roots alone whatever scope the reader picked.
func (c *computation) splitting() Splitting {
	metric := Splitting{Filed: make([]FiledIssue, 0, len(c.population.Filed))}
	type input struct {
		finished []IssueFacts
		filed    []IssueFiling
	}
	days := map[time.Time]*input{}
	on := func(at time.Time) *input {
		day := at.UTC().Truncate(24 * time.Hour)
		if days[day] == nil {
			days[day] = &input{}
		}
		return days[day]
	}
	// The toggle reads each filed issue's tree as it stands now (inScope), while its class reads
	// the parent it had when it was filed (classify); a filing into another project has no tree
	// here and counts as a root.
	for _, filed := range c.population.Filed {
		row := c.classify(filed)
		if c.inScope(filed.Key) {
			metric.Filed = append(metric.Filed, row)
		}
		day := on(filed.At)
		day.filed = append(day.filed, IssueFiling{FiledIssue: row, Root: c.treeRoot(filed.Key)})
	}
	for _, key := range c.population.Keys {
		if issue := c.population.Issues[key]; c.finishedInWindow(issue) {
			day := on(*issue.ClosedAt)
			day.finished = append(day.finished, c.factsOf(issue))
		}
	}
	var parts []measures.DayPart
	for day, held := range days {
		parts = append(parts, SplittingDay(day, held.finished, held.filed)...)
	}

	metric.Weeks = []SplittingWeek{}
	if scoped := FoldSplitting(parts, c.scope); scoped != nil {
		for _, week := range scoped.Weeks {
			metric.Weeks = append(metric.Weeks, SplittingWeek{Week: week.Week, Filed: week.Filed, Finished: week.Finished})
		}
	}
	if all := FoldSplitting(parts, ScopeAll); all != nil && all.Roots != nil {
		roots := all.Roots
		metric.FinishedRoots = roots.FinishedRoots
		metric.ChildrenPerRoot = valueOrZero(roots.ChildrenPerRoot)
		metric.MedianHoursRootsWithChildren = valueOrZero(roots.HoursWithChildren.Median)
		metric.MedianHoursRootsNoChildren = valueOrZero(roots.HoursWithoutChildren.Median)
	}
	return metric
}

// classify is one filing's class: by the controller, in the filer's tree at design or later, or
// out of the tree.
func (c *computation) classify(filed Filed) FiledIssue {
	row := FiledIssue{
		Key: filed.Key, Title: filed.Title, At: filed.At,
		Filer: filed.Filer.Role, FilerIssue: filed.Filer.Issue,
	}
	if filed.Filer.Controller {
		row.Class = ClassByTheController
		return row
	}
	row.Class = ClassOutOfTree
	filer := c.population.Issues[filed.Filer.Issue]
	if filer == nil {
		return row
	}
	// "For every class, the table names the filer's phase at the filing", so the phase is read
	// off the filer's own timeline before the class is decided, and an out-of-tree filing carries
	// it as an in-tree one does. A filing by the controller carries none: the controller works no
	// issue, so there is no timeline to read a phase off.
	row.FilerPhase = tablePhase(phaseAt(c.phasesOf(filer), filed.At))
	// The tree of a filed issue: walk up from the parent it had at its `issue.created` event,
	// through each issue's parent as Dispatch records it, and take the top it reaches; the filer
	// walks the same way. The new issue is in the filer's tree when both walks reach the same
	// issue, at any depth. The event's own `ancestors` list is read by nothing here: Dispatch
	// leaves it empty, so a filing under a child of the filer read as out of the tree (the
	// spec's "Rule details — The tree of a filed issue").
	root := c.treeRoot(filer.Key)
	if filed.Parent == "" || c.treeRoot(filed.Parent) != root {
		return row
	}
	// "created before the filer's issue had its first implementer event": an issue whose filer
	// never reached an implementer never passed that moment, so its filings are at design.
	implementer, started := filer.FirstRoleEvent["implementer"]
	if !started || filed.At.Before(implementer) {
		row.Class = ClassInTreeAtDesign
		return row
	}
	row.Class = ClassInTreeLater
	return row
}

// phaseAt is the phase a timeline holds at a moment, or "" where it holds none.
func phaseAt(timeline []PhaseSpan, at time.Time) string {
	for _, span := range timeline {
		if !at.Before(span.Span.From) && at.Before(span.Span.To) {
			return span.Phase
		}
	}
	return ""
}

// tablePhase is the word a table names a phase by: the bar it falls on (the spec's "Phase words
// in tables"). The merger's bar holds three of the daemon's phases (legion.MergerPhases, the one
// declaration of that grouping), so a filer in any of them is named merging, and every other
// phase is named as the timeline names it.
//
// The whole of what it answers, which packages/contracts/src/dispatch-api.ts declares as
// `LegionTablePhase`: the seven words of the bars — `design`, `planning`, `implementing`,
// `testing`, `reviewing`, `accepting` and `merging`; the three words a timeline holds that are
// on no role's bar — `queued`, which is the waiting line, and `awaiting_person` and `held`,
// which are the "no role" bar's; `done`, which an issue the daemon has finished and Dispatch
// has not closed yet holds; and "" for a moment no span of the timeline covers, which is what a
// filing by the controller carries, since the controller works no issue.
func tablePhase(phase string) string {
	if legion.IsMergerPhase(phase) {
		return legion.PhaseMerging
	}
	return phase
}

// weekStart is the UTC Monday the moment's week begins on.
func weekStart(at time.Time) time.Time {
	day := at.UTC().Truncate(24 * time.Hour)
	back := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -back)
}

// quantile is the q-th quantile of a sorted list, "linear interpolation between the closest
// ranks", and zero for an empty one: the delivery measures' own rule (measures.Quantile),
// rounded as every number of a report is.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return round(measures.Quantile(sorted, q))
}

// hours is a duration in hours, rounded as every number of a report is.
func hours(spent time.Duration) float64 {
	return round(spent.Hours())
}

// round keeps four decimals: enough that a sum of minutes reads exactly, and few enough that no
// answer carries floating-point noise.
func round(value float64) float64 {
	return math.Round(value*1e4) / 1e4
}

// rounded rounds each of a bar's segments.
func rounded(segments Segments) Segments {
	return Segments{
		WaitingOnPerson:   round(segments.WaitingOnPerson),
		InATurn:           round(segments.InATurn),
		CIWait:            round(segments.CIWait),
		WaitingOnChildren: round(segments.WaitingOnChildren),
		Idle:              round(segments.Idle),
	}
}
