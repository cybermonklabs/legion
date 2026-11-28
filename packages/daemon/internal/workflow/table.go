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
	TriggerRetroCompleted      TriggerKind = "retro_completed"
	TriggerPullRequestMerged   TriggerKind = "pull_request_merged"
	TriggerSignOff             TriggerKind = "sign_off"
	TriggerBackward            TriggerKind = "backward"
	// TriggerConflict is the move back to implementing of a head that conflicts with its base,
	// under a slot rule whose conflicting head moves back (record.ConflictMovesBack, TableFor).
	// The daemon's own rule keeps the phase and merges the base forward, so its table carries no
	// row of it.
	TriggerConflict TriggerKind = "conflict"
	// TriggerChecksRed is the move back to implementing by red checks, under a slot rule whose
	// conflicting head moves back that way (record.ConflictChecksRed, TableFor). The daemon's own
	// rule carries no row of it.
	TriggerChecksRed TriggerKind = "checks_red"
)

// EffectKind identifies the durable outbox consequences selected by a row. EffectSuspend is the
// issue's close alone (Engine.leave): a phase worker stays live from its role's first assignment
// until its issue closes, so no row that only moves the issue between phases suspends anyone.
type EffectKind string

const (
	EffectSuspend EffectKind = "suspend"
	EffectStart   EffectKind = "start"
	EffectNotice  EffectKind = "notice"
	EffectLinger  EffectKind = "linger"
)

// Snapshot is the record state a row guard reads. Guards are pure and contain no side effects.
// HasOpenOrMergedPR is whether the issue's pull request record is open or merged. A merged record
// counts: the work landed, the merge told the architect (pr-merged), and the workflow runs on. A
// completion never moves an issue on a pull request closed without merging, nor with none recorded.
type Snapshot struct {
	Phase             phase.Phase
	HasOpenOrMergedPR bool
	GateOpen          bool
}

// Row is one declarative workflow transition. The engine interprets its effects transactionally.
type Row struct {
	From    phase.Phase
	Trigger TriggerKind
	Guard   func(Snapshot) bool
	To      phase.Phase
	Effects []EffectKind
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
// there, and the two returns, the tester's fail and a review's request for changes, to
// implementing. A phase the line does not hold carries none of them. Retro's move on is to
// merging, the phase after it in every line.
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
	{phase.Retro, TriggerRetroCompleted, allow, ""},
}

// TableOf is the complete transition table of an issue whose task walks line: the phases of its
// steps in the task's order, which holds implementing, then retro and merging (plan.Line of a plan
// plan.Validate took). Rows, not switch-case scheduling, define every phase change. Its rows, in
// this order: the gate's opening, out of admitted into the first phase of the line; each phase's
// rows on its role's trigger (stepRows); the merge, out of merging into done; and a backward row
// from each phase of the line to each phase before it in the line (backwardRows). A head that
// conflicts with its base moves no phase (merge_forward.go). A row into accepting starts nobody,
// because accepting's role is the tree's architect; every other row into a phase of the line
// starts its role. No row carries a status of its own: every status a row writes is its target
// phase's, which PhaseStatus holds.
func TableOf(line []phase.Phase) []Row {
	rows := []Row{{From: phase.Admitted, Trigger: TriggerGateOpened, Guard: allow, To: line[0], Effects: effectsInto(line[0])}}
	for _, step := range stepRows {
		at := slices.Index(line, step.from)
		if at < 0 {
			continue
		}
		to := step.to
		if to == "" {
			to = line[at+1]
		}
		rows = append(rows, Row{From: step.from, Trigger: step.trigger, Guard: step.guard, To: to, Effects: effectsInto(to)})
	}
	// The merger merges, and the merge is what ends merging: the daemon submits none and reads
	// GitHub's merge of the pull request as the phase's own terminating fact.
	rows = append(rows, Row{From: phase.Merging, Trigger: TriggerPullRequestMerged, Guard: allow, To: phase.Done,
		Effects: []EffectKind{EffectSuspend, EffectNotice, EffectLinger}})
	return append(rows, backwardRows(line)...)
}

// effectsInto is what a row into to does beyond the move: it starts to's role and tells the
// architect, except into accepting, whose role is the architect, which no row starts.
func effectsInto(to phase.Phase) []EffectKind {
	if to == phase.Accepting {
		return []EffectKind{EffectNotice}
	}
	return []EffectKind{EffectStart, EffectNotice}
}

// TableFor is the transition table of a task that walks line under rule: TableOf(line), and for a
// rule whose conflicting head moves back (record.SlotRule.Conflict) the rows of that move on that
// line beside them (movesBackRows). Under the daemon's own rule, whose conflicting head keeps its
// phase while the base is merged forward, it is TableOf(line) row for row, so an engine running
// the daemon's rule moves by the table of each issue's line alone and an issue with no plan rows
// moves by Table (TestAWalkWithNoPlanRowsMovesByTodaysTable). A replay of a recorded window reads
// the table of the rule it replays through here, so the rows a past release ran are the rule's and
// never a second table beside the daemon's.
func TableFor(rule record.SlotRule, line []phase.Phase) []Row {
	trigger, _ := conflictTrigger(rule)
	return tableUnder(trigger, line)
}

// tableUnder is the table of line under the trigger a slot rule's conflicting head moves back by,
// "" under the daemon's own (conflictTrigger): TableOf(line), with that move's rows where the
// trigger names one. Each walk reads its own table through it, under the trigger its engine runs
// (Walk.table, Engine.conflict), so a rule's conflict rows go on the table of a stored walk's line
// as well as on today's.
func tableUnder(trigger TriggerKind, line []phase.Phase) []Row {
	rows := TableOf(line)
	if trigger == "" {
		return rows
	}
	return append(rows, movesBackRows(trigger, line)...)
}

// conflictTrigger is the trigger a head that conflicts with its base moves its issue back to
// implementing by under rule (record.SlotRule.Conflict): TriggerConflict under
// record.ConflictMovesBack, TriggerChecksRed under record.ConflictChecksRed, and false under the
// daemon's own, whose conflicting head keeps its phase while the implementer merges the base
// forward (record.ConflictMergeForward). The engine's mergeability moves the issue by it
// (conflictMovesBack), and the table of the issue's line carries a row of it from each phase that
// can stand on a conflicting head (tableUnder).
func conflictTrigger(rule record.SlotRule) (TriggerKind, bool) {
	switch rule.OrMain().Conflict {
	case record.ConflictMovesBack:
		return TriggerConflict, true
	case record.ConflictChecksRed:
		return TriggerChecksRed, true
	}
	return "", false
}

// conflictTable and checksRedTable are Table with the moves back of a conflicting head added
// (movesBackRows), by TriggerConflict and by TriggerChecksRed: the tables of today's line under
// the two rules whose conflicting head moves back, which every run with no plan rows walks
// (Walk.table). Table is clipped first, so the rows are added to a copy and Table itself stays the
// daemon's own.
var (
	conflictTable  = append(slices.Clip(Table), movesBackRows(TriggerConflict, phase.Order)...)
	checksRedTable = append(slices.Clip(Table), movesBackRows(TriggerChecksRed, phase.Order)...)
)

// movesBackRows is the move back to implementing by trigger from each phase of line after
// implementing, the phases of the line that stand on a head of a pull request — on today's line
// testing, reviewing, accepting, retro and merging — as a release whose conflicting head moved
// back ran it. Each starts the implementer and tells the architect, as every move back does
// (backwardRows). A line with no implementing step has no phase to move back to and carries none
// of them.
func movesBackRows(trigger TriggerKind, line []phase.Phase) []Row {
	at := slices.Index(line, phase.Implementing)
	if at < 0 {
		return nil
	}
	froms := line[at+1:]
	rows := make([]Row, 0, len(froms))
	for _, from := range froms {
		rows = append(rows, Row{From: from, Trigger: trigger, Guard: allow, To: phase.Implementing,
			Effects: []EffectKind{EffectStart, EffectNotice}})
	}
	return rows
}

// backwardRows is the edge from every phase of line back to each phase of line before it, by the
// line's own order: each phase of the line paired with every phase before it in the line, in that
// order. A line in today's order (a subsequence of phase.Order) so gets the pairs of
// phase.BackwardPairs whose two phases it holds, in that list's order, and TableOf(phase.Order)
// stays Table row for row; a line in another order goes back by its own order, so the line
// `implementing reviewing testing` moves back from testing to reviewing, and not from reviewing to
// testing, which is its move on (TestBackwardRowsOfEachLineAreItsOwnPairs). Like every other row,
// each writes the status of the phase it enters, so a move back into planning writes in_progress,
// the status an issue shows while Legion works it, rather than leaving Dispatch showing the phase
// the issue left. No phase of a line waits for a merge somebody else makes, so no pair is carried
// by a row above instead (dispatch://INFRA-714).
func backwardRows(line []phase.Phase) []Row {
	rows := make([]Row, 0, len(line)*(len(line)-1)/2)
	for at, from := range line {
		for _, to := range line[:at] {
			rows = append(rows, Row{From: from, Trigger: TriggerBackward, Guard: allow, To: to,
				Effects: []EffectKind{EffectStart, EffectNotice}})
		}
	}
	return rows
}

// PhaseStatus is the Dispatch status an issue standing in to shows: what a row that moves an issue
// into to writes, and what the status repair writes for an issue whose status is not its phase's.
// An issue in planning or implementing is in_progress, which is where a child first shows that
// Legion works it, and merging keeps the retro that the phase before it wrote. The phases where an
// issue waits rather than works have no status of their own: the issue keeps the one it already
// shows, which is what the empty status does, since no write is enqueued for one (Engine.status).
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
	phase.Accepting:        "needs_review",
	phase.Retro:            "retro",
	phase.Merging:          "retro",
	phase.Done:             "done",
	// The issue waits on an unfinished Dispatch issue and runs nothing.
	phase.Held: "",
}
