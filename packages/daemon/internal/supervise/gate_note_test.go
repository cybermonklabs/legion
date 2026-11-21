package supervise

// A merger's task carries the design gate's state at its send (dispatch://INFRA-2040 R7, D15 of
// its plan). The daemon answers what that state is (Deps.GateNote, internal/daemon's gateNote);
// these tests stand a fake in for that answer and pin what the machine does with it: the prompt
// opens with the note the answer gives at that send, a send after the answer changed carries the
// new note, the note is never stored with the delivery, each send that carries one says so on a
// line of its own, and no other role's delivery is put to the answer.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// mergerToken is the merger's claim of the harness's issue.
const mergerToken claim.Token = "legion-LEGION-209-merger"

// gateNoteLine is the line each send of a merger's task that carries a note writes (startSend).
const gateNoteLine = "supervise: the merger's task carries the design gate's state"

var (
	// closedGateNote and openGateNote are the two notes the fake answer gives, as the workflow
	// writes them (workflow.MergerGateNote).
	closedGateNote = GateNote{Text: "The design gate of LEGION-208 closed: version 2 of its spec document (spec-artifact) is with its reviewer. " +
		"Merge nothing, enqueue nothing, and take a pull request you queued back out of the merge queue.",
		State: "closed", Step: "spec", Version: 2}
	openGateNote = GateNote{Text: "The design gate of LEGION-208 stands open: every gated step that someone registered is approved.",
		State: "open", Step: "spec", Version: 2}
)

// gateAnswer is a fake Deps.GateNote: it gives note for every delivery it is asked of, and keeps
// the id of each. The machine asks it from its own goroutine, so it is locked.
type gateAnswer struct {
	mu    sync.Mutex
	note  GateNote
	asked []string
}

func (a *gateAnswer) of(_ context.Context, _ Claim, d Delivery) (GateNote, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, d.ID)
	return a.note, nil
}

func (a *gateAnswer) give(note GateNote) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.note = note
}

func (a *gateAnswer) questions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asked...)
}

// designGateHarness is a machine over c whose design gate's answer is answer, with c brought up
// to ready.
func designGateHarness(t *testing.T, c Claim, answer *gateAnswer) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.token = c.Token
	h.deps.GateNote = answer.of
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateReady)
	return h
}

// mergerClaim is the queued claim of the harness's issue's merger.
func mergerClaim() Claim {
	c := queuedClaim()
	c.Token, c.Role = mergerToken, claim.RoleMerger
	return c
}

// A merger's task is sent with the note the design gate's answer gives at that send, and the same
// task sent again after the gate moved carries the new note. The merger is sent its task while the
// answer says the gate is closed: the prompt opens with the closed note and then the task, and the
// send's line names the delivery, the state, the step and the version. The answer then says the
// gate stands open, and a death in the task's turn takes the task back: the relaunched agent's
// ready sends it again, opening with the open note and no closed one. The note is never stored:
// the delivery the store holds is the task as it was queued.
func TestAMergersTaskCarriesTheDesignGateNoteOfEachSend(t *testing.T) {
	answer := &gateAnswer{note: closedGateNote}
	h := designGateHarness(t, mergerClaim(), answer)
	const task = "Merge the head the review approved."
	h.must(RequestDeliver{Claim: h.token, Task: task, Generation: 5, Entry: 3})

	first := h.wantPrompts(1)[0]
	if want := closedGateNote.Text + "\n\n" + task; first.Message != want {
		t.Fatalf("the merger was sent %q, want %q: the closed note, then the task", first.Message, want)
	}
	if lines := h.logs.lines(gateNoteLine, "issue=LEGION-209", "delivery="+first.DeliveryID, "state=closed", "step=spec",
		"version=2"); len(lines) != 1 {
		t.Fatalf("the send was logged %d times as carrying the gate's state, want once; the log is:\n%s", len(lines), h.logs.String())
	}
	if stored := h.store.load(h.token).Pending; stored == nil || stored.Task != task || stored.ID != first.DeliveryID {
		t.Fatalf("the store holds %+v, want the task as it was queued, under the id it was sent with (%s)", stored, first.DeliveryID)
	}

	h.must(StreamTurnStart{Claim: h.token})
	answer.give(openGateNote)
	h.observe(runtime.Gone)
	h.relaunched()
	again := h.wantPrompts(2)[1]
	if !strings.HasPrefix(again.Message, openGateNote.Text+"\n\n") || !strings.HasSuffix(again.Message, task) ||
		strings.Contains(again.Message, closedGateNote.Text) {
		t.Fatalf("after the gate moved the merger was sent %q, want the open note in front of its task and no closed note", again.Message)
	}
	if lines := h.logs.lines(gateNoteLine, "delivery="+again.DeliveryID, "state=open"); len(lines) != 1 {
		t.Fatalf("the second send was logged %d times as carrying the open gate, want once; the log is:\n%s", len(lines), h.logs.String())
	}
	if stored := h.store.load(h.token).Pending; stored == nil || stored.Task != task {
		t.Fatalf("the store holds %+v after the second send, want the task as it was queued", stored)
	}
	if asked := answer.questions(); len(asked) != 2 || asked[0] != first.DeliveryID || asked[1] != again.DeliveryID {
		t.Fatalf("the gate's answer was asked of %v, want once at each send: %s, then %s", asked, first.DeliveryID, again.DeliveryID)
	}
}

// A delivery of any other role is never put to the design gate's answer and carries no note:
// the gate holds only the merger's work.
func TestNoDesignGateNoteIsAskedOfANonMergersTask(t *testing.T) {
	answer := &gateAnswer{note: closedGateNote}
	h := designGateHarness(t, queuedClaim(), answer)
	const task = "Implement the change."
	h.must(RequestDeliver{Claim: h.token, Task: task, Generation: 5, Entry: 3})
	if sent := h.wantPrompts(1)[0]; sent.Message != task {
		t.Fatalf("the implementer was sent %q, want its task alone, %q", sent.Message, task)
	}
	if asked := answer.questions(); len(asked) != 0 {
		t.Fatalf("the gate's answer was asked of %v for the implementer's task, want never", asked)
	}
	if lines := h.logs.lines(gateNoteLine); len(lines) != 0 {
		t.Fatalf("the implementer's send was logged as carrying the gate's state: %q", lines)
	}
}

// The note goes in front of everything else a prompt says, as the last rewrite of the send
// (Delivery.message): in front of a task, in front of the sentence that a task's turn was
// interrupted, and in front of the daemon's own delivery of a relaunch's record, which a merger
// reads before it takes its phase up again. No note leaves the prompt as it was.
func TestTheDesignGateNoteOpensEveryPromptOfAMerger(t *testing.T) {
	merger := Claim{Role: claim.RoleMerger}
	for _, tc := range []struct {
		name     string
		delivery Delivery
		bare     string
	}{
		{name: "a task", delivery: Delivery{ID: "outbox:3", Task: "Merge it."}, bare: "Merge it."},
		{name: "a task taken back from its turn", delivery: Delivery{ID: "outbox:4", Task: "Merge it.", Interrupted: true},
			bare: interruptedTask("") + "Merge it."},
		{name: "the relaunch's record", delivery: Delivery{ID: relaunchTaskID(), Task: "the record"}, bare: "the record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := tc.delivery.message(merger, GateNote{}); got != tc.bare {
				t.Fatalf("with no note the %s is sent %q, want %q", tc.name, got, tc.bare)
			}
			if got, _ := tc.delivery.message(merger, closedGateNote); got != closedGateNote.Text+"\n\n"+tc.bare {
				t.Fatalf("the %s is sent %q, want the closed note in front of %q", tc.name, got, tc.bare)
			}
		})
	}
}
