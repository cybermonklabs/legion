package supervise

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// cuttableResume holds the relaunch's start on the runtime, once armed, until its context ends,
// as a Sandbox start waits minutes on its pod: the only way out of it is a suspension's cut
// (Machine.stopStart).
type cuttableResume struct {
	*fake.Runtime
	armed   atomic.Bool
	entered chan struct{}
}

func (g *cuttableResume) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if !g.armed.Load() {
		return g.Runtime.Resume(ctx, prev, spec)
	}
	g.entered <- struct{}{}
	<-ctx.Done()
	return runtime.Locator{}, ctx.Err()
}

// cancelRefusingStore is the store double that refuses a claim write on an ended context, as the
// database's driver does: the write a daemon's halt cancels is not taken.
type cancelRefusingStore struct{ Store }

func (s cancelRefusingStore) PutClaim(ctx context.Context, c Claim) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.PutClaim(ctx, c)
}

// operatorSuspension is the operator's suspension of the harness's claim.
func (h *harness) operatorSuspension() RequestSuspend {
	return RequestSuspend{Claim: h.token, Reason: "an operator stops the claim", By: SuspendedByOperator}
}

// awaitAnswer is the answer on answered within 10 seconds.
func awaitAnswer(t *testing.T, what string, answered <-chan error) error {
	t.Helper()
	select {
	case err := <-answered:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never answered", what)
		return nil
	}
}

// An operator's suspension that reaches the claim while the daemon's relaunch waits on its start
// cuts that start, and the relaunch answers ErrSuspensionWaiting, not the start's own context
// error: the suspension decides the claim's process next, so the daemon's keeper writes no line for
// the relaunch, as it writes none for any act of an operator's.
func TestASuspensionThatCutsTheRelaunchsStartAnswersTheRelaunchTyped(t *testing.T) {
	h := newBareHarness(t)
	rt := &cuttableResume{Runtime: h.rt, entered: make(chan struct{}, 1)}
	h.deps.Runtime = rt
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.token = c.Token
	h.start(c)
	h.waitingSilently()
	rt.armed.Store(true)

	relaunched := make(chan error, 1)
	go func() { relaunched <- h.m.Handle(h.ctx, h.relaunchOf(false)) }()
	select {
	case <-rt.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the relaunch's start never reached the runtime")
	}
	suspended := make(chan error, 1)
	go func() { suspended <- h.m.Handle(h.ctx, h.operatorSuspension()) }()

	if err := awaitAnswer(t, "the relaunch", relaunched); !errors.Is(err, ErrSuspensionWaiting) {
		t.Fatalf("relaunch whose start an operator's suspension cut = %v, want ErrSuspensionWaiting", err)
	}
	_ = awaitAnswer(t, "the suspension", suspended)
	h.m.Wait()
}

// An operator's suspension that waits for the claim while the relaunch's stop fails finds the
// process still running and decides it next, so the relaunch answers ErrSuspensionWaiting with the
// stop's own error: the daemon's keeper writes no line for a stop that an operator's act will end.
func TestASuspensionWaitingWhileTheRelaunchsStopFailsAnswersTheRelaunchTyped(t *testing.T) {
	h := newBareHarness(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	h.deps.Runtime = &gatedRelaunchStop{inPlaceRuntime: &inPlaceRuntime{Runtime: h.rt, stopErr: runtime.ErrProcessOutlivedStop},
		entered: entered, release: release}
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.token = c.Token
	h.start(c)
	h.waitingSilently()

	relaunched := make(chan error, 1)
	go func() { relaunched <- h.m.Handle(h.ctx, h.relaunchOf(false)) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the relaunch's stop never reached the runtime")
	}
	suspended := make(chan error, 1)
	go func() { suspended <- h.m.Handle(h.ctx, h.operatorSuspension()) }()
	testwait.Eventually(t, "the operator's suspension to wait for the claim", func() bool { return suspensionQueued(h.m) })
	close(release)

	err := awaitAnswer(t, "the relaunch", relaunched)
	if !errors.Is(err, ErrSuspensionWaiting) || !errors.Is(err, runtime.ErrProcessOutlivedStop) {
		t.Fatalf("relaunch whose stop failed with a suspension waiting = %v, want ErrSuspensionWaiting with the stop's error", err)
	}
	_ = awaitAnswer(t, "the suspension", suspended)
	h.m.Wait()
}

// A daemon's halt that cancels the relaunch's context while its stop waits on the runtime fails
// the stop, and the record of the relaunch written before the stop is put back all the same: the
// write outlives the cancelled context, so the next daemon reads no relaunch that stopped nothing,
// and the 6 hours are not spent.
func TestAHaltDuringTheRelaunchsStopPutsItsRecordBack(t *testing.T) {
	h := newBareHarness(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	h.deps.Runtime = &gatedRelaunchStop{inPlaceRuntime: &inPlaceRuntime{Runtime: h.rt, stopErr: context.Canceled},
		entered: entered, release: release}
	h.deps.Store = cancelRefusingStore{Store: h.store}
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.token = c.Token
	h.start(c)
	h.waitingSilently()

	halting, halt := context.WithCancel(h.ctx)
	relaunched := make(chan error, 1)
	go func() { relaunched <- h.m.Handle(halting, h.relaunchOf(false)) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the relaunch's stop never reached the runtime")
	}
	halt()
	close(release)
	if err := awaitAnswer(t, "the relaunch", relaunched); err == nil {
		t.Fatal("relaunch whose stop the halt cut = nil, want the stop's failure")
	}
	h.m.Wait()
	if stored := h.store.load(h.token); !stored.LastRelaunch.IsZero() {
		t.Fatalf("after a halt cut the relaunch's stop the store records the relaunch at %v, want none", stored.LastRelaunch)
	}
}
