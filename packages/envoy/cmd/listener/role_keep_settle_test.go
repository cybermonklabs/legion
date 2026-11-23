package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A handover whose confirm of a forwarded copy could not be read back - its acknowledgement
// withheld until the publish's bound ended, which is no loss of the connection, and its read-back
// answering a false "down" - ends on that write, settles it once the guard is given back (the write
// landed, so the copy is reported), and runs again at once: the lane's next copy is forwarded by
// the same handover, not left to the next sweep, and no handover failure is logged at ERROR. The
// hold withholds the acknowledgement of the cursor writes from the first forward on, and holds
// nothing more once it has withheld one; the probe reads "down" for the first read-back alone,
// keyed to bus.Client.ReadKeptRoleCursor's frames. Waits are bounded only by the test's deadline.
//
// The control is the drain at 263ef43b50, which ran no handover again after a settle: the handover
// ends with the first copy, reports the confirm's time-out as a failed drain, and the row fails at
// its check that the second copy was forwarded.
func TestAHandoverWhoseConfirmCouldNotBeReadBackGoesOnToTheLanesNextCopy(t *testing.T) {
	goesOnAfterAnUnreadableConfirm(t, "kept-settle-goes-on", func(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) error {
		return drainKeptRole(ctx, cfg, role, time.Now)
	})
}

// The same, for a handover the keep sweep started (handOverSweptLane): once the confirm is settled
// with the guard given back, the sweep's handover takes the guard again and runs again, so the
// lane's next copy is forwarded in the same pass of the sweep rather than the next one, five
// minutes on. The pass ends once every handover it started has (sweepOnce).
//
// The control is E1b, the sweep's run again after a settle taken out (role_keep_sweep.go, the
// `owed.settle(cfg)` branch that sets again): the sweep's handover ends with the first copy, and
// the row fails at its check that the second copy was forwarded.
func TestASweptHandoverWhoseConfirmCouldNotBeReadBackGoesOnToTheLanesNextCopy(t *testing.T) {
	goesOnAfterAnUnreadableConfirm(t, "kept-settle-sweep-goes-on", func(ctx context.Context, cfg listenerDeliveryHandlerConfig, _ string) error {
		sweepOnce(ctx, cfg)
		return nil
	})
}

// goesOnAfterAnUnreadableConfirm keeps two copies for a live holder of role, makes the first
// forward's confirm unreadable as the rows above say, hands the lane over through handOver, and
// wants both copies forwarded in order, each reported once, and nothing logged at ERROR.
func goesOnAfterAnUnreadableConfirm(t *testing.T, role string, handOver func(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) error) {
	t.Helper()
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1), bus.WithConnectTimeout(testnats.Remaining(t)))
	if err != nil {
		t.Fatalf("connect the main client through the hold: %v", err)
	}
	t.Cleanup(client.Close)
	harness := newListenerDeliveryHarnessOn(t, client)
	cfg := harness.config
	holder := "ses_" + strings.ReplaceAll(role, "-", "_")
	first := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-1")
	second := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-2")
	for _, item := range []contracts.Envelope{first, second} {
		if _, err := keepRoleMessage(cfg, role, item); err != nil {
			t.Fatalf("keep %s; the listener log says why:\n%s", item.DedupeKey, harness.logs.String())
		}
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	var (
		armed     atomic.Bool
		holding   sync.Once
		mu        sync.Mutex
		forwarded []string
	)
	cfg.forwardRole = inlineForward(func(_ string, item contracts.Envelope, _ string, _ time.Duration) error {
		mu.Lock()
		forwarded = append(forwarded, item.DedupeKey)
		mu.Unlock()
		holding.Do(func() {
			held := hold.Hold(bus.KeptRoleCursorSubject(role))
			armed.Store(true)
			go func() {
				<-held.Withheld()
				hold.Hold("unused.after.the.first.withheld.ack")
			}()
		})
		return nil
	})
	var readBacks atomic.Int64
	restore := client.ProbeReadsDownForTest(func() bool {
		return armed.Load() && inFrame("github.com/sjawhar/envoy/internal/bus.(*Client).ReadKeptRoleCursor") && readBacks.Add(1) == 1
	})
	defer restore()

	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	if err := handOver(ctx, cfg, role); err != nil {
		t.Fatalf("the handover answered %v; log:\n%s", err, harness.logs.String())
	}

	mu.Lock()
	got := append([]string(nil), forwarded...)
	mu.Unlock()
	if len(got) != 2 || !strings.HasSuffix(got[0], first.DedupeKey) || !strings.HasSuffix(got[1], second.DedupeKey) {
		t.Fatalf("the handover forwarded %v, want the first copy and then the second; log:\n%s", got, harness.logs.String())
	}
	logs := harness.logs.String()
	if n := strings.Count(logs, "listener role forwarded"); n != 2 {
		t.Fatalf("the handover reported %d forwards, want each copy once; log:\n%s", n, logs)
	}
	if strings.Contains(logs, `"level":"ERROR"`) {
		t.Fatalf("the handover logged at ERROR:\n%s", logs)
	}
	if readBacks.Load() == 0 {
		t.Fatal("no read-back of the confirm was made, so the row did not reach the write it means")
	}
}

// A report the handover owes once the listener's stop has given up its wait for the handovers
// (roleKeepGuards.await ended by its deadline) is not run: the stop goes on to drain the role lane
// and then the main client the report publishes through, so a report started then races those
// drains. It is dropped with a warning that names the stop. Here a handover the stop waits for
// never returns, the stop's wait ends at a deadline already past, and a report owed afterwards
// must not run.
//
// The control is the reporter at 263ef43b50, which ran every report it was owed whatever the stop
// had done, so the report runs and the row fails at its check.
func TestAReportOwedAfterTheStopGaveUpItsWaitIsDropped(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	guards := newRoleKeepGuards()
	cfg.roleKeepDrains = guards
	stuck := make(chan struct{})
	defer close(stuck)
	guards.start(func(context.Context) { <-stuck })
	ended, end := context.WithCancel(context.Background())
	end()
	if guards.await(ended) {
		t.Fatal("the stop's wait reported every handover returned while one still runs")
	}
	owed := newKeptRoleOwed(context.Background(), cfg, "kept-stop-gave-up")
	var ran atomic.Bool
	owed.owe(func(context.Context) { ran.Store(true) })
	owed.settle(cfg)
	if ran.Load() {
		t.Fatal("a report owed after the stop gave up its wait ran, against the drains the stop makes next")
	}
	if !strings.Contains(harness.logs.String(), "listener kept role report dropped: the listener's stop gave up its wait") {
		t.Fatalf("the dropped report was not logged; log:\n%s", harness.logs.String())
	}
}

// End to end: a forward's confirm that did not land, whose read-back did not answer either, ends
// the handover; the handover settles it with the guard given back and finds it did not land, so the
// copy is not reported as delivered; and the handover's next pass, once the claim is no longer
// outstanding (its clock two claim windows on), takes the claim over and ends the copy as a repeat
// its attempt record holds. The copy reaches the holder once and is counted once, as a repeat, and
// the cursor ends past it. The probe reads "down" for the two reads made in the confirm's frames,
// the write's wait for its connection and the read-back, so the write is never sent.
//
// The control is a settle that takes an unknown write as landed without reading it: the copy is
// then reported as forwarded beside the takeover's repeat, and the row fails at its count.
func TestAnUnknownConfirmIsSettledThenTakenOverAndCountedOnce(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	const (
		role   = "kept-settle-takeover"
		holder = "ses_kept_settle_takeover"
	)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-1")
	kept, err := keepRoleMessage(cfg, role, item)
	if err != nil {
		t.Fatalf("keep the role message; the listener log says why:\n%s", harness.logs.String())
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	var forwards atomic.Int32
	cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
		forwards.Add(1)
		return nil
	})
	var confirmReads atomic.Int64
	restore := harness.client.ProbeReadsDownForTest(func() bool {
		return inFrame(listenerPackage+".(*roleKeepLaneCursor).confirm") && confirmReads.Add(1) <= 2
	})
	defer restore()
	later := func() time.Time { return time.Now().Add(2 * bus.KeptRoleClaimWindow) }

	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	if err := drainKeptRole(ctx, cfg, role, later); err != nil {
		t.Fatalf("the handover answered %v; log:\n%s", err, harness.logs.String())
	}

	logs := harness.logs.String()
	delivered := strings.Count(logs, "listener role forwarded")
	repeats := strings.Count(logs, keptRoleRefusalLines.repeat)
	if n := forwards.Load(); n != 1 || delivered != 0 || repeats != 1 {
		t.Fatalf("the copy was forwarded %d times, reported delivered %d times and as a repeat %d times; want forwarded once and counted once, as the repeat:\n%s", n, delivered, repeats, logs)
	}
	stored, err := harness.client.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		t.Fatalf("read the cursor: %v", err)
	}
	if stored.Cursor.Kept != kept.sequence || stored.Cursor.Claiming != 0 {
		t.Fatalf("the cursor stands at %+v, want past the copy at %d with nothing claimed", stored.Cursor, kept.sequence)
	}
	if confirmReads.Load() < 2 {
		t.Fatalf("%d reads were made in the confirm's frames, want its write's wait and its read-back", confirmReads.Load())
	}
}
