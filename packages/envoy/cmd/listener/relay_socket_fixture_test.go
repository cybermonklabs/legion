package main

// relay_socket_fixture_test.go is the relay socket's test plumbing, shared by its in-process rows
// (relay_socket_test.go) and its live rows (relay_socket_live_test.go): the in-process rig of two
// edge replicas, the session's socket client and the frames it decodes, and the helpers that
// publish a relay frame and read what a replica holds.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The edge project every row of this file's rig admits, and the second project a row names when
// it changes what a token lists.
const (
	relaySocketProject      = "PRIV"
	relaySocketOtherProject = "INFRA"
	relaySocketPerson       = "outside@example.com"
	// relaySocketTestCheck is how often a socket of this file's rig asks the broker again: a
	// tenth of a second, so a row that waits for a close waits for it within a second of the
	// answer that ends it.
	relaySocketTestCheck = 100 * time.Millisecond
)

// relaySocketClock is the rig's reading of now for the verifier's cache and for the socket's own
// test of whether a token's expiry has passed, so a row moves past the five seconds a live answer
// is reused for (ciphertoken.AffirmativeCacheTTL) without sleeping through them. A proof's own
// window is the library's, on the real clock, which is why this clock starts at the real one and
// only moves forward.
type relaySocketClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *relaySocketClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *relaySocketClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// relaySocketRig is two replicas of an edge deployment over the in-process server. The owner is
// the replica that holds the session's consumer and relays it, as an edge listener's relay runs:
// its receipts arrive under bus.RelayReplyPrefix. The socket is the other replica's: its /v1
// behind apiAuth with broker tokens on, on a NATS connection of its own, the broker the stub that
// answers what each row says each token is.
type relaySocketRig struct {
	owner   *relayRig
	client  *bus.Client
	sockets *relaySockets
	server  *httptest.Server
	broker  *ciphertokentest.StubBroker
	caller  *ciphertokentest.Caller
	clock   *relaySocketClock
	logs    *cmdtest.LockedBuffer
}

// relaySocketRigOption changes how newRelaySocketRig builds the socket's replica.
type relaySocketRigOption func(*relaySocketRigConfig)

type relaySocketRigConfig struct {
	// natsLatency is the round trip the socket replica's own connection is given (a
	// testnats.Proxy with ProxyLatency), so a row can tell a subscribe that reached the server
	// from one that has not yet; none by default.
	natsLatency time.Duration
	// checkEvery is the sockets' check interval, relaySocketTestCheck by default.
	checkEvery time.Duration
	// maxSockets is the replica's socket cap, relaySocketMaxPerListener by default.
	maxSockets int
	// pendingMsgs and pendingBytes are what one socket may hold for its client, the replica's
	// own by default.
	pendingMsgs, pendingBytes int
	// budget is what the replica's sockets together may hold, relaySocketReplicaPendingBytes by
	// default, and callerBudget what one caller's may, relaySocketCallerPendingBytes by default.
	budget, callerBudget int64
	// closeGrace is how long a closed socket waits for its close frame to be written,
	// relaySocketCloseGrace by default.
	closeGrace time.Duration
	// writeWait bounds one data frame's write and one ping's, relaySocketWriteWait by default.
	writeWait time.Duration
}

func withSocketNATSLatency(rtt time.Duration) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.natsLatency = rtt }
}

func withCheckEvery(every time.Duration) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.checkEvery = every }
}

func withMaxSockets(n int) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.maxSockets = n }
}

func withPendingLimits(msgs, bytes int) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.pendingMsgs, c.pendingBytes = msgs, bytes }
}

func withReplicaBudget(bytes int64) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.budget = bytes }
}

func withCallerBudget(bytes int64) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.callerBudget = bytes }
}

// withCloseGrace raises how long a closed socket waits for its close frame to be written
// (relaySockets.closeGrace, relaySocketCloseGrace by default), so a row reads the order of a close
// and a data frame on the wire with no deadline deciding it.
func withCloseGrace(grace time.Duration) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.closeGrace = grace }
}

// withWriteWait raises how long one data frame's write and one ping's may take
// (relaySockets.writeWait, relaySocketWriteWait by default), so a row that holds its client's
// reads decides nothing by that deadline.
func withWriteWait(wait time.Duration) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.writeWait = wait }
}

func newRelaySocketRig(t *testing.T, options ...relaySocketRigOption) *relaySocketRig {
	t.Helper()
	config := relaySocketRigConfig{checkEvery: relaySocketTestCheck}
	for _, option := range options {
		option(&config)
	}
	grant, err := newEdgeGrant("edge-a", []string{relaySocketProject}, metrics.New())
	if err != nil {
		t.Fatalf("the owner's edge grant: %v", err)
	}
	owner := newRelayRigUnownedAs(t, relayPauseFirst, relayPauseMax, grant)
	owner.relays.setOwned(true)

	url := owner.url
	if config.natsLatency > 0 {
		url = testnats.StartProxy(t, url, testnats.ProxyLatency(config.natsLatency)).URL()
	}
	client, err := bus.Connect([]string{url})
	if err != nil {
		t.Fatalf("connect the socket's replica: %v", err)
	}
	t.Cleanup(client.Close)

	broker := ciphertokentest.NewStubBroker(t)
	clock := &relaySocketClock{}
	tokens := newBrokerTokens(listenerBrokerConfig{
		BrokerURL:        broker.Server.URL,
		AccountTokenFile: "/var/run/secrets/broker/token",
		Origin:           listenerBrokerOrigin,
	})
	verifier, err := ciphertoken.New(ciphertoken.Options{
		BrokerURL: broker.Server.URL,
		Account:   listenerBrokerAccount("listener-account-token"),
		Origins:   []string{listenerBrokerOrigin},
		Replay:    &spentProofsInMemory{},
		Address:   listenerSourceAddress,
		Now:       clock.now,
	})
	if err != nil {
		t.Fatalf("build the broker-token verifier: %v", err)
	}
	tokens.verifier.Store(verifier)

	logs := &cmdtest.LockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the socket replica's log:\n%s", logs.String())
		}
	})
	logger := logging.NewWithWriter("edge-b", logs)
	sockets := newRelaySockets(client, tokens, logger)
	sockets.relaySocketCheckEvery = config.checkEvery
	if config.maxSockets > 0 {
		sockets.maxSockets = config.maxSockets
	}
	if config.pendingMsgs > 0 {
		sockets.pendingMsgs, sockets.pendingBytes = config.pendingMsgs, config.pendingBytes
	}
	if config.budget > 0 {
		sockets.budget = config.budget
	}
	if config.callerBudget > 0 {
		sockets.callerBudget = config.callerBudget
	}
	if config.closeGrace > 0 {
		sockets.closeGrace = config.closeGrace
	}
	if config.writeWait > 0 {
		sockets.writeWait = config.writeWait
	}
	sockets.now = clock.now
	routes := http.NewServeMux()
	registerV1Routes(routes, &listenerDeps{client: client, relaySockets: sockets}, "edge-b", logger)
	server := httptest.NewServer(apiAuth(listenerAPIAuth{brokers: tokens, logger: logger}, routes))
	t.Cleanup(server.Close)
	t.Cleanup(sockets.stop)
	return &relaySocketRig{
		owner: owner, client: client, sockets: sockets, server: server,
		broker: broker, caller: ciphertokentest.NewCaller(t), clock: clock, logs: logs,
	}
}

// relaySocketAnswer is the broker's answer about a token traded for session acting for the rig's
// person, listing projects and expiring after life: what an outside agent's access token is.
func relaySocketAnswer(session, jkt string, life time.Duration, projects ...string) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      session,
		Person:       relaySocketPerson,
		ProjectScope: ciphertoken.ProjectScopePerson,
		Projects:     projects,
		ExpiresAt:    time.Now().Add(life).Unix(),
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// relaySocketPath is session's relay socket.
func relaySocketPath(session string) string { return "/v1/sessions/" + session + "/relay" }

// upgradeHeader is the upgrade's credential: token under the DPoP scheme and a proof the rig's
// caller signed for a GET of session's socket at the listener's own origin.
func (rig *relaySocketRig) upgradeHeader(t *testing.T, session, token string) http.Header {
	t.Helper()
	signed := rig.caller.Signed(t, http.MethodGet, listenerBrokerOrigin+relaySocketPath(session), token)
	return http.Header{"Authorization": {signed.Header.Get("Authorization")}, "DPoP": {signed.Header.Get("DPoP")}}
}

// dialAs opens session's relay socket presenting header, offering the subprotocols named, and
// answers the socket (nil when the upgrade was refused) with the upgrade's response.
func (rig *relaySocketRig) dialAs(t *testing.T, session string, header http.Header, subprotocols ...string) (*relaySocketClient, *http.Response, error) {
	t.Helper()
	dialer := websocket.Dialer{Subprotocols: subprotocols, HandshakeTimeout: 5 * time.Second}
	conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(rig.server.URL, "http")+relaySocketPath(session), header)
	if err != nil {
		return nil, response, err
	}
	return startRelaySocketClient(t, conn), response, nil
}

// open is a socket session opened with a fresh token the broker holds live for an hour, listing
// the rig's project, which it fails the test unless the upgrade admits.
func (rig *relaySocketRig) open(t *testing.T, session string) (*relaySocketClient, string) {
	t.Helper()
	return rig.openAs(t, relaySocketPerson, session)
}

// openAs is open for a session of person.
func (rig *relaySocketRig) openAs(t *testing.T, person, session string) (*relaySocketClient, string) {
	t.Helper()
	token := "access-token-of-" + session + "-" + t.Name()
	answer := relaySocketAnswer(session, rig.caller.JKT(), time.Hour, relaySocketProject)
	answer.Person = person
	rig.broker.Says(token, answer)
	return rig.openWith(t, session, token), token
}

// openWith opens session's socket with token, failing the test unless the upgrade admits it.
func (rig *relaySocketRig) openWith(t *testing.T, session, token string) *relaySocketClient {
	t.Helper()
	client, response, err := rig.dialAs(t, session, rig.upgradeHeader(t, session, token), relaySubprotocol)
	if err != nil {
		t.Fatalf("open the relay socket of %s: %v (%s)\n%s", session, err, describeUpgrade(response), rig.logs.String())
	}
	if got := client.conn.Subprotocol(); got != relaySubprotocol {
		t.Fatalf("the socket was opened with the subprotocol %q, want %q", got, relaySubprotocol)
	}
	return client
}

// revoke has the broker hold token live no longer, as a revoke does, and moves the rig's clock past
// the time its last live answer is reused for (ciphertoken.AffirmativeCacheTTL).
func (rig *relaySocketRig) revoke(token string) {
	rig.broker.Says(token, ciphertoken.Introspection{})
	rig.clock.advance(ciphertoken.AffirmativeCacheTTL + time.Second)
}

// describeUpgrade is an upgrade's refusal as a failure names it: its status and its body.
func describeUpgrade(response *http.Response) string {
	if response == nil {
		return "no response"
	}
	body := &bytes.Buffer{}
	if response.Body != nil {
		_, _ = body.ReadFrom(response.Body)
	}
	return response.Status + " " + strings.TrimSpace(body.String())
}

// relayFrameRead is one frame the server sent a socket client, decoded as the wire spells it
// (relay_socket.go).
type relayFrameRead struct {
	Op      string              `json:"op"`
	SID     *int64              `json:"sid"`
	Subject string              `json:"subject"`
	Reply   string              `json:"reply"`
	Headers map[string][]string `json:"headers"`
	Data    string              `json:"data"`
	Code    string              `json:"code"`
	// raw is the frame's own text, so a row can read a field's presence and not only its value.
	raw string
}

// relaySocketClient is a session's end of a relay socket: one goroutine reads every frame the
// server sends, answering the server's pings as gorilla's client does, and hands each frame, each
// pong and the close the server ends with to the row.
type relaySocketClient struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan relayFrameRead
	pongs  chan time.Time
	// closed carries the close the read ended with: the server's close frame, or the error that
	// ended the read without one.
	closed  chan error
	writeMu sync.Mutex
}

func startRelaySocketClient(t *testing.T, conn *websocket.Conn) *relaySocketClient {
	t.Helper()
	client := &relaySocketClient{t: t, conn: conn, frames: make(chan relayFrameRead, 64), pongs: make(chan time.Time, 8), closed: make(chan error, 1)}
	conn.SetPongHandler(func(string) error {
		client.pongs <- time.Now()
		return nil
	})
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				client.closed <- err
				return
			}
			frame := relayFrameRead{raw: string(data)}
			if err := json.Unmarshal(data, &frame); err != nil {
				client.closed <- err
				return
			}
			client.frames <- frame
		}
	}()
	return client
}

// send writes one client frame.
func (c *relaySocketClient) send(frame map[string]any) {
	c.t.Helper()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.WriteJSON(frame); err != nil {
		c.t.Fatalf("send %v: %v", frame, err)
	}
}

// sendRaw writes one WebSocket message of kind holding data as it is, which may be no frame the
// wire reads.
func (c *relaySocketClient) sendRaw(kind int, data []byte) {
	c.t.Helper()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.WriteMessage(kind, data); err != nil {
		c.t.Fatalf("send %s: %v", data, err)
	}
}

func (c *relaySocketClient) sub(sid int, subject string) {
	c.t.Helper()
	c.send(map[string]any{"op": "sub", "sid": sid, "subject": subject})
}

// subscribeOwn subscribes session's own relay subject as sid 1, as the client does, and returns
// once a ping sent after the sub has its pong: once the replica holds the subscription.
func (c *relaySocketClient) subscribeOwn(session string) {
	c.t.Helper()
	c.sub(1, bus.DeliverSubject(session))
	c.ping()
	<-c.pongs
}

func (c *relaySocketClient) pub(subject, data string) {
	c.t.Helper()
	c.send(map[string]any{"op": "pub", "subject": subject, "data": base64.StdEncoding.EncodeToString([]byte(data))})
}

// ping sends a ping, as the client's flush() does.
func (c *relaySocketClient) ping() {
	c.t.Helper()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
		c.t.Fatalf("send a ping: %v", err)
	}
}

// next is the next frame within the bound, failing the test when none arrives. A frame the reader
// queued before the close came first on the wire, and is the next frame: the reader puts each frame
// on frames before it reads on, so once the close is on closed, every frame before it is already on
// frames. A select over both would pick either at random when both are ready - under load, where
// the reader queues the server's last frame and its close before the row asks - and report a frame
// that did arrive as missing; so next takes a queued frame first, and on the close looks at frames
// once more before it fails, putting the close back for closeCode.
func (c *relaySocketClient) next(within time.Duration) relayFrameRead {
	c.t.Helper()
	select {
	case frame := <-c.frames:
		return frame
	default:
	}
	select {
	case frame := <-c.frames:
		return frame
	case err := <-c.closed:
		select {
		case frame := <-c.frames:
			c.closed <- err
			return frame
		default:
		}
		c.t.Fatalf("the socket closed (%v) before a frame arrived", err)
	case <-time.After(within):
		c.t.Fatalf("no frame arrived within %s", within)
	}
	return relayFrameRead{}
}

// closeQueued waits for the reader to have read the server's close, after every frame before it:
// the state in which next must still answer the frames first. A row calls it to stand where load
// puts it. It sets no span of its own: a close that load holds back is still a close, so the wait
// ends when the close is queued or gives up 30 s before the test binary's deadline (testwait.Until).
func (c *relaySocketClient) closeQueued() {
	c.t.Helper()
	testwait.Until(c.t, "the reader to read the server's close", func() (int, bool) {
		queued := len(c.closed)
		return queued, queued == 1
	})
}

// none fails the test when a frame arrives within the window.
func (c *relaySocketClient) none(window time.Duration, what string) {
	c.t.Helper()
	select {
	case frame := <-c.frames:
		c.t.Fatalf("%s: the socket sent %s within %s, want nothing", what, frame.raw, window)
	case <-time.After(window):
	}
}

// closeCode waits up to within for the server to close the socket and answers its close frame's
// code and text, failing the test when the socket stays open or ends without a close frame.
func (c *relaySocketClient) closeCode(within time.Duration) (int, string) {
	c.t.Helper()
	select {
	case err := <-c.closed:
		var closed *websocket.CloseError
		if !errors.As(err, &closed) {
			c.t.Fatalf("the socket ended with %v, not a close frame", err)
		}
		return closed.Code, closed.Text
	case frame := <-c.frames:
		c.t.Fatalf("the socket sent %s, want it closed", frame.raw)
	case <-time.After(within):
		c.t.Fatalf("the socket was still open %s later", within)
	}
	return 0, ""
}

// open fails the test when the socket closes within the window.
func (c *relaySocketClient) stillOpen(window time.Duration, what string) {
	c.t.Helper()
	select {
	case err := <-c.closed:
		c.t.Fatalf("%s: the socket closed within %s (%v), want it open", what, window, err)
	case <-time.After(window):
	}
}

// sid is a frame's sid, failing the test when it names none.
func (f relayFrameRead) sid(t *testing.T) int64 {
	t.Helper()
	if f.SID == nil {
		t.Fatalf("the frame %s names no sid", f.raw)
	}
	return *f.SID
}

// data is a msg frame's data, decoded.
func (f relayFrameRead) data(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(f.Data)
	if err != nil {
		t.Fatalf("the frame %s carries data that is not base64: %v", f.raw, err)
	}
	return data
}

// openSocket is the socket the rig's replica holds open for session once its upgrade is attached,
// failing the test when none is within 5 s.
func (rig *relaySocketRig) openSocket(t *testing.T, session string) *relaySocket {
	t.Helper()
	var found *relaySocket
	if !testwait.Holds(5*time.Second, 10*time.Millisecond, func() bool {
		rig.sockets.mu.Lock()
		defer rig.sockets.mu.Unlock()
		for socket := range rig.sockets.open[session] {
			if socket.conn != nil {
				found = socket
				return true
			}
		}
		return false
	}) {
		t.Fatalf("the replica holds no open socket of %s", session)
	}
	return found
}

// awaitSubscription is the subscription the socket holds once its client's sub is taken, failing
// the test when it holds none within 5 s.
func (socket *relaySocket) awaitSubscription(t *testing.T) *natsgo.Subscription {
	t.Helper()
	var held *natsgo.Subscription
	if !testwait.Holds(5*time.Second, 10*time.Millisecond, func() bool {
		socket.subsMu.Lock()
		defer socket.subsMu.Unlock()
		for _, sub := range socket.subs {
			held = sub
			return true
		}
		return false
	}) {
		t.Fatalf("the socket of %s took no subscription", socket.session)
	}
	return held
}

// holds is how many sockets of session the rig's replica holds open.
func (rig *relaySocketRig) holds(session string) int {
	rig.sockets.mu.Lock()
	defer rig.sockets.mu.Unlock()
	return len(rig.sockets.open[session])
}

// callerRecords is how many callers the rig's replica keeps a record of (relaySockets.callers).
func (rig *relaySocketRig) callerRecords() int {
	rig.sockets.mu.Lock()
	defer rig.sockets.mu.Unlock()
	return len(rig.sockets.callers)
}

// relayFrameReceipt is a receipt subject of a relay frame of session, shaped as an edge relay
// names one (relayReceiptOf): the receipt family, a machine id, a relay's nuid, the session and a
// token of its own.
func relayFrameReceipt(session string) string {
	return bus.SessionReplySubject(bus.RelayReplyPrefix+"edge-rows.rows", session)
}

// publishRelayFrame publishes, on session's relay subject, a message shaped as a relay frame -
// its receipt subject a receipt of session's (relayFrameReceipt) - carrying header and the relay's
// Envoy-Subject, set on header itself. The subject and the envelope are an event of
// relaySocketProject, which the edge's rule (edgeFrameVerdict) passes to a socket of any session
// whose token lists it, so a row that holds a frame away from a socket holds it there by the
// socket's subscriptions alone.
func publishRelayFrame(t *testing.T, url, session string, header natsgo.Header) *natsgo.Msg {
	t.Helper()
	conn := testnats.Connect(t, url)
	if header == nil {
		header = natsgo.Header{}
	}
	subject := "notifications.dispatch.issue." + relaySocketProject + "." + relaySocketProject + "-1.comment.created"
	header.Set(relayHeaderSubject, subject)
	msg := &natsgo.Msg{
		Subject: bus.DeliverSubject(session),
		Reply:   relayFrameReceipt(session),
		Header:  header,
		Data:    marshalListenerEnvelope(t, listenerTestEnvelope(subject, "row-"+t.Name())),
	}
	if err := conn.PublishMsg(msg); err != nil {
		t.Fatalf("publish a frame for %s: %v", session, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the frame for %s: %v", session, err)
	}
	return msg
}

// dialStalled opens session's socket with token on a client that reads nothing, subscribes the
// session's relay subject as sid 1, and answers the client's connection and the replica's socket
// once the replica holds its subscription.
func (rig *relaySocketRig) dialStalled(t *testing.T, session, token string) (*websocket.Conn, *relaySocket) {
	t.Helper()
	return rig.dialSubscribed(t, session, token, &websocket.Dialer{Subprotocols: []string{relaySubprotocol}, HandshakeTimeout: 5 * time.Second})
}

// dialSubscribed opens session's socket with token through dialer, which the row reads itself,
// subscribes the session's relay subject as sid 1, and answers the client's connection and the
// replica's socket once the replica holds its subscription.
func (rig *relaySocketRig) dialSubscribed(t *testing.T, session, token string, dialer *websocket.Dialer) (*websocket.Conn, *relaySocket) {
	t.Helper()
	conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(rig.server.URL, "http")+relaySocketPath(session), rig.upgradeHeader(t, session, token))
	if err != nil {
		t.Fatalf("open the socket of %s: %v (%s)", session, err, describeUpgrade(response))
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]any{"op": "sub", "sid": 1, "subject": bus.DeliverSubject(session)}); err != nil {
		t.Fatalf("send the sub: %v", err)
	}
	socket := rig.openSocket(t, session)
	socket.awaitSubscription(t)
	return conn, socket
}

// heldConn is a client's connection that counts what it has read and, when resume is set before
// the reader starts, holds the read after its first successful one (counted) until resume closes:
// a client that has read the first bytes of a frame and reads no more until the row lets it. Only
// the reader consumes resume, and clears it once it closes; after that the client reads at once.
type heldConn struct {
	net.Conn
	read   atomic.Int64
	resume <-chan struct{}
}

func (c *heldConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	if n > 0 && c.resume != nil {
		<-c.resume
		c.resume = nil
	}
	return n, err
}

// dialHeldReader opens session's socket with token on a client whose reads the row holds
// (heldConn), through a connection whose buffers hold the least the kernel allows (the client's
// receive buffer, set before it connects, and the socket's send buffer, set before any frame is
// written), so a data frame the socket writes while the client is held blocks in its write. It
// subscribes the session's relay subject as sid 1 (dialSubscribed) and answers the client's
// connection, its heldConn and the replica's socket once the replica holds its subscription.
func (rig *relaySocketRig) dialHeldReader(t *testing.T, session, token string) (*websocket.Conn, *heldConn, *relaySocket) {
	t.Helper()
	smallReceiveBuffer := func(_, _ string, raw syscall.RawConn) error {
		var setErr error
		if err := raw.Control(func(fd uintptr) {
			setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1)
		}); err != nil {
			return err
		}
		return setErr
	}
	var held *heldConn
	dialer := &websocket.Dialer{
		Subprotocols:     []string{relaySubprotocol},
		HandshakeTimeout: 5 * time.Second,
		NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{Control: smallReceiveBuffer}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			held = &heldConn{Conn: conn}
			return held, nil
		},
	}
	conn, socket := rig.dialSubscribed(t, session, token, dialer)
	server, ok := socket.conn.NetConn().(*net.TCPConn)
	if !ok {
		t.Fatalf("the socket's connection is %T, not a TCP connection whose send buffer the row can set", socket.conn.NetConn())
	}
	if err := server.SetWriteBuffer(1); err != nil {
		t.Fatalf("set the socket's send buffer: %v", err)
	}
	return conn, held, socket
}

// awaitControlWriteWaiting returns once a goroutine of the process waits in gorilla/websocket's
// WriteControl for its connection's write lock (blocked in its select), bounded only by the test
// binary's deadline: a close or a ping is then queued for that lock, so it is handed the lock the
// moment the write holding it lets go (a send on a channel a receiver waits on passes to that
// receiver). The row's socket must be the only one of the process with a control frame to write.
func awaitControlWriteWaiting(t *testing.T, what string) {
	t.Helper()
	testwait.Until(t, what, func() (string, bool) {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		for n == len(buf) {
			buf = make([]byte, 2*len(buf))
			n = runtime.Stack(buf, true)
		}
		for _, goroutine := range strings.Split(string(buf[:n]), "\n\n") {
			header, _, _ := strings.Cut(goroutine, "\n")
			if strings.Contains(header, "[select") && strings.Contains(goroutine, "gorilla/websocket.(*Conn).WriteControl(") {
				return header, true
			}
		}
		return "no goroutine waits in WriteControl", false
	})
}

// flood publishes n relay frames of 512 KiB on session's relay subject, far more than a
// connection's buffers hold when n is in the tens, and returns once the server holds them. Each
// names an event of relaySocketProject as its stored subject, which the edge's rule passes
// (edgeFrameVerdict), so each is written to the socket.
func flood(t *testing.T, url, session string, n int) {
	t.Helper()
	publisher := testnats.Connect(t, url)
	data := bytes.Repeat([]byte("x"), 512<<10)
	header := natsgo.Header{relayHeaderSubject: {"notifications.dispatch.issue." + relaySocketProject + "." + relaySocketProject + "-1.comment.created"}}
	for i := range n {
		if err := publisher.PublishMsg(&natsgo.Msg{Subject: bus.DeliverSubject(session), Reply: relayFrameReceipt(session), Header: header, Data: data}); err != nil {
			t.Fatalf("publish frame %d: %v", i, err)
		}
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush the frames: %v", err)
	}
}

// awaitBlockedWrite returns once socket holds a frame behind the one it is writing to its
// client, failing the test when it never does within 10 s.
func awaitBlockedWrite(t *testing.T, socket *relaySocket) {
	t.Helper()
	if !testwait.Holds(10*time.Second, 10*time.Millisecond, func() bool {
		frames, _ := socket.held()
		return frames > 1
	}) {
		t.Fatal("the socket never held a frame behind the one it was writing")
	}
}
