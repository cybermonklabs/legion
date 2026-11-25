package intake

// ApplyFact measured through a proxy that holds or cuts its commit's reply on the way to a real
// PostgreSQL (testdb.CommitFaults): a fact whose commit landed is reported applied and runs its
// commit hooks, whatever became of the reply; a fact whose commit never landed runs none, and its
// retry applies; and a fact whose outcome the store cannot say yet runs its hooks once it can.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// factCommitMatch is the SQL every fact's transaction sends (SerializeFacts), which each fault is
// armed on.
const factCommitMatch = "legion.apply_fact"

// factFaultRig is one cloned database reached two ways: direct, which observes what committed,
// and faulted, through a proxy whose armed fault holds or cuts one transaction's commit. Both pools
// close before the proxy and the clone at the test's end.
type factFaultRig struct {
	direct  *pgxpool.Pool
	faulted *pgxpool.Pool
	fault   *testdb.CommitFault
}

func newFactFaultRig(t *testing.T) *factFaultRig {
	t.Helper()
	dsn := testpg.Clone(t, "legion_intake_test")
	proxied, fault := testdb.CommitFaults(t, dsn)
	rig := &factFaultRig{direct: openFactPool(t, dsn), faulted: openFactPool(t, proxied), fault: fault}
	createWrites(t, rig.direct)
	return rig
}

func openFactPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open a pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// hooked is a fact's one handler: one write, and a hook that counts in ran.
func hooked(ran *atomic.Int32) Handler {
	return handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		if _, err := writeHandler("first", nil).Apply(ctx, tx, fact); err != nil {
			return Result{}, err
		}
		store.OnCommit(ctx, func() { ran.Add(1) })
		return Result{}, nil
	})
}

// applied is what an ApplyFact run on its own goroutine returned.
type applied struct {
	result Result
	err    error
}

// apply applies one hooked fact with eventID on the faulted pool, on a goroutine of its own, so the
// test can meet its fault meanwhile.
func (r *factFaultRig) apply(ctx context.Context, eventID string, ran *atomic.Int32) <-chan applied {
	done := make(chan applied, 1)
	go func() {
		result, err := ApplyFact(ctx, r.faulted, "dispatch", eventID, DispatchIssue{Key: "LEGION-208"}, hooked(ran))
		done <- applied{result: result, err: err}
	}()
	return done
}

// waitApplied waits for the run apply started, failing the test rather than hanging it.
func waitApplied(t *testing.T, done <-chan applied) applied {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("ApplyFact did not return")
		return applied{}
	}
}

// cancelWhileCommitting cancels a fact still waiting for its commit's reply, and returns what
// ApplyFact returned. A fact that returned before the cancel fails the test: its reply came first,
// so the cancel tested nothing.
func cancelWhileCommitting(t *testing.T, done <-chan applied, cancel context.CancelFunc) applied {
	t.Helper()
	select {
	case got := <-done:
		t.Fatalf("ApplyFact returned %+v before the cancel: its commit's reply came first, so the cancel tested nothing", got)
	default:
	}
	cancel()
	return waitApplied(t, done)
}

// lostReplyAfterTheCancel applies a fact whose commit's reply the proxy holds for hold, cancels its
// context once its write is visible, and checks that the fact is reported applied with its hook
// run, and that its retry is a duplicate that runs no hook.
func lostReplyAfterTheCancel(t *testing.T, eventID string, hold time.Duration) {
	rig := newFactFaultRig(t)
	var ran atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := rig.fault.HoldReply(factCommitMatch, hold)
	done := rig.apply(ctx, eventID, &ran)
	testdb.Met(t, fired)
	testwait.Eventually(t, "the fact's write to be visible to the observer", func() bool { return writeCount(t, rig.direct) == 1 })
	got := cancelWhileCommitting(t, done, cancel)
	if got.err != nil {
		t.Errorf("the fact whose commit landed = %v, want nil", got.err)
	}
	if n := ran.Load(); n != 1 {
		t.Errorf("the fact's hook ran %d times, want once: its commit landed", n)
	}

	retry, err := ApplyFact(context.Background(), rig.faulted, "dispatch", eventID, DispatchIssue{Key: "LEGION-208"}, hooked(&ran))
	if err != nil {
		t.Fatalf("the retry = %v, want nil", err)
	}
	if !retry.Duplicate {
		t.Errorf("the retry = %+v, want a duplicate: the fact committed", retry)
	}
	if n := ran.Load(); n != 1 {
		t.Errorf("the hook ran %d times after the retry, want still once", n)
	}
	if n := writeCount(t, rig.direct); n != 1 {
		t.Errorf("%d writes committed, want the fact's one", n)
	}
}

func TestAFactWhoseCommitReplyComesAfterTheCancelIsAppliedAndRunsItsHooks(t *testing.T) {
	// Not parallel: its cancel has to land inside the commit reply's 300 ms hold, a margin the load of tests beside it can eat.
	lostReplyAfterTheCancel(t, "event-reply-after-the-cancel", 300*time.Millisecond)
}

func TestAFactWhoseCommitReplyNeverComesIsReadAsApplied(t *testing.T) {
	t.Parallel()
	lostReplyAfterTheCancel(t, "event-reply-never-comes", time.Hour)
}

func TestAFactCutBeforeItsCommitRunsNoHookAndItsRetryApplies(t *testing.T) {
	t.Parallel()
	rig := newFactFaultRig(t)
	var ran atomic.Int32

	fired := rig.fault.Cut(factCommitMatch, testdb.CutBeforeCommit)
	done := rig.apply(context.Background(), "event-cut-before-commit", &ran)
	testdb.Met(t, fired)
	got := waitApplied(t, done)
	if got.err == nil {
		t.Errorf("the fact whose commit never reached the server = %+v, want an error", got.result)
	}
	if n := ran.Load(); n != 0 {
		t.Errorf("the fact's hook ran %d times, want none: nothing committed", n)
	}
	if n := writeCount(t, rig.direct); n != 0 {
		t.Errorf("%d writes committed, want none", n)
	}

	retry, err := ApplyFact(context.Background(), rig.faulted, "dispatch", "event-cut-before-commit", DispatchIssue{Key: "LEGION-208"}, hooked(&ran))
	if err != nil {
		t.Fatalf("the retry = %v, want nil", err)
	}
	if retry.Duplicate {
		t.Errorf("the retry = %+v, want it applied: the first never committed", retry)
	}
	if n := ran.Load(); n != 1 {
		t.Errorf("the hook ran %d times after the retry, want once", n)
	}
	if n := writeCount(t, rig.direct); n != 1 {
		t.Errorf("%d writes committed after the retry, want one", n)
	}
}

func TestAFactWhoseOutcomeTheStoreCannotSayRunsItsHooksOnceItCan(t *testing.T) {
	t.Parallel()
	rig := newFactFaultRig(t)
	var ran atomic.Int32

	fired := rig.fault.Cut(factCommitMatch, testdb.CutAfterCommitRefusing)
	done := rig.apply(context.Background(), "event-outcome-unknown", &ran)
	testdb.Met(t, fired)
	got := waitApplied(t, done)
	if !errors.Is(got.err, store.ErrCommitOutcomeUnknown) {
		t.Fatalf("the fact whose commit landed while the store refused = %v, want store.ErrCommitOutcomeUnknown", got.err)
	}
	if n := ran.Load(); n != 0 {
		t.Fatalf("the fact's hook ran %d times before the store could say it committed, want none", n)
	}
	if _, err := ApplyFact(context.Background(), rig.faulted, "dispatch", "event-outcome-unknown", DispatchIssue{Key: "LEGION-208"}, hooked(&ran)); err == nil {
		t.Fatal("the retry while the store refuses = nil, want an error")
	}

	rig.fault.Refuse(false)
	testwait.Eventually(t, "the fact's hook to run once the store says it committed", func() bool { return ran.Load() == 1 })
	retry, err := ApplyFact(context.Background(), rig.faulted, "dispatch", "event-outcome-unknown", DispatchIssue{Key: "LEGION-208"}, hooked(&ran))
	if err != nil {
		t.Fatalf("the retry once the store answers = %v, want nil", err)
	}
	if !retry.Duplicate {
		t.Errorf("the retry = %+v, want a duplicate: the fact committed", retry)
	}
	if n := ran.Load(); n != 1 {
		t.Errorf("the hook ran %d times after the retry, want still once", n)
	}
	if n := writeCount(t, rig.direct); n != 1 {
		t.Errorf("%d writes committed, want the fact's one", n)
	}
}
