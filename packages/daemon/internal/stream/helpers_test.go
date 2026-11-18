package stream

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// testClaim is the claim the default resolver hands out for testToken.
const (
	testClaim      claim.Token = "legion-acme-LEGION-1-tester"
	testToken                  = "boot-token-1"
	testGeneration uint64      = 3
	// productionRPCTimeout is worker_rpc_timeout_seconds's default (internal/config), the RPC
	// timeout the harness's listener runs with: the hello's read deadline on the socket, and the
	// request bound measured on the harness's fake clock, which moves only when a test moves it.
	productionRPCTimeout = 5 * time.Second
)

// testMoment is where a harness's fake clock stands until the test moves it.
var testMoment = time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)

// logRecorder is a slog handler that keeps every line the listener logs, message and
// attributes, so a test can assert the exact text an operator would read.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	line := record.Message
	record.Attrs(func(attr slog.Attr) bool {
		line += " " + attr.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	return nil
}

// The listener logs through the logger it is given and never derives one; a handler that
// dropped attributes here would hide them from the assertions, so it refuses to be derived.
func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { panic("stream derived a logger") }
func (r *logRecorder) WithGroup(string) slog.Handler      { panic("stream derived a logger") }

func (r *logRecorder) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lines)
}

// resolver counts its calls, so a test can assert a rejected hello resolved its token at most
// once and a malformed one never.
type resolver struct {
	calls atomic.Int32
	fn    HelloResolver
}

func (r *resolver) resolve(bootToken string) (claim.Token, uint64, bool, bool, error) {
	r.calls.Add(1)
	return r.fn(bootToken)
}

func defaultResolver() *resolver {
	return &resolver{fn: func(bootToken string) (claim.Token, uint64, bool, bool, error) {
		if bootToken == testToken {
			return testClaim, testGeneration, false, true, nil
		}
		return "", 0, false, false, nil
	}}
}

// harness is one listener under test and the context that bounds it. clock is the fake the
// listener measures its request bounds and its launcher wait on.
type harness struct {
	t        *testing.T
	listener *Listener
	logs     *logRecorder
	resolver *resolver
	clock    *clocktest.Fake
	cancel   context.CancelFunc
	drained  []Event
	done     bool
}

type harnessOptions struct {
	addr     string
	resolver *resolver
	// helloDeadline is Options.RPCTimeout for the test of a hello that never completes, whose
	// read deadline on the socket is that timeout; zero is productionRPCTimeout.
	helloDeadline time.Duration
	// launcherWait is Options.LauncherWait, for the tests of how long a launcher's hello waits
	// for an acceptor; zero leaves the listener's own default.
	launcherWait time.Duration
}

func startListener(t *testing.T, options harnessOptions) *harness {
	t.Helper()
	if options.addr == "" {
		options.addr = "tcp://127.0.0.1:0"
	}
	if options.resolver == nil {
		options.resolver = defaultResolver()
	}
	logs := &logRecorder{}
	clock := clocktest.New(testMoment)
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := Listen(ctx, options.addr, options.resolver.resolve, Options{
		RPCTimeout:   cmp.Or(options.helloDeadline, productionRPCTimeout),
		LauncherWait: options.launcherWait,
		Log:          slog.New(logs),
		Clock:        clock,
	})
	if err != nil {
		cancel()
		t.Fatalf("Listen(%q): %v", options.addr, err)
	}
	h := &harness{t: t, listener: listener, logs: logs, resolver: options.resolver, clock: clock, cancel: cancel}
	t.Cleanup(func() { h.stop() })
	return h
}

// next is the listener's next event.
func (h *harness) next() Event {
	h.t.Helper()
	event := testwait.Receive(h.t, "the listener's next event", h.listener.Events())
	if event == nil {
		h.t.Fatal("Events() closed while an event was expected")
	}
	return event
}

// stop ends the listener and returns every event it had not yet handed to next, through the
// channel's close — which is the only way to prove an event was never emitted.
func (h *harness) stop() []Event {
	h.t.Helper()
	if h.done {
		return h.drained
	}
	h.done = true
	h.cancel()
	for {
		event := testwait.Receive(h.t, "Events() to close after the listener's context ended", h.listener.Events())
		if event == nil {
			return h.drained
		}
		h.drained = append(h.drained, event)
	}
}

// noAnswer moves the harness's clock past the RPC bound of the request the peer has just read
// and leaves unanswered, once the listener has armed it.
func (h *harness) noAnswer() {
	h.t.Helper()
	if armed := h.clock.AwaitArmed(h.t, "the request's RPC bound", 1); len(armed) != 1 || !armed[0].Equal(h.clock.Now().Add(productionRPCTimeout)) {
		h.t.Fatalf("the listener armed %v, want the one request's RPC bound of %s from %v", armed, productionRPCTimeout, h.clock.Now())
	}
	h.clock.Advance(productionRPCTimeout)
}

// peer is a shim stand-in on the far side of one real connection: it writes raw lines and
// reads the daemon's frames back.
type peer struct {
	t      *testing.T
	conn   net.Conn
	frames chan shimwire.Frame
	closed chan struct{}
	writer *shimwire.Writer
}

func dial(t *testing.T, addr string) *peer {
	t.Helper()
	network, address, err := parseAddr(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(testwait.Context(t), network, address)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	p := &peer{
		t:      t,
		conn:   conn,
		frames: make(chan shimwire.Frame, 64),
		closed: make(chan struct{}),
		writer: shimwire.NewWriter(conn),
	}
	go func() {
		defer close(p.closed)
		reader := shimwire.NewReader(conn)
		for {
			line, err := reader.ReadLine()
			if err != nil {
				return
			}
			frame, err := shimwire.Decode(line)
			if err != nil {
				t.Errorf("the daemon wrote a line that is not a frame: %v", err)
				return
			}
			p.frames <- frame
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return p
}

func (p *peer) writeRaw(text string) {
	p.t.Helper()
	if _, err := io.WriteString(p.conn, text); err != nil {
		p.t.Fatalf("peer write: %v", err)
	}
}

func (p *peer) send(frame shimwire.Frame) {
	p.t.Helper()
	if err := p.writer.WriteFrame(frame); err != nil {
		p.t.Fatalf("peer write %s: %v", frame.FrameType(), err)
	}
}

func (p *peer) hello(bootToken string) {
	p.t.Helper()
	p.send(shimwire.Hello2{BootToken: bootToken})
}

// expect is the daemon's next frame, which must be of the given type.
func (p *peer) expect(frameType string) shimwire.Frame {
	p.t.Helper()
	var frame shimwire.Frame
	testwait.Eventually(p.t, "the daemon's "+frameType, func() bool {
		select {
		case frame = <-p.frames:
			return true
		default:
		}
		select {
		case <-p.closed:
			// The reader hands over every frame it read before it ends, so one more look.
			select {
			case frame = <-p.frames:
				return true
			default:
			}
			p.t.Fatalf("the daemon closed the connection while %s was expected", frameType)
		default:
		}
		return false
	})
	if frame.FrameType() != frameType {
		p.t.Fatalf("the daemon sent %s (%#v), want %s", frame.FrameType(), frame, frameType)
	}
	return frame
}

// awaitClosed is the daemon hanging up, with nothing sent first.
func (p *peer) awaitClosed() {
	p.t.Helper()
	testwait.Receive(p.t, "the daemon to close the connection", p.closed)
	select {
	case frame := <-p.frames:
		p.t.Fatalf("the daemon sent %s before closing", frame.FrameType())
	default:
	}
}

// connect is a peer whose hello the listener accepted: hello sent, hello_ack read, and the
// Hello event consumed.
func (h *harness) connect(bootToken string) *peer {
	h.t.Helper()
	p := dial(h.t, h.listener.Addr())
	p.hello(bootToken)
	p.expect(shimwire.TypeHelloAck)
	if event, ok := h.next().(Hello); !ok {
		h.t.Fatalf("first event = %#v, want Hello", event)
	}
	return p
}

// negotiate answers the negotiation the connection sends ahead of its first agent request.
func (p *peer) negotiate() {
	p.t.Helper()
	request := p.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)
	if request.ProtocolVersion != 2 {
		p.t.Fatalf("negotiate_protocol asked for version %d, want 2", request.ProtocolVersion)
	}
	p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
}

// conn is the listener's connection for testClaim as the concrete type, which is what carries
// Negotiate.
func (h *harness) conn() *Conn {
	h.t.Helper()
	found, ok := h.listener.Conn(testClaim)
	if !ok {
		h.t.Fatalf("no connection registered for %s", testClaim)
	}
	return found.(*Conn)
}

// async runs call on its own goroutine — every Conn request blocks until the peer answers,
// and the peer is driven from the test goroutine.
func async(call func() error) <-chan error {
	result := make(chan error, 1)
	go func() { result <- call() }()
	return result
}

func awaitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	return testwait.Receive(t, "the call's return", result)
}

// shortSocketPath is this package's socket path: a socket named worker-stream.sock in a
// directory of the test's own, placed by TMPDIR and falling back to /tmp for the socket path's
// length alone, so that the bind holds within the 107 bytes a unix socket path takes
// (docs/testing.md).
func shortSocketPath(t *testing.T) string {
	t.Helper()
	return testtmp.Socket(t, "worker-stream.sock")
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return encoded
}

func isClosed(err error) bool { return errors.Is(err, ErrClosed) }
