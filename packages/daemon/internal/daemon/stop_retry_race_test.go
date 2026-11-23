package daemon

// A stop row that reads its claim failed or retired gives up only once a decision in flight has
// ended (claimToStop, supervise.Machine.Claim). The outbox's suspend and release rows and a pause's
// stop each read the claim's published view first (supervise.Machine.View), and a retry of a
// failed claim leaves that view reading failed until the retry's launch is first written: a stop
// that gave up on that read would leave the retry to launch the claim the stop was for
// (dispatch://INFRA-5297).

import (
	"context"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// heldPace is a wake pacing (supervise.WakePace) that lets every launch out the moment it is
// reserved, but for the one reservation it is armed for, which waits until release. A launch
// reserves its wake inside its decision, which holds its machine, after the project pause's check
// and before the launch's first write (supervise's reserveWake, writeTry), so while that
// reservation waits the published view still reads the claim as the decision found it.
type heldPace struct {
	armed   atomic.Bool
	entered chan struct{}
	let     chan struct{}
	release func()
}

// holdingPace sets a heldPace on sup's machines. It is set before any machine is made, since a
// machine takes the deps it is made with, and its reservation is let go when the test ends, so a
// test that fails while a decision waits in it leaves no machine held.
func holdingPace(t *testing.T, sup *supervisor) *heldPace {
	t.Helper()
	p := &heldPace{entered: make(chan struct{}), let: make(chan struct{})}
	var once sync.Once
	p.release = func() { once.Do(func() { close(p.let) }) }
	t.Cleanup(p.release)
	sup.deps.WakePace = supervise.WakePace{Reserve: p.reserve}
	return p
}

func (p *heldPace) reserve(now time.Time) time.Time {
	if p.armed.CompareAndSwap(true, false) {
		close(p.entered)
		<-p.let
	}
	return now
}

// goroutineHeader is the first line of the calling goroutine's stack dump, "goroutine N [running]:"
// cut to its "goroutine N [" prefix, which names that goroutine in a dump of every goroutine.
func goroutineHeader() string {
	buf := make([]byte, 64)
	buf = buf[:goruntime.Stack(buf, false)]
	header, _, _ := strings.Cut(string(buf), "[")
	return header + "["
}

// waitsOnADecision is whether the goroutine whose dump starts with header, the one running the
// stop row (outbox.execute), is inside supervise.Machine.Claim, which takes its machine's lock and
// so waits out the decision that holds it. A dump of every goroutine names the frame whether or not
// the compiler inlined it, and the header keeps another test's rows, which run in parallel, out of
// it.
func waitsOnADecision(header string) bool {
	buf := make([]byte, 1<<20)
	n := goruntime.Stack(buf, true)
	for n == len(buf) {
		buf = make([]byte, 2*len(buf))
		n = goruntime.Stack(buf, true)
	}
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.HasPrefix(g, header) {
			return strings.Contains(g, "daemon.(*outbox).execute(") && strings.Contains(g, "supervise.(*Machine).Claim(")
		}
	}
	return false
}

// stopRacingARetry runs the row stop makes through runner's executor while the failed tester's
// retry, asked as the outbox's start of a failed claim asks it (supervise.RequestRetry), waits
// inside its decision at pace. stop is called once the retry waits there, so what it does — a
// project's pause among it — comes after the retry passed the pause's check. The retry is let go
// only once the row has returned or waits on the retry's decision (waitsOnADecision): the row reads
// the claim inside the window however fast the machine runs it. It answers the row's error.
func stopRacingARetry(t *testing.T, runner *outbox, pace *heldPace, pair podPair, stop func() record.OutboxRow) error {
	t.Helper()
	ctx := context.Background()
	pace.armed.Store(true)
	retried := make(chan error, 1)
	go func() { retried <- pair.waiting.Handle(ctx, supervise.RequestRetry{Claim: pair.waitingToken}) }()
	testwait.Receive(t, "the tester's retry to reserve its wake", pace.entered)
	if c := pair.waiting.View().Claim; c.State != supervise.StateFailed {
		t.Fatalf("the tester is published %s while its retry waits before its first write, want %s: the window this test is for",
			c.State, supervise.StateFailed)
	}

	row := stop()
	done := make(chan error, 1)
	running := make(chan string, 1)
	go func() {
		running <- goroutineHeader()
		done <- runner.execute(ctx, row)
	}()
	header := testwait.Receive(t, "the stop row's goroutine to start", running)
	var stopped error
	returned := false
	testwait.Eventually(t, "the stop row to return, or to wait on the retry's decision", func() bool {
		select {
		case stopped = <-done:
			returned = true
			return true
		default:
			return waitsOnADecision(header)
		}
	})
	if returned {
		t.Logf("the stop row returned before the retry was let go, answering %v", stopped)
	} else {
		t.Log("the stop row waits on the retry's decision")
	}
	pace.release()
	if err := testwait.Receive(t, "the tester's retry", retried); err != nil {
		t.Fatalf("retry the tester: %v", err)
	}
	if !returned {
		stopped = testwait.Receive(t, "the stop row", done)
	}
	return stopped
}

// TestAStopRowStopsTheClaimARetryInFlightLaunches holds a failed tester's retry inside its
// decision, past the pause's check and before its first write, while the implementer's load holds
// the pod, and runs a stop row of the tester meanwhile: the row reads the tester failed in its
// published view, and must stop the launch the retry then makes rather than finish without acting.
// The retry's launch parks at the pod's gate behind the load (supervise's askPod), so the stop
// takes it out of the gate and the tester's process is never asked of the runtime.
func TestAStopRowStopsTheClaimARetryInFlightLaunches(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  func(t *testing.T, r *podStopRig) record.OutboxRow
		by   supervise.SuspendSource
	}{
		{name: "the outbox's suspend row", row: testerSuspendRow, by: supervise.SuspendedByDaemon},
		{name: "the outbox's release row", row: testerReleaseRow, by: supervise.SuspendedByReleased},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPodStopRig(t, podIssueRecord(), true)
			pace := holdingPace(t, r.sup)
			pair := loadingPair(t, r.sup, supervise.StateFailed)

			if err := stopRacingARetry(t, r.runner, pace, pair, func() record.OutboxRow { return tc.row(t, r) }); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			pair.wantStopped(t, r.rt, r.logs, supervise.StateSuspended, tc.by)
		})
	}

	t.Run("a pause's stop", func(t *testing.T) {
		p := newPauseRig(t)
		sup, rt := p.supervisor()
		sup.deps.PauseStopped = p.pauses.stopped
		p.pauses.supervisor = sup
		logs := podGated(sup)
		pace := holdingPace(t, sup)
		pair := loadingPair(t, sup, supervise.StateFailed)

		// The project is paused once the retry has passed the pause's check, which is the start
		// row of a failed claim racing a pause's stop on a lane of its own.
		var held store.Pause
		if err := stopRacingARetry(t, podPauseRunner(p, sup), pace, pair, func() record.OutboxRow {
			held = p.pauseLegion()
			return testerPauseStopRow(t, held.ID)
		}); err != nil {
			t.Fatalf("the pause's stop: %v", err)
		}
		pair.wantStopped(t, rt, logs, supervise.StateSuspended, supervise.SuspendedByReleased)
		if got := p.stops(held.ID, claim.RoleTester); len(got) != 1 || got[0] != (pauseStopRecord{end: store.PauseStopStopped, owed: true}) {
			t.Fatalf("the tester's stops = %+v, want one stopped and owed the launch the retry made", got)
		}
	})
}
