package supervise

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// busyAnswer is Oh My Pi's refusal of a prompt that reaches a session already in a prompt
// (agentBusy).
const busyAnswer = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."

// busyTurnBegunLine is the line the agent_start of a turn a busy answer recorded writes in place
// of a second start line (busyTurnBegun).
const busyTurnBegunLine = "supervise: the turn a busy answer recorded has begun"

// marks reads the machine's two marks of the turn it is in, under its lock: a send's goroutine
// may be handling its outcome.
func (h *harness) marks() (busyTurn, askFirst bool) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.busyTurn, h.m.askFirst
}

// busyRecorded delivers the task of run 7 to a ready worker whose agent is in a turn of its own,
// one whose start the daemon never read, and has Oh My Pi refuse the send busy.
func (h *harness) busyRecorded() {
	h.t.Helper()
	h.conn.SetStreaming(true)
	h.conn.RefusePrompt(busyAnswer)
	h.must(RequestDeliver{Claim: h.token, Task: "the task", Generation: 7})
	h.wantPrompts(1)
	h.wantState(StateWorking)
}

// A worker in a turn of its own — an Envoy notice's, say, whose start the daemon never read — is
// sent its task once, and Oh My Pi answers that send busy. The answer records the claim working
// (recordBusyTurn), so nothing sends the task into that turn again: not the runtime's sweep, which
// asks the agent whether the turn is over and hears that it is still streaming (askTurnOver), not
// the shim's hello, and not the send's own acknowledgement when it lands behind its busy answer
// (fenceOutcome). The next send is the one the turn's end makes. Before, the claim stayed ready,
// and every sweep, hello and fenced acknowledgement sent the task into the busy agent again, about
// every 2.5 seconds for the length of the turn (dispatch://INFRA-3276 R4).
//
// The daemon learns of the busy answer at three places, and each is driven: the send refused
// outright (refused), and the acknowledged prompt answered busy, by a late refusal (lateRefused) or
// by its result (promptResulted), handled after the acknowledgement or before it.
func TestABusyAnswerHoldsTheTaskUntilTheTurnEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		// answer delivers the task to the ready worker and has Oh My Pi answer its send busy. It
		// returns the connection the task's sends went through, whose prompts are the sends.
		answer func(t *testing.T, h *harness) *fake.Conn
	}{
		{name: "the send refused busy", answer: func(t *testing.T, h *harness) *fake.Conn {
			h.busyRecorded()
			return h.conn
		}},
		{name: "the acknowledged prompt refused busy", answer: func(t *testing.T, h *harness) *fake.Conn {
			h.conn.SetStreaming(true)
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			sent := h.wantPrompts(1)[0].DeliveryID
			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: sent, Error: busyAnswer})
			return h.conn
		}},
		{name: "the acknowledged prompt's result answering busy", answer: func(t *testing.T, h *harness) *fake.Conn {
			h.conn.SetStreaming(true)
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			sent := h.wantPrompts(1)[0].DeliveryID
			h.must(StreamPromptResult{Claim: testToken, DeliveryID: sent, Error: busyAnswer})
			return h.conn
		}},
		{name: "the busy answer handled before its own acknowledgement", answer: func(t *testing.T, h *harness) *fake.Conn {
			gated := newGatedConn()
			gated.SetStreaming(true)
			h.conns.Register(testToken, gated)
			if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Generation: 7}); err != nil {
				t.Fatal(err)
			}
			sent := waitFor(t, "the send", gated.entered)
			// The stream reads the busy answer while the send's goroutine is still waiting on
			// its acknowledgement: nothing orders the two.
			if err := h.m.Handle(h.ctx, StreamLateRefusal{Claim: testToken, DeliveryID: sent, Error: busyAnswer}); err != nil {
				t.Fatal(err)
			}
			// The acknowledgement lands now, under an id the answer took the task back from, and
			// is fenced out. The gate lets this send through and every later one at once, so a
			// send the fence made would be counted rather than held.
			for range 4 {
				gated.release <- struct{}{}
			}
			h.m.Wait()
			return gated.Conn
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			conn := tc.answer(t, h)

			if sent := conn.Prompts(); len(sent) != 1 || sent[0].Message != "the task" {
				t.Fatalf("sends into the turn after the busy answer = %d (%+v), want the one send of the task it answered", len(sent), sent)
			}

			// The turn goes on, and every trigger that used to send the task again comes and goes.
			h.observe(runtime.Alive)
			h.must(StreamHello{Claim: testToken, Generation: h.generation()})
			h.observe(runtime.Alive)
			if sent := conn.Prompts(); len(sent) != 1 {
				t.Fatalf("sends into the turn = %d (%+v), want only the one the busy answer refused", len(sent), sent)
			}
			h.wantState(StateWorking)
			if stored := h.store.load(testToken); stored.State != StateWorking {
				t.Fatalf("the stored claim is %s after the busy answer, want it recorded working", stored.State)
			}
			if p := h.pending(); p.Task != "the task" || !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
				t.Fatalf("pending in the turn = %+v, want the task waiting unread", p)
			}
			h.wantBudgets(Budgets{})

			// The turn ends; its agent_end is the first frame of it the daemon reads.
			conn.FailPrompt(nil)
			conn.SetStreaming(false)
			h.mustQuiet(StreamTurnEnd{Claim: testToken})
			sent := conn.Prompts()
			if len(sent) != 2 || sent[1].Message != "the task" || sent[1].DeliveryID != h.pending().ID {
				t.Fatalf("sends after the turn ended = %+v, want the task sent once more, under its pending id %s", sent, h.pending().ID)
			}
			if starts, ends := h.logs.lines(turnStartedLine), h.logs.lines(turnEndedLine); len(starts) != 1 || len(ends) != 1 {
				t.Errorf("turn lines = %d starts (%v) and %d ends (%v), want the one turn bracketed once", len(starts), starts, len(ends), ends)
			}
		})
	}
}

// A busy answer can name an earlier prompt of the task, one whose wait for its turn already ran
// out (noTurn) and took the task back under a new id: Oh My Pi answers each prompt once, and that
// answer can come after the wait has moved on. It is the same statement that the agent's session
// is in a prompt, so a ready claim is recorded working on it as on the busy answer to the pending
// prompt (waitedOutAnswer, recordBusyTurn): no sweep or hello sends the task into the turn the
// agent named, and the next send is the one that turn's end makes. Before, the claim stayed ready
// and the next sweep sent the task into the busy agent. The wait's own charge stands, and nothing
// more is charged.
//
// Where a later prompt of the task is outstanding, the turn the agent is in is that prompt's
// (Machine.outstanding): the claim is recorded working all the same, nothing more is sent, and that
// turn's start confirms the task and its result settles it.
func TestABusyAnswerToAWaitedOutPromptHoldsTheTaskUntilTheTurnEnds(t *testing.T) {
	// waitedOut delivers the task of run 7 to a ready worker whose agent is in a prompt, has Oh
	// My Pi acknowledge it, and lets the wait for its turn run out. It returns the prompt's id.
	waitedOut := func(t *testing.T, h *harness) string {
		t.Helper()
		h.conn.SetStreaming(true)
		h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
		first := h.wantPrompts(1)[0].DeliveryID
		h.advance(testRPC)
		if p := h.pending(); p.ID == first {
			t.Fatalf("pending after the wait ran out = %+v, want the task back under an id of its own", p)
		}
		h.wantState(StateReady)
		h.wantBudgets(Budgets{PromptFailures: 1})
		return first
	}
	// noSendInto drives every trigger that used to send the task again, and holds the claim to
	// the sends it had before them.
	noSendInto := func(t *testing.T, h *harness, sends int) {
		t.Helper()
		h.observe(runtime.Alive)
		h.must(StreamHello{Claim: testToken, Generation: h.generation()})
		h.observe(runtime.Alive)
		if sent := h.prompts(); len(sent) != sends {
			t.Fatalf("sends into the turn = %d (%+v), want the %d made before the busy answer", len(sent), sent, sends)
		}
		h.wantState(StateWorking)
		h.wantBudgets(Budgets{PromptFailures: 1})
	}

	for _, tc := range []struct {
		name   string
		answer func(h *harness, first string)
	}{
		{name: "its result answering busy", answer: func(h *harness, first string) {
			h.must(StreamPromptResult{Claim: testToken, DeliveryID: first, Error: busyAnswer})
		}},
		{name: "a late refusal answering busy", answer: func(h *harness, first string) {
			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: first, Error: busyAnswer})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			first := waitedOut(t, h)

			tc.answer(h, first)
			if got := h.outstanding(); len(got) != 0 {
				t.Fatalf("outstanding after the busy answer = %v, want none: the agent answered %s", got, first)
			}
			noSendInto(t, h, 1)
			if p := h.pending(); !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
				t.Fatalf("pending in the turn = %+v, want the task waiting unread", p)
			}

			h.conn.SetStreaming(false)
			h.mustQuiet(StreamTurnEnd{Claim: testToken})
			sent := h.prompts()
			if len(sent) != 2 || sent[1].Message != "the task" || sent[1].DeliveryID != h.pending().ID {
				t.Fatalf("sends after the turn ended = %+v, want the task sent once more, under its pending id %s", sent, h.pending().ID)
			}
		})
	}

	t.Run("its result answering busy while a later prompt is outstanding", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		first := waitedOut(t, h)
		h.observe(runtime.Alive) // the next sweep sends the task again, as a silent prompt's bound has it
		resend := h.wantPrompts(2)[1].DeliveryID

		h.must(StreamPromptResult{Claim: testToken, DeliveryID: first, Error: busyAnswer})
		if got := h.outstanding(); !slices.Equal(got, []string{resend}) {
			t.Fatalf("outstanding after the busy answer = %v, want the later prompt %s alone", got, resend)
		}
		noSendInto(t, h, 2)

		h.must(StreamTurnStart{Claim: testToken}) // the later prompt's turn
		if p := h.pending(); p.ConfirmedAt.IsZero() {
			t.Fatalf("pending in the later prompt's turn = %+v, want it confirmed", p)
		}
		h.must(StreamTurnEnd{Claim: testToken}) // the later prompt's result follows its end
		if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
			t.Fatalf("after the later prompt's turn: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
		}
		h.wantPrompts(2)
	})
}

// The control: a worker in no turn is sent its task the moment it is delivered, and the
// acknowledgement leaves the claim ready, waiting for the turn the prompt starts; no busy answer
// came, so nothing is recorded. That turn confirms the task and its end retires it, and the next
// task the idle worker is given goes at once too.
func TestAWorkerInNoTurnIsSentAtOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	if sent := h.wantPrompts(1)[0]; sent.Message != "the task" || sent.DeliveryID != h.pending().ID {
		t.Fatalf("sent %+v, want the task at once under its pending id %s", sent, h.pending().ID)
	}
	h.wantState(StateReady)
	if busy, _ := h.marks(); busy {
		t.Fatalf("marks after the acknowledgement: busy turn %v, want none", busy)
	}
	if lines := h.logs.lines("busy answer"); len(lines) != 0 {
		t.Fatalf("a busy record was made with no busy answer: %v", lines)
	}

	h.must(StreamTurnStart{Claim: testToken})
	if p := h.pending(); p.ConfirmedAt.IsZero() {
		t.Fatalf("pending in the prompt's turn = %+v, want it confirmed", p)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the turn: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
	h.wantState(StateIdle)
	h.wantPrompts(1)

	h.must(RequestDeliver{Claim: testToken, Task: "the next task", Generation: 8})
	if sent := h.wantPrompts(2)[1]; sent.Message != "the next task" {
		t.Fatalf("sent %+v to the idle worker, want the next task at once", sent)
	}
}

// A claim a busy answer recorded working may never read a frame of the turn it stands for, so the
// runtime's sweep asks the agent at each pass whether that turn is over (observe, askTurnOver).
// While the agent says it is streaming — at the pin, from a prompt's admission through its turn,
// pre-turn work included — the claim stays working and the question stays armed. Once it says it
// is in no prompt, the turn ends through turnEnded at that sweep, and the task goes. An ask the
// agent leaves unanswered says nothing, and is asked again. A prompt of the task the agent
// acknowledged and never answered goes with the turn (Machine.outstanding), so it cannot hold the
// task.
func TestABusyRecordedClaimEndsAtTheSweepWhenTheAgentStopsStreaming(t *testing.T) {
	t.Run("an agent still streaming stays working", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.busyRecorded()

		for range 3 {
			h.observe(runtime.Alive)
		}
		h.wantState(StateWorking)
		h.wantPrompts(1)
		if ends := h.logs.lines(turnEndedLine); len(ends) != 0 {
			t.Fatalf("turn ends = %v, want none: the agent is still streaming", ends)
		}
		if busy, _ := h.marks(); !busy {
			t.Fatalf("marks after three sweeps of a streaming agent: busy turn %v, want it standing", busy)
		}
	})

	t.Run("an agent in no prompt ends the turn at the sweep", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.busyRecorded()
		h.observe(runtime.Alive)
		h.wantState(StateWorking)

		h.conn.SetStreaming(false)
		h.conn.FailPrompt(nil)
		h.observe(runtime.Alive)

		if lines := h.logs.lines("the turn its busy answer recorded is over"); len(lines) != 1 {
			t.Fatalf("lines of the sweep's end = %v, want one", lines)
		}
		if ends := h.logs.lines(turnEndedLine); len(ends) != 1 {
			t.Fatalf("turn ends = %v, want the one the sweep found", ends)
		}
		if c := h.claim(); c.LastTurnEnd.IsZero() {
			t.Fatal("the turn the sweep ended has no end time")
		}
		if sent := h.wantPrompts(2)[1]; sent.Message != "the task" || sent.DeliveryID != h.pending().ID {
			t.Fatalf("sent %+v at the sweep's end of the turn, want the task under its pending id %s", sent, h.pending().ID)
		}
		h.wantState(StateIdle)
		if busy, _ := h.marks(); busy {
			t.Fatalf("marks after the end: busy turn %v, want none", busy)
		}
	})

	t.Run("an ask the agent leaves unanswered is asked again", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.busyRecorded()

		h.conn.FailGetState(errors.New("the shim did not answer get_state in time"))
		h.observe(runtime.Alive)
		h.wantState(StateWorking)
		h.wantPrompts(1)
		if lines := h.logs.lines("did not say whether the turn its busy answer recorded is over"); len(lines) != 1 {
			t.Fatalf("lines of the unanswered ask = %v, want one", lines)
		}

		h.conn.FailGetState(nil)
		h.conn.SetStreaming(false)
		h.conn.FailPrompt(nil)
		h.observe(runtime.Alive)
		h.wantState(StateIdle)
		h.wantPrompts(2)
	})

	t.Run("an outstanding prompt of the task goes with the turn", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
		first := h.wantPrompts(1)[0].DeliveryID
		h.advance(testRPC)       // the wait for the turn runs out, and the prompt stays outstanding
		h.observe(runtime.Alive) // the sweep sends the task again
		resend := h.wantPrompts(2)[1].DeliveryID
		h.must(StreamPromptResult{Claim: testToken, DeliveryID: resend, Error: busyAnswer})
		h.wantState(StateWorking)
		if got := h.outstanding(); !slices.Equal(got, []string{first}) {
			t.Fatalf("outstanding after the busy answer = %v, want the first prompt %s", got, first)
		}

		// The first prompt's turn never comes: the agent is in no prompt at the next sweep.
		h.observe(runtime.Alive)
		h.wantState(StateIdle)
		third := h.wantPrompts(3)[2].DeliveryID
		if third != h.pending().ID {
			t.Fatalf("the send at the sweep's end went under %s, want the pending id %s", third, h.pending().ID)
		}
		if got := h.outstanding(); slices.Contains(got, first) {
			t.Fatalf("outstanding after the sweep ended the turn = %v, want the first prompt %s gone with it", got, first)
		}
	})
}

// The record is durable (durableWrite): the daemon hears the claim working once (OnDurableState),
// as it hears a turn's start, and a daemon restarted inside the turn restores the claim working
// with the question armed (askFirst), so its first sweep asks the agent rather than prompting it.
// The agent answers that it is still streaming, which disarms the question and leaves the claim
// in the turn, and the turn's own end sends the task.
func TestABusyRecordedClaimIsRestoredWorkingAndAskedAgain(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	var mu sync.Mutex
	var reported []ClaimState
	h.m.OnDurableState(func(_ Claim, state ClaimState) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, state)
	})
	h.busyRecorded()
	mu.Lock()
	heard := slices.Clone(reported)
	mu.Unlock()
	if !slices.Equal(heard, []ClaimState{StateWorking}) {
		t.Fatalf("durable states reported = %v, want the one working the busy answer recorded", heard)
	}

	h.restart()
	h.wantState(StateWorking)
	if _, askFirst := h.marks(); !askFirst {
		t.Fatal("the restored claim has no question armed for its agent")
	}
	h.observe(runtime.Alive) // still streaming: the claim stays working, and nothing is sent
	h.wantState(StateWorking)
	h.wantPrompts(1)
	if _, askFirst := h.marks(); askFirst {
		t.Fatalf("marks after the sweep: question armed %v, want the question asked and answered streaming", askFirst)
	}

	h.conn.SetStreaming(false)
	h.conn.FailPrompt(nil)
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)
	if sent := h.wantPrompts(2)[1]; sent.Message != "the task" {
		t.Fatalf("sent %+v at the turn's end, want the task", sent)
	}
}

// The agent_start of a turn a busy answer recorded is the start of that turn, and the first frame
// of it the daemon reads (turnStartedWhileBusy). It is decided and recorded as any start is — the
// moment it began and the turn's step — but it writes no second start line, since the record wrote
// the turn's one: one start line and one end line bracket the turn, and the start writes a line
// of its own instead. The record dated the turn from the busy answer,
// its first word of it (recordBusyTurn), and the start dates it again at its own moment. It
// answers the record's question, so the sweep stops asking, and the turn's agent_end ends it. A
// turn of the agent's own confirms nothing.
func TestTheStartOfATurnABusyAnswerRecordedWritesNoSecondStartLine(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.busyRecorded()
	recorded := h.clock.Now()
	if step := h.store.step(testToken); !step.TurnStartedAt.Equal(recorded) {
		t.Fatalf("the step after the busy answer starts its turn at %v, want the record's %v", step.TurnStartedAt, recorded)
	}
	if start := h.claim().LastTurnStart; !start.Equal(recorded) {
		t.Fatalf("the claim's turn began at %v after the busy answer, want the record's %v", start, recorded)
	}
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: testToken})
	started := h.clock.Now()

	if starts := h.records.of(turnStartedLine); len(starts) != 1 {
		t.Fatalf("start lines = %d, want only the one the busy answer's record wrote", len(starts))
	}
	begun := h.records.of(busyTurnBegunLine)
	if len(begun) != 1 {
		t.Fatalf("lines of the recorded turn's start = %d, want one", len(begun))
	}
	if delivery, named := begun[0].attr("delivery"); named {
		t.Errorf("the start of a turn of the agent's own names delivery %v, want none", delivery)
	}
	c := h.claim()
	if !c.LastTurnStart.Equal(started) {
		t.Errorf("the claim's turn began at %v, want the start's %v", c.LastTurnStart, started)
	}
	if p := c.Pending; p == nil || !p.ConfirmedAt.IsZero() {
		t.Errorf("pending after a start of the agent's own turn = %+v, want it waiting unconfirmed", p)
	}
	if step := h.store.step(testToken); !step.TurnStartedAt.Equal(started) {
		t.Errorf("the step after the start begins its turn at %v, want the start's %v", step.TurnStartedAt, started)
	}
	if busy, _ := h.marks(); busy {
		t.Fatalf("marks after the start: busy turn %v, want none: the daemon read this start", busy)
	}

	// The start answered the record's question: the sweep asks nothing of the turn any more.
	h.conn.SetStreaming(false)
	h.observe(runtime.Alive)
	h.wantState(StateWorking)
	if ends := h.logs.lines(turnEndedLine); len(ends) != 0 {
		t.Fatalf("turn ends after the sweep = %v, want none: the turn's own end ends it now", ends)
	}

	h.conn.FailPrompt(nil)
	h.must(StreamTurnEnd{Claim: testToken})
	if ends := h.records.of(turnEndedLine); len(ends) != 1 {
		t.Fatalf("end lines = %d, want the one turn's", len(ends))
	}
	h.wantState(StateIdle)
	h.wantPrompts(2)
}

// While a prompt of the task Oh My Pi acknowledged is still outstanding, the turn a later send's
// busy answer refused that send for is that prompt's (Machine.outstanding). The busy answer records
// the claim working all the same, and the turn's agent_start, when it comes, confirms the task as a
// start of a ready claim would (turnStartedWhileBusy, beginTurn): its line names the task, and the
// end line names the task the start named. The prompt's own result settles the task, and nothing
// is sent after it.
func TestTheStartOfATurnABusyAnswerRecordedConfirmsAnOutstandingPromptsTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	first := h.wantPrompts(1)[0].DeliveryID
	h.advance(testRPC)
	h.observe(runtime.Alive)
	resend := h.wantPrompts(2)[1].DeliveryID
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: resend, Error: busyAnswer})
	h.wantState(StateWorking)
	task := h.pending().ID

	h.must(StreamTurnStart{Claim: testToken}) // the first prompt's turn, late
	if p := h.pending(); p.ID != task || p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the start = %+v, want %s confirmed by the outstanding prompt's turn", p, task)
	}
	if run := h.claim().ServingRun(); run != 7 {
		t.Fatalf("during the turn the claim serves run %d, want the task's 7", run)
	}
	begun := h.records.of(busyTurnBegunLine)
	if len(begun) != 1 {
		t.Fatalf("lines of the recorded turn's start = %d, want one", len(begun))
	}
	if delivery, named := begun[0].attr("delivery"); !named || delivery.String() != task {
		t.Fatalf("the start's line names delivery %v (named %v), want the task %s it confirms", delivery, named, task)
	}
	if busy, _ := h.marks(); busy {
		t.Fatalf("marks after the start: busy turn %v, want none: it is the outstanding prompt's turn", busy)
	}
	if starts := h.records.of(turnStartedLine); len(starts) != 1 {
		t.Fatalf("start lines = %d, want only the record's", len(starts))
	}

	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	ends := h.records.of(turnEndedLine)
	if len(ends) != 1 {
		t.Fatalf("end lines = %d, want one", len(ends))
	}
	if delivery, named := ends[0].attr("delivery"); !named || delivery.String() != task {
		t.Errorf("the end line names delivery %v (named %v), want the task %s its start confirmed", delivery, named, task)
	}
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: first, AgentInvoked: true})
	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the first prompt's result: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
	h.wantPrompts(2)
	if lines := h.logs.lines("dropped a stale event"); len(lines) != 0 {
		t.Errorf("an event was dropped as stale: %v", lines)
	}
}
