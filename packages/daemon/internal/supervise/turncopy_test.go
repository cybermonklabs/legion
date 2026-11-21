package supervise

// The copy each turn's end owes (R1 of dispatch://INFRA-1117) as the machine meets it: every
// turn's end starts it, handing the daemon the claim whose turn ended, and a stop held for that
// end waits for it, at most the copy's budget and never past the stop's own deadline.

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// testCopyBudget is the budget the daemon's double gives each copy it starts, the production
// runningCopyBudget.
const testCopyBudget = 4 * time.Minute

// turnEndCopies is the daemon's start of a turn's end copy as a double (Deps.CopyTurnEnd): it
// records every claim it was handed and the end it is to hand back, and starts each copy under
// testCopyBudget. It hands nothing back of its own accord: a test hands an end back to the
// machine itself (copied), as the daemon does once the copy is over, or never, as a copy that
// never answers does.
type turnEndCopies struct {
	mu     sync.Mutex
	asked  []Claim
	copies []TurnEndCopied
}

func (c *turnEndCopies) start(asked Claim, copied TurnEndCopied) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, copyClaim(asked))
	c.copies = append(c.copies, copied)
	return testCopyBudget, true
}

// started is every claim a copy was started for, in order.
func (c *turnEndCopies) started() []Claim {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked)
}

// copied is the end of the i-th copy started, which the daemon hands the machine once that copy
// is over.
func (c *turnEndCopies) copied(t *testing.T, i int) TurnEndCopied {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if i >= len(c.copies) {
		t.Fatalf("copy %d was never started; %d were", i, len(c.copies))
	}
	return c.copies[i]
}

// copyingHarness is a machine over the queued claim whose daemon starts a copy at each turn's
// end (turnEndCopies).
func copyingHarness(t *testing.T) (*harness, *turnEndCopies) {
	t.Helper()
	copies := &turnEndCopies{}
	h := newBareHarness(t)
	h.deps.CopyTurnEnd = copies.start
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, copies
}

// A stop held for a turn's end waits for the copy that end started before it stops the process:
// the copy asks the shim of the role whose turn ended, so a stop made first would take the process
// the copy is asking, and the turn's end would be in no copy once the role is stopped (R1 of
// dispatch://INFRA-1117). The end hands the daemon the claim as it stands at the end, its role
// and its process among it, and the claim waits idle meanwhile, holding the stop. The copy's end
// runs the stop, once: an end of the same copy handed back again changes nothing.
func TestAStopHeldForATurnsEndWaitsForTheCopyThatEndStarted(t *testing.T) {
	h, copies := copyingHarness(t)
	h.reach(StateWorking)
	loc := h.locator()
	if err := h.handle(RequestSuspend{Claim: h.token, Reason: "LEGION-209 left implementing", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}

	h.must(StreamTurnEnd{Claim: h.token})

	started := copies.started()
	if len(started) != 1 {
		t.Fatalf("the turn's end started %d copies, want one", len(started))
	}
	if asked := started[0]; asked.Token != h.token || asked.Role != claim.RoleImplementer || asked.Locator == nil || *asked.Locator != loc {
		t.Fatalf("the copy was handed %s (%s) on %+v, want the claim whose turn ended on its process %+v", asked.Token, asked.Role, asked.Locator, loc)
	}
	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)
	if c := h.claim(); !c.SuspensionHeld || c.SuspensionHeldBy != SuspendedByDaemon || c.Locator == nil || *c.Locator != loc {
		t.Fatalf("the claim waiting for its copy is %+v, want its process kept and the daemon's stop held", c)
	}

	h.must(copies.copied(t, 0))

	h.wantState(StateSuspended)
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Fatalf("suspended %+v, want the process whose turn ended, %+v", suspend.Locator, loc)
	}
	h.must(copies.copied(t, 0))
	h.wantCalls("Suspend", 1)
	h.wantState(StateSuspended)
}

// A copy that never ends does not hold the stop for ever: the held stop runs once the copy's own
// budget has passed since the turn's end, as a copy abandoned at its budget would let it. The
// copy's end, if it comes after all, finds the stop already made and changes nothing.
//
// The control is the wait with no bound: the stop then waits for the copy alone, and the claim
// is still idle, its process running, past the budget.
func TestAStopHeldForATurnsEndGoesAtTheCopysBudgetWhenTheCopyNeverEnds(t *testing.T) {
	h, copies := copyingHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: h.token, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}
	h.advance(time.Minute)
	h.must(StreamTurnEnd{Claim: h.token})

	h.advance(testCopyBudget - time.Second)
	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)

	h.advance(time.Second)
	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
	if lines := h.logs.lines("did not end within its budget"); len(lines) != 1 {
		t.Fatalf("the stop at the copy's budget was logged %q, want one line saying the copy did not end within it", lines)
	}

	h.must(copies.copied(t, 0))
	h.wantCalls("Suspend", 1)
}

// The wait never makes a stop late: an operator's suspension is bounded at the stop timeout from
// its arrival, and a turn that ends inside that timeout leaves the stop waiting for its copy only
// for what is left of it, though the copy's budget runs far longer.
func TestAStopHeldForATurnsEndWaitsForItsCopyNoLongerThanTheStopsOwnDeadline(t *testing.T) {
	h, _ := copyingHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn returned %v, want ErrSuspendHeld", err)
	}
	h.advance(testStop / 2)
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)

	h.advance(testStop/2 - time.Second)
	h.wantCalls("Suspend", 0)
	h.advance(time.Second)
	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
	if c := h.claim(); c.SuspendedBy != SuspendedByOperator {
		t.Fatalf("the claim records %q as who suspended it, want the operator", c.SuspendedBy)
	}
}

// A pause's stop that waits for the result of the turn's prompt (stopWaitsForResult) waits for
// the turn's copy as well: the copy started at the turn's end, and the result that comes right
// behind that end, a provider's refusal here, is no stop of the copy's. The pause's stop row,
// asked again while the stop waits, is held with it rather than stopping the process at once. The
// copy's end runs the one stop, through the pause's stop transaction, and the role keeps the task
// the provider refused.
func TestAPausesStopAtThePromptsResultWaitsForTheTurnsCopy(t *testing.T) {
	copies := &turnEndCopies{}
	h, flag, stops := pauseHarness(t, queuedClaim(), func(d *Deps) { d.CopyTurnEnd = copies.start })
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: providerErrorTaskText})
	h.must(StreamTurnStart{Claim: h.token})
	task := h.pending().ID
	flag.set(7)
	if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the pause's stop of a claim in a turn = %v, want it held", err)
	}

	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	if started := copies.started(); len(started) != 1 || started[0].Token != h.token {
		t.Fatalf("the turn's end started copies for %+v, want one for the claim whose turn ended", started)
	}
	h.must(StreamPromptResult{Claim: h.token, DeliveryID: task, AgentInvoked: true, Error: "overloaded_error", HTTPStatus: 529})
	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)
	stops.wantStops(t)
	if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the pause's stop asked again while the stop waits for the copy = %v, want it held", err)
	}
	h.wantCalls("Suspend", 0)

	h.must(copies.copied(t, 0))

	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
	stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
	if p := h.claim().Pending; p == nil || p.Task != providerErrorTaskText {
		t.Fatalf("the stopped claim holds %+v, want the task the provider refused", p)
	}
}

// No turn waits for a copy (R2 of dispatch://INFRA-1117): a turn that ends with no stop held for
// it starts its copy and goes idle, and the next task is sent while the copy is still being taken.
func TestATurnsEndWithNoStopHeldSendsTheNextTaskWhileItsCopyRuns(t *testing.T) {
	h, copies := copyingHarness(t)
	h.reach(StateIdle)
	if started := copies.started(); len(started) != 1 {
		t.Fatalf("the turn's end started %d copies, want one", len(started))
	}

	h.must(RequestDeliver{Claim: h.token, Task: "the next task"})

	if prompts := h.wantPrompts(2); prompts[1].Message != "the next task" {
		t.Fatalf("the second prompt is %q, want the next task sent while the copy runs", prompts[1].Message)
	}
	h.wantCalls("Suspend", 0)
}
