package supervise

// A phase worker that declares a wait is sent no task it had already taken while the wait stands
// (dispatch://INFRA-2040 R3, D4 of its plan). The daemon answers whether a wait holds a send
// (Deps.WaitHolds, internal/daemon's waitHolds) and ends a wait at the send of new work
// (Deps.EndWait, internal/daemon's endWait); these tests stand a fake in for those answers and pin
// what the machine does with them: a held task stays pending and unconfirmed, the line that says
// so is written once per delivery, the task goes at the first send after the wait ends, and new
// work ends the wait only as it leaves.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// heldWait is the id the fake answer names for the wait that stands.
const heldWait = "wait-of-the-test"

// waitAnswer is a fake of the daemon's two answers on a wait (Deps.WaitHolds, Deps.EndWait).
// While standing is set it names heldWait for every task, and holds a task a death took back
// (Delivery.Interrupted), the one task these tests hold; asked counts the questions. Each end
// asked is kept (waitEnd) with the prompts sent by then (sent, set by the test), and ends the
// wait. The machine asks it from its own goroutine, so it is locked.
type waitAnswer struct {
	mu       sync.Mutex
	standing bool
	asked    int
	ends     []waitEnd
	sent     func() int
}

// waitEnd is one end the machine asked of the fake: the delivery whose send ended the wait, the
// wait it named, and how many prompts the claim had been sent when it asked.
type waitEnd struct {
	delivery, wait string
	sent           int
}

// wire sets the answer as the machine's Deps.WaitHolds and Deps.EndWait.
func (a *waitAnswer) wire(d *Deps) { d.WaitHolds, d.EndWait = a.holds, a.end }

func (a *waitAnswer) holds(_ context.Context, _ Claim, d Delivery) (string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	if !a.standing {
		return "", false, nil
	}
	return heldWait, d.Interrupted, nil
}

func (a *waitAnswer) end(_ context.Context, _ Claim, d Delivery, wait string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	sent := 0
	if a.sent != nil {
		sent = a.sent()
	}
	a.ends = append(a.ends, waitEnd{delivery: d.ID, wait: wait, sent: sent})
	a.standing = false
	return nil
}

func (a *waitAnswer) stand(standing bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.standing = standing
}

func (a *waitAnswer) questions() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

func (a *waitAnswer) endsAsked() []waitEnd {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]waitEnd(nil), a.ends...)
}

// heldInADeclaringTurn is the task of entry 3 taken in a turn the worker declares its wait in, then
// taken back by a death in that turn and held from the relaunched agent: the claim stands ready,
// holding the task unconfirmed and marked interrupted, with the one prompt that started the turn
// sent.
func heldInADeclaringTurn(t *testing.T, h *harness, answer *waitAnswer) Delivery {
	t.Helper()
	h.must(RequestDeliver{Claim: h.token, Task: "test the head", Generation: 5, Entry: 3})
	h.must(StreamTurnStart{Claim: h.token})
	sent := h.wantPrompts(1)[0].DeliveryID
	answer.stand(true)
	h.observe(runtime.Gone)
	h.relaunched()
	held := h.pending()
	if held.ID == sent || !held.Interrupted || !held.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v after the death, want the task taken back under a new id, unconfirmed and interrupted", held)
	}
	h.wantPrompts(1)
	return held
}

// heldLines is every line saying that a task the claim had taken waits on heldWait, for delivery.
func heldLines(h *harness, delivery string) []string {
	return h.logs.lines("supervise: a task the claim had taken waits: the claim's wait stands", "delivery="+delivery,
		"wait="+heldWait, "issue=LEGION-209", "role=implementer")
}

// New work ends the wait it meets only as it leaves (R3's third end, D4): the end is asked just
// before the task's send, once every other hold of the send has let it through, so no end names
// a send that did not happen. A task of entry 4 reaches a claim whose wait stands, and a hold the
// wait does not decide keeps it back first; while that hold stands the task is not sent and no end
// is asked. Once the hold is over the task is sent, and the wait is ended once, naming that
// delivery, before its prompt.
//
//   - The working copy a tester's phase task adopts is in another role's turn (adopts,
//     issueInTurn), and the turn's end sends the task.
//   - The design gate's read for a merger's task fails (gateNote), and the next sweep, which reads
//     it, sends the task.
func TestNewWorkEndsAWaitOnlyAsItsTaskIsSent(t *testing.T) {
	bot := runtime.GitIdentity{Name: "legion-tester[bot]", Email: "7+legion-tester[bot]@users.noreply.github.com"}
	errUnread := errors.New("the record did not answer")
	for _, tc := range []struct {
		name    string
		claim   Claim
		phase   phase.Phase
		holding func(holds *bool) func(*Deps)
		release func(h *harness)
	}{
		{name: "the working copy another role's turn holds", claim: testerClaim(), phase: phase.Testing,
			holding: func(holds *bool) func(*Deps) {
				return func(d *Deps) {
					d.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) { return bot, nil }
					d.IssueInTurn = func(string, claim.Token) (claim.Token, bool) { return testToken, *holds }
				}
			},
			release: func(h *harness) { h.must(IssueTurnEnded{Claim: h.token}) }},
		{name: "the design gate's read that fails", claim: mergerClaim(), phase: phase.Merging,
			holding: func(holds *bool) func(*Deps) {
				return func(d *Deps) {
					d.GateNote = func(context.Context, Claim, Delivery) (GateNote, error) {
						if *holds {
							return GateNote{}, errUnread
						}
						return GateNote{}, nil
					}
				}
			},
			release: func(h *harness) { h.observe(runtime.Alive) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := &waitAnswer{standing: true}
			holds := true
			h := readyHarness(t, tc.claim, answer.wire, tc.holding(&holds))
			answer.sent = func() int { return len(h.prompts()) }

			// The hold that fails the send is the delivery's own error, which the deliver answers.
			_ = h.handle(RequestDeliver{Claim: h.token, Task: "work the new entry", Phase: tc.phase, Generation: 5, Entry: 4})
			h.wantPrompts(0)
			if ends := answer.endsAsked(); len(ends) != 0 {
				t.Fatalf("the wait was ended %+v while the task waited, want no end: no send happened", ends)
			}

			holds = false
			tc.release(h)
			sent := h.wantPrompts(1)[0]
			if ends := answer.endsAsked(); len(ends) != 1 || ends[0] != (waitEnd{delivery: sent.DeliveryID, wait: heldWait}) {
				t.Fatalf("the ends asked = %+v, want one, of %s naming the task's delivery %s before its prompt", ends, heldWait, sent.DeliveryID)
			}
		})
	}
}

// A worker that declares a wait in the turn of its task and dies in that turn gets its task again
// only once the wait ends. The death takes the task back unconfirmed and marked interrupted; while
// the wait stands, the relaunched agent's ready, a hello and every sweep of the live process send
// it nothing, and the line saying the task waits is written once however often they ask. Once the
// wait ends, the next sweep sends it, behind the sentence that its turn was interrupted.
func TestAStandingWaitHoldsTheInterruptedTaskUntilItEnds(t *testing.T) {
	answer := &waitAnswer{}
	h := readyHarness(t, queuedClaim(), answer.wire)
	held := heldInADeclaringTurn(t, h, answer)

	for range 3 {
		h.observe(runtime.Alive)
	}
	h.connect()
	h.wantPrompts(1)
	if p := h.pending(); p.ID != held.ID || !p.ConfirmedAt.IsZero() || !p.DeliveredAt.Equal(held.DeliveredAt) {
		t.Fatalf("the claim holds %+v while the wait stands, want the held task as it was, %+v", p, held)
	}
	if asked := answer.questions(); asked < 5 {
		t.Fatalf("the wait was asked %d times, want once at the ready and at each sweep and hello after it", asked)
	}
	if lines := heldLines(h, held.ID); len(lines) != 1 {
		t.Fatalf("the held task was logged %d times while the wait stood, want once; the log is:\n%s", len(lines), h.logs.String())
	}

	answer.stand(false)
	h.observe(runtime.Alive)
	resent := h.wantPrompts(2)[1]
	if want := interruptedTask(h.claim().EndedWork.Cause) + "test the head"; resent.DeliveryID != held.ID || resent.Message != want {
		t.Fatalf("once the wait ended the claim sent %+v, want %q under the held task's id %s", resent, want, held.ID)
	}
	if lines := heldLines(h, held.ID); len(lines) != 1 {
		t.Fatalf("the held task was logged %d times, want once", len(lines))
	}
}

// A daemon restarted while a wait holds a task finds that task pending and unconfirmed, and the
// question a restart arms of such a task — whether the turn the agent may be in is the task's — is
// false of it, since no send carried it while the wait stood. So the first turn of the worker's own
// after the restart, news reaching its session, is no turn of the held task: the turn neither
// confirms it nor retires it at its end, and the task is sent once the wait ends.
func TestATaskAWaitHoldsIsNotTakenForTheWorkersOwnTurnAfterARestart(t *testing.T) {
	answer := &waitAnswer{}
	h := readyHarness(t, queuedClaim(), answer.wire)
	held := heldInADeclaringTurn(t, h, answer)

	h.restart()
	h.connect()
	h.wantPrompts(1)
	h.mustQuiet(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)
	if p := h.pending(); p.ID != held.ID || !p.ConfirmedAt.IsZero() {
		t.Fatalf("the worker's own turn left the claim holding %+v, want the held task unconfirmed: the turn is not that task's", p)
	}
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	if p := h.claim().Pending; p == nil || p.ID != held.ID || !p.ConfirmedAt.IsZero() {
		t.Fatalf("the end of the worker's own turn left the claim holding %+v, want the held task still owed, %s", p, held.ID)
	}
	h.wantPrompts(1)

	answer.stand(false)
	h.observe(runtime.Alive)
	if resent := h.wantPrompts(2)[1]; resent.DeliveryID != held.ID {
		t.Fatalf("once the wait ended the claim sent %+v, want the held task %s", resent, held.ID)
	}
}

// A wait is a phase worker's: an architect declares none, so the machine never asks whether one
// holds an architect's task back, and the daemon reads no wait for it.
func TestAWaitIsNeverAskedOfAnArchitectsTask(t *testing.T) {
	answer := &waitAnswer{standing: true}
	h := readyHarness(t, rootClaim(), answer.wire)
	h.must(RequestDeliver{Claim: h.token, Task: "the tree's catch-up"})
	h.wantPrompts(1)
	if asked := answer.questions(); asked != 0 {
		t.Fatalf("the wait was asked %d times of an architect's task, want never", asked)
	}
}
