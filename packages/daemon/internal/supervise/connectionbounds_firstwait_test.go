package supervise

// The bounds and the connection (connectionbounds_test.go): the row of R5's first wait
// (TimerTake), over a value the worker reported seeing.

import (
	"testing"
	"time"
)

// Row 13: a value the worker reported seeing and has not taken, its first wait armed, when the
// connection closes: the first wait is disarmed, a whole first bound with no connection charges
// nothing, and the wait is armed whole again at the agent's answer to the reconnect hello's ask.
func TestChargeNothingForTheFirstWaitWhileNoConnectionStands(t *testing.T) {
	h := boundsHarness(t, longReconnect)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	h.seen()
	if !h.armed(TimerTake) {
		t.Fatal("the first wait over the seen value is not armed")
	}

	h.reconnectLater()

	h.wantUnarmed("after the close", TimerTake)
	h.advance(takeBound)
	h.wantCharges()
	h.helloOf(true, false)
	h.advance(takeBound)
	h.wantCharges()
	h.answerAsk(false)
	h.wantArmedFromNow(TimerTake, takeBound, "at the answer")
	h.advance(takeBound - time.Second)
	h.wantCharges()
	h.advance(time.Second)
	h.wantCharges(WaitTake)
}
