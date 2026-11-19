package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The engine reads a step's agent inside a fact's transaction, which holds the fact lock
// (intake.SerializeFacts), while the supervisor's Create holds its own lock across the store's
// AdmitClaim, which waits on that same fact lock. PostgreSQL cannot see the Go lock, so a read of
// the agent that waited on it would hold the fact's transaction until the store's
// idle-in-transaction timeout ended it, and every caller of the supervisor's lock would wait with
// it. The read takes no supervisor lock: with a fact's transaction holding the fact lock and a
// Create waiting for it inside the supervisor's lock, the read answers at once.
func TestTheReadOfAStepsAgentWaitsOnNoCreateThatWaitsOnTheFactLock(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st := outboxTreeStore(t, pool, "LEGION-208")
	putOutboxIssue(t, pool, record.NewStore(), record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Planning, Generation: 1,
		Status: "in_progress", Rank: "U"})
	sup := newSupervisor(ctx, st, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: fake.NewRuntime(), Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	t.Cleanup(sup.stop)
	w := &workflowRuntime{log: quietLogger()}
	w.attach(&supervision{supervisor: sup, log: quietLogger()})
	planner := mustClaimToken(t, "LEGION-208", claim.RolePlanner)

	fact, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fact.Rollback(ctx)
	if err := intake.SerializeFacts(ctx, fact); err != nil {
		t.Fatalf("take the fact lock: %v", err)
	}
	created := make(chan error, 1)
	go func() {
		_, _, err := sup.Create(ctx, supervise.Claim{Token: planner, Project: "legion", Tree: "LEGION-208", TreeEpoch: 1,
			Issue: "LEGION-208", Role: claim.RolePlanner, Generation: 1, State: supervise.StateSuspended,
			SuspendedBy: supervise.SuspendedByDaemon, Session: "ses-planner"}, "")
		created <- err
	}()
	// Create holds the supervisor's lock from its start to its end, so once a backend of this
	// database waits on an advisory lock, Create is inside it, waiting on the fact lock.
	if !testwait.Holds(10*time.Second, 10*time.Millisecond, func() bool {
		var waiting int
		err := pool.QueryRow(ctx, `select count(*) from pg_locks
			where locktype = 'advisory' and not granted
			  and database = (select oid from pg_database where datname = current_database())`).Scan(&waiting)
		return err == nil && waiting > 0
	}) {
		t.Fatal("Create never waited on the fact lock, so the read below would prove nothing")
	}

	read := make(chan struct{})
	go func() {
		w.stepAgent(planner)
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(3 * time.Second):
		t.Fatal("the read of a step's agent did not answer within 3s while Create waited on the fact lock inside the supervisor's lock")
	}

	if err := fact.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-created:
		if err != nil {
			t.Fatalf("Create once the fact lock was free: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Create did not finish once the fact lock was free")
	}
	if got := w.stepAgent(planner); got != (workflow.StepAgent{Session: "ses-planner"}) {
		t.Fatalf("the agent read once Create finished = %+v, want the claim's session", got)
	}
}
