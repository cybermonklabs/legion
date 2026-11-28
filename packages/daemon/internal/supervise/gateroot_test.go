package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A claim whose record says its process died owes the relaunch that death raised, and a gate root
// that moved since is written for that relaunch rather than met by a replacement of the dead
// process (relaunchUnder). The boot posts GateRootMoved to every running architect, a recorded
// death's among them, before its replay of the deaths (internal/daemon's start): a replacement
// there would launch outside the boot's owed order, charge the death nothing, and leave the turn
// the boot reserves for the replay spent. So GateRootMoved launches nothing and writes the new
// gate root, and the boot's replay of the death (RecordedDeath) launches once, under it.
//
// The death's relaunch is refused before the restart, so the claim is stored over its dead process
// with the death charged and no relaunch waiting, as a restart that forgot a parked wake leaves it.
//
// The negative control is relaunchUnder without its recorded-death branch: over the ready claim the
// event replaces the dead process at once, a second start before the replay, and over the working
// one it waits for a turn the dead process will never end, so the gate root is never written.
func TestAGateRootMovedOverARecordedDeathIsWrittenForTheRelaunchItOwes(t *testing.T) {
	const moved = "LEGION-300"
	for _, state := range []ClaimState{StateReady, StateWorking} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
			if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}}); err == nil {
				t.Fatal("the relaunch the death raised was taken, want the lifecycle recheck's refusal of it")
			}
			h.restart()
			if c := h.claim(); c.State != state || !c.DeathCharged || c.Locator == nil || c.LaunchedTree() == moved {
				t.Fatalf("the restored claim is %s, DeathCharged %v, recording %+v under %q; want %s over its dead process with the death charged, under another gate root",
					c.State, c.DeathCharged, c.Locator, c.LaunchedTree(), state)
			}

			h.must(GateRootMoved{Claim: h.token, Tree: moved})

			h.wantStarts(h.token, 1)
			if held := h.m.View().WakeWaiting; held != nil {
				t.Fatalf("the claim waits on wake %+v after the gate root moved, want none: the death's own relaunch is the launch it owes", *held)
			}
			if got := h.claim().LaunchTree; got != moved {
				t.Fatalf("the claim's launch tree is %q after the gate root moved, want %q written for the relaunch its death owes", got, moved)
			}
			if stored := h.store.load(h.token).LaunchTree; stored != moved {
				t.Fatalf("the claim is stored with launch tree %q, want %q: a restart before the relaunch reads it", stored, moved)
			}

			h.must(RecordedDeath{Claim: h.token, Generation: h.claim().Generation})

			h.wantStarts(h.token, 2)
			var relaunch runtime.SpawnSpec
			for _, method := range []string{"Spawn", "Resume"} {
				for _, call := range h.rt.CallsOf(method) {
					if call.Spec.Claim == h.token && call.Spec.Generation >= relaunch.Generation {
						relaunch = call.Spec
					}
				}
			}
			if relaunch.Tree != moved {
				t.Fatalf("the death's relaunch is launched under %q, want the gate root its issue stands under now, %q", relaunch.Tree, moved)
			}
		})
	}
}
