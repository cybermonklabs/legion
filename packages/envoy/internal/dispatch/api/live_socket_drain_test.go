package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// countingConn counts the bytes read from and written to one connection, and closes closed once
// the connection is closed, with readAtClose the bytes read by then.
type countingConn struct {
	net.Conn
	read, written atomic.Int64
	closeOnce     sync.Once
	closed        chan struct{}
	readAtClose   int64
}

func newCountingConn(conn net.Conn) *countingConn {
	return &countingConn{Conn: conn, closed: make(chan struct{})}
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

func (c *countingConn) Close() error {
	c.closeOnce.Do(func() {
		c.readAtClose = c.read.Load()
		close(c.closed)
	})
	return c.Conn.Close()
}

// countingListener hands each connection the server accepts on accepted, counted.
type countingListener struct {
	net.Listener
	accepted chan<- *countingConn
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return conn, err
	}
	counted := newCountingConn(conn)
	l.accepted <- counted
	return counted, nil
}

// drainStart is where a drain row's fakes start, and drainHeartbeat its rig's heartbeat: the pong
// deadline the upgrade arms, two heartbeats on, falls inside the second the drain waits
// (liveSocketCloseWait).
var drainStart = time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)

const drainHeartbeat = 100 * time.Millisecond

// drainChunk is one piece of the frame past the read limit that a drain row sends.
var drainChunk = bytes.Repeat([]byte("a"), 4096)

// countedRig is a live socket rig whose connections are counted on both sides (countingConn). Its
// sockets' read deadlines run on the row's own clock, a fake the row moves, and their pings on a
// fake no one moves, so no ping draws a pong that moves the pong deadline.
type countedRig struct {
	*liveSocketRig
	accepted chan *countingConn
}

func newCountedRig(t *testing.T, deadlines clock.Clock) *countedRig {
	t.Helper()
	accepted := make(chan *countingConn, 1)
	options := testServerOptions{streamHeartbeat: drainHeartbeat, liveSocketClock: deadlines, liveSocketPingClock: clocktest.New(drainStart)}
	rig := newLiveSocketRig(t, options, func(listener net.Listener) net.Listener {
		return countingListener{Listener: listener, accepted: accepted}
	})
	return &countedRig{liveSocketRig: rig, accepted: accepted}
}

// countedSocket is one page's socket on a countedRig: client reads it, page counts the page's side
// of its connection and server the server's.
type countedSocket struct {
	client       *socketClient
	page, server *countingConn
}

// openCounted opens a socket as alice and starts reading it.
func (rig *countedRig) openCounted(t *testing.T) *countedSocket {
	t.Helper()
	socket := &countedSocket{}
	dialer := &gws.Dialer{NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		socket.page = newCountingConn(conn)
		return socket.page, nil
	}}
	socket.client = rig.open(t, dialer, asUser("alice"))
	socket.server = testwait.Receive(t, "the server's side of the socket's connection", rig.accepted)
	socket.client.read()
	return socket
}

func (socket *countedSocket) send(t *testing.T, what string, data []byte) {
	t.Helper()
	if _, err := socket.page.Write(data); err != nil {
		t.Fatalf("send %s: %v", what, err)
	}
}

// sendOverTheLimit sends the header of a 9 MiB text frame, masked as a page's frames are, and the
// first of its bytes, and requires the socket closed with 1009. gorilla compares the declared
// length with the limit as it reads the header, writes the 1009 under a deadline of its own, a
// second of the wall clock on, and only then returns the read loop's error, reading nothing more.
func (socket *countedSocket) sendOverTheLimit(t *testing.T) {
	t.Helper()
	header := []byte{0x81, 0x80 | 127, 0, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4}
	binary.BigEndian.PutUint64(header[2:10], 9<<20)
	socket.send(t, "the frame's header", append(header, drainChunk...))
	if code, _ := socket.client.closeCode(t); code != gws.CloseMessageTooBig {
		t.Fatalf("a 9 MiB frame closed the socket with %d, want 1009", code)
	}
}

// drainReading sends more of the frame and waits until the server has read every byte the page
// sent. The read loop has ended once the socket closed with 1009, so only the drain reads what the
// page sends from there: the drain is reading, and its deadline is set.
func (socket *countedSocket) drainReading(t *testing.T) {
	t.Helper()
	socket.send(t, "more of the frame", drainChunk)
	testwait.Until(t, "the server to read every byte the page sent, the last of them in its drain", func() ([2]int64, bool) {
		read, written := socket.server.read.Load(), socket.page.written.Load()
		return [2]int64{read, written}, read == written
	})
}

// endFrame sends the rest of the frame and the page's FIN, waits for the server to close the
// connection, and requires that the server read every byte the page sent by then; cut says what a
// drain that closed it having read less did.
func (socket *countedSocket) endFrame(t *testing.T, cut string) {
	t.Helper()
	// The rest goes out whatever the server did before: a drain cut short has closed the
	// connection, and the page's write then draws a reset, which the verdict below names.
	rest := bytes.Repeat([]byte("a"), 2*len(drainChunk))
	if _, err := socket.page.Write(rest); err != nil {
		t.Logf("send the rest of the frame: %v", err)
	}
	if err := socket.page.Conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Logf("send the page's FIN: %v", err)
	}
	testwait.Receive(t, "the server's close of the connection once its drain ended", socket.server.closed)
	if sent := socket.page.written.Load(); socket.server.readAtClose != sent {
		t.Errorf("the server closed the connection having read %d of the %d bytes the page sent: %s", socket.server.readAtClose, sent, cut)
	}
}

// A frame past the read limit closes the socket with 1009, and the socket then reads off the rest
// of the frame before it closes the connection, so the close is a FIN behind the 1009 and not a
// reset that can overtake it. The drain's own deadline, liveSocketCloseWait on the socket's
// read-deadline clock, replaces the pong deadline, so a pong deadline that falls due inside the
// drain does not cut it short.
//
// The read deadlines run on a fake of the row's own (countedRig), on which the upgrade arms the
// pong deadline two heartbeats on, inside the second the drain waits; the pings run on a fake no
// one moves, so no pong moves that deadline. Once the drain is reading, the row moves the fake to
// the pong deadline. The page then sends the rest of the frame and its FIN, and the server must
// have read every byte the page sent when it closes the connection. A drain the pong deadline cut
// short closes it having read none of that rest. Nothing in the row reads the wall clock but
// gorilla's own deadline on its 1009 write (sendOverTheLimit).
func TestAFrameOverTheReadLimitIsDrainedPastThePongDeadline(t *testing.T) {
	deadlines := clocktest.New(drainStart)
	pongBy, drainBy := drainStart.Add(2*drainHeartbeat), drainStart.Add(liveSocketCloseWait)
	rig := newCountedRig(t, deadlines)
	socket := rig.openCounted(t)
	deadlines.AwaitArmedAt(t, "the pong deadline the upgrade armed", pongBy)
	socket.sendOverTheLimit(t)

	socket.drainReading(t)
	if armed := deadlines.Armed(); !slices.Equal(armed, []time.Time{drainBy}) {
		t.Errorf("the drain's read deadlines armed %v, want only the drain's own, liveSocketCloseWait on, at %s, in place of the pong deadline at %s",
			armed, drainBy.Format(time.RFC3339Nano), pongBy.Format(time.RFC3339Nano))
	}

	deadlines.Set(pongBy)
	socket.endFrame(t, "its drain ended at the pong deadline, before the frame's end")
	rig.sockets(t, 0)
}

// A read deadline that the pong deadline set on the connection before the drain began is lifted
// with the pong deadline itself (endReads). The pong deadline can pass after the read loop has
// ended on a frame past the read limit, while the handler waits for its writer to end, and a drain
// that kept the deadline it set would end at its first read.
//
// The row holds the writer at its first look, before it picks anything (idleWriterHold), so once
// the read loop has ended on the frame's header the handler waits for the writer, short of the
// drain. It then moves the read deadlines' fake to the pong deadline the upgrade armed, whose timer
// fires and sets the connection's read deadline to the moment it fired, and lets the writer go.
// The page sends the rest of the frame and its FIN, and the server must have read every byte the
// page sent when it closes the connection. Nothing in the row reads the wall clock but gorilla's
// own deadline on its 1009 write (sendOverTheLimit), which it makes while the writer is held at its
// look and holds none of the connection's writes.
func TestAFrameOverTheReadLimitIsDrainedPastAPongDeadlineThatPassedBeforeTheDrain(t *testing.T) {
	deadlines := clocktest.New(drainStart)
	pongBy := drainStart.Add(2 * drainHeartbeat)
	rig := newCountedRig(t, deadlines)
	writer := newIdleWriterHold(t)
	rig.served.liveWriterBeforePick = writer.beforePick
	writer.holdNext()
	socket := rig.openCounted(t)
	writer.waitHeld(t)
	deadlines.AwaitArmedAt(t, "the pong deadline the upgrade armed", pongBy)
	socket.sendOverTheLimit(t)

	deadlines.Set(pongBy)
	if armed := deadlines.Armed(); len(armed) != 0 {
		t.Fatalf("the read deadlines armed %v once the fake reached the pong deadline at %s, want none: its timer fires there, with the writer held, before the drain arms its own",
			armed, pongBy.Format(time.RFC3339Nano))
	}
	writer.release()
	socket.endFrame(t, "the read deadline the pong deadline set before the drain began ended the drain at its first read")
	rig.sockets(t, 0)
}
