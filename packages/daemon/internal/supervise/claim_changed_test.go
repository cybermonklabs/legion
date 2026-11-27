package supervise

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// claimChanges keeps every change of the claim its machine told (Deps.ClaimChanged), with what
// the callback read of the machine at the moment it was told: whether a decision was in flight
// (Machine.Deciding) and whether Machine.Claim, which waits for the machine's lock, answered.
type claimChanges struct {
	mu    sync.Mutex
	told  []toldChange
	since int
}

// toldChange is one call of Deps.ClaimChanged: the view the decision replaced, the view it
// published, and what the callback could read of the machine then.
type toldChange struct {
	before, after ClaimView
	deciding      Event
	read          bool
	readState     ClaimState
}

// tell is the callback: it reads the machine's claim under its lock, bounded, so a call made
// inside the decision is recorded as one that could not read it rather than hanging the test.
func (c *claimChanges) tell(m func() *Machine) func(before, after ClaimView) {
	return func(before, after ClaimView) {
		machine := m()
		change := toldChange{before: before, after: after, deciding: machine.Deciding()}
		read := make(chan Claim, 1)
		go func() { read <- machine.Claim() }()
		select {
		case claimed := <-read:
			change.read, change.readState = true, claimed.State
		case <-time.After(5 * time.Second):
		}
		c.mu.Lock()
		c.told = append(c.told, change)
		c.mu.Unlock()
	}
}

// step is every change told since the last step.
func (c *claimChanges) step() []toldChange {
	c.mu.Lock()
	defer c.mu.Unlock()
	told := append([]toldChange(nil), c.told[c.since:]...)
	c.since = len(c.told)
	return told
}

// A claim's machine tells the daemon each change of the claim that a row waiting for that claim
// waits on (dispatch://INFRA-4131/spec#b-r-wait-ready, #b-r-wait-event): its state, its launch
// (Generation), its ready (LastReady), its turn start (LastTurnStart), its pending delivery (the
// delivery itself, or its confirmation), the end of a send in flight, and a caller refused the
// machine's lock while a decision held it (PublishUpdateAsk's ErrMachineBusy). It tells each
// after the decision that made it, with the lock released, so the callback reads the machine's
// claim without waiting on anything; and it tells nothing for a decision that changed none of
// them, a turn's tool step. A ready writes one Info line, `supervise: ready`, naming the claim
// and its generation, which a reader of the daemon's log counts the claim's readies by.
func TestClaimChangedIsToldAfterEachDecision(t *testing.T) {
	h := newBareHarness(t)
	changes := &claimChanges{}
	h.deps.ClaimChanged = changes.tell(func() *Machine { return h.m })
	c := queuedClaim()
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)

	// told fails the test unless the step told at least one change, every one of them after its
	// decision, and one of them is a change want names.
	told := func(what string, want func(before, after ClaimView) bool) {
		t.Helper()
		step := changes.step()
		if len(step) == 0 {
			t.Fatalf("%s: told nothing, want the change told", what)
		}
		found := false
		for _, change := range step {
			if change.deciding != nil || !change.read {
				t.Fatalf("%s: told inside the decision (deciding %T, claim read %t), want it told once the decision ended",
					what, change.deciding, change.read)
			}
			if change.readState != change.after.State {
				t.Fatalf("%s: told a view in state %s while the machine holds %s, want the view the decision published",
					what, change.after.State, change.readState)
			}
			found = found || want(change.before, change.after)
		}
		if !found {
			t.Fatalf("%s: told %d changes, none of them this one", what, len(step))
		}
	}

	h.launch()
	told("the launch", func(before, after ClaimView) bool {
		return before.Generation == 0 && after.Generation == 1 && after.State == StateLaunching
	})
	h.connect()
	told("the shim's hello", func(before, after ClaimView) bool {
		return before.State == StateLaunching && after.State == StateShimConnected
	})
	h.register()
	told("the registration", func(before, after ClaimView) bool { return after.State == StateRegistered })

	h.advance(time.Second)
	h.ready()
	told("the ready", func(before, after ClaimView) bool {
		return after.State == StateReady && !after.LastReady.Equal(before.LastReady) && after.LastReady.Equal(h.clock.Now())
	})
	readies := h.records.of("supervise: ready")
	if len(readies) != 1 || readies[0].level != slog.LevelInfo {
		t.Fatalf("the ready wrote %d supervise: ready lines (%+v), want one at Info", len(readies), readies)
	}
	if token, ok := readies[0].attr("claim"); !ok || token.String() != string(h.token) {
		t.Fatalf("the ready line names claim %v, want %s", token, h.token)
	}
	if generation, ok := readies[0].attr("generation"); !ok || generation.Uint64() != 1 {
		t.Fatalf("the ready line names generation %v, want 1", generation)
	}

	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	step := changes.step()
	pendingQueued, sendEnded := false, false
	for _, change := range step {
		pendingQueued = pendingQueued || (change.before.Pending == nil && change.after.Pending != nil)
		sendEnded = sendEnded || (change.before.SendInFlight && !change.after.SendInFlight)
	}
	if !pendingQueued || !sendEnded {
		t.Fatalf("the delivery told %d changes, the task queued %t and its send ended %t; want both told", len(step), pendingQueued, sendEnded)
	}

	h.advance(time.Second)
	h.must(StreamTurnStart{Claim: h.token})
	told("the turn's start", func(before, after ClaimView) bool {
		return after.State == StateWorking && after.LastTurnStart.Equal(h.clock.Now()) &&
			before.Pending != nil && after.Pending != nil && before.Pending.ConfirmedAt.IsZero() && !after.Pending.ConfirmedAt.IsZero()
	})

	h.advance(time.Second)
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	h.stepped("bash", "toolu_turn1_0", "", true)
	if step := changes.step(); len(step) != 0 {
		t.Fatalf("the turn's tool steps told %d changes (%+v), want none: they change nothing a waiting row waits on", len(step), step)
	}

	h.must(StreamTurnEnd{Claim: h.token})
	told("the turn's end", func(before, after ClaimView) bool { return before.State == StateWorking && after.State == StateIdle })

	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	told("the operator's suspension", func(before, after ClaimView) bool { return after.State == StateSuspended && after.RuleHolds() })
	h.must(RequestResume{Claim: h.token, By: SuspendedByOperator})
	told("the operator's resume", func(before, after ClaimView) bool {
		return before.RuleHolds() && !after.RuleHolds() && after.Generation == 2
	})

	// A section that holds the lock and changes nothing of the claim, which refuses the update
	// ask's try of the lock: its end tells that refusal, and only that.
	h.m.mu.Lock()
	_, err := h.m.PublishUpdateAsk(context.Background(), PodRound("pod-1"), func(context.Context) (bool, error) {
		t.Error("the ask over a held lock published")
		return false, nil
	})
	h.m.unlock()
	if !errors.Is(err, ErrMachineBusy) {
		t.Fatalf("the ask over a held lock = %v, want ErrMachineBusy", err)
	}
	told("the refused lock", func(before, after ClaimView) bool {
		return before.State == after.State && before.Generation == after.Generation
	})
	h.m.mu.Lock()
	h.m.unlock()
	if step := changes.step(); len(step) != 0 {
		t.Fatalf("a second section that changed nothing told %d changes, want none: the refusal was told once", len(step))
	}
}
