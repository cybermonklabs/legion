package supervise

import (
	"bytes"
	"testing"
)

// The copy of the news an accepted take carried is written by the accept itself (accept,
// Delivery.Carried), in the one write that records the take, and it is the one record of that
// take's credit that outlasts the daemon: a daemon restarted before the take's turn starts reads
// the take, its text and its copy from the store. A take of the number the worker sends again
// after the restart is answered the text the record kept (answeredFromRecord), so nothing renders
// the news again and nothing is credited anew; the take's turn is the restored take's, and the
// report that names its number finishes the task, which makes the restored copy final
// (Delivery.Ran) before the retirement. Without the copy kept with the take, the restored take
// would credit nothing of the news the worker read, and a take after it would credit news its text
// never carried.
func TestARestartTakesTheTurnOfAnAcknowledgedNewsPromptForItsTask(t *testing.T) {
	answer := &designNewsAnswer{task: queuedTask, news: []byte(queuedNews)}
	h := newsHarness(t, answer)
	h.reach(StateReady)
	h.must(newsStart("outbox:46", 46))
	taken := h.wantPrompts(1)[0]

	h.restart()

	stored, ok := h.store.delivery(testToken)
	if !ok || stored.ID != taken.DeliveryID || stored.ConfirmedAt.IsZero() || stored.TakenText != taken.Message ||
		!bytes.Equal(stored.Carried, []byte(queuedNews)) {
		t.Fatalf("stored delivery after the restart = %+v (held %t), want %s taken, its text kept, "+
			"crediting the news its accepted take carried", stored, ok, taken.DeliveryID)
	}
	if p := h.pending(); !bytes.Equal(p.Carried, []byte(queuedNews)) {
		t.Fatalf("the restored delivery = %+v, want it crediting the news its accepted take carried", p)
	}
	value, held := h.values.value(testToken)
	if !held {
		t.Fatal("the bucket holds no value of the taken task")
	}
	again, err := h.takeValue(value)
	if err != nil || again.Outcome != TakeOutcomeAccepted || again.Text != taken.Message {
		t.Fatalf("the take sent again after the restart = %+v, %v; want it answered the text the record kept", again, err)
	}
	if len(answer.asked) != 1 {
		t.Fatalf("DesignNews was asked %d times, want once: the restored take is answered from the record", len(answer.asked))
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})

	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending after the restored take's report = %+v, want the task finished", p)
	}
	final, ok := h.store.lastWritten(taken.DeliveryID)
	if !ok || !final.Ran || !bytes.Equal(final.Carried, []byte(queuedNews)) {
		t.Fatalf("the last write of %s = %+v (held %t), want the restored credit marked final", taken.DeliveryID, final, ok)
	}
}
