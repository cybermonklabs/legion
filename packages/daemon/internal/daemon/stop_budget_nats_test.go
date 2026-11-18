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
// daemon this test leaves behind closes what it holds before its NATS server's leak check runs
// (internal/testnats, leakcheck.go).
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

// run closes the workflow's Envoy NATS connection when its stop runs out of budget, so the
// server holds nothing of the daemon's once run has returned.
//
// The close cannot be left to workflowRuntime.stop (workflow.go): the serving goroutine reaches
// that stop only after supervision.stop's wait for the work in flight, and that wait is
// unbounded, while run returns through awaitStop's deadline arm as soon as the budget passes
// (daemon.go). Nothing else closes the connection either, because the daemon dials under
// natsauth.ReconnectForever (internal/natsauth/natsauth.go), which never gives a connection up
// by itself, so one run left open stays open for as long as the process does.
//
// The test's NATS server fails the test at its end for a client still open there (internal/testnats,
// leakcheck.go), but it names the client rather than the stop that left it open; this test's own
// wait names the stop (INFRA-1964). The server drops a client the moment its socket closes, so the
// wait is for the server's report of the close, which comes as soon as the daemon has closed it.
func TestRunClosesTheWorkflowsNATSConnectionWhenItsStopBudgetRunsOut(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	rt := newSweepStall(t)
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = repositoriesGitHub(t)
	passes := triggering(&o)
	o.stopBudget = time.Second
	// Every connection the server holds before the daemon dials is the fixture's own, and those
	// outlast the stop: what this test reads afterwards is what the daemon added.
	before := testnats.Connections(t)

	cancel, done, logs := runLogging(t, cfg, o)
	// The sweep this test stops the daemon inside is one it asks for: the pass never ends, so
	// nothing waits for its report, and the runtime's own stall below is what says it began.
	passes.ask(passOrphans)
	testwait.Eventually(t, "the daemon's orphan sweep to reach the runtime", func() bool {
		select {
		case <-rt.sweeping:
			return true
		case err := <-done:
			t.Fatalf("run returned %v before its orphan sweep reached the runtime; log:\n%s", err, logs)
		default:
		}
		return false
	})

	took, stopLog := stopRun(t, cancel, done, logs)

	t.Logf("stopped in %s with its orphan sweep waiting in the runtime", took)
	if logLine(stopLog, stoppedWaiting) == "" {
		t.Fatalf("the stop did not reach its budget, so this test proved nothing about the connection run leaves open; it logged:\n%s", stopLog)
	}
	testwait.Until(t, "the daemon's workflow NATS connection closed once run returned from a stop that ran out of budget (the reading is the client connections the daemon opened that the server still holds)", func() ([]testnats.Connection, bool) {
		var left []testnats.Connection
		for _, connection := range testnats.Connections(t) {
			if !slices.ContainsFunc(before, func(c testnats.Connection) bool { return c.CID == connection.CID }) {
				left = append(left, connection)
			}
		}
		return left, len(left) == 0
	})
}
