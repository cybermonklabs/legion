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
