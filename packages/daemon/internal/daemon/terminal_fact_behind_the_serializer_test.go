package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A claim's failure that queues on the fact serializer for longer than the boot's budget does not
// stop the daemon: the fact waits its turn and is applied once the serializer is free, as every
// fact of the intake does (intake.HandlerLimitFor gives a message at least ten minutes for the
// same wait). The serializer is a queue behind every fact of the project, so a wait of tens of
// seconds is load and not a fault, and INFRA-3169 measured `pg_advisory_xact_lock` as the
// costliest statement of its window. A live terminal fact the daemon gives up on is sent to
// w.failed, which ends the workflow and with it the daemon: a slow fact would then cost a
// restart, and a restart is what started INFRA-3169.
//
// The test holds the serializer for a little longer than bootTimeout, the bound terminal applies
// its fact under at this change's head, then frees it.
func TestALiveTerminalFactQueuedPastTheBootBudgetDoesNotStopTheWorkflow(t *testing.T) {
	pool := isolatedOutboxPool(t)
	handler := &outboxFactHandler{}
	w := &workflowRuntime{pool: pool, handlers: []intake.Handler{handler}, log: quietLogger(), failed: make(chan error, 1), limits: replayLimits}
	token, err := claim.NewToken("legion", "LEGION-1001", claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	failed := supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-1001", Issue: "LEGION-1001", Role: claim.RoleTester, Generation: 1,
		State: supervise.StateFailed, Failure: "pane exited", FailureCause: supervise.CauseNeverRegistered, Budgets: supervise.Budgets{LaunchFailures: 3}}

	live, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Rollback(context.Background()) }()
	if err := intake.SerializeFacts(context.Background(), live); err != nil {
		t.Fatalf("take the fact lock as a long live fact does: %v", err)
	}

	applied := make(chan struct{})
	go func() {
		defer close(applied)
		w.terminal(failed, supervise.StateFailed)
	}()
	time.Sleep(bootTimeout + 5*time.Second)
	if err := live.Rollback(context.Background()); err != nil {
		t.Fatalf("free the fact lock: %v", err)
	}

	held, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, 30*time.Second, time.Second))
	defer cancel()
	select {
	case <-applied:
	case <-held.Done():
		t.Fatalf("the terminal fact had not returned %s after the serializer was freed", 30*time.Second)
	}
	select {
	case err := <-w.failed:
		t.Fatalf("a failure fact that waited %s on the serializer stopped the workflow: %v", bootTimeout+5*time.Second, err)
	default:
	}
	want := intake.ClaimFailed{Issue: failed.Issue, Role: failed.Role, Reason: failed.Failure, Cause: failed.FailureCause, Budgets: failed.Budgets, Limits: replayLimits}
	if facts := handler.facts(); len(facts) != 1 || facts[0] != want {
		t.Fatalf("facts applied = %#v, want the one failure once the serializer was free", facts)
	}
}
