package supervise

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

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

// wantCredited fails the test unless the store holds the claim's delivery taken (ConfirmedAt set),
// carrying news as both what it holds and the copy its accepted take carried (Delivery.Carried), a
// credit once the take's turn starts.
func wantCredited(t *testing.T, h *harness, when string, news []byte) Delivery {
	t.Helper()
	stored, ok := h.store.delivery(h.token)
	if !ok || stored.ConfirmedAt.IsZero() || !bytes.Equal(stored.News, news) || !bytes.Equal(stored.Carried, news) {
		t.Fatalf("stored delivery %s = %+v (held %t), want it taken carrying and crediting %s", when, stored, ok, news)
	}
	return stored
}

// A task that carries news of the approved design is taken as the record reads at its take, not
// as it read when the start was queued: the gate it names can have closed since, and the text the
// worker runs must say so. The machine asks Deps.DesignNews about the delivery a take of it is
// decided for (currentNews, takeable), writes the answer to the store under the same id before the
// take is answered, and answers the take that text, so a restart reads the task this take carried
// and the accept copies what it named (Delivery.Carried).
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
	if taken := h.wantPrompts(1)[0]; taken.DeliveryID != "outbox:41" || taken.Message != closed {
		t.Fatalf("take = %+v, want outbox:41 answered the text DesignNews rendered at the take, %q", taken, closed)
	}
	stored, ok := h.store.delivery(testToken)
	if !ok || stored.ID != "outbox:41" || stored.Task != closed || !bytes.Equal(stored.News, []byte(queuedNews)) {
		t.Fatalf("stored delivery = %+v (held %t), want outbox:41 holding the text it was taken with", stored, ok)
	}
	wantCredited(t, h, "after the take", []byte(queuedNews))
	if log := h.store.deliveryLog(); len(log) < 3 || !slices.Equal(log[:3], []string{"put outbox:41", "put outbox:41", "put outbox:41"}) {
		t.Fatalf("delivery writes = %v, want the queue's write, the rendered task's, then the accept's, all under the same id", log)
	}
}

// An item DesignNews drops — a later approval replaced it, or another delivery's take told it —
// goes from the delivery as well as from the text. The accept of the delivery's take is what
// credits each item it carries as taken, so a dropped item left on it would be recorded as told by
// a take whose text never named it.
func TestAnItemThatStopsStandingIsGoneFromTheDeliveryItsConfirmationWrites(t *testing.T) {
	narrowed := `{"task":"Review the change. Issue: LEGION-209. Phase: reviewing.",` +
		`"items":[{"issue":"LEGION-209","audience":"holder","step":"spec","artifact":"artifact-spec","from":3,"to":5}]}`
	text := "Review the change. Issue: LEGION-209. Phase: reviewing.\n\nThe approved design of LEGION-208 changed for LEGION-209."
	answer := &designNewsAnswer{task: text, news: []byte(narrowed)}
	h := newsHarness(t, answer)
	h.reach(StateReady)

	h.must(newsStart("outbox:41", 41))

	if taken := h.wantPrompts(1)[0]; taken.Message != text {
		t.Fatalf("take = %+v, want the text with the dropped item gone, %q", taken, text)
	}
	if p := h.pending(); !bytes.Equal(p.News, []byte(narrowed)) || p.Task != text {
		t.Fatalf("pending delivery = %+v, want it to carry only the item that still stands", p)
	}
	wantCredited(t, h, "after the take", []byte(narrowed))

	h.must(StreamTurnStart{Claim: testToken})

	wantCredited(t, h, "after the take's turn started", []byte(narrowed))
}

// A task that has nothing left to say at its take — every item it carried stopped standing, and
// no opening is owed while the gate stands open — is dropped rather than taken: a text saying
// nothing costs a model turn, and its accept would credit nothing. The take is answered dropped,
// the value is deleted, and the claim stays as it was, ready for the next task.
func TestANewsOnlyTaskLeftWithNothingToSayIsRetiredUnsent(t *testing.T) {
	answer := &designNewsAnswer{}
	h := newsHarness(t, answer)
	h.reach(StateReady)

	h.must(newsStart("outbox:42", 42))

	if len(answer.asked) != 1 {
		t.Fatalf("DesignNews was asked %d times, want once, at the take", len(answer.asked))
	}
	h.wantPrompts(0)
	if answers := h.worker.answers; len(answers) != 1 || answers[0].Outcome != TakeOutcomeDropped {
		t.Fatalf("the worker's takes were answered %+v, want one answered dropped", answers)
	}
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending delivery = %+v, want the news task dropped", p)
	}
	if log := h.store.deliveryLog(); len(log) == 0 || log[len(log)-1] != "retire outbox:42" {
		t.Fatalf("delivery writes = %v, want the retirement of outbox:42 last", log)
	}
	if value, held := h.values.value(testToken); held {
		t.Fatalf("the bucket still holds %+v, want the dropped task's value deleted", value)
	}
	h.wantState(StateReady)
}

// A daemon restarted over a claim the store holds working, its task owed and no take of it
// accepted, has its question of the agent armed (askFirst), and a take answers it: the worker asks
// only once its host is idle, so the turn the store recorded is over (takeInTurn). A task queued
// for its news alone that has nothing left to say is dropped by that take, as every take of it
// would drop it, so the question is answered by the drop: the claim is idle, nothing takes the
// dropped task, and no credit is written for news the record no longer owes.
func TestANewsOnlyTaskARestartRetiresAnswersTheQuestionTheRestartLeft(t *testing.T) {
	restored := fixture(StateWorking)
	restored.TaskTaken = 0
	restored.Pending = &Delivery{ID: "outbox:45", Number: 1, Task: queuedTask, Phase: phase.Reviewing, Generation: 7, QueuedAt: epoch,
		News: []byte(queuedNews)}
	answer := &designNewsAnswer{}
	h := newBareHarness(t)
	h.deps.DesignNews = answer.answer
	h.manualTakes()
	h.restore(restored)

	if got := h.take(); got.Outcome != TakeOutcomeDropped {
		t.Fatalf("the worker's take of the restored task = %+v, want it answered dropped", got)
	}

	h.wantPrompts(0)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending delivery = %+v, want the news task dropped", p)
	}
	if log := h.store.deliveryLog(); len(log) == 0 || log[len(log)-1] != "retire outbox:45" {
		t.Fatalf("delivery writes = %v, want the untaken retirement of outbox:45 last", log)
	}
	if len(answer.asked) != 1 {
		t.Fatalf("DesignNews was asked %d times, want once, at the take", len(answer.asked))
	}
	h.wantState(StateIdle)
}

// restoredUntakenNews is a daemon restarted over a ready claim that holds a task carrying news of
// the approved design no take of which was accepted (Delivery.Carried empty): a task queued while
// the agent's turn ran, with the daemon stopped before that turn ended, or, where adoptionFailed
// says so, one an earlier daemon's take refused because its adoption of the working copy failed
// (refusedAdoption, which writes the failure on the delivery and no take). The claim's connection
// is registered and the worker drives no take of its own.
func restoredUntakenNews(t *testing.T, answer *designNewsAnswer, adoptionFailed bool) *harness {
	t.Helper()
	restored := fixture(StateReady)
	restored.Pending = &Delivery{ID: "outbox:46", Number: 1, Task: queuedTask, Phase: phase.Reviewing, Generation: 7, QueuedAt: epoch,
		News: []byte(queuedNews)}
	if adoptionFailed {
		restored.Pending.SendFailures, restored.Pending.LastSendFailureAt = 1, epoch
		restored.Pending.LastSendError = "the working copy was not adopted: jj metaedit: the git lock was held"
	}
	h := newBareHarness(t)
	h.deps.DesignNews = answer.answer
	h.manualTakes()
	h.restore(restored)
	h.conns.Register(testToken, h.conn)
	return h
}

// After a restart, a task carrying news that no accepted take carried is never confirmed by a turn
// the agent started by itself: only an accepted take confirms a task (accept), and its accept is
// what credits the news, so an Envoy message's turn the agent runs while the task waits credits
// nothing the agent never read. That holds for a task an earlier daemon's take refused because its
// adoption of the working copy failed, which credited nothing. The task stays untaken through the
// agent's own turn, and the take after that turn's end is accepted and credits the news its text
// carried.
func TestARestartNeverConfirmsANewsTaskNoPromptCarriedByATurnOfTheAgentsOwn(t *testing.T) {
	for _, tc := range []struct {
		name           string
		adoptionFailed bool
	}{
		{name: "a turn of the agent's own starts before anything is taken"},
		{name: "a turn of the agent's own starts after a take whose adoption failed", adoptionFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := &designNewsAnswer{task: queuedTask, news: []byte(queuedNews)}
			h := restoredUntakenNews(t, answer, tc.adoptionFailed)

			h.must(StreamTurnStart{Claim: testToken})

			if stored, ok := h.store.delivery(testToken); !ok || !stored.ConfirmedAt.IsZero() || len(stored.Carried) != 0 {
				t.Fatalf("stored delivery after %s = %+v (held %t), want outbox:46 untaken, crediting nothing", tc.name, stored, ok)
			}
			h.wantRefused(HoldInTurn)
			h.wantPrompts(0)
			h.wantState(StateWorking)

			h.mustQuiet(StreamTurnEnd{Claim: testToken})

			text := h.mustTake()
			if text != queuedTask || h.pending().ID != "outbox:46" {
				t.Fatalf("the take at the end of the agent's own turn was answered %q on %+v, want the news task %q", text, h.pending(), queuedTask)
			}
			wantCredited(t, h, "after the take its own turn's end let through", []byte(queuedNews))
		})
	}
}

// A task carrying news is a task like any other in when it is taken: queued while the claim is in
// a turn, its take waits for that turn's end, and nothing asks the record about it until then. The
// news it carries is rendered at the take that turn's end lets through, not at the queue.
func TestATaskCarryingNewsQueuedInATurnWaitsForTheTurnsEnd(t *testing.T) {
	rendered := "Review the change. Issue: LEGION-209. Phase: reviewing.\n\nrendered at the take"
	answer := &designNewsAnswer{task: rendered, news: []byte(queuedNews)}
	h := newsHarness(t, answer)
	h.reach(StateWorking)
	before := len(h.prompts())

	h.must(newsStart("outbox:43", 43))

	if len(answer.asked) != 0 {
		t.Fatalf("DesignNews was asked about %+v while the claim worked a turn, want nothing asked before a take", answer.asked)
	}
	h.wantPrompts(before)
	if p := h.pending(); p.ID != "outbox:43" || p.taken() {
		t.Fatalf("pending delivery = %+v, want outbox:43 waiting untaken", p)
	}

	h.must(StreamTurnEnd{Claim: testToken})

	if len(answer.asked) != 1 {
		t.Fatalf("DesignNews was asked %d times after the turn ended, want once, at the take", len(answer.asked))
	}
	if taken := h.wantPrompts(before + 1)[before]; taken.DeliveryID != "outbox:43" || taken.Message != rendered {
		t.Fatalf("take = %+v, want outbox:43 answered the text rendered at the turn's end", taken)
	}
}

// A task that carries no news is taken as it was queued, and nothing asks the record about it.
func TestATaskCarryingNoNewsIsNeverPutToDesignNews(t *testing.T) {
	answer := &designNewsAnswer{task: "never this"}
	h := newsHarness(t, answer)
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:44", Row: 44, Phase: phase.Implementing, Generation: 7})

	if len(answer.asked) != 0 {
		t.Fatalf("DesignNews was asked about %+v, want a task carrying no news never put to it", answer.asked)
	}
	if taken := h.wantPrompts(1)[0]; taken.Message != "implement the plan" {
		t.Fatalf("take = %+v, want the task as it was queued", taken)
	}
	if stored, ok := h.store.delivery(testToken); !ok || len(stored.Carried) != 0 {
		t.Fatalf("stored delivery = %+v (held %t), want a take of a task carrying no news crediting nothing", stored, ok)
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

// A task that carries news is taken once by a busy worker, as every task is
// (dispatch://INFRA-3276 R4): a worker in a turn of its own has its take refused in-turn, and
// nothing hands the task into that turn — not the sweep, not the shim's hello — so nothing reads
// its news while the turn runs either. The turn's end lets the one next take through, and that take
// renders the news again from the record as it reads then (currentNews): the item that stopped
// standing while the task waited goes from the text and from the delivery, and the accept copies
// only what its text told, which the store reads as taken once the take's turn starts (the store's
// settleDesignNews).
func TestATaskCarryingNewsABusyAnswerHoldsIsSentOnceMoreAtTheTurnsEnd(t *testing.T) {
	for _, tc := range newsCarryingStarts() {
		t.Run(tc.name, func(t *testing.T) {
			answer := &designNewsAnswer{task: tc.start.Task, news: tc.start.News}
			h := newsHarness(t, answer)
			h.reach(StateReady)
			h.manualTakes()
			h.must(StreamTurnStart{Claim: testToken})

			h.must(tc.start)
			h.wantRefused(HoldInTurn)
			h.wantState(StateWorking)

			h.observe(runtime.Alive)
			h.conn.SetStreaming(true)
			h.must(StreamHello{Claim: testToken, Generation: h.generation()})
			h.observe(runtime.Alive)
			h.wantPrompts(0)
			if len(answer.asked) != 0 {
				t.Fatalf("DesignNews was asked %d times while the agent's own turn ran, want none: no take was decided", len(answer.asked))
			}

			answer.task, answer.news = tc.movedTask, []byte(tc.movedNews)
			h.conn.SetStreaming(false)
			h.worker.manual = false
			h.mustQuiet(StreamTurnEnd{Claim: testToken})

			taken := h.wantPrompts(1)
			if taken[0].Message != tc.movedTask || taken[0].DeliveryID != h.pending().ID {
				t.Fatalf("the take at the turn's end = %+v, want the task as the record reads now, %q, under its pending id %s", taken[0], tc.movedTask, h.pending().ID)
			}
			// The hold's end decides the take as a take would (offerPending, takeable) before it
			// writes the value again, and the take decides it once more: two renders, both after
			// the turn, none while it ran.
			if len(answer.asked) != 2 {
				t.Fatalf("DesignNews was asked %d times, want twice, at the hold's end and at the take, both after the turn", len(answer.asked))
			}
			h.must(StreamTurnStart{Claim: testToken})
			wantCredited(t, h, "after the take's turn started", []byte(tc.movedNews))
		})
	}
}

// A task that carries news whose take's answer was lost is taken again under the same number, and
// the record answers that take the text it kept (answeredFromRecord): nothing renders the news
// again and nothing is copied anew, since the accept already wrote the copy of what that text
// carries. The take's turn, when it starts, is the accepted take's, and makes that copy the take's
// credit; the report that names the number finishes the task, which first makes the credit final
// (Delivery.Ran, finishTask) in a write of its own before the retirement. The worker is answered
// twice in all and the news is credited by the one accept's copy, from the take's turn's start.
func TestATaskCarryingNewsIsConfirmedByTheLateTurnOfItsFirstPrompt(t *testing.T) {
	for _, tc := range newsCarryingStarts() {
		t.Run(tc.name, func(t *testing.T) {
			answer := &designNewsAnswer{task: tc.start.Task, news: tc.start.News}
			h := newsHarness(t, answer)
			h.reach(StateReady)

			h.must(tc.start)
			first := h.wantPrompts(1)[0]
			value, held := h.values.value(testToken)
			if !held {
				t.Fatal("the bucket holds no value of the taken task")
			}
			again, err := h.takeValue(value)
			if err != nil || again.Outcome != TakeOutcomeAccepted || again.Text != first.Message {
				t.Fatalf("the take sent again = %+v, %v; want it answered the text the first take was, %q", again, err, first.Message)
			}
			if len(answer.asked) != 1 {
				t.Fatalf("DesignNews was asked %d times, want once: the take sent again is answered from the record", len(answer.asked))
			}

			h.must(StreamTurnStart{Claim: testToken})
			wantCredited(t, h, "after the late start", tc.start.News)

			h.must(StreamTurnEnd{Claim: testToken})
			if c := h.claim(); c.Pending != nil {
				t.Fatalf("pending after the take's report = %+v, want the task finished", c.Pending)
			}
			final, ok := h.store.lastWritten(first.DeliveryID)
			if !ok || !final.Ran || !bytes.Equal(final.Carried, tc.start.News) {
				t.Fatalf("the last write of %s before its retirement = %+v (held %t), want its credit of %s marked final", first.DeliveryID, final, ok, tc.start.News)
			}
			if log := h.store.deliveryLog(); len(log) < 2 || !slices.Equal(log[len(log)-2:], []string{"put " + first.DeliveryID, "retire " + first.DeliveryID}) {
				t.Fatalf("delivery writes = %v, want the final credit's write, then the retirement", log)
			}
			h.wantPrompts(2)
			if len(answer.asked) != 1 {
				t.Fatalf("DesignNews was asked %d times, want only at the one take decided afresh", len(answer.asked))
			}
		})
	}
}

// A relaunched claim that holds nothing at its ready holds the daemon's own delivery of what the
// relaunch ended (queueRelaunch), and a task carrying news that arrives while no take of that
// delivery is accepted takes its place (replaceable), as any newer task does. The relaunch neither
// drops the news nor tells it twice, and the record's sentence is neither lost nor duplicated:
//
//   - a task whose news still stands is taken once, with the record's sentence in front of the
//     text its news renders to then, and the one accepted take carries both: the delivery carrying
//     the news, and the record told by that delivery;
//   - a task queued for its news alone that has nothing left to say at its take is dropped, and the
//     drop gives the claim the daemon's own delivery of the record again (takeable's
//     queueRelaunch), which is taken at once, the sentence alone.
func TestATaskCarryingNewsTakesTheRelaunchDeliverysPlace(t *testing.T) {
	// relaunched is a claim whose worker ran a turn while a subagent of its session worked, died,
	// and came back holding the daemon's own delivery of what that death ended, which its worker
	// has not taken.
	relaunched := func(t *testing.T, answer *designNewsAnswer) *harness {
		t.Helper()
		h := newsHarness(t, answer)
		h.reach(StateIdle)
		h.reportActivity("subagent")
		h.observe(runtime.Gone)
		h.manualTakes()
		h.relaunched()
		if p := h.pending(); !relaunchTask(p.ID) || p.taken() {
			t.Fatalf("the relaunched claim holds %+v, want the relaunch's delivery, no take of it accepted", p)
		}
		h.worker.manual = false
		return h
	}

	t.Run("its news still stands", func(t *testing.T) {
		answer := &designNewsAnswer{task: queuedTask, news: []byte(queuedNews)}
		h := relaunched(t, answer)
		before := len(h.prompts())

		h.must(newsStart("outbox:47", 47))
		if p := h.pending(); p.ID != "outbox:47" {
			t.Fatalf("the claim holds %+v after the task carrying news, want it in the relaunch delivery's place", p)
		}

		taken := h.wantPrompts(before + 1)[before]
		if want := relaunchSentence(h.claim().EndedWork, true) + "\n\n" + queuedTask; taken.DeliveryID != "outbox:47" || taken.Message != want {
			t.Fatalf("the task carrying news was taken as %+v, want %q under outbox:47", taken, want)
		}
		if len(answer.asked) != 1 {
			t.Fatalf("DesignNews was asked %d times, want once, at the one take", len(answer.asked))
		}
		wantCredited(t, h, "after the take", []byte(queuedNews))
		h.wantEndedTold(true, "outbox:47", "once the take of the task carrying news was accepted")
	})

	t.Run("a news-only task with nothing left to say", func(t *testing.T) {
		answer := &designNewsAnswer{}
		h := relaunched(t, answer)
		// The agent is in a turn of its own when the news-only start arrives, as main's busy answer
		// left it, so the start's task takes the relaunch delivery's place untaken and the turn's
		// end is where a take of it is next decided (offerPending, takeable).
		h.must(StreamTurnStart{Claim: testToken})
		start := newsCarryingStarts()[1].start
		start.ID, start.Row = "outbox:48", 48
		h.must(start)
		if p := h.pending(); p.ID != start.ID || p.taken() {
			t.Fatalf("the claim holds %+v after the news-only start, want it in the relaunch delivery's place, untaken", p)
		}
		before := len(h.prompts())

		h.mustQuiet(StreamTurnEnd{Claim: testToken})

		// The turn's end let the worker's take through, the take dropped the news-only task, and
		// the drop queued the daemon's own delivery of the record and wrote its value at a newer
		// revision in the same decision. Main's push sent that delivery at once (relaunchAfterDrop);
		// here the value write is that send, and the worker's ask at the newer revision, which its
		// relay hands it, is the take.
		if answers := h.worker.answers; len(answers) == 0 || answers[len(answers)-1].Outcome != TakeOutcomeDropped {
			t.Fatalf("the worker's takes were answered %+v, want the last answered dropped", answers)
		}
		if p, value, held := h.pending(), h.claim().TaskWrittenRevision, false; relaunchTask(p.ID) {
			current, ok := h.values.value(testToken)
			held = ok && current.Number == p.Number
			if !held {
				t.Fatalf("after the drop the bucket holds %+v (written %+v), want the relaunch delivery's value", current, value)
			}
		} else {
			t.Fatalf("after the drop the claim holds %+v, want the relaunch delivery", p)
		}
		h.workerAsks()

		ended := h.claim().EndedWork
		taken := h.wantPrompts(before + 1)[before]
		if !relaunchTask(taken.DeliveryID) || taken.Message != RelaunchSentence(ended) {
			t.Fatalf("the take after the news-only task was dropped = %+v, want the relaunch's record alone, %q", taken, RelaunchSentence(ended))
		}
		if p := h.pending(); p.ID != taken.DeliveryID {
			t.Fatalf("the claim holds %+v, want the daemon's own delivery %s it took", p, taken.DeliveryID)
		}
		if log := h.store.deliveryLog(); !slices.Contains(log, "retire outbox:48") {
			t.Fatalf("delivery writes = %v, want the news-only task dropped untaken", log)
		}
		h.wantEndedTold(true, taken.DeliveryID, "once the take of the relaunch's own delivery was accepted")
	})

	// The same drop made by the worker's own take, with no turn of the agent's in between: the take
	// is answered dropped, and the drop's relaunch delivery is written to the bucket at a newer
	// revision in the same decision (takeable's queueRelaunch, queue's putValue), which is the
	// worker's signal to ask again, so its next ask takes the sentence alone.
	t.Run("a news-only task with nothing left to say, dropped by the worker's own take", func(t *testing.T) {
		answer := &designNewsAnswer{}
		h := relaunched(t, answer)
		start := newsCarryingStarts()[1].start
		start.ID, start.Row = "outbox:49", 49
		before := len(h.prompts())

		h.must(start)

		if answers := h.worker.answers; len(answers) == 0 || answers[len(answers)-1].Outcome != TakeOutcomeDropped {
			t.Fatalf("the worker's takes were answered %+v, want the last answered dropped", answers)
		}
		p := h.pending()
		value, held := h.values.value(testToken)
		if !relaunchTask(p.ID) || !held || value.Number != p.Number {
			t.Fatalf("after the drop the claim holds %+v and the bucket %+v (held %t), want the relaunch delivery and its value written", p, value, held)
		}
		h.workerAsks()
		ended := h.claim().EndedWork
		taken := h.wantPrompts(before + 1)[before]
		if taken.DeliveryID != p.ID || taken.Message != RelaunchSentence(ended) {
			t.Fatalf("the worker's next ask took %+v, want the relaunch's record alone, %q", taken, RelaunchSentence(ended))
		}
	})
}

// A newer task that takes the place of an older one whose take's turn is running credits none of
// the older take's news, only what its own accepted take's text carried (accept). The older task
// carrying news is taken, which writes its copy, and its take's turn starts, which makes that copy
// its credit; a newer phase task carrying news takes its place, which retires the older delivery
// and so ends its credit (the store counts only the copy of a taken delivery whose take's turn has
// started, design_news_told_in); the turn ends, and the newer task's take, rendered from the
// record as it reads then, carries the newer task's own news and nothing of the older one's.
// It is the take's counterpart of the turn the push path took for a newer task while an older
// task's send was still out: a newer task is credited only by a take of its own.
func TestATurnTakenForANewerTaskWhileAnOlderTasksSendIsOutRaisesNoneOfThatSendsNews(t *testing.T) {
	newer := `{"task":"Review the change. Issue: LEGION-209. Phase: reviewing.",` +
		`"items":[{"issue":"LEGION-209","audience":"holder","step":"spec","artifact":"artifact-spec","from":5,"to":6}]}`
	answer := &designNewsAnswer{task: queuedTask, news: []byte(queuedNews)}
	h := newsHarness(t, answer)
	h.reach(StateReady)

	h.must(newsStart("outbox:41", 41))
	wantCredited(t, h, "after the older task's take", []byte(queuedNews))
	h.must(StreamTurnStart{Claim: testToken})

	later := newsStart("outbox:42", 42)
	later.News = []byte(newer)
	answer.task, answer.news = "Review the change. Issue: LEGION-209. Phase: reviewing.\n\nversion 6", []byte(newer)
	if err := h.handle(later); err != nil {
		t.Fatalf("the newer task while the older task's turn runs: %v", err)
	}
	if p := h.pending(); p.ID != "outbox:42" || p.taken() {
		t.Fatalf("pending = %+v, want the newer task in the older one's place, untaken", p)
	}
	if log := h.store.deliveryLog(); !slices.Contains(log, "retire outbox:41") {
		t.Fatalf("delivery writes = %v, want the older task retired, which ends its credit", log)
	}
	h.mustQuiet(StreamTurnEnd{Claim: testToken})

	stored := wantCredited(t, h, "after the newer task's take", []byte(newer))
	if stored.ID != "outbox:42" || bytes.Contains(stored.Carried, []byte(`"to":2`)) {
		t.Fatalf("the stored delivery %s credits %s, want outbox:42 crediting none of outbox:41's news", stored.ID, stored.Carried)
	}
}

// A report of the take's turn that carries a provider error never makes the take's credit of news
// of the approved design final: the ladder takes the task back (takeToLadder, clearTake), and that
// take-back ends the credit — the copy goes from the delivery, and its write ends what the store
// read as taken — so the take after the ladder's wait renders the news again and its accept
// credits it again. Only the finish of a take's turn, the report naming its number with no provider
// error, marks the credit final (Delivery.Ran, finishTask), in the one write before the retirement.
// A Ran written at the provider error's report would fold into the told rows news whose turn
// failed, and the retake's news would be owed by nothing.
func TestAProviderErrorEndsTheCreditOfATakeCarryingNewsAndItsRetakeCarriesItAgain(t *testing.T) {
	answer := &designNewsAnswer{task: queuedTask, news: []byte(queuedNews)}
	h := newsHarness(t, answer)
	h.reach(StateReady)
	h.must(newsStart("outbox:50", 50))
	first := h.wantPrompts(1)[0]
	wantCredited(t, h, "after the first take", []byte(queuedNews))
	h.must(StreamTurnStart{Claim: testToken})

	h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})

	stored, ok := h.store.delivery(testToken)
	if !ok || !stored.ConfirmedAt.IsZero() || len(stored.Carried) != 0 || stored.Ran {
		t.Fatalf("stored delivery after the provider error = %+v (held %t), want the take and its credit taken back, nothing final", stored, ok)
	}

	h.advance(30 * time.Second)
	retake := h.wantPrompts(2)[1]
	if retake.DeliveryID != first.DeliveryID || !strings.HasSuffix(retake.Message, queuedTask) {
		t.Fatalf("the take after the ladder's wait = %+v, want %s answered the news again, ending %q", retake, first.DeliveryID, queuedTask)
	}
	// Asked at the first take, at the ladder's end, which decides the take before it writes the
	// value again (offerPending, takeable), and at the retake.
	if len(answer.asked) != 3 {
		t.Fatalf("DesignNews was asked %d times, want three times: the retake renders the news again", len(answer.asked))
	}
	wantCredited(t, h, "after the retake", []byte(queuedNews))

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})

	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending after the clean report = %+v, want the task finished", p)
	}
	h.store.mu.Lock()
	var finals []int
	writes := len(h.store.written)
	for i, written := range h.store.written {
		if written.ID == first.DeliveryID && written.Ran {
			finals = append(finals, i)
		}
	}
	h.store.mu.Unlock()
	if len(finals) != 1 || finals[0] != writes-1 {
		t.Fatalf("the writes marking %s's credit final are at %v of %d writes, want the one, the last before its retirement", first.DeliveryID, finals, writes)
	}
}
