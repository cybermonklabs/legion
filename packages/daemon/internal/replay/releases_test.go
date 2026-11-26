package replay

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The operator's retry of a phase's worker writes `workflow: a held phase is released` whether or
// not the issue stands held (workflow's restartPhase and retryPhase). Three implementers run from
// 09:00. ACME-1 stands in no hold: its turn ends at 09:20, and at 09:30 the operator retries its
// worker, which is launched again and starts a turn — that retry releases nothing, and the trace
// holds no fact it could not place. ACME-2's claim fails at 09:10, which holds it, and the
// operator's retry at 09:30 releases that hold. ACME-3 stands in no hold either, and a release of
// another reason at 09:30 is one of a hold the trace does not hold, a fact of a hold it cannot
// place, as before.
func TestAnOperatorsRetryOfAWorkerInNoHoldReleasesNothing(t *testing.T) {
	t.Parallel()
	b := newWindowBuilder(t, clock(8, 0), clock(12, 0))
	b.boot(clock(8, 0))
	implementing := builtMove{clock(9, 0), phase.Implementing, workflow.TriggerPlannerCompleted}
	rootThrough(b, "ACME-1", "a1", clock(8, 5), clock(8, 20), implementing)
	rootThrough(b, "ACME-2", "a2", clock(8, 10), clock(8, 25), implementing)
	rootThrough(b, "ACME-3", "a3", clock(8, 15), clock(8, 30), implementing)
	released := func(key, reason string) {
		b.info(clock(9, 30), "workflow: a held phase is released", "issue", key, "phase", string(phase.Implementing), "reason", reason)
	}
	b.turnEnds(clock(9, 20), "ACME-1", claim.RoleImplementer)
	released("ACME-1", operatorRetryReason)
	b.launched(clock(9, 30), "ACME-1", claim.RoleImplementer, true)
	b.turnStarts(clock(9, 31), "ACME-1", claim.RoleImplementer)
	b.claimFailed(clock(9, 10), "ACME-2", claim.RoleImplementer)
	released("ACME-2", operatorRetryReason)
	b.launched(clock(9, 30), "ACME-2", claim.RoleImplementer, true)
	b.turnStarts(clock(9, 31), "ACME-2", claim.RoleImplementer)
	released("ACME-3", "retry held phase")

	trace, err := BuildTrace(b.build())
	if err != nil {
		t.Fatalf("build the trace: %v", err)
	}
	if len(trace.Unplaced) != 1 || trace.Unplaced[0].Issue != "ACME-3" || trace.Unplaced[0].Kind != UnplacedHold ||
		!trace.Unplaced[0].At.Equal(clock(9, 30)) {
		t.Errorf("the trace could not place %+v; want ACME-3's release at 09:30 alone, a fact of a hold", trace.Unplaced)
	}
	isHold := func(w Wait) bool { return w.Kind == WaitHeld }
	if slices.ContainsFunc(trace.Issues["ACME-1"].Waits, isHold) {
		t.Errorf("ACME-1's waits are %+v; want no hold, since the operator's retry released none", trace.Issues["ACME-1"].Waits)
	}
	waits := trace.Issues["ACME-2"].Waits
	if i := slices.IndexFunc(waits, isHold); i < 0 || !waits[i].Start.Equal(clock(9, 10)) || !waits[i].End.Equal(clock(9, 30)) ||
		waits[i].EndedBy != WaitReleased {
		t.Errorf("ACME-2's waits are %+v; want its hold from 09:10 released at 09:30", waits)
	}
	if visits := trace.Issues["ACME-1"].Visits; len(visits) == 0 || visits[len(visits)-1].Phase != phase.Implementing ||
		!visits[len(visits)-1].Exit.IsZero() {
		t.Errorf("ACME-1's visits are %s; want it still in implementing at the window's end", describeReading(visits))
	}
}
