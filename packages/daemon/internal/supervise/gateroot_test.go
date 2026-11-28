package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// An architect whose issue comes to hold its own design gate while its process runs — a person
// unlinked the issue from its parent, so it is a root now — is relaunched in its session under
// its own key and with the gate (relaunchUnder): the launch is built for a claim that holds the
// gate, which is what the daemon's spec reads to tell the architect its design gate steps, and
// from then on the machine reads the claim as its tree's root (Claim.treeRoot), which a stop, a
// close, an exit and a delivery are decided by. A claim in a turn keeps its process to the turn's
// end and is relaunched with the gate then. A claim launched under its own key with no gate,
// whose gate root therefore did not move, is relaunched the same way.
//
// The negative control is relaunchUnder writing the gate root alone, as it did: the first two
// relaunches were built for a claim that holds no gate, and the third claim was not relaunched
// at all.
func TestARelaunchUnderTheIssuesOwnGateCarriesTheGate(t *testing.T) {
	const parent, child = "LEGION-208", "LEGION-209"
	for _, tc := range []struct {
		name     string
		state    ClaimState
		launched string
	}{
		{"ready under its parent's gate root", StateReady, parent},
		{"in a turn under its parent's gate root", StateWorking, parent},
		{"ready under its own key with no gate", StateReady, child},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, Claim{Token: "legion-LEGION-209-architect", Project: "legion", Tree: parent, LaunchTree: tc.launched,
				Issue: child, Role: claim.RoleArchitect, State: StateQueued})
			h.reach(tc.state)
			if c := h.claim(); c.HoldsGate || c.TreeRoot() {
				t.Fatalf("the architect of %s holds the gate before its issue does: %+v", child, c)
			}

			h.must(GateRootMoved{Claim: h.token, Tree: child, HoldsGate: true, Root: true})

			if tc.state == StateWorking {
				h.wantState(StateWorking)
				h.wantCalls("Resume", 0)
				if c := h.claim(); c.HoldsGate || c.LaunchedTree() != tc.launched {
					t.Fatalf("the architect in a turn is launched under %q holding the gate %t, want its launch kept until the turn ends", c.LaunchedTree(), c.HoldsGate)
				}
				h.must(StreamTurnEnd{Claim: h.token})
			}

			h.wantState(StateLaunching)
			h.wantCalls("Resume", 1)
			if built := h.specs.last(t); built.LaunchedTree() != child || !built.HoldsGate {
				t.Fatalf("the relaunch was built for a claim under %q holding the gate %t, want %s holding its own gate", built.LaunchedTree(), built.HoldsGate, child)
			}
			if c := h.claim(); !c.TreeRoot() {
				t.Fatalf("the relaunched architect of %s is no tree root: %+v", child, c)
			}
			if stored := h.store.load(h.token); stored.LaunchTree != child || !stored.HoldsGate {
				t.Fatalf("the claim is stored under %q holding the gate %t, want %s holding it: a restart reads it", stored.LaunchTree, stored.HoldsGate, child)
			}
		})
	}
}

// A start marks a claim with no process of its own with what the Dispatch chain reads of its
// issue (MarkLaunch), and then posts its architect GateRootMoved, as the daemon's outbox does. A
// claim whose process runs keeps what its launch carried until the relaunch that tells the
// process: a running architect whose issue now holds its own gate, under the same gate root, is
// relaunched with the gate, whatever the mark wrote before the event. A queued claim takes both
// at the mark.
//
// The negative control is MarkLaunch writing the gate on a running claim, as it did: the event
// then found the claim marked already and relaunched nothing.
func TestAStartsMarkOfARunningClaimWaitsForItsRelaunch(t *testing.T) {
	const child = "LEGION-209"
	running := Claim{Token: "legion-LEGION-209-architect", Project: "legion", Tree: "LEGION-208", LaunchTree: child,
		Issue: child, Role: claim.RoleArchitect, State: StateQueued}
	t.Run("a running architect, the same gate root, now holding its gate", func(t *testing.T) {
		h := newHarnessOf(t, running)
		h.reach(StateReady)

		if err := h.m.MarkLaunch(h.ctx, child, true); err != nil {
			t.Fatalf("mark the start: %v", err)
		}
		if c, stored := h.claim(), h.store.load(h.token); c.HoldsGate || stored.HoldsGate {
			t.Errorf("the running architect holds the gate %t (stored %t) after the mark, want its launch's answer kept until its relaunch", c.HoldsGate, stored.HoldsGate)
		}
		h.must(GateRootMoved{Claim: h.token, Tree: child, HoldsGate: true, Root: true})

		h.wantState(StateLaunching)
		h.wantCalls("Resume", 1)
		if built := h.specs.last(t); built.LaunchedTree() != child || !built.HoldsGate {
			t.Fatalf("the relaunch was built for a claim under %q holding the gate %t, want %s holding its own gate", built.LaunchedTree(), built.HoldsGate, child)
		}
	})
	t.Run("a queued claim", func(t *testing.T) {
		h := newHarnessOf(t, running)

		if err := h.m.MarkLaunch(h.ctx, "LEGION-100", true); err != nil {
			t.Fatalf("mark the start: %v", err)
		}
		if stored := h.store.load(h.token); stored.LaunchTree != "LEGION-100" || !stored.HoldsGate {
			t.Fatalf("the queued claim is stored under %q holding the gate %t, want both the mark wrote", stored.LaunchTree, stored.HoldsGate)
		}
	})
}

// Each launch records the architect its addressing named (Claim.LaunchOwner), which is the claim's
// own exactly where the launch had a root architect's prompt (LaunchedRoot). A running architect
// whose issue's owner walk now ends at its own issue, under the same gate root and with the same
// gate, as when the parent of a gate holder leaves Legion's work, is relaunched in its session
// (GateRootMoved's Root), and the new launch records its own claim; the pass's next post decides
// nothing.
//
// The negative control is relaunchUnder weighing the gate root and the gate alone: the claim was
// not relaunched.
func TestAnArchitectIsRelaunchedWhenItBecomesItsTreesRoot(t *testing.T) {
	const parent, holder = "LEGION-208", "LEGION-209"
	h := newHarnessOf(t, Claim{Token: "legion-LEGION-209-architect", Project: "legion", Tree: parent, LaunchTree: holder,
		Issue: holder, Role: claim.RoleArchitect, State: StateQueued, HoldsGate: true})
	above := claim.Token("legion-LEGION-208-architect")
	h.specs.addresses(above)
	h.reach(StateReady)
	if c, stored := h.claim(), h.store.load(h.token); c.LaunchOwner != above || stored.LaunchOwner != above || c.LaunchedRoot() {
		t.Fatalf("the gate holder's architect under %s records the owner %q (stored %q), root %t; want %s, no root",
			parent, c.LaunchOwner, stored.LaunchOwner, c.LaunchedRoot(), above)
	}

	h.specs.addresses(h.token)
	h.must(GateRootMoved{Claim: h.token, Tree: holder, HoldsGate: true, Root: true})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	if c, stored := h.claim(), h.store.load(h.token); c.LaunchOwner != h.token || stored.LaunchOwner != h.token || !c.LaunchedRoot() {
		t.Fatalf("the relaunched architect records the owner %q (stored %q), root %t; want its own claim, a root",
			c.LaunchOwner, stored.LaunchOwner, c.LaunchedRoot())
	}
	h.relaunched()
	h.must(GateRootMoved{Claim: h.token, Tree: holder, HoldsGate: true, Root: true})
	h.wantState(StateReady)
	h.wantCalls("Resume", 1)
}

// A claim reads whether its last launch had a root architect's prompt from the architect that
// launch named (LaunchOwner); one no launch of this release built reads it from the gate root its
// launch named (LaunchedTree), which for a claim no start marked is the tree it stores, the release
// before's rule for a tree's root. A phase worker's launch is never a root's.
func TestALaunchedRootReadsTheLaunchesOwner(t *testing.T) {
	const own, above, issue = claim.Token("legion-LEGION-209-architect"), claim.Token("legion-LEGION-208-architect"), "LEGION-209"
	for _, tc := range []struct {
		name string
		c    Claim
		want bool
	}{
		{"a launch addressed to the claim itself", Claim{Token: own, Role: claim.RoleArchitect, Issue: issue, LaunchTree: issue, LaunchOwner: own}, true},
		{"a gate holder's launch addressed to the architect above it", Claim{Token: own, Role: claim.RoleArchitect, Issue: issue, LaunchTree: issue, LaunchOwner: above}, false},
		{"no launch of this release, its stored tree its own", Claim{Token: own, Role: claim.RoleArchitect, Issue: issue, Tree: issue}, true},
		{"no launch of this release, another stored tree", Claim{Token: own, Role: claim.RoleArchitect, Issue: issue, Tree: "LEGION-208"}, false},
		{"a phase worker", Claim{Token: testToken, Role: claim.RoleImplementer, Issue: issue, Tree: issue, LaunchOwner: testToken}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.LaunchedRoot(); got != tc.want {
				t.Fatalf("LaunchedRoot of %+v = %t, want %t", tc.c, got, tc.want)
			}
		})
	}
}

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
