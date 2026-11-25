package metrics

import (
	"iter"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// Span is the half-open interval [From, To). Every rule here is written over spans rather than
// over clock hours: "each hour of a phase" is the hour as a measure, so a segment that holds for
// ten minutes claims ten minutes.
type Span struct {
	From time.Time
	To   time.Time
}

// Duration is the span's length, zero for an empty or inverted one.
func (s Span) Duration() time.Duration {
	if !s.To.After(s.From) {
		return 0
	}
	return s.To.Sub(s.From)
}

// The two phases this package adds to the daemon's own, which are named once in
// internal/dispatch/legion (legion.PhaseAdmitted and the rest, mirroring
// packages/daemon/internal/phase) and read from there. They are its reading of `admitted`,
// which the daemon does not split: an admitted issue holding a slot is its architect's design
// ("Design: roots that hold a slot while their phase is still `admitted`"), and an admitted
// issue holding none is the waiting line, whose time is on no bar ("Time in the queue is on no
// bar").
const (
	phaseQueued = "queued"
	phaseDesign = "design"
)

// childWorkingPhases are the phases "from planning to awaiting merge": a root with a child in one
// of them is waiting on its children (segment 4), `step` among them, where a role the deployment
// defines works a step of the child's task. `done` is none of them: a child in `done` before
// its close is out of the workflow, whose hours are outside its own work time too (the spec's "Out
// of the workflow before the close", outOfWorkflowSpans), so its root waits on it through none.
var childWorkingPhases = []string{
	legion.PhasePlanning, legion.PhaseImplementing, legion.PhaseTesting, legion.PhaseReviewing, legion.PhaseStep,
	legion.PhaseAccepting, legion.PhaseRetro, legion.PhaseMerging, legion.PhaseAwaitingMerge,
}

// parkedStatuses are the statuses whose time the work time subtracts: the daemon writes `backlog`
// when it parks a root, and the gate treats both as parked (daemon workflow/gate.go).
var parkedStatuses = []string{"backlog", "icebox"}

// The statuses the workflow writes with the phases a fallback timeline reads: the implementer's
// `in_progress`, the tester's `testing`, `needs_review` for both the reviewer's and the
// architect's, and `retro`, which the workflow writes as the implementer's retro starts and keeps
// through the merger's merging (daemon workflow/table.go's phaseStatus).
const (
	statusImplementing = "in_progress"
	statusTesting      = "testing"
	statusNeedsReview  = "needs_review"
	statusRetro        = "retro"
)

// The two statuses a stretch out of the workflow starts at before observations
// (outOfWorkflowByStatus): `triage`, and a `done` a reopen follows.
const (
	statusTriage = "triage"
	statusDone   = "done"
)

// sentBackStatuses are the statuses a send-back moves out of before observations exist:
// `needs_review` is the status of both reviewing and accepting, `testing` is its own, and
// `retro` is the status of both retro and merging, out of which a backward move
// (`request_backward_move`, the daemon workflow's TriggerBackward rows) sends the issue back to
// implementing like any other, as a head that started conflicting with its base did before
// cybermonklabs/legion#4464. Those are the words the spec's "an `in_progress` after `testing`,
// `needs_review` or `retro` (a send-back) is implementing" is read by.
var sentBackStatuses = []string{statusTesting, statusNeedsReview, statusRetro}

// phaseOrder is the order of the default steps' phases a fallback timeline names, the order the
// daemon's table of today's line (phase.Order) moves an issue with no plan rows through. It is the
// line of a moment whose run has no plan this reading can place (Issue.phaseLine).
var phaseOrder = []string{
	phaseDesign, legion.PhasePlanning, legion.PhaseImplementing, legion.PhaseTesting,
	legion.PhaseReviewing, legion.PhaseAccepting, legion.PhaseRetro, legion.PhaseMerging,
}

// stepPhase is the phase the role of each step a plan names runs as (the daemon's
// plan.BuiltinRoles): the planner's planning, the implementer's implementing, the tester's
// testing, the reviewer's reviewing, and the owner's acceptance, accepting. Every other role a
// step names is one the deployment defines, whose step runs as `step` (ownRole, phaseLine).
var stepPhase = map[string]string{
	"planner":     legion.PhasePlanning,
	"implementer": legion.PhaseImplementing,
	"tester":      legion.PhaseTesting,
	"reviewer":    legion.PhaseReviewing,
	"acceptance":  legion.PhaseAccepting,
}

// phaseLine is the order of the phases a fallback timeline names at the moment at: design, then
// the phase of each step of the task the issue's plan names, in the task's order, `step` for a
// step of a role the deployment defines, then retro and merging. The plan is the one of the
// newest run whose node was made at or before at, or the first run's where at falls before every
// plan, since a run's design comes before its plan is written. An issue with no plan, or a plan
// whose steps this reading cannot place, reads the default steps' order (phaseOrder). Three rules
// read it: which phase a handoff starts (handoffStart), the rank that keeps a role's first event
// from starting a phase the issue has passed (phaseSteps), and the phase `needs_review` names
// (statusPhase). Where a step of a role the deployment defines stands is where its run (Issue.Runs)
// starts it, which names its role.
func (i *Issue) phaseLine(at time.Time) []string {
	if len(i.Plans) == 0 {
		return phaseOrder
	}
	plan := i.Plans[0]
	for _, run := range i.Plans[1:] {
		if run.Made.After(at) {
			break
		}
		plan = run
	}
	if len(plan.Steps) == 0 {
		return phaseOrder
	}
	line := make([]string, 0, len(plan.Steps)+3)
	line = append(line, phaseDesign)
	for _, step := range plan.Steps {
		phase, placed := stepPhase[step]
		switch {
		case !placed && ownRole(step):
			return phaseOrder
		case !placed:
			phase = legion.PhaseStep
		}
		line = append(line, phase)
	}
	return append(line, legion.PhaseRetro, legion.PhaseMerging)
}

// rolePhase is the phase each role's first event on the issue starts, until the issue is first
// sent back (phaseSteps). The implementer works two phases, implementing and retro, "because the
// daemon gives retro to the implementer" (RoleFor in packages/daemon/internal/phase), and its
// first event starts the first of them: retro starts at the `retro` status the workflow writes
// for it (statusPhase). The merger's first event starts merging, which the event log tells from
// retro by nothing else, since the workflow keeps the status `retro` through both: so before
// observations an issue holds no merging after its first send-back, and a retro there runs on to
// the next status or the close. No mark of the event log names awaiting merge.
var rolePhase = map[string]string{
	"architect":   phaseDesign,
	"planner":     legion.PhasePlanning,
	"implementer": legion.PhaseImplementing,
	"tester":      legion.PhaseTesting,
	"reviewer":    legion.PhaseReviewing,
	"merger":      legion.PhaseMerging,
}

// ownRole says whether role is one of Legion's own: a role whose first event starts a phase of its
// own (rolePhase), or the owner's `acceptance`, which runs as accepting. Every other role a step
// names is one the deployment defines, which runs as `step` (StepRun, PhaseSpan.Role).
func ownRole(role string) bool {
	_, own := rolePhase[role]
	return own || role == "acceptance"
}

// handoffPhase is the phase each handoff word (the daemon's phase.Handoff names them) ends: the
// planner's `plan` ends planning, the implementer's `implement` implementing, the tester's `test`
// testing and the reviewer's `review` reviewing. Retro and merging write none, since each reports
// the commit it stands on, accepting ends at the architect's sign-off, which is no handoff (the
// daemon's phase.Handoff), and a step of a role the deployment defines writes none either. A word
// Legion never writes ends no phase.
var handoffPhase = map[string]string{
	"plan":      legion.PhasePlanning,
	"implement": legion.PhaseImplementing,
	"test":      legion.PhaseTesting,
	"review":    legion.PhaseReviewing,
}

// handoffStart is the phase a handoff of word starts on line (Issue.phaseLine): the phase after
// the one the word ends. In the default steps the planner's `plan` starts implementing, the
// implementer's `implement` testing, the tester's `test` reviewing and the reviewer's `review`
// accepting ("Accepting starts at the `review` handoff"), and the handoff of a task's last step
// starts its retro. The spec's "a handoff starts the next phase" is this rule: a handoff ends the
// phase it names and leaves no hour between the two. A phase the line does not hold starts
// nothing, and neither does a handoff whose next step is a step of a role the deployment
// defines: that step starts at its run, which names its role (fallbackMarks), where `step` alone
// would name none.
func handoffStart(line []string, word string) (string, bool) {
	ends, known := handoffPhase[word]
	at := slices.Index(line, ends)
	if !known || at < 0 || at+1 >= len(line) || line[at+1] == legion.PhaseStep {
		return "", false
	}
	return line[at+1], true
}

// PhaseSpan is one stretch of one phase. Role is the role of the step a `step` span is of, a role
// the deployment defines, whose bar holds its hours (barsOf); "" for every other phase, whose
// role the phase itself names.
type PhaseSpan struct {
	Phase string
	Role  string
	Span  Span
}

// Phases is the issue's phase timeline and the index of the first of its spans the sampler
// observed, which is the timeline's length for an issue the sampler never observed.
//
// The event log draws the timeline up to the issue's first observed phase, and the daemon's own
// observations draw it from there on: "An issue in flight when its first observation is stored
// takes its phases from observations only from that observation on. Its hours before it come
// from these status rules, so they keep the phases its events mark rather than all falling on
// the first phase the sampler saw" (the spec's "Statuses before observations"). So an issue the
// sampler met before any mark of the event log is its observations' whole, and one it never met
// is the event log's whole (appendStepSpans, the spec's "Phase boundaries before observations").
//
// Before observations, the event log's statuses also say where the issue was out of the workflow
// (outOfWorkflowByStatus), and those stretches are laid over its steps as `done`, the phase the
// daemon reports for such an hour, so the two timelines name it alike and the work time and the
// bars read it off either one the same way (outOfWorkflowSpans). The steps run on beneath a
// stretch, as they run on beneath a park: after it the phase is the reopen's own where its status
// marks one ("the reopen's status sets the phase from then", the spec's "The close"), and where it
// marks none, a `todo` or an admission's `in_progress`, the phase the issue had before the stretch.
//
// Where the observations take over is the answer's second half because the rules that read a
// phase before observations — metric 3's review rounds, its send-backs and the first review its
// hours are counted from — read the status history up to that moment. An issue whose only
// observation is the mark the sampler left after it closed has no observed hour of its own life:
// the stretch that mark opens lies after the close, so observedPhases clips it to nothing, and the
// whole of its life is before observations.
func (p *Population) Phases(issue *Issue) ([]PhaseSpan, int) {
	until := p.issueEnd(issue)
	steps := phaseSteps(p.fallbackMarks(issue))
	observed := p.observedPhases(issue)
	if len(observed) > 0 {
		until = observed[0].Span.From
		if cut := slices.IndexFunc(steps, func(step phaseMark) bool { return !step.at.Before(until) }); cut >= 0 {
			steps = steps[:cut]
		}
	}
	// An hour of the work time before the first phase starts is on that phase's bar (the spec's
	// "Work hours before any phase mark"), so no hour of the work time is off every bar. The
	// first phase is the event log's first step where one falls before the first observation,
	// and the first observed phase where none does. Every role's first event is a mark of its
	// own, so this reaches back only on an issue no worker session wrote on, whose work time a
	// handoff naming no phase opens (firstWorkerEvent).
	if at, worked := firstWorkerEvent(issue); worked {
		switch {
		case len(steps) > 0:
			if at.Before(steps[0].at) {
				steps[0].at = at
			}
		case len(observed) > 0:
			if at.Before(observed[0].Span.From) {
				observed[0].Span.From = at
			}
		}
	}
	timeline := appendStepSpans(make([]PhaseSpan, 0, len(steps)+len(observed)), steps, until)
	if out := p.outOfWorkflowByStatus(issue); len(out) > 0 {
		timeline = layOutOfWorkflow(timeline, out)
	}
	firstObserved := len(timeline)
	return append(timeline, observed...), firstObserved
}

// layOutOfWorkflow lays stretches out of the workflow over the event log's part of a timeline:
// each step keeps its hours outside them, and its hours inside them are `done`. A stretch that
// runs on past the event log's part is the observations' to name from there (Phases), so only
// the steps it overlaps are cut. The answer keeps the room Phases made for the observed spans.
func layOutOfWorkflow(timeline []PhaseSpan, out []Span) []PhaseSpan {
	laid := make([]PhaseSpan, 0, cap(timeline)+2*len(out))
	for _, step := range timeline {
		for _, kept := range subtract([]Span{step.Span}, out) {
			laid = append(laid, PhaseSpan{Phase: step.Phase, Role: step.Role, Span: kept})
		}
		for _, left := range intersect([]Span{step.Span}, out) {
			laid = append(laid, PhaseSpan{Phase: legion.PhaseDone, Span: left})
		}
	}
	slices.SortFunc(laid, func(a, b PhaseSpan) int { return a.Span.From.Compare(b.Span.From) })
	return laid
}

// observedPhases is the timeline the sampler's `phase` observations draw, with each admitted
// stretch split into the design the issue held a slot through and the queue it did not, and each
// `step` stretch split by the role of the step it ran (stepRoleSpans).
func (p *Population) observedPhases(issue *Issue) []PhaseSpan {
	end := p.issueEnd(issue)
	var timeline []PhaseSpan
	for stretch := range p.phaseStretches(issue) {
		if stretch.Span.Duration() == 0 {
			continue
		}
		switch stretch.Phase {
		case legion.PhaseStep:
			timeline = append(timeline, stepRoleSpans(issue, stretch.Span)...)
			continue
		case legion.PhaseAdmitted:
		default:
			timeline = append(timeline, stretch)
			continue
		}
		held := intersect([]Span{stretch.Span}, p.slotSpans(issue, end))
		for _, holding := range held {
			timeline = append(timeline, PhaseSpan{Phase: phaseDesign, Span: holding})
		}
		for _, waiting := range subtract([]Span{stretch.Span}, held) {
			timeline = append(timeline, PhaseSpan{Phase: phaseQueued, Span: waiting})
		}
	}
	slices.SortFunc(timeline, func(a, b PhaseSpan) int { return a.Span.From.Compare(b.Span.From) })
	return timeline
}

// stepRoleSpans is the observed `step` stretch span cut where the issue's step role changed
// (legion.FieldStepRole), each part a `step` span of the role the daemon reported through it. The
// role the stretch starts in is the newest step_role mark at or before its start, `null` where
// the issue left its last step since; where the read that named the role landed a moment after
// the stretch began, it is the first role named in the stretch. Inside the stretch a `null` mark
// ends no step, as a `null` phase mark ends no phase (phaseStretches): it is a read that no longer
// carried the issue. A stretch no read named a role for keeps the role "".
func stepRoleSpans(issue *Issue, span Span) []PhaseSpan {
	role := ""
	var cuts []PhaseSpan
	for _, mark := range issue.Observed[legion.FieldStepRole] {
		named := legion.JSONString([]byte(mark.Value))
		if !mark.At.After(span.From) {
			role = named
			continue
		}
		if !mark.At.Before(span.To) {
			break
		}
		if named == "" {
			continue
		}
		if role == "" && len(cuts) == 0 {
			role = named
			continue
		}
		if named != role {
			cuts = append(cuts, PhaseSpan{Phase: legion.PhaseStep, Role: role, Span: Span{From: span.From, To: mark.At}})
			span.From, role = mark.At, named
		}
	}
	return append(cuts, PhaseSpan{Phase: legion.PhaseStep, Role: role, Span: span})
}

// phaseStretches is each stretch the issue's `phase` marks hold a phase through, in time order:
// each mark that carries a phase runs to the next mark that carries one, and the last to the
// issue's end. A stretch can be empty, or lie after the close, and the reader drops it.
//
// A `null` mark is a read that no longer carried the issue's phase, which the sampler records for
// every field a read drops: it says the daemon stopped reporting the issue, not that its phase
// changed, so it names no phase and ends none. The phase before it runs on through it, to the
// next mark that carries a phase or to the issue's end where none follows, and its hours stay on
// the bar of the phase the issue was last seen in, so the bars of the issue still add up to its
// work time (the spec's "Parked and pre-work hours in phase time"). Both readers of an issue's
// phase stretches take them from here, the timeline (observedPhases) and the root's recorded wait
// on a person (waitingOnPersonSpans), so the two cannot read one mark two ways. slotSpans reads
// the drops through markedPhase too.
func (p *Population) phaseStretches(issue *Issue) iter.Seq[PhaseSpan] {
	return func(yield func(PhaseSpan) bool) {
		var open PhaseSpan
		for _, mark := range issue.Observed[legion.FieldPhase] {
			phase := markedPhase(mark)
			if phase == "" {
				continue
			}
			if open.Phase != "" {
				open.Span.To = mark.At
				if !yield(open) {
					return
				}
			}
			open = PhaseSpan{Phase: phase, Span: Span{From: mark.At}}
		}
		if open.Phase != "" {
			open.Span.To = p.issueEnd(issue)
			yield(open)
		}
	}
}

// markedPhase is the phase a `phase` mark names, and "" for a mark that names none: a read that no
// longer carried the issue.
func markedPhase(mark Mark) string { return legion.JSONString([]byte(mark.Value)) }

// slotSpans are the stretches the issue held an admission slot, from its own `slot` observations:
// never from the daemon's `admission.active`, which dispatch://INFRA-980 widens to every issue
// holding one, children included.
//
// A slot mark that holds an index starts a hold, and the hold ends where the daemon released the
// slot: at a slot mark that holds none in a read that still carries the issue. The sampler writes
// two facts as a `null` slot mark, and only the phase marks of the same moment tell them apart. A
// read that carries the issue observes its slot, `null` where it holds none (legion.State's
// Observations), and its phase, which is never `null`: so a `null` slot mark beside a phase mark
// that names a phase, or beside none, is a release. A read that no longer carries the issue writes
// `null` for every field of it at one moment (legion's recordObservations): so a `null` slot mark
// at the moment the phase mark went `null` is that drop, and a drop is not a release. It says the
// daemon stopped reporting the issue, not that the issue gave up its slot, just as it is no news
// of a phase change (phaseStretches): the hold runs on through it, so an admitted issue last seen
// holding a slot stays in design and its hours stay on the architect's bar, since no read observed
// it in the queue.
//
// The hold the drop left open runs to the first read that carries the issue again, which is the
// next phase mark that names a phase. The issue's newest slot row is then the drop's `null`, so
// that read writes a slot mark only where the issue holds a slot, and a return with no slot mark
// at its moment is a release. Where no read carries the issue again, the hold runs to end.
//
// This is the one reader of the slot marks. The timeline's split of an admitted stretch into
// design and queue (observedPhases), and so the queue time (queuedSpans) and every bar, take them
// from here, as does the start of the design phase before observations (fallbackMarks).
func (p *Population) slotSpans(issue *Issue, end time.Time) []Span {
	phases, slots := issue.Observed[legion.FieldPhase], issue.Observed[legion.FieldSlot]
	var spans []Span
	var from time.Time
	holding, gone := false, false
	for len(phases) > 0 || len(slots) > 0 {
		// The next moment either field has a mark at, and the mark each has there: both are one
		// read's, since a read writes every row it writes at the one moment.
		var moment time.Time
		if len(slots) == 0 || len(phases) > 0 && phases[0].At.Before(slots[0].At) {
			moment = phases[0].At
		} else {
			moment = slots[0].At
		}
		var phase, slot *Mark
		if len(phases) > 0 && phases[0].At.Equal(moment) {
			phase, phases = &phases[0], phases[1:]
		}
		if len(slots) > 0 && slots[0].At.Equal(moment) {
			slot, slots = &slots[0], slots[1:]
		}
		dropped := phase != nil && markedPhase(*phase) == ""
		holds := holding
		switch {
		case slot != nil:
			// A slot the read carried decides, except a drop's `null`, which ends nothing.
			if held := slot.Value != "" && slot.Value != "null"; held || !dropped {
				holds = held
			}
		case phase != nil && gone && !dropped:
			// The issue is carried again, holding no slot.
			holds = false
		}
		if phase != nil {
			gone = dropped
		}
		switch {
		case holds && !holding:
			from = moment
		case !holds && holding:
			spans = append(spans, Span{From: from, To: moment})
		}
		holding = holds
	}
	if holding {
		spans = append(spans, Span{From: from, To: end})
	}
	return normalize(spans)
}

// phaseMark is one moment the event log says the issue's phase changed, and what says so.
// sendBack is statusPhase's own decision that a status change sends the issue back to
// implementing, or a failed run's return (fallbackMarks); a role's first event and a handoff
// never do. role is the role of a `step` mark, the role the deployment defines whose run it is.
// rank is the phase's place on the issue's line at that moment (Issue.phaseLine, phaseRank).
type phaseMark struct {
	at       time.Time
	source   markSource
	phase    string
	role     string
	sendBack bool
	rank     int
}

// markSource is which of the sources wrote a mark. It is also the order two marks of one moment
// are read in, and the last of them is the phase: a role's first event is the earliest word on a
// change, the handoff that starts the phase is the next, the status the workflow wrote with it the
// next, and the run of a step of a role the deployment defines, which Dispatch records itself
// (Issue.Runs), the last: its end before its start, so a run that starts where another ended is
// the phase from then.
type markSource int

const (
	sourceRole markSource = iota
	sourceHandoff
	sourceStatus
	sourceRunEnd
	sourceRunStart
)

// appendStepSpans appends the timeline the event log's steps draw up to until: the phase the
// issue is in, as its role events, its handoffs and its Dispatch statuses move it, each phase
// running to the next one's start and the last to until.
//
// The spec's "Phase boundaries before observations": "Each phase ends, and the next one starts,
// at the earlier of the phase's own handoff and the next role's first event ... So a handoff
// starts the next phase, and no hour between phases is left over ... After a send-back, the
// Dispatch status sets the phase". The three sources are therefore one list of marks and the
// timeline is the step function they draw, with no gap between one phase and the next — which is
// what lets the bars of metric 2 add up to the work time while the no-role bar holds
// `awaiting_person` and `held` alone.
func appendStepSpans(timeline []PhaseSpan, steps []phaseMark, until time.Time) []PhaseSpan {
	for index, step := range steps {
		span := Span{From: step.at, To: until}
		if index+1 < len(steps) && steps[index+1].at.Before(until) {
			span.To = steps[index+1].at
		}
		if span.Duration() == 0 {
			continue
		}
		timeline = append(timeline, PhaseSpan{Phase: step.phase, Role: step.role, Span: span})
	}
	return timeline
}

// fallbackMarks is every mark the event log holds of an issue's phase, in time order: each role's
// first event on the issue, each handoff, each change of its Dispatch status, and each run of a
// step of a role the deployment defines.
func (p *Population) fallbackMarks(issue *Issue) []phaseMark {
	architect, hasArchitect := issue.FirstRoleEvent["architect"]
	var marks []phaseMark
	add := func(source markSource, at time.Time, phase, role string, sendBack bool) {
		marks = append(marks, phaseMark{at: at, source: source, phase: phase, role: role, sendBack: sendBack, rank: phaseRank(issue.phaseLine(at), phase)})
	}
	for role, at := range issue.FirstRoleEvent {
		if phase, works := rolePhase[role]; works && role != "architect" {
			add(sourceRole, at, phase, "", false)
		}
	}
	// "An issue has a design phase when a session titled `Legion architect · <its own key>` wrote
	// an event on it ... whether or not it has a parent. The design phase runs from the issue's
	// first slot, or else from that architect's first event". A slot the sampler recorded after
	// the architect had spoken is a later admission, not the start of the design, so the earlier
	// of the two opens the phase.
	if hasArchitect {
		if slots := p.slotSpans(issue, p.issueEnd(issue)); len(slots) > 0 && slots[0].From.Before(architect) {
			architect = slots[0].From
		}
		add(sourceRole, architect, phaseDesign, "", false)
	}
	for _, handoff := range issue.Handoffs {
		if phase, starts := handoffStart(issue.phaseLine(handoff.At), handoff.Phase); starts {
			add(sourceHandoff, handoff.At, phase, "", false)
		}
	}
	previous := ""
	for _, mark := range issue.StatusMarks {
		if mark.Value == previous {
			continue
		}
		if phase, sendBack, named := statusPhase(previous, mark.Value, issue.phaseLine(mark.At)); named && !issue.stepStatus(mark) {
			add(sourceStatus, mark.At, phase, "", sendBack)
		}
		previous = mark.Value
	}
	// A run of a step of a role the deployment defines starts that role's time at `step`, and a
	// run that failed and returned its task sends the issue back to implementing as it ends. No
	// handoff says either, and no status does: the workflow writes `in_progress` for `step` as for
	// implementing (stepStatus), so the run is the one word of where the issue stands.
	for _, run := range issue.Runs {
		add(sourceRunStart, run.Start, legion.PhaseStep, run.Role, false)
		if run.Returned && run.End != nil {
			add(sourceRunEnd, *run.End, legion.PhaseImplementing, "", true)
		}
	}
	slices.SortFunc(marks, func(a, b phaseMark) int {
		if at := a.at.Compare(b.at); at != 0 {
			return at
		}
		if source := int(a.source) - int(b.source); source != 0 {
			return source
		}
		return a.rank - b.rank
	})
	return marks
}

// phaseSteps is the step function the marks draw: the phase the issue is in, changing only where
// a mark changes it.
func phaseSteps(marks []phaseMark) []phaseMark {
	var steps []phaseMark
	reached := -1
	sentBack := false
	for _, mark := range marks {
		rank := mark.rank
		// A role's first event starts that role's phase only where the issue has not reached
		// that phase already: the boundary the spec draws is "the next role's first event", so a
		// role that speaks again after its phase has passed — an implementer answering during
		// retro — does not send the issue back. Nor does any role's first event start a phase
		// once the issue has been sent back, however late that phase: "After a send-back, the
		// next phases come from status writes and handoffs" (the spec's "No re-entry by a first
		// event"). A handoff and a status move the phase either way, before a send-back and
		// after it.
		if mark.source == sourceRole && (sentBack || rank <= reached) {
			continue
		}
		// The send-back is the status change statusPhase decides is one (phaseMark.sendBack), the
		// same move metric 3 counts as a send-back before observations (sentBackOnce). A handoff
		// that names an earlier phase moves the phase back, but it is no send-back.
		if mark.sendBack {
			sentBack = true
		}
		reached = max(reached, rank)
		last := len(steps) - 1
		if last >= 0 && steps[last].phase == mark.phase && steps[last].role == mark.role {
			continue
		}
		if last >= 0 && steps[last].at.Equal(mark.at) {
			// Two marks of one moment are one change, and the last word on it is the phase.
			steps[last].phase, steps[last].role = mark.phase, mark.role
			if last > 0 && steps[last-1].phase == mark.phase && steps[last-1].role == mark.role {
				steps = steps[:last]
			}
			continue
		}
		steps = append(steps, mark)
	}
	return steps
}

// phaseRank is a phase's place on line (Issue.phaseLine), and -1 for one the line does not hold.
func phaseRank(line []string, phase string) int {
	return slices.Index(line, phase)
}

// statusPhase is the phase a status change sets before observations exist, on the issue's line
// at that moment (Issue.phaseLine), and whether that change is a send-back: the spec's "After a
// send-back, the Dispatch status sets the phase: `in_progress` after `testing`, `needs_review` or
// `retro` is implementing, `testing` is testing, and `needs_review` is reviewing until the
// `review` handoff of that round, then accepting until the status changes". The workflow writes
// `needs_review` for both reviewing and accepting, so it names the first of the two the line
// holds: reviewing in the default steps, where the handoff mark turns the rest into accepting,
// and accepting on a task whose acceptance comes before its review or that has no reviewer step.
// This is the one place the timeline decides a send-back, which phaseSteps reads off the mark.
//
// The statuses that name no phase of their own — `todo`, `triage`, `backlog`, `icebox` and
// `done` — leave the phase as it was. The work time leaves out the hours of all of them but
// `todo`, whatever phase the issue was in: `backlog` and `icebox` are parked (parkedSpans), a
// `triage`, or a `done` a reopen follows, is out of the workflow, which Phases lays over these
// steps as `done` (outOfWorkflowByStatus), and the last `done` is the close, where the issue's
// life ends. The phase beneath a park or a stretch out of the workflow runs on after it, unless
// the status that ends it names one. The hours after a `todo` stay on the phase the issue had,
// since before observations the queue is none (queuedSpans).
//
// Neither does the `in_progress` of admission: "A status write such as the `in_progress` of
// admission starts no phase" and "Design runs once, to the first planner event: an issue parked
// and admitted again goes back into the phase it left, not into design" (the spec's "What starts
// planning"). The design mark is `fallbackMarks`' own, from the issue's first slot or its
// architect's first event, and planning starts at the first planner event, so an `in_progress`
// here names the send-back alone.
func statusPhase(previous, status string, line []string) (phase string, sendBack, named bool) {
	switch status {
	case statusTesting:
		return legion.PhaseTesting, false, true
	case statusNeedsReview:
		for _, held := range line {
			if held == legion.PhaseReviewing || held == legion.PhaseAccepting {
				return held, false, true
			}
		}
		return legion.PhaseReviewing, false, true
	case statusRetro:
		return legion.PhaseRetro, false, true
	case statusImplementing:
		// The workflow writes `in_progress` three times over: where it admits an issue out of the
		// queue or out of parked, for every send-back to implementing, whatever phase the issue
		// comes back from (workflow/table.go's TriggerTesterFailed, TriggerReviewRejected and
		// TriggerBackward all write it, and before cybermonklabs/legion#4464 a conflict's move did
		// too), and for every start of a step of a role of the deployment (`step`). Only a move out
		// of `testing`, `needs_review` or `retro` names a phase here, and it is read as a send-back;
		// the start of such a step is told from it by the step's run (stepStatus), which its
		// readers ask first.
		if slices.Contains(sentBackStatuses, previous) {
			return legion.PhaseImplementing, true, true
		}
		return "", false, false
	}
	return "", false, false
}

// stepStatus is whether mark is the `in_progress` the workflow writes as a step of a role the
// deployment defines starts (the daemon's status of `step`): one written while a run of such a
// step is open, from its start to its end (Issue.Runs). It names no phase of its own and is no
// send-back, whatever status came before it, since the run itself says whose time it is
// (fallbackMarks). Both readers of a status before observations ask it: the phase marks and the
// send-backs of metric 3 (sentBackOnce).
func (i *Issue) stepStatus(mark Mark) bool {
	if mark.Value != statusImplementing {
		return false
	}
	for _, run := range i.Runs {
		if !mark.At.Before(run.Start) && (run.End == nil || !mark.At.After(*run.End)) {
			return true
		}
	}
	return false
}

// enteredStatus is whether the mark at index of a status history is where the status became
// status: the mark holds it and the one before does not.
func enteredStatus(marks []Mark, index int, status string) bool {
	return marks[index].Value == status && (index == 0 || marks[index-1].Value != status)
}

// firstStatusEntry is when a status history first became status.
func firstStatusEntry(marks []Mark, status string) (time.Time, bool) {
	for index, mark := range marks {
		if enteredStatus(marks, index, status) {
			return mark.At, true
		}
	}
	return time.Time{}, false
}

// statusEntries counts how many times a status history became status.
func statusEntries(marks []Mark, status string) int {
	entries := 0
	for index := range marks {
		if enteredStatus(marks, index, status) {
			entries++
		}
	}
	return entries
}

// statusMoved is whether a status history ever went straight from one of `from` to `to`, which
// is how a move between two phases reads before any observation of them exists.
func statusMoved(marks []Mark, from []string, to string) bool {
	for index := 1; index < len(marks); index++ {
		if marks[index].Value == to && slices.Contains(from, marks[index-1].Value) {
			return true
		}
	}
	return false
}

// parkedSpans are the stretches the issue sat in a parked status (backlog or icebox), from the
// statuses its own events snapshotted.
func (p *Population) parkedSpans(issue *Issue) []Span {
	return markSpans(issue.StatusMarks, p.issueEnd(issue), func(value string) bool {
		return slices.Contains(parkedStatuses, value)
	})
}

// outOfWorkflowByStatus is the stretches in which the issue was out of the workflow before
// observations, from the statuses its own events snapshotted: "the stretch from an
// `issue.closed` that a reopen follows, or from a `triage` status write, to the next status
// write" (the spec's "Out of the workflow before the close"). The close is the issue's last move
// to `done` (the spec's "The close"), so the `done` that ends its status history, and any
// snapshot of it Dispatch wrote after, starts no stretch: only a `done` some other status follows
// does, which is a close a reopen undid. Phases reads them over the event log's part of the
// timeline alone, since with observations the observed `done` says where the issue was out.
func (p *Population) outOfWorkflowByStatus(issue *Issue) []Span {
	marks := issue.StatusMarks
	closing := len(marks)
	for closing > 0 && marks[closing-1].Value == statusDone {
		closing--
	}
	end := p.issueEnd(issue)
	if closing < len(marks) {
		end = marks[closing].At
	}
	return markSpans(marks[:closing], end, func(value string) bool {
		return value == statusTriage || value == statusDone
	})
}

// outOfWorkflowSpans is the timeline's hours out of the workflow, and there is one definition of
// them: "An hour in which the issue is out of the workflow, before its close, is outside the work
// time and on no bar, as a park is" (the spec's "Out of the workflow before the close"). They are
// the timeline's own `done` spans. With observations, that is "every stretch whose observed phase
// is `done` before the close: a merge before Dispatch's status write, a reopened issue's done
// stretch, and a child that left the workflow, such as one moved to `triage`", all of which the
// daemon reports as `done`; before observations, the stretches the issue's statuses mark, which
// Phases lays over the event log's steps as `done` (outOfWorkflowByStatus). So the work time and
// the bars cannot read them two ways, and the work time reads them inside the issue's life alone,
// which ends at the close.
func outOfWorkflowSpans(timeline []PhaseSpan) []Span {
	return phaseSpansOf(timeline, legion.PhaseDone)
}

// queuedSpans is the timeline's queue time, and there is one definition of it: "every observed
// stretch in which the issue's phase is `admitted` and it holds no slot, whenever it falls. Work
// time to merge and every bar of phase time subtract the same queue time. A root in `admitted`
// that holds a slot is in design, not in the queue" (the spec's "Rule details — Queue time, one
// definition"). It is the phase timeline's own `queued` spans, so the work time and the bars
// cannot read it two ways and the bars of an issue add up to its work time on either
// timeline. Whether the issue holds a slot is slotSpans' answer, in which the daemon dropping the
// issue from its state releases nothing: an issue keeps through a drop the phase and the slot it
// was last seen with, so one that held a slot when it dropped is not in the queue until a read
// carries it again holding none.
//
// Before observations it is none: the timeline the event log draws holds no queued phase, and
// the parked subtraction already covers the daemon's own parking.
func queuedSpans(timeline []PhaseSpan) []Span {
	return phaseSpansOf(timeline, phaseQueued)
}

// markSpans is the stretches in which a field's recorded value satisfies holds, from the marks the
// sampler wrote for it: each mark runs until the next, and the last until end.
func markSpans(marks []Mark, end time.Time, holds func(value string) bool) []Span {
	var spans []Span
	for index, mark := range marks {
		if !holds(mark.Value) {
			continue
		}
		span := Span{From: mark.At, To: end}
		if index+1 < len(marks) {
			span.To = marks[index+1].At
		}
		if span.Duration() > 0 {
			spans = append(spans, span)
		}
	}
	return normalize(spans)
}

// phaseSpansOf is every span of the timeline whose phase is one of phases.
func phaseSpansOf(timeline []PhaseSpan, phases ...string) []Span {
	var spans []Span
	for _, one := range timeline {
		if slices.Contains(phases, one.Phase) {
			spans = append(spans, one.Span)
		}
	}
	return normalize(spans)
}

// isNormalized is whether spans are already what normalize would return: every span positive and
// each one starting strictly after the one before ends. An empty set is not, so normalize keeps
// answering nil for it.
func isNormalized(spans []Span) bool {
	if len(spans) == 0 {
		return false
	}
	for index, span := range spans {
		if span.Duration() <= 0 {
			return false
		}
		if index > 0 && !span.From.After(spans[index-1].To) {
			return false
		}
	}
	return true
}

// normalize drops the empty spans, orders what is left and merges what overlaps, so the set
// algebra below needs no special cases. A set already in that shape — which is what every
// caller here hands it, since each one is itself the answer of a normalize — is returned as it
// is, rather than sorted and copied again.
func normalize(spans []Span) []Span {
	if isNormalized(spans) {
		return spans
	}
	kept := make([]Span, 0, len(spans))
	for _, span := range spans {
		if span.Duration() > 0 {
			kept = append(kept, span)
		}
	}
	slices.SortFunc(kept, func(a, b Span) int { return a.From.Compare(b.From) })
	merged := kept[:0]
	for _, span := range kept {
		if last := len(merged) - 1; last >= 0 && !span.From.After(merged[last].To) {
			if span.To.After(merged[last].To) {
				merged[last].To = span.To
			}
			continue
		}
		merged = append(merged, span)
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// intersect is the time both sets cover.
func intersect(left, right []Span) []Span {
	left, right = normalize(left), normalize(right)
	var shared []Span
	for _, one := range left {
		for _, other := range right {
			if !other.To.After(one.From) {
				continue
			}
			if !other.From.Before(one.To) {
				break
			}
			span := Span{From: one.From, To: one.To}
			if other.From.After(span.From) {
				span.From = other.From
			}
			if other.To.Before(span.To) {
				span.To = other.To
			}
			if span.Duration() > 0 {
				shared = append(shared, span)
			}
		}
	}
	return normalize(shared)
}

// subtract is the time left carries that right does not.
func subtract(left, right []Span) []Span {
	left, right = normalize(left), normalize(right)
	var rest []Span
	for _, one := range left {
		from := one.From
		for _, other := range right {
			if !other.To.After(from) {
				continue
			}
			if !other.From.Before(one.To) {
				break
			}
			if other.From.After(from) {
				rest = append(rest, Span{From: from, To: other.From})
			}
			if other.To.After(from) {
				from = other.To
			}
		}
		if one.To.After(from) {
			rest = append(rest, Span{From: from, To: one.To})
		}
	}
	return normalize(rest)
}

// totalOf is how long a set of spans covers.
func totalOf(spans []Span) time.Duration {
	var total time.Duration
	for _, span := range normalize(spans) {
		total += span.Duration()
	}
	return total
}
