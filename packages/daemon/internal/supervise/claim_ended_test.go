package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// endedHarness is a machine over a queued claim whose ends are recorded (Deps.ClaimEnded): each
// claim the machine told of, with the state the store held for it at that moment.
func endedHarness(t *testing.T) (*harness, *[]Claim, *[]ClaimState) {
	t.Helper()
	h := newBareHarness(t)
	told, stored := &[]Claim{}, &[]ClaimState{}
	h.deps.ClaimEnded = func(c Claim) {
		*told = append(*told, c)
		*stored = append(*stored, h.store.load(c.Token).State)
	}
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	return h, told, stored
}

// A claim ends at its retirement, at its failure and at each suspension, and at each of them the
// machine tells the daemon (Deps.ClaimEnded), which queues the publication of the issue's role
// sessions: the ended session leaves the set. It tells once, after the store holds the ended state,
// because the publication reads the live set from the store. A ready and a turn's end are no end of
// the claim, so they tell nothing.
func TestAnEndOfAClaimTellsTheWorkflow(t *testing.T) {
	for _, end := range []struct {
		state ClaimState
		ended func(*harness)
	}{
		{StateRetired, func(h *harness) { h.must(RequestStop{Claim: testToken}) }},
		{StateFailed, func(h *harness) {
			for range 3 {
				h.observe(runtime.Gone)
			}
		}},
		{StateSuspended, func(h *harness) { h.must(RequestSuspend{Claim: testToken, By: SuspendedByOperator}) }},
	} {
		t.Run(string(end.state), func(t *testing.T) {
			h, told, stored := endedHarness(t)
			h.reach(StateReady)
			h.must(StreamTurnStart{Claim: testToken})
			h.must(StreamTurnEnd{Claim: testToken})
			h.wantState(StateIdle)
			if len(*told) != 0 {
				t.Fatalf("a ready and a turn's end told of %+v, want nothing", *told)
			}

			end.ended(h)
			h.wantState(end.state)

			if len(*told) != 1 {
				t.Fatalf("the end told of %d claims, want the one claim once", len(*told))
			}
			if c := (*told)[0]; c.Token != testToken || c.State != end.state || c.Issue != queuedClaim().Issue {
				t.Fatalf("told of %s in state %s on %q, want %s %s on %q", c.Token, c.State, c.Issue, testToken, end.state, queuedClaim().Issue)
			}
			if got := (*stored)[0]; got != end.state {
				t.Fatalf("the store held %s when the end was told, want %s: the publication reads the store", got, end.state)
			}
		})
	}
}

// An end whose write the store refused tells nobody: the store still holds the claim live, so a
// publication would read the session as holding its role. The claim's next decision makes the write
// it owes, and the daemon's boot pass publishes the issue (the window the daemon's claimEnded names).
func TestAnEndTheStoreRefusedTellsNobody(t *testing.T) {
	h, told, _ := endedHarness(t)
	h.reach(StateIdle)
	h.store.failures["PutClaim"] = errBoom
	_ = h.handle(RequestStop{Claim: testToken})
	if len(*told) != 0 {
		t.Fatalf("an end the store refused told of %+v, want nobody", *told)
	}
}
