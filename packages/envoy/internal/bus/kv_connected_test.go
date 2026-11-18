package bus

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// connectedLateBucket is the bucket the rows below open, one server per row.
const connectedLateBucket = "envoy_connected_late_test"

// reconnectingConn is a connection to the server at uri that reconnects to it for ever, a step of
// 50 ms apart, as every connection a program of the deployment holds reconnects (options): the
// pace only shortens the row, since the open waits for the connection's own status event.
func reconnectingConn(t *testing.T, uri string) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	conn, err := nats.Connect(uri, nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return conn, js
}

// stopped stops server and waits until conn has seen it go.
func stopped(t *testing.T, server *testnats.Server, conn *nats.Conn) {
	t.Helper()
	server.Stop(t)
	testwait.Eventually(t, "the connection to leave CONNECTED once its server stopped", func() bool {
		return conn.Status() != nats.CONNECTED
	})
}

// A bucket opened on a connection that is reconnecting is opened once the connection is back: the
// open waits for the connection, on its status events, rather than send its lookup on a
// connection nats.go refuses as a server older than 2.6.2 while it is not CONNECTED. That refusal
// ended the listener's start under load ("KV_envoy_roles create: nats: key-value requires at least
// server version 2.6.2", INFRA-4240's CPU-starvation control, 2026-11-17). The row's connection is
// made connected late: its server is stopped before the open and started once the open has armed
// its wait for the connection, on the row's still clock, so nothing but the server's return ends
// that wait.
func TestABucketOpenedWhileTheConnectionReconnectsOpensOnceItIsBack(t *testing.T) {
	server, uri := testnats.Start(t)
	conn, js := reconnectingConn(t, uri)
	stopped(t, server, conn)

	still := stillClock()
	answered := ensuredOn(still, conn, js, &nats.KeyValueConfig{Bucket: connectedLateBucket})
	waitsAt := still.Now().Add(RequestTimeout)
	testwait.Until(t, "the open to arm its wait for the connection, or to return", func() ([]time.Time, bool) {
		select {
		case err := <-answered:
			t.Fatalf("the open on a reconnecting connection returned before it waited for the connection: %v", err)
		default:
		}
		armed := still.Armed()
		for _, at := range armed {
			if at.Equal(waitsAt) {
				return armed, true
			}
		}
		return armed, false
	})

	server.Start(t)
	if err := testwait.Receive(t, "the open once the connection is back", answered); err != nil {
		t.Fatalf("the open on a connection made connected late = %v, want the bucket opened", err)
	}
	if _, err := js.KeyValue(connectedLateBucket); err != nil {
		t.Fatalf("the bucket the open created: %v", err)
	}
}

// The wait for the connection ends, and the refusal is real, in the two cases where it is: a
// connection that is not back within the wait fails the open naming the step and the wait, so a
// start whose NATS never returns exits as it did and never hangs; and a connection to a server that
// is older than 2.6.2 is refused at once, its version read while it is connected, with no wait at
// all.
func TestABucketOpenWaitsForTheConnectionOnlyWhileItCanComeBack(t *testing.T) {
	t.Run("a connection that is not back within the wait", func(t *testing.T) {
		server, uri := testnats.Start(t)
		conn, js := reconnectingConn(t, uri)
		stopped(t, server, conn)

		still := stillClock()
		answered := ensuredOn(still, conn, js, &nats.KeyValueConfig{Bucket: connectedLateBucket})
		waitsAt := still.Now().Add(RequestTimeout)
		testwait.Until(t, "the open to arm its wait for the connection", func() ([]time.Time, bool) {
			armed := still.Armed()
			for _, at := range armed {
				if at.Equal(waitsAt) {
					return armed, true
				}
			}
			return armed, false
		})
		still.Advance(RequestTimeout)
		err := testwait.Receive(t, "the open once its wait for the connection has passed", answered)
		want := "KV_" + connectedLateBucket + " STREAM.INFO: the NATS connection was not connected within 10s"
		if err == nil || !strings.HasPrefix(err.Error(), want) || !errors.Is(err, ErrNotConnected) {
			t.Fatalf("the open whose connection never came back = %v, want an error beginning %q", err, want)
		}
	})

	t.Run("a connected server older than 2.6.2", func(t *testing.T) {
		conn, js := connectedToAServerOf(t, "2.6.0")
		still := stillClock()
		answered := ensuredOn(still, conn, js, &nats.KeyValueConfig{Bucket: connectedLateBucket})
		err := testwait.Until(t, "the open on a server too old to answer, with no wait armed", func() (error, bool) {
			if armed := still.Armed(); len(armed) != 0 {
				t.Fatalf("the open on a connected server older than 2.6.2 armed a wait for the connection (%v), want the server's refusal at once", armed)
			}
			select {
			case err := <-answered:
				return err, true
			default:
				return nil, false
			}
		})
		want := "KV_" + connectedLateBucket + " STREAM.INFO: nats: key-value requires at least server version 2.6.2"
		if err == nil || err.Error() != want {
			t.Fatalf("the open on a connected server older than 2.6.2 = %v, want %q", err, want)
		}
	})
}

// connectedToAServerOf connects to a fake server that names version in its INFO, completes the
// handshake and answers every PING, and holds the connection open until the test ends: a
// connection that is CONNECTED to a server of that version, as far as nats.go can tell.
func connectedToAServerOf(t *testing.T, version string) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, `INFO {"server_id":"fake","version":"`+version+`","proto":1,"max_payload":1048576}`+"\r\n")
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(line, "PING") {
						_, _ = io.WriteString(conn, "PONG\r\n")
					}
				}
			}()
		}
	}()
	conn, err := nats.Connect("nats://"+listener.Addr().String(), nats.MaxReconnects(0))
	if err != nil {
		t.Fatalf("connect to the fake server: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return conn, js
}
