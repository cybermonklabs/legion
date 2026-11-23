package bus_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testsocks5"
)

// A call whose wait for a connection dials one through the SOCKS5 proxy to replace the running
// client's closed connection, and finds the proxy refusing every dial, answers ErrConnectionLost,
// as the same dial without a proxy does (TestADialThatFindsNoServerToReplaceAClosedConnectionIsLost):
// the proxy's own dial error names what failed, and nats.go's ErrNoServers stays in its chain
// (connectWithContext), which is what the client reads as its connection's loss (lostIfClosed). Once
// the proxy is closed nothing listens at its address, so every dial is refused, which the bus answers
// only once its ten attempts have run out, about ten seconds. The answer still names the proxy's
// address. Reconnected matches it once the proxy serves again at that address (testsocks5's
// Reopen), and when it returns the client is connected and the proxy has accepted a connection
// since it served again: the row reads both before its next call, which would dial a connection of
// its own. The call made then succeeds through the proxy.
//
// The controls: this row with connectWithContext putting the proxy's dial error in ErrNoServers's
// place, where the subscribe answers that dial error alone, which no caller retries; this row
// with the proxy left closed, where Reconnected's own dial finds no server and answers false; and
// this row with Reconnected's own dial taken out (it answers true without ensureConnWithContext),
// where the client is not connected when it returns.
func TestADialThroughTheProxyThatFindsNoServerToReplaceAClosedConnectionIsLost(t *testing.T) {
	uri := testnats.URL(t)
	proxy := testsocks5.Start(t, hostOf(t, uri))
	client, err := bus.Connect([]string{withHost(t, uri, unresolvable)}, bus.WithEnvironment(proxied(proxy.Addr)))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	proxy.Close()
	client.Connection().Close()
	const subject = "envoy.closelost.socks5"
	_, err = client.SubscribeCore(subject, func(*nats.Msg) {})
	if !errors.Is(err, bus.ErrConnectionLost) {
		t.Fatalf("a subscribe whose dial through the proxy found no server answered %v, want ErrConnectionLost", err)
	}
	if !strings.Contains(err.Error(), proxy.Addr) {
		t.Fatalf("the lost subscribe's error %q does not name the proxy %s", err, proxy.Addr)
	}
	accepted := proxy.Accepted()
	proxy.Reopen(t)
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	if !bus.Reconnected(ctx, err) {
		t.Fatalf("Reconnected did not match %v once the proxy served again", err)
	}
	if !client.Connected() {
		t.Fatal("Reconnected answered true and the client is not connected")
	}
	if proxy.Accepted() <= accepted {
		t.Fatalf("the proxy accepted no connection between serving again and Reconnected's answer (%d before, %d now), so the client did not reconnect through it", accepted, proxy.Accepted())
	}
	if _, err := client.SubscribeCore(subject, func(*nats.Msg) {}); err != nil {
		t.Fatalf("the subscribe made once the client reconnected through the proxy answered %v", err)
	}
}
