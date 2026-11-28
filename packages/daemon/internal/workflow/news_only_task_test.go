package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A task of news of the approved design alone that a claim holds (supervise.Delivery.NewsOnly)
// names its member's phase and run, so that it is dropped once the issue has left that phase, but
// it is no task of the phase (supervise.Delivery.StillWorked, supervise.HoldsWork). So each rule
// that asks whether a claim still holds its phase's work reads a claim holding only such a task as
// one holding none, and a claim holding the phase's own task as one holding that work:
//
//   - the silence repair sees a worker up and between turns holding only the news task as silent
//     (SilentPhaseWorker), so it sends the phase's task;
//   - a claim whose launch is not yet known holding only the news task is one a start must bring
//     back (StartFor);
//   - an architect whose untaken task is news alone owes no work it was given (owed).
func TestAPendingTaskOfNewsAloneIsNoTaskOfItsPhase(t *testing.T) {
	t.Parallel()
	silent := aSilentImplementer()
	phaseTask := supervise.Delivery{ID: "outbox:3", Task: "implement the plan", Generation: silent.issue.Generation,
		Phase: silent.issue.Phase, Entry: 2, QueuedAt: silentNow.Add(-3 * time.Hour)}
	newsTask := supervise.Delivery{ID: "outbox:4", Task: "The approved design changed.", Generation: silent.issue.Generation,
		Phase: silent.issue.Phase, QueuedAt: silentNow.Add(-3 * time.Hour), News: []byte(`{"task":""}`), NewsOnly: true}
	for _, tc := range []struct {
		name    string
		pending supervise.Delivery
		// works is whether the pending task is the work of the phase.
		works bool
	}{
		{name: "a task of news alone", pending: newsTask},
		{name: "the phase's own task", pending: phaseTask, works: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := aSilentImplementer()
			worker.run.Claim.Pending = &tc.pending
			if got := worker.answer(); got == tc.works {
				t.Errorf("SilentPhaseWorker = %t holding %s, want %t: a worker holding no task of its phase is silent", got, tc.name, !tc.works)
			}

			uncertain := record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateLaunchUncertain, Tree: silent.root.Key, Pending: &tc.pending}}
			if got := StartFor(uncertain, silent.root, silent.issue.Generation, silent.issue.Phase); got == tc.works {
				t.Errorf("StartFor of a claim whose launch is not known = %t holding %s, want %t: only its phase's task is work its launch carries", got, tc.name, !tc.works)
			}

			architect := record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateIdle, Tree: silent.root.Key, Pending: &tc.pending}}
			if why, got := owed(architect); got != tc.works {
				t.Errorf("owed = %t (%q) holding %s untaken, want %t", got, why, tc.name, tc.works)
			}
		})
	}
}
