package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// probeCallers is how many callers the row below sends to the probe during one held handshake.
const probeCallers = 1000

// Callers that ask the probe while nats.go holds its lock for a reconnect's handshake share one
// read of nats.go's state (connProber.probe): each answers down at ConnLockWait, and the
// handshake, however long, leaves one goroutine waiting on nats.go's lock, not one per caller.
//
// The control is this row with probe starting a read for each caller: the count is probeCallers.
func TestCallersDuringAHeldHandshakeShareOneProbe(t *testing.T) {
	client, hold := proxiedClient(t, withLossAtOnce())
	conn := client.Connection()
	awaitHeld(t, hold.Hold())
	before := client.ProbesForTest()
	var callers sync.WaitGroup
	var up atomic.Int64
	for range probeCallers {
		callers.Go(func() {
			if ok, _ := client.upNow(conn); ok {
				up.Add(1)
			}
		})
	}
	callers.Wait()
	probes := client.ProbesForTest() - before
	t.Logf("%d callers during the held handshake started %d probes; %d read the connection up", probeCallers, probes, up.Load())
	if up.Load() != 0 {
		t.Fatalf("%d of %d callers read the connection up during the held handshake, want none", up.Load(), probeCallers)
	}
	if probes != 1 {
		t.Fatalf("%d callers during the held handshake started %d probes, want 1", probeCallers, probes)
	}
}

// A caller takes the answer of a read begun after it asked, never one that may have read nats.go's
// state before its question: a close answer read once the connection is closed is lost though a
// read begun before the close, which read the connection connected, is the prober's read in flight
// when the caller asks. The row puts that earlier read in place by hand, the one order it cannot
// make with nats.go's own lock alone, and marks it done with its CONNECTED answer while it stays
// in place, so the order in which the caller arrives decides nothing: whenever it asks, the read in
// front of it is one begun before its question, which it must not take, and it reads the
// connection down at its budget. It holds the life's end (withLifeEndGate), so the close answer is
// judged by closedUnder rather than by a life its own listener already ended.
//
// The control is this row with probe taking the answer of the read in flight whatever its number:
// the caller reads the earlier read's CONNECTED, and the close answer stays nats.go's.
func TestACloseAnswerAfterTheCloseIsLostThoughAnEarlierProbeIsInFlight(t *testing.T) {
	gate := newLifeEndHold()
	client, proxy := proxiedClient(t, withLossAtOnce(), withLifeEndGate(gate.gate))
	t.Cleanup(gate.release)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	_, finish := client.bindRequest(ctx)
	conn := client.Connection()
	client.mu.Lock()
	prober := client.proberLocked()
	client.mu.Unlock()
	earlier := &connProbe{done: make(chan struct{}), status: nats.CONNECTED, reconnects: conn.Stats().Reconnects}
	prober.mu.Lock()
	earlier.seq = prober.probes.Add(1)
	prober.probing = earlier
	prober.mu.Unlock()
	proxy.Cut()
	conn.Close()
	close(earlier.done)
	answered := make(chan error, 1)
	go func() { answered <- finish(natsgoClosedAnswer) }()
	select {
	case got := <-answered:
		if !errors.Is(got, ErrConnectionLost) {
			t.Fatalf("a close answer read after the close, with an earlier read in flight, answered %v, want ErrConnectionLost", got)
		}
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the close answer had not been read by the test's deadline")
	}
}

// A request on a client whose callers can hold a lock (withLossAtOnce) whose probe reads a false
// "down" - the read a loaded box keeps waiting past ConnLockWait, made here by the test's seam -
// is not sent, answers ErrConnectionLost, unsent, and lands on its retry: Reconnected reads nats.go
// itself, which says the connection is connected, and the request made again reads the probe
// again. A false "down" costs the caller one retry and nothing else.
//
// The control is this row with the seam reading "down" for every read: the retry is refused too.
func TestARequestWhoseProbeReadsAFalseDownRetriesAndLands(t *testing.T) {
	client, _ := proxiedClient(t, withLossAtOnce())
	endRunningLife(client)
	var downs atomic.Int64
	restore := client.ProbeReadsDownForTest(func() bool { return downs.Add(1) == 1 })
	defer restore()
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	sends := 0
	send := func(context.Context) (struct{}, error) {
		sends++
		return struct{}{}, nil
	}
	_, err := Request(ctx, client, send)
	if !errors.Is(err, ErrConnectionLost) || !requestUnsent(err) || sends != 0 {
		t.Fatalf("a request whose probe read a false down answered %v (unsent: %t) after %d sends, want ErrConnectionLost, unsent, and no send", err, requestUnsent(err), sends)
	}
	if !Reconnected(ctx, err) {
		t.Fatal("Reconnected did not report the connection back, though nats.go reads it connected")
	}
	if _, err := Request(ctx, client, send); err != nil || sends != 1 {
		t.Fatalf("the request made again answered %v after %d sends, want an answer after one", err, sends)
	}
}

// A close answer on a client whose callers can hold a lock, read while its probe reads a false
// "down", counts as lost, which costs its caller one retry: the same answer read again with the
// probe answering stays nats.go's, so a caller never retries a stale handle's close answer for
// ever. The seam starts reading down only after the request is bound, so its life runs and the
// answer is judged by closedUnder.
//
// The control is this row with closedUnder taking an unanswered probe as connected: the first
// answer stays nats.go's, and the row fails.
func TestACloseAnswerReadWhileTheProbeReadsAFalseDownCostsOneRetry(t *testing.T) {
	client, _ := proxiedClient(t, withLossAtOnce())
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	var down atomic.Bool
	_, finish := client.bindRequest(ctx)
	down.Store(true)
	restore := client.ProbeReadsDownForTest(down.Load)
	defer restore()
	if got := finish(natsgoClosedAnswer); !errors.Is(got, ErrConnectionLost) {
		t.Fatalf("a close answer read while the probe read down answered %v, want ErrConnectionLost", got)
	}
	down.Store(false)
	_, finish = client.bindRequest(ctx)
	if got := finish(natsgoClosedAnswer); errors.Is(got, ErrConnectionLost) || !errors.Is(got, nats.ErrConnectionClosed) {
		t.Fatalf("the same close answer read again with the probe answering answered %v, want nats.go's %v", got, natsgoClosedAnswer)
	}
}

// Connected reads nats.go's own status, not the probe's, so a "down" the probe's budget gives under
// load reaches none of its readers: the dispatch outbox's loss grace, the health checks, the
// keeper's pass. The row makes every probe read "down" and requires Connected to read the
// connection connected while nats.go does.
//
// The control is this row with Connected reading the probe: it reads false, and the row fails.
func TestConnectedReadsNatsGosOwnStatusWhenTheProbeReadsDown(t *testing.T) {
	client, _ := proxiedClient(t, withLossAtOnce())
	restore := client.ProbeReadsDownForTest(func() bool { return true })
	defer restore()
	if up, _ := client.upNow(client.Connection()); up {
		t.Fatal("the seam did not make the probe read down, so the row shows nothing")
	}
	if !client.Connected() {
		t.Fatal("Connected read false while nats.go reads the connection connected and only the probe reads down")
	}
}
