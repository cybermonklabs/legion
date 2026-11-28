package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// heldCatchUpBoot is a workflow daemon's configuration over a store that holds one live tree of
// its project, its root running in implementing (seedLiveTreeOn), and the catch-up of that root a
// previous daemon left held in the wake pacing (pacedCatchUp at record.CatchUpHeldAt): the row the
// boot's restore of held catch-ups reads and joins to the pacing again (catchUpPacing.restore). It
// answers the configuration, the test's own store over the daemon's database and the row's id.
// The daemon's Envoy listener takes every publish (newNoticeListener), so a state event the outbox
// sends is no ERROR line of its own.
func heldCatchUpBoot(t *testing.T) (config.Config, *store.Store, int64) {
	t.Helper()
	cfg := workflowConfigOn(t, workflowNATS(t), liveTreeBoard)
	listener := httptest.NewServer(newNoticeListener())
	t.Cleanup(listener.Close)
	cfg.EnvoyURL = listener.URL
	root := seedLiveTreeOn(t, cfg, "acme/widgets")
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	id := pacedCatchUp(t, st.Pool(), record.NewStore(), root, 1, time.Now().UTC().Truncate(time.Microsecond), record.CatchUpHeldAt)
	return cfg, st, id
}

// heldCatchUpOverrides are the overrides of a daemon heldCatchUpBoot configured, over rt.
func heldCatchUpOverrides(t *testing.T, rt runtime.Runtime) overrides {
	t.Helper()
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = repositoriesGitHub(t)
	return o
}

// timedBootReconcile is bootReconcileStall telling, of its boot orphan reconciliation (grace
// zero), when the call began and returned, the deadline of the context it was given and why that
// context ended. The tests read these against each other and against the boot's record of its
// start, never how long the call took, so the seconds a loaded machine spends on the boot's steps
// before the call, or in waking the call once its deadline has passed, move nothing they read.
type timedBootReconcile struct {
	*bootReconcileStall
	ended chan bootReconcileEnded
}

// bootReconcileEnded is how one boot orphan reconciliation of a timedBootReconcile ended.
type bootReconcileEnded struct {
	began, returned, deadline time.Time
	err                       error
}

func newTimedBootReconcile(t *testing.T) *timedBootReconcile {
	t.Helper()
	return &timedBootReconcile{
		bootReconcileStall: &bootReconcileStall{stallingRuntime: newStallingRuntime(t, fake.NewRuntime(), false), reconciling: make(chan struct{}, 1)},
		ended:              make(chan bootReconcileEnded, 1),
	}
}

func (r *timedBootReconcile) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	began := time.Now()
	deadline, _ := ctx.Deadline()
	err := r.bootReconcileStall.ReconcileOrphans(ctx, known, grace)
	if grace == 0 {
		select {
		case r.ended <- bootReconcileEnded{began: began, returned: time.Now(), deadline: deadline, err: err}:
		default:
		}
	}
	return err
}

// A boot whose orphan reconciliation waits until its context ends, as INFRA's did on 2026-11-27
// while the Sandbox runtime's repository-cache sweep replaced every drifted cache pod one at a time,
// spends the whole of the shared budget supervision.start runs its steps on (bootBudget), and the
// reconciliation ends without failing the boot. Its own bound is set to bootTimeout here
// (overrides.bootOrphanReconcile), no shorter than that budget, so the budget has run out before
// settle, and the boot starts only because the restore of the catch-ups the previous daemon left
// held runs on a budget of its own (catchUpPacing.restore): the row the previous daemon held is
// joined to the pacing again — here, with no pacer, made due at once (join) — and the daemon
// starts. Before, the restore ran on what the shared budget had left, and the boot exited with
// `read held catch-ups: context deadline exceeded` (dispatch://INFRA-4131/spec#b-r-boot-budget).
//
// The shared budget begins as the boot is recorded (bootBudget, just before store.RecordBoot), so
// the test reads when it ran out off the boot's recorded start, not off how long the reconciliation
// took: on a loaded machine the steps between the budget's start and the reconciliation take
// seconds of it.
func TestBootBudgetRestoresHeldCatchUpsAfterAnOrphanReconcileSpendsTheSharedBudget(t *testing.T) {
	t.Parallel()
	cfg, st, held := heldCatchUpBoot(t)
	rt := newTimedBootReconcile(t)
	o := heldCatchUpOverrides(t, rt)
	o.bootOrphanReconcile = bootTimeout
	startDaemon(t, cfg, o)

	ended := testwait.Receive(t, "the boot's orphan reconciliation to end", rt.ended)
	var recorded time.Time
	if err := st.Pool().QueryRow(context.Background(), "select started_at from daemon_boot where project = $1 order by id desc limit 1",
		cfg.Project).Scan(&recorded); err != nil {
		t.Fatalf("read the start of the boot the daemon recorded: %v", err)
	}
	// The budget ran out bootTimeout after the boot's recorded start, to within the moment between
	// the budget's start and the clock read the record keeps: a reconciliation its context cut no
	// earlier than that spent all of it.
	spent := recorded.Add(bootTimeout)
	if !errors.Is(ended.err, context.DeadlineExceeded) || ended.returned.Before(spent.Add(-time.Second)) {
		t.Fatalf("the boot's orphan reconciliation returned at %s with %v, want it to wait out the shared budget of %s, which ran out at %s",
			ended.returned.UTC().Format(time.RFC3339Nano), ended.err, bootTimeout, spent.UTC().Format(time.RFC3339Nano))
	}
	var nextAt time.Time
	if err := st.Pool().QueryRow(context.Background(), "select next_at from outbox where id = $1", held).Scan(&nextAt); err != nil {
		t.Fatalf("read the catch-up the previous daemon left held: %v", err)
	}
	if record.CatchUpIsHeld(nextAt) {
		t.Fatalf("the catch-up the previous daemon left held is still held at %s once the daemon started, want it restored to the pacing", nextAt)
	}
}

// The boot's orphan reconciliation ends at a bound of its own, inside the shared budget
// (bootOrphanReconcileBound): a reconciliation that waits until its context ends is cut there, well
// before the shared budget of bootTimeout ends, and the daemon starts. The reconciliation ends
// without failing the boot when its time runs out, so on the shared budget alone it could leave a
// later step an expired context (dispatch://INFRA-4131/spec#b-r-boot-budget). The test reads the
// deadline the call was given against the moment it began, never how long it waited, so a machine
// late to wake the call once that deadline passed moves nothing it reads.
func TestBootBudgetBoundsTheBootOrphanReconcile(t *testing.T) {
	t.Parallel()
	const bound = bootOrphanReconcileBound
	rt := newTimedBootReconcile(t)
	startDaemon(t, testConfig(t), fakeRuntime(rt, &built{}))

	ended := testwait.Receive(t, "the boot's orphan reconciliation to end", rt.ended)
	// The call is given its own bound, less the moment between the bound's start and the call, and
	// never more; the shared budget's deadline, had it no bound of its own, is nearly all of
	// bootTimeout off.
	given := ended.deadline.Sub(ended.began)
	if !errors.Is(ended.err, context.DeadlineExceeded) || given > bound || given < bound-3*time.Second {
		t.Fatalf("the boot's orphan reconciliation was given %s and ended with %v, want it cut at its own bound of %s, well inside the shared budget of %s",
			given, ended.err, bound, bootTimeout)
	}
}

// outboxLockedAtBootReconcile is the fake runtime whose boot orphan reconciliation (grace zero),
// the last step of supervision.start before settle, first has the test's own connection take the
// outbox table in access exclusive mode, so the restore of the held catch-ups that runs next
// (catchUpPacing.restore) waits on that lock in Postgres. It hands the test the transaction that
// holds the lock, once. The lock is taken on a context of the test's own (taking), never on the
// call's: the call's carries the boot's hold mark (store.WithTransactionTracking), and a
// transaction begun on it and left open would have the daemon's next connection on that mark
// refused (store.ErrNestedAcquire) rather than wait in Postgres. taking is the test's own
// (testwait.Context), with no bound of a fixed length: the lock waits behind every transaction of
// the daemon's that holds the outbox when it is asked for, for as long as a loaded machine makes
// those transactions take, up to the test binary's own deadline.
type outboxLockedAtBootReconcile struct {
	*fake.Runtime
	pool   *store.Pool
	taking context.Context
	once   sync.Once
	locked chan outboxLock
}

// outboxLock is the transaction that holds the outbox locked, or why it could not take the lock.
type outboxLock struct {
	tx  pgx.Tx
	err error
}

func (r *outboxLockedAtBootReconcile) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	if grace == 0 {
		r.once.Do(func() {
			tx, err := r.pool.BeginTx(r.taking, pgx.TxOptions{})
			if err == nil {
				if _, err = tx.Exec(r.taking, "lock table outbox in access exclusive mode"); err != nil {
					_ = tx.Rollback(context.Background())
				}
			}
			r.locked <- outboxLock{tx: tx, err: err}
		})
	}
	return r.Runtime.ReconcileOrphans(ctx, known, grace)
}

// A boot whose read of the catch-ups the previous daemon left held fails still fails: the restore
// has a budget of its own now, but its failure is the boot's, as before. Postgres cancels the read
// (record.Store.WaitingNotices) while it waits on a lock the test holds on the outbox, and the
// daemon exits with the read's error, never saying it started
// (dispatch://INFRA-4131/spec#b-r-boot-budget).
func TestBootBudgetAFailingReadOfHeldCatchUpsStillFailsTheBoot(t *testing.T) {
	t.Parallel()
	cfg, st, _ := heldCatchUpBoot(t)
	rt := &outboxLockedAtBootReconcile{Runtime: fake.NewRuntime(), pool: st.Pool(), taking: testwait.Context(t), locked: make(chan outboxLock, 1)}
	_, done, logs := runLogging(t, cfg, heldCatchUpOverrides(t, rt))

	var lock outboxLock
	select {
	case lock = <-rt.locked:
	case err := <-done:
		t.Fatalf("run returned %v before its boot reconciled orphans; log:\n%s", err, logs)
	case <-testwait.Context(t).Done():
		t.Fatalf("the boot never reconciled orphans; log:\n%s", logs)
	}
	if lock.err != nil {
		t.Fatalf("lock the outbox against the boot's read of the held catch-ups: %v", lock.err)
	}
	unlock := func() { _ = lock.tx.Rollback(context.Background()) }
	t.Cleanup(unlock)
	pid := testwait.Until(t, "the boot's read of the held catch-ups to wait on the outbox's lock", func() (int, bool) {
		var pid int
		err := st.Pool().QueryRow(context.Background(), `select pid from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like '%next_at > $2 and split_part(issue%'`).Scan(&pid)
		return pid, err == nil
	})
	var cancelled bool
	if err := st.Pool().QueryRow(context.Background(), "select pg_cancel_backend($1)", pid).Scan(&cancelled); err != nil || !cancelled {
		t.Fatalf("cancel the boot's read of the held catch-ups (backend %d): %t, %v", pid, cancelled, err)
	}
	unlock()

	err := testwait.Receive(t, "the daemon to exit", done)
	if err == nil || !strings.Contains(err.Error(), "list waiting notices: ") || !strings.Contains(err.Error(), "canceling statement due to user request") {
		t.Fatalf("run = %v, want the boot failed by the cancelled read of the held catch-ups; log:\n%s", err, logs)
	}
	if line := logLine(logs.String(), "legion daemon started"); line != "" {
		t.Fatalf("a daemon whose read of the held catch-ups failed said it started: %s", line)
	}
}

// cacheSweepingRuntime is the fake runtime with a repository cache pass of its own
// (runtime.RepositoryCacheSweeper), as the Sandbox runtime has: it notes, in the order they came,
// each orphan reconciliation with its grace and each cache pass. A pass waits until its context
// ends while stalls is set, as a pass that replaces many drifted cache pods one after another does.
type cacheSweepingRuntime struct {
	*fake.Runtime
	stalls bool
	mu     sync.Mutex
	calls  []string
}

func (r *cacheSweepingRuntime) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	r.note(fmt.Sprintf("reconcile orphans with grace %s", grace))
	return r.Runtime.ReconcileOrphans(ctx, known, grace)
}

func (r *cacheSweepingRuntime) SweepRepositoryCaches(ctx context.Context) error {
	r.note(cachePass)
	if r.stalls {
		<-ctx.Done()
	}
	return ctx.Err()
}

func (r *cacheSweepingRuntime) note(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *cacheSweepingRuntime) noted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// cachePass is the call cacheSweepingRuntime notes for a cache pass.
const cachePass = "repository cache pass"

// The repository caches are passed over beside the claims, never on the boot: the orphan sweep
// runs one pass as it begins, after the boot's reconciliation, and one after each reconciliation
// of its own, and a pass that its bound, one sweep interval, cuts short is logged at ERROR, since
// nothing else would say the caches it did not reach wait for the next pass
// (dispatch://INFRA-4131/spec#b-r-boot-budget).
func TestTheOrphanSweepPassesOverTheRepositoryCachesBesideTheClaims(t *testing.T) {
	t.Parallel()
	boot := fmt.Sprintf("reconcile orphans with grace %s", time.Duration(0))
	sweep := fmt.Sprintf("reconcile orphans with grace %s", orphanGrace)
	t.Run("as the sweep begins and after each of its reconciliations", func(t *testing.T) {
		t.Parallel()
		rt := &cacheSweepingRuntime{Runtime: fake.NewRuntime()}
		o := fakeRuntime(rt, &built{})
		o.orphanSweep = 20 * time.Millisecond
		startDaemon(t, testConfig(t), o)
		calls := testwait.Until(t, "three cache passes", func() ([]string, bool) {
			calls := rt.noted()
			return calls, slices.Index(calls, sweep) >= 0 && strings.Count(strings.Join(calls, "\n"), cachePass) >= 3
		})
		if len(calls) < 2 || calls[0] != boot || calls[1] != cachePass {
			t.Fatalf("the runtime was called %q, want the boot's reconciliation and then the sweep's first cache pass, before any reconciliation of the sweep's own", calls)
		}
		for i := 2; i < len(calls); i++ {
			if calls[i] == cachePass && calls[i-1] != sweep {
				t.Fatalf("call %d of %q is a cache pass after %q, want every pass after the first right after a reconciliation of the sweep's", i, calls, calls[i-1])
			}
		}
	})
	t.Run("a pass its bound cuts short is logged at ERROR", func(t *testing.T) {
		t.Parallel()
		rt := &cacheSweepingRuntime{Runtime: fake.NewRuntime(), stalls: true}
		o := fakeRuntime(rt, &built{})
		o.orphanSweep = 200 * time.Millisecond
		logs := &syncBuffer{}
		startDaemonLogging(t, testConfig(t), o, slog.New(slog.NewJSONHandler(logs, nil)))
		const cutShort = "reconcile orphans: the repository cache pass ran out of its bound; the caches it did not reach wait for the next pass"
		line := testwait.Until(t, "the cut-short pass logged", func() (string, bool) {
			line := logLine(logs.String(), cutShort)
			return line, line != ""
		})
		if !strings.Contains(line, `"level":"ERROR"`) || !strings.Contains(line, `"bound":200000000`) || !strings.Contains(line, context.DeadlineExceeded.Error()) {
			t.Fatalf("the cut-short pass was logged as %s, want an ERROR line naming its bound of 200ms and the deadline", line)
		}
	})
}
