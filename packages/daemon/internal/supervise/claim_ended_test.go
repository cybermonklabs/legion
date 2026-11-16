package supervise

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

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

// refusingStates is the store behind a database trigger that refuses outright every claim write
// that sets one of the states it names, as the acceptance rig's trigger refused an end's write.
// The server judged the statement (SQLSTATE P0001), so the machine owes no repair for it
// (writeOwed), and the row stays behind memory until a later claim write lands. Every claim write
// passes it: PutClaim, the admission's AdmitClaim, PutClaimAndDelivery and RetireDelivery.
type refusingStates struct {
	*memStore
	mu     sync.Mutex
	states []ClaimState
}

// refuse names the states the trigger refuses from now on; naming none lifts it.
func (s *refusingStates) refuse(states ...ClaimState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = states
}

func (s *refusingStates) refusal(c Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.states, c.State) {
		return nil
	}
	return &pgconn.PgError{Code: "P0001", Message: "the trigger refuses a claim write that sets " + string(c.State), Severity: "ERROR"}
}

func (s *refusingStates) PutClaim(ctx context.Context, c Claim) error {
	if err := s.refusal(c); err != nil {
		return err
	}
	return s.memStore.PutClaim(ctx, c)
}

func (s *refusingStates) AdmitClaim(ctx context.Context, c Claim) (Claim, error) {
	if err := s.refusal(c); err != nil {
		return Claim{}, err
	}
	return s.memStore.AdmitClaim(ctx, c)
}

func (s *refusingStates) PutClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error {
	if err := s.refusal(c); err != nil {
		return err
	}
	return s.memStore.PutClaimAndDelivery(ctx, c, d)
}

func (s *refusingStates) RetireDelivery(ctx context.Context, c Claim, deliveryID string) error {
	if err := s.refusal(c); err != nil {
		return err
	}
	return s.memStore.RetireDelivery(ctx, c, deliveryID)
}

// behindTrigger puts the trigger in front of the harness's store before the machine is built
// (endedHarness's before), and hands the test the trigger.
func behindTrigger(trigger **refusingStates) func(*harness) {
	return func(h *harness) {
		*trigger = &refusingStates{memStore: h.store}
		h.deps.Store = *trigger
	}
}

// The three claim writes TestAnEndOfAClaimTellsTheWorkflow does not reach tell the daemon too,
// each where it is the first claim write to land since the row's holder changed: a retirement
// that writes the retired claim with the delete of its round's unanswered ask (retirePending), a
// resume's admission over an end whose own write the server refused (revive), and the note a
// provider error's turn writes beside its task over a registration whose writes the server
// refused (providerErrorClears). Each is told once, with the store already holding the state the
// tell names, because the publication reads the live set from the store.
func TestEveryClaimWriteThatLandsTellsTheWorkflow(t *testing.T) {
	// toldOnce fails unless the daemon was told once since was, of the claim in state want, with
	// the store already at want.
	toldOnce := func(t *testing.T, told *[]Claim, stored *[]ClaimState, was int, want ClaimState) {
		t.Helper()
		if len(*told) != was+1 {
			t.Fatalf("the write told the daemon %d times, want once: %+v", len(*told)-was, (*told)[was:])
		}
		if c, at := (*told)[was], (*stored)[was]; c.Token != testToken || c.State != want || at != want {
			t.Fatalf("told of %s in state %s with the store at %s, want %s %s with the store at %s",
				c.Token, c.State, at, testToken, want, want)
		}
	}

	t.Run("a retirement that drops its round's unanswered ask", func(t *testing.T) {
		h, told, stored := endedHarness(t, func(h *harness) { h.deps.PluginContract = PodUpdateContract })
		h.reach(StateIdle)
		h.openUpdate(testRound, testReasons)
		// The restart that restores an older daemon's ask builds the machine again from the row,
		// which holds the idle claim and its session.
		ask := h.unsentAsk(testRound)
		was := len(*told)

		_ = h.handle(RequestStop{Claim: h.token})
		h.wantState(StateRetired)
		if row := h.store.load(h.token); row.State != StateRetired || row.Pending != nil {
			t.Fatalf("control: the stop left the row %s holding %+v, want it retired with the ask %s deleted", row.State, row.Pending, ask.ID)
		}
		toldOnce(t, told, stored, was, StateRetired)
	})

	t.Run("a resume's admission over a suspension the server refused", func(t *testing.T) {
		var trigger *refusingStates
		h, told, stored := endedHarness(t, behindTrigger(&trigger))
		h.reach(StateIdle)
		was := len(*told)
		trigger.refuse(StateSuspended)
		_ = h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		h.wantState(StateSuspended)
		if row := h.store.load(h.token).State; row != StateIdle || len(*told) != was {
			t.Fatalf("control: the refused suspension left the row %s and told %d times, want idle and none", row, len(*told)-was)
		}

		// The launch's own write is refused too, so the admission is the one write of the resume
		// that lands.
		trigger.refuse(StateLaunching)
		_ = h.handle(RequestResume{Claim: h.token})
		if row := h.store.load(h.token).State; row != StateSuspended {
			t.Fatalf("control: the resume left the row %s, want the suspended claim its admission wrote", row)
		}
		toldOnce(t, told, stored, was, StateSuspended)
	})

	t.Run("a provider error's note over a registration the server refused", func(t *testing.T) {
		var trigger *refusingStates
		h, told, stored := endedHarness(t, behindTrigger(&trigger))
		h.reach(StateShimConnected)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
		was := len(*told)
		// Every write of the registered agent's states is refused, from its registration to its
		// turn's end, so the row still names no holder when the turn's prompt answers.
		trigger.refuse(StateRegistered, StateReady, StateWorking, StateIdle)
		_ = h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
			SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
		_ = h.handle(RequestReady{Claim: h.token, Generation: h.generation(), Session: session})
		_ = h.handle(StreamTurnStart{Claim: h.token})
		_ = h.handleQuiet(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		if row := h.store.load(h.token).State; row != StateShimConnected || len(*told) != was {
			t.Fatalf("control: the refused writes left the row %s and told %d times, want it shim-connected and none", row, len(*told)-was)
		}

		trigger.refuse()
		delivery := h.pending().ID
		if err := h.handle(StreamPromptResult{Claim: h.token, DeliveryID: delivery, AgentInvoked: true,
			Error: "overloaded_error", HTTPStatus: 529}); err != nil {
			t.Fatalf("the provider error's result: %v", err)
		}
		if row := h.store.load(h.token); row.State != StateIdle || row.Pending == nil || row.Pending.ProviderErrorRetries != 1 {
			t.Fatalf("control: the result left the row %s holding %+v, want idle with the task counted one send", row.State, row.Pending)
		}
		toldOnce(t, told, stored, was, StateIdle)
	})
}
