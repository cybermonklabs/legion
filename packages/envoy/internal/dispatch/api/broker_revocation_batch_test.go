package api

import (
	"testing"
	"time"
)

// TestAPassClaimsOnlyWhatItsWindowLetsItSend holds brokerRevocationBatch to the rule it exists
// for: a pass sends its claimed rows one after another, each call bounded by the broker client's
// own timeout, and may start a call only while the claim is younger than the retry window less
// that timeout (store.BrokerRevocations.Sendable). So the kth call starts (k-1) × timeout after
// the claim, and a batch of n needs (n-1) × timeout below the window — a batch beyond that stamps
// rows the pass never sends, and since the next claim orders by tried_at, the same moment for all
// of them, then recorded_at, the pass sends the same first rows again each time and the rest
// starve.
//
// Control, in the test: the batch before this change, 20, breaks the rule at production's
// five-second client, which is what the starvation was.
func TestAPassClaimsOnlyWhatItsWindowLetsItSend(t *testing.T) {
	t.Parallel()
	fits := func(batch int, timeout time.Duration) bool {
		return time.Duration(batch-1)*timeout < brokerRevocationRetry-timeout
	}
	for _, c := range []struct {
		timeout time.Duration
		want    int
	}{
		{5 * time.Second, 5},
		{time.Second, 29},
		{10 * time.Second, 2},
		{20 * time.Second, 1},
		{30 * time.Second, 1},
		{time.Minute, 1},
		{0, 1},
		{-time.Second, 1},
	} {
		got := brokerRevocationBatch(c.timeout)
		if got != c.want {
			t.Errorf("brokerRevocationBatch(%s) = %d, want %d", c.timeout, got, c.want)
		}
		if got < 1 {
			t.Errorf("brokerRevocationBatch(%s) = %d; a pass must claim at least one row", c.timeout, got)
		}
		if c.timeout > 0 && c.timeout < brokerRevocationRetry && !fits(got, c.timeout) {
			t.Errorf("a pass of %d calls bounded at %s each starts its last past the send window %s; want a batch whose calls all start inside it",
				got, c.timeout, brokerRevocationRetry-c.timeout)
		}
	}
	if fits(20, 5*time.Second) {
		t.Error("control: a batch of 20 at a five-second client fits the send window, so this test cannot see the starvation it exists for")
	}
}
