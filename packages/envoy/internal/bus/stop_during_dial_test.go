package bus_test

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logtest"
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
	client, err := bus.ConnectOwningStream([]string{proxy.url()})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	var logs bytes.Buffer
	var logsMu sync.Mutex
	logtest.SetDefault(t, slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logsMu.Lock()
		defer logsMu.Unlock()
		return logs.Write(p)
	}), nil))

	letGo := proxy.holdDials(t)
	client.Conn.Close()
	testwait.Receive(t, "the recovery to dial", proxy.held)
	_ = client.Drain(listenerDrain)
	letGo()
	// The recovery's dial comes back once the hold is let go, and the recovery then finds the
	// client stopped and says so; nothing of it is left to install a connection after that.
	testwait.Eventually(t, "the recovery stopped by Drain to end", func() bool { return logged(&logsMu, &logs, "envoy nats recovery cancelled") })

	if client.Connected() {
		t.Fatal("the recovery installed the connection it dialled after Drain stopped the client")
	}
	logsMu.Lock()
	defer logsMu.Unlock()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			t.Fatalf("a recovery stopped by Drain logged an error: %s", line)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// logged reports whether the captured log holds fragment.
func logged(mu *sync.Mutex, logs *bytes.Buffer, fragment string) bool {
	mu.Lock()
	defer mu.Unlock()
	return strings.Contains(logs.String(), fragment)
}

// A recovery whose dial is failing when Close stops the client ends at the stop: it logs the
// cancellation and never a reconnect failure. Its dial retries a lost server for up to ten
// attempts, so without that the recovery would outlive the client by seconds and then report a
// shutdown as an ERROR. client.Conn.Close() stands in for nats.go closing the connection itself,
// as in the test above.
func TestARecoveryDiallingALostServerEndsAtTheStop(t *testing.T) {
	_, uri := startNATS(t)
	proxy := startRelayProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{proxy.url()})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	var logs bytes.Buffer
	var logsMu sync.Mutex
	logtest.SetDefault(t, slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logsMu.Lock()
		defer logsMu.Unlock()
		return logs.Write(p)
	}), nil))

	letGo := proxy.holdDials(t)
	proxy.refuse.Store(true)
	client.Conn.Close()
	testwait.Receive(t, "the recovery to dial", proxy.held)
	client.Close()
	letGo()

	testwait.Eventually(t, "the recovery stopped by Close to end", func() bool {
		return logged(&logsMu, &logs, "envoy nats recovery cancelled")
	})
	logsMu.Lock()
	defer logsMu.Unlock()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			t.Fatalf("a recovery stopped by Close logged an error: %s", line)
		}
	}
}
