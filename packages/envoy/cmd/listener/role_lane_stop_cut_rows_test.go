package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The first four rows of this file. TestAForwardWhoseOutcomeComesLateInTheDrainsWaitEndsBeforeIt names no
// symbol the stop context's rounds added, so it builds against the forwarder before them and fails
// there for the reason it names. TestAHeldRetryInItsClaimReadAtTheStopEndsBeforeTheDrainsWait and
// TestAForwardResolvedAgainOnItsWorkerAtTheStopEndsBeforeTheDrainsWait read the claim through the
// delivery configuration's claim store (cfg.claims, added at 106f8d769f), and the first of them
// checks the cut keep's line (roleKeepCutLine, added at 106f8d769f); their controls are the head
// with the delivery off the handler under context.Background() - the hold timer's, then publish's,
// as it was at 106f8d769f for a queued forward resolved again.
// TestAKeepARuleRefusesAtTheStopIsLoggedAsAFailure holds that a keep a rule refuses while the stop
// cancels its caller is logged at ERROR, and its control that one whose failure holds the message
// is the cut line at INFO; it fails at 106f8d769f, which logged both at INFO.
//
// The last three rows hold that the stop cuts a forward in flight. TestAForwardWhoseAcknowledgementIsLateAtTheStopEndsForwarded
// and TestAForwardWhosePublishIsHeldAtTheStopEndsWithTheMessageBehindIt stand in for the bus with a
// seam that waits as it does, until its context or its window ends; their control is the head with
// the worker's forward under context.Background(), as at 9e1535310a, where each ends with an ERROR
// that a message was not kept. TestARoleForwardOverTheBusEndsAtItsContextsCancel drives the real
// forward (startRoleForward over the bus), whose control is its uncancelled forward.

// keepThatNeverAnswers is a keep whose publish NATS never answers: it waits for its context to
// end and fails with the context's error, as a keep over a connection whose replies are lost does.
func keepThatNeverAnswers(ctx context.Context, _ string, _ contracts.Envelope, _ string) (bus.StoredAck, error) {
	<-ctx.Done()
	return bus.StoredAck{}, ctx.Err()
}

// endsOf counts the lines that end the held message eventID: kept on its role's kept lane, or the
// ERROR that it was not.
func endsOf(logs, eventID string) int {
	return countLogged(logs, keptLine, eventID) + countLogged(logs, roleHeldUnkeptLine, eventID)
}

// A forward whose outcome comes late in the drain's wait - its window ends 100 ms before that wait
// does, with no acknowledgement - ends with its own line before the wait ends. The listener's
// read of the deployed stream, which can end such a forward for good, is not made once the
// listener has stopped, so the message holds, parks and ends under the stop's deadline at once.
// The read goes through a proxy whose server replies are frozen, so a read that is made takes its
// whole window. Before, finish read the stream for a fresh 2 s after the forward's own wait, and
// the message's end came after the drain's wait had ended and the process had gone on to exit.
func TestAForwardWhoseOutcomeComesLateInTheDrainsWaitEndsBeforeIt(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "outcome-late-in-the-drain"
	claimFor(t, harness, role, "ses_outcome_late")
	proxy := testnats.StartProxy(t, harness.client.Conn.ConnectedUrl())
	slow, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(slow.Close)
	entered := make(chan struct{})
	waitUntil := make(chan time.Time, 1)
	var forwards atomic.Int32
	cfg := harness.config
	cfg.client = slow
	cfg.keepPublish = keepThatNeverAnswers
	cfg.forwardRole = func(context.Context, string, contracts.Envelope, string, time.Duration) (func() (bus.StoredAck, error), error) {
		if forwards.Add(1) > 1 {
			return func() (bus.StoredAck, error) { return bus.StoredAck{}, nil }, nil
		}
		return func() (bus.StoredAck, error) {
			close(entered)
			time.Sleep(time.Until(<-waitUntil))
			return bus.StoredAck{}, fmt.Errorf("%w: %w", bus.ErrForwardNotStored, natsgo.ErrTimeout)
		}, nil
	}
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder := cfg.roleForwards
	item, msg := laneMessage(t, role, "outcome-late-in-the-drain")

	coreNATSDeliveryHandler(cfg)(msg)
	<-entered
	proxy.FreezeServerReplies()
	forwarder.stopHolding()
	waitEnds := forwarder.keepHeldNow()
	waitUntil <- waitEnds.Add(-100 * time.Millisecond)
	forwarder.settled(t)
	ended := time.Now()

	logs := harness.logs.String()
	if got := endsOf(logs, item.EventID); got != 1 {
		t.Fatalf("the message ended %d times, want once: kept, or the line that it was not:\n%s", got, logs)
	}
	if ended.After(waitEnds) {
		t.Fatalf("the message ended %s after the drain's wait ended, want before it: its line comes after the process has gone on to exit:\n%s", ended.Sub(waitEnds), logs)
	}
}

// A forward whose wait ends with no acknowledgement before the stop has finish read the deployed
// stream, for its own window, to learn whether the stream captures the subject (roleForwardEnded).
// A stop that comes while that read is in flight cuts it: the message, pinned to its holder since
// the forward may be stored, parks at once with all of the stop's share for its end, and is
// forwarded once more to that holder before the drain's wait ends. The read goes through a proxy
// whose server replies are frozen, so only a cancel ends it before its window; the row stops the
// listener once the server has the read's request, which a subscriber of the stream's info
// subject on the server's side sees under the reply prefix of the connection through the proxy.
// The read must end with the stop's own cause (errRoleLaneStopped, through context.Cause on the
// context the read was given), which the row reads as the read returns: a read deaf to the stop
// ends with its own window's cause, or none, however soon that window ends.
func TestAStopDuringTheDeployedStreamReadEndsTheReadAtTheStop(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "stop-during-the-stream-read"
	claimFor(t, harness, role, "ses_stop_during_read")
	proxy := testnats.StartProxy(t, harness.client.Conn.ConnectedUrl())
	// No periodic read of the stream's subjects runs on this connection while the row does, so
	// the one request the row waits for is finish's.
	slow, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1), bus.WithStreamSubjectsRefresh(time.Hour))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(slow.Close)
	inbox := slow.Conn.NewRespInbox()
	replyPrefix := inbox[:strings.LastIndexByte(inbox, '.')+1]
	reading := make(chan struct{})
	var readOnce sync.Once
	monitor, err := harness.client.Conn.Subscribe("$JS.API.STREAM.INFO."+bus.Stream, func(request *natsgo.Msg) {
		if strings.HasPrefix(request.Reply, replyPrefix) {
			readOnce.Do(func() { close(reading) })
		}
	})
	if err != nil {
		t.Fatalf("subscribe to the stream's info requests: %v", err)
	}
	t.Cleanup(func() { _ = monitor.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the info subscription: %v", err)
	}
	var forwards atomic.Int32
	readEnds := make(chan error, 4)
	cfg := harness.config
	cfg.client = slow
	cfg.streamCaptures = func(ctx context.Context, subject string, window time.Duration) (bool, bool) {
		captures, read := slow.ReadDeployedStreamCaptures(ctx, subject, window)
		readEnds <- context.Cause(ctx)
		return captures, read
	}
	cfg.keepPublish = keepThatNeverAnswers
	cfg.forwardRole = func(context.Context, string, contracts.Envelope, string, time.Duration) (func() (bus.StoredAck, error), error) {
		if forwards.Add(1) > 1 {
			return func() (bus.StoredAck, error) { return bus.StoredAck{}, nil }, nil
		}
		return func() (bus.StoredAck, error) {
			proxy.FreezeServerReplies()
			return bus.StoredAck{}, fmt.Errorf("%w: %w", bus.ErrForwardNotStored, natsgo.ErrTimeout)
		}, nil
	}
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder := cfg.roleForwards
	item, msg := laneMessage(t, role, "stop-during-the-stream-read")

	coreNATSDeliveryHandler(cfg)(msg)
	testwait.Receive(t, "finish's read of the deployed stream to reach the server", reading)
	forwarder.stopHolding()
	waitEnds := forwarder.keepHeldNow()
	cause := testwait.Receive(t, "finish's read of the deployed stream to end", readEnds)
	forwarder.settled(t)
	ended := time.Now()

	logs := harness.logs.String()
	if !errors.Is(cause, errRoleLaneStopped) {
		t.Fatalf("finish's read of the deployed stream ended with cause %v, want the stop's (%v): the read was deaf to the stop:\n%s", cause, errRoleLaneStopped, logs)
	}
	if got := countLogged(logs, "listener role forward failed", item.EventID); got != 0 {
		t.Fatalf("the message was given up as a failed forward %d times, want it held:\n%s", got, logs)
	}
	if got := countLogged(logs, "listener role forwarded", item.EventID); got != 1 || endsOf(logs, item.EventID) != 0 {
		t.Fatalf("the message was forwarded once more %d times and kept or not %d times, want forwarded once: the read the stop should cut ran on past the stop's share:\n%s", got, endsOf(logs, item.EventID), logs)
	}
	if ended.After(waitEnds) {
		t.Fatalf("the message ended %s after the drain's wait ended, want before it:\n%s", ended.Sub(waitEnds), logs)
	}
}

// claimReadThatStopsAnswering is a claim store (roles.ClaimStore) that answers from claims until
// stopAnswering is set, and from then on signals entered as a read starts and answers nothing:
// the read waits for its context to end, as a read over a connection whose replies are lost does.
type claimReadThatStopsAnswering struct {
	claims        roles.ClaimStore
	stopAnswering atomic.Bool
	entered       chan struct{}
	enteredOnce   sync.Once
}

func (c *claimReadThatStopsAnswering) RoleClaim(ctx context.Context, role string) (store.RoleClaim, error) {
	if !c.stopAnswering.Load() {
		return c.claims.RoleClaim(ctx, role)
	}
	c.enteredOnce.Do(func() { close(c.entered) })
	<-ctx.Done()
	return store.RoleClaim{}, ctx.Err()
}

// A held message whose retry is in its claim read when the listener stops ends with its own line
// before the drain's wait ends: the stop cuts the read, the message holds and parks, and it ends
// under the stop's deadline. The first forward leaves nothing on the wire, so the message is held
// and resolved again; from then on the claim read answers nothing and the keep after it never
// answers either, and the row stops the listener once the retry is in its read. Before, the
// retry's read and its keep ran under context.Background(), 2 s each, and the message ended after
// the drain's wait.
func TestAHeldRetryInItsClaimReadAtTheStopEndsBeforeTheDrainsWait(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "retry-in-its-claim-read"
	claimFor(t, harness, role, "ses_retry_claim_read")
	claims := &claimReadThatStopsAnswering{claims: harness.registry, entered: make(chan struct{})}
	var forwards atomic.Int32
	cfg := harness.config
	cfg.claims = claims
	cfg.keepPublish = keepThatNeverAnswers
	cfg.forwardRole = func(context.Context, string, contracts.Envelope, string, time.Duration) (func() (bus.StoredAck, error), error) {
		if forwards.Add(1) == 1 {
			claims.stopAnswering.Store(true)
			return nil, fmt.Errorf("%w: the window was spent before the publish", bus.ErrNotSent)
		}
		return func() (bus.StoredAck, error) { return bus.StoredAck{}, nil }, nil
	}
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder := cfg.roleForwards
	item, msg := laneMessage(t, role, "retry-in-its-claim-read")

	coreNATSDeliveryHandler(cfg)(msg)
	testwait.Receive(t, "the held message's retry to start its claim read", claims.entered)
	forwarder.stopHolding()
	waitEnds := forwarder.keepHeldNow()
	forwarder.settled(t)
	ended := time.Now()

	logs := harness.logs.String()
	if got := forwards.Load(); got != 1 {
		t.Fatalf("the lane made %d forwards, want only the first: the retry was cut in its claim read:\n%s", got, logs)
	}
	// The stop cut the retry's keep after its claim read: that is no failure of the keep's, so it
	// is the INFO line that says the message is held, and no ERROR says the keep failed on a
	// cancelled context. An ordered SIGTERM shutdown logs no ERROR
	// (TestListenerSIGTERMIsAnOrderedShutdown), and a keep the stop cut logged one.
	cut := linesIn(logs, roleKeepCutLine, map[string]any{"event_id": item.EventID})
	failed := 0
	for _, line := range linesIn(logs, roleKeepFailedLine, map[string]any{"event_id": item.EventID}) {
		if text, _ := line["error"].(string); strings.Contains(text, context.Canceled.Error()) {
			failed++
		}
	}
	if len(cut) != 1 || failed != 0 {
		t.Fatalf("%d lines say the stop cut the keep and %d ERROR lines say the keep failed on a cancelled context, want 1 and 0:\n%s", len(cut), failed, logs)
	}
	if got := endsOf(logs, item.EventID); got != 1 {
		t.Fatalf("the message ended %d times, want once: kept, or the line that it was not:\n%s", got, logs)
	}
	if ended.After(waitEnds) {
		t.Fatalf("the message ended %s after the drain's wait ended, want before it: its line comes after the process has gone on to exit:\n%s", ended.Sub(waitEnds), logs)
	}
}

// A queued forward whose resolution grew too old while it waited behind its holder's worker
// (roles.ForwardFenceAge) is resolved again on that worker, off the lane's handler, and when the
// listener stops in that resolution's claim read the message still ends with its own line before
// the drain's wait ends: the worker resolves under the forwarder's stop context, so the stop cuts
// the read, the message holds and parks, and it ends under the stop's deadline. The holder's first
// forward blocks in its publish while the second message queues behind it; the listener's clock
// then moves past the fence, the claim read stops answering and the keep never answers, and the
// first forward is let go, so the worker takes the second and resolves it again. The row stops the
// listener once that read has started. Before, the context of a delivery was chosen by its attempts,
// and this first delivery off the handler read and kept under context.Background(), 2 s each, so
// the message ended 2 s after the drain's wait.
func TestAForwardResolvedAgainOnItsWorkerAtTheStopEndsBeforeTheDrainsWait(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "resolved-again-on-its-worker"
	claimFor(t, harness, role, "ses_resolved_again")
	claims := &claimReadThatStopsAnswering{claims: harness.registry, entered: make(chan struct{})}
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	entered, release := make(chan struct{}), make(chan struct{})
	var forwards atomic.Int32
	cfg := harness.config
	cfg.claims = claims
	cfg.keepPublish = keepThatNeverAnswers
	cfg.now = func() time.Time { return time.Unix(0, clock.Load()) }
	cfg.forwardRole = func(context.Context, string, contracts.Envelope, string, time.Duration) (func() (bus.StoredAck, error), error) {
		if forwards.Add(1) == 1 {
			close(entered)
			<-release
		}
		return func() (bus.StoredAck, error) { return bus.StoredAck{}, nil }, nil
	}
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder := cfg.roleForwards
	handler := coreNATSDeliveryHandler(cfg)
	_, firstMsg := laneMessage(t, role, "resolved-again-first")
	second, secondMsg := laneMessage(t, role, "resolved-again-second")

	handler(firstMsg)
	testwait.Receive(t, "the holder's first forward to start", entered)
	handler(secondMsg)
	clock.Add(int64(roles.ForwardFenceAge + time.Minute))
	claims.stopAnswering.Store(true)
	close(release)
	testwait.Receive(t, "the queued forward's resolution again to start its claim read", claims.entered)
	forwarder.stopHolding()
	waitEnds := forwarder.keepHeldNow()
	forwarder.settled(t)
	ended := time.Now()

	logs := harness.logs.String()
	if got := forwards.Load(); got != 1 {
		t.Fatalf("the lane made %d forwards, want only the first: the second was cut in its resolution:\n%s", got, logs)
	}
	if got := endsOf(logs, second.EventID); got != 1 {
		t.Fatalf("the second message ended %d times, want once: kept, or the line that it was not:\n%s", got, logs)
	}
	if ended.After(waitEnds) {
		t.Fatalf("the second message ended %s after the drain's wait ended, want before it: its line comes after the process has gone on to exit:\n%s", ended.Sub(waitEnds), logs)
	}
}

// A keep whose caller's context the stop cancelled is logged as cut, at INFO, only when its failure
// holds the message (keepFailureHolds): a keep a rule refuses for good in the same instant is
// logged at ERROR, as roleKeepFailedLine, since the caller gives the message up and that line is
// where the refusal's text appears. Control: the same cancelled caller with a failure that holds
// the message is the INFO line.
func TestAKeepARuleRefusesAtTheStopIsLoggedAsAFailure(t *testing.T) {
	for _, row := range []struct {
		name      string
		failure   error
		wantLine  string
		wantLevel string
	}{
		{"a refusal for good", fmt.Errorf("keep: %w", bus.ErrRefused), roleKeepFailedLine, "ERROR"},
		{"control: a failure that holds the message", context.Canceled, roleKeepCutLine, "INFO"},
	} {
		t.Run(row.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t)
			cfg := harness.config
			cfg.keepPublish = func(context.Context, string, contracts.Envelope, string) (bus.StoredAck, error) {
				return bus.StoredAck{}, row.failure
			}
			item := listenerTestEnvelope(contracts.RoleTopicPrefix+"refused-at-the-stop", "refused-at-the-stop")
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := keepRoleMessage(parent, cfg, "refused-at-the-stop", item); err == nil {
				t.Fatal("the keep succeeded, want its failure")
			}
			logs := harness.logs.String()
			lines := linesIn(logs, row.wantLine, map[string]any{"event_id": item.EventID})
			if len(lines) != 1 || lines[0]["level"] != row.wantLevel {
				t.Fatalf("lines %v, want one %q at %s:\n%s", lines, row.wantLine, row.wantLevel, logs)
			}
		})
	}
}

// forwardedLine is the line of a role forward the stream acknowledged (finishRoleForward).
const forwardedLine = "listener role forwarded"

// noErrorLines fails t when logs holds a record at ERROR: a forward the stop cut ends forwarded
// or kept, and neither is an error.
func noErrorLines(t *testing.T, logs string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			t.Fatalf("the stop logged an ERROR: %s\n%s", line, logs)
		}
	}
}

// A forward whose acknowledgement has not come when the listener stops ends forwarded, with no
// ERROR, before the drain's wait ends: the stop cuts the forward's wait (forwardRole takes the
// stop context), the message holds pinned to its holder, since the stream may have stored it, and
// parks at once, and its end makes the forward once more to that holder under the same Nats-Msg-Id,
// which the stream answers as the duplicate of the copy it stored. The seam waits for the
// acknowledgement as bus.PendingForward.Wait does, until its context ends or its window does, and
// none comes. The drain releases the parked message right after the stop, so the window of a wait
// that ran on past the stop would end after the end's deadline (keepBy): before, the message parked
// then, its forward once more had no time and its keep failed, an ERROR that it was not kept.
func TestAForwardWhoseAcknowledgementIsLateAtTheStopEndsForwarded(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role, holder = "acknowledgement-late-at-the-stop", "ses_acknowledgement_late"
	claimFor(t, harness, role, holder)
	entered := make(chan struct{})
	var mu sync.Mutex
	var msgIDs []string
	cfg := harness.config
	cfg.forwardRole = func(ctx context.Context, _ string, _ contracts.Envelope, msgID string, timeout time.Duration) (func() (bus.StoredAck, error), error) {
		mu.Lock()
		msgIDs = append(msgIDs, msgID)
		first := len(msgIDs) == 1
		mu.Unlock()
		if !first {
			return func() (bus.StoredAck, error) { return bus.StoredAck{}, nil }, nil
		}
		return func() (bus.StoredAck, error) {
			close(entered)
			select {
			case <-ctx.Done():
				return bus.StoredAck{}, fmt.Errorf("%w: %w", bus.ErrForwardNotStored, ctx.Err())
			case <-time.After(timeout):
				return bus.StoredAck{}, bus.ErrForwardNotStored
			}
		}, nil
	}
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder := cfg.roleForwards
	item, msg := laneMessage(t, role, "acknowledgement-late-at-the-stop")

	coreNATSDeliveryHandler(cfg)(msg)
	testwait.Receive(t, "the forward's wait for its acknowledgement to start", entered)
	forwarder.stopHolding()
	waitEnds := forwarder.keepHeldNow()
	forwarder.settled(t)
	ended := time.Now()

	logs := harness.logs.String()
	mu.Lock()
	made := slices.Clone(msgIDs)
	mu.Unlock()
	if len(made) != 2 || made[0] != made[1] {
		t.Fatalf("the lane made the forwards %v, want two under one Nats-Msg-Id: the cut one and its forward once more to the same holder:\n%s", made, logs)
	}
	if got := countLogged(logs, forwardedLine, item.EventID); got != 1 {
		t.Fatalf("the message was forwarded %d times, want once, by its forward once more:\n%s", got, logs)
	}
	if got := endsOf(logs, item.EventID); got != 0 {
		t.Fatalf("the message ended kept, or as not kept, %d times, want none: its forward once more ended it:\n%s", got, logs)
	}
	noErrorLines(t, logs)
	if ended.After(waitEnds) {
		t.Fatalf("the message ended %s after the drain's wait ended, want before it:\n%s", ended.Sub(waitEnds), logs)
	}
}

// A forward whose publish is held when the listener stops, and the message queued behind it on its
// holder's worker, both end before the drain's wait ends, with no ERROR: the stop cuts the publish
// (forwardRole takes the stop context), the message holds pinned, since the publish was made, and
// parks, and the worker parks the message behind it rather than forward it. At the end the first
// is forwarded once more to its holder under the same Nats-Msg-Id, and the second, never forwarded,
// is kept on its role's kept lane. The seam's publish waits for its flush as
// bus.Client.StartRequestStoredTo does, until its context ends or its window does, and the flush
// never answers. Before, the publish ran out its window past the end's deadline, and both messages
// parked after it with no time for their ends: an ERROR that each was not kept.
func TestAForwardWhosePublishIsHeldAtTheStopEndsWithTheMessageBehindIt(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role, holder = "publish-held-at-the-stop", "ses_publish_held"
	claimFor(t, harness, role, holder)
	entered := make(chan struct{})
	var mu sync.Mutex
	var msgIDs []string
	cfg := harness.config
	cfg.forwardRole = func(ctx context.Context, subject string, _ contracts.Envelope, msgID string, timeout time.Duration) (func() (bus.StoredAck, error), error) {
		mu.Lock()
		msgIDs = append(msgIDs, msgID)
		first := len(msgIDs) == 1
		mu.Unlock()
		if !first {
			return func() (bus.StoredAck, error) { return bus.StoredAck{}, nil }, nil
		}
		close(entered)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("bus: flush publish to %q: %w", subject, ctx.Err())
		case <-time.After(timeout):
			return nil, fmt.Errorf("bus: flush publish to %q: %w", subject, natsgo.ErrTimeout)
		}
	}
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder := cfg.roleForwards
	handler := coreNATSDeliveryHandler(cfg)
	first, firstMsg := laneMessage(t, role, "publish-held-first")
	behind, behindMsg := laneMessage(t, role, "publish-held-behind")

	handler(firstMsg)
	testwait.Receive(t, "the holder's first forward to start its publish", entered)
	handler(behindMsg)
	forwarder.stopHolding()
	waitEnds := forwarder.keepHeldNow()
	forwarder.settled(t)
	ended := time.Now()

	logs := harness.logs.String()
	mu.Lock()
	made := slices.Clone(msgIDs)
	mu.Unlock()
	if len(made) != 2 || made[0] != made[1] {
		t.Fatalf("the lane made the forwards %v, want two under one Nats-Msg-Id: the cut publish and its forward once more, and none of the message behind it:\n%s", made, logs)
	}
	if got := countLogged(logs, forwardedLine, first.EventID); got != 1 {
		t.Fatalf("the first message was forwarded %d times, want once, by its forward once more:\n%s", got, logs)
	}
	if got := countLogged(logs, keptLine, behind.EventID); got != 1 {
		t.Fatalf("the message behind it was kept %d times, want once:\n%s", got, logs)
	}
	noErrorLines(t, logs)
	if ended.After(waitEnds) {
		t.Fatalf("the messages ended %s after the drain's wait ended, want before it:\n%s", ended.Sub(waitEnds), logs)
	}
}

// A role forward over the bus (startRoleForward) ends at once when its context is cancelled, with an
// error that holds its message pinned to its holder (forwardMayBeStored): the publish was made, so
// the stream may have stored it. A plain subscriber takes the subject and never answers, so neither
// an acknowledgement nor a no-responders answer ends the wait first. The control is the same forward
// with no cancel, which waits out its window.
func TestARoleForwardOverTheBusEndsAtItsContextsCancel(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	silent := testnats.Connect(t, harness.client.Conn.ConnectedUrl())
	t.Cleanup(silent.Close)
	const subject = "test.silent.role-forward"
	if _, err := silent.SubscribeSync(subject); err != nil {
		t.Fatalf("subscribe the silent taker: %v", err)
	}
	if err := silent.Flush(); err != nil {
		t.Fatalf("flush the silent taker: %v", err)
	}
	forward := startRoleForward(harness.client)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+"cut-role-forward", "cut-role-forward")
	const window = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	wait, err := forward(ctx, subject, item, bus.ForwardMsgID(item.DedupeKey, subject), window)
	if err != nil {
		t.Fatalf("start the forward: %v", err)
	}
	cancel()
	started := time.Now()
	_, err = wait()
	if elapsed := time.Since(started); elapsed >= window/2 {
		t.Fatalf("the cancelled forward's wait took %s, its window's; want it to end at the cancel", elapsed)
	}
	if !errors.Is(err, bus.ErrForwardNotStored) || !errors.Is(err, context.Canceled) || !forwardMayBeStored(err) {
		t.Fatalf("the cancelled forward = %v, want ErrForwardNotStored wrapping context.Canceled, held as may be stored", err)
	}

	control := listenerTestEnvelope(contracts.RoleTopicPrefix+"cut-role-forward", "uncut-role-forward")
	wait, err = forward(context.Background(), subject, control, bus.ForwardMsgID(control.DedupeKey, subject), 300*time.Millisecond)
	if err != nil {
		t.Fatalf("start the forward, control: %v", err)
	}
	if _, err := wait(); !errors.Is(err, bus.ErrForwardNotStored) || errors.Is(err, context.Canceled) {
		t.Fatalf("the uncancelled forward = %v, want ErrForwardNotStored at its window, without a cancel", err)
	}
}
