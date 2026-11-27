package bus

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// ErrReconnecting is wrapped by the error of a send made while nats.go connects or reconnects the
// connection the send would go on. Nothing of such a send was sent, and nothing of it is sent
// later: the client keeps no reconnect buffer (connect.go, options).
//
// Two errors wrap it. A send that waited for the connection (ensureConnWithContext for a publish
// or a request, Bounded for a bucket call a caller bounds by its deadline) and whose ctx ended
// first is `bus: NATS is reconnecting: context deadline exceeded`, which wraps ctx's error too. So
// is a bucket call whose ctx ended before it started while nats.go reconnects the connection
// (Bounded, endedUnsent). A send that did not wait, or that the connection dropped under after its
// wait, is `bus: NATS is reconnecting` (Reconnecting).
var ErrReconnecting = errors.New("NATS is reconnecting")

// reconnectingSend is a send nats.go refused while it connects or reconnects the connection:
// nats.ErrReconnectBufExceeded, which it answers to every publish then, since the client keeps no
// reconnect buffer, nats.ErrConnectionReconnecting, or any error a JetStream request returns while
// it does (namedOn). Its text is `bus: NATS is reconnecting`,
// ErrReconnecting's under this package's prefix, so a caller that wraps it adds no `bus:` of its
// own. The text of nats.ErrReconnectBufExceeded is `nats: outbound buffer limit exceeded`, and a
// reader of an answer or a log line reads that as a client that sent too much, where the bus is
// only reconnecting. It unwraps to nats.go's error, so a caller that reads that error, such as
// kvwatch's lostConnection or cistore's kvErrorLasts, reads it as nats.go's own.
type reconnectingSend struct {
	refusal error
}

func (e reconnectingSend) Error() string   { return "bus: " + ErrReconnecting.Error() }
func (e reconnectingSend) Unwrap() []error { return []error{ErrReconnecting, e.refusal} }

// Reconnecting names err as the ErrReconnecting it is when it is nats.go's refusal of a send while
// it connects or reconnects the connection (reconnectingSend), and returns any other err as it is.
// This package calls it once for each kind of send it makes, at the one place that kind is sent:
//   - a JetStream request through a legacy JetStream context (namedRequests), which also names
//     any error a request returns while the connection reconnects, and which wraps every such
//     context the package builds and every one a caller builds through it (JetStreamOn), and
//     every one a caller hands it (namedJetStream): the client's own, which Client.JS hands its
//     callers, the stream ensure's, the subject read's, the leader read of a key
//     (KeyValue.GetFromLeader), the known-session store's lookup of its bucket
//     (internal/session), and the contexts EnsureKeyValue, OpenKeyValue and SetKeyValueMaxAge are
//     handed;
//   - a JetStream API request this package builds itself (Client.requestConfirmed), which also
//     names any error a request returns while the connection reconnects (namedOn): every request
//     SessionConsumers sends - a consumer's info, create, update and delete, the consumer list
//     and the stream read - and each stored-message read (Client.NextStoredMessage,
//     Client.LastStoredMessage); and a SessionConsumers call that waits for a stream read another
//     call started (streamRead.wait);
//   - a session consumer's pull request (SessionPull.pull) and each answer to a message it pulled
//     (sessionAck);
//   - a publish, JetStream or core, and a core request (Client.refused, window.unsent,
//     SessionConsumers.Publish);
//   - a call through a KV bucket's handles (kvAnswer, KeyValue.Status, KeyValue.ReadStreamState,
//     wrapKeyValue).
//
// A caller that sends through a nats.go handle none of these cover names its error with it too,
// such as kvwatch's delete of a watcher's consumer and cmd/listener's relay socket's publishes.
func Reconnecting(err error) error {
	if err == nil {
		return nil
	}
	var named reconnectingSend
	if errors.As(err, &named) {
		return err
	}
	if errors.Is(err, nats.ErrReconnectBufExceeded) || errors.Is(err, nats.ErrConnectionReconnecting) {
		return reconnectingSend{refusal: err}
	}
	return err
}

// namedRequests is a legacy JetStream context whose requests name their error (namedWith): each
// method below sends one or more JetStream API requests on the context's connection, and answers an
// error it returns while nats.go connects or reconnects that connection as the ErrReconnecting it
// is. It is the one place this package names the error of such a request, so no call through the
// context names it again. Every other method is the context's own: a publish names its own error
// (Client.refused, window.unsent), and ConsumerNames answers no error.
//
// conn is the connection the context sends on, whose state namedWith reads.
type namedRequests struct {
	nats.JetStreamContext
	conn *nats.Conn
}

// JetStreamOn is conn's legacy JetStream context, built with opts, with its requests named
// (namedRequests). Every legacy JetStream context this package builds is built here. A caller
// outside the package that sends a request through a context of its own builds the context here
// too, so the request's error names a reconnect as every other does: the known-session store's
// lookup of its bucket (internal/session openKnownBucket).
func JetStreamOn(conn *nats.Conn, opts ...nats.JSOpt) (nats.JetStreamContext, error) {
	js, err := conn.JetStream(opts...)
	if err != nil {
		return nil, err
	}
	return namedRequests{JetStreamContext: js, conn: conn}, nil
}

// namedJetStream is js, a context a caller handed this package, with its requests named
// (namedRequests); conn is the connection js sends on. A context that names them already is
// returned as it is: a namedRequests, or a context that reads the server's refusals over one
// (refusalReads), which is the shape of Client.JS's (Client.readsOwnRefusals over JetStreamOn's
// context). A refusalReads over a context that names nothing gets the names under its refusal
// reads, in the same order: so a refusal the server reports on the connection is returned as the
// server's answer (ErrPublishDenied, refusedBy) and is never named a reconnect, and a caller that
// then reads the refusals too (readsRefusals) finds them read already and adds nothing.
func namedJetStream(conn *nats.Conn, js nats.JetStreamContext) nats.JetStreamContext {
	switch wrapped := js.(type) {
	case namedRequests:
		return js
	case refusalReads:
		wrapped.JetStreamContext = namedJetStream(conn, wrapped.JetStreamContext)
		return wrapped
	}
	return namedRequests{JetStreamContext: js, conn: conn}
}

// namedWith is err, the answer of a request j made with opts, named by the state of j's connection
// (namedOn). The state is read within the request's own context (requestContext): the caller's
// bound on the request bounds the read too. A request made with no nats.Context, which only its
// context's MaxWait bounds, reads the state at once on the caller's goroutine.
func namedWith[O any](j namedRequests, err error, opts []O) error {
	if err == nil {
		return nil
	}
	return namedOn(requestContext(opts), j.conn, err)
}

// requestContext is the context nats.go makes a request with opts under: the one the last
// nats.Context among opts gives, since nats.go applies the options in order, and
// context.Background() when they give none. The callers that bound a legacy JetStream request by
// their own deadline give it that way: the takeover and kept-lane reads (TakeoverRecords,
// KeptRoleLanes), the leader read of a key (KeyValue.GetFromLeader), the stream ensure's requests
// (requestsUnder), and callers outside this package, such as the listener's /healthz read of its
// durable.
func requestContext[O any](opts []O) context.Context {
	for i := len(opts) - 1; i >= 0; i-- {
		if given, ok := any(opts[i]).(nats.ContextOpt); ok {
			if given.Context == nil {
				return context.Background()
			}
			return given.Context
		}
	}
	return context.Background()
}

// namedOn is err, the answer of a request made on conn, as the ErrReconnecting it is when the
// request was made while nats.go connects or reconnects conn: nats.go's refusal of its send
// (Reconnecting), or any error the request returns while conn reports that state. The state is
// read because nats.go answers some requests then with words of its own that say nothing true. It
// refuses a KV bucket's lookup and create before it sends anything: both first check the server's
// version, which a connection that is reconnecting does not report, and answer "nats: key-value
// requires at least server version 2.6.2". The create EnsureKeyValue makes in nats.go's place
// checks the version first too (createBucketStream), and its step names that refusal here
// (createBucket). And a request sent just before the connection dropped waits for a reply that
// never comes, and answers its caller's deadline. The error nats.go gave stays in the chain
// (reconnectingSend), so a caller that reads it still finds it. An error that is ErrReconnecting
// already is returned as it is, such as a wait for the connection that ended first
// (reconnectWaitEnded).
//
// A nil conn names nats.go's refusal alone and reads no state: connecting would panic on it. The
// one caller that can pass one is streamRead.wait, which passes its client's connection
// (Client.Connection). A client the constructors return holds a connection from the constructor
// on (install), and a replacement of a closed one installs another in its place
// (ensureConnWithContext), so only a Client built as a struct literal, as a test builds one,
// holds none. Every legacy context's conn is the connection it was built on or handed with.
//
// The read of conn's state waits for nats.go's connection lock, so it is made within ctx
// (readWithin). A read that has not answered by then was made while nats.go holds the lock through
// a reconnect's handshake, and err is named the ErrReconnecting it is, as for a read that answers
// that nats.go reconnects conn. Each caller passes the context its request was made under:
// requestConfirmed its own, streamRead.wait the one whose end it names, a legacy JetStream
// context's request the one its nats.Context option gives (namedWith), and createBucket its step's.
// A request made with none passes context.Background(), and the read is then made at once on the
// caller's goroutine.
//
// It is the one rule every place that names a request's error follows: a legacy JetStream context
// (namedRequests), a request this package builds itself (Client.requestConfirmed), a call that
// waits for such a request another call made (streamRead.wait), and the create of a bucket's
// stream, whose check of the server's version is no request of a context (createBucket).
func namedOn(ctx context.Context, conn *nats.Conn, err error) error {
	if err == nil || errors.Is(err, ErrReconnecting) || conn == nil {
		return Reconnecting(err)
	}
	if reconnecting, answered := readWithin(ctx, func() bool { return connecting(conn) }); answered && !reconnecting {
		return Reconnecting(err)
	}
	return reconnectingSend{refusal: err}
}

func (j namedRequests) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	info, err := j.JetStreamContext.AddStream(cfg, opts...)
	return info, namedWith(j, err, opts)
}

func (j namedRequests) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	info, err := j.JetStreamContext.UpdateStream(cfg, opts...)
	return info, namedWith(j, err, opts)
}

func (j namedRequests) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	info, err := j.JetStreamContext.StreamInfo(stream, opts...)
	return info, namedWith(j, err, opts)
}

// AccountInfo reads the account's JetStream info: the first request of the create of a bucket's
// stream (createBucketStream).
func (j namedRequests) AccountInfo(opts ...nats.JSOpt) (*nats.AccountInfo, error) {
	info, err := j.JetStreamContext.AccountInfo(opts...)
	return info, namedWith(j, err, opts)
}

func (j namedRequests) PurgeStream(stream string, opts ...nats.JSOpt) error {
	return namedWith(j, j.JetStreamContext.PurgeStream(stream, opts...), opts)
}

func (j namedRequests) GetLastMsg(stream, subject string, opts ...nats.JSOpt) (*nats.RawStreamMsg, error) {
	message, err := j.JetStreamContext.GetLastMsg(stream, subject, opts...)
	return message, namedWith(j, err, opts)
}

func (j namedRequests) AddConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	info, err := j.JetStreamContext.AddConsumer(stream, cfg, opts...)
	return info, namedWith(j, err, opts)
}

func (j namedRequests) UpdateConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	info, err := j.JetStreamContext.UpdateConsumer(stream, cfg, opts...)
	return info, namedWith(j, err, opts)
}

func (j namedRequests) ConsumerInfo(stream, consumer string, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	info, err := j.JetStreamContext.ConsumerInfo(stream, consumer, opts...)
	return info, namedWith(j, err, opts)
}

func (j namedRequests) DeleteConsumer(stream, consumer string, opts ...nats.JSOpt) error {
	return namedWith(j, j.JetStreamContext.DeleteConsumer(stream, consumer, opts...), opts)
}

// KeyValue looks the bucket up: a STREAM.INFO of its stream, after a check of the server's version.
// The call takes no options, so it is made with no nats.Context.
func (j namedRequests) KeyValue(bucket string) (nats.KeyValue, error) {
	kv, err := j.JetStreamContext.KeyValue(bucket)
	return kv, namedWith[nats.JSOpt](j, err, nil)
}

// CreateKeyValue creates the bucket: after a check of the server's version, the account's
// JetStream info, then its stream's create. The call takes no options, so it is made with no
// nats.Context.
func (j namedRequests) CreateKeyValue(cfg *nats.KeyValueConfig) (nats.KeyValue, error) {
	kv, err := j.JetStreamContext.CreateKeyValue(cfg)
	return kv, namedWith[nats.JSOpt](j, err, nil)
}

// Subscribe binds or creates the subscription's consumer: a CONSUMER.INFO, and a CONSUMER.CREATE
// when it creates one.
func (j namedRequests) Subscribe(subject string, handler nats.MsgHandler, opts ...nats.SubOpt) (*nats.Subscription, error) {
	sub, err := j.JetStreamContext.Subscribe(subject, handler, opts...)
	return sub, namedWith(j, err, opts)
}

// reconnectWaitEnded is the error of a wait for a connection nats.go is reconnecting whose ctx
// ended first. It wraps ErrReconnecting and ctx's error.
func reconnectWaitEnded(ctx context.Context) error {
	return fmt.Errorf("bus: %w: %w", ErrReconnecting, ctx.Err())
}

// endedUnsent is the error of a send on conn that was not sent because ctx had ended before it
// started: while nats.go connects or reconnects conn, the error of a wait for the connection that
// ctx ended (reconnectWaitEnded), since the send could not have gone then either and its caller
// is told why; and ctx's own error otherwise. A nil conn is no connection to wait for, and gives
// ctx's own error.
//
// The read of conn's state waits for nats.go's connection lock, so it is made through readWithin.
// ctx has ended, so the read gets ConnLockWait. When the read has not answered by then, nats.go
// holds the lock through a reconnect's handshake, and endedUnsent gives reconnectWaitEnded.
func endedUnsent(ctx context.Context, conn *nats.Conn) error {
	if conn == nil {
		return ctx.Err()
	}
	if reconnecting, answered := readWithin(ctx, func() bool { return connecting(conn) }); !answered || reconnecting {
		return reconnectWaitEnded(ctx)
	}
	return ctx.Err()
}

// awaitConnected returns once conn can take a send. A nil conn, a connected one, a draining one
// and a closed one return at once: a send on a closed connection fails on its own, and nothing
// waits it out. A connection nats.go is connecting or reconnecting is waited for, until it
// connects or closes, or until ctx ends, which returns reconnectWaitEnded. The wait reads the
// connection again at each of its status events (nats.Conn.StatusChanged) and at least every
// reconnectRecheck, as ensureConnWithContext's wait does, rather than polling.
//
// Each read of conn's state, and each registration and removal of the status listener, waits for
// nats.go's connection lock. nats.go holds that lock through a reconnect's dial and handshake, up
// to the attempt's connect timeout (connectTimeout, 5 s). So the wait runs on a goroutine of its
// own, and awaitConnected waits for it only within ctx (readWithin). When ctx ends while the wait
// is still in one of those calls, nats.go is reconnecting conn, and awaitConnected returns
// reconnectWaitEnded. When ctx ends less than ConnLockWait after the start, awaitConnected waits
// until ConnLockWait has passed: a connection whose lock is free answers in that time.
//
// It is ensureConnWithContext's wait for one connection alone. It is for a send on a handle that
// holds only its connection, a KV bucket's (Bounded): such a handle cannot dial a replacement,
// and the client that owns the connection does that.
func awaitConnected(ctx context.Context, conn *nats.Conn) error {
	if conn == nil {
		return nil
	}
	err, answered := readWithin(ctx, func() error { return waitWhileConnecting(ctx, conn) })
	if !answered {
		return reconnectWaitEnded(ctx)
	}
	return err
}

// waitWhileConnecting is awaitConnected's wait, on the goroutine readWithin starts. It removes each
// status listener it registers before it returns. When awaitConnected has returned without its
// answer, it goes on until nats.go lets its connection lock go, then finds ctx ended and returns.
func waitWhileConnecting(ctx context.Context, conn *nats.Conn) error {
	for connecting(conn) {
		if ctx.Err() != nil {
			return reconnectWaitEnded(ctx)
		}
		// The listener is registered before the status is read again, so a connect that lands in
		// between is either read here or sent to the listener.
		changed := conn.StatusChanged(nats.CONNECTED, nats.CLOSED)
		if !connecting(conn) {
			conn.RemoveStatusListener(changed)
			return nil
		}
		recheck := time.NewTimer(reconnectRecheck)
		select {
		case <-ctx.Done():
			recheck.Stop()
			conn.RemoveStatusListener(changed)
			return reconnectWaitEnded(ctx)
		case <-changed:
		case <-recheck.C:
		}
		recheck.Stop()
		conn.RemoveStatusListener(changed)
	}
	return nil
}

// readWithin runs read, a call that waits for nats.go's connection lock, on a goroutine of its own,
// and waits for its answer until ctx ends. answered is false when read has not answered by then.
// nats.go holds that lock for microseconds to read or write a frame, and through a whole reconnect
// attempt, its dial and its handshake, up to the attempt's connect timeout (connectTimeout, 5 s).
// So each caller takes a read that has not answered when ctx ends for one made during a
// reconnect's handshake, and answers as for a reconnect. A write to a slow socket also holds the
// lock, for as long as the write takes, and a read that such a write keeps waiting past ctx is
// answered the same way: nothing tells the two apart without the lock.
//
// It is the one rule for every read of a connection's state that a caller's ctx bounds: Bounded's
// and BoundedOn's (awaitConnected, endedUnsent), the client's wait for a connection
// (ensureConnWithContext), a core publish's reads before and after its send (publishConfirmed,
// confirmPublished), a window request's read before its send, which its own deadline bounds
// (window.observeToSend), a request's read of the connection it watches for a loss
// (watchConnectionWithin: requestConfirmed, RequestCore, RequestOnConnection), and the read that
// names a request's error (namedOn).
//
// The wait lasts at least ConnLockWait from the start (readFloor), whatever ctx does. A read with
// the lock free answers in that time, so a ctx that has ended already, or that ends a moment after
// the start, still gets the read's answer. So a caller can answer up to ConnLockWait (100 ms) after
// its ctx ended, for each such read it makes once ctx has ended. A ctx that can never end
// (context.Background) bounds nothing: read is then made on the caller's goroutine, as it would be
// without readWithin.
//
// A read that has not answered in time is left running, since nothing stops a wait for nats.go's
// lock: it goes on until nats.go lets the lock go. Its answer goes to a channel with room for one,
// which nothing reads then. So the goroutine never blocks on its send, and it writes nothing that
// its caller reads after readWithin has returned.
//
// Some calls that take nats.go's lock stay on the caller's goroutine, with no bound but nats.go's.
// nats.go takes the lock before it starts any timer of the call's or waits on its context, so a
// timeout or a nats.Context passed to such a call bounds only what follows the lock:
//   - the sends themselves: a core publish and its flush (publishConfirmed, confirmFlushed), a
//     window's request publish (window.request) and its probe's flush (window.Probe), a JetStream
//     publish (sendStored), and nats.go's own steps of a legacy JetStream request; a send left
//     running could still go out after its caller was told it did not;
//   - the subscribe of a request's reply inbox (requestConfirmedOn, RequestCore, window.request,
//     StartRequestStoredTo), the status listener a request of the client's own registers before
//     it on a connection nothing follows yet (lifeOf), and the unsubscribe of one a reply answered
//     (endReplies);
//   - a read of the state for a call that takes no ctx: the name of a legacy JetStream request made
//     with no nats.Context (namedWith), and SubOK, CoreSubOK and a recovery's
//     subscriptionsHealthy, which hold no lock of the client's across it (connOpen). Connected
//     reads no state of nats.go's at all (Client.ConnectedConn).
//
// Each send and subscribe is made right after a read found the connection connected, so it waits
// for a handshake only when the connection drops and nats.go begins an attempt within those few
// microseconds; a write to a slow socket holds the lock for as long as that write takes.
func readWithin[T any](ctx context.Context, read func() T) (value T, answered bool) {
	if ctx.Done() == nil {
		return read(), true
	}
	began := time.Now()
	answer := make(chan T, 1)
	go func() { answer <- read() }()
	select {
	case value = <-answer:
		return value, true
	case <-ctx.Done():
	}
	if rest := time.Duration(readFloor.Load()) - time.Since(began); rest > 0 {
		floor := time.NewTimer(rest)
		defer floor.Stop()
		select {
		case value = <-answer:
			return value, true
		case <-floor.C:
		}
	}
	// select picks at random among ready cases, so an answer that came with the end is read here.
	select {
	case value = <-answer:
		return value, true
	default:
		return value, false
	}
}

// readFloor is how long readWithin waits for a read at the least, whatever its ctx does. It is
// ConnLockWait outside tests. A row that must have a read answer after its ctx ended, once a held
// handshake ends, raises it, since under the race detector a handshake can take longer than
// ConnLockWait to end once it is released.
var readFloor atomic.Int64

func init() { readFloor.Store(int64(ConnLockWait)) }

// endReplies unsubscribes sub, the reply inbox of a request whose wait has ended. The unsubscribe
// waits for nats.go's connection lock. replied says a reply answered the request over a live link
// a moment ago: the request's wait took the reply, and the connection is still in the life the
// inbox was subscribed in (connLife). A window's request reads the window's life
// (window.current); a request of the client's own reads the life it took before its subscribe
// (requestConfirmedOn, RequestCore, PendingForward.Wait). Such a request unsubscribes at once on
// the caller's goroutine. Any other request - one whose wait ended with no reply, at its caller's
// bound, at a refusal or at the connection's loss, or took its reply once the connection had
// changed - may end while nats.go holds that lock through a reconnect's handshake, so it
// unsubscribes on a goroutine of its own and its caller answers at once. That goroutine ends when
// nats.go lets the lock go; until then the inbox can take a late reply, which nothing reads.
func endReplies(sub *nats.Subscription, replied bool) {
	if replied {
		_ = sub.Unsubscribe()
		return
	}
	go func() { _ = sub.Unsubscribe() }()
}

// connecting reports whether nats.go is connecting conn for the first time or reconnecting it: the
// states in which it refuses every publish (nats.ErrReconnectBufExceeded). It reads conn's status,
// which waits for nats.go's connection lock (readWithin says when it is held).
func connecting(conn *nats.Conn) bool {
	switch conn.Status() {
	case nats.CONNECTED, nats.DRAINING_SUBS, nats.DRAINING_PUBS, nats.CLOSED:
		return false
	default:
		return true
	}
}
