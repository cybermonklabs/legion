// What a request on a client's connection waits for: its answer, or the loss of the connection it
// was sent on, whichever comes first (Request).

package bus

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// ErrConnectionLost is what a request answers when the connection it was sent on was lost before
// its answer came (Request), and the cause of a drain hook's context when the connection was lost
// while the hooks ran (AddDrainHook). nats.go v1.53.1 fails nothing a lost connection was waiting
// for: a dropped socket goes through processOpErr, which clears the pending flushes and leaves the
// pending requests (nats.go:3556-3586), so a request whose reply the drop lost waits for its whole
// deadline, and only a close or a forced reconnect clears them (clearPendingRequestCalls,
// nats.go:2609-2610, :6085-6086). A request made while the connection is reconnecting is refused
// at once instead, since the reconnect buffer is off ("outbound buffer limit exceeded",
// connect.go's options).
//
// A request that answers it may or may not have reached the server, so its caller treats it as a
// request that drew no answer: a read is read again, and a write is either one the server takes
// twice without harm (a consumer create of the same configuration, a publish under a Nats-Msg-Id)
// or one the caller reads back before it writes again. Its retry rule is Reconnected: wait for the
// client's connection to be back, inside the caller's own budget, and send the request again on it.
// A caller that holds a lock or a shared read releases it before it waits, since that wait is the
// whole of the outage.
//
// It is the error of a client that is still running and so recovers its connection, also when that
// connection closed under the request rather than dropped (bindRequest): the client replaces a
// closed connection (onClosed). A client that was stopped (Close, or Drain once its connection has
// closed) recovers nothing, and a request cut off by its stop answers nats.ErrConnectionClosed
// instead, as nats.go answers a request its own close cut off: a caller that reads that error as
// lasting (cistore's kvErrorLasts, sendStored's reconnect branch) reads it as it always did
// (lostOrClosed).
var ErrConnectionLost = errors.New("bus: the NATS connection was lost")

// connectionLost is the error Request returns for a request the loss of client's connection cut
// off. It is ErrConnectionLost (errors.Is) and names the client whose reconnect the retry waits
// for (Reconnected).
type connectionLost struct{ client *Client }

func (e *connectionLost) Error() string {
	return ErrConnectionLost.Error() + " before the request was answered"
}

func (e *connectionLost) Is(target error) bool { return target == ErrConnectionLost }

// Reconnected reports whether err is a request the loss of its connection cut off (Request) and
// that client is connected again within ctx, so the caller sends the request again: it waits for
// the reconnect as a publish does (ensureConnWithContext), until it lands, ctx ends or the client
// stops. Any other err, and a ctx that has ended, report false at once: the caller answers err as
// it always did. The caller releases what it holds before it calls this, since the wait lasts as
// long as the outage.
func Reconnected(ctx context.Context, err error) bool {
	var lost *connectionLost
	if !errors.As(err, &lost) || ctx.Err() != nil {
		return false
	}
	return lost.client.ensureConnWithContext(ctx) == nil
}

// connForRequest is the wait a request on c makes for a connection before it is sent:
// ensureConnWithContext's, except on a client every caller of which retries a lost request itself
// (withLossAtOnce), where a request made while nats.go reconnects its connection answers
// ErrConnectionLost at once. Such a caller holds a session's lock, the shared stream read or a
// relay's restart turn, and releases it before it waits (Reconnected); a wait made here would be
// made with it held. A closed connection is still replaced, a client still awaiting its first
// connection still waits for it, and a stopped client still answers as ensureConnWithContext does.
func (c *Client) connForRequest(ctx context.Context) error {
	if c.lossAtOnce && !c.stopped() {
		c.mu.Lock()
		conn, awaiting := c.Conn, c.awaitingConnection
		c.mu.Unlock()
		if conn != nil && !awaiting && !conn.IsClosed() && !conn.IsConnected() {
			return &connectionLost{client: c}
		}
	}
	return c.ensureConnWithContext(ctx)
}

// Request sends one request on c's connection through send and returns its answer. send is handed
// a context that ends with ctx and also the moment that connection is lost, and passes it on to
// the nats.go call it makes (RequestWithContext, NextMsgWithContext, a JetStream call's
// nats.Context): nats.go then removes the request's reply wait as the context ends, so a request
// whose reply the loss took fails within the time nats.go takes to report the loss, instead of
// waiting out its deadline. Such a request answers an error that is ErrConnectionLost
// (connectionLost), whatever send returned for it, and so does a request made while the
// connection is down, which is not sent at all. Its caller retries it as Reconnected says.
//
// A send that a context does not bound - a nats.go call that takes none, or one whose own wait
// (the JetStream MaxWait) is the bound the caller relies on - goes through RequestBounded instead,
// since a context passed to the call would replace that wait. A nil c is a connection no client
// tracks (bus.Dial's, a test's own): send runs under ctx alone.
//
// The connection's life is the client's (requestLife): it begins at the first request after the
// connection is connected, and ends at the first report of its loss nats.go gives, the status
// change it makes inside processOpErr (nats.go:3565). That report is taken rather than the
// disconnect callback the client already logs and follows (onDisconnect, wired by connect.go's
// options as the DisconnectedErrCB), because nats.go runs that callback on its one callback
// goroutine, after the connection's reader has exited and behind every callback queued before
// it, the reconnect hooks of the reconnect before among them: a request a reconnect hook made
// would wait for its own hook to return before the callback could end it. The callback ends it
// too, and so does a close (onClosed), for a status report nats.go dropped (endLifeIfDown).
func Request[T any](ctx context.Context, c *Client, send func(context.Context) (T, error)) (T, error) {
	bound, finish := c.bindRequest(ctx)
	if bound.Err() != nil && ctx.Err() == nil {
		var none T
		return none, finish(context.Cause(bound))
	}
	value, err := send(bound)
	return value, finish(err)
}

// RequestBounded is Request for call, a request whose nats.go call takes no context (a legacy KV
// read or write) or whose JetStream MaxWait is its bound: it is waited for as Bounded waits for
// one, so the wait ends at the earliest of call's answer, ctx's end and the connection's loss, and
// a call cut off goes on to its own end in the background with its answer dropped. A write it cut
// off may land after the caller was answered ErrConnectionLost, which is that error's own
// ambiguity: the caller reads before it writes again, as it does after a write that drew no
// answer.
func RequestBounded[T any](ctx context.Context, c *Client, call func() (T, error)) (T, error) {
	return Request(ctx, c, func(ctx context.Context) (T, error) {
		value, _, err := Bounded(ctx, call)
		return value, err
	})
}

// endedLife is the life a request on a connection that is down is bound to: it has ended, so the
// request is not sent (Request).
var endedLife = func() context.Context {
	ctx, end := context.WithCancelCause(context.Background())
	end(ErrConnectionLost)
	return ctx
}()

// bindRequest is the context a request made under ctx on c's connection is sent with, and the func
// that ends it and names the request's error: one the end of the connection's life cut off, while
// ctx itself had not ended, is lostOrClosed's. A nil c binds nothing.
//
// A request nats.go answered nats.ErrConnectionClosed is one such: the close cut it off, whether or
// not the life has ended yet. nats.go's close answers every pending request (clearPendingRequestCalls,
// nats.go:6086) before it reports CLOSED (changeConnStatus, :6142), and the life ends only once its
// goroutine reads that report, so the request can come back first. Read by the life alone, a close
// on a running client - nats.go's own, after the same authorization error twice in a row or an
// unclassified -ERR (connect.go's options) - would answer its caller the raw closed error, which
// Reconnected does not retry, where the client goes on to replace the connection (onClosed).
func (c *Client) bindRequest(ctx context.Context) (context.Context, func(error) error) {
	if c == nil {
		return ctx, func(err error) error { return err }
	}
	life := c.requestLife()
	bound, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(life, func() { cancel(ErrConnectionLost) })
	if life.Err() != nil {
		cancel(ErrConnectionLost)
	}
	return bound, func(err error) error {
		stop()
		cancel(nil)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if life.Err() == nil && !errors.Is(err, nats.ErrConnectionClosed) {
			return err
		}
		return c.lostOrClosed(err)
	}
}

// lostOrClosed is the error of a request, which answered err, whose connection's life ended before
// its answer: connectionLost on a client that is still running, which recovers its connection and
// whose caller retries (Reconnected); and on a client that was stopped, err itself when it is
// nats.ErrConnectionClosed and otherwise an error wrapping that one, since such a client's
// connection is gone for good and every caller reads that error as lasting.
func (c *Client) lostOrClosed(err error) error {
	if !c.stopped() {
		return &connectionLost{client: c}
	}
	if errors.Is(err, nats.ErrConnectionClosed) {
		return err
	}
	return fmt.Errorf("bus: the client was stopped before the request was answered: %w", nats.ErrConnectionClosed)
}

// requestLife is the life of the client's connection that a request made now is bound to: the one
// running while its connection is still the client's and connected, or else a new one when the
// connection is connected; a connection that is not is endedLife. Reading the state here as well
// as listening for its change ends a life whose report of the loss went missing at the next
// request.
//
// A new life listens for the connection's loss before it reads the connection's state, so a loss
// between the two still ends it, and a goroutine of its own ends it at the first of those reports
// and then stops listening. nats.go drops a status report it finds a listener's channel holding an
// unread one for (sendStatusEvent, nats.go:6682-6699), which a goroutine that reads at once makes a
// microsecond window; the disconnect and close callbacks end it besides (endLifeIfDown).
func (c *Client) requestLife() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn := c.Conn
	if c.life != nil && c.life.Err() == nil && c.lifeConn == conn && conn.IsConnected() {
		return c.life
	}
	if c.endLife != nil {
		c.endLife(ErrConnectionLost)
	}
	if conn == nil {
		return endedLife
	}
	lost := conn.StatusChanged(nats.DISCONNECTED, nats.RECONNECTING, nats.CLOSED)
	if !conn.IsConnected() {
		conn.RemoveStatusListener(lost)
		return endedLife
	}
	life, end := context.WithCancelCause(context.Background())
	c.life, c.endLife, c.lifeConn = life, end, conn
	go func() {
		select {
		case <-lost:
		case <-life.Done():
		}
		end(ErrConnectionLost)
		conn.RemoveStatusListener(lost)
	}()
	return life
}

// endLifeIfDown ends the life of the connection requests are bound to once that connection is no
// longer connected: the disconnect and close callbacks' share of ending it (requestLife). It reads
// the connection's own state rather than trusting the callback, since nats.go runs a callback
// late, behind those queued before it, and a life begun on a connection that is connected again
// by then is not the one that was lost.
func (c *Client) endLifeIfDown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.endLife != nil && c.lifeConn != nil && !c.lifeConn.IsConnected() {
		c.endLife(ErrConnectionLost)
	}
}
