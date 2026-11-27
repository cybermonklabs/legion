// How a request tells that the connection it was sent on can no longer answer it.

package bus

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go"
)

// ErrRequestLostWithConnection ends a request whose connection was lost before its answer came:
// the connection the request went out on was replaced by another the client dialled, was closed,
// or reconnected in place (connWatch.lostRequest). The server drops a request with the
// connection it came on, and nats.go fails none that is waiting for its answer when its
// connection drops: it fails the pending flushes (processOpErr) but leaves its request map and
// every sync subscription waiting, so such a request would otherwise wait out its whole deadline
// for an answer nothing will send.
//
// It is no answer of the server's. The request may have reached the server and been acted on
// before its answer was lost, so a caller that judges whether a call changed anything counts it
// as unanswered, and a caller that retries sends it again on the connection that came back, at
// once rather than after a backoff: the failure says nothing about the request itself.
// Client.requestConfirmed returns it (every session consumer call, the stream read behind them
// and the stored-message reads), and so do Client.RequestCore (the send route's connection probe),
// a KV call BoundedOn waits for, a call BoundedOnConnection waits for (roles.Buckets' reads) and a
// JetStream request RequestOnConnection makes (the stream's ensure on each connection event, the
// STREAM.INFO of Client.TakeoverRecords and Client.KeptRoleLanes, and Dispatch's delivery intake
// taking its durable over).
//
// A close, and so the replacement that follows one, is the exception. nats.go then fails each
// request in its request map and wakes each sync subscription itself, with
// nats.ErrConnectionClosed, before it runs the close callback the watch hears. A ForceReconnect
// fails each request in the request map too, with nats.ErrDisconnected, since it leaves the
// connection open (nats.go@v1.55.0 nats.go:4962-4966, context.go:69-73). So each of those callers
// but RequestCore, whose channel subscription a close leaves open, can return
// nats.ErrConnectionClosed for a close and a request in the request map nats.ErrDisconnected for a
// ForceReconnect, in place of this error.
var ErrRequestLostWithConnection = errors.New("bus: the request was lost with its NATS connection")

// connWatch follows the connection one request, or one pull's requests (SessionPull), went out on,
// to tell a request the server may still answer from one a connection event dropped.
type connWatch struct {
	client *Client
	// conn is the connection the request goes out on, the client's connection when the watch
	// began (watchConnection).
	conn *nats.Conn
	// reconnects is conn's reconnect count when the watch began (nats.Conn.Stats): the count
	// moves on a reconnect in place, which drops the request while the connection stays the same
	// one.
	reconnects uint64
	// changed is the client's connection-event channel (Client.ConnectionChanged), taken before
	// conn was read, so an event between the two closes it, and taken again after each event the
	// watch wakes on before the watch reads the connection again (lost, bound), for the same
	// reason. Only the one goroutine that waits on the watch reads or replaces it.
	changed <-chan struct{}
}

// watchConnection reads the client's connection for a request about to go out on it. The event
// channel is taken before the connection and its reconnect count are read, so an event between
// the two is seen rather than leaving the request waiting on an answer the server dropped. An
// event before the request is sent, which nats.go then sends on the connection that came back,
// ends it too: the cost is one request sent again, where the cost of missing the event is a wait
// to the request's deadline. A client with no connection answers a watch whose conn is nil.
func (c *Client) watchConnection() connWatch {
	watch := connWatch{client: c, changed: c.ConnectionChanged()}
	if watch.conn = c.Connection(); watch.conn != nil {
		watch.reconnects = watch.conn.Stats().Reconnects
	}
	return watch
}

// watchConnectionWithin is watchConnection for a request its caller bounds by ctx. The reconnect
// count's read waits for nats.go's connection lock (nats.Conn.Stats), which nats.go holds through
// a reconnect's dial and handshake, so it is made within ctx (readWithin): a read that has not
// answered when ctx ends reports false, and the request sends nothing and answers as a wait for a
// reconnecting connection that ctx ended (reconnectWaitEnded), rather than holding its caller
// until the attempt ends. A ctx that can never end reads it at once on the caller's goroutine.
func (c *Client) watchConnectionWithin(ctx context.Context) (connWatch, bool) {
	return readWithin(ctx, c.watchConnection)
}

// lost reports whether a connection event of the client (ConnectionChanged) has dropped the
// watched request. An event that left the request's connection the client's current one, open,
// and with its reconnect count unmoved is none of the request's (lostRequest): it is a disconnect
// the reconnect has not followed yet, or the moment between a reconnect nats.go has installed,
// which the watch read, and the callback that reports it. The watch goes on past such an event,
// on the next channel, which it takes before it reads the connection: the reconnect that follows
// a disconnect closes no channel when none is taken (Client.connectionChangedLocked), so a channel
// taken after a read that found the connection still unchanged could miss it and wait on for an
// event that has already come.
func (w *connWatch) lost() bool {
	for {
		select {
		case <-w.changed:
		default:
			return false
		}
		w.changed = w.client.ConnectionChanged()
		if w.lostRequest() {
			return true
		}
		w.client.watchWentOnForTest()
	}
}

// lostRequest reports whether the watched request is lost: the connection it went out on can no
// longer answer it, because the server drops the request when that connection is replaced by
// another the client dialled, when it is closed, and when it reconnects in place, which leaves
// the connection the same one and moves its reconnect count. A disconnect alone ends no request:
// it ends at the reconnect that follows, which is the first moment the caller can send it again,
// and by then the server has dropped the old connection's request. A pull ended at the disconnect
// could have its caller publish or pull while the server still held that request, and a message
// the server delivered to it would wait out the consumer's ack wait.
func (w *connWatch) lostRequest() bool {
	return w.client.Connection() != w.conn || w.conn.IsClosed() || w.conn.Stats().Reconnects != w.reconnects
}

// bound returns ctx ended, with ErrRequestLostWithConnection as its cause (lostWithConnection),
// as soon as the watched request is lost, and the func that releases the goroutine watching for
// that, which the caller defers. A wait that takes no channel but a context - a sync
// subscription's NextMsgWithContext, a JetStream request handed a context (RequestOnConnection) -
// waits on it to end at the connection event rather than at ctx's deadline, and so does a wait that
// must read no state of nats.go's itself (boundedBeside, the wait of BoundedOn and
// BoundedOnConnection), since the loss is read here. After each event it wakes on it takes the next
// channel before it reads the connection, as lost does, so a reconnect between the two is seen.
func (w connWatch) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithCancelCause(ctx)
	go func() {
		for {
			select {
			case <-bounded.Done():
				return
			case <-w.changed:
			}
			w.changed = w.client.ConnectionChanged()
			if w.lostRequest() {
				cancel(ErrRequestLostWithConnection)
				return
			}
			w.client.watchWentOnForTest()
		}
	}()
	return bounded, func() { cancel(nil) }
}

// watchWentOnForTest runs the client's watchWentOn test hook, when one is set, on a watch that
// has taken its next channel after an event that left its request unlost (lost, bound). Nothing
// outside a test sets it.
func (c *Client) watchWentOnForTest() {
	if c.watchWentOn != nil {
		c.watchWentOn()
	}
}

// lostWithConnection reports whether ctx, a context bound returned, ended because the request it
// watched was lost with its connection.
func lostWithConnection(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrRequestLostWithConnection)
}
