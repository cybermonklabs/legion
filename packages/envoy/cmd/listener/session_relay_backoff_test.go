package main

// Rows for the relay's backoff of a quick second loss (sessionRelays.relay). They decide by counted
// events and read no clock to pass: the relay's clock is the row's own (sessionRelays.clock), and
// each retry wait the relay asks for comes to the row (sessionRelays.retryWait), which reads its
// length and the lost restarts logged so far, and releases it. A row that fails ends at the test's
// deadline, or at a count of restarts no backoff would allow.

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

const relayLostLine = "session relay lost its NATS connection"

// relayClock is the clock a relay row moves itself (sessionRelays.clock).
type relayClock struct {
	mu sync.Mutex
	at time.Time
}

func newRelayClock() *relayClock { return &relayClock{at: time.Unix(1_800_000_000, 0)} }

func (c *relayClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *relayClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// askedWait is one retry wait the relay asked for: its length, the lost restarts the log held when
// it asked, and where the row releases it.
type askedWait struct {
	d       time.Duration
	losses  int
	release chan bool
}

// relayWaits hands each retry wait the relay asks for to the row (waits), which releases it.
type relayWaits struct {
	waits chan askedWait
	logs  func() string
}

func (w *relayWaits) seam(ctx context.Context, _ *relayRun, d time.Duration) bool {
	asked := askedWait{d: d, losses: strings.Count(w.logs(), relayLostLine), release: make(chan bool, 1)}
	select {
	case w.waits <- asked:
	case <-ctx.Done():
		return false
	}
	select {
	case ok := <-asked.release:
		return ok
	case <-ctx.Done():
		return false
	}
}

// next is the relay's next asked wait, by the test's deadline, or a failure when the probe's seam
// counted restarts past spun first: a relay that restarts with no backoff asks for none.
func (w *relayWaits) next(t *testing.T, spun <-chan struct{}, what string) askedWait {
	t.Helper()
	select {
	case asked := <-w.waits:
		return asked
	case <-spun:
		t.Fatalf("%s: the relay read its probe %d times and asked for no retry wait: it restarts with no backoff", what, relaySpinReads)
	case <-time.After(testnats.Remaining(t)):
		t.Fatalf("%s: the relay asked for no retry wait by the test's deadline", what)
	}
	return askedWait{}
}

// relaySpinReads is a count of probe reads no backoff allows before the waits a row reads: a relay
// that backs off reads its probe a few times between waits, and one that does not reads it without
// end.
const relaySpinReads = 400

// countingDown is a probe seam that reads every read "down" and closes spun once it has read
// relaySpinReads of them.
func countingDown() (down func() bool, spun <-chan struct{}) {
	var reads atomic.Int64
	closed := make(chan struct{})
	var once sync.Once
	return func() bool {
		if reads.Add(1) >= relaySpinReads {
			once.Do(func() { close(closed) })
		}
		return true
	}, closed
}

// newCountedRelayRig is newRelayRig with the relay's clock and retry waits the row's, set before the
// first relay starts.
func newCountedRelayRig(t *testing.T, clock *relayClock) (*relayRig, *relayWaits) {
	t.Helper()
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	waits := &relayWaits{waits: make(chan askedWait), logs: rig.harness.logs.String}
	rig.relays.clock = clock.now
	rig.relays.retryWait = waits.seam
	rig.relays.receiptWait = relayReceiptWait
	rig.relays.setOwned(true)
	return rig, waits
}

// A relay whose every probe read reads a false "down" even though each restart waits in line for
// the session's lock backs off its restarts: the time a restart waited for the lock (or its restart
// turn) is no part of how quickly it was lost, since a loss is timed from when the restart took the
// lock (relayRun.lockedAt). Before each restart the row takes the session's lock while the relay is
// in its retry wait, and moves the relay's clock 1.1 s, past relayRetryFirst, while the relay waits
// in line for it; then it lets the lock go. Each such restart is lost at once and asks for a wait,
// so the waits come one lost restart apart.
//
// The control is the loss timed from before the restart's wait in line: a restart that waited in
// line reads as no quick loss and asks for no wait, so the waits come two lost restarts apart.
func TestARelayWhoseRestartWaitsInLineStillBacksOff(t *testing.T) {
	clock := newRelayClock()
	rig, waits := newCountedRelayRig(t, clock)
	const sessionID = "ses_relay_wait_in_line"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	before, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, testnats.Remaining(t)), before)

	locks := &rig.deps.consumers.locks
	down, spun := countingDown()
	restore := rig.deps.consumers.consumers.ProbeReadsDownForTest(down)
	defer restore()
	rig.deps.consumers.consumers.CloseConnection()
	previous := -1
	for i := range 4 {
		asked := waits.next(t, spun, "a restart that waited in line")
		if previous >= 0 && asked.losses-previous != 1 {
			t.Fatalf("wait %d came %d lost restarts after the one before it, want 1: a restart that waited %s in line for the session's lock was not read as a quick loss", i+1, asked.losses-previous, 1100*time.Millisecond)
		}
		previous = asked.losses
		if i == 3 {
			restore()
			asked.release <- true
			break
		}
		// The relay is in its retry wait and holds nothing: the row takes the session's lock, and
		// moves the clock once the relay's next restart waits for it.
		unlock := locks.lock(sessionID)
		go func() {
			defer unlock()
			for deadline := time.Now().Add(testnats.Remaining(t)); lockUsers(locks, sessionID) < 2; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					return
				}
			}
			clock.advance(1100 * time.Millisecond)
		}()
		asked.release <- true
	}
}

// A run that served the session for at least relayRetryMax resets the relay's retry backoff to
// relayRetryFirst, so a relay that met a false "down" for a while once does not carry the doubled
// backoff for the rest of its life. Episode one: every probe read is "down", and the relay asks for
// waits of 1, 2, 4 and 8 s. Then the row lets the relay serve, moves its clock past relayRetryMax,
// and drops the connection with every read "down" again: the first wait of episode two is
// relayRetryFirst.
//
// The control is the relay without that reset: the first wait of episode two is 16 s.
func TestARelayBackoffAfterAHealthyRunStartsAgainFromTheFirst(t *testing.T) {
	clock := newRelayClock()
	rig, waits := newCountedRelayRig(t, clock)
	const sessionID = "ses_relay_backoff_reset"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	first, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, testnats.Remaining(t)), first)

	// Episode one: four waits, each twice the one before.
	down, spun := countingDown()
	restore := rig.deps.consumers.consumers.ProbeReadsDownForTest(down)
	rig.deps.consumers.consumers.CloseConnection()
	want := relayRetryFirst
	for i := range 4 {
		asked := waits.next(t, spun, "episode one")
		if asked.d != want {
			t.Fatalf("episode one's wait %d is %s, want %s", i+1, asked.d, want)
		}
		want = min(2*want, relayRetryMax)
		if i == 3 {
			restore()
		}
		asked.release <- true
	}
	// The relay serves the session again.
	second, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	awaitRelayedFrame(t, standIn, second, testnats.Remaining(t))

	// A healthy run longer than relayRetryMax, then episode two.
	clock.advance(relayRetryMax + time.Second)
	down, spun = countingDown()
	restore = rig.deps.consumers.consumers.ProbeReadsDownForTest(down)
	defer restore()
	rig.deps.consumers.consumers.CloseConnection()
	asked := waits.next(t, spun, "episode two")
	restore()
	asked.release <- true
	if asked.d != relayRetryFirst {
		t.Fatalf("after a healthy run of %s, episode two's first wait is %s, want %s: the relay's retry backoff, doubled by the losses of an episode long past, was never put back", relayRetryMax+time.Second, asked.d, relayRetryFirst)
	}
}

// awaitRelayedFrame waits for the frame carrying want, passing over the redelivery of an earlier frame
// whose ack a close took (a restart at the ack floor sends it again).
func awaitRelayedFrame(t *testing.T, s *sessionStandIn, want contracts.Envelope, within time.Duration) relayedFrame {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case frame := <-s.frames:
			if frame.item.EventID == want.EventID {
				return frame
			}
		case <-deadline:
			t.Fatalf("no frame carrying %s within %s", want.EventID, within)
		}
	}
}
