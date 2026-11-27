package bus

import (
	"fmt"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// reconnectsBeforeCut reads how many times each connection has reconnected so far. A test reads
// them before CutStream, then awaitReconnectsAfterCut waits until each count has risen and both
// connections are CONNECTED: that is the event CutStream causes, as the relay closes each client
// once its upstream ends, and nats.go reconnects through it. A wait on Connected() alone can
// pass on a connection that was connected before the Stop, before the client reads the relay's
// EOF: the publish that follows then meets the reconnect, and nats.go, whose reconnect buffer is
// off, answers 'outbound buffer limit exceeded'.
func reconnectsBeforeCut(owner *Client, consumers *SessionConsumers) (uint64, uint64) {
	return owner.Conn.Stats().Reconnects, consumers.client.Conn.Stats().Reconnects
}

// awaitReconnectsAfterCut waits until both connections have reconnected at least once more than
// the counts read before CutStream, both report CONNECTED, and both flush without error. The
// flush check catches the case where the relay's late close of the old client socket triggers a
// second reconnect after the first one passed.
//
// Read Reconnects before Status. A reconnect attempt that dials the server but then fails raises
// Stats().Reconnects (nats.go v1.55.0 :3578) before processConnectInit fails and sets the status
// back to RECONNECTING (:3587). Read Status first, and a CONNECTED read from before that attempt
// can pair with the count the attempt raised, so the wait would pass on a connection that is
// still reconnecting. A reconnect that succeeds cannot be seen half done: it raises the count and
// sets CONNECTED under one hold of nc.mu, which Stats() and Status() each take. Reading the count
// first stops the stale pair, and so does the Flush that follows; each is enough on its own, and
// the test does both.
func awaitReconnectsAfterCut(t *testing.T, owner *Client, consumers *SessionConsumers, ownerBefore, consumersBefore uint64) {
	t.Helper()
	testwait.Until(t, "the stream's owner and the session consumers to reconnect after the cut", func() (string, bool) {
		ownerReconnects := owner.Conn.Stats().Reconnects
		ownerStatus := owner.Conn.Status()
		consumersReconnects := consumers.client.Conn.Stats().Reconnects
		consumersStatus := consumers.client.Conn.Status()

		if ownerStatus != natsgo.CONNECTED || ownerReconnects <= ownerBefore {
			return fmt.Sprintf("owner: status %v, %d reconnect(s) above before; consumers: status %v, %d above before",
				ownerStatus, ownerReconnects-ownerBefore, consumersStatus, consumersReconnects-consumersBefore), false
		}
		if consumersStatus != natsgo.CONNECTED || consumersReconnects <= consumersBefore {
			return fmt.Sprintf("owner: status %v, %d reconnect(s) above before; consumers: status %v, %d above before",
				ownerStatus, ownerReconnects-ownerBefore, consumersStatus, consumersReconnects-consumersBefore), false
		}
		if err := owner.Conn.Flush(); err != nil {
			return fmt.Sprintf("owner flush: %v", err), false
		}
		if err := consumers.client.Conn.Flush(); err != nil {
			return fmt.Sprintf("consumers flush: %v", err), false
		}
		return fmt.Sprintf("owner: status %v, %d reconnect(s) above before; consumers: status %v, %d above before",
			ownerStatus, ownerReconnects-ownerBefore, consumersStatus, consumersReconnects-consumersBefore), true
	})
}
