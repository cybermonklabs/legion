package supervise

// An update round is the daemon asking a claim's role to push its work and answer
// ready_for_update before the process holding that claim is replaced. These tests pin what the
// machine does with the round's events: a round that opens once, records its terms once and asks
// nothing of the machine's own — the sweep publishes the ask to the role's Envoy topic and marks
// it on the claim (PodUpdateAsked) — a suspended claim recorded in the round and not revived for
// it, a worker answering by the end of the turn an older daemon's ask started, at every plugin
// contract, the next task held back from the moment the worker answers and confirmed by no turn
// after a daemon restart, and the round's end — which releases that task, relaunches nothing,
// retires an older daemon's ask nothing read, and tells the worker of a pod round nothing of the
// daemon's own, since the pod that round was opened over is gone and the launch into its
// replacement is what starts that worker. The round the answer names is here too: an answer for
// the claim's own round counts, one for any other round is refused, and a round that replaces
// another under a turn in flight — a late turn, a restart's turn, a turn the daemon sent nothing
// for — switches all the same. The stale-plugin lane is here as well: a stale claim, the
// controller's among them, and a stale registration are each relaunched at once, whatever
// contract the claim records, since that lane asks its worker nothing. The last group is the
// claim the daemon started again in another pod while its pod round ran: the launch writes which
// round the claim left, and the answer it had given by then, if any, is the one it gave in the
// round's own pod — nothing of the new process writes one. Its part in that round is frozen
// there, the ask a restart left it still holding is dropped with every mark armed for it, a
// suspension afterwards changes none of it, only the round's end clears it, and the control
// beside it is a claim still in the round's own pod, which takes its full part.
//
// Each test drives the machine through Handle, as the daemon's sweep and the API do; no action is
// called directly.

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

const (
	// testRoundPod is the uid of the pod testRound is the round of, and testOtherPod the uid of
	// the pod a claim is relaunched into once that one has died. A pod round names the pod every
	// claim of it runs in, so testRound is written from testRoundPod rather than beside it.
	testRoundPod = "9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22"
	testOtherPod = "4b8e1d60-3c92-4f7a-ae15-8d0c2b5f6a34"
	// testRound is a pod round's identity, as the daemon's sweep writes it.
	testRound = PodRoundPrefix + testRoundPod
	// testReasons is what the daemon found, as Claim.UpdateReasons holds it.
	testReasons = "image"
)

// updateHarness is a machine whose daemon speaks contract. The register() walk records that same
// number as the claim's own plugin contract, so contract decides whether this claim can answer an
// update round: PodUpdateContract and above can, anything below cannot.
func updateHarness(t *testing.T, contract int) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.PluginContract = contract
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

// openUpdate is the daemon's sweep posting the round over this claim, as it does at every tick
// while the round is open.
func (h *harness) openUpdate(round, reasons string) {
	h.t.Helper()
	h.must(PodUpdateOpen{Claim: h.token, Round: round, Reasons: reasons})
}

// leftAsk is an ask of round that an older daemon queued for this claim as a task, with the
// clock of that send: a delivery under an id of the round's own (updateTaskID), handed to the
// worker as that daemon handed it, and UpdateTaskedAt stamped as the sweep's mark stamps it
// (PodUpdateAsked). This daemon queues no such task — the round's ask is a message on the role's
// Envoy topic — but a claim restored from an older daemon's record can hold one, and what reads it
// (answerByTurnEnd, takeBackSignal, dropUpdateTask, rotateTaskID) still runs over it. The claim
// is in round already, ready or idle.
func (h *harness) leftAsk(round string) {
	h.t.Helper()
	h.must(RequestDeliver{Claim: h.token, ID: updateTaskID(round), Task: UpdateTask(round, testReasons, epoch.Add(time.Hour))})
	if asked, err := h.m.PodUpdateAsked(h.ctx, round); err != nil || !asked {
		h.t.Fatalf("mark the older daemon's ask of %s = %t, %v; want it marked", round, asked, err)
	}
}

// asksRound is whether a delivery id is an ask of round. Each ask of a round has an id of its own
// (updateTaskID), so a test names the round the id carries rather than the whole id.
func asksRound(id, round string) bool { return updatesRound(&Delivery{ID: id}, round) }

// signalUpdate is the worker answering ready_for_update through the API, for the round its claim
// stands in — the round the task the worker read named, which is the answer every worker that
// reads its newest task gives.
func (h *harness) signalUpdate(head string) {
	h.t.Helper()
	h.signalUpdateFor(h.claim().UpdateRound, head)
}

// signalUpdateFor is that same answer naming a round of the test's choosing: the round a worker
// read before a later one replaced it, or one no claim of this daemon is in.
func (h *harness) signalUpdateFor(round, head string) {
	h.t.Helper()
	h.must(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: round, Head: head})
}

func (h *harness) wantRound(want string) Claim {
	h.t.Helper()
	c := h.claim()
	if c.UpdateRound != want {
		h.t.Fatalf("UpdateRound = %q, want %q", c.UpdateRound, want)
	}
	return c
}

// podProcess is a process of the test's claim in the pod with this uid, as the Sandbox runtime
// hands one back: the claim's locator is the one record of which pod its worker runs in, and
// PodRound of that uid is the round of that pod.
func podProcess(pod string, generation uint64) runtime.Locator {
	return runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: testToken, Incarnation: runtime.SandboxIncarnation(pod, generation),
		Sandbox: &runtime.SandboxLocator{
			Namespace: "legion", Name: "legion-legion-209", PodUID: pod,
			Container: runtime.IssuePodContainer, Generation: generation,
		},
	}
}

// inPod brings the harness's claim up to ready in the pod with this uid, so the round the test
// opens over it is its own pod's.
func (h *harness) inPod(pod string) {
	h.t.Helper()
	h.rt.ScriptSpawn(fake.SpawnResult{Locator: podProcess(pod, 1)})
	h.reach(StateReady)
}

// diesIntoPod is the claim's pod dying under it and the relaunch coming up in another: the
// runtime reports the process gone, the machine relaunches it on its recorded session, and the
// Resume answers with a process in the pod named. The machine moves its own locator, as it does
// in the cluster — nothing here writes the claim behind the machine's back — and the walk back
// to ready is the one every relaunch takes.
func (h *harness) diesIntoPod(pod string) {
	h.t.Helper()
	h.rt.ScriptResume(fake.SpawnResult{Locator: podProcess(pod, 1)})
	h.observe(runtime.Gone)
	h.relaunched()
	if got := h.locator().Sandbox.PodUID; got != pod {
		h.t.Fatalf("the relaunched claim runs in pod %q, want %q", got, pod)
	}
}

// A round opened over a ready claim records itself — its id, its reasons and its own clock — and
// persists, and the machine gives the worker nothing: no task is queued and no prompt is sent. The
// round's ask is the sweep's, published to the role's Envoy topic once the claim records the
// round, and the publish mark is unset until the sweep makes it (PodUpdateAsked).
func TestPodUpdateOpensARoundAndSendsTheWorkerNothing(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)

	h.openUpdate(testRound, testReasons)

	h.wantState(StateReady)
	c := h.wantRound(testRound)
	if c.UpdateReasons != testReasons {
		t.Errorf("UpdateReasons = %q, want %q", c.UpdateReasons, testReasons)
	}
	if c.UpdateOpenedAt == nil || !c.UpdateOpenedAt.Equal(epoch) {
		t.Errorf("UpdateOpenedAt = %v, want the round's own clock at %v", c.UpdateOpenedAt, epoch)
	}
	if c.UpdateTaskedAt != nil {
		t.Errorf("UpdateTaskedAt = %v, want nil: the sweep has published no ask yet", c.UpdateTaskedAt)
	}
	if c.Pending != nil {
		t.Errorf("pending = %+v, want no task: the round's ask is a message on the role's topic", c.Pending)
	}
	h.wantPrompts(0)
	if stored := h.store.load(h.token); stored.UpdateRound != testRound || stored.UpdateReasons != testReasons {
		t.Errorf("the round was not persisted: stored claim %+v", stored)
	}
}

// The round's terms are fixed when it opens (dispatch://INFRA-2981 R9). The sweep posts the same
// round at every tick, and the drift it reads can move while the round runs, but a later post
// writes none of it: the reasons and the clock stay the first post's, the claim is written no
// more times, and the worker is sent nothing. So the ask the sweep publishes from the claim's
// record and the record an operator reads name one thing for one round id.
func TestPodUpdateALaterOpenOfTheSameRoundKeepsTheTermsItOpenedWith(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	settled := len(h.store.history())

	h.advance(time.Minute)
	h.openUpdate(testRound, testReasons+", resources")
	h.openUpdate(testRound, testReasons)

	c := h.wantRound(testRound)
	if c.UpdateReasons != testReasons {
		t.Errorf("UpdateReasons = %q, want the %q the round opened with", c.UpdateReasons, testReasons)
	}
	if c.UpdateOpenedAt == nil || !c.UpdateOpenedAt.Equal(epoch) {
		t.Errorf("UpdateOpenedAt = %v, want the first post's %v", c.UpdateOpenedAt, epoch)
	}
	if got := len(h.store.history()); got != settled {
		t.Errorf("two later posts of the open round wrote the claim %d times, want none", got-settled)
	}
	if stored := h.store.load(h.token); stored.UpdateReasons != testReasons {
		t.Errorf("stored reasons = %q, want the %q the round opened with", stored.UpdateReasons, testReasons)
	}
	h.wantPrompts(0)
}

// A claim working a phase task keeps it: the round records itself, and the task in the worker's
// hands is neither displaced nor joined by a second one.
func TestPodUpdateLeavesTheTaskAWorkingClaimHolds(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: h.token})
	own := h.wantPrompts(1)[0]

	h.openUpdate(testRound, testReasons)
	h.must(StreamTurnEnd{Claim: h.token})
	h.openUpdate(testRound, testReasons)

	h.wantRound(testRound)
	h.wantPrompts(1)
	if p := h.claim().Pending; p != nil && p.ID != own.DeliveryID {
		t.Fatalf("pending = %+v, want nothing but the worker's own task %q", p, own.DeliveryID)
	}
}

// A suspended claim is recorded in the round and is not revived for it, whoever suspended it: the
// ask is a message on the role's topic, a topic no live session holds is asked nothing, and the
// round ends at its bound whether or not the role answers (dispatch://INFRA-2981 R1).
func TestPodUpdateRecordsTheRoundOnASuspendedClaimAndRevivesNothing(t *testing.T) {
	for _, by := range []SuspendSource{SuspendedByDaemon, SuspendedByOperator} {
		t.Run(string(by), func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.reach(StateIdle)
			h.must(RequestSuspend{Claim: h.token, By: by})
			h.wantState(StateSuspended)
			sent := len(h.prompts())

			h.openUpdate(testRound, testReasons)

			h.wantState(StateSuspended)
			h.wantCalls("Resume", 0)
			h.wantPrompts(sent)
			if c := h.wantRound(testRound); c.Pending != nil || c.UpdateTaskedAt != nil {
				t.Errorf("the suspended claim = %+v, want the round recorded and no task queued for it", c)
			}
		})
	}
}

// The sweep's publish mark (PodUpdateAsked) is what makes one ask per claim per round. It stamps
// the claim of the round it names once, at the sweep's clock, and durably; it refuses a round the
// claim is not in and a round already marked, so a later tick asks nothing more; and a write the
// store refuses takes the stamp back, so the next tick publishes again rather than lose the ask.
func TestPodUpdateAskedMarksTheRoundsAskOnce(t *testing.T) {
	t.Run("once, for the claim's own round", func(t *testing.T) {
		h := updateHarness(t, PodUpdateContract)
		h.reach(StateReady)
		h.openUpdate(testRound, testReasons)

		if asked, err := h.m.PodUpdateAsked(h.ctx, PodRound(testOtherPod)); err != nil || asked {
			t.Fatalf("the mark of a round the claim is not in = %t, %v; want it refused", asked, err)
		}
		if c := h.claim(); c.UpdateTaskedAt != nil {
			t.Fatalf("UpdateTaskedAt = %v after the refused mark, want nil", c.UpdateTaskedAt)
		}

		h.advance(time.Minute)
		if asked, err := h.m.PodUpdateAsked(h.ctx, testRound); err != nil || !asked {
			t.Fatalf("the mark of the claim's own round = %t, %v; want it made", asked, err)
		}
		at := h.clock.Now()
		if c := h.claim(); c.UpdateTaskedAt == nil || !c.UpdateTaskedAt.Equal(at) {
			t.Fatalf("UpdateTaskedAt = %v, want the mark's clock at %v", c.UpdateTaskedAt, at)
		}
		if stored := h.store.load(h.token); stored.UpdateTaskedAt == nil || !stored.UpdateTaskedAt.Equal(at) {
			t.Fatalf("stored UpdateTaskedAt = %v, want the mark at %v: a restarted daemon reads it", stored.UpdateTaskedAt, at)
		}

		h.advance(time.Minute)
		if asked, err := h.m.PodUpdateAsked(h.ctx, testRound); err != nil || asked {
			t.Fatalf("a second mark of the round = %t, %v; want it refused", asked, err)
		}
		if c := h.claim(); !c.UpdateTaskedAt.Equal(at) {
			t.Errorf("UpdateTaskedAt = %v after the second mark, want the first mark's %v", c.UpdateTaskedAt, at)
		}
		h.wantPrompts(0)
	})

	t.Run("a write the store refuses takes the stamp back", func(t *testing.T) {
		h := updateHarness(t, PodUpdateContract)
		h.reach(StateReady)
		h.openUpdate(testRound, testReasons)

		h.store.fail("PutClaim", errBoom)
		if asked, err := h.m.PodUpdateAsked(h.ctx, testRound); !errors.Is(err, errBoom) || asked {
			t.Fatalf("the mark over a failing write = %t, %v; want it refused with the store's error", asked, err)
		}
		h.store.fail("PutClaim", nil)
		if c := h.claim(); c.UpdateTaskedAt != nil {
			t.Fatalf("UpdateTaskedAt = %v after the failed write, want the stamp taken back", c.UpdateTaskedAt)
		}

		if asked, err := h.m.PodUpdateAsked(h.ctx, testRound); err != nil || !asked {
			t.Fatalf("the next tick's mark = %t, %v; want it made", asked, err)
		}
	})
}

// A claim whose registration declared a contract below PodUpdateContract has no ready_for_update
// op to answer with. The end of the turn an older daemon's queued ask starts is its answer,
// recorded with no head, so the round completes and the pod is replaced like any other.
//
// The claim is at exactly one contract below this daemon's, which is the worker of the release
// before this one: the plugin main has released, and the plugin every live worker runs at the
// first boot of this release. That is the case this path exists for, and the one a bound rather
// than a pin on PodUpdateContract let through — with the constant left at 45 while the op moved
// to 46, such a worker read as able to answer an op its plugin does not have, and every round
// of the first roll stalled. api's TestPodUpdateContractIsTheContractThatIntroducedTheOp pins
// the constant to the contract that introduced the op, so PodUpdateContract-1 here cannot name
// anything but the released plugin.
func TestPodUpdateTakesTheTurnsEndAsTheAnswerOfAClaimBelowTheContract(t *testing.T) {
	h := updateHarness(t, PodUpdateContract-1)
	h.reach(StateReady)
	h.m.deps.PluginContract = PodUpdateContract

	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)

	h.wantPrompts(1)
	if c := h.wantRound(testRound); c.UpdateSignalledAt != nil {
		t.Errorf("UpdateSignalledAt = %v before any turn ran, want nil", c.UpdateSignalledAt)
	}

	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})

	c := h.claim()
	if c.UpdateSignalledAt == nil {
		t.Fatal("the end of the update task's turn recorded no answer for a claim that cannot call the op")
	}
	if c.UpdateHead != "" {
		t.Errorf("UpdateHead = %q, want none: a turn's end reports no head", c.UpdateHead)
	}
	if stored := h.store.load(h.token); stored.UpdateSignalledAt == nil {
		t.Error("the answer the turn's end gave was not persisted")
	}
}

// The end of the turn an update task started is that worker's answer at every plugin contract,
// and the claim records that the answer came that way. A `legion` tool built with no
// ready_for_update operation is one the daemon cannot tell from a tool that has it: the contract
// a registration declares says which daemon the plugin was built against, never which operations
// the closed enum compiled into it holds. Read as able to call the operation, such a worker at
// this daemon's own contract recorded no answer at all when its task's turn ended, its round
// stayed open for hours, and its own phase-stall reminder started a turn every fifteen minutes
// of them — which is the measured failure this pins.
//
// The mark is what carries the rule past the answer, and it is in the store because the turn it
// decides can come after a restart: a turn the daemon sent nothing for keeps an answer a turn's
// end gave, and takes back one the worker made the call for (takeBackSignal).
func TestAnswerByTurnEndEveryContract(t *testing.T) {
	// asked brings a claim at contract up in the round's own pod and opens the round over it,
	// with this daemon one contract ahead of the oldest plugin a test here uses.
	asked := func(t *testing.T, contract int) *harness {
		t.Helper()
		h := updateHarness(t, contract)
		h.inPod(testRoundPod)
		h.m.deps.PluginContract = PodUpdateContract
		h.openUpdate(testRound, testReasons)
		h.leftAsk(testRound)
		h.wantPrompts(1)
		return h
	}
	// wantAnswered holds the claim to an answer the turn's end gave: recorded, marked as that
	// kind of answer, with no head, and in the store, which is what a later daemon reads.
	wantAnswered := func(t *testing.T, h *harness) time.Time {
		t.Helper()
		c := h.claim()
		if c.UpdateSignalledAt == nil {
			t.Fatal("the end of the update task's turn recorded no answer")
		}
		if !c.UpdateAnsweredByTurnEnd {
			t.Error("UpdateAnsweredByTurnEnd is false; this answer is the end of the task's own turn")
		}
		if c.UpdateHead != "" {
			t.Errorf("UpdateHead = %q, want none: a turn's end reports no head", c.UpdateHead)
		}
		if stored := h.store.load(h.token); stored.UpdateSignalledAt == nil || !stored.UpdateAnsweredByTurnEnd {
			t.Errorf("stored claim = %+v, want the answer and its mark written; a restart reads them", stored)
		}
		return *c.UpdateSignalledAt
	}

	for _, contract := range []int{PodUpdateContract, PodUpdateContract - 1} {
		t.Run("the ask's turn ends at contract "+strconv.Itoa(contract), func(t *testing.T) {
			h := asked(t, contract)

			h.must(StreamTurnStart{Claim: h.token})
			h.must(StreamTurnEnd{Claim: h.token})

			wantAnswered(t, h)
		})
	}

	t.Run("the reminder turns of an open round leave that answer standing", func(t *testing.T) {
		h := asked(t, PodUpdateContract)
		h.must(StreamTurnStart{Claim: h.token})
		h.must(StreamTurnEnd{Claim: h.token})
		answered := wantAnswered(t, h)

		// The worker's own plugin reminds it of the WAITING line it left, a turn at a time, for
		// as long as the round waits: the daemon sent none of them and cannot tell one from an
		// Envoy notice. On the first roll of this contract they arrived every fifteen minutes.
		for reminder := 1; reminder <= 3; reminder++ {
			h.advance(15 * time.Minute)
			h.must(StreamTurnStart{Claim: h.token})
			h.openUpdate(testRound, testReasons)
			h.must(StreamTurnEnd{Claim: h.token})

			c := h.wantRound(testRound)
			if c.UpdateSignalledAt == nil || !c.UpdateSignalledAt.Equal(answered) {
				t.Fatalf("UpdateSignalledAt after reminder turn %d = %v, want the answer of %v standing",
					reminder, c.UpdateSignalledAt, answered)
			}
			if !c.UpdateAnsweredByTurnEnd {
				t.Fatalf("the mark of how the answer was given went with reminder turn %d", reminder)
			}
			h.wantPrompts(1)
		}
	})

	t.Run("a turn something else started is no answer", func(t *testing.T) {
		h := asked(t, PodUpdateContract)

		h.must(StreamTurnStart{Claim: h.token, DeliveryID: "a-delivery-of-the-worker-s-own"})
		h.must(StreamTurnEnd{Claim: h.token})

		if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateAnsweredByTurnEnd {
			t.Errorf("claim after a turn of the worker's own = %+v, want no answer: that turn read none of the round's words", c)
		}
	})

	t.Run("a turn this daemon did not see start is no answer", func(t *testing.T) {
		h := asked(t, PodUpdateContract)

		h.restart()
		h.must(StreamTurnStart{Claim: h.token})
		h.must(StreamTurnEnd{Claim: h.token})

		c := h.claim()
		if c.UpdateSignalledAt != nil || c.UpdateAnsweredByTurnEnd {
			t.Errorf("claim after a turn restored from an earlier daemon's record = %+v, want no answer", c)
		}
		// The refusal takes the ask's publish mark back, so the sweep's next tick publishes the
		// round's ask to the role again. The machine queues no task for it.
		if c.UpdateTaskedAt != nil {
			t.Errorf("UpdateTaskedAt = %v after the refusal, want nil: the sweep asks again", c.UpdateTaskedAt)
		}
		if p := c.Pending; p != nil {
			t.Errorf("pending after the refusal = %+v, want nothing: the turn retired the ask and the machine asks nothing", p)
		}
		h.wantPrompts(1)
	})

	t.Run("an answer the call gave is still taken back by a turn the daemon sent nothing for", func(t *testing.T) {
		h := asked(t, PodUpdateContract)
		h.must(StreamTurnStart{Claim: h.token})
		h.signalUpdate("c0ffee")
		h.must(StreamTurnEnd{Claim: h.token})
		if c := h.claim(); c.UpdateSignalledAt == nil || c.UpdateAnsweredByTurnEnd {
			t.Fatalf("claim after the call = %+v, want its answer recorded and marked as the call's", c)
		}

		h.must(StreamTurnStart{Claim: h.token})

		if c := h.claim(); c.UpdateSignalledAt != nil {
			t.Errorf("UpdateSignalledAt = %v, want the called answer taken back: the worker's own plugin holds its reminder while such an answer stands", c.UpdateSignalledAt)
		}
	})
}

// A turn that read none of the round's words answers nothing, whatever the claim's contract. An
// Envoy delivery or a person's steer starts such a turn, and it says nothing about the branch.
func TestPodUpdateTakesNoForeignTurnsEndAsTheAnswerBelowTheContract(t *testing.T) {
	h := updateHarness(t, PodUpdateContract-1)
	h.reach(StateReady)

	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)
	h.must(StreamTurnStart{Claim: h.token, DeliveryID: "a-turn-the-daemon-sent-nothing-for"})
	h.must(StreamTurnEnd{Claim: h.token})

	if c := h.claim(); c.UpdateSignalledAt != nil {
		t.Errorf("UpdateSignalledAt = %v, want nil: this turn never read the round's task", c.UpdateSignalledAt)
	}
}

// Once the worker has answered, the next task is held rather than sent: it would be lost with the
// process (answeredOpenRound, dispatch://INFRA-2981 R5). The round's end releases it.
func TestPodUpdateHoldsTheNextTaskUntilTheRoundEnds(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	h.signalUpdate("c0ffee")

	h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})

	h.wantPrompts(0)
	h.wantState(StateReady)
	if got := h.pending().Task; got != "review the pull request" {
		t.Fatalf("pending task = %q, want the held next task", got)
	}

	h.must(PodUpdateEnded{Claim: h.token, Round: h.claim().UpdateRound})

	released := h.wantPrompts(1)[0]
	if released.Message != "review the pull request" {
		t.Errorf("the task after the round = %q, want the held one", released.Message)
	}
	c := h.wantRound("")
	if c.UpdateReasons != "" || c.UpdateOpenedAt != nil || c.UpdateTaskedAt != nil ||
		c.UpdateSignalledAt != nil || c.UpdateHead != "" || c.UpdateToldAt != nil {
		t.Errorf("claim after the round ended = %+v, want every update field cleared", c)
	}
	h.wantCalls("Resume", 0)
}

// A round that replaces one the worker answered lifts the hold that answer put on the claim's next
// task (answeredOpenRound), and the switch sends that task: the claim is ready, nothing else holds
// the task back, and nothing else would send it before the claim's next observation. The new
// round's ask is the sweep's, on the role's topic, so the task is the one prompt.
func TestPodUpdateANewRoundSendsTheTaskTheAnsweredRoundHeld(t *testing.T) {
	const task = "review the pull request"
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	h.signalUpdate("c0ffee")
	h.must(RequestDeliver{Claim: h.token, Task: task})
	h.wantPrompts(0) // held: this worker has answered and waits to be replaced

	h.openUpdate(PodRound(testOtherPod), testReasons)

	c := h.wantRound(PodRound(testOtherPod))
	if c.UpdateSignalledAt != nil {
		t.Fatalf("UpdateSignalledAt = %v in the new round, want the old round's answer gone with it", c.UpdateSignalledAt)
	}
	if sent := h.wantPrompts(1)[0]; sent.Message != task {
		t.Errorf("the prompt after the switch = %q, want the task the old round's answer held, %q", sent.Message, task)
	}
}

// A task the open round holds is no turn's task, after a daemon restart too. The restored machine
// marks a pending, unconfirmed task askFirst — an earlier daemon may already have sent it, so the
// agent is asked before it is sent again — and the first turn of any kind is then taken as that
// task's turn: confirmed, its run served, and retired by that turn's end. Nobody ever sent a task
// the round holds, so the mark would destroy it and the worker would never read it at all.
//
// The notice's turn takes the worker's answer back on its way in (takeBackSignal), which is what
// lifts the hold and lets the task go, and the sweep publishes the round's ask to the worker
// again. What this pins is that the task is still there to be given.
func TestPodUpdateARestartLeavesTheHeldTaskToNoTurnButItsOwn(t *testing.T) {
	const task = "review the pull request"
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	h.signalUpdate("c0ffee")
	h.must(RequestDeliver{Claim: h.token, Task: task, Generation: 7})
	h.wantPrompts(0) // held: this worker has answered and waits to be replaced

	h.restart()

	// An Envoy notice reaches the worker: a turn the daemon sent nothing for, whose start names no
	// delivery, as no agent_start of a live turn does.
	h.must(StreamTurnStart{Claim: h.token})

	p := h.claim().Pending
	if p == nil || p.Task != task {
		t.Fatalf("pending during the notice's turn = %+v, want the held task %q", p, task)
	}
	if !p.ConfirmedAt.IsZero() {
		t.Fatalf("the notice's turn was taken as the held task's: %+v; no daemon ever sent that task", p)
	}
	// The turn took the worker's answer back on its way in, so the round holds the task no longer
	// (openRoundHoldsTask) and the task is the only work this claim can be on: the run it carries
	// is what a completion from this turn answers for, which is INFRA-264's case. The task itself
	// is still unsent and unconfirmed, as the assertions above and below hold.
	if run := h.claim().ServingRun(); run != 7 {
		t.Fatalf("during the notice's turn the claim serves run %d, want the held task's 7: the turn took its answer back, so the round holds that task no longer", run)
	}

	h.must(StreamTurnEnd{Claim: h.token})

	p = h.claim().Pending
	if p == nil || p.Task != task {
		t.Fatalf("pending after the notice's turn = %+v, want the held task %q, which that turn never had", p, task)
	}
	if !p.ConfirmedAt.IsZero() {
		t.Fatalf("the held task is confirmed after a turn that was not its own: %+v", p)
	}

	h.must(PodUpdateEnded{Claim: h.token, Round: h.claim().UpdateRound})

	h.wantRound("")
	sent := h.prompts()
	if len(sent) != 1 || sent[0].Message != task {
		t.Fatalf("prompts = %+v, want the held task, once", sent)
	}
}

// The ask says when the pod goes and what that costs. It keeps the round's id, the drift and the
// push instruction, names the moment the round ends in RFC 3339 UTC, says that every answer
// brings that moment forward and that no answer delays it, and names what goes with the pod
// then. It says what the push check saves, the rescue branch it saves to, and the pod whose shim
// cannot run the check. A role whose tool has no ready_for_update is told to push, because the
// round ends at that moment whatever it does: the end of its turn is no answer.
func TestUpdateTaskNamesTheMomentThePodGoesAndWhatGoesWithIt(t *testing.T) {
	endsAt := time.Date(2026, 11, 13, 14, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	text := UpdateTask(testRound, `image "sha256:1"`, endsAt)
	for _, want := range []string{
		"The round is " + testRound + ".",
		`The reasons are: image "sha256:1".`,
		"Push every commit that you want to keep to this issue's branch.",
		`Call the ` + "`legion`" + ` tool with op: "ready_for_update" and round: "` + testRound + `".`,
		"The round ends at 2026-11-13T12:30:00Z (UTC).",
		"If every role answers, the round can end before that moment.",
		"If a role does not answer, the round still ends at 2026-11-13T12:30:00Z.",
		"A turn in progress does not delay that moment.",
		"The round ends at 2026-11-13T12:30:00Z whatever you do.",
		"- The turn that is in progress.",
		"- The background jobs of that turn.",
		"- Each file that .gitignore ignores.",
		"- The pod's test database.",
		"- The pod's caches.",
		"the daemon runs a push check on each workspace of the pod.",
		"- Each commit that the workspace holds and the remote does not have.",
		"- One commit of the workspace's files as they are at that moment. This commit does not hold the files that .gitignore ignores.",
		"The push check pushes them to a rescue branch.",
		`The name of a rescue branch is this issue's branch name, then "-rescue-", then the first 8 characters of the pod's ID.`,
		"The daemon does not run the push check on a pod whose shim is older than the push check. On such a pod, work that you do not push is lost.",
		"The daemon holds your next task until your process runs again.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the ask does not say %q:\n%s", want, text)
		}
	}
	for _, gone := range []string{"reads the end of this turn as your answer", "WAITING"} {
		if strings.Contains(text, gone) {
			t.Errorf("the ask still says %q, which no answer of this round rests on:\n%s", gone, text)
		}
	}
}

// A rescue branch is named for the pod rather than for a session, so it can hold another
// session's commits. The ask tells a role to read a branch's commits before it takes any, by
// subject and Omp-Session trailer, and names the daemon's own commit, whose files are read
// rather than its authorship.
func TestUpdateTaskWarnsToReadARescueBranchsCommitsFirst(t *testing.T) {
	text := UpdateTask(testRound, testReasons, epoch)
	for _, want := range []string{
		"A rescue branch has the name of the pod, not of a session.",
		"A rescue branch can hold commits of another session.",
		"Before you take a commit from a rescue branch, read each commit on it.",
		"Read the subject of each commit.",
		"For a commit that an agent made, read its Omp-Session trailer.",
		`Its subject starts with "Edits left in the workspace".`,
		"Read its files to know whose work it holds.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the ask does not say %q:\n%s", want, text)
		}
	}
}

// A stale plugin is relaunched at once, whatever contract the claim records: the lane asks its
// worker nothing, so a plugin that holds the ready_for_update op is relaunched exactly as one
// that does not. No round is opened and no task is sent. The relaunch keeps the pod and the
// disk, so what it costs is the turn alone.
func TestPodUpdateRelaunchesAStalePluginAtOnce(t *testing.T) {
	for _, contract := range []int{PodUpdateContract - 1, PodUpdateContract} {
		t.Run("at contract "+strconv.Itoa(contract), func(t *testing.T) {
			h := updateHarness(t, contract)
			h.reach(StateReady)
			h.m.deps.PluginContract = PodUpdateContract + 1

			h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

			h.wantState(StateLaunching)
			h.wantCalls("Resume", 1)
			h.wantPrompts(0)
			c := h.wantRound("")
			if c.PluginRelaunchFor != PodUpdateContract+1 {
				t.Errorf("PluginRelaunchFor = %d, want %d", c.PluginRelaunchFor, PodUpdateContract+1)
			}
		})
	}
}

// A stale registration keeps today's immediate relaunch whatever the claim's contract: the agent
// that registered is refused and its process ends on that refusal, so a round there would wait for
// a signal no running turn could send.
func TestPodUpdateRelaunchesAStaleRegistrationAtOnce(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.m.deps.PluginContract = PodUpdateContract + 1
	h.m.deps.PluginRelaunchable = true

	err := h.handle(RequestRegister{
		Claim:          h.token,
		Generation:     h.generation(),
		Session:        session,
		SessionFile:    sessionFile,
		CapabilityHash: []byte("capability"),
		PluginContract: PodUpdateContract,
	})

	if err == nil {
		t.Fatal("the stale registration was accepted, want the refusal that names both contracts")
	}
	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	h.wantPrompts(0)
	c := h.wantRound("")
	if c.PluginRelaunchFor != PodUpdateContract+1 {
		t.Errorf("PluginRelaunchFor = %d, want %d", c.PluginRelaunchFor, PodUpdateContract+1)
	}
}

// A round opened over a claim with no registered agent records itself and sends nothing, as it
// sends nothing to any claim: the round's ask is the sweep's, on the role's own topic.
func TestPodUpdateRecordsTheRoundOnAClaimWithNoAgent(t *testing.T) {
	for _, state := range []ClaimState{StateQueued, StateLaunching, StateShimConnected} {
		t.Run(string(state), func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.reach(state)

			h.openUpdate(testRound, testReasons)

			h.wantState(state)
			h.wantPrompts(0)
			if c := h.wantRound(testRound); c.UpdateTaskedAt != nil {
				t.Errorf("UpdateTaskedAt = %v, want nil: no agent has registered", c.UpdateTaskedAt)
			}
		})
	}
}

// The worker's answer is fenced as every other request an agent makes is: it ends a round, and a
// round ending deletes the pod's disk, so a process of a superseded generation or of another
// session must never be what says the work is pushed.
func TestPodUpdateSignalIsFenced(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)

	err := h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: "ses_intruder", Round: testRound, Head: "c0ffee"})

	if !errors.Is(err, claim.SameAgentRefusal) {
		t.Errorf("ready_for_update as another session returned %v, want the same-agent refusal", err)
	}
	if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("claim after the refused signal = %+v, want the round still waiting for its answer", c)
	}

	// The relaunch moves the claim to the next generation; the process of the one before it is the
	// stale one.
	h.observe(runtime.Gone)
	h.wantState(StateLaunching)

	err = h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation() - 1, Session: session, Round: testRound, Head: "c0ffee"})

	if !errors.Is(err, claim.StaleGeneration) {
		t.Errorf("ready_for_update at the superseded generation returned %v, want the stale-generation refusal", err)
	}
	if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("claim after the stale signal = %+v, want the round still waiting for its answer", c)
	}
}

// A claim that retires while a round is open leaves no round behind: a worker that finished its
// phase and exited, and a claim the workflow stopped, each take their round with them. A round
// left recorded on a retired claim outlives the pod it was opened over, and the daemon's sweep
// reads it against the pod that replaced it at every tick it runs.
func TestPodUpdateARetiredClaimLeavesNoRoundBehind(t *testing.T) {
	for name, retire := range map[string]func(*harness){
		"the agent reports its exit": func(h *harness) { h.exit() },
		"the workflow stops it":      func(h *harness) { h.must(RequestStop{Claim: h.token}) },
	} {
		t.Run(name, func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.reach(StateIdle)
			h.openUpdate(testRound, testReasons)
			h.signalUpdate("c0ffee")
			h.wantRound(testRound)

			retire(h)

			h.wantState(StateRetired)
			c := h.wantRound("")
			if c.UpdateSignalledAt != nil || c.UpdateHead != "" || c.UpdateOpenedAt != nil ||
				c.UpdateTaskedAt != nil || c.UpdateReasons != "" {
				t.Errorf("retired claim = %+v, want every update field cleared with the round", c)
			}
			stored := h.store.load(h.token)
			if stored.UpdateRound != "" || stored.UpdateSignalledAt != nil || stored.UpdateHead != "" {
				t.Errorf("stored claim = %+v, want the round cleared in the store too", stored)
			}
		})
	}
}

// The round ends for a failed claim as it ends for every other: the pod it was opened over has
// been replaced, so the round goes. Nothing is sent — the process that would have read a task died
// — and the claim stays failed for the workflow's retry to decide. The deaths are charged to the
// phase task the claim holds, which is what fails it.
func TestPodUpdateEndedOnAFailedClaimClearsTheRoundAndSendsNothing(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	for death := 1; death < testLimits().LaunchFailures; death++ {
		h.observe(runtime.Gone)
		h.relaunched()
	}
	h.observe(runtime.Gone)
	h.wantState(StateFailed)
	h.wantRound(testRound)
	sent := len(h.prompts())

	h.must(PodUpdateEnded{Claim: h.token, Round: h.claim().UpdateRound})

	h.wantState(StateFailed)
	h.wantPrompts(sent)
	c := h.wantRound("")
	if c.UpdateReasons != "" || c.UpdateOpenedAt != nil || c.UpdateTaskedAt != nil {
		t.Errorf("failed claim after the round ended = %+v, want every update field cleared", c)
	}
	if stored := h.store.load(h.token); stored.UpdateRound != "" || stored.State != StateFailed {
		t.Errorf("stored claim = %+v, want the round cleared and the claim still failed", stored)
	}
}

// A turn the worker starts after it answered is not an ask's: an Envoy notice delivered into its
// session, or a person's steer, starts a turn the daemon sent nothing for, and that turn can
// commit work the answer said was pushed. The answer is taken back with the ask's publish mark,
// so the sweep publishes the round's ask to the worker again, and nothing of the round reads as
// answered until the second answer arrives. The machine itself sends nothing.
func TestPodUpdateATurnAfterTheAnswerTakesItBackAndAsksAgain(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	if asked, err := h.m.PodUpdateAsked(h.ctx, testRound); err != nil || !asked {
		t.Fatalf("mark the sweep's ask = %t, %v; want it marked", asked, err)
	}
	h.signalUpdate("c0ffee")
	if c := h.claim(); c.UpdateSignalledAt == nil {
		t.Fatalf("claim after the answer = %+v, want the answer recorded", c)
	}

	h.must(StreamTurnStart{Claim: h.token})

	c := h.wantRound(testRound)
	if c.UpdateSignalledAt != nil || c.UpdateHead != "" || c.UpdateTaskedAt != nil {
		t.Fatalf("claim after a turn of the agent's own = %+v, want the answer and the publish mark taken back", c)
	}
	if stored := h.store.load(h.token); stored.UpdateSignalledAt != nil || stored.UpdateHead != "" || stored.UpdateTaskedAt != nil {
		t.Errorf("stored claim = %+v, want the answer and the mark taken back in the store too", stored)
	}

	h.openUpdate(testRound, testReasons)
	h.must(StreamTurnEnd{Claim: h.token})

	h.wantPrompts(0)
}

// A turn the daemon sent nothing for, after the answer, is decided by how that answer was given
// (Claim.UpdateAnsweredByTurnEnd), never by the contract the claim's plugin speaks. A worker
// that answered by ending its task's turn was told to end its last message with a WAITING line,
// and its own phase-stall reminder starts a turn when that wait is over: on the first
// contract-46 roll every one of 194 withdrawals was such a turn, with nothing pending, and each
// cost the round a re-ask and a model turn. That claim keeps its answer, asks nothing more, and
// the round ends. A worker that made the call holds its reminder while that answer stands, so
// the same turn there is an Envoy notice or a person's steer: it takes the answer back and the
// round asks again, as TestPodUpdateATurnAfterTheAnswerTakesItBackAndAsksAgain pins.
//
// The contract is in the table beside the answer because it decides nothing here: a plugin at
// this daemon's own contract can be built with no ready_for_update in it, and then it answers
// by its turn's end exactly as the released plugin before it does.
func TestPodUpdateATurnTheDaemonSentNothingForKeepsOnlyTheAnswerATurnsEndGave(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contract int
		calls    bool
		keeps    bool
	}{
		{name: "a turn's end answered, at a contract behind", contract: PodUpdateContract - 1, keeps: true},
		{name: "a turn's end answered, at this daemon's own contract", contract: PodUpdateContract, keeps: true},
		{name: "the worker called ready_for_update", contract: PodUpdateContract, calls: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := updateHarness(t, tc.contract)
			h.reach(StateReady)
			h.m.deps.PluginContract = PodUpdateContract
			h.openUpdate(testRound, testReasons)
			h.leftAsk(testRound)
			h.wantPrompts(1)
			h.must(StreamTurnStart{Claim: h.token})
			if tc.calls {
				h.signalUpdate("c0ffee")
			}
			h.must(StreamTurnEnd{Claim: h.token})
			answered := h.claim().UpdateSignalledAt
			if answered == nil || h.claim().Pending != nil {
				t.Fatalf("claim after the ask's turn = %+v, want the answer recorded and the ask retired", h.claim())
			}

			// The turn the daemon sent nothing for, the sweep's next post while it runs, and its end.
			h.must(StreamTurnStart{Claim: h.token})
			h.openUpdate(testRound, testReasons)
			h.must(StreamTurnEnd{Claim: h.token})

			c := h.wantRound(testRound)
			if !tc.keeps {
				if c.UpdateSignalledAt != nil {
					t.Fatalf("UpdateSignalledAt = %v, want the answer taken back by a turn this plugin's reminder never starts", c.UpdateSignalledAt)
				}
				if c.UpdateTaskedAt != nil {
					t.Errorf("UpdateTaskedAt = %v, want the publish mark taken back so the sweep asks again", c.UpdateTaskedAt)
				}
				h.wantPrompts(1)
				return
			}
			if c.UpdateSignalledAt == nil || !c.UpdateSignalledAt.Equal(*answered) {
				t.Fatalf("UpdateSignalledAt = %v, want the answer of %v standing", c.UpdateSignalledAt, *answered)
			}
			if stored := h.store.load(h.token); stored.UpdateSignalledAt == nil {
				t.Error("the store lost the answer the turn left standing")
			}
			h.wantPrompts(1)

			h.must(PodUpdateEnded{Claim: h.token, Round: testRound})
			h.wantRound("")
		})
	}
}

// The reminder turn of a claim below the contract confirms nothing it did not send: the next
// phase's task, queued after the answer, stays held and unread through that turn, as an answered
// round holds it (answeredOpenRound), and the round's end releases it.
func TestPodUpdateAReminderTurnBelowTheContractLeavesTheNextTaskHeld(t *testing.T) {
	h := updateHarness(t, PodUpdateContract-1)
	h.reach(StateReady)
	h.m.deps.PluginContract = PodUpdateContract
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})
	h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})
	h.wantPrompts(1)

	h.must(StreamTurnStart{Claim: h.token})

	if c := h.claim(); c.UpdateSignalledAt == nil {
		t.Fatal("the reminder turn took the answer back")
	}
	if p := h.pending(); p.Task != "review the pull request" || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending = %+v, want the next task held and unconfirmed", p)
	}
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantPrompts(1)

	h.must(PodUpdateEnded{Claim: h.token, Round: testRound})

	if released := h.wantPrompts(2)[1]; released.Message != "review the pull request" {
		t.Errorf("the task after the round = %q, want the held one", released.Message)
	}
}

// An ask an older daemon queued as a task goes when the round ends. A claim left holding it would
// be prompted with "The daemon must replace the pod" for a round that is over, and until some
// turn read it every task of the claim's next phase would wait behind it (ErrDeliveryPending).
func TestPodUpdateAnEndedRoundLeavesNoTaskPending(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	if got := h.pending().ID; !asksRound(got, testRound) {
		t.Fatalf("pending delivery = %q, want the older daemon's ask of the round %q", got, testRound)
	}

	h.must(PodUpdateEnded{Claim: h.token, Round: h.claim().UpdateRound})

	h.wantRound("")
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending = %+v, want the ask nobody read taken back with the round", p)
	}
	if d, held := h.store.delivery(h.token); held {
		t.Errorf("the store still holds %+v, want the ended round's ask deleted", d)
	}

	// The claim takes its next phase task, which nothing of the ended round is in the way of.
	h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})
	if got := h.pending().Task; got != "review the pull request" {
		t.Errorf("pending task = %q, want the next phase's", got)
	}
}

// A round that replaces an earlier one takes the earlier round's unread ask with it — an ask an
// older daemon queued as a task — and clears the publish mark, so the sweep publishes the new
// round's ask. The claim holds one task at a time, so an ask of a round that is over would hold
// every task of the claim's next phase behind it (ErrDeliveryPending).
func TestPodUpdateANewRoundReplacesTheEarlierRoundsUnreadAsk(t *testing.T) {
	const second = "pod:1f0b4c8d-7e21-4a55-8c3b-9d2e6f0a4b13"
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	sent := len(h.prompts())

	h.openUpdate(second, "image, resources")

	c := h.wantRound(second)
	if c.UpdateOpenedAt == nil || c.UpdateReasons != "image, resources" {
		t.Fatalf("claim in the second round = %+v, want it opened with its own reasons", c)
	}
	if c.UpdateTaskedAt != nil {
		t.Errorf("UpdateTaskedAt = %v, want nil: the sweep has not asked the second round yet", c.UpdateTaskedAt)
	}
	if c.Pending != nil {
		t.Fatalf("pending = %+v, want the first round's unread ask taken back", c.Pending)
	}
	h.wantPrompts(sent)
}

// The project controller's own claim keeps the immediate relaunch whatever contract it records,
// and it keeps it mid-turn. Every other worker's turn is waited out because what that turn
// would lose is work it has not pushed; the controller's pod holds no issue workspace, so it
// has nothing to push and nothing to wait for.
func TestPodUpdateRelaunchesTheControllersStalePluginAtOnce(t *testing.T) {
	h := newBareHarness(t)
	h.deps.PluginContract = PodUpdateContract
	c := queuedClaim()
	c.Token, c.Role, c.Issue, c.Tree = claim.ControllerToken("legion"), claim.RoleController, "", ""
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateReady)
	h.m.deps.PluginContract = PodUpdateContract + 1

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	h.wantPrompts(0)
	got := h.wantRound("")
	if got.PluginRelaunchFor != PodUpdateContract+1 {
		t.Errorf("PluginRelaunchFor = %d, want the contract the relaunch was made for %d",
			got.PluginRelaunchFor, PodUpdateContract+1)
	}
}

// A registration that already speaks this daemon's contract ends a plugin round open over the
// claim: what the round waited for has happened. The ask nothing read goes with it, and the task
// the round was holding back is sent. A round left open here would hold that task until a sweep
// ended the round, over a process that is already current.
func TestPodUpdateACurrentRegistrationEndsThePluginRound(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	// The walk to idle delivers a phase task and works it, so one prompt is already sent.
	sent := len(h.prompts())
	round := PluginRoundPrefix + strconv.Itoa(PodUpdateContract+1)
	h.openUpdate(round, testReasons)
	h.leftAsk(round)
	h.wantPrompts(sent + 1)
	// The worker reads the ask, answers it and ends its turn, which retires the round's own task.
	// The next phase task then arrives while the round holds it back.
	h.must(StreamTurnStart{Claim: h.token})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: h.token})
	h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})
	h.wantPrompts(sent + 1)
	h.m.deps.PluginContract = PodUpdateContract + 1
	h.m.deps.PluginRelaunchable = true

	h.must(RequestRegister{
		Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: PodUpdateContract + 1,
	})

	c := h.wantRound("")
	if c.UpdateReasons != "" || c.UpdateOpenedAt != nil || c.UpdateTaskedAt != nil || c.UpdateSignalledAt != nil {
		t.Errorf("claim after the current registration = %+v, want every update field cleared", c)
	}
	if stored := h.store.load(h.token); stored.UpdateRound != "" {
		t.Errorf("stored claim = %+v, want the round cleared in the store too", stored)
	}
	released := h.wantPrompts(sent + 2)[sent+1]
	if released.Message != "review the pull request" {
		t.Errorf("the task after the registration = %q, want the one the round held back", released.Message)
	}
	h.wantCalls("Resume", 0)
}

// A claim an earlier daemon left in a plugin update round leaves that round at the end this
// release's boot posts for it, whatever state it is in, and the task the round was holding back
// is released (dispatch://INFRA-2712 R5, internal/daemon's endPluginRounds). Nothing of this
// release opens a plugin round, and nothing of it carries one either: the sweep reads pod rounds
// alone (reconcilePodUpdates), so a round left standing would hold the claim's next task behind
// it for as long as the claim lives. The end takes the round whole — the ask nothing read, every
// field the round wrote, the claim's write — and launches nothing of its own: a plugin round's
// process is the lane's to replace at a paced wake, not this end's (endPodUpdate, leaveRound).
//
// Both states are driven because the boot posts for every claim in a round, not only the live
// ones: a suspended claim's round is as durable as a live one's and holds the task it will be
// resumed for. The live claim's task goes at the end itself, the suspended claim's at the resume
// that brings its process back, which is where every task of a suspended claim goes.
//
// The daemon here can relaunch onto no plugin (PluginRelaunchable false, LEGION-164's degraded
// Kubernetes mode), which is what makes the release readable as a prompt: on a daemon that can,
// the released task meets the refresh lane's own hold and goes at the ready of the process that
// lane's relaunch starts instead (sendPending's staleRefusesClaim,
// TestAStaleClaimHoldsItsTaskUntilItsReplacementRegistersOnThisDaemonsContract).
//
// The negative control is the end made to pass over a plugin round (`if m.claim.InPluginRound()
// { return nil }` at the head of endPodUpdate): the round then stands, and with it the hold its
// answer puts on the claim's task, which both halves of this test read at the prompts.
func TestAPluginRoundAnEarlierDaemonLeftEndsAndReleasesTheTaskItHeld(t *testing.T) {
	const next = "review the pull request"
	round := PluginRoundPrefix + strconv.Itoa(PodUpdateContract+1)
	for _, tc := range []struct {
		name    string
		suspend bool
	}{
		{name: "live"},
		{name: "suspended", suspend: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.reach(StateIdle)
			// The walk to idle delivers a phase task and works it, so one prompt is already sent.
			sent := len(h.prompts())
			// This daemon speaks the contract the round was opened for; the claim's process
			// still speaks the one it registered on. PluginRelaunchable is left false.
			h.m.deps.PluginContract = PodUpdateContract + 1
			// The round an earlier daemon opened, answered by the worker and holding the task
			// the claim was handed next: the state this boot finds the claim in.
			h.openUpdate(round, testReasons)
			h.leftAsk(round)
			h.wantPrompts(sent + 1)
			h.must(StreamTurnStart{Claim: h.token})
			h.signalUpdate("c0ffee")
			h.must(StreamTurnEnd{Claim: h.token})
			h.must(RequestDeliver{Claim: h.token, Task: next, ID: "outbox:42", Row: 42, Generation: 7})
			h.wantPrompts(sent + 1)
			if tc.suspend {
				h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
				h.wantState(StateSuspended)
			}

			h.must(PodUpdateEnded{Claim: h.token, Round: round})

			c := h.wantRound("")
			if c.UpdateReasons != "" || c.UpdateOpenedAt != nil || c.UpdateTaskedAt != nil ||
				c.UpdateSignalledAt != nil || c.UpdateHead != "" {
				t.Errorf("the claim after the end = %+v, want every field the round wrote cleared", c)
			}
			if stored := h.store.load(h.token); stored.UpdateRound != "" {
				t.Errorf("the stored claim = %+v, want the round cleared in the store too: a restart must not read it back",
					stored)
			}
			h.wantCalls("Resume", 0)
			if p := h.claim().Pending; p == nil || p.Task != next {
				t.Fatalf("the claim after the end holds %+v, want the task %q the round was holding back", p, next)
			}

			if tc.suspend {
				// A suspended claim is handed nothing, as it is handed nothing in any other
				// round or none (sendPending): the resume is what brings its process back and
				// sends the task the round no longer holds.
				h.wantPrompts(sent + 1)
				h.must(RequestResume{Claim: h.token})
				h.relaunched()
			}

			if got := h.wantPrompts(sent + 2)[sent+1]; got.Message != next {
				t.Fatalf("the prompt after the end is %+v, want the task %q the round held back", got, next)
			}
		})
	}
}

// The sweep posts the end from what it read at the start of a tick, so a claim whose round ended
// in between is posted an end for a round that is already over. That post decides nothing: it
// writes the claim nothing, and a claim in no round has nothing left for an end to take.
func TestPodUpdateEndedOnAClaimInNoRoundDecidesNothing(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	h.openUpdate(testRound, testReasons)
	h.signalUpdate("c0ffee")
	h.must(PodUpdateEnded{Claim: h.token, Round: h.claim().UpdateRound})
	h.wantRound("")
	writes := len(h.store.history())

	h.must(PodUpdateEnded{Claim: h.token, Round: testRound})

	h.wantCalls("Resume", 0)
	if got := len(h.store.history()); got != writes {
		t.Errorf("the second end wrote the claim %d times, want none: it decides nothing", got-writes)
	}
}

// An answer counts for the round whose task the worker read, and for no other: the task names
// the round, and the answer carries that id back (UpdateTask, PodUpdateSignalled). An answer for
// any other round is refused, names both rounds, and records nothing — neither the answer nor
// the head it reported — so the round it named cannot be ended by it and the round the claim is
// in is still waiting for an answer of its own.
func TestPodUpdateAnAnswerForARoundTheClaimIsNotInIsRefused(t *testing.T) {
	other := PodRound(testOtherPod)
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	writes := len(h.store.history())

	err := h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: other, Head: "c0ffee"})

	if !errors.Is(err, ErrForeignUpdateRound) {
		t.Fatalf("the answer for another round returned %v, want the foreign-round refusal", err)
	}
	for _, name := range []string{other, testRound} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal %q names neither the round answered nor the round the claim is in; want both, missing %q", err, name)
		}
	}
	if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("claim after the refused answer = %+v, want the round still waiting for an answer of its own", c)
	}
	if got := len(h.store.history()); got != writes {
		t.Errorf("the refused answer wrote the claim %d times, want none", got-writes)
	}

	// A claim in no round at all is the same case: the answer names a round it is not in.
	h.must(PodUpdateEnded{Claim: h.token, Round: h.claim().UpdateRound})
	h.wantRound("")

	err = h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: testRound, Head: "c0ffee"})

	if !errors.Is(err, ErrForeignUpdateRound) || !strings.Contains(err.Error(), testRound) {
		t.Fatalf("the answer to a claim in no round returned %v, want the foreign-round refusal naming %q", err, testRound)
	}
	if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("claim after the answer it was asked nothing for = %+v, want nothing of the answer recorded", c)
	}
}

// The answer that names the round the claim stands in is taken, exactly as it was before the id
// travelled with it: the signal and the head the worker reported are recorded, and the round's
// own sweep is what acts on them.
func TestPodUpdateAnAnswerForTheClaimsOwnRoundIsRecorded(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)

	h.signalUpdateFor(testRound, "c0ffee")

	c := h.wantRound(testRound)
	if c.UpdateSignalledAt == nil || !c.UpdateSignalledAt.Equal(epoch) {
		t.Errorf("UpdateSignalledAt = %v, want the answer's own clock at %v", c.UpdateSignalledAt, epoch)
	}
	if c.UpdateHead != "c0ffee" {
		t.Errorf("UpdateHead = %q, want the head the worker reported", c.UpdateHead)
	}
	if stored := h.store.load(h.token); stored.UpdateSignalledAt == nil {
		t.Errorf("the answer was not persisted: stored claim %+v", stored)
	}
}

// A worker answers the round its task named, so a round never takes the answer of the round it
// replaced, however the two meet. Each round names a pod of its own, and the pushing a worker
// did for the pod it was told of says nothing about the pod the next round deletes. The switch
// happens the moment the daemon opens the second round, whatever turn is in flight, and the
// answer the worker gives under the first round's words is refused by its id rather than
// recorded against the round that replaced it — which would have the sweep replace a pod no
// worker has pushed for. The sweep then publishes the new round's own words, and the answer to
// that round counts.
//
// The three turns are the three ways a turn can be in flight across a switch, each of which the
// daemon once had to guess at: a turn that started late, after noTurn took the round's ask back
// unconfirmed; a turn an earlier daemon saw start, whose claim this daemon restored with the
// question still to ask (askFirst); and a turn the daemon sent nothing for, an Envoy notice or a
// person's steer. With the id carried, the daemon has to know nothing about any of them.
func TestPodUpdateARoundDoesNotTakeTheAnswerOfTheRoundItReplaced(t *testing.T) {
	first := PodRound(testOtherPod)
	for name, turn := range map[string]func(*harness){
		"a late turn of the old round's ask": func(h *harness) {
			// The ask was acknowledged and started no turn within the bound, so it went back to
			// waiting unconfirmed; the worker's turn on those same words starts after that.
			h.advance(2 * testRPC)
			if p := h.pending(); !p.ConfirmedAt.IsZero() || !asksRound(p.ID, first) {
				h.t.Fatalf("pending after the wait ran out = %+v, want the round's own ask back in waiting, unconfirmed", p)
			}
			h.must(StreamTurnStart{Claim: h.token})
		},
		"a restart that has not heard from the agent": func(h *harness) {
			h.restart()
			if !h.m.askFirst {
				h.t.Fatal("the restored claim has nothing to ask its agent; this test needs the question a restart leaves open")
			}
			h.must(StreamTurnStart{Claim: h.token})
		},
		"a turn the daemon sent nothing for": func(h *harness) {
			// The round's ask had its turn, which ended without an answer, and a turn of the
			// agent's own starts after it.
			h.must(StreamTurnStart{Claim: h.token})
			h.must(StreamTurnEnd{Claim: h.token})
			h.must(StreamTurnStart{Claim: h.token})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.reach(StateReady)
			h.openUpdate(first, testReasons)
			h.leftAsk(first)
			h.wantPrompts(1)
			turn(h)
			h.wantState(StateWorking)

			h.openUpdate(testRound, testReasons)

			c := h.wantRound(testRound)
			if c.UpdateSignalledAt != nil || c.UpdateHead != "" {
				t.Fatalf("claim in the second round = %+v, want the first round's fields left behind with it", c)
			}

			err := h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: first, Head: "c0ffee"})

			if !errors.Is(err, ErrForeignUpdateRound) || !strings.Contains(err.Error(), first) ||
				!strings.Contains(err.Error(), testRound) {
				t.Fatalf("the answer under the first round's words returned %v, want the refusal naming %q and %q", err, first, testRound)
			}
			if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
				t.Fatalf("claim after the refused answer = %+v, want the second round still waiting for an answer of its own", c)
			}

			// The worker's turn ends and the sweep posts the round again. The machine sends no
			// ask of the second round: the publish mark is clear, so the sweep publishes that
			// round's own words on the role's topic. The answer to that round is the one that
			// counts.
			h.must(StreamTurnEnd{Claim: h.token})
			h.openUpdate(testRound, testReasons)

			for _, p := range h.prompts() {
				if asksRound(p.DeliveryID, testRound) {
					t.Errorf("prompt %+v asks the second round; want the machine to queue no ask of it", p)
				}
			}
			if c := h.claim(); c.UpdateTaskedAt != nil {
				t.Errorf("UpdateTaskedAt = %v, want nil: the sweep has not published the second round's ask yet", c.UpdateTaskedAt)
			}
			h.signalUpdateFor(testRound, "c0ffee")
			if c := h.claim(); c.UpdateSignalledAt == nil || c.UpdateHead != "c0ffee" {
				t.Errorf("claim after the second round's own answer = %+v, want that answer recorded", c)
			}
		})
	}
}

// An ask's delivery id is the one record of which round it is of. Three paths rotate a pending
// id in the middle of a round — a late agent_start, Oh My Pi's "Agent is already processing"
// refusal, and a death mid-turn — and the rotation keeps the round (rotateTaskID). An ask that
// lost it would read as a phase task: the round's end would leave it pending, the worker would be
// prompted with the words of a round whose pod is already replaced, and every task of its next
// phase would wait behind it.
func TestPodUpdateAnAskKeepsItsRoundWhenItsIdRotates(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	ask := h.wantPrompts(1)[0]

	// The ask was acknowledged and started no turn: the wait for it runs out, and the task goes
	// back to waiting under a new id the shim has no record of.
	h.advance(2 * testRPC)

	rotated := h.pending()
	if rotated.ID == ask.DeliveryID {
		t.Fatalf("pending = %q, want an id of its own after the wait ran out", rotated.ID)
	}
	if !asksRound(rotated.ID, testRound) {
		t.Fatalf("the rotated id = %q, want it still an ask of the round %q", rotated.ID, testRound)
	}

	h.must(PodUpdateEnded{Claim: h.token, Round: h.claim().UpdateRound})

	h.wantRound("")
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending after the round ended = %+v, want the round's own ask taken back with it", p)
	}
	if d, held := h.store.delivery(h.token); held {
		t.Errorf("the store still holds %+v, want the ended round's ask deleted", d)
	}
}

// A round's end tells its worker nothing of the daemon's own. The pod the round was opened over
// is gone by the time the end is posted — this round replaced it, or something else did — so the
// worker comes back on a new session and a new disk, where the launch is what starts it, and a
// task telling it to continue would lift the ask's "start no new work" over a workspace that
// holds none of its work.
func TestPodUpdateARoundThatReplacedThePodTellsItsWorkerNothing(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)
	h.must(StreamTurnStart{Claim: h.token})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: h.token})

	h.must(PodUpdateEnded{Claim: h.token, Round: h.claim().UpdateRound})

	h.wantRound("")
	h.wantPrompts(1)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending after the round that replaced the pod = %+v, want nothing queued for a worker the launch starts", p)
	}
}

// A turn this daemon learned of from the agent's own get_state answer, rather than from an
// agent_start it read, is one it cannot vouch for: a restart leaves the agent in a turn
// an earlier daemon sent the task for, and the send that follows asks the agent whether it is
// streaming and confirms the pending ask against that answer (sendPending's askFirst branch).
// Nothing there says what the turn has already done, so its end is no answer to the round. The
// same holds for a turn start that is the first thing a restarted daemon hears of the turn,
// which only askFirst confirms (turnStarted). Each refusal takes the ask's publish mark back, so
// the sweep publishes the round's ask to the role again; the machine queues nothing for it.
func TestPodUpdateTakesNoTurnTheDaemonLearnedOfFromTheAgentAsTheAnswer(t *testing.T) {
	t.Run("the send asked the agent whether it was streaming", func(t *testing.T) {
		h := updateHarness(t, PodUpdateContract-1)
		h.reach(StateReady)
		h.openUpdate(testRound, testReasons)
		h.leftAsk(testRound)
		h.wantPrompts(1)
		// The daemon restarts: the ask is pending and unconfirmed, and the restored machine asks
		// the agent first.
		h.restart()
		h.conn.SetStreaming(true) // the agent is in the turn the earlier daemon's ask started
		h.must(RequestReady{Claim: h.token, Generation: h.generation(), Session: session})
		h.wantState(StateWorking)

		h.must(StreamTurnEnd{Claim: h.token})

		c := h.claim()
		if c.UpdateSignalledAt != nil {
			t.Errorf("UpdateSignalledAt = %v, want nil: this daemon never saw the turn start", c.UpdateSignalledAt)
		}
		if c.UpdateTaskedAt != nil || c.Pending != nil {
			t.Errorf("claim after the refusal = %+v, want the publish mark taken back and no task queued", c)
		}
	})

	t.Run("the start is the first thing a restarted daemon hears of the turn", func(t *testing.T) {
		h := updateHarness(t, PodUpdateContract-1)
		h.reach(StateReady)
		h.openUpdate(testRound, testReasons)
		h.leftAsk(testRound)
		h.wantPrompts(1)
		// The daemon restarts with the ask pending and unconfirmed, so askFirst is armed and no
		// send of its own is in flight. The start it then reads is of a turn an earlier daemon's
		// process began.
		h.restart()

		h.must(StreamTurnStart{Claim: h.token})
		h.must(StreamTurnEnd{Claim: h.token})

		c := h.claim()
		if c.UpdateSignalledAt != nil {
			t.Errorf("UpdateSignalledAt = %v, want nil: only askFirst made this turn the ask's, and this daemon never saw it begin", c.UpdateSignalledAt)
		}
		if c.UpdateTaskedAt != nil || c.Pending != nil {
			t.Errorf("claim after the refusal = %+v, want the publish mark taken back and no task queued", c)
		}
	})

	// The control for both: the turn of a live worker, whose agent_start names no delivery
	// because no agent_start ever does, is the turn of the ask the daemon handed it and saw begin.
	// Its end is the answer.
	t.Run("a live turn of the ask this daemon handed the worker", func(t *testing.T) {
		h := updateHarness(t, PodUpdateContract-1)
		h.reach(StateReady)
		h.openUpdate(testRound, testReasons)
		h.leftAsk(testRound)
		h.wantPrompts(1)

		h.must(StreamTurnStart{Claim: h.token})
		h.must(StreamTurnEnd{Claim: h.token})

		if c := h.claim(); c.UpdateSignalledAt == nil {
			t.Error("the end of the update task's turn recorded no answer; no agent_start of a live turn names a delivery")
		}
	})
}

// A pod dies and the claim comes up in another one while the round opened over the dead pod is
// still recorded on it. The worker now running holds none of the work that round asks about, so
// the claim's part in the round is frozen unanswered: the ask the dead pod never read is dropped
// rather than sent again, the sweep's next post asks nothing, and an answer given from the new
// pod is refused. An answer counted there would report work as pushed that nobody pushed.
func TestPodUpdateAnUnansweredClaimRelaunchedIntoAnotherPodIsAskedNothingMore(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.inPod(testRoundPod)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	if asked := h.wantPrompts(1)[0]; !asksRound(asked.DeliveryID, testRound) {
		t.Fatalf("delivery id = %q, want an ask of the round %q", asked.DeliveryID, testRound)
	}

	h.diesIntoPod(testOtherPod)

	h.wantPrompts(1)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending = %+v, want the round's ask dropped rather than sent in the new pod", p)
	}
	if d, held := h.store.delivery(h.token); held {
		t.Errorf("the store holds %+v, want the ask dropped with the pod it was sent for", d)
	}
	if c := h.wantRound(testRound); c.UpdateSignalledAt != nil {
		t.Errorf("claim = %+v, want its part in the round still unanswered", c)
	}

	// The sweep posts the round at every tick while it is open; this claim is asked nothing.
	h.advance(2 * time.Hour)
	h.openUpdate(testRound, testReasons)

	h.wantPrompts(1)
	if p := h.claim().Pending; p != nil {
		t.Errorf("pending = %+v, want no ask of the round in the new pod", p)
	}

	err := h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: testRound, Head: "c0ffee"})

	if !errors.Is(err, ErrLeftPodRound) {
		t.Fatalf("the answer from the new pod returned %v, want the refusal of a process in another pod", err)
	}
	// The refusal names one round, the round the claim left, which is the round the answer
	// carried. It names no round of the pod the claim came up in: no round is open over that
	// pod, and a worker that answered the id of one would be refused again, as answering a
	// round that is not its claim's (foreignRoundRefusal).
	if !strings.Contains(err.Error(), testRound) {
		t.Errorf("refusal = %q, want it to name the round the claim left, %q", err, testRound)
	}
	if of := PodRound(testOtherPod); strings.Contains(err.Error(), of) {
		t.Errorf("refusal = %q, want it to name no round of the pod the claim came up in, %q", err, of)
	}
	if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("claim after the refused answer = %+v, want nothing of it recorded", c)
	}
}

// The ask of the pod the claim has left goes, and the question a restart armed for it goes with
// it. A daemon that died between the write of ready in the new pod and the write of that drop —
// or whose RetireDelivery failed, which leaves exactly the same two rows — comes back holding
// the dead pod's ask, pending and unconfirmed, and marks it askFirst: the agent may already be
// in the turn an earlier daemon's prompt started. The sweep drops the ask. Left armed over
// nothing pending, that question would make the claim's next task the running turn's —
// confirmed by a turn of another cause, never prompted, and retired unsent by that turn's end —
// and the worker would never receive the task at all.
func TestPodUpdateADroppedAskLeavesNoQuestionArmedForTheNextTask(t *testing.T) {
	const task = "review the pull request"
	h := updateHarness(t, PodUpdateContract)
	h.inPod(testRoundPod)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)

	// The pod dies and the claim comes up in another one, but the drop's write fails, so the
	// store holds the claim ready in the new pod with the dead pod's ask still pending.
	h.rt.ScriptResume(fake.SpawnResult{Locator: podProcess(testOtherPod, 1)})
	h.observe(runtime.Gone)
	h.wantState(StateLaunching)
	h.connect()
	h.register()
	h.store.fail("RetireDelivery", errBoom)
	if err := h.handle(RequestReady{Claim: h.token, Generation: h.generation(), Session: session}); !errors.Is(err, errBoom) {
		t.Fatalf("ready over a failing retirement returned %v, want the store's error", err)
	}
	h.store.fail("RetireDelivery", nil)

	h.restart()
	if !h.m.askFirst {
		t.Fatal("the restored claim has nothing to ask its agent; this test needs the question a restart leaves open")
	}
	// The daemon's next observation of the live process drops the ask of the round this claim's
	// process has left.
	h.observe(runtime.Alive)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending = %+v, want the dead pod's ask dropped in the new pod", p)
	}

	// A turn of another cause — an Envoy notice, a person's steer — is running, one this daemon
	// never saw begin, and the workflow hands the claim the task of its next phase.
	h.conn.SetStreaming(true)
	h.must(RequestDeliver{Claim: h.token, Task: task})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	sent := h.wantPrompts(2)[1]
	if sent.Message != task {
		t.Errorf("the second prompt carried %q, want the next task %q", sent.Message, task)
	}
}

// A claim that answered its round and was then relaunched into another pod keeps that answer:
// the answer was given in the pod the round is about, and no turn of the pod that replaced it
// can move the branch the dead pod held. A turn there would otherwise take the answer back and
// report a worker that did push as having lost its work.
func TestPodUpdateAnAnsweredClaimRelaunchedIntoAnotherPodKeepsItsAnswer(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.inPod(testRoundPod)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)
	h.must(StreamTurnStart{Claim: h.token})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: h.token})
	answered := h.claim().UpdateSignalledAt
	if answered == nil {
		t.Fatalf("claim = %+v, want the answer recorded in the round's own pod", h.claim())
	}

	h.diesIntoPod(testOtherPod)
	h.must(StreamTurnStart{Claim: h.token})

	c := h.wantRound(testRound)
	if c.UpdateSignalledAt == nil || !c.UpdateSignalledAt.Equal(*answered) || c.UpdateHead != "c0ffee" {
		t.Fatalf("claim after a turn in the new pod = %+v, want the answer of %v and the head it reported", c, answered)
	}
	if stored := h.store.load(h.token); stored.UpdateSignalledAt == nil || stored.UpdateHead != "c0ffee" {
		t.Errorf("stored claim = %+v, want the answer standing in the store too", stored)
	}
}

// The control that the guard reads the pod and nothing else: a claim still in the round's own
// pod answers, a turn the daemon sent nothing for takes that answer back with the ask's publish
// mark, and the sweep publishes the round's ask to it again — exactly as a claim that never left
// its pod always has.
func TestPodUpdateAClaimInTheRoundsOwnPodStillLosesItsAnswerToAForeignTurn(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.inPod(testRoundPod)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)
	h.must(StreamTurnStart{Claim: h.token})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: h.token})
	if c := h.claim(); c.UpdateSignalledAt == nil {
		t.Fatalf("claim = %+v, want the answer recorded", c)
	}

	h.must(StreamTurnStart{Claim: h.token})

	c := h.wantRound(testRound)
	if c.UpdateSignalledAt != nil || c.UpdateHead != "" || c.UpdateTaskedAt != nil {
		t.Fatalf("claim after a turn of the agent's own = %+v, want the answer and the publish mark taken back", c)
	}

	h.openUpdate(testRound, testReasons)
	h.must(StreamTurnEnd{Claim: h.token})

	h.wantPrompts(1)
}

// The round's own record of a claim that left it: the round it left, written where the launch
// records which pod the claim's process came up in, with the answer the claim already had
// standing beside it. The two are the whole of what the claim's part in that round is read from
// afterwards.
func TestPodUpdateRecordsTheRoundAClaimLeavesAndWhetherItHadAnswered(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers bool
	}{
		{name: "a claim that had not answered the round", answers: false},
		{name: "a claim that had answered the round", answers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.inPod(testRoundPod)
			h.openUpdate(testRound, testReasons)
			h.leftAsk(testRound)
			h.wantPrompts(1)
			if tc.answers {
				h.must(StreamTurnStart{Claim: h.token})
				h.signalUpdate("c0ffee")
				h.must(StreamTurnEnd{Claim: h.token})
			}

			h.diesIntoPod(testOtherPod)

			c := h.wantRound(testRound)
			if c.UpdateLeftRound != testRound || (c.UpdateSignalledAt != nil) != tc.answers {
				t.Fatalf("the claim in the new pod left %q with answered=%t, want the round %q left with answered=%t",
					c.UpdateLeftRound, c.UpdateSignalledAt != nil, testRound, tc.answers)
			}
			if s := h.store.load(h.token); s.UpdateLeftRound != testRound || (s.UpdateSignalledAt != nil) != tc.answers {
				t.Errorf("the stored claim left %q with answered=%t, want the record durable: a restart reads it from the store alone",
					s.UpdateLeftRound, s.UpdateSignalledAt != nil)
			}
		})
	}
}

// The control for the record: a claim whose process comes up in its round's own pod has left no
// round, and a claim in no round leaves none whatever pod it comes up in.
func TestPodUpdateRecordsNoLeftRoundWhereNoRoundWasLeft(t *testing.T) {
	t.Run("the claim comes up in its round's own pod", func(t *testing.T) {
		h := updateHarness(t, PodUpdateContract)
		h.inPod(testRoundPod)
		h.openUpdate(testRound, testReasons)
		h.leftAsk(testRound)
		h.wantPrompts(1)

		h.diesIntoPod(testRoundPod)

		if c := h.wantRound(testRound); c.UpdateLeftRound != "" {
			t.Fatalf("the claim back in the round's own pod left %q, want no round left", c.UpdateLeftRound)
		}
	})
	t.Run("the claim is in no round", func(t *testing.T) {
		h := updateHarness(t, PodUpdateContract)
		h.inPod(testRoundPod)

		h.diesIntoPod(testOtherPod)

		if c := h.wantRound(""); c.UpdateLeftRound != "" {
			t.Fatalf("the claim in another pod left %q, want no round left: it was in none", c.UpdateLeftRound)
		}
	})
}

// A suspension does not bring a claim back into the round it left. The suspension lets the
// claim's process go (letGo clears the locator), which is where a part of the round read from
// that locator thawed: the claim read as one that had never left, and the late answer of the
// worker in the pod that replaced the round's own was counted — work that died with a disk
// reported as pushed.
func TestPodUpdateASuspensionKeepsAClaimOutOfTheRoundItLeft(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.inPod(testRoundPod)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)
	h.diesIntoPod(testOtherPod)

	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})

	h.wantState(StateSuspended)
	c := h.claim()
	if c.Locator != nil {
		t.Fatalf("the suspended claim records the process %+v; this test is about the claim whose locator the suspension took", c.Locator)
	}
	if c.UpdateLeftRound != testRound || c.UpdateSignalledAt != nil {
		t.Fatalf("the suspended claim left %q with answered=%t, want the round it left standing unanswered",
			c.UpdateLeftRound, c.UpdateSignalledAt != nil)
	}

	err := h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: testRound, Head: "c0ffee"})

	if !errors.Is(err, ErrLeftPodRound) {
		t.Fatalf("the late answer after the suspension returned %v, want the refusal of a claim that left the round", err)
	}
	if !strings.Contains(err.Error(), testRound) {
		t.Errorf("refusal = %q, want it to name the round %q the answer carried", err, testRound)
	}
	if c := h.wantRound(testRound); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("the claim after the refused answer = %+v, want nothing of that answer recorded", c)
	}
}

// Only the round's end forgets the round a claim left, and it forgets it with every other field
// that round wrote. The claim's part in a round that is over says nothing about the round the
// sweep opens next over the pod the claim runs in now, which it takes its full part in.
func TestPodUpdateTheRoundsEndClearsTheRoundTheClaimLeft(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.inPod(testRoundPod)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)
	h.diesIntoPod(testOtherPod)
	if c := h.claim(); c.UpdateLeftRound != testRound {
		t.Fatalf("the claim in the new pod left %q, want the round %q", c.UpdateLeftRound, testRound)
	}

	h.must(PodUpdateEnded{Claim: h.token, Round: testRound})

	if c := h.wantRound(""); c.UpdateLeftRound != "" {
		t.Fatalf("the claim after the round ended left %q, want the record cleared with the round", c.UpdateLeftRound)
	}

	h.openUpdate(PodRound(testOtherPod), testReasons)
	h.signalUpdate("c0ffee")

	if c := h.wantRound(PodRound(testOtherPod)); c.UpdateSignalledAt == nil || c.UpdateHead != "c0ffee" {
		t.Fatalf("the claim in the round of its own pod = %+v, want its answer counted", c)
	}
}

// The round's end drops the ask the worker never read, and the question a restart armed for that
// ask goes with it. A daemon that comes back with the ask pending and unconfirmed marks it
// askFirst — the agent may already be in the turn an earlier daemon's prompt started — and the
// round's end lands before any sweep can ask, so the end is what drops the ask (leaveRound).
// Left armed over nothing pending, that question would make the claim's next task the running
// turn's: confirmed by a turn of another cause, never prompted, and retired unsent by that
// turn's end, so the worker would never receive the task at all.
func TestPodUpdateTheRoundsEndLeavesNoQuestionArmedForTheNextTask(t *testing.T) {
	const task = "review the pull request"
	h := updateHarness(t, PodUpdateContract)
	h.inPod(testRoundPod)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)

	h.restart()
	if !h.m.askFirst {
		t.Fatal("the restored claim has nothing to ask its agent; this test needs the question a restart leaves open")
	}

	h.must(PodUpdateEnded{Claim: h.token, Round: testRound})

	h.wantRound("")
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending = %+v, want the ask nothing read dropped with the round", p)
	}

	// A turn of another cause — an Envoy notice, a person's steer — is running, one this daemon
	// never saw begin, and the workflow hands the claim the task of its next phase.
	h.conn.SetStreaming(true)
	h.must(RequestDeliver{Claim: h.token, Task: task})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	sent := h.wantPrompts(2)[1]
	if sent.Message != task {
		t.Errorf("the second prompt carried %q, want the next task %q", sent.Message, task)
	}
}

// The same question, armed by a restart after the drop itself: the round's end dropped the ask
// and the write of that drop failed, which leaves the store holding the round and its ask
// exactly as a daemon that died between the two writes does. The restart restores both and marks
// the ask askFirst, and the end the next sweep posts drops it for real.
func TestPodUpdateARestartAfterAFailedDropLeavesNoQuestionArmedForTheNextTask(t *testing.T) {
	const task = "review the pull request"
	h := updateHarness(t, PodUpdateContract)
	h.inPod(testRoundPod)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)

	h.store.fail("RetireDelivery", errBoom)
	if err := h.handle(PodUpdateEnded{Claim: h.token, Round: testRound}); !errors.Is(err, errBoom) {
		t.Fatalf("the round's end over a failing retirement returned %v, want the store's error", err)
	}
	h.store.fail("RetireDelivery", nil)

	h.restart()
	if !h.m.askFirst {
		t.Fatal("the restored claim has nothing to ask its agent; this test needs the question a restart leaves open")
	}

	h.must(PodUpdateEnded{Claim: h.token, Round: testRound})

	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending = %+v, want the ask nothing read dropped with the round", p)
	}

	h.conn.SetStreaming(true)
	h.must(RequestDeliver{Claim: h.token, Task: task})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	sent := h.wantPrompts(2)[1]
	if sent.Message != task {
		t.Errorf("the second prompt carried %q, want the next task %q", sent.Message, task)
	}
}

// A round that replaces another drops the old round's ask — one an older daemon queued as a task
// — and the question a restart armed for that ask goes in the same step. The agent is in a turn
// of another cause here, so the answer moves the claim to working. A question left armed would
// make the claim's next task the turn's instead — confirmed by it, never prompted, and retired by
// its end — and the worker would never receive that task at all. The new round's own ask is the
// sweep's, on the role's topic, so the switch leaves the publish mark clear.
func TestPodUpdateARoundThatReplacesAnotherLeavesNoQuestionArmedForTheNextTask(t *testing.T) {
	const task = "review the pull request"
	h := updateHarness(t, PodUpdateContract)
	h.inPod(testRoundPod)
	h.openUpdate(testRound, testReasons)
	h.leftAsk(testRound)
	h.wantPrompts(1)

	h.restart()
	if !h.m.askFirst {
		t.Fatal("the restored claim has nothing to ask its agent; this test needs the question a restart leaves open")
	}

	second := PodRound(testOtherPod)
	h.conn.SetStreaming(true)
	h.openUpdate(second, testReasons)

	h.wantState(StateWorking)
	if c := h.wantRound(second); c.Pending != nil || c.UpdateTaskedAt != nil {
		t.Fatalf("claim in the round that replaced the other = %+v, want the old ask dropped and the publish mark clear", c)
	}

	h.must(RequestDeliver{Claim: h.token, Task: task})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	sent := h.wantPrompts(2)[1]
	if sent.Message != task {
		t.Errorf("the second prompt carried %q, want the next task %q", sent.Message, task)
	}
}

// An open round holds a task back from its worker only once that worker has answered it
// (sendPending's answeredOpenRound), so only then is a task no work of the claim's. A claim whose
// answer a turn of its own took back (takeBackSignal) is a claim the daemon will hand its task to
// at this very turn's end, and INFRA-264 is what refusing its completion looks like — a tester
// whose first turn an Envoy message started ran its whole phase and was answered that it belonged
// to no run.
//
// The third shape, an open round nobody has answered with a phase task pending, holds nothing
// back either: the hold reads the worker's answer alone, and the round's ask is a message on the
// role's topic rather than a task the claim holds (TestPodUpdateLeavesTheTaskAWorkingClaimHolds).
func TestPodUpdateAnOpenRoundHoldsATaskOnlyFromAWorkerThatAnsweredIt(t *testing.T) {
	const task = "review the pull request"
	for _, tc := range []struct {
		name string
		// answer is what the worker does with the round before the task is delivered: an answer,
		// or an answer a turn of its own takes back.
		answer   string
		wantRun  uint64
		wantHeld bool
	}{
		{name: "a round whose answer a turn took back", answer: "withdrawn", wantRun: 7},
		{name: "a round the worker answered", answer: "given", wantRun: 0, wantHeld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.reach(StateReady)
			h.openUpdate(testRound, testReasons)
			h.leftAsk(testRound)
			h.wantPrompts(1)
			// The worker reads the round's ask in a turn of its own and answers in it; the end of
			// that turn retires the ask, so the phase's task is delivered behind it.
			h.must(StreamTurnStart{Claim: h.token})
			h.signalUpdate("c0ffee")
			h.must(StreamTurnEnd{Claim: h.token})
			h.must(RequestDeliver{Claim: h.token, Task: task, Generation: 7})
			if tc.answer == "withdrawn" {
				// A turn of the worker's own — an Envoy message's — takes the answer back on its
				// way in, since the work that answer spoke for may have moved since.
				h.must(StreamTurnStart{Claim: h.token})
			}

			claim := h.claim()
			if claim.openRoundHoldsTask() != tc.wantHeld {
				t.Fatalf("openRoundHoldsTask = %v, want %v: the hold is the worker's answer, not the round's being open",
					claim.openRoundHoldsTask(), tc.wantHeld)
			}
			if run := claim.ServingRun(); run != tc.wantRun {
				t.Fatalf("the claim serves run %d, want %d: a completion of this turn is judged by it", run, tc.wantRun)
			}
			if p := claim.Pending; p == nil || p.Task != task {
				t.Fatalf("pending = %+v, want the task %q, which no case here consumes", p, task)
			}
		})
	}
}
