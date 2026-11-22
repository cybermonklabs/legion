package supervise

import (
	"context"
	"testing"
	"time"
)

// stoppedStore is the claims store as the database answers a decision made on the context the
// daemon's stop has cancelled: the lifecycle recheck fails with that context's error, as the
// pool's query does. Every call is counted (countingStore), the recheck with the writes, so a
// test reads each call a decision made, whatever the store answered it.
type stoppedStore struct{ *countingStore }

func (s stoppedStore) CheckLaunch(ctx context.Context, c Claim) error {
	s.record("CheckLaunch")
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.countingStore.CheckLaunch(ctx, c)
}

// newStoppedStoreHarness is a machine over a queued claim the store already holds, built on a
// stoppedStore, so the claim is seeded through the double and its tally holds the machine's own
// calls and nothing else.
func newStoppedStoreHarness(t *testing.T) (*harness, stoppedStore) {
	t.Helper()
	h := newBareHarness(t)
	store := stoppedStore{&countingStore{Store: h.store}}
	h.deps.Store = store
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, store
}

// timersGone waits for the machine to hold no armed timer and the clock to hold no live one,
// which the end of the machine's context brings about on a goroutine of its own, and reports
// whether that came within five seconds.
func (h *harness) timersGone() bool {
	h.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		h.m.mu.Lock()
		held := len(h.m.timers)
		h.m.mu.Unlock()
		if held == 0 && h.clock.Live() == 0 {
			return true
		}
	}
	return false
}

// wantTimesNothing is the machine of a stopped supervision having timed nothing since the counts
// it is handed were read: no timer was armed, the runtime was asked nothing, the store was called
// for nothing, and no timer's decision was logged.
func (h *harness) wantTimesNothing(store stoppedStore, armed, calls, runtimeCalls int) {
	h.t.Helper()
	if got := h.clock.Armed(); got != armed {
		h.t.Errorf("%d timers were armed after supervision stopped, want none", got-armed)
	}
	if got := h.rt.Methods(); len(got) != runtimeCalls {
		h.t.Errorf("the runtime was asked %v after supervision stopped, want nothing", got[runtimeCalls:])
	}
	if got := store.writes(); len(got) != calls {
		h.t.Errorf("the store was called for %v after supervision stopped, want nothing", got[calls:])
	}
	if lines := h.logs.lines(`msg="supervise: timer"`); len(lines) != 0 {
		h.t.Errorf("logged %d timer decisions after supervision stopped, want none: %v", len(lines), lines)
	}
}

// A supervision that has stopped times nothing of its claims. Its stop cancels every machine's
// context (the daemon's halt), and then no timer of a machine fires, none is armed again, and none
// decides: no probe, no lifecycle recheck and no claim write.
//
// On the rig a claim met mid-launch kept its registration deadline after the daemon logged that
// it had stopped. The deadline's probe found the process alive, the lifecycle recheck on the
// cancelled context failed, and the deadline armed itself again at the probe interval, firing
// every 30 seconds for as long as the process lived; the stale machine's probe also charged a death
// to a process the next boot's own relaunch had replaced.
//
// Two subtests, one per guard. The first is the claim met mid-launch: the stop disarms what its
// launch armed, so the clock moving past several probe intervals fires nothing. The second is a
// deadline armed after the stop by a registration an API route decided while the stop drained the
// routes (the daemon's endDrain): that decision runs on the drain's context, not the machine's, so
// it arms its deadline as ever; the deadline fires into a machine whose context is done, and it
// decides nothing and arms nothing again.
func TestAStoppedSupervisionTimesNothing(t *testing.T) {
	t.Run("the timers of a claim met mid-launch", func(t *testing.T) {
		h, store := newStoppedStoreHarness(t)
		h.launch()
		h.m.mu.Lock()
		_, boot := h.m.timers[TimerBoot]
		_, registration := h.m.timers[TimerRegistration]
		h.m.mu.Unlock()
		if !boot || !registration || h.clock.Live() == 0 {
			t.Fatalf("the launch armed the boot interval %v and the registration deadline %v (%d live), want both: the control for the stop to take them back",
				boot, registration, h.clock.Live())
		}
		armed, fired, calls, runtimeCalls := h.clock.Armed(), h.clock.Fired(), len(store.writes()), len(h.rt.Methods())

		h.shutdown()

		if !h.timersGone() {
			t.Errorf("%d timers are still armed after supervision stopped, want none", h.clock.Live())
		}
		h.advance(deadline + 4*testProbe)

		if got := h.clock.Fired(); got != fired {
			t.Errorf("%d timers fired after supervision stopped, want none", got-fired)
		}
		h.wantTimesNothing(store, armed, calls, runtimeCalls)
	})

	t.Run("a deadline the drain arms after the stop", func(t *testing.T) {
		h, store := newStoppedStoreHarness(t)
		h.launch()
		h.connect()
		h.shutdown()
		if !h.timersGone() {
			t.Errorf("%d timers are still armed after supervision stopped, want none", h.clock.Live())
		}

		// The agent's registration, decided by its API route on the drain's context.
		draining := context.Background()
		if err := h.m.Handle(draining, RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
			SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract}); err != nil {
			t.Fatalf("the registration in the drain: %v", err)
		}
		h.wantState(StateRegistered)
		h.m.mu.Lock()
		_, registration := h.m.timers[TimerRegistration]
		h.m.mu.Unlock()
		if !registration || h.clock.Live() != 1 {
			t.Fatalf("the registration armed the deadline %v (%d live), want it alone: the control for its firing to decide nothing",
				registration, h.clock.Live())
		}
		armed, fired, calls, runtimeCalls := h.clock.Armed(), h.clock.Fired(), len(store.writes()), len(h.rt.Methods())

		h.advance(deadline + 4*testProbe)

		if got := h.clock.Fired(); got != fired+1 {
			t.Errorf("%d timers fired, want the deadline the registration armed, once", got-fired)
		}
		h.wantTimesNothing(store, armed, calls, runtimeCalls)
		h.wantState(StateRegistered)
		if lines := h.logs.lines("a timer fired after the machine's context ended"); len(lines) != 1 {
			t.Errorf("logged %v, want the deadline's firing logged once as deciding nothing", lines)
		}
	})
}
