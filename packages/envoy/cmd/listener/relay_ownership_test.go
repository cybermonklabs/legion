package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Which pod of a machine relays its sessions' consumers is the receive term's to say and nothing
// else's: the term elects this pod when it begins (durableReceiver.begin) and stands it down when
// it ends (durableReceiver.end, sessionRelays.setOwned). The self-health monitor's durable probe
// and its rebuild, which judge whether the durable is bound to this listener's subscription, say
// nothing about it, and the two rows below are why.
//
// A second writer that went by the binding - the probe clearing the flag on an unbound durable,
// the rebuild setting it after a resubscribe - reads a state the term does not have: a resubscribe
// reports success for a pod with no term at all (durableReceiver.resubscribe), so a lease lost
// between the two would leave a standby relaying every session's consumer beside the real holder,
// each pull and each ack-floor restart racing the other's; and an unbound probe inside a live term
// that something else repaired would leave the holder relaying nothing for the rest of it.

// startRelayingReceiverRig is startReceiverRig for a pod whose term elects it the relayer of its
// machine's session consumers, as main hands its receiver the listener's relays. The set is a bare
// one, with no session to relay: these rows are about which pod is elected, not about what it
// then pulls, and sessionRelays.setOwned is the whole of the election. The lease polls slowly as a
// standby, so a pod that gives its lease up does not take it back while the row reads it.
func startRelayingReceiverRig(t *testing.T, uri, machineID, instance string, handle durableHandler) (*receiverRig, *sessionRelays) {
	t.Helper()
	leaseTiming := testLeaseTiming()
	leaseTiming.standbyPoll = 3 * time.Second
	rig := newReceiverRig(t, uri, machineID, instance, handle, testReceiveTiming(), leaseTiming)
	relays := &sessionRelays{logger: logging.New("relay-ownership " + instance)}
	rig.receiver.relays = relays
	rig.lease.Start(context.Background())
	rig.receiver.Start()
	return rig, relays
}

// A lease lost between an unbound probe and the resubscribe that follows it leaves the pod not
// relaying: its term is over, and the resubscribe that reports success for a pod with no term
// elects nobody.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late.
func TestALeaseLostBetweenAnUnboundProbeAndTheResubscribeLeavesThePodNotRelaying(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-relay-ownership-lost"
	handle, _ := recordingHandler()
	rig, relays := startRelayingReceiverRig(t, uri, machineID, "pod-a", handle)
	rig.awaitState(t, receiveActive, 10*time.Second)
	if !relays.owned.Load() {
		t.Fatal("the pod receiving the durable relays none of its machine's session consumers")
	}

	// The holder's probe reads the durable unbound: it was recreated server-side, so its deliver
	// subject is no longer this pod's subscription's.
	recreateListenerDurable(t, rig.client, rig.consumer)
	if err := rig.receiver.probe(context.Background()); !errors.Is(err, errDurableUnbound) {
		t.Fatalf("the probe of a recreated durable = %v, want errDurableUnbound", err)
	}

	// The term ends before the resubscribe runs, as it does when the lease is released because
	// the receive connection is down.
	rig.lease.GiveUp(rig.lease.Status().Term)
	waitFor(t, "pod-a to give the lease up", func() bool { return !rig.lease.Holding() })
	rig.awaitState(t, receiveStandby, 10*time.Second)
	if relays.owned.Load() {
		t.Fatal("the pod goes on relaying after its term ended")
	}

	// The rebuild runs on, with nothing left to subscribe.
	if err := rig.receiver.resubscribe(context.Background()); err != nil {
		t.Fatalf("the rebuild's resubscribe on a pod with no term = %v, want nil", err)
	}
	probe := func() error { return rig.receiver.probe(context.Background()) }
	if err := rebuildListenerDependencies(context.Background(), rig.client, nil, probe, rig.receiver.resubscribe); err != nil {
		t.Fatalf("the self-health rebuild on a pod with no term: %v", err)
	}
	if relays.owned.Load() {
		t.Fatal("a pod with no term relays its machine's session consumers: it pulls every session's consumer beside the pod that holds the lease, and each ack-floor restart deletes the other's")
	}
}

// An unbound probe inside a live term leaves the holder relaying, whether or not a rebuild
// resubscribes after it: the durable's binding is repaired by the receive loop as well (repair),
// and a rebuild whose probe then finds the durable bound returns without resubscribing. Neither
// says anything about the term, which is what elects this pod.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late.
func TestAnUnboundProbeInsideALiveTermLeavesTheHolderRelaying(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-relay-ownership-live"
	handle, _ := recordingHandler()
	rig, relays := startRelayingReceiverRig(t, uri, machineID, "pod-a", handle)
	rig.awaitState(t, receiveActive, 10*time.Second)

	recreateListenerDurable(t, rig.client, rig.consumer)
	if err := rig.receiver.probe(context.Background()); !errors.Is(err, errDurableUnbound) {
		t.Fatalf("the probe of a recreated durable = %v, want errDurableUnbound", err)
	}
	if !relays.owned.Load() {
		t.Fatal("an unbound probe stopped the relays of a pod whose term is running")
	}

	// The receive loop repairs the binding itself, so the rebuild's own probe finds the durable
	// bound again and returns without a resubscribe of its own.
	if err := rig.receiver.resubscribe(context.Background()); err != nil {
		t.Fatalf("the repair's resubscribe: %v", err)
	}
	probe := func() error { return rig.receiver.probe(context.Background()) }
	if err := rebuildListenerDependencies(context.Background(), rig.client, nil, probe, rig.receiver.resubscribe); err != nil {
		t.Fatalf("the self-health rebuild of a repaired durable: %v", err)
	}
	if !relays.owned.Load() {
		t.Fatal("the holder relays nothing for the rest of its term after an unbound probe its own repair answered")
	}
	if !rig.lease.Holding() || rig.receiver.report().State != receiveActive {
		t.Fatalf("pod-a is no longer the holder receiving: lease held %v, state %s", rig.lease.Holding(), rig.receiver.report().State)
	}
}

// A relay acts on a session's consumer only while this pod's receive term is current and no other
// pod can hold the lease yet. The rows below hold the windows in which a pod whose term had ended
// still acted on a consumer: a term that ended before its receiving began (durableReceiver.begin),
// a lease that ended the term before the receive loop heard it, which each restart checks for and
// is cut off at (sessionRelays.termContext), and a pull whose request still waited at the consumer
// after the term ended, which the term's end unsubscribes, or whose message arrived after it
// (sessionRelays.pullBound). Each lease is on a fake clock, as the lease's own rows are, and is
// stepped by hand.

// stepLeaseDown has l step down as a holder does whose renewal finds another pod's record: pod-d
// writes the record over l's, and l's next round reads it (lease.stepDown). It reports false when
// l holds the lease after that round.
func stepLeaseDown(t *testing.T, kv bus.KeyValue, l *lease) bool {
	t.Helper()
	putLease(t, kv, `{"holder":"pod-d","boot":"00000000000000dd"}`)
	l.step()
	return !l.Holding()
}

// A term that ended after the receive loop prepared the durable and before it began receiving
// elects this pod the relayer of nothing: begin hands the relays the term's context, and they take
// the term only while that context is live (sessionRelays.setOwned), so no relay starts and none
// restarts a session's consumer on a pod whose lease another pod has taken. The relay set here has
// a session on consumer delivery, whose relay a term that took it would start at once.
func TestATermEndedBetweenPrepareAndBeginStartsNoRelayRun(t *testing.T) {
	t.Parallel()
	relayRig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	relayRig.subscribe(t, consumerRegistration("ses_term_ended_before_begin_"+id.New(), "epoch-1"))
	var restarts atomic.Int32
	relayRig.deps.consumers.afterStoredRowRead = func() { restarts.Add(1) }

	handle, _ := recordingHandler()
	pod := newReceiverRig(t, relayRig.url, leaseTestMachine, "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	clock := newFakeLeaseClock()
	held := newLease(pod.lease.kv, leaseTestMachine, "pod-a", productionLeaseConfig(), clock.now, logging.NewWithWriter(leaseTestMachine, pod.logs))
	pod.receiver.lease.Store(held)
	pod.receiver.relays = relayRig.relays
	held.step()
	status := held.Status()
	if !status.Holding {
		t.Fatalf("pod-a did not take the lease: %+v", status)
	}
	info, err := ensureListenerDurable(context.Background(), pod.client, pod.consumer)
	if err != nil {
		t.Fatalf("create the durable: %v", err)
	}

	// The durable is prepared. Then another pod takes the record, the lease steps down at its next
	// round, and the receive loop's watcher ends the term's context, all before begin.
	termCtx, endTerm := context.WithCancel(context.Background())
	if !stepLeaseDown(t, pod.lease.kv, held) {
		t.Fatal("pod-a's lease did not step down for the record another pod wrote")
	}
	endTerm()
	if err := pod.receiver.begin(termCtx, info.Config.DeliverSubject, 0, status.Term); err != nil {
		t.Fatalf("subscribe to the durable's deliver subject: %v", err)
	}
	t.Cleanup(func() { pod.receiver.end(context.Background()) })

	if relayRig.relays.owned.Load() {
		t.Fatal("a term that ended before its receiving began elected this pod the relayer of its machine's session consumers: its relays restart each session's consumer on a pod whose lease another pod has taken")
	}
	time.Sleep(time.Second)
	if n := restarts.Load(); n != 0 {
		t.Fatalf("%d relay restarts of a session's consumer began for a term that ended before its receiving began, want none", n)
	}
}

// A relay run that reaches its restart after the lease stepped down does not restart the
// session's consumer: the lease ends the term a moment before the receive loop hears it and stops
// the relays (durableReceiver.end), and the restart checks the term just before its RestartAtFloor
// (sessionRelays.termContext). Here the step-down lands between the restart's read of the session's
// row and that call, with the term's context still live, as the receive loop has not heard it.
func TestARelayRestartAfterTheLeaseSteppedDownDoesNotRestartTheConsumer(t *testing.T) {
	t.Parallel()
	rig, run := newRestartableRun(t, "ses_restart_after_step_down_")
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	held := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	held.step()
	status := held.Status()
	if !status.Holding {
		t.Fatalf("pod-a did not take the lease: %+v", status)
	}
	rig.relays.term = &relayTerm{ctx: context.Background(), untilLoss: func() time.Duration { return held.UntilLoss(status.Term) }}
	steppedDown := false
	rig.deps.consumers.afterStoredRowRead = func() {
		if !steppedDown {
			steppedDown = stepLeaseDown(t, kv, held)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, pull, outcome := rig.relays.restart(ctx, run)
	rig.deps.consumers.afterStoredRowRead = nil
	if !steppedDown {
		t.Fatal("pod-a's lease did not step down inside the restart")
	}
	if pull != nil {
		pull.Stop()
		t.Fatal("a relay run that reached its restart after this pod's lease stepped down restarted the session's consumer and pulled it: two pods can restart one session's consumer at once")
	}
	if at := run.restartedAt.Load(); at != 0 {
		t.Fatalf("the restart recorded a restart at mark %d after the lease stepped down, want none", at)
	}
	if outcome != relayRetry {
		t.Fatalf("the restart's outcome is %d, want relayRetry (%d): the relay tries again while the session is still this listener's, and the term's end stops it", outcome, relayRetry)
	}
}

// A restart that starts just before the lease's loss deadline is cut off at it: the restart sends
// RestartAtFloor on a context that ends at that deadline (sessionRelays.termContext), the moment
// another pod may take the lease, so it sends the consumer no call after that and starts no pull.
// A call it had already sent can still land, which termContext's doc bounds. The
// session-consumer connection goes through a proxy that the restart's read of the session's row
// freezes, so the call waits with no answer until its context ends.
// It must not run in parallel: the restart must end within 1 s of the loss deadline it times.
func TestARelayRestartStartedJustBeforeTheLeasesLossDeadlineIsCutOffAtIt(t *testing.T) {
	server := testnats.URL(t)
	proxy := testnats.StartProxy(t, server)
	client, err := bus.ConnectOwningStream([]string{server}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newRelayRigUnownedOn(t, client, proxy.URL(), relayPauseFirst, relayPauseMax)
	t.Cleanup(proxy.Restore)
	sessionID := "ses_restart_at_loss_deadline_" + id.New()
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	held := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	held.step()
	status := held.Status()
	if !status.Holding {
		t.Fatalf("pod-a did not take the lease: %+v", status)
	}
	// The holder's validity runs to its last renewal's start + ttl - margin: left of it remains.
	const left = 500 * time.Millisecond
	clock.advance(leaseTTL - leaseMargin - left)
	if got := held.UntilLoss(status.Term); got != left {
		t.Fatalf("the lease's loss deadline is %s away, want %s", got, left)
	}
	rig.relays.term = &relayTerm{ctx: context.Background(), untilLoss: func() time.Duration { return held.UntilLoss(status.Term) }}
	var frozeAt time.Time
	var freeze sync.Once
	rig.deps.consumers.afterStoredRowRead = func() {
		freeze.Do(func() {
			proxy.Freeze()
			frozeAt = time.Now()
		})
	}

	type restarted struct {
		pull    *bus.SessionPull
		outcome relayOutcome
		at      time.Time
	}
	done := make(chan restarted, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*sessionConsumerCallTimeout)
		defer cancel()
		_, pull, outcome := rig.relays.restart(ctx, &relayRun{sessionID: sessionID})
		done <- restarted{pull: pull, outcome: outcome, at: time.Now()}
	}()
	const slack = time.Second
	select {
	case r := <-done:
		proxy.Restore()
		if r.pull != nil {
			r.pull.Stop()
			t.Fatal("a restart whose call had no answer before the lease's loss deadline pulled the session's consumer")
		}
		ran := r.at.Sub(frozeAt)
		if ran < left || ran > left+slack {
			t.Fatalf("a restart started %s before the lease's loss deadline ended %s after it started, want at that deadline: its context did not end there, so it acts on the consumer once another pod may hold the lease", left, ran)
		}
		if r.outcome != relayRetry {
			t.Fatalf("the restart's outcome is %d, want relayRetry (%d)", r.outcome, relayRetry)
		}
	case <-time.After(left + 2*slack):
		proxy.Restore()
		<-done
		t.Fatalf("a restart started %s before the lease's loss deadline was still running %s after it: its context did not end there, so it acts on the consumer once another pod may hold the lease", left, 2*slack)
	}
}

// A restart whose term ends after its RestartAtFloor landed, while it settles the consumer's
// filter, starts no pull: a pull starts only while the term is current (sessionRelays.pullBound),
// so the consumer the restart left is the next holder's to pull. The settle makes a second pass
// because a newer interest row lands under the restart, and the term's context ends inside that
// pass.
func TestARelayRestartWhoseTermEndsAfterItsRestartAtFloorStartsNoPull(t *testing.T) {
	t.Parallel()
	rig, run := newRestartableRun(t, "ses_term_ends_in_settle_")
	termCtx, endTerm := context.WithCancel(context.Background())
	defer endTerm()
	rig.relays.term = &relayTerm{ctx: termCtx}
	reads := 0
	rig.deps.consumers.afterStoredRowRead = func() {
		reads++
		switch reads {
		case 1:
			// The restart has read the row: a newer one lands under it, so its settle corrects
			// the filter the RestartAtFloor writes.
			if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: run.sessionID, MachineID: "test-machine"}, []string{"notifications.dispatch.issue.SETTLEEND.>"}); err != nil {
				t.Errorf("store a newer interest row for %s: %v", run.sessionID, err)
			}
		case 2:
			// The settle's corrective pass has read that row: the term ends.
			endTerm()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, pull, outcome := rig.relays.restart(ctx, run)
	rig.deps.consumers.afterStoredRowRead = nil
	if reads < 2 {
		t.Fatalf("the restart read the session's row %d times, want its own read and its settle's corrective pass", reads)
	}
	if pull != nil {
		pull.Stop()
		t.Fatal("a restart whose term ended while it settled the consumer's filter started a pull of the consumer: this pod pulls it beside the pod that holds the lease next")
	}
	if run.restartedAt.Load() == 0 {
		t.Fatal("the restart recorded no restart, want the RestartAtFloor that landed before the term ended")
	}
	if outcome != relayRetry {
		t.Fatalf("the restart's outcome is %d, want relayRetry (%d)", outcome, relayRetry)
	}
}

// consumerCounts reads sessionID's consumer through rig's own view of the session consumers: the
// pull requests waiting on it, and the messages it delivered that await an ack.
func consumerCounts(t *testing.T, rig *relayRig, sessionID string) (waiting, ackPending int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := rig.consumers.Info(ctx, sessionID)
	if err != nil {
		t.Fatalf("read the consumer of %s: %v", sessionID, err)
	}
	return info.NumWaiting, info.NumAckPending
}

// heldLongLease is pod-a's lease on clock, taken, with a holder's validity of 30 s, as long as a
// pull request waits at the server, so that a row about a pull's term reads the term alone. It
// returns the lease, its bucket, and the term it holds.
func heldLongLease(t *testing.T, clock *fakeLeaseClock) (*lease, bus.KeyValue, uint64) {
	t.Helper()
	kv := leaseBucket(t)
	cfg := productionLeaseConfig()
	cfg.ttl = 30*time.Second + cfg.margin
	held := testLease(kv, "pod-a", cfg, clock.now)
	held.step()
	status := held.Status()
	if !status.Holding {
		t.Fatalf("pod-a did not take the lease: %+v", status)
	}
	return held, kv, status.Term
}

// A relay whose term ends unsubscribes its pull's inbox at once, so the server drops the pull
// request still waiting at the consumer and the next holder is its only puller (relayNATS): a
// message stored after that never reaches the old pod's pull, and the session takes it once, from
// the next holder, on its first delivery. The next holder restarts the consumer with nothing
// awaiting an ack, and such a restart updates the consumer in place and ends no waiting request
// of the old pod's: only the unsubscribe does. The old pod's term ends the way the lease ends it -
// another pod takes the record, the lease steps down and the receive loop's watcher ends the
// term's context - before the term's end reaches the relays (durableReceiver.end), so nothing but
// the term's end makes the run let go. Its pull request would otherwise wait out its 30 s.
//
// The bound on the delivery is 5 s. With the old pod's request gone, the next holder's is the one
// request waiting, so the server hands it the message as soon as it is stored, and the frame and
// its reply take milliseconds. A message handed to the old pod's request instead comes back only
// after the consumer's 60 s ack wait, since nothing restarts the consumer after the next holder's
// own restart.
// It must not run in parallel: the old pull must leave the consumer within 2 s of the term's end.
func TestARelayUnsubscribesItsPullWhenItsTermEndsSoTheNextHolderTakesEachMessage(t *testing.T) {
	old := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	next := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	sessionID := "ses_pull_unsubscribed_" + id.New()
	standIn := startSessionStandIn(t, old.url, sessionID, answerDone)
	old.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	clock := newFakeLeaseClock()
	held, kv, term := heldLongLease(t, clock)
	termCtx, endTerm := context.WithCancel(context.Background())
	defer endTerm()
	old.relays.setOwned(&relayTerm{ctx: termCtx, untilLoss: func() time.Duration { return held.UntilLoss(term) }})
	waitFor(t, "the old pod's pull request to wait at the session's consumer", func() bool {
		waiting, _ := consumerCounts(t, old, sessionID)
		return waiting == 1
	})

	if !stepLeaseDown(t, kv, held) {
		t.Fatal("pod-a's lease did not step down for the record another pod wrote")
	}
	endTerm()
	const gone = 2 * time.Second
	for ended := time.Now(); ; time.Sleep(10 * time.Millisecond) {
		if waiting, _ := consumerCounts(t, old, sessionID); waiting == 0 {
			break
		}
		if time.Since(ended) > gone {
			t.Fatalf("the old pod's pull request still waits at the session's consumer %s after its term ended: the server can hand it a message that then comes back only after the consumer's 60 s ack wait", gone)
		}
	}

	next.relays.setOwned(endlessTerm())
	waitFor(t, "the next holder's pull request to wait at the session's consumer", func() bool {
		waiting, _ := consumerCounts(t, next, sessionID)
		return waiting == 1
	})
	item, seq := old.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	frame := standIn.collect(t, 1, 5*time.Second)[0]
	if frame.seq != seq || frame.item.EventID != item.EventID {
		t.Fatalf("the session took seq %d (event %s), want seq %d (event %s)", frame.seq, frame.item.EventID, seq, item.EventID)
	}
	if got, want := frame.listener(), strings.TrimSuffix(next.relays.replyPrefix, "."); got != want {
		t.Fatalf("the message was relayed by %s, want the next holder %s", got, want)
	}
	if delivered := frame.msg.Header.Get(relayHeaderDelivered); delivered != "1" {
		t.Fatalf("the next holder relayed delivery %s of the message, want its first: an earlier delivery went to the old pod's pull", delivered)
	}
	standIn.none(t, time.Second)
}

// A message that reaches a relay's pull after the relay's term ended is neither forwarded nor
// answered: the pull drops it where it arrives (bus.PullBound), and the session takes it once, from
// the pod that holds the lease next, whose restart of the consumer at its ack floor hands the
// message to it again. The old pod's term ends by its lease's loss deadline passing on the fake
// clock, with the term's context still live: the pull's wait, which runs on the real clock and was
// timed to the 30 s then left, does not wake, so the run neither unsubscribes its pull nor is
// cancelled. That holds open the gap between a term's end and the unsubscribe that follows it
// (relayNATS), in which the old pod's pull request still waits at the consumer, so the server
// delivers the message to the old pod.
// It must not run in parallel: the old pull must take the message within the 1 s the row watches.
func TestARelayLeavesAMessageArrivingAfterItsTermEndedForTheNextHolder(t *testing.T) {
	old := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	sessionID := "ses_after_term_end_" + id.New()
	standIn := startSessionStandIn(t, old.url, sessionID, answerDone)
	old.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	clock := newFakeLeaseClock()
	held, _, term := heldLongLease(t, clock)
	old.relays.setOwned(&relayTerm{ctx: context.Background(), untilLoss: func() time.Duration { return held.UntilLoss(term) }})
	waitFor(t, "the old pod's pull request to wait at the session's consumer", func() bool {
		waiting, _ := consumerCounts(t, old, sessionID)
		return waiting == 1
	})

	clock.advance(30 * time.Second)
	if left := held.UntilLoss(term); left != 0 {
		t.Fatalf("pod-a's lease has %s left before its loss deadline, want none", left)
	}
	item, seq := old.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	select {
	case frame := <-standIn.frames:
		t.Fatalf("the old pod forwarded seq %d (event %s), which reached its pull after its term ended: the session takes it from this pod and again from the next holder, which the server delivers it to unacked", frame.seq, frame.item.EventID)
	case <-time.After(time.Second):
	}
	if _, ackPending := consumerCounts(t, old, sessionID); ackPending != 1 {
		t.Fatalf("%d of the consumer's messages await an ack, want the one the old pod's pull was delivered after its term ended and left unanswered", ackPending)
	}

	// The term's end reaches the old pod's relays, and the next holder's relays take a term.
	old.relays.setOwned(nil)
	next := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	// The next holder's restart at the ack floor recreates this consumer.
	awaitSessionConsumerStored(t, old.harness.client.Conn, sessionID)
	next.relays.setOwned(endlessTerm())
	frame := standIn.collect(t, 1, 15*time.Second)[0]
	if frame.seq != seq || frame.item.EventID != item.EventID {
		t.Fatalf("the session took seq %d (event %s), want seq %d (event %s)", frame.seq, frame.item.EventID, seq, item.EventID)
	}
	if got, want := frame.listener(), strings.TrimSuffix(next.relays.replyPrefix, "."); got != want {
		t.Fatalf("the message was relayed by %s, want the next holder %s", got, want)
	}
	standIn.none(t, time.Second)
}

// answerCountingAcker counts every answer a relay gives a frame - an ack, a progress or a nak -
// and passes each on to the consumer.
type answerCountingAcker struct {
	bus.SessionAcker
	answers atomic.Int32
}

func (a *answerCountingAcker) Ack() error {
	a.answers.Add(1)
	return a.SessionAcker.Ack()
}

func (a *answerCountingAcker) InProgress() error {
	a.answers.Add(1)
	return a.SessionAcker.InProgress()
}

func (a *answerCountingAcker) NakWithDelay(delay time.Duration) error {
	a.answers.Add(1)
	return a.SessionAcker.NakWithDelay(delay)
}

// A role message the relay pulled while its term was current, and judges (takeOverFrame) once the
// term has ended, is neither copied to the role's kept lane nor answered: its claim read, its
// copy and its ack or nak act for this pod, and the pod that holds the lease next may have
// restarted the session's consumer at its ack floor by then. The frame stays delivered and
// unanswered at the consumer, which hands it to that pod's relay, and the relay does not send it.
// No session claims the role, so a judgement under a current term would copy the frame and ack it.
func TestARoleFrameWhoseTakeOverIsReachedAfterTheTermEndedCopiesAndAcksNothing(t *testing.T) {
	t.Parallel()
	const role = "relay-term-ended"
	rig, run := newRestartableRun(t, "ses_take_over_after_term_")
	termCtx, endTerm := context.WithCancel(context.Background())
	defer endTerm()
	rig.relays.term = &relayTerm{ctx: termCtx}
	var claimReads atomic.Int32
	setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayClaimRead: func(string) error {
		claimReads.Add(1)
		return nil
	}})
	subject := contracts.LegacyAgentSubject(run.sessionID)
	item := listenerTestEnvelope(subject, "relay-after-term-"+id.New())
	item.Source = "dispatch"
	item.Role = role
	stored, err := rig.harness.client.JS().Publish(subject, marshalListenerEnvelope(t, item))
	if err != nil {
		t.Fatalf("store a role message on %s: %v", subject, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, pull, outcome := rig.relays.restart(ctx, run)
	if pull == nil {
		t.Fatalf("the restart under a current term started no pull (outcome %d):\n%s", outcome, rig.harness.logs.String())
	}
	defer pull.Stop()
	msg, err := pull.Next(ctx)
	if err != nil || msg.StreamSeq != stored.Sequence {
		t.Fatalf("the pull handed out seq %d (%v), want the role message at %d", msg.StreamSeq, err, stored.Sequence)
	}
	acker := &answerCountingAcker{SessionAcker: msg.SessionAcker}
	msg.SessionAcker = acker

	endTerm()
	taken := rig.relays.takeOverFrame(ctx, run.sessionID, msg)
	if copies := copiesOf(storedEnvelopes(t, rig.harness.client, bus.KeptRoleSubject(role)), item.EventID); copies != 0 {
		t.Fatalf("the role's kept lane holds %d copies of a role message the relay judged after its term ended, want none: the judgement acted once the next holder may hold the session's consumer", copies)
	}
	if answers := acker.answers.Load(); answers != 0 {
		t.Fatalf("the relay answered the consumer %d times for a role message it judged after its term ended, want none", answers)
	}
	if reads := claimReads.Load(); reads != 0 {
		t.Fatalf("the relay read the role's claim %d times for a role message it judged after its term ended, want none", reads)
	}
	if !taken {
		t.Fatal("the relay left a role message it judged after its term ended to be sent to the session, which holds no claim on its role")
	}
	if _, ackPending := consumerCounts(t, rig, run.sessionID); ackPending != 1 {
		t.Fatalf("%d of the consumer's messages await an ack, want the role message the relay left unanswered", ackPending)
	}
}
