package bus

import (
	"bytes"
	"net"
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

// awaitWritten waits until nats.go has written everything conn had buffered into its socket.
func awaitWritten(t *testing.T, conn *nats.Conn, what string) {
	t.Helper()
	testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, what+" written into the socket", func() bool {
		buffered, err := conn.Buffered()
		if err != nil {
			t.Fatalf("read what nats.go holds of %s: %v", what, err)
		}
		return buffered == 0
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
	testwait.WithinPolling(t, 30*time.Second, 50*time.Millisecond, "a write of the stalled socket failed at its deadline", func() bool {
		return writeFailedOn(dialer)
	})
	select {
	case <-gaveUp:
	case <-time.After(30 * time.Second):
		t.Fatal("control: nats.go did not give the stalled connection up within 30 s of its failed write")
	}
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
