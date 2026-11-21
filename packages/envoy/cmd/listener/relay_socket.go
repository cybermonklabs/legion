package main

// relay_socket.go is an edge listener's relay socket: GET /v1/sessions/{session}/relay, a
// WebSocket on which a session that holds no NATS connection of its own (an outside person's
// agent, reaching the edge over HTTPS) receives its relay frames and answers them, for as long as
// the broker still holds the token that opened it live. Only a listener in edge mode
// (ENVOY_EDGE_PROJECTS) serves it; every other listener answers the path 404 (relaySocketRoute),
// so no shared listener's NATS grant has to cover it.
//
// The wire is the subprotocol envoy-relay.v1, text frames of JSON:
//
//   - the client's {"op":"sub","sid":<int>,"subject":<s>} subscribes, on the sockets' own NATS
//     connection, exactly the socket's own relay subject (bus.DeliverSubject), and is answered
//     {"op":"err","sid":<int>,"code":"subject_not_granted"} for any other subject, the agent
//     stream's included; a sub past relaySocketMaxSubscriptions (one) is answered that sid's err
//     subscription_limit and the socket stays open; a sub that succeeds is answered nothing;
//   - {"op":"unsub","sid":<int>} ends that subscription;
//   - {"op":"pub","subject":<s>,"data":<base64>} publishes data on s, which must be the receipt
//     subject of a frame this socket forwarded in the last relaySocketReplyWindow and data one of
//     the relay's reply words (received, progress, done, nak), else
//     {"op":"err","code":"publish_not_granted"};
//   - the server's {"op":"msg","sid":…,"subject":…,"reply":…,"headers":{…},"data":<base64>} is
//     one relay frame, its headers always present as nats.Header in JSON (each name as NATS carries
//     it, case kept, to every one of its values in order; {} when there are none);
//   - {"op":"err","code":"bad_frame"}, then a close 1008 with reason bad_frame, answers a frame the
//     socket cannot read at all: not JSON, an unknown op, a sub or unsub naming no sid, or a sub
//     of a sid already held.
//
// The socket reads its frames one at a time on one reader and handles each whole before it reads
// the next: a sub's subscribe and the flush that makes the server hold it, a pub's publish and
// its flush. So the client's flush(), a WebSocket ping, is answered by gorilla/websocket's own
// ping handler on that reader only once every frame read before it is handled; nothing here
// installs another.
//
// A frame's receipt subject is the owning machine's, under its receipt family
// (natsgrant.EdgeRelayReplyPrefix; startSessionRelays in edge mode), which the edge's grant
// publishes for every machine and subscribes for its own: the socket takes a receipt under any
// machine's family (bus.RelayReplyPrefix), since it answers frames other machines own, and no reply
// inbox is answered from here. The connection probe (connection_probe.go) is never forwarded and
// never answered: the subscription itself is what keeps the session connected, and a probe costs
// its sender at most its one second. A message on the relay subject that is no relay frame (its
// reply not a relay receipt) is not forwarded either.
//
// While a socket is open it asks the broker again every relaySocketCheckEvery, on its ping tick,
// and once at its token's expiry (watch), and closes with the code its client reopens by
// (verdict). A subscription holds at most relaySocketPendingMsgs frames and relaySocketPendingBytes
// for a client that reads slower than they come, past which the socket closes 1013 slow_consumer;
// a replica holds at most relaySocketMaxPerListener sockets, past which an upgrade is refused 503.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/natsgrant"
)

const (
	// relaySocketPattern is the socket's route, one segment the session.
	relaySocketPattern = "/v1/sessions/{session}/relay"
	// relaySubprotocol is the one subprotocol the socket speaks; an upgrade that offers none of
	// it is refused 400.
	relaySubprotocol = "envoy-relay.v1"
	// relaySocketCheckDefault is how often an open socket pings its client and asks the broker
	// about its token: inside the 120 s backend timeout of the edge's load balancer, and the 20 s
	// of a revoke's 25 s bound (the broker's answer is reused for at most
	// ciphertoken.AffirmativeCacheTTL more).
	relaySocketCheckDefault = 20 * time.Second
	// relaySocketReadLimit is the largest frame a client may send; a larger one closes the socket
	// 1009, which gorilla/websocket writes.
	relaySocketReadLimit = 1 << 20
	// relaySocketMaxPerSession is how many sockets one session may hold open on one replica; the
	// next is refused 429.
	relaySocketMaxPerSession = 4
	// relaySocketMaxPerListener is how many sockets one replica holds open, of every session; the
	// next is refused 503, on which its client opens it on another replica.
	relaySocketMaxPerListener = 256
	// relaySocketMaxSubscriptions is how many subscriptions one socket may hold. A socket has one
	// subject to subscribe, so a client needs one; a second would multiply every frame.
	relaySocketMaxSubscriptions = 1
	// relaySocketPendingMsgs and relaySocketPendingBytes bound what one socket holds for its
	// client: the frames it took from its subscription and has not yet written, the one being
	// written included. The frame that would pass either closes the socket 1013 slow_consumer,
	// and each frame it held is sent again by its owning relay, which hears no receipt for it. The
	// owning relay holds at most relayEdgeInFlight of a session's frames in flight, so a client
	// that reads promptly never comes near them: twice that many frames (a redelivery beside the
	// frames it replaces), and that many frames of relayFrameMaxBytes, which leaves no such room
	// for frames near that size. A socket alone may so hold 16 MiB; what all of a replica's
	// sockets hold together is bounded by relaySocketReplicaPendingBytes, not by their number.
	relaySocketPendingMsgs  = 2 * relayEdgeInFlight
	relaySocketPendingBytes = relayEdgeInFlight * relayFrameMaxBytes
	// relaySocketReplicaPendingBytes bounds what all of one replica's sockets hold for their
	// clients together, the replica's count (relaySockets.buffered) of the bytes each socket took
	// and has not yet written: 256 MiB, a quarter of the listener container's default 1Gi
	// (infra/gcp, defaultClusteredHubListenerMemory), whoever opened the sockets. The frame that
	// would take the count past it closes its socket 1013 slow_consumer,
	// so one person's many slow sockets cannot hold more, though they can close other sockets of
	// the replica while they fill it. Beside it, each socket remembers at most
	// relaySocketMaxReplies receipt subjects of about 150 bytes each, about 150 KiB a socket and
	// 38 MiB for relaySocketMaxPerListener of them, which the count does not hold.
	relaySocketReplicaPendingBytes = 256 << 20
	// relayFrameMaxBytes is the largest message the hub carries (nats-server's max_payload, 1 MiB),
	// and so the largest relay frame.
	relayFrameMaxBytes = 1 << 20
	// relaySocketReplyWindow is how long a forwarded frame's receipt subject may be published on.
	relaySocketReplyWindow = 10 * time.Minute
	// relaySocketMaxReplies bounds the receipt subjects one socket remembers; past it the oldest
	// is forgotten first, and a receipt of a frame it forgot is refused publish_not_granted, on
	// which the owning relay sends the frame again. 1024 is 64 times the frames the owning relay
	// holds in flight for the session (relayEdgeInFlight).
	relaySocketMaxReplies = 1 << 10
	// relaySocketWriteWait bounds one write to the client, a frame or a control frame.
	relaySocketWriteWait = 10 * time.Second
	// relaySocketFlushWait bounds the flush that makes the server hold a sub, an unsub or a pub.
	relaySocketFlushWait = 5 * time.Second
	// relaySocketCheckWait bounds one ask of the broker.
	relaySocketCheckWait = 10 * time.Second
	// relaySocketCloseGrace is how long a socket the server closed waits for its client's close
	// frame before it drops the connection, so the close frame is read before the connection ends.
	relaySocketCloseGrace = time.Second
	// relaySocketPongFloor is the least the reader waits for a frame or a pong before it takes
	// the client for gone, whatever relaySocketCheckEvery a test gives it.
	relaySocketPongFloor = 10 * time.Second
)

// The close codes and reasons an open socket ends with, and what its client does on each: 4401
// reopens at once with a fresh trade; 4403 reopens with a fresh trade after 1 s, doubling to 60 s;
// 1013 after 5 s, doubling to 60 s; 1012 at once.
const (
	relaySocketCloseExpired = 4401
	relaySocketCloseRefused = 4403

	relaySocketReasonExpired           = "credential_expired"
	relaySocketReasonRefused           = "credential_refused"
	relaySocketReasonBrokerUnavailable = "broker_unavailable"
	relaySocketReasonNATSUnavailable   = "nats_unavailable"
	relaySocketReasonSlowConsumer      = "slow_consumer"
	relaySocketReasonStopping          = "listener_stopping"
	relaySocketReasonTextOnly          = "text_frames_only"
)

// The codes of the err frame.
const (
	relayErrSubjectNotGranted = "subject_not_granted"
	relayErrPublishNotGranted = "publish_not_granted"
	relayErrBadFrame          = "bad_frame"
	relayErrSubscriptionLimit = "subscription_limit"
)

// relaySocketCheckBuild is a test build's check interval, as time.ParseDuration reads it, which a
// test that runs the listener's binary sets with -ldflags "-X main.relaySocketCheckBuild=1s" so a
// socket's token is asked about again within a second (relay_socket_live_test.go). It is empty in
// every other build, whose sockets check every relaySocketCheckDefault.
var relaySocketCheckBuild string

// relaySocketCheckInterval is relaySocketCheckBuild when a build set it, and relaySocketCheckDefault
// otherwise. A build that set it to anything but a positive duration is a broken build, and stops.
func relaySocketCheckInterval() time.Duration {
	if relaySocketCheckBuild == "" {
		return relaySocketCheckDefault
	}
	every, err := time.ParseDuration(relaySocketCheckBuild)
	if err != nil || every <= 0 {
		panic("main.relaySocketCheckBuild is " + strconv.Quote(relaySocketCheckBuild) + ", which is no positive duration")
	}
	return every
}

// relaySockets is an edge listener's relay sockets: the route that opens them and every one this
// replica holds open, so a stop closes each with 1012. Its client is a NATS connection of the
// sockets' own, so a socket's slow consumer, which nats.go records as its connection's last error,
// never reaches the listener's main connection, whose publishes read that error.
type relaySockets struct {
	client   *bus.Client
	brokers  *brokerTokens
	logger   *logging.Logger
	upgrader websocket.Upgrader
	// relaySocketCheckEvery is how often an open socket pings its client and asks the broker
	// about its token: relaySocketCheckDefault, which only a test lowers.
	relaySocketCheckEvery time.Duration
	// maxSockets is how many sockets the replica holds open (relaySocketMaxPerListener),
	// pendingMsgs and pendingBytes what one socket holds for its client
	// (relaySocketPendingMsgs, relaySocketPendingBytes), and budget what all of them hold
	// together (relaySocketReplicaPendingBytes); only a test changes them.
	maxSockets                int
	pendingMsgs, pendingBytes int
	budget                    int64
	// buffered is the bytes every socket of the replica holds for its client, which budget
	// bounds (relaySocket.enqueue).
	buffered atomic.Int64
	// now is the clock a socket judges its token's expiry by; a test moves it with the
	// verifier's.
	now func() time.Time

	mu      sync.Mutex
	open    map[string]map[*relaySocket]struct{}
	count   int
	stopped bool
}

// newRelaySockets serves relay sockets on client's connection, admitting the callers brokers
// admits and asking it about each one again while it is open.
func newRelaySockets(client *bus.Client, brokers *brokerTokens, logger *logging.Logger) *relaySockets {
	return &relaySockets{
		client:  client,
		brokers: brokers,
		logger:  logger,
		upgrader: websocket.Upgrader{
			Subprotocols:     []string{relaySubprotocol},
			HandshakeTimeout: relaySocketWriteWait,
			// CheckOrigin is gorilla's own: an upgrade that carries no Origin, as a client
			// outside a browser sends none, or an Origin of the request's own host is taken, and
			// any other Origin refused 403. The socket is admitted by a DPoP-bound token and its
			// proof, which a page of another origin cannot present, so this fences nothing
			// today; it keeps the socket safe should a cookie ever admit it.
		},
		relaySocketCheckEvery: relaySocketCheckInterval(),
		maxSockets:            relaySocketMaxPerListener,
		pendingMsgs:           relaySocketPendingMsgs,
		pendingBytes:          relaySocketPendingBytes,
		budget:                relaySocketReplicaPendingBytes,
		now:                   time.Now,
		open:                  map[string]map[*relaySocket]struct{}{},
	}
}

// relaySocketRoute is the handler of relaySocketPattern: the sockets' own on an edge listener, and
// on every other listener the 404 /v1 answers a path it serves nothing at.
func relaySocketRoute(sockets *relaySockets) http.Handler {
	if sockets == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSONError(w, http.StatusNotFound, "not found")
		})
	}
	return http.HandlerFunc(sockets.serve)
}

// serve opens one socket. The token was verified by apiAuth with its proof, as every /v1 request's
// is, and held to the edge's projects there; here the session the path names is refused 400 unless
// it is one NATS subject token (natsgrant.SessionToken), since its relay subject would otherwise be
// a wildcard, or reach under another session's, on this replica's connection; then it is held to
// the token's session, and the upgrade is refused 400 without the subprotocol, 429 past
// relaySocketMaxPerSession and 503 once the listener is stopping.
func (s *relaySockets) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	session := r.PathValue("session")
	if !natsgrant.SessionToken(session) {
		writeJSONError(w, http.StatusBadRequest, "the session is not one NATS subject token: it holds a '.', '*', '>', whitespace or a control character")
		return
	}
	caller, ok := brokerCaller(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if refusal := sessionNotBound(r, s.logger, session); refusal != "" {
		writeJSONError(w, http.StatusForbidden, refusal)
		return
	}
	if !slices.Contains(websocket.Subprotocols(r), relaySubprotocol) {
		writeJSONError(w, http.StatusBadRequest, "the relay socket speaks the subprotocol "+relaySubprotocol)
		return
	}
	// The answer apiAuth's verify just read is the one this reads, from the verifier's cache: the
	// instant the token stops being live, which tells its expiry from a revoke once it is over.
	_, expires, err := s.brokers.verifier.Load().LiveUntil(r.Context(), r)
	if err != nil {
		refuseBrokerToken(w, s.logger, r, s.brokers, err)
		return
	}
	socket := &relaySocket{
		sockets:  s,
		session:  session,
		admitted: caller,
		expires:  expires,
		check:    r.Clone(context.Background()),
		subs:     map[int64]*nats.Subscription{},
		replies:  map[string]time.Time{},
		done:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
	}
	if status, refusal := s.admit(socket); status != 0 {
		writeJSONError(w, status, refusal)
		return
	}
	defer s.release(socket)
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has answered the request itself.
		return
	}
	if !s.attach(socket, conn) {
		// The listener began to stop during the upgrade.
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, relaySocketReasonStopping), time.Now().Add(relaySocketWriteWait))
		_ = conn.Close()
		return
	}
	s.logger.Info("listener: relay socket opened", slog.String("session_id", session))
	socket.read()
}

// attach gives socket the connection its upgrade made, unless the listener began to stop
// meanwhile. stop ends only the sockets attached, under the same lock, so it never reaches one
// without its connection.
func (s *relaySockets) attach(socket *relaySocket, conn *websocket.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	socket.conn = conn
	return true
}

// admit records socket as open, or answers the status it is refused with: 503 once the listener
// is stopping or holds its cap of sockets (maxSockets), on which the client opens it on another
// replica, and 429 when its session already holds relaySocketMaxPerSession.
func (s *relaySockets) admit(socket *relaySocket) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return http.StatusServiceUnavailable, "the listener is stopping; open the socket again on another replica"
	}
	held := s.open[socket.session]
	if len(held) >= relaySocketMaxPerSession {
		return http.StatusTooManyRequests, "this session already holds the most relay sockets a replica serves it"
	}
	if s.count >= s.maxSockets {
		return http.StatusServiceUnavailable, "this replica holds the most relay sockets it serves; open the socket again on another replica"
	}
	if held == nil {
		held = map[*relaySocket]struct{}{}
		s.open[socket.session] = held
	}
	held[socket] = struct{}{}
	s.count++
	return 0, ""
}

// release forgets socket, whatever ended it.
func (s *relaySockets) release(socket *relaySocket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.open[socket.session][socket]; held {
		s.count--
	}
	delete(s.open[socket.session], socket)
	if len(s.open[socket.session]) == 0 {
		delete(s.open, socket.session)
	}
}

// stop closes every open socket with 1012, so each client opens it again at once on another
// replica, refuses every upgrade after it 503, and then closes the sockets' own NATS connection.
// It returns once each close frame is written, or its connection dropped for a client that reads
// nothing, within relaySocketCloseGrace (end). It is safe to call more than once.
func (s *relaySockets) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopped = true
	var open []*relaySocket
	for _, held := range s.open {
		for socket := range held {
			if socket.conn != nil {
				open = append(open, socket)
			}
		}
	}
	s.mu.Unlock()
	var ends sync.WaitGroup
	for _, socket := range open {
		ends.Go(func() { socket.end(websocket.CloseServiceRestart, relaySocketReasonStopping) })
	}
	ends.Wait()
	s.client.Close()
}

// relaySocket is one open socket.
type relaySocket struct {
	sockets *relaySockets
	conn    *websocket.Conn
	session string
	// admitted is the caller the upgrade was admitted as, which every later answer is compared
	// with.
	admitted ciphertoken.Caller
	// expires is when the upgrade's answer said the token stops being live, zero when it named no
	// expiry.
	expires time.Time
	// check is a detached copy of the upgrade's request, which every later ask of the broker
	// presents: its token, and no proof checked again, since that proof is spent.
	check *http.Request

	// writeMu orders the data frames written to the client, which gorilla/websocket takes from
	// one writer at a time: the reader's err frames and the pump's msg frames. ended is
	// set once, by end, which takes no lock a data write holds: a write blocked on a client that
	// reads nothing holds writeMu for up to relaySocketWriteWait.
	writeMu sync.Mutex
	ended   atomic.Bool
	// pinging is set while a ping is being written, off the watch's goroutine.
	pinging atomic.Bool

	subsMu sync.Mutex
	subs   map[int64]*nats.Subscription

	// queueMu guards queue, the frames the socket took from its subscription and has not yet
	// begun to write, oldest first, and heldMsgs and heldBytes, what it holds for its client: the
	// queue and the frame the pump is writing. enqueue adds to them, and the pump and end take
	// from them, each the replica's count (relaySockets.buffered) with them. wake tells the pump
	// the queue holds a frame.
	queueMu   sync.Mutex
	queue     []queuedFrame
	heldMsgs  int
	heldBytes int
	wake      chan struct{}

	// repliesMu guards replies, the receipt subjects of the frames forwarded in the last
	// relaySocketReplyWindow, by when each was forwarded, and replyOrder, the same in the order
	// they were, oldest first, which the prune walks.
	repliesMu  sync.Mutex
	replies    map[string]time.Time
	replyOrder []forwardedReply
	// strayLogged is set once this socket has logged a message on its relay subject that was no
	// relay frame.
	strayLogged atomic.Bool

	endOnce sync.Once
	done    chan struct{}
}

// forwardedReply is one receipt subject and when its frame was forwarded.
type forwardedReply struct {
	subject string
	at      time.Time
}

// relayClientFrame is a frame the client sends.
type relayClientFrame struct {
	Op      string `json:"op"`
	SID     *int64 `json:"sid"`
	Subject string `json:"subject"`
	Data    []byte `json:"data"`
}

// relayMsgFrame is one relay frame, forwarded.
type relayMsgFrame struct {
	Op      string      `json:"op"`
	SID     int64       `json:"sid"`
	Subject string      `json:"subject"`
	Reply   string      `json:"reply"`
	Headers nats.Header `json:"headers"`
	Data    string      `json:"data"`
}

// relayErrFrame is the answer to a frame the socket did not take.
type relayErrFrame struct {
	Op   string `json:"op"`
	SID  *int64 `json:"sid,omitempty"`
	Code string `json:"code"`
}

// pongWait is how long the reader waits for any frame or pong before it takes the client for gone:
// three pings' worth, so one late pong closes nothing, and never under relaySocketPongFloor.
func (s *relaySockets) pongWait() time.Duration {
	return max(3*s.relaySocketCheckEvery, relaySocketPongFloor)
}

// read is the socket's one reader: it handles each frame whole before it reads the next, and
// returns once the connection ends.
func (socket *relaySocket) read() {
	conn := socket.conn
	conn.SetReadLimit(relaySocketReadLimit)
	extend := func() { _ = conn.SetReadDeadline(time.Now().Add(socket.sockets.pongWait())) }
	extend()
	conn.SetPongHandler(func(string) error {
		extend()
		return nil
	})
	go socket.watch()
	go socket.pump()
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			// The client's close frame, which gorilla answers, a frame past the read limit, which
			// gorilla answers 1009, or a connection that ended: the socket ends with nothing more
			// to write, and the connection is dropped now. When the listener closed the socket
			// itself, end already ran and left the connection to its grace (relaySocketCloseGrace)
			// for this answer; the answer is in, so the client is not kept waiting out the rest of
			// that grace for its connection's end, which a client that reopens on it waits for.
			socket.end(0, "")
			_ = conn.Close()
			return
		}
		extend()
		if kind != websocket.TextMessage {
			socket.end(websocket.CloseUnsupportedData, relaySocketReasonTextOnly)
			continue
		}
		socket.handle(data)
	}
}

// handle takes one client frame.
func (socket *relaySocket) handle(data []byte) {
	select {
	case <-socket.done:
		// The socket has ended; a frame its client sent before the close is not taken.
		return
	default:
	}
	var frame relayClientFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		socket.badFrame(nil)
		return
	}
	switch frame.Op {
	case "sub":
		socket.subscribe(frame)
	case "unsub":
		socket.unsubscribe(frame)
	case "pub":
		socket.publish(frame)
	default:
		socket.badFrame(frame.SID)
	}
}

// badFrame answers a frame the socket cannot read as the wire's - one that is not JSON, of an
// unknown op, a sub or unsub naming no sid, or a sub of a sid the socket already holds - with the
// err frame bad_frame, under sid when it named one, and closes the socket 1008 with that reason:
// its client speaks something else, and nothing it sends next can be read either.
func (socket *relaySocket) badFrame(sid *int64) {
	socket.refuse(sid, relayErrBadFrame)
	socket.end(websocket.ClosePolicyViolation, relayErrBadFrame)
}

// subscribe takes a sub: the socket's own relay subject alone, subscribed on the sockets'
// connection under the socket's pending limits and flushed before the reader reads on.
func (socket *relaySocket) subscribe(frame relayClientFrame) {
	if frame.SID == nil {
		socket.badFrame(nil)
		return
	}
	sid := *frame.SID
	subject := bus.DeliverSubject(socket.session)
	if frame.Subject != subject {
		socket.refuse(frame.SID, relayErrSubjectNotGranted)
		return
	}
	socket.subsMu.Lock()
	_, held := socket.subs[sid]
	full := len(socket.subs) >= relaySocketMaxSubscriptions
	socket.subsMu.Unlock()
	switch {
	case held:
		socket.badFrame(frame.SID)
		return
	case full:
		// The socket stays open: its subscriptions up to the bound go on.
		socket.refuse(frame.SID, relayErrSubscriptionLimit)
		return
	}
	conn := socket.sockets.client.Connection()
	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) { socket.forward(sid, msg) })
	if err != nil {
		socket.endNATS("subscribe", err)
		return
	}
	if err := sub.SetPendingLimits(socket.sockets.pendingMsgs, socket.sockets.pendingBytes); err != nil {
		_ = sub.Unsubscribe()
		socket.endNATS("bound a subscription's pending frames", err)
		return
	}
	if err := conn.FlushTimeout(relaySocketFlushWait); err != nil {
		_ = sub.Unsubscribe()
		socket.endNATS("flush a subscribe", err)
		return
	}
	socket.subsMu.Lock()
	defer socket.subsMu.Unlock()
	if socket.subs == nil {
		// The socket ended while the subscribe was in flight.
		_ = sub.Unsubscribe()
		return
	}
	socket.subs[sid] = sub
}

// unsubscribe takes an unsub: the subscription of that sid ends, and the flush that makes the
// server drop it comes before the reader reads on. A sid the socket does not hold is nothing.
func (socket *relaySocket) unsubscribe(frame relayClientFrame) {
	if frame.SID == nil {
		socket.badFrame(nil)
		return
	}
	socket.subsMu.Lock()
	sub := socket.subs[*frame.SID]
	delete(socket.subs, *frame.SID)
	socket.subsMu.Unlock()
	if sub == nil {
		return
	}
	_ = sub.Unsubscribe()
	if err := socket.sockets.client.Connection().FlushTimeout(relaySocketFlushWait); err != nil {
		socket.endNATS("flush an unsubscribe", err)
	}
}

// publish takes a pub: a receipt of a frame this socket forwarded, whose body is one of the
// relay's reply words (relayReplyReceived, Progress, Done, Nak; the relay ignores any other),
// published on the sockets' connection and flushed before the reader reads on. A pub of another
// subject or another body is refused publish_not_granted, so a client publishes nothing but
// receipts, each a word long.
func (socket *relaySocket) publish(frame relayClientFrame) {
	if !socket.forwarded(frame.Subject) || !relayReceiptBody(frame.Data) {
		socket.refuse(nil, relayErrPublishNotGranted)
		return
	}
	conn := socket.sockets.client.Connection()
	if err := conn.Publish(frame.Subject, frame.Data); err != nil {
		socket.endNATS("publish a receipt", err)
		return
	}
	if err := conn.FlushTimeout(relaySocketFlushWait); err != nil {
		socket.endNATS("flush a receipt", err)
	}
}

// relayReceiptBody reports whether data is one of the replies the owning relay reads (onReply),
// read as onReply reads it.
func relayReceiptBody(data []byte) bool {
	switch string(bytes.TrimSpace(data)) {
	case relayReplyReceived, relayReplyProgress, relayReplyDone, relayReplyNak:
		return true
	}
	return false
}

// forward takes one message of a subscription, on the subscription's own goroutine: a relay frame
// alone, never the connection probe, is held for the client (enqueue), and the pump writes it. A
// subscription that dropped frames past nats.go's pending limits closes the socket 1013
// slow_consumer (lagging); with the pump taking each frame at once, that is a backstop.
func (socket *relaySocket) forward(sid int64, msg *nats.Msg) {
	if _, probe := msg.Header[bus.ConnectionProbeHeader]; probe {
		return
	}
	if !strings.HasPrefix(msg.Reply, bus.RelayReplyPrefix) {
		socket.stray(msg)
		return
	}
	if socket.lagging() {
		socket.endSlowConsumer("its subscription dropped frames past nats.go's pending limits")
		return
	}
	socket.enqueue(sid, msg)
}

// lagging reports whether a subscription of the socket dropped frames past nats.go's pending
// limits: a backstop, since the pump takes each frame from the subscription at once.
func (socket *relaySocket) lagging() bool {
	socket.subsMu.Lock()
	defer socket.subsMu.Unlock()
	for _, sub := range socket.subs {
		if dropped, err := sub.Dropped(); err == nil && dropped > 0 {
			return true
		}
	}
	return false
}

// endSlowConsumer ends the socket 1013 slow_consumer, for why: its client opens it again after 5 s,
// and the owning relay sends the frames the socket held again, since it heard no receipt for them.
func (socket *relaySocket) endSlowConsumer(why string) {
	held, bytes := socket.held()
	socket.sockets.logger.Warn("listener: the relay socket's client reads its frames slower than they come; it is closed",
		slog.String("session_id", socket.session), slog.String("why", why),
		slog.Int("held_frames", held), slog.Int("held_bytes", bytes), slog.Int64("replica_held_bytes", socket.sockets.buffered.Load()))
	socket.end(websocket.CloseTryAgainLater, relaySocketReasonSlowConsumer)
}

// stray logs, once a socket, a message on the relay subject that is no relay frame: one whose
// reply is no relay receipt, which this socket could not publish an answer to.
func (socket *relaySocket) stray(msg *nats.Msg) {
	if !socket.strayLogged.Swap(true) {
		socket.sockets.logger.Warn("listener: the relay socket passed over a message on its relay subject whose reply is no relay receipt",
			slog.String("session_id", socket.session), slog.String("reply", msg.Reply))
	}
}

// remember records reply as a receipt subject the client may publish on, and forgets the ones
// older than relaySocketReplyWindow.
func (socket *relaySocket) remember(reply string) {
	now := socket.sockets.now()
	socket.repliesMu.Lock()
	defer socket.repliesMu.Unlock()
	socket.replies[reply] = now
	socket.replyOrder = append(socket.replyOrder, forwardedReply{subject: reply, at: now})
	for len(socket.replyOrder) > 0 {
		oldest := socket.replyOrder[0]
		if now.Sub(oldest.at) <= relaySocketReplyWindow && len(socket.replyOrder) <= relaySocketMaxReplies {
			break
		}
		if socket.replies[oldest.subject].Equal(oldest.at) {
			delete(socket.replies, oldest.subject)
		}
		socket.replyOrder = socket.replyOrder[1:]
	}
}

// forwarded reports whether subject is the receipt subject of a frame this socket forwarded in the
// last relaySocketReplyWindow.
func (socket *relaySocket) forwarded(subject string) bool {
	socket.repliesMu.Lock()
	defer socket.repliesMu.Unlock()
	at, ok := socket.replies[subject]
	return ok && socket.sockets.now().Sub(at) <= relaySocketReplyWindow
}

// errRelaySocketEnded is a write to a socket that has ended.
var errRelaySocketEnded = errors.New("the relay socket has ended")

// write writes one data frame to the client. A write that ends because the socket ended under it
// (end wrote its close frame first, or dropped the connection) is errRelaySocketEnded too.
func (socket *relaySocket) write(frame any) error {
	socket.writeMu.Lock()
	defer socket.writeMu.Unlock()
	if socket.ended.Load() {
		return errRelaySocketEnded
	}
	if err := socket.conn.SetWriteDeadline(time.Now().Add(relaySocketWriteWait)); err != nil {
		return err
	}
	if err := socket.conn.WriteJSON(frame); err != nil {
		if socket.ended.Load() {
			return errRelaySocketEnded
		}
		return err
	}
	return nil
}

// refuse answers a frame the socket did not take, under sid when it named one.
func (socket *relaySocket) refuse(sid *int64, code string) {
	if err := socket.write(relayErrFrame{Op: "err", SID: sid, Code: code}); err != nil && !errors.Is(err, errRelaySocketEnded) {
		socket.end(0, "")
	}
}

// endNATS ends the socket 1013 over a NATS call that did not complete, which says nothing about
// the session: its client opens it again after 5 s, on whichever replica answers.
func (socket *relaySocket) endNATS(step string, err error) {
	socket.sockets.logger.Warn("listener: the relay socket could not reach NATS; it is closed",
		slog.String("session_id", socket.session), slog.String("step", step), slog.String("error", err.Error()))
	socket.end(websocket.CloseTryAgainLater, relaySocketReasonNATSUnavailable)
}

// end ends the socket once: it forwards nothing more, drops every subscription and the frames it
// held and had not begun to write (the pump drops the one it is writing once its write returns).
// With code 0 - a read or a write that failed - it drops the connection at once. Otherwise it
// writes the close frame of code and drops the connection once the client has answered it or
// relaySocketCloseGrace has passed since end began, so a close and a stop are bounded by the grace
// whatever the client reads.
func (socket *relaySocket) end(code int, reason string) {
	socket.endOnce.Do(func() {
		socket.ended.Store(true)
		socket.subsMu.Lock()
		subs := socket.subs
		socket.subs = nil
		socket.subsMu.Unlock()
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
		socket.queueMu.Lock()
		dropped := 0
		for _, frame := range socket.queue {
			dropped += frame.bytes
		}
		socket.heldMsgs -= len(socket.queue)
		socket.heldBytes -= dropped
		socket.queue = nil
		socket.queueMu.Unlock()
		socket.sockets.buffered.Add(-int64(dropped))
		close(socket.done)
		if code == 0 {
			// Nothing more is written to a connection whose read or write failed.
			_ = socket.conn.Close()
			return
		}
		socket.sockets.logger.Info("listener: relay socket closed",
			slog.String("session_id", socket.session), slog.Int("code", code), slog.String("reason", reason))
		// No data writer's lock is taken, since a data frame blocked on a client that reads
		// nothing holds it for up to relaySocketWriteWait. gorilla/websocket writes the close frame
		// once a frame being written has ended, waiting for that until the deadline at most, and
		// refuses every frame after the close (ErrCloseSent). A close frame it could not write by
		// then goes to a client that reads nothing, which is dropped at once.
		deadline := time.Now().Add(relaySocketCloseGrace)
		if err := socket.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), deadline); err != nil {
			_ = socket.conn.Close()
			return
		}
		time.AfterFunc(time.Until(deadline), func() { _ = socket.conn.Close() })
	})
}

// watch is the socket's tick: every relaySocketCheckEvery, and once at the token's expiry, it asks
// the broker about the socket's token and closes it on the verdict; it closes it 1013
// slow_consumer once a subscription dropped frames past nats.go's pending limits (lagging, a
// backstop), and 1013 nats_unavailable once a subscription has ended under it (its connection
// closed and was replaced); and otherwise pings the client (ping). It writes nothing itself, so a
// write blocked on a client that reads nothing never holds back a check. One ask is in flight at
// a time: a tick's ask that is still waiting on the broker when the token's expiry passes holds
// the expiry's check back until it returns, up to relaySocketCheckWait, so the 4401 can come that
// much after the expiry.
func (socket *relaySocket) watch() {
	ticker := time.NewTicker(socket.sockets.relaySocketCheckEvery)
	defer ticker.Stop()
	// A token closes at its expiry, not at the next tick after it, so its client trades for a
	// fresh one at once.
	var expiry <-chan time.Time
	if !socket.expires.IsZero() {
		timer := time.NewTimer(socket.expires.Sub(socket.sockets.now()))
		defer timer.Stop()
		expiry = timer.C
	}
	for {
		select {
		case <-socket.done:
			return
		case <-ticker.C:
		case <-expiry:
			expiry = nil
		}
		if code, reason := socket.verdict(); code != 0 {
			socket.end(code, reason)
			return
		}
		if socket.lagging() {
			socket.endSlowConsumer("its subscription dropped frames past nats.go's pending limits")
			return
		}
		if !socket.subscribed() {
			socket.end(websocket.CloseTryAgainLater, relaySocketReasonNATSUnavailable)
			return
		}
		socket.ping()
	}
}

// ping writes a ping to the client on a goroutine of its own, one at a time: gorilla/websocket
// writes it once a data frame being written has ended, which on a client that reads nothing is
// up to relaySocketWriteWait later. A ping that cannot be written within relaySocketWriteWait takes
// the client for gone and ends the socket, which drops the connection, unless the socket ended
// meanwhile.
func (socket *relaySocket) ping() {
	if !socket.pinging.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer socket.pinging.Store(false)
		if err := socket.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(relaySocketWriteWait)); err != nil && !socket.ended.Load() {
			socket.end(0, "")
		}
	}()
}

// subscribed reports whether every subscription the socket holds is still the server's.
func (socket *relaySocket) subscribed() bool {
	socket.subsMu.Lock()
	defer socket.subsMu.Unlock()
	for _, sub := range socket.subs {
		if !sub.IsValid() {
			return false
		}
	}
	return true
}

// verdict asks the broker about the socket's token again, the answer it gave reused for at most
// ciphertoken.AffirmativeCacheTTL and no proof checked a second time, and answers the close code
// and reason the socket ends with, 0 while it stays open:
//
//   - the token's expiry has passed: 4401, on which the client trades for a fresh token and
//     opens again at once. That is so whether the broker no longer holds it live or still
//     answers it live with an exp that has passed: the broker writes exp as the whole second at
//     or before the true expiry, and holds the token live until that true expiry, up to a second
//     later, which the expiry's timer (watch) would otherwise miss until the next tick;
//   - the broker no longer holds it live before its expiry (a revoke, a login ended early), or
//     holds it as something no session's token is: 4403;
//   - the answer names another caller than the one the socket was admitted as (its session, its
//     person or service, its project scope and its projects): 4403. The admitted caller passed the
//     edge's admission (EdgeAdmits) in apiAuth, which reads no field but these, so an answer equal
//     to it passes that admission too;
//   - the broker could not be asked: 1013, and every upgrade meanwhile is refused 503 by apiAuth.
func (socket *relaySocket) verdict() (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), relaySocketCheckWait)
	defer cancel()
	caller, until, err := socket.sockets.brokers.verifier.Load().LiveUntil(ctx, socket.check)
	var why string
	switch {
	case err == nil:
		if !until.IsZero() && !socket.sockets.now().Before(until) {
			return relaySocketCloseExpired, relaySocketReasonExpired
		}
		if sameRelayCaller(caller, socket.admitted) {
			return 0, ""
		}
		why = "the broker's answer names another caller than the socket was opened as"
	case errors.Is(err, ciphertoken.ErrNotActive):
		if !socket.expires.IsZero() && !socket.sockets.now().Before(socket.expires) {
			return relaySocketCloseExpired, relaySocketReasonExpired
		}
		why = err.Error()
	case errors.Is(err, ciphertoken.ErrNotASession), errors.Is(err, ciphertoken.ErrKeyMismatch):
		why = err.Error()
	default:
		socket.sockets.logger.Warn("listener: the relay socket could not ask the broker about its token; it is closed",
			slog.String("session_id", socket.session), slog.String("reason", brokerTokenReason(err)))
		return websocket.CloseTryAgainLater, relaySocketReasonBrokerUnavailable
	}
	socket.sockets.logger.Warn("listener: the relay socket's token no longer admits it; it is closed",
		slog.String("session_id", socket.session), slog.String("why", why))
	return relaySocketCloseRefused, relaySocketReasonRefused
}

// sameRelayCaller reports whether a later answer names the caller the socket was admitted as.
func sameRelayCaller(answer, admitted ciphertoken.Caller) bool {
	return answer.Session == admitted.Session && answer.Person == admitted.Person && answer.Service == admitted.Service &&
		answer.ProjectScope == admitted.ProjectScope && answer.ReadOnly == admitted.ReadOnly &&
		slices.Equal(answer.Projects, admitted.Projects)
}
