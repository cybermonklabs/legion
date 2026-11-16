package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// endedHarness is a machine over a queued claim whose role-holder changes are recorded
// (Deps.RoleHolderChanged): each claim the machine told of, with the state the store held for it
// at that moment. before runs on the harness before the machine is built, so a test can change
// its deps.
func endedHarness(t *testing.T, before ...func(*harness)) (*harness, *[]Claim, *[]ClaimState) {
	t.Helper()
	h := newBareHarness(t)
	for _, f := range before {
		f(h)
	}
	told, stored := &[]Claim{}, &[]ClaimState{}
	h.deps.RoleHolderChanged = func(c Claim) {
		*told = append(*told, c)
		*stored = append(*stored, h.store.load(c.Token).State)
	}
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	return h, told, stored
}

// deathIn is the death the runtime reports of the claim's process, as a probe judges it gone.
func deathIn(h *harness) Event {
	return RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}}
}

// The daemon is told each time a landed claim write changes the session the row names as the
// holder of the claim's role: once when the agent registers and so holds its role, and once when
// the claim ends — at its retirement, at its failure and at each suspension. It queues the
// publication of the issue's role sessions, so the ended session leaves the set. Each is told
// once, after the store holds the state it names, because the publication reads the live set from
// the store. A ready, a turn's start and a turn's end change no holder, so they tell nothing. A
// stop and a suspension end an idle claim (a working one's suspension waits for its turn's end),
// and a death ends a working one.
func TestAnEndOfAClaimTellsTheWorkflow(t *testing.T) {
	for _, end := range []struct {
		state  ClaimState
		from   ClaimState
		before func(*harness)
		ended  func(*harness) Event
	}{
		{StateRetired, StateIdle, nil, func(*harness) Event { return RequestStop{Claim: testToken} }},
		// One allowed death with work outstanding, so the death in the agent's turn fails the
		// claim rather than relaunching it.
		{StateFailed, StateWorking, func(h *harness) { h.deps.Limits.LaunchFailures = 1 }, deathIn},
		{StateSuspended, StateIdle, nil, func(*harness) Event {
			return RequestSuspend{Claim: testToken, By: SuspendedByOperator}
		}},
	} {
		t.Run(string(end.state), func(t *testing.T) {
			var before []func(*harness)
			if end.before != nil {
				before = append(before, end.before)
			}
			h, told, stored := endedHarness(t, before...)
			h.reach(StateRegistered)
			if len(*told) != 1 || (*told)[0].State != StateRegistered || (*stored)[0] != StateRegistered {
				t.Fatalf("the registration told of %+v with the store at %v, want the registered claim once", *told, *stored)
			}
			h.reach(end.from)
			if len(*told) != 1 {
				t.Fatalf("the walk to %s told of %+v, want nothing past the registration", end.from, (*told)[1:])
			}

			_ = h.handle(end.ended(h))
			h.wantState(end.state)

			if len(*told) != 2 {
				t.Fatalf("the end told of %d claims, want the one claim once", len(*told)-1)
			}
			if c := (*told)[1]; c.Token != testToken || c.State != end.state || c.Issue != queuedClaim().Issue {
				t.Fatalf("told of %s in state %s on %q, want %s %s on %q", c.Token, c.State, c.Issue, testToken, end.state, queuedClaim().Issue)
			}
			if got := (*stored)[1]; got != end.state {
				t.Fatalf("the store held %s when the end was told, want %s: the publication reads the store", got, end.state)
			}
		})
	}
}

// An end whose write the store refused tells nobody while the store still holds the claim live:
// a publication then would read the session as holding its role. The claim's next decision makes
// the write it owes, and that landed write tells (TestARepairedEndTellsTheWorkflowOnce).
func TestAnEndTheStoreRefusedTellsNobody(t *testing.T) {
	h, told, _ := endedHarness(t)
	h.reach(StateIdle)
	was := len(*told)
	h.store.failures["PutClaim"] = errBoom
	_ = h.handle(RequestStop{Claim: testToken})
	if len(*told) != was {
		t.Fatalf("an end the store refused told of %+v, want nobody", (*told)[was:])
	}
}

// A relaunch takes the claim to launching, so its old session no longer holds the role: the move
// tells the daemon, and the registration of the relaunched agent tells again, with the session
// the new row names. A death of an idle agent with nothing outstanding is charged nothing and
// relaunches.
func TestARelaunchTellsTheWorkflowTwice(t *testing.T) {
	h, told, stored := endedHarness(t)
	h.reach(StateIdle)
	was := len(*told)

	h.observe(runtime.Gone)
	h.wantState(StateLaunching)
	if len(*told) != was+1 || (*told)[was].State != StateLaunching || (*stored)[was] != StateLaunching {
		t.Fatalf("the relaunch told of %+v with the store at %v, want the launching claim once", (*told)[was:], (*stored)[was:])
	}

	h.reach(StateRegistered)
	if len(*told) != was+2 || (*told)[was+1].State != StateRegistered || (*told)[was+1].Session == "" {
		t.Fatalf("the relaunched agent's registration told of %+v, want the registered claim with its session once", (*told)[was+1:])
	}
}

// An end whose own write the store refused is owed, not lost: the claim's next decision makes the
// write it owes (repair), and from then on the store holds the ended state. That landed write is
// what tells the daemon, once, so the ended session leaves Dispatch's set at the repair rather
// than at the next boot within seven days, or at the next ready or end on the issue, or never. Each
// end is driven from a claim whose agent holds its role: a stop and a suspension of an idle claim,
// and a death of a working one whose one allowed death with work outstanding fails it.
func TestARepairedEndTellsTheWorkflowOnce(t *testing.T) {
	for _, end := range []struct {
		state  ClaimState
		from   ClaimState
		before func(*harness)
		ended  func(*harness) Event
	}{
		{StateRetired, StateIdle, nil, func(*harness) Event { return RequestStop{Claim: testToken} }},
		{StateSuspended, StateIdle, nil, func(*harness) Event {
			return RequestSuspend{Claim: testToken, By: SuspendedByOperator}
		}},
		{StateFailed, StateWorking, func(h *harness) { h.deps.Limits.LaunchFailures = 1 }, deathIn},
	} {
		t.Run(string(end.state), func(t *testing.T) {
			var before []func(*harness)
			if end.before != nil {
				before = append(before, end.before)
			}
			h, told, stored := endedHarness(t, before...)
			h.reach(end.from)
			was := len(*told)

			h.store.failures["PutClaim"] = errBoom
			_ = h.handle(end.ended(h))
			h.wantState(end.state)
			if got := h.store.load(testToken).State; got != end.from {
				t.Fatalf("control: the refused end left the store holding %s, want %s", got, end.from)
			}
			if len(*told) != was {
				t.Fatalf("the refused end told of %+v, want nothing before its write lands", (*told)[was:])
			}

			delete(h.store.failures, "PutClaim")
			_ = h.handle(StreamTurnEnd{Claim: testToken})
			if got := h.store.load(testToken).State; got != end.state {
				t.Fatalf("the repair left the store holding %s, want %s", got, end.state)
			}
			t.Logf("after the repair the store holds %s; the daemon was told %d times since the end", end.state, len(*told)-was)
			if len(*told) != was+1 {
				t.Fatalf("the repaired end told the daemon %d times, want once", len(*told)-was)
			}
			if c, at := (*told)[was], (*stored)[was]; c.Token != testToken || c.State != end.state || at != end.state {
				t.Fatalf("told of %s in state %s with the store at %s, want %s %s with the store at %s",
					c.Token, c.State, at, testToken, end.state, end.state)
			}
		})
	}
}
