package testnats

import (
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testwait"
)

// proxiedClient connects to the proxy's URL as a deployed client does, reconnecting for as long as
// the test runs, and reports each disconnect and reconnect on the returned channels.
func proxiedClient(t *testing.T, proxy *Proxy) (conn *natsgo.Conn, disconnected, reconnected <-chan struct{}) {
	t.Helper()
	down, up := make(chan struct{}, 8), make(chan struct{}, 8)
	conn, err := natsgo.Connect(proxy.URL(),
		natsgo.Timeout(dialTimeout),
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(50*time.Millisecond),
		natsgo.DisconnectErrHandler(func(*natsgo.Conn, error) { down <- struct{}{} }),
		natsgo.ReconnectHandler(func(*natsgo.Conn) { up <- struct{}{} }),
	)
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn, down, up
}

// A proxy relays a client's connection, credentials and all, until Cut: the connection drops at
// once and every dial is refused while it stays cut. Restore lets the client reconnect, and a new
// client connect, through the same URL.
func TestAProxyRelaysUntilCutAndAgainOnceRestored(t *testing.T) {
	proxy := StartProxy(t, URL(t))
	conn, disconnected, reconnected := proxiedClient(t, proxy)
	if err := conn.FlushTimeout(time.Second); err != nil {
		t.Fatalf("flush through the proxy: %v", err)
	}

	proxy.Cut()
	testwait.Receive(t, "the client's disconnect after the cut", disconnected)
	if refused, err := natsgo.Connect(proxy.URL(), natsgo.Timeout(dialTimeout), natsgo.NoReconnect()); err == nil {
		refused.Close()
		t.Fatal("a client connected through a cut proxy")
	}
	// The client tried to reconnect and the cut proxy refused it, which the proxy's own count of
	// refused dials says with no span: the row's own dial through the cut proxy, just above, is one,
	// and any later one is the client's reconnect. A window of 300 ms (before) could pass before the
	// client's first reconnect, whose wait is 50 ms plus up to 100 ms of jitter and a handshake, came
	// at all on a loaded machine. A reconnect that comes first ends the wait and fails the row then,
	// so a cut proxy that relays the client's reconnect fails it at once, not at the test's deadline.
	var through bool
	testwait.Until(t, "the cut proxy to refuse the client's reconnect", func() (int, bool) {
		select {
		case <-reconnected:
			through = true
			return 0, true
		default:
		}
		refused := proxy.refusedDials()
		return refused, refused >= 2
	})
	if through {
		t.Fatal("the client reconnected through a cut proxy")
	}

	proxy.Restore()
	testwait.Receive(t, "the client's reconnect after the restore", reconnected)
	if err := conn.FlushTimeout(time.Second); err != nil {
		t.Fatalf("flush after the restore: %v", err)
	}
	fresh, err := natsgo.Connect(proxy.URL(), natsgo.Timeout(dialTimeout), natsgo.NoReconnect())
	if err != nil {
		t.Fatalf("connect through the restored proxy: %v", err)
	}
	fresh.Close()
}

// A frozen proxy keeps the client connected and moves nothing: what the client publishes does not
// reach a subscriber, and a flush gets no answer. Restore delivers what was held, and the
// connection that stayed open answers again.
func TestAFrozenProxyKeepsTheConnectionAndMovesNothingUntilRestored(t *testing.T) {
	uri := URL(t)
	proxy := StartProxy(t, uri)
	conn, disconnected, _ := proxiedClient(t, proxy)
	direct := Connect(t, uri)
	t.Cleanup(direct.Close)
	received := make(chan *natsgo.Msg, 4)
	if _, err := direct.ChanSubscribe("proxy.frozen", received); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := direct.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	proxy.Freeze()
	if err := conn.Publish("proxy.frozen", []byte("held")); err != nil {
		t.Fatalf("publish through the frozen proxy: %v", err)
	}
	if err := conn.FlushTimeout(500 * time.Millisecond); err == nil {
		t.Fatal("a flush through the frozen proxy was answered")
	}
	select {
	case msg := <-received:
		t.Fatalf("the subscriber received %q through a frozen proxy", msg.Data)
	case <-disconnected:
		t.Fatal("the client was disconnected by a freeze")
	case <-time.After(300 * time.Millisecond):
	}
	if !conn.IsConnected() {
		t.Fatalf("the client's status is %v behind a frozen proxy, want CONNECTED", conn.Status())
	}

	proxy.Restore()
	if err := conn.FlushTimeout(2 * time.Second); err != nil {
		t.Fatalf("flush after the restore: %v", err)
	}
	select {
	case msg := <-received:
		if string(msg.Data) != "held" {
			t.Fatalf("the subscriber received %q, want the held publish", msg.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the publish the freeze held never arrived after the restore")
	}
}

// ProxyLatency adds its round trip to every round trip through the proxy.
func TestAProxyWithLatencyAddsItsRoundTrip(t *testing.T) {
	const rtt = 200 * time.Millisecond
	proxy := StartProxy(t, URL(t), ProxyLatency(rtt))
	conn, _, _ := proxiedClient(t, proxy)
	started := time.Now()
	if err := conn.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush through the proxy: %v", err)
	}
	if took := time.Since(started); took < rtt {
		t.Fatalf("a round trip through the proxy took %v, want at least the %v it adds", took, rtt)
	}
}

// ProxyRate moves no faster than its rate: a payload of twice the rate takes about two seconds to
// cross the proxy, where a proxy with no rate moves it at once. The proxy forwards each read before
// it sleeps for it, so the last read of the payload, up to proxyReadBuffer bytes, crosses
// uncharged; the bound allows exactly that and nothing more.
func TestAProxyWithARateMovesNoFasterThanIt(t *testing.T) {
	const rate = 200_000
	uri := URL(t)
	proxy := StartProxy(t, uri, ProxyRate(rate))
	conn, _, _ := proxiedClient(t, proxy)
	direct := Connect(t, uri)
	t.Cleanup(direct.Close)
	var received atomic.Int64
	if _, err := direct.Subscribe("proxy.rate", func(m *natsgo.Msg) { received.Add(int64(len(m.Data))) }); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := direct.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	payload := make([]byte, 16<<10)
	const chunks = 2 * rate / (16 << 10)
	total := int64(chunks * len(payload))
	started := time.Now()
	for range chunks {
		if err := conn.Publish("proxy.rate", payload); err != nil {
			t.Fatalf("publish through the proxy: %v", err)
		}
	}
	for received.Load() < total {
		if time.Since(started) > 10*time.Second {
			t.Fatalf("%d of %d bytes crossed the proxy in 10 s", received.Load(), total)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if took, least := time.Since(started), time.Duration(total-proxyReadBuffer)*time.Second/rate; took < least {
		t.Fatalf("%d bytes crossed a proxy of %d B/s in %v, want at least %v", total, rate, took, least)
	}
}
