package testnats

import (
	"net"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"
)

// HoldStalledWrite returns only once a write of nats.go's is in the client's socket and has not come
// back: the row counts, at the socket, the writes begun and those that returned, and reads them once
// HoldStalledWrite has returned. A publish that holds nats.go's connection lock without writing is
// no stalled write, however long it holds it. The negative control is nats.go's publish held 300 ms
// under its lock before it writes (the stall:300 forcing of forced-pod.sh, an overlay of nats.go): a
// HoldStalledWrite that takes the lock held for 200 ms by a publish inside nats.go to be a stalled
// write returns while that publish sleeps, with every write at the socket returned, and fails this
// row. The row has a file of its own, and its dialer with it, so the control overlays it on a tree
// without it.
func TestHoldStalledWriteReturnsWithAWriteStillInTheSocket(t *testing.T) {
	RequireSocketQueues(t)
	hold := StartHandshakeHold(t, URL(t))
	dialer := &writeCountingDialer{}
	conn, err := natsgo.Connect(hold.URL(), natsgo.SetCustomDialer(dialer), natsgo.NoReconnect())
	if err != nil {
		t.Fatalf("connect through the hold: %v", err)
	}
	t.Cleanup(conn.Close)
	release := hold.HoldStalledWrite(t, conn)
	t.Cleanup(release)
	if begun, returned := dialer.begun.Load(), dialer.returned.Load(); begun == returned {
		t.Fatalf("HoldStalledWrite returned with no write in the client's socket: %d write(s) begun there, each of them returned", begun)
	}
}

// writeCountingDialer dials as nats.go's own dialer does, with nats.go's default timeout (as
// giveUpDialer dials), and counts the writes begun on the sockets it hands out and those that
// returned.
type writeCountingDialer struct {
	begun, returned atomic.Int64
}

func (d *writeCountingDialer) Dial(network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: natsgo.DefaultTimeout}).Dial(network, address)
	if err != nil {
		return nil, err
	}
	return &writeCountingConn{Conn: conn, dialer: d}, nil
}

// writeCountingConn is a socket whose writes its dialer counts.
type writeCountingConn struct {
	net.Conn
	dialer *writeCountingDialer
}

func (c *writeCountingConn) Write(p []byte) (int, error) {
	c.dialer.begun.Add(1)
	defer c.dialer.returned.Add(1)
	return c.Conn.Write(p)
}
