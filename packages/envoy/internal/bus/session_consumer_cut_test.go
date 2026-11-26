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
// Reconnects is read before Status on each connection: nats.go counts a reconnect before the
// connection reads CONNECTED again (doReconnect increments the count under the lock, then changes
// status to CONNECTED), so a reconnect between the two reads raises the count while the status
// still reads RECONNECTING, and the next poll sees both. The other order — Status before
// Reconnects — could pair a CONNECTED status from before the cut with a reconnect count that has
// not yet risen, the same class of stale-state read the row itself is about.
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
