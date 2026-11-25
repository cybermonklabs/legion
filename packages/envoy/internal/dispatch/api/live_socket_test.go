package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// liveSocketRig is the API test server behind a real listener and run on a lifetime, so Register
// builds the delivery hub and mounts GET /ws/live as production does.
type liveSocketRig struct {
	handler  http.Handler
	database *store.Store
	served   *server
	host     string
	url      string
	// stop ends the server's lifetime, as the signal that shuts Dispatch down does.
	stop context.CancelFunc
}

// newLiveSocketRig starts the rig on options; listen, when given, wraps the server's listener.
// The lifetime derives from the test's context, which ends before the test's cleanups wait out the
// sockets and the hub.
func newLiveSocketRig(t *testing.T, options testServerOptions, listen func(net.Listener) net.Listener) *liveSocketRig {
	t.Helper()
	lifetime, stop := context.WithCancel(t.Context())
	options.lifetime = lifetime
	handler, database, _, served := newRegisteredTestServer(t, options)
	httpServer := httptest.NewUnstartedServer(handler)
	if listen != nil {
		httpServer.Listener = listen(httpServer.Listener)
	}
	httpServer.Start()
	t.Cleanup(httpServer.Close)
	host := strings.TrimPrefix(httpServer.URL, "http://")
	return &liveSocketRig{handler: handler, database: database, served: served, host: host, url: "ws://" + host + "/ws/live", stop: stop}
}

// deliverySubscribe is a page's subscribe frame for a delivery view, as a socket test sends it.
func deliverySubscribe(sub string, view DeliveryLiveView) LiveSubscribe {
	return LiveSubscribe{Type: "subscribe", Sub: sub, View: LiveView{DeliveryLiveView: view}}
}

func asUser(login string) http.Header { return http.Header{"X-Dispatch-User": {login}} }

func withBearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

// sockets waits for exactly n sockets open on the server and answers them. Its one bound is the
// test binary's deadline (testwait.Until), and its failure names the count it last read.
func (rig *liveSocketRig) sockets(t *testing.T, n int) []*liveSocket {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("%d live sockets open on the server", n), func() ([]*liveSocket, bool) {
		open := rig.served.liveSockets.all()
		return open, len(open) == n
	})
}

// socketClient is one page's GET /ws/live connection. Once it reads, every message it is sent
// lands in frames, and ended closes when the connection ends, with err saying how.
type socketClient struct {
	conn   *gws.Conn
	frames chan []byte
	ended  chan struct{}
	err    error
	// database is the store of the server behind the socket, which a wait on its event feed
	// names as the test's own (eventFeedFrame): the rig's, or the one servedBy names.
	database *store.Store
}

// open dials the socket, failing the test unless the upgrade succeeds; it reads nothing until read.
func (rig *liveSocketRig) open(t *testing.T, dialer *gws.Dialer, header http.Header) *socketClient {
	t.Helper()
	return openLiveSocketAt(t, dialer, rig.url, header).servedBy(rig.database)
}

// openLiveSocketAt dials the live socket at url, failing the test unless the upgrade succeeds; it
// reads nothing until read.
func openLiveSocketAt(t *testing.T, dialer *gws.Dialer, url string, header http.Header) *socketClient {
	t.Helper()
	conn, response, err := dialLive(t, dialer, url, header)
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("open %s: %v (%s)", url, err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &socketClient{conn: conn, frames: make(chan []byte, 4096), ended: make(chan struct{})}
}

// dialLive dials url with dialer and answers what the dial answered. The handshake ends on the
// server's answer: dialer's own HandshakeTimeout, 45 s on gorilla's DefaultDialer, is dropped, and
// the wait's one bound is the test binary's deadline (testwait.Receive), so an answer a loaded
// machine sends late still ends it and one that never comes fails the test by name.
//
// The dial runs on a context the test's cleanup cancels, and the cleanup waits for the dial's
// goroutine to return, so no dial outlives the test. That context ends gorilla's TCP connect but
// not its read of the server's answer, which DialContext makes with no deadline when the dialer
// sets no timeout, so until the handshake ends the context's end also closes the connection the
// dial opened. A connection the handshake answers once the test no longer waits for it is closed,
// as the test would have closed it. A dialer whose connect takes no context (NetDial alone) could
// outlive the cleanup, so dialLive refuses it.
func dialLive(t *testing.T, dialer *gws.Dialer, url string, header http.Header) (*gws.Conn, *http.Response, error) {
	t.Helper()
	if dialer.NetDial != nil && dialer.NetDialContext == nil {
		t.Fatalf("dialLive dials %s with a dialer whose connect takes no context (NetDial), which no cleanup can end; give it NetDialContext", url)
	}
	ctx, stop := context.WithCancel(context.Background())
	unbounded := *dialer
	unbounded.HandshakeTimeout = 0
	// connect is the TCP connect gorilla makes for a ws:// URL: the dialer's NetDialContext, else a
	// plain net.Dialer's. Both end with ctx.
	connect := (&net.Dialer{}).DialContext
	if dialer.NetDialContext != nil {
		connect = dialer.NetDialContext
	}
	// release stops the close the context's end would make of the dialed connection. Gorilla calls
	// NetDialContext on the goroutine that called DialContext, so only that goroutine sets it and
	// reads it.
	var release func() bool
	unbounded.NetDial = nil
	unbounded.NetDialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		conn, err := connect(dialCtx, network, address)
		if err == nil {
			release = context.AfterFunc(ctx, func() { _ = conn.Close() })
		}
		return conn, err
	}
	type dialed struct {
		conn     *gws.Conn
		response *http.Response
		err      error
	}
	answered := make(chan dialed, 1)
	// worked closes once the dial's goroutine has returned, which the cleanup waits for.
	worked := make(chan struct{})
	// abandoned says no one takes the answer any more, which the cleanup sets; mu orders it with
	// the answer's send, so an answer is either taken from answered or closed, never left open.
	var mu sync.Mutex
	abandoned := false
	go func() {
		defer close(worked)
		conn, response, err := unbounded.DialContext(ctx, url, header)
		if release != nil {
			release()
		}
		mu.Lock()
		defer mu.Unlock()
		if abandoned && conn != nil {
			_ = conn.Close()
			conn = nil
		}
		answered <- dialed{conn: conn, response: response, err: err}
	}()
	t.Cleanup(func() {
		stop()
		mu.Lock()
		abandoned = true
		// An answer the dial sent that the test never took, its wait having given up first.
		select {
		case got := <-answered:
			if got.conn != nil {
				_ = got.conn.Close()
			}
		default:
		}
		mu.Unlock()
		// The cancel ends the connect, and the close the context's end makes ends the handshake's
		// read, so the goroutine returns once it has closed any connection it was answered.
		<-worked
	})
	got := testwait.Receive(t, "the server's answer to the upgrade of "+url, answered)
	return got.conn, got.response, got.err
}

// dial opens the socket and reads it.
func (rig *liveSocketRig) dial(t *testing.T, header http.Header) *socketClient {
	t.Helper()
	return rig.open(t, gws.DefaultDialer, header).read()
}

// refused dials the socket and answers the status Dispatch refused the upgrade with.
func (rig *liveSocketRig) refused(t *testing.T, header http.Header) int {
	t.Helper()
	conn, response, err := dialLive(t, gws.DefaultDialer, rig.url, header)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the upgrade succeeded; want it refused")
	}
	if response == nil {
		t.Fatalf("dial: %v, with no response", err)
	}
	return response.StatusCode
}

// read starts reading the socket: its ping handler answers Dispatch's pings only while it reads.
func (c *socketClient) read() *socketClient {
	go func() {
		defer close(c.ended)
		for {
			kind, data, err := c.conn.ReadMessage()
			if err != nil {
				c.err = err
				return
			}
			if kind != gws.TextMessage {
				c.err = fmt.Errorf("a message of kind %d", kind)
				return
			}
			c.frames <- data
		}
	}()
	return c
}

func (c *socketClient) send(t *testing.T, frame any) {
	t.Helper()
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encode %T: %v", frame, err)
	}
	c.sendText(t, data)
}

func (c *socketClient) sendText(t *testing.T, data []byte) {
	t.Helper()
	if err := c.conn.WriteMessage(gws.TextMessage, data); err != nil {
		t.Fatalf("send %.120s: %v", data, err)
	}
}

// take is the next message, failing the test when the socket ends first. Its one bound is the test
// binary's deadline (testwait.Receive): a frame a loaded machine sends late still ends the wait,
// and one that never comes fails it by name.
func (c *socketClient) take(t *testing.T, what string) []byte {
	t.Helper()
	arrived := testwait.Receive(t, what, c.arrivalOf(true))
	if arrived.ended {
		t.Fatalf("the socket ended (%v) waiting for %s", arrived.err, what)
	}
	return arrived.data
}

// next is the next frame but a heartbeat, decoded by its type.
func (c *socketClient) next(t *testing.T, what string) any {
	t.Helper()
	for {
		frame := decodeSocketFrame(t, c.take(t, what))
		if _, beat := frame.(LiveHeartbeat); !beat {
			return frame
		}
	}
}

// socketArrival is what arrival sends: the next message but a heartbeat, or, when ended, the
// socket's end and err its reason.
type socketArrival struct {
	data  []byte
	ended bool
	err   error
}

// arrival sends the next message but a heartbeat, or the socket's end, on a channel, for a wait
// whose bound is not a clock (eventFeedFrame). It takes from frames on a goroutine of its own
// until it has sent, so the caller takes from frames again only once it has received; the channel
// is buffered, so the goroutine leaves even when nobody receives, at the latest when the test's
// cleanup closes the socket.
func (c *socketClient) arrival() <-chan socketArrival { return c.arrivalOf(false) }

// arrivalOf is arrival, sending a heartbeat on too when heartbeats is set (take).
func (c *socketClient) arrivalOf(heartbeats bool) <-chan socketArrival {
	arrived := make(chan socketArrival, 1)
	// deliver sends data unless it is a heartbeat arrival passes over, and says whether it sent.
	deliver := func(data []byte) bool {
		var head struct {
			Type string `json:"type"`
		}
		if !heartbeats && json.Unmarshal(data, &head) == nil && head.Type == "heartbeat" {
			return false
		}
		arrived <- socketArrival{data: data}
		return true
	}
	go func() {
		for {
			select {
			case data := <-c.frames:
				if deliver(data) {
					return
				}
			case <-c.ended:
				// The reader queues every message before it ends.
				for {
					select {
					case data := <-c.frames:
						if deliver(data) {
							return
						}
					default:
						arrived <- socketArrival{ended: true, err: c.err}
						return
					}
				}
			}
		}
	}()
	return arrived
}

func (c *socketClient) snapshot(t *testing.T, sub string) DeliveryLiveSnapshot {
	t.Helper()
	frame := c.next(t, "the snapshot of "+sub)
	snapshot, ok := frame.(DeliveryLiveSnapshot)
	if !ok || snapshot.Sub != sub {
		t.Fatalf("frame = %+v, want the snapshot of %s", frame, sub)
	}
	return snapshot
}

func (c *socketClient) delta(t *testing.T, sub string) DeliveryLiveDelta {
	t.Helper()
	frame := c.next(t, "a delta of "+sub)
	delta, ok := frame.(DeliveryLiveDelta)
	if !ok || delta.Sub != sub {
		t.Fatalf("frame = %+v, want a delta of %s", frame, sub)
	}
	return delta
}

func (c *socketClient) refusal(t *testing.T, sub, code string) LiveError {
	t.Helper()
	frame := c.next(t, code+" for "+strconv.Quote(sub))
	refusal, ok := frame.(LiveError)
	if !ok || refusal.Sub != sub || refusal.Code != code {
		t.Fatalf("frame = %+v, want %s for %q", frame, code, sub)
	}
	return refusal
}

// refusals reads frames until each of subs has been refused with code, in any order, failing on
// any other frame but a heartbeat.
func (c *socketClient) refusals(t *testing.T, code string, subs ...string) {
	t.Helper()
	waiting := make(map[string]bool, len(subs))
	for _, sub := range subs {
		waiting[sub] = true
	}
	for len(waiting) > 0 {
		switch frame := decodeSocketFrame(t, c.take(t, code+" for each of "+strings.Join(subs, ", "))).(type) {
		case LiveHeartbeat:
		case LiveError:
			if frame.Code != code || !waiting[frame.Sub] {
				t.Fatalf("frame = %+v, want %s for one of %v not yet refused", frame, code, subs)
			}
			delete(waiting, frame.Sub)
		default:
			t.Fatalf("frame = %+v, want %s for one of %v", frame, code, subs)
		}
	}
}

// event is the next frame but a heartbeat, which must be one of sub's events.
func (c *socketClient) event(t *testing.T, sub string) LiveEventFrame {
	t.Helper()
	frame := c.next(t, "an event of "+sub)
	event, ok := frame.(LiveEventFrame)
	if !ok || event.Sub != sub {
		t.Fatalf("frame = %+v, want an event of %s", frame, sub)
	}
	return event
}

// quiet fails when the socket sends any frame but a heartbeat, or ends, within within.
func (c *socketClient) quiet(t *testing.T, within time.Duration, what string) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case data := <-c.frames:
			if _, beat := decodeSocketFrame(t, data).(LiveHeartbeat); !beat {
				t.Fatalf("%s: the socket sent %.200s", what, data)
			}
		case <-c.ended:
			t.Fatalf("%s: the socket ended (%v)", what, c.err)
		case <-deadline:
			return
		}
	}
}

// closeError waits for the socket to end, passing over the messages it is sent first, and answers
// the close frame Dispatch ended it with.
func (c *socketClient) closeError(t *testing.T) *gws.CloseError {
	t.Helper()
	c.awaitEnd(t)
	var closed *gws.CloseError
	if !errors.As(c.err, &closed) {
		t.Fatalf("the socket ended with %v, which carries no close code", c.err)
	}
	return closed
}

// closeCode waits for the socket to end, counting the messages it is sent first, and answers the
// close code Dispatch ended it with: 1006 for an end with no close frame, which gorilla reads from
// the connection's end, and 0 for an error with no code, such as a reset.
func (c *socketClient) closeCode(t *testing.T) (code, messages int) {
	t.Helper()
	messages = c.awaitEnd(t)
	var closed *gws.CloseError
	if errors.As(c.err, &closed) {
		return closed.Code, messages
	}
	t.Logf("the socket ended with an error that carries no close code: %v", c.err)
	return 0, messages
}

// awaitEnd waits for the socket to end, taking every message it is sent meanwhile so the reader
// never stops at a full frames, and answers how many it was sent. Its one bound is the test
// binary's deadline (testwait.Until), and its failure names the messages counted so far.
func (c *socketClient) awaitEnd(t *testing.T) int {
	t.Helper()
	messages := 0
	return testwait.Until(t, "the socket's end (messages taken so far)", func() (int, bool) {
		for {
			select {
			case <-c.frames:
				messages++
			case <-c.ended:
				// The reader queues every message before it ends.
				return messages + len(c.frames), true
			default:
				return messages, false
			}
		}
	})
}

// open reports whether the socket has not ended.
func (c *socketClient) open() bool {
	select {
	case <-c.ended:
		return false
	default:
		return true
	}
}

// decodeSocketFrame decodes one frame Dispatch sent as its type's struct, refusing a member the type
// does not declare.
func decodeSocketFrame(t *testing.T, data []byte) any {
	t.Helper()
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatalf("a frame that is not a JSON object: %.200s: %v", data, err)
	}
	switch head.Type {
	case "snapshot":
		return decodeSocketFrameAs[DeliveryLiveSnapshot](t, data)
	case "delta":
		return decodeSocketFrameAs[DeliveryLiveDelta](t, data)
	case "freshness":
		return decodeSocketFrameAs[DeliveryLiveFreshness](t, data)
	case "error":
		return decodeSocketFrameAs[LiveError](t, data)
	case "heartbeat":
		return decodeSocketFrameAs[LiveHeartbeat](t, data)
	case "events_start":
		return decodeSocketFrameAs[LiveEventsStart](t, data)
	case "event":
		return decodeSocketFrameAs[LiveEventFrame](t, data)
	case "agent":
		return decodeSocketFrameAs[LiveAgentFrame](t, data)
	case "ended":
		return decodeSocketFrameAs[LiveEnded](t, data)
	}
	t.Fatalf("a frame of no known type: %.200s", data)
	return nil
}

func decodeSocketFrameAs[T any](t *testing.T, data []byte) T {
	t.Helper()
	var frame T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&frame); err != nil {
		t.Fatalf("decode %T from %.200s: %v", frame, data, err)
	}
	return frame
}

// mintAgentToken is a personal agent token alice mints.
func mintAgentToken(t *testing.T, handler http.Handler) agentTokenResponse {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "delivery-probe"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("mint an agent token: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[agentTokenResponse](t, response)
}

// hubConnCount is how many sockets hold a subscription on the hub.
func hubConnCount(hub *deliveryHub) int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.generation == nil {
		return 0
	}
	return len(hub.generation.conns)
}

// smallSendBuffers shrinks the send buffer of every connection the server accepts, so a socket whose
// page stops reading blocks its writer after kilobytes rather than the megabytes the kernel tunes to.
type smallSendBuffers struct{ net.Listener }

func (l smallSendBuffers) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(4096)
	}
	return conn, err
}

// smallReadDialer dials with a small receive buffer, so a page that stops reading holds back the
// socket's writer after kilobytes; beside smallSendBuffers it is how a test plays a slow page. A
// test that must know the writer stays held holds it with heldWrites instead. Its handshake ends on
// the server's answer (dialLive).
func smallReadDialer() *gws.Dialer {
	return &gws.Dialer{NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetReadBuffer(4096)
		}
		return conn, err
	}}
}

// heldWrites wraps the server's listener so a test can hold a socket's writer inside a write, as a
// page that reads nothing holds it, and know that it stays held until the test lets it go. Holding
// it with TCP backpressure instead (smallSendBuffers, smallReadDialer) leaves the transport to
// decide when the write resumes, and a frame the page sends can resume it: under gVisor a write
// blocked for a third of a second completed the moment the page sent its next subscribe, in 8 of
// 300 runs of TestARepeatedSubReplacesAnotherKind.
type heldWrites struct {
	mu sync.Mutex
	// gate is open (nil) or, while the test holds writes, the channel release closes.
	gate chan struct{}
	// waiting counts the writes waiting at the gate.
	waiting atomic.Int32
}

// listen is newLiveSocketRig's listen: every connection the server accepts writes through the gate.
func (held *heldWrites) listen(listener net.Listener) net.Listener {
	return heldListener{Listener: listener, held: held}
}

// hold makes every write the server starts from now on wait at the gate until release, however
// long the test takes to release it (heldConn). The test's cleanup releases it, ahead of the rig's,
// so no socket's handler is left waiting on its writer.
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

// waitHeld waits until socket's writer waits at the gate with its stream queue full: from then on
// the writer writes nothing and takes nothing from either queue until release.
func (held *heldWrites) waitHeld(t *testing.T, socket *liveSocket) {
	t.Helper()
	waitUntil(t, "the writer held with the stream queue full", func() bool {
		return held.waiting.Load() > 0 && streamFull(socket)
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
// test's release alone: the write deadline the writer set (liveSocketWriteWait) moves on by the time
// the write waited, so a release later than that deadline, as a loaded machine can make it between
// the test's steps, still lets the write through rather than failing it as a stalled one.
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

// streamFull reports whether socket's stream queue holds all it can.
func streamFull(socket *liveSocket) bool {
	return cap(socket.stream) > 0 && len(socket.stream) == cap(socket.stream)
}

// servedSpans wraps the server's listener so a test reads, for each connection the server accepts,
// in the order it accepts them, how long the server itself served it: from the first read deadline
// the server sets ahead of now, which a live socket sets as it opens (awaitPong, its first pong
// deadline, before its writer starts the socket's age), to the server's end of the socket - the
// close frame it writes, or, where it writes none (a page that answered no ping), its close of the
// connection. A span read off the server's own calls holds none of the client's dial, scheduling,
// reads or close reply, which a loaded machine stretches, so a bound on it bounds the server alone.
type servedSpans struct {
	mu    sync.Mutex
	conns []*spannedConn
}

// listen is newLiveSocketRig's listen: every connection the server accepts records its span.
func (spans *servedSpans) listen(listener net.Listener) net.Listener {
	return spannedListener{Listener: listener, spans: spans}
}

// span is how long the server served the nth connection it accepted, and false until the server
// has both set that connection's first read deadline and closed it.
func (spans *servedSpans) span(n int) (time.Duration, bool) {
	spans.mu.Lock()
	defer spans.mu.Unlock()
	if n >= len(spans.conns) {
		return 0, false
	}
	conn := spans.conns[n]
	if conn.opened.IsZero() || conn.closed.IsZero() {
		return 0, false
	}
	if !conn.closeFrame.IsZero() {
		return conn.closeFrame.Sub(conn.opened), true
	}
	return conn.closed.Sub(conn.opened), true
}

type spannedListener struct {
	net.Listener
	spans *servedSpans
}

func (l spannedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return conn, err
	}
	spanned := &spannedConn{Conn: conn, spans: l.spans}
	l.spans.mu.Lock()
	l.spans.conns = append(l.spans.conns, spanned)
	l.spans.mu.Unlock()
	return spanned, nil
}

// spannedConn is one accepted connection, which records when the server first set its read
// deadline ahead of now, when it began to write its close frame, and when it closed the connection;
// spans.mu guards the three.
type spannedConn struct {
	net.Conn
	spans                      *servedSpans
	opened, closeFrame, closed time.Time
}

func (c *spannedConn) SetReadDeadline(at time.Time) error {
	c.record(&c.opened, at.After(time.Now()))
	return c.Conn.SetReadDeadline(at)
}

// Write records the server's close frame as it starts to write it. gorilla writes a control frame
// in one Write, its first byte the FIN bit and the opcode; no data frame's write starts with that
// byte, and a JSON payload written on its own starts with '{'.
func (c *spannedConn) Write(p []byte) (int, error) {
	c.record(&c.closeFrame, len(p) > 0 && p[0] == 0x80|gws.CloseMessage)
	return c.Conn.Write(p)
}

func (c *spannedConn) Close() error {
	c.record(&c.closed, true)
	return c.Conn.Close()
}

// record stamps moment with now, when ok is set and moment holds no stamp yet.
func (c *spannedConn) record(moment *time.Time, ok bool) {
	c.spans.mu.Lock()
	defer c.spans.mu.Unlock()
	if ok && moment.IsZero() {
		*moment = time.Now()
	}
}

func TestTheDeliverySocketAnswersASubscribeWithASnapshot(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seedHubSettings(t, rig.database)
	now := time.Now().UTC()
	upsertPR(t, rig.database, hubPR(1, now.Add(-3*time.Hour)))

	client := rig.dial(t, asUser("alice"))
	client.send(t, deliverySubscribe("t", timelineRange(now.Add(-48*time.Hour), now, "")))
	snapshot := client.snapshot(t, "t")
	requireSameBody(t, "the socket's snapshot", snapshot.Body, getBody(t, rig.handler, windowTarget(t, deliveryLiveTimeline, snapshot.Body, "")))
	if _, ok := snapshot.Revs.Items["prs"]["acme/widgets#1"]; !ok {
		t.Fatalf("the snapshot's revisions %v hold no revision of acme/widgets#1", snapshot.Revs.Items)
	}

	// A merge after the snapshot reaches the page as a delta that lands on the GET route's body.
	upsertPR(t, rig.database, hubPR(2, now.Add(-2*time.Hour)))
	delta := client.delta(t, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#2"}) {
		t.Fatalf("the merge's delta upserts %v, want acme/widgets#2", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

func TestAnUnauthenticatedUpgradeIsRefused(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	for name, header := range map[string]http.Header{
		"no credential":     nil,
		"an unknown bearer": withBearer("not-a-token"),
	} {
		if status := rig.refused(t, header); status != http.StatusUnauthorized {
			t.Errorf("%s: the upgrade answered %d, want 401", name, status)
		}
	}
	if open := rig.served.liveSockets.all(); len(open) != 0 {
		t.Fatalf("%d sockets open after refused upgrades", len(open))
	}
}

func TestAForeignOriginIsRefused(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	foreign := asUser("alice")
	foreign.Set("Origin", "https://other.example")
	if status := rig.refused(t, foreign); status != http.StatusForbidden {
		t.Fatalf("another site's page opening the socket with alice's credential was answered %d, want 403", status)
	}

	// The page's own origin opens it.
	own := asUser("alice")
	own.Set("Origin", "http://"+rig.host)
	client := rig.dial(t, own)
	client.send(t, LiveUnsubscribe{Type: "unsubscribe"})
	client.refusal(t, "", "INVALID_FRAME")
}

func TestABearerMayReadTheSocket(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seedHubSettings(t, rig.database)
	minted := mintAgentToken(t, rig.handler)
	for name, token := range map[string]string{"the shared token": "agent-token", "a personal token": minted.Token} {
		t.Run(name, func(t *testing.T) {
			client := rig.dial(t, withBearer(token))
			client.send(t, deliverySubscribe("p", presetView(deliveryLiveTimeline, 7, "")))
			client.snapshot(t, "p")
		})
	}
}

// A bearer revoked while its socket is open loses the socket at the next heartbeat's resolution, as
// the document socket does: the request's context ends, and the socket closes
// with 1008. The heartbeat is a second, so the client's pong, which gorilla's default handler
// writes, has a heartbeat to meet the pong deadline on a loaded runner: the socket must last until
// the revocation, and a pong timeout would end it first.
func TestARevokedBearerClosesItsSocketWithPolicyViolation(t *testing.T) {
	t.Parallel()
	const heartbeat = time.Second
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: heartbeat}, nil)
	minted := mintAgentToken(t, rig.handler)
	client := rig.dial(t, withBearer(minted.Token))

	// The token's resolutions run on a heartbeat that starts before the socket's writer's, so by the
	// writer's third heartbeat frame the caller has been resolved again on each earlier heartbeat. A
	// resolution that failed would have closed the socket, which take reports as its end.
	for n := range 3 {
		if data := client.take(t, fmt.Sprintf("heartbeat %d of a socket whose token resolves", n+1)); string(data) != `{"type":"heartbeat"}` {
			t.Fatalf("the idle socket was sent %s, want a heartbeat", data)
		}
	}
	if !client.open() {
		t.Fatalf("the socket of a token that resolves ended: %v", client.err)
	}
	if response := dispatchRequest(t, rig.handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+minted.ID, nil, "alice"); response.Code != http.StatusNoContent {
		t.Fatalf("revoke the token: status=%d body=%s", response.Code, response.Body.String())
	}
	if code, _ := client.closeCode(t); code != gws.ClosePolicyViolation {
		t.Fatalf("the revoked token's socket closed with %d, want 1008", code)
	}
	rig.sockets(t, 0)
}

// A page of a newer build may send members this build does not declare while a release rolls
// out: a subscribe frame carrying them is answered with its snapshot. A frame type or a view kind
// this build cannot serve is still refused, so the page reconnects to another process.
func TestASubscribeWithAMemberNoFrameDeclaresIsAnswered(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seedHubSettings(t, rig.database)
	client := rig.dial(t, asUser("alice"))

	client.sendText(t, []byte(`{"type":"subscribe","sub":"p","since":1,"view":{"kind":"timeline","window":{"preset":7,"anchor":"end"},"query":"","density":"compact"}}`))
	client.snapshot(t, "p")

	client.sendText(t, []byte(`{"type":"resubscribe","sub":"q"}`))
	client.refusal(t, "q", "INVALID_FRAME")
	client.sendText(t, []byte(`{"type":"subscribe","sub":"r","view":{"kind":"forecast"}}`))
	client.refusal(t, "r", "INVALID_FRAME")
}

func TestAMalformedFrameIsAnErrorFrameNotAClose(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	client := rig.dial(t, asUser("alice"))
	for _, test := range []struct {
		name   string
		frame  string
		binary bool
		sub    string
		code   string
		names  string
	}{
		{name: "a subscribe without a view", frame: `{"type":"subscribe","sub":"a"}`, sub: "a", code: "INVALID_FRAME"},
		{name: "a subscribe without a sub", frame: `{"type":"subscribe"}`, code: "INVALID_FRAME"},
		{name: "a view of no kind Dispatch serves", frame: `{"type":"subscribe","sub":"b","view":{"kind":"forecast"},"since":1}`, sub: "b", code: "INVALID_FRAME"},
		{name: "an unknown type", frame: `{"type":"resubscribe","sub":"c"}`, sub: "c", code: "INVALID_FRAME"},
		{name: "an unsubscribe without a sub", frame: `{"type":"unsubscribe"}`, code: "INVALID_FRAME"},
		{name: "text that is not JSON", frame: `subscribe`, code: "INVALID_FRAME"},
		{name: "two JSON values", frame: `{"type":"unsubscribe","sub":"d"} {}`, code: "INVALID_FRAME"},
		{name: "a binary message", frame: `{"type":"unsubscribe","sub":"e"}`, binary: true, code: "INVALID_FRAME"},
		{name: "a NUL in the sub", frame: `{"type":"unsubscribe","sub":"f\u0000"}`, code: "NUL_CHARACTER", names: "sub "},
		{name: "a NUL in the query", frame: `{"type":"subscribe","sub":"g","view":{"kind":"timeline","window":{"preset":7},"query":"repo=a%00b"}}`, sub: "g", code: "NUL_CHARACTER", names: "query parameter repo "},
	} {
		kind := gws.TextMessage
		if test.binary {
			kind = gws.BinaryMessage
		}
		if err := client.conn.WriteMessage(kind, []byte(test.frame)); err != nil {
			t.Fatalf("%s: send: %v", test.name, err)
		}
		refusal := client.refusal(t, test.sub, test.code)
		if !strings.HasPrefix(refusal.Message, test.names) || strings.ContainsRune(refusal.Message, 0) {
			t.Fatalf("%s: refusal %q, want one naming %q with no NUL in it", test.name, refusal.Message, test.names)
		}
	}

	// The socket is still open, and the hub answers its next subscribe.
	client.send(t, deliverySubscribe("h", presetView(deliveryLiveTimeline, 7, "")))
	client.refusal(t, "h", "DELIVERY_NOT_CONFIGURED")
}

func TestAFrameOverTheReadLimitClosesTheSocket(t *testing.T) {
	t.Parallel()
	// The server's pong deadline, twice the heartbeat, would end the transfer: a frame holds the
	// client's write lock until it is written, and gorilla's ping handler drops a pong it waited 1 s
	// for that lock. A heartbeat past the run leaves the test binary's deadline the one bound.
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: pastTheRun}, nil)
	// A write buffer larger than either message sends each as one frame, as Firefox sends a message.
	dialer := *gws.DefaultDialer
	dialer.WriteBufferSize = 10 << 20
	client := rig.open(t, &dialer, asUser("alice")).read()

	// A resume's revisions may run to megabytes: a frame under 8 MiB is read, and this one is answered
	// for its view, which names no kind. The answer comes only once all 7 MiB have arrived and been
	// parsed, and the write returns only once the server has read most of them, each as late as the
	// loaded machine makes it: the write runs on a goroutine of its own, and the test waits for what
	// it answers (testwait.Receive), with the test binary's deadline its one bound.
	revisions := strings.Repeat("0", 7<<20)
	subscribe := []byte(`{"type":"subscribe","sub":"r","view":{"kind":"bogus"},"resume":{"seq":"0","revs":{"items":{},"maps":{},"counts":{},"fields":{"f":"` + revisions + `"}}}}`)
	written := make(chan error, 1)
	go func() { written <- client.conn.WriteMessage(gws.TextMessage, subscribe) }()
	if err := testwait.Receive(t, "the write of the 7 MiB subscribe frame", written); err != nil {
		t.Fatalf("write the 7 MiB subscribe frame: %v", err)
	}
	client.refusal(t, "r", "INVALID_FRAME")

	// One over it closes the socket with 1009. gorilla compares a frame's declared length with the
	// limit as it reads the frame's header, so Dispatch answers before reading any of the 9 MiB.
	oversized := []byte(`{"type":"unsubscribe","sub":"` + strings.Repeat("a", 9<<20) + `"}`)
	go func() { _ = client.conn.WriteMessage(gws.TextMessage, oversized) }()
	if code, _ := client.closeCode(t); code != gws.CloseMessageTooBig {
		t.Fatalf("a 9 MiB frame closed the socket with %d, want 1009", code)
	}
	rig.sockets(t, 0)
}

// Chrome on desktop sends a message over 64 KiB in frames of at most 131,000 bytes, the capacity of
// the data pipe its renderer fills for the network service (services/network/websocket.cc). The read
// limit counts a message's frames together: the frame that takes the message past 8 MiB closes the
// socket with 1009, though the message never finishes.
func TestAMessageOverTheReadLimitInChromesFramesClosesTheSocket(t *testing.T) {
	t.Parallel()
	// A heartbeat past the run, as TestAFrameOverTheReadLimitClosesTheSocket's: the transfer holds
	// back the client's pongs.
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: pastTheRun}, nil)
	const frame = 131000
	dialer := *gws.DefaultDialer
	dialer.WriteBufferSize = frame
	client := rig.open(t, &dialer, asUser("alice")).read()

	// 65 frames and a byte: the writer sends a full frame once the next byte needs its buffer, so
	// the byte sends the 65th frame, which takes the message past the limit. No last frame follows.
	writer, err := client.conn.NextWriter(gws.TextMessage)
	if err != nil {
		t.Fatalf("open a message: %v", err)
	}
	go func() { _, _ = writer.Write(bytes.Repeat([]byte("a"), 65*frame+1)) }()
	// Dispatch reads the 64 frames under the limit before the 65th's header, so the close comes only
	// once 8 MiB have crossed the socket, as late as the loaded machine makes it.
	if code, _ := client.closeCode(t); code != gws.CloseMessageTooBig {
		t.Fatalf("65 frames of 131,000 bytes closed the socket with %d, want 1009", code)
	}
	rig.sockets(t, 0)
}

func TestAWindowWiderThanTheMaximumIsAnErrorFrame(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seedHubSettings(t, rig.database)
	client := rig.dial(t, asUser("alice"))
	to := time.Now().UTC().Truncate(time.Second)

	client.send(t, deliverySubscribe("wide", timelineRange(to.Add(-maxDeliveryWindow-time.Second), to, "")))
	if refusal := client.refusal(t, "wide", "INVALID_QUERY"); refusal.Message != "from..to must not span more than 84 days" {
		t.Fatalf("refusal %q, want the GET route's", refusal.Message)
	}

	// The widest window the GET route answers is answered too, on the same socket.
	client.send(t, deliverySubscribe("widest", timelineRange(to.Add(-maxDeliveryWindow), to, "")))
	client.snapshot(t, "widest")
}

func TestASlowReaderIsClosedNotBuffered(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, func(listener net.Listener) net.Listener { return smallSendBuffers{listener} })
	client := rig.open(t, smallReadDialer(), asUser("alice"))

	// Every frame is answered with an error frame echoing its 4 KiB sub, and the page reads none of
	// them: the socket's writer blocks, its queue fills, and the next frame overflows it.
	const sent = 300
	sub := strings.Repeat("s", 4096)
	for n := range sent {
		client.sendText(t, []byte(`{"type":"resubscribe","sub":"`+sub+strconv.Itoa(n)+`"}`))
	}
	socket := rig.sockets(t, 1)[0]
	waitUntil(t, fmt.Sprintf("the socket's %d-frame queue to overflow", liveSocketQueue), socket.overflowed)

	// Once the page reads again it is sent what the connection already carried, then 1013, and not
	// the answers queued behind the overflow.
	code, messages := client.read().closeCode(t)
	if code != gws.CloseTryAgainLater {
		t.Fatalf("the slow page's socket closed with %d, want 1013", code)
	}
	if messages >= sent {
		t.Fatalf("the slow page was sent %d of %d answers: the socket buffered them", messages, sent)
	}
	rig.sockets(t, 0)
}

func TestShutdownClosesEverySocketWithServiceRestart(t *testing.T) {
	t.Parallel()
	events := newFakeLiveKind(0, 0)
	rig := newLiveSocketRig(t, testServerOptions{liveKinds: map[string]liveKind{liveEventsView: events.kind(true)}}, nil)
	seedHubSettings(t, rig.database)
	person := rig.dial(t, asUser("alice"))
	agent := rig.dial(t, withBearer("agent-token"))
	person.send(t, deliverySubscribe("t", presetView(deliveryLiveTimeline, 7, "")))
	person.snapshot(t, "t")
	agent.send(t, liveSubscribe("e", eventsView(), ""))
	running := events.next(t, "the agent's events subscription")

	rig.stop()
	for name, client := range map[string]*socketClient{"the person's": person, "the agent's": agent} {
		if code, _ := client.closeCode(t); code != gws.CloseServiceRestart {
			t.Errorf("%s socket closed with %d, want 1012", name, code)
		}
	}
	rig.served.liveSockets.wait()
	rig.served.deliveryHub.wait()
	if held := hubConnCount(rig.served.deliveryHub); held != 0 {
		t.Fatalf("the hub holds subscriptions of %d closed sockets", held)
	}
	// The socket's wait covers the subscriptions it ran: the events subscription stopped once, and
	// its goroutine had returned before the wait did.
	select {
	case <-running.done:
	default:
		t.Fatal("the socket's wait returned with its events subscription still running")
	}
	if stops := running.stops.Load(); stops != 1 {
		t.Fatalf("the events subscription was stopped %d times, want once", stops)
	}
}

func TestASocketPastItsAgeClosesWithMaxAge(t *testing.T) {
	t.Parallel()
	const age = 200 * time.Millisecond
	spans := &servedSpans{}
	rig := newLiveSocketRig(t, testServerOptions{liveSocketAge: func() time.Duration { return age }}, spans.listen)
	client := rig.dial(t, asUser("alice"))
	closed := client.closeError(t)
	if closed.Code != contracts.LiveSocketMaxAgeClose || closed.Text != "max age" {
		t.Fatalf("the socket past its age closed with %d %q, want %d \"max age\", which a page answers by reopening at once", closed.Code, closed.Text, contracts.LiveSocketMaxAgeClose)
	}
	// The server drops the socket from its set once it has closed the connection, so its span is
	// whole by then: from the pong deadline it set as the socket opened, before its writer started
	// the age, to the close frame it wrote at the age. The close comes at the age, so a second past
	// it leaves only the scheduling of a loaded machine, and a writer that closes later fails.
	rig.sockets(t, 0)
	if lived, ok := spans.span(0); !ok {
		t.Fatal("UNKNOWN: the server's span of the socket was not recorded, so its age was not measured")
	} else if lived < age {
		t.Fatalf("the server served the socket for %s, closing it before its age (%s)", lived, age)
	} else if lived > age+time.Second {
		t.Fatalf("the server served the socket for %s, closing it more than a second after its age (%s)", lived, age)
	}
}

func TestEachSocketsAgeFallsInItsWindow(t *testing.T) {
	t.Parallel()
	served := &server{}
	drawn := map[time.Duration]struct{}{}
	for range 1000 {
		age := served.liveSocketAge()
		if age < contracts.LiveSocketMinAge || age >= contracts.LiveSocketMaxAge {
			t.Fatalf("a socket's age %s is outside [%s, %s)", age, contracts.LiveSocketMinAge, contracts.LiveSocketMaxAge)
		}
		drawn[age] = struct{}{}
	}
	// Drawn at random, so the sockets a deploy opened together do not all close together.
	if len(drawn) < 990 {
		t.Fatalf("1,000 sockets drew %d distinct ages, want them drawn at random", len(drawn))
	}
	fixed := &server{deps: Deps{LiveSocketAge: func() time.Duration { return time.Minute }}}
	if age := fixed.liveSocketAge(); age != time.Minute {
		t.Fatalf("with Deps.LiveSocketAge set, a socket's age is %s, want its %s", age, time.Minute)
	}
}

// A test hook closes every open socket, a person's and a bearer's, with its code, and the page's
// reconnect is served.
func requireTheHookClosesEverySocket(t *testing.T, hook string, code int, reason string) {
	t.Helper()
	rig := newLiveSocketRig(t, testServerOptions{testHooks: true}, nil)
	person := rig.dial(t, asUser("alice"))
	agent := rig.dial(t, withBearer("agent-token"))
	rig.sockets(t, 2)

	if response := dispatchRequest(t, rig.handler, http.MethodPost, hook, nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("POST %s: status=%d body=%s", hook, response.Code, response.Body.String())
	}
	for name, client := range map[string]*socketClient{"the person's": person, "the agent's": agent} {
		if closed := client.closeError(t); closed.Code != code || closed.Text != reason {
			t.Errorf("%s socket closed with %d %q, want %d %q", name, closed.Code, closed.Text, code, reason)
		}
	}

	again := rig.dial(t, asUser("alice"))
	again.send(t, LiveUnsubscribe{Type: "unsubscribe"})
	again.refusal(t, "", "INVALID_FRAME")
}

func TestTheRotateHookClosesEverySocketWithMaxAge(t *testing.T) {
	t.Parallel()
	requireTheHookClosesEverySocket(t, "/api/v1/live/_test/rotate", contracts.LiveSocketMaxAgeClose, "max age")
}

func TestTheDisconnectHookClosesEverySocketWithServiceRestart(t *testing.T) {
	t.Parallel()
	requireTheHookClosesEverySocket(t, "/api/v1/live/_test/disconnect", gws.CloseServiceRestart, "")
}

func TestTheLiveTestHooksAreMountedOnlyUnderTestHooks(t *testing.T) {
	t.Parallel()
	t.Run("without test hooks", func(t *testing.T) {
		rig := newLiveSocketRig(t, testServerOptions{}, nil)
		client := rig.dial(t, asUser("alice"))
		for _, hook := range []string{"/api/v1/live/_test/disconnect", "/api/v1/live/_test/rotate"} {
			if response := dispatchRequest(t, rig.handler, http.MethodPost, hook, nil, "alice"); response.Code != http.StatusNotFound {
				t.Fatalf("POST %s without DISPATCH_TEST_HOOKS: status=%d, want 404", hook, response.Code)
			}
		}
		client.send(t, LiveUnsubscribe{Type: "unsubscribe"})
		client.refusal(t, "", "INVALID_FRAME")
	})
	t.Run("the delivery socket's hook is gone", func(t *testing.T) {
		rig := newLiveSocketRig(t, testServerOptions{testHooks: true}, nil)
		if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/delivery/_test/disconnect", nil, "alice"); response.Code != http.StatusNotFound {
			t.Fatalf("POST /api/v1/delivery/_test/disconnect: status=%d, want 404", response.Code)
		}
	})
}

// An events subscription that keeps the stream queue full while its writer is held neither holds
// back the hub's frames nor overflows the hub's queue: the hub's queue fills beside it, and once
// the writer writes again every hub frame is written ahead of the stream frames still queued.
func TestHubFramesAreWrittenAheadOfAFullStreamQueue(t *testing.T) {
	t.Parallel()
	busy := newFakeLiveKind(-1, 512)
	held := &heldWrites{}
	// A heartbeat past the run: the page reads nothing while the writer is held, so it answers no
	// ping, and the pong deadline would end the socket however long the held steps take.
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: pastTheRun, liveKinds: map[string]liveKind{liveEventsView: busy.kind(true)}}, held.listen)
	client := rig.open(t, gws.DefaultDialer, asUser("alice"))
	held.hold(t)
	client.send(t, liveSubscribe("busy", eventsView(), ""))
	busy.next(t, "the busy events subscription")
	socket := rig.sockets(t, 1)[0]
	held.waitHeld(t, socket)

	// Eight delivery views, each told delivery is not configured: the hub's answers queue beside
	// the full stream queue, and the socket stays open.
	const answers = 8
	subs := make([]string, answers)
	for n := range answers {
		subs[n] = "d" + strconv.Itoa(n)
		client.send(t, deliverySubscribe(subs[n], livePipelineView(nil)))
	}
	waitUntil(t, "the hub answers every delivery view", func() bool { return len(socket.queue) == answers })
	if socket.overflowed() {
		t.Fatal("the hub's queue overflowed beside a full stream queue")
	}

	// The writer writes again: the frame it held comes first, then all eight hub frames together,
	// then the stream again.
	held.release()
	client.read()
	var first LiveError
	for {
		frame := decodeSocketFrame(t, client.take(t, "the first hub frame"))
		if refusal, ok := frame.(LiveError); ok {
			first = refusal
			break
		}
	}
	if first.Code != "DELIVERY_NOT_CONFIGURED" {
		t.Fatalf("the first hub frame = %+v, want DELIVERY_NOT_CONFIGURED", first)
	}
	rest := slices.DeleteFunc(slices.Clone(subs), func(sub string) bool { return sub == first.Sub })
	if len(rest) != answers-1 {
		t.Fatalf("the first hub frame refuses %q, not one of the delivery views %v", first.Sub, subs)
	}
	client.refusals(t, "DELIVERY_NOT_CONFIGURED", rest...)
	client.event(t, "busy")
	if !client.open() {
		t.Fatalf("the socket ended: %v", client.err)
	}
}

// A page that answers each ping keeps its socket, idle, across many heartbeats. The heartbeat is a
// second, so the page's first pong has a heartbeat to arrive before the pong deadline, two
// heartbeats after the socket opened, and a loaded runner that delays it most of a heartbeat still
// meets it. A page that answers no ping, which a short heartbeat ends quickly, is the next test's.
func TestPingsKeepAnIdleSocketOpen(t *testing.T) {
	t.Parallel()
	const heartbeat = time.Second
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: heartbeat}, nil)
	answering := rig.open(t, gws.DefaultDialer, asUser("alice"))
	var pings atomic.Int32
	answering.conn.SetPingHandler(func(data string) error {
		pings.Add(1)
		// No write deadline: the pong waits for the connection's write lock alone, which nothing
		// else on this idle page takes.
		return answering.conn.WriteControl(gws.PongMessage, []byte(data), time.Time{})
	})
	answering.read()

	// The socket sends a ping each heartbeat, so the fourth comes four heartbeats after it opened,
	// twice the pong deadline: a page that still holds its socket then holds it only through its
	// pongs. The wait ends on the fourth ping, or on the socket's end.
	testwait.Until(t, "the idle page's fourth ping, or its socket's end (pings so far)", func() (int32, bool) {
		got := pings.Load()
		return got, got >= 4 || !answering.open()
	})
	if !answering.open() {
		t.Fatalf("an idle page that answers every ping lost its socket after %d pings: %v", pings.Load(), answering.err)
	}
	rig.sockets(t, 1)
}

// A page that answers no ping is closed once two heartbeats pass without a pong. It has gone or
// stopped reading, so the socket sends it no close frame: the connection ends, which gorilla and a
// browser report as 1006, and the page reconnects on its backoff, as after every close but 1012,
// which starts the backoff over (packages/dispatch/web/src/api/live-socket.ts, lost). 1008 would
// say its caller no longer resolves.
func TestAPageThatAnswersNoPingLosesItsSocketWithNoCloseFrame(t *testing.T) {
	t.Parallel()
	const heartbeat = 200 * time.Millisecond
	spans := &servedSpans{}
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: heartbeat}, spans.listen)
	// Where a page that answers no ping ends with the wrong close, it does so through a race in the
	// writer, so one silent page would show a regression only in some runs. Eight pages, each racing
	// on its own, make it fail nearly every run: at an even race a run misses with odds of 1 in 256.
	const silentPages = 8
	silent := make([]*socketClient, silentPages)
	for i := range silent {
		silent[i] = rig.open(t, gws.DefaultDialer, asUser("bob"))
		silent[i].conn.SetPingHandler(func(string) error { return nil })
		silent[i].read()
	}

	for i, page := range silent {
		testwait.Receive(t, fmt.Sprintf("the end of silent page %d's socket, which answers no ping", i), page.ended)
		var closed *gws.CloseError
		if !errors.As(page.err, &closed) || closed.Code != gws.CloseAbnormalClosure {
			t.Errorf("silent page %d's socket ended with %v, want an end with no close frame (1006)", i, page.err)
		}
	}
	// The server drops each socket from its set once it has closed the connection, so every span is
	// whole by then: from the pong deadline the server set as the socket opened to that close. The
	// pages opened one after another, so the server accepted their connections in their order.
	rig.sockets(t, 0)
	for i := range silent {
		served, ok := spans.span(i)
		switch {
		case !ok:
			t.Errorf("UNKNOWN: the server's span of silent page %d's socket was not recorded", i)
		case served < heartbeat || served > 2*heartbeat+time.Second:
			t.Errorf("the server served silent page %d's socket for %s, want about two heartbeats (%s), its pong deadline", i, served, 2*heartbeat)
		}
	}
}

func TestAnIdleDeliverySocketIsSentAHeartbeat(t *testing.T) {
	t.Parallel()
	const heartbeat = 300 * time.Millisecond
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: heartbeat}, nil)
	client := rig.dial(t, asUser("alice"))
	if data := client.take(t, "the heartbeat"); string(data) != `{"type":"heartbeat"}` {
		t.Fatalf("an idle socket's first frame = %s, want {\"type\":\"heartbeat\"} alone", data)
	}
}
