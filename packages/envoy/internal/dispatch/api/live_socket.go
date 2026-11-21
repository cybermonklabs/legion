package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// GET /ws/live is a page's one live socket. The page sends subscribe and unsubscribe frames
// (LiveSubscribe, LiveUnsubscribe), each naming a subscription by a sub id of the page's own, and
// the socket starts each by its view's kind (live_subscriptions.go): a delivery view's goes to the
// delivery hub (LEGION-690), and an events or agent view's runs on a goroutine of its own. The
// socket writes every frame they send; it keeps no view state of its own.
//
// It holds two queues. queue takes the hub's frames and the socket's own refusals, which never wait
// (Send); stream takes the frames an events or agent subscription waits for room to queue
// (SendWait). The writer drains queue before stream, so a busy subscription can neither hold back
// the hub's frames nor fill the hub's queue and close the socket.
//
// It closes with:
//   - 4001 (max age, contracts.LiveSocketMaxAgeClose) once it reaches its age, drawn as it opens
//     (liveSocketAge), or when the rotate test hook asks, so no socket holds one of the browser's
//     connections indefinitely: the page reopens at once and resumes every subscription. It closes
//     with 4001 too once the credential its upgrade carried expires, an Identity-Aware Proxy
//     assertion (whileCallerResolves, errCredentialExpired), and the reopened socket's fresh one is
//     judged again;
//   - 1012 (service restart) when Dispatch shuts down (server.lifetime) or the disconnect test hook
//     asks, and as it opens once the shutdown waits for the open sockets (closeUnserved);
//   - 1008 (policy violation) once its caller no longer resolves (whileCallerResolves);
//   - 1013 (try again later) when the page falls liveSocketQueue of the hub's frames behind;
//   - 1009 (message too big) for a frame over liveSocketReadLimit, which gorilla writes;
//   - 1011 (internal error) for a frame Dispatch cannot encode.
//
// A page that answers no ping for two heartbeats, or whose connection fails, has gone or stopped
// reading: its socket ends with no close frame, which the page reads as 1006.
//
// The page reconnects after each and resumes every subscription from what it applied, so what a
// close cut off reaches it as part of what changed since.

const (
	// liveSocketQueue is how many of the hub's frames and the socket's refusals a socket holds for
	// a page that is slow to read them.
	liveSocketQueue = 16
	// liveSocketStream is how many frames the events and agent subscriptions can queue ahead of the
	// writer before SendWait waits for room.
	liveSocketStream = 64
	// liveSocketReadLimit bounds one frame from the page. A resume frame carries the page's
	// revisions, one 16-digit revision per pull request, run, map entry and count it holds.
	liveSocketReadLimit = 8 << 20
	// liveSocketWriteWait bounds every write to the page.
	liveSocketWriteWait = 10 * time.Second
	// liveSocketCloseWait is how long a socket that sent its close frame waits for the page's.
	liveSocketCloseWait = time.Second
	// liveSocketReadBuffer is the socket's own read buffer, gorilla's default size. Setting one at
	// all is what matters: gorilla must read the raw connection through a reader of its own, which a
	// nonzero ReadBufferSize guarantees (with zero, "buffers allocated by the HTTP server are used").
	// A read through net/http's reader cancels the request's context on a read error, and the writer
	// reads that context's end as the caller no longer resolving (1008).
	liveSocketReadBuffer = 4096
	// liveSocketMaxAgeReason is the reason a close for the socket's age carries.
	liveSocketMaxAgeReason = "max age"
	// liveSocketCredentialExpiredReason is the reason a 4001 close carries when the credential the
	// socket's upgrade carried has expired rather than the socket's own age (errCredentialExpired).
	liveSocketCredentialExpiredReason = "credential expired"
)

// liveSocket is one open GET /ws/live socket, the deliveryLiveConn the hub sends to.
type liveSocket struct {
	conn *gws.Conn
	// ctx is the socket's own, derived from its request's: it ends as the writer ends (ended), so
	// with the socket however it ends, and before that with the request once its caller no longer
	// resolves. Each events or agent subscription runs on a context derived from it.
	ctx    context.Context
	cancel context.CancelFunc
	// actor is the caller as the upgrade resolved it (optionalActor), its credential kept, which
	// is where every socket's authorization is decided: a frame carries no credential of its own,
	// so a view the socket serves reads what the caller proved when it opened (holds), and resolves
	// from it what the caller sees now (caller, readerFor): the delivery hub at each subscribe and
	// at each event that can move what the caller sees, an events subscription as it starts and at
	// each such event (feedEvents), and an agent subscription as it starts and at each re-arm
	// (relayAgentConversation).
	actor model.Actor
	// queue holds the hub's frames and the socket's own refusals (Send).
	queue chan any
	// stream holds the frames of the events and agent subscriptions (SendWait).
	stream chan liveStreamFrame
	// full closes the first time Send finds the queue full.
	full     chan struct{}
	fullOnce sync.Once
	// shut closes when a test hook asks the writer to close the socket, with shutCode and
	// shutReason; the first ask wins.
	shut       chan struct{}
	shutOnce   sync.Once
	shutCode   int
	shutReason string
	// closed closes once the writer has ended, so a subscription waiting for room in stream stops
	// waiting.
	closed chan struct{}
	// deadline orders the pong handler's extension of the read deadline with the last one the
	// writer sets once it has closed; closing stops further extensions.
	deadline sync.Mutex
	closing  bool
	// subs is every subscription the socket holds, by sub. A delivery view's stays here after the
	// hub drops it on its own until the socket next counts at the cap (subscribeLive). Only the
	// handler's goroutine touches it: the read loop, then the cleanup once the loop has ended.
	subs map[string]*liveSub
	// ending counts the subscriptions stopping beside the read loop, which the socket waits for
	// once it ends.
	ending sync.WaitGroup
	// beforePick runs in the writer on every turn, between its look at the hub's queue and its
	// pick of the next frame, told whether that pick may take a stream frame. Nil outside tests; a
	// test holds the writer there to put a frame on each queue while it waits
	// (server.liveWriterBeforePick).
	beforePick func(socket *liveSocket, takesStream bool)
}

func newLiveSocket(ctx context.Context, conn *gws.Conn, actor model.Actor) *liveSocket {
	ctx, cancel := context.WithCancel(ctx)
	return &liveSocket{
		conn:   conn,
		ctx:    ctx,
		cancel: cancel,
		actor:  actor,
		queue:  make(chan any, liveSocketQueue),
		stream: make(chan liveStreamFrame, liveSocketStream),
		full:   make(chan struct{}),
		shut:   make(chan struct{}),
		closed: make(chan struct{}),
		subs:   map[string]*liveSub{},
	}
}

// holds reports whether the caller's credential holds scope: the socket's own test of what the
// caller may watch, which is model.Credential.Holds over the credential the upgrade kept.
func (socket *liveSocket) holds(scope string) bool {
	return socket.actor.Credential.Holds(scope)
}

// caller is the caller as the upgrade resolved it, its credential kept: what a view resolves
// again (readerFor) to learn what the caller sees now.
func (socket *liveSocket) caller() model.Actor {
	return socket.actor
}

// liveSocketAge is how long a socket opening now lives: Deps.LiveSocketAge's, or
// randomLiveSocketAge's.
func (s *server) liveSocketAge() time.Duration {
	if s.deps.LiveSocketAge != nil {
		return s.deps.LiveSocketAge()
	}
	return randomLiveSocketAge()
}

// randomLiveSocketAge draws a socket's age uniformly from [contracts.LiveSocketMinAge,
// contracts.LiveSocketMaxAge), so the sockets a deploy opened together do not all close together.
func randomLiveSocketAge() time.Duration {
	return contracts.LiveSocketMinAge + rand.N(contracts.LiveSocketMaxAge-contracts.LiveSocketMinAge)
}

// Send queues frame for the page without blocking. A full queue answers false and closes the
// socket with 1013, since a page that far behind reads a delta of what changed since sooner by
// resuming than by reading every frame it missed.
func (socket *liveSocket) Send(frame any) bool {
	select {
	case socket.queue <- frame:
		return true
	default:
		socket.fullOnce.Do(func() { close(socket.full) })
		return false
	}
}

// overflowed reports whether a Send has found the queue full.
func (socket *liveSocket) overflowed() bool {
	select {
	case <-socket.full:
		return true
	default:
		return false
	}
}

// ended marks the socket ended once its writer has: a subscription waiting for room in stream
// stops waiting (errLiveSocketClosed), and then every subscription's context ends, before the
// socket waits for each to return (endLiveSubscriptions).
func (socket *liveSocket) ended() {
	close(socket.closed)
	socket.cancel()
}

// shutWith asks the writer to close the socket with code and reason.
func (socket *liveSocket) shutWith(code int, reason string) {
	socket.shutOnce.Do(func() {
		socket.shutCode, socket.shutReason = code, reason
		close(socket.shut)
	})
}

// awaitPong gives the page wait to answer the next ping, unless the socket is closing.
func (socket *liveSocket) awaitPong(wait time.Duration) {
	socket.deadline.Lock()
	defer socket.deadline.Unlock()
	if !socket.closing {
		_ = socket.conn.SetReadDeadline(time.Now().Add(wait))
	}
}

// endReads ends the page's reads within wait: the time its close frame has to arrive once the
// socket sent its own, or none when the connection failed.
func (socket *liveSocket) endReads(wait time.Duration) {
	socket.deadline.Lock()
	defer socket.deadline.Unlock()
	socket.closing = true
	_ = socket.conn.SetReadDeadline(time.Now().Add(wait))
}

// close sends the page a close frame with code and reason, and waits for its reply.
func (socket *liveSocket) close(code int, reason string) {
	err := socket.conn.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(code, reason), time.Now().Add(liveSocketWriteWait))
	if err != nil {
		socket.endReads(0)
		return
	}
	socket.endReads(liveSocketCloseWait)
}

// closeUnserved ends a socket that opened once Dispatch's shutdown was already waiting for the
// open ones (liveSocketSet.wait), which it does not serve: it closes with 1012, as every open
// socket does at shutdown, so the page reconnects to a server that is not stopping. It reads off
// whatever the page sent until the page's close arrives or liveSocketCloseWait passes, so the
// connection ends with a FIN behind the close frame rather than a reset that can overtake it.
func (socket *liveSocket) closeUnserved() {
	defer socket.cancel()
	defer socket.conn.Close()
	socket.close(gws.CloseServiceRestart, "")
	for {
		if _, _, err := socket.conn.NextReader(); err != nil {
			return
		}
	}
}

// write sends the page each queued frame, the hub's queue ahead of the stream, and every heartbeat
// a ping beside a heartbeat frame (a browser shows the page no ping), until the reader ends,
// something closes the socket, or the socket reaches its age. No stream frame goes out ahead of a
// hub frame queued before it: a sub the read loop replaces has every frame the hub sends it queued
// before the replacement starts (Unsubscribe takes the hub's lock, which the hub sends under), so
// the replacement's frames follow the replaced view's last answer.
func (socket *liveSocket) write(ctx context.Context, lifetime <-chan struct{}, heartbeat, age time.Duration, readerDone <-chan struct{}) {
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	aged := time.NewTimer(age)
	defer aged.Stop()
	// taken is a stream frame the pick took while a hub frame waited; its from is nil while it
	// holds none. It goes out once the hub's queue is empty.
	var taken liveStreamFrame
	for {
		// A full queue wins over the frames still in it: the page resumes from what it applied.
		if socket.overflowed() {
			slog.Info("dispatch: live socket closed: its page fell behind", "queued", liveSocketQueue)
			socket.close(gws.CloseTryAgainLater, "")
			return
		}
		if taken.from != nil && len(socket.queue) == 0 {
			queued := taken
			taken = liveStreamFrame{}
			if queued.from.stopped.Load() {
				continue // its subscription has stopped, and what replaced it may already have written
			}
			if !socket.writeFrame(queued.frame) {
				return
			}
			continue
		}
		// While a frame waits on the hub's queue, the stream waits, and so does a frame taken from it.
		stream := socket.stream
		if len(socket.queue) > 0 || taken.from != nil {
			stream = nil
		}
		if socket.beforePick != nil {
			socket.beforePick(socket, stream != nil)
		}
		var frame any
		select {
		case <-readerDone:
			return
		case <-socket.full:
			continue
		case <-socket.shut:
			socket.close(socket.shutCode, socket.shutReason)
			return
		case <-aged.C:
			socket.close(contracts.LiveSocketMaxAgeClose, liveSocketMaxAgeReason)
			return
		case <-lifetime:
			socket.close(gws.CloseServiceRestart, "")
			return
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), errCredentialExpired) {
				socket.close(contracts.LiveSocketMaxAgeClose, liveSocketCredentialExpiredReason)
			} else {
				socket.close(gws.ClosePolicyViolation, "")
			}
			return
		case <-ticker.C:
			if err := socket.conn.WriteControl(gws.PingMessage, nil, time.Now().Add(liveSocketWriteWait)); err != nil {
				socket.endReads(0)
				return
			}
			frame = liveHeartbeat
		case frame = <-socket.queue:
		case queued := <-stream:
			// The look above found the hub's queue empty, but a hub frame can arrive before the pick,
			// and the pick then chooses between the two at random. One queued before this frame goes
			// first, so this one waits until the hub's queue is empty.
			if len(socket.queue) > 0 {
				taken = queued
				continue
			}
			if queued.from.stopped.Load() {
				continue // its subscription has stopped, and what replaced it may already have written
			}
			frame = queued.frame
		}
		if !socket.writeFrame(frame) {
			return
		}
	}
}

// writeFrame writes frame to the page, and answers false once the socket is done: a frame that did
// not encode closes it, and a failed write ends its reads.
func (socket *liveSocket) writeFrame(frame any) bool {
	data, err := json.Marshal(frame)
	if err != nil {
		slog.Error("dispatch: live socket closed: a frame did not encode", "frame", fmt.Sprintf("%T", frame), "error", err)
		socket.close(gws.CloseInternalServerErr, "")
		return false
	}
	_ = socket.conn.SetWriteDeadline(time.Now().Add(liveSocketWriteWait))
	if err := socket.conn.WriteMessage(gws.TextMessage, data); err != nil {
		socket.endReads(0)
		return false
	}
	return true
}

// serveLiveSocket serves GET /ws/live to a signed-in person or an agent's bearer, keeping what
// that caller's credential may do: a frame carries no credential, so the credential the caller
// proved here is what each of its views is answered against (liveSocket.actor). The upgrade
// keeps gorilla's same-origin check, so another site's page cannot open the socket with a
// person's cookie.
func (s *server) serveLiveSocket(w http.ResponseWriter, r *http.Request) {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	// A caller whose viewer does not resolve is refused here, as every read refuses one; each view
	// resolves what the caller sees again for itself (readerFor).
	if _, err := s.readerFor(r.Context(), actor); err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	// The handler is counted before the upgrade hands its connection over, while the server still
	// tracks the request as its own: a page that reads the upgrade's answer and closes at once can
	// end the test that opened it, whose cleanup waits for every handler (liveSocketSet.wait), and
	// a count taken after the upgrade could come after that wait began.
	defer s.liveSockets.serve()()
	upgrader := gws.Upgrader{EnableCompression: true, ReadBufferSize: liveSocketReadBuffer}
	conn, err := upgrader.Upgrade(w, r, access.UpgradeHeader(w))
	if err != nil {
		return // the upgrader has answered the request
	}
	age := s.liveSocketAge()
	socket := newLiveSocket(r.Context(), conn, actor)
	socket.beforePick = s.liveWriterBeforePick
	if !s.liveSockets.add(socket) {
		socket.closeUnserved()
		return
	}
	defer s.liveSockets.remove(socket)
	defer conn.Close()
	defer s.deliveryHub.Close(socket)
	defer s.endLiveSubscriptions(socket)

	pongWait := 2 * s.deps.StreamHeartbeat
	conn.SetReadLimit(liveSocketReadLimit)
	socket.awaitPong(pongWait)
	conn.SetPongHandler(func(string) error {
		socket.awaitPong(pongWait)
		return nil
	})
	readerDone := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer socket.ended()
		socket.write(r.Context(), s.lifetime.Done(), s.deps.StreamHeartbeat, age, readerDone)
	}()

	var readErr error
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			readErr = err
			break
		}
		if kind != gws.TextMessage {
			socket.Send(liveRefusal("", "INVALID_FRAME", "a frame is one JSON object in a text message"))
			continue
		}
		s.liveSocketFrame(socket, data)
	}
	close(readerDone)
	<-writerDone
	if errors.Is(readErr, gws.ErrReadLimit) {
		// The rest of the frame past the limit is still arriving. Reading it off before closing makes
		// the close a FIN behind the 1009 gorilla wrote, not a reset that can overtake it.
		_ = conn.NetConn().SetReadDeadline(time.Now().Add(liveSocketCloseWait))
		_, _ = io.Copy(io.Discard, conn.NetConn())
	}
}

// liveSocketFrame reads one frame from the page and hands it on (subscribeLive, unsubscribeLive),
// or answers the error frame refusing it, which names the frame's sub when it has one Dispatch can
// send back. Every frame passes the input layer's checks first: a U+0000 anywhere in its JSON
// (unstorableJSON), and a delivery view's query as the GET routes' wrapper checks theirs
// (unstorableQuery).
func (s *server) liveSocketFrame(socket *liveSocket, data []byte) {
	var head struct {
		Type string `json:"type"`
		Sub  string `json:"sub"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		socket.Send(liveRefusal("", "INVALID_FRAME", "a frame is one JSON object with a string type and sub: "+err.Error()))
		return
	}
	sub := head.Sub
	if refusal := unstorableJSON("", data); refusal != nil {
		if !text.Storable(sub) {
			sub = ""
		}
		socket.Send(liveRefusal(sub, refusal.code, refusal.message))
		return
	}
	switch head.Type {
	case "subscribe":
		var frame LiveSubscribe
		if err := decodeSocketClientFrame(data, &frame); err != nil {
			socket.Send(liveRefusal(sub, "INVALID_FRAME", "a subscribe frame: "+err.Error()))
			return
		}
		if frame.View.Query != nil {
			if refusal := unstorableQuery(*frame.View.Query); refusal != nil {
				socket.Send(liveRefusal(sub, refusal.code, refusal.message))
				return
			}
		}
		s.subscribeLive(socket, frame)
	case "unsubscribe":
		var frame LiveUnsubscribe
		if err := decodeSocketClientFrame(data, &frame); err != nil {
			socket.Send(liveRefusal(sub, "INVALID_FRAME", "an unsubscribe frame: "+err.Error()))
			return
		}
		if frame.Sub == "" {
			socket.Send(liveRefusal("", "INVALID_FRAME", "an unsubscribe frame names its sub"))
			return
		}
		s.unsubscribeLive(socket, frame.Sub)
	default:
		socket.Send(liveRefusal(sub, "INVALID_FRAME", "type must be subscribe or unsubscribe"))
	}
}

// decodeSocketClientFrame decodes one frame from the page into its struct. A member the struct
// does not declare is ignored, as decodeJSON ignores one in a body, since a page of a newer build
// can reach this build while a release rolls out. A frame type or a view kind this build cannot
// serve is still refused (liveSocketFrame, the hub's view check), so that page reconnects until it
// reaches a process that serves it.
func decodeSocketClientFrame(data []byte, frame any) error {
	return json.Unmarshal(data, frame)
}

// disconnectLiveSockets closes every open GET /ws/live socket on every process with 1012, as a
// restart does, so a test can drive the page's reconnect, its resume and its refresh of what the
// socket does not keep live, and answers once every process has (runTestHookEverywhere).
// Test-only: mounted only under Deps.TestHooksEnabled.
func (s *server) disconnectLiveSockets(w http.ResponseWriter, r *http.Request) {
	s.shutLiveSockets(w, r, hookDisconnectSockets)
}

// rotateLiveSockets closes every open GET /ws/live socket on every process with 4001 (max age),
// as its age does, so a test can drive the page's quiet reopen without waiting half an hour, and
// answers once every process has. Test-only: mounted only under Deps.TestHooksEnabled.
func (s *server) rotateLiveSockets(w http.ResponseWriter, r *http.Request) {
	s.shutLiveSockets(w, r, hookRotateSockets)
}

func (s *server) shutLiveSockets(w http.ResponseWriter, r *http.Request, hook string) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.runTestHookEverywhere(w, r, testHookCall{Hook: hook})
}

// shutAllLiveSockets is the local half of those hooks: every socket this process holds, closed
// with code and reason.
func (s *server) shutAllLiveSockets(code int, reason string) {
	for _, socket := range s.liveSockets.all() {
		socket.shutWith(code, reason)
	}
}

// liveSocketSet is every open GET /ws/live socket: the test hooks close them, and the shutdown
// (server.waitBackground) waits for every socket's handler, and so every subscription it ran, to
// return before the database pool closes. http.Server.Shutdown counts no hijacked connection, so a
// socket request in flight at the signal can reach serve, and its upgraded socket add, after that
// wait has begun. The set takes no socket from then on: its count is a sync.WaitGroup, whose count
// may not rise from zero beside its Wait, and a socket taken after the wait returned would serve on
// while the pool closes.
type liveSocketSet struct {
	mu      sync.Mutex
	open    map[*liveSocket]struct{}
	serving sync.WaitGroup
	// closed is set as wait begins. It is read and set under mu, the lock serve counts a handler
	// under, so every Add either comes before the Wait or does not happen.
	closed bool
}

// serve counts one socket handler until the func it answers runs (wait). Once wait has begun it
// counts nothing and answers a func that does nothing, and add then refuses the handler's socket.
func (set *liveSocketSet) serve() (done func()) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return func() {}
	}
	set.serving.Add(1)
	return set.serving.Done
}

// add takes socket among the open sockets, and answers false, taking nothing, once wait has
// begun.
func (set *liveSocketSet) add(socket *liveSocket) bool {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return false
	}
	if set.open == nil {
		set.open = map[*liveSocket]struct{}{}
	}
	set.open[socket] = struct{}{}
	return true
}

func (set *liveSocketSet) remove(socket *liveSocket) {
	set.mu.Lock()
	defer set.mu.Unlock()
	delete(set.open, socket)
}

// all is every socket open now.
func (set *liveSocketSet) all() []*liveSocket {
	set.mu.Lock()
	defer set.mu.Unlock()
	sockets := make([]*liveSocket, 0, len(set.open))
	for socket := range set.open {
		sockets = append(sockets, socket)
	}
	return sockets
}

// wait closes the set to new sockets and returns once every socket's handler has returned.
func (set *liveSocketSet) wait() {
	set.mu.Lock()
	set.closed = true
	set.mu.Unlock()
	set.serving.Wait()
}
