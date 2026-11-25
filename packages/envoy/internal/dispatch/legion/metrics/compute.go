package metrics

import (
	"math"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// Scope is which of a project's Legion issues an answer counts: all of them, the tree roots, or
// the children. A caller names one of the three constants, and nothing parses one from text: a
// `scope` parameter was the metrics route's, and that route never ships (Report), while the evals
// of dispatch://INFRA-2916 take no scope from a request either, since they carry roots and
// children as series of each measure beside all issues (that spec's R6 and "Series, not a
// toggle").
type Scope string

const (
	ScopeAll      Scope = "all"
	ScopeRoots    Scope = "roots"
	ScopeChildren Scope = "children"
)

// The bars of metric 2: Legion's six roles, each holding the phases that role works, a bar for
// each role the deployment defines that worked a step of an issue, holding that role's spans of
// `step` (barsOf), and the bar for the phases no role works. "A seventh bar, 'no role', holds the
// phases no role works: `awaiting_person` and `held`", and the spec's "Phase boundaries before
// observations" holds it to those two alone — a handoff starts the next phase, so no hour falls
// between two phases for it to hold. Time in the queue is on no bar, and neither is an hour the
// work time leaves out, so the bars of an issue add up to its work time (phaseTime).
const barNoRole = "no_role"

// mergerPhases are the two phases of the merger's bar, "merging and awaiting merge", and the two a
// table names merging (tablePhase). Retro is not one of them: "the daemon gives retro to the
// implementer" (RoleFor in packages/daemon/internal/phase), so its hours are on the
// implementer's bar and a table names it retro (the spec's metric 2 and "Phase words in tables").
var mergerPhases = []string{legion.PhaseMerging, legion.PhaseAwaitingMerge}

// bar is one bar of metric 2: its role and the phases it holds, or, for a role the deployment
// defines (step), that role's spans of `step` (PhaseSpan.Role).
type bar struct {
	Role   string
	Phases []string
	step   bool
}

// spansOf is the spans of timeline the bar holds.
func (b bar) spansOf(timeline []PhaseSpan) []Span {
	if !b.step {
		return phaseSpansOf(timeline, b.Phases...)
	}
	var spans []Span
	for _, one := range timeline {
		if one.Phase == legion.PhaseStep && one.Role == b.Role {
			spans = append(spans, one.Span)
		}
	}
	return normalize(spans)
}

// ownBars are the bars of Legion's six roles, in the order PhaseTime.Bars lists them, and
// noRoleBar the last bar.
var (
	ownBars = []bar{
		{Role: "architect", Phases: []string{phaseDesign, legion.PhaseAccepting}},
		{Role: "planner", Phases: []string{legion.PhasePlanning}},
		{Role: "implementer", Phases: []string{legion.PhaseImplementing, legion.PhaseRetro}},
		{Role: "tester", Phases: []string{legion.PhaseTesting}},
		{Role: "reviewer", Phases: []string{legion.PhaseReviewing}},
		{Role: "merger", Phases: mergerPhases},
	}
	noRoleBar = bar{Role: barNoRole, Phases: []string{legion.PhaseAwaitingPerson, legion.PhaseHeld}}
)

// barsOf is the bars of metric 2 over timelines, in the order PhaseTime.Bars lists them: Legion's
// six (ownBars), then one for each role the deployment defines whose step a span of timelines
// holds, by name, then the bar of no role. A `step` span whose role no read named is on the bar
// of the role "", which holds its hours rather than letting them fall off every bar.
func barsOf(timelines [][]PhaseSpan) []bar {
	bars := slices.Clone(ownBars)
	var roles []string
	for _, timeline := range timelines {
		for _, one := range timeline {
			if one.Phase == legion.PhaseStep && !slices.Contains(roles, one.Role) {
				roles = append(roles, one.Role)
			}
		}
	}
	slices.Sort(roles)
	for _, role := range roles {
		bars = append(bars, bar{Role: role, step: true})
	}
	return append(bars, noRoleBar)
}

// The four classes of issue a Legion session filed (metric 4).
const (
	ClassInTreeAtDesign  = "in_tree_at_design"
	ClassInTreeLater     = "in_tree_later"
	ClassOutOfTree       = "out_of_tree"
	ClassByTheController = "by_the_controller"
)

// classes is every class: each week's Filed map counts all four, a class nothing was filed in as
// zero (splitting).
var classes = []string{ClassInTreeAtDesign, ClassInTreeLater, ClassOutOfTree, ClassByTheController}

// Report is one answer of the metrics computation: the window, the issues that finished in it,
// and the four metrics. No route of this repository serves it — the root spec says the metrics
// route never ships — so only tests call it today, and it is here for the evals of
// dispatch://INFRA-2916 to call; the shape is this type rather than an OpenAPI schema.
type Report struct {
	Project    string    `json:"project"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Scope      Scope     `json:"scope"`
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
// and queued time and its time out of the workflow.
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
	// OutOfWorkflowHours is the time out of the workflow before the close (outOfWorkflowSpans),
	// which ParkedHours does not hold: that is the time in `backlog` or `icebox` alone. Each of
	// the three is its own measure, so an hour can be in two of them, such as a child the daemon
	// reports `done` while it sits in `backlog`, and the work time leaves it out once.
	OutOfWorkflowHours float64 `json:"out_of_workflow_hours"`
}

// PhaseTime is metric 2: the bars (barsOf), each split into the five segments.
type PhaseTime struct {
	Bars []RoleBar `json:"bars"`
	// IdleShare is the idle hours of every bar over their whole time.
	IdleShare float64 `json:"idle_share"`
}

// RoleBar is one worker type's bar. Role is one of Legion's six worker types, a role the
// deployment defines, or `no_role`, the last bar (barsOf, barNoRole).
type RoleBar struct {
	Role string `json:"role"`
	// Hours is the bar's whole time across the finished issues.
	Hours float64 `json:"hours"`
	// Issues is how many finished issues hold hours on this bar. The median is taken over every
	// finished issue, one the role never worked counting as zero, so Issues can be far below the population.
	Issues              int      `json:"issues"`
	MedianHoursPerIssue float64  `json:"median_hours_per_issue"`
	Segments            Segments `json:"segments"`
}

// Rework is metric 3.
type Rework struct {
	MedianReviewRounds float64 `json:"median_review_rounds"`
	// SentBackShare is the share of finished issues moved from testing, reviewing, accepting,
	// retro, merging or awaiting merge back to implementing at least once (sentBackPhases), or
	// whose step of a role the deployment defines failed and returned its task (sentBackOnce).
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
// finished (inScope); the four root figures below are the project's whatever scope the caller
// names.
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
// closed set the computation decides: Class is one of the four classes, Filer one of Legion's six
// worker types or the controller, which works no issue, and FilerPhase the phase the filer's issue
// was in, named by the word a table names it by (tablePhase).
type FiledIssue struct {
	Key   string    `json:"key"`
	Title string    `json:"title"`
	At    time.Time `json:"at"`
	Class string    `json:"class"`
	// Filer is the role that filed it and the issue that role was working, or the controller.
	Filer      string `json:"filer"`
	FilerIssue string `json:"filer_issue"`
	// FilerPhase is the phase the filer's issue was in when it filed, named by the word a table
	// names it by (tablePhase): the eight words of the spec's "Phase words in tables" — `design`,
	// `planning`, `implementing`, `testing`, `reviewing`, `accepting`, `retro` and `merging` —,
	// `queued` for the waiting line, `awaiting_person` and `held` for the "no role" bar, `done`
	// for an hour the issue was out of the workflow before its close (outOfWorkflowSpans), with
	// observations or before them, and the empty string for a moment the issue's timeline does
	// not cover, which is what every filing by the controller carries.
	FilerPhase string `json:"filer_phase"`
}

// Compute is the metrics answer for a population. It reads nothing: a caller hands it a
// population Read gathered, so two callers of one population answer the same numbers.
func Compute(population *Population, scope Scope, now time.Time) Report {
	computed := &computation{
		population: population,
		scope:      scope,
		phases:     map[string]phaseTimeline{},
		views:      map[string]*issueView{},
		roots:      map[string]string{},
		workTimes:  map[string]workTimeRow{},
	}
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
		Splitting:  computed.splitting(finished),
	}
}

// computation is one answer being computed, with the timelines and views each metric shares.
type computation struct {
	population *Population
	scope      Scope
	phases     map[string]phaseTimeline
	views      map[string]*issueView
	// roots is each key's tree root and workTimes each issue's work-time row, each computed
	// once: TreeRoot walks a parent chain allocating a set as it goes, workTimeOf runs the whole
	// span algebra, and the four metrics ask for each of them three or four times per issue.
	roots     map[string]string
	workTimes map[string]workTimeRow
}

// phaseTimeline is one issue's phase timeline with where each part of it came from: the event
// log's reading before the issue's first observed phase, and the daemon's own observations from
// it on (Population.Phases).
type phaseTimeline struct {
	spans []PhaseSpan
	// firstObserved is the index of the first span the sampler observed, and len(spans) for an
	// issue it never observed.
	firstObserved int
}

// statusesBefore is the part of the issue's status history the rules before observations read:
// the statuses written before the observed part of its timeline begins, which is all of them for
// an issue the sampler never observed.
func (t phaseTimeline) statusesBefore(issue *Issue) []Mark {
	if t.firstObserved == len(t.spans) {
		return issue.StatusMarks
	}
	from := t.spans[t.firstObserved].Span.From
	if cut := slices.IndexFunc(issue.StatusMarks, func(mark Mark) bool { return !mark.At.Before(from) }); cut >= 0 {
		return issue.StatusMarks[:cut]
	}
	return issue.StatusMarks
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
// not the wider rule that places an issue in a tree (legion.IsLegionIssue): an issue the
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

// inScope is what the scope a caller names keeps, written once because two rules read it: the
// finished population (finished) and the filings of the splitting table (splitting) are narrowed
// the same way, so a reader of either number is reading the same rule. In `roots` an issue that
// is a root of its tree counts, in `children` one that is not, and in `all` every issue.
//
// It takes a key rather than an issue because the row of the splitting table is the issue that
// was filed: a filing is read by the tree the new issue is in, not by the filer's, so one filer's
// filings can fall under either scope, `roots` or `children`.
func (c *computation) inScope(key string) bool {
	switch c.scope {
	case ScopeRoots:
		return c.isRoot(key)
	case ScopeChildren:
		return !c.isRoot(key)
	}
	return true
}

// timelineOf is the issue's phase timeline with where each part of it came from, computed once.
func (c *computation) timelineOf(issue *Issue) phaseTimeline {
	if timeline, held := c.phases[issue.Key]; held {
		return timeline
	}
	spans, firstObserved := c.population.Phases(issue)
	timeline := phaseTimeline{spans: spans, firstObserved: firstObserved}
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
// `backlog` or `icebox`, any time back in the queue after it, and any time out of the workflow
// before the close."
func (c *computation) workTime(finished []*Issue) WorkTime {
	metric := WorkTime{Issues: make([]WorkTimeIssue, 0, len(finished))}
	hoursPer := make([]float64, 0, len(finished))
	for _, issue := range finished {
		row, ok := c.workTimeOf(issue)
		if !ok {
			continue
		}
		metric.Issues = append(metric.Issues, row)
		hoursPer = append(hoursPer, row.Hours)
	}
	slices.Sort(hoursPer)
	metric.MedianHours, metric.P90Hours = quantile(hoursPer, 0.5), quantile(hoursPer, 0.9)
	return metric
}

// workTimeOf is one finished issue's work time, computed once: the work-time metric asks for
// every finished issue's row, and the splitting metric asks again for every finished root.
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

// barSpansOf is the hours of an issue its bars share out: the work time metric 1 reports,
// less the time it sat in the queue, which is on no bar. Metric 2's bars and metric 3's hours
// after the first review are both the measure of these spans, so metric 3 counts no hour that
// metric 2's bars leave out.
func (c *computation) barSpansOf(issue *Issue) []Span {
	return subtract(c.workedSpansOf(issue), phaseSpansOf(c.phasesOf(issue), phaseQueued))
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

// computeWorkTime is the work time itself: the issue's life, less the time it sat parked, the
// time it sat back in the queue and the time it was out of the workflow. The spans that are left
// are the answer's second half, because they are what the issue's bars of phase time are shared
// out over. The queue and the time out of the workflow are read off the issue's own timeline
// (queuedSpans, outOfWorkflowSpans), the one the bars read, so the two cannot disagree on them.
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
	timeline := c.phasesOf(issue)
	// A life of no length leaves no span to park, to queue or to put on a bar, which the span
	// algebra below answers on its own.
	life := []Span{{From: start, To: end}}
	parked := intersect(c.population.parkedSpans(issue), life)
	queued := intersect(queuedSpans(timeline), life)
	out := intersect(outOfWorkflowSpans(timeline), life)
	worked := subtract(subtract(subtract(life, parked), queued), out)
	return workTimeRow{
		row: WorkTimeIssue{
			Key: issue.Key, Title: issue.Title, Priority: issue.Priority, Root: c.isRoot(issue.Key),
			StartedAt: start, ClosedAt: end, MergedAt: issue.MergedAt,
			Hours:              hours(totalOf(worked)),
			ParkedHours:        hours(totalOf(parked)),
			QueuedHours:        hours(totalOf(queued)),
			OutOfWorkflowHours: hours(totalOf(out)),
		},
		worked: worked,
		ok:     true,
	}
}

// firstWorkerEvent is where an issue's work time starts: "the issue's first worker event", the
// first event of any session titled `Legion <role> · <its key>` (Issue.FirstRoleEvent), and its
// first handoff only for an issue whose Legion work is handoffs alone.
//
// A handoff is recorded with a `handoff.recorded` event of the session that wrote it
// (api/handoffs.go), so a handoff a worker of the issue wrote is already among that role's
// events, and one written before the first of them is some other session's: it does not start
// the work time of an issue its workers wrote on.
func firstWorkerEvent(issue *Issue) (time.Time, bool) {
	var first time.Time
	for _, at := range issue.FirstRoleEvent {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	if !first.IsZero() {
		return first, true
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
// agree"). Each bar takes the phases it holds, so the bars are a partition of the work time: the
// timeline leaves no hour between two phases (the spec's "Phase boundaries before observations"),
// and every phase an issue can be in is on one of the bars but `queued` and `done`, the
// queue and the time out of the workflow before the close, whose hours the work time leaves out
// (queuedSpans, outOfWorkflowSpans): `step` is on the bar of the role each of its spans names
// (barsOf).
func (c *computation) phaseTime(finished []*Issue) PhaseTime {
	views := make([]*issueView, len(finished))
	timelines := make([][]PhaseSpan, len(finished))
	for index, issue := range finished {
		views[index] = c.viewOf(issue)
		timelines[index] = views[index].timeline
	}
	bars := barsOf(timelines)
	metric := PhaseTime{Bars: make([]RoleBar, len(bars))}
	perIssue := make([][]float64, len(bars))
	for index, bar := range bars {
		metric.Bars[index].Role = bar.Role
		perIssue[index] = make([]float64, 0, len(finished))
	}
	for issueIndex, issue := range finished {
		view := views[issueIndex]
		shared := c.barSpansOf(issue)
		for index, bar := range bars {
			spans := intersect(bar.spansOf(view.timeline), shared)
			segments := view.split(spans)
			// Every issue of the population counts towards each bar's median, one the role
			// never worked as a zero, so the medians describe one population and not one per
			// bar.
			perIssue[index] = append(perIssue[index], segments.total())
			if len(spans) == 0 {
				continue
			}
			metric.Bars[index].Segments.addSegments(segments)
			metric.Bars[index].Issues++
		}
	}
	var whole, idle float64
	for index := range metric.Bars {
		row := &metric.Bars[index]
		row.Hours = round(row.Segments.total())
		row.MedianHoursPerIssue = median(perIssue[index])
		row.Segments = rounded(row.Segments)
		whole += row.Hours
		idle += row.Segments.Idle
	}
	if whole > 0 {
		metric.IdleShare = round(idle / whole)
	}
	return metric
}

// rework is metric 3: review rounds, the share of issues sent back, and the implementing and
// testing hours after the first review.
func (c *computation) rework(finished []*Issue) Rework {
	metric := Rework{Issues: make([]ReworkIssue, 0, len(finished))}
	rounds, afterReview := make([]float64, 0, len(finished)), make([]float64, 0, len(finished))
	var sentBack int
	for _, issue := range finished {
		phases := c.timelineOf(issue)
		timeline := phases.spans
		row := ReworkIssue{Key: issue.Key, ReviewRounds: reviewRounds(issue, phases), SentBack: sentBackOnce(issue, phases)}
		if at, reviewed := firstReview(issue, phases); reviewed {
			after := intersect(c.barSpansOf(issue), []Span{{From: at, To: c.population.issueEnd(issue)}})
			row.ImplementingHoursAfterFirst = hours(totalOf(intersect(phaseSpansOf(timeline, legion.PhaseImplementing), after)))
			row.TestingHoursAfterFirst = hours(totalOf(intersect(phaseSpansOf(timeline, legion.PhaseTesting), after)))
		}
		if row.SentBack {
			sentBack++
		}
		rounds = append(rounds, float64(row.ReviewRounds))
		hoursAfter := row.ImplementingHoursAfterFirst + row.TestingHoursAfterFirst
		afterReview = append(afterReview, hoursAfter)
		metric.HoursAfterFirstReview += hoursAfter
		metric.Issues = append(metric.Issues, row)
	}
	metric.MedianReviewRounds = median(rounds)
	metric.MedianHoursAfterFirstReview = median(afterReview)
	metric.HoursAfterFirstReview = round(metric.HoursAfterFirstReview)
	if len(finished) > 0 {
		metric.SentBackShare = round(float64(sentBack) / float64(len(finished)))
	}
	return metric
}

// reviewRounds is "entries into `reviewing`, or into status `needs_review` before observations":
// the entries into `needs_review` of the status history written before the observed part of the
// issue's timeline begins, and the entries into `reviewing` of that observed part. A `reviewing`
// stretch the sampler first saw is no new entry where the event log's part ends in reviewing too.
// An issue whose only `phase` mark is the one the sampler's first pass left after it closed has
// no observed part: the stretch that mark opens lies after the close, so observedPhases clips it
// to nothing, and its rounds are its status entries.
func reviewRounds(issue *Issue, timeline phaseTimeline) int {
	rounds := statusEntries(timeline.statusesBefore(issue), statusNeedsReview)
	for index := timeline.firstObserved; index < len(timeline.spans); index++ {
		if timeline.spans[index].Phase == legion.PhaseReviewing && (index == 0 || timeline.spans[index-1].Phase != legion.PhaseReviewing) {
			rounds++
		}
	}
	return rounds
}

// sentBackPhases are the phases a send-back moves out of: "a move from `testing`, `reviewing`,
// `accepting` `retro`, `merging` or `awaiting_merge` to `implementing`". A tester's fail and a
// review that requests changes move out of testing and reviewing, and a backward move
// (`request_backward_move`, the daemon workflow's TriggerBackward rows) out of any of them. Before
// cybermonklabs/legion#4464 a head that started conflicting with its base sent the issue back out
// of any of them too, and the history this reads holds those moves. `step` is not one of them: a
// step of a role the deployment defines that stands before the implementer moves to implementing
// when it passes, a forward move, and a step after the implementer moves there when it fails, so
// a move from `step` to implementing says neither. Its run does (StepRun.Returned), and
// sentBackOnce reads that.
var sentBackPhases = []string{
	legion.PhaseTesting, legion.PhaseReviewing, legion.PhaseAccepting,
	legion.PhaseRetro, legion.PhaseMerging, legion.PhaseAwaitingMerge,
}

// sentBackOnce is whether the issue was sent back to `implementing`. Out of one of
// sentBackPhases, the move is read off the status history written before the observed part of
// its timeline begins, and off that observed part from there on, as the review rounds beside it
// are. Before observations the move is the status's (sentBackStatuses): `needs_review` is the
// status of both reviewing and accepting, `retro` the status of both retro and merging, and
// `in_progress` the status of implementing, but not the `in_progress` a step of a role the
// deployment defines starts with (Issue.stepStatus). Out of such a step, the send-back is the
// return of a run that failed (StepRun.Returned), before observations and after them alike: no
// status says it, since the step's `in_progress` is its own, and no phase does, since a step that
// passes can move to implementing too (sentBackPhases).
func sentBackOnce(issue *Issue, timeline phaseTimeline) bool {
	before := timeline.statusesBefore(issue)
	if statusMoved(slices.DeleteFunc(slices.Clone(before), issue.stepStatus), sentBackStatuses, statusImplementing) {
		return true
	}
	for _, run := range issue.Runs {
		if run.Returned && run.End != nil {
			return true
		}
	}
	for index := max(timeline.firstObserved, 1); index < len(timeline.spans); index++ {
		if timeline.spans[index].Phase == legion.PhaseImplementing && slices.Contains(sentBackPhases, timeline.spans[index-1].Phase) {
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

// firstReview is when the issue was first reviewed: its first entry into status `needs_review`
// before the observed part of its timeline begins, and else the first `reviewing` stretch of that
// observed part ("Rule details — The first review"), where the rounds and the send-back beside it
// are read off the same two halves ("Rule details — Review rounds and send-backs before
// observations").
func firstReview(issue *Issue, timeline phaseTimeline) (time.Time, bool) {
	if at, entered := firstStatusEntry(timeline.statusesBefore(issue), statusNeedsReview); entered {
		return at, true
	}
	return firstEntry(timeline.spans[timeline.firstObserved:], legion.PhaseReviewing)
}

// splitting is metric 4: each issue a Legion session created in the window, by the parent it had
// at its `issue.created` event, counted per week against the issues that finished per week. The
// rows and each week's four class counts are the filings the scope keeps, read by the issue the
// filing created (inScope); each week's finished count is the scoped population it is passed.
func (c *computation) splitting(finished []*Issue) Splitting {
	metric := Splitting{Filed: make([]FiledIssue, 0, len(c.population.Filed))}
	weeks := map[time.Time]*SplittingWeek{}
	week := func(at time.Time) *SplittingWeek {
		start := weekStart(at)
		if held, ok := weeks[start]; ok {
			return held
		}
		fresh := &SplittingWeek{Week: start, Filed: map[string]int{}}
		for _, class := range classes {
			fresh.Filed[class] = 0
		}
		weeks[start] = fresh
		return fresh
	}
	// The scope reads each filed issue's tree as it stands now (inScope), while its class reads
	// the parent it had when it was filed (classify); a filing into another project has no tree
	// here and counts as a root.
	for _, filed := range c.population.Filed {
		if !c.inScope(filed.Key) {
			continue
		}
		row := c.classify(filed)
		metric.Filed = append(metric.Filed, row)
		week(filed.At).Filed[row.Class]++
	}
	for _, issue := range finished {
		week(*issue.ClosedAt).Finished++
	}
	metric.Weeks = make([]SplittingWeek, 0, len(weeks))
	for _, one := range weeks {
		metric.Weeks = append(metric.Weeks, *one)
	}
	slices.SortFunc(metric.Weeks, func(a, b SplittingWeek) int { return a.Week.Compare(b.Week) })

	// The roots that finished in the window, whatever scope the caller named: the four root
	// figures are about roots by name, so no scope changes them.
	var withChildren, withoutChildren []float64
	var children int
	for _, key := range c.population.Keys {
		issue := c.population.Issues[key]
		if !c.finishedInWindow(issue) || !c.isRoot(issue.Key) {
			continue
		}
		metric.FinishedRoots++
		children += len(issue.Children)
		row, ok := c.workTimeOf(issue)
		if !ok {
			continue
		}
		if len(issue.Children) > 0 {
			withChildren = append(withChildren, row.Hours)
		} else {
			withoutChildren = append(withoutChildren, row.Hours)
		}
	}
	if metric.FinishedRoots > 0 {
		metric.ChildrenPerRoot = round(float64(children) / float64(metric.FinishedRoots))
	}
	metric.MedianHoursRootsWithChildren = median(withChildren)
	metric.MedianHoursRootsNoChildren = median(withoutChildren)
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

// tablePhase is the word a table names a phase by (the spec's "Phase words in tables"): "Every
// table names a phase by the bar it falls on: design, planning, implementing, testing, reviewing,
// accepting, retro, merging (for `merging` and `awaiting_merge`). So splitting and follow-ups
// names a filer in `awaiting_merge` as merging." The two phases of the merger's bar
// (mergerPhases) are named merging, and every other phase is named as the timeline names it:
// retro is on the implementer's bar and keeps its own word.
//
// The whole of what it answers: those eight words; the four words a timeline holds that are on
// no role's bar — `queued` and `done`, which are on no bar at all, being the waiting line and an
// hour out of the workflow before the close (queuedSpans, outOfWorkflowSpans), with observations
// or before them, and `awaiting_person` and `held`, which are the "no role" bar's; and "" for a
// moment no span of the timeline covers, which is what a filing by the controller carries, since
// the controller works no issue.
func tablePhase(phase string) string {
	if slices.Contains(mergerPhases, phase) {
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

// median sorts values and is their middle. Every caller builds the slice for its median alone
// and reads it no further, so the sort in place is unobservable.
func median(values []float64) float64 {
	slices.Sort(values)
	return quantile(values, 0.5)
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
