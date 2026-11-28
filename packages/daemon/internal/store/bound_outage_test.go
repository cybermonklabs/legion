package store

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// A bounded transaction (BoundAfterAcquire) holds its caller for its write bound when the database
// stops answering, not for its wait in the pool's queue, and it still begins when the server has
// closed the pooled connection it is handed. Each case runs on the daemon's own pool configuration
// (PoolConfig), with an acquire bound far longer than the write's, and the database stops
// answering through testdb.Outage:
//
//   - a server that answers no connection: the pool's dial for the transaction ends at the write
//     bound (PoolConfig's BeforeConnect);
//   - a server that goes silent before a transaction's BEGIN: the BEGIN runs under the write bound;
//   - a server that goes silent while a connection sits idle past the pool's ping threshold
//     (pingAfterIdle): a bounded transaction is handed it unpinged, and its BEGIN is the check,
//     under the write bound;
//   - a server that closed the pooled connection while it sat idle, as a restart, a
//     pg_terminate_backend or an idle_session_timeout does: the BEGIN meets the server's FATAL,
//     and the transaction begins on another connection (Pool.BeginTx).
//
// Before, pgxpool.BeginTx ran the dial's wait, the health ping and the BEGIN on the one context
// TransactWith gave the acquire bound, so each of the first three held the caller for the whole
// acquire bound (thirty seconds for a claim write, against the five the claim's machine is
// promised).
func TestABoundedTransactionOnADatabaseThatStopsAnsweringEndsAtItsWriteBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("PoolConfig bounds a bounded transaction's dial by its write bound and pings none of its connections", func(t *testing.T) {
		t.Parallel()
		config, err := PoolConfig(testDSN(t))
		if err != nil {
			t.Fatal(err)
		}
		bounded, cancel := BoundAfterAcquire(ctx, time.Minute, 500*time.Millisecond)
		defer cancel()
		for _, dial := range []struct {
			name string
			ctx  context.Context
			want time.Duration
		}{
			{"a bounded transaction's dial", bounded, 500 * time.Millisecond},
			{"any other dial", ctx, 2 * time.Minute},
		} {
			connConfig := config.ConnConfig.Copy()
			// pgxpool gives every dial two minutes before BeforeConnect runs, where dsn names none
			// (pgx v5.13.0 pgxpool/pool.go:291-293).
			connConfig.ConnectTimeout = 2 * time.Minute
			if err := config.BeforeConnect(dial.ctx, connConfig); err != nil || connConfig.ConnectTimeout != dial.want {
				t.Fatalf("%s after BeforeConnect: connect timeout %s (%v), want %s", dial.name, connConfig.ConnectTimeout, err, dial.want)
			}
		}
		idle := pgxpool.ShouldPingParams{IdleDuration: pingAfterIdle + time.Millisecond}
		fresh := pgxpool.ShouldPingParams{IdleDuration: pingAfterIdle}
		if config.ShouldPing(bounded, idle) || !config.ShouldPing(ctx, idle) || config.ShouldPing(ctx, fresh) {
			t.Fatalf("ShouldPing: bounded idle %t, unbounded idle %t, unbounded at the threshold %t; want false, true, false",
				config.ShouldPing(bounded, idle), config.ShouldPing(ctx, idle), config.ShouldPing(ctx, fresh))
		}
	})

	t.Run("a server that answers no connection", func(t *testing.T) {
		t.Parallel()
		outage := testdb.Outage(t, testDSN(t))
		outage.Silence()
		pool := poolOn(t, outage.DSN)
		const acquire = 20 * time.Second
		_, took, err := bounded(ctx, pool, acquire, 500*time.Millisecond)
		if err == nil || errors.Is(err, errAcquireBound) {
			t.Fatalf("a transaction on a server that answers no connection = %v, want the dial's failure at the write bound, not the acquire bound's", err)
		}
		if took > 1500*time.Millisecond {
			t.Fatalf("a transaction on a server that answers no connection took %s (%v), want about its 500ms write bound, far short of its %s acquire bound", took, err, acquire)
		}
	})

	t.Run("a server silent on BEGIN", func(t *testing.T) {
		t.Parallel()
		outage := testdb.Outage(t, testDSN(t))
		pool := poolOn(t, outage.DSN)
		if _, _, err := bounded(ctx, pool, 3*time.Second, 500*time.Millisecond); err != nil {
			t.Fatalf("a transaction before the server went silent: %v", err)
		}
		outage.Silence()
		_, took, err := bounded(ctx, pool, 3*time.Second, 500*time.Millisecond)
		if err == nil || errors.Is(err, errAcquireBound) {
			t.Fatalf("a transaction whose BEGIN the server never answers = %v, want the write bound's failure, not the acquire bound's", err)
		}
		if took > 1500*time.Millisecond {
			t.Fatalf("a transaction whose BEGIN the server never answers took %s, want about its 500ms write bound, short of its 3s acquire bound", took)
		}
	})

	t.Run("a server silent once a connection sits idle past the ping threshold", func(t *testing.T) {
		t.Parallel()
		outage := testdb.Outage(t, testDSN(t))
		pool := poolOn(t, outage.DSN)
		if _, _, err := bounded(ctx, pool, 3*time.Second, 500*time.Millisecond); err != nil {
			t.Fatalf("a transaction before the server went silent: %v", err)
		}
		time.Sleep(pingAfterIdle + 200*time.Millisecond)
		outage.Silence()
		_, took, err := bounded(ctx, pool, 3*time.Second, 500*time.Millisecond)
		if err == nil || errors.Is(err, errAcquireBound) {
			t.Fatalf("a transaction on an idle connection of a silent server = %v, want the write bound's failure, not the acquire bound's", err)
		}
		if took > 1500*time.Millisecond {
			t.Fatalf("a transaction on an idle connection of a silent server took %s, want about its 500ms write bound, short of its 3s acquire bound", took)
		}
	})

	for _, closed := range []struct {
		name  string
		conns int
	}{
		// One connection: the transaction is handed the one the server closed, and its second try
		// is a fresh dial.
		{"a server that closed the pool's one connection while it sat idle", 1},
		// Every connection of a pool of three, as a restart closes them all: the transaction is
		// handed each closed one in turn and its fourth try, the pool's size and one more, is a
		// fresh dial.
		{"a server that closed every connection of a pool of three while they sat idle", 3},
	} {
		t.Run(closed.name, func(t *testing.T) {
			t.Parallel()
			pool := storeWith(t, map[string]string{"pool_max_conns": strconv.Itoa(closed.conns)}).Pool()
			ended := idleBackends(t, pool, closed.conns)
			// A pgx pool and not pgx.Connect: the test DSN carries pool_max_conns
			// (testdb.PoolBounded), which a plain connection sends the server as a setting it
			// refuses.
			closer, err := pgxpool.New(ctx, testDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			for _, backend := range ended {
				var terminated bool
				if err := closer.QueryRow(ctx, "select pg_terminate_backend($1, 5000)", backend).Scan(&terminated); err != nil || !terminated {
					t.Fatalf("terminate the pool's connection (backend %d) = %t (%v), want it ended", backend, terminated, err)
				}
			}
			// Past the ping threshold, so an unbounded acquire would ping each connection and
			// replace it.
			time.Sleep(pingAfterIdle + 500*time.Millisecond)
			backend, took, err := bounded(ctx, pool, 3*time.Second, 2*time.Second)
			if err != nil {
				t.Fatalf("a bounded transaction handed the pool's %d closed connections = %v after %s, want it begun on another connection", len(ended), err, took)
			}
			if slices.Contains(ended, backend) {
				t.Fatalf("the transaction ran on backend %d, one of the %v the server ended", backend, ended)
			}
		})
	}
}

// bounded runs one transaction on pool under BoundAfterAcquire(acquire, write), and answers the
// backend it ran on and how long it took.
func bounded(ctx context.Context, pool *Pool, acquire, write time.Duration) (backend int, took time.Duration, err error) {
	boundedCtx, cancel := BoundAfterAcquire(ForConcurrentWorker(ctx), acquire, write)
	defer cancel()
	started := time.Now()
	err = Transact(boundedCtx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select pg_backend_pid()").Scan(&backend)
	})
	return backend, time.Since(started), err
}

// idleBackends opens n connections of pool at once and gives them back, so the pool holds n idle
// connections, and answers their backends.
func idleBackends(t *testing.T, pool *Pool, n int) []int {
	t.Helper()
	ctx := context.Background()
	backends := make([]int, 0, n)
	for range n {
		conn, err := pool.pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("open a connection of the pool: %v", err)
		}
		defer conn.Release()
		var backend int
		if err := conn.QueryRow(ctx, "select pg_backend_pid()").Scan(&backend); err != nil {
			t.Fatalf("read a connection's backend: %v", err)
		}
		backends = append(backends, backend)
	}
	return backends
}

// poolOn is a pool on dsn as the daemon builds its own (PoolConfig), closed at the test's end.
func poolOn(t *testing.T, dsn string) *Pool {
	t.Helper()
	config, err := PoolConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	pool := NewPool(raw)
	t.Cleanup(pool.Close)
	return pool
}
