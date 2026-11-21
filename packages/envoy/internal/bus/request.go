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
// while the hooks ran (AddDrainHook). nats.go v1.55.0 fails nothing a lost connection was waiting
// for: a dropped socket goes through processOpErr, which clears the pending flushes and leaves the
// pending requests (nats.go:3652-3682), so a request whose reply the drop lost waits for its whole
// deadline, and only a close or a forced reconnect clears them (clearPendingRequestCalls,
// nats.go:2667, :6265). A request made while the connection is reconnecting is refused
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
// It is the error of a client that is still running and so recovers its connection, and on such a
// client it is also the answer for a request whose connection nats.go closed under it, whichever of
// nats.go's close error and the end of the connection's life comes first (closedUnder). So is
// nats.go's close error for every other call the bus makes on the client's connection itself,
// outside Request: the dial that replaces a closed connection, a core publish and its flush, a
// subscribe and the end of one, a KV watch's create, and a pulled message's acknowledgement
// (lostIfClosed). A client that was stopped (Close, or Drain once its connection has closed)
// recovers nothing, and a request cut off by its stop answers nats.ErrConnectionClosed instead, as
// nats.go answers a request its own close cut off: a caller that reads that error as lasting
// (cistore's kvErrorLasts, sendStored's reconnect branch) reads it as it always did (lostOrClosed).
// Any other call a stopped client makes answers nats.go's own error.
var ErrConnectionLost = errors.New("bus: the NATS connection was lost")

// connectionLost is the error Request returns for a request the loss of client's connection cut
// off, and lostIfClosed for any other call on that connection that nats.go answered with its close.
// It is ErrConnectionLost (errors.Is) and names the client whose reconnect the retry waits for
// (Reconnected). unsent says the call sent nothing: Request did not send the request at all, since
// the connection's life had ended before it (requestUnsent), or nats.go refused the call before it
// wrote anything. cause is nats.go's own error for that call, kept for the error's text alone, so
// a log line still says what nats.go answered while no comparison matches it.
type connectionLost struct {
	client *Client
	unsent bool
	cause  error
}

func (e *connectionLost) Error() string {
	if e.cause != nil {
		return ErrConnectionLost.Error() + ": " + e.cause.Error()
	}
	return ErrConnectionLost.Error() + " before the request was answered"
}

func (e *connectionLost) Is(target error) bool { return target == ErrConnectionLost }

// Reconnected reports whether err is a request the loss of its connection cut off (Request), or a
// call on that connection nats.go answered with its close (lostIfClosed), and that client is
// connected again within ctx, so the caller sends the request again: it waits for the reconnect as
// a publish does (ensureConnWithContext), and dials a replacement for a connection nats.go has
// closed, until one is connected, ctx ends, the client stops, or each of the dial's ten attempts
// (connectWithContext) has failed, for whatever reason nats.go's dial gave: no server answered,
// the server refused the credential or closed the connection during the handshake, the host did
// not resolve, or the attempt's connect timeout passed. It reports true only once the client's
// connection is connected, so a caller that retries on it does not retry before, and false for
// each other end of that wait. Any other err, and a ctx that has ended, report false at once: the
// caller answers err as it always did. The caller releases what it holds before it calls this,
// since the wait lasts as long as the outage.
func Reconnected(ctx context.Context, err error) bool {
	var lost *connectionLost
	if !errors.As(err, &lost) || ctx.Err() != nil {
		return false
	}
	return lost.client.ensureConnWithContext(ctx) == nil
}

// requestUnsent reports whether err is a request Request did not send, the life of its
// connection having ended before the send (connectionLost.unsent): the server cannot hold what it
// asked, which a stored publish's caller reads as nothing stored (sendUnanswered). A request whose
// send began is never one, though the loss may have come before its bytes left: Request cannot
// tell that from a send the server took.
func requestUnsent(err error) bool {
	var lost *connectionLost
	return errors.As(err, &lost) && lost.unsent
}

// connForRequest is the wait a request on c makes for a connection before it is sent, and a stored
// publish whose connection closed under it before it is sent again (sendStored):
// ensureConnWithContext's, except for a request whose caller holds a lock across it - every request
// on a client every caller of which retries a lost request itself (withLossAtOnce), and one made
// under a context its caller marks so (WithLockHeld) - where a request made while its connection
// is down - nats.go reconnecting it, or nats.go having closed it - answers ErrConnectionLost at
// once, unsent. Such a caller holds a session's lock, a role's keep guard, the shared stream read or
// a relay's restart turn, and releases it before it waits (Reconnected), so a wait made here would
// be made with it held: the dial that replaces a
// closed connection, which lasts until a server answers, up to ten attempts a second apart
// (connectWithContext), is made by that retry instead, through Reconnected's
// ensureConnWithContext, with nothing held. Whether the connection is connected is the probe's
// answer (upNow), which waits at most ConnLockWait for nats.go's connection lock, which nats.go
// holds through a whole reconnect attempt, the dial and the handshake: a lock held longer reads as
// a connection mid-reconnect, down. IsConnected itself would wait for the lock, with the caller's
// held, for up to the attempt's connect timeout, or a hub's authorization timeout while its auth
// service holds the CONNECT. A false "down" the budget gives on a loaded box costs the caller one
// retry, which it makes with nothing held. A client still awaiting its first connection still waits
// for it, a connection nats.go is draining counts as connected (nats.go v1.55.0 nats.go:6586-6588),
// and a stopped client still answers as ensureConnWithContext does.
//
// The caller keeps its lock across the call itself: a session's consumer lock serializes create,
// update and delete of that session's NATS consumer, which have no conditional form, so the call
// cannot be made with the lock released and checked after. So the bound this gives such a caller
// is on the question, not the call: it waits for nats.go at most ConnLockWait before it hears
// the connection is down. The send that follows an "up" answer takes nats.go's lock as every send
// does, and a reconnect whose dial and handshake begin between the answer and the send - a forced
// reconnect, or a drop on a client with several servers, which dials the next at once - holds it
// for that handshake, as the base held every such send.
func (c *Client) connForRequest(ctx context.Context) error {
	return c.connFor(ctx, c.lossAtOnce || lockHeld(ctx))
}

// connForCall is the wait for a connection that a call other than a request bound to the
// connection's life makes before it sends: a publish (publishJetStreamOn, PublishCoreContext,
// SessionConsumers.Publish), a forward to a role's holder (StartRequestStoredTo), a stream read
// (KeptRoleLanes, RequestCore). Like connForRequest it answers ErrConnectionLost, unsent, at once,
// when the probe does not read the connection up, for a caller that marks its context as made
// under a lock (WithLockHeld), and waits for the connection within ctx for any other caller.
func (c *Client) connForCall(ctx context.Context) error {
	return c.connFor(ctx, lockHeld(ctx))
}

// connFor is connForRequest's and connForCall's wait: with atOnce, a running client with a
// connection answers ErrConnectionLost, unsent, at once unless the probe reads it up (upNow);
// otherwise, and for a client with no connection yet, one still awaiting its first, or a stopped
// one, it is ensureConnWithContext.
func (c *Client) connFor(ctx context.Context, atOnce bool) error {
	if atOnce && !c.stopped() {
		c.mu.Lock()
		conn, awaiting := c.Conn, c.awaitingConnection
		c.mu.Unlock()
		if conn != nil && !awaiting {
			if up, _ := c.upNow(conn); !up {
				return &connectionLost{client: c, unsent: true}
			}
			return nil
		}
	}
	return c.ensureConnWithContext(ctx)
}

// LostNow answers ErrConnectionLost, unsent, when the client's connection is down or its state
// unknown as the probe reads it (upNow), which waits at most ConnLockWait for nats.go's
// connection lock, and nil when it is up, on any client. A caller that holds a lock - a session's
// consumer lock, a relay's restart turn, a role's keep guard - asks it before a call on the main
// client, so it hears that the connection is mid-reconnect within the probe's budget rather than
// waiting for the handshake with the lock held, and releases the lock before it waits for the
// connection (Reconnected), as it does for the session consumers' client; it makes the call with
// its context marked (WithLockHeld), so the request's own read answers within the budget too. Like
// connForRequest it bounds the question, not the call that follows an "up" answer. A client with no
// connection yet, one still awaiting its first, and a stopped one answer nil: their own calls answer
// as they always did.
func (c *Client) LostNow() error {
	if c == nil || c.stopped() {
		return nil
	}
	c.mu.Lock()
	conn, awaiting := c.Conn, c.awaitingConnection
	c.mu.Unlock()
	if conn == nil || awaiting {
		return nil
	}
	if up, _ := c.upNow(conn); !up {
		return &connectionLost{client: c, unsent: true}
	}
	return nil
}

// lockHeldKey is the key WithLockHeld marks a context with.
type lockHeldKey struct{}

// WithLockHeld returns ctx marked as the context of calls its caller makes while it holds a lock
// of its own across them: a session's consumer lock, a role's keep guard. A request made under it
// whose read of its connection's state the probe's budget ends (ConnLockWait) answers the loss
// at once, unsent, so the caller lets its lock go and retries, where a request whose caller holds
// nothing waits for nats.go's own answer within its context (requestRead). Every request on the
// session consumers' client answers so, marked or not, since every caller of that client holds
// such a lock (withLossAtOnce).
func WithLockHeld(ctx context.Context) context.Context {
	return context.WithValue(ctx, lockHeldKey{}, true)
}

// lockHeld reports whether ctx is marked by WithLockHeld.
func lockHeld(ctx context.Context) bool {
	held, _ := ctx.Value(lockHeldKey{}).(bool)
	return held
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
// connection is connected, and ends at the first report of its loss nats.go gives to a status
// listener of the life's own, the status change it makes inside processOpErr (nats.go:3661), or
// at the first connection event after which nats.go no longer reads the connection connected in
// the same run (requestLife). That report is taken rather than the disconnect callback the client
// already logs and follows (onDisconnect, wired by connect.go's options as the DisconnectedErrCB),
// because nats.go runs that callback on its one callback goroutine, after the connection's reader
// has exited and behind every callback queued before it, the reconnect hooks of the reconnect
// before among them: a request a reconnect hook made would wait for its own hook to return before
// the callback could end it.
func Request[T any](ctx context.Context, c *Client, send func(context.Context) (T, error)) (value T, err error) {
	bound, finish := c.bindRequest(ctx)
	if bound.Err() != nil && ctx.Err() == nil {
		err := finish(context.Cause(bound))
		var lost *connectionLost
		if errors.As(err, &lost) {
			lost.unsent = true
		}
		return value, err
	}
	// finish is deferred, so a send that panics still stops the request's wait on its life.
	defer func() { err = finish(err) }()
	return send(bound)
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
// ctx itself had not ended, is lostOrClosed's, and so is nats.go's answer for that connection's
// close or forced reconnect given before the life ended (closedUnder, against the reconnect count
// the life began at). A nil c binds nothing.
func (c *Client) bindRequest(ctx context.Context) (context.Context, func(error) error) {
	if c == nil {
		return ctx, func(err error) error { return err }
	}
	life, conn, reconnects := c.requestLife(ctx)
	bound, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(life, func() { cancel(ErrConnectionLost) })
	if life.Err() != nil {
		cancel(ErrConnectionLost)
	}
	return bound, func(err error) error {
		stop()
		cancel(nil)
		if err == nil || ctx.Err() != nil || life.Err() == nil && !c.closedUnder(ctx, err, conn, reconnects) {
			return err
		}
		return c.lostOrClosed(err)
	}
}

// closedUnder reports whether err, a request's answer, is nats.go's answer for a close of conn
// (closeAnswer) given while conn is closed or going away, or once conn has reconnected since the
// request's life began at reconnect count reconnects. nats.go answers a request a close cut off
// before the status report that ends the life (close clears the pending requests, nats.go v1.55.0
// :6265, before changeConnStatus, :6321, and sets the status CLOSED first, :6249), and a forced
// reconnect (ForceReconnect, which the keeper of a service's token makes to present a new one,
// natscred) answers every pending request nats.ErrDisconnected (clearPendingRequestCalls, :2667;
// newRequest, :4962-4966) and reconnects in place, counting the reconnect before it reports the
// connection connected (doReconnect, :3578), so by the time the request reads its answer the
// connection can be connected again: each answer is as lost as one after the life ended, and a
// running client recovers its connection for the caller's retry.
// The same answer while conn is still connected and has not reconnected since is a handle's of an
// earlier connection the client has since replaced (a request's send may use such a handle, where a
// direct call is made on conn itself, lostIfClosed), which no retry on the client's connection
// answers, so it stays nats.go's: a caller reads it as lasting, rather than retrying it for ever,
// and a reconnect under such a handle's call makes its error lost once, for one retry.
//
// Whether conn is connected and its reconnect count are read outside c.mu, through the probe
// (requestRead): a caller that holds a lock across the request reads within ConnLockWait, and
// counts an answer the budget ends ("unknown") lost, which costs it one retry; a caller that holds
// none waits for nats.go's own answer, within ctx.
func (c *Client) closedUnder(ctx context.Context, err error, conn *nats.Conn, reconnects uint64) bool {
	if conn == nil || !closeAnswer(err) {
		return false
	}
	status, now, answered := c.requestRead(ctx, conn)
	return !answered || status != nats.CONNECTED || now != reconnects
}

// requestRead is the read of conn's state for a request's own bookkeeping (closedUnder,
// requestLife), made under ctx, the request's context. It reads through the probe, which waits at
// most ConnLockWait for nats.go's connection lock, and answers "unknown" (answered false) when
// the budget ends first: a reconnect's dial or handshake holds that lock, or the box is too loaded
// to run the read in time. That is the answer for a request whose caller holds a lock across it:
// every request on the session consumers' client (withLossAtOnce), and one made under a context
// WithLockHeld marks, so the caller lets its lock go rather than wait for a handshake with it held.
// A request whose caller holds nothing waits on for nats.go's own answer, as it would for
// IsConnected, until ctx ends, and at most RequestTimeout when ctx has no deadline
// (requestReadUntil): a slow read on a loaded box is not a lost connection, and answering it as one
// would fail a publish that its caller sends once.
//
// The test's seam (ProbeReadsDownForTest) applies to the budgeted read on a client whose callers
// all hold a lock (withLossAtOnce), as it does to that client's other reads, and not on any other,
// where it is the callers' own reads (LostNow) that a row counts; requestReadSlow is this read's own
// seam on every client.
func (c *Client) requestRead(ctx context.Context, conn *nats.Conn) (status nats.Status, reconnects uint64, answered bool) {
	if slow := c.requestReadSlow.Load(); slow == nil || !(*slow)() {
		status, reconnects, answered = c.readConnState(conn, c.lossAtOnce)
	}
	if answered || c.lossAtOnce || lockHeld(ctx) {
		return status, reconnects, answered
	}
	return c.requestReadUntil(ctx, conn)
}

// requestReadUntil is requestRead's wait for nats.go's own answer, for a request whose caller holds
// no lock: the probe's read, bounded by ctx, and by RequestTimeout, the bound nats.go's own requests
// get here, when ctx has none. answered is false only once that bound has ended. A connection the
// client no longer holds is read from nats.go at once, as readConnState reads it.
func (c *Client) requestReadUntil(ctx context.Context, conn *nats.Conn) (status nats.Status, reconnects uint64, answered bool) {
	c.mu.Lock()
	var prober *connProber
	if conn == c.Conn && conn != nil {
		prober = c.proberLocked()
	}
	c.mu.Unlock()
	if prober == nil {
		return conn.Status(), conn.Stats().Reconnects, true
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
	}
	if tick := c.answerWaitTick.Load(); tick != nil {
		(*tick)()
	}
	return prober.probeUntil(ctx)
}

// lostIfClosed is err, nats.go's answer for a call the bus made directly on conn rather than
// through Request, as a call the loss of a running client's connection cut off (connectionLost,
// unsent when nats.go refused the call before writing anything) when it is nats.go's answer for a
// close (closeAnswer), and err as it is otherwise. conn is the client's connection the call was
// made on: the one the client held when the call began, or for a call of an object bound to one
// connection (a pulled message's acknowledgement, a pull's request, a KV handle's watch), that
// object's connection while it is still the client's (heldNow), and nil once the client has
// replaced it. A close answer for a call made on conn comes from conn's close or drain, or from a
// drop or a forced reconnect of it (ForceReconnect, which the keeper of a service's token makes to
// present a new one, natscred), each of which answers every pending flush nats.ErrConnectionClosed
// (processOpErr's clearPendingFlushCalls, nats.go v1.55.0 :3673; FlushTimeout, :6129-6131), and a
// forced reconnect every pending request nats.ErrDisconnected (ForceReconnect's
// clearPendingRequestCalls, :2667; newRequest, :4962-4966): each is a loss of the connection,
// however late the answer is read, so no read of conn's state decides it, and none waits for a
// reconnect's handshake. A nil conn answers err as it is, so a call on the handle of a connection
// the client has replaced keeps nats.go's error, which no retry on the client's connection answers
// and which its caller reads as lasting. So does a stopped client's call, since that client
// recovers nothing, and a nil c's, a connection no client tracks.
func (c *Client) lostIfClosed(err error, conn *nats.Conn, unsent bool) error {
	if c == nil || conn == nil || !closeAnswer(err) || c.stopped() {
		return err
	}
	return &connectionLost{client: c, unsent: unsent, cause: err}
}

// closeAnswer reports whether err is nats.go's answer for a close of its connection: the
// connection closed, draining, its drain timed out, no server left to reconnect to, or a forced
// reconnect that cut the call off, which nats.go v1.55.0 answers nats.ErrDisconnected where v1.53.1
// answered nats.ErrConnectionClosed (newRequest, nats.go:4962-4966).
func closeAnswer(err error) bool {
	return errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrConnectionDraining) ||
		errors.Is(err, nats.ErrDrainTimeout) || errors.Is(err, nats.ErrNoServers) ||
		errors.Is(err, nats.ErrDisconnected)
}

// heldNow is conn while it is c's connection, and nil once c has replaced it (lostIfClosed): the
// connection an object bound to conn makes its calls on counts as the client's only until then. A
// call nats.go refuses for a closed connection at its start, before it writes anything - a publish,
// an acknowledgement and a pull request among them (nats.go v1.55.0 nats.go:4738-4745), and a
// subscription's drain (:5602-5603) - reads it once nats.go has answered, so a call that succeeds
// takes no lock for it: it reads what it would have read as the call began unless a dial of a new
// connection connected in between, which is when the client replaces a closed one
// (ensureConnWithContext). A KV watch's create, whose close can also come at the end of a request
// it waits on, reads it before the create (KeyValue.WatchKeys).
func (c *Client) heldNow(conn *nats.Conn) *nats.Conn {
	if c == nil || c.Connection() != conn {
		return nil
	}
	return conn
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

// requestLife is the life of the client's connection that a request made now under ctx is bound
// to, that connection, and nats.go's reconnect count when the life began: the life running while
// its connection is still the client's and connected, or else a new one when the connection is
// connected; a connection that is not gets endedLife.
//
// Whether the connection is connected is read at every request, outside c.mu (requestRead): within
// ConnLockWait for a request whose caller holds a lock across it, which reads a lock held longer
// as "unknown" and is not sent, and for as long as ctx lasts for one whose caller holds none. So a
// request made once nats.go has closed the connection or reported its drop is not sent, nor one
// made under a lock while a reconnect's handshake holds nats.go's lock. Such a read also ends the
// life that was running on the connection when it began, when nothing else would end that life at
// the drop: a read that answered the connection down (its requests are lost), or a life whose
// goroutine has not yet received its status listener (lifeListening), as a goroutine not yet
// scheduled on a loaded box has not. An "unknown" read ends no life that hears nats.go's own
// reports: on a loaded box it may come on a connection that is up, and ending the life there would
// answer every request bound to it, a relay's pull among them, with the loss. A life begun after
// the read began, by a request made once a reconnect in place was done, is another life and is not
// ended.
//
// A life ends at the first drop nats.go reports to a status listener of the life's own, registered
// fresh when the life begins. That listener alone is not enough: nats.go v1.55.0 drops a report
// when the listener's buffer of ten is full (notifyStatusListener, nats.go:6888-6899; StatusChanged,
// :6832), and a drop and reconnect can both come before the goroutine has registered at all. So
// the life's goroutine also wakes at each of the client's connection events
// (ConnectionChanged, taken before the listener), and ends the life whenever the probe no longer
// reads the connection connected, or reads a reconnect count other than the one the life began at.
// The listener is registered on a goroutine of its own: StatusChanged takes nats.go's connection
// lock, which a reconnect's attempt holds through its dial and handshake, and a life whose
// registration waited there would outlive a drop until that attempt ended, with every request
// bound to it waiting too; meanwhile the life's goroutine probes, and ends the life at the drop.
// The disconnect and close callbacks end at once the life of a connection the client has since
// replaced (endLifeIfDown).
//
// A request made in the moment between nats.go's drop and the listener's ending the life is handed
// to nats.go. After a close, nats.go refuses it as closed before writing it, and its answer is
// ErrConnectionLost (closedUnder), but not marked unsent, so a stored publish's charge counts it as
// one that may have landed: a cost bounded by that moment, on a running client's connection, which
// nats.go closes only at the client's own Close, a drain, or the hub refusing its credential twice.
// After a drop it is refused, or abandoned at the life's end (RequestBounded).
func (c *Client) requestLife(ctx context.Context) (context.Context, *nats.Conn, uint64) {
	conn := c.Connection()
	if conn == nil {
		return endedLife, nil, 0
	}
	c.mu.Lock()
	prior, _, _ := c.runningLifeLocked(conn)
	c.mu.Unlock()
	status, reconnects, answered := c.requestRead(ctx, conn)
	if !answered || !statusUp(status) {
		// A read the request's own context ended says nothing of the connection, and ends no life.
		c.mu.Lock()
		var end context.CancelCauseFunc
		if prior != nil && c.life == prior && (answered || !c.lifeListening && ctx.Err() == nil) {
			end = c.takeLifeLocked()
		}
		c.mu.Unlock()
		c.endLifeNow(end)
		return endedLife, conn, 0
	}
	c.mu.Lock()
	if life, running, ok := c.runningLifeLocked(conn); ok {
		c.mu.Unlock()
		return life, conn, running
	}
	if c.Conn != conn {
		c.mu.Unlock()
		return endedLife, conn, 0
	}
	replaced := c.takeLifeLocked()
	life, end := context.WithCancelCause(context.Background())
	c.life, c.endLife, c.lifeConn, c.lifeReconnects, c.lifeListening = life, end, conn, reconnects, false
	var changed <-chan struct{} = c.connChangedLocked()
	c.mu.Unlock()
	c.endLifeNow(replaced)
	go func() {
		if gate := c.lifeListenGate; gate != nil {
			gate("register", life)
		}
		registered := make(chan chan nats.Status, 1)
		register := func() {
			registered <- conn.StatusChanged(nats.DISCONNECTED, nats.RECONNECTING, nats.CLOSED)
		}
		go register()
		var lost chan nats.Status
		defer func() {
			if lost != nil {
				conn.RemoveStatusListener(lost)
				return
			}
			go func() { conn.RemoveStatusListener(<-registered) }()
		}()
		gated := false
		// woke is whether a connection event has come since the life's last read; for its first
		// read, since the life began.
		woke := false
		for {
			if !woke {
				select {
				case <-changed:
					woke, changed = true, c.ConnectionChanged()
				default:
				}
			}
			// The probe, not IsConnected: a wake at a connection event can come while nats.go holds
			// its lock for the next reconnect's handshake, and the life's end, which every request
			// bound to it waits for, must not wait for that. A read nats.go's lock kept waiting past
			// ConnLockWait ("unknown") after a connection event ends the life, as the drop it found
			// does; one with no event since the last read is a loaded box's slow read on a connection
			// that was up, and ends nothing, so a new life is not ended by the box's load.
			status, now, answered := c.lifeRead(conn)
			if answered && (!statusUp(status) || now != reconnects) || !answered && woke {
				c.endLifeNow(end)
				return
			}
			woke = false
			if gate := c.lifeListenGate; gate != nil && !gated && lost != nil {
				gated = true
				gate("select", life)
			}
			select {
			case lost = <-registered:
				c.mu.Lock()
				if c.life == life {
					c.lifeListening = true
				}
				c.mu.Unlock()
			case <-lost:
				c.endLifeNow(end)
				return
			case <-changed:
				woke, changed = true, c.ConnectionChanged()
			case <-life.Done():
				return
			}
		}
	}()
	return life, conn, reconnects
}

// runningLifeLocked is the life running on conn, and the reconnect count it began at, when there
// is one.
func (c *Client) runningLifeLocked(conn *nats.Conn) (context.Context, uint64, bool) {
	if c.life == nil || c.life.Err() != nil || c.lifeConn != conn {
		return nil, 0, false
	}
	return c.life, c.lifeReconnects, true
}

// takeLifeLocked is the end of the life running, nil when none is, for its caller to make once it
// has let c.mu go (endLifeNow).
func (c *Client) takeLifeLocked() context.CancelCauseFunc {
	if c.life == nil || c.life.Err() != nil {
		return nil
	}
	return c.endLife
}

// endLifeIfDown is the disconnect and close callbacks' share of ending a life (requestLife): it
// ends at once the life of a connection the client has since replaced. A drop of the client's own
// connection ends its life through the life's own listener.
func (c *Client) endLifeIfDown() {
	c.mu.Lock()
	var end context.CancelCauseFunc
	if c.lifeConn != nil && c.lifeConn != c.Conn {
		end = c.takeLifeLocked()
	}
	c.mu.Unlock()
	c.endLifeNow(end)
}

// endLifeNow ends a connection's life through end, once lifeEndGate, a test's seam, lets it; a nil
// end, no life running, ends nothing. It never runs under c.mu, so a gate that holds an end holds
// no call that takes the client's lock.
func (c *Client) endLifeNow(end context.CancelCauseFunc) {
	if end == nil {
		return
	}
	if gate := c.lifeEndGate; gate != nil {
		gate()
	}
	end(ErrConnectionLost)
}

// lifeRead is a life's own goroutine's read of conn (requestLife): the probe's, within
// ConnLockWait, past the callers' seam (ProbeReadsDownForTest), or "unknown" while a row's own
// seam on it answers true (lifeReadSlow).
func (c *Client) lifeRead(conn *nats.Conn) (status nats.Status, reconnects uint64, answered bool) {
	if slow := c.lifeReadSlow.Load(); slow != nil && (*slow)() {
		return 0, 0, false
	}
	return c.readConnState(conn, false)
}
