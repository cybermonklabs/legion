package workflow

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/phase"
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
	// TriggerChecksPassed is the judge's answer that a merged issue's close waits for nothing
	// (record.CloseWait.Done, judgeClose): the one trigger out of verifying.
	TriggerChecksPassed TriggerKind = "checks_passed"
	TriggerSignOff      TriggerKind = "sign_off"
	TriggerBackward     TriggerKind = "backward"
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
// CloseWaits is whether the close of a merged issue waits for anything (record.CloseWaitOf, read
// by closeWait at the merge): transition fills it for TriggerPullRequestMerged alone, and it picks
// which of the merge's two rows the merge takes.
type Snapshot struct {
	Phase             phase.Phase
	HasOpenOrMergedPR bool
	GateOpen          bool
	CloseWaits        bool
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

// closeWaits and closeWaitsNot are the guards of the merge's two rows: a merge whose close waits
// for anything enters verifying, and only a merge whose close waits for nothing closes the issue.
func closeWaits(s Snapshot) bool    { return s.CloseWaits }
func closeWaitsNot(s Snapshot) bool { return !s.CloseWaits }

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
// rows on its role's trigger (stepRows); the merge's two rows, out of merging into done where the
// close waits for nothing and into verifying where it waits, and the close out of verifying into
// done once the checks of the goal pass; and a backward row from each phase of the line to each
// phase before it in the line (backwardRows). A head that conflicts with its base moves no phase
// (merge_forward.go). A row into accepting starts nobody, because accepting's role is the tree's
// architect; every other row into a phase of the line starts its role. A row into verifying starts
// nobody either: no role works the wait. No row carries a status of its own: every status a row
// writes is its target phase's, which PhaseStatus holds.
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
	// GitHub's merge of the pull request as the phase's own terminating fact. The merge closes the
	// issue only where its close waits for nothing (closeWaitsNot). Else the issue waits in
	// verifying, on no slot and with no role, until the judge finds that the checks of its goal
	// passed (judgeClose), which closes it out of verifying.
	rows = append(rows,
		Row{From: phase.Merging, Trigger: TriggerPullRequestMerged, Guard: closeWaitsNot, To: phase.Done,
			Effects: []EffectKind{EffectSuspend, EffectNotice, EffectLinger}},
		Row{From: phase.Merging, Trigger: TriggerPullRequestMerged, Guard: closeWaits, To: phase.Verifying,
			Effects: []EffectKind{EffectNotice}},
		Row{From: phase.Verifying, Trigger: TriggerChecksPassed, Guard: allow, To: phase.Done,
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
// Verifying is the one wait with a status of its own, testing, since what it waits for is the
// checks of the merged work.
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
	phase.Planning:       "in_progress",
	phase.Implementing:   "in_progress",
	phase.Testing:        "testing",
	phase.Reviewing:      "needs_review",
	phase.Accepting:      "needs_review",
	phase.Retro:          "retro",
	phase.Merging:        "retro",
	// The issue's work merged and its close waits for the checks of its goal: Dispatch's own word for
	// testing on a production-like surface.
	phase.Verifying: "testing",
	phase.Done:      "done",
	// The issue waits on an unfinished Dispatch issue and runs nothing.
	phase.Held: "",
}
