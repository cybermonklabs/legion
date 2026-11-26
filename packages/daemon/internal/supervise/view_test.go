package supervise

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// View never waits on a decision in flight, where Claim does: while a relaunch waits in the
// runtime it reports the claim as the relaunch last wrote it — launching at the next generation,
// with no process yet — and once the decision ends, as the decision left it. A reader of every
// claim reads it, so one claim's relaunch never holds the others' answer (LEGION-650).
func TestViewAnswersWhileARelaunchWaitsInTheRuntime(t *testing.T) {
	h := newBareHarness(t)
	gated := &gatedResume{Runtime: h.rt, entered: make(chan struct{}, 1), release: make(chan struct{})}
	h.deps.Runtime = gated
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateIdle)
	dead := h.locator()

	died := make(chan error, 1)
	go func() {
		died <- h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now()}})
	}()
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the relaunch never reached the runtime")
	}
	viewed := make(chan ClaimView, 1)
	go func() { viewed <- h.m.View() }()
	select {
	case c := <-viewed:
		if c.State != StateLaunching || c.Generation != 2 || c.Locator != nil {
			t.Errorf("while the relaunch waits View reports %s at generation %d with locator %+v, want launching at 2 with none",
				c.State, c.Generation, c.Locator)
		}
	case <-time.After(time.Second):
		t.Fatal("View waited on the relaunch in the runtime")
	}

	close(gated.release)
	if err := <-died; err != nil {
		t.Fatalf("handle the death: %v", err)
	}
	if c := h.m.View(); c.State != StateLaunching || c.Generation != 2 || c.Locator == nil || *c.Locator == dead {
		t.Errorf("once the relaunch returns View reports %s at generation %d with locator %+v, want launching at 2 with the new process",
			c.State, c.Generation, c.Locator)
	}
}

// HoldsItsSession never waits on a decision in flight, as View does not (review round 14 of
// #4220): the outbox asks it before each publish to a claim's role, and a publish that waited out a
// relaunch in the runtime would hold its issue's lane behind it (LEGION-650). It answers both from
// the session the store last admitted for the claim and, with that forgotten, from the store
// itself, while the relaunch holds the machine's lock.
//
// The control (the pull request records the run): HoldsItsSession under the machine's lock, as
// round 14 first wrote it; the read waits for the relaunch, and the test fails.
func TestHoldsItsSessionAnswersWhileARelaunchWaitsInTheRuntime(t *testing.T) {
	h := newBareHarness(t)
	gated := &gatedResume{Runtime: h.rt, entered: make(chan struct{}, 1), release: make(chan struct{})}
	h.deps.Runtime = gated
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateIdle)
	if h.m.View().Session == "" {
		t.Fatal("the claim records no session, so its hold would be asked of nothing")
	}
	dead := h.locator()
	died := make(chan error, 1)
	go func() {
		died <- h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now()}})
	}()
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the relaunch never reached the runtime")
	}
	for _, from := range []string{"the admitted session", "the store"} {
		if from == "the store" {
			h.m.admitted.Store(nil)
		}
		answered := make(chan error, 1)
		go func() { answered <- h.m.HoldsItsSession(h.ctx) }()
		select {
		case err := <-answered:
			if err != nil {
				t.Errorf("HoldsItsSession from %s answered %v, want nil: the claim holds its own session", from, err)
			}
		case <-time.After(time.Second):
			close(gated.release)
			t.Fatalf("HoldsItsSession from %s waited on the relaunch in the runtime", from)
		}
	}
	close(gated.release)
	if err := <-died; err != nil {
		t.Fatalf("handle the death: %v", err)
	}
}

// View says a claim's process is being stopped (ClaimView.Stopping) only while the decision that
// stops it ends the claim's run, as the operator's suspension does. A process the registration
// deadline retires is relaunched in the same decision, so its claim keeps its role throughout and
// View does not say it is stopping: notice routing reads Stopping, and a sub-architect being
// relaunched would otherwise lose its notices to the architect above it (LEGION-650).
func TestViewSaysAProcessIsStoppingOnlyWhenItsClaimDoesNotRunAgain(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reach    ClaimState
		stop     func(h *harness)
		stopping bool
	}{
		{name: "the operator's suspension", reach: StateIdle, stopping: true,
			stop: func(h *harness) { _ = h.m.Handle(h.ctx, RequestSuspend{Claim: testToken, By: SuspendedByOperator}) }},
		{name: "the registration deadline's retirement and relaunch", reach: StateLaunching,
			stop: func(h *harness) { h.clock.Advance(deadline) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBareHarness(t)
			gated := &gatedRuntime{Runtime: h.rt, entered: make(chan struct{}, 1), release: make(chan struct{})}
			h.deps.Runtime = gated
			if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
				t.Fatal(err)
			}
			h.start(queuedClaim())
			h.reach(tc.reach)

			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				tc.stop(h)
			}()
			select {
			case <-gated.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the suspension never reached the runtime")
			}
			if v := h.m.View(); v.Stopping != tc.stopping {
				t.Errorf("while the runtime stops the process View says stopping %t, want %t", v.Stopping, tc.stopping)
			}
			close(gated.release)
			<-stopped
			h.m.Wait()
			if h.m.View().Stopping {
				t.Error("once the decision returned View still says the process is stopping")
			}
		})
	}
}

// A turn's start is published as the decision writes it, as a launch is (putClaim): the start
// writes the claim working together with the task it carries, in one transaction
// (confirmCarrying, putClaimAndPending), and then goes on to write the turn's step record. A
// reader of the published claim reads it working from the moment the store holds it working, not
// only once the decision ends. The daemon's refresh pass is such a reader: it posts a claim in a
// turn no credit of the refresh gate, and one it read ready would be posted a credit its machine
// gives back only when the turn's decision has ended (internal/daemon's refreshStalePlugins).
//
// The decision is held at the step record's write. The control is putClaimAndPending's publish
// taken out: the claim then reads ready, as the delivery's decision left it, while the store holds
// it working.
func TestATurnsStartIsPublishedAsItIsWrittenWithTheTaskItCarries(t *testing.T) {
	h := newBareHarness(t)
	// reached says which came first: the step record's write, held, or the decision's return.
	reached := make(chan string, 2)
	held := &stepHeldStore{memStore: h.store, reached: reached, release: make(chan struct{})}
	h.deps.Store = held
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})

	held.armed.Store(true)
	var (
		started     error
		decided     = make(chan struct{})
		releaseOnce sync.Once
	)
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
	// A failure below lets the held decision go and waits for it, so no decision outlives the
	// test holding the machine's lock.
	t.Cleanup(func() {
		release()
		<-decided
	})
	go func() {
		defer close(decided)
		started = h.m.Handle(h.ctx, StreamTurnStart{Claim: h.token})
		reached <- "returned"
	}()
	if first := testwait.Receive(t, "the turn's start to write its step record", reached); first != "held" {
		t.Fatalf("the turn's start returned (%v) before it wrote its step record", started)
	}
	if stored := h.store.load(h.token); stored.State != StateWorking {
		t.Fatalf("the store holds the claim %s while the turn's step is written, want %s", stored.State, StateWorking)
	}
	if published := h.m.Published(); published.State != StateWorking {
		t.Fatalf("the claim is published %s while the store holds it %s, want it published as written", published.State, StateWorking)
	}
	release()
	<-decided
	if started != nil {
		t.Fatalf("the turn's start: %v", started)
	}
	if c := h.m.View(); c.State != StateWorking {
		t.Fatalf("the claim reads %s once the turn's start ended, want %s", c.State, StateWorking)
	}
}

// stepHeldStore holds the first step record written once it is armed until the test lets it
// through, and says on reached that it holds it.
type stepHeldStore struct {
	*memStore
	armed   atomic.Bool
	reached chan<- string
	release chan struct{}
}

func (s *stepHeldStore) PutClaimStep(ctx context.Context, token claim.Token, step StepRecord) (bool, error) {
	if s.armed.CompareAndSwap(true, false) {
		s.reached <- "held"
		<-s.release
	}
	return s.memStore.PutClaimStep(ctx, token, step)
}

// The claim the joint write publishes is the claim it writes: normalized, as the store takes it
// (stored, NormalizeClaim). A publish made before that call would show a reader state a claim may
// hold only while it records a compatible running process — a suspension source on a claim that
// is not suspended, a death charged against a process that is gone — where the store holds
// neither. The daemon's refresh pass reads the published claim and decides the lane's terms from
// it (internal/daemon's refreshStalePlugins).
//
// The claim is given both of those to hold and the write is made under the machine's lock,
// which is where every caller makes it (confirmCarrying, settle, repair, retirePending): the
// published claim is read inside that section, before the decision's end publishes again. The
// control is the publish put back before the stored call: each field then reads as the claim held
// it.
func TestTheJointWriteOfAClaimAndItsTaskPublishesTheClaimTheStoreTakes(t *testing.T) {
	h := newBareHarness(t)
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})

	// What only a claim recording a compatible running process may hold, on a claim that is ready
	// and records none: the write takes neither, so neither may the publish.
	h.m.mu.Lock()
	h.m.claim.Locator, h.m.claim.SuspendedBy, h.m.claim.DeathCharged = nil, SuspendedByOperator, true
	err := h.m.putClaimAndPending(h.ctx)
	published := h.m.Published()
	h.m.mu.Unlock()
	if err != nil {
		t.Fatalf("the joint write of the claim and its task: %v", err)
	}
	stored := h.store.load(h.token)
	for _, held := range []struct {
		what             string
		published, store bool
	}{
		{"a suspension source", published.SuspendedBy != "", stored.SuspendedBy != ""},
		{"a death charged", published.DeathCharged, stored.DeathCharged},
	} {
		if held.store {
			t.Fatalf("the store holds %s for a ready claim recording no process: the write normalizes the claim it takes", held.what)
		}
		if held.published {
			t.Errorf("the claim is published holding %s where the store holds none: the publish is of the claim the write takes", held.what)
		}
	}
}
