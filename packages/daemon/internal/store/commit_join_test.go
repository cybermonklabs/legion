package store

// A transaction opened inside an open one, a read-only transaction, the rollback after a caller's
// cancel and the outcome read's own transaction, measured against the PostgreSQL
// LEGION_TEST_PG_DSN names, through the fault proxy where a commit's reply is held or cut.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// joinedHooks records the order the hooks of a transaction ran in.
type joinedHooks struct {
	mu  sync.Mutex
	ran []string
}

func (h *joinedHooks) hook(name string) func() {
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.ran = append(h.ran, name)
	}
}

func (h *joinedHooks) order() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.ran)
}

// insertProbe inserts id into commit_probe and answers the transaction's id.
func insertProbe(ctx context.Context, tx pgx.Tx, id int) (string, error) {
	if _, err := tx.Exec(ctx, "insert into commit_probe values ($1)", id); err != nil {
		return "", err
	}
	var xid string
	err := tx.QueryRow(ctx, "select pg_current_xact_id()::text").Scan(&xid)
	return xid, err
}

// probeIDs is every id of commit_probe the observer sees committed.
func (r *commitRig) probeIDs(t *testing.T) []int {
	t.Helper()
	rows, err := r.observer.Query(context.Background(), "select id from commit_probe order by id")
	if err != nil {
		t.Fatalf("read commit_probe: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("read commit_probe: %v", err)
	}
	return ids
}

func TestATransactionOpenedInsideAnOpenOneJoinsIt(t *testing.T) {
	rig := newCommitRig(t)
	ctx := WithTransactionTracking(context.Background())
	var hooks joinedHooks
	var outer, inner string
	err := Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if outer, err = insertProbe(ctx, tx, 1); err != nil {
			return err
		}
		OnCommit(ctx, hooks.hook("A"))
		return Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if inner, err = insertProbe(ctx, tx, 2); err != nil {
				return err
			}
			OnCommit(ctx, hooks.hook("B"))
			return nil
		})
	})
	if err != nil {
		t.Fatalf("Transact with a transaction opened inside it = %v, want nil", err)
	}
	if ids := rig.probeIDs(t); !slices.Equal(ids, []int{1, 2}) {
		t.Fatalf("committed rows = %v, want [1 2]", ids)
	}
	if got := hooks.order(); !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("hooks ran %v, want [A B]", got)
	}
	if outer != inner {
		t.Fatalf("the outer body ran in transaction %s and the inner in %s, want one transaction", outer, inner)
	}
}

func TestAJoinedTransactionRollsBackWithItsOuter(t *testing.T) {
	rig := newCommitRig(t)
	ctx := WithTransactionTracking(context.Background())
	var hooks joinedHooks
	outerFails := errors.New("the outer body fails after the inner one")
	err := Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := insertProbe(ctx, tx, 1); err != nil {
			return err
		}
		OnCommit(ctx, hooks.hook("A"))
		if err := Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := insertProbe(ctx, tx, 2); err != nil {
				return err
			}
			OnCommit(ctx, hooks.hook("B"))
			return nil
		}); err != nil {
			t.Errorf("the inner Transact = %v, want nil: it joins the outer transaction", err)
			return err
		}
		return outerFails
	})
	if !errors.Is(err, outerFails) {
		t.Fatalf("Transact = %v, want the outer body's error", err)
	}
	if ids := rig.probeIDs(t); len(ids) != 0 {
		t.Fatalf("committed rows = %v, want none: the inner rows roll back with the outer", ids)
	}
	if got := hooks.order(); len(got) != 0 {
		t.Fatalf("hooks ran %v, want none", got)
	}
}

func TestATransactionOnAnotherPoolInsideAnOpenOneIsRefused(t *testing.T) {
	rig := newCommitRig(t)
	ctx := WithTransactionTracking(context.Background())
	other := NewPool(rig.observer)
	ran := false
	var inner error
	err := Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		inner = Transact(ctx, other, func(context.Context, pgx.Tx) error {
			ran = true
			return nil
		})
		return inner
	})
	if !errors.Is(inner, errTransactionOnAnotherPool) {
		t.Fatalf("the inner Transact on another pool = %v, want %v", inner, errTransactionOnAnotherPool)
	}
	if !errors.Is(err, errTransactionOnAnotherPool) {
		t.Fatalf("Transact = %v, want the inner refusal", err)
	}
	if ran {
		t.Fatal("the inner body ran, want it refused before it runs")
	}
}

func TestATransactionOnAClosedScopeIsItsOwn(t *testing.T) {
	rig := newCommitRig(t)
	ctx := WithTransactionTracking(context.Background())
	var kept context.Context
	var outer, later string
	if err := Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		kept = ctx
		var err error
		outer, err = insertProbe(ctx, tx, 1)
		return err
	}); err != nil {
		t.Fatalf("the outer Transact = %v", err)
	}
	if err := Transact(kept, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		later, err = insertProbe(ctx, tx, 3)
		return err
	}); err != nil {
		t.Fatalf("Transact on the closed scope's context = %v, want nil", err)
	}
	if ids := rig.probeIDs(t); !slices.Equal(ids, []int{1, 3}) {
		t.Fatalf("committed rows = %v, want [1 3]", ids)
	}
	if later == outer {
		t.Fatalf("the transaction on the closed scope ran in the outer's %s, want one of its own", outer)
	}
}

// A joined write that fails is undone on its own, whatever its outer body does with the error: the
// join runs under a savepoint that rolls back, and the hooks it registered are cut, so an outer
// body that swallows the error commits neither the inner write's row nor its hook.
func TestAJoinedWriteThatFailsKeepsNeitherItsWritesNorItsHooks(t *testing.T) {
	rig := newCommitRig(t)
	ctx := WithTransactionTracking(context.Background())
	var hooks joinedHooks
	innerFails := errors.New("the inner body fails after its write")
	err := Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := insertProbe(ctx, tx, 1); err != nil {
			return err
		}
		OnCommit(ctx, hooks.hook("A"))
		if err := Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := insertProbe(ctx, tx, 2); err != nil {
				return err
			}
			OnCommit(ctx, hooks.hook("B"))
			return innerFails
		}); !errors.Is(err, innerFails) {
			t.Errorf("the joined Transact = %v, want its body's error", err)
		}
		if _, err := insertProbe(ctx, tx, 3); err != nil {
			return err
		}
		OnCommit(ctx, hooks.hook("C"))
		return nil
	})
	if err != nil {
		t.Fatalf("Transact = %v, want nil: the outer body swallowed the inner error", err)
	}
	if ids := rig.probeIDs(t); !slices.Equal(ids, []int{1, 3}) {
		t.Fatalf("committed rows = %v, want [1 3]: the failed joined write rolls back alone", ids)
	}
	if got := hooks.order(); !slices.Equal(got, []string{"A", "C"}) {
		t.Fatalf("hooks ran %v, want [A C]: the failed joined write's hook is cut", got)
	}
}

// A join keeps its outer transaction's options, so a join that asks for others is refused before
// its body runs: a repeatable-read read joined into a read-committed transaction would lose the
// snapshot it asked for, and a read-only one would lose PostgreSQL's refusal of its writes.
func TestAJoinThatAsksForOtherOptionsIsRefused(t *testing.T) {
	rig := newCommitRig(t)
	for _, row := range []struct {
		name         string
		outer, inner pgx.TxOptions
	}{
		{"repeatable read inside read committed", pgx.TxOptions{}, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}},
		{"read only inside read write", pgx.TxOptions{}, pgx.TxOptions{AccessMode: pgx.ReadOnly}},
		{"read write inside read only", pgx.TxOptions{AccessMode: pgx.ReadOnly}, pgx.TxOptions{}},
	} {
		t.Run(row.name, func(t *testing.T) {
			ran := false
			err := TransactWith(WithTransactionTracking(context.Background()), rig.pool, row.outer, func(ctx context.Context, tx pgx.Tx) error {
				return TransactWith(ctx, rig.pool, row.inner, func(context.Context, pgx.Tx) error {
					ran = true
					return nil
				})
			})
			if !errors.Is(err, errJoinOfOtherOptions) {
				t.Fatalf("Transact = %v, want %v", err, errJoinOfOtherOptions)
			}
			if ran {
				t.Fatal("the inner body ran, want it refused before it runs")
			}
		})
	}
	if err := TransactWith(WithTransactionTracking(context.Background()), rig.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return TransactWith(ctx, rig.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(context.Context, pgx.Tx) error { return nil })
	}); err != nil {
		t.Fatalf("a join with the outer's own options = %v, want nil", err)
	}
}

// A goroutine a body starts runs beside the body, so its context (ForConcurrentWorker) holds no
// commit scope: a transaction it opens is its own, and it registers no hook on the body's list,
// which only the body's goroutine appends to.
func TestAWorkerStartedInsideABodyJoinsNothing(t *testing.T) {
	rig := newCommitRig(t)
	var scoped bool
	var worker context.Context
	if err := Transact(WithTransactionTracking(context.Background()), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		worker = ForConcurrentWorker(ctx)
		scoped = CommitScoped(worker)
		return nil
	}); err != nil {
		t.Fatalf("Transact: %v", err)
	}
	if scoped {
		t.Fatal("CommitScoped on a worker's context started inside a body = true, want false")
	}
	if recovered := onCommitPanic(worker); recovered == nil {
		t.Fatal("OnCommit on a worker's context did not panic, want the panic of a context no transaction scopes")
	}
}

func TestAJoinedHookWaitsForItsOuterOutcome(t *testing.T) {
	rig := newCommitRig(t)
	ctx := WithTransactionTracking(context.Background())
	fired := rig.fault.Cut(commitMatch, testdb.CutAfterCommitRefusing)
	err := Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := insertProbe(ctx, tx, 1); err != nil {
			return err
		}
		return Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			OnCommit(ctx, func() { rig.hooks.Add(1) })
			return nil
		})
	})
	testdb.Met(t, fired)
	if !errors.Is(err, ErrCommitOutcomeUnknown) {
		t.Fatalf("Transact = %v, want %v", err, ErrCommitOutcomeUnknown)
	}
	rig.wantHooks(t, 0)
	rig.fault.Refuse(false)
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the joined hook to run once the store says the outer committed", func() bool {
		return rig.hooks.Load() == 1
	})
	time.Sleep(2 * outcomeRetryFirst)
	rig.wantHooks(t, 1)
}

// idReads is a pool whose transactions count the reads of their own id.
type idReads struct {
	pool  TxPool
	reads atomic.Int32
}

func (p *idReads) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &idReadsTx{Tx: tx, reads: &p.reads}, nil
}

type idReadsTx struct {
	pgx.Tx
	reads *atomic.Int32
}

func (t *idReadsTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "pg_current_xact_id_if_assigned") {
		t.reads.Add(1)
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

func TestAReadOnlyTransactionReadsNoID(t *testing.T) {
	rig := newCommitRig(t)
	ctx := WithTransactionTracking(context.Background())
	counted := &idReads{pool: rig.pool}
	read := func(ctx context.Context, tx pgx.Tx) error {
		var one int
		return tx.QueryRow(ctx, "select 1").Scan(&one)
	}
	if err := TransactWith(ctx, counted, pgx.TxOptions{AccessMode: pgx.ReadOnly}, read); err != nil {
		t.Fatalf("the read-only transaction = %v", err)
	}
	if got := counted.reads.Load(); got != 0 {
		t.Fatalf("the read-only transaction read its id %d times, want 0", got)
	}
	if err := Transact(ctx, counted, read); err != nil {
		t.Fatalf("the default transaction = %v", err)
	}
	if got := counted.reads.Load(); got != 1 {
		t.Fatalf("the default transaction read its id %d times, want 1", got)
	}
}

// The database's half of the guard that refuses every statement outside the boundary: a read
// runs read-only, and PostgreSQL refuses each write there, however its SQL is spelled.
func TestAReadOnlyTransactionRefusesEveryWrite(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.Database(t, "legion_store_commit_test"))
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "create table probe (id int primary key); insert into probe values (1); create sequence probe_seq"); err != nil {
		t.Fatalf("create the probe table and sequence: %v", err)
	}
	for _, statement := range []string{
		`select "nextval"('probe_seq')`,
		`select nextval('probe_seq')`,
		`select setval('probe_seq', 40)`,
		`update probe set id = id`,
		`with moved as (update probe set id = id returning id) select count(*) from moved`,
		`insert into probe values (2)`,
		`select id from probe for update`,
		`select id from probe for share`,
	} {
		t.Run(statement, func(t *testing.T) {
			ran := false
			err := TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
				OnCommit(ctx, func() { ran = true })
				_, err := tx.Exec(ctx, statement)
				return err
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
				t.Fatalf("%s in a read-only transaction = %v, want SQLSTATE 25006", statement, err)
			}
			if ran {
				t.Fatal("the refused write's hook ran")
			}
		})
	}
	var last int64
	var called bool
	if err := pool.QueryRow(ctx, "select last_value, is_called from probe_seq").Scan(&last, &called); err != nil {
		t.Fatalf("read the sequence: %v", err)
	}
	if last != 1 || called {
		t.Fatalf("the sequence reads last_value %d, is_called %t; want 1, false", last, called)
	}
	var rows int
	if err := pool.QueryRow(ctx, "select count(*) from probe").Scan(&rows); err != nil {
		t.Fatalf("count the probe table: %v", err)
	}
	if rows != 1 {
		t.Fatalf("the probe table holds %d rows, want its 1", rows)
	}
}

func TestARollbackAfterTheCallersCancelKeepsItsConnection(t *testing.T) {
	dsn := testdb.Database(t, "legion_store_commit_test")
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the dsn: %v", err)
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	t.Cleanup(pool.Close)
	commits := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "select 1")
		return err
	}
	if err := Transact(context.Background(), pool, commits); err != nil {
		t.Fatalf("warm the pool: %v", err)
	}
	before := pool.Stat().NewConnsCount()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var pid uint32
	err = Transact(ctx, testdb.CancelAfterScan(pool, "probe", cancel), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select pg_backend_pid() /* probe */").Scan(&pid)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Transact after the caller's cancel = %v, want %v", err, context.Canceled)
	}
	if got := pool.Stat().NewConnsCount(); got != before {
		t.Fatalf("the pool opened %d connections, want the %d it had: the rollback lost the connection", got, before)
	}
	observer, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the observer: %v", err)
	}
	defer func() { _ = observer.Close(context.Background()) }()
	var state string
	if err := observer.QueryRow(context.Background(), "select state from pg_stat_activity where pid = $1", pid).Scan(&state); err != nil {
		t.Fatalf("read the transaction's connection: %v", err)
	}
	if state != "idle" {
		t.Fatalf("the connection after the rollback is %q, want idle", state)
	}
	if err := Transact(context.Background(), pool, commits); err != nil {
		t.Fatalf("the next Transact = %v, want nil", err)
	}
	if got := pool.Stat().NewConnsCount(); got != before {
		t.Fatalf("the pool opened %d connections after the next Transact, want %d", got, before)
	}
}

func TestAnOutcomeReadWhoseCommitReplyIsWithheldEndsAtItsBound(t *testing.T) {
	t.Run("the read", func(t *testing.T) {
		rig := newCommitRig(t)
		ctx := context.Background()
		tx, err := rig.observer.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		xid, err := insertProbe(ctx, tx, 1)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		fired := rig.fault.HoldReply("pg_xact_status", time.Hour)
		reading, cancel := context.WithTimeout(ForConcurrentWorker(WithTransactionTracking(context.Background())), outcomeReadBound)
		defer cancel()
		start := time.Now()
		got, err := readOutcome(reading, rig.pool, xid)
		elapsed := time.Since(start)
		testdb.Met(t, fired)
		if got != outcomeCommitted || err != nil {
			t.Fatalf("readOutcome = %v, %v; want committed: the status was read before the COMMIT", got, err)
		}
		t.Logf("the read returned after %s", elapsed)
		// A COMMIT given the grace a caller's commit has ends at outcomeReadBound plus
		// commitReplyBound; half that grace over the read's own bound leaves a loaded runner room
		// and still tells the two apart.
		if bound := outcomeReadBound + commitReplyBound/2; elapsed > bound {
			t.Fatalf("readOutcome returned after %s, want at most %s: its COMMIT has no grace", elapsed, bound)
		}
	})
	t.Run("the settlement", func(t *testing.T) {
		rig := newCommitRig(t)
		log := captureCommitLog(t)
		parent := WithTransactionTracking(context.Background())
		child, cancel := context.WithCancel(parent)
		defer cancel()
		fired := rig.fault.HoldReply(commitMatch, time.Hour)
		done := rig.transact(child, rig.write)
		testdb.Met(t, fired)
		rig.waitVisible(t)
		held := rig.fault.HoldReply("pg_xact_status", time.Hour)
		cancelled := time.Now()
		cancel()
		got := returned(t, done)
		testdb.Met(t, held)
		if got.err != nil {
			t.Fatalf("Transact = %v, want nil: the store said the transaction committed", got.err)
		}
		rig.wantHooks(t, 1)
		if n := logged(log, lostReplyCommitted); n != 1 {
			t.Fatalf("the log holds %d lines %q, want 1", n, lostReplyCommitted)
		}
		elapsed := got.at.Sub(cancelled)
		t.Logf("Transact returned %s after the cancel", elapsed)
		if elapsed > CommitSettleBound+500*time.Millisecond {
			t.Fatalf("Transact returned %s after the cancel, want at most %s", elapsed, CommitSettleBound+500*time.Millisecond)
		}
	})
}
