package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/testwait"
)

// oneConnection opens a store of one connection onto s's database, with s's settings, and answers
// it with the backend process id that connection holds. It is storetest.OneConnection for this
// package's own tests, which cannot import storetest, since storetest imports this package. A
// call a test makes on it runs on that one backend, so a wait for the call's queue watches that
// backend alone (awaitBlockedBy), and no other backend waiting on the same lock is taken for it.
// The pool closes when t ends. A call that needs two connections at once cannot run on it.
func oneConnection(t *testing.T, s *Store) (*Store, uint32) {
	t.Helper()
	config := s.Pool.Config()
	config.MaxConns = 1
	config.MinConns = 0
	config.MinIdleConns = 0
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a pool of one connection: %v", err)
	}
	one := NewPool(opened)
	t.Cleanup(one.Close)
	conn, err := one.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire the one connection: %v", err)
	}
	pid := conn.Conn().PgConn().PID()
	conn.Release()
	return &Store{Pool: one}, pid
}

// awaitBlockedBy waits for the backend pid, the one connection a call under test runs on
// (oneConnection), to wait on a lock the backend holder holds, reading pg_blocking_pids through
// s's pool. It returns the call's answer and true if the call answers on answered before its
// backend waits: such a call never waited on the lock, and the caller fails with what it answered.
// Its one bound is the test binary's deadline less testwait's margin.
func awaitBlockedBy[T any](t *testing.T, s *Store, holder, pid uint32, answered <-chan T) (T, bool) {
	t.Helper()
	type seen struct {
		answer T
		early  bool
	}
	got := testwait.UntilPolling(t, 2*time.Millisecond, fmt.Sprintf("backend %d to wait on a lock backend %d holds, or its call to answer", pid, holder), func() (seen, bool) {
		select {
		case answer := <-answered:
			return seen{answer: answer, early: true}, true
		default:
		}
		var blocked bool
		if err := s.Pool.QueryRow(context.Background(),
			`select $1::integer = any(pg_blocking_pids($2::integer))`, holder, pid).Scan(&blocked); err != nil {
			t.Fatalf("read whether backend %d waits on backend %d: %v", pid, holder, err)
		}
		return seen{}, blocked
	})
	return got.answer, got.early
}
