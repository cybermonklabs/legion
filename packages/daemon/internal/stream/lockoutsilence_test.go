package stream

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fleetalert"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// olderSilence is R3's window for an older generation's connection that was told to stop: 2
// minutes in which it sends nothing at all, after which the daemon ends it. Written here as the
// requirement says it, not read from the listener, so a listener that waited longer fails.
const olderSilence = 2 * time.Minute

// The case the kernel's timers cannot end: an older generation's connection whose other end
// still acknowledges every byte while nothing behind it reads or writes, as when a pod's sandbox
// outlives its force delete. Here the older connection completes its hello and is then neither
// read nor written, its socket left open: no FIN, no RST. Its stop frame lands in a socket nobody
// reads, and once it has sent nothing for olderSilence after that frame the daemon ends
// the connection itself. The lock-out ends with no report and no fleet alert, the close is the
// older generation's and not the claim's, and the newer process binds on its next hello.
func TestAnOlderConnectionSilentSinceItsStopFrameIsEnded(t *testing.T) {
	clock := &lockOutClock{now: time.Date(2026, 11, 16, 16, 52, 34, 0, time.UTC)}
	reports := make(chan LockOut, 4)
	alertLog := &logRecorder{}
	h := startListener(t, harnessOptions{
		resolver:      generations(map[string]uint64{testToken: testGeneration, newerToken: testGeneration + 1}),
		clock:         clock,
		alerts:        fleetalert.New(slog.New(alertLog), "acme"),
		reportLockOut: func(out LockOut) { reports <- out },
	})
	older := h.connectSilent(testToken)
	bound := h.conn()

	newer := dial(t, h.listener.Addr())
	newer.hello(newerToken)
	newer.awaitClosed()
	// The report's wait, registered at the refusal, and the silence's, registered once the stop
	// frame is written.
	clock.awaitTimers(t, 2)

	clock.advance(olderSilence - time.Second)
	if !daemonEndOpen(bound) {
		t.Fatalf("the older connection was ended %s after its stop frame, before %s", olderSilence-time.Second, olderSilence)
	}
	clock.advance(time.Second)
	if daemonEndOpen(bound) {
		t.Fatalf("the older connection is still open %s after its stop frame, with nothing heard from it", olderSilence)
	}

	if event := h.next(); event != (Closed{Claim: testClaim, Generation: testGeneration}) {
		t.Fatalf("event = %#v, want the older generation's close", event)
	}
	if out, ok := h.listener.LockedOut(testClaim); ok {
		t.Fatalf("LockedOut = %#v after the silent connection was ended, want no lock-out", out)
	}
	frames := older.rest(t)
	if len(frames) == 0 || frames[0].FrameType() != shimwire.TypeShutdown {
		t.Fatalf("the daemon wrote %v to the silent connection, want its stop frame first", frameTypes(frames))
	}
	for _, frame := range frames[1:] {
		if frame.FrameType() != shimwire.TypeNegotiateProtocol {
			t.Fatalf("the daemon wrote %v to the silent connection, want its stop frame and the question for its state", frameTypes(frames))
		}
	}
	want := []string{
		"worker-stream: rejected hello (already bound to a live stream of an older generation, which has been told to stop) " +
			"claim=" + string(testClaim) + " generation=4 boundGeneration=3",
		"worker-stream: ended an older generation's connection that sent nothing after its stop frame " +
			"claim=" + string(testClaim) + " generation=4 boundGeneration=3 silent=2m0s",
	}
	if got := h.logs.Lines(); !slices.Equal(got, want) {
		t.Fatalf("logs = %q, want %q", got, want)
	}

	// Nothing is reported once the report's own wait has passed: the lock-out ended at the cut.
	clock.advance(LockOutReportAfter)
	select {
	case out := <-reports:
		t.Fatalf("the lock-out was reported after its older connection was ended: %#v", out)
	default:
	}
	if lines := alertLog.Lines(); len(lines) != 0 {
		t.Fatalf("the fleet alert lines = %q, want none", lines)
	}

	next := dial(t, h.listener.Addr())
	next.hello(newerToken)
	next.expect(shimwire.TypeHelloAck)
	if event := h.next(); event != (Hello{Claim: testClaim, Generation: testGeneration + 1}) {
		t.Fatalf("event = %#v, want the newer generation bound", event)
	}
}

// A peer that reads what the daemon writes and never sends a line is the same dropped peer from
// the daemon's end. It is asked for its state again each time a request's RPC timeout ends first,
// and ended at the end of the window all the same.
func TestAnOlderConnectionThatNeverAnswersIsAskedAgainAndEnded(t *testing.T) {
	clock := &lockOutClock{now: time.Date(2026, 11, 16, 16, 52, 34, 0, time.UTC)}
	h := startListener(t, harnessOptions{
		resolver:   generations(map[string]uint64{testToken: testGeneration, newerToken: testGeneration + 1}),
		clock:      clock,
		rpcTimeout: time.Second,
	})
	older := h.connect(testToken)

	newer := dial(t, h.listener.Addr())
	newer.hello(newerToken)
	older.expect(shimwire.TypeShutdown)
	newer.awaitClosed()

	first := older.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)
	again := older.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)
	if again.ID == first.ID {
		t.Fatalf("the second question carried the first one's id %q, want a request of its own", first.ID)
	}

	clock.advance(olderSilence)
	older.awaitHangUp()
	if event := h.next(); event != (Closed{Claim: testClaim, Generation: testGeneration}) {
		t.Fatalf("event = %#v, want the older generation's close", event)
	}
}

// An older connection that is silent for most of the window and answers before it ends has a
// process behind it, which only a person can end: it is not ended, and the lock-out goes on to
// its report.
func TestAnOlderConnectionThatAnswersLateIsNotEnded(t *testing.T) {
	clock := &lockOutClock{now: time.Date(2026, 11, 16, 16, 52, 34, 0, time.UTC)}
	reports := make(chan LockOut, 4)
	h := startListener(t, harnessOptions{
		resolver:      generations(map[string]uint64{testToken: testGeneration, newerToken: testGeneration + 1}),
		clock:         clock,
		reportLockOut: func(out LockOut) { reports <- out },
	})
	older := h.connect(testToken)
	bound := h.conn()

	newer := dial(t, h.listener.Addr())
	newer.hello(newerToken)
	older.expect(shimwire.TypeShutdown)
	newer.awaitClosed()
	// The question comes once the window is open.
	asked := older.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)

	clock.advance(olderSilence - time.Second)
	older.answerState(asked)
	clock.advance(time.Second)

	if !daemonEndOpen(bound) {
		t.Fatal("the older connection was ended although it answered within the window")
	}
	if _, ok := h.listener.LockedOut(testClaim); !ok {
		t.Fatal("the lock-out ended while its older connection was still bound")
	}
	clock.advance(LockOutReportAfter - olderSilence)
	if out := awaitReport(t, reports); out.Older != testGeneration || out.Newer != testGeneration+1 {
		t.Fatalf("report = %#v, want generation %d refused while %d is bound", out, testGeneration+1, testGeneration)
	}
}

// Any line the older connection sends after its stop frame says a process is behind it, not
// only the answer to the daemon's question: a shim whose Oh My Pi leaves get_state unanswered
// but reports a turn ending is not ended.
func TestAnOlderConnectionThatSendsAnyFrameIsNotEnded(t *testing.T) {
	clock := &lockOutClock{now: time.Date(2026, 11, 16, 16, 52, 34, 0, time.UTC)}
	h := startListener(t, harnessOptions{
		resolver: generations(map[string]uint64{testToken: testGeneration, newerToken: testGeneration + 1}),
		clock:    clock,
	})
	older := h.connect(testToken)
	bound := h.conn()

	newer := dial(t, h.listener.Addr())
	newer.hello(newerToken)
	older.expect(shimwire.TypeShutdown)
	newer.awaitClosed()
	// The question comes once the window is open, and is never answered.
	older.expect(shimwire.TypeNegotiateProtocol)

	older.send(shimwire.AgentEnd{})
	if event := h.next(); event != (TurnEnd{Claim: testClaim}) {
		t.Fatalf("event = %#v, want the older connection's turn end", event)
	}
	clock.advance(olderSilence)
	if !daemonEndOpen(bound) {
		t.Fatal("the older connection was ended although it sent a frame after its stop frame")
	}
}

// daemonEndOpen is whether the daemon's own end of c is still open. The window's end closes it
// before the clock's advance returns, and a closed net.Conn refuses even the read deadline it
// already has, so this answers at once, where the events of a close come later.
func daemonEndOpen(c *Conn) bool {
	return c.nc.SetReadDeadline(time.Time{}) == nil
}

// silentPeer is a shim stand-in whose hello the listener accepted and which then neither reads
// nor writes, its socket left open: the daemon's kernel sees a live TCP peer, and nothing reads
// the frames it acknowledges.
type silentPeer struct {
	conn   net.Conn
	reader *shimwire.Reader
}

// connectSilent sends the hello, reads its hello_ack and consumes the Hello event, and then
// leaves the connection alone.
func (h *harness) connectSilent(bootToken string) *silentPeer {
	h.t.Helper()
	network, address, err := parseAddr(h.listener.Addr())
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	conn, err := net.DialTimeout(network, address, wait)
	if err != nil {
		h.t.Fatalf("dial %s: %v", address, err)
	}
	h.t.Cleanup(func() { conn.Close() })
	if err := shimwire.NewWriter(conn).WriteFrame(shimwire.Hello2{BootToken: bootToken}); err != nil {
		h.t.Fatalf("write the hello: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		h.t.Fatalf("set the hello_ack's read deadline: %v", err)
	}
	p := &silentPeer{conn: conn, reader: shimwire.NewReader(conn)}
	line, err := p.reader.ReadLine()
	if err != nil {
		h.t.Fatalf("read the hello_ack: %v", err)
	}
	if frame, err := shimwire.Decode(line); err != nil || frame.FrameType() != shimwire.TypeHelloAck {
		h.t.Fatalf("the daemon answered the hello with %q (%v), want hello_ack", line, err)
	}
	if event, ok := h.next().(Hello); !ok {
		h.t.Fatalf("first event = %#v, want Hello", event)
	}
	return p
}

// rest is every frame the daemon wrote after the hello_ack, read only once the daemon has hung
// up: the socket's buffer holds them, and the end of the stream is the daemon's close.
func (p *silentPeer) rest(t *testing.T) []shimwire.Frame {
	t.Helper()
	if err := p.conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	var frames []shimwire.Frame
	for {
		line, err := p.reader.ReadLine()
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("the daemon did not hang up on the silent connection: %v (read so far: %v)", err, frameTypes(frames))
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			t.Fatalf("the daemon wrote a line that is not a frame: %v", err)
		}
		frames = append(frames, frame)
	}
}

// answerState answers what the daemon asks an older connection after its stop frame as Oh My Pi
// answers it through its shim: the negotiation asked, and the get_state behind it. The get_state
// arriving is the daemon having read the negotiation's answer.
func (p *peer) answerState(asked shimwire.NegotiateProtocol) {
	p.t.Helper()
	p.send(shimwire.Response{ID: asked.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
	request := p.expect(shimwire.TypeGetState).(shimwire.GetState)
	p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeGetState, Success: true,
		Data: mustJSON(p.t, map[string]any{"isStreaming": true})})
}

func frameTypes(frames []shimwire.Frame) []string {
	types := make([]string, 0, len(frames))
	for _, frame := range frames {
		types = append(types, frame.FrameType())
	}
	return types
}
