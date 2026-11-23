package supervise

import (
	"bytes"
	"testing"
)

// The copy of the news a prompt carried is written where Oh My Pi acknowledges that prompt (acked,
// Delivery.Carried), and it is the one record of that prompt that outlasts the daemon: a daemon
// restarted before the prompt's turn starts holds no prompt outstanding (Machine.outstanding is
// memory only), so the copy is what tells it that a prompt of the task reached the agent, and what
// that prompt carried. The booted daemon's question (askFirst) then asks the agent, takes the turn
// the agent is in for the task's, and confirms the task with that copy, and nothing is sent into
// that turn. No send fails on this path, so no refusal takes anything back here. Without the copy
// the task would read as one no acknowledged prompt carried (unsentNews): the booted daemon would
// send it again into the turn of the prompt the agent is already running, and that turn's
// confirmation would raise nothing of the news the agent read.
func TestARestartTakesTheTurnOfAnAcknowledgedNewsPromptForItsTask(t *testing.T) {
	answer := &designNewsAnswer{task: queuedTask, news: []byte(queuedNews)}
	h := newsHarness(t, answer)
	h.reach(StateReady)
	h.must(newsStart("outbox:46", 46))
	sent := h.wantPrompts(1)[0]

	h.restart()
	h.conn.SetStreaming(true)
	h.must(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})

	h.wantPrompts(1)
	h.wantState(StateWorking)
	stored, ok := h.store.delivery(testToken)
	if !ok || stored.ID != sent.DeliveryID || stored.ConfirmedAt.IsZero() || !bytes.Equal(stored.Carried, []byte(queuedNews)) {
		t.Fatalf("stored delivery after the booted daemon's question = %+v (held %t), want %s confirmed by the turn the agent is in, "+
			"carrying the news its acknowledged prompt carried", stored, ok, sent.DeliveryID)
	}
}
