package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// silentCutBound is the upper limit on the listener's own record of the cut.
// bus.WithSilentCutDetection declares the connection stale after two to three seconds of silence.
const silentCutBound = 5 * time.Second

// disconnectLine finds the bus lifecycle record for exactly the named client. A close without
// an error is not evidence that the listener detected the loss of a connection.
func disconnectLine(output, name string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if _, fields, ok := strings.Cut(line, " INFO envoy nats disconnected "); ok &&
			slices.Contains(strings.Fields(fields), "name="+name) && strings.Contains(fields, " error=") {
			return line
		}
	}
	return ""
}

// The listener's stream client declares a connection lost within seconds of a hub that stops
// answering it (listenerStreamOptions). A hub whose packets stop closes nothing, so on nats.go's
// default pings that client would read connected for four to six minutes into the cut.
//
// The test runs the listener binary and freezes the hub under it, so the client is the one
// production builds. A test that connected with bus.WithSilentCutDetection itself would report
// the cut within seconds whether or not that call site carries the option, and would prove
// nothing.
func TestTheListenersNATSClientReportsASilentCutWithinSeconds(t *testing.T) {
	t.Parallel()
	server, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "silent-cut", "TZ=UTC")
	listener.waitHealthy(t)

	// Sample the child's clock immediately before closing the relay gate. Its HTTP Date and the
	// default slog text timestamp both have second precision. Reject the boundary second too:
	// rounding must not make a disconnect at or after five seconds pass.
	beforeCut := silentCutProbeDate(t, listener)
	_, _, line := freezeUntilTheClientReportsTheCut(t, server, listener)
	stamp, _, _ := strings.Cut(line, " INFO ")
	disconnectedAt, err := time.Parse("2006/01/02 15:04:05", stamp)
	if err != nil {
		t.Fatalf("read the listener's disconnect timestamp: %v (%q)", err, line)
	}
	if elapsed := disconnectedAt.Sub(beforeCut); elapsed < 0 || elapsed >= silentCutBound {
		t.Fatalf("the listener's clock reports the NATS disconnect %s after its pre-cut probe, want [0, %s): %s",
			elapsed, silentCutBound, line)
	}
}

// silentCutProbeDate reads the child's clock, not the test runner's. /livez does not query NATS.
func silentCutProbeDate(t *testing.T, listener *listenerProcess) time.Time {
	t.Helper()
	req, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/livez", listener.port), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read the listener's pre-cut /livez response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pre-cut /livez status = %d, want 204", resp.StatusCode)
	}
	date, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		t.Fatalf("read the listener's pre-cut /livez Date: %v", err)
	}
	return date
}

// freezeUntilTheClientReportsTheCut freezes the hub under a healthy listener and waits for
// its main client's disconnect record. It returns that record, the thaw, and a reader that
// excludes lines already logged before the freeze.
func freezeUntilTheClientReportsTheCut(t *testing.T, server *testnats.Server, listener *listenerProcess) (thaw func(), output func() string, disconnect string) {
	t.Helper()
	logged := len(listener.Output.String())
	output = func() string { return listener.Output.String()[logged:] }
	thaw = server.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes the server's shutdown. Thawing twice
	// is harmless, so a caller may run it itself.
	t.Cleanup(thaw)
	const fact = "the listener's main NATS client (name=envoy) to log its disconnect during the silent cut"
	testwait.Until(t, fact, func() (string, bool) {
		logged := output()
		disconnect = disconnectLine(logged, "envoy")
		if disconnect != "" {
			return "", true
		}
		select {
		case <-listener.Exited:
			t.Fatalf("the listener exited without reporting its main NATS client's disconnect during the silent cut:\n%s",
				listener.Output.String())
		default:
		}
		return logged, false
	})
	return thaw, output, disconnect
}
