package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A direct call on the client's connection that a forced reconnect cut off is lost however late its
// answer is read. The keeper of a service's token reconnects with ForceReconnect to present a new
// token (natscred), which answers every pending flush nats.ErrConnectionClosed (FlushTimeout,
// nats.go v1.55.0 :6129-6131) and every pending request nats.ErrDisconnected (ForceReconnect's
// clearPendingRequestCalls, :2667; newRequest, :4962-4966), where v1.53.1 answered requests
// nats.ErrConnectionClosed too, and reconnects in place. The answer is read only once nats.go
// reads the connection connected again, so its state says nothing of the reconnect: a close answer
// for a call made on conn itself is a loss of conn (lostIfClosed, closeAnswer). A publish's request
// a forced reconnect cut off is one whose answer never came, so the stream may hold it
// (sendUnanswered). The row makes a real request on the connection with the server's replies held
// (testnats.Proxy.FreezeServerReplies), so the request is pending when the reconnect clears it,
// and reads nats.go's own answer. Every wait is bounded by the test's deadline alone.
//
// The controls are closeAnswer without nats.ErrDisconnected (the request's answer stays nats.go's
// "server is disconnected", which a caller reads as lasting), sendUnanswered without it (the
// answer reads as the server's own), and this row's first check under nats.go v1.53.1, whose
// answer is nats.ErrConnectionClosed.
func TestADirectCallAForcedReconnectCutOffIsLostOnceTheConnectionIsBack(t *testing.T) {
	proxy := testnats.StartProxy(t, testnats.URL(t))
	client, err := Connect([]string{proxy.URL()})
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	conn := client.Connection()
	before := conn.Stats().OutMsgs
	proxy.FreezeServerReplies()
	answered := make(chan error, 1)
	go func() {
		_, err := conn.Request("envoy.lost-direct.no-answer", nil, testnats.Remaining(t))
		answered <- err
	}()
	if !testwait.Holds(testnats.Remaining(t), time.Millisecond, func() bool { return conn.Stats().OutMsgs > before }) {
		t.Fatal("the request did not go out with the server's replies held by the test's deadline")
	}
	if err := conn.ForceReconnect(); err != nil {
		t.Fatalf("force a reconnect: %v", err)
	}
	var raw error
	select {
	case raw = <-answered:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the request the forced reconnect cut off did not return by the test's deadline")
	}
	proxy.Restore()
	if !errors.Is(raw, nats.ErrDisconnected) {
		t.Fatalf("nats.go answered the request a forced reconnect cut off %v, want nats.ErrDisconnected", raw)
	}
	waitForNatsStatus(t, conn, "connected again after the forced reconnect", func(s nats.Status) bool { return s == nats.CONNECTED })
	if got := client.lostIfClosed(raw, conn, false); !errors.Is(got, ErrConnectionLost) {
		t.Fatalf("a request a forced reconnect answered %v, read once the connection was back, answered %v, want ErrConnectionLost", raw, got)
	}
	if !sendUnanswered(raw) {
		t.Fatalf("a publish a forced reconnect answered %v is not counted as one whose answer never came, so a caller would read its copy as unstored", raw)
	}
	if got := client.lostIfClosed(nats.ErrConnectionClosed, conn, false); !errors.Is(got, ErrConnectionLost) {
		t.Fatalf("a flush a forced reconnect answered %v, read once the connection was back, answered %v, want ErrConnectionLost", nats.ErrConnectionClosed, got)
	}
	if got := client.lostIfClosed(raw, nil, false); !errors.Is(got, nats.ErrDisconnected) || errors.Is(got, ErrConnectionLost) {
		t.Fatalf("a call on the handle of a replaced connection (nil) answered %v, want nats.go's %v", got, raw)
	}
}
