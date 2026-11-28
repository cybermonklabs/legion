package main

// relay_socket_fixture_test.go is the relay socket's test plumbing, shared by its in-process rows
// (relay_socket_test.go, relay_socket_reconnect_test.go) and its live rows
// (relay_socket_live_test.go): the in-process rig of two edge replicas, the session's socket
// client and the frames it decodes, and the helpers that publish a relay frame and read what a
// replica holds.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
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
	// relaySocketTestCheck is how often a socket of this file's rig checks its token, on the rig's
	// clock: the step a row moves that clock by from one check to the next, a fiftieth of the five
	// seconds a live answer is reused for (ciphertoken.AffirmativeCacheTTL).
	relaySocketTestCheck = 100 * time.Millisecond
	// relaySocketPastTheBinary is a span past any deadline go test gives a test binary, for a check
	// interval, a pong floor or a token's life that a row must never meet on its own.
	relaySocketPastTheBinary = 24 * time.Hour
)

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
	// clock is the rig's fake clock, which the socket replica's verifier reads (Options.Now: its
	// cache of the broker's answers) and its sockets run on (relaySockets.clock: each socket's
	// check, its token's expiry, its close's grace and its receipt window), so one move of it moves
	// both: a row moves past the five seconds a live answer is reused for
	// (ciphertoken.AffirmativeCacheTTL), and through a socket's checks, without sleeping through
	// either. It is clocktest.New, which stands still until the row moves it, and not
	// clocktest.Held, whose Now goes on with the wall clock: a socket's watch reads the clock and
	// then arms its expiry's timer for the time left to the token's exp, and each check one interval
	// past the last, and only a clock that stood still between the reading and the arm arms them at
	// the exp and the check's grid moment exactly, which a row waits for (AwaitArmedAt). It starts at
	// the real now, since the broker's answers date their exp on the real clock
	// (relaySocketAnswer), and the proof an upgrade presents is judged on the real clock by the
	// library, which Options.Now does not reach.
	clock *clocktest.Fake
	logs  *cmdtest.LockedBuffer
	// proxy is the socket replica's way to the server when a row asked for one
	// (withSocketNATSLatency, withSocketNATSProxy), nil otherwise: a row cuts it to make that
	// replica's connection reconnect.
	proxy *testnats.Proxy
}

// relaySocketRigOption changes how newRelaySocketRig builds the socket's replica.
type relaySocketRigOption func(*relaySocketRigConfig)

type relaySocketRigConfig struct {
	// natsLatency is the round trip the socket replica's own connection is given (a
	// testnats.Proxy with ProxyLatency), so a row can tell a subscribe that reached the server
	// from one that has not yet; none by default.
	natsLatency time.Duration
	// proxied puts the socket replica's connection behind a proxy with no added latency
	// (withSocketNATSProxy), which a row can cut.
	proxied bool
	// checkEvery is the sockets' check interval, relaySocketTestCheck by default.
	checkEvery time.Duration
	// pongFloor is the least the socket's reader waits for a frame or a pong,
	// relaySocketPongFloor by default.
	pongFloor time.Duration
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
	// held, when set, is the gate every connection the replica's server accepts writes through
	// (heldWrites), which a row holds to hold a socket's writer inside a write.
	held *heldWrites
}

func withSocketNATSLatency(rtt time.Duration) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.natsLatency = rtt }
}

func withSocketNATSProxy() relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.proxied = true }
}

func withCheckEvery(every time.Duration) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.checkEvery = every }
}

// withPongFloor raises the least the socket's reader waits for a frame or a pong
// (relaySockets.pongFloor, relaySocketPongFloor by default), so a row that drives a socket's checks
// on the rig's clock at its own pace, with real time passing between two of them, is never cut off
// by the reader's deadline, which is on the real clock; the check interval the row exercises stays
// as it is.
func withPongFloor(floor time.Duration) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.pongFloor = floor }
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

// withHeldWrites has every connection the socket's replica accepts write through held's gate, so a
// row holds a socket's writer inside a write for as long as the row takes (heldWrites).
func withHeldWrites(held *heldWrites) relaySocketRigOption {
	return func(c *relaySocketRigConfig) { c.held = held }
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
	var proxy *testnats.Proxy
	switch {
	case config.natsLatency > 0:
		proxy = testnats.StartProxy(t, url, testnats.ProxyLatency(config.natsLatency))
		url = proxy.URL()
	case config.proxied:
		proxy = testnats.StartProxy(t, url)
		url = proxy.URL()
	}
	client, err := bus.Connect([]string{url})
	if err != nil {
		t.Fatalf("connect the socket's replica: %v", err)
	}
	t.Cleanup(client.Close)

	broker := ciphertokentest.NewStubBroker(t)
	clock := clocktest.New(time.Now())
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
		Now:       clock.Now,
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
	if config.pongFloor > 0 {
		sockets.pongFloor = config.pongFloor
	}
	sockets.clock = clock
	routes := http.NewServeMux()
	registerV1Routes(routes, &listenerDeps{client: client, relaySockets: sockets}, "edge-b", logger)
	server := httptest.NewUnstartedServer(apiAuth(listenerAPIAuth{brokers: tokens, logger: logger}, routes))
	if config.held != nil {
		server.Listener = config.held.listen(server.Listener)
	}
	server.Start()
	t.Cleanup(server.Close)
	t.Cleanup(sockets.stop)
	return &relaySocketRig{
		owner: owner, client: client, sockets: sockets, server: server,
		broker: broker, caller: ciphertokentest.NewCaller(t), clock: clock, logs: logs, proxy: proxy,
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
// the time its last live answer is reused for (ciphertoken.AffirmativeCacheTTL). The sockets run on
// the same clock, so the move also runs a socket's check that falls due in it: with the rig's own
// check interval, the check that reads the revoke.
func (rig *relaySocketRig) revoke(token string) {
	rig.broker.Says(token, ciphertoken.Introspection{})
	rig.clock.Advance(ciphertoken.AffirmativeCacheTTL + time.Second)
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
// server sends, answering the server's pings as gorilla's client does and counting them, and hands
// each frame, each pong and the close the server ends with to the row.
type relaySocketClient struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan relayFrameRead
	pongs  chan time.Time
	// pinged counts the server's pings the reader has read. The reader reads the connection in
	// order, so a frame the server sent before a ping is on frames by the time pinged counts it.
	pinged atomic.Int64
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
	conn.SetPingHandler(func(data string) error {
		client.pinged.Add(1)
		// The answer gorilla's own ping handler gives: a pong, and nothing ended by a pong that
		// could not be written to a connection already closing.
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
		var netErr net.Error
		if errors.Is(err, websocket.ErrCloseSent) || errors.As(err, &netErr) {
			return nil
		}
		return err
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

// frame waits for the next frame the server sends, failing the test, naming what it waited for,
// when the socket closes before one arrives. A frame the reader queued before the close came first
// on the wire, and is the next frame: the reader puts each frame on frames before it reads on, so
// once the close is on closed, every frame before it is already on frames. frame takes a queued
// frame first, and on the close looks at frames once more before it fails, putting the close back
// for closeNext. It waits through testwait, with no bound of its own.
func (c *relaySocketClient) frame(what string) relayFrameRead {
	c.t.Helper()
	var got relayFrameRead
	testwait.Until(c.t, what, func() (string, bool) {
		select {
		case got = <-c.frames:
			return got.raw, true
		default:
		}
		select {
		case err := <-c.closed:
			select {
			case got = <-c.frames:
				c.closed <- err
				return got.raw, true
			default:
			}
			c.t.Fatalf("%s: the socket closed (%v) before a frame arrived", what, err)
		default:
		}
		return "no frame and no close yet", false
	})
	return got
}

// closeQueued waits for the reader to have read the server's close, after every frame before it:
// the state in which frame must still answer the frames first. A row calls it to stand where load
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

// closeNext waits for the client's next event to be the server's close and returns its code and
// text, failing the test, naming what it waited for, when that event is a data frame or the socket
// ends with no close frame: nothing may come before the close. A frame the reader queued before the
// close is that event, as frame reads it. It waits through testwait, with no bound of its own.
func (c *relaySocketClient) closeNext(what string) (int, string) {
	c.t.Helper()
	var closed *websocket.CloseError
	testwait.Until(c.t, what, func() (string, bool) {
		select {
		case frame := <-c.frames:
			c.t.Fatalf("%s: the socket sent %s, want its close next", what, frame.raw)
		default:
		}
		select {
		case err := <-c.closed:
			select {
			case frame := <-c.frames:
				c.t.Fatalf("%s: the socket sent %s before its close (%v), want the close next", what, frame.raw, err)
			default:
			}
			if !errors.As(err, &closed) {
				c.t.Fatalf("%s: the socket ended with %v, not a close frame", what, err)
			}
			return closed.Error(), true
		default:
		}
		return "the socket still open", false
	})
	return closed.Code, closed.Text
}

// closeAfterFrames waits for the server's close, passing over the data frames it sends before it,
// and returns the close's code and text, failing the test, naming what it waited for, when the
// socket ends with no close frame. It is closeNext for a row whose socket may rightly send frames
// before its close, as requireClosed's callers (relay_socket_live_test.go) read theirs. It waits
// through testwait, with no bound of its own.
func (c *relaySocketClient) closeAfterFrames(what string) (int, string) {
	c.t.Helper()
	var closed *websocket.CloseError
	passed := 0
	testwait.Until(c.t, what, func() (string, bool) {
	drain:
		for {
			select {
			case <-c.frames:
				passed++
			default:
				break drain
			}
		}
		select {
		case err := <-c.closed:
			if !errors.As(err, &closed) {
				c.t.Fatalf("%s: the socket ended with %v after %d frames, not a close frame", what, err, passed)
			}
			return closed.Error(), true
		default:
		}
		return fmt.Sprintf("the socket still open after %d frames", passed), false
	})
	return closed.Code, closed.Text
}

// requireNothingYet fails the test, naming what, when the client holds a data frame or the
// socket's close: what the server sent before a ping the client has counted (pinged) is on frames,
// or on closed, by then.
func (c *relaySocketClient) requireNothingYet(what string) {
	c.t.Helper()
	select {
	case frame := <-c.frames:
		c.t.Fatalf("%s: the socket sent %s, want nothing", what, frame.raw)
	default:
	}
	select {
	case err := <-c.closed:
		c.t.Fatalf("%s: the socket closed (%v), want it open", what, err)
	default:
	}
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
	if !testwait.Holds(listenerWaitBound(t), 10*time.Millisecond, func() bool {
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
	if !testwait.Holds(listenerWaitBound(t), 10*time.Millisecond, func() bool {
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

// socketChecks drives one open socket's checks on its rig's clock, one check a move. A socket's
// watch arms its next check one check interval past the one before it, on a grid that starts where
// the clock stood when the watch began, and only once the check before it is done (clock.Ticker);
// so a row moves the clock to the next check only once that check is armed and the ping of the one
// before it is written, and a move to it runs that one check.
type socketChecks struct {
	rig    *relaySocketRig
	client *relaySocketClient
	socket *relaySocket
	// origin is where the clock stood when the socket's watch began, and next the moment of the
	// socket's next check.
	origin, next time.Time
}

// checksOf is the checks of session's socket, whose client is client. The row calls it before it
// moves the clock after the upgrade: the watch begins its grid wherever the clock stands then, and
// the rig's clock stands still until a row moves it.
func (rig *relaySocketRig) checksOf(t *testing.T, client *relaySocketClient, session string) *socketChecks {
	t.Helper()
	origin := rig.clock.Now()
	return &socketChecks{rig: rig, client: client, socket: rig.openSocket(t, session), origin: origin, next: origin.Add(rig.sockets.relaySocketCheckEvery)}
}

// atOrAfter is the socket's first check at or after at.
func (c *socketChecks) atOrAfter(at time.Time) time.Time {
	interval := c.rig.sockets.relaySocketCheckEvery
	return c.origin.Add((at.Sub(c.origin) + interval - 1) / interval * interval)
}

// ready waits until the socket's watch is between two checks, its next check armed and the ping of
// the check before it written (pinging clear: a check while a ping is still being written sends
// none), and returns how many pings the client has counted then. Every ping the server wrote is
// counted by then: the client sends a ping of its own once the server's is written, and its pong
// comes after the server's ping on the wire, which the client's reader counts before it hands the
// pong over.
func (c *socketChecks) ready(t *testing.T, what string) int64 {
	t.Helper()
	c.rig.clock.AwaitArmedAt(t, what+": the socket's next check", c.next)
	testwait.Until(t, what+": the last check's ping to be written", func() (bool, bool) {
		pinging := c.socket.pinging.Load()
		return pinging, !pinging
	})
	c.client.ping()
	testwait.Receive(t, what+": the pong of the client's own ping, behind any ping the server wrote", c.client.pongs)
	return c.client.pinged.Load()
}

// move moves the clock to the socket's next check.
func (c *socketChecks) move() time.Time {
	at := c.next
	c.rig.clock.Set(at)
	c.next = at.Add(c.rig.sockets.relaySocketCheckEvery)
	return at
}

// keep runs the socket's next check and waits for the ping its verdict sends when it keeps the
// socket, failing the row on a data frame or a close before that ping.
func (c *socketChecks) keep(t *testing.T, what string) {
	t.Helper()
	pinged := c.ready(t, what)
	c.awaitPing(t, what, c.move(), pinged)
}

// keepFresh is keep for a check that must take the broker's answer afresh: it waits for the
// broker's count of the introspections it answered to pass the count read before the move, the
// check having asked it, and then for that check's ping. A ping of a check made on a cached answer
// before the move cannot stand for it, since ready counted every ping the server wrote before it.
func (c *socketChecks) keepFresh(t *testing.T, what string) {
	t.Helper()
	pinged := c.ready(t, what)
	asked := c.rig.broker.Count()
	at := c.move()
	testwait.Until(t, what+": the check at "+at.Format(time.RFC3339Nano)+" to ask the broker afresh", func() (int64, bool) {
		count := c.rig.broker.Count()
		return count, count > asked
	})
	c.awaitPing(t, what, at, pinged)
}

// awaitPing waits for the client to count a ping past pinged, the one the check at at sends,
// failing the row, naming what, on a data frame or a close the client holds: a frame or a close the
// server sent before that ping is the client's by the time it counts the ping.
func (c *socketChecks) awaitPing(t *testing.T, what string, at time.Time, pinged int64) {
	t.Helper()
	testwait.Until(t, what+": the ping of the check at "+at.Format(time.RFC3339Nano), func() (int64, bool) {
		count := c.client.pinged.Load()
		c.client.requireNothingYet(what)
		return count, count > pinged
	})
}

// skipTo moves the clock to the socket's last check before at, the checks between reading whatever
// answer they read (the cached one, in the rows that call it), so the socket's next check is its
// first at or after at.
func (c *socketChecks) skipTo(t *testing.T, at time.Time, what string) {
	t.Helper()
	interval := c.rig.sockets.relaySocketCheckEvery
	last := c.origin.Add((at.Sub(c.origin) - 1) / interval * interval)
	if last.Before(c.next) {
		return
	}
	c.ready(t, what)
	c.rig.clock.Set(last)
	c.next = last.Add(interval)
}

// closeAt moves the clock to at, where a check of the socket is armed (its next check, or the
// check at its token's expiry, at or before the next, which skipTo leaves the row short of), and
// leaves it there: the socket's close must come from that one check, with nothing before it
// (closeNext), and the close's code and text are returned.
func (c *socketChecks) closeAt(t *testing.T, at time.Time, what string) (int, string) {
	t.Helper()
	c.ready(t, what)
	c.rig.clock.AwaitArmedAt(t, what+": the socket's check", at)
	c.rig.clock.Set(at)
	return c.client.closeNext(what)
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

// countedConn is a client's connection that counts what it has read.
type countedConn struct {
	net.Conn
	read atomic.Int64
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// dialCounted opens session's socket with token on a client that counts what it reads
// (countedConn), subscribes the session's relay subject as sid 1 (dialSubscribed), and answers the
// client's connection, its countedConn and the replica's socket once the replica holds its
// subscription.
func (rig *relaySocketRig) dialCounted(t *testing.T, session, token string) (*websocket.Conn, *countedConn, *relaySocket) {
	t.Helper()
	var counted *countedConn
	dialer := &websocket.Dialer{
		Subprotocols:     []string{relaySubprotocol},
		HandshakeTimeout: 5 * time.Second,
		NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			counted = &countedConn{Conn: conn}
			return counted, nil
		},
	}
	conn, socket := rig.dialSubscribed(t, session, token, dialer)
	return conn, counted, socket
}

// heldWrites wraps the socket replica's listener (withHeldWrites) so a row can hold a socket's
// writer inside a write, as a client that reads nothing holds it, and know that it stays held until
// the row lets it go: the shape of internal/dispatch/api's heldWrites. gorilla/websocket holds its
// connection's write lock for the whole of each write to the connection, so a writer held at the
// gate holds that lock, and a close or a ping (WriteControl) waits for it. Holding the writer with
// TCP backpressure instead (a client that reads nothing, through the least buffers the kernel
// allows) holds the lock only while the kernel blocks a write: between two fragments of a frame the
// writer holds no lock, and a close or a ping that comes then takes it at once.
type heldWrites struct {
	mu sync.Mutex
	// gate is open (nil) or, while the row holds writes, the channel release closes.
	gate chan struct{}
	// waiting counts the writes waiting at the gate.
	waiting atomic.Int32
}

// listen is newRelaySocketRig's: every connection the server accepts writes through the gate.
func (held *heldWrites) listen(listener net.Listener) net.Listener {
	return heldListener{Listener: listener, held: held}
}

// hold makes every write the server starts from now on wait at the gate until release, however
// long the row takes to release it. The row's cleanup releases it, ahead of the rig's, so no
// socket's writer is left waiting at the gate.
func (held *heldWrites) hold(t *testing.T) {
	t.Helper()
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.gate == nil {
		held.gate = make(chan struct{})
	}
	t.Cleanup(held.release)
}

// release lets every waiting write through and opens the gate.
func (held *heldWrites) release() {
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.gate != nil {
		close(held.gate)
		held.gate = nil
	}
}

// waitHeld returns once a write waits at the gate, bounded only by the test binary's deadline: from
// then on its writer holds the connection's write lock until release.
func (held *heldWrites) waitHeld(t *testing.T, what string) {
	t.Helper()
	testwait.Until(t, what, func() (int32, bool) {
		waiting := held.waiting.Load()
		return waiting, waiting > 0
	})
}

func (held *heldWrites) current() chan struct{} {
	held.mu.Lock()
	defer held.mu.Unlock()
	return held.gate
}

type heldListener struct {
	net.Listener
	held *heldWrites
}

func (l heldListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return conn, err
	}
	return &heldConn{Conn: conn, held: l.held}, nil
}

// heldConn is one accepted connection whose writes wait at the gate. A write waits there for the
// row's release alone: the write deadline the writer set moves on by the time the write waited, so
// a release later than that deadline still lets the write through rather than failing it.
type heldConn struct {
	net.Conn
	held     *heldWrites
	mu       sync.Mutex
	deadline time.Time
}

func (c *heldConn) SetDeadline(at time.Time) error {
	c.setWriteDeadline(at)
	return c.Conn.SetDeadline(at)
}

func (c *heldConn) SetWriteDeadline(at time.Time) error {
	c.setWriteDeadline(at)
	return c.Conn.SetWriteDeadline(at)
}

func (c *heldConn) setWriteDeadline(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = at
}

func (c *heldConn) Write(p []byte) (int, error) {
	if gate := c.held.current(); gate != nil {
		reached := time.Now()
		c.held.waiting.Add(1)
		<-gate
		c.held.waiting.Add(-1)
		c.mu.Lock()
		deadline := c.deadline
		c.mu.Unlock()
		if !deadline.IsZero() {
			if err := c.Conn.SetWriteDeadline(deadline.Add(time.Since(reached))); err != nil {
				return 0, err
			}
		}
	}
	return c.Conn.Write(p)
}

// awaitControlWriteWaiting returns once a goroutine of the process waits in gorilla/websocket's
// WriteControl for its connection's write lock (blocked in its select), bounded only by the test
// binary's deadline: a close or a ping is then queued for that lock, so it is handed the lock the
// moment the write holding it lets go (a send on a channel a receiver waits on passes to that
// receiver). The wait comes only while the lock is held, so the row holds the socket's writer
// inside a write (heldWrites) before the close or the ping comes. The row's socket must be the
// only one of the process with a control frame to write.
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
	if !testwait.Holds(listenerWaitBound(t), 10*time.Millisecond, func() bool {
		frames, _ := socket.held()
		return frames > 1
	}) {
		t.Fatal("the socket never held a frame behind the one it was writing")
	}
}
