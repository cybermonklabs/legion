package supervise

// A phase worker that declares a wait is sent no task it had already taken while the wait stands
// (dispatch://INFRA-2040 R3, D4 of its plan). The daemon answers whether a wait holds a send
// (Deps.WaitHolds, internal/daemon's waitHolds); these tests stand a fake in for that answer and
// pin what the machine does with it: a held task stays pending and unconfirmed, the line that says
// so is written once per delivery, and the task goes at the first send after the wait ends.

import (
	"context"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// heldWait is the id the fake answer names for the wait that holds a task.
const heldWait = "wait-of-the-test"

// waitAnswer is a fake Deps.WaitHolds. While standing is set it names heldWait for a task a death
// took back (Delivery.Interrupted), the one task these tests hold, and nothing for any other; asked
// counts the questions. The machine asks it from its own goroutine, so it is locked.
type waitAnswer struct {
	mu       sync.Mutex
	standing bool
	asked    int
}

func (a *waitAnswer) holds(_ context.Context, _ Claim, d Delivery) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	if a.standing && d.Interrupted {
		return heldWait, nil
	}
	return "", nil
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

// waitingHarness is a machine over c whose wait answer is answer, with c brought up to ready.
func waitingHarness(t *testing.T, c Claim, answer *waitAnswer) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.token = c.Token
	h.deps.WaitHolds = answer.holds
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateReady)
	return h
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

// A worker that declares a wait in the turn of its task and dies in that turn gets its task again
// only once the wait ends. The death takes the task back unconfirmed and marked interrupted; while
// the wait stands, the relaunched agent's ready, a hello and every sweep of the live process send
// it nothing, and the line saying the task waits is written once however often they ask. Once the
// wait ends, the next sweep sends it, behind the sentence that its turn was interrupted.
func TestAStandingWaitHoldsTheInterruptedTaskUntilItEnds(t *testing.T) {
	answer := &waitAnswer{}
	h := waitingHarness(t, queuedClaim(), answer)
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
	if want := interruptedTask(h.claim().LastDeathCause) + "test the head"; resent.DeliveryID != held.ID || resent.Message != want {
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
	h := waitingHarness(t, queuedClaim(), answer)
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
	h := waitingHarness(t, rootClaim(), answer)
	h.must(RequestDeliver{Claim: h.token, Task: "the tree's catch-up"})
	h.wantPrompts(1)
	if asked := answer.questions(); asked != 0 {
		t.Fatalf("the wait was asked %d times of an architect's task, want never", asked)
	}
}
