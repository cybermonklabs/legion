package daemon

// GPT-6.1 Sol's point 2 on #5285 at fa9deb04 (the Legion reviewer's minor point 1), which
// the spec's architect ruled a fix inside R7: a launch that makes a round's own replacement
// (roundPodEndsAtLaunch) commits the replacement's record, and only then keeps in memory that it
// replaced the pod (replacedAtLaunch), all under the pod's launch turn, and suspends the pod after.
// A sweep that reads the record in that gap sees no launch in memory and the pod still running. It
// must not forget the record: the sweep reads a recorded replacement only under the pod's launch
// turn (runtime.ActReader), and a turn the launch holds leaves the record standing. Once the launch
// has replaced the pod, a later sweep finishes the replacement from its record, and the architect is
// told what the launcher's read before it found.

import (
	"context"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func TestASweepInsideALaunchsReplacementLeavesItsRecordStanding(t *testing.T) {
	t.Parallel()
	rig := podlessBreakingRound(t)

	// The launch's hook, as the boot binds it, with a sweep's finish of the standing records in the
	// gap between the record's commit and the launch's in-memory record of its replacement: the
	// hook has done both by the time it answers, so the in-memory record is taken away first, as it
	// stands in that gap. The sweep runs on this launch's own goroutine, under the pod's launch turn
	// the launch holds.
	var (
		mu        sync.Mutex
		midLaunch = -1
	)
	hook := rig.s.roundPodEndsAtLaunch
	rig.rt.RoundPodEnds = func(ctx context.Context, token claim.Token, pod string) (bool, error) {
		ends, err := hook(ctx, token, pod)
		if !ends || err != nil {
			return ends, err
		}
		rig.s.forgetLaunchRound(podUpdateIssue, podUpdateRound)
		if claims, err := rig.sup.Claims(rig.ctx); err == nil {
			if _, err := rig.s.finishReplacements(rig.ctx, claims); err == nil {
				var n int
				if err := rig.pool.QueryRow(rig.ctx, `select count(*) from pod_replacements`).Scan(&n); err == nil {
					mu.Lock()
					midLaunch = n
					mu.Unlock()
				}
			}
		}
		return true, nil
	}

	token := rig.tokens[claim.RoleImplementer]
	rig.intoRoundPod(claim.RoleImplementer)
	rig.handle(claim.RoleImplementer, supervise.RequestResume{Claim: token, By: supervise.SuspendedByOperator})
	mu.Lock()
	standing := midLaunch
	mu.Unlock()
	if standing != 1 {
		t.Fatalf("the sweep inside the launch's replacement left %d records of it, want the one record standing", standing)
	}
	if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 1 || replaced[0].PodUID != podUpdatePod {
		t.Fatalf("the launch replaced %+v, want the round's pod %s once", replaced, podUpdatePod)
	}
	pod := podOf(rig.claimOf(claim.RoleImplementer).Locator)
	if pod == "" || pod == podUpdatePod {
		t.Fatalf("the launch started the implementer in pod %q, want the pod in place of the round's %s", pod, podUpdatePod)
	}
	rig.registerInto(claim.RoleImplementer, pod)
	rig.awaitSwept(claim.RoleImplementer, "its round to end as the launch's replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.wantToldOnce("once the launch replaced the pod")
	if records := rig.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d records of the launch's replacement once its notice is queued, want none", records)
	}
}
