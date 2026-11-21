package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testwait"
)

// These rows name no symbol this change added, so each builds against the forwarder before it and
// fails there for the reason it names. The row that needs the stop's own shares is in
// role_lane_stop_share_test.go.

// keptLine is the line of a message the role lane kept on its role's kept lane
// (keepRoleLaneMessage).
const keptLine = "listener kept role message for the role's next holder"

// claimFor registers holder as a live session and gives it role.
func claimFor(t *testing.T, harness listenerDeliveryHarness, role, holder string) {
	t.Helper()
	if err := harness.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", holder, err)
	}
	if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
}

// laneMessage is one role message on role's lane, which every forward path takes as a new message.
func laneMessage(t *testing.T, role, key string) (contracts.Envelope, *natsgo.Msg) {
	t.Helper()
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, key)
	item.Payload = `{"type":"worker-queued"}`
	return item, &natsgo.Msg{Data: marshalListenerEnvelope(t, item)}
}

// A forward the stream did not answer in its window: it left the process, so it may be stored
// (forwardMayBeStored), and no rule ends it without a read (roleForwardEnded).
var stalledForward = fmt.Errorf("bus: flush publish: %w", natsgo.ErrTimeout)

// A message pinned to its holder when the listener stops makes its forward once more and then its
// keep under one deadline inside the drain's wait (keepHeldNow): each step is given a window that
// ends before the wait does, so the message's own line comes before the main client's drain and
// the process's exit. Here every step after the stop runs out its whole window - the forward once
// more stalls until its window ends, and so does the keep - and the row reads the windows each step
// was given, so its verdict does not depend on how fast this machine runs. Before the deadline the
// forward once more took its own 2 s and the keep its own 2 s after it, one after another, against
// a wait of 2 s, so the message could be cut off by the exit with no line of its own.
func TestAPinnedMessageAtTheStopEndsInsideTheDrainsWait(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "pinned-at-the-stop-stalled"
	claimFor(t, harness, role, "ses_pinned_stalled")
	var forwarder *roleForwarder
	var forwards atomic.Int32
	var mu sync.Mutex
	var forwardEnds, keepEnds []time.Time
	cfg := harness.config
	cfg.forwardRole = func(_ string, _ contracts.Envelope, _ string, timeout time.Duration) (func() error, error) {
		first := forwards.Add(1) == 1
		end := time.Now().Add(timeout)
		return func() error {
			if first {
				forwarder.stopHolding()
				return stalledForward
			}
			mu.Lock()
			forwardEnds = append(forwardEnds, end)
			mu.Unlock()
			time.Sleep(time.Until(end))
			return stalledForward
		}, nil
	}
	cfg.keepPublish = func(ctx context.Context, _ string, _ contracts.Envelope, _ string) (bus.StoredAck, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Errorf("a keep at the stop ran with no deadline")
		}
		mu.Lock()
		keepEnds = append(keepEnds, deadline)
		mu.Unlock()
		<-ctx.Done()
		return bus.StoredAck{}, ctx.Err()
	}
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder = cfg.roleForwards
	item, msg := laneMessage(t, role, "pinned-at-the-stop-stalled")

	coreNATSDeliveryHandler(cfg)(msg)
	if !testwait.Holds(20*time.Second, 10*time.Millisecond, func() bool {
		return countLogged(harness.logs.String(), roleParkedLine, item.EventID) == 1
	}) {
		t.Fatalf("the message whose forward stalled was not parked at the stop:\n%s", harness.logs.String())
	}
	forwarder.keepHeldNow()
	// The drain's wait, as roleLane.drain sets it once it has released the held messages: no
	// step of a held message's end may be given longer than this.
	waitEnds := time.Now().Add(roleForwardTimeout)
	forwarder.settled(t)

	mu.Lock()
	defer mu.Unlock()
	if got := forwards.Load(); got != 2 {
		t.Fatalf("the lane made %d forwards, want 2: the one that stalled before the stop and its forward once more at it", got)
	}
	for _, end := range forwardEnds {
		if end.After(waitEnds) {
			t.Errorf("the forward once more at the stop was given until %s, %s past the end of the drain's wait", end.Format(time.RFC3339Nano), end.Sub(waitEnds))
		}
	}
	if len(keepEnds) == 0 {
		t.Fatalf("the message made no keep after its forward once more:\n%s", harness.logs.String())
	}
	for _, end := range keepEnds {
		if end.After(waitEnds) {
			t.Errorf("the keep at the stop was given until %s, %s past the end of the drain's wait", end.Format(time.RFC3339Nano), end.Sub(waitEnds))
		}
	}
	if got := countLogged(harness.logs.String(), roleHeldUnkeptLine, item.EventID); got != 1 {
		t.Fatalf("%d lines say the message was not kept, want 1: the message no step could store ends with its own line:\n%s", got, harness.logs.String())
	}
}

// clockOnLine is a log writer that moves a test's clock the first time the listener logs line:
// for roleHeldLine, after a hold has judged its bound for the wait it sets and before that wait
// ends, which is what a wait that ends late - a process paused, a clock that moved - finds.
type clockOnLine struct {
	logs io.Writer
	line []byte
	once sync.Once
	move func()
}

func (c *clockOnLine) Write(p []byte) (int, error) {
	if bytes.Contains(p, c.line) {
		c.once.Do(c.move)
	}
	return c.logs.Write(p)
}

// A held message whose wait ends past its hold bound makes no forward: the bound is judged where
// the attempt starts, not only when the wait is set. The listener's clock passes the bound - and
// the stream's duplicate window past it - while the first hold waits, and the message is kept on
// its role's kept lane under held_past_bound. That holds for a message resolved again after a
// forward that left nothing on the wire, and for one pinned to its holder after a forward that may
// be stored. Before, the hold judged only the wait it set, and the late wake forwarded the message.
func TestAHeldMessageWhoseWaitEndsPastItsBoundMakesNoForward(t *testing.T) {
	for _, row := range []struct {
		name  string
		first error
	}{
		{"resolved again", fmt.Errorf("%w: the window was spent before the publish", bus.ErrNotSent)},
		{"pinned to its holder", stalledForward},
	} {
		t.Run(row.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t)
			const role = "held-past-its-bound"
			claimFor(t, harness, role, "ses_held_past_bound")
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			var forwards atomic.Int32
			cfg := harness.config
			cfg.now = func() time.Time { return time.Unix(0, clock.Load()) }
			cfg.logger = logging.NewWithWriter("test", &clockOnLine{logs: harness.logs, line: []byte(roleHeldLine), move: func() {
				clock.Add(int64(roleHoldBound + roleKeepWindow + time.Hour))
			}})
			cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
				if forwards.Add(1) == 1 {
					return row.first
				}
				return nil
			})
			cfg = wireTestRoleForwarder(t, cfg)
			item, msg := laneMessage(t, role, "held-past-its-bound")

			coreNATSDeliveryHandler(cfg)(msg)
			cfg.roleForwards.settled(t)

			logs := harness.logs.String()
			if got := forwards.Load(); got != 1 {
				t.Fatalf("the lane made %d forwards, want only the first: none starts past the hold bound:\n%s", got, logs)
			}
			if kept := linesIn(logs, keptLine, map[string]any{"event_id": item.EventID, "reason": string(roleKeptPastHoldBound)}); len(kept) != 1 {
				t.Fatalf("kept lines %v, want one naming %s:\n%s", kept, roleKeptPastHoldBound, logs)
			}
			if got := countLogged(logs, "listener role forwarded", item.EventID); got != 0 {
				t.Fatalf("the message was forwarded %d times past its bound, want none:\n%s", got, logs)
			}
		})
	}
}

// A forward queued behind its holder's earlier one while the listener's clock passes the message's
// hold bound is not made when the worker takes it: the bound is judged where the forward starts,
// and the message is kept on its role's kept lane under held_past_bound. The holder's first forward
// blocks in its publish while the second message queues behind it and the clock moves. Before,
// the worker forwarded it, past the stream's duplicate window.
func TestAForwardQueuedPastItsBoundIsKeptNotMade(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "queued-past-its-bound"
	claimFor(t, harness, role, "ses_queued_past_bound")
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	entered, release := make(chan struct{}), make(chan struct{})
	var forwards atomic.Int32
	cfg := harness.config
	cfg.now = func() time.Time { return time.Unix(0, clock.Load()) }
	cfg.forwardRole = func(string, contracts.Envelope, string, time.Duration) (func() error, error) {
		if forwards.Add(1) == 1 {
			close(entered)
			<-release
		}
		return func() error { return nil }, nil
	}
	cfg = wireTestRoleForwarder(t, cfg)
	handler := coreNATSDeliveryHandler(cfg)
	first, firstMsg := laneMessage(t, role, "queued-past-its-bound-first")
	second, secondMsg := laneMessage(t, role, "queued-past-its-bound-second")

	handler(firstMsg)
	<-entered
	handler(secondMsg)
	clock.Add(int64(roleHoldBound + roleKeepWindow + time.Hour))
	close(release)
	cfg.roleForwards.settled(t)

	logs := harness.logs.String()
	if got := forwards.Load(); got != 1 {
		t.Fatalf("the lane made %d forwards, want only the first, started inside the bound:\n%s", got, logs)
	}
	if got := countLogged(logs, "listener role forwarded", first.EventID); got != 1 {
		t.Fatalf("the first message was forwarded %d times, want 1:\n%s", got, logs)
	}
	if kept := linesIn(logs, keptLine, map[string]any{"event_id": second.EventID, "reason": string(roleKeptPastHoldBound)}); len(kept) != 1 {
		t.Fatalf("kept lines %v for the queued message, want one naming %s:\n%s", kept, roleKeptPastHoldBound, logs)
	}
}
