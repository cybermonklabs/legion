package daemon

// GPT-6.1 Sol's point 4 on #5285 at fa9deb04 (the Legion reviewer's point B), which the spec's
// architect ruled a fix inside R7: a record names what its act takes and the uid of its target, and
// the architect is told that a disk is gone only once the deletion of that target is confirmed. A
// launch's replacement of the issue's Sandbox and the kept volume it owns, for the volume's reset,
// records that act before it deletes the Sandbox. The Sandbox stands Suspended, as the round's
// replacement left it: a sweep tells nobody and forgets nothing, since the volume and the work on it
// are still there. Once the Sandbox of that uid is gone, the next sweep tells the architect once,
// and the launch's own word after its delete, coming late, tells nobody again: the launch writes no
// record of its own a second time (tellLaunchTook).

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// keptSandboxUID is the uid of the issue's Sandbox that owns the kept volume a launch deletes.
const keptSandboxUID = "uid-sandbox-kept"

// volumeResetTaken is the implementer's launch's replacement of the issue's Sandbox of uid
// keptSandboxUID and the kept volume it owns, for the volume's reset, under operation: the Sandbox
// runs no live pod, so no read could be made. pod is the last pod known to have used the volume,
// "" where none is.
func volumeResetTaken(r *podUpdateRig, operation, pod string) runtime.TakenPod {
	return runtime.TakenPod{Operation: operation, Claim: r.tokens[claim.RoleImplementer], Issue: podUpdateIssue, PodUID: pod,
		Why: "the launch replaces the issue's Sandbox and the volume it keeps (the volume carries the reset mark)",
		Act: runtime.VolumeAct(keptSandboxUID),
		Err: errors.New("the issue's Sandbox runs no live pod, so no process can read the workspace on its volume")}
}

func TestALaunchsVolumeRecordIsToldOnlyOnceItsSandboxIsGone(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.rt.ScriptSuspendedOver(podUpdatePod)
	taken := volumeResetTaken(rig, "launch-reset-1", "")
	if err := rig.s.recordLaunchTakes(rig.ctx, taken); err != nil {
		t.Fatalf("record the launch's replacement of the Sandbox and its volume: %v", err)
	}

	for range roundSweeps {
		rig.tick()
	}
	if told := rig.architectNotices(record.WorkspaceTakenNotice); len(told) != 0 {
		t.Fatalf("the architect was told %+v while the Sandbox and its kept volume stand, want nothing yet", told)
	}
	if records := rig.replacementRecords(); records != 1 {
		t.Fatalf("the store holds %d records while the Sandbox and its kept volume stand, want the launch's record standing", records)
	}

	rig.rt.ScriptSandboxGone(keptSandboxUID)
	for range roundSweeps {
		rig.tick()
	}
	told := rig.architectNotices(record.WorkspaceTakenNotice)
	if len(told) != 1 || !strings.Contains(told[0].Reason, "No read of the workspace could be made") || !strings.Contains(told[0].Reason, keptSandboxUID) {
		t.Fatalf("the architect was told %+v once the Sandbox is gone, want one workspace-taken notice naming the Sandbox %s and that no read could be made", told, keptSandboxUID)
	}
	rig.s.tellLaunchTook(rig.ctx, taken)
	if again := rig.architectNotices(record.WorkspaceTakenNotice); len(again) != 1 {
		t.Fatalf("the architect was told %d times once the launch's late word came, want once", len(again))
	}
	if records := rig.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d records once the notice is queued, want none", records)
	}
}

// The Legion reviewer's Minor on #5285 at 7da847c0 (fake.go:926-930): a launch's record of its
// delete of the issue's Sandbox and the kept volume it owns is not made while that Sandbox runs a
// live pod of its own: the pod uses the volume, as the Sandbox runtime reads it (actState). The
// sweep forgets the record and tells nobody.
func TestALaunchsVolumeRecordWhoseSandboxRunsALivePodIsForgotten(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	if err := rig.s.recordLaunchTakes(rig.ctx, volumeResetTaken(rig, "launch-reset-1", podUpdatePod)); err != nil {
		t.Fatalf("record the launch's delete of the Sandbox and its volume: %v", err)
	}
	for range roundSweeps {
		rig.tick()
	}
	if records := rig.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d records while the Sandbox runs its live pod on the volume, want the launch's forgotten", records)
	}
	if told := rig.architectNotices(record.WorkspaceTakenNotice); len(told) != 0 {
		t.Fatalf("the architect was told %+v of a delete that was not made, want nothing", told)
	}
}
