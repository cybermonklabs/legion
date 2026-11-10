package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// sweepStall is a fake runtime whose periodic orphan sweep (grace orphanGrace) answers to no
// context, as a Sandbox sweep whose cluster API takes the request and never answers it does. The
// boot's own reconciliation (grace zero) passes through, so the daemon still starts. The sweep
// runs in one of the goroutines supervision.stop waits for (supervision.start), and that wait is
// the one this test needs to outlast the stop's budget; the stall ends when the test does, so the
// daemon this test leaves behind closes what it holds before the next test takes the shared NATS
// server.
//
// The stop's other unbounded waits — a launch or a suspension in flight, which inflight_test's
// stallingRuntime holds — are no use here: under a Stage 3 configuration a worker's launch first
// provisions the issue's workspace, so it never reaches the runtime on a machine that cannot clone
// the mapped repository.
type sweepStall struct {
	*fake.Runtime
	sweeping chan struct{}
	release  chan struct{}
}

func newSweepStall(t *testing.T) *sweepStall {
	t.Helper()
	r := &sweepStall{Runtime: fake.NewRuntime(), sweeping: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(r.release) })
	return r
}

func (r *sweepStall) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	if grace == 0 {
		return r.Runtime.ReconcileOrphans(ctx, known, grace)
	}
	select {
	case r.sweeping <- struct{}{}:
	default:
	}
	<-r.release
	return nil
}

// sweepStallTick is how often the daemon sweeps for orphans here: the first tick is what the test
// waits for, so it is short enough that the wait costs nothing and long enough not to sweep a
// loaded machine's store in a tight loop.
const sweepStallTick = 100 * time.Millisecond

// natsConnectionCloseBound is how long the server is given, once run has returned, to report the
// daemon's connection gone. Closing the connection is the last thing a stopping daemon does with
// it, and the server drops a client the moment its socket closes, so this bounds the report rather
// than any work of the daemon's.
const natsConnectionCloseBound = 5 * time.Second

// A daemon whose stop runs out of budget returns from run with its Envoy NATS connection still
// open, and the process that outlives run keeps it open: the only close of that connection is
// workflowRuntime.stop (workflow.go), which the serving goroutine reaches only after
// supervision.stop's wait for the work in flight, and that wait is unbounded, while run returns
// through awaitStop's deadline arm as soon as the budget passes (daemon.go). Nothing else closes
// it, because the daemon dials under natsauth.ReconnectForever (internal/natsauth/natsauth.go),
// which never gives a connection up by itself.
//
// The daemon's tests share one NATS server, and its fixture resets the server only once every
// client connection to it has gone (internal/testnats, URL's drain), so the connection this stop
// leaves open is charged to whichever test runs next — a test that had no part in it (INFRA-1964).
func TestRunReturnsAtItsStopBudgetWithTheWorkflowsNATSConnectionOpen(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	rt := newSweepStall(t)
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = repositoriesGitHub(t)
	o.orphanSweep = sweepStallTick
	o.stopBudget = time.Second
	// Every connection the server holds before the daemon dials is the fixture's own, and those
	// outlast the stop: what this test reads afterwards is what the daemon added.
	before := testnats.Connections(t)

	cancel, done, logs := runLogging(t, cfg, o)
	select {
	case <-rt.sweeping:
	case err := <-done:
		t.Fatalf("run returned %v before its orphan sweep reached the runtime; log:\n%s", err, logs)
	case <-time.After(30 * time.Second):
		t.Fatalf("the daemon's orphan sweep never reached the runtime; log:\n%s", logs)
	}

	took, stopLog := stopRun(t, cancel, done, logs, stopBound(o.stopBudget))

	t.Logf("stopped in %s with its orphan sweep waiting in the runtime", took)
	if logLine(stopLog, stoppedWaiting) == "" {
		t.Fatalf("the stop did not reach its budget, so this test proved nothing about the connection run leaves open; it logged:\n%s", stopLog)
	}
	var left []string
	closed := func() bool {
		left = nil
		for _, connection := range testnats.Connections(t) {
			if !slices.Contains(before, connection) {
				left = append(left, connection)
			}
		}
		return len(left) == 0
	}
	if !testwait.Holds(natsConnectionCloseBound, testwait.Poll, closed) {
		t.Fatalf("the daemon returned from run with its workflow connection open: %s later the NATS server still holds %d client connection(s) %v the daemon opened",
			natsConnectionCloseBound, len(left), left)
	}
}
