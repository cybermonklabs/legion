package bus

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
)

// Through a silent cut each reconnect attempt holds nats.go's lock of the connection across its
// dial and handshake, up to connectTimeout, and a read of the connection's status waits that lock
// out. Connected waits with it, but without the client's own mutex, so ConnectionChanged, which
// the client's connection callbacks and every waiter on a connection event take that mutex for,
// still answers at once beside it; and ConnectedWithin answers not connected within its bound.
func TestAStatusReadWaitingOnAReconnectAttemptHoldsNothingElse(t *testing.T) {
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	client, err := Connect([]string{proxy.URL()}, WithSilentCutDetection())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	proxy.Freeze()
	t.Cleanup(proxy.Restore)

	// The pings find the cut two to three seconds in, and the first attempt starts a ReconnectWait
	// later: wait for a read of the status that does not answer, which is that attempt.
	const waiting = 500 * time.Millisecond
	deadline := time.Now().Add(30 * time.Second)
	for {
		read := make(chan struct{})
		go func() {
			client.Connected()
			close(read)
		}()
		select {
		case <-read:
			if time.Now().After(deadline) {
				t.Fatal("no read of the connection's status waited within 30 s of the freeze")
			}
			time.Sleep(50 * time.Millisecond)
			continue
		case <-time.After(waiting):
		}
		changing := time.Now()
		client.ConnectionChanged()
		if took := time.Since(changing); took > 100*time.Millisecond {
			t.Errorf("ConnectionChanged took %v beside a status read waiting on a reconnect attempt, want at once", took.Round(time.Millisecond))
		}
		const bound = 100 * time.Millisecond
		reading := time.Now()
		connected := ConnectedWithin(client, bound)
		took := time.Since(reading)
		t.Logf("beside a status read waiting on a reconnect attempt, ConnectedWithin answered %t in %v", connected, took.Round(time.Millisecond))
		if connected {
			t.Error("ConnectedWithin answered connected for a client in the middle of a reconnect attempt")
		}
		if took > bound+400*time.Millisecond {
			t.Errorf("ConnectedWithin answered in %v, past its %v bound", took.Round(time.Millisecond), bound)
		}
		<-read
		return
	}
}
