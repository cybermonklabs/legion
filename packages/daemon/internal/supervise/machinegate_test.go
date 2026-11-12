package supervise

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// entryBound is the bound a route that must answer within one gives its registration here: long
// enough that a machine which is free answers inside it, short enough that a row which waits out
// an entry holding the machine fails quickly rather than by the test runner's own timeout.
const entryBound = 100 * time.Millisecond

// A registration is answered within its bound while a bookkeeping entry holds the machine: those
// entries take the gate before mu (takeMachine), so a caller against a deadline meets the gate it
// can select on rather than mu, which offers no bounded Lock. Before that, a registration whose
// bound passed while one of them wrote to the store waited on mu for the whole write — and
// StartedBy's write is followed by a delivery over the worker stream — so the route answered
// however late that took, the hang the bound exists to end (dispatch://INFRA-2883 R4).
func TestARegistrationIsAnsweredWithinItsBoundWhileABookkeepingEntryHoldsTheMachine(t *testing.T) {
	h := newBareHarness(t)
	uncertain := queuedClaim()
	uncertain.State, uncertain.Generation = StateLaunchUncertain, 1
	if err := h.store.PutClaim(h.ctx, uncertain); err != nil {
		t.Fatal(err)
	}
	h.start(uncertain)

	// The release's own store write, held: the entry holds the machine for as long as the write
	// runs, which is what a slow store or a busy worker stream looks like from a route.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.store.onPut = func(Claim) {
		once.Do(func() { close(entered) })
		<-release
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		if _, err := h.m.ReleaseUncertainLaunch(h.ctx); err != nil {
			t.Errorf("release the uncertain launch: %v", err)
		}
	}()
	<-entered

	ctx, cancel := context.WithTimeout(h.ctx, entryBound)
	defer cancel()
	started := time.Now()
	err := h.m.HandleWithin(ctx, RequestRegister{Claim: testToken, Generation: 1, Session: session, SessionFile: sessionFile})

	if !errors.Is(err, ErrMachineBusy) {
		t.Fatalf("register while a bookkeeping entry holds the machine = %v, want %v", err, ErrMachineBusy)
	}
	// The allowance over the bound is the room a loaded runner takes to schedule the answer; what
	// the row is about is that the registration does not wait out the entry, which no allowance
	// hides.
	if took := time.Since(started); took > entryBound+5*time.Second {
		t.Fatalf("the registration answered after %s, want it answered inside its %s bound", took, entryBound)
	}

	close(release)
	<-released
	if got := h.m.Claim().State; got != StateQueued {
		t.Fatalf("the claim is %s once the release finished, want queued: the refused registration decided nothing", got)
	}
}
