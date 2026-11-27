package outbox

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// gatedNotices is the bus client an outbox reads its kept lanes and its expiry notices through
// (Deps.KeptLanes), with a gate in front of the notices' read: once armed, the next read of the
// notices reports that it has begun (reading) and goes on to the client only once open is closed.
// So a row can cut the client's NATS connection while the holder is inside that read.
type gatedNotices struct {
	*bus.Client
	armed   atomic.Bool
	reading chan struct{}
	open    chan struct{}
}

func (g *gatedNotices) NextStoredMessage(ctx context.Context, subject string, seq uint64) (bus.StoredMessage, error) {
	if subject == keptExpiryNotices && g.armed.CompareAndSwap(true, false) {
		close(g.reading)
		select {
		case <-g.open:
		case <-ctx.Done():
			return bus.StoredMessage{}, ctx.Err()
		}
	}
	return g.Client.NextStoredMessage(ctx, subject, seq)
}

// A holder whose NATS connection is cut while its expiry pass reads the listeners' expiry notices
// releases the publisher hold within natsLossGrace of the cut, as a holder cut between its passes
// does (TestAnOutboxThatLosesNATSReleasesThePublisher). cmd/dispatch gives the outbox its own bus
// client to read the notices through (Deps.KeptLanes), and a read that client makes while it
// reconnects waits for the reconnect up to its own bound (bus.Client.NextStoredMessage;
// storedMessageWait in internal/bus, 10 s). A holder that waits that read out before it counts the
// loss releases the hold only after that bound and natsLossGrace, so the other process takes the
// hold past the 10 s that TestAPublisherCutFromNATSHandsOverWithin10Seconds
// (cmd/dispatch/multiprocess_test.go) holds.
//
// Not parallel: its subject is that bound, the release within natsLossGrace and a half-second of it.
func TestAnOutboxCutFromNATSInItsExpiryReadReleasesThePublisher(t *testing.T) {
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	gate := &gatedNotices{reading: make(chan struct{}), open: make(chan struct{})}
	cut := startProcessWith(t, "cut", database, proxy.URL(), counter, func(client *bus.Client, deps *Deps) {
		gate.Client = client
		deps.KeptLanes = gate
	})
	waitFor(t, "the first process taking the publisher hold", cut.outbox.Publishing)
	working := startProcess(t, "working", storetest.SecondProcess(t, database), url, counter)

	// An event wakes the holder: it scans, makes its fallback pass and then its expiry pass, whose
	// read of the notices the gate holds until the proxy has cut the connection and the client
	// reports itself without it.
	gate.armed.Store(true)
	writeMessage(t, cut, "before the cut")
	testwait.Receive(t, "the holder's expiry pass reading the notices", gate.reading)
	proxy.Cut()
	downAt := time.Now()
	waitFor(t, "the cut process's client reporting itself disconnected", func() bool { return !gate.Client.Connected() })
	close(gate.open)
	waitWithin(t, natsLossGrace+500*time.Millisecond, "the cut process releasing the publisher hold", func() bool {
		return cut.outbox.hold.Load() == nil
	})
	t.Logf("the cut process released the publisher hold %s after a cut inside its read of the expiry notices",
		time.Since(downAt).Round(time.Millisecond))
	event := writeMessage(t, working, "after the cut")
	waitForPublished(t, database, counter, event)
	if !working.outbox.Publishing() || cut.outbox.Publishing() {
		t.Fatalf("publishing %v after the cut, want only the process whose NATS works", publishing(cut, working))
	}
}
