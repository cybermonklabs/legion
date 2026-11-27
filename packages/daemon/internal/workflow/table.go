package workflow

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// TriggerKind names the observation that can advance a workflow phase.
type TriggerKind string

const (
	TriggerGateOpened          TriggerKind = "gate_opened"
	TriggerPlannerCompleted    TriggerKind = "planner_completed"
	TriggerImplementationReady TriggerKind = "implementation_ready"
	TriggerTesterPassed        TriggerKind = "tester_passed"
	TriggerTesterFailed        TriggerKind = "tester_failed"
	TriggerReviewApproved      TriggerKind = "review_approved"
	TriggerReviewRejected      TriggerKind = "review_rejected"
	// TriggerStepPassed and TriggerStepFailed are the report of a step that runs as step, a role
	// the deployment defines: its verdict pass moves the task on, and fail sends the work back.
	TriggerStepPassed        TriggerKind = "step_passed"
	TriggerStepFailed        TriggerKind = "step_failed"
	TriggerRetroCompleted    TriggerKind = "retro_completed"
	TriggerPullRequestMerged TriggerKind = "pull_request_merged"
	TriggerSignOff           TriggerKind = "sign_off"
	TriggerBackward          TriggerKind = "backward"
)

// EffectKind identifies the durable outbox consequences selected by a row. EffectSuspend is the
// issue's close alone (Engine.leave): a phase worker stays live until its issue closes, so no row
// that only moves the issue between phases suspends anyone. The one other suspension of a claim of
// an open issue, of a keyed agent's claim whose next step is of another role, is that step's
// start's, at the claim's turn's end, and no row's (the outbox's reroleForStep).
type EffectKind string

const (
	EffectSuspend EffectKind = "suspend"
	EffectStart   EffectKind = "start"
	EffectNotice  EffectKind = "notice"
	EffectLinger  EffectKind = "linger"
)

// Snapshot is the record state a row guard reads. Guards are pure and contain no side effects.
// HasOpenOrMergedPR is whether the issue's pull request record is open or merged. A merged record
// counts: the work landed, the merge told the architect (pr-merged), and the workflow runs on. The
// implementer's completion never moves an issue on a pull request closed without merging, nor with
// none recorded (hasOpenOrMergedPR). No other step's completion reads it.
type Snapshot struct {
	Phase             phase.Phase
	HasOpenOrMergedPR bool
	GateOpen          bool
}

// Row is one declarative workflow transition. The engine interprets its effects transactionally.
// FromPlace and ToPlace are where the task stands before and after the move, by the positions of
// the line the row was built from (TableOf): a step at its place from 1, the retro and the merge at
// place 0, and admitted and done at no place. A stored walk takes the row whose FromPlace is where
// its task stands (Walk.row) and moves the task to its ToPlace, so two steps that run as one phase
// are two rows; a walk with no plan rows reads neither.
type Row struct {
	From      phase.Phase
	Trigger   TriggerKind
	Guard     func(Snapshot) bool
	To        phase.Phase
	FromPlace record.TaskPlace
	ToPlace   record.TaskPlace
	Effects   []EffectKind
}

func allow(Snapshot) bool { return true }

// hasOpenOrMergedPR is the guard of the implementer's move on: a completion moves an issue only on
// a pull request that is open or merged (Snapshot).
func hasOpenOrMergedPR(s Snapshot) bool { return s.HasOpenOrMergedPR }

// Table is the table of today's line, phase.Order: every row an issue with no plan rows walks
// (Walk), built once. TableOf(phase.Order) is the table the workflow had before a plan picked an
// issue's line, row for row (TestAWalkWithNoPlanRowsMovesByTodaysTable).
var Table = TableOf(phase.Order)

// stepRows are the rows each phase of a line carries on its own role's trigger, in the table's
// order: the step's move on to the next phase of the line (to ""), whichever phase the line puts
// there, and the three returns, the tester's fail, a review's request for changes and the fail of
// a step that runs as step, to implementing. A phase the line does not hold carries none of them,
// and a phase the line holds at two places carries them at each. Retro's move on is to merging,
// the phase after it in every line.
var stepRows = []struct {
	from    phase.Phase
	trigger TriggerKind
	guard   func(Snapshot) bool
	to      phase.Phase
}{
	{phase.Planning, TriggerPlannerCompleted, allow, ""},
	{phase.Implementing, TriggerImplementationReady, hasOpenOrMergedPR, ""},
	{phase.Testing, TriggerTesterPassed, allow, ""},
	{phase.Testing, TriggerTesterFailed, allow, phase.Implementing},
	{phase.Reviewing, TriggerReviewApproved, allow, ""},
	{phase.Accepting, TriggerSignOff, allow, ""},
	{phase.Reviewing, TriggerReviewRejected, allow, phase.Implementing},
	{phase.Step, TriggerStepPassed, allow, ""},
	{phase.Step, TriggerStepFailed, allow, phase.Implementing},
	{phase.Retro, TriggerRetroCompleted, allow, ""},
}

// TableOf is the complete transition table of an issue whose task walks line: the phases of its
// steps in the task's order, which holds implementing, then retro and merging (plan.Line of a plan
// plan.Validate took). Rows, not switch-case scheduling, define every phase change. Its rows, in
// this order: the gate's opening, out of admitted into the first phase of the line; each phase's
// rows on its role's trigger (stepRows), at each position of the line that phase stands at; the
// merge, out of merging into done; and a backward row from each position of the line to each
// position before it (backwardRows). A head that conflicts with its base moves no phase
// (merge_forward.go). A failed step's return is one of those backward rows: to the first step of
// the type implementation before it (Walk.returnTarget), or to the earlier step its completion
// names, and a failed step with neither stops its task. Each row carries the places of
// its two positions (placeAt), so a line that holds a phase at two places, two steps of roles the
// deployment defines or two steps of one role, has a row for each. A row into accepting starts
// nobody, because accepting's role is the tree's architect; every other row into a phase of the
// line starts its role. No row carries a status of its own: every status a row writes is its
// target phase's, which PhaseStatus holds.
func TableOf(line []phase.Phase) []Row {
	rows := []Row{{From: phase.Admitted, Trigger: TriggerGateOpened, Guard: allow, To: line[0], ToPlace: placeAt(line, 0),
		Effects: effectsInto(line[0])}}
	for _, step := range stepRows {
		for at, from := range line {
			if from != step.from {
				continue
			}
			next := at + 1
			if step.to != "" {
				if next = slices.Index(line[:at], step.to); next < 0 {
					continue
				}
			}
			to := line[next]
			rows = append(rows, Row{From: from, Trigger: step.trigger, Guard: step.guard, To: to, FromPlace: placeAt(line, at),
				ToPlace: placeAt(line, next), Effects: effectsInto(to)})
		}
	}
	// The merger merges, and the merge is what ends merging: the daemon submits none and reads
	// GitHub's merge of the pull request as the phase's own terminating fact. The task stays at its
	// merge, done.
	merge := record.TaskPlace{Kind: record.PlaceMerge}
	rows = append(rows, Row{From: phase.Merging, Trigger: TriggerPullRequestMerged, Guard: allow, To: phase.Done,
		FromPlace: merge, ToPlace: merge, Effects: []EffectKind{EffectSuspend, EffectNotice, EffectLinger}})
	return append(rows, backwardRows(line)...)
}

// StoredTableOf is the table of a stored walk's line (Walk.table): TableOf(line), every row with
// its places, then, for each step of the line whose phase has a return (stepRows: the tester's
// fail, a review's request for changes, the fail of a step that runs as step), one row of that
// return to each step before it that TableOf's return to implementing does not reach, in the
// order of the line. So the work of a failed step goes back to whichever earlier step its report
// or its plan names (returnTarget, returnStep), the step before an implementer included.
func StoredTableOf(line []phase.Phase) []Row {
	table := TableOf(line)
	type move struct {
		from, to record.TaskPlace
		trigger  TriggerKind
	}
	held := make(map[move]bool, len(table))
	for _, row := range table {
		held[move{row.FromPlace, row.ToPlace, row.Trigger}] = true
	}
	rows := table
	for at, from := range line {
		fromPlace := placeAt(line, at)
		if fromPlace.Kind != record.PlaceStep {
			continue
		}
		for _, step := range stepRows {
			if step.from != from || step.to == "" {
				continue
			}
			for back := range at {
				to := placeAt(line, back)
				if held[move{fromPlace, to, step.trigger}] {
					continue
				}
				rows = append(rows, Row{From: from, Trigger: step.trigger, Guard: step.guard, To: line[back], FromPlace: fromPlace,
					ToPlace: to, Effects: effectsInto(line[back])})
			}
		}
	}
	return rows
}

// placeAt is where a task stands at position at of line: the retro or the merge at place 0, and
// any other phase at the step of place at+1.
func placeAt(line []phase.Phase, at int) record.TaskPlace {
	switch line[at] {
	case phase.Retro:
		return record.TaskPlace{Kind: record.PlaceRetro}
	case phase.Merging:
		return record.TaskPlace{Kind: record.PlaceMerge}
	}
	return record.TaskPlace{Kind: record.PlaceStep, Position: at + 1}
}

// effectsInto is what a row into to does beyond the move: it starts to's role and tells the
// architect, except into accepting, whose role is the architect, which no row starts.
func effectsInto(to phase.Phase) []EffectKind {
	if to == phase.Accepting {
		return []EffectKind{EffectNotice}
	}
	return []EffectKind{EffectStart, EffectNotice}
}

// backwardRows is the edge from every position of line back to each position of line before it,
// by the line's own order: each phase of the line paired with every phase before it in the line,
// in that order, each row with the places of its two positions (placeAt). A line in today's order
// (a subsequence of phase.Order) so gets the pairs of phase.BackwardPairs whose two phases it
// holds, in that list's order, and TableOf(phase.Order) stays Table row for row; a line in another
// order goes back by its own order, so the line `implementing reviewing testing` moves back from
// testing to reviewing, and not from reviewing to testing, which is its move on
// (TestBackwardRowsOfEachLineAreItsOwnPairs), and the line of two steps that run as step moves
// back from the second to the first, step-step. Like every other row, each writes the status of
// the phase it enters, so a move back into planning writes in_progress, the status an issue shows
// while Legion works it, rather than leaving Dispatch showing the phase the issue left. No phase
// of a line waits for a merge somebody else makes, so no pair is carried by a row above instead
// (dispatch://INFRA-714).
func backwardRows(line []phase.Phase) []Row {
	rows := make([]Row, 0, len(line)*(len(line)-1)/2)
	for at, from := range line {
		for back, to := range line[:at] {
			rows = append(rows, Row{From: from, Trigger: TriggerBackward, Guard: allow, To: to, FromPlace: placeAt(line, at),
				ToPlace: placeAt(line, back), Effects: []EffectKind{EffectStart, EffectNotice}})
		}
	}
	return rows
}

// PhaseStatus is the Dispatch status an issue standing in to shows: what a row that moves an issue
// into to writes, and what the status repair writes for an issue whose status is not its phase's.
// An issue in planning, implementing or step is in_progress, which is where a child first shows
// that Legion works it, and merging keeps the retro that the phase before it wrote. The phases
// where an issue waits rather than works have no status of their own: the issue keeps the one it
// already shows, which is what the empty status does, since no write is enqueued for one
// (Engine.status).
func PhaseStatus(to phase.Phase) string {
	return phaseStatus[to]
}

// phaseStatus holds an entry for every phase of phase.All, which TestEveryPhaseHasAStatusEntry
// holds it to. A phase missing from it would write no status at all, which is a wrong status
// shown for as long as the issue stands there, not a failure anything would report.
var phaseStatus = map[phase.Phase]string{
	// The issue waits before its first phase, and a status written here would take a root waiting
	// in the admission line out of that line.
	phase.Admitted: "",
	// The root keeps the status of the phase it waits in.
	phase.AwaitingPerson: "",
	// A parent waiting on its open children shows in_progress, as a child first shows that Legion
	// works it: it holds no slot, but its work is under way, not stopped (dispatch://INFRA-3320 R11).
	phase.AwaitingChildren: "in_progress",
	phase.Planning:         "in_progress",
	phase.Implementing:     "in_progress",
	phase.Testing:          "testing",
	phase.Reviewing:        "needs_review",
	phase.Step:             "in_progress",
	phase.Accepting:        "needs_review",
	phase.Retro:            "retro",
	phase.Merging:          "retro",
	phase.Done:             "done",
	// The issue waits on an unfinished Dispatch issue and runs nothing.
	phase.Held: "",
}
