package supervise

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// cancelAtCheckLaunch is the store whose launch recheck (CheckLaunch) is where the daemon's stop
// lands: once the relaunch's admission has committed, it cancels the decision's context, as the stop
// does.
type cancelAtCheckLaunch struct {
	*memStore
	cancel context.CancelFunc
}

func (s cancelAtCheckLaunch) CheckLaunch(ctx context.Context, _ Claim) error {
	s.cancel()
	return ctx.Err()
}

// A released uncertain launch is written queued at the generation it had — by the release, then
// by its relaunch's admission and, before that, by the retirement of a task whose turn is over —
// and launching at its next generation only by the launch itself. A stop between the two leaves the
// claim stored queued at that generation with no process, which the daemon's next boot counts as a
// launch to finish, never launching at a generation no process was started for (LEGION-650).
func TestAStopBetweenAReleasedLaunchsAdmissionAndItsLaunchLeavesItQueuedAtItsGeneration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending *Delivery
	}{
		{name: "a claim holding no task"},
		{name: "a claim holding a task whose turn is over", pending: &Delivery{ID: "the-task", Task: "the task", Generation: 3, ConfirmedAt: epoch}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBareHarness(t)
			uncertain := queuedClaim()
			uncertain.State, uncertain.Generation = StateLaunchUncertain, 1
			if err := h.store.PutClaim(h.ctx, uncertain); err != nil {
				t.Fatal(err)
			}
			if tc.pending != nil {
				if err := h.store.PutDelivery(h.ctx, uncertain.Token, *tc.pending); err != nil {
					t.Fatal(err)
				}
				uncertain.Pending = tc.pending
			}
			ctx, cancel := context.WithCancel(h.ctx)
			h.deps.Store = cancelAtCheckLaunch{memStore: h.store, cancel: cancel}
			h.start(uncertain)
			if released, err := h.m.ReleaseUncertainLaunch(h.ctx); err != nil || !released {
				t.Fatalf("release = %t, %v; want the uncertain launch released", released, err)
			}

			if err := h.m.Handle(ctx, RequestSpawn{Claim: testToken}); !errors.Is(err, context.Canceled) {
				t.Fatalf("spawn = %v, want the stop's cancellation", err)
			}

			stored := h.store.load(testToken)
			if stored.State != StateQueued || stored.Generation != 1 || stored.Locator != nil {
				t.Fatalf("the store holds %s at generation %d with locator %+v, want queued at 1 with none",
					stored.State, stored.Generation, stored.Locator)
			}
			if !stored.ReleasedLaunch() {
				t.Errorf("ReleasedLaunch() is false for the row a release leaves, so the next boot would not finish it: %+v", stored)
			}
			if fresh := queuedClaim(); fresh.ReleasedLaunch() {
				t.Errorf("ReleasedLaunch() is true for a claim created queued and never launched: %+v", fresh)
			}
			if tc.pending != nil && stored.Pending != nil {
				t.Errorf("the store still holds the task whose turn is over: %+v", *stored.Pending)
			}
			h.wantCalls("Spawn", 0)
		})
	}
}

// A boot relaunch whose admission is refused — its tree's cleanup reserved — leaves the store and
// the machine agreeing: both hold the claim queued at the generation it had, the release written
// before the admission is asked. A release kept in memory alone would leave the store saying
// launch_uncertain, which its readers take for a claim that may still have a process, after the
// boot's reconciliation had shown it has none (LEGION-650).
func TestARefusedRelaunchOfAReleasedLaunchLeavesTheStoreAndTheMachineAgreeing(t *testing.T) {
	uncertain := queuedClaim()
	uncertain.State, uncertain.Generation, uncertain.TreeEpoch = StateLaunchUncertain, 1, 1
	store := &epochStore{epoch: 1, reserved: true}
	h := lifecycleHarness(t, store, uncertain)
	if released, err := h.m.ReleaseUncertainLaunch(h.ctx); err != nil || !released {
		t.Fatalf("release = %t, %v; want the uncertain launch released", released, err)
	}

	if err := h.handle(RequestSpawn{Claim: h.token}); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
		t.Fatalf("relaunch during a reserved cleanup = %v, want the reservation's wait", err)
	}

	stored, held := h.store.load(h.token), h.m.Claim()
	if stored.State != held.State || stored.Generation != held.Generation {
		t.Fatalf("the store holds %s at generation %d and the machine %s at generation %d, want them to agree",
			stored.State, stored.Generation, held.State, held.Generation)
	}
	if !stored.ReleasedLaunch() {
		t.Errorf("the store holds %s at generation %d, want the released launch the next boot finishes (queued at 1)", stored.State, stored.Generation)
	}
	h.wantCalls("Spawn", 0)
}

// A release whose write the rule owes stands, as a decision whose write is owed stands (D8 of
// dispatch://INFRA-4099's plan): the claim is queued in memory, the write is owed, and the release
// answers true with no error, so its caller launches, and the launch's decision makes the owed
// write first (repair). A release the rule refuses is undone: the claim stays launch_uncertain in
// memory as it is in the store, so nothing launches on a release no restart would find.
func TestAReleaseTheStoreDoesNotTakeIsUndone(t *testing.T) {
	for _, tc := range claimWriteFailures() {
		t.Run(tc.name, func(t *testing.T) {
			h := newBareHarness(t)
			uncertain := queuedClaim()
			uncertain.State, uncertain.Generation = StateLaunchUncertain, 1
			if err := h.store.PutClaim(h.ctx, uncertain); err != nil {
				t.Fatal(err)
			}
			h.start(uncertain)
			h.store.fail("PutClaim", tc.err)

			released, err := h.m.ReleaseUncertainLaunch(h.ctx)
			want := StateLaunchUncertain
			if tc.owed {
				want = StateQueued
				if !released || err != nil {
					t.Fatalf("release = %t, %v; want it released, its write owed", released, err)
				}
			} else if released || !errors.Is(err, tc.err) {
				t.Fatalf("release = %t, %v; want it refused with the store's error", released, err)
			}
			if got := h.m.Claim().State; got != want {
				t.Fatalf("after the failed release write the machine holds %s, want %s", got, want)
			}
			if got := h.m.View().State; got != want {
				t.Fatalf("after the failed release write View reports %s, want %s", got, want)
			}
			if owed := h.m.owedWrite != nil; owed != tc.owed {
				t.Fatalf("the release write is owed = %t, want %t", owed, tc.owed)
			}
			if got := h.store.load(uncertain.Token).State; got != StateLaunchUncertain {
				t.Fatalf("the store holds %s, want the launch_uncertain the failed write left", got)
			}
		})
	}
}

func TestForgetSessionDropsAReleasedUncertainLaunchBeforeItStartsAgain(t *testing.T) {
	h := newBareHarness(t)
	uncertain := queuedClaim()
	uncertain.State, uncertain.Generation, uncertain.Session, uncertain.SessionFile = StateLaunchUncertain, 1, session, sessionFile
	if err := h.store.PutClaim(h.ctx, uncertain); err != nil {
		t.Fatal(err)
	}
	h.start(uncertain)
	if released, err := h.m.ReleaseUncertainLaunch(h.ctx); err != nil || !released {
		t.Fatalf("release = %t, %v; want the uncertain launch released", released, err)
	}
	if got := h.m.Claim(); got.State != StateQueued || got.Session != session || got.SessionFile != sessionFile {
		t.Fatalf("released claim = %+v, want queued with its session kept", got)
	}

	h.must(RequestForgetSession{Claim: testToken, Session: session, By: SuspendedByOperator})
	h.must(RequestSpawn{Claim: testToken})

	spawns := h.rt.CallsOf("Spawn")
	if len(spawns) != 1 || spawns[0].Spec.ResumeSessionFile != "" {
		t.Fatalf("relaunch after forget = %+v, want one fresh spawn", spawns)
	}
}
