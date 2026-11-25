package testnats

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// proxiedClient connects to the proxy's URL as a deployed client does, reconnecting for as long as
// the test runs, and reports each disconnect and reconnect on the returned channels. A dial that
// fails is made again by the shared wait, as Connect's is.
func proxiedClient(t *testing.T, proxy *Proxy) (conn *natsgo.Conn, disconnected, reconnected <-chan struct{}) {
	t.Helper()
	down, up := make(chan struct{}, 8), make(chan struct{}, 8)
	dial := testwait.Until(t, fmt.Sprintf("a connection through the proxy at %s", proxy.URL()), func() (dialAttempt, bool) {
		conn, err := natsgo.Connect(proxy.URL(),
			natsgo.MaxReconnects(-1),
			// nats.go's own wait between reconnect attempts, shortened from its two seconds so a test
			// that waits for a reconnect is not held by it; no verdict here rests on an attempt not
			// being made within it.
			natsgo.ReconnectWait(50*time.Millisecond),
			natsgo.DisconnectErrHandler(func(*natsgo.Conn, error) { down <- struct{}{} }),
			natsgo.ReconnectHandler(func(*natsgo.Conn) { up <- struct{}{} }),
		)
		return dialAttempt{conn: conn, err: err}, err == nil
	})
	t.Cleanup(dial.conn.Close)
	return dial.conn, down, up
}

// A proxy relays a client's connection, credentials and all, until Cut: the connection drops at
// once and every dial is refused while it stays cut. Restore lets the client reconnect, and a new
// client connect, through the same URL.
func TestAProxyRelaysUntilCutAndAgainOnceRestored(t *testing.T) {
	proxy := StartProxy(t, URL(t))
	conn, disconnected, reconnected := proxiedClient(t, proxy)
	if err := conn.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("flush through the proxy: %v", err)
	}

	proxy.Cut()
	testwait.Receive(t, "the client's disconnect after the cut", disconnected)
	// The dial's own timer is what is left of the shared wait (left), so only the cut proxy's
	// refusal, which comes at once, ends it with an error: a dial that would get through, however
	// slowly, does.
	if refused, err := natsgo.Connect(proxy.URL(), natsgo.NoReconnect(), natsgo.Timeout(left(t))); err == nil {
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
	if err := conn.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("flush after the restore: %v", err)
	}
	Connect(t, proxy.URL()).Close()
}

// A frozen proxy keeps the client connected and moves nothing: what the client publishes does not
// reach a subscriber, and a flush gets no answer. Restore delivers what was held, and the
// connection that stayed open answers again.
func TestAFrozenProxyKeepsTheConnectionAndMovesNothingUntilRestored(t *testing.T) {
	uri := URL(t)
	proxy := StartProxy(t, uri)
	conn, _, _ := proxiedClient(t, proxy)
	direct := Connect(t, uri)
	t.Cleanup(direct.Close)
	received := make(chan *natsgo.Msg, 4)
	if _, err := direct.ChanSubscribe("proxy.frozen", received); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := direct.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	proxy.Freeze()
	if err := conn.Publish("proxy.frozen", []byte("held")); err != nil {
		t.Fatalf("publish through the frozen proxy: %v", err)
	}
	// nats.go's own request timer is the span that proves the proxy answered nothing: a PONG the
	// server never sent leaves no state to read, and nothing the test does later orders against it.
	if err := conn.FlushTimeout(500 * time.Millisecond); err == nil {
		t.Fatal("a flush through the frozen proxy was answered")
	}
	// The marker reaches the server on the subscriber's own connection, which no freeze holds, and
	// is published after the held one, so a publish a leaking freeze let through is on the channel
	// before it.
	if err := direct.Publish("proxy.frozen", []byte("marker")); err != nil {
		t.Fatalf("publish the marker through the unfrozen connection: %v", err)
	}
	if msg := testwait.Receive(t, "the marker published after the frozen publish", received); string(msg.Data) != "marker" {
		t.Fatalf("the subscriber received %q before the marker: a frozen proxy moved it", msg.Data)
	}
	// A freeze that dropped the client would leave it RECONNECTING, which this reads.
	if !conn.IsConnected() {
		t.Fatalf("the client's status is %v behind a frozen proxy, want CONNECTED", conn.Status())
	}

	proxy.Restore()
	if err := conn.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("flush after the restore: %v", err)
	}
	if msg := testwait.Receive(t, "the publish the freeze held, after the restore", received); string(msg.Data) != "held" {
		t.Fatalf("the subscriber received %q after the restore, want the held publish", msg.Data)
	}
}

// echoListener copies back everything it is sent, the far end of the round trip a latency proxy's
// holding is measured over: the proxy relays bytes whatever protocol they belong to, so this needs
// no NATS, and the test then drives the clock with no handshake of its own to carry.
func echoListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the echo: %v", err)
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
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// ProxyLatency holds each chunk it relays for half the round trip, on the proxy's own clock, so a
// round trip through the proxy costs rtt: the test's fake clock arms one deadline for each leg,
// half the round trip away from the moment that leg's bytes were read, and nothing moves until the
// test moves the clock to it.
func TestAProxyWithLatencyHoldsEachLegHalfTheRoundTrip(t *testing.T) {
	const rtt = 200 * time.Millisecond
	start := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	fake := clocktest.New(start)
	proxy := StartProxy(t, "nats://"+echoListener(t), ProxyLatency(rtt), ProxyClock(fake))
	client, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL(), "nats://"))
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	echoed := make(chan string, 1)
	go func() {
		answer := make([]byte, len("ping"))
		if _, err := io.ReadFull(client, answer); err != nil {
			echoed <- "the read of the echo failed: " + err.Error()
			return
		}
		echoed <- string(answer)
	}()

	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("write through the proxy: %v", err)
	}
	armed := fake.AwaitArmed(t, "the proxy holding the leg to the server", 1)
	if want := start.Add(rtt / 2); !armed[0].Equal(want) {
		t.Fatalf("the proxy holds the leg to the server until %s, want %s, half the %s round trip after it read the bytes", armed[0], want, rtt)
	}
	fake.Advance(rtt / 2)
	armed = fake.AwaitArmed(t, "the proxy holding the leg back to the client", 1)
	if want := start.Add(rtt); !armed[0].Equal(want) {
		t.Fatalf("the proxy holds the leg back until %s, want %s, half the %s round trip after the echo answered", armed[0], want, rtt)
	}
	fake.Advance(rtt / 2)
	if answer := testwait.Receive(t, "the bytes echoed back through the proxy", echoed); answer != "ping" {
		t.Fatalf("the client read %q through the proxy, want the bytes it wrote", answer)
	}
}

// sinkListener takes every connection's bytes and counts them, the far end a rate proxy's
// throughput is read at: the proxy relays bytes whatever protocol they belong to, so this needs no
// NATS, and the test drives the proxy's clock with no handshake of its own to carry.
func sinkListener(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the sink: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var received atomic.Int64
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buffer := make([]byte, 32<<10)
				for {
					read, err := conn.Read(buffer)
					received.Add(int64(read))
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String(), &received
}

// ProxyRate moves no faster than its rate, on the proxy's own clock: after each read it relays it
// waits the read's length at the rate before it reads again, so a payload of twice the rate
// crosses only once the test has moved the clock through at least that payload's time at the
// rate. The proxy forwards each read before it waits for it, so the last read of the payload, up
// to proxyReadBuffer bytes, crosses uncharged; the bound allows exactly that and nothing more.
// The test moves the fake to each wait the proxy arms and sums the waits, so what it reads is the
// proxy's own charge for the bytes and no machine's speed.
func TestAProxyWithARateMovesNoFasterThanIt(t *testing.T) {
	const rate = 200_000
	fake := clocktest.New(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	sink, received := sinkListener(t)
	proxy := StartProxy(t, "nats://"+sink, ProxyRate(rate), ProxyClock(fake))
	client, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL(), "nats://"))
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	total := int64(2 * rate)
	// The write backs up behind the proxy's rate, so it runs beside the test, which moves the
	// clock; a write that failed leaves the sink short, which the wait below names.
	go func() { _, _ = client.Write(make([]byte, total)) }()

	var charged time.Duration
	for {
		crossed := testwait.Until(t, "the proxy's next wait on its rate, or the whole payload across it", func() (string, bool) {
			if got := received.Load(); got >= total {
				return "all", true
			} else if len(fake.Armed()) == 0 {
				return fmt.Sprintf("%d of %d bytes across, and no wait armed", got, total), false
			}
			return "a wait armed", true
		})
		if crossed == "all" {
			break
		}
		at := fake.Armed()[0]
		charged += at.Sub(fake.Now())
		fake.Set(at)
	}
	if least := time.Duration(total-proxyReadBuffer) * time.Second / rate; charged < least {
		t.Fatalf("the proxy charged %v on its clock for %d bytes at %d B/s, want at least %v", charged, total, rate, least)
	}
}
