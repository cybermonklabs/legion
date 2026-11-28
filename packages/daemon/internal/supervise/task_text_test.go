package supervise

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The task an implementer's step start queued, composed with the text of version 1 of its task's
// block, and the same task once version 2 changed that text alone.
const (
	textOneTask = "Continue the widget. Issue: LEGION-209. Phase: implementing. Task: a.\n```\nText one\n```\n"
	textTwoTask = "Continue the widget. Issue: LEGION-209. Phase: implementing. Task: a.\n```\nText two\n```\n"
)

// taskTextAnswer is a Deps.TaskText that answers task for every delivery, and records each
// delivery it was asked about, in order.
type taskTextAnswer struct {
	task  string
	asked []Delivery
}

func (a *taskTextAnswer) answer(_ context.Context, _ Claim, d Delivery) (string, []byte, error) {
	a.asked = append(a.asked, d)
	return a.task, d.News, nil
}

// taskTextHarness is a machine over the queued implementer claim whose Deps.TaskText is answer.
func taskTextHarness(t *testing.T, answer *taskTextAnswer) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.TaskText = answer.answer
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

// stepStart is the implementer's step start of run 7, queued from outbox row 41 with the text of
// version 1 of its task.
func stepStart() RequestDeliver {
	return RequestDeliver{Claim: testToken, Task: textOneTask, ID: "outbox:41", Row: 41, Phase: phase.Implementing, Generation: 7, Entry: 1}
}

// A later version of the plan can change only the text of a running task, and the claim of its
// step holds the start task it was queued with. The task is sent as Deps.TaskText answers it at
// the send, under the same id, and written to the store before the prompt goes, so a restart
// reads the task this send carried.
func TestAHeldStepTaskIsSentWithTheTextOfItsTaskAtTheSend(t *testing.T) {
	answer := &taskTextAnswer{task: textTwoTask}
	h := taskTextHarness(t, answer)
	h.reach(StateReady)

	h.must(stepStart())

	if len(answer.asked) != 1 || answer.asked[0].Task != textOneTask {
		t.Fatalf("TaskText was asked about %+v, want once, about the task as the start queued it", answer.asked)
	}
	if sent := h.wantPrompts(1)[0]; sent.DeliveryID != "outbox:41" || sent.Message != textTwoTask {
		t.Fatalf("prompt = %+v, want outbox:41 carrying the text TaskText answered, %q", sent, textTwoTask)
	}
	if stored, ok := h.store.delivery(testToken); !ok || stored.ID != "outbox:41" || stored.Task != textTwoTask {
		t.Fatalf("stored delivery = %+v (held %t), want outbox:41 holding the text it was sent with", stored, ok)
	}
}

// The step's agent is in the turn of its task when the version changes the text: nothing is sent
// to it for the version. Its process then dies in that turn, and the task taken back is sent to
// the relaunched agent, which can run in a fresh session, with the text TaskText answers then,
// behind the sentence of the interruption. The control is a TaskText that answers the text the
// task was queued with: the relaunched agent is then sent version 1's text.
func TestATaskTakenBackFromATurnIsSentAgainWithTheTextOfItsTaskThen(t *testing.T) {
	for _, tc := range []struct {
		name, answers, want, not string
	}{
		{"a later text", textTwoTask, "Text two", "Text one"},
		{"control: the text it was queued with", textOneTask, "Text one", "Text two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := &taskTextAnswer{task: textOneTask}
			h := taskTextHarness(t, answer)
			h.reach(StateReady)
			h.must(stepStart())
			h.must(StreamTurnStart{Claim: testToken})
			h.wantState(StateWorking)
			if p := h.pending(); p.ConfirmedAt.IsZero() {
				t.Fatalf("the working claim holds %+v, want its task confirmed by the turn", p)
			}
			asked := len(answer.asked)

			answer.task = tc.answers
			if len(answer.asked) != asked || len(h.prompts()) != 1 {
				t.Fatalf("the change of the text asked TaskText %d times and sent %d prompts, want nothing in the turn", len(answer.asked)-asked, len(h.prompts())-1)
			}
			h.observe(runtime.Gone)
			h.relaunched()

			sent, p := h.wantPrompts(2)[1], h.pending()
			if !p.Interrupted || sent.DeliveryID != p.ID || !strings.HasSuffix(sent.Message, tc.answers) ||
				!strings.Contains(sent.Message, tc.want) || strings.Contains(sent.Message, tc.not) {
				t.Fatalf("the relaunched agent was sent %+v as %+v, want the task taken back, interrupted, ending with %q", sent, p, tc.answers)
			}
			if last := answer.asked[len(answer.asked)-1]; last.Task != textOneTask || !last.Interrupted {
				t.Fatalf("TaskText was last asked about %+v, want the task taken back as the start queued it", last)
			}
		})
	}
}

// A delivery that names no phase or no run is no phase worker's start, and none is put to
// TaskText: an operator's own delivery and a delivery of no phase.
func TestADeliveryOfNoPhaseOrNoRunIsNeverPutToTaskText(t *testing.T) {
	for _, request := range []RequestDeliver{
		{Claim: testToken, Task: textOneTask, ID: "operator:1", Generation: 7},
		{Claim: testToken, Task: textOneTask, ID: "outbox:42", Row: 42, Phase: phase.Implementing},
	} {
		answer := &taskTextAnswer{task: textTwoTask}
		h := taskTextHarness(t, answer)
		h.reach(StateReady)
		h.must(request)
		if len(answer.asked) != 0 {
			t.Fatalf("TaskText was asked about %+v, want never", answer.asked)
		}
		if sent := h.wantPrompts(1)[0]; sent.Message != textOneTask {
			t.Fatalf("prompt = %+v, want the task as it was queued", sent)
		}
	}
}

// The daemon's own delivery of a relaunch's record, which the machine queues once the process of
// an idle claim is gone and launched again (queueRelaunch), names no phase and no run, so the phase
// test of currentTaskText leaves it out and it is never put to TaskText: it is sent as its
// sentence alone. The fault is a relaunch delivery that carried a phase, which a test of its id
// alone would have had to keep out.
func TestTheDeliveryOfARelaunchsRecordIsNeverPutToTaskText(t *testing.T) {
	answer := &taskTextAnswer{task: textTwoTask}
	h := taskTextHarness(t, answer)
	h.reach(StateIdle)
	h.observe(runtime.Gone)
	h.relaunched()
	p := h.pending()
	if !p.Relaunch() || p.Phase != "" || p.Generation != 0 {
		t.Fatalf("the relaunched idle claim holds %+v, want the daemon's own delivery of the relaunch's record, with no phase and no run", p)
	}
	if prompts := h.prompts(); len(prompts) == 0 || prompts[len(prompts)-1].DeliveryID != p.ID || len(answer.asked) != 0 {
		t.Fatalf("the claim was sent %+v and TaskText was asked about %+v, want the relaunch's delivery sent last and no ask", prompts, answer.asked)
	}
}

// A held task that carries news of the approved design is rendered from its news (currentNews), so
// a later text of its task's block reaches the prompt through that news: Deps.TaskText answers the
// news rewritten, and the render's task and news are written together, under the same id. A
// render that fails writes nothing, so the stored task keeps the news its text names.
func TestAHeldTaskWithNewsTakesALaterTextOnlyWithItsRender(t *testing.T) {
	const later = `{"task":"the start's own text with a later task text"}`
	for _, tc := range []struct {
		name  string
		fails bool
	}{{"the render answers", false}, {"the render fails", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBareHarness(t)
			h.deps.TaskText = func(_ context.Context, _ Claim, d Delivery) (string, []byte, error) {
				return d.Task, []byte(later), nil
			}
			h.deps.DesignNews = func(_ context.Context, _ Claim, d Delivery) (string, []byte, error) {
				if tc.fails {
					return "", nil, errBoom
				}
				return "rendered from " + string(d.News), d.News, nil
			}
			c := queuedClaim()
			if err := h.store.PutClaim(h.ctx, c); err != nil {
				t.Fatal(err)
			}
			h.start(c)
			h.reach(StateReady)

			err := h.handle(newsStart("outbox:41", 41))
			stored, ok := h.store.delivery(testToken)
			if tc.fails {
				if !ok || stored.Task != queuedTask || !bytes.Equal(stored.News, []byte(queuedNews)) {
					t.Fatalf("a failed render (the send answered %v) left the stored delivery %+v (held %t), want the task as the start queued it, with the news its text names",
						err, stored, ok)
				}
				return
			}
			want := "rendered from " + later
			if err != nil || !ok || stored.ID != "outbox:41" || stored.Task != want || !bytes.Equal(stored.News, []byte(later)) {
				t.Fatalf("the send answered %v and stored %+v (held %t), want outbox:41 holding the render of the later news and that news", err, stored, ok)
			}
			if sent := h.wantPrompts(1)[0]; sent.DeliveryID != "outbox:41" || sent.Message != want {
				t.Fatalf("prompt = %+v, want outbox:41 carrying %q", sent, want)
			}
		})
	}
}
