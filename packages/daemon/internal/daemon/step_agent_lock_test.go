package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
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
		w.stepAgent(ctx, fact, planner)
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
	// The claim holds its session as a registration leaves it; this machine has not seen that
	// answer, so the read asks claim_sessions in the fact's own transaction.
	if _, err := st.HoldSessions(ctx, "legion", planner, supervise.RecordedHolds("ses-planner", "")); err != nil {
		t.Fatalf("hold ses-planner for %s: %v", planner, err)
	}
	next, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Rollback(ctx)
	if got := w.stepAgent(ctx, next, planner); got != (workflow.StepAgent{Session: "ses-planner"}) {
		t.Fatalf("the agent read once Create finished = %+v, want the claim's session", got)
	}
}

// stepAgentRig is a supervisor on the real store of pool, with LEGION-208's root recorded and the
// store it reads counting each HoldSessions call, and the workflow runtime that reads a step's
// agent through it.
func stepAgentRig(t *testing.T) (*store.Pool, *store.Store, *supervisor, *workflowRuntime, *countingHolds) {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st := outboxTreeStore(t, pool, "LEGION-208")
	putOutboxIssue(t, pool, record.NewStore(), record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Planning, Generation: 1,
		Status: "in_progress", Rank: "U"})
	counted := &countingHolds{Store: st}
	sup := newSupervisor(ctx, st, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: fake.NewRuntime(), Conns: fake.NewConns(), Store: counted, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	t.Cleanup(sup.stop)
	w := &workflowRuntime{log: quietLogger()}
	w.attach(&supervision{supervisor: sup, log: quietLogger()})
	return pool, st, sup, w, counted
}

// countingHolds is a supervise.Store that counts the HoldSessions calls made through it.
type countingHolds struct {
	supervise.Store
	holds atomic.Int64
}

func (s *countingHolds) HoldSessions(ctx context.Context, project string, token claim.Token, holds []supervise.Hold) ([]claim.Token, error) {
	s.holds.Add(1)
	return s.Store.HoldSessions(ctx, project, token, holds)
}

// A step's record never names a session its claim does not hold (stepAgent, plan E's t-e-54): a
// claim the boot could not suspend, recording a session another claim recorded first, has none
// named on its step, while a claim holding its own session is named. The read is made inside a
// fact's transaction holding the fact lock, as the engine makes it, on a machine that has not
// seen either claim's hold answered, as after a restart.
//
// The control (the pull request records the run): stepAgent with no hold check names ses_taken.
func TestAStepsAgentNamesNoSessionItsClaimDoesNotHold(t *testing.T) {
	ctx := context.Background()
	pool, st, sup, w, _ := stepAgentRig(t)
	holder := mustClaimToken(t, "LEGION-943", claim.RoleImplementer)
	if _, err := st.HoldSessions(ctx, "legion", holder, supervise.RecordedHolds("ses_taken", "")); err != nil {
		t.Fatalf("hold ses_taken for %s, the claim that recorded it first: %v", holder, err)
	}
	tester, reviewer := mustClaimToken(t, "LEGION-208", claim.RoleTester), mustClaimToken(t, "LEGION-208", claim.RoleReviewer)
	if _, err := st.HoldSessions(ctx, "legion", reviewer, supervise.RecordedHolds("ses_own", "")); err != nil {
		t.Fatalf("hold ses_own for %s: %v", reviewer, err)
	}
	for _, c := range []struct {
		token   claim.Token
		role    claim.Role
		session string
	}{{tester, claim.RoleTester, "ses_taken"}, {reviewer, claim.RoleReviewer, "ses_own"}} {
		if _, _, err := sup.Create(ctx, supervise.Claim{Token: c.token, Project: "legion", Tree: "LEGION-208", TreeEpoch: 1,
			Issue: "LEGION-208", Role: c.role, Generation: 1, State: supervise.StateSuspended,
			SuspendedBy: supervise.SuspendedByOperator, Session: c.session}, ""); err != nil {
			t.Fatalf("create %s recording %s: %v", c.token, c.session, err)
		}
	}
	fact, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fact.Rollback(ctx)
	if err := intake.SerializeFacts(ctx, fact); err != nil {
		t.Fatalf("take the fact lock: %v", err)
	}
	if got := w.stepAgent(ctx, fact, tester); got != (workflow.StepAgent{}) {
		t.Fatalf("the step agent of %s, recording ses_taken, which %s holds = %+v, want no session", tester, holder, got)
	}
	if got := w.stepAgent(ctx, fact, reviewer); got != (workflow.StepAgent{Session: "ses_own"}) {
		t.Fatalf("the step agent of %s, holding ses_own = %+v, want its session", reviewer, got)
	}
}

// The read of a step's agent writes nothing while the fact lock is held (stepAgent): no
// HoldSessions, whose write would go through the store's pool, a connection other than the one
// holding the fact lock, bounded at five seconds, after a restart on every fact that ends a step.
// It reads claim_sessions in the fact's own transaction instead, at once.
//
// The control (the pull request records the run): stepAgent asking Machine.HoldsItsSession, as
// 5da94fd7fc did: "HoldSessions was called 1 times while the fact lock was held, want none".
func TestTheReadOfAStepsAgentWritesNoHoldUnderTheFactLock(t *testing.T) {
	ctx := context.Background()
	pool, st, sup, w, counted := stepAgentRig(t)
	planner := mustClaimToken(t, "LEGION-208", claim.RolePlanner)
	if _, err := st.HoldSessions(ctx, "legion", planner, supervise.RecordedHolds("ses-planner", "")); err != nil {
		t.Fatalf("hold ses-planner for %s: %v", planner, err)
	}
	if _, _, err := sup.Create(ctx, supervise.Claim{Token: planner, Project: "legion", Tree: "LEGION-208", TreeEpoch: 1,
		Issue: "LEGION-208", Role: claim.RolePlanner, Generation: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByOperator, Session: "ses-planner"}, ""); err != nil {
		t.Fatalf("create the planner: %v", err)
	}
	fact, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fact.Rollback(ctx)
	if err := intake.SerializeFacts(ctx, fact); err != nil {
		t.Fatalf("take the fact lock: %v", err)
	}
	before := counted.holds.Load()
	start := time.Now()
	got := w.stepAgent(ctx, fact, planner)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the read of the step's agent took %s under the fact lock, want it answered from the fact's own transaction at once", took)
	}
	if n := counted.holds.Load() - before; n != 0 {
		t.Fatalf("HoldSessions was called %d times while the fact lock was held, want none", n)
	}
	if got != (workflow.StepAgent{Session: "ses-planner"}) {
		t.Fatalf("the step agent of the planner holding its session = %+v, want its session", got)
	}
}
