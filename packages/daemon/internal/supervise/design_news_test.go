package supervise

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// queuedNews is what a reviewer's phase start carries of news of the approved design, as the start
// read it from the record (record.DeliveryNews, encoded JSON): the start's own text, two items, and
// the gate's opening. The machine carries it as the bytes the start wrote (Delivery.News).
const queuedNews = `{"task":"Review the change. Issue: LEGION-209. Phase: reviewing.",` +
	`"items":[{"issue":"LEGION-209","audience":"holder","step":"spec","artifact":"artifact-spec","from":3,"to":5},` +
	`{"issue":"LEGION-209","audience":"holder","step":"decisions","artifact":"artifact-decisions","from":1,"to":2}],` +
	`"openings":[{"issue":"LEGION-209","audience":"holder"}]}`

// queuedTask is the text the start queued with it, rendered when the start ran.
const queuedTask = "Review the change. Issue: LEGION-209. Phase: reviewing.\n\n" +
	"The design gate of LEGION-208 opened: a person approved version 5 of its spec document (artifact-spec)."

// designNewsAnswer is a Deps.DesignNews that answers task and news, and records each delivery it
// was asked about, in order.
type designNewsAnswer struct {
	task  string
	news  []byte
	asked []Delivery
}

func (a *designNewsAnswer) answer(_ context.Context, _ Claim, d Delivery) (string, []byte, error) {
	a.asked = append(a.asked, d)
	return a.task, a.news, nil
}

// newsHarness is a machine over the queued implementer claim whose Deps.DesignNews is answer.
func newsHarness(t *testing.T, answer *designNewsAnswer) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.DesignNews = answer.answer
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

// newsStart is the reviewer's phase start carrying queuedNews, from outbox row id.
func newsStart(id string, row int64) RequestDeliver {
	return RequestDeliver{Claim: testToken, Task: queuedTask, ID: id, Row: row, Phase: phase.Reviewing, Generation: 7,
		News: []byte(queuedNews)}
}

// A task that carries news of the approved design is sent as the record reads at its send, not as
// it read when the start was queued: the gate it names can have closed since, and the prompt must
// say so. The machine asks Deps.DesignNews about the delivery it is about to send, writes the
// answer to the store under the same id before the prompt goes, and prompts that text, so a
// restart reads the task this send carried and its confirmation raises what it named.
func TestATaskCarryingNewsIsSentAsTheRecordReadsAtItsSend(t *testing.T) {
	closed := "Review the change. Issue: LEGION-209. Phase: reviewing.\n\n" +
		"The design gate of LEGION-208 closed: version 6 of its spec document (artifact-spec) is with its reviewer."
	answer := &designNewsAnswer{task: closed, news: []byte(queuedNews)}
	h := newsHarness(t, answer)
	h.reach(StateReady)

	h.must(newsStart("outbox:41", 41))

	if len(answer.asked) != 1 || !bytes.Equal(answer.asked[0].News, []byte(queuedNews)) || answer.asked[0].Task != queuedTask {
		t.Fatalf("DesignNews was asked about %+v, want once, about the task as the start queued it", answer.asked)
	}
	if sent := h.wantPrompts(1)[0]; sent.DeliveryID != "outbox:41" || sent.Message != closed {
		t.Fatalf("prompt = %+v, want outbox:41 carrying the text DesignNews rendered at the send, %q", sent, closed)
	}
	stored, ok := h.store.delivery(testToken)
	if !ok || stored.ID != "outbox:41" || stored.Task != closed || !bytes.Equal(stored.News, []byte(queuedNews)) {
		t.Fatalf("stored delivery = %+v (held %t), want outbox:41 holding the text it was sent with", stored, ok)
	}
	if log := h.store.deliveryLog(); len(log) < 2 || !slices.Equal(log[:2], []string{"put outbox:41", "put outbox:41"}) {
		t.Fatalf("delivery writes = %v, want the queue's write, then the rendered task's under the same id", log)
	}
}

// An item DesignNews drops — a later approval replaced it, or another delivery's confirmation told
// it — goes from the delivery as well as from the text. The confirmation of the delivery is what
// raises each item it carries as taken, so a dropped item left on it would be recorded as told by a
// prompt that never named it.
func TestAnItemThatStopsStandingIsGoneFromTheDeliveryItsConfirmationWrites(t *testing.T) {
	narrowed := `{"task":"Review the change. Issue: LEGION-209. Phase: reviewing.",` +
		`"items":[{"issue":"LEGION-209","audience":"holder","step":"spec","artifact":"artifact-spec","from":3,"to":5}]}`
	text := "Review the change. Issue: LEGION-209. Phase: reviewing.\n\nThe approved design of LEGION-208 changed for LEGION-209."
	answer := &designNewsAnswer{task: text, news: []byte(narrowed)}
	h := newsHarness(t, answer)
	h.reach(StateReady)

	h.must(newsStart("outbox:41", 41))

	if sent := h.wantPrompts(1)[0]; sent.Message != text {
		t.Fatalf("prompt = %+v, want the text with the dropped item gone, %q", sent, text)
	}
	if p := h.pending(); !bytes.Equal(p.News, []byte(narrowed)) || p.Task != text {
		t.Fatalf("pending delivery = %+v, want it to carry only the item that still stands", p)
	}

	h.must(StreamTurnStart{Claim: testToken})

	stored, ok := h.store.delivery(testToken)
	if !ok || stored.ConfirmedAt.IsZero() || !bytes.Equal(stored.News, []byte(narrowed)) {
		t.Fatalf("stored delivery after the turn started = %+v (held %t), want it confirmed carrying only %s", stored, ok, narrowed)
	}
}

// A task queued for its news alone that has nothing left to say at its send — every item it
// carried stopped standing, and no opening is owed while the gate stands open — is retired rather
// than sent: a prompt saying nothing costs a model turn, and its confirmation would raise nothing.
// The claim stays as it was, ready for the next task.
func TestANewsOnlyTaskLeftWithNothingToSayIsRetiredUnsent(t *testing.T) {
	answer := &designNewsAnswer{}
	h := newsHarness(t, answer)
	h.reach(StateReady)

	h.must(newsStart("outbox:42", 42))

	if len(answer.asked) != 1 {
		t.Fatalf("DesignNews was asked %d times, want once, at the send", len(answer.asked))
	}
	h.wantPrompts(0)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending delivery = %+v, want the news-only task retired", p)
	}
	if log := h.store.deliveryLog(); len(log) == 0 || log[len(log)-1] != "retire outbox:42" {
		t.Fatalf("delivery writes = %v, want the retirement of outbox:42 last", log)
	}
	h.wantState(StateReady)
}

// A daemon restarted over a ready claim holding a task it may already have sent asks the agent,
// before anything else, whether the turn it is in is that task's (askFirst). A task queued for its
// news alone that has nothing left to say is retired before that question is asked of it, so the
// question is answered as every drop answers it (answerAskFirstAfterDrop): the agent's turn moves
// the claim to working, and nothing confirms the retired task, so no told version rises for news
// the record no longer owes.
func TestANewsOnlyTaskARestartRetiresAnswersTheQuestionTheRestartLeft(t *testing.T) {
	restored := queuedClaim()
	restored.State, restored.Generation = StateReady, 3
	restored.Session, restored.SessionFile = session, sessionFile
	restored.Locator = &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: testToken, Incarnation: "4242:77",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
	restored.Pending = &Delivery{ID: "outbox:45", Task: queuedTask, Phase: phase.Reviewing, Generation: 7, QueuedAt: epoch,
		Prompted: true, News: []byte(queuedNews)}
	answer := &designNewsAnswer{}
	h := newBareHarness(t)
	h.deps.DesignNews = answer.answer
	if err := h.store.PutClaim(h.ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}
	h.conns.Register(testToken, h.conn)
	h.conn.SetStreaming(true)
	h.start(restored)

	h.must(RequestReady{Claim: testToken, Generation: restored.Generation, Session: session})

	h.wantPrompts(0)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending delivery = %+v, want the news-only task retired", p)
	}
	if log := h.store.deliveryLog(); len(log) == 0 || log[len(log)-1] != "retire outbox:45" {
		t.Fatalf("delivery writes = %v, want the unconfirmed retirement of outbox:45 last", log)
	}
	h.wantState(StateWorking)
}

// restoredUnsentNews is a daemon restarted over a ready claim that holds a task carrying news of
// the approved design that no acknowledged prompt carried (Delivery.Carried empty): a task queued
// while the agent's turn ran, with the daemon stopped before that turn ended, or, where prompted
// says so, one an earlier daemon's send marked Prompted and never sent, its adoption of the working
// copy refused (runtime.ErrAdoptRefused) or its identity mint failed
// (runtime.ErrIdentityUnavailable). The restart arms its question over the task (askFirst), as over
// every unconfirmed task it restores. The agent's connection is registered, and the agent is in a
// turn of its own, an Envoy message's, so it answers a prompt busy.
func restoredUnsentNews(t *testing.T, answer *designNewsAnswer, prompted bool) *harness {
	t.Helper()
	restored := queuedClaim()
	restored.State, restored.Generation = StateReady, 3
	restored.Session, restored.SessionFile = session, sessionFile
	restored.Locator = &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: testToken, Incarnation: "4242:77",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
	restored.Pending = &Delivery{ID: "outbox:46", Task: queuedTask, Phase: phase.Reviewing, Generation: 7, QueuedAt: epoch,
		News: []byte(queuedNews), Prompted: prompted}
	h := newBareHarness(t)
	h.deps.DesignNews = answer.answer
	if err := h.store.PutClaim(h.ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}
	h.conns.Register(testToken, h.conn)
	h.conn.SetStreaming(true)
	h.conn.FailPrompt(busyRefusal())
	h.start(restored)
	return h
}

// After a restart, a task carrying news that no acknowledged prompt carried is never confirmed by a
// turn the agent started by itself. Its confirmation records the news as taken, and the agent never
// read it. So the question the restart left (askFirst) never takes the agent's turn for the task's,
// at either place it could: the send the ready makes, where the agent says it is in a turn, and a
// turn's start seen before anything was sent. That holds for a task an earlier daemon's send marked
// Prompted before its adoption or identity mint failed, which sent nothing. The task stays
// unconfirmed and is sent: the send the busy agent refuses records the claim working for the
// agent's own turn, and the end of that turn sends the task again, so the turn that starts on it
// confirms it.
func TestARestartNeverConfirmsANewsTaskNoPromptCarriedByATurnOfTheAgentsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// first is what reaches the restored claim first, and refused the prompts it leads to.
		first   Event
		refused int
		// prompted is whether an earlier daemon's send marked the task Prompted and failed before
		// its prompt went out.
		prompted bool
	}{
		{name: "the ready's send finds the agent in a turn of its own",
			first: RequestReady{Claim: testToken, Generation: 3, Session: session}, refused: 1},
		{name: "a turn of the agent's own starts before anything is sent",
			first: StreamTurnStart{Claim: testToken}},
		{name: "the ready's send finds the agent in a turn of its own, after a send whose adoption failed",
			first: RequestReady{Claim: testToken, Generation: 3, Session: session}, refused: 1, prompted: true},
		{name: "a turn of the agent's own starts after a send whose adoption failed",
			first: StreamTurnStart{Claim: testToken}, prompted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := &designNewsAnswer{task: queuedTask, news: []byte(queuedNews)}
			h := restoredUnsentNews(t, answer, tc.prompted)

			h.must(tc.first)

			if stored, ok := h.store.delivery(testToken); !ok || !stored.ConfirmedAt.IsZero() {
				t.Fatalf("stored delivery after %s = %+v (held %t), want outbox:46 unconfirmed: no prompt carried its news", tc.name, stored, ok)
			}
			h.wantPrompts(tc.refused)
			h.wantState(StateWorking)

			h.conn.FailPrompt(nil)
			h.conn.SetStreaming(false)
			h.mustQuiet(StreamTurnEnd{Claim: testToken})

			sent := h.wantPrompts(tc.refused + 1)[tc.refused]
			if sent.DeliveryID != h.pending().ID || sent.Message != queuedTask {
				t.Fatalf("the send at the end of the agent's own turn = %+v, want the news task %q under its pending id", sent, queuedTask)
			}
			h.must(StreamTurnStart{Claim: testToken})
			if stored, ok := h.store.delivery(testToken); !ok || stored.ConfirmedAt.IsZero() || !bytes.Equal(stored.News, []byte(queuedNews)) {
				t.Fatalf("stored delivery after the turn its send started = %+v (held %t), want it confirmed carrying its news", stored, ok)
			}
		})
	}
}

// A task carrying news is a task like any other in when it is sent: queued while the claim is in
// a turn, it waits for that turn's end, and nothing asks the record about it until then. The news
// it carries is rendered at the send that turn's end makes, not at the queue.
func TestATaskCarryingNewsQueuedInATurnWaitsForTheTurnsEnd(t *testing.T) {
	rendered := "Review the change. Issue: LEGION-209. Phase: reviewing.\n\nrendered at the send"
	answer := &designNewsAnswer{task: rendered, news: []byte(queuedNews)}
	h := newsHarness(t, answer)
	h.reach(StateWorking)
	before := len(h.prompts())

	h.must(newsStart("outbox:43", 43))

	if len(answer.asked) != 0 {
		t.Fatalf("DesignNews was asked about %+v while the claim worked a turn, want nothing asked before a send", answer.asked)
	}
	h.wantPrompts(before)
	if p := h.pending(); p.ID != "outbox:43" || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending delivery = %+v, want outbox:43 waiting unsent", p)
	}

	h.must(StreamTurnEnd{Claim: testToken})

	if len(answer.asked) != 1 {
		t.Fatalf("DesignNews was asked %d times after the turn ended, want once, at the send", len(answer.asked))
	}
	if sent := h.wantPrompts(before + 1)[before]; sent.DeliveryID != "outbox:43" || sent.Message != rendered {
		t.Fatalf("prompt = %+v, want outbox:43 carrying the text rendered at the turn's end", sent)
	}
}

// A task that carries no news is sent as it was queued, and nothing asks the record about it.
func TestATaskCarryingNoNewsIsNeverPutToDesignNews(t *testing.T) {
	answer := &designNewsAnswer{task: "never this"}
	h := newsHarness(t, answer)
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:44", Row: 44, Phase: phase.Implementing, Generation: 7})

	if len(answer.asked) != 0 {
		t.Fatalf("DesignNews was asked about %+v, want a task carrying no news never put to it", answer.asked)
	}
	if sent := h.wantPrompts(1)[0]; sent.Message != "implement the plan" {
		t.Fatalf("prompt = %+v, want the task as it was queued", sent)
	}
}

// newsOnlyNews is what a start queued for the news alone carries (record.DeliveryNews, encoded
// JSON): no text of the start's own, and two items. That it was queued for its news is the
// request's own mark (RequestDeliver.NewsOnly), never the news's.
const newsOnlyNews = `{"task":"",` +
	`"items":[{"issue":"LEGION-209","audience":"holder","step":"spec","artifact":"artifact-spec","from":3,"to":5},` +
	`{"issue":"LEGION-209","audience":"holder","step":"decisions","artifact":"artifact-decisions","from":1,"to":2}]}`

// newsCarryingStart is one start that carries news of the approved design, and what DesignNews
// answers of its task once the record has moved under it: its decisions item stopped standing.
type newsCarryingStart struct {
	name      string
	start     RequestDeliver
	movedTask string
	movedNews string
}

// newsCarryingStarts is the two starts that carry news: a phase start, whose news follows its own
// task, and a start queued for the news alone (the daemon's news-only start).
func newsCarryingStarts() []newsCarryingStart {
	return []newsCarryingStart{
		{
			name:      "a phase start carrying news",
			start:     newsStart("outbox:46", 46),
			movedTask: "Review the change. Issue: LEGION-209. Phase: reviewing.\n\nThe approved spec of LEGION-208 changed for LEGION-209.",
			movedNews: `{"task":"Review the change. Issue: LEGION-209. Phase: reviewing.",` +
				`"items":[{"issue":"LEGION-209","audience":"holder","step":"spec","artifact":"artifact-spec","from":3,"to":5}]}`,
		},
		{
			name: "a news-only start",
			start: RequestDeliver{Claim: testToken, Task: "The approved design of LEGION-208 changed for LEGION-209.", ID: "outbox:46", Row: 46,
				Phase: phase.Reviewing, Generation: 7, News: []byte(newsOnlyNews), NewsOnly: true},
			movedTask: "The approved spec of LEGION-208 changed for LEGION-209.",
			movedNews: `{"task":"",` +
				`"items":[{"issue":"LEGION-209","audience":"holder","step":"spec","artifact":"artifact-spec","from":3,"to":5}]}`,
		},
	}
}

// A task that carries news is sent once into a busy worker, as every task is
// (dispatch://INFRA-3276 R4): a worker in a turn of its own answers the send busy, the answer
// records the claim working (recordBusyTurn), and nothing sends the task into that turn again —
// not the sweep, not the shim's hello — so nothing reads its news again either. The turn's end
// makes the one next send, and that send renders the news again from the record as it reads then
// (currentNews): the item that stopped standing while the task waited goes from the text and from
// the delivery, and the confirmation of that send carries only what it told, which is what the
// store raises as taken (the store's settleDesignNews).
func TestATaskCarryingNewsABusyAnswerHoldsIsSentOnceMoreAtTheTurnsEnd(t *testing.T) {
	for _, tc := range newsCarryingStarts() {
		t.Run(tc.name, func(t *testing.T) {
			answer := &designNewsAnswer{task: tc.start.Task, news: tc.start.News}
			h := newsHarness(t, answer)
			h.reach(StateReady)
			h.conn.SetStreaming(true)
			h.conn.FailPrompt(busyRefusal())

			h.must(tc.start)
			h.wantPrompts(1)
			h.wantState(StateWorking)

			h.observe(runtime.Alive)
			h.must(StreamHello{Claim: testToken, Generation: h.generation()})
			h.observe(runtime.Alive)
			h.wantPrompts(1)
			if len(answer.asked) != 1 {
				t.Fatalf("DesignNews was asked %d times by the end of the turn the busy answer recorded, want once, at the one send", len(answer.asked))
			}

			answer.task, answer.news = tc.movedTask, []byte(tc.movedNews)
			h.conn.FailPrompt(nil)
			h.conn.SetStreaming(false)
			h.mustQuiet(StreamTurnEnd{Claim: testToken})

			sent := h.wantPrompts(2)
			if sent[1].Message != tc.movedTask || sent[1].DeliveryID != h.pending().ID {
				t.Fatalf("the send at the turn's end = %+v, want the task as the record reads now, %q, under its pending id %s", sent[1], tc.movedTask, h.pending().ID)
			}
			if len(answer.asked) != 2 {
				t.Fatalf("DesignNews was asked %d times, want twice: once at each send", len(answer.asked))
			}
			h.must(StreamTurnStart{Claim: testToken})
			stored, ok := h.store.delivery(testToken)
			if !ok || stored.ConfirmedAt.IsZero() || !bytes.Equal(stored.News, []byte(tc.movedNews)) {
				t.Fatalf("stored delivery after the turn started = %+v (held %t), want it confirmed carrying only %s", stored, ok, tc.movedNews)
			}
		})
	}
}

// A task that carries news whose first prompt's turn starts late is confirmed by that turn, as
// every task is (Machine.outstanding): Oh My Pi acknowledges the prompt, the wait for its turn
// runs out (noTurn), the next sweep sends the task again and renders its news again, and Oh My Pi
// answers that send busy, since it is preparing the first prompt's turn. The busy answer records
// the claim working and nothing more is sent. The first prompt's turn, when it starts, confirms the
// task under the id it holds now, and the store's write of that confirmation carries the news the
// task holds, which is what raises it as taken; the first prompt's result retires the task. The
// worker is prompted twice in all and the news is told by the one confirmation.
func TestATaskCarryingNewsIsConfirmedByTheLateTurnOfItsFirstPrompt(t *testing.T) {
	for _, tc := range newsCarryingStarts() {
		t.Run(tc.name, func(t *testing.T) {
			answer := &designNewsAnswer{task: tc.start.Task, news: tc.start.News}
			h := newsHarness(t, answer)
			h.reach(StateReady)

			h.must(tc.start)
			first := h.wantPrompts(1)[0].DeliveryID
			h.advance(testRPC)
			h.observe(runtime.Alive) // the next sweep sends the task again, as a silent prompt's bound has it
			resend := h.wantPrompts(2)[1].DeliveryID
			if len(answer.asked) != 2 {
				t.Fatalf("DesignNews was asked %d times, want twice: once at each send", len(answer.asked))
			}
			h.must(StreamPromptResult{Claim: testToken, DeliveryID: resend, Error: busyWords, Outcome: runtime.OutcomeAgentBusy})
			h.wantState(StateWorking)
			task := h.pending().ID

			h.must(StreamTurnStart{Claim: testToken}) // the first prompt's turn, late
			stored, ok := h.store.delivery(testToken)
			if !ok || stored.ID != task || stored.ConfirmedAt.IsZero() || !bytes.Equal(stored.News, tc.start.News) {
				t.Fatalf("stored delivery after the late start = %+v (held %t), want %s confirmed carrying %s", stored, ok, task, tc.start.News)
			}

			h.mustQuiet(StreamTurnEnd{Claim: testToken})
			h.must(StreamPromptResult{Claim: testToken, DeliveryID: first, AgentInvoked: true})
			if c := h.claim(); c.Pending != nil {
				t.Fatalf("pending after the first prompt's result = %+v, want the task retired", c.Pending)
			}
			h.wantPrompts(2)
			if len(answer.asked) != 2 {
				t.Fatalf("DesignNews was asked %d times, want only at the two sends", len(answer.asked))
			}
		})
	}
}

// A relaunched claim that holds nothing at its ready holds the daemon's own delivery of what the
// relaunch ended (queueRelaunch), and a task carrying news that arrives while no prompt of that
// delivery waits on a result takes its place (replaceable), as any newer task does. The relaunch
// neither drops the news nor tells it twice, and the record's sentence is neither lost nor
// duplicated:
//
//   - a task whose news still stands is sent once at the end of the turn the agent is in, with
//     the record's sentence in front of the text its news renders to then, and the one turn that
//     takes it confirms both: the delivery carrying the news, and the record told by that delivery;
//   - a task queued for its news alone that has nothing left to say at that send is retired
//     unsent, and the drop gives the claim the daemon's own delivery of the record again
//     (relaunchAfterDrop), which is sent at once, the sentence alone.
func TestATaskCarryingNewsTakesTheRelaunchDeliverysPlace(t *testing.T) {
	// relaunched is a claim whose worker ran a turn, died, and came back holding the daemon's own
	// delivery of what that death ended, which its agent, in a turn of its own, answered busy at
	// the send, so no prompt of it waits on a result.
	relaunched := func(t *testing.T, answer *designNewsAnswer) *harness {
		t.Helper()
		h := newsHarness(t, answer)
		h.reach(StateIdle)
		h.observe(runtime.Gone)
		h.conn.FailPrompt(busyRefusal())
		h.relaunched()
		h.conn.FailPrompt(nil)
		if p := h.pending(); !relaunchTask(p.ID) || len(h.outstanding()) != 0 {
			t.Fatalf("the relaunched claim holds %+v with %v outstanding, want the relaunch's delivery and no prompt of it waiting", p, h.outstanding())
		}
		return h
	}

	t.Run("its news still stands", func(t *testing.T) {
		answer := &designNewsAnswer{task: queuedTask, news: []byte(queuedNews)}
		h := relaunched(t, answer)

		h.must(newsStart("outbox:47", 47))
		if p := h.pending(); p.ID != "outbox:47" {
			t.Fatalf("the claim holds %+v after the task carrying news, want it in the relaunch delivery's place", p)
		}
		h.mustQuiet(StreamTurnEnd{Claim: testToken})

		sent := h.prompts()[len(h.prompts())-1]
		if want := relaunchSentence(h.claim().EndedWork, true) + "\n\n" + queuedTask; sent.DeliveryID != "outbox:47" || sent.Message != want {
			t.Fatalf("the task carrying news was sent as %+v, want %q under outbox:47", sent, want)
		}
		if len(answer.asked) != 1 {
			t.Fatalf("DesignNews was asked %d times, want once, at the one send", len(answer.asked))
		}
		h.must(StreamTurnStart{Claim: testToken})
		if stored, ok := h.store.delivery(testToken); !ok || stored.ID != "outbox:47" || stored.ConfirmedAt.IsZero() || !bytes.Equal(stored.News, []byte(queuedNews)) {
			t.Fatalf("stored delivery after the turn started = %+v (held %t), want outbox:47 confirmed carrying its news", stored, ok)
		}
		h.wantEndedTold(true, "outbox:47", "once the turn of the task carrying news began")
	})

	t.Run("a news-only task with nothing left to say", func(t *testing.T) {
		answer := &designNewsAnswer{}
		h := relaunched(t, answer)
		start := newsCarryingStarts()[1].start
		start.ID, start.Row = "outbox:48", 48

		h.must(start)
		before := len(h.prompts())
		h.mustQuiet(StreamTurnEnd{Claim: testToken})

		ended := h.claim().EndedWork
		sent := h.wantPrompts(before + 1)[before]
		if !relaunchTask(sent.DeliveryID) || sent.Message != RelaunchSentence(ended) {
			t.Fatalf("the send after the news-only task was retired = %+v, want the relaunch's record alone, %q", sent, RelaunchSentence(ended))
		}
		if p := h.pending(); p.ID != sent.DeliveryID {
			t.Fatalf("the claim holds %+v, want the daemon's own delivery %s it was sent", p, sent.DeliveryID)
		}
		if log := h.store.deliveryLog(); !slices.Contains(log, "retire outbox:48") {
			t.Fatalf("delivery writes = %v, want the news-only task retired unsent", log)
		}
		h.wantEndedTold(false, "", "before the relaunch delivery's turn")
	})
}

// A turn the in-flight rule takes for the pending task while the send in flight is an older task's
// credits none of that send's news (turnAnswers). The send of a task carrying news is past its
// adoption and on its way when a turn starts, which confirms that task with the news the send
// carries. A newer task carrying news takes the confirmed task's place while the send is still
// out, the turn ends, and another turn starts before the send returns: the start is taken for the
// newer task, since a send is in flight, and its confirmation records no copy at all, since the
// send in flight carried the older task's news under the older task's id.
func TestATurnTakenForANewerTaskWhileAnOlderTasksSendIsOutRaisesNoneOfThatSendsNews(t *testing.T) {
	answer := &designNewsAnswer{task: queuedTask, news: []byte(queuedNews)}
	h := newsHarness(t, answer)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, newsStart("outbox:41", 41)); err != nil {
		t.Fatal(err)
	}
	older := waitFor(t, "the older task's send", gated.entered)
	if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
		t.Fatal(err)
	}
	if p := h.pending(); p.ID != older || p.ConfirmedAt.IsZero() || !bytes.Equal(p.Carried, []byte(queuedNews)) {
		t.Fatalf("pending after a turn started with its send in flight = %+v, want %s confirmed with the news its send carries", p, older)
	}

	if err := h.m.Handle(h.ctx, newsStart("outbox:42", 42)); err != nil {
		t.Fatalf("the newer task: %v", err)
	}
	if err := h.m.Handle(h.ctx, StreamTurnEnd{Claim: testToken}); err != nil {
		t.Fatalf("the older task's turn ends: %v", err)
	}
	if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
		t.Fatalf("a turn starts with the older task's send still out: %v", err)
	}
	stored, ok := h.store.delivery(testToken)
	if !ok || stored.ID != "outbox:42" || stored.ConfirmedAt.IsZero() {
		t.Fatalf("stored delivery after the second start = %+v (held %t), want outbox:42 taken by that turn", stored, ok)
	}
	if len(stored.Carried) != 0 {
		t.Fatalf("outbox:42's confirmation records the copy %s, want none: the send in flight carried %s's news, under its id", stored.Carried, older)
	}
	gated.release <- struct{}{}
	h.m.Wait()
}
