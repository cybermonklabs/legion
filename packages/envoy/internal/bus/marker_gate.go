package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/consumernames"
)

// ErrConvertedStore is ReadMarker's answer for a selection bucket the cutover converted.
var ErrConvertedStore = errors.New("bus: the selection bucket says the cutover converted every session consumer to a fresh name")

// ErrMarkerUnread refuses, unsent, a consumer call of a client whose marker gate is shut
// (GateOnMarker): no read of the selection bucket's cutover marker on the connection the call would
// go out on has answered that the cutover did not convert the store. The listener answers a
// registration it refuses so 503 consumer_error, naming the marker.
var ErrMarkerUnread = fmt.Errorf("bus: the cutover marker %s of the selection bucket %s has not been read on this NATS connection", consumernames.SentinelKey, consumernames.SessionBucket)

// ErrMarkerRead marks ReadMarker's failed read of the selection bucket's cutover marker: the gate
// stays shut, and the reconnect hook that read it fails its run, so the client's reconnect hooks
// stall until a read succeeds. The listener's /healthz names the marker read, not the KV watch
// rebuild, when it is what stalls them.
var ErrMarkerRead = fmt.Errorf("bus: read the cutover marker %s of the selection bucket %s before running fixed names", consumernames.SentinelKey, consumernames.SessionBucket)

// markerGate is a client's gate on the selection bucket's cutover marker, in the fixed naming mode
// (GateOnMarker). It is open only for the connection a read of the marker answered on: the client's
// *nats.Conn and that connection's reconnect count when the read began. A reconnect in place counts
// one more and a connection dialled to replace a closed one is another *nats.Conn, so every
// disconnect shuts it, before nats.go runs any callback of the next connection.
type markerGate struct {
	mu sync.Mutex
	// conn and reconnects name the connection the gate is open for; conn is nil while it is shut.
	conn       *nats.Conn
	reconnects uint64
}

// connectionNow is conn's reconnect count, and whether conn is connected. The status is read
// first: nats.go counts a reconnect before it marks the connection connected, under the same lock,
// so a count read after a connected status is the count of a connection that was up when the
// status was read.
func connectionNow(conn *nats.Conn) (uint64, bool) {
	if conn == nil || conn.Status() != nats.CONNECTED {
		return 0, false
	}
	return conn.Stats().Reconnects, true
}

// openFor opens g for conn at reconnects when conn is still that connection, connected; a read
// that a disconnect overtook opens nothing.
func (g *markerGate) openFor(conn *nats.Conn, reconnects uint64) {
	if now, ok := connectionNow(conn); !ok || now != reconnects {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.conn, g.reconnects = conn, reconnects
}

// shut shuts g: a disconnect hook (AddDisconnectHook).
func (g *markerGate) shut() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.conn = nil
}

// admits is nil when g is open for conn as it is now, and ErrMarkerUnread otherwise.
func (g *markerGate) admits(conn *nats.Conn) error {
	g.mu.Lock()
	open, reconnects := g.conn, g.reconnects
	g.mu.Unlock()
	if open == nil || open != conn {
		return ErrMarkerUnread
	}
	if now, ok := connectionNow(conn); !ok || now != reconnects {
		return ErrMarkerUnread
	}
	return nil
}

// GateOnMarker gives c, in the fixed naming mode, a gate on the selection bucket's cutover marker:
// from now on every consumer call c makes for the session consumers (SessionConsumers) and every
// create, update or bind of the listener's durable on c (MarkerUnread) is refused with
// ErrMarkerUnread, unsent, until a read of the marker on c's current connection (ReadMarker)
// answers that the cutover did not convert the store. Each connection event shuts it again: a
// disconnect hook shuts it at once, and a connection the gate was not opened for is refused
// whatever the hooks have run (markerGate). A reconnect hook reads the marker on each new
// connection: a read that answers no marker or one that is not done opens the gate; one that
// answers done calls converted with ErrConvertedStore, on which the listener stops as it does at
// its start, and leaves the gate shut; any other read fails the hook's run, which the client's
// recovery runs again with backoff, and the gate stays shut meanwhile. So today's names never run
// over a store the cutover converted while this process was cut off, nor over a marker it cannot
// read. A second call does nothing.
func (c *Client) GateOnMarker(converted func(error)) {
	gate := &markerGate{}
	if !c.marker.CompareAndSwap(nil, gate) {
		return
	}
	c.AddDisconnectHook(gate.shut)
	c.AddReconnectHook(func(ctx context.Context, _ *nats.Conn) error {
		err := c.ReadMarker(ctx)
		switch {
		case err == nil, errors.Is(err, ErrConsumerNotGranted):
			return nil
		case errors.Is(err, ErrConvertedStore):
			if converted != nil {
				converted(err)
			}
			return nil
		}
		return err
	})
}

// ReadMarker reads, in the fixed naming mode, the selection bucket's cutover marker on c's current
// connection, and opens c's gate (GateOnMarker) for that connection when it answers that the cutover
// did not convert the store. It answers:
//   - nil for no marker or one that is not done: no bucket (the stream not found, 10059), or a
//     sentinel whose marker is none or rolled back;
//   - ErrConsumerNotGranted for a read the connection's user may not make, which is no marker too,
//     for its caller to log: a hub whose listener grant has no read of the bucket was never given the
//     window's grants, which come before the bucket's creation, and the listener beside Dispatch
//     holds no such read at all;
//   - ErrConvertedStore, naming ENVOY_CONSUMER_NAMES, when the sentinel says the cutover is done: the
//     bucket holds the sessions' fresh names, and today's fixed names must not run over it;
//   - any other failed read as ErrMarkerRead wrapping its error, on which the gate stays shut and
//     the caller reads again: no connection, a timeout, no responder (only the stream's own
//     not-found answer is no bucket), a bucket that exists with no sentinel or a delete marker over
//     it (consumernames.ErrNoSentinel), or a value under the sentinel's key that is not the session
//     family's sentinel (consumernames.ErrMalformedSentinel: a server answer stored there through a
//     request's reply subject, another key's record, another family's sentinel).
func (c *Client) ReadMarker(ctx context.Context) error {
	conn := c.Connection()
	reconnects, connected := connectionNow(conn)
	state, err := c.SelectionStore().State(ctx)
	switch {
	case errors.Is(err, consumernames.ErrNoStore):
		err = nil
	case errors.Is(err, ErrPublishDenied):
		err = fmt.Errorf("%w: %w", ErrConsumerNotGranted, err)
	case err != nil:
		return fmt.Errorf("%w: %w", ErrMarkerRead, err)
	case state.Cutover == consumernames.CutoverDone:
		return fmt.Errorf("%w: %s=%s runs today's fixed names, and the bucket %s holds fresh ones: set %s=%s", ErrConvertedStore, consumernames.ModeEnvVar, consumernames.ModeFixed, consumernames.SessionBucket, consumernames.ModeEnvVar, consumernames.ModeFresh)
	}
	if gate := c.marker.Load(); gate != nil && connected {
		gate.openFor(conn, reconnects)
	}
	return err
}

// MarkerUnread is nil when c has no marker gate (GateOnMarker) or its gate is open for c's current
// connection, and ErrMarkerUnread otherwise: the listener asks it before it creates, corrects or
// binds its durable in the fixed mode.
func (c *Client) MarkerUnread() error {
	return c.markerUnreadOn(c.Connection())
}

// markerUnreadOn is MarkerUnread for a call about to go out on conn.
func (c *Client) markerUnreadOn(conn *nats.Conn) error {
	if gate := c.marker.Load(); gate != nil {
		return gate.admits(conn)
	}
	return nil
}
