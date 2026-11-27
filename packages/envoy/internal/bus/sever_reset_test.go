package bus

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// markerSubscriber subscribes to subject on a connection of its own to the test's server, straight
// rather than through any relay.
func markerSubscriber(t *testing.T, subject string) (*nats.Conn, chan *nats.Msg) {
	t.Helper()
	subscriber := testnats.Connect(t, testnats.URL(t))
	received := make(chan *nats.Msg, 16)
	if _, err := subscriber.ChanSubscribe(subject, received); err != nil {
		t.Fatalf("subscribe to the marker: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the marker's subscription: %v", err)
	}
	return subscriber, received
}

// awaitWritten waits until nats.go has written everything conn had buffered into its socket,
// bounded only by the test binary's deadline (testwait.Until).
func awaitWritten(t *testing.T, conn *nats.Conn, what string) {
	t.Helper()
	testwait.Until(t, what+" written into the socket", func() (int, bool) {
		buffered, err := conn.Buffered()
		if err != nil {
			t.Fatalf("read what nats.go holds of %s: %v", what, err)
		}
		return buffered, buffered == 0
	})
}

// stallWithMarker stalls relay with conn's connection through it, fills the relay's window with a
// 64 KiB filler, and writes marker to subject on conn, so the marker's bytes wait in the socket's own
// kernel send queue, where nothing in user space has read them.
func stallWithMarker(t *testing.T, relay *testnats.StallRelay, conn *nats.Conn, subject string, marker []byte) {
	t.Helper()
	relay.Stall()
	if err := conn.Publish("testbus.stall.filler", make([]byte, 64<<10)); err != nil {
		t.Fatalf("publish the filler: %v", err)
	}
	awaitWritten(t, conn, "the filler")
	relay.AwaitParked(t)
	if err := conn.Publish(subject, marker); err != nil {
		t.Fatalf("publish the marker: %v", err)
	}
	awaitWritten(t, conn, "the marker")
	if bytes.Contains(relay.ReadByRelay(t), marker) {
		t.Fatal("control: the marker reached the relay before its window filled, so it left the socket")
	}
}

// receivedOnceSettled lets the stalled link move, waits until the server has handled everything the
// stalled socket still sent and the subscriber has had everything the server sent it by then, and
// reports whether the marker reached the subscriber.
func receivedOnceSettled(t *testing.T, relay *testnats.StallRelay, subscriber *nats.Conn, received chan *nats.Msg, marker []byte) bool {
	t.Helper()
	relay.Move()
	relay.AwaitEnded(t)
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscriber: %v", err)
	}
	for {
		select {
		case msg := <-received:
			if bytes.Equal(msg.Data, marker) {
				return true
			}
		default:
			return false
		}
	}
}

// A socket closes normally unless it carries a write the server has not confirmed. Each subtest has a
// client's connection go through a relay it then stalls (testnats.StallRelay), writes a marker into
// the socket's own kernel send queue, closes the client, lets the link move, and waits until the
// server has handled everything that socket still sent.
//   - no write marked (the control): the close is normal, and the marker reaches a subscriber of the
//     row's own, sent by the kernel after the close, as every client that marks nothing closes.
//   - the marker marked as unconfirmed (Client.MarkUnconfirmed): the close is a reset, and the marker
//     never reaches the subscriber.
//
// The negative control is a close that does not read the mark: the second subtest fails with the
// marker received.
func TestASocketIsResetAtCloseOnlyWhileItCarriesAnUnconfirmedWrite(t *testing.T) {
	const subject = "testbus.reset.marker"
	marker := []byte("unconfirmed-marker")
	for _, marked := range []bool{false, true} {
		name := "no write marked"
		if marked {
			name = "the marker marked as unconfirmed"
		}
		t.Run(name, func(t *testing.T) {
			relay := testnats.StartStallRelay(t, testnats.URL(t))
			subscriber, received := markerSubscriber(t, subject)
			client, err := ConnectOwningStream([]string{relay.URL()})
			if err != nil {
				t.Fatalf("connect through the relay: %v", err)
			}
			t.Cleanup(client.Close)
			conn := client.Connection()
			written := func() {}
			if marked {
				written = client.MarkUnconfirmed()
			}
			stallWithMarker(t, relay, conn, subject, marker)
			written()
			client.Close()
			got := receivedOnceSettled(t, relay, subscriber, received, marker)
			switch {
			case marked && got:
				t.Fatal("the marked socket's close sent the marker once the link moved: the close was not a reset")
			case !marked && !got:
				t.Fatal("control: the unmarked socket's close did not send the marker once the link moved, so the row cannot tell a normal close from a reset")
			}
		})
	}
}

// A socket whose write failed sends nothing more once the link moves. The row's connection goes
// through a relay it then stalls (testnats.StallRelay), with a write deadline of half a second
// (nats.FlusherTimeout) and the pings of WithSilentCutDetection. It writes a marker into the socket's
// own kernel send queue, then publishes until a write fails at its deadline, so the socket notes the
// failure (severableConn.writeFailed). nats.go then declares the connection stale and closes the
// socket, and reconnects. Once the link moves and the server has handled everything the stalled
// socket still sent, a subscriber of the row's own has not received the marker. The negative control
// is a close that does not read the failed write: the marker arrives.
func TestASocketWhoseWriteFailedSendsNothingMoreOnceTheLinkMoves(t *testing.T) {
	const subject = "testbus.write-failed.marker"
	marker := []byte("write-failed-marker")
	relay := testnats.StartStallRelay(t, testnats.URL(t))
	subscriber, received := markerSubscriber(t, subject)
	dialer := newSeverableDialer((&net.Dialer{}).DialContext)
	gaveUp := make(chan struct{}, 1)
	conn, err := nats.Connect(relay.URL(),
		nats.SetCustomDialer(dialer),
		nats.FlusherTimeout(500*time.Millisecond),
		nats.PingInterval(silentCutPingInterval),
		nats.MaxPingsOutstanding(silentCutMaxPingsOut),
		nats.ReconnectWait(time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(*nats.Conn, error) {
			select {
			case gaveUp <- struct{}{}:
			default:
			}
		}),
	)
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	t.Cleanup(conn.Close)
	stallWithMarker(t, relay, conn, subject, marker)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		payload := make([]byte, 64<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := conn.Publish("testbus.stall.filler", payload); err != nil {
				return
			}
		}
	}()
	testwait.Until(t, "a write of the stalled socket to fail at its deadline", func() (bool, bool) {
		failed := writeFailedOn(dialer)
		return failed, failed
	})
	testwait.Receive(t, "control: nats.go to give the stalled connection up after its failed write", gaveUp)
	if receivedOnceSettled(t, relay, subscriber, received, marker) {
		t.Fatal("the socket whose write failed sent the marker once the link moved: its close was not a reset")
	}
}

// writeFailedOn reports whether a socket dialer handed out noted a failed write.
func writeFailedOn(dialer *severableDialer) bool {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	for _, ref := range dialer.open {
		if conn := ref.Value(); conn != nil {
			conn.mu.Lock()
			failed := conn.writeFailed
			conn.mu.Unlock()
			if failed {
				return true
			}
		}
	}
	return false
}

// stalledPeer listens on loopback through a socket whose receive buffer is the smallest the kernel
// allows, and returns the address and a func that accepts one connection, which the row reads only
// once the socket under test has closed: until then the peer's window stays full after about a
// kilobyte, so what the dialer's socket writes after that stays in its own kernel send queue.
func stalledPeer(t *testing.T) (address string, accept func() net.Conn) {
	t.Helper()
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var set error
		if err := raw.Control(func(fd uintptr) {
			set = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1)
		}); err != nil {
			return err
		}
		return set
	}}
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the stalled peer: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() net.Conn {
		t.Helper()
		peer, err := listener.Accept()
		if err != nil {
			t.Fatalf("accept the dialer's socket: %v", err)
		}
		t.Cleanup(func() { _ = peer.Close() })
		return peer
	}
}

// queueMarker writes a 64 KiB filler and then marker on conn, each write into the kernel's send queue,
// so the marker waits there behind the stalled peer's full window. The writes' deadline is
// testwait.Context's, the test binary's deadline less testwait's margin.
func queueMarker(t *testing.T, conn net.Conn, marker []byte) {
	t.Helper()
	deadline, _ := testwait.Context(t).Deadline()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		t.Fatalf("set the write deadline: %v", err)
	}
	for _, write := range [][]byte{make([]byte, 64<<10), marker} {
		if _, err := conn.Write(write); err != nil {
			t.Fatalf("control: a write did not fit in the socket's send queue: %v", err)
		}
	}
}

// closeSendsMarker closes conn and reports whether the peer then reads marker: a normal close sends
// what the kernel still held, and a reset discards it. The peer's read deadline is
// testwait.Context's, the test binary's deadline less testwait's margin.
func closeSendsMarker(t *testing.T, conn, peer net.Conn, marker []byte) bool {
	t.Helper()
	// A reset closes the socket before severableConn closes it again, which answers that it is closed.
	_ = conn.Close()
	deadline, _ := testwait.Context(t).Deadline()
	if err := peer.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set the peer's read deadline: %v", err)
	}
	// A reset ends the read with an error once the peer has read what it had queued; a read that
	// runs into the deadline means neither came.
	read, err := io.ReadAll(peer)
	if err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read the peer: %v", err)
	}
	return bytes.Contains(read, marker)
}

// A socket the dialer hands out while a write is marked is marked too, so its close is a reset. This
// is the mark that covers each socket nats.go dials to reconnect (severableDialer.Dial). End to end it
// is a safeguard: the bus refuses a publish while nats.go reconnects (ReconnectBufSize -1, nats.go
// v1.55.0 :2426 and :4758), and nats.go drops what it had buffered for the old socket as it binds the
// new one (bindToNewConn, :2336), so a hand-back cannot reach a socket dialed after its mark. The row
// dials a stalled peer (stalledPeer), queues a marker behind the full window, and closes the socket.
// Two subtests: with nothing marked (the control), the close is normal and the peer reads the marker;
// with a write marked before the dial, the close is a reset and the peer never reads it. The negative
// control is a dial that does not take the dialer's mark: the second subtest fails.
func TestASocketDialedWhileAWriteIsMarkedIsResetAtClose(t *testing.T) {
	marker := []byte("dialed-while-marked")
	for _, marked := range []bool{false, true} {
		name := "nothing marked"
		if marked {
			name = "a write marked before the dial"
		}
		t.Run(name, func(t *testing.T) {
			address, accept := stalledPeer(t)
			dialer := newSeverableDialer((&net.Dialer{}).DialContext)
			if marked {
				dialer.mark()()
			}
			conn, err := dialer.Dial("tcp", address)
			if err != nil {
				t.Fatalf("dial the stalled peer: %v", err)
			}
			peer := accept()
			queueMarker(t, conn, marker)
			sent := closeSendsMarker(t, conn, peer, marker)
			switch {
			case marked && sent:
				t.Fatal("the socket dialed while a write was marked closed normally and sent the marker: the dial did not take the mark")
			case !marked && !sent:
				t.Fatal("control: the unmarked socket's close did not send the marker, so the row cannot tell a normal close from a reset")
			}
		})
	}
}

// A flush clears the marks only when every marked write had been taken as it began and none was
// marked since (severableDialer.confirmMarks): a write marked after the flush began goes out behind
// its PING, and one not yet taken goes out whenever nats.go takes it, so the flush's answer confirms
// neither. The row dials a stalled peer, queues a marker behind the full window, and confirms the
// marks with a markState read at the flush's start. Three subtests:
//   - nothing marked since the flush began (the control): the marks clear, and the close is normal, so
//     the peer reads the marker;
//   - a write marked after the flush began, and written: the marks stay, and the close is a reset;
//   - a marked write not yet written as the flush began: the marks stay, and the close is a reset.
//
// The negative control is a confirm that clears the marks whatever came after the flush began: the
// second and third subtests fail.
func TestAFlushConfirmsNoMarkMadeOrUnwrittenAsItBegan(t *testing.T) {
	marker := []byte("confirmed-or-not")
	for _, tc := range []struct {
		name    string
		confirm func(*severableDialer) bool
		cleared bool
	}{
		{name: "nothing marked since the flush began", cleared: true, confirm: func(d *severableDialer) bool {
			d.mark()()
			return d.confirmMarks(d.markState())
		}},
		{name: "a write marked after the flush began", confirm: func(d *severableDialer) bool {
			d.mark()()
			before := d.markState()
			d.mark()()
			return d.confirmMarks(before)
		}},
		{name: "a marked write not yet written as the flush began", confirm: func(d *severableDialer) bool {
			written := d.mark()
			before := d.markState()
			written()
			return d.confirmMarks(before)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, accept := stalledPeer(t)
			dialer := newSeverableDialer((&net.Dialer{}).DialContext)
			conn, err := dialer.Dial("tcp", address)
			if err != nil {
				t.Fatalf("dial the stalled peer: %v", err)
			}
			peer := accept()
			queueMarker(t, conn, marker)
			if cleared := tc.confirm(dialer); cleared != tc.cleared {
				t.Errorf("confirmMarks reported the marks cleared=%t, want %t", cleared, tc.cleared)
			}
			sent := closeSendsMarker(t, conn, peer, marker)
			switch {
			case tc.cleared && !sent:
				t.Error("control: the socket whose marks the flush cleared did not send the marker at its close, so the row cannot tell a normal close from a reset")
			case !tc.cleared && sent:
				t.Error("the socket closed normally and sent the marker: the flush cleared a mark it had not confirmed")
			}
		})
	}
}

// ConfirmMarked confirms only a mark whose write nats.go had taken as the flush began, even on a
// healthy connection whose flush answers at once: it reports confirmed only when the flush cleared
// every mark (severableDialer.confirmMarks), and a write still to be taken can go out behind the
// flush's PING. The row marks a write on a client of the test's server and, before the write is
// taken, requires ConfirmMarked false and the mark still set; once the write is taken (written), it
// requires ConfirmMarked true and no mark left, which is the control that the flush answers. The
// negative control is a ConfirmMarked that answers true whenever its flush answers, whatever the
// marks: the first half fails and the second stays quiet.
func TestConfirmMarkedConfirmsNoMarkedWriteNatsGoHasNotTaken(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect to the test's server: %v", err)
	}
	t.Cleanup(client.Close)
	written := client.MarkUnconfirmed()
	if client.ConfirmMarked(10 * time.Second) {
		t.Error("ConfirmMarked reported every mark confirmed while the marked write had not been taken by nats.go")
	}
	if !client.CarriesUnconfirmed() {
		t.Error("the mark of a write nats.go had not taken was cleared")
	}
	written()
	if !client.ConfirmMarked(10 * time.Second) {
		t.Error("control: ConfirmMarked did not confirm once the marked write was taken and the flush answered")
	}
	if client.CarriesUnconfirmed() {
		t.Error("control: a mark is left after a flush that confirmed every marked write")
	}
}
