package workflow

import "github.com/sjawhar/legion/daemon/internal/phase"

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
	// TriggerConflict is a head GitHub will not merge into its base: the daemon's own reading of
	// its own read, raised by no agent (mergeability). GitHub computes no merge ref for such a
	// head and runs no checks on it, so nothing else would ever arrive for it, and the issue goes
	// back to implementing from whatever later phase it stands in, for the implementer to merge
	// the base forward.
	TriggerConflict TriggerKind = "conflict"
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

// Table is the default sequence's transition table (phase.DefaultSequence, TableOf): the table
// every issue walked before an issue could name another sequence, and the one every issue that
// names none and every project that sets none still walks (dispatch://INFRA-3570). It was written
// out here by hand until then; TestTheDefaultSequenceGeneratesTodaysTable holds the generated
// table to those rows. Rows, not switch-case scheduling, define every phase change, and the engine
// reads the table of each issue's own sequence (Engine.row, tableFor), of which this is one.
var Table = tables[phase.DefaultSequenceName]

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
	phase.Planning:       "in_progress",
	phase.Implementing:   "in_progress",
	phase.Testing:        "testing",
	phase.Reviewing:      "needs_review",
	phase.Accepting:      "needs_review",
	phase.Retro:          "retro",
	phase.Merging:        "retro",
	phase.Done:           "done",
	// The issue waits on an unfinished Dispatch issue and runs nothing.
	phase.Held: "",
}
