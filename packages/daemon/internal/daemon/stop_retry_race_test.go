package daemon

// A stop row that reads its claim failed or retired gives up only once a decision in flight has
// ended (claimToStop). The outbox's suspend and release rows and a pause's stop each read the
// claim's published view first (supervise.Machine.View), and an operator's retry of a failed claim
// (supervise.RequestOperatorRetry, api's machineRetry) leaves that view reading failed until the
// retry's launch is first written: a stop that gave up on that read would leave the retry to launch
// the claim the stop was for (dispatch://INFRA-5297). The workflow's own start of a failed claim is
// no such competitor for these rows: it runs on the issue's lifecycle lane, one row at a time with
// them.

import (
	"context"
	"log/slog"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// heldLaunch holds the first launch of one claim that its machine lets out of the wake gate, at
// the line the gate writes as it lets the launch out (supervise.WakeRanMessage), until release.
// The gate writes that line inside the launch's decision, which holds the machine, after the
// project pause's check and before the launch's first write, for a launch the wake pacing lets
// through, as an operator's retry is (supervise's exemptWake), as for one it paces (supervise's
// reserveWake, wakeRan, writeTry). So while the line waits, the published view still reads the
// claim as the decision found it.
type heldLaunch struct {
	claim   string
	armed   atomic.Bool
	entered chan struct{}
	let     chan struct{}
	release func()
}

// holdingLaunch sets a heldLaunch of token's claim on sup's machines, around the handler of their
// log. It is set before any machine is made, since a machine takes the deps it is made with, and
// the held launch is let go when the test ends, so a test that fails while a decision waits in it
// leaves no machine held.
func holdingLaunch(t *testing.T, sup *supervisor, token claim.Token) *heldLaunch {
	t.Helper()
	h := &heldLaunch{claim: string(token), entered: make(chan struct{}), let: make(chan struct{})}
	var once sync.Once
	h.release = func() { once.Do(func() { close(h.let) }) }
	t.Cleanup(h.release)
	sup.deps.Log = slog.New(heldLaunchLog{Handler: sup.deps.Log.Handler(), held: h})
	return h
}

// heldLaunchLog is a machine's log handler that holds the wake gate's line of the held claim while
// its heldLaunch is armed, then writes it. A machine's logger names its claim (supervise's
// NewMachine), which reaches the handler through WithAttrs.
type heldLaunchLog struct {
	slog.Handler
	held  *heldLaunch
	claim string
}

func (l heldLaunchLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	named := l.claim
	for _, attr := range attrs {
		if attr.Key == "claim" {
			named = attr.Value.String()
		}
	}
	return heldLaunchLog{Handler: l.Handler.WithAttrs(attrs), held: l.held, claim: named}
}

func (l heldLaunchLog) WithGroup(name string) slog.Handler {
	return heldLaunchLog{Handler: l.Handler.WithGroup(name), held: l.held, claim: l.claim}
}

func (l heldLaunchLog) Handle(ctx context.Context, line slog.Record) error {
	if line.Message == supervise.WakeRanMessage && l.claim == l.held.claim && l.held.armed.CompareAndSwap(true, false) {
		close(l.held.entered)
		<-l.held.let
	}
	return l.Handler.Handle(ctx, line)
}

// goroutineHeader is the first line of the calling goroutine's stack dump, "goroutine N [running]:"
// cut to its "goroutine N [" prefix, which names that goroutine in a dump of every goroutine.
func goroutineHeader() string {
	buf := make([]byte, 64)
	buf = buf[:goruntime.Stack(buf, false)]
	header, _, _ := strings.Cut(string(buf), "[")
	return header + "["
}

// frameOf is the frame a goroutine dump writes for a call of method, a method expression such as
// (*T).M: its name as the runtime gives it, then the open parenthesis of its arguments. A method
// expression's name is the method's own, where a method value's (v.M) ends in -fm, which no frame
// of a call carries. The name is read off the method, so a rename of the method changes it.
func frameOf(method any) string {
	return goruntime.FuncForPC(reflect.ValueOf(method).Pointer()).Name() + "("
}

// stopRowFrame and claimReadFrame are the frames of a stop row run through the outbox's executor
// (outbox.execute) and of a read of a claim that waits out the decision in flight
// (supervise.Machine.Claim).
var (
	stopRowFrame   = frameOf((*outbox).execute)
	claimReadFrame = frameOf((*supervise.Machine).Claim)
)

// waitsOnADecision is whether the goroutine whose dump starts with header, the one running the
// stop row (stopRowFrame), is inside supervise.Machine.Claim (claimReadFrame), which takes its
// machine's lock and so waits out the decision that holds it. A dump of every goroutine names the
// frame whether or not the compiler inlined it, and the header keeps another test's rows, which
// run in parallel, out of it.
func waitsOnADecision(header string) bool {
	buf := make([]byte, 1<<20)
	n := goruntime.Stack(buf, true)
	for n == len(buf) {
		buf = make([]byte, 2*len(buf))
		n = goruntime.Stack(buf, true)
	}
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.HasPrefix(g, header) {
			return strings.Contains(g, stopRowFrame) && strings.Contains(g, claimReadFrame)
		}
	}
	return false
}

// stopRacingARetry runs the row stop makes through runner's executor while an operator's retry of
// the failed tester (supervise.RequestOperatorRetry) waits inside its decision at held. stop is
// called once the retry waits there, so what it does — a project's pause among it — comes after
// the retry passed the pause's check. The row must wait on the retry's decision (waitsOnADecision,
// claimToStop), and the retry is let go only once it does: the row reads the claim inside the
// window however fast the machine runs it. A row that returns first read the tester failed and
// gave up while the retry's decision held the machine, and fails the test there, whatever the
// rig's clock would let the pause's gate do with the launch afterwards. It answers the row's error.
func stopRacingARetry(t *testing.T, runner *outbox, held *heldLaunch, pair podPair, stop func() record.OutboxRow) error {
	t.Helper()
	ctx := context.Background()
	held.armed.Store(true)
	retried := make(chan error, 1)
	go func() { retried <- pair.waiting.Handle(ctx, supervise.RequestOperatorRetry{Claim: pair.waitingToken}) }()
	testwait.Receive(t, "the operator's retry of the tester to pass the wake gate", held.entered)
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
		t.Fatalf("the stop row returned before the retry it races was let go, answering %v: it read the tester failed and gave up "+
			"while the retry's decision held the machine, so the retry would launch the claim the row was to stop", stopped)
	}
	t.Log("the stop row waits on the retry's decision")
	held.release()
	if err := testwait.Receive(t, "the operator's retry of the tester", retried); err != nil {
		t.Fatalf("retry the tester: %v", err)
	}
	return testwait.Receive(t, "the stop row", done)
}

// pauseHeldLine is the line a launch writes when the project's pause holds it at the pause's gate
// (supervise's pauseGate).
const pauseHeldLine = "supervise: the project is paused; its gate holds this claim's launch"

// TestAStopRowStopsTheClaimARetryInFlightLaunches holds an operator's retry of a failed tester
// inside its decision, past the pause's check and before its first write, while the implementer's
// load holds the pod, and runs a stop row of the tester meanwhile: the row reads the tester failed
// in its published view, and must stop the launch the retry then makes rather than finish without
// acting. The tester's process is never asked of the runtime.
func TestAStopRowStopsTheClaimARetryInFlightLaunches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		row  func(t *testing.T, r *podStopRig) record.OutboxRow
		by   supervise.SuspendSource
	}{
		{name: "the outbox's suspend row", row: testerSuspendRow, by: supervise.SuspendedByDaemon},
		{name: "the outbox's release row", row: testerReleaseRow, by: supervise.SuspendedByReleased},
	} {
		// The retry's launch waits at the pod's gate behind the load (supervise's askPod), so the
		// stop takes it out of the gate.
		t.Run(tc.name, func(t *testing.T) {
			r := newPodStopRig(t, podIssueRecord(), true)
			held := holdingLaunch(t, r.sup, mustClaimToken(t, pauseIssue, claim.RoleTester))
			pair := loadingPair(t, r.sup, supervise.StateFailed)

			if err := stopRacingARetry(t, r.runner, held, pair, func() record.OutboxRow { return tc.row(t, r) }); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			pair.wantStopped(t, r.rt, r.logs, supervise.StateSuspended, tc.by)
		})
	}

	// The project is paused once the operator's retry has passed the pause's check, and the
	// pause's stop of the tester is a `pause_stop` row made by hand: production writes one for a
	// claim the pause's transaction or its catch-up found live (projectPause.Paused), so one that
	// reads this tester failed comes only in rarer orders. The gap the catch-up leaves, a launch past
	// the pause's check whose first write it has not seen, is closed in the launch itself, which
	// checks the pause again after that write (supervise's launchTries): the retry's launch never
	// asks the pod's gate, and the pause's gate holds it until the stop takes it.
	t.Run("a pause's stop", func(t *testing.T) {
		p := newPauseRig(t)
		sup, rt := p.supervisor()
		sup.deps.PauseStopped = p.pauses.stopped
		p.pauses.supervisor = sup
		logs := podGated(sup)
		held := holdingLaunch(t, sup, mustClaimToken(t, pauseIssue, claim.RoleTester))
		pair := loadingPair(t, sup, supervise.StateFailed)

		var pause store.Pause
		if err := stopRacingARetry(t, podPauseRunner(p, sup), held, pair, func() record.OutboxRow {
			pause = p.pauseLegion()
			return testerPauseStopRow(t, pause.ID)
		}); err != nil {
			t.Fatalf("the pause's stop: %v", err)
		}
		pair.wantNeverStarted(t, rt, supervise.StateSuspended, supervise.SuspendedByReleased)
		if pod, paused := pair.testerLines(t, logs, waitsAtGateLine), pair.testerLines(t, logs, pauseHeldLine); pod != 0 || paused != 1 {
			t.Fatalf("the tester's launch waited at the pod's gate %d times and at the pause's %d times, want 0 and 1; log:\n%s",
				pod, paused, logs.String())
		}
		if got := p.stops(pause.ID, claim.RoleTester); len(got) != 1 || got[0] != (pauseStopRecord{end: store.PauseStopStopped, owed: true}) {
			t.Fatalf("the tester's stops = %+v, want one stopped and owed the launch the retry made", got)
		}
	})
}
