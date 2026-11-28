package workflow

import (
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// TableOf is the complete transition table of one sequence (phase.Sequence), generated from its
// phases rather than written out, so the library's every line has a table and no two tables can
// drift apart (dispatch://INFRA-3570). It is pure: the same sequence gives the same rows. It
// assumes a sequence of the library, which keeps phase.Sequence's two rules — a sub-list of
// phase.Order in its order, holding implementing, reviewing and merging — so merging is always
// the sequence's last phase.
//
// The rows are these, and nothing else:
//   - the gate's opening moves an admitted issue into the sequence's first phase;
//   - each phase's own forward trigger moves it into the next phase of the sequence, and the merge
//     ends merging, the last, for done;
//   - the tester's failure and the reviewer's request for changes send it back to implementing;
//   - a head that conflicts with its base sends it back to implementing from every phase of the
//     sequence after implementing;
//   - a backward move takes it from each phase of the sequence to any phase of the sequence
//     before it.
//
// A trigger of a phase the sequence does not walk has no row, so the engine moves nothing on it
// (Engine.row answers false, and transition does nothing). No row carries a status of its own:
// every status a row writes is its target phase's, which PhaseStatus holds.
func TableOf(sequence phase.Sequence) []Row {
	first := sequence.Phases[0]
	rows := []Row{{From: phase.Admitted, Trigger: TriggerGateOpened, Guard: allow, To: first, Effects: forwardEffects(first)}}
	for index, from := range sequence.Phases {
		to := phase.Done
		if index+1 < len(sequence.Phases) {
			to = sequence.Phases[index+1]
		}
		trigger, guard := forwardTrigger(from)
		rows = append(rows, Row{From: from, Trigger: trigger, Guard: guard, To: to, Effects: forwardEffects(to)})
		switch from {
		case phase.Testing:
			rows = append(rows, Row{From: from, Trigger: TriggerTesterFailed, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}})
		case phase.Reviewing:
			rows = append(rows, Row{From: from, Trigger: TriggerReviewRejected, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}})
		}
		// A head that conflicts with its base goes back to implementing from every phase after the
		// implementer's own (transition). It is one trigger for all of them so that the role's
		// phase-ended message (phaseEnded) and the architect's notice (noticeFor) have a single
		// name to key on, and so that the architect's own backward move stays the separate thing
		// it is.
		if phase.MovesBack(from, phase.Implementing) {
			rows = append(rows, Row{From: from, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}})
		}
	}
	return append(rows, backwardRows(sequence)...)
}

// forwardTrigger is the trigger that ends from and moves the issue on, with the guard that row
// reads. The implementer's completion moves the issue only on a pull request recorded open or
// merged (Snapshot.HasOpenOrMergedPR), whichever phase follows implementing. The merger merges,
// and the merge is what ends merging: the daemon submits none and reads GitHub's merge of the pull
// request as the phase's own terminating fact. Accepting ends with its architect's sign-off.
func forwardTrigger(from phase.Phase) (TriggerKind, func(Snapshot) bool) {
	switch from {
	case phase.Planning:
		return TriggerPlannerCompleted, allow
	case phase.Implementing:
		return TriggerImplementationReady, func(s Snapshot) bool { return s.HasOpenOrMergedPR }
	case phase.Testing:
		return TriggerTesterPassed, allow
	case phase.Reviewing:
		return TriggerReviewApproved, allow
	case phase.Accepting:
		return TriggerSignOff, allow
	case phase.Retro:
		return TriggerRetroCompleted, allow
	default:
		return TriggerPullRequestMerged, allow
	}
}

// forwardEffects is what a forward row into to selects: the issue's close for done, a notice
// alone for accepting, the tree architect's phase, which no row starts since that architect is
// already live, and the start of to's worker with a notice for every other phase.
func forwardEffects(to phase.Phase) []EffectKind {
	switch to {
	case phase.Done:
		return []EffectKind{EffectSuspend, EffectNotice, EffectLinger}
	case phase.Accepting:
		return []EffectKind{EffectNotice}
	default:
		return []EffectKind{EffectStart, EffectNotice}
	}
}

// backwardRows is the edge from every phase of the sequence back to each phase of it before. Like
// every other row, each writes the status of the phase it enters, so a move back into planning
// writes in_progress, the status an issue shows while Legion works it, rather than leaving
// Dispatch showing the phase the issue left. The pairs come from phase.BackwardPairs, the one
// list the config loader reads as well, kept to the pairs whose two phases the sequence walks, so
// TestBackwardRowsMatchPhaseBackwardPairs holds every sequence's moves back to that list and the
// default's equal to it. No phase of any sequence waits for a merge somebody else makes, so no
// pair is a row of its own above (dispatch://INFRA-714).
func backwardRows(sequence phase.Sequence) []Row {
	var rows []Row
	for _, text := range phase.BackwardPairs() {
		from, to, _ := phase.ParsePair(text)
		if !sequence.Holds(from) || !sequence.Holds(to) {
			continue
		}
		rows = append(rows, Row{From: from, Trigger: TriggerBackward, Guard: allow, To: to,
			Effects: []EffectKind{EffectStart, EffectNotice}})
	}
	return rows
}

// tables is each library sequence's table, generated once, by name: the engine reads an issue's
// table on every fact, and the library is fixed for the life of the process.
var tables = buildTables()

func buildTables() map[string][]Row {
	built := map[string][]Row{}
	for _, sequence := range phase.Sequences() {
		built[sequence.Name] = TableOf(sequence)
	}
	return built
}

// tableFor is sequence's table: the one generated for it at start where it is the library's, which
// every sequence the engine reads is (record.Issue.Sequence answers only the library's), and
// generated now for any other.
func tableFor(sequence phase.Sequence) []Row {
	if rows, ok := tables[sequence.Name]; ok {
		return rows
	}
	return TableOf(sequence)
}
