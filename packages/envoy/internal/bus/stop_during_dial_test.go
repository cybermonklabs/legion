package bus_test

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A recovery that is dialling when Drain stops the client neither installs the connection its dial
// produces nor reports the stop as a failure: the connection would be one nothing drains, and the
// stop is a shutdown. client.Conn.Close() stands in for nats.go closing the connection itself (the
// same authorization error twice, or an unclassified server -ERR), which is what starts that
// recovery in production.
func TestARecoveryStoppedMidDialInstallsNothingAndLogsNoError(t *testing.T) {
	_, uri := startNATS(t)
	proxy := startRelayProxy(t, uri)
	clientLogs := captureBusLogs()
	client, err := bus.ConnectOwningStream([]string{proxy.url()}, bus.WithLogger(clientLogs.logger))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	logs := clientLogs.fromHere()

	proxy.delay.Store(int64(time.Second))
	client.Conn.Close()
	testwait.Receive(t, "the recovery to dial", proxy.held)
	_ = client.Drain(5 * time.Second)
	time.Sleep(2 * time.Second)

	if client.Connected() {
		t.Fatal("the recovery installed the connection it dialled after Drain stopped the client")
	}
	if line := logs.errorLine(); line != "" {
		t.Fatalf("a recovery stopped by Drain logged an error: %s", line)
	}
}

// A recovery whose dial is failing when Close stops the client ends at the stop: it logs the
// cancellation and never a reconnect failure. Its dial retries a lost server for up to ten
// attempts, so without that the recovery would outlive the client by seconds and then report a
// shutdown as an ERROR. client.Conn.Close() stands in for nats.go closing the connection itself,
// as in the test above.
func TestARecoveryDiallingALostServerEndsAtTheStop(t *testing.T) {
	_, uri := startNATS(t)
	proxy := startRelayProxy(t, uri)
	clientLogs := captureBusLogs()
	client, err := bus.ConnectOwningStream([]string{proxy.url()}, bus.WithLogger(clientLogs.logger))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	logs := clientLogs.fromHere()

	proxy.refuse.Store(true)
	proxy.delay.Store(int64(500 * time.Millisecond))
	client.Conn.Close()
	testwait.Receive(t, "the recovery to dial", proxy.held)
	client.Close()

	// The bound under test, not a wait: a recovery that ends at the stop logs its cancellation at
	// once, and one that dials on makes its ten attempts, a second apart, first.
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), "envoy nats recovery cancelled") {
		if time.Now().After(deadline) {
			t.Fatal("the recovery was still dialling 3s after Close stopped the client")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	if line := logs.errorLine(); line != "" {
		t.Fatalf("a recovery stopped by Close logged an error: %s", line)
	}
}
