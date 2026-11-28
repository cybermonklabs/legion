package record

import "slices"

// CIState is what the one rule of a check that a CI job runs (CIVerdict) reads of its state at a
// head: what fails there and what passes there. The rule is declared here, below every reader,
// so the workflow's fix and its hold of a task's end, and the held list that names what holds
// that end (EndHoldsOf), read one rule.
type CIState int

const (
	// CIUnknownKind is a check of which the daemon holds no state at the head, so whether a CI
	// job runs it is not known yet.
	CIUnknownKind CIState = iota
	// CINotCI is a check that no CI job runs: its state carries no ci_case.
	CINotCI
	// CIRunning is a check whose CI job has not ended at the head.
	CIRunning
	// CIPassed is a check whose CI job ended at the head and that reads yes there.
	CIPassed
	// CIFailed is a check whose CI job ended at the head without a pass.
	CIFailed
)

// CINotCompleted is the case of a check CI proves whose run at the head has not concluded.
const CINotCompleted = "not_completed"

// ciPasses is the state of a check that passes at its head, as Dispatch publishes it.
const ciPasses = "yes"

// ciWaitCases are the ci_case words of a job that has not ended at the head, as Dispatch writes
// them: no run yet, a run not concluded, a run not started, no head to read at, and a read of
// GitHub that failed.
var ciWaitCases = []string{"absent", CINotCompleted, "not_started", "no_head", "github_unreadable"}

// CIVerdict is the one rule of a check that a CI job runs, on state, the state the daemon holds
// of it at a head, held false for none: what fails at a head and what passes there. No state held
// is CIUnknownKind, and a state with no ci_case is CINotCI. A case of ciWaitCases is CIRunning,
// whatever the state reads, since the job has not ended. Then yes is CIPassed, and every other
// state is CIFailed: the job ended there without a pass, under any word Dispatch writes for its
// end, a word the daemon does not know among them, so no such word waits without end.
func CIVerdict(state CheckState, held bool) CIState {
	return ciVerdictWaiting(state, held, ciWaitCases)
}

// ciVerdictWaiting is CIVerdict with waits as the cases of a job that has not ended.
func ciVerdictWaiting(state CheckState, held bool, waits []string) CIState {
	switch {
	case !held:
		return CIUnknownKind
	case state.CICase == "":
		return CINotCI
	case slices.Contains(waits, state.CICase):
		return CIRunning
	case state.State == ciPasses:
		return CIPassed
	default:
		return CIFailed
	}
}

// CIWaits is what of the CI at a head has not passed, head being what the daemon holds of the
// head: each check of the head's set that a CI job may run and that has not passed there
// (CIVerdict's CIUnknownKind, CIRunning or CIFailed), in the set's order. A check that no CI job
// runs waits for nothing, whatever its state, and a set held empty, or not held at all, names
// nothing. It is the one statement of which checks hold a task's end, which the workflow's hold
// of that end waits for and the held list names (EndHoldsOf).
func CIWaits(head HeadStates) []string {
	var waits []string
	for _, check := range head.Checks {
		state, stated := head.States[check]
		switch CIVerdict(state, stated) {
		case CIUnknownKind, CIRunning, CIFailed:
			waits = append(waits, check)
		}
	}
	return waits
}
