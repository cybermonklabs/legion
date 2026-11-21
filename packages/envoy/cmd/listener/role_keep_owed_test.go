package main

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A kept copy the role's holder does not take for its delivery mode is answered with the
// delivery_failed exception every forward path gives that refusal (refusedRoleForwardOutcome),
// and the exception goes out though the probe of the main client reads a false "down" for every
// read the exception's own publish makes - a read a loaded box keeps waiting past the probe's
// budget, made here by the seam, keyed to the publish's own frames (publishDeliveryException).
// The handover's reporter runs the copy's report off the role's guard, under the context of the
// handover's caller, which no guard marks (keptRoleOwed), so the publish waits for a connection
// that is down within its own bound and never asks the probe.
//
// The control is the report run under the walk's context, marked as made under the role's guard,
// as at 820ab98fa6: the publish asks the probe, reads the false "down" as the connection lost,
// answers it at once, and the exception is logged as failed and never sent, so the row fails at
// its read of the exception.
func TestAKeptCopysRefusalExceptionGoesOutThoughTheProbeReadsAFalseDown(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	var forwards atomic.Int32
	cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
		forwards.Add(1)
		return nil
	})
	const (
		role   = "kept-report-false-down"
		holder = "ses_kept_report_false_down"
	)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-1")
	item.Payload = `{"delivery":{"mode":"steer"}}`
	if _, err := keepRoleMessage(cfg, role, item); err != nil {
		t.Fatalf("keep the role message; the listener log says why:\n%s", harness.logs.String())
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}})
	exceptions, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + role)
	if err != nil {
		t.Fatalf("subscribe to the role's exceptions: %v", err)
	}
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}
	var downs atomic.Int64
	restore := harness.client.ProbeReadsDownForTest(func() bool {
		if !inFrame(listenerPackage + ".publishDeliveryException") {
			return false
		}
		downs.Add(1)
		return true
	})
	defer restore()

	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	if err := drainKeptRole(ctx, cfg, role, time.Now); err != nil {
		t.Fatalf("the handover answered %v", err)
	}

	msg, err := exceptions.NextMsg(testnats.Remaining(t))
	if err != nil {
		t.Fatalf("no delivery exception for the refused copy by the test's deadline (%v); %d probe reads of its publish read down; log:\n%s", err, downs.Load(), harness.logs.String())
	}
	var exception contracts.Envelope
	if err := json.Unmarshal(msg.Data, &exception); err != nil {
		t.Fatalf("decode the exception envelope %q: %v", msg.Data, err)
	}
	var payload deliveryExceptionPayload
	if err := json.Unmarshal([]byte(exception.Payload), &payload); err != nil {
		t.Fatalf("decode the exception payload %q: %v", exception.Payload, err)
	}
	if payload.Reason != "delivery_failed" || payload.EventID != item.EventID {
		t.Fatalf("the exception names reason %q for event %q, want delivery_failed for %q", payload.Reason, payload.EventID, item.EventID)
	}
	if logs := harness.logs.String(); strings.Contains(logs, exceptionPublishFailedLine) {
		t.Fatalf("the handover logged %q:\n%s", exceptionPublishFailedLine, logs)
	}
	if n := forwards.Load(); n != 0 {
		t.Fatalf("the handover forwarded the refused copy %d times", n)
	}
}

// A forward's confirm, the walk's last cursor write for the copy, whose acknowledgement a drop took
// while the stream stored the write, and whose read-back under the role's guard the loss answers
// at once, is settled once the handover has given the guard back and the connection is up
// (keptRoleOwed.settle): the first cursor message after the walk's claim is the write, so the copy
// is reported once, with its line and its dedupe record. The hold withholds the write's
// acknowledgement and then refuses every reconnect (testnats.ReplyHold.Cut) until the row has taken
// the role's guard, which the handover gives back once its walk has ended on the loss, so the
// read-back is made with the connection down.
//
// The control is the walk at 820ab98fa6, whose confirm took a read-back that did not answer for a
// write that did not land: the copy went unreported, the next walk started past it, and the row
// fails at its count of the forward's line.
func TestAKeptForwardsConfirmWhoseReadBackTheLossCutIsSettledAndReportedOnce(t *testing.T) {
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1), bus.WithConnectTimeout(testnats.Remaining(t)))
	if err != nil {
		t.Fatalf("connect the main client through the hold: %v", err)
	}
	t.Cleanup(client.Close)
	t.Cleanup(hold.Restore)
	harness := newListenerDeliveryHarnessOn(t, client)
	cfg := harness.config
	const (
		role   = "kept-unsettled-confirm"
		holder = "ses_kept_unsettled_confirm"
	)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-1")
	if _, err := keepRoleMessage(cfg, role, item); err != nil {
		t.Fatalf("keep the role message; the listener log says why:\n%s", harness.logs.String())
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	// The forward stands in for the stream's acknowledgement, and holds the acknowledgement of the
	// next cursor write, the walk's confirm of the copy.
	helds := make(chan *testnats.HeldReplies, 1)
	cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
		select {
		case helds <- hold.Hold(bus.KeptRoleCursorSubject(role)):
		default:
		}
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- drainKeptRole(ctx, cfg, role, time.Now) }()
	var held *testnats.HeldReplies
	select {
	case held = <-helds:
	case <-time.After(testnats.Remaining(t)):
		t.Fatalf("the handover forwarded nothing by the test's deadline; log:\n%s", harness.logs.String())
	}
	select {
	case <-held.Withheld():
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the stream's acknowledgement of the confirm's cursor write was never withheld by the test's deadline")
	}
	hold.Cut()
	release, taken := cfg.roleKeepDrains.hold(ctx, role)
	if !taken {
		t.Fatalf("the handover never gave the role's guard back by the test's deadline; log:\n%s", harness.logs.String())
	}
	release()
	hold.Restore()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("the handover answered %v", err)
		}
	case <-time.After(testnats.Remaining(t)):
		t.Fatalf("the handover had not ended by the test's deadline; log:\n%s", harness.logs.String())
	}
	if n := strings.Count(harness.logs.String(), "listener role forwarded"); n != 1 {
		t.Fatalf("the forwarded copy was reported %d times, want once; log:\n%s", n, harness.logs.String())
	}
	if !cfg.dedupeCache.Seen(item.DedupeKey, holder) {
		t.Fatalf("the dedupe cache holds no record of %s for %s", item.DedupeKey, holder)
	}
}

// The reports a handover owes run as they are owed, on the handover's reporter, so what it holds
// does not grow with the copies its walks end: each of a hundred reports runs before the next is
// owed, and the queue is empty each time one has run. The wait for each is bounded only by the
// test's deadline.
//
// The control is owe with no reporter started, as the handover at 7eb4320b67 held its reports until
// it ended: the first report never runs, and the row fails at its wait for it.
func TestAHandoversReportsRunAsTheyAreOwedAndQueueNothing(t *testing.T) {
	owed := newKeptRoleOwed(t.Context(), listenerDeliveryHandlerConfig{}, "kept-queue")
	queued := func() int {
		owed.mu.Lock()
		defer owed.mu.Unlock()
		return len(owed.queue)
	}
	for i := range 100 {
		ran := make(chan struct{})
		owed.owe(func(context.Context) { close(ran) })
		select {
		case <-ran:
		case <-time.After(testnats.Remaining(t)):
			t.Fatalf("report %d had not run by the test's deadline, with %d reports queued", i, queued())
		}
		owed.settle(listenerDeliveryHandlerConfig{})
		if n := queued(); n != 0 {
			t.Fatalf("after report %d ran, %d reports are still queued", i, n)
		}
	}
}

// A confirm whose answer was lost and whose read-back did not answer is settled by its own token
// (bus.KeptRoleCursor.Confirm). When its write did not land, the cursor written after the walk's
// claim can be another walk's {kept} of the same copy - a forward repeat this listener's attempt
// record ends with no claim (keptCopyEnd), once the claim is no longer outstanding - whose kept and
// claim are the confirm's own: the settle does not take it for the confirm, so the copy's forward is
// not reported beside that walk's report of it, and the copy counts once. A confirm that did land is
// settled as landed, and reported. The other walk's write is {"kept":9,"claiming":0}, byte for byte
// what a listener built before the token writes, so the row stands for that listener's walk too.
//
// The control is the settle at 7eb4320b67, which compared the cursor's kept and claim alone: it
// takes the other walk's {kept} for its confirm, and the row fails at its count of the first
// settle's reports.
func TestAnUnsettledConfirmIsNotTakenForAnotherWalksWriteAfterItsClaim(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	const role = "kept-unsettled-token"
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	lane := &roleKeepLaneCursor{cfg: cfg, role: role, subject: bus.KeptRoleCursorSubject(role)}
	settle := func(claim uint64, confirm bus.KeptRoleCursor) int32 {
		t.Helper()
		var reported atomic.Int32
		owed := newKeptRoleOwed(ctx, cfg, role)
		owed.unsettled = append(owed.unsettled, unsettledCursorWrite{role: role, claim: claim, cursor: confirm,
			report: func(context.Context) { reported.Add(1) }})
		owed.settle(cfg)
		return reported.Load()
	}

	// The walk's claim of copy 9, whose confirm did not land; another walk then ended copy 9.
	if err := lane.write(ctx, bus.KeptRoleCursor{Kept: 8, Claiming: 9}); err != nil {
		t.Fatalf("write the walk's claim: %v", err)
	}
	claim := lane.seq
	if err := lane.write(ctx, bus.KeptRoleCursor{Kept: 9}); err != nil {
		t.Fatalf("write the other walk's end of the copy: %v", err)
	}
	if n := settle(claim, bus.KeptRoleCursor{Kept: 9, Confirm: id.New()}); n != 0 {
		t.Fatalf("the settle reported a confirm that did not land %d times, taking the other walk's write after its claim for its own", n)
	}

	// The walk's claim of copy 10, whose confirm landed.
	if err := lane.write(ctx, bus.KeptRoleCursor{Kept: 9, Claiming: 10}); err != nil {
		t.Fatalf("write the walk's second claim: %v", err)
	}
	claim = lane.seq
	confirm := bus.KeptRoleCursor{Kept: 10, Confirm: id.New()}
	if err := lane.write(ctx, confirm); err != nil {
		t.Fatalf("write the walk's confirm: %v", err)
	}
	if n := settle(claim, confirm); n != 1 {
		t.Fatalf("the settle reported a confirm that landed %d times, want once", n)
	}
}

// inFrame reports whether the calling goroutine is running function, named as the runtime names
// it (the package path, a dot, the function): a frame of it is on the stack. A row keys the probe's
// false "down" to the reads one function makes with it (bus.Client.ProbeReadsDownForTest).
func inFrame(function string) bool {
	pcs := make([]uintptr, 128)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if frame.Function == function {
			return true
		}
		if !more {
			return false
		}
	}
}
